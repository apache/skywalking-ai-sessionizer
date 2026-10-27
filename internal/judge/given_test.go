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

package judge

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// givenDoc is a conversation of two main talks and two sub-tasks. talk/1
// holds an input, a reply and two tools whose results share one record by
// its two blocks; s1 starts child a, whose s2 starts child b, and a reports
// back. talk/2 holds an input, a tool on a record of its own, and a step too
// large to land. talk/3 was opened by a command, so the talk itself stands
// for its input. cl is loose work.
func givenDoc() *sessionview.Conversation {
	block := func(i int) *int { return &i }
	step := func(id, kind, text string) sessionview.Node { return sessionview.Node{ID: id, Kind: kind, Text: text} }
	tool := func(id string, ref sessionflow.Ref) sessionview.Node {
		return sessionview.Node{ID: id, Kind: model.KindTool, Name: "Bash", Text: `{"command":"ls"}`, Result: "done", Ref: &ref}
	}
	big := sessionview.Node{ID: "big", Kind: model.KindTool, Name: "Read", State: model.ContentTooLarge}
	huge := sessionview.Node{ID: "huge", Kind: model.KindTool, Name: strings.Repeat("n", PageBytes+1), Result: "done"}
	t1 := sessionview.Node{ID: "talk/1", Kind: "talk", Stream: "main", Label: "fix it", Reply: "I will look", Children: []sessionview.Node{
		step("in/1", model.KindMessageExternal, "fix it"), call("c1", "main", 100, "m", nil), step("r1", model.KindMessageAssistant, "I will look"),
		tool("s1", sessionflow.Ref{Seq: 5, Row: 1, Block: block(0)}), tool("s3", sessionflow.Ref{Seq: 5, Row: 1, Block: block(1)}),
	}}
	t2 := sessionview.Node{ID: "talk/2", Kind: "talk", Stream: "main", Label: "and this", Children: []sessionview.Node{
		step("in/2", model.KindMessageExternal, "and this"), call("c2", "main", 600, "m", nil), tool("s4", sessionflow.Ref{Seq: 5, Row: 2}), big, huge,
	}}
	ta := sessionview.Node{ID: "talk/a", Kind: "talk", Stream: "a", Children: []sessionview.Node{
		call("ca", "a", 150, "m", nil), tool("s2", sessionflow.Ref{Seq: 6, Row: 1}), step("out/a", model.KindAgentOutput, "found it"),
	}}
	tb := sessionview.Node{ID: "talk/b", Kind: "talk", Stream: "b", Children: []sessionview.Node{call("cb", "b", 200, "m", nil)}}
	starts := func(from, to string) sessionview.Relation {
		return sessionview.Relation{ID: from + to, Type: model.RelStarts, From: from, To: "stream/" + to}
	}
	return &sessionview.Conversation{
		Conversation: "conv",
		Streams:      []sessionview.Stream{{ID: "stream/main", Name: "main"}, {ID: "stream/a", Name: "a"}, {ID: "stream/b", Name: "b"}},
		Talks: []sessionview.Node{t1, ta, tb, t2,
			{ID: "talk/3", Kind: "talk", Stream: "main", Label: "/compact"}},
		Loose:     []sessionview.Node{call("cl", "main", 800, "m", nil)},
		Relations: []sessionview.Relation{starts("s1", "a"), starts("s2", "b")},
	}
}

// givenNav navigates givenDoc, its records read: one record holding s1's
// and s3's results as its two blocks, and s4's record of one empty part.
func givenNav() *Nav {
	d := givenDoc()
	recs := map[[2]uint64]*sessiondata.Record{
		{5, 1}: {Parts: []sessiondata.Part{{Kind: "text", Text: "first result"}, {Kind: "text", Text: "second result"}}},
		{5, 2}: {Parts: []sessiondata.Part{{Kind: "text", Text: ""}}},
		{6, 1}: {Parts: []sessiondata.Part{{Kind: "text", Text: "child result"}}},
	}
	return NewNav(d, Compute(d, "claude-code/1", nil, nil), func(seq, row uint64) (*sessiondata.Record, error) {
		if r := recs[[2]uint64{seq, row}]; r != nil {
			return r, nil
		}
		return nil, fmt.Errorf("no record %d/%d", seq, row)
	})
}

// receiptOf answers one call and returns its receipt.
func receiptOf(t *testing.T, nav *Nav, tool, args string) Receipt {
	t.Helper()
	_, r, err := nav.Answer(tool, json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s %s: %v", tool, args, err)
	}
	return r
}

