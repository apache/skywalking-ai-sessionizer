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

package claudecodechanges_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecodechanges"
)

// TestASessionFromAnotherRuntimeIsFound holds the one thing the two halves of
// change capture share.
//
// The recorder writes under whatever session id it was told, and only Claude
// Code's is a UUID. A LangChain conversation lands under a name derived from
// its project and thread, because a supplied thread key is never safe as a
// path. If discovery does not accept that shape, the shim writes records
// nobody ever reads and the loss is silent: no error, just a conversation that
// never mentions the files it wrote.
func TestASessionFromAnotherRuntimeIsFound(t *testing.T) {
	root := t.TempDir()
	session := "ls-tsb-advisor-thread-files-1d45ca4f65bf"
	dir := filepath.Join(root, "asz-changes", "output", session)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"schema":"changes/1","id":"call_1","session":"` + session +
		`","stream":"main","tool":"call_1","time":"2026-09-20T10:00:00Z",` +
		`"basis":"tool_window","changed_files":1,"changes":[{"path":"findings.md"}]}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "main.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	sessions, err := claudecodechanges.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ID != session {
		t.Fatalf("discovered %+v, want one session %s", sessions, session)
	}
	if len(sessions[0].Sources) != 1 {
		t.Fatalf("%d sources, want 1", len(sessions[0].Sources))
	}

	// And a directory nobody wrote for a session is still left alone, so the
	// widened shape has not turned the guard off.
	for _, stray := range []string{"notes", "..", "_metrics", "ls-", "ls-nothex-zzzz"} {
		if err := os.MkdirAll(filepath.Join(root, "asz-changes", "output", stray), 0o755); err != nil {
			continue
		}
	}
	again, err := claudecodechanges.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 {
		t.Fatalf("a stray directory was taken for a session: %+v", again)
	}
}

// TestADirectlyConfiguredDataDirectoryIsFound holds the layout every runtime
// but Claude Code produces.
//
// Claude Code hands each plugin its own data directory under
// plugins/data/<plugin>-<marketplace>, so a root holds one directory per
// plugin. Anything else is told a directory outright with ASZ_CHANGES_DATA and
// writes straight into it. Reading only the first layout would mean a root
// configured the second way discovers nothing and says nothing: the recorder
// writes, no session is found, and the conversation never mentions the files
// it wrote.
func TestADirectlyConfiguredDataDirectoryIsFound(t *testing.T) {
	root := t.TempDir()
	session := "ls-tsb-advisor-a-thread-1d45ca4f65bf"
	dir := filepath.Join(root, "output", session)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"schema":"changes/1","id":"call_1","session":"` + session +
		`","stream":"main","tool":"call_1","time":"2026-09-20T10:00:00Z",` +
		`"basis":"tool_window","changed_files":1,"changes":[{"path":"findings.md"}]}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "main.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	sessions, err := claudecodechanges.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ID != session {
		t.Fatalf("discovered %+v, want one session %s", sessions, session)
	}
	if len(sessions[0].Sources) != 1 {
		t.Fatalf("%d sources, want 1", len(sessions[0].Sources))
	}
}
