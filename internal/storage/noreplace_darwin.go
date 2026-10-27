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

//go:build darwin

package storage

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplaceAt moves a file or a directory between two bound
// directories and refuses an existing destination in the same call. exFAT
// returns ENOTSUP for RENAME_EXCL (internal/storage/exclusive_darwin.go), and
// only such an answer lets the caller fall back to an absence check under the
// lock that owns the destination.
func renameNoReplaceAt(from *dirHandle, name string, to *dirHandle, toName, fromPath, toPath string) error {
	err := unix.RenameatxNp(from.fd, name, to.fd, toName, unix.RENAME_EXCL)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EEXIST):
		return existsError(toPath)
	case errors.Is(err, unix.ENOSYS), errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EOPNOTSUPP):
		return ErrNoReplaceUnsupported
	}
	return &os.LinkError{Op: "rename", Old: fromPath, New: toPath, Err: err}
}
