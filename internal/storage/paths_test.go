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
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

const testTag = "20261025T103200.123456789Z"

func mkdirs(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		check(t, os.MkdirAll(p, 0o755))
	}
}

func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("this system makes no symbolic links: %v", err)
	}
}

// Every component asz owns, in any letter case, and every lock file, is
// refused before anything is opened. Nothing else is.
func TestCheckSuppliedPathRefusesOwnedTrees(t *testing.T) {
	base := t.TempDir()
	for _, tc := range []struct {
		rel     string
		refused bool
	}{
		{"labels.yaml", false},
		{"notes/pricing.yaml", false},
		{"a_remote/x.yaml", false},
		{"_remote/prod/c/x.yaml", true},
		{"root/_pull/conversations/c/x", true},
		{"root/_root/x", true},
		{"root/_admit/x", true},
		{"root/_leases/read-x.lock", true},
		{"root/_building/b/x", true},
		{"root/_removing/b/x", true},
		{"root/_Remote/prod/x", true},
		{"root/_PULL/x", true},
		{"root/session/.lock", true},
		{"root/session/.LOCK", true},
	} {
		_, err := storage.CheckSuppliedPath(filepath.Join(base, tc.rel), "")
		if tc.refused != errors.Is(err, storage.ErrProtected) {
			t.Fatalf("%s: %v", tc.rel, err)
		}
	}
}

// A link can lead neither into an owned tree nor out of one.
func TestCheckSuppliedPathFollowsLinksBothWays(t *testing.T) {
	base := t.TempDir()
	pull := filepath.Join(base, "root", "_pull")
	mkdirs(t, pull, filepath.Join(base, "outside"))
	check(t, os.WriteFile(filepath.Join(pull, ".lock"), nil, 0o644))
	check(t, os.WriteFile(filepath.Join(base, "outside", "x"), []byte("x"), 0o644))
	symlinkOrSkip(t, filepath.Join(pull, ".lock"), filepath.Join(base, "token"))
	if _, err := storage.CheckSuppliedPath(filepath.Join(base, "token"), ""); !errors.Is(err, storage.ErrProtected) {
		t.Fatalf("a link into _pull: %v", err)
	}
	symlinkOrSkip(t, filepath.Join(base, "outside"), filepath.Join(pull, "out"))
	if _, err := storage.CheckSuppliedPath(filepath.Join(pull, "out", "x"), ""); !errors.Is(err, storage.ErrProtected) {
		t.Fatalf("a path through _pull that resolves outside: %v", err)
	}
	// A new file is classified by where it would be created.
	symlinkOrSkip(t, pull, filepath.Join(base, "logs"))
	if _, err := storage.CheckSuppliedPath(filepath.Join(base, "logs", "new", "navigation.log"), ""); !errors.Is(err, storage.ErrProtected) {
		t.Fatalf("a new file under a link into _pull: %v", err)
	}
	if _, err := storage.CheckSuppliedPath(filepath.Join(base, "outside", "new", "navigation.log"), ""); err != nil {
		t.Fatalf("a new file in an ordinary directory: %v", err)
	}
}

// The admitted snapshot's own files are usable, but not a lock file or a
// protected directory inside it, and not another snapshot.
func TestCheckSuppliedPathInsideTheAdmittedSnapshot(t *testing.T) {
	root := t.TempDir()
	conversation := filepath.Join(root, "_remote", "prod", "c1")
	snapshot := filepath.Join(conversation, testTag)
	other := filepath.Join(conversation, "20261025T103201.000000000Z")
	mkdirs(t, filepath.Join(snapshot, "s1", "judgments", "j000001", "work"), filepath.Join(snapshot, "_leases"), filepath.Join(snapshot, "_root"), other)
	for _, tc := range []struct {
		path    string
		refused bool
	}{
		{filepath.Join(snapshot, "s1", "judgments", "j000001", "work", "navigation.log"), false},
		{filepath.Join(snapshot, "labels.yaml"), false},
		{filepath.Join(snapshot, "_root", ".lock"), true},
		{filepath.Join(snapshot, "_leases", "read-x.lock"), true},
		{filepath.Join(other, "labels.yaml"), true},
		{filepath.Join(conversation, "_copy"), true},
	} {
		_, err := storage.CheckSuppliedPath(tc.path, snapshot)
		if tc.refused != errors.Is(err, storage.ErrProtected) {
			t.Fatalf("%s: %v", tc.path, err)
		}
	}
}