// A receipt is taken from what an answer returned: a step opened when the
// evaluator asked for it and was answered with something, listed when a row
// of the page names it. A row of a talk or a sub-task names the steps that
// hold its input, reply, start and report, never the steps inside it.
func TestAReceiptComesFromTheAnswerReturned(t *testing.T) {
	nav := givenNav()
	// ids writes some step ids sorted, so a receipt reads the same in any
	// order.
	ids := func(xs []string) string {
		xs = slices.Clone(xs)
		slices.Sort(xs)
		return strings.Join(xs, " ")
	}
	for _, c := range []struct {
		name, tool, args string
		opened, listed   string
	}{
		{"a step", "step", `{"id":"s1"}`, "s1", ""},
		{"a step with nothing to read", "step", `{"id":"c1"}`, "", ""},
		{"a step shrunk to its addresses", "step", `{"id":"huge"}`, "", ""},
		{"a container", "step", `{"id":"talk/1"}`, "", ""},
		{"a talk's outline", "talk", `{"id":"talk/1"}`, "", "c1 in/1 r1 s1 s3"},
		{"the talks", "talks", `{}`, "", "in/1 in/2 r1"},
		{"the inputs", "inputs", `{}`, "", "in/1 in/2"},
		{"the sub-tasks", "subtasks", `{"talks":["talk/1"]}`, "", "out/a s1"},
		{"a search", "search", `{"query":"done"}`, "", "huge s1 s2 s3 s4"},
		{"a search of one step that matches", "search", `{"query":"first","node":"s1"}`, "s1", ""},
		{"a search of one step that does not", "search", `{"query":"nothing","node":"s1"}`, "", ""},
		{"a part of a record two steps share", "record", `{"seq":5,"row":1,"part":1}`, "s3", ""},
		{"an empty part", "record", `{"seq":5,"row":2,"part":0}`, "", ""},
		{"a record's JSON", "record", `{"seq":5,"row":1}`, "", ""},
		// An item is read whole by its number, with no page asked for first.
		{"a row read as an item", "talks", `{"item":1}`, "", "in/1 r1"},
		{"an input read as an item", "inputs", `{"item":2}`, "", "in/2"},
		{"a match read as an item", "search", `{"query":"first","node":"s1","item":1}`, "s1", ""},
	} {
		r := receiptOf(t, nav, c.tool, c.args)
		if ids(r.Opened) != c.opened || ids(r.Listed) != c.listed {
			t.Errorf("%s: opened %q listed %q, want %q %q", c.name, ids(r.Opened), ids(r.Listed), c.opened, c.listed)
		}
	}
	// A slice from an offset past a row's ids names none of them.
	whole, _, _ := nav.Answer("talks", json.RawMessage(`{"item":1}`))
	tail := fmt.Sprintf(`{"item":1,"offset":%d}`, whole.(map[string]any)["bytes"].(int)-2)
	if r := receiptOf(t, nav, "talks", tail); len(r.Listed) != 0 {
		t.Errorf("the end of a row: %+v", r)
	}
}

// Only what the evaluator was given counts. An answer counts once delivered,
// and a call's prompt rows count as listed; nothing else does.
func TestOnlyWhatWasGivenCounts(t *testing.T) {
	nav := givenNav().Fork()
	_, r, _ := nav.Answer("step", json.RawMessage(`{"id":"s1"}`))
	if c := nav.GivenWindow().Counts; c.Opened != 0 {
		t.Fatalf("an answer not given yet counted: %+v", c)
	}
	nav.Deliver(r)
	nav.DeliverRows(nav.TalkRows([]string{"talk/2"}))
	c := nav.GivenWindow().Counts
	// s1 opened; in/2, the input of talk/2's row, listed; the rest none.
	if c.Opened != 1 || c.Listed != 1 || c.Steps != 15 || c.None != 13 || c.Unavailable != 1 {
		t.Fatalf("counts %+v", c)
	}
	nav.LoseReceipts("lost")
	if g := nav.GivenWindow(); g.Counts != nil || g.Reason != "lost" {
		t.Fatalf("lost receipts: %+v", g)
	}
}

