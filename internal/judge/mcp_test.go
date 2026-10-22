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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/apache/skywalking-ai-sessionizer/pkg/changes"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

func serve(t *testing.T, lines ...string) []map[string]any {
	t.Helper()
	nav := NewNav(doc(), Compute(doc(), "claude-code/1", nil, nil), nil)
	var out bytes.Buffer
	var calls []string
	if err := ServeMCP(strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, nav, func(tool string, _ json.RawMessage, _ int, _ error) {
		calls = append(calls, tool)
	}); err != nil {
		t.Fatal(err)
	}
	var replies []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		replies = append(replies, m)
	}
	return replies
}

// The server speaks the protocol an agent CLI expects: the version it asked
// for, the tools marked read-only, a tool's answer as text, a tool's failure
// as a result the model reads, and nothing for a notification.
func TestTheNavigationServerSpeaksMCP(t *testing.T) {
	r := serve(t,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"talks","arguments":{"talks":["talk/2"]}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"talk","arguments":{"id":"nothing"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"resources/list"}`,
		`not json`,
	)
	if len(r) != 6 {
		t.Fatalf("%d replies, want 6: %v", len(r), r)
	}
	if v := r[0]["result"].(map[string]any)["protocolVersion"]; v != "2025-03-26" {
		t.Fatalf("version %v", v)
	}
	for _, tool := range r[1]["result"].(map[string]any)["tools"].([]any) {
		ann := tool.(map[string]any)["annotations"].(map[string]any)
		if ann["readOnlyHint"] != true || ann["destructiveHint"] != false {
			t.Fatalf("%v is not marked read-only", tool)
		}
	}
	text := r[2]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, `"id":"talk/2"`) || strings.Contains(text, `"id":"talk/1"`) {
		t.Fatalf("talks filtered to talk/2 gave %s", text)
	}
	if r[3]["result"].(map[string]any)["isError"] != true {
		t.Fatalf("an unknown talk was not an error result: %v", r[3])
	}
	if r[4]["error"].(map[string]any)["code"].(float64) != -32601 || r[5]["error"].(map[string]any)["code"].(float64) != -32700 {
		t.Fatalf("errors %v %v", r[4], r[5])
	}
}

// A page is never larger than the bound, however many rows there are, and the
// pages together hold every row once.
func TestPagesAreBoundedAndComplete(t *testing.T) {
	nav := NewNav(doc(), Compute(doc(), "claude-code/1", nil, nil), nil)
	var rows []any
	for i := 0; i < 400; i++ {
		rows = append(rows, map[string]string{"row": strings.Repeat("r", 100)})
	}
	first := nav.paged(rows, 1)
	seen := 0
	for p := 1; p <= first.Pages; p++ {
		pg := nav.paged(rows, p)
		b, _ := json.Marshal(pg.Rows)
		if len(b) > PageBytes+2 {
			t.Fatalf("page %d holds %d bytes", p, len(b))
		}
		seen += len(pg.Rows)
	}
	if seen != 400 || first.Pages < 2 || len(nav.paged(rows, first.Pages+1).Rows) != 0 {
		t.Fatalf("pages %d hold %d rows", first.Pages, seen)
	}
}

// A row too large for a page stands as a note, so no answer passes the
// bound, and a diff is paged line by line, so a hunk larger than a page can
// still be read.
func TestNoAnswerPassesThePage(t *testing.T) {
	nav := NewNav(doc(), Compute(doc(), "claude-code/1", nil, nil), nil)
	huge := strings.Repeat("<x>", PageBytes)
	pg := nav.paged([]any{map[string]string{"row": "small"}, map[string]string{"row": huge}}, 1)
	b, _ := json.Marshal(pg)
	if len(b) > PageBytes {
		t.Fatalf("a page of %d bytes", len(b))
	}
	if _, ok := pg.Rows[len(pg.Rows)-1].(oversized); !ok && pg.Pages == 1 {
		t.Fatalf("the oversized row was not replaced: %T", pg.Rows[len(pg.Rows)-1])
	}
	d := doc()
	lines := make([]string, 2000)
	for i := range lines {
		lines[i] = "+" + strings.Repeat("y", 40)
	}
	d.WorkspaceChanges = []sessionview.WorkspaceChange{{Step: "c1"}}
	d.WorkspaceChanges[0].ID = "w1"
	d.WorkspaceChanges[0].Changes = []changes.FileChange{{Path: "a.go", Hunks: []changes.Hunk{{NewStart: 1, NewLines: 2000, Lines: lines}}}}
	nav = NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
	first, err := nav.Call("diff", json.RawMessage(`{"id":"w1","path":"a.go"}`))
	if err != nil {
		t.Fatal(err)
	}
	p := first.(page)
	if p.Pages < 2 || p.Total != 2001 {
		t.Fatalf("the diff is %d rows on %d pages", p.Total, p.Pages)
	}
	for i := 1; i <= p.Pages; i++ {
		out, _ := nav.Call("diff", json.RawMessage(fmt.Sprintf(`{"id":"w1","path":"a.go","page":%d}`, i)))
		if b, _ := json.Marshal(out); len(b) > PageBytes {
			t.Fatalf("diff page %d holds %d bytes", i, len(b))
		}
	}
}

// Text that escaping makes several times its size still answers within a
// page, in a step and in a slice of a record.
func TestEscapedTextStaysWithinThePage(t *testing.T) {
	d := doc()
	heavy := strings.Repeat("<\"", 1000)
	d.Talks[0].Children[0].Text, d.Talks[0].Children[0].Result = heavy, heavy
	read := func(uint64, uint64) (*sessiondata.Record, error) {
		return &sessiondata.Record{Label: strings.Repeat("<\"\\", 9000)}, nil
	}
	nav := NewNav(d, Compute(d, "claude-code/1", nil, nil), read)
	for _, call := range []struct{ tool, args string }{{"step", `{"id":"c1"}`}, {"record", `{"seq":1,"row":1}`}} {
		out, err := nav.Call(call.tool, json.RawMessage(call.args))
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := json.Marshal(out); len(b) > PageBytes {
			t.Errorf("%s answered %d bytes", call.tool, len(b))
		}
	}
}

// The navigation server prices from the run's ledger: the rates it applied,
// and nothing else.
func TestPricesComeFromTheLedger(t *testing.T) {
	ledger := &sessionjudge.File{Prices: []sessionjudge.Price{
		{Model: "m", Key: "m", Currency: "USD", PerMillion: sessionjudge.Rates{In: 1}},
		{Model: "m", Key: "m", From: "2026-01-01T00:00:00Z", Currency: "USD", PerMillion: sessionjudge.Rates{In: 2}},
	}}
	p, err := PricingOf(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if p.Lookup("m", 1).PerMillion.In != 1 || p.Lookup("m", 1767225600001).PerMillion.In != 2 || p.Lookup("other", 1) != nil {
		t.Fatal("the ledger's rates were not kept")
	}
}

// A step with thousands of fragments, and a change record naming thousands
// of files, still answer within a page, with their addresses.
func TestManyRefsAndFilesStayWithinThePage(t *testing.T) {
	d := doc()
	for i := 0; i < 4000; i++ {
		d.Talks[0].Children[0].Refs = append(d.Talks[0].Children[0].Refs, sessionflow.Ref{Seq: uint64(i), Row: 1})
	}
	d.WorkspaceChanges = []sessionview.WorkspaceChange{{Step: "c1", Ref: sessionflow.Ref{Seq: 9, Row: 4}}}
	d.WorkspaceChanges[0].ID = "w1"
	for i := 0; i < 3000; i++ {
		d.WorkspaceChanges[0].Changes = append(d.WorkspaceChanges[0].Changes, changes.FileChange{Path: fmt.Sprintf("dir/file-%04d.go", i)})
	}
	nav := NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
	for _, call := range []struct{ tool, args string }{{"step", `{"id":"c1"}`}, {"changes", `{}`}} {
		out, err := nav.Call(call.tool, json.RawMessage(call.args))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(out)
		if len(b) > PageBytes {
			t.Errorf("%s answered %d bytes", call.tool, len(b))
		}
		if call.tool == "changes" && (!strings.Contains(string(b), `"seq":9,"row":4`) || !strings.Contains(string(b), `"files_total":3000`)) {
			t.Errorf("the change row lost its address: %.300s", b)
		}
	}
}

// The subtasks tool reads the tree one level at a time, and lists a stream no
// single step started with its reason rather than leaving it out.
func TestSubtasksAreReadOneLevelAtATime(t *testing.T) {
	d := doc()
	nav := NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
	ids := func(args string) []string {
		t.Helper()
		ans, err := nav.Call("subtasks", json.RawMessage(args))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range ans.(page).Rows {
			row := r.(subtaskRow)
			out = append(out, row.ID+":"+row.Reason)
		}
		return out
	}
	if got := strings.Join(ids(`{}`), ","); got != "stream/a:,stream/c:several starting steps,stream/d:no starting step" {
		t.Fatalf("top level %s", got)
	}
	if got := strings.Join(ids(`{"parent":"stream/a"}`), ","); got != "stream/b:" {
		t.Fatalf("under a %s", got)
	}
	if got := strings.Join(ids(`{"talks":["talk/2"]}`), ","); got != "" {
		t.Fatalf("talk 2 started none alone: %s", got)
	}
	ans, _ := nav.Call("subtasks", json.RawMessage(`{"talks":["talk/1"]}`))
	a := ans.(page).Rows[0].(subtaskRow)
	if a.Nested != 1 || a.Cost.Calls != 3 || a.Cost.Scope != sessionjudge.ScopeSubtask {
		t.Fatalf("a %+v", a)
	}
	if _, err := nav.Call("subtasks", json.RawMessage(`{"parent":"stream/x"}`)); err == nil {
		t.Fatal("an unknown parent was answered")
	}
}

// A call that would read past the budget is refused whole, never cut, and
// what was refused is counted apart from what was read.
func TestABudgetRefusesWhatWouldCrossIt(t *testing.T) {
	nav := NewNav(doc(), Compute(doc(), "claude-code/1", nil, nil), nil)
	card, _ := nav.Call("card", nil)
	b, _ := json.Marshal(card)
	nav.ResetBudget(len(b) + 10)
	if _, err := nav.Call("card", nil); err != nil {
		t.Fatalf("the first card: %v", err)
	}
	if _, err := nav.Call("talks", nil); !errors.Is(err, ErrBudgetSpent) {
		t.Fatalf("a read past the budget: %v", err)
	}
	if read, refused := nav.Spent(); read != len(b) || refused != 1 {
		t.Fatalf("read %d refused %d", read, refused)
	}
}

// Once the budget is spent the server answers that it is, a few times, and
// then ends, so an evaluator that keeps asking has nothing left to ask.
func TestTheServerEndsOnceTheBudgetIsSpent(t *testing.T) {
	nav := NewNav(doc(), Compute(doc(), "claude-code/1", nil, nil), nil)
	nav.ResetBudget(1)
	var lines []string
	for i := 1; i <= 6; i++ {
		lines = append(lines, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"card","arguments":{}}}`, i))
	}
	var out bytes.Buffer
	answered := 0
	if err := ServeMCP(strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, nav, func(string, json.RawMessage, int, error) { answered++ }); err != nil {
		t.Fatal(err)
	}
	if answered != budgetRefusals || !strings.Contains(out.String(), "reading budget of this call is spent") {
		t.Fatalf("answered %d: %s", answered, out.String())
	}
}

