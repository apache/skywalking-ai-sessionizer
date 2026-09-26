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
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/pkg/execution"
	"github.com/apache/skywalking-ai-sessionizer/plugins/claude-code/internal/output"
)

// mcpHooks are the events Claude Code 2.1.282 handed a hook after calls to
// two MCP servers, one over stdio and one over HTTP: a call that returned, two
// identical calls at once, an error the server returned, a server that died,
// a timeout, a call whose input a hook rewrote, and a call inside a subagent.
// Local paths are replaced; nothing else is changed.
func mcpHooks(t *testing.T) map[string][]string {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "mcp-hooks-2.1.282.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string][]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var c struct {
			Case string `json:"probe_case"`
		}
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		out[c.Case] = append(out[c.Case], sc.Text())
	}
	return out
}

func executions(t *testing.T, data, session, stream string) []*execution.Record {
	t.Helper()
	b, err := os.ReadFile(output.ExecutionPath(data, session, stream))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []*execution.Record
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		r, ok := execution.Decode([]byte(line))
		if !ok {
			t.Fatalf("not an execution record: %s", line)
		}
		if err := r.Validate(); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func sessionOf(t *testing.T, line string) string {
	t.Helper()
	var h struct {
		Session string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(line), &h); err != nil {
		t.Fatal(err)
	}
	return h.Session
}

// TestEachMCPCallIsRecordedAsTheRuntimeEndedIt runs every captured event
// through the hook and reads back what it wrote.
func TestEachMCPCallIsRecordedAsTheRuntimeEndedIt(t *testing.T) {
	hooks := mcpHooks(t)
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		kase, stream, server, source, outcome string
		records                               int
		duration                              int64
		result                                bool
	}{
		{"echo", "main", "probe", "dynamic", execution.OutcomeReturned, 1, 2, true},
		// Two identical calls at once are two calls: the runtime gave each
		// its own id, and the server was sent each one.
		{"parallel", "main", "probe", "dynamic", execution.OutcomeReturned, 2, 1, true},
		// An error the server returned, a lost connection and a timeout all
		// reach the failure hook. None of them keeps an answer.
		{"fail", "main", "probe", "dynamic", execution.OutcomeFailed, 1, 1, false},
		{"crash", "main", "probe", "dynamic", execution.OutcomeFailed, 1, 1, false},
		{"slow", "main", "probe", "dynamic", execution.OutcomeFailed, 1, 2004, false},
		{"http", "main", "web", "dynamic", execution.OutcomeReturned, 1, 3, true},
		{"rewrite", "main", "probe", "dynamic", execution.OutcomeReturned, 1, 2, true},
		// A call inside a subagent is recorded in the subagent's stream.
		{"child", "ad1f9feae632355a5", "probe", "dynamic", execution.OutcomeReturned, 1, 1, true},
	} {
		t.Run(tc.kase, func(t *testing.T) {
			data := t.TempDir()
			for _, line := range hooks[tc.kase] {
				runHook(strings.NewReader(line), data, "/work/"+tc.kase, now)
			}
			got := executions(t, data, sessionOf(t, hooks[tc.kase][0]), tc.stream)
			if len(got) != tc.records {
				t.Fatalf("%d records, want %d", len(got), tc.records)
			}
			ids := map[string]bool{}
			for _, r := range got {
				ids[r.ID] = true
				if r.Server == nil || r.Server.Name != tc.server || r.Server.Source != tc.source {
					t.Errorf("server %+v, want %s from %s", r.Server, tc.server, tc.source)
				}
				if r.Outcome != tc.outcome {
					t.Errorf("outcome %s, want %s", r.Outcome, tc.outcome)
				}
				if r.DurationMS == nil || *r.DurationMS != tc.duration {
					t.Errorf("duration %v, want %d", r.DurationMS, tc.duration)
				}
				if (r.Result != nil) != tc.result {
					t.Errorf("result kept: %v, want %v", r.Result != nil, tc.result)
				}
				if r.Arguments == nil || r.Arguments.State != execution.ContentSizeOnly {
					t.Errorf("arguments %+v, want their size only", r.Arguments)
				}
				if r.Protocol != execution.ProtocolMCP || r.Boundary != execution.BoundaryClientHook ||
					r.ObservedBy != execution.ObservedByASZPlugin || !strings.HasPrefix(r.ID, r.Tool+"/") {
					t.Errorf("record %+v", r)
				}
			}
			if len(ids) != tc.records {
				t.Errorf("%d distinct ids for %d records", len(ids), tc.records)
			}
		})
	}
}

// TestTheInputRecordedIsWhatTheServerWasSent. A hook before the call
// rewrote {"text":"original"} to {"text":"rewritten by the hook"}; the
// server received the second, the transcript kept the first. The record's
// digest is of what the server received, in any key order.
func TestTheInputRecordedIsWhatTheServerWasSent(t *testing.T) {
	data := t.TempDir()
	line := mcpHooks(t)["rewrite"][0]
	runHook(strings.NewReader(line), data, "/work/rewrite", time.Now())
	got := executions(t, data, sessionOf(t, line), "main")
	if len(got) != 1 {
		t.Fatalf("%d records", len(got))
	}
	sent := execution.Measure([]byte(`{ "text" : "rewritten by the hook" }`))
	model := execution.Measure([]byte(`{"text":"original"}`))
	if got[0].Arguments.SHA256 != sent.SHA256 || got[0].Arguments.Bytes != sent.Bytes {
		t.Errorf("arguments %+v, want those of what the server was sent %+v", got[0].Arguments, sent)
	}
	if got[0].Arguments.SHA256 == model.SHA256 {
		t.Error("the record holds the model's input, not what the server was sent")
	}
}

// TestTheSameEventTwiceIsOneRecord. A record's id is the call and where it
// was observed, so an event handed over again reads back as the same record,
// which the collector keeps once.
func TestTheSameEventTwiceIsOneRecord(t *testing.T) {
	data := t.TempDir()
	line := mcpHooks(t)["echo"][0]
	runHook(strings.NewReader(line), data, "/work/echo", time.Now())
	runHook(strings.NewReader(line), data, "/work/echo", time.Now())
	got := executions(t, data, sessionOf(t, line), "main")
	if len(got) != 2 || got[0].ID != got[1].ID {
		t.Fatalf("records %+v, want two lines with one id", got)
	}
}

// TestMCPRecordingCanBeTurnedOff, and a tool that is not an MCP call is
// never recorded as one.
func TestMCPRecordingCanBeTurnedOff(t *testing.T) {
	line := mcpHooks(t)["echo"][0]
	session := sessionOf(t, line)
	data := t.TempDir()
	if err := os.WriteFile(filepath.Join(data, "settings.yaml"), []byte("mcp:\n  enabled: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runHook(strings.NewReader(line), data, "/work/echo", time.Now())
	if got := executions(t, data, session, "main"); len(got) != 0 {
		t.Errorf("%d records with recording turned off", len(got))
	}
	data = t.TempDir()
	runHook(strings.NewReader(hookInput("PostToolUse", session, "", "Read", "toolu_read", `{"file_path":"a"}`, `{"file":{}}`, "")), data, "/work/echo", time.Now())
	if got := executions(t, data, session, "main"); len(got) != 0 {
		t.Errorf("%d records for a call that went to no MCP server", len(got))
	}
	// The hook recovers a panic and logs it, so no record is also what a
	// hook that crashed on the missing server would leave.
	if log, _ := os.ReadFile(filepath.Join(data, "log", "plugin.log")); strings.Contains(string(log), "panic") {
		t.Errorf("the hook failed on a call that went to no MCP server: %s", log)
	}
}