// An API evaluator's answer counts once a request carried it to the model
// whole: one shortened before its first send never reached it, and the
// calls made in the turn that holds the final answer are never sent.
func TestAnAPIAnswerCountsOnceItReachesTheModel(t *testing.T) {
	nav := givenNav()
	turns := []turn{{role: "user", results: []toolResult{
		{ID: "a", given: Receipt{Opened: []string{"s1"}}, masked: true},
		{ID: "b", given: Receipt{Opened: []string{"s3"}}},
	}}}
	deliverNew(nav, turns)
	turns[0].results[1].masked = true
	turns = append(turns, turn{role: "user", results: []toolResult{{ID: "c", given: Receipt{Opened: []string{"s4"}}}}})
	deliverNew(nav, turns)
	if c := nav.GivenWindow().Counts; c.Opened != 2 || nav.given.opened["s1"] {
		t.Fatalf("opened %v", nav.given.opened)
	}

	for _, c := range []struct {
		name, first, second string
		opened              int
	}{
		{"read in the turn that answers", `{"content":[{"type":"tool_use","id":"u1","name":"step","input":{"id":"s1"}},` +
			`{"type":"tool_use","id":"u2","name":"submit","input":{}}]}`, "", 0},
		{"read, then answered", `{"content":[{"type":"tool_use","id":"u1","name":"step","input":{"id":"s1"}}]}`,
			`{"content":[{"type":"tool_use","id":"u2","name":"submit","input":{}}]}`, 1},
	} {
		srv, _ := fakeAPI(t, c.first, c.second)
		t.Setenv("TEST_KEY", "k")
		ex, err := NewExecutor("e", config.Evaluator{Kind: "api", Program: "anthropic", Endpoint: srv.URL, Model: "m", APIKeyEnv: "TEST_KEY"})
		if err != nil {
			t.Fatal(err)
		}
		n := givenNav().Fork()
		res, err := ex.Run(t.Context(), Task{Stage: "divergence", Prompt: "judge", Schema: json.RawMessage(`{"type":"object"}`), Dir: t.TempDir(), Nav: n})
		if err != nil || !res.InProcess {
			t.Fatalf("%s: %v %v", c.name, err, res.InProcess)
		}
		if got := n.GivenWindow().Counts.Opened; got != c.opened {
			t.Errorf("%s: %d opened, want %d", c.name, got, c.opened)
		}
	}
}

