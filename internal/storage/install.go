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
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
)

// ErrNoReplaceUnsupported says the platform or the file system has no rename
// that refuses an existing destination in the same call.
var ErrNoReplaceUnsupported = errors.New("storage: this file system has no rename that refuses an existing destination")

// ErrChecked says copied bytes do not have the stored size or SHA-256 they were
// copied for.
var ErrChecked = errors.New("storage: copied bytes differ from their stored size or SHA-256")

func existsError(path string) error { return fmt.Errorf("%w: %s", ErrExists, path) }

// InstallNoReplace moves a file or a directory to to, and never deletes or
// replaces anything already there, not even an empty file a crashed writer
// left. RenameExclusive repairs such a file (internal/storage/rename.go), so
// pull installation does not use it.
//
// Where no native call refuses an existing destination, the absence check and
// the rename are two calls. The caller must then hold the lock that owns to's
// directory, which is why every pull installation happens under one.
func InstallNoReplace(from, to string, files FileSystem) error {
	files = writeFileSystem(files)
	err := files.RenameNoReplace(from, to)
	if !errors.Is(err, ErrNoReplaceUnsupported) {
		return err
	}
	absent := inParent(to, func(d *Dir, name string) error {
		if _, err := d.lstat(name); err == nil {
			return existsError(to)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	})
	if absent != nil {
		return absent
	}
	return files.Rename(from, to)
}

// WriteChecked writes a private temporary copy of evidence. The file is created
// exclusively and checked against its stored size and SHA-256 while it is
// written. It is synced, made read-only and synced again, so nothing can see a
// partial or writable file once InstallFile gives it its name.
func WriteChecked(temp string, r io.Reader, size int64, sum [sha256.Size]byte, files FileSystem) (err error) {
	files = writeFileSystem(files)
	file, err := files.Create(temp, 0o600)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if err == nil {
			return
		}
		if !closed {
			err = errors.Join(err, file.Close())
		}
		err = errors.Join(err, files.Remove(temp))
	}()
	digest := sha256.New()
	// One byte past the stored size is enough to see that the source is longer.
	written, err := io.Copy(io.MultiWriter(file, digest), io.LimitReader(r, size+1))
	if err != nil {
		return err
	}
	if written != size || !bytes.Equal(digest.Sum(nil), sum[:]) {
		return fmt.Errorf("%w: %s", ErrChecked, temp)
	}
	if err = file.Sync(); err != nil {
		return err
	}
	closed = true
	if err = file.Close(); err != nil {
		return err
	}
	if err = files.Chmod(temp, 0o444); err != nil {
		return err
	}
	return files.SyncFile(temp)
}

// InstallFile gives a checked temporary its final name without replacement,
// then syncs the installed file and both rename parents. A later step may
// depend on the file only once all three syncs have succeeded. A caller that
// installs a directory's files confirms their spellings together with
// CheckSpellings once they are all in place.
func InstallFile(temp, dest string, files FileSystem) error {
	files = writeFileSystem(files)
	if err := InstallNoReplace(temp, dest, files); err != nil {
		return err
	}
	if err := files.SyncFile(dest); err != nil {
		return err
	}
	return SyncRenameParents(temp, dest, files)
}

// CopyChecked copies src to dest through the private temporary temp, checked
// against src's stored size and SHA-256.
func CopyChecked(src, temp, dest string, size int64, sum [sha256.Size]byte, files FileSystem) error {
	in, err := OpenShared(src)
	if err != nil {
		return err
	}
	err = WriteChecked(temp, in, size, sum, files)
	// The source handle closes before installation: on Windows a live source
	// held open would stop retention from renaming its session away.
	if closeErr := in.Close(); err == nil && closeErr != nil {
		_ = writeFileSystem(files).Remove(temp)
		return closeErr
	}
	if err != nil {
		return err
	}
	return InstallFile(temp, dest, files)
}

// ShareFile gives dest the bytes src holds, which a published snapshot or the
// live source keeps with the given stored size and SHA-256. On Linux and macOS
// the two names share one file through a hard link. Elsewhere, or where a link
// is unavailable, dest gets a checked copy made through temp. It reports
// whether it copied, so the caller can say once per conversation that copies
// take a snapshot's whole size.
func ShareFile(src, dest, temp string, size int64, sum [sha256.Size]byte, files FileSystem) (copied bool, err error) {
	files = writeFileSystem(files)
	if sharesLinks {
		err := files.Link(src, dest)
		if err == nil {
			return false, files.SyncDir(filepath.Dir(dest))
		}
		if errors.Is(err, fs.ErrExist) {
			return false, existsError(dest)
		}
		unavailable, probeErr := linkUnavailable(err, filepath.Dir(dest), files)
		if probeErr != nil {
			return false, errors.Join(err, probeErr)
		}
		if !unavailable {
			return false, err
		}
	}
	return true, CopyChecked(src, temp, dest, size, sum, files)
}

// PublishDir gives a complete directory its final name without replacement and
// syncs both rename parents. The caller closes every handle inside the tree
// first: Windows refuses to rename a directory holding an open file.
func PublishDir(from, to string, files FileSystem) error {
	files = writeFileSystem(files)
	if err := InstallNoReplace(from, to, files); err != nil {
		return err
	}
	return SyncRenameParents(from, to, files)
}
