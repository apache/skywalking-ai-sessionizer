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
)

// OpenEvidence opens a file an inventory found, for reading. It never follows
// a link at the file's own name, and the file it opens must be the regular
// file the inventory described, by identity: a name replaced since, by a
// link or another file, is refused rather than read.
func OpenEvidence(path string, want fs.FileInfo) (*os.File, error) {
	f, err := openNoFollow(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	switch {
	case err != nil:
	case !info.Mode().IsRegular():
		err = fmt.Errorf("%w: %s is not a regular file", ErrProtected, path)
	case want != nil && !os.SameFile(info, want):
		err = fmt.Errorf("%w: %s changed since it was listed", ErrProtected, path)
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}
