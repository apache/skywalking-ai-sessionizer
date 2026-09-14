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
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// Keep the move on this volume, without either replacement or a copy fallback.
// Removing a temporary hard link instead would clear the shared read-only
// attribute on Windows, leaving the published file writable.
func installExclusive(from, to string) error {
	old, err := exclusiveWindowsPath(from)
	if err != nil {
		return err
	}
	next, err := exclusiveWindowsPath(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(old, next, windows.MOVEFILE_WRITE_THROUGH)
}

// os.CreateTemp accepts long paths too. Preserve that support when calling the
// Windows API directly, including a root on a network share.
func exclusiveWindowsPath(path string) (*uint16, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(path, `\\?\`) {
		if strings.HasPrefix(path, `\\`) {
			path = `\\?\UNC\` + path[2:]
		} else {
			path = `\\?\` + path
		}
	}
	return windows.UTF16PtrFromString(path)
}
