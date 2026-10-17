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
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// A session is retired when its marker is there. A root with no _retired
// retires nothing, a temporary file left by an interrupted write is no
// marker, and a marker that cannot be looked at is an error rather than
// taken for none.
func TestARetiredSessionIsKnownByItsMarker(t *testing.T) {
	root := t.TempDir()
	z := storage.NewZone(root)
	if retired, err := storage.Retired(z, "s1"); err != nil || retired {
		t.Fatalf("a root with no %s says %v, %v", storage.RetiredDir, retired, err)
	}
	if ids, err := storage.RetiredSessions(z); err != nil || len(ids) != 0 {
		t.Fatalf("a root with no %s lists %v, %v", storage.RetiredDir, ids, err)
	}
	dir := filepath.Join(root, storage.RetiredDir)
	if err := os.MkdirAll(filepath.Join(dir, "a-directory"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"s1", ".tmp-123"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o444); err != nil {
			t.Fatal(err)
		}
	}
	if retired, err := storage.Retired(z, "s1"); err != nil || !retired {
		t.Fatalf("s1 with its marker says %v, %v", retired, err)
	}
	if retired, err := storage.Retired(z, "s2"); err != nil || retired {
		t.Fatalf("s2 with no marker says %v, %v", retired, err)
	}
	if ids, err := storage.RetiredSessions(z); err != nil || strings.Join(ids, ",") != "s1" {
		t.Fatalf("lists %v, %v; want s1 alone", ids, err)
	}

	if runtime.GOOS == "windows" {
		return // A file where a directory should be reads as not there.
	}
	other := storage.NewZone(t.TempDir())
	if err := os.WriteFile(filepath.Join(other.Root(), storage.RetiredDir), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Retired(other, "s1"); err == nil {
		t.Fatal("a marker that cannot be looked at was taken for none")
	}
}
