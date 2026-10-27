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

//go:build linux || darwin

package storage_test

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// A session retirement or a snapshot removal deletes a tree that can hold a
// hard link a pulled snapshot shares. The surviving name keeps its read-only
// mode and its bytes: a mode changed through the deleted name would make the
// frozen snapshot writable.
func TestDeleteTreeKeepsASharedFilesMode(t *testing.T) {
	base := t.TempDir()
	snapshot := filepath.Join(base, "snapshot", "a.sd")
	if err := storage.WriteAtomic(snapshot, storage.PermLanded, func(w io.Writer) error {
		_, err := io.WriteString(w, "frozen\n")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "S")
	sessionTree(t, dir)
	if err := os.Link(snapshot, filepath.Join(dir, "streams", "shared.sd")); err != nil {
		t.Fatal(err)
	}
	if err := storage.DeleteTree(dir, nil); err != nil {
		t.Fatal(err)
	}
	if !gone(t, dir) {
		t.Fatalf("%s is still there", dir)
	}
	info, err := os.Stat(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != storage.PermLanded {
		t.Fatalf("deleting the other name changed the shared file's mode to %v", info.Mode().Perm())
	}
	requireText(t, snapshot, "frozen\n")
}