// A navigation server's log counts only whole: every line this attempt's,
// and a closing line whose count matches. Anything else makes the receipts
// unavailable, never fewer; a complete log of no answers counts as given
// nothing.
func TestAServerLogCountsOnlyWhole(t *testing.T) {
	line := func(v map[string]any) string { b, _ := json.Marshal(v); return string(b) }
	answer := line(map[string]any{"attempt": "a1", "tool": "step", "args": map[string]any{"id": "s1"}, "bytes": 10, "error": false,
		"refused": false, "opened": []string{"s1"}})
	closing := func(n int, failed bool) string {
		return line(map[string]any{"attempt": "a1", "closing": true, "answers": n, "write_failed": failed})
	}
	for _, c := range []struct {
		name, log, reason string
		opened            int
	}{
		{"whole", answer + "\n" + closing(1, false) + "\n", "", 1},
		{"whole, and empty", closing(0, false) + "\n", "", 0},
		{"missing", "", "could not be read", 0},
		{"no closing line", answer + "\n", "no closing line", 0},
		{"a count that does not match", answer + "\n" + closing(2, false) + "\n", "closes on 2 answers and holds 1", 0},
		{"a line that does not read", "{broken\n" + closing(0, false) + "\n", "does not read", 0},
		{"another attempt's line", strings.Replace(answer, `"a1"`, `"a2"`, 1) + "\n" + closing(1, false) + "\n", "not this attempt's", 0},
		{"a write that failed", answer + "\n" + closing(1, true) + "\n", "a write to the navigation log failed", 0},
		{"a line after the closing one", closing(0, false) + "\n" + answer + "\n", "goes on after its closing line", 0},
		{"a line with more after it", answer + " {}\n" + closing(1, false) + "\n", "does not read", 0},
		{"a closing line with no count", line(map[string]any{"attempt": "a1", "closing": true, "write_failed": false}) + "\n", "gives no count", 0},
	} {
		path := filepath.Join(t.TempDir(), "mcp-calls.jsonl")
		if c.name != "missing" {
			if err := os.WriteFile(path, []byte(c.log), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		nav := givenNav().Fork()
		nav.TakeLog(path, "a1")
		g := nav.GivenWindow()
		switch {
		case c.reason == "" && (g.Counts == nil || g.Counts.Opened != c.opened):
			t.Errorf("%s: %+v", c.name, g)
		case c.reason != "" && (g.Counts != nil || !strings.Contains(g.Reason, c.reason)):
			t.Errorf("%s: %+v, want a reason with %q", c.name, g, c.reason)
		}
	}
}

// The judged work is a window's main talks with every sub-task they
// started, never the earlier talks it shows as context; a run over the
// whole conversation judges every talk and its loose work.
func TestTheJudgedWorkIsTheRunsOwn(t *testing.T) {
	whole := givenNav()
	if c := whole.GivenWindow().Counts; c.Steps != 15 {
		t.Fatalf("whole conversation: %+v", c)
	}
	first := givenNav()
	first.Within(&Selection{Talks: []string{"talk/1"}})
	if c := first.GivenWindow().Counts; c.Steps != 9 {
		t.Fatalf("talk/1 with children a and b: %+v", c)
	}
	second := givenNav()
	second.Within(&Selection{Talks: []string{"talk/2"}, Before: []string{"talk/1"}})
	if c := second.GivenWindow().Counts; c.Steps != 5 || c.Unavailable != 1 {
		t.Fatalf("talk/2, with talk/1 as context: %+v", c)
	}
}

// A span is one stream from the step where a question or an assumption
// arose to the finding's step, in stream order across its talks, with the
// sub-tasks a step of it started. A span asz cannot order says why.
func TestASpanIsOneStreamInOrder(t *testing.T) {
	nav := givenNav()
	for _, c := range []struct {
		name, from, at string
		steps          int
		reason         string
	}{
		{"within one talk, with the sub-tasks it started", "in/1", "s1", 8, ""},
		{"across two talks", "s3", "c2", 3, ""},
		{"one step", "s4", "s4", 1, ""},
		{"no start given", "", "s1", 0, "no step where"},
		{"a start after the step", "s4", "s1", 0, "does not come before"},
		{"a start in another stream", "s2", "s4", 0, "are not in one stream"},
		{"a start that is no step", "talk/1", "s1", 0, "is not a step of a talk"},
		{"a loose start", "cl", "s1", 0, "is not a step of a talk"},
	} {
		g, valid := nav.GivenSpan(c.from, c.at)
		switch {
		case valid != (c.reason == ""):
			t.Errorf("%s: valid %v", c.name, valid)
		case c.reason == "" && (g.Counts == nil || g.Counts.Steps != c.steps):
			t.Errorf("%s: %+v %+v", c.name, g, g.Counts)
		case c.reason != "" && (g.Counts != nil || !strings.Contains(g.Reason, c.reason)):
			t.Errorf("%s: %+v, want a reason with %q", c.name, g, c.reason)
		}
	}
}

// A tool's result that carried an image, as a screenshot does, holds
// evidence no evaluator here reads, though the view keeps its words. Only
// the block the step stands on counts, and a call's request is the model's
// own words, though its input names an image.
func TestAResultImageIsEvidenceNoOneIsGiven(t *testing.T) {
	d := givenDoc()
	zero, one := 0, 1
	// s1's result is the other block of the record that holds s4's.
	results := map[string][]sessionflow.Ref{"s1": {{Seq: 8, Row: 1}, {Seq: 7, Row: 1, Block: &zero}},
		"s4": {{Seq: 5, Row: 2}, {Seq: 7, Row: 1, Block: &one}}}
	for _, tk := range d.Talks {
		for i, x := range tk.Children {
			if refs, ok := results[x.ID]; ok {
				tk.Children[i].Refs = refs
			}
		}
	}
	shot := json.RawMessage(`[{"type":"text","text":"Took a screenshot"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}]`)
	recs := map[[2]uint64]*sessiondata.Record{
		{5, 1}: {Parts: []sessiondata.Part{{Kind: sessiondata.PartText, Text: "first result"}, {Kind: sessiondata.PartText, Text: "second result"}}},
		{7, 1}: {Parts: []sessiondata.Part{{Kind: sessiondata.PartText, Text: "another tool's result"}, {Kind: sessiondata.PartData, Data: shot}}},
		{8, 1}: {Parts: []sessiondata.Part{{Kind: sessiondata.PartCall, Data: json.RawMessage(`{"name":"render","input":{"type":"image"}}`)}}},
	}
	nav := NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
	err := nav.MarkResultImages(func(refs []sessionflow.Ref) (map[[2]uint64]*sessiondata.Record, error) {
		out := map[[2]uint64]*sessiondata.Record{}
		for _, r := range refs {
			out[[2]uint64{r.Seq, r.Row}] = recs[[2]uint64{r.Seq, r.Row}]
		}
		return out, nil
	})
	if err != nil || !nav.missing("s4") || nav.missing("s1") || nav.missing("s3") {
		t.Fatalf("%v: s4 %v s1 %v s3 %v", err, nav.missing("s4"), nav.missing("s1"), nav.missing("s3"))
	}
}

// An input that carried an image holds evidence no evaluator here reads.
func TestAnImageIsEvidenceNoOneIsGiven(t *testing.T) {
	nav := givenNav()
	if nav.missing("in/2") {
		t.Fatal("an input with words only is unavailable")
	}
	for i := range nav.inputs {
		if nav.inputs[i].Node == "in/2" {
			nav.inputs[i].Images = 1
		}
	}
	if !nav.missing("in/2") || !nav.missing("big") || nav.missing("s1") {
		t.Fatalf("in/2 %v big %v s1 %v", nav.missing("in/2"), nav.missing("big"), nav.missing("s1"))
	}
}
