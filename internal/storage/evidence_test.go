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
	"os"
	"path/filepath"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// A listed file is opened only while its name still holds the file the
// listing saw: a replacement holding the same bytes, a link and a directory
// are refused.
func TestOpenEvidenceOpensOnlyTheListedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "evidence.sd")
	if err := os.WriteFile(path, []byte("bytes"), 0o444); err != nil {
		t.Fatal(err)
	}
	listed, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := storage.OpenEvidence(path, listed)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	// The replacement is made before the file is removed: a file system may
	// give a removed file's identity to the next file made.
	next := filepath.Join(dir, "next.sd")
	if err := os.WriteFile(next, []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.OpenEvidence(path, listed); !errors.Is(err, storage.ErrProtected) {
		t.Fatalf("a replaced file: %v", err)
	}
	link := filepath.Join(dir, "link.sd")
	if err := os.Symlink(path, link); err != nil {
		t.Skip(err)
	}
	if _, err := storage.OpenEvidence(link, nil); err == nil {
		t.Fatal("a link was opened")
	}
	if _, err := storage.OpenEvidence(dir, nil); !errors.Is(err, storage.ErrProtected) {
		t.Fatalf("a directory: %v", err)
	}
}
