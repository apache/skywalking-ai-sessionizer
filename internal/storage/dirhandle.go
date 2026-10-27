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

// fileOpen is what an open inside a bound directory may do.
type fileOpen int

const (
	// readShared reads an existing regular file. On Windows the handle shares
	// read, write and delete, so a control record can be replaced under it.
	readShared fileOpen = iota
	// appendExisting appends to an existing regular file, such as the log a
	// parent created for its child.
	appendExisting
	// writeNew creates a file that must not exist yet.
	writeNew
	// lockExisting and lockNew take an exclusive lock on an existing file or
	// a new one. The process holds it until the file is closed.
	lockExisting
	lockNew
	// changeMode opens an existing regular file to change its mode or
	// attributes through the handle.
	changeMode
)

// Dir is a directory bound by a handle: reached from its volume's root
// through real directories only, then held open, with its identity checked
// at every step. Every file created, opened, locked or removed in it goes
// through the handle, so neither the directory nor any ancestor replaced by a
// link afterwards can move what happens there, and no open follows a link in
// the name it opens.
type Dir struct {
	path     string
	resolved string
	info     fs.FileInfo
	dir      *dirHandle
}

// Path is the directory's path as the caller named it, which names it in
// errors and in the durability model's records.
func (d *Dir) Path() string { return d.path }

// Real is the path the directory was reached by, through real directories
// only, when it was bound. A caller that classifies paths by their components
// classifies this one.
func (d *Dir) Real() string { return d.resolved }

// Info is the bound directory's identity.
func (d *Dir) Info() fs.FileInfo { return d.info }

// OpenDir binds dir, an absolute path naming a real directory, not a link to
// one. It resolves the links in the part of the path above pull's layout,
// the first component such as _remote or _pull, and keeps the rest as
// written. It then descends that path from the volume's root one entry at a
// time, refusing a link or anything but a directory at every step, and binds
// the handle only to the directory the path names now. A link above the
// storage root is therefore fine, and a link anywhere in the layout below it
// is refused: a layout directory replaced by a link after it was checked
// cannot lead the bind into another tree.
func OpenDir(dir string) (*Dir, error) {
	clean := filepath.Clean(dir)
	if !filepath.IsAbs(clean) {
		return nil, fmt.Errorf("storage: %s is not an absolute path", dir)
	}
	named, err := os.Lstat(clean)
	if err != nil {
		return nil, err
	}
	if named.Mode().Type() != fs.ModeDir {
		return nil, fmt.Errorf("%w: %s is not a real directory", ErrProtected, clean)
	}
	resolved, err := resolveAboveLayout(clean)
	if err != nil {
		return nil, err
	}
	reached, err := descendTo(resolved)
	if err != nil {
		return nil, err
	}
	return bind(clean, resolved, named, reached)
}

// resolveAboveLayout resolves the links of clean above its first protected
// component and appends the rest unresolved. A path with no such component
// is resolved whole.
func resolveAboveLayout(clean string) (string, error) {
	volume := filepath.VolumeName(clean)
	parts := strings.Split(clean[len(volume):], string(filepath.Separator))
	for i, part := range parts {
		if !protectedName(part) {
			continue
		}
		above := volume + string(filepath.Separator) + filepath.Join(parts[:i]...)
		resolved, err := filepath.EvalSymlinks(above)
		if err != nil {
			return "", err
		}
		return filepath.Join(append([]string{resolved}, parts[i:]...)...), nil
	}
	return filepath.EvalSymlinks(clean)
}

// bindParent binds the directory holding path, and returns path's last
// component.
func bindParent(path string) (*Dir, string, error) {
	clean := filepath.Clean(path)
	d, err := OpenDir(filepath.Dir(clean))
	if err != nil {
		return nil, "", err
	}
	return d, filepath.Base(clean), nil
}

// descendTo walks resolved from its volume's root through real directories
// alone, and returns the identity of the directory it reaches.
func descendTo(resolved string) (fs.FileInfo, error) {
	volume := filepath.VolumeName(resolved) + string(filepath.Separator)
	rel, err := filepath.Rel(volume, resolved)
	if err != nil {
		return nil, err
	}
	current, err := os.OpenRoot(volume)
	if err != nil {
		return nil, err
	}
	if rel != "." {
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			next, err := descend(current, part)
			err = errors.Join(err, current.Close())
			if err != nil {
				if next != nil {
					_ = next.Close()
				}
				return nil, err
			}
			current = next
		}
	}
	reached, err := current.Stat(".")
	return reached, errors.Join(err, current.Close())
}

// bind opens clean's handle and keeps it only when it is the directory the
// path named and the descent reached. The handle is opened by path once the
// descent is done; an ancestor replaced in between would give it another
// directory.
func bind(clean, resolved string, named, reached fs.FileInfo) (*Dir, error) {
	d, err := openDir(clean)
	if err != nil {
		return nil, err
	}
	d.resolved = resolved
	if !os.SameFile(reached, d.info) || !os.SameFile(named, d.info) {
		return nil, errors.Join(fmt.Errorf("%w: %s changed while it was opened", ErrProtected, clean), d.Close())
	}
	return d, nil
}

// descend opens part, one entry of parent, as a root of its own, refusing a
// link and anything that changed between its check and its open.
func descend(parent *os.Root, part string) (*os.Root, error) {
	info, err := parent.Lstat(part)
	if err != nil {
		return nil, err
	}
	if info.Mode().Type() != fs.ModeDir {
		return nil, fmt.Errorf("%w: %s is a link or not a directory", ErrProtected, filepath.Join(parent.Name(), part))
	}
	next, err := parent.OpenRoot(part)
	if err != nil {
		return nil, err
	}
	opened, err := next.Stat(".")
	if err == nil && !os.SameFile(info, opened) {
		err = fmt.Errorf("%w: %s changed while it was opened", ErrProtected, filepath.Join(parent.Name(), part))
	}
	if err != nil {
		return nil, errors.Join(err, next.Close())
	}
	return next, nil
}

