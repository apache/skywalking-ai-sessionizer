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

package local_test

import (
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/verify"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// first returns the first record in a landed directory matching a predicate.
func (c *collected) first(dir, kind string, match func(sessiondata.Record) bool) sessiondata.Record {
	c.t.Helper()
	for _, rec := range c.records(dir, kind) {
		if match(rec) {
			return rec
		}
	}
	c.t.Fatalf("no landed %s record in %s matched", kind, dir)
	return sessiondata.Record{}
}

// firstPart returns the first content part matching a predicate.
func (c *collected) firstPart(dir, kind string, match func(sessiondata.Part) bool) sessiondata.Part {
	c.t.Helper()
	for _, rec := range c.records(dir, kind) {
		for _, p := range rec.Parts {
			if match(p) {
				return p
			}
		}
	}
	c.t.Fatalf("no content part in %s matched", dir)
	return sessiondata.Part{}
}

func TestConversationValuesSurvive(t *testing.T) {
	c := collect(t)
	mainDir := c.zone.StreamDir(case1, storage.StreamMain)

	t.Run("human input", func(t *testing.T) {
		rec := c.first(mainDir, "transcript", func(r sessiondata.Record) bool {
			return r.Trigger == model.TriggerExternal
		})
		if got := rec.Text(); !strings.Contains(got, "run the build") {
			t.Errorf("human input = %q, want it to contain %q", got, "run the build")
		}
	})

	t.Run("tool command and result", func(t *testing.T) {
		// The command IS the tool's input - for a shell tool there is nothing else.
		call := c.firstPart(mainDir, "transcript", func(p sessiondata.Part) bool {
			return p.Kind == sessiondata.PartCall && p.Name == "Bash"
		})
		if !strings.Contains(string(call.Data), `"command":"make build"`) {
			t.Errorf("tool command lost: %s", call.Data)
		}
		res := c.firstPart(mainDir, "transcript", func(p sessiondata.Part) bool {
			return p.Kind == sessiondata.PartResult && p.Of == "tool-run-make-build"
		})
		// The readable output and the structured form both survive: they are two
		// views of one result, and a reader wants whichever it can use.
		if !strings.Contains(res.Text, "build succeeded") {
			t.Errorf("tool output lost: %q", res.Text)
		}
		if !strings.Contains(string(res.Data), `"stdout":"build succeeded"`) {
			t.Errorf("the structured form of the result was lost: %s", res.Data)
		}
	})

	t.Run("agent output lives in the child stream", func(t *testing.T) {
		child := c.first(c.zone.StreamDir(case1, agentChecker), "transcript", func(r sessiondata.Record) bool {
			return strings.Contains(r.Text(), "Tests pass.")
		})
		if child.Call != "checker-call1" {
			t.Errorf("child output has call %q, want checker-call1", child.Call)
		}
		// The parent must reference the child, never absorb its messages.
		for _, rec := range c.mainRecords() {
			if strings.Contains(content(rec), "Tests pass.") {
				t.Error("the parent stream absorbed the child's output")
			}
		}
	})

	t.Run("spawn chain is exact", func(t *testing.T) {
		launch := c.first(mainDir, "transcript", func(r sessiondata.Record) bool {
			return hasFlag(r, "launch_ack")
		})
		if launch.Child != agentChecker {
			t.Errorf("launch acknowledgement names child %q, want %q", launch.Child, agentChecker)
		}
		note := c.first(mainDir, "transcript", func(r sessiondata.Record) bool {
			return r.Trigger == model.TriggerNotification
		})
		if note.Child != agentChecker {
			t.Errorf("notification names child %q, want %q", note.Child, agentChecker)
		}
		if note.Tool != "tool-spawn-checker" {
			t.Errorf("notification names call %q, want tool-spawn-checker", note.Tool)
		}
	})

	t.Run("a context reset keeps its explicit backward pointer", func(t *testing.T) {
		b := c.first(mainDir, "transcript", func(r sessiondata.Record) bool {
			return hasFlag(r, "context_reset")
		})
		if b.Continues != "call4-final-answer" {
			// Without it an epoch would have to be inferred, and a reset's own
			// parent is empty.
			t.Errorf("the reset continues from %q, want call4-final-answer", b.Continues)
		}
	})

	t.Run("a batch keeps journal, manifest and script together", func(t *testing.T) {
		runDir := c.zone.RunDir(case1, runBuildCheck)
		j := c.first(runDir, "journal", func(r sessiondata.Record) bool { return r.Child != "" })
		if j.Child != agentWFStep {
			t.Errorf("journal names child %q, want %q", j.Child, agentWFStep)
		}
		m := c.first(runDir, "manifest", func(sessiondata.Record) bool { return true })
		if !strings.Contains(content(m), "task-buildcheck") {
			t.Error("the manifest lost the task id that resolves notifications")
		}
		// A script is JavaScript, not JSON. It travels whole, in an unknown part.
		sc := c.first(runDir, "script", func(sessiondata.Record) bool { return true })
		if len(sc.Parts) != 1 || sc.Parts[0].Kind != sessiondata.PartUnknown {
			t.Fatalf("script converted to %d part(s): %+v", len(sc.Parts), sc.Parts)
		}
		if !strings.Contains(string(sc.Parts[0].Data), "build-check") {
			t.Errorf("script content lost: %s", sc.Parts[0].Data)
		}
	})
}

// TestIndexMapsRolesNotRuntimeFields asserts the vocabulary boundary: the index
// stores roles, and a runtime's field names stop at the adapter.
func TestIndexMapsRolesNotRuntimeFields(t *testing.T) {
	c := collect(t)
	ix, ok, err := index.Load(c.zone.IndexDir(case1), case1)
	if err != nil || !ok {
		t.Fatalf("no index: ok=%v err=%v", ok, err)
	}
	ix.Build()

	// uuid -> Record
	if _, found := ix.EntryByRecord("turn1-human-input"); !found {
		t.Error("record id not indexed")
	}
	// message.id -> Call. One call, three fragments.
	if got := ix.ProviderCall("call1"); len(got) != 3 {
		t.Errorf("provider call msg_A has %d fragments, want 3", len(got))
	}
	// promptId -> Run. Two runs in one Talk: the human turn and the
	// notification that woke the agent when the child finished.
	cycles := map[uint32]bool{}
	for i := range ix.Entries {
		if e := &ix.Entries[i]; e.Run != 0 && ix.Strings.String(e.Stream) == storage.StreamMain {
			cycles[e.Run] = true
		}
	}
	if len(cycles) != 2 {
		t.Errorf("main stream has %d prompt cycles, want 2", len(cycles))
	}
	// A server-side tool id would be dropped by a ^toolu_ regex.
	if got := ix.ToolBlocks("srvtool-websearch"); len(got) != 2 {
		t.Errorf("srvtool-websearch resolved to %d blocks, want 2 (use + result)", len(got))
	}
	if got := ix.ToolBlocks("tool-run-make-build"); len(got) != 2 {
		t.Errorf("tool-run-make-build resolved to %d blocks, want 2", len(got))
	}
	// Streams: main plus two children, all under one session.
	if got := ix.Streams(); len(got) != 3 {
		t.Errorf("streams = %v, want main + 2 children", got)
	}
}

// TestLandedDataIsContiguousAndIntact runs the same audit `asz verify` runs.
func TestLandedDataIsContiguousAndIntact(t *testing.T) {
	c := collect(t)
	for _, s := range []string{case1, case2} {
		rep, err := verify.Session(c.zone, s)
		if err != nil {
			t.Fatal(err)
		}
		if !rep.OK() {
			t.Errorf("%s: %d problem(s): %v", s, rep.Problems, rep.Details())
		}
		if rep.Records == 0 {
			t.Errorf("%s: no records verified", s)
		}
	}
}
