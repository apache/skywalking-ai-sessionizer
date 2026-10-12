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
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// A check holds each root it builds for the whole of its run. A command
// that holds one already is one a person started over it, and the check
// stops before it writes anything there.
func TestACheckHoldsTheRootItBuilds(t *testing.T) {
	out := t.TempDir()
	root := filepath.Join(out, string(scenario.FormatSD))
	held, err := storage.LockRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Unlock()
	_, err = Check(filepath.Join("..", "..", "..", "tests", "scenarios", "fixture.yaml"),
		Options{Formats: []scenario.Format{scenario.FormatSD}, Out: out})
	if err == nil || !strings.Contains(err.Error(), "scenario check") {
		t.Fatalf("a check over a held root returned %v", err)
	}
	if sessions, _ := filepath.Glob(filepath.Join(root, "*", "session.state")); len(sessions) != 0 {
		t.Fatalf("a check over a held root wrote %v", sessions)
	}
}

// A check copies a root while it holds that root's lock. The copy leaves
// every lock file out: none carries anything, and on Windows a held one
// cannot be opened for reading.
func TestACopyOfAHeldRootLeavesTheLocksOut(t *testing.T) {
	from, to := t.TempDir(), filepath.Join(t.TempDir(), "copy")
	held, err := storage.LockRoot(from)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Unlock()
	data := filepath.Join(from, "sess1", "streams", "main", "f.sd")
	if err := os.MkdirAll(filepath.Dir(data), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(data, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := copyRoot(from, to); err != nil {
		t.Fatalf("copying a held root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(to, "sess1", "streams", "main", "f.sd")); err != nil {
		t.Fatalf("the copy lost a file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(to, storage.RootLockDir, ".lock")); !os.IsNotExist(err) {
		t.Fatalf("the copy carried the root's lock file: %v", err)
	}
}