// The images an input carried are counted from its landed record, whose
// words are all the view keeps.
func TestImagesAreCountedFromTheRecord(t *testing.T) {
	rec := &sessiondata.Record{Parts: []sessiondata.Part{
		{Kind: "media", Media: "image/png", Bytes: 75076},
		{Kind: "text", Text: "the widget setup does not make sense"},
		{Kind: "data", Data: json.RawMessage(`{"type":"queued_command","prompt":[{"type":"image","source":{}},{"type": "image"}]}`)},
	}}
	if n, size := imagesOf(rec); n != 3 || size < 75076 {
		t.Fatalf("images %d, %d bytes", n, size)
	}
}

// Sub-tasks no main talk started can start each other, and one can start
// itself. Counting what is under each must still end, and count each once.
func TestALoopOfSubtasksIsCountedOnce(t *testing.T) {
	d := doc()
	d.Streams = append(d.Streams, sessionview.Stream{ID: "stream/e", Name: "e"}, sessionview.Stream{ID: "stream/f", Name: "f"},
		sessionview.Stream{ID: "stream/g", Name: "g"})
	d.Talks = append(d.Talks,
		sessionview.Node{ID: "talk/e", Kind: "talk", Stream: "e", Children: []sessionview.Node{{ID: "se", Kind: "tool", Result: "ok"}}},
		sessionview.Node{ID: "talk/f", Kind: "talk", Stream: "f", Children: []sessionview.Node{{ID: "sf", Kind: "tool", Result: "ok"}}},
		sessionview.Node{ID: "talk/g", Kind: "talk", Stream: "g", Children: []sessionview.Node{{ID: "sg", Kind: "tool", Result: "ok"}}})
	d.Relations = append(d.Relations,
		sessionview.Relation{ID: "ef", Type: model.RelStarts, From: "se", To: "stream/f"},
		sessionview.Relation{ID: "fe", Type: model.RelStarts, From: "sf", To: "stream/e"},
		sessionview.Relation{ID: "gg", Type: model.RelStarts, From: "sg", To: "stream/g"})
	nav := NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
	nested := map[string]int{}
	for _, parent := range []string{"stream/e", "stream/f", "stream/g"} {
		ans, err := nav.Call("subtasks", json.RawMessage(`{"parent":"`+parent+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range ans.(page).Rows {
			nested[r.(subtaskRow).ID] = r.(subtaskRow).Nested
		}
	}
	if nested["stream/e"] != 1 || nested["stream/f"] != 1 || nested["stream/g"] != 0 {
		t.Fatalf("nested %v", nested)
	}
}

// A record can hold the results of several tools. Searching one step reads
// only the part its reference names, and says when it stopped at the
// most matches one search gives.
func TestSearchOfAStepReadsOnlyItsOwnPart(t *testing.T) {
	d := doc()
	one := 1
	d.Talks[0].Children = append(d.Talks[0].Children, sessionview.Node{ID: "second", Kind: "tool", Result: "r",
		Ref: &sessionflow.Ref{Seq: 3, Row: 1}, Refs: []sessionflow.Ref{{Seq: 3, Row: 1, Block: &one}}})
	nav := NewNav(d, Compute(d, "claude-code/1", nil, nil), func(_, _ uint64) (*sessiondata.Record, error) {
		return &sessiondata.Record{Parts: []sessiondata.Part{{Kind: "result", Text: "the first tool's token"},
			{Kind: "result", Text: "the second tool's" + strings.Repeat(" token", maxHits+5)}}}, nil
	})
	ans, err := nav.Call("search", json.RawMessage(`{"query":"first tool","node":"second"}`))
	if err != nil {
		t.Fatal(err)
	}
	if rows := ans.(page).Rows; len(rows) != 0 {
		t.Fatalf("the search found another tool's result: %+v", rows)
	}
	ans, _ = nav.Call("search", json.RawMessage(`{"query":"token","node":"second"}`))
	rows := ans.(page).Rows
	if len(rows) != maxHits+1 || ans.(page).Pages != 1 {
		t.Fatalf("%d rows", len(rows))
	}
	if last, _ := json.Marshal(rows[maxHits]); !strings.Contains(string(last), "search again with skip 50") {
		t.Fatalf("the search did not say how to go on: %s", last)
	}
	// Going on gives the matches after them, ending with the last.
	ans, _ = nav.Call("search", json.RawMessage(`{"query":"token","node":"second","skip":50}`))
	rest := ans.(page).Rows
	if len(rest) != 5 || *rest[0].(hit).Offset != *rows[maxHits-1].(hit).Offset+len(" token") {
		t.Fatalf("after skipping: %+v", rest)
	}
	// A record landed after the head the run judges is not searched.
	nav.throughSeq = 2
	if ans, _ := nav.Call("search", json.RawMessage(`{"query":"token","node":"second"}`)); len(ans.(page).Rows) != 0 {
		t.Fatal("a record past the head was searched")
	}
}

// A part of a record is read from the offset search gives, in slices, and
// only within the records the run judges.
func TestARecordPartIsReadFromItsOffset(t *testing.T) {
	d := doc()
	d.Talks[0].Children = append(d.Talks[0].Children, sessionview.Node{ID: "long", Kind: "tool", Result: "r",
		Ref: &sessionflow.Ref{Seq: 3, Row: 1}})
	text := strings.Repeat("a", SliceBytes) + "the place"
	nav := NewNav(d, Compute(d, "claude-code/1", nil, nil), func(_, _ uint64) (*sessiondata.Record, error) {
		return &sessiondata.Record{Parts: []sessiondata.Part{{Kind: "text", Text: "head"}, {Kind: "result", Text: text}}}, nil
	})
	ans, err := nav.Call("record", json.RawMessage(fmt.Sprintf(`{"seq":3,"row":1,"part":1,"offset":%d}`, SliceBytes)))
	if err != nil {
		t.Fatal(err)
	}
	if got := ans.(map[string]any); got["slice"] != "the place" || got["bytes"] != len(text) || got["next_offset"] != nil {
		t.Fatalf("part %+v", got)
	}
	ans, _ = nav.Call("record", json.RawMessage(`{"seq":3,"row":1,"part":1}`))
	if got := ans.(map[string]any); len(got["slice"].(string)) != SliceBytes || got["next_offset"] != SliceBytes {
		t.Fatalf("first slice %d bytes, next %v", len(got["slice"].(string)), got["next_offset"])
	}
	if _, err := nav.Call("record", json.RawMessage(`{"seq":3,"row":1,"part":2}`)); err == nil {
		t.Fatal("a part the record does not have was read")
	}
	// Text that escapes to several times its size still fits a page.
	escaped := NewNav(d, Compute(d, "claude-code/1", nil, nil), func(_, _ uint64) (*sessiondata.Record, error) {
		return &sessiondata.Record{Parts: []sessiondata.Part{{Kind: "result", Text: strings.Repeat("<", 3*SliceBytes)}}}, nil
	})
	ans, _ = escaped.Call("record", json.RawMessage(`{"seq":3,"row":1,"part":0}`))
	if written, _ := json.Marshal(ans); len(written) > PageBytes {
		t.Fatalf("a slice of %d bytes once written", len(written))
	}
	// Nothing past the head the run judges, nor outside its window.
	nav.throughSeq = 2
	if _, err := nav.Call("record", json.RawMessage(`{"seq":3,"row":1,"part":1}`)); err == nil {
		t.Fatal("a part landed after the head was read")
	}
	nav.throughSeq, nav.refs = 0, map[[2]uint64]bool{{3, 2}: true}
	if _, err := nav.Call("record", json.RawMessage(`{"seq":3,"row":1,"part":1}`)); err == nil {
		t.Fatal("a part outside the window was read")
	}
}

// The targets tool gives the targets the call was given, and no other.
func TestTheTargetsToolGivesWhatTheCallWasGiven(t *testing.T) {
	nav := NewNav(doc(), Compute(doc(), "claude-code/1", nil, nil), nil)
	nav.Assign(Assignment{Catalogue: []sessionjudge.Target{
		{ID: "t1", Statement: "fix the build", Status: sessionjudge.TargetOpen, Criteria: []sessionjudge.Criterion{{ID: "t1.c1", Text: "it builds"}},
			Cite: []sessionjudge.Cite{{Node: "in/1"}}},
		{ID: "t2", Statement: "add a page", Status: sessionjudge.TargetClosed}}})
	ans, err := nav.Call("targets", nil)
	if err != nil || ans.(page).Total != 2 {
		t.Fatalf("targets %+v %v", ans, err)
	}
	one, err := nav.Call("targets", json.RawMessage(`{"id":"t1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(one); !strings.Contains(string(b), `"cite":[{"node":"in/1"}]`) || !strings.Contains(string(b), `"text":"it builds"`) {
		t.Fatalf("t1 %s", b)
	}
	// A target larger than a page is read in pages, every criterion whole,
	// even one larger than a page, in pieces.
	var many []sessionjudge.Criterion
	for i := range 40 {
		many = append(many, sessionjudge.Criterion{ID: fmt.Sprintf("t3.c%d", i+1), Text: strings.Repeat("a requirement ", 60)})
	}
	huge := strings.Repeat("<", 3*PageBytes)
	many = append(many, sessionjudge.Criterion{ID: "t3.c41", Text: huge})
	nav.Assign(Assignment{Catalogue: []sessionjudge.Target{{ID: "t3", Statement: huge, Criteria: many}}})
	first, _ := nav.Call("targets", json.RawMessage(`{"id":"t3"}`))
	text := map[string]string{}
	for p := 1; p <= first.(page).Pages; p++ {
		ans, _ := nav.Call("targets", json.RawMessage(fmt.Sprintf(`{"id":"t3","page":%d}`, p)))
		if written, _ := json.Marshal(ans); len(written) > PageBytes {
			t.Fatalf("page %d is %d bytes", p, len(written))
		}
		for _, r := range ans.(page).Rows {
			if m, ok := r.(map[string]any); ok {
				if s, ok := m["statement"].(string); ok {
					text["statement"] += s
				}
				if id, ok := m["id"].(string); ok && id != "t3" {
					text[id] += m["text"].(string)
				}
			}
		}
	}
	if len(text) != 42 || text["statement"] != huge || text["t3.c41"] != huge || text["t3.c1"] != many[0].Text {
		t.Fatalf("%d pages gave %d texts", first.(page).Pages, len(text))
	}
	if _, err := nav.Call("targets", json.RawMessage(`{"id":"t9"}`)); err == nil {
		t.Fatal("a target the call was not given was answered")
	}
}

// A talk read with only gives the steps of that sort and no other.
func TestATalkCanBeReadForOneSortOfStep(t *testing.T) {
	failed := true
	d := doc()
	d.Talks[0].Children = append(d.Talks[0].Children,
		sessionview.Node{ID: "m1", Kind: "message.assistant", Text: "done"},
		sessionview.Node{ID: "bad", Kind: "tool", Name: "Bash", Result: "FAIL", Failed: &failed})
	nav := NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
	ids := func(only string) string {
		t.Helper()
		ans, err := nav.Call("talk", json.RawMessage(`{"id":"talk/1","only":"`+only+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range ans.(page).Rows {
			out = append(out, r.(outlineRow).ID)
		}
		return strings.Join(out, ",")
	}
	if got := ids("failed"); got != "bad" {
		t.Fatalf("failed %s", got)
	}
	if got := ids("messages"); got != "m1" {
		t.Fatalf("messages %s", got)
	}
	if got := ids("tools"); got != "s1,s3a,bad" {
		t.Fatalf("tools %s", got)
	}
}

// A call's tools are answered while a progress line reads what the call has
// read so far. The race detector, which the suite runs under, fails this
// test if the counts are not guarded.
func TestTheCountsCanBeReadWhileACallReads(t *testing.T) {
	nav := NewNav(doc(), Compute(doc(), "claude-code/1", nil, nil), nil)
	nav.ResetBudget(1 << 30)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				nav.Spent()
				nav.Calls()
			}
		}
	}()
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				_, _ = nav.Call("talks", nil)
			}
		}()
	}
	wg.Wait()
	close(stop)
	<-done
	if read, _ := nav.Spent(); nav.Calls() != 80 || read == 0 {
		t.Fatalf("%d calls, %d bytes read", nav.Calls(), read)
	}
}

