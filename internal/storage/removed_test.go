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
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// sessionTree writes the shape a session directory has: a lock, an index,
// read-only landed files, and cursors. It returns how many files it wrote.
func sessionTree(t *testing.T, dir string) int {
	t.Helper()
	write := func(p string, perm os.FileMode) {
		if err := storage.WriteAtomic(p, perm, func(w io.Writer) error {
			_, err := io.WriteString(w, "{}\n")
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, ".lock"), storage.PermState)
	write(filepath.Join(dir, "index", "index.state"), storage.PermState)
	write(filepath.Join(dir, "index", "entries.bin"), storage.PermLanded)
	write(filepath.Join(dir, "streams", "main", storage.LandedName("transcript", storage.Stamp(time.Unix(0, 0)), 1)), storage.PermLanded)
	write(filepath.Join(dir, "streams", "main", "transcript.cursor"), storage.PermState)
	write(filepath.Join(dir, "runs", "wf_r1", storage.LandedName("journal", storage.Stamp(time.Unix(0, 0)), 2)), storage.PermLanded)
	return 6
}

// countFiles counts the entries under dir that are not directories, and
// gives 0 when dir is gone.
func countFiles(t *testing.T, dir string) int {
	t.Helper()
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	n := 0
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func gone(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	return errors.Is(err, fs.ErrNotExist)
}

// A removal deletes landed files, which are read-only by contract, and a
// directory someone made read-only. It says so once, after the first entry,
// which is where the crash tests stop it.
func TestDeleteTreeDeletesReadOnlyFilesAndCallsAfterOnce(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "S")
	total := sessionTree(t, dir)
	locked := filepath.Join(dir, "runs", "wf_r1")
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	// If the test fails before the delete, the temporary directory must
	// still be removable.
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	calls, left := 0, -1
	err := storage.DeleteTree(dir, func() error {
		calls++
		left = countFiles(t, dir)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || left != total-1 {
		t.Fatalf("after ran %d times with %d files left; want once, with %d left", calls, left, total-1)
	}
	if !gone(t, dir) {
		t.Fatalf("%s is still there", dir)
	}
	if err := storage.DeleteTree(dir, nil); err != nil {
		t.Fatalf("a tree that is gone already is not an error: %v", err)
	}
}

// A symbolic link inside the tree is deleted as a link. What it points at,
// outside the tree, is not touched, and its mode is not changed.
func TestDeleteTreeNeverFollowsALink(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	kept := filepath.Join(outside, "real.sd")
	if err := storage.WriteAtomic(kept, storage.PermLanded, func(w io.Writer) error {
		_, err := io.WriteString(w, "keep me\n")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "S")
	sessionTree(t, dir)
	if err := os.Symlink(outside, filepath.Join(dir, "streams", "to-a-directory")); err != nil {
		t.Skipf("this system makes no symbolic links: %v", err)
	}
	if err := os.Symlink(kept, filepath.Join(dir, "to-a-file")); err != nil {
		t.Skipf("this system makes no symbolic links: %v", err)
	}
	if err := storage.DeleteTree(dir, nil); err != nil {
		t.Fatal(err)
	}
	if !gone(t, dir) {
		t.Fatalf("%s is still there", dir)
	}
	fi, err := os.Stat(kept)
	if err != nil {
		t.Fatalf("the file a link pointed at was deleted: %v", err)
	}
	if fi.Mode().Perm() != storage.PermLanded {
		t.Fatalf("the file a link pointed at changed mode to %v", fi.Mode().Perm())
	}
}

// An error from after stops the delete where it is, as a crash would. The
// rest of the tree stays for the next attempt, which finishes it.
func TestDeleteTreeStopsWhereAfterFails(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "S")
	total := sessionTree(t, dir)
	stop := errors.New("stop")
	if err := storage.DeleteTree(dir, func() error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("DeleteTree gave %v, want the error after returned", err)
	}
	if n := countFiles(t, dir); n != total-1 {
		t.Fatalf("%d files left, want %d: the delete must stop right after the first", n, total-1)
	}
	if err := storage.DeleteTree(dir, nil); err != nil {
		t.Fatal(err)
	}
	if !gone(t, dir) {
		t.Fatalf("%s is still there", dir)
	}
}

// A removal renames the directory away in one call, so no reader ever sees
// half of it, and then deletes it where no listing looks.
func TestRemoveDirRenamesInOneCallThenDeletes(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "S")
	total := sessionTree(t, dir)
	moved := filepath.Join(root, storage.RemovedDir, "S")
	var seen []string
	err := storage.RemoveDir(root, dir, "S", func(where string) error {
		seen = append(seen, where)
		switch where {
		case "renamed":
			if !gone(t, dir) {
				t.Errorf("after the rename %s is still there", dir)
			}
			if n := countFiles(t, moved); n != total {
				t.Errorf("after the rename %d of %d files are under %s", n, total, moved)
			}
		case "deleting":
			if n := countFiles(t, moved); n != total-1 {
				t.Errorf("after the first delete %d files are left, want %d", n, total-1)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(seen, ",") != "renamed,deleting" {
		t.Fatalf("the hook saw %v, want renamed then deleting", seen)
	}
	if !gone(t, moved) {
		t.Fatalf("%s is still there", moved)
	}
	items, err := os.ReadDir(filepath.Join(root, storage.RemovedDir))
	if err != nil || len(items) != 0 {
		t.Fatalf("%s holds %d entries, want none: %v", storage.RemovedDir, len(items), err)
	}
}

// A removal stopped after its rename leaves the whole directory under
// _removed. The next attempt deletes it: RemoveDir again, which finds dir
// gone, or the sweep a removal starts with.
func TestARemovalStoppedAfterTheRenameIsFinishedLater(t *testing.T) {
	stop := errors.New("stop")
	for _, finish := range []struct {
		name string
		fn   func(root, dir string) error
	}{
		{"again", func(root, dir string) error { return storage.RemoveDir(root, dir, "S", nil) }},
		{"sweep", func(root, _ string) error { return storage.SweepRemoved(root) }},
	} {
		t.Run(finish.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "S")
			total := sessionTree(t, dir)
			moved := filepath.Join(root, storage.RemovedDir, "S")
			err := storage.RemoveDir(root, dir, "S", func(where string) error {
				if where == "renamed" {
					return stop
				}
				return nil
			})
			if !errors.Is(err, stop) {
				t.Fatalf("RemoveDir gave %v, want the hook's error", err)
			}
			if n := countFiles(t, moved); n != total || !gone(t, dir) {
				t.Fatalf("after a stop at the rename: %d of %d files moved, and %s gone: %v", n, total, dir, gone(t, dir))
			}
			if err := finish.fn(root, dir); err != nil {
				t.Fatal(err)
			}
			if !gone(t, moved) {
				t.Fatalf("%s is still there", moved)
			}
		})
	}
}

// A removal never takes a path outside the root, never follows a link, and
// never overwrites a leftover a person has to look at.
func TestRemoveDirRefusesWhatItMustNotTake(t *testing.T) {
	root := t.TempDir()
	elsewhere := t.TempDir()
	total := sessionTree(t, filepath.Join(elsewhere, "S"))

	if err := storage.RemoveDir(root, filepath.Join(elsewhere, "S"), "S", nil); err == nil {
		t.Fatal("a directory outside the root was taken")
	}
	if err := storage.RemoveDir(root, root, "S", nil); err == nil {
		t.Fatal("the root itself was taken")
	}
	if err := storage.RemoveDir(root, filepath.Join(root, storage.RemovedDir), "x", nil); err == nil {
		t.Fatalf("%s itself was taken", storage.RemovedDir)
	}
	for _, name := range []string{"", ".", "..", "a/b", `a\b`} {
		if err := storage.RemoveDir(root, filepath.Join(root, "S"), name, nil); err == nil {
			t.Fatalf("the name %q was accepted", name)
		}
	}

	// A file is not a directory, and it is left as it was.
	file := filepath.Join(root, "F")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := storage.RemoveDir(root, file, "F", nil); err == nil {
		t.Fatal("a file was taken as a directory")
	}
	if gone(t, file) {
		t.Fatal("the refused file was deleted")
	}

	// A leftover under the same name stops the removal, and the directory
	// stays whole.
	dir := filepath.Join(root, "S2")
	n := sessionTree(t, dir)
	if err := os.MkdirAll(filepath.Join(root, storage.RemovedDir, "S2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := storage.RemoveDir(root, dir, "S2", nil); !errors.Is(err, storage.ErrExists) {
		t.Fatalf("RemoveDir over a leftover gave %v, want ErrExists", err)
	}
	if got := countFiles(t, dir); got != n {
		t.Fatalf("the refused directory holds %d of its %d files", got, n)
	}

	// A link is refused, and nothing behind it is touched.
	link := filepath.Join(root, "S3")
	if err := os.Symlink(filepath.Join(elsewhere, "S"), link); err != nil {
		t.Skipf("this system makes no symbolic links: %v", err)
	}
	if err := storage.RemoveDir(root, link, "S3", nil); err == nil {
		t.Fatal("a symbolic link was taken as a directory")
	}
	if gone(t, link) {
		t.Fatal("the refused link was deleted")
	}
	if got := countFiles(t, filepath.Join(elsewhere, "S")); got != total {
		t.Fatalf("the directory behind the link holds %d of its %d files", got, total)
	}
}

// A _removed that is a symbolic link would lead the sweep to delete what it
// points at, and a rename to move a session directory out of the root.
// Before the check, a link to a directory holding a transcript and a
// read-only file left that directory empty, with no error. Both refuse it
// now, and every file behind the link survives with its mode.
func TestALinkAtRemovedIsRefused(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	precious := filepath.Join(outside, "precious.jsonl")
	readOnly := filepath.Join(outside, "keepdir", "x")
	// Named as a session a stopped removal would finish, so RemoveDir of a
	// directory that is gone reaches it through the link.
	leftover := filepath.Join(outside, "S2", "streams", "main", "kept.sd")
	for _, p := range []string{precious, readOnly, leftover} {
		if err := storage.WriteAtomic(p, storage.PermLanded, func(w io.Writer) error {
			_, err := io.WriteString(w, "keep me\n")
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	root := filepath.Join(base, "root")
	dir := filepath.Join(root, "S")
	total := sessionTree(t, dir)
	if err := os.Symlink(outside, filepath.Join(root, storage.RemovedDir)); err != nil {
		t.Skipf("this system makes no symbolic links: %v", err)
	}

	if err := storage.SweepRemoved(root); !errors.Is(err, storage.ErrUnsafeRemoved) {
		t.Fatalf("the sweep through a link gave %v, want ErrUnsafeRemoved", err)
	}
	if err := storage.RemoveDir(root, dir, "S", nil); !errors.Is(err, storage.ErrUnsafeRemoved) {
		t.Fatalf("RemoveDir through a link gave %v, want ErrUnsafeRemoved", err)
	}
	if got := countFiles(t, dir); got != total {
		t.Fatalf("the refused directory holds %d of its %d files", got, total)
	}
	if err := storage.RemoveDir(root, filepath.Join(root, "S2"), "S2", nil); !errors.Is(err, storage.ErrUnsafeRemoved) {
		t.Fatalf("RemoveDir of a directory that is gone, through a link, gave %v, want ErrUnsafeRemoved", err)
	}
	for _, p := range []string{precious, readOnly, leftover} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("%s, behind the link, was deleted: %v", p, err)
		}
		if fi.Mode().Perm() != storage.PermLanded {
			t.Fatalf("%s, behind the link, changed mode to %v", p, fi.Mode().Perm())
		}
	}
	if !gone(t, filepath.Join(outside, "S")) {
		t.Fatal("a session directory was moved through the link")
	}
}

// A _removed that is a plain file is refused the same way, and it stays.
func TestAFileAtRemovedIsRefused(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "S")
	total := sessionTree(t, dir)
	file := filepath.Join(root, storage.RemovedDir)
	if err := os.WriteFile(file, []byte("not a directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := storage.SweepRemoved(root); !errors.Is(err, storage.ErrUnsafeRemoved) {
		t.Fatalf("the sweep over a file gave %v, want ErrUnsafeRemoved", err)
	}
	if err := storage.RemoveDir(root, dir, "S", nil); !errors.Is(err, storage.ErrUnsafeRemoved) {
		t.Fatalf("RemoveDir into a file gave %v, want ErrUnsafeRemoved", err)
	}
	if got := countFiles(t, dir); got != total {
		t.Fatalf("the refused directory holds %d of its %d files", got, total)
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "not a directory\n" {
		t.Fatalf("the file at %s changed: %q, %v", storage.RemovedDir, data, err)
	}
}

// The sweep deletes every leftover, and a root with none is left as it is.
func TestSweepRemovedDeletesEveryLeftover(t *testing.T) {
	root := t.TempDir()
	if err := storage.SweepRemoved(root); err != nil {
		t.Fatal(err)
	}
	if !gone(t, filepath.Join(root, storage.RemovedDir)) {
		t.Fatalf("the sweep made %s in a root that had none", storage.RemovedDir)
	}
	sessionTree(t, filepath.Join(root, storage.RemovedDir, "S"))
	sessionTree(t, filepath.Join(root, storage.RemovedDir, "S.chain"))
	if err := storage.SweepRemoved(root); err != nil {
		t.Fatal(err)
	}
	items, err := os.ReadDir(filepath.Join(root, storage.RemovedDir))
	if err != nil || len(items) != 0 {
		t.Fatalf("%s holds %d entries after the sweep, want none: %v", storage.RemovedDir, len(items), err)
	}
}
