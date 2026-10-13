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
	"os"
	"syscall"
)

// syncDir is how a write makes its directory's entries durable. A test
// replaces it to see what a write does when the disk fails.
var syncDir = syncDirEntries

// syncDirEntries flushes a directory's entries to disk, so a rename into it
// survives a power loss. A filesystem that cannot sync a directory, as some
// network, FUSE and container mounts cannot, says so with EINVAL, ENOTSUP,
// ENOSYS or ENOTTY, and that is as far as such a filesystem goes, not a
// failure. Any other error is returned: the entry may not be on disk.
func syncDirEntries(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	err = d.Sync()
	if err == nil || !unsupported(err) {
		return err
	}
	// On macOS, Sync asks for F_FULLFSYNC and falls back to plain fsync only
	// for ENOTSUP, so a filesystem that refuses F_FULLFSYNC on a directory
	// another way may still take a plain fsync. A real error, such as EIO,
	// is never retried: on Linux a second fsync after one can succeed though
	// the data was lost.
	if ferr := syscall.Fsync(int(d.Fd())); ferr != nil && !unsupported(ferr) {
		return ferr
	}
	return nil
}

func unsupported(err error) bool {
	for _, no := range []syscall.Errno{syscall.EINVAL, syscall.ENOTSUP, syscall.EOPNOTSUPP, syscall.ENOSYS, syscall.ENOTTY} {
		if errors.Is(err, no) {
			return true
		}
	}
	return false
}

// SyncFile makes a file's data and metadata, such as a time set on it after
// it was written, survive a power loss, and says when it could not. It opens
// the file for reading, so a read-only file is synced as it is. A filesystem
// that cannot sync it is not an error, as for a directory.
func SyncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Sync(); err != nil && !unsupported(err) {
		return err
	}
	return nil
}
