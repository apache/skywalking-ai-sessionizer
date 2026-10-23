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
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/pkg/changes"
	"github.com/apache/skywalking-ai-sessionizer/pkg/execution"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// Three main talks. The first starts child a, which works on past the start
// of the second, as 14 children of 0947c9ef did; the third starts child b.
func threeTalks() *sessionview.Conversation {
	ref := func(row uint64) *sessionflow.Ref { return &sessionflow.Ref{Seq: 1, Row: row} }
	c := func(id, stream string, at int64, row uint64) sessionview.Node {
		n := call(id, stream, at, "m", usage(10, 0, 0, 0))
		n.Ref = ref(row)
		return n
	}
	in := func(id string, at int64) sessionview.Node {
		return sessionview.Node{ID: id, Kind: model.KindMessageExternal, At: at, Text: "do it"}
	}
	t1 := sessionview.Node{ID: "talk/main/1", Kind: "talk", Stream: "main", At: 100, From: 100, To: 300, Children: []sessionview.Node{
		in("input/1", 100), c("c1", "main", 110, 1), {ID: "s1", Kind: "tool", At: 120}}}
	ta := sessionview.Node{ID: "talk/a", Kind: "talk", Stream: "a", At: 200, From: 200, To: 900, Children: []sessionview.Node{
		c("ca1", "a", 200, 2), c("ca2", "a", 900, 3)}}
	t2 := sessionview.Node{ID: "talk/main/2", Kind: "talk", Stream: "main", At: 500, From: 500, To: 600, Children: []sessionview.Node{
		in("input/2", 500), c("c2", "main", 550, 4)}}
	t3 := sessionview.Node{ID: "talk/main/3", Kind: "talk", Stream: "main", At: 1000, From: 1000, To: 1100, Children: []sessionview.Node{
		in("input/3", 1000), c("c3", "main", 1010, 5), {ID: "s3", Kind: "tool", At: 1020}}}
	tb := sessionview.Node{ID: "talk/b", Kind: "talk", Stream: "b", At: 1050, From: 1050, To: 1060, Children: []sessionview.Node{
		c("cb", "b", 1050, 6)}}
	return &sessionview.Conversation{
		Conversation: "conv",
		Streams: []sessionview.Stream{{ID: "stream/main", Name: "main", Role: "main"}, {ID: "stream/a", Name: "a", Role: "child"},
			{ID: "stream/b", Name: "b", Role: "child"}},
		Talks: []sessionview.Node{t1, ta, t2, t3, tb},
		Relations: []sessionview.Relation{{ID: "r1", Type: model.RelStarts, From: "s1", To: "stream/a"},
			{ID: "r3", Type: model.RelStarts, From: "s3", To: "stream/b"}},
	}
}

