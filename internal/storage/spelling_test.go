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

package storage_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// folds reports whether the file system under dir answers to other, a
// different spelling of name, as one that folds case or normalization does.
func folds(t *testing.T, dir, name, other string) bool {
	t.Helper()
	probe := filepath.Join(dir, "probe")
	check(t, os.Mkdir(probe, 0o755))
	defer os.RemoveAll(probe)
	check(t, os.Mkdir(filepath.Join(probe, name), 0o755))
	_, err := os.Lstat(filepath.Join(probe, other))
	return err == nil
}

// Two spellings pull keeps apart must never land in one directory. Where the
// file system folds them, the second is refused; where it does not, both
// directories exist apart.
func TestAncestorsRefuseAnotherSpelling(t *testing.T) {
	for _, pair := range [][2]string{{"Main", "main"}, {"\u00e9", "e\u0301"}} {
		root := t.TempDir()
		aliased := folds(t, root, pair[0], pair[1])
		t.Logf("%q and %q are one name here: %v", pair[0], pair[1], aliased)
		check(t, storage.MakeAncestorsDurable(root, filepath.Join(root, "streams", pair[0]), 0o755, nil))
		err := storage.MakeAncestorsDurable(root, filepath.Join(root, "streams", pair[1]), 0o755, nil)
		if aliased && !errors.Is(err, storage.ErrAliased) {
			t.Fatalf("%q after %q: %v", pair[1], pair[0], err)
		}
		if !aliased && err != nil {
			t.Fatalf("%q after %q on a file system that keeps them apart: %v", pair[1], pair[0], err)
		}
	}
}

func TestCheckSpellings(t *testing.T) {
	dir := t.TempDir()
	check(t, os.WriteFile(filepath.Join(dir, "a.sd"), nil, 0o444))
	check(t, os.WriteFile(filepath.Join(dir, "b.sd"), nil, 0o444))
	check(t, storage.CheckSpellings(dir, []string{"a.sd", "b.sd"}))
	err := storage.CheckSpellings(dir, []string{"a.sd", "A.sd"})
	if folds(t, dir, "x", "X") {
		if !errors.Is(err, storage.ErrAliased) {
			t.Fatalf("another spelling of a.sd: %v", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a missing name: %v", err)
	}
}
