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

package tests_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecodechanges"
	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/langsmith"
	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// TestAToolCallsChangesReachTheSameConversation carries the file-change half
// the whole way, for a runtime that is not Claude Code.
//
// Everything in it was a wall at some point, and none of them showed up in a
// unit test: the recorder skipped a tool with no shell command, its name could
// not be listed, its data directory could only be named by a Claude Code
// variable, and discovery read only Claude Code's directory layout. Each was
// found by running the pieces together, which is what this does.
//
// The join is the point. The receiver lands the conversation under a session
// name it derives, the recorder writes change records under a session name the
// integration derives, and if those two ever differ the records go somewhere
// nobody assembles and nothing says so.
func TestAToolCallsChangesReachTheSameConversation(t *testing.T) {
	recorder := build(t)
	root := t.TempDir()
	data := filepath.Join(root, "changes")   // what ASZ_CHANGES_DATA would be
	work := filepath.Join(root, "workspace") // what ASZ_WATCH would be
	for _, d := range []string{data, work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A tool the application named, which nobody could have listed in advance.
	if err := os.WriteFile(filepath.Join(data, "settings.yaml"),
		[]byte("tools:\n  scope: [\"*\"]\n  exclude: [\"re:^read_\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The session the receiver would land this conversation under, derived the
	// way the shim derives it.
	owner, ok := langsmith.DefaultOwnership().Owner("tsb-advisor",
		map[string]any{"thread_id": "the-conversation"})
	if !ok {
		t.Fatal("no owner")
	}
	session := langsmith.StorageID(owner)

	// Each tool gets its own call id. They shared one before, so a record
	// the exclusions should have stopped was indistinguishable from the one
	// that belonged, and the test passed either way.
	const wrote = "call_write_report_5"
	const read = "call_read_status_6"
	event := func(name, tool, call string) string {
		payload, _ := json.Marshal(map[string]any{
			"hook_event_name": name, "session_id": session, "cwd": work,
			"tool_name": tool, "tool_use_id": call,
			"tool_input": map[string]any{"cluster": "prod-1"},
		})
		return string(payload)
	}
	run := func(payload string) {
		t.Helper()
		cmd := exec.Command(recorder, "hook")
		cmd.Stdin = strings.NewReader(payload)
		cmd.Env = append(os.Environ(), "ASZ_CHANGES_DATA="+data)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("recorder: %v: %s", err, out)
		}
	}

	run(event("tool.begin", "write_report", wrote))
	if err := os.WriteFile(filepath.Join(work, "findings.md"),
		[]byte("# Findings\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(event("tool.end", "write_report", wrote))

	// A tool the exclusions name changes a file and must leave no record.
	run(event("tool.begin", "read_status", read))
	if err := os.WriteFile(filepath.Join(work, "ignored.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(event("tool.end", "read_status", read))

	zone := storage.NewZone(filepath.Join(root, "data"))
	stats, err := claudecodechanges.New(data, zone, 2<<20).CollectAll(nil)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if stats.Sessions != 1 {
		t.Fatalf("%d sessions discovered, want 1 — the records went somewhere nobody reads",
			stats.Sessions)
	}

	// The records landed under the session the conversation would use, and
	// name the call they belong to.
	found, tools := 0, map[string]bool{}
	files, err := storage.LandedFiles(zone, session)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		file, err := os.Open(f.Path)
		if err != nil {
			t.Fatal(err)
		}
		reader, err := sessiondata.NewReader(file)
		if err != nil {
			t.Fatal(err)
		}
		if reader.Header().Kind != sessiondata.KindChanges {
			_ = file.Close()
			continue
		}
		for {
			record, err := reader.Next()
			if err != nil {
				break
			}
			found++
			tools[record.Tool] = true
		}
		_ = file.Close()
	}
	if found == 0 {
		t.Fatalf("no change record landed under %s", session)
	}
	if !tools[wrote] {
		t.Errorf("no record names the call it belongs to; got %v", tools)
	}
	// The exclusion has to have done something. Nothing asserted this
	// before, so the setting could have been ignored entirely - which it
	// was, until the loader was fixed to keep it.
	if tools[read] {
		t.Errorf("a tool the exclusions name recorded anyway; got %v", tools)
	}
	if mentioned(t, zone, session, "ignored.txt") {
		t.Error("the excluded tool's file is named in a landed record")
	}
	if !mentioned(t, zone, session, "findings.md") {
		t.Error("the file the recorded tool wrote is named nowhere")
	}
	t.Logf("%d change record(s) landed under %s, naming %v", found, session, tools)
}

// build compiles the recorder once for this test.
func build(t *testing.T) string {
	t.Helper()
	// Windows will not run a file with no extension, and the failure it gives
	// is that the executable was not found - which reads as a build that
	// never happened rather than a name that cannot be run.
	name := "asz-changes"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	out := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", out, "../plugins/claude-code")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build the recorder: %v: %s", err, output)
	}
	return out
}

// mentioned reports whether any landed file of the session names a path.
func mentioned(t *testing.T, zone *storage.Zone, session, name string) bool {
	t.Helper()
	files, err := storage.LandedFiles(zone, session)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		data, err := os.ReadFile(f.Path)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(name)) {
			return true
		}
	}
	return false
}

// TestThePythonShimNamesTheSessionTheReceiverDoes.
//
// The parity tables hold both sides to one rule, but they are data: they say
// what the two implementations produced, not that the shipped shim still
// produces it. This runs the installed module and compares it with the
// receiver, which is the only check that fails when the shim itself changes.
func TestThePythonShimNamesTheSessionTheReceiverDoes(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	const project = "tsb-advisor"
	const thread = "the-conversation"

	script := `
import importlib.util, json, sys
spec = importlib.util.spec_from_file_location("i", sys.argv[1])
m = importlib.util.module_from_spec(spec); spec.loader.exec_module(m)
values = m.owner_values(sys.argv[2], {"thread_id": sys.argv[3]})
print(m.storage_id(values))
`
	out, err := exec.Command(python, "-c", script,
		filepath.Join("..", "plugins", "langchain", "asz_langchain", "identity.py"),
		project, thread).Output()
	if err != nil {
		t.Fatalf("running the shim: %v", err)
	}
	fromPython := strings.TrimSpace(string(out))

	owner, ok := langsmith.DefaultOwnership().Owner(project,
		map[string]any{"thread_id": thread})
	if !ok {
		t.Fatal("the receiver found no owner")
	}
	fromGo := langsmith.StorageID(owner)
	if fromPython != fromGo {
		t.Fatalf("the two halves disagree\n  receiver %s\n  shim     %s", fromGo, fromPython)
	}
	t.Logf("both halves name it %s", fromGo)
}

// TestTheChangesAndTheConversationMeetInOneParse.
//
// The previous test proves the records land in the right directory. This one
// proves they become one conversation: the receiver lands the turn, the
// recorder lands what the tool changed, and a single parse has to produce a
// conversation whose tool step is the call the change records name.
func TestTheChangesAndTheConversationMeetInOneParse(t *testing.T) {
	recorder := build(t)
	root := t.TempDir()
	data := filepath.Join(root, "changes")
	work := filepath.Join(root, "workspace")
	for _, d := range []string{data, work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The application names its own tools, so the recorder is told which to
	// watch. Without this nothing is in scope and nothing is recorded.
	if err := os.WriteFile(filepath.Join(data, "settings.yaml"),
		[]byte("tools:\n  scope: [\"*\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	zone := storage.NewZone(filepath.Join(root, "data"))
	const call = "call_write_report_5"

	// The turn, as the client would send it.
	at := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	receiver := &langsmith.Receiver{Zone: zone, Listen: "127.0.0.1:0",
		Now: func() time.Time { at = at.Add(time.Millisecond); return at }}
	if err := receiver.Start(); err != nil {
		t.Fatal(err)
	}
	defer receiver.Stop()
	resp, err := http.Post("http://"+receiver.Addr()+"/runs/batch",
		"application/json", strings.NewReader(oneTurnWithATool(call)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("the receiver answered %d", resp.StatusCode)
	}
	collected := time.Date(2026, 9, 20, 11, 0, 0, 0, time.UTC)
	landed, err := (&langsmith.Collector{Zone: zone,
		Now: func() time.Time { collected = collected.Add(time.Second); return collected }}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(landed.Sessions) != 1 {
		t.Fatalf("the turn landed in %v, want one session", landed.Sessions)
	}
	session := landed.Sessions[0]

	// What the tool changed, through the recorder, under the session the
	// shim derives - which is the join.
	event := func(name string) string {
		payload, _ := json.Marshal(map[string]any{
			"hook_event_name": name, "session_id": session, "cwd": work,
			"tool_name": "write_report", "tool_use_id": call,
			"tool_input": map[string]any{"cluster": "prod-1"},
		})
		return string(payload)
	}
	runHook := func(payload string) {
		t.Helper()
		cmd := exec.Command(recorder, "hook")
		cmd.Stdin = strings.NewReader(payload)
		cmd.Env = append(os.Environ(), "ASZ_CHANGES_DATA="+data)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("recorder: %v: %s", err, out)
		}
	}
	runHook(event("tool.begin"))
	if err := os.WriteFile(filepath.Join(work, "findings.md"),
		[]byte("# Findings\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runHook(event("tool.end"))
	if _, err := claudecodechanges.New(data, zone, 2<<20).CollectAll(nil); err != nil {
		t.Fatal(err)
	}

	// One parse over both.
	if _, err := parse.Session(zone, parse.Options{
		Conversation: session, Session: session, Reindex: index.Rebuild}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	view := fold(t, zone, session)
	var tool *sessionflow.Node
	for _, n := range view.Nodes {
		if n.Kind == model.KindTool {
			tool = n
		}
	}
	if tool == nil {
		t.Fatal("the conversation has no tool step for the call the changes name")
	}
	if !strings.Contains(tool.ID, call) {
		t.Errorf("the tool step is %s, which is not the call the changes name (%s)", tool.ID, call)
	}
	if !mentioned(t, zone, session, "findings.md") {
		t.Error("the file the tool wrote is named nowhere in the conversation's session")
	}
	for _, u := range view.Unresolved {
		t.Errorf("unresolved %s: %s", u.Kind, u.Reason)
	}
	t.Logf("%s: %d nodes, tool step %s", session, len(view.Nodes), tool.ID)
}

// oneTurnWithATool is a batch carrying a turn that calls one tool: the
// question, the model's call, and the tool's answer.
func oneTurnWithATool(call string) string {
	const trace = "55555555-5555-7555-8555-555555555555"
	const llm = "55555555-5555-7555-8555-555555555556"
	const tool = "55555555-5555-7555-8555-555555555557"
	owner := `"session_name":"tsb-advisor","extra":{"metadata":{"thread_id":"the-conversation"}}`
	return `{"post":[` +
		`{"id":"` + trace + `","trace_id":"` + trace + `",` +
		`"dotted_order":"20260920T100000000000Z` + trace + `",` +
		`"run_type":"chain","name":"agent","start_time":"2026-09-20T10:00:00Z",` +
		`"end_time":"2026-09-20T10:00:03Z",` + owner + `,` +
		`"inputs":{"messages":[{"role":"user","content":"write the report"}]}},` +

		`{"id":"` + llm + `","trace_id":"` + trace + `","parent_run_id":"` + trace + `",` +
		`"dotted_order":"20260920T100000000000Z` + trace + `.20260920T100001000000Z` + llm + `",` +
		`"run_type":"llm","name":"ChatOpenAI","start_time":"2026-09-20T10:00:01Z",` +
		`"end_time":"2026-09-20T10:00:02Z",` + owner + `,` +
		`"outputs":{"generations":[[{"message":{"kwargs":{"content":"",` +
		`"tool_calls":[{"id":"` + call + `","name":"write_report","args":{"cluster":"prod-1"}}]}}}]]}},` +

		`{"id":"` + tool + `","trace_id":"` + trace + `","parent_run_id":"` + trace + `",` +
		`"dotted_order":"20260920T100000000000Z` + trace + `.20260920T100002000000Z` + tool + `",` +
		`"run_type":"tool","name":"write_report","start_time":"2026-09-20T10:00:02Z",` +
		`"end_time":"2026-09-20T10:00:03Z",` + owner + `,` +
		`"outputs":{"output":{"type":"tool","tool_call_id":"` + call + `","content":"written"}}}` +
		`]}`
}