// openDir opens path's handle and checks it is the real directory the path
// names, not a link to one.
func openDir(path string) (*Dir, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode().Type() != fs.ModeDir {
		return nil, fmt.Errorf("%w: %s is not a real directory", ErrProtected, path)
	}
	dir, opened, err := openDirHandle(path)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, errors.Join(fmt.Errorf("%w: %s changed while it was opened", ErrProtected, path), dir.close())
	}
	return &Dir{path: path, info: info, dir: dir}, nil
}

// Close releases the handle.
func (d *Dir) Close() error { return d.dir.close() }

// checkEntry refuses a name that is not one entry of the directory.
func (d *Dir) checkEntry(name string) error {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return fmt.Errorf("%w: %q is not an entry of %s", ErrProtected, name, d.path)
	}
	return nil
}

// open opens name, a single entry of the directory, as how says. The file must
// be a regular file; for an existing file, the one opened must be the one its
// name held, never what a link there points at.
func (d *Dir) open(name string, how fileOpen, perm os.FileMode) (*os.File, error) {
	if err := d.checkEntry(name); err != nil {
		return nil, err
	}
	f, err := d.dir.openAt(name, how, perm)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("%w: %s is not a regular file", ErrProtected, filepath.Join(d.path, name))
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

// inRoot runs fn on a root opened on the directory's path, once it is known
// to be the bound directory, so an ancestor replaced since cannot redirect
// it. fn names single entries only, which a root never follows a link for.
func (d *Dir) inRoot(name string, fn func(*os.Root) error) error {
	if err := d.checkEntry(name); err != nil {
		return err
	}
	root, err := os.OpenRoot(d.path)
	if err != nil {
		return err
	}
	opened, err := root.Stat(".")
	if err == nil && !os.SameFile(opened, d.info) {
		err = fmt.Errorf("%w: %s changed after it was bound", ErrProtected, d.path)
	}
	if err == nil {
		err = fn(root)
	}
	return closing(err, root.Close())
}

// closing returns err as it is when closing succeeded, so a caller can still
// test it with os.IsExist and its kin, which see through no joined error.
func closing(err, closeErr error) error {
	if closeErr == nil {
		return err
	}
	return errors.Join(err, closeErr)
}

// remove removes name, a file or an empty directory, from the directory.
func (d *Dir) remove(name string) error {
	return d.inRoot(name, func(root *os.Root) error { return root.Remove(name) })
}

// mkdir makes the directory name in the directory.
func (d *Dir) mkdir(name string, mode os.FileMode) error {
	return d.inRoot(name, func(root *os.Root) error { return root.Mkdir(name, mode) })
}

// lstat describes name in the directory without following a link there.
func (d *Dir) lstat(name string) (fs.FileInfo, error) {
	var info fs.FileInfo
	err := d.inRoot(name, func(root *os.Root) error {
		var err error
		info, err = root.Lstat(name)
		return err
	})
	return info, err
}

// replace renames from to to within the directory, replacing to, through
// the directory's own handle. On Windows it asks for POSIX semantics, so a
// reader holding the old file keeps it, and replaces a read-only file too:
// a record is first written read-only, and os.Root's rename refuses that.
func (d *Dir) replace(from, to string) error {
	if err := d.checkEntry(from); err != nil {
		return err
	}
	if err := d.checkEntry(to); err != nil {
		return err
	}
	return d.dir.renameAt(from, d.dir, to, filepath.Join(d.path, from), filepath.Join(d.path, to))
}

// rename moves name to toName in the directory to, through both handles.
// noReplace refuses an existing destination in the same call, or returns
// ErrNoReplaceUnsupported having changed nothing.
func (d *Dir) rename(name string, to *Dir, toName string, noReplace bool) error {
	if err := d.checkEntry(name); err != nil {
		return err
	}
	if err := to.checkEntry(toName); err != nil {
		return err
	}
	from, dest := filepath.Join(d.path, name), filepath.Join(to.path, toName)
	if noReplace {
		return renameNoReplaceAt(d.dir, name, to.dir, toName, from, dest)
	}
	return d.dir.renameAt(name, to.dir, toName, from, dest)
}

// link gives the file name a second name, toName in the directory to.
func (d *Dir) link(name string, to *Dir, toName string) error {
	if err := d.checkEntry(name); err != nil {
		return err
	}
	if err := to.checkEntry(toName); err != nil {
		return err
	}
	return d.dir.linkAt(name, to.dir, toName, filepath.Join(d.path, name), filepath.Join(to.path, toName))
}

// chmod changes the mode of the regular file name, through a handle on it.
func (d *Dir) chmod(name string, mode os.FileMode) error {
	f, err := d.open(name, changeMode, 0)
	if err != nil {
		return err
	}
	return errors.Join(f.Chmod(mode), f.Close())
}

// syncEntry makes the regular file name's data durable.
func (d *Dir) syncEntry(name string) error {
	f, err := d.open(name, readShared, 0)
	if err != nil {
		return err
	}
	return errors.Join(syncFileHandle(f), f.Close())
}

// sync makes the directory's entries durable.
func (d *Dir) sync() error { return d.dir.sync() }

// lockIn takes an exclusive lock on name in dir, without waiting.
func lockIn(dir, name string, how fileOpen) (*os.File, error) {
	d, err := OpenDir(dir)
	if err != nil {
		return nil, err
	}
	f, err := d.open(name, how, PermState)
	return f, errors.Join(err, d.Close())
}
