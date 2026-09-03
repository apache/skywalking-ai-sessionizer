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

//go:build windows

package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// errSharingViolation is ERROR_SHARING_VIOLATION, which the syscall package
// does not name.
const errSharingViolation syscall.Errno = 32

// lockDir takes an exclusive lock on the directory's lock file.
//
// Windows has no flock. Opening the file with no sharing allowed does the same
// job: every other open of it fails until this handle closes, and the system
// closes the handle if the process dies, so a crash cannot leave a session
// permanently unlockable.
func lockDir(dir string) (*SessionLock, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, ".lock")
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, errSharingViolation) {
			return nil, ErrSessionBusy
		}
		return nil, fmt.Errorf("storage: lock %s: %w", dir, err)
	}
	return &SessionLock{f: os.NewFile(uintptr(h), path)}, nil
}

// unlock has nothing to do: closing the handle releases the exclusive open.
func unlock(*os.File) error { return nil }
