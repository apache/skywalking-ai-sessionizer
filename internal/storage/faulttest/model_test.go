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

package faulttest_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage/faulttest"
)

func modelFor(t *testing.T) *faulttest.Model {
	t.Helper()
	model, err := faulttest.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func create(t *testing.T, model *faulttest.Model, name, text string, syncFile, syncParent bool) {
	t.Helper()
	path := filepath.Join(model.Root, name)
	file, err := model.Create(path, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(file, text); err != nil {
		t.Fatal(err)
	}
	if syncFile {
		if err := file.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if syncParent {
		if err := model.SyncDir(filepath.Dir(path)); err != nil {
			t.Fatal(err)
		}
	}
}

func crash(t *testing.T, model *faulttest.Model, point int) *faulttest.Model {
	t.Helper()
	recovered, err := model.PowerLoss(t.TempDir(), point)
	if err != nil {
		t.Fatal(err)
	}
	return recovered
}

func content(t *testing.T, model *faulttest.Model, path, want string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(model.Root, path))
	if err != nil || string(data) != want {
		t.Fatalf("%s: %q, %v; want %q", path, data, err, want)
	}
}

func absent(t *testing.T, model *faulttest.Model, path string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(model.Root, path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s: want absent, got %v", path, err)
	}
}

func TestFileAndEntrySyncAreSeparate(t *testing.T) {
	for _, tc := range []struct {
		name         string
		file, parent bool
	}{
		{"neither", false, false}, {"file", true, false}, {"parent", false, true}, {"both", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := modelFor(t)
			create(t, model, "record", "visible", tc.file, tc.parent)
			content(t, model, "record", "visible")
			for _, point := range []int{faulttest.DropAll, 0} {
				recovered := crash(t, model, point)
				if !tc.parent {
					absent(t, recovered, "record")
					continue
				}
				want := ""
				if tc.file {
					want = "visible"
				}
				content(t, recovered, "record", want)
			}
		})
	}
}

func TestOrderedLossKeepsOnlyEarlierChanges(t *testing.T) {
	model := modelFor(t)
	create(t, model, "early", "early", false, false)
	point := model.Point()
	create(t, model, "late", "late", false, false)
	durable := filepath.Join(model.Root, "durable")
	if err := storage.MakeAncestorsDurable(model.Root, durable, 0o755, model); err != nil {
		t.Fatal(err)
	}
	// Directory sync made both entries durable, but not their contents.
	create(t, model, "durable/record", "synced later", true, true)
	all := crash(t, model, faulttest.DropAll)
	ordered := crash(t, model, point)
	content(t, all, "early", "")
	content(t, all, "late", "")
	content(t, ordered, "early", "early")
	content(t, ordered, "late", "")
	content(t, all, "durable/record", "synced later")
	content(t, ordered, "durable/record", "synced later")
	create(t, ordered, "after-restart", "restart", true, true)
	content(t, crash(t, ordered, faulttest.DropAll), "after-restart", "restart")
}

func TestFailedSyncCannotBeCured(t *testing.T) {
	for _, fileSync := range []bool{true, false} {
		model := modelFor(t)
		file, err := model.Create(filepath.Join(model.Root, "record"), 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(file, "lost"); err != nil {
			t.Fatal(err)
		}
		model.FailSync(1)
		sync := func() error { return model.SyncDir(model.Root) }
		if fileSync {
			sync = file.Sync
		}
		if err := sync(); !errors.Is(err, syscall.EIO) {
			t.Fatalf("selected sync: %v", err)
		}
		if err := sync(); err != nil {
			t.Fatal(err)
		}
		if err := file.Sync(); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if err := model.SyncDir(model.Root); err != nil {
			t.Fatal(err)
		}
		for _, point := range []int{faulttest.DropAll, 0} {
			recovered := crash(t, model, point)
			if fileSync {
				content(t, recovered, "record", "")
			} else {
				absent(t, recovered, "record")
			}
		}
	}
}

func TestUnmodeledWritesNeverBecomeDurable(t *testing.T) {
	model := modelFor(t)
	create(t, model, "modeled", "original", true, true)
	if err := os.WriteFile(filepath.Join(model.Root, "modeled"), []byte("unmodeled replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := storage.WriteAtomicDurable(filepath.Join(model.Root, "outside"), 0o600, func(out io.Writer) error {
		_, err := io.WriteString(out, "judge output")
		return err
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := model.SyncDir(model.Root); err != nil {
		t.Fatal(err)
	}
	if err := model.SyncFile(filepath.Join(model.Root, "modeled")); err != nil {
		t.Fatal(err)
	}
	for _, point := range []int{faulttest.DropAll, model.Point()} {
		recovered := crash(t, model, point)
		content(t, recovered, "modeled", "original")
		absent(t, recovered, "outside")
	}
}

func TestRenameParentsAndDescendants(t *testing.T) {
	for _, parents := range []int{0, 1, 2, 3} {
		model := modelFor(t)
		first, second := filepath.Join(model.Root, "first"), filepath.Join(model.Root, "second")
		for _, directory := range []string{first, second} {
			if err := storage.MakeAncestorsDurable(model.Root, directory, 0o755, model); err != nil {
				t.Fatal(err)
			}
		}
		tree := filepath.Join(first, "tree")
		if err := storage.MakeAncestorsDurable(model.Root, tree, 0o755, model); err != nil {
			t.Fatal(err)
		}
		create(t, model, "first/tree/record", "complete", true, true)
		point := model.Point()
		if err := model.Rename(tree, filepath.Join(second, "tree")); err != nil {
			t.Fatal(err)
		}
		if parents&1 != 0 {
			if err := model.SyncDir(first); err != nil {
				t.Fatal(err)
			}
		}
		if parents&2 != 0 {
			if err := model.SyncDir(second); err != nil {
				t.Fatal(err)
			}
		}
		content(t, model, "second/tree/record", "complete")
		for _, cutoff := range []int{faulttest.DropAll, point} {
			recovered := crash(t, model, cutoff)
			if parents&1 != 0 {
				absent(t, recovered, "first/tree")
			} else {
				content(t, recovered, "first/tree/record", "complete")
			}
			if parents&2 == 0 {
				absent(t, recovered, "second/tree")
			} else {
				content(t, recovered, "second/tree/record", "complete")
			}
		}
	}
}

func TestLinksModesAndUnlinks(t *testing.T) {
	model := modelFor(t)
	create(t, model, "first", "same object", true, true)
	first, second := filepath.Join(model.Root, "first"), filepath.Join(model.Root, "second")
	if err := model.Link(first, second); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if err := model.SyncDir(model.Root); err != nil {
		t.Fatal(err)
	}
	point := model.Point()
	if err := model.Chmod(first, 0o444); err != nil {
		t.Fatal(err)
	}
	before := crash(t, model, faulttest.DropAll)
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(before.Root, "second"))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o640 {
			t.Fatalf("mode became durable without file sync: %o", info.Mode())
		}
	}
	if err := model.SyncFile(second); err != nil {
		t.Fatal(err)
	}
	for _, cutoff := range []int{faulttest.DropAll, point} {
		recovered := crash(t, model, cutoff)
		one, err := os.Stat(filepath.Join(recovered.Root, "first"))
		if err != nil {
			t.Fatal(err)
		}
		two, err := os.Stat(filepath.Join(recovered.Root, "second"))
		if err != nil {
			t.Fatal(err)
		}
		// Both names exist at once, so a reused file identity cannot make two
		// different files look like one.
		if !os.SameFile(one, two) {
			t.Fatal("materialized links have different identities")
		}
		if two.Mode().Perm()&0o222 != 0 {
			t.Fatalf("mode sync missing: %o", two.Mode())
		}
	}
	if runtime.GOOS == "windows" {
		// Windows removal may clear a shared read-only attribute. Pull never links there.
		if err := model.Chmod(first, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := model.SyncFile(first); err != nil {
			t.Fatal(err)
		}
	}
	if err := model.Remove(first); err != nil {
		t.Fatal(err)
	}
	model.Fault = func(op faulttest.Operation) error {
		if op.Kind == "directory sync" {
			return syscall.EIO
		}
		return nil
	}
	if err := model.SyncDir(model.Root); !errors.Is(err, syscall.EIO) {
		t.Fatal(err)
	}
	model.Fault = nil
	if err := model.SyncDir(model.Root); err != nil {
		t.Fatal(err)
	}
	for _, cutoff := range []int{faulttest.DropAll, point} {
		recovered := crash(t, model, cutoff)
		content(t, recovered, "first", "same object")
		content(t, recovered, "second", "same object")
	}
}

func TestPowerLossRequiresFreshDirectory(t *testing.T) {
	model := modelFor(t)
	create(t, model, "record", "keep", true, true)
	if _, err := model.PowerLoss(model.Root, faulttest.DropAll); err == nil {
		t.Fatal("overwrote the visible tree")
	}
	content(t, model, "record", "keep")
}

// A file made through a bound directory is recorded in that
// directory, though the directory was renamed after it was bound and a new
// one took its old path. A power loss keeps what that directory's sync made
// durable, where it really is, whether the directory is synced by its new
// path or through the handle.
func TestBoundDirectoryChangesAreRecordedWhereTheHandleIs(t *testing.T) {
	for name, sync := range map[string]func(model *faulttest.Model, d *storage.Dir, b string) error{
		"by its path":        func(model *faulttest.Model, _ *storage.Dir, b string) error { return model.SyncDir(b) },
		"through its handle": func(model *faulttest.Model, d *storage.Dir, _ string) error { return model.SyncDirIn(d) },
	} {
		t.Run(name, func(t *testing.T) { boundDirectoryChanges(t, sync) })
	}
}

func boundDirectoryChanges(t *testing.T, sync func(model *faulttest.Model, d *storage.Dir, b string) error) {
	model := modelFor(t)
	a, b := filepath.Join(model.Root, "a"), filepath.Join(model.Root, "b")
	if err := model.Mkdir(a, 0o755); err != nil {
		t.Fatal(err)
	}
	create(t, model, "a/old", "old", true, true)
	if err := model.SyncDir(model.Root); err != nil {
		t.Fatal(err)
	}
	d, err := storage.OpenDir(a)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := model.Rename(a, b); err != nil {
		t.Fatal(err)
	}
	if err := model.Mkdir(a, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := model.SyncDir(model.Root); err != nil {
		t.Fatal(err)
	}
	file, err := model.CreateIn(d, "new", 0o640)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(file, "new"); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	// A removal through the handle refuses once the path leads elsewhere,
	// so it cannot be recorded in the wrong directory.
	if err := model.RemoveIn(d, "old"); !errors.Is(err, storage.ErrProtected) {
		t.Fatalf("removal through a renamed directory: %v", err)
	}
	if err := sync(model, d, b); err != nil {
		t.Fatal(err)
	}
	recovered := crash(t, model, faulttest.DropAll)
	content(t, recovered, "b/new", "new")
	content(t, recovered, "b/old", "old")
	absent(t, recovered, "a/new")
	// A creation through a directory bound in the recovered tree is
	// recorded there too.
	d2, err := storage.OpenDir(filepath.Join(recovered.Root, "b"))
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	again, err := recovered.CreateIn(d2, "after", 0o640)
	if err != nil {
		t.Fatal(err)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
	if err := recovered.SyncDir(filepath.Join(recovered.Root, "b")); err != nil {
		t.Fatal(err)
	}
	content(t, crash(t, recovered, faulttest.DropAll), "b/after", "")
}

// createDurableIn makes name through a handle on dir and syncs the file and
// the directory, in model.
func createDurableIn(t *testing.T, model *faulttest.Model, dir, name string) {
	t.Helper()
	d, err := storage.OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	file, err := model.CreateIn(d, name, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(file, name); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := model.SyncDir(dir); err != nil {
		t.Fatal(err)
	}
}

// A directory removed and made again under the same name may get the
// removed one's inode back. The removed one's identity is forgotten, so a
// creation in the new one is recorded there and survives its syncs.
func TestARemovedDirectoryIsNeverTakenForANewOne(t *testing.T) {
	model := modelFor(t)
	a := filepath.Join(model.Root, "a")
	for range 3 {
		if err := model.Mkdir(a, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := model.Remove(a); err != nil {
			t.Fatal(err)
		}
	}
	if err := model.Mkdir(a, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := model.SyncDir(model.Root); err != nil {
		t.Fatal(err)
	}
	createDurableIn(t, model, a, "made")
	content(t, crash(t, model, faulttest.DropAll), "a/made", "made")
}

// A rename whose source parent was not synced comes back in both places
// after a power loss. Each copy is a directory of its own with the one
// node's identity, so a creation through either is recorded and survives
// the next power loss.
func TestADirectoryRestoredTwiceKeepsBothIdentities(t *testing.T) {
	model := modelFor(t)
	first, second := filepath.Join(model.Root, "first"), filepath.Join(model.Root, "second")
	for _, directory := range []string{filepath.Join(first, "tree"), second} {
		if err := storage.MakeAncestorsDurable(model.Root, directory, 0o755, model); err != nil {
			t.Fatal(err)
		}
	}
	if err := model.Rename(filepath.Join(first, "tree"), filepath.Join(second, "tree")); err != nil {
		t.Fatal(err)
	}
	if err := model.SyncDir(second); err != nil {
		t.Fatal(err)
	}
	recovered := crash(t, model, faulttest.DropAll)
	for _, parent := range []string{"first", "second"} {
		if info, err := os.Lstat(filepath.Join(recovered.Root, parent, "tree")); err != nil || !info.IsDir() {
			t.Fatalf("%s/tree after the power loss: %v", parent, err)
		}
	}
	createDurableIn(t, recovered, filepath.Join(recovered.Root, "first", "tree"), "through-first")
	createDurableIn(t, recovered, filepath.Join(recovered.Root, "second", "tree"), "through-second")
	again := crash(t, recovered, faulttest.DropAll)
	for _, parent := range []string{"first", "second"} {
		content(t, again, parent+"/tree/through-first", "through-first")
		content(t, again, parent+"/tree/through-second", "through-second")
	}
}

// restoredTwice makes first/tree and second/tree two copies of one node, as
// a power loss after a rename whose source parent was not synced does.
func restoredTwice(t *testing.T) *faulttest.Model {
	t.Helper()
	model := modelFor(t)
	first, second := filepath.Join(model.Root, "first"), filepath.Join(model.Root, "second")
	for _, directory := range []string{filepath.Join(first, "tree"), second} {
		if err := storage.MakeAncestorsDurable(model.Root, directory, 0o755, model); err != nil {
			t.Fatal(err)
		}
	}
	if err := model.Rename(filepath.Join(first, "tree"), filepath.Join(second, "tree")); err != nil {
		t.Fatal(err)
	}
	if err := model.SyncDir(second); err != nil {
		t.Fatal(err)
	}
	return crash(t, model, faulttest.DropAll)
}

// Removing one restored copy forgets that copy alone. The other still
// records a creation, and so does a new directory that may reuse the
// removed one's inode.
func TestRemovingOneRestoredCopyKeepsTheOther(t *testing.T) {
	model := restoredTwice(t)
	first := filepath.Join(model.Root, "first")
	if err := model.Remove(filepath.Join(first, "tree")); err != nil {
		t.Fatal(err)
	}
	if err := model.SyncDir(first); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(model.Root, "fresh")
	if err := model.Mkdir(fresh, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := model.SyncDir(model.Root); err != nil {
		t.Fatal(err)
	}
	createDurableIn(t, model, filepath.Join(model.Root, "second", "tree"), "kept")
	createDurableIn(t, model, fresh, "new")
	again := crash(t, model, faulttest.DropAll)
	content(t, again, "second/tree/kept", "kept")
	content(t, again, "fresh/new", "new")
	absent(t, again, "first/tree")
}

// A rename over the other restored copy replaces a directory of its own,
// though the model holds both as one node. The replaced one is forgotten:
// a new directory that may reuse its inode records its creation, and so
// does the directory the rename moved.
func TestARenameOverARestoredCopyForgetsTheReplacedOne(t *testing.T) {
	model := restoredTwice(t)
	first, second := filepath.Join(model.Root, "first"), filepath.Join(model.Root, "second")
	if err := model.Rename(filepath.Join(first, "tree"), filepath.Join(second, "tree")); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{first, second} {
		if err := model.SyncDir(dir); err != nil {
			t.Fatal(err)
		}
	}
	fresh := filepath.Join(model.Root, "fresh")
	if err := model.Mkdir(fresh, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := model.SyncDir(model.Root); err != nil {
		t.Fatal(err)
	}
	createDurableIn(t, model, filepath.Join(second, "tree"), "moved")
	createDurableIn(t, model, fresh, "new")
	again := crash(t, model, faulttest.DropAll)
	content(t, again, "second/tree/moved", "moved")
	content(t, again, "fresh/new", "new")
}

// A directory that cannot be read is still identified when it is removed,
// so a new directory that reuses its inode is never taken for it.
func TestAnUnreadableRemovedDirectoryIsForgotten(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a Unix user that permissions bind")
	}
	model := modelFor(t)
	a := filepath.Join(model.Root, "a")
	for range 3 {
		if err := model.Mkdir(a, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(a, 0); err != nil {
			t.Fatal(err)
		}
		if err := model.Remove(a); err != nil {
			t.Fatal(err)
		}
	}
	if err := model.Mkdir(a, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := model.SyncDir(model.Root); err != nil {
		t.Fatal(err)
	}
	createDurableIn(t, model, a, "made")
	content(t, crash(t, model, faulttest.DropAll), "a/made", "made")
}

// Removing a link written with a trailing slash removes the link, and the
// directory it leads to keeps its identity: a creation in it is recorded and
// survives its syncs.
func TestRemovingALinkKeepsItsTargetsIdentity(t *testing.T) {
	model := modelFor(t)
	a := filepath.Join(model.Root, "a")
	if err := model.Mkdir(a, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := model.SyncDir(model.Root); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(model.Root, "link")
	if err := os.Symlink(a, link); err != nil {
		t.Skip(err)
	}
	if err := model.Remove(link + string(filepath.Separator)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(a); err != nil {
		t.Fatalf("the link's target went: %v", err)
	}
	createDurableIn(t, model, a, "kept")
	content(t, crash(t, model, faulttest.DropAll), "a/kept", "kept")
}
