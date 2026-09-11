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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/pkg/changes"
	"github.com/apache/skywalking-ai-sessionizer/plugins/claude-code/internal/output"
)

// hookInput is what Claude Code writes to a hook, in the shape a run of
// version 2.1.260 was seen to write.
func hookInput(event, session, agent, tool, toolUse, input, response, errText string) string {
	agentField := ""
	if agent != "" {
		agentField = fmt.Sprintf(`"agent_id":%q,"agent_type":"general-purpose",`, agent)
	}
	extra := ""
	if response != "" {
		extra = `,"tool_response":` + response
	}
	if errText != "" {
		extra = fmt.Sprintf(`,"error":%q,"is_interrupt":false`, errText)
	}
	return fmt.Sprintf(`{"hook_event_name":%q,"session_id":%q,%s"cwd":"/w","prompt_id":"p1","tool_name":%q,"tool_use_id":%q,"tool_input":%s%s}`,
		event, session, agentField, tool, toolUse, input, extra)
}

func readLines(t *testing.T, path string) []*changes.Record {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	var out []*changes.Record
	for _, l := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		r, ok := changes.Decode([]byte(l))
		if !ok {
			t.Fatalf("not a record: %s", l)
		}
		out = append(out, r)
	}
	return out
}

// TestAShellCommandIsObservedEndToEnd runs the hook as Claude Code would:
// a BEFORE event, the tool's writes, an AFTER event, and the record on
// disk, with the plugin's data directory and the project directory where
// the environment would put them.
func TestAShellCommandIsObservedEndToEnd(t *testing.T) {
	data := t.TempDir()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "server.go"), []byte("package main\n\nvar timeout = 10\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "node_modules", "dep"), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	const session = "11111111-2222-4333-8444-555555555555"

	// A read-only command skips the scans and still leaves a record.
	runHook(strings.NewReader(hookInput("PreToolUse", session, "", "Bash", "toolu_ro", `{"command":"ls -la | head"}`, "", "")), data, ws, now)
	runHook(strings.NewReader(hookInput("PostToolUse", session, "", "Bash", "toolu_ro", `{"command":"ls -la | head"}`, `{"stdout":"","stderr":""}`, "")), data, ws, now.Add(time.Second))
	recs := readLines(t, output.Path(data, session, "main"))
	if len(recs) != 1 || recs[0].Basis != changes.BasisSkippedReadOnly || recs[0].Tool != "toolu_ro" || len(recs[0].Changes) != 0 {
		t.Fatalf("read-only record: %+v", recs)
	}
	if _, err := os.Stat(filepath.Join(data, "roots")); err == nil {
		if roots, _ := os.ReadDir(filepath.Join(data, "roots")); len(roots) > 0 {
			if _, err := os.Stat(filepath.Join(data, "roots", roots[0].Name(), "manifest.json")); err == nil {
				t.Fatal("a read-only command scanned the root")
			}
		}
	}

	// A writing command: BEFORE scans, the tool writes, AFTER scans.
	cmd := `{"command":"sed -i 's/10/30/' server.go && printf 'x' > new.txt"}`
	runHook(strings.NewReader(hookInput("PreToolUse", session, "", "Bash", "toolu_w", cmd, "", "")), data, ws, now.Add(10*time.Second))
	if err := os.WriteFile(filepath.Join(ws, "server.go"), []byte("package main\n\nvar timeout = 30\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "new.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "node_modules", "dep", "index.js"), []byte("ignored"), 0o644); err != nil {
		t.Fatal(err)
	}
	runHook(strings.NewReader(hookInput("PostToolUseFailure", session, "", "Bash", "toolu_w", cmd, "", "Exit code 1\nsed: something")), data, ws, now.Add(20*time.Second))
	recs = readLines(t, output.Path(data, session, "main"))
	if len(recs) != 2 {
		t.Fatalf("records: %d", len(recs))
	}
	r := recs[1]
	if r.Basis != changes.BasisToolWindow || r.Tool != "toolu_w" || r.ToolName != "Bash" || r.Stream != "main" || r.Session != session {
		t.Fatalf("record: %+v", r)
	}
	if r.Outcome == nil || r.Outcome.State != changes.OutcomeFailed || r.Outcome.ExitCode == nil || *r.Outcome.ExitCode != 1 {
		t.Fatalf("outcome: %+v", r.Outcome)
	}
	if r.Policy == nil || r.Policy.Exclusions != "standard-v1" || r.Policy.ReadOnly != "readonly-v1" || len(r.Policy.Expanded) == 0 {
		t.Fatalf("policy: %+v", r.Policy)
	}
	if *r.ChangedFiles != 2 || r.Root.Path != ws || r.Coverage != changes.CoverageComplete {
		t.Fatalf("record: changed=%d root=%s coverage=%s", *r.ChangedFiles, r.Root.Path, r.Coverage)
	}
	byPath := map[string]changes.FileChange{}
	for _, c := range r.Changes {
		byPath[c.Path] = c
	}
	if c := byPath["server.go"]; c.Operation != changes.OpModify || len(c.Hunks) != 1 || c.Attribution != changes.AttributionOnlyThisWindow ||
		strings.Join(c.Hunks[0].Lines, "\n") != " package main\n \n-var timeout = 10\n+var timeout = 30" {
		t.Fatalf("server.go: %+v", c)
	}
	if c := byPath["new.txt"]; c.Operation != changes.OpCreate || !c.After.NoNewlineAtEnd || *c.Additions != 1 {
		t.Fatalf("new.txt: %+v", c)
	}
	if _, ok := byPath["node_modules/dep/index.js"]; ok {
		t.Fatal("an excluded path was observed")
	}

	// A change made by nobody between two calls lands unattributed.
	if err := os.WriteFile(filepath.Join(ws, "notes.md"), []byte("by hand\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd2 := `{"command":"go build ./..."}`
	runHook(strings.NewReader(hookInput("PreToolUse", session, "a0123456789abcdef", "Bash", "toolu_2", cmd2, "", "")), data, ws, now.Add(30*time.Second))
	runHook(strings.NewReader(hookInput("PostToolUse", session, "a0123456789abcdef", "Bash", "toolu_2", cmd2, `{"stdout":"","stderr":""}`, "")), data, ws, now.Add(40*time.Second))
	agentRecs := readLines(t, output.Path(data, session, "a0123456789abcdef"))
	if len(agentRecs) != 2 {
		t.Fatalf("agent stream records: %d", len(agentRecs))
	}
	if gap := agentRecs[0]; gap.Basis != changes.BasisUnattributed || gap.Tool != "" || len(gap.Changes) != 1 || gap.Changes[0].Path != "notes.md" || gap.Changes[0].Attribution != changes.AttributionOutsideAnyWindow {
		t.Fatalf("gap record: %+v", gap)
	}
	if w := agentRecs[1]; w.Tool != "toolu_2" || *w.ChangedFiles != 0 || w.Stream != "a0123456789abcdef" {
		t.Fatalf("second window: %+v", w)
	}
}

// TestAnEditInsideASubagentComesFromTheResponse: the runtime records no
// patch in a subagent's transcript, so the plugin takes it from the hook.
func TestAnEditInsideASubagentComesFromTheResponse(t *testing.T) {
	data := t.TempDir()
	ws := t.TempDir()
	const session = "11111111-2222-4333-8444-555555555555"
	resp := `{"filePath":` + jsonString(filepath.Join(ws, "poc.txt")) + `,"oldString":"alpha","newString":"beta","originalFile":"line one alpha\nline two\n","structuredPatch":[{"oldStart":1,"oldLines":2,"newStart":1,"newLines":2,"lines":["-line one alpha","+line one beta"," line two"]}],"userModified":false,"replaceAll":false}`
	now := time.Now()
	runHook(strings.NewReader(hookInput("PostToolUse", session, "a0123456789abcdef", "Edit", "toolu_e", `{"file_path":"x"}`, resp, "")), data, ws, now)
	recs := readLines(t, output.Path(data, session, "a0123456789abcdef"))
	if len(recs) != 1 || recs[0].Basis != changes.BasisRuntimeReported || recs[0].Changes[0].Path != "poc.txt" {
		t.Fatalf("records: %+v", recs)
	}
	// On the main stream the runtime records the patch itself; the plugin
	// writes nothing.
	runHook(strings.NewReader(hookInput("PostToolUse", session, "", "Edit", "toolu_m", `{"file_path":"x"}`, resp, "")), data, ws, now)
	if _, err := os.Stat(output.Path(data, session, "main")); !os.IsNotExist(err) {
		t.Fatal("a main-stream edit was recorded by the plugin")
	}
}

// TestAnEditIsNeverFoundAsNobodysChange: the runtime records an edit on
// the main stream and the plugin records one inside a subagent, and in
// both cases the plugin's manifest must learn what was written, or the
// next scan would find the edit as a change nobody made.
func TestAnEditIsNeverFoundAsNobodysChange(t *testing.T) {
	data := t.TempDir()
	ws := t.TempDir()
	const session = "11111111-2222-4333-8444-555555555555"
	now := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(ws, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("poc.txt", "line one alpha\nline two\n")
	// A first scanned window establishes the manifest. The command must
	// not be one the classifier marks read-only, or nothing is scanned.
	cmd := `{"command":"make build"}`
	runHook(strings.NewReader(hookInput("PreToolUse", session, "", "Bash", "toolu_0", cmd, "", "")), data, ws, now)
	runHook(strings.NewReader(hookInput("PostToolUse", session, "", "Bash", "toolu_0", cmd, `{"stdout":"","stderr":""}`, "")), data, ws, now.Add(2*time.Second))

	// The runtime's own Edit on the main stream: the file changes, the
	// hook reports it, the plugin writes no record and learns the file.
	write("poc.txt", "line one beta\nline two\n")
	resp := `{"filePath":` + jsonString(filepath.Join(ws, "poc.txt")) + `,"oldString":"alpha","newString":"beta","originalFile":"line one alpha\nline two\n","structuredPatch":[],"userModified":false,"replaceAll":false}`
	runHook(strings.NewReader(hookInput("PostToolUse", session, "", "Edit", "toolu_e", `{"file_path":"x"}`, resp, "")), data, ws, now.Add(4*time.Second))
	// A Write inside a subagent: recorded from the response, and learned.
	write("sub.txt", "from subagent")
	wresp := `{"type":"create","filePath":` + jsonString(filepath.Join(ws, "sub.txt")) + `,"content":"from subagent","structuredPatch":[],"originalFile":null,"userModified":false}`
	runHook(strings.NewReader(hookInput("PostToolUse", session, "a0123456789abcdef", "Write", "toolu_w", `{"file_path":"x"}`, wresp, "")), data, ws, now.Add(6*time.Second))

	// The next scanned window finds nothing nobody made.
	runHook(strings.NewReader(hookInput("PreToolUse", session, "", "Bash", "toolu_1", cmd, "", "")), data, ws, now.Add(8*time.Second))
	runHook(strings.NewReader(hookInput("PostToolUse", session, "", "Bash", "toolu_1", cmd, `{"stdout":"","stderr":""}`, "")), data, ws, now.Add(10*time.Second))
	for _, r := range readLines(t, output.Path(data, session, "main")) {
		if r.Basis == changes.BasisUnattributed {
			t.Fatalf("an edit the runtime recorded was found as nobody's: %+v", r)
		}
		if r.Tool == "toolu_1" && (r.ChangedFiles == nil || *r.ChangedFiles != 0) {
			t.Fatalf("the later window saw the edits as its own: %+v", r.Changes)
		}
	}
	agent := readLines(t, output.Path(data, session, "a0123456789abcdef"))
	if len(agent) != 1 || agent[0].Tool != "toolu_w" {
		t.Fatalf("subagent records: %+v", agent)
	}

	// An edit made while a window is open is shared with that window and
	// named on it, not taken as the window's own.
	runHook(strings.NewReader(hookInput("PreToolUse", session, "a0123456789abcdef", "Bash", "toolu_2", cmd, "", "")), data, ws, now.Add(12*time.Second))
	write("poc.txt", "line one gamma\nline two\n")
	resp2 := `{"filePath":` + jsonString(filepath.Join(ws, "poc.txt")) + `,"oldString":"beta","newString":"gamma","originalFile":"line one beta\nline two\n","structuredPatch":[],"userModified":false,"replaceAll":false}`
	runHook(strings.NewReader(hookInput("PostToolUse", session, "", "Edit", "toolu_e2", `{"file_path":"x"}`, resp2, "")), data, ws, now.Add(14*time.Second))
	runHook(strings.NewReader(hookInput("PostToolUse", session, "a0123456789abcdef", "Bash", "toolu_2", cmd, `{"stdout":"","stderr":""}`, "")), data, ws, now.Add(16*time.Second))
	agent = readLines(t, output.Path(data, session, "a0123456789abcdef"))
	last := agent[len(agent)-1]
	if last.Tool != "toolu_2" || len(last.Changes) != 1 || last.Changes[0].Attribution != changes.AttributionShared || len(last.Changes[0].Windows) != 2 || len(last.Overlaps) != 1 || last.Overlaps[0].Tool != "toolu_e2" {
		t.Fatalf("window with a concurrent edit: %+v overlaps %+v", last.Changes, last.Overlaps)
	}
}

// TestIdleRootsDropTheirBytesAndKeepTheirManifest: the retention rule for
// snapshot state, run at a session's end, and the one for output files.
func TestIdleRootsDropTheirBytesAndKeepTheirManifest(t *testing.T) {
	data := t.TempDir()
	ws := t.TempDir()
	const session = "11111111-2222-4333-8444-555555555555"
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	cmd := `{"command":"make build"}`
	runHook(strings.NewReader(hookInput("PreToolUse", session, "", "Bash", "toolu_1", cmd, "", "")), data, ws, now)
	runHook(strings.NewReader(hookInput("PostToolUse", session, "", "Bash", "toolu_1", cmd, `{"stdout":"","stderr":""}`, "")), data, ws, now.Add(time.Second))
	roots, err := os.ReadDir(filepath.Join(data, "roots"))
	if err != nil || len(roots) != 1 {
		t.Fatalf("roots: %v err=%v", roots, err)
	}
	root := filepath.Join(data, "roots", roots[0].Name())
	content := func() int {
		items, _ := os.ReadDir(filepath.Join(root, "content"))
		return len(items)
	}
	if content() == 0 {
		t.Fatal("the scans kept no bytes")
	}
	// Ten minutes later the root is not idle yet.
	runHook(strings.NewReader(`{"hook_event_name":"SessionEnd","session_id":"`+session+`"}`), data, ws, now.Add(10*time.Minute))
	if content() == 0 {
		t.Fatal("bytes dropped before the idle time")
	}
	// An hour later it is: the bytes and the steps go, the manifest stays.
	runHook(strings.NewReader(`{"hook_event_name":"SessionEnd","session_id":"`+session+`"}`), data, ws, now.Add(time.Hour))
	if content() != 0 {
		t.Fatal("bytes kept past the idle time")
	}
	if steps, _ := os.ReadDir(filepath.Join(root, "steps")); len(steps) != 0 {
		t.Fatal("steps kept past the idle time")
	}
	if _, err := os.Stat(filepath.Join(root, "manifest.json")); err != nil {
		t.Fatal("the manifest went with the bytes")
	}
	// The output file outlives the idle time and goes at its TTL.
	if _, err := os.Stat(output.Path(data, session, "main")); err != nil {
		t.Fatal("the output went with the bytes")
	}
	old := now.Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(output.Path(data, session, "main"), old, old); err != nil {
		t.Fatal(err)
	}
	runHook(strings.NewReader(`{"hook_event_name":"SessionStart","session_id":"`+session+`"}`), data, ws, now.Add(2*time.Hour))
	if _, err := os.Stat(output.Path(data, session, "main")); !os.IsNotExist(err) {
		t.Fatal("an output file past its TTL is still there")
	}
}

func TestNothingBlocksTheTool(t *testing.T) {
	if code := runHook(strings.NewReader("not json"), t.TempDir(), "", time.Now()); code != 0 {
		t.Fatal("bad input changed the exit status")
	}
	if code := runHook(strings.NewReader(hookInput("PreToolUse", "s", "", "Bash", "t", `{"command":"rm x"}`, "", "")), "", "", time.Now()); code != 0 {
		t.Fatal("no data directory changed the exit status")
	}
}
