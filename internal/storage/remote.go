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
)

// RemoteDir is where a storage root keeps the copies it pulled from remote asz
// servers, one directory per remote. The name starts with an underscore, so
// every listing of the root's sessions passes over it, and no pass over the
// root reads, sends or removes anything of a copy.
const RemoteDir = "_remote"

// CopyMarker marks a directory that holds copies pulled from a remote: a
// conversation's directory under RemoteDir, and every snapshot in it. Copied
// files keep the remote's session ids and sequence numbers, which must never
// meet a root that collects. So every command that writes refuses such a
// directory, apart from the pull, which writes the copies, and the judge,
// which adds its runs to a snapshot.
const CopyMarker = "_copy"

// ErrCopy means a command that writes was pointed at a copy.
var ErrCopy = errors.New("storage: the directory holds copies pulled from a remote, which only asz pull and asz judge write")

// IsCopy reports whether dir holds the copy marker. A marker that cannot be
// checked is an error, never taken for none, as a retirement marker is.
func IsCopy(dir string) (bool, error) {
	_, err := os.Lstat(filepath.Join(dir, CopyMarker))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// RefuseCopy returns ErrCopy, naming dir, when dir holds the copy marker.
// A command that writes calls it at its start, before it opens a listener,
// starts a receiver or runs a pass.
func RefuseCopy(dir string) error {
	copied, err := IsCopy(dir)
	if err != nil {
		return fmt.Errorf("storage: cannot tell whether %s holds copies: %w", dir, err)
	}
	if copied {
		return fmt.Errorf("%w: %s", ErrCopy, dir)
	}
	return nil
}
