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
	"io"
	"os"
)

// FileSystem keeps writes observable by durability tests. Reads use real files.
// Callers own path checks and locks. No operation retries a failed sync.
//
// The real file system binds the directory each operation names, through
// OpenDir, and acts through that handle on the one entry it names: no
// operation follows a link in that entry, and none follows a link in pull's
// layout above it. A path whose layout directory was replaced by a link is
// refused, never written through.
type FileSystem interface {
	Create(path string, mode os.FileMode) (WriteFile, error)
	Chmod(path string, mode os.FileMode) error
	Mkdir(path string, mode os.FileMode) error
	Link(from, to string) error
	Rename(from, to string) error
	// RenameNoReplace refuses an existing destination in the same call, or
	// returns ErrNoReplaceUnsupported having changed nothing.
	RenameNoReplace(from, to string) error
	Replace(parent, from, to string) error
	// ReplaceIn is Replace inside a directory the caller bound by OpenDir
	// and holds.
	ReplaceIn(dir *Dir, from, to string) error
	Remove(path string) error
	// CreateIn and RemoveIn work inside a directory the caller bound by
	// OpenDir and holds.
	CreateIn(dir *Dir, name string, mode os.FileMode) (WriteFile, error)
	RemoveIn(dir *Dir, name string) error
	SyncFile(path string) error
	SyncDir(path string) error
	// SyncDirIn syncs a directory the caller bound by OpenDir and holds,
	// through its handle: the directory bound, wherever its path leads now.
	SyncDirIn(dir *Dir) error
}

// WriteFile is a file FileSystem.Create opened for writing.
type WriteFile interface {
	io.Writer
	Chmod(os.FileMode) error
	Sync() error
	Close() error
}

// DiskFileSystem is the real file system. Production passes it, or nil.
type DiskFileSystem struct{}

// inParent binds path's directory and runs fn on its last component.
func inParent(path string, fn func(d *Dir, name string) error) error {
	d, name, err := bindParent(path)
	if err != nil {
		return err
	}
	return closing(fn(d, name), d.Close())
}

// inParents binds the directories of from and to, and runs fn on both last
// components.
func inParents(from, to string, fn func(fromDir *Dir, fromName string, toDir *Dir, toName string) error) error {
	return inParent(from, func(fromDir *Dir, fromName string) error {
		return inParent(to, func(toDir *Dir, toName string) error {
			return fn(fromDir, fromName, toDir, toName)
		})
	})
}

func (DiskFileSystem) Create(path string, mode os.FileMode) (WriteFile, error) {
	var file *os.File
	err := inParent(path, func(d *Dir, name string) error {
		var err error
		file, err = d.open(name, writeNew, mode)
		return err
	})
	if err != nil {
		return nil, err
	}
	return file, nil
}

func (DiskFileSystem) CreateIn(dir *Dir, name string, mode os.FileMode) (WriteFile, error) {
	return dir.open(name, writeNew, mode)
}

func (DiskFileSystem) Chmod(path string, mode os.FileMode) error {
	return inParent(path, func(d *Dir, name string) error { return d.chmod(name, mode) })
}

func (DiskFileSystem) Mkdir(path string, mode os.FileMode) error {
	return inParent(path, func(d *Dir, name string) error { return d.mkdir(name, mode) })
}

func (DiskFileSystem) Link(from, to string) error {
	return inParents(from, to, func(fromDir *Dir, fromName string, toDir *Dir, toName string) error {
		return fromDir.link(fromName, toDir, toName)
	})
}

func (DiskFileSystem) Rename(from, to string) error {
	return inParents(from, to, func(fromDir *Dir, fromName string, toDir *Dir, toName string) error {
		return fromDir.rename(fromName, toDir, toName, false)
	})
}

func (DiskFileSystem) RenameNoReplace(from, to string) error {
	return inParents(from, to, func(fromDir *Dir, fromName string, toDir *Dir, toName string) error {
		return fromDir.rename(fromName, toDir, toName, true)
	})
}

func (DiskFileSystem) Remove(path string) error {
	return inParent(path, func(d *Dir, name string) error { return d.remove(name) })
}

func (DiskFileSystem) RemoveIn(dir *Dir, name string) error { return dir.remove(name) }

func (DiskFileSystem) SyncFile(path string) error {
	return inParent(path, func(d *Dir, name string) error { return d.syncEntry(name) })
}

func (DiskFileSystem) SyncDir(path string) error {
	d, err := OpenDir(path)
	if err != nil {
		return err
	}
	return closing(d.sync(), d.Close())
}

func (DiskFileSystem) SyncDirIn(dir *Dir) error { return dir.sync() }

func (DiskFileSystem) Replace(parent, from, to string) error {
	d, err := OpenDir(parent)
	if err != nil {
		return err
	}
	err = d.replace(from, to)
	if closeError := d.Close(); closeError != nil {
		return errors.Join(err, closeError)
	}
	// Only rooted replacement errors use the Windows busy classification.
	return controlRenameError(err)
}

func (DiskFileSystem) ReplaceIn(dir *Dir, from, to string) error {
	return controlRenameError(dir.replace(from, to))
}

func writeFileSystem(files FileSystem) FileSystem {
	if files == nil {
		return DiskFileSystem{}
	}
	return files
}
