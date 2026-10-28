// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements.  See the NOTICE file distributed with
// this work for additional information regarding copyright ownership.
// The ASF licenses this file to You under the Apache License, Version 2.0
// (the "License"); you may not use this file except in compliance with
// the License.  You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package judge

import (
	"fmt"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// worked is a conversation of three main talks, every step inside the model
// call that asked for it, landed in order. The person asks in in/1, and c1
// answers in r1 and writes a.go in s1. The person corrects it in in/2; c2
// reverts in s2, and c3 answers again in r2 and writes b.go in s3. The
// person asks a question in in/3, which c4 answers in r3; s4 then writes
// notes.md with no model call asking for it, as some runtimes record.
func worked() (*Nav, *Ledger) {
	row := uint64(0)
	node := func(id, kind, text string, children ...sessionview.Node) sessionview.Node {
		row++
		n := sessionview.Node{ID: id, Kind: kind, Text: text, Ref: &sessionflow.Ref{Seq: 1, Row: row}, Children: children}
		if kind == model.KindTool {
			n.Name, n.Result = "Write", "ok"
		}
		return n
	}
	call := func(id string, tokens int, children ...sessionview.Node) sessionview.Node {
		n := node(id, model.KindLLMCall, "", children...)
		n.Stream, n.Model, n.Usage = "main", "m", usage(tokens, 0, 0, 0)
		return n
	}
	talk := func(id string, children ...sessionview.Node) sessionview.Node {
		return sessionview.Node{ID: id, Kind: "talk", Stream: "main", Children: children}
	}
	// Each step is made in the order it landed, a call before what it asked.
	in1 := node("in/1", model.KindMessageExternal, "find why it fails and fix it")
	c1 := call("c1", 10, node("r1", model.KindMessageAssistant, "the cache"), node("s1", model.KindTool, `{"file":"a.go"}`))
	in2 := node("in/2", model.KindMessageExternal, "wrong, it is not the cache")
	c2 := call("c2", 20, node("s2", model.KindTool, `{"command":"git revert"}`))
	c3 := call("c3", 30, node("r2", model.KindMessageAssistant, "the clock"), node("s3", model.KindTool, `{"file":"b.go"}`))
	in3 := node("in/3", model.KindMessageExternal, "and why is it slow?")
	c4 := call("c4", 40, node("r3", model.KindMessageAssistant, "the disk"))
	s4 := node("s4", model.KindTool, `{"file":"notes.md"}`)
	d := &sessionview.Conversation{Conversation: "conv", Streams: []sessionview.Stream{{ID: "stream/main", Name: "main"}},
		Talks: []sessionview.Node{talk("talk/1", in1, c1), talk("talk/2", in2, c2, c3), talk("talk/3", in3, c4, s4)}}
	l := Compute(d, "claude-code/1", nil, nil)
	nav := NewNav(d, l, nil)
	nav.Decode(writes)
	return nav, l
}

func cites(ids ...string) []sessionjudge.Cite {
	var out []sessionjudge.Cite
	for _, id := range ids {
		out = append(out, sessionjudge.Cite{Node: id})
	}
	return out
}

// workedRun is the stages' decisions over worked: t1, the fix, and t2, the
// question, each a goal of its own.
type workedRun struct {
	targets      []sessionjudge.Target
	inputs       []*sessionjudge.Input
	achievements []*sessionjudge.Achievement
	labels       []*sessionjudge.Label
	subtasks     map[string]*sessionjudge.Subtask
	refs         references
}

func workedDecisions() *workedRun {
	two := []sessionjudge.Criterion{{ID: "t1.c1", Text: "the cause named"}, {ID: "t1.c2", Text: "fixed"}}
	return &workedRun{
		targets: []sessionjudge.Target{
			{ID: "t1", Status: sessionjudge.TargetClosed, Talks: []string{"talk/1", "talk/2"}, Criteria: two, Cite: cites("in/1")},
			{ID: "t2", Status: sessionjudge.TargetClosed, Talks: []string{"talk/3"}, Criteria: []sessionjudge.Criterion{{ID: "t2.c1", Text: "answered"}},
				Cite: cites("in/3", "in/2")},
		},
		inputs: []*sessionjudge.Input{
			{Node: "in/1", Talk: "talk/1", Actions: []sessionjudge.Action{{Target: "t1", Action: sessionjudge.ActionCreate}}},
			{Node: "in/2", Talk: "talk/2", Actions: []sessionjudge.Action{{Target: "t1", Action: sessionjudge.ActionCorrect}}},
			{Node: "in/3", Talk: "talk/3", Actions: []sessionjudge.Action{{Target: "t2", Action: sessionjudge.ActionCreate}}},
		},
		achievements: []*sessionjudge.Achievement{
			{Target: "t1", Level: sessionjudge.LevelPartly, Basis: sessionjudge.BasisObserved,
				Criteria: []sessionjudge.Check{{ID: "t1.c1", Text: "the cause named", State: sessionjudge.CheckMet, Cite: cites("r2")},
					{ID: "t1.c2", Text: "fixed", State: sessionjudge.CheckNotMet, Cite: cites("s3", "s1")}},
				Events: []sessionjudge.Event{{ID: "t1.e1", Kind: sessionjudge.EventDelivered, Artifact: sessionjudge.ArtifactFile, Cite: cites("s1")},
					{ID: "t1.e2", Kind: sessionjudge.EventUndone, Artifact: sessionjudge.ArtifactFile, Cite: cites("s2")},
					{ID: "t1.e3", Kind: sessionjudge.EventDeliveredAgain, Artifact: sessionjudge.ArtifactFile, Cite: cites("s3")}},
				Contradictions: []sessionjudge.Contradiction{{Event: "t1.e1", Conclusion: true, Text: "not the cache", Cite: cites("in/2")},
					{Event: "t1.e3", Criteria: []string{"t1.c1"}, Text: "not the clock either", Cite: cites("in/3")}},
				Rework:   []sessionjudge.Rework{{ID: "t1.r1", Trigger: sessionjudge.TriggerPerson, Cause: sessionjudge.ReworkAgentError, From: "in/2", To: "s3"}},
				Findings: []sessionjudge.Finding{{At: "r1", Problem: "it named the cause without reading the log"}}},
			{Target: "t2", Level: sessionjudge.LevelAchieved, Basis: sessionjudge.BasisDelivered,
				Criteria: []sessionjudge.Check{{ID: "t2.c1", Text: "answered", State: sessionjudge.CheckMet, Cite: cites("r3")}},
				Events:   []sessionjudge.Event{{ID: "t2.e1", Kind: sessionjudge.EventDelivered, Artifact: sessionjudge.ArtifactAnswer, Cite: cites("r3")}}},
		},
	}
}

// closed runs the close and the report over worked.
func (w *workedRun) closed(t *testing.T) (map[string]*sessionjudge.Outcome, []sessionjudge.Finding, *sessionjudge.Summary) {
	t.Helper()
	nav, l := worked()
	frames, sum := closeFrames(l, w.targets, w.inputs, w.achievements, w.labels, w.subtasks, nil)
	more := report(nav, l, w.targets, w.inputs, w.achievements, w.labels, w.subtasks, w.refs, frames, sum)
	outcomes := map[string]*sessionjudge.Outcome{}
	for _, f := range frames {
		if o, ok := f.(*sessionjudge.Outcome); ok {
			outcomes[o.Target] = o
		}
	}
	var found []sessionjudge.Finding
	ops := 0
	for _, f := range more {
		switch f := f.(type) {
		case *sessionjudge.Finding:
			found = append(found, *f)
		case *sessionjudge.Operation:
			ops++
		}
	}
	// The close holds every operation, which outcomes name by id.
	if ops != len(nav.Operations()) {
		t.Fatalf("%d operations in the close, of %d", ops, len(nav.Operations()))
	}
	return outcomes, found, sum
}

func fraction(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprint(*v)
}

// The close says how each target ended. t1's last delivery is contradicted
// on a criterion it met, so it is contradicted, and its final score counts
// that criterion not met; the contradiction of its first delivery stays in
// the history. Its rework cost the calls from the correction to the redo.
func TestTheCloseSaysHowEachTargetEnded(t *testing.T) {
	o, found, sum := workedDecisions().closed(t)
	t1, t2 := o["t1"], o["t2"]
	if t1.Class != sessionjudge.ClassContradicted || fmt.Sprint(t1.Standing) != "[t1.e3]" || fraction(t1.ScoreRecord) != "0.5" || fraction(t1.ScoreFinal) != "0" {
		t.Fatalf("t1 %s %v record %s final %s", t1.Class, t1.Standing, fraction(t1.ScoreRecord), fraction(t1.ScoreFinal))
	}
	if t2.Class != sessionjudge.ClassAchieved || fraction(t2.ScoreRecord) != "1" || fraction(t2.ScoreFinal) != "1" {
		t.Fatalf("t2 %s record %s final %s", t2.Class, fraction(t2.ScoreRecord), fraction(t2.ScoreFinal))
	}
	// No stage labeled the work, so the rework is not reviewed.
	if len(t1.Rework) != 1 || t1.Rework[0].Work != sessionjudge.WorkNotReviewed || t1.Rework[0].Cause != "" || t1.Rework[0].Cost.Usage.Input != 50 {
		t.Fatalf("t1 rework %+v", t1.Rework)
	}
	if fmt.Sprint(t1.Corrections) != "[in/2]" || len(t2.Corrections) != 0 {
		t.Fatalf("corrections %v %v", t1.Corrections, t2.Corrections)
	}
	if fmt.Sprint(t1.Operations) != "[{o1 <nil>} {o2 <nil>}]" || fmt.Sprint(t2.Operations) != "[{o3 <nil>}]" || len(sum.UnassignedOperations) != 0 {
		t.Fatalf("operations %+v %+v %v", t1.Operations, t2.Operations, sum.UnassignedOperations)
	}
	// The run's final score is the mean of the targets' final scores, beside
	// the mean of their record scores.
	if fraction(sum.Score.Final) != "0.5" || fraction(sum.Score.Achievement) != "0.75" {
		t.Fatalf("score %+v", sum.Score)
	}
	// asz's own findings: the criterion not met at its first evidence, the
	// standing contradiction at its delivery, and the round of rework at the
	// correction, which a correction finding of its own would repeat.
	var got []string
	for _, f := range found {
		if f.Source != sessionjudge.SourceASZ || f.Target != "t1" || f.T != sessionjudge.FrameFinding || f.Ref == nil {
			t.Fatalf("finding %+v", f)
		}
		got = append(got, f.At+": "+f.Problem)
	}
	want := []string{"s1: criterion t1.c2 was not met: fixed", "s3: the delivered result t1.e3 was contradicted: not the clock either",
		"in/2: round t1.r1 of rework began, triggered by person, cause agent_error"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("findings\n%s", strings.Join(got, "\n"))
	}
}

// Each rule of an outcome on its own, from the decisions of
// TestTheCloseSaysHowEachTargetEnded with one changed.
func TestEachOutcomeRuleStandsAlone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(w *workedRun)
		check  func(o map[string]*sessionjudge.Outcome, found []sessionjudge.Finding, sum *sessionjudge.Summary) string
	}{
		{"a contradiction of an earlier delivery only", func(w *workedRun) { w.achievements[0].Contradictions = w.achievements[0].Contradictions[:1] },
			func(o map[string]*sessionjudge.Outcome, _ []sessionjudge.Finding, _ *sessionjudge.Summary) string {
				if t1 := o["t1"]; t1.Class != sessionjudge.ClassPartly || fraction(t1.ScoreFinal) != "0.5" {
					return fmt.Sprintf("t1 %s %s", t1.Class, fraction(t1.ScoreFinal))
				}
				return ""
			}},
		{"a contradiction of the delivered conclusion", func(w *workedRun) {
			w.achievements[0].Contradictions[1].Conclusion, w.achievements[0].Contradictions[1].Criteria = true, nil
		},
			func(o map[string]*sessionjudge.Outcome, _ []sessionjudge.Finding, _ *sessionjudge.Summary) string {
				if t1 := o["t1"]; t1.Class != sessionjudge.ClassContradicted || fraction(t1.ScoreFinal) != "0" {
					return fmt.Sprintf("t1 %s %s", t1.Class, fraction(t1.ScoreFinal))
				}
				return ""
			}},
		{"an achieved result undone whole", func(w *workedRun) {
			a := w.achievements[1]
			a.Events = []sessionjudge.Event{{ID: "t2.e1", Kind: sessionjudge.EventDelivered, Artifact: sessionjudge.ArtifactAnswer, Cite: cites("r3")},
				{ID: "t2.e2", Kind: sessionjudge.EventUndone, Artifact: sessionjudge.ArtifactAnswer, Text: "withdrawn", Cite: cites("r3")}}
		}, func(o map[string]*sessionjudge.Outcome, found []sessionjudge.Finding, _ *sessionjudge.Summary) string {
			if t2 := o["t2"]; t2.Class != sessionjudge.ClassReverted || fraction(t2.ScoreRecord) != "1" || fraction(t2.ScoreFinal) != "0" {
				return fmt.Sprintf("t2 %s %s %s", t2.Class, fraction(t2.ScoreRecord), fraction(t2.ScoreFinal))
			}
			if !strings.Contains(fmt.Sprint(found), "the delivered result was undone: withdrawn") {
				return fmt.Sprint(found)
			}
			return ""
		}},
		{"some criteria undone", func(w *workedRun) {
			a := w.achievements[0]
			a.Contradictions, a.Events = nil, a.Events[:2]
			a.Events[1].Criteria = []string{"t1.c1"}
		}, func(o map[string]*sessionjudge.Outcome, _ []sessionjudge.Finding, _ *sessionjudge.Summary) string {
			if t1 := o["t1"]; t1.Class != sessionjudge.ClassReverted || fmt.Sprint(t1.Standing) != "[t1.e1]" || fraction(t1.ScoreFinal) != "0" {
				return fmt.Sprintf("t1 %s %v %s", t1.Class, t1.Standing, fraction(t1.ScoreFinal))
			}
			return ""
		}},
		{"a target that cannot be verified", func(w *workedRun) {
			a := w.achievements[1]
			a.Level, a.Criteria[0].State = sessionjudge.LevelCannotVerify, sessionjudge.CheckUnknown
		}, func(o map[string]*sessionjudge.Outcome, _ []sessionjudge.Finding, sum *sessionjudge.Summary) string {
			if t2 := o["t2"]; t2.Class != sessionjudge.ClassCannotVerify || t2.ScoreRecord != nil || t2.ScoreFinal != nil || fraction(sum.Score.Final) != "0" {
				return fmt.Sprintf("t2 %s %s %s, final %s", t2.Class, fraction(t2.ScoreRecord), fraction(t2.ScoreFinal), fraction(sum.Score.Final))
			}
			return ""
		}},
		{"a cancelled target", func(w *workedRun) { w.targets[0].Status = sessionjudge.TargetCancelled },
			func(o map[string]*sessionjudge.Outcome, found []sessionjudge.Finding, sum *sessionjudge.Summary) string {
				t1 := o["t1"]
				if t1.Class != sessionjudge.ClassCancelled || t1.ScoreRecord != nil || t1.ScoreFinal != nil || fraction(sum.Score.Final) != "1" {
					return fmt.Sprintf("t1 %s %s %s, final %s", t1.Class, fraction(t1.ScoreRecord), fraction(t1.ScoreFinal), fraction(sum.Score.Final))
				}
				// Its rework is still a problem; what its result missed is not.
				if len(found) != 1 || !strings.Contains(found[0].Problem, "round t1.r1 of rework") {
					return fmt.Sprint(found)
				}
				return ""
			}},
		{"a delivery in parts", func(w *workedRun) {
			a := w.achievements[0]
			a.Events = []sessionjudge.Event{{ID: "t1.e1", Kind: sessionjudge.EventDelivered, Artifact: sessionjudge.ArtifactFile, Cite: cites("s1")},
				{ID: "t1.e2", Kind: sessionjudge.EventDeliveredAgain, Artifact: sessionjudge.ArtifactFile, Cite: cites("s3")},
				{ID: "t1.e3", Kind: sessionjudge.EventDelivered, Artifact: sessionjudge.ArtifactAnswer, Cite: cites("r2")}}
			a.Contradictions = []sessionjudge.Contradiction{{Event: "t1.e1", Conclusion: true, Text: "corrected since", Cite: cites("in/2")},
				{Event: "t1.e3", Criteria: []string{"t1.c1"}, Text: "the answer is wrong", Cite: cites("in/3")}}
		}, func(o map[string]*sessionjudge.Outcome, _ []sessionjudge.Finding, _ *sessionjudge.Summary) string {
			// The corrected delivery and the part after it stand; the first is
			// history, so its contradiction does not apply, and the part's does.
			if t1 := o["t1"]; fmt.Sprint(t1.Standing) != "[t1.e2 t1.e3]" || t1.Class != sessionjudge.ClassContradicted || fraction(t1.ScoreFinal) != "0" {
				return fmt.Sprintf("t1 %v %s %s", t1.Standing, t1.Class, fraction(t1.ScoreFinal))
			}
			return ""
		}},
		{"a contradicted claim stays in the final score", func(w *workedRun) {
			a := w.achievements[1]
			a.Level, a.Claims = sessionjudge.LevelPartly, []sessionjudge.Check{{Text: "it is fast", State: sessionjudge.CheckContradicted, Cite: cites("in/3")}}
		}, func(o map[string]*sessionjudge.Outcome, _ []sessionjudge.Finding, _ *sessionjudge.Summary) string {
			if t2 := o["t2"]; t2.Class != sessionjudge.ClassPartly || fraction(t2.ScoreRecord) != "0.5" || fraction(t2.ScoreFinal) != "0.5" {
				return fmt.Sprintf("t2 %s %s %s", t2.Class, fraction(t2.ScoreRecord), fraction(t2.ScoreFinal))
			}
			return ""
		}},
		{"a correction of one part", func(w *workedRun) {
			a := w.achievements[0]
			a.Events = []sessionjudge.Event{{ID: "t1.e1", Kind: sessionjudge.EventDelivered, Artifact: sessionjudge.ArtifactFile, Cite: cites("s1")},
				{ID: "t1.e2", Kind: sessionjudge.EventDelivered, Artifact: sessionjudge.ArtifactAnswer, Cite: cites("r2")},
				{ID: "t1.e3", Kind: sessionjudge.EventDeliveredAgain, Artifact: sessionjudge.ArtifactFile, Replaces: []string{"t1.e1"}, Cite: cites("s3")}}
			a.Contradictions = []sessionjudge.Contradiction{{Event: "t1.e1", Conclusion: true, Text: "corrected since", Cite: cites("in/2")},
				{Event: "t1.e2", Criteria: []string{"t1.c1"}, Text: "the answer is wrong", Cite: cites("in/3")}}
		}, func(o map[string]*sessionjudge.Outcome, _ []sessionjudge.Finding, _ *sessionjudge.Summary) string {
			// The part it did not correct still stands, with its contradiction.
			if t1 := o["t1"]; fmt.Sprint(t1.Standing) != "[t1.e2 t1.e3]" || t1.Class != sessionjudge.ClassContradicted || fraction(t1.ScoreFinal) != "0" {
				return fmt.Sprintf("t1 %v %s %s", t1.Standing, t1.Class, fraction(t1.ScoreFinal))
			}
			return ""
		}},
		{"an undoing of one part, and the other part corrected", func(w *workedRun) {
			a := w.achievements[0]
			a.Events = []sessionjudge.Event{{ID: "t1.e1", Kind: sessionjudge.EventDelivered, Artifact: sessionjudge.ArtifactFile, Cite: cites("s1")},
				{ID: "t1.e2", Kind: sessionjudge.EventDelivered, Artifact: sessionjudge.ArtifactAnswer, Cite: cites("r2")},
				{ID: "t1.e3", Kind: sessionjudge.EventUndone, Artifact: sessionjudge.ArtifactFile, Criteria: []string{"t1.c1"}, Undoes: []string{"t1.e1"}, Cite: cites("s2")},
				{ID: "t1.e4", Kind: sessionjudge.EventDeliveredAgain, Artifact: sessionjudge.ArtifactAnswer, Replaces: []string{"t1.e2"}, Cite: cites("r3")}}
			a.Contradictions = nil
		}, func(o map[string]*sessionjudge.Outcome, _ []sessionjudge.Finding, _ *sessionjudge.Summary) string {
			// t1.e1 was never delivered again, so its undoing still applies.
			if t1 := o["t1"]; t1.Class != sessionjudge.ClassReverted || fmt.Sprint(t1.Undone) != "[t1.e3]" || fmt.Sprint(t1.Standing) != "[t1.e1 t1.e4]" ||
				fraction(t1.ScoreFinal) != "0" {
				return fmt.Sprintf("t1 %s %v %v %s", t1.Class, t1.Undone, t1.Standing, fraction(t1.ScoreFinal))
			}
			return ""
		}},
		{"an undoing of one part, and that part delivered again", func(w *workedRun) {
			a := w.achievements[0]
			a.Events = []sessionjudge.Event{{ID: "t1.e1", Kind: sessionjudge.EventDelivered, Artifact: sessionjudge.ArtifactFile, Cite: cites("s1")},
				{ID: "t1.e2", Kind: sessionjudge.EventDelivered, Artifact: sessionjudge.ArtifactAnswer, Cite: cites("r2")},
				{ID: "t1.e3", Kind: sessionjudge.EventUndone, Artifact: sessionjudge.ArtifactFile, Criteria: []string{"t1.c1"}, Undoes: []string{"t1.e1"}, Cite: cites("s2")},
				{ID: "t1.e4", Kind: sessionjudge.EventDeliveredAgain, Artifact: sessionjudge.ArtifactFile, Replaces: []string{"t1.e1"}, Cite: cites("s3")}}
			a.Contradictions = nil
		}, func(o map[string]*sessionjudge.Outcome, _ []sessionjudge.Finding, _ *sessionjudge.Summary) string {
			if t1 := o["t1"]; t1.Class != sessionjudge.ClassPartly || len(t1.Undone) != 0 || fraction(t1.ScoreFinal) != fraction(t1.ScoreRecord) {
				return fmt.Sprintf("t1 %s %v %s", t1.Class, t1.Undone, fraction(t1.ScoreFinal))
			}
			return ""
		}},
		{"deliveries in parts with none again", func(w *workedRun) {
			a := w.achievements[0]
			a.Events = []sessionjudge.Event{{ID: "t1.e1", Kind: sessionjudge.EventDelivered, Artifact: sessionjudge.ArtifactFile, Cite: cites("s1")},
				{ID: "t1.e2", Kind: sessionjudge.EventDelivered, Artifact: sessionjudge.ArtifactAnswer, Cite: cites("r2")}}
			a.Contradictions = []sessionjudge.Contradiction{{Event: "t1.e1", Conclusion: true, Text: "not the cache", Cite: cites("in/2")}}
		}, func(o map[string]*sessionjudge.Outcome, _ []sessionjudge.Finding, _ *sessionjudge.Summary) string {
			if t1 := o["t1"]; fmt.Sprint(t1.Standing) != "[t1.e1 t1.e2]" || t1.Class != sessionjudge.ClassContradicted {
				return fmt.Sprintf("t1 %v %s", t1.Standing, t1.Class)
			}
			return ""
		}},
		{"a source contradicts a standing delivery", func(w *workedRun) {
			w.achievements[0].Contradictions = nil
			w.refs = references{conclusions: []sessionjudge.Conclusion{{ID: "r1", Statement: "the clock is fine", Scope: []string{"in/1"}}},
				divergences: []*sessionjudge.Divergence{{Conclusion: "r1", Verdict: sessionjudge.VerdictContradicted, Target: "t1", Event: "t1.e3", Whole: true, Cite: cites("in/3")}}}
		}, func(o map[string]*sessionjudge.Outcome, found []sessionjudge.Finding, _ *sessionjudge.Summary) string {
			if t1 := o["t1"]; t1.Class != sessionjudge.ClassContradicted || fraction(t1.ScoreFinal) != "0" || fmt.Sprint(t1.References) != "[r1]" {
				return fmt.Sprintf("t1 %s %s %v", t1.Class, fraction(t1.ScoreFinal), t1.References)
			}
			if !strings.Contains(fmt.Sprint(found), "contradicted: conclusion r1: the clock is fine") {
				return fmt.Sprint(found)
			}
			return ""
		}},
		{"a source corrected the conclusion later", func(w *workedRun) {
			w.achievements[0].Contradictions = nil
			w.refs = references{conclusions: []sessionjudge.Conclusion{{ID: "r1", Scope: []string{"in/1"}, SupersededBy: "r2"}, {ID: "r2", Scope: []string{"in/1"}}},
				divergences: []*sessionjudge.Divergence{{Conclusion: "r1", Verdict: sessionjudge.VerdictContradicted, Target: "t1", Event: "t1.e3", Whole: true}},
				rollbacks:   []*sessionjudge.Rollback{{Conclusion: "r1", Target: "t1", Event: "t1.e3"}}}
		}, func(o map[string]*sessionjudge.Outcome, _ []sessionjudge.Finding, _ *sessionjudge.Summary) string {
			if t1 := o["t1"]; t1.Class != sessionjudge.ClassPartly || len(t1.References) != 0 {
				return fmt.Sprintf("t1 %s %v", t1.Class, t1.References)
			}
			return ""
		}},
		{"a disagreement with a conclusion corrected since", func(w *workedRun) {
			w.achievements[0].Contradictions = nil
			w.refs = references{conclusions: []sessionjudge.Conclusion{{ID: "r1", Scope: []string{"in/1"}},
				{ID: "r2", Scope: []string{"in/1"}, Conflicts: []string{"r1"}, SupersededBy: "r3"}, {ID: "r3", Scope: []string{"in/1"}}},
				divergences: []*sessionjudge.Divergence{{Conclusion: "r1", Verdict: sessionjudge.VerdictContradicted, Target: "t1", Event: "t1.e3", Whole: true}}}
		}, func(o map[string]*sessionjudge.Outcome, _ []sessionjudge.Finding, _ *sessionjudge.Summary) string {
			if t1 := o["t1"]; t1.Class != sessionjudge.ClassContradicted || fmt.Sprint(t1.References) != "[r1]" {
				return fmt.Sprintf("t1 %s %v", t1.Class, t1.References)
			}
			return ""
		}},
		{"a source contradicts a delivery corrected since", func(w *workedRun) {
			w.achievements[0].Contradictions = nil
			w.refs = references{conclusions: []sessionjudge.Conclusion{{ID: "r1", Scope: []string{"in/1"}}},
				divergences: []*sessionjudge.Divergence{{Conclusion: "r1", Verdict: sessionjudge.VerdictContradicted, Target: "t1", Event: "t1.e1", Whole: true}}}
		}, func(o map[string]*sessionjudge.Outcome, _ []sessionjudge.Finding, _ *sessionjudge.Summary) string {
			if t1 := o["t1"]; t1.Class != sessionjudge.ClassPartly || len(t1.References) != 0 {
				return fmt.Sprintf("t1 %s %v", t1.Class, t1.References)
			}
			return ""
		}},
		{"sources that disagree", func(w *workedRun) {
			w.achievements[0].Contradictions = nil
			w.refs = references{conclusions: []sessionjudge.Conclusion{{ID: "r1", Scope: []string{"in/1"}}, {ID: "r2", Scope: []string{"in/2"}, Conflicts: []string{"r1"}}},
				divergences: []*sessionjudge.Divergence{{Conclusion: "r1", Verdict: sessionjudge.VerdictContradicted, Target: "t1", Event: "t1.e3", Whole: true}}}
		}, func(o map[string]*sessionjudge.Outcome, _ []sessionjudge.Finding, _ *sessionjudge.Summary) string {
			if t1 := o["t1"]; t1.Class != sessionjudge.ClassPartly || len(t1.References) != 0 {
				return fmt.Sprintf("t1 %s %v", t1.Class, t1.References)
			}
			return ""
		}},
		{"a disagreement about another target", func(w *workedRun) {
			w.achievements[0].Contradictions = nil
			w.refs = references{conclusions: []sessionjudge.Conclusion{{ID: "r1", Scope: []string{"in/1"}, Conflicts: []string{"r2"}},
				{ID: "r2", Scope: []string{"in/3"}, Conflicts: []string{"r1"}}},
				divergences: []*sessionjudge.Divergence{{Conclusion: "r1", Verdict: sessionjudge.VerdictContradicted, Target: "t1", Event: "t1.e3", Criteria: []string{"t1.c1"}}}}
		}, func(o map[string]*sessionjudge.Outcome, _ []sessionjudge.Finding, _ *sessionjudge.Summary) string {
			// r2 is about t2 only, so its disagreement with r1 does not stop
			// r1 applying to t1, but the one r2 names stops r2.
			if t1 := o["t1"]; t1.Class != sessionjudge.ClassContradicted || fmt.Sprint(t1.References) != "[r1]" {
				return fmt.Sprintf("t1 %s %v", t1.Class, t1.References)
			}
			return ""
		}},
		{"a source reports a rollback", func(w *workedRun) {
			w.achievements[0].Contradictions = nil
			w.refs = references{conclusions: []sessionjudge.Conclusion{{ID: "r1", Statement: "0.4.0 was rolled back", Scope: []string{"in/1"}}},
				rollbacks: []*sessionjudge.Rollback{{Conclusion: "r1", Target: "t1", Event: "t1.e3", Criteria: []string{"t1.c1"}}}}
		}, func(o map[string]*sessionjudge.Outcome, found []sessionjudge.Finding, _ *sessionjudge.Summary) string {
			if t1 := o["t1"]; t1.Class != sessionjudge.ClassReverted || fraction(t1.ScoreFinal) != "0" || fmt.Sprint(t1.References) != "[r1]" {
				return fmt.Sprintf("t1 %s %s %v", t1.Class, fraction(t1.ScoreFinal), t1.References)
			}
			if !strings.Contains(fmt.Sprint(found), "the delivered result was undone: conclusion r1 reports it rolled back: 0.4.0 was rolled back") {
				return fmt.Sprint(found)
			}
			return ""
		}},
		{"a rollback of a delivery corrected since", func(w *workedRun) {
			w.achievements[0].Contradictions = nil
			w.refs = references{conclusions: []sessionjudge.Conclusion{{ID: "r1", Scope: []string{"in/1"}}},
				rollbacks: []*sessionjudge.Rollback{{Conclusion: "r1", Target: "t1", Event: "t1.e1"}}}
		}, func(o map[string]*sessionjudge.Outcome, _ []sessionjudge.Finding, _ *sessionjudge.Summary) string {
			if t1 := o["t1"]; t1.Class != sessionjudge.ClassPartly || len(t1.References) != 0 {
				return fmt.Sprintf("t1 %s %v", t1.Class, t1.References)
			}
			return ""
		}},
		{"a rollback of some criteria beside an undoing of all", func(w *workedRun) {
			a := w.achievements[0]
			a.Contradictions, a.Events = nil, a.Events[:2]
			w.refs = references{conclusions: []sessionjudge.Conclusion{{ID: "r1", Scope: []string{"in/1"}}},
				rollbacks: []*sessionjudge.Rollback{{Conclusion: "r1", Target: "t1", Event: "t1.e1", Criteria: []string{"t1.c2"}}}}
		}, func(o map[string]*sessionjudge.Outcome, _ []sessionjudge.Finding, _ *sessionjudge.Summary) string {
			// The undoing took the whole result back, whatever the rollback
			// names.
			if t1 := o["t1"]; t1.Class != sessionjudge.ClassReverted || fraction(t1.ScoreFinal) != "0" {
				return fmt.Sprintf("t1 %s %s", t1.Class, fraction(t1.ScoreFinal))
			}
			return ""
		}},
		{"a rollback beside an undoing", func(w *workedRun) {
			a := w.achievements[0]
			a.Contradictions, a.Events = nil, a.Events[:2]
			a.Events[1].Criteria = []string{"t1.c2"}
			w.refs = references{conclusions: []sessionjudge.Conclusion{{ID: "r1", Scope: []string{"in/1"}}},
				rollbacks: []*sessionjudge.Rollback{{Conclusion: "r1", Target: "t1", Event: "t1.e1", Criteria: []string{"t1.c1"}}}}
		}, func(o map[string]*sessionjudge.Outcome, _ []sessionjudge.Finding, _ *sessionjudge.Summary) string {
			// Both criteria are taken back, so nothing is met.
			if t1 := o["t1"]; t1.Class != sessionjudge.ClassReverted || fraction(t1.ScoreFinal) != "0" {
				return fmt.Sprintf("t1 %s %s", t1.Class, fraction(t1.ScoreFinal))
			}
			return ""
		}},
		{"a replaced target", func(w *workedRun) { w.targets[0].Status = sessionjudge.TargetReplaced },
			func(o map[string]*sessionjudge.Outcome, _ []sessionjudge.Finding, _ *sessionjudge.Summary) string {
				if t1 := o["t1"]; t1.Class != sessionjudge.ClassReplaced || t1.ScoreRecord != nil || t1.ScoreFinal != nil {
					return fmt.Sprintf("t1 %s %s %s", t1.Class, fraction(t1.ScoreRecord), fraction(t1.ScoreFinal))
				}
				return ""
			}},
		{"a contradicted claim", func(w *workedRun) {
			a := w.achievements[0]
			a.Contradictions = nil
			a.Claims = []sessionjudge.Check{{Text: "the tests pass", State: sessionjudge.CheckContradicted}}
		}, func(o map[string]*sessionjudge.Outcome, _ []sessionjudge.Finding, _ *sessionjudge.Summary) string {
			if t1 := o["t1"]; t1.Class != sessionjudge.ClassPartly || fraction(t1.ScoreRecord) != "0.3333" || fraction(t1.ScoreFinal) != "0.3333" {
				return fmt.Sprintf("t1 %s %s %s", t1.Class, fraction(t1.ScoreRecord), fraction(t1.ScoreFinal))
			}
			return ""
		}},
		{"a talk another target took", func(w *workedRun) {
			w.inputs[1].Actions = []sessionjudge.Action{{Target: "t2", Action: sessionjudge.ActionCreate}}
			w.targets[1].Talks = append(w.targets[1].Talks, "talk/2")
		}, func(o map[string]*sessionjudge.Outcome, _ []sessionjudge.Finding, _ *sessionjudge.Summary) string {
			// t1's round ran in calls t2 received, so it cost t1 nothing, and
			// the file c3 wrote is t2's alone.
			if r := o["t1"].Rework; len(r) != 0 {
				return fmt.Sprint(r)
			}
			if fmt.Sprint(o["t1"].Operations) != "[{o1 <nil>}]" || fmt.Sprint(o["t2"].Operations) != "[{o2 <nil>} {o3 <nil>}]" {
				return fmt.Sprint(o["t1"].Operations, o["t2"].Operations)
			}
			return ""
		}},
		{"rework priced by how its calls were labeled", func(w *workedRun) {
			w.labels = []*sessionjudge.Label{{Target: "t1", Talk: "talk/2", Label: sessionjudge.WorkRepair}}
		}, func(o map[string]*sessionjudge.Outcome, _ []sessionjudge.Finding, _ *sessionjudge.Summary) string {
			// The round's own cause does not price it: its calls' label does.
			if r := o["t1"].Rework; len(r) != 1 || r[0].Work != sessionjudge.WorkRepair || r[0].Cause != sessionjudge.CauseMain || r[0].Cost.Usage.Input != 50 {
				return fmt.Sprint(r)
			}
			return ""
		}},
		{"a correction no round of rework began at", func(w *workedRun) { w.achievements[0].Rework = nil },
			func(o map[string]*sessionjudge.Outcome, found []sessionjudge.Finding, _ *sessionjudge.Summary) string {
				if len(o["t1"].Rework) != 0 || len(found) != 3 || found[2].At != "in/2" || found[2].Problem != "the person corrected the target" {
					return fmt.Sprint(o["t1"].Rework, found)
				}
				return ""
			}},
		{"rounds that overlap", func(w *workedRun) {
			a := w.achievements[0]
			a.Rework = append([]sessionjudge.Rework{{ID: "t1.r0", Cause: sessionjudge.ReworkUnknown, From: "in/1", To: "r1"}}, a.Rework...)
			a.Rework = append(a.Rework, sessionjudge.Rework{ID: "t1.r2", Cause: sessionjudge.ReworkRequirementChange, From: "s2", To: "s3"})
			w.labels = []*sessionjudge.Label{{Target: "t1", Talk: "talk/1", Label: sessionjudge.WorkAvoidable},
				{Target: "t1", Talk: "talk/2", Label: sessionjudge.WorkRepair}}
		}, func(o map[string]*sessionjudge.Outcome, _ []sessionjudge.Finding, _ *sessionjudge.Summary) string {
			// r0 holds c1, the call that asked for its last step, and no
			// later call; r1 and r2 both hold c2 and c3, counted once. Each
			// is priced by its label.
			if got := fmt.Sprint(o["t1"].Rework); got != "[{avoidable_attempt main {{10 0 0 0} <nil> 1 0 0}} {repair_own_mistake main {{50 0 0 0} <nil> 2 0 0}}]" {
				return got
			}
			return ""
		}},
		{"a call two targets share", func(w *workedRun) {
			w.inputs[1].Actions = append(w.inputs[1].Actions, sessionjudge.Action{Target: "t2", Action: sessionjudge.ActionCreate})
			w.targets[1].Talks = append(w.targets[1].Talks, "talk/2")
		}, func(o map[string]*sessionjudge.Outcome, _ []sessionjudge.Finding, _ *sessionjudge.Summary) string {
			if ops := o["t2"].Operations; len(ops) != 2 || ops[0].ID != "o2" || ops[0].Share == nil || *ops[0].Share != 0.5 || ops[1].Share != nil {
				return fmt.Sprint(ops)
			}
			return ""
		}},
		{"an operation of a call no target received", func(w *workedRun) { w.targets[0].Talks = []string{"talk/2"} },
			func(o map[string]*sessionjudge.Outcome, _ []sessionjudge.Finding, sum *sessionjudge.Summary) string {
				if fmt.Sprint(sum.UnassignedOperations) != "[o1]" || fmt.Sprint(o["t1"].Operations) != "[{o2 <nil>}]" {
					return fmt.Sprint(sum.UnassignedOperations, o["t1"].Operations)
				}
				return ""
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := workedDecisions()
			tc.change(w)
			o, found, sum := w.closed(t)
			if why := tc.check(o, found, sum); why != "" {
				t.Error(why)
			}
		})
	}
}
