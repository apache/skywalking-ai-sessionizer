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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/verify"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// TestAMarkedSummaryResetsTheContext runs the summarized capture: a real
// SummarizationMiddleware, which replaced the history with a summary twice in
// four turns. Each summary is sent again with the calls after it.
//
// The conversation has to come out the way a Claude Code compaction does:
// three epochs in main, each reset a boundary with its summary. Every call
// before a boundary was not sent its summary, and every call after it was.
// The last call before it is the summariser, whose answer the summary carries.
func TestAMarkedSummaryResetsTheContext(t *testing.T) {
	zone, sessions := land(t, "summarized")
	if len(sessions) != 1 {
		t.Fatalf("%d sessions, want 1", len(sessions))
	}
	session := sessions[0]
	view := parsed(t, zone, session)
	resetsAre(t, view, 2)

	// Each reset landed once, although its summary was sent four times.
	if got := resetRecords(t, zone, session); got != 4 {
		t.Errorf("%d reset records landed, want 4: a summary sent again is not a new reset", got)
	}

	held := heldBodies(t, zone, session)
	type call struct {
		at      sessionflow.Ref
		id, out string
	}
	var calls []call
	for _, n := range view.Nodes {
		if n.Kind == model.KindLLMCall && n.Ref != nil {
			r := recordAt(t, zone, session, *n.Ref)
			calls = append(calls, call{*n.Ref, r.ID, ""})
		}
	}
	for i := range calls {
		// The call's answer is on the record that carries its end.
		eachRecord(t, zone, session, func(_, _ uint64, rec sessiondata.Record) {
			if rec.Call == calls[i].id && landedRecord(rec).text() != "" {
				calls[i].out = landedRecord(rec).text()
			}
		})
	}
	// The resets in landed order, each with its message and its summary.
	type reset struct {
		at               sessionflow.Ref
		message, summary string
	}
	var resets []reset
	for _, r := range view.Relations {
		if r.Type != model.RelSummarizes {
			continue
		}
		var b, s sessionflow.Ref
		for _, n := range view.Nodes {
			switch n.ID {
			case r.To:
				b = *n.Ref
			case r.From:
				s = *n.Ref
			}
		}
		id := recordAt(t, zone, session, b).ID
		resets = append(resets, reset{b,
			strings.TrimSuffix(strings.TrimPrefix(id, "main/"), ":reset"),
			recordAt(t, zone, session, s).text()})
	}
	sort.Slice(resets, func(i, j int) bool { return before(resets[i].at, resets[j].at) })
	sort.Slice(calls, func(i, j int) bool { return before(calls[i].at, calls[j].at) })

	for i, c := range calls {
		request, err := held.Body(c.id + ":request")
		if err != nil {
			t.Fatalf("%s: %v", c.id, err)
		}
		latest := -1
		for k, r := range resets {
			if before(r.at, c.at) {
				latest = k
			}
		}
		// The call just before a reset is the summariser that wrote it.
		if i+1 < len(calls) {
			for _, r := range resets {
				if before(c.at, r.at) && before(r.at, calls[i+1].at) &&
					(!strings.HasPrefix(c.out, "summary ") || !strings.HasSuffix(r.summary, c.out)) {
					t.Errorf("the call before reset %s answered %q, not its summary %q", r.message, c.out, r.summary)
				}
			}
		}
		if strings.HasPrefix(c.out, "summary ") {
			continue
		}
		for k, r := range resets {
			sent := strings.Contains(string(request), r.message)
			if k == latest && !sent {
				t.Errorf("%s lands after reset %s and was not sent its summary", c.id, r.message)
			}
			if k > latest && sent {
				t.Errorf("%s was sent summary %s and lands before its reset", c.id, r.message)
			}
		}
	}
	if len(resets) != 2 {
		t.Errorf("%d resets paired with a summary, want 2", len(resets))
	}

	report, err := verify.Session(zone, session)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if report.Problems != 0 {
		t.Errorf("verify found %d problems", report.Problems)
	}
}

