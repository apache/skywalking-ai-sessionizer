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

// ErrAliased says a file system holds a name under another spelling than the
// one asked for: it folds letter case or Unicode normalization, so two names
// pull keeps apart would be one file there.
var ErrAliased = errors.New("storage: the file system holds the name under another spelling")

// CheckSpellings confirms that dir holds each of names under that exact
// spelling, byte for byte, reading the directory once. A name the directory
// answers to under another spelling is ErrAliased. A caller that installs
// many files into one directory checks them together once they are all in
// place, rather than reading the directory back after each.
func CheckSpellings(dir string, names []string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	held := make(map[string]bool, len(entries))
	for _, entry := range entries {
		held[entry.Name()] = true
	}
	for _, name := range names {
		if held[name] {
			continue
		}
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			return fmt.Errorf("%w: %s in %s", ErrAliased, name, dir)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return fmt.Errorf("storage: %s is not in %s: %w", name, dir, fs.ErrNotExist)
	}
	return nil
}
