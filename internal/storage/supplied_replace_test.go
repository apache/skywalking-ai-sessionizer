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
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// A supplied file is replaced only in the directory its check resolved: an
// ancestor swapped for a link after the check leads the bind elsewhere, and
// the write is refused, with nothing written where the link leads.
func TestASuppliedFileIsReplacedOnlyWhereItsCheckLed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("links need privileges on Windows")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"safe/sub", "elsewhere/sub"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(base, "safe", "sub", "tokens")
	resolved, err := storage.CheckSuppliedPath(file, "")
	if err != nil {
		t.Fatal(err)
	}
	write := func(w io.Writer) error {
		_, err := io.WriteString(w, "prod\n")
		return err
	}
	if err := storage.ReplaceSuppliedDurable(resolved, 0o600, write, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(base, "safe"), filepath.Join(base, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "elsewhere"), filepath.Join(base, "safe")); err != nil {
		t.Fatal(err)
	}
	if err := storage.ReplaceSuppliedDurable(resolved, 0o600, write, nil); !errors.Is(err, storage.ErrProtected) {
		t.Fatalf("a parent that leads elsewhere: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "elsewhere", "sub", "tokens")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a file was written where the link leads: %v", err)
	}
}

// swapsAfterReplace swaps an ancestor for a link once the replacement is
// done, and records the directories synced after, by path or by handle.
type swapsAfterReplace struct {
	storage.DiskFileSystem
	swap   func()
	byPath []string
	bound  []string
}

func (s *swapsAfterReplace) ReplaceIn(dir *storage.Dir, from, to string) error {
	err := s.DiskFileSystem.ReplaceIn(dir, from, to)
	s.swap()
	return err
}

func (s *swapsAfterReplace) SyncDir(path string) error {
	s.byPath = append(s.byPath, path)
	return s.DiskFileSystem.SyncDir(path)
}

func (s *swapsAfterReplace) SyncDirIn(dir *storage.Dir) error {
	s.bound = append(s.bound, dir.Real())
	return s.DiskFileSystem.SyncDirIn(dir)
}

// The directory a supplied file was replaced in is the one synced, through
// the handle the replacement used, even when an ancestor is swapped for a
// link between the replacement and the sync.
func TestASuppliedFilesDirectoryIsSyncedThroughItsHandle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("links need privileges on Windows")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"safe/sub", "elsewhere/sub"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	resolved, err := storage.CheckSuppliedPath(filepath.Join(base, "safe", "sub", "tokens"), "")
	if err != nil {
		t.Fatal(err)
	}
	files := &swapsAfterReplace{swap: func() {
		if err := os.Rename(filepath.Join(base, "safe"), filepath.Join(base, "moved")); err != nil {
			t.Error(err)
		}
		if err := os.Symlink(filepath.Join(base, "elsewhere"), filepath.Join(base, "safe")); err != nil {
			t.Error(err)
		}
	}}
	if err := storage.ReplaceSuppliedDurable(resolved, 0o600, func(w io.Writer) error {
		_, err := io.WriteString(w, "prod\n")
		return err
	}, files); err != nil {
		t.Fatal(err)
	}
	if len(files.byPath) != 0 || len(files.bound) != 1 || files.bound[0] != filepath.Join(base, "safe", "sub") {
		t.Fatalf("synced by path %v, by handle %v", files.byPath, files.bound)
	}
	if _, err := os.Stat(filepath.Join(base, "moved", "sub", "tokens")); err != nil {
		t.Fatal(err)
	}
}