// whole reads one row of a tool's answer whole, in slices, by the item its
// note names, and fails the test when an answer passes the page.
func whole(t *testing.T, nav *Nav, tool, args string, item int) string {
	t.Helper()
	var b strings.Builder
	for offset := 0; ; {
		ans, err := nav.Call(tool, json.RawMessage(fmt.Sprintf(`{%s"item":%d,"offset":%d}`, args, item, offset)))
		if err != nil {
			t.Fatal(err)
		}
		if written, _ := json.Marshal(ans); len(written) > PageBytes {
			t.Fatalf("a slice of %d bytes", len(written))
		}
		m := ans.(map[string]any)
		b.WriteString(m["slice"].(string))
		next, ok := m["next_offset"].(int)
		if !ok {
			return b.String()
		}
		offset = next
	}
}

// A row too large for a page names its item, and is read whole by it: a
// target that cites thousands of inputs, a sub-task with thousands of talks,
// a step that started a sub-task with thousands.
func TestARowTooLargeForAPageIsReadWhole(t *testing.T) {
	var talks []string
	var cites []sessionjudge.Cite
	for i := range 2000 {
		talks = append(talks, fmt.Sprintf("talk/b/%d", i))
		cites = append(cites, sessionjudge.Cite{Node: fmt.Sprintf("input/%d", i)})
	}
	d := doc()
	for i := range talks[1:] {
		d.Talks = append(d.Talks, sessionview.Node{ID: talks[i+1], Kind: "talk", Stream: "b"})
	}
	d.Talks[2].ID = talks[0]
	nav := NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
	nav.Assign(Assignment{Catalogue: []sessionjudge.Target{{ID: "t1", Statement: "s", Talks: talks, Cite: cites}}})
	read := func(tool, args string, want string) {
		t.Helper()
		ans, err := nav.Call(tool, json.RawMessage("{"+strings.TrimSuffix(args, ",")+"}"))
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range ans.(page).Rows {
			if o, ok := r.(oversized); ok {
				if got := whole(t, nav, tool, args, o.Row); !strings.Contains(got, want) {
					t.Fatalf("%s item %d read whole lacks %s", tool, o.Row, want)
				}
				return
			}
		}
		t.Fatalf("%s gave no oversized row", tool)
	}
	read("targets", `"id":"t1",`, `"input/1999"`)
	read("subtasks", `"parent":"stream/a",`, `"talk/b/1999"`)
	read("talk", `"id":"talk/a",`, `"talk/b/1999"`)
	if _, err := nav.Call("targets", json.RawMessage(`{"id":"t1","item":99}`)); err == nil {
		t.Fatal("an item past the rows was read")
	}
}