// Replacing an open lock's pathname would leave its holder on the old file
// while a second process locks the new one. Token creation aimed at a held
// lock through a link is refused, and the holder stays exclusive.
func TestCreateSuppliedCannotReplaceAHeldLock(t *testing.T) {
	base := t.TempDir()
	pull := filepath.Join(base, "root", storage.PullDir)
	mkdirs(t, pull)
	held, err := storage.LockOwnedCreate(pull, nil)
	check(t, err)
	defer held.Unlock()
	symlinkOrSkip(t, filepath.Join(pull, ".lock"), filepath.Join(base, "tokens.yaml"))
	if _, err := storage.CreateSupplied(filepath.Join(base, "tokens.yaml"), ""); !errors.Is(err, storage.ErrProtected) {
		t.Fatalf("token file through a link to a lock: %v", err)
	}
	if _, err := storage.LockOwned(pull); !errors.Is(err, storage.ErrBusy) {
		t.Fatalf("the holder is no longer exclusive: %v", err)
	}
}

func TestCreateSupplied(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "navigation.log")
	file, err := storage.CreateSupplied(path, "")
	check(t, err)
	check(t, file.Close())
	// An independent log is never appended to: an existing file is refused.
	if _, err := storage.CreateSupplied(path, ""); !errors.Is(err, os.ErrExist) {
		t.Fatalf("created over an existing file: %v", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	// The last component may not be a link at open time.
	evidence := filepath.Join(dir, "a.sd")
	check(t, os.WriteFile(evidence, []byte("evidence"), 0o444))
	symlinkOrSkip(t, evidence, filepath.Join(dir, "log-link"))
	if _, err := storage.CreateSupplied(filepath.Join(dir, "log-link"), ""); err == nil {
		t.Fatal("created through a link")
	}
	requireText(t, evidence, "evidence")
}

func TestClassifyRoot(t *testing.T) {
	root := t.TempDir()
	conversation := filepath.Join(root, "_remote", "prod", "c1")
	snapshot := filepath.Join(conversation, testTag)
	mkdirs(t,
		filepath.Join(snapshot, "s1"),
		filepath.Join(conversation, "_building", "b"),
		filepath.Join(conversation, "_removing", "r"),
		filepath.Join(conversation, "_admit"),
		filepath.Join(conversation, "notatag"),
		filepath.Join(root, "_remote", "_root", "c1", testTag),
		filepath.Join(root, "_pull", "conversations", "c1", testTag),
		filepath.Join(root, "session"),
	)
	got, err := storage.ClassifyRoot(snapshot)
	check(t, err)
	resolvedRoot, err := filepath.EvalSymlinks(root)
	check(t, err)
	if got.Kind != storage.SnapshotRoot || got.Remote != "prod" || got.Conversation != "c1" || got.Tag != testTag || got.StorageRoot != resolvedRoot {
		t.Fatalf("snapshot: %+v", got)
	}
	got, err = storage.ClassifyRoot(root)
	if err != nil || got.Kind != storage.OrdinaryRoot {
		t.Fatalf("ordinary root: %+v, %v", got, err)
	}
	for _, refused := range []string{
		filepath.Join(root, "_remote"),
		filepath.Join(root, "_remote", "prod"),
		conversation,
		filepath.Join(snapshot, "s1"),
		filepath.Join(conversation, "_building", "b"),
		filepath.Join(conversation, "_removing", "r"),
		filepath.Join(conversation, "_admit"),
		filepath.Join(conversation, "notatag"),
		filepath.Join(root, "_remote", "_root", "c1", testTag),
		filepath.Join(root, "_pull", "conversations", "c1", testTag),
	} {
		if _, err := storage.ClassifyRoot(refused); !errors.Is(err, storage.ErrProtected) {
			t.Fatalf("%s: %v", refused, err)
		}
	}
}

// A link cannot make a snapshot look ordinary, or an owned path look like a
// snapshot or an ordinary root.
func TestClassifyRootThroughLinks(t *testing.T) {
	base := t.TempDir()
	snapshot := filepath.Join(base, "root", "_remote", "prod", "c1", testTag)
	mkdirs(t, snapshot, filepath.Join(base, "ordinary"))
	symlinkOrSkip(t, snapshot, filepath.Join(base, "looks-ordinary"))
	if _, err := storage.ClassifyRoot(filepath.Join(base, "looks-ordinary")); !errors.Is(err, storage.ErrProtected) {
		t.Fatalf("an ordinary-looking link to a snapshot: %v", err)
	}
	other := filepath.Join(base, "root", "_remote", "prod", "c1", "20261025T103201.000000000Z")
	symlinkOrSkip(t, filepath.Join(base, "ordinary"), other)
	if _, err := storage.ClassifyRoot(other); !errors.Is(err, storage.ErrProtected) {
		t.Fatalf("a snapshot-looking link to an ordinary directory: %v", err)
	}
}

// A link inside the layout cannot pass for it: _remote, the remote, the
// conversation and the tag must each be a real directory where the path names
// them, even when the link leads to a layout of the same shape.
func TestClassifyRootRefusesLinksInsideTheLayout(t *testing.T) {
	base := t.TempDir()
	snapshot := filepath.Join(base, "b", storage.RemoteDir, "prod", "c1", testTag)
	mkdirs(t, snapshot, filepath.Join(base, "a"))
	symlinkOrSkip(t, filepath.Join(base, "b", storage.RemoteDir), filepath.Join(base, "a", storage.RemoteDir))
	if _, err := storage.ClassifyRoot(filepath.Join(base, "a", storage.RemoteDir, "prod", "c1", testTag)); !errors.Is(err, storage.ErrProtected) {
		t.Fatalf("a linked _remote: %v", err)
	}
	// A link above the storage root is fine, and the identities come back.
	symlinkOrSkip(t, filepath.Join(base, "b"), filepath.Join(base, "storage"))
	got, err := storage.ClassifyRoot(filepath.Join(base, "storage", storage.RemoteDir, "prod", "c1", testTag))
	check(t, err)
	info, err := os.Lstat(snapshot)
	check(t, err)
	if got.Kind != storage.SnapshotRoot || got.SnapshotInfo == nil || got.ConversationInfo == nil || !os.SameFile(got.SnapshotInfo, info) {
		t.Fatalf("snapshot through a link above the root: %+v", got)
	}
}

// A layout directory replaced after classification, even by a link to its own
// moved tree, which keeps every directory below it the same, is refused when
// admission checks the root again.
func TestRootRecheckSeesALayoutDirectoryReplaced(t *testing.T) {
	root := t.TempDir()
	snapshot := filepath.Join(root, storage.RemoteDir, "prod", "c1", testTag)
	mkdirs(t, snapshot)
	classified, err := storage.ClassifyRoot(snapshot)
	check(t, err)
	check(t, classified.Recheck())
	moved := filepath.Join(root, "moved")
	check(t, os.Rename(filepath.Join(root, storage.RemoteDir), moved))
	symlinkOrSkip(t, moved, filepath.Join(root, storage.RemoteDir))
	if err := classified.Recheck(); !errors.Is(err, storage.ErrProtected) {
		t.Fatalf("a _remote replaced by a link to itself: %v", err)
	}
}

// The same snapshot moved into a fresh layout at the same path resolves to the
// same names and keeps its own identity, but its conversation directory, where
// admission finds its gate, is another. That is refused too.
func TestRootRecheckSeesAFreshLayoutTree(t *testing.T) {
	root := t.TempDir()
	snapshot := filepath.Join(root, storage.RemoteDir, "prod", "c1", testTag)
	mkdirs(t, snapshot)
	classified, err := storage.ClassifyRoot(snapshot)
	check(t, err)
	moved := filepath.Join(root, "moved")
	check(t, os.Rename(filepath.Join(root, storage.RemoteDir), moved))
	mkdirs(t, filepath.Dir(snapshot))
	check(t, os.Rename(filepath.Join(moved, "prod", "c1", testTag), snapshot))
	if err := classified.Recheck(); !errors.Is(err, storage.ErrProtected) {
		t.Fatalf("the snapshot moved into a fresh layout: %v", err)
	}
}

// A file inside the admitted snapshot is classified from the snapshot down,
// however its path spells the snapshot: written through a link to an
// ancestor, it is the same file. A protected name below the snapshot is
// still refused, in either spelling.
func TestAnAdmittedSnapshotIsMatchedInEitherSpelling(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "real")
	snapshot := filepath.Join(target, storage.RemoteDir, "prod", "c1", "20261025T103200.000000000Z")
	for _, dir := range []string{filepath.Join(snapshot, "s1", "judgments", "j000001", "work"), filepath.Join(snapshot, storage.LeasesDir)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skip(err)
	}
	admitted, err := filepath.EvalSymlinks(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	through := filepath.Join(link, storage.RemoteDir, "prod", "c1", "20261025T103200.000000000Z")
	for _, root := range []string{snapshot, through} {
		if _, err := storage.CheckSuppliedPath(filepath.Join(root, "s1", "judgments", "j000001", "work", "labels.yaml"), admitted); err != nil {
			t.Errorf("a file in the admitted snapshot, written as %s: %v", root, err)
		}
		if _, err := storage.CheckSuppliedPath(filepath.Join(root, storage.LeasesDir, "read-x.lock"), admitted); !errors.Is(err, storage.ErrProtected) {
			t.Errorf("a lease in the admitted snapshot, written as %s: %v", root, err)
		}
		if _, err := storage.CheckSuppliedPath(filepath.Join(root, "s1", "labels.yaml"), ""); !errors.Is(err, storage.ErrProtected) {
			t.Errorf("a file in a snapshot no lease admits, written as %s: %v", root, err)
		}
	}
}
