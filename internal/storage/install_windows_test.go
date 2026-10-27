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

package storage_test

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// Windows never shares a file between trees: deletion there can clear a
// read-only attribute (src/os/file_windows.go:247), and the change would reach
// the other tree. A share is a separate, read-only copy.
func TestShareFileAlwaysCopiesOnWindows(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	check(t, os.WriteFile(src, []byte("evidence"), 0o444))
	dest := filepath.Join(dir, "a.sd")
	copied, err := storage.ShareFile(src, dest, filepath.Join(dir, ".tmp-x"), 8, digestOf("evidence"), nil)
	if err != nil || !copied {
		t.Fatalf("copied %v, %v", copied, err)
	}
	// A real deletion stopped halfway: with another handle open on the
	// source that does not share delete, os.Remove clears the read-only
	// attribute and then fails to delete (src/os/file_windows.go:247), leaving
	// the source writable. The copy is another file and stays read-only.
	name, err := windows.UTF16PtrFromString(src)
	check(t, err)
	holder, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	check(t, err)
	if err := os.Remove(src); err == nil {
		t.Fatal("the source was deleted while a handle without delete sharing held it")
	}
	check(t, windows.CloseHandle(holder))
	if after, err := os.Stat(src); err != nil || after.Mode().Perm()&0o200 == 0 {
		t.Fatalf("the stopped deletion did not leave the source writable: %v, %v", after, err)
	}
	info, err := os.Stat(dest)
	check(t, err)
	if info.Mode().Perm()&0o200 != 0 {
		t.Fatalf("the copy became writable with its source: %v", info.Mode())
	}
	check(t, os.Remove(src))
	requireText(t, dest, "evidence")
	info, err = os.Stat(dest)
	check(t, err)
	if info.Mode().Perm()&0o200 != 0 {
		t.Fatalf("the copy is writable: %v", info.Mode())
	}
}

// A short read handle with delete sharing never stops another process renaming
// or deleting the file it reads, which is how a live source is read.
func TestOpenSharedAllowsRenameAndDelete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.sd")
	check(t, os.WriteFile(path, []byte("evidence"), 0o644))
	f, err := storage.OpenShared(path)
	check(t, err)
	defer f.Close()
	moved := filepath.Join(dir, "b.sd")
	check(t, os.Rename(path, moved))
	text, err := io.ReadAll(f)
	if err != nil || string(text) != "evidence" {
		t.Fatalf("reader after rename: %q, %v", text, err)
	}
	check(t, os.Remove(moved))
}

// A tree whose every handle is closed detaches into _removing in one rename.
func TestDetachAfterClosingHandles(t *testing.T) {
	dir := t.TempDir()
	removing := filepath.Join(dir, storage.RemovingDir)
	tree := filepath.Join(dir, testTag)
	mkdirs(t, removing, filepath.Join(tree, "s1"))
	file := filepath.Join(tree, "s1", "a.sd")
	check(t, os.WriteFile(file, []byte("evidence"), 0o444))
	f, err := storage.OpenShared(file)
	check(t, err)
	check(t, f.Close())
	target, err := storage.Detach(tree, removing, testTag, nil)
	check(t, err)
	requireText(t, filepath.Join(target, "s1", "a.sd"), "evidence")
	check(t, storage.DeleteOwned(removing, filepath.Base(target), nil))
}
