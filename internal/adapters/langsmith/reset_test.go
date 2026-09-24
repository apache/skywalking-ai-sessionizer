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

package langsmith

import (
	"encoding/json"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// callSent is a model call's arrival whose request is the given messages.
func callSent(t *testing.T, runType, id string, messages ...string) Operation {
	t.Helper()
	list := "["
	for i, m := range messages {
		if i > 0 {
			list += ","
		}
		list += m
	}
	list += "]"
	envelope := `{"id":"` + id + `","trace_id":"trace-1","parent_run_id":"trace-1",` +
		`"run_type":"` + runType + `","name":"ChatOpenAI","start_time":"2026-09-24T10:00:00Z"}`
	return Operation{Op: "post", RunID: id, Envelope: json.RawMessage(envelope),
		Fields: map[string]json.RawMessage{"inputs": json.RawMessage(`{"messages":[` + list + `]}`)}}
}

func resetsOf(t *testing.T, op Operation) []sessiondata.Record {
	t.Helper()
	records, err := Convert(op, false, Hints{})
	if err != nil {
		t.Fatal(err)
	}
	var out []sessiondata.Record
	for _, r := range records {
		if isReset(r) {
			out = append(out, r)
		}
	}
	return out
}

const (
	marked = `{"lc":1,"type":"constructor","id":["langchain","schema","messages","HumanMessage"],` +
		`"kwargs":{"content":"Here is a summary of the conversation to date:\n\nall healthy",` +
		`"additional_kwargs":{"lc_source":"summarization"},"type":"human","id":"sum-1"}}`
	question = `{"lc":1,"type":"constructor","id":["langchain","schema","messages","HumanMessage"],` +
		`"kwargs":{"content":"Check prod-3.","type":"human","id":"q-3"}}`
)

// TestAMarkedSummaryIsAReset: the boundary comes before the call, and the
// summary names the boundary as its parent, which is how assembly pairs them.
func TestAMarkedSummaryIsAReset(t *testing.T) {
	records, err := Convert(callSent(t, "llm", "call-1", marked, question), false, Hints{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("%d records, want a boundary, a summary and the call", len(records))
	}
	boundary, summary, call := records[0], records[1], records[2]
	if boundary.ID != "sum-1:reset" || boundary.Flags[0] != "context_reset" {
		t.Errorf("boundary %s %v", boundary.ID, boundary.Flags)
	}
	if summary.ID != "sum-1:summary" || summary.Flags[0] != "reset_summary" || summary.Parent != boundary.ID {
		t.Errorf("summary %s %v parent %s", summary.ID, summary.Flags, summary.Parent)
	}
	if len(summary.Parts) != 1 || summary.Parts[0].Text != "Here is a summary of the conversation to date:\n\nall healthy" {
		t.Errorf("summary parts %+v", summary.Parts)
	}
	if call.Call != "call-1" {
		t.Errorf("the call is not last: %+v", call)
	}
}

// TestEveryCallSentTheSummaryNamesTheSameReset: a summary is sent again with
// every later call. The records must be the same ones each time, so the index
// keeps one reset even when the placement has forgotten it already landed.
func TestEveryCallSentTheSummaryNamesTheSameReset(t *testing.T) {
	first := resetsOf(t, callSent(t, "llm", "call-1", marked, question))
	later := resetsOf(t, callSent(t, "llm", "call-2", marked, question))
	if len(first) != 2 || len(later) != 2 {
		t.Fatalf("%d and %d reset records, want 2 each", len(first), len(later))
	}
	for i := range first {
		if first[i].ID != later[i].ID {
			t.Errorf("record %d: %s, then %s", i, first[i].ID, later[i].ID)
		}
	}
}

// TestNothingButTheMarkIsAReset. Each of these looks like a summary, and none
// was marked as one.
func TestNothingButTheMarkIsAReset(t *testing.T) {
	for name, op := range map[string]Operation{
		// The same words with no mark: a person can type them.
		"unmarked": callSent(t, "llm", "call-1",
			`{"role":"user","content":"Here is a summary of the conversation to date:\n\nall healthy"}`),
		// langmem's summary is a system message and carries no mark.
		"langmem": callSent(t, "llm", "call-2",
			`{"role":"system","content":"Summary of the conversation so far: all healthy"}`, question),
		// Marked, but with no id to tell it from another summary with the
		// same words.
		"no id": callSent(t, "llm", "call-5",
			`{"role":"user","content":"all healthy","additional_kwargs":{"lc_source":"summarization"}}`),
		// The key with another value is another source.
		"another source": callSent(t, "llm", "call-3",
			`{"role":"user","content":"x","additional_kwargs":{"lc_source":"something_else"}}`),
		// The graph's own runs carry the marked message in their state.
		// Only a model call's request says what the model was sent.
		"a graph run": callSent(t, "chain", "run-4", marked, question),
	} {
		if got := resetsOf(t, op); len(got) != 0 {
			t.Errorf("%s: %d reset records, want none", name, len(got))
		}
	}
}
