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

package storage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// RemovedDir is the directory under a storage root where a removal moves a
// directory before it deletes it.
//
// Deleting a session directory in place takes one call per file, and a walk
// in name order reaches index/ before streams/. A crash in the middle left a
// session with no index and some of its landed files. A parse then rebuilt
// the index from what was left and published a round of tombstones. A rename
// is one call. Before it the directory is whole, and after it no listing sees
// any of it, because every listing of a root passes over names that start
// with an underscore.
const RemovedDir = "_removed"

// RemoveDir removes dir whole: one rename to <root>/_removed/<name>, then a
// delete there with DeleteTree.
//
// A dir that does not exist is not an error. A removal that stopped after its
// rename meets it that way when it runs again, and then deletes what it moved
// under the same name. dir must be a directory under root, and not a symbolic
// link, so a link can never lead a removal out of the root. For the same
// reason <root>/_removed must be a real directory, or absent, and anything
// else there is refused with ErrUnsafeRemoved. A target that exists already
// is refused. Only a leftover that could not be deleted leaves one, and a
// person has to look at it.
//
// The caller releases every lock held inside dir first. Windows cannot rename
// a directory that holds a file opened with no sharing, and that is how a
// lock is held there.
//
// hook, when it is not nil, is called with "renamed" after the rename, and
// with "deleting" after the first entry of the moved tree is deleted. An
// error from it stops RemoveDir there, as a crash would. The tests use it.
func RemoveDir(root, dir, name string, hook func(where string) error) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("storage: %q is not a name for an entry of %s", name, RemovedDir)
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." || !filepath.IsLocal(rel) ||
		rel == RemovedDir || strings.HasPrefix(rel, RemovedDir+string(filepath.Separator)) {
		return fmt.Errorf("storage: %s is not a directory under %s that a removal may take", dir, root)
	}
	removed, exists, err := removedDir(root)
	if err != nil {
		return err
	}
	target := filepath.Join(removed, name)
	var deleting func() error
	if hook != nil {
		deleting = func() error { return hook("deleting") }
	}
	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return DeleteTree(target, deleting)
	case err != nil:
		return err
	case !fi.IsDir():
		return fmt.Errorf("storage: %s is a symbolic link or not a directory, so it is not removed", dir)
	}
	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("%w: %s is left from an earlier removal, so %s is not removed", ErrExists, target, dir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if !exists {
		// Mkdir, never MkdirAll, and the check once more after it. A link
		// someone made at _removed since the first check makes Mkdir fail
		// as existing, and the check then refuses it.
		if err := os.Mkdir(removed, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		if _, _, err := removedDir(root); err != nil {
			return err
		}
	}
	if err := os.Rename(dir, target); err != nil {
		return err
	}
	// Either side of the rename is a whole directory, so a crash that loses
	// the rename is safe. Syncing both directories only narrows that window.
	syncDir(filepath.Dir(dir))
	syncDir(filepath.Dir(target))
	if hook != nil {
		if err := hook("renamed"); err != nil {
			return err
		}
	}
	return DeleteTree(target, deleting)
}

// DeleteTree deletes dir and everything under it: every file first, then the
// directories, deepest first.
//
// A landed file is read-only by contract. Each file is made writable before
// it is deleted, as the scenario runner does, so the delete does not depend
// on how a platform treats a read-only file. A directory is made readable
// and writable before its entries are read. A symbolic link is deleted as a
// link. Nothing it points at is read, changed or deleted. A dir that does not
// exist is not an error.
//
// after, when it is not nil, is called once, right after the first entry is
// deleted. An error from it stops DeleteTree there, as a crash would.
func DeleteTree(dir string, after func() error) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	called := false
	deleted := func() error {
		if called || after == nil {
			return nil
		}
		called = true
		return after()
	}
	if !fi.IsDir() {
		if err := removeEntry(dir, fi.Mode().IsRegular()); err != nil {
			return err
		}
		return deleted()
	}
	type entry struct {
		path    string
		regular bool
	}
	var files []entry
	var dirs []string
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, entry{p, d.Type().IsRegular()})
			return nil
		}
		// WalkDir calls this before it reads the directory, so a directory
		// someone made read-only can still be listed and emptied.
		if info, ierr := d.Info(); ierr == nil && info.Mode().Perm()&0o700 != 0o700 {
			if err := os.Chmod(p, info.Mode().Perm()|0o700); err != nil {
				return err
			}
		}
		dirs = append(dirs, p)
		return nil
	})
	if err != nil {
		return err
	}
	for _, f := range files {
		if err := removeEntry(f.path, f.regular); err != nil {
			return err
		}
		if err := deleted(); err != nil {
			return err
		}
	}
	// WalkDir lists a directory before everything under it, so the reverse
	// order deletes every directory after its children.
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := removeEntry(dirs[i], false); err != nil {
			return err
		}
		if err := deleted(); err != nil {
			return err
		}
	}
	return nil
}

// ErrUnsafeRemoved says <root>/_removed exists and is a symbolic link or not
// a directory. Nothing is read, deleted or moved through it.
var ErrUnsafeRemoved = errors.New("is a symbolic link or not a directory, so nothing is removed")

// removedDir checks <root>/_removed before a removal reads it or moves
// anything into it, and reports whether it exists. It uses Lstat, which
// does not follow a link. A link there would lead the sweep to delete what
// the link points at, and a rename to move a session directory out of the
// root. Before this check, a _removed linked to a directory holding a
// transcript and a read-only file left that directory empty, with no error.
func removedDir(root string) (dir string, exists bool, err error) {
	dir = filepath.Join(root, RemovedDir)
	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return dir, false, nil
	case err != nil:
		return dir, false, err
	case fi.Mode().Type() != fs.ModeDir:
		return dir, false, fmt.Errorf("storage: %s %w", dir, ErrUnsafeRemoved)
	}
	return dir, true, nil
}

// SweepRemoved deletes every entry under <root>/_removed. Each is what a
// removal moved there and did not finish deleting, because the process
// stopped or a delete failed. Every entry is tried, and the failures are
// returned together. A root with no _removed is left as it is. A _removed
// that is a symbolic link, or not a directory, is refused with
// ErrUnsafeRemoved, and nothing is read or deleted.
func SweepRemoved(root string) error {
	dir, exists, err := removedDir(root)
	if err != nil || !exists {
		return err
	}
	items, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	var errs []error
	for _, it := range items {
		if err := DeleteTree(filepath.Join(dir, it.Name()), nil); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// removeEntry deletes one entry. An entry that is already gone counts as
// deleted, so a delete that stopped halfway can simply run again.
func removeEntry(path string, regular bool) error {
	if regular {
		// A failure here is left to the delete, which reports it.
		_ = os.Chmod(path, 0o644)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// syncDir flushes a directory's entries to disk, as far as the platform
// allows. It is best effort by design.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
