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

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/pkg/changes"
	"github.com/apache/skywalking-ai-sessionizer/plugins/claude-code/internal/hook"
)

// TestAnotherRuntimeCanDriveThis carries one tool call in the neutral
// vocabulary, from a runtime that is not Claude Code: a tool with no shell
// command, a name nobody could have listed in advance, and no
// CLAUDE_PLUGIN_DATA anywhere.
//
// Each of the three things it needs was a wall before: a tool with no command
// was classified read-only and skipped, its name was not in the scope list and
// could not be, and the data directory could only be named by a Claude Code
// variable.
func TestAnotherRuntimeCanDriveThis(t *testing.T) {
	data, work := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(data, "settings.yaml"),
		[]byte("tools:\n  scope: [\"*\"]\n  exclude: [\"re:^(read|get|list)_\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "notes.txt"), []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASZ_CHANGES_DATA", data)

	event := func(name, tool string) string {
		payload, _ := json.Marshal(map[string]any{
			"hook_event_name": name, "session_id": "ls-a-thread", "cwd": work,
			"tool_name": tool, "tool_use_id": "call_write_report_5",
			"tool_input": map[string]any{"cluster": "prod-1"},
		})
		return string(payload)
	}
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	if code := runHook(strings.NewReader(event("tool.begin", "write_report")), data, "", now); code != 0 {
		t.Fatalf("tool.begin exited %d", code)
	}
	if err := os.WriteFile(filepath.Join(work, "findings.md"), []byte("# found\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := runHook(strings.NewReader(event("tool.end", "write_report")), data,
		"", now.Add(time.Second)); code != 0 {
		t.Fatalf("tool.end exited %d", code)
	}

	record := lastRecord(t, data)
	if record.Basis != "tool_window" {
		t.Fatalf("basis %q: a tool with no shell command was not observed", record.Basis)
	}
	if record.Tool != "call_write_report_5" {
		t.Errorf("the record names %q, not the call it answers", record.Tool)
	}
	if record.ChangedFiles == nil || *record.ChangedFiles == 0 {
		t.Fatal("nothing was recorded as changed")
	}
	var added bool
	for _, c := range record.Changes {
		if strings.HasSuffix(c.Path, "findings.md") {
			added = true
		}
	}
	if !added {
		t.Error("the file the tool wrote is not in the record")
	}

	// And a tool the exclusions name is left alone, which is what makes the
	// wildcard usable for a runtime whose tools nobody can list.
	if code := runHook(strings.NewReader(event("tool.begin", "read_status")), data,
		"", now.Add(2*time.Second)); code != 0 {
		t.Fatalf("excluded tool.begin exited %d", code)
	}
	if err := os.WriteFile(filepath.Join(work, "ignored.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := runHook(strings.NewReader(event("tool.end", "read_status")), data,
		"", now.Add(3*time.Second)); code != 0 {
		t.Fatalf("excluded tool.end exited %d", code)
	}
	if got := lastRecord(t, data); got.Tool == "call_write_report_5" {
		return // nothing new was written, which is the point
	} else if got.Basis == "tool_window" && got.ToolName == "read_status" {
		t.Error("an excluded tool was scanned")
	}
}

// TestNeutralNamesAreTheSameEvents keeps the two vocabularies in step.
func TestNeutralNamesAreTheSameEvents(t *testing.T) {
	for neutral, want := range map[string]string{
		"tool.begin": hook.PreToolUse, "tool.end": hook.PostToolUse,
		"tool.failed":   hook.PostToolUseFailure,
		"session.begin": hook.SessionStart, "session.end": hook.SessionEnd,
	} {
		in, err := hook.Read(strings.NewReader(`{"hook_event_name":"` + neutral + `"}`))
		if err != nil {
			t.Fatalf("%s: %v", neutral, err)
		}
		if in.Event != want {
			t.Errorf("%s read as %q, want %q", neutral, in.Event, want)
		}
		if got := hook.Neutral(want); got != neutral {
			t.Errorf("%s renders back as %q", want, got)
		}
	}
}

func lastRecord(t *testing.T, data string) changes.Record {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(data, "output", "*", "*.jsonl"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("no output under %s: %v", data, err)
	}
	body, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	var record changes.Record
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &record); err != nil {
		t.Fatal(err)
	}
	return record
}