// A long text is cut into pieces that each end on a whole character, and
// that join back into the text.
func TestPiecesEndOnWholeCharacters(t *testing.T) {
	text := strings.Repeat("中", 3000)
	ps := pieces(text)
	if len(ps) < 2 || strings.Join(ps, "") != text {
		t.Fatalf("%d pieces", len(ps))
	}
	for i, p := range ps {
		if !utf8.ValidString(p) || len(p) > pieceBytes {
			t.Fatalf("piece %d is %d bytes, valid %v", i, len(p), utf8.ValidString(p))
		}
	}
	if ps := pieces(""); len(ps) != 1 || ps[0] != "" {
		t.Fatalf("an empty text %q", ps)
	}
}

// The card stays within a page however long the session's title is, and so
// does every prompt that gives it.
func TestTheCardStaysWithinAPage(t *testing.T) {
	d := doc()
	d.Summary.Title = strings.Repeat("a long title ", 2000)
	nav := NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
	card, err := nav.Call("card", nil)
	if err != nil {
		t.Fatal(err)
	}
	if written, _ := json.Marshal(card); len(written) > PageBytes {
		t.Fatalf("a card of %d bytes", len(written))
	}
}

// Search ignores case without moving its offsets: a letter whose other case
// takes more bytes, as Ⱥ and ⱥ do, neither breaks the search nor points it
// at the wrong bytes.
func TestSearchKeepsOffsetsWhereCaseChangesSize(t *testing.T) {
	text := strings.Repeat("Ⱥ", 100) + "needle and NEEDLE"
	d := doc()
	d.Talks[0].Children = append(d.Talks[0].Children, sessionview.Node{ID: "wide", Kind: "tool", Result: text,
		Ref: &sessionflow.Ref{Seq: 3, Row: 1}})
	nav := NewNav(d, Compute(d, "claude-code/1", nil, nil), func(_, _ uint64) (*sessiondata.Record, error) {
		return &sessiondata.Record{Parts: []sessiondata.Part{{Kind: "result", Text: text}}}, nil
	})
	ans, err := nav.Call("search", json.RawMessage(`{"query":"needle","node":"wide"}`))
	if err != nil {
		t.Fatal(err)
	}
	rows := ans.(page).Rows
	if len(rows) != 2 || *rows[0].(hit).Offset != 200 || *rows[1].(hit).Offset != 211 {
		t.Fatalf("hits %+v", rows)
	}
	// The search over the document shows the match it found.
	ans, _ = nav.Call("search", json.RawMessage(`{"query":"ⱥⱥneedle"}`))
	if rows := ans.(page).Rows; len(rows) != 1 || !strings.Contains(rows[0].(hit).Snippet, "ȺȺneedle") {
		t.Fatalf("the search over the document %+v", rows)
	}
}
