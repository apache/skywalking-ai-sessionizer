// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package storage_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage/faulttest"
)

func entryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	check(t, err)
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

// A first bootstrap makes the layout, writes an empty history before anything
// else, makes the readers' gate without taking it, and keeps _pull. All of it
// survives a power loss.
func TestBootstrapConversationFirst(t *testing.T) {
	model := controlModel(t)
	dir, pull, err := storage.BootstrapConversation(context.Background(), model.Root, "prod", "c1", model)
	check(t, err)
	defer pull.Unlock()
	if _, err := storage.LockOwned(filepath.Join(dir, storage.PullDir)); !errors.Is(err, storage.ErrBusy) {
		t.Fatalf("_pull is not held: %v", err)
	}
	gate, err := storage.LockOwned(filepath.Join(dir, storage.AdmitDir))
	check(t, err)
	check(t, gate.Unlock())
	history, err := storage.ReadOuterCopy(dir, "prod", "c1")
	check(t, err)
	if history.GreatestTag != "" || history.Publishing != nil || len(removedTags(t, dir)) != 0 {
		t.Fatalf("history %+v", history)
	}
	recovered := powerLoss(t, model, faulttest.DropAll)
	again := filepath.Join(recovered.Root, storage.RemoteDir, "prod", "c1")
	if _, err := storage.ReadOuterCopy(again, "prod", "c1"); err != nil {
		t.Fatalf("history after power loss: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(again, storage.AdmitDir, ".lock")); err != nil {
		t.Fatalf("gate after power loss: %v", err)
	}
}

// A later bootstrap keeps the history as it is, and removes the temporary
// files a stopped marker write leaves.
func TestBootstrapConversationAgain(t *testing.T) {
	root := t.TempDir()
	dir, pull, err := storage.BootstrapConversation(context.Background(), root, "prod", "c1", nil)
	check(t, err)
	history, err := storage.ReadOuterCopy(dir, "prod", "c1")
	check(t, err)
	_, err = history.Reserve(time.Now(), buildID, digestA)
	check(t, err)
	check(t, storage.WriteOuterCopy(dir, history, "", nil))
	check(t, pull.Unlock())
	check(t, os.WriteFile(filepath.Join(dir, ".tmp-123"), []byte("half"), 0o600))
	_, pull, err = storage.BootstrapConversation(context.Background(), root, "prod", "c1", nil)
	check(t, err)
	defer pull.Unlock()
	if _, err := os.Lstat(filepath.Join(dir, ".tmp-123")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("leftover temporary: %v", err)
	}
	kept, err := storage.ReadOuterCopy(dir, "prod", "c1")
	check(t, err)
	if kept.GreatestTag != history.GreatestTag || kept.Publishing == nil {
		t.Fatalf("history changed: %+v", kept)
	}
}

// Anything that is not a copy of this conversation is refused, and nothing in
// it changes apart from lock directories.
func TestBootstrapConversationRefusals(t *testing.T) {
	for name, prepare := range map[string]func(t *testing.T, dir string){
		"another conversation's marker": func(t *testing.T, dir string) {
			check(t, storage.WriteOuterCopy(filepath.Join(filepath.Dir(dir), "c2"), storage.OuterCopy{Remote: "prod", Conversation: "c2"}, "", nil))
			data, err := os.ReadFile(filepath.Join(filepath.Dir(dir), "c2", storage.CopyMarker))
			check(t, err)
			check(t, os.WriteFile(filepath.Join(dir, storage.CopyMarker), data, 0o644))
		},
		"content without a marker": func(t *testing.T, dir string) {
			check(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("mine"), 0o644))
		},
		"a tree without a marker": func(t *testing.T, dir string) {
			mkdirs(t, filepath.Join(dir, testTag))
		},
		"a temporary directory": func(t *testing.T, dir string) {
			mkdirs(t, filepath.Join(dir, ".tmp-x"))
		},
		"a temporary link": func(t *testing.T, dir string) {
			symlinkOrSkip(t, filepath.Join(dir, "elsewhere"), filepath.Join(dir, ".tmp-x"))
		},
		// With a valid marker, only the check of the reserved prefix refuses it.
		"a temporary directory beside a marker": func(t *testing.T, dir string) {
			check(t, storage.WriteOuterCopy(dir, storage.OuterCopy{Remote: "prod", Conversation: "c1"}, "", nil))
			mkdirs(t, filepath.Join(dir, ".tmp-x"))
		},
	} {
		root := t.TempDir()
		dir := filepath.Join(root, storage.RemoteDir, "prod", "c1")
		mkdirs(t, dir, filepath.Join(filepath.Dir(dir), "c2"))
		prepare(t, dir)
		before := entryNames(t, dir)
		if _, _, err := storage.BootstrapConversation(context.Background(), root, "prod", "c1", nil); err == nil {
			t.Fatalf("%s: admitted", name)
		}
		var after []string
		for _, entry := range entryNames(t, dir) {
			if entry != storage.PullDir && entry != storage.RootLockDir {
				after = append(after, entry)
			}
		}
		if len(after) != len(before) {
			t.Fatalf("%s: %v became %v", name, before, after)
		}
		// The refusal released both locks.
		for _, lock := range []string{storage.PullDir, storage.RootLockDir} {
			if l, err := storage.LockOwned(filepath.Join(dir, lock)); err == nil {
				check(t, l.Unlock())
			} else if !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("%s: %s still held: %v", name, lock, err)
			}
		}
	}
}