func before(a, b sessionflow.Ref) bool {
	return a.Seq < b.Seq || (a.Seq == b.Seq && a.Row < b.Row)
}

// TestAResetIsNotRepeatedWhenTheShapeIsLost. The shape is what remembers a
// reset already landed, and it is disposable. Without it the same capture
// lands every reset record again. They carry the same ids, so the index keeps
// the first, and the next round changes nothing.
func TestAResetIsNotRepeatedWhenTheShapeIsLost(t *testing.T) {
	zone, sessions := land(t, "summarized")
	session := sessions[0]
	resetsAre(t, parsed(t, zone, session), 2)
	if err := os.Remove(filepath.Join(zone.SessionDir(session), "langsmith.shape.json")); err != nil {
		t.Fatal(err)
	}
	landInto(t, zone, "summarized")
	resetsAre(t, parsed(t, zone, session), 2)
}

// TestARedeliveredCaptureLandsNoResetAgain. The client sends a batch again
// when it is not sure the first arrived. With the shape intact, no reset
// record is landed a second time.
func TestARedeliveredCaptureLandsNoResetAgain(t *testing.T) {
	zone, sessions := land(t, "summarized")
	session := sessions[0]
	landInto(t, zone, "summarized")
	if got := resetRecords(t, zone, session); got != 4 {
		t.Errorf("%d reset records after a redelivery, want 4", got)
	}
	resetsAre(t, parsed(t, zone, session), 2)
}

// TestAForwardedSummaryResetsEachStream. An epoch belongs to a stream. A
// summary forwarded into a nested agent's history resets that agent as well
// as the one that wrote it, so each stream has its own reset.
func TestAForwardedSummaryResetsEachStream(t *testing.T) {
	zone, post, collect := receiveInto(t)
	const trace = "31313131-3131-7131-8131-313131313130"
	const main1 = "31313131-3131-7131-8131-313131313131"
	const toolID = "31313131-3131-7131-8131-313131313132"
	const inner = "31313131-3131-7131-8131-313131313133"
	const child1 = "31313131-3131-7131-8131-313131313134"
	owner := `"session_name":"asz","extra":{"metadata":{"thread_id":"t-forwarded"}}`
	root := "20260920T100000000000Z" + trace
	toolDotted := root + ".20260920T100002000000Z" + toolID
	innerDotted := toolDotted + ".20260920T100003000000Z" + inner
	marked := `{"lc":1,"type":"constructor","id":["langchain","schema","messages","HumanMessage"],` +
		`"kwargs":{"content":"Here is a summary of the conversation to date:\n\nall healthy",` +
		`"additional_kwargs":{"lc_source":"summarization"},"type":"human","id":"sum-forwarded"}}`
	llm := func(id, parent, dotted, at, text string) string {
		return `{"id":"` + id + `","trace_id":"` + trace + `","parent_run_id":"` + parent + `",` +
			`"dotted_order":"` + dotted + `","run_type":"llm","name":"ChatOpenAI",` +
			`"start_time":"2026-09-20T10:00:0` + at + `Z","end_time":"2026-09-20T10:00:0` + at + `Z",` + owner + `,` +
			`"inputs":{"messages":[[` + marked + `,{"role":"user","content":"go on"}]]},` +
			`"outputs":{"generations":[[{"message":{"kwargs":{"content":"` + text + `"}}}]]}}`
	}
	post(`{"post":[{"id":"` + trace + `","trace_id":"` + trace + `","dotted_order":"` + root + `",` +
		`"run_type":"chain","name":"agent","start_time":"2026-09-20T10:00:00Z","end_time":"2026-09-20T10:00:09Z",` + owner + `,` +
		`"inputs":{"messages":[{"role":"user","content":"go on"}]}},` +
		llm(main1, trace, root+".20260920T100001000000Z"+main1, "1", "delegating") + `,` +
		`{"id":"` + toolID + `","trace_id":"` + trace + `","parent_run_id":"` + trace + `",` +
		`"dotted_order":"` + toolDotted + `","run_type":"tool","name":"delegate",` +
		`"start_time":"2026-09-20T10:00:02Z","end_time":"2026-09-20T10:00:08Z",` + owner + `,` +
		`"outputs":{"output":{"type":"tool","tool_call_id":"call_delegate_1","content":"done"}}},` +
		`{"id":"` + inner + `","trace_id":"` + trace + `","parent_run_id":"` + toolID + `",` +
		`"dotted_order":"` + innerDotted + `","run_type":"chain","name":"analyst",` +
		`"start_time":"2026-09-20T10:00:03Z","end_time":"2026-09-20T10:00:07Z",` + owner + `},` +
		llm(child1, inner, innerDotted+".20260920T100004000000Z"+child1, "4", "analysed") + `]}`)
	session := collect().Sessions[0]
	view := parsed(t, zone, session)
	byStream := map[string]int{}
	for _, n := range view.Nodes {
		if n.Kind == model.KindEpochBoundary {
			byStream[n.Stream]++
		}
	}
	if len(byStream) != 2 || byStream["main"] != 1 {
		t.Errorf("resets by stream %v, want one in main and one in the nested agent", byStream)
	}
}

