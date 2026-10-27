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
	"os"

	"golang.org/x/sys/windows"
)

// OpenShared opens a file for reading with read, write and delete sharing.
// Delete sharing lets another process replace a control record through a
// rooted rename while this handle is open; the reader finishes the old file.
// Ordinary Go opens omit it (src/syscall/syscall_windows.go:395). It does not
// let an ancestor directory be renamed: an open file can still stop that, so
// a reader of live evidence closes its handle as soon as it has copied the
// file. Lock files keep their exclusive open.
func OpenShared(path string) (*os.File, error) {
	name, err := exclusiveWindowsPath(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}

func controlRenameError(err error) error {
	for _, code := range []error{windows.ERROR_SHARING_VIOLATION, windows.ERROR_LOCK_VIOLATION, windows.ERROR_ACCESS_DENIED} {
		if errors.Is(err, code) {
			return &ControlBusyError{Err: err}
		}
	}
	return err
}