func TestBootstrapConversationRefusesLinksAndBadNames(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	symlinkOrSkip(t, outside, filepath.Join(root, storage.RemoteDir))
	if _, _, err := storage.BootstrapConversation(context.Background(), root, "prod", "c1", nil); err == nil {
		t.Fatal("bootstrapped through a link at _remote")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("wrote through the link: %v", entries)
	}
	for _, names := range [][2]string{{"_pull", "c1"}, {"prod", "_admit"}, {"prod/x", "c1"}, {"prod", "a:b"}, {"prod", ".tmp-c"}} {
		if _, _, err := storage.BootstrapConversation(context.Background(), t.TempDir(), names[0], names[1], nil); err == nil {
			t.Fatalf("%v: bootstrapped", names)
		}
	}
}

func TestBootstrapConversationWaitsForBusyPull(t *testing.T) {
	root := t.TempDir()
	_, pull, err := storage.BootstrapConversation(context.Background(), root, "prod", "c1", nil)
	check(t, err)
	defer pull.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, _, err := storage.BootstrapConversation(ctx, root, "prod", "c1", nil); !errors.Is(err, storage.ErrBusy) {
		t.Fatalf("a busy conversation: %v", err)
	}
}

func TestCheckCollision(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, storage.RemoteDir, "prod", "c1")
	mkdirs(t, dir)
	if got, err := storage.CheckCollision(dir, "prod", "c1", testTag, digestB); err != nil || got != storage.TagAbsent {
		t.Fatalf("absent: %v, %v", got, err)
	}
	tree := filepath.Join(dir, testTag)
	mkdirs(t, tree)
	if got, _ := storage.CheckCollision(dir, "prod", "c1", testTag, digestB); got != storage.TagUnowned {
		t.Fatalf("a tree without a marker: %v", got)
	}
	check(t, storage.WriteSnapshotCopy(tree, testTag, snapshotCopy(), nil))
	if got, err := storage.CheckCollision(dir, "prod", "c1", testTag, digestB); err != nil || got != storage.TagOwned {
		t.Fatalf("owned: %v, %v", got, err)
	}
	if got, _ := storage.CheckCollision(dir, "prod", "c1", testTag, digestA); got != storage.TagUnowned {
		t.Fatalf("another intent's tree: %v", got)
	}
	if got, _ := storage.CheckCollision(dir, "stage", "c1", testTag, digestB); got != storage.TagUnowned {
		t.Fatalf("another remote's tree: %v", got)
	}
	other := "20261025T103201.000000000Z"
	symlinkOrSkip(t, tree, filepath.Join(dir, other))
	if got, _ := storage.CheckCollision(dir, "prod", "c1", other, digestB); got != storage.TagUnowned {
		t.Fatalf("a link: %v", got)
	}
}

