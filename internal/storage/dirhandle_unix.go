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

//go:build !windows

package storage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// dirHandle is a directory's own descriptor. Every open, rename and link in
// it is relative to the descriptor, with O_NOFOLLOW on the one name it
// names, so neither an ancestor nor that name can be a link that leads
// elsewhere.
type dirHandle struct {
	f  *os.File
	fd int
}

func openDirHandle(path string) (*dirHandle, fs.FileInfo, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil {
		return nil, nil, errors.Join(err, f.Close())
	}
	return &dirHandle{f: f, fd: fd}, info, nil
}

func (d *dirHandle) close() error { return d.f.Close() }

func (d *dirHandle) openAt(name string, how fileOpen, perm os.FileMode) (*os.File, error) {
	path := filepath.Join(d.f.Name(), name)
	// O_NONBLOCK keeps an open of an existing name from waiting on a FIFO
	// put in a file's place; the caller's regular-file check refuses it.
	flag := unix.O_RDONLY | unix.O_NONBLOCK
	switch how {
	case appendExisting:
		flag = unix.O_WRONLY | unix.O_APPEND | unix.O_NONBLOCK
	case writeNew:
		flag = unix.O_WRONLY | unix.O_CREAT | unix.O_EXCL
	case lockExisting:
		flag = unix.O_RDWR | unix.O_NONBLOCK
	case lockNew:
		flag = unix.O_RDWR | unix.O_CREAT | unix.O_EXCL
	}
	fd, err := unix.Openat(d.fd, name, flag|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(perm.Perm()))
	if errors.Is(err, unix.ELOOP) {
		return nil, fmt.Errorf("%w: %s is a link", ErrProtected, path)
	}
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(fd), path)
	if how == lockExisting || how == lockNew {
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if errors.Is(err, unix.EWOULDBLOCK) {
			err = ErrBusy
		}
		if err != nil {
			return nil, errors.Join(err, f.Close())
		}
	}
	return f, nil
}

func (d *dirHandle) renameAt(name string, to *dirHandle, toName, from, dest string) error {
	if err := unix.Renameat(d.fd, name, to.fd, toName); err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: dest, Err: err}
	}
	return nil
}

// linkAt makes the link without following name, should it be a link.
func (d *dirHandle) linkAt(name string, to *dirHandle, toName, from, dest string) error {
	if err := unix.Linkat(d.fd, name, to.fd, toName, 0); err != nil {
		return &os.LinkError{Op: "link", Old: from, New: dest, Err: err}
	}
	return nil
}

// sync flushes the directory's entries, as syncDirEntries does for a path.
func (d *dirHandle) sync() error {
	err := d.f.Sync()
	if err == nil || !unsupported(err) {
		return err
	}
	if ferr := unix.Fsync(d.fd); ferr != nil && !unsupported(ferr) {
		return ferr
	}
	return nil
}

// syncFileHandle makes a file's data durable, as SyncFile does for a path.
func syncFileHandle(f *os.File) error {
	if err := f.Sync(); err != nil && !unsupported(err) {
		return err
	}
	return nil
}