// A window takes whole main talks. The talks before it are context, kept
// with their sub-tasks only as far as the window's end; the talks after it,
// and their sub-tasks, are gone; and the run prices the window alone.
func TestAWindowTakesWholeTalks(t *testing.T) {
	// s1's change was captured in a file of its own.
	three := threeTalks()
	three.WorkspaceChanges = []sessionview.WorkspaceChange{{Step: "s1", Ref: sessionflow.Ref{Seq: 2, Row: 1}}}
	three.ToolExecutions = []sessionview.ToolExecution{{Step: "s1", Ref: sessionflow.Ref{Seq: 3, Row: 1}}}
	doc, sel, err := Narrow(three, Window{FromTalk: "talk/main/2", ToTalk: "talk/main/2"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(sel.Talks, ",") != "talk/main/2" || strings.Join(sel.Before, ",") != "talk/main/1" || sel.End != 600 {
		t.Fatalf("selection %+v", sel)
	}
	var kept []string
	for _, tk := range doc.Talks {
		for _, n := range tk.Children {
			kept = append(kept, n.ID)
		}
	}
	// ca2 was recorded at 900, after the window's end at 600.
	if got := strings.Join(kept, ","); got != "input/1,c1,s1,ca1,input/2,c2" {
		t.Fatalf("kept %s", got)
	}
	l := Compute(doc, "claude-code/1", nil, nil)
	l.Select(sel)
	if l.Total.Calls != 1 || strings.Join(l.Judged(), ",") != "talk/main/2" {
		t.Fatalf("the window prices %+v of %v", l.Total, l.Judged())
	}
	nav := NewNav(doc, l, nil)
	nav.Within(sel)
	if strings.Join(nav.Inputs(), ",") != "input/2" {
		t.Fatalf("inputs to answer %v", nav.Inputs())
	}
	ans, _ := nav.Call("talks", nil)
	var marks []string
	for _, r := range ans.(page).Rows {
		marks = append(marks, r.(talkRow).ID+":"+r.(talkRow).Window)
	}
	if got := strings.Join(marks, ","); got != "talk/main/1:before,talk/main/2:in" {
		t.Fatalf("talks %s", got)
	}
	// c3's record, after the window, is refused before it is read.
	nav.read = func(_, _ uint64) (*sessiondata.Record, error) { return &sessiondata.Record{}, nil }
	if _, err := nav.Call("record", json.RawMessage(`{"seq":1,"row":5}`)); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("a record after the window: %v", err)
	}
	if _, err := nav.Call("step", json.RawMessage(`{"id":"c3"}`)); err == nil {
		t.Fatal("a step after the window was answered")
	}
	// The change and execution records the window keeps are read whole.
	for _, at := range []string{`{"seq":2,"row":1}`, `{"seq":3,"row":1}`} {
		if _, err := nav.Call("record", json.RawMessage(at)); err != nil {
			t.Fatalf("a record the window keeps, %s: %v", at, err)
		}
	}
}

// The sub-tasks of a window's earlier talks are context: the sub-task stage
// is not given them, does not count them as left out, and the close file
// prices none of them. Child a, which talk/main/1 started, works on into
// the window, and child b is the window's own.
func TestAWindowJudgesAndPricesOnlyItsOwnSubtasks(t *testing.T) {
	doc, sel, err := Narrow(threeTalks(), Window{FromTalk: "talk/main/2", ToTalk: "talk/main/3"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	l := Compute(doc, "claude-code/1", nil, nil)
	l.Select(sel)
	nav := NewNav(doc, l, nil)
	nav.Within(sel)
	var given []string
	for _, it := range stageItems(sessionjudge.StageSubtasks, nav, nil) {
		given = append(given, it.id)
	}
	if strings.Join(given, ",") != "stream/b" {
		t.Fatalf("the sub-task stage is given %v", given)
	}
	c, err := checkSubtasks(nav, json.RawMessage(`{"subtasks":[]}`))
	if err != nil || c.note() != "1 of 1 sub-tasks were not judged" {
		t.Fatalf("an answer judging none: %v %s", err, c.note())
	}
	frames, _ := closeFrames(l, nil, nil, nil, nil, nil, nil)
	var priced []string
	for _, f := range frames {
		if c, ok := f.(*sessionjudge.Cost); ok && c.Scope == sessionjudge.ScopeSubtask {
			priced = append(priced, c.ID)
		}
	}
	if strings.Join(priced, ",") != "stream/b" {
		t.Fatalf("the close file prices sub-tasks %v", priced)
	}
}

// A window by time takes the main talks that start in it, from its start up
// to but not including its end; one that takes nothing is refused.
func TestAWindowByTimeTakesTheTalksThatStartInIt(t *testing.T) {
	_, sel, err := Narrow(threeTalks(), Window{From: time.UnixMilli(450), To: time.UnixMilli(1000)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(sel.Talks, ",") != "talk/main/2" {
		t.Fatalf("selected %v", sel.Talks)
	}
	if _, _, err := Narrow(threeTalks(), Window{From: time.UnixMilli(2000)}, nil); err == nil {
		t.Fatal("a window with no talk was accepted")
	}
	if _, _, err := Narrow(threeTalks(), Window{FromTalk: "talk/main/3", ToTalk: "talk/main/1"}, nil); err == nil {
		t.Fatal("a window ending before it starts was accepted")
	}
}

// A stage is one call when what it is given fits, and chunks when it does
// not; an achievement packet holds one target; every item is in exactly one
// chunk; and a stage with nothing to answer makes no call.
func TestChunksAreCutByWhatTheyAreGiven(t *testing.T) {
	d := threeTalks()
	nav := NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
	if mode, chunks := planChunks("targets", ModeAuto, 1<<20, 1, 0, nav, nil); mode != ModeSingle || len(chunks) != 1 || len(chunks[0].assign.Inputs) != 3 {
		t.Fatalf("auto, all fitting: %s %+v", mode, chunks)
	}
	mode, chunks := planChunks("targets", ModeAuto, 1, 1, 0, nav, nil)
	if mode != ModeStream || len(chunks) != 3 {
		t.Fatalf("auto, nothing fitting: %s %d", mode, len(chunks))
	}
	seen := map[string]int{}
	for _, c := range chunks {
		for _, id := range c.assign.Inputs {
			seen[id]++
		}
	}
	if len(seen) != 3 || seen["input/1"] != 1 || seen["input/3"] != 1 {
		t.Fatalf("inputs across chunks %v", seen)
	}
	targets := []sessionjudge.Target{{ID: "t1", Talks: []string{"talk/main/1"}}, {ID: "t2", Talks: []string{"talk/main/2", "talk/main/3"}}}
	// Achievement is one call per target even when every target would fit
	// one, so each has the reading budget to itself; only single mode joins
	// them.
	if mode, chunks := planChunks("achievement", ModeAuto, 1<<20, 1<<20, 0, nav, targets); mode != ModeStream || len(chunks) != 2 || chunks[1].assign.Targets[0] != "t2" {
		t.Fatalf("achievement packets %s %+v", mode, chunks)
	}
	if _, chunks := planChunks("achievement", ModeSingle, 1, 1, 0, nav, targets); len(chunks) != 1 {
		t.Fatalf("achievement in single mode %+v", chunks)
	}
	ans, _ := nav.Call("talks", json.RawMessage(`{"talks":["talk/main/2"]}`))
	if row := ans.(page).Rows[0].(talkRow); row.ReplyNode != "" {
		t.Fatalf("a talk with no reply named one: %+v", row)
	}
	if _, chunks := planChunks("efficiency", ModeStream, 1, 1<<20, 0, nav, nil); chunks != nil {
		t.Fatalf("a stage with nothing to answer: %+v", chunks)
	}
	// A reference stage is one call whatever it is given, so its conclusions
	// are checked together.
	for _, id := range []string{"R1", "R2"} {
		nav.sources = append(nav.sources, sessionjudge.Source{ID: id, Outcome: sessionjudge.SourceSaved})
		nav.sourceDocs = append(nav.sourceDocs, SourceDoc{ID: id, Text: "x\n"})
	}
	if mode, chunks := planChunks("reference", ModeStream, 1, 1, 0, nav, nil); mode != ModeSingle || len(chunks) != 1 || len(chunks[0].assign.Sources) != 2 {
		t.Fatalf("reference %s %+v", mode, chunks)
	}
	// A regroup is one call, even with no target: it answers for the
	// person's goals.
	if mode, chunks := planChunks("regroup", ModeStream, 1, 1, 0, nav, targets); mode != ModeSingle || len(chunks) != 1 || len(chunks[0].assign.Targets) != 2 {
		t.Fatalf("regroup %s %+v", mode, chunks)
	}
	if _, chunks := planChunks("regroup", ModeStream, 1, 1, 0, nav, nil); len(chunks) != 1 || len(chunks[0].assign.Targets) != 0 {
		t.Fatalf("regroup of no target %+v", chunks)
	}
}

// A step that started before the window ends keeps only what was recorded
// by then: a tool whose result came later shows none.
func TestAToolFinishingAfterTheWindowShowsNoResult(t *testing.T) {
	d := threeTalks()
	// Child a's tool starts at 250, inside the window's work, and its result
	// is recorded at 700, after the window's end at 600.
	d.Talks[1].Children = append(d.Talks[1].Children, sessionview.Node{ID: "late", Kind: "tool", At: 250, Result: "passed",
		Ref: &sessionflow.Ref{Seq: 1, Row: 20}, Refs: []sessionflow.Ref{{Seq: 1, Row: 20}, {Seq: 1, Row: 21}}})
	read := func(refs []sessionflow.Ref) map[[2]uint64]*sessiondata.Record {
		out := map[[2]uint64]*sessiondata.Record{}
		for _, r := range refs {
			at := int64(250)
			if r.Row == 21 {
				at = 700
			}
			out[[2]uint64{r.Seq, r.Row}] = &sessiondata.Record{Time: time.UnixMilli(at).UTC().Format(time.RFC3339Nano)}
		}
		return out
	}
	doc, _, err := Narrow(d, Window{FromTalk: "talk/main/2", ToTalk: "talk/main/2"}, read)
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range doc.Talks {
		for _, n := range tk.Children {
			if n.ID == "late" && (n.Result != "" || len(n.Refs) != 1) {
				t.Fatalf("the late tool %+v", n)
			}
		}
	}
	if doc.Summary.To != 600 {
		t.Fatalf("the window's summary ends at %d", doc.Summary.To)
	}
}

// What was recorded after the window's end is left out everywhere: an
// agent call's result, the workspace changes and executions written later,
// and the part of a talk that ran past the end: its reply, its counts, and
// what a model call still running had consumed by its end.
func TestNothingRecordedAfterTheWindowIsShown(t *testing.T) {
	late := map[uint64]bool{31: true, 41: true, 51: true, 61: true, 63: true}
	read := func(refs []sessionflow.Ref) map[[2]uint64]*sessiondata.Record {
		out := map[[2]uint64]*sessiondata.Record{}
		for _, r := range refs {
			at := int64(250)
			if late[r.Row] {
				at = 700
			}
			name := "early-model"
			switch {
			case late[r.Row]:
				name = "late-model"
			case r.Row == 62:
				name = ""
			}
			out[[2]uint64{r.Seq, r.Row}] = &sessiondata.Record{Time: time.UnixMilli(at).UTC().Format(time.RFC3339Nano),
				Usage: usage(3, 1, 500, 40), Model: name}
		}
		return out
	}
	narrow := func(early bool) (*sessionview.Conversation, *sessionview.Node) {
		t.Helper()
		d := threeTalks()
		a := &d.Talks[1]
		a.Children = append(a.Children,
			sessionview.Node{ID: "agent", Kind: model.KindAgentCall, At: 250, Result: "the child's report",
				Ref: &sessionflow.Ref{Seq: 1, Row: 30}, Refs: []sessionflow.Ref{{Seq: 1, Row: 30}, {Seq: 1, Row: 31}}},
			sessionview.Node{ID: "running", Kind: model.KindLLMCall, At: 260, Model: "late-model", Usage: usage(3, 90, 500, 40),
				Attrs: json.RawMessage(`{"stop":"end"}`), Refs: []sessionflow.Ref{{Seq: 1, Row: 60}, {Seq: 1, Row: 61}}},
			sessionview.Node{ID: "later", Kind: model.KindMessageAssistant, At: 800, Text: "all done"},
			sessionview.Node{ID: "unnamed", Kind: model.KindLLMCall, At: 270, Model: "late-model", Usage: usage(3, 90, 500, 40),
				Refs: []sessionflow.Ref{{Seq: 1, Row: 62}, {Seq: 1, Row: 63}}})
		if early {
			a.Children = append(a.Children, sessionview.Node{ID: "early", Kind: model.KindMessageAssistant, At: 300, Text: "working"})
		}
		a.Reply, a.Steps, a.Tools = "all done", 99, 99
		d.Talks[0].Children[2].Changes = []string{"ch1", "ch2"}
		d.Talks[0].Children[2].Executions = []string{"ex1", "ex2"}
		d.WorkspaceChanges = []sessionview.WorkspaceChange{{Step: "s1", Ref: sessionflow.Ref{Seq: 1, Row: 40}, Record: changes.Record{ID: "ch1"}},
			{Step: "s1", Ref: sessionflow.Ref{Seq: 1, Row: 41}, Record: changes.Record{ID: "ch2"}}}
		d.ToolExecutions = []sessionview.ToolExecution{{Step: "s1", Ref: sessionflow.Ref{Seq: 1, Row: 50}, Record: execution.Record{ID: "ex1"}},
			{Step: "s1", Ref: sessionflow.Ref{Seq: 1, Row: 51}, Record: execution.Record{ID: "ex2"}}}
		doc, _, err := Narrow(d, Window{FromTalk: "talk/main/2", ToTalk: "talk/main/2"}, read)
		if err != nil {
			t.Fatal(err)
		}
		for i := range doc.Talks {
			if doc.Talks[i].ID == "talk/a" {
				return doc, &doc.Talks[i]
			}
		}
		t.Fatal("no talk a")
		return nil, nil
	}
	doc, a := narrow(true)
	// ca1, agent, running, unnamed and early remain; agent is its one tool.
	if a.To != 600 || a.Reply != "working" || a.Steps != 5 || a.Tools != 1 {
		t.Fatalf("talk a %+v", a)
	}
	for _, n := range a.Children {
		if n.ID == "agent" && n.Result != "" {
			t.Fatalf("the agent call shows a result recorded later: %+v", n)
		}
		// What came with its end, as why it stopped, is unknown; its model
		// is the one the records it keeps name.
		if n.ID == "running" && (n.Usage != nil || !unfinished(&n) || strings.Contains(string(n.Attrs), "stop") || n.Model != "early-model") {
			t.Fatalf("the call still running at the end %+v %s", n, n.Attrs)
		}
		// A call whose kept records name no model names none.
		if n.ID == "unnamed" && n.Model != "" {
			t.Fatalf("the call still running at the end %+v %s", n, n.Attrs)
		}
	}
	if len(doc.WorkspaceChanges) != 1 || doc.WorkspaceChanges[0].Ref.Row != 40 || len(doc.ToolExecutions) != 1 || doc.ToolExecutions[0].Ref.Row != 50 {
		t.Fatalf("changes %+v executions %+v", doc.WorkspaceChanges, doc.ToolExecutions)
	}
	if s1 := doc.Talks[0].Children[2]; strings.Join(s1.Changes, ",") != "ch1" || strings.Join(s1.Executions, ",") != "ex1" {
		t.Fatalf("s1 names %v and %v", s1.Changes, s1.Executions)
	}
	steps := 0
	for _, tk := range doc.Talks {
		steps += tk.Steps
	}
	if doc.Summary.Steps != steps {
		t.Fatalf("the summary counts %d steps, its talks %d", doc.Summary.Steps, steps)
	}
	// The running call counts what it had read, and its output is unknown.
	l := Compute(doc, "claude-code/1", nil, read)
	for _, c := range l.Calls {
		if c.ID == "running" && (!c.OutputUnknown || c.Usage == nil || c.Usage.Output != 0 || c.Usage.CacheRead != 500) {
			t.Fatalf("the running call in the ledger %+v %+v", c, c.Usage)
		}
	}
	// A talk whose only reply came after the end has none.
	if _, a := narrow(false); a.Reply != "" {
		t.Fatalf("a reply recorded after the end: %q", a.Reply)
	}
}

// A target's observed time is the union of its talks' own spans, so the gap
// between a talk and a sub-task that started later is not counted.
func TestObservedTimeLeavesOutTheGaps(t *testing.T) {
	d := threeTalks()
	l := Compute(d, "claude-code/1", nil, nil)
	// talk/main/1 runs 100 to 300 and child a 200 to 900: 800 in all.
	// talk/main/2, 500 to 600, falls inside a's span.
	if got := union(append(l.TalkSpans("talk/main/1"), l.TalkSpans("talk/main/2")...)); got != 800 {
		t.Fatalf("union %d", got)
	}
	d.Talks[1].From, d.Talks[1].To = 1500, 1600
	l = Compute(d, "claude-code/1", nil, nil)
	// Now a runs 1500 to 1600: 200 of the main talk and 100 of a, not the
	// 1500 an enclosing range would give.
	if got := union(l.TalkSpans("talk/main/1")); got != 300 {
		t.Fatalf("union %d, want the two spans and not the range around them", got)
	}
	frames, _ := closeFrames(l, []sessionjudge.Target{{ID: "t1", Talks: []string{"talk/main/1"}}}, nil, nil, nil, nil, nil)
	for _, f := range frames {
		if o, ok := f.(*sessionjudge.Outcome); ok && o.Span != 300 {
			t.Fatalf("the target's observed time %d", o.Span)
		}
	}
}

// Efficiency is cut so that every call can read the outlines of the talks it
// labels within its budget, even when their rows would fit one call.
func TestChunksAreCutByWhatTheyMustRead(t *testing.T) {
	d := threeTalks()
	nav := NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
	targets := []sessionjudge.Target{{ID: "t1", Talks: []string{"talk/main/1", "talk/main/2", "talk/main/3"}}}
	if _, chunks := planChunks("efficiency", ModeAuto, 1<<20, 1<<20, 0, nav, targets); len(chunks) != 1 {
		t.Fatalf("no budget, one call: %d", len(chunks))
	}
	// A budget whose three quarters hold one outline: a call per talk.
	need := nav.outlineBytes("talk/main/1")
	if _, chunks := planChunks("efficiency", ModeAuto, 1<<20, 1<<20, need*4/3+4, nav, targets); len(chunks) != 3 {
		t.Fatalf("a budget for one outline: %d calls", len(chunks))
	}
}

// A window is by time or by talk ids. Given both, the run would select by
// the ids and leave the times out without a word.
func TestAWindowIsByTimeOrByTalkNotBoth(t *testing.T) {
	_, _, err := Narrow(threeTalks(), Window{FromTalk: "talk/main/2", From: time.UnixMilli(450)}, nil)
	if err == nil || !strings.Contains(err.Error(), "a window is by time or by talk ids, not both") {
		t.Fatalf("a window by time and by talk: %v", err)
	}
}

// A window's first or last talk must be a main talk, and the error names
// the one that is not. A child's talk is not one.
func TestAWindowNamesOnlyMainTalks(t *testing.T) {
	for _, w := range []Window{{FromTalk: "talk/a"}, {ToTalk: "talk/none"}} {
		name := w.FromTalk + w.ToTalk
		_, _, err := Narrow(threeTalks(), w, nil)
		if err == nil || !strings.Contains(err.Error(), `"`+name+`" is no main talk`) {
			t.Errorf("a window naming %s: %v", name, err)
		}
	}
}

// A main talk with no recorded time cannot be placed in a window by time.
// It is never selected, and the selection counts it.
func TestAWindowByTimeLeavesOutAnUntimedTalk(t *testing.T) {
	d := threeTalks()
	d.Talks[0].At = 0
	_, sel, err := Narrow(d, Window{To: time.UnixMilli(1000)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(sel.Talks, ",") != "talk/main/2" || sel.Untimed != 1 {
		t.Fatalf("selected %v, untimed %d", sel.Talks, sel.Untimed)
	}
}

// A window shows its talks, the talks before it, and the streams they
// started. Child a of talk/main/1 works until 900, past the start of
// talk/main/2 at 500, yet talk/main/2 is not shown. Neither is a stream no
// step can be shown to have started: only a run over the whole conversation
// judges it.
func TestAWindowShowsNoTalkOutsideIt(t *testing.T) {
	d := threeTalks()
	d.Streams = append(d.Streams, sessionview.Stream{ID: "stream/c", Name: "c", Role: "child"})
	d.Talks = append(d.Talks, sessionview.Node{ID: "talk/c", Kind: "talk", Stream: "c", At: 300, From: 300, To: 400})
	doc, sel, err := Narrow(d, Window{FromTalk: "talk/main/1", ToTalk: "talk/main/1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var shown []string
	for _, tk := range doc.Talks {
		shown = append(shown, tk.ID)
	}
	if sel.End != 900 || strings.Join(shown, ",") != "talk/main/1,talk/a" {
		t.Fatalf("a window ending at %d shows %v", sel.End, shown)
	}
}

// Only a talk that ran past the window's end is cut there. A talk that
// ended before it keeps its span and its reply as the view gave them.
func TestATalkThatEndedInTheWindowKeepsItsSpanAndReply(t *testing.T) {
	d := threeTalks()
	d.Talks[0].Reply = "done"
	doc, _, err := Narrow(d, Window{FromTalk: "talk/main/2", ToTalk: "talk/main/2"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if one := doc.Talks[0]; one.ID != "talk/main/1" || one.To != 300 || one.Reply != "done" {
		t.Fatalf("%s to %d, reply %q", one.ID, one.To, one.Reply)
	}
}

// A record with no time, or one the reader does not return, cannot be shown
// to come after the window's end, so the step keeps it and its result.
func TestARecordWithNoTimeIsNotTakenForLate(t *testing.T) {
	d := threeTalks()
	d.Talks[1].Children = append(d.Talks[1].Children, sessionview.Node{ID: "kept", Kind: "tool", At: 250, Result: "passed",
		Ref: &sessionflow.Ref{Seq: 1, Row: 20}, Refs: []sessionflow.Ref{{Seq: 1, Row: 20}, {Seq: 1, Row: 21}, {Seq: 1, Row: 22}}})
	// Row 20 has no time, and the reader does not find row 21.
	read := func([]sessionflow.Ref) map[[2]uint64]*sessiondata.Record {
		return map[[2]uint64]*sessiondata.Record{{1, 20}: {}, {1, 22}: {Time: time.UnixMilli(250).UTC().Format(time.RFC3339Nano)}}
	}
	doc, _, err := Narrow(d, Window{FromTalk: "talk/main/2", ToTalk: "talk/main/2"}, read)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tk := range doc.Talks {
		for _, n := range tk.Children {
			if n.ID == "kept" {
				found = true
				if n.Result != "passed" || len(n.Refs) != 3 {
					t.Fatalf("the step lost what it showed: %+v", n)
				}
			}
		}
	}
	if !found {
		t.Fatal("the step is not shown")
	}
}
