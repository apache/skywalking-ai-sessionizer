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

package run

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// A copy of a root that holds a retired session is refused, as repack
// refuses it, and makes nothing: it would carry what is left of the session
// into a root without its marker, beside a live session's files.
func TestACopyOfARootWithARetiredSessionIsRefused(t *testing.T) {
	from, to := t.TempDir(), filepath.Join(t.TempDir(), "twin")
	z := storage.NewZone(from)
	for _, session := range []string{"retired", "live"} {
		dir := z.StreamDir(session, storage.StreamMain)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "transcript-x-000001.sd"), []byte("{}\n"), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(from, storage.RetiredDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(z.RetiredPath("retired"), nil, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := copyLanded(from, to); err == nil {
		t.Fatal("a root holding a retired session was copied")
	}
	if _, err := os.Stat(to); !os.IsNotExist(err) {
		t.Fatalf("the refused copy made its destination: %v", err)
	}
}

// A copy goes to a new directory only. A destination already there may hold
// the marker of the very session copied, which the copy would collect again;
// one that is there is refused, and nothing of the copy is made in it.
func TestACopyIntoARootThatIsThereIsRefused(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	dir := storage.NewZone(from).StreamDir("s1", storage.StreamMain)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "transcript-x-000001.sd"), []byte("{}\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	dst := storage.NewZone(to)
	if err := os.MkdirAll(filepath.Join(to, storage.RetiredDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst.RetiredPath("s1"), nil, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := copyLanded(from, to); err == nil {
		t.Fatal("a copy went into a root that is already there")
	}
	if _, err := os.Stat(dst.SessionDir("s1")); !os.IsNotExist(err) {
		t.Fatalf("the refused copy made the retired session again: %v", err)
	}
}