// An owned entry is deleted by its place alone, even with every record inside
// it gone, and no link is followed.
func TestDeleteOwned(t *testing.T) {
	base := t.TempDir()
	building := filepath.Join(base, storage.BuildingDir)
	outside := filepath.Join(base, "outside")
	tree := filepath.Join(building, "b")
	mkdirs(t, filepath.Join(tree, "_work", "files"), outside)
	check(t, os.WriteFile(filepath.Join(tree, "_work", "files", "x.tmp"), []byte("x"), 0o444))
	check(t, os.WriteFile(filepath.Join(outside, "keep"), []byte("keep"), 0o444))
	symlinkOrSkip(t, outside, filepath.Join(tree, "link"))
	check(t, storage.DeleteOwned(building, "b", nil))
	if _, err := os.Lstat(tree); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("tree: %v", err)
	}
	requireText(t, filepath.Join(outside, "keep"), "keep")
	check(t, storage.DeleteOwned(building, "b", nil))
	for _, name := range []string{"..", "a/b", ""} {
		if err := storage.DeleteOwned(building, name, nil); !errors.Is(err, storage.ErrProtected) {
			t.Fatalf("%q: %v", name, err)
		}
	}
	symlinkOrSkip(t, outside, filepath.Join(building, "linked"))
	if err := storage.DeleteOwned(building, "linked", nil); !errors.Is(err, storage.ErrProtected) {
		t.Fatalf("a link entry: %v", err)
	}
	requireText(t, filepath.Join(outside, "keep"), "keep")
}

func TestDetach(t *testing.T) {
	model := controlModel(t)
	dir := filepath.Join(model.Root, "c1")
	removing := filepath.Join(dir, storage.RemovingDir)
	tree := filepath.Join(dir, testTag)
	check(t, storage.MakeAncestorsDurable(model.Root, removing, 0o755, model))
	check(t, model.Mkdir(tree, 0o755))
	target, err := storage.Detach(tree, removing, testTag, model)
	check(t, err)
	if filepath.Dir(target) != removing || len(filepath.Base(target)) != len(testTag)+1+64 {
		t.Fatalf("target %s", target)
	}
	ops := model.Operations()
	tail := ops[len(ops)-3:]
	if tail[0].Kind != "rename-noreplace" || tail[1].Kind != "directory sync" || tail[1].Path != dir ||
		tail[2].Kind != "directory sync" || tail[2].Path != removing {
		t.Fatalf("operations %+v", tail)
	}
	if _, err := os.Lstat(tree); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("tree left at its name: %v", err)
	}
}

// A bootstrap stopped at any step, the first marker's write included, leaves
// what the next bootstrap admits: on the files left visible, and after a power
// loss.
func TestBootstrapResumesAfterEveryInterruption(t *testing.T) {
	faultEach(t, func(_ *testing.T, model *faulttest.Model) func() error {
		return func() error {
			_, pull, err := storage.BootstrapConversation(context.Background(), model.Root, "prod", "c1", model)
			if err == nil {
				err = pull.Unlock()
			}
			return err
		}
	}, func(t *testing.T, recovered *faulttest.Model) {
		dir, pull, err := storage.BootstrapConversation(context.Background(), recovered.Root, "prod", "c1", recovered)
		if err != nil {
			t.Fatalf("bootstrap after a power loss: %v", err)
		}
		check(t, pull.Unlock())
		if _, err := storage.ReadOuterCopy(dir, "prod", "c1"); err != nil {
			t.Fatal(err)
		}
	})
}

// An owned deletion stopped before it starts deletes nothing, a tree of
// directories alone included, and the next deletion finishes it.
func TestAStoppedOwnedDeletionLeavesTheTree(t *testing.T) {
	building := filepath.Join(t.TempDir(), "_building")
	check(t, os.MkdirAll(filepath.Join(building, "b", "x", "y"), 0o755))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := storage.DeleteOwnedContext(ctx, building, "b", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("a stopped deletion: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(building, "b", "x", "y")); err != nil {
		t.Fatalf("a stopped deletion removed: %v", err)
	}
	check(t, storage.DeleteOwned(building, "b", nil))
	if _, err := os.Lstat(filepath.Join(building, "b")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the next deletion: %v", err)
	}
}