func parsed(t *testing.T, zone *storage.Zone, session string) *sessionflow.View {
	t.Helper()
	if _, err := parse.Session(zone, parse.Options{
		Conversation: session, Session: session, Reindex: index.Rebuild}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return fold(t, zone, session)
}

func resetsAre(t *testing.T, view *sessionflow.View, resets int) {
	t.Helper()
	nodes, relations := map[string]int{}, map[string]int{}
	for _, n := range view.Nodes {
		nodes[n.Kind]++
	}
	for _, r := range view.Relations {
		relations[r.Type]++
	}
	want := map[string]int{model.KindEpoch: resets + 1, model.KindEpochBoundary: resets,
		model.KindEpochSummary: resets, model.KindStream: 1}
	for kind, n := range want {
		if nodes[kind] != n {
			t.Errorf("%d %s, want %d", nodes[kind], kind, n)
		}
	}
	if relations[model.RelFollows] != resets || relations[model.RelSummarizes] != resets {
		t.Errorf("%d follows and %d summarizes, want %d of each",
			relations[model.RelFollows], relations[model.RelSummarizes], resets)
	}
	for _, u := range view.Unresolved {
		t.Errorf("unresolved %s: %s", u.Kind, u.Reason)
	}
}

// landedRecord is a landed record with its text joined.
type landedRecord sessiondata.Record

func (r landedRecord) text() string {
	var b strings.Builder
	for _, p := range r.Parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

func recordAt(t *testing.T, zone *storage.Zone, session string, ref sessionflow.Ref) landedRecord {
	t.Helper()
	var found *landedRecord
	eachRecord(t, zone, session, func(seq, row uint64, rec sessiondata.Record) {
		if seq == ref.Seq && row == ref.Row {
			r := landedRecord(rec)
			found = &r
		}
	})
	if found == nil {
		t.Fatalf("no record at seq %d row %d", ref.Seq, ref.Row)
	}
	return *found
}

func resetRecords(t *testing.T, zone *storage.Zone, session string) int {
	t.Helper()
	n := 0
	eachRecord(t, zone, session, func(_, _ uint64, rec sessiondata.Record) {
		for _, f := range rec.Flags {
			if f == "context_reset" || f == "reset_summary" {
				n++
			}
		}
	})
	return n
}

func eachRecord(t *testing.T, zone *storage.Zone, session string, fn func(seq, row uint64, rec sessiondata.Record)) {
	t.Helper()
	files, err := storage.LandedFiles(zone, session)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		fh, err := os.Open(f.Path)
		if err != nil {
			t.Fatal(err)
		}
		reader, err := sessiondata.NewReader(fh)
		if err != nil {
			fh.Close()
			t.Fatal(err)
		}
		for row := uint64(1); ; row++ {
			rec, err := reader.Next()
			if err != nil {
				break
			}
			fn(f.Seq, row, *rec)
		}
		fh.Close()
	}
}

// TestPlacementLeavesTheRequestAsItArrived. A request is placed once to check
// it can land and again to land it. When placement changed the request's own
// records, the second placement built on the first, and the link to a plain
// call landed with its auxiliary flag twice.
func TestPlacementLeavesTheRequestAsItArrived(t *testing.T) {
	zone, sessions := land(t, "subagent")
	links := 0
	eachRecord(t, zone, sessions[0], func(_, _ uint64, rec sessiondata.Record) {
		n := 0
		for _, f := range rec.Flags {
			if f == "auxiliary" {
				n++
			}
		}
		if n > 1 {
			t.Errorf("%s carries the auxiliary flag %d times: %v", rec.ID, n, rec.Flags)
		}
		if n == 1 {
			links++
		}
	})
	if links == 0 {
		t.Error("no link to a plain call landed; the capture no longer tests this")
	}
}

// TestABodyNamesThePromptItsCallWasPlacedUnder. A nested agent's call is
// placed under the tool it ran inside, and its request must name the same
// prompt. Read from the request as it arrived, it named the trace.
func TestABodyNamesThePromptItsCallWasPlacedUnder(t *testing.T) {
	for _, kase := range []string{"subagent", "summarized"} {
		zone, sessions := land(t, kase)
		calls, bodies := map[string]string{}, map[string]string{}
		eachRecord(t, zone, sessions[0], func(_, _ uint64, rec sessiondata.Record) {
			if rec.Call != "" && rec.Call == rec.ID {
				calls[rec.Call] = rec.Run
			}
		})
		// A response joins by its call and its request, and names no prompt.
		for id, m := range manifestsOf(t, zone, sessions[0]) {
			if strings.HasSuffix(id, ":request") {
				bodies[id] = m.Run
			}
		}
		checked := 0
		for id, run := range bodies {
			call := id[:strings.LastIndex(id, ":")]
			if want, ok := calls[call]; ok {
				checked++
				if run != want {
					t.Errorf("%s: %s names prompt %q, its call %q", kase, id, run, want)
				}
			}
		}
		if checked == 0 {
			t.Errorf("%s: no body was matched to its call", kase)
		}
	}
}

// TestAResetRecoversWhenOnlyItsBoundaryLanded. A stream's records are cut
// into files at a byte budget, so a boundary, its summary and the call can
// land in three files. A crash after the first leaves the boundary with no
// summary, and the shape unsaved. The client sends the batch again, and the
// conversation has to come out whole: the boundary is kept once and the
// summary joins it.
func TestAResetRecoversWhenOnlyItsBoundaryLanded(t *testing.T) {
	zone := storage.NewZone(t.TempDir())
	session := landIntoBudget(t, zone, "summarized", 1)[0]
	files, err := storage.LandedFiles(zone, session)
	if err != nil {
		t.Fatal(err)
	}
	crashAt := uint64(0)
	for _, f := range files {
		eachRecordIn(t, f.Path, func(rec sessiondata.Record) {
			if crashAt == 0 && strings.HasSuffix(rec.ID, ":reset") {
				crashAt = f.Seq
			}
		})
	}
	if crashAt == 0 {
		t.Fatal("no boundary landed")
	}
	removed := 0
	for _, f := range files {
		if f.Seq > crashAt {
			if err := os.Remove(f.Path); err != nil {
				t.Fatal(err)
			}
			removed++
		}
	}
	if err := os.Remove(filepath.Join(zone.SessionDir(session), "langsmith.shape.json")); err != nil {
		t.Fatal(err)
	}
	t.Logf("crashed after file %d, %d files lost", crashAt, removed)

	landIntoBudget(t, zone, "summarized", 1)
	resetsAre(t, parsed(t, zone, session), 2)
}

func eachRecordIn(t *testing.T, path string, fn func(rec sessiondata.Record)) {
	t.Helper()
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	reader, err := sessiondata.NewReader(fh)
	if err != nil {
		t.Fatal(err)
	}
	for {
		rec, err := reader.Next()
		if err != nil {
			return
		}
		fn(*rec)
	}
}
