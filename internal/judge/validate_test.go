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
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// navWithInputs is the test document with a person's input in each main talk.
func navWithInputs() *Nav {
	d := doc()
	for i := range d.Talks {
		if d.Talks[i].Stream == "main" {
			d.Talks[i].Children = append([]sessionview.Node{{ID: "in/" + d.Talks[i].ID, Kind: "message.external", Text: "do it"}}, d.Talks[i].Children...)
		}
	}
	return NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
}

// An answer is kept only as far as the conversation holds what it names:
// invented citations, talks that are no main talk and answers for no input
// are left out and counted, an input left unanswered is unknown, and the
// stage says all of it.
func TestATargetsAnswerIsCheckedAgainstTheConversation(t *testing.T) {
	nav := navWithInputs()
	raw := json.RawMessage(`{"targets":[
	  {"id":"t1","statement":"s","criteria":["c"],"status":"done","replaced_by":"t9","talks":["talk/1","talk/a","talk/1"],"cite":["in/talk/1","made/up"]},
	  {"id":"t1","statement":"again","criteria":[],"status":"open","replaced_by":"","talks":[],"cite":[]}],
	 "inputs":[{"node":"in/talk/1","actions":[{"target":"t1","action":"create"},{"target":"t2","action":"refine"}]},
	           {"node":"not/an/input","actions":[{"target":"","action":"none"}]}]}`)
	c, err := checkTargets(nav, catalogue{}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.targets) != 1 {
		t.Fatalf("targets %+v", c.targets)
	}
	tg := c.targets[0]
	if tg.Status != sessionjudge.TargetOpen || tg.ReplacedBy != "" || len(tg.Talks) != 1 || tg.Talks[0] != "talk/1" || len(tg.Cite) != 1 {
		t.Fatalf("target %+v", tg)
	}
	var inputs []*sessionjudge.Input
	for _, f := range c.frames {
		if in, ok := f.(*sessionjudge.Input); ok {
			inputs = append(inputs, in)
		}
	}
	if len(inputs) != 2 || len(inputs[0].Actions) != 1 || inputs[1].Actions[0].Action != sessionjudge.ActionUnknown {
		t.Fatalf("inputs %+v %+v", inputs[0], inputs[1])
	}
	note := c.note()
	for _, want := range []string{"key t1 was given twice", "status \"done\"", "replaced by \"t9\"", "acted on \"t2\"", "not an input of this call",
		"1 inputs were not answered", "1 talk ids", "1 citations"} {
		if !strings.Contains(note, want) {
			t.Errorf("the note does not say %q: %s", want, note)
		}
	}
	if *c.coverage.Inputs != 2 || *c.coverage.InputsRead != 1 {
		t.Fatalf("coverage %+v", c.coverage)
	}
}

// A label is kept only on a talk of its own target, and a part only from a
// step of that talk; not_reviewed is asz's word and is never taken from an
// evaluator.
func TestLabelsStayOnTheirTargetsTalks(t *testing.T) {
	nav := navWithInputs()
	targets := []sessionjudge.Target{{ID: "t1", Talks: []string{"talk/1"}}}
	raw := json.RawMessage(`{"labels":[
	  {"target":"t1","talk":"talk/1","from":"c1","label":"useful","note":"","cite":["c1"]},
	  {"target":"t1","talk":"talk/1","from":"s1","label":"repeated","note":"","cite":["talk/1"]},
	  {"target":"t1","talk":"talk/2","from":"","label":"useful","note":"","cite":[]},
	  {"target":"t1","talk":"talk/1","from":"c2","label":"repeated","note":"","cite":["c2"]},
	  {"target":"t1","talk":"talk/1","from":"","label":"not_reviewed","note":"","cite":[]},
	  {"target":"t1","talk":"talk/1","from":"s1","label":"avoidable_attempt","note":"","cite":["made/up"]}]}`)
	c, err := checkEfficiency(nav, targets, nil, false, raw)
	if err != nil {
		t.Fatal(err)
	}
	// Only the part from c1, a step of talk/1, is kept. The repeated part
	// starting at c2, a step of another talk, is left out rather than spread
	// over the whole talk.
	if len(c.frames) != 1 || c.frames[0].(*sessionjudge.Label).From != "c1" {
		t.Fatalf("frames %+v", c.frames)
	}
	// An avoidable label with nothing valid behind it is left out, so its
	// cost is not reviewed rather than charged.
	if !strings.Contains(c.note(), `repeated label on talk talk/1 starting at "c2"`) ||
		!strings.Contains(c.note(), "the avoidable_attempt label on talk talk/1 cites no step of it") ||
		!strings.Contains(c.note(), "the repeated label on talk talk/1 cites no step of it") {
		t.Fatalf("note %s", c.note())
	}
	// An answer that labels nothing says so.
	empty, err := checkEfficiency(nav, targets, nil, false, json.RawMessage(`{"labels":[]}`))
	if err != nil || !strings.Contains(empty.note(), "1 of 1 talks were not labeled") {
		t.Fatalf("an empty answer: %v %s", err, empty.note())
	}
	// So does one that labels some of the talks it was given and not all.
	two := []sessionjudge.Target{{ID: "t1", Talks: []string{"talk/1", "talk/2"}}}
	some, err := checkEfficiency(nav, two, nil, false, json.RawMessage(`{"labels":[
	  {"target":"t1","talk":"talk/1","from":"","label":"useful","note":"","cite":["c1"]}]}`))
	if err != nil || len(some.frames) != 1 || some.note() != "1 of 2 talks were not labeled" {
		t.Fatalf("a partial answer: %v %+v %s", err, some.frames, some.note())
	}
	// An answer without its key is refused, not read as nothing found.
	if _, err := checkEfficiency(nav, targets, nil, false, json.RawMessage(`{}`)); err == nil {
		t.Fatal("an answer without labels was accepted")
	}
}

// A criterion and a claim each keep their own vocabulary, and whatever is
// left out is said.
func TestAchievementStatesAreCheckedApart(t *testing.T) {
	nav := navWithInputs()
	targets := []sessionjudge.Target{{ID: "t1", Talks: []string{"talk/1"},
		Criteria: []sessionjudge.Criterion{{ID: "t1.c1", Text: "a"}, {ID: "t1.c2", Text: "b"}}}}
	raw := json.RawMessage(`{"achievements":[{"target":"t1","basis":"observed_result",
	  "criteria":[{"id":"t1.c1","state":"met","cite":["s1"]},{"id":"t1.c2","state":"supported","cite":[]},{"id":"t9.c1","state":"met","cite":[]}],
	  "claims":[{"text":"c","state":"contradicted","cite":["s1"]},{"text":"d","state":"met","cite":[]}],
	  "note":"","cite":[]}]}`)
	c, err := checkAchievement(nav, targets, raw)
	if err != nil {
		t.Fatal(err)
	}
	a := c.frames[0].(*sessionjudge.Achievement)
	if len(a.Criteria) != 2 || a.Criteria[1].State != sessionjudge.CheckUnknown || len(a.Claims) != 1 ||
		a.Criteria[0].Text != "a" || a.Level != sessionjudge.LevelPartly {
		t.Fatalf("criteria %+v claims %+v level %s", a.Criteria, a.Claims, a.Level)
	}
	for _, want := range []string{`criterion t1.c2 in state "supported"`, `claim of target t1 in state "met"`, `answered "t9.c1"`} {
		if !strings.Contains(c.note(), want) {
			t.Errorf("the note does not say %q: %s", want, c.note())
		}
	}
}

// navWithReply is navWithInputs with the agent's reply in talk/2.
func navWithReply() *Nav {
	d := doc()
	for i := range d.Talks {
		if d.Talks[i].Stream == "main" {
			d.Talks[i].Children = append([]sessionview.Node{{ID: "in/" + d.Talks[i].ID, Kind: "message.external", Text: "do it"}}, d.Talks[i].Children...)
		}
		if d.Talks[i].ID == "talk/2" {
			d.Talks[i].Children = append(d.Talks[i].Children, sessionview.Node{ID: "reply/2", Kind: "message.assistant", Text: "done"})
		}
	}
	return NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
}

// The level is asz's, from the criteria: a criterion left out is unknown, a
// met or not met with nothing valid behind it is unknown, and a met that
// rests on the agent's words alone is unknown unless the target was an answer
// the reply delivered. A basis nothing cited shows is weakened.
func TestTheLevelComesFromTheCriteria(t *testing.T) {
	nav := navWithReply()
	two := func(id, talk string) sessionjudge.Target {
		return sessionjudge.Target{ID: id, Talks: []string{talk},
			Criteria: []sessionjudge.Criterion{{ID: id + ".c1", Text: "x"}, {ID: id + ".c2", Text: "y"}}}
	}
	targets := []sessionjudge.Target{two("t1", "talk/1"), two("t2", "talk/2"), two("t3", "talk/1"), two("t4", "talk/2"),
		{ID: "t5", Talks: []string{"talk/1"}}, two("t6", "talk/1")}
	raw := json.RawMessage(`{"achievements":[
	  {"target":"t1","basis":"observed_result","criteria":[{"id":"t1.c1","state":"met","cite":["s1"]}],"claims":[],"note":"","cite":[]},
	  {"target":"t2","basis":"observed_result","criteria":[{"id":"t2.c1","state":"met","cite":["c2"]},{"id":"t2.c2","state":"met","cite":["talk/2"]}],
	   "claims":[{"text":"tests pass","state":"supported","cite":["reply/2"]}],"note":"","cite":[]},
	  {"target":"t3","basis":"observed_result","criteria":[{"id":"t3.c1","state":"met","cite":[]},{"id":"t3.c2","state":"not_met","cite":["s3a"]}],"claims":[],"note":"","cite":[]},
	  {"target":"t4","basis":"delivered_answer","criteria":[{"id":"t4.c1","state":"met","delivered":true,"cite":["reply/2"]},{"id":"t4.c2","state":"met","delivered":true,"cite":["reply/2"]}],"claims":[],"note":"","cite":[]},
	  {"target":"t5","basis":"no_evidence","criteria":[],"claims":[],"note":"","cite":[]},
	  {"target":"t6","basis":"no_evidence","criteria":[{"id":"t6.c1","state":"not_met","cite":["made/up"]}],"claims":[],"note":"","cite":[]}]}`)
	c, err := checkAchievement(nav, targets, raw)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*sessionjudge.Achievement{}
	for _, f := range c.frames {
		a := f.(*sessionjudge.Achievement)
		got[a.Target] = a
	}
	want := map[string][2]string{
		// t1: c2 left out, so partly, never achieved.
		"t1": {sessionjudge.LevelPartly, sessionjudge.BasisObserved},
		// t2: the agent's own call and a whole talk show nothing; no tool was
		// cited, so the basis is the agent's claim.
		"t2": {sessionjudge.LevelCannotVerify, sessionjudge.BasisClaimOnly},
		// t3: c1 met on nothing is unknown, and c2 not met stands.
		"t3": {sessionjudge.LevelNotAchieved, sessionjudge.BasisObserved},
		// t4: an answer the reply delivered.
		"t4": {sessionjudge.LevelAchieved, sessionjudge.BasisDelivered},
		// t5: no criterion at all.
		"t5": {sessionjudge.LevelCannotVerify, sessionjudge.BasisNone},
		// t6: a failure cited only by an id that does not exist shows
		// nothing.
		"t6": {sessionjudge.LevelCannotVerify, sessionjudge.BasisNone},
	}
	for id, w := range want {
		if a := got[id]; a == nil || a.Level != w[0] || a.Basis != w[1] {
			t.Errorf("%s: %+v, want %v", id, a, w)
		}
	}
	if got["t2"].Claims[0].State != sessionjudge.CheckNoEvidence {
		t.Errorf("a claim the agent's reply alone supports was kept: %+v", got["t2"].Claims)
	}
	for _, want := range []string{"2 criteria were not answered", "rests on observed_result", "were weakened"} {
		if !strings.Contains(c.note(), want) {
			t.Errorf("the note does not say %q: %s", want, c.note())
		}
	}
}

// A sub-task decision names sub-tasks the ledger holds, groups only those one
// step started together, is taken once per sub-task, and claims nothing it
// cannot cite.
func TestSubtaskDecisionsAreChecked(t *testing.T) {
	nav := navWithInputs()
	raw := json.RawMessage(`{"subtasks":[
	  {"ids":["stream/a"],"outcome":"achieved","report":"accurate","use":"used","dispatch":"reasonable","work":"useful","note":"","cite":["s2","s3a"]},
	  {"ids":["stream/b","stream/c"],"outcome":"achieved","report":"accurate","use":"used","dispatch":"reasonable","work":"useful","note":"","cite":["s2","s3a"]},
	  {"ids":["stream/d"],"outcome":"achieved","report":"accurate","use":"unknown","dispatch":"redundant","work":"repeated","note":"","cite":[]},
	  {"ids":["stream/a"],"outcome":"not_achieved","report":"inaccurate","use":"unused","dispatch":"reasonable","work":"useful","note":"","cite":["s2"]},
	  {"ids":["stream/x"],"outcome":"achieved","report":"accurate","use":"used","dispatch":"reasonable","work":"useful","note":"","cite":["s2"]}]}`)
	c, err := checkSubtasks(nav, raw)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*sessionjudge.Subtask{}
	for _, f := range c.frames {
		st := f.(*sessionjudge.Subtask)
		got[st.ID] = st
	}
	// a's result is used by a later step of the talk that started it.
	if a := got["stream/a"]; a == nil || a.Outcome != sessionjudge.LevelAchieved || a.StartedBy != "s1" || a.Parent != "talk/1" || a.Use != sessionjudge.UseUsed {
		t.Fatalf("a %+v", a)
	}
	// b and c, judged together on a step of a's work, have no evidence of
	// their own: each is kept, with nothing claimed.
	for _, id := range []string{"stream/b", "stream/c"} {
		if st := got[id]; st == nil || st.Outcome != sessionjudge.LevelCannotVerify || st.Report != sessionjudge.ReportUnverifiable {
			t.Fatalf("%s %+v", id, st)
		}
	}
	if d := got["stream/d"]; d == nil || d.Outcome != sessionjudge.LevelCannotVerify || d.Report != sessionjudge.ReportUnverifiable ||
		d.Dispatch != sessionjudge.DispatchUnknown || d.Work != sessionjudge.WorkNotReviewed {
		t.Fatalf("d %+v", d)
	}
	// b and c also had their work weakened, since nothing of their own was
	// read, and their use: b was started in a's stream, and a step of the
	// main stream shows nothing of what a did next.
	if got["stream/b"].Work != sessionjudge.WorkNotReviewed || got["stream/b"].Use != sessionjudge.UseUnknown {
		t.Fatalf("b %+v", got["stream/b"])
	}
	for _, want := range []string{"1 ids that are no sub-task", "1 sub-tasks judged twice", "12 decisions without valid evidence"} {
		if !strings.Contains(c.note(), want) {
			t.Errorf("the note does not say %q: %s", want, c.note())
		}
	}
	if *c.coverage.Subtasks != 4 || *c.coverage.SubtasksJudged != 4 {
		t.Fatalf("coverage %+v", c.coverage)
	}
}

// A targets scan merges each chunk into the catalogue the earlier chunks
// left: an update names a target by its id, a new target gets the next id in
// the order its first input came, a criterion is numbered once and a dropped
// one is marked, never removed; and the catalogue passed in is not changed.
func TestATargetsScanMergesItsChunks(t *testing.T) {
	nav := navWithInputs()
	nav.Assign(Assignment{Inputs: []string{"in/talk/2"}})
	found := []sessionjudge.Target{{ID: "t1", Statement: "build it", Status: sessionjudge.TargetOpen, Talks: []string{"talk/1"},
		Criteria: []sessionjudge.Criterion{{ID: "t1.c1", Text: "it builds"}}, Cite: []sessionjudge.Cite{}}}
	raw := json.RawMessage(`{"targets":[
	  {"id":"","key":"n7","statement":"ship it","criteria":["a release exists"],"drop_criteria":[],"status":"open","replaced_by":"","talks":["talk/2"],"cite":["in/talk/2"]},
	  {"id":"t1","key":"","statement":"","criteria":["tests pass"],"drop_criteria":["t1.c1"],"status":"closed","replaced_by":"","talks":["talk/2"],"cite":[]}],
	 "inputs":[{"node":"in/talk/2","actions":[{"target":"n7","action":"create"},{"target":"t1","action":"refine"}]},
	           {"node":"in/talk/1","actions":[{"target":"t1","action":"steer"}]}]}`)
	c, err := checkTargets(nav, catalogue{targets: found}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.targets) != 2 || len(found[0].Criteria) != 1 || found[0].Status != sessionjudge.TargetOpen {
		t.Fatalf("targets %+v, the catalogue passed in %+v", c.targets, found)
	}
	t1, t2 := c.targets[0], c.targets[1]
	if t1.Status != sessionjudge.TargetClosed || t1.Statement != "build it" || len(t1.Criteria) != 2 ||
		!t1.Criteria[0].Dropped || t1.Criteria[1].ID != "t1.c2" || len(t1.Talks) != 2 {
		t.Fatalf("t1 %+v", t1)
	}
	if t2.ID != "t2" || t2.Criteria[0].ID != "t2.c1" || t2.Statement != "ship it" {
		t.Fatalf("t2 %+v", t2)
	}
	in := c.frames[0].(*sessionjudge.Input)
	if len(c.frames) != 1 || in.Node != "in/talk/2" || in.Actions[0].Target != "t2" || in.Actions[1].Target != "t1" {
		t.Fatalf("inputs %+v", c.frames)
	}
	if !strings.Contains(c.note(), `"in/talk/1", which is not an input of this call`) {
		t.Fatalf("note %s", c.note())
	}
	// A dropped criterion is never judged.
	a, err := checkAchievement(nav, c.targets[:1], json.RawMessage(`{"achievements":[{"target":"t1","basis":"observed_result",
	  "criteria":[{"id":"t1.c2","state":"met","cite":["s1"]}],"claims":[],"note":"","cite":[]}]}`))
	if err != nil || a.frames[0].(*sessionjudge.Achievement).Level != sessionjudge.LevelAchieved {
		t.Fatalf("achievement %+v %v %s", a.frames, err, a.note())
	}
}

// An answer asked for within a change is met by the reply that delivered
// it, and by nothing else that only the agent said.
func TestADeliveredCriterionRestsOnItsReply(t *testing.T) {
	nav := navWithReply()
	targets := []sessionjudge.Target{{ID: "t1", Talks: []string{"talk/2"}, Criteria: []sessionjudge.Criterion{
		{ID: "t1.c1", Text: "the fix works"}, {ID: "t1.c2", Text: "explain the cause"}, {ID: "t1.c3", Text: "explain the risk"}}}}
	raw := json.RawMessage(`{"achievements":[{"target":"t1","basis":"observed_result","criteria":[
	  {"id":"t1.c1","state":"met","delivered":false,"cite":["s3b"]},
	  {"id":"t1.c2","state":"met","delivered":true,"cite":["reply/2"]},
	  {"id":"t1.c3","state":"met","delivered":true,"cite":["c2"]}],"claims":[],"note":"","cite":["s3b"]}]}`)
	c, err := checkAchievement(nav, targets, raw)
	if err != nil {
		t.Fatal(err)
	}
	a := c.frames[0].(*sessionjudge.Achievement)
	if a.Criteria[0].State != "met" || a.Criteria[1].State != "met" || a.Criteria[2].State != "unknown" || a.Level != "partly" {
		t.Fatalf("criteria %+v level %s", a.Criteria, a.Level)
	}
}

// A talk row names the reply that ends it, the step to cite when it
// delivered an answer.
func TestATalkRowNamesItsReply(t *testing.T) {
	nav := navWithReply()
	ans, _ := nav.Call("talks", json.RawMessage(`{"talks":["talk/2"]}`))
	if row := ans.(page).Rows[0].(talkRow); row.ReplyNode != "reply/2" {
		t.Fatalf("row %+v", row)
	}
}

// What shows that something happened: a tool that returned, or the person
// later; never the request that set the target, a tool call whose result
// never came, or a reply for work that is not an answer.
func TestEvidenceIsWhatShowsTheWorkHappened(t *testing.T) {
	d := doc()
	for i := range d.Talks {
		if d.Talks[i].ID == "talk/2" {
			d.Talks[i].Children = append(d.Talks[i].Children,
				sessionview.Node{ID: "ask/2", Kind: "message.external", Text: "fix it"},
				sessionview.Node{ID: "pending/2", Kind: "tool"},
				sessionview.Node{ID: "reply/2", Kind: "message.assistant", Text: "done"},
				sessionview.Node{ID: "thanks/2", Kind: "message.external", Text: "works, thanks"})
		}
	}
	nav := NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
	target := func(id string) sessionjudge.Target {
		return sessionjudge.Target{ID: id, Talks: []string{"talk/2"}, Cite: []sessionjudge.Cite{{Node: "ask/2"}},
			Criteria: []sessionjudge.Criterion{{ID: id + ".c1", Text: "fixed"}}}
	}
	targets := []sessionjudge.Target{target("t1"), target("t2"), target("t3"), target("t4"), target("t5")}
	raw := json.RawMessage(`{"achievements":[
	  {"target":"t1","basis":"person_accepted","criteria":[{"id":"t1.c1","state":"met","delivered":false,"cite":["ask/2"]}],"claims":[],"note":"","cite":["ask/2"]},
	  {"target":"t2","basis":"observed_result","criteria":[{"id":"t2.c1","state":"met","delivered":false,"cite":["pending/2"]}],"claims":[],"note":"","cite":[]},
	  {"target":"t3","basis":"person_accepted","criteria":[{"id":"t3.c1","state":"met","delivered":false,"cite":["thanks/2"]}],"claims":[],"note":"","cite":["thanks/2"]},
	  {"target":"t4","basis":"observed_result","criteria":[{"id":"t4.c1","state":"met","delivered":true,"cite":["s3b"]}],"claims":[],"note":"","cite":["s3b"]},
	  {"target":"t5","basis":"observed_result","criteria":[{"id":"t5.c1","state":"met","delivered":false,"cite":["s3b"]}],
	   "claims":[{"text":"the tests pass","state":"contradicted","cite":["thanks/2"]}],"note":"","cite":["s3b"]}]}`)
	c, err := checkAchievement(nav, targets, raw)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*sessionjudge.Achievement{}
	for _, f := range c.frames {
		a := f.(*sessionjudge.Achievement)
		got[a.Target] = a
	}
	want := map[string]string{
		"t1": sessionjudge.LevelCannotVerify, // the request is no evidence of the result
		"t2": sessionjudge.LevelCannotVerify, // a tool whose result never came shows nothing
		"t3": sessionjudge.LevelAchieved,     // the person's later word does
		"t4": sessionjudge.LevelAchieved,     // a tool result stands, delivered or not
		"t5": sessionjudge.LevelPartly,       // a contradicted claim keeps it below achieved
	}
	for id, w := range want {
		if got[id].Level != w {
			t.Errorf("%s: %s, want %s (%+v)", id, got[id].Level, w, got[id].Criteria)
		}
	}
	if got["t1"].Basis == sessionjudge.BasisAccepted {
		t.Error("the request that set a target was taken as the person accepting it")
	}
}

// A sub-task may be left not reviewed by the evaluator itself.
func TestASubtaskMayBeLeftNotReviewed(t *testing.T) {
	nav := navWithInputs()
	nav.Assign(Assignment{Subtasks: []string{"stream/a"}})
	c, err := checkSubtasks(nav, json.RawMessage(`{"subtasks":[{"ids":["stream/a"],"outcome":"cannot_verify","report":"unverifiable",
	  "use":"unknown","dispatch":"reasonable","work":"not_reviewed","note":"its work was not read","cite":[]}]}`))
	if err != nil || len(c.frames) != 1 || c.frames[0].(*sessionjudge.Subtask).Work != sessionjudge.WorkNotReviewed || len(c.problems) != 0 {
		t.Fatalf("frames %+v %v %s", c.frames, err, c.note())
	}
}

// The second confirmation's cases: an empty result that came is a result; the
// person accepting needs the person's later word, not the request beside a
// tool result; a reply shows a requested answer missing as well as given; an
// avoidable label needs what shows why, not the agent's account of itself.
func TestEvidenceCasesFromTheConfirmation(t *testing.T) {
	d := doc()
	for i := range d.Talks {
		if d.Talks[i].ID == "talk/2" {
			d.Talks[i].Children = append(d.Talks[i].Children,
				sessionview.Node{ID: "ask/2", Kind: "message.external", Text: "fix it and explain"},
				sessionview.Node{ID: "empty/2", Kind: "tool", ResultState: "available"},
				sessionview.Node{ID: "reply/2", Kind: "message.assistant", Text: "fixed"})
		}
	}
	nav := NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
	target := func(id string) sessionjudge.Target {
		return sessionjudge.Target{ID: id, Talks: []string{"talk/2"}, Cite: []sessionjudge.Cite{{Node: "ask/2"}},
			Criteria: []sessionjudge.Criterion{{ID: id + ".c1", Text: "fixed"}}}
	}
	raw := json.RawMessage(`{"achievements":[
	  {"target":"t1","basis":"observed_result","criteria":[{"id":"t1.c1","state":"met","delivered":false,"cite":["empty/2"]}],"claims":[],"note":"","cite":[]},
	  {"target":"t2","basis":"person_accepted","criteria":[{"id":"t2.c1","state":"met","delivered":false,"cite":["empty/2"]}],"claims":[],"note":"","cite":["ask/2","empty/2"]},
	  {"target":"t3","basis":"delivered_answer","criteria":[{"id":"t3.c1","state":"not_met","delivered":true,"cite":["reply/2"]}],"claims":[],"note":"","cite":[]},
	  {"target":"t4","basis":"delivered_answer","criteria":[{"id":"t4.c1","state":"met","delivered":false,"cite":["reply/2"]}],"claims":[],"note":"","cite":["reply/2"]}]}`)
	c, err := checkAchievement(nav, []sessionjudge.Target{target("t1"), target("t2"), target("t3"), target("t4")}, raw)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*sessionjudge.Achievement{}
	for _, f := range c.frames {
		a := f.(*sessionjudge.Achievement)
		got[a.Target] = a
	}
	if got["t1"].Level != sessionjudge.LevelAchieved {
		t.Errorf("an empty result that came: %+v", got["t1"])
	}
	if got["t2"].Basis == sessionjudge.BasisAccepted {
		t.Errorf("the request beside a tool result was taken as the person accepting: %+v", got["t2"])
	}
	if got["t3"].Level != sessionjudge.LevelNotAchieved {
		t.Errorf("a reply missing a requested answer: %+v", got["t3"])
	}
	// A reply shows only the criteria marked delivered, whatever the basis
	// of the target: one that asks for a change is not shown by it.
	if got["t4"].Criteria[0].State != sessionjudge.CheckUnknown {
		t.Errorf("a criterion not marked delivered was shown by a reply: %+v", got["t4"])
	}
	labels, err := checkEfficiency(nav, []sessionjudge.Target{target("t1")}, nil, false, json.RawMessage(`{"labels":[
	  {"target":"t1","talk":"talk/2","from":"","label":"repeated","note":"","cite":["reply/2"]}]}`))
	if err != nil || len(labels.frames) != 0 || !strings.Contains(labels.note(), "cites nothing that shows why") {
		t.Fatalf("an avoidable label on the agent's words: %+v %s", labels.frames, labels.note())
	}
}

// Calling a sub-task's dispatch redundant, or its work avoidable, needs what
// shows it: a tool result, an error or the person's words. Its own words are
// not enough.
func TestASubtaskIsNotCalledAvoidableOnItsOwnWords(t *testing.T) {
	d := doc()
	d.Talks[1].Children = append(d.Talks[1].Children, sessionview.Node{ID: "say/a", Kind: "message.assistant", Text: "I did it twice"})
	d.Talks[3].Children = append(d.Talks[3].Children, sessionview.Node{ID: "in/talk/2", Kind: "message.external", Text: "you did that already"})
	nav := NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
	decide := func(cite string) *sessionjudge.Subtask {
		t.Helper()
		c, err := checkSubtasks(nav, json.RawMessage(`{"subtasks":[{"ids":["stream/a"],"outcome":"achieved","report":"accurate",
		  "use":"used","dispatch":"redundant","work":"repeated","note":"","cite":`+cite+`}]}`))
		if err != nil || len(c.frames) != 1 {
			t.Fatalf("%+v %v", c, err)
		}
		return c.frames[0].(*sessionjudge.Subtask)
	}
	if s := decide(`["say/a"]`); s.Dispatch != sessionjudge.DispatchUnknown || s.Work != sessionjudge.WorkNotReviewed {
		t.Fatalf("on its words alone: %+v", s)
	}
	if s := decide(`["say/a","s2"]`); s.Dispatch != sessionjudge.DispatchRedundant || s.Work != sessionjudge.WorkRedundant {
		t.Fatalf("with a tool result: %+v", s)
	}
	// The person saying it was done already shows it too.
	if s := decide(`["say/a","in/talk/2"]`); s.Dispatch != sessionjudge.DispatchRedundant || s.Work != sessionjudge.WorkRedundant {
		t.Fatalf("with the person's words: %+v", s)
	}
}

// The person accepting is the person's own input after every input that set
// the target. An earlier input, a child stream's input, which the agent
// wrote, and an input from no person do not show it.
func TestOnlyThePersonsLaterWordAcceptsAWork(t *testing.T) {
	d := doc()
	d.Talks[0].Children = append([]sessionview.Node{{ID: "early", Kind: "message.external", At: 50, Text: "first"}}, d.Talks[0].Children...)
	d.Talks[3].Children = append(d.Talks[3].Children,
		sessionview.Node{ID: "ask", Kind: "message.external", At: 600, Text: "fix it"},
		sessionview.Node{ID: "hook", Kind: "message.external", At: 650, Text: "a hook ran"},
		sessionview.Node{ID: "thanks", Kind: "message.external", At: 690, Text: "thanks, it works"})
	d.Talks[1].Children = append([]sessionview.Node{{ID: "child/in", Kind: "message.external", At: 680, Text: "check the build"}}, d.Talks[1].Children...)
	// The main stream's steps landed in the order above, talk 1 first.
	row := uint64(0)
	for _, ti := range []int{0, 3} {
		for i := range d.Talks[ti].Children {
			row++
			d.Talks[ti].Children[i].Ref = &sessionflow.Ref{Seq: 1, Row: row}
		}
	}
	nav := NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
	nav.TargetsFound([]*sessionjudge.Input{{Node: "hook", Actions: []sessionjudge.Action{{Action: sessionjudge.ActionNotPerson}}}})
	target := sessionjudge.Target{ID: "t1", Talks: []string{"talk/2"}, Cite: []sessionjudge.Cite{{Node: "ask"}},
		Criteria: []sessionjudge.Criterion{{ID: "t1.c1", Text: "fixed"}}}
	basis := func(cite string) string {
		t.Helper()
		c, err := checkAchievement(nav, []sessionjudge.Target{target}, json.RawMessage(`{"achievements":[{"target":"t1","basis":"person_accepted",
		  "criteria":[{"id":"t1.c1","state":"met","delivered":false,"cite":["s3b"]}],"claims":[],"note":"","cite":["ask",`+cite+`]}]}`))
		if err != nil || len(c.frames) != 1 {
			t.Fatalf("%+v %v", c, err)
		}
		return c.frames[0].(*sessionjudge.Achievement).Basis
	}
	if got := basis(`"thanks"`); got != sessionjudge.BasisAccepted {
		t.Fatalf("the person's later word: %s", got)
	}
	for _, cite := range []string{`"early"`, `"child/in"`, `"hook"`} {
		if got := basis(cite); got == sessionjudge.BasisAccepted {
			t.Errorf("%s was taken for the person accepting", cite)
		}
	}
	// Nor does an approval from before the request show a criterion met.
	state := func(cite string) string {
		t.Helper()
		c, err := checkAchievement(nav, []sessionjudge.Target{target}, json.RawMessage(`{"achievements":[{"target":"t1","basis":"observed_result",
		  "criteria":[{"id":"t1.c1","state":"met","delivered":false,"cite":[`+cite+`]}],"claims":[],"note":"","cite":[]}]}`))
		if err != nil || len(c.frames) != 1 {
			t.Fatalf("%+v %v", c, err)
		}
		return c.frames[0].(*sessionjudge.Achievement).Criteria[0].State
	}
	if got := state(`"early"`); got != sessionjudge.CheckUnknown {
		t.Errorf("an approval before the request: %s", got)
	}
	if got := state(`"thanks"`); got != sessionjudge.CheckMet {
		t.Errorf("the person's word after it: %s", got)
	}
	// A target that cites nothing is still bounded by the inputs the
	// targets stage found set it; with none known, no word of the person
	// shows its work.
	target.Cite = nil
	if got := state(`"thanks"`); got != sessionjudge.CheckUnknown {
		t.Errorf("no input known to have asked: %s", got)
	}
	nav.TargetsFound([]*sessionjudge.Input{{Node: "ask", Actions: []sessionjudge.Action{{Target: "t1", Action: sessionjudge.ActionCreate}}},
		{Node: "hook", Actions: []sessionjudge.Action{{Action: sessionjudge.ActionNotPerson}}}})
	if got := state(`"early"`); got != sessionjudge.CheckUnknown {
		t.Errorf("an approval before the input that set the target: %s", got)
	}
	if got := state(`"thanks"`); got != sessionjudge.CheckMet {
		t.Errorf("the person's word after the input that set the target: %s", got)
	}
	// Nor does a child stream's input show a sub-task's work was avoidable.
	c, _ := checkSubtasks(nav, json.RawMessage(`{"subtasks":[{"ids":["stream/a"],"outcome":"cannot_verify","report":"unverifiable",
	  "use":"used","dispatch":"redundant","work":"useful","note":"","cite":["child/in"]}]}`))
	if s := c.frames[0].(*sessionjudge.Subtask); s.Dispatch != sessionjudge.DispatchUnknown {
		t.Fatalf("a child's input showed the dispatch redundant: %+v", s)
	}
}

// A sub-task's result is used when the agent that asked acts on it: a step
// of its own, in its stream, after the step that started the sub-task, its
// final output when it is a sub-agent passing the result on included. A
// step before it, an input the agent was sent, and the starting step, whose
// result is only the report arriving, show nothing.
func TestUseIsShownByWhatTheAskingAgentDidNext(t *testing.T) {
	d := doc()
	for i := range d.Talks[0].Children {
		if d.Talks[0].Children[i].ID == "s1" {
			d.Talks[0].Children[i].At = 150
		}
	}
	// before was recorded ahead of s1, the step that started a; the rest
	// after it.
	kids := d.Talks[0].Children
	d.Talks[0].Children = append([]sessionview.Node{kids[0], {ID: "before", Kind: "tool", At: 120, Result: "ok"}}, kids[1:]...)
	d.Talks[0].Children = append(d.Talks[0].Children,
		sessionview.Node{ID: "sent", Kind: "message.external", At: 300, Text: "a task finished"},
		sessionview.Node{ID: "notice", Kind: "runtime.notification", At: 310, Text: "agent a completed"},
		sessionview.Node{ID: "injected", Kind: "context.injection", At: 320, Text: "a reminder"},
		// next was recorded after s1, though its time says 10: times can go
		// back, recorded order cannot.
		sessionview.Node{ID: "next", Kind: "tool", At: 10, Result: "ok"})
	nav := NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
	use := func(cite string) string {
		t.Helper()
		c, err := checkSubtasks(nav, json.RawMessage(`{"subtasks":[{"ids":["stream/a"],"outcome":"cannot_verify","report":"unverifiable",
		  "use":"used","dispatch":"reasonable","work":"not_reviewed","note":"","cite":["`+cite+`"]}]}`))
		if err != nil || len(c.frames) != 1 {
			t.Fatalf("%+v %v", c, err)
		}
		return c.frames[0].(*sessionjudge.Subtask).Use
	}
	if got := use("next"); got != sessionjudge.UseUsed {
		t.Fatalf("the agent's next step: %s", got)
	}
	// A step of another agent's stream, a's own, landed after s1, is not
	// what the agent that asked did.
	for i := range d.Talks[0].Children {
		if c := &d.Talks[0].Children[i]; c.ID == "s1" {
			c.Ref = &sessionflow.Ref{Seq: 1, Row: 5}
		}
	}
	d.Talks[1].Children = append(d.Talks[1].Children, sessionview.Node{ID: "theirs", Kind: "tool", Result: "ok",
		Ref: &sessionflow.Ref{Seq: 1, Row: 50}})
	nav = NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
	if got := use("theirs"); got != sessionjudge.UseUnknown {
		t.Fatalf("another stream's later step: %s", got)
	}
	// b, started by a's step s2, is passed on in a's final output.
	for i := range d.Talks {
		if d.Talks[i].ID == "talk/a" {
			d.Talks[i].Children = append(d.Talks[i].Children, sessionview.Node{ID: "a/out", Kind: "agent.output", Text: "b found the cause"})
		}
	}
	nav = NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
	c, _ := checkSubtasks(nav, json.RawMessage(`{"subtasks":[{"ids":["stream/b"],"outcome":"cannot_verify","report":"unverifiable",
	  "use":"used","dispatch":"reasonable","work":"not_reviewed","note":"","cite":["a/out"]}]}`))
	if got := c.frames[0].(*sessionjudge.Subtask).Use; got != sessionjudge.UseUsed {
		t.Fatalf("passed on in the asking agent's output: %s", got)
	}
	for _, cite := range []string{"before", "sent", "notice", "injected", "s1"} {
		if got := use(cite); got != sessionjudge.UseUnknown {
			t.Errorf("%s showed the result used", cite)
		}
	}
}

// A task is called wrong only on the step that gave it and what shows it
// wrong besides, and its sub-task's work is then the asking agent's cost.
// Misled holds only on a report found untrue that the agent acted on.
func TestAWrongTaskAndAMisleadingReportNeedTheirEvidence(t *testing.T) {
	nav := NewNav(doc(), Compute(doc(), "claude-code/1", nil, nil), nil)
	decide := func(fields, cite string) *sessionjudge.Subtask {
		t.Helper()
		c, err := checkSubtasks(nav, json.RawMessage(`{"subtasks":[{"ids":["stream/a"],`+fields+`,"note":"","cite":`+cite+`}]}`))
		if err != nil || len(c.frames) != 1 {
			t.Fatalf("%+v %v", c, err)
		}
		return c.frames[0].(*sessionjudge.Subtask)
	}
	wrong := `"outcome":"achieved","report":"accurate","use":"used","dispatch":"misdirected","work":"useful"`
	// s1 gave the task; s3a, a later tool result, shows it wrong.
	if d := decide(wrong, `["s1","s3a","s2"]`); d.Dispatch != sessionjudge.DispatchMisdirected || d.Work != sessionjudge.WorkMisdirected {
		t.Fatalf("shown wrong: %+v", d)
	}
	// The step that gave it alone, or what shows it wrong without that
	// step, is not enough. A tool result of the sub-task's own does show it
	// wrong, as a command built wrong fails there.
	for _, cite := range []string{`["s1"]`, `["s3a","s2"]`} {
		if d := decide(wrong, cite); d.Dispatch != sessionjudge.DispatchUnknown || d.Work == sessionjudge.WorkMisdirected {
			t.Errorf("%s: %+v", cite, d)
		}
	}
	if d := decide(wrong, `["s1","s2"]`); d.Dispatch != sessionjudge.DispatchMisdirected {
		t.Fatalf("shown wrong by its own result: %+v", d)
	}
	// Its work cannot be called misdirected without the dispatch.
	if d := decide(`"outcome":"achieved","report":"accurate","use":"used","dispatch":"reasonable","work":"misdirected_delegation"`, `["s1","s3a","s2"]`); d.Work != sessionjudge.WorkNotReviewed {
		t.Fatalf("misdirected work on a reasonable dispatch: %+v", d)
	}
	// s2, its own tool, shows the report untrue; s3a is the agent acting on it.
	misled := `"outcome":"not_achieved","report":"inaccurate","use":"misled","dispatch":"reasonable","work":"useful"`
	if d := decide(misled, `["s2","s3a"]`); d.Use != sessionjudge.UseMisled {
		t.Fatalf("misled: %+v", d)
	}
	if d := decide(misled, `["s3a"]`); d.Report != sessionjudge.ReportUnverifiable || d.Use != sessionjudge.UseUsed {
		t.Fatalf("misled on a report not shown untrue: %+v", d)
	}
}

// A misled_by_subtask label names a sub-task the sub-task stage found misled
// the agent; no other label names one.
func TestMisledWorkNamesTheSubtask(t *testing.T) {
	// a ran in the foreground: its output came back as s1's result, in
	// the record after s1's request.
	d := doc()
	d.Relations = append(d.Relations, sessionview.Relation{ID: "s2s1", Type: model.RelResultOf, From: "s2", To: "s1"})
	for i, row := range []uint64{1, 2, 4} {
		d.Talks[0].Children[i].Ref = &sessionflow.Ref{Seq: 1, Row: row}
	}
	d.Talks[0].Children[1].Refs = []sessionflow.Ref{{Seq: 1, Row: 2}, {Seq: 1, Row: 3}}
	for i := range d.Talks[3].Children {
		d.Talks[3].Children[i].Ref = &sessionflow.Ref{Seq: 1, Row: uint64(10 + i)}
	}
	nav := NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
	targets := []sessionjudge.Target{{ID: "t1", Talks: []string{"talk/1", "talk/2"}}}
	c, err := checkEfficiency(nav, targets, map[string]bool{"stream/a": true}, false, json.RawMessage(`{"labels":[
	  {"target":"t1","talk":"talk/1","from":"s3a","label":"misled_by_subtask","subtask":"stream/a","note":"","cite":["s3a"]},
	  {"target":"t1","talk":"talk/2","from":"","label":"misled_by_subtask","subtask":"stream/c","note":"","cite":["s3b"]},
	  {"target":"t1","talk":"talk/2","from":"","label":"useful","subtask":"stream/a","note":"","cite":["s3b"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*sessionjudge.Label{}
	for _, f := range c.frames {
		l := f.(*sessionjudge.Label)
		got[l.Talk] = l
	}
	if got["talk/1"] == nil || got["talk/1"].Subtask != "stream/a" || got["talk/2"] == nil || got["talk/2"].Label != sessionjudge.WorkUseful ||
		got["talk/2"].Subtask != "" || !strings.Contains(c.note(), `names "stream/c"`) {
		t.Fatalf("labels %+v, note %s", got, c.note())
	}
}

// The same agent giving a task again shows the first one wrong; misled work
// begins after the sub-task was started; and a sub-task's own work called
// misled_by_subtask is not reviewed, its decision kept.
func TestTheBoundaryRulesFromTheReview(t *testing.T) {
	d := doc()
	// s1, at 150, started a; s5, later in the same talk, started e.
	d.Streams = append(d.Streams, sessionview.Stream{ID: "stream/e", Name: "e"})
	d.Talks = append(d.Talks, sessionview.Node{ID: "talk/e", Kind: "talk", Stream: "e", Children: []sessionview.Node{call("ce", "e", 0, "m", usage(1, 0, 0, 0))}})
	for i := range d.Talks[0].Children {
		if d.Talks[0].Children[i].ID == "s1" {
			d.Talks[0].Children[i].At = 150
		}
	}
	// In recorded order: c1, early, s1, s3a, the notice that a ended, s5,
	// a second notice of the same end. s5's time, 50, is before s1's:
	// times can go back, recorded order cannot.
	kids := d.Talks[0].Children
	d.Talks[0].Children = append([]sessionview.Node{kids[0], {ID: "early", Kind: "tool", At: 120, Result: "ok"}}, kids[1:]...)
	d.Talks[0].Children = append(d.Talks[0].Children,
		sessionview.Node{ID: "notice", Kind: "runtime.notification", At: 400, Text: "a finished"},
		sessionview.Node{ID: "s5", Kind: "agent.call", At: 50, Text: "check it again, with the new brief"},
		sessionview.Node{ID: "said", Kind: "message.assistant", At: 60, Text: "the brief I gave was wrong"},
		sessionview.Node{ID: "notice2", Kind: "runtime.notification", At: 70, Text: "a finished"})
	d.Relations = append(d.Relations, sessionview.Relation{ID: "na", Type: "reports", From: "notice", To: "stream/a"},
		sessionview.Relation{ID: "na2", Type: "reports", From: "notice2", To: "stream/a"})
	d.Relations = append(d.Relations, sessionview.Relation{ID: "s5e", Type: "starts", From: "s5", To: "stream/e"})
	// a6, in a's own stream, starts f: another agent giving a task.
	d.Streams = append(d.Streams, sessionview.Stream{ID: "stream/f", Name: "f"})
	d.Talks = append(d.Talks, sessionview.Node{ID: "talk/f", Kind: "talk", Stream: "f", Children: []sessionview.Node{call("cf", "f", 0, "m", usage(1, 0, 0, 0))}})
	d.Talks[1].Children = append(d.Talks[1].Children, sessionview.Node{ID: "a6", Kind: "agent.call", At: 310, Text: "look at it"})
	d.Relations = append(d.Relations, sessionview.Relation{ID: "a6f", Type: "starts", From: "a6", To: "stream/f"})
	// s7, in talk 2, starts g: its report cannot reach talk 1, before it.
	d.Streams = append(d.Streams, sessionview.Stream{ID: "stream/g", Name: "g"})
	d.Talks = append(d.Talks, sessionview.Node{ID: "talk/g", Kind: "talk", Stream: "g", Children: []sessionview.Node{call("cg", "g", 0, "m", usage(1, 0, 0, 0))}})
	// In talk 2, s7 gives g its task in the foreground; mid is work done
	// while g ran, before its result came back, and after is work after.
	d.Talks[3].Children = append(d.Talks[3].Children, sessionview.Node{ID: "s7", Kind: "agent.call", Text: "look again"},
		sessionview.Node{ID: "mid", Kind: "tool", Result: "ok"}, sessionview.Node{ID: "after", Kind: "tool", Result: "ok"})
	d.Relations = append(d.Relations, sessionview.Relation{ID: "s7g", Type: "starts", From: "s7", To: "stream/g"},
		sessionview.Relation{ID: "cgs7", Type: model.RelResultOf, From: "cg", To: "s7"})
	// Each main-stream step landed on its own line, in the order above,
	// and g's result came back to s7 between mid and after.
	row, one := uint64(0), 1
	for _, ti := range []int{0, 3} {
		for i := range d.Talks[ti].Children {
			row++
			d.Talks[ti].Children[i].Ref = &sessionflow.Ref{Seq: 1, Row: row}
			if d.Talks[ti].Children[i].ID == "after" {
				for j := range d.Talks[ti].Children {
					if d.Talks[ti].Children[j].ID == "s7" {
						s7 := &d.Talks[ti].Children[j]
						s7.Refs = []sessionflow.Ref{*s7.Ref, {Seq: 1, Row: row - 1, Block: &one}}
					}
				}
				d.Talks[ti].Children[i].Ref = &sessionflow.Ref{Seq: 1, Row: row + 1}
			}
		}
	}
	nav := NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
	misdirected := func(cite string) string {
		t.Helper()
		c, err := checkSubtasks(nav, json.RawMessage(`{"subtasks":[{"ids":["stream/a"],"outcome":"cannot_verify","report":"unverifiable",
		  "use":"unknown","dispatch":"misdirected","work":"useful","note":"","cite":`+cite+`}]}`))
		if err != nil || len(c.frames) != 1 {
			t.Fatalf("%+v %v", c, err)
		}
		return c.frames[0].(*sessionjudge.Subtask).Dispatch
	}
	if got := misdirected(`["s1","s5"]`); got != sessionjudge.DispatchMisdirected {
		t.Fatalf("the task given again: %s", got)
	}
	if got := misdirected(`["s1","a6"]`); got != sessionjudge.DispatchUnknown {
		t.Fatalf("another agent giving a task: %s", got)
	}
	// The agent saying so is its own account.
	if got := misdirected(`["s1","said"]`); got != sessionjudge.DispatchUnknown {
		t.Fatalf("the agent's words: %s", got)
	}
	var c *checked
	c, _ = checkSubtasks(nav, json.RawMessage(`{"subtasks":[{"ids":["stream/a"],"outcome":"cannot_verify","report":"unverifiable",
	  "use":"unknown","dispatch":"reasonable","work":"misled_by_subtask","note":"","cite":["s2"]}]}`))
	if len(c.frames) != 1 || c.frames[0].(*sessionjudge.Subtask).Work != sessionjudge.WorkNotReviewed {
		t.Fatalf("a sub-task's work called misled: %+v", c.frames)
	}
	targets := []sessionjudge.Target{{ID: "t1", Talks: []string{"talk/1", "talk/2"}}}
	label := func(talk, from string, subtask ...string) string {
		t.Helper()
		sub := "stream/a"
		if len(subtask) > 0 {
			sub = subtask[0]
		}
		c, err := checkEfficiency(nav, targets, map[string]bool{"stream/a": true, "stream/e": true, "stream/g": true}, false, json.RawMessage(`{"labels":[
		  {"target":"t1","talk":"`+talk+`","from":"`+from+`","label":"misled_by_subtask","subtask":"`+sub+`","note":"","cite":["s3a","s3b"]}]}`))
		if err != nil {
			t.Fatal(err)
		}
		if len(c.frames) == 0 {
			return "left out"
		}
		return "kept"
	}
	// a's report came back with the notice: work in talk 1 from a step after
	// it rests on the report, and the whole of talk 2, which came later.
	for _, tc := range [][3]string{{"talk/1", "", "left out"}, {"talk/1", "early", "left out"}, {"talk/1", "s1", "left out"},
		{"talk/1", "s3a", "left out"}, {"talk/1", "notice", "left out"}, {"talk/1", "s5", "kept"}, {"talk/2", "", "kept"}} {
		if got := label(tc[0], tc[1]); got != tc[2] {
			t.Errorf("%s from %q: %s", tc[0], tc[1], got)
		}
	}
	// g's report came back to talk 2, after talk 1's work landed.
	if got := label("talk/1", "s5", "stream/g"); got != "left out" {
		t.Errorf("a talk before the one g's report reached: %s", got)
	}
	// Work done while g ran, between its dispatch and its result, did not
	// rest on its report; work after the result did.
	if got := label("talk/2", "mid", "stream/g"); got != "left out" {
		t.Errorf("work before g's result came back: %s", got)
	}
	if got := label("talk/2", "after", "stream/g"); got != "kept" {
		t.Errorf("work after g's result came back: %s", got)
	}
	// A dispatch that kept only its request, as one a window cut before
	// its result: nothing shows where the result came back.
	for ti := range d.Talks {
		for i := range d.Talks[ti].Children {
			if c := &d.Talks[ti].Children[i]; c.ID == "s7" {
				c.Refs = c.Refs[:1]
			}
		}
	}
	nav = NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
	if got := label("talk/2", "after", "stream/g"); got != "left out" {
		t.Errorf("a dispatch with no result record: %s", got)
	}
	// Nothing shows e's report came back: no notice, and its start's
	// result is not its own.
	if got := label("talk/2", "", "stream/e"); got != "left out" {
		t.Errorf("a report nothing shows came back: %s", got)
	}
	// Talk 2 follows talk 1 in the document, by time, but its records
	// landed first: by recorded order its work came before a's report.
	for i := range d.Talks[3].Children {
		d.Talks[3].Children[i].Ref = &sessionflow.Ref{Seq: 0, Row: uint64(i + 1)}
	}
	nav = NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
	if got := label("talk/2", ""); got != "left out" {
		t.Errorf("a talk that landed before the report: %s", got)
	}
}

// A label given twice for the same part is taken once, the first, so a
// part is never priced twice.
func TestALabelGivenTwiceIsTakenOnce(t *testing.T) {
	nav := NewNav(doc(), Compute(doc(), "claude-code/1", nil, nil), nil)
	targets := []sessionjudge.Target{{ID: "t1", Talks: []string{"talk/1"}}}
	c, err := checkEfficiency(nav, targets, nil, false, json.RawMessage(`{"labels":[
	  {"target":"t1","talk":"talk/1","from":"s3a","label":"repeated","subtask":"","note":"","cite":["s3a"]},
	  {"target":"t1","talk":"talk/1","from":"s3a","label":"useful","subtask":"","note":"","cite":["s3a"]}]}`))
	if err != nil || len(c.frames) != 1 || c.frames[0].(*sessionjudge.Label).Label != sessionjudge.WorkRepeated {
		t.Fatalf("frames %+v %v", c.frames, err)
	}
}

// A redundant dispatch shown by a tool result makes the sub-task's work
// redundant_delegation whatever label came with it, and that cost is the
// avoidable cost of the agent that gave the task. Without the dispatch, the
// label is not taken.
func TestARedundantDispatchIsChargedToItsGiver(t *testing.T) {
	d := doc()
	nav := NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
	decide := func(dispatch, work string) *sessionjudge.Subtask {
		t.Helper()
		c, err := checkSubtasks(nav, json.RawMessage(`{"subtasks":[{"ids":["stream/a"],"outcome":"achieved","report":"accurate",
		  "use":"unknown","dispatch":"`+dispatch+`","work":"`+work+`","note":"","cite":["s2","s3a"]}]}`))
		if err != nil || len(c.frames) != 1 {
			t.Fatalf("%+v %v", c, err)
		}
		return c.frames[0].(*sessionjudge.Subtask)
	}
	a := decide("redundant", "useful")
	if a.Work != sessionjudge.WorkRedundant {
		t.Fatalf("a redundant dispatch's work %s", a.Work)
	}
	if b := decide("reasonable", "redundant_delegation"); b.Work != sessionjudge.WorkNotReviewed {
		t.Fatalf("redundant work on a reasonable dispatch %s", b.Work)
	}
	targets := []sessionjudge.Target{{ID: "t1", Status: sessionjudge.TargetClosed, Talks: []string{"talk/1"}}}
	frames, sum := closeFrames(nav.ledger, targets, nil, nil, []*sessionjudge.Label{{Target: "t1", Talk: "talk/1", Label: sessionjudge.WorkUseful}},
		map[string]*sessionjudge.Subtask{"stream/a": a}, nil)
	cause := ""
	for _, f := range frames {
		if c, ok := f.(*sessionjudge.Cost); ok && c.Scope == sessionjudge.ScopeWork && c.Work == sessionjudge.WorkRedundant {
			cause = c.Cause
		}
	}
	if cause != sessionjudge.CauseMain || sum.Avoidable.Input == 0 {
		t.Fatalf("cause %q, avoidable %+v", cause, sum.Avoidable)
	}
}

// A new target set under a key that is an existing target's id is left out:
// an input naming that key would reach the target found earlier, and what it
// directed, and its cost, would go to the wrong one.
func TestANewTargetCannotTakeAnExistingId(t *testing.T) {
	nav := navWithInputs()
	nav.Assign(Assignment{Inputs: []string{"in/talk/2"}})
	found := []sessionjudge.Target{{ID: "t1", Statement: "build it", Status: sessionjudge.TargetOpen, Talks: []string{"talk/1"},
		Criteria: []sessionjudge.Criterion{{ID: "t1.c1", Text: "it builds"}}, Cite: []sessionjudge.Cite{}}}
	c, err := checkTargets(nav, catalogue{targets: found}, json.RawMessage(`{"targets":[
	  {"id":"","key":"t1","statement":"ship it","criteria":["a release exists"],"drop_criteria":[],"status":"open","replaced_by":"","talks":["talk/2"],"cite":["in/talk/2"]}],
	 "inputs":[{"node":"in/talk/2","actions":[{"target":"t1","action":"create"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.targets) != 1 || c.targets[0].Statement != "build it" || !strings.Contains(c.note(), "key t1 is the id of a target found so far") {
		t.Fatalf("targets %+v, note %s", c.targets, c.note())
	}
}

// An input or a replacement naming a key left out for clashing is left out
// too, since the key may mean either target. Given to the old target, the
// calls after the input would be charged to a target it never asked for.
func TestAClashingKeyDirectsNothing(t *testing.T) {
	d := twoRequests()
	l := Compute(d, "claude-code/1", nil, nil)
	nav := NewNav(d, l, nil)
	nav.Assign(Assignment{Inputs: []string{"input/2"}})
	open := func(id string) sessionjudge.Target {
		return sessionjudge.Target{ID: id, Statement: id, Status: sessionjudge.TargetOpen, Talks: []string{"talk/1"},
			Criteria: []sessionjudge.Criterion{}, Cite: []sessionjudge.Cite{}}
	}
	found := []sessionjudge.Target{open("t1"), open("t2")}
	c, err := checkTargets(nav, catalogue{targets: found}, json.RawMessage(`{"targets":[
	  {"id":"","key":"t1","statement":"ship it","criteria":[],"drop_criteria":[],"status":"open","replaced_by":"","talks":["talk/1"],"cite":["input/2"]},
	  {"id":"t2","key":"","statement":"","criteria":[],"drop_criteria":[],"status":"","replaced_by":"t1","talks":[],"cite":[]}],
	 "inputs":[{"node":"input/2","actions":[{"target":"t1","action":"create"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.note(), `input input/2 acted on "t1", which names a target found so far and a new one alike`) ||
		!strings.Contains(c.note(), `target t2 was replaced by "t1", which names a target found so far and a new one alike`) {
		t.Fatalf("note %s", c.note())
	}
	if len(c.targets) != 2 || c.targets[1].ReplacedBy != "" {
		t.Fatalf("targets %+v", c.targets)
	}
	inputs := []*sessionjudge.Input{{Node: "input/1", Talk: "talk/1", Actions: []sessionjudge.Action{{Target: "t2", Action: sessionjudge.ActionCreate}}}}
	for _, f := range c.frames {
		if in, ok := f.(*sessionjudge.Input); ok {
			inputs = append(inputs, in)
		}
	}
	if len(inputs) != 2 || inputs[1].Actions[0].Action != sessionjudge.ActionUnknown {
		t.Fatalf("inputs %+v", inputs)
	}
	// input/1 directed every call of the talk to t2, and nothing the record
	// shows moved them.
	frames, _ := closeFrames(l, c.targets, inputs, nil, nil, nil, nil)
	cost := map[string]int{}
	for _, f := range frames {
		if o, ok := f.(*sessionjudge.Outcome); ok {
			cost[o.Target] = o.Cost.Usage.Input
		}
	}
	if cost["t1"] != 0 || cost["t2"] != 270 {
		t.Fatalf("costs %v, want t1 0 and t2 270", cost)
	}
}

// A talk counts as labeled only when every call of it the target was given
// has a label. In a talk two targets share, the calls before the input that
// asked for one served the other; and with a sub-task stage, a sub-task's
// own calls are that stage's to price.
func TestATalkIsLabeledOnlyWhenEveryCallIs(t *testing.T) {
	d := twoRequests()
	l := Compute(d, "claude-code/1", nil, nil)
	nav := NewNav(d, l, nil)
	targets := []sessionjudge.Target{{ID: "t1", Talks: []string{"talk/1"}}, {ID: "t2", Talks: []string{"talk/1"}}}
	raw := json.RawMessage(`{"labels":[
	  {"target":"t1","talk":"talk/1","from":"","label":"useful","note":"","cite":["q1"]},
	  {"target":"t2","talk":"talk/1","from":"w1","label":"useful","note":"","cite":["w1"]}]}`)
	// With no inputs found, every call of the talk is both targets', and
	// t2's part leaves q1 and q2 unlabeled.
	c, err := checkEfficiency(nav, targets, nil, false, raw)
	if err != nil || c.note() != "talk talk/1 of target t2 is labeled only from a later part, and its calls before that part are not reviewed (2)" ||
		*c.coverage.TalksLabeled != 1 {
		t.Fatalf("no inputs: %v %s %d", err, c.note(), *c.coverage.TalksLabeled)
	}
	nav.TargetsFound([]*sessionjudge.Input{
		{Node: "input/1", Talk: "talk/1", Actions: []sessionjudge.Action{{Target: "t1", Action: sessionjudge.ActionCreate}}},
		{Node: "input/2", Talk: "talk/1", Actions: []sessionjudge.Action{{Target: "t2", Action: sessionjudge.ActionCreate}}},
	})
	c, err = checkEfficiency(nav, targets, nil, false, raw)
	if err != nil || c.note() != "" || *c.coverage.Talks != 2 || *c.coverage.TalksLabeled != 2 {
		t.Fatalf("t2's own calls: %v %s %+v", err, c.note(), c.coverage)
	}

	// In talk/1 of doc, c1 asks for s1, which starts a, and s3a, which
	// starts c; a starts b. A part from s3a covers c1, which asked for it,
	// and c's calls, but not those of a and b.
	d = doc()
	t1 := &d.Talks[0]
	c1 := t1.Children[0]
	c1.Children = append(c1.Children, t1.Children[1:]...)
	t1.Children = []sessionview.Node{c1}
	nav = NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
	one := []sessionjudge.Target{{ID: "t1", Talks: []string{"talk/1"}}}
	raw = json.RawMessage(`{"labels":[{"target":"t1","talk":"talk/1","from":"s3a","label":"useful","note":"","cite":["s3a"]}]}`)
	c, err = checkEfficiency(nav, one, nil, false, raw)
	if err != nil || c.note() != "talk talk/1 of target t1 is labeled only from a later part, and its calls before that part are not reviewed (3)" {
		t.Fatalf("no sub-task stage: %v %s", err, c.note())
	}
	c, err = checkEfficiency(nav, one, nil, true, raw)
	if err != nil || c.note() != "" || *c.coverage.TalksLabeled != 1 {
		t.Fatalf("a sub-task stage: %v %s", err, c.note())
	}
}

// Each check of a targets answer, on its own: every case is a valid answer
// with one thing changed, and says what was left out and what stayed.
func TestEachTargetsCheckStandsAlone(t *testing.T) {
	found := func() []sessionjudge.Target {
		return []sessionjudge.Target{{ID: "t1", Statement: "build it", Status: sessionjudge.TargetOpen, Talks: []string{"talk/1"},
			Criteria: []sessionjudge.Criterion{{ID: "t1.c1", Text: "it builds"}}, Cite: []sessionjudge.Cite{}}}
	}
	entry := func(fields string) string {
		return `{"id":"","key":"","statement":"","criteria":[],"drop_criteria":[],"status":"","replaced_by":"","talks":[],"cite":[],` + fields + `}`
	}
	ship := entry(`"key":"n1","statement":"ship it","criteria":["released"],"status":"open","talks":["talk/2"],"cite":["in/talk/2"]`)
	create := `{"node":"in/talk/2","relations":[{"kind":"unclear"}],"actions":[{"target":"n1","action":"create"}]}`
	steer := `{"node":"in/talk/2","relations":[{"kind":"unclear"}],"actions":[{"target":"t1","action":"steer"}]}`
	for _, tc := range []struct {
		name, targets, inputs, note string
		check                       func(c *checked) string
	}{
		{"id and key", entry(`"id":"t1","key":"n2","statement":"changed"`), steer, "an entry names both target t1 and key n2, left out",
			func(c *checked) string {
				if c.targets[0].Statement != "build it" || len(c.targets) != 1 {
					return "the entry was taken"
				}
				return ""
			}},
		{"an id no earlier part found", entry(`"id":"n1","statement":"ship it","status":"open","talks":["talk/2"],"cite":["in/talk/2"]`), create, "",
			func(c *checked) string {
				if len(c.targets) != 2 || c.targets[0].Statement != "build it" || c.targets[1].ID != "t2" || c.targets[1].Statement != "ship it" ||
					inputOf(c, "in/talk/2").Actions[0].Target != "t2" {
					return fmt.Sprintf("targets %+v", c.targets)
				}
				return ""
			}},
		{"no id or key", entry(`"statement":"changed"`), steer, "an entry with no id and no key was left out",
			func(c *checked) string {
				if len(c.targets) != 1 || c.targets[0].Statement != "build it" {
					return "the entry was taken"
				}
				return ""
			}},
		{"an unknown id and a key", entry(`"id":"t9","key":"n2","statement":"changed"`), steer, "an entry names both target t9 and key n2, left out",
			func(c *checked) string {
				if len(c.targets) != 1 {
					return "the entry was taken for a new target"
				}
				return ""
			}},
		{"an entry left out before one with its key", entry(`"id":"t1","key":"n1","statement":"wrong"`) + "," + ship, create,
			"an entry names both target t1 and key n1, left out",
			func(c *checked) string {
				if len(c.targets) != 2 || c.targets[1].Statement != "ship it" || c.targets[0].Statement != "build it" {
					return fmt.Sprintf("targets %+v", c.targets)
				}
				return ""
			}},
		{"an unknown action and a known one", ship, `{"node":"in/talk/2","relations":[{"kind":"unclear"}],"actions":[{"target":"","action":"unknown"},{"target":"n1","action":"create"}]}`, "",
			func(c *checked) string {
				if *c.coverage.InputsRead != 1 {
					return fmt.Sprintf("inputs read %d", *c.coverage.InputsRead)
				}
				return ""
			}},
		{"drop a criterion it has not", entry(`"id":"t1","drop_criteria":["t1.c9"]`), steer, `target t1 has no criterion "t1.c9" to drop`,
			func(c *checked) string {
				if c.targets[0].Criteria[0].Dropped {
					return "a criterion was dropped"
				}
				return ""
			}},
		{"unknown status", entry(`"id":"t1","status":"done"`), steer, `target t1 had status "done", left as it was`,
			func(c *checked) string {
				if c.targets[0].Status != sessionjudge.TargetOpen {
					return "status " + c.targets[0].Status
				}
				return ""
			}},
		{"replaced by no target", entry(`"id":"t1","replaced_by":"t9"`), steer, `target t1 was replaced by "t9", which is no target`,
			func(c *checked) string {
				if c.targets[0].ReplacedBy != "" {
					return "replaced by " + c.targets[0].ReplacedBy
				}
				return ""
			}},
		{"replaced by a new target", entry(`"id":"t1","status":"replaced","replaced_by":"n1"`) + "," + ship, create, "",
			func(c *checked) string {
				if c.targets[0].ReplacedBy != "t2" || c.targets[0].Status != sessionjudge.TargetReplaced {
					return fmt.Sprintf("t1 %+v", c.targets[0])
				}
				return ""
			}},
		{"an input answered twice", ship, create + `,{"node":"in/talk/2","relations":[{"kind":"unclear"}],"actions":[{"target":"","action":"none"}]}`, "",
			func(c *checked) string {
				if in := inputOf(c, "in/talk/2"); in == nil || in.Actions[0].Action != sessionjudge.ActionCreate {
					return fmt.Sprintf("input %+v", in)
				}
				return ""
			}},
		{"unknown action", ship, `{"node":"in/talk/2","relations":[{"kind":"unclear"}],"actions":[{"target":"n1","action":"approve"}]}`, `input in/talk/2 had action "approve", left out`,
			func(c *checked) string {
				if in := inputOf(c, "in/talk/2"); in == nil || in.Actions[0].Action != sessionjudge.ActionUnknown {
					return fmt.Sprintf("input %+v", in)
				}
				return ""
			}},
		{"a citation twice", entry(`"key":"n1","statement":"ship it","status":"open","talks":["talk/2"],"cite":["in/talk/2","in/talk/2"]`), create, "",
			func(c *checked) string {
				if len(c.targets) != 2 || len(c.targets[1].Cite) != 1 {
					return fmt.Sprintf("targets %+v", c.targets)
				}
				return ""
			}},
	} {
		nav := navWithInputs()
		nav.Assign(Assignment{Inputs: []string{"in/talk/2"}})
		raw := `{"targets":[` + tc.targets + `],"inputs":[` + tc.inputs + `]}`
		c, err := checkTargets(nav, catalogue{targets: found()}, json.RawMessage(raw))
		switch {
		case err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case c.note() != tc.note:
			t.Errorf("%s: note %s", tc.name, c.note())
		default:
			if why := tc.check(c); why != "" {
				t.Errorf("%s: %s", tc.name, why)
			}
		}
	}
}

func inputOf(c *checked, node string) *sessionjudge.Input {
	for _, f := range c.frames {
		if in, ok := f.(*sessionjudge.Input); ok && in.Node == node {
			return in
		}
	}
	return nil
}

// Each check of an achievement answer, on its own.
func TestEachAchievementCheckStandsAlone(t *testing.T) {
	targets := []sessionjudge.Target{{ID: "t1", Talks: []string{"talk/1"}, Criteria: []sessionjudge.Criterion{{ID: "t1.c1", Text: "x"}}}}
	for _, tc := range []struct {
		name, answer, note string
		check              func(c *checked) string
	}{
		{"unknown basis", `{"target":"t1","basis":"a_guess","criteria":[{"id":"t1.c1","state":"met","cite":["s1"]}],"claims":[],"note":"","cite":["s1"]}`,
			`target t1 had basis "a_guess", left out`,
			func(c *checked) string {
				if len(c.frames) != 0 || !strings.Contains(c.note(), "1 targets were not judged") {
					return fmt.Sprintf("frames %d", len(c.frames))
				}
				return ""
			}},
		{"a criterion answered twice", `{"target":"t1","basis":"observed_result","criteria":[{"id":"t1.c1","state":"unknown","cite":[]},{"id":"t1.c1","state":"met","cite":["s1"]}],"claims":[],"note":"","cite":["s1"]}`,
			"", func(c *checked) string {
				if a := c.frames[0].(*sessionjudge.Achievement); a.Criteria[0].State != sessionjudge.CheckUnknown || len(a.Criteria) != 1 {
					return fmt.Sprintf("criteria %+v", a.Criteria)
				}
				return ""
			}},
		{"a target answered twice", `{"target":"t1","basis":"observed_result","criteria":[{"id":"t1.c1","state":"unknown","cite":[]}],"claims":[],"note":"","cite":["s1"]},` +
			`{"target":"t1","basis":"observed_result","criteria":[{"id":"t1.c1","state":"met","cite":["s1"]}],"claims":[],"note":"","cite":["s1"]}`,
			`an answer for "t1", which is no target or was answered already, was left out`,
			func(c *checked) string {
				if len(c.frames) != 1 || *c.coverage.TargetsJudged != 1 || c.frames[0].(*sessionjudge.Achievement).Criteria[0].State != sessionjudge.CheckUnknown {
					return fmt.Sprintf("%d frames, %d judged", len(c.frames), *c.coverage.TargetsJudged)
				}
				return ""
			}},
		{"a claim with no evidence", `{"target":"t1","basis":"observed_result","criteria":[{"id":"t1.c1","state":"met","cite":["s1"]}],"claims":[{"text":"x","state":"no_evidence","cite":[]}],"events":[{"key":"e1","kind":"delivered","artifact":"file","criteria":[],"text":"written","cite":["s1"]}],"note":"","cite":["s1"]}`,
			"", func(c *checked) string {
				if a := c.frames[0].(*sessionjudge.Achievement); a.Claims[0].State != sessionjudge.CheckNoEvidence {
					return "claim " + a.Claims[0].State
				}
				return ""
			}},
		{"a contradicted claim on a target not achieved", `{"target":"t1","basis":"observed_result","criteria":[{"id":"t1.c1","state":"not_met","cite":["s1"]}],"claims":[{"text":"x","state":"contradicted","cite":["s3a"]}],"note":"","cite":["s1"]}`,
			"", func(c *checked) string {
				if a := c.frames[0].(*sessionjudge.Achievement); a.Level != sessionjudge.LevelNotAchieved {
					return "level " + a.Level
				}
				return ""
			}},
		{"a citation twice", `{"target":"t1","basis":"observed_result","criteria":[{"id":"t1.c1","state":"met","cite":["s1"]}],"claims":[],"events":[{"key":"e1","kind":"delivered","artifact":"file","criteria":[],"text":"written","cite":["s1"]}],"note":"","cite":["s1","s1"]}`,
			"", func(c *checked) string {
				if a := c.frames[0].(*sessionjudge.Achievement); len(a.Cite) != 1 {
					return fmt.Sprintf("cites %+v", a.Cite)
				}
				return ""
			}},
		{"a citation of nothing", `{"target":"t1","basis":"observed_result","criteria":[{"id":"t1.c1","state":"met","cite":["s1","made/up"]}],"claims":[],"note":"","cite":["s1"]}`,
			"1 citations of nodes that do not exist were left out",
			func(c *checked) string {
				if a := c.frames[0].(*sessionjudge.Achievement); a.Level != sessionjudge.LevelAchieved {
					return "level " + a.Level
				}
				return ""
			}},
	} {
		c, err := checkAchievement(navWithInputs(), targets, json.RawMessage(`{"achievements":[`+tc.answer+`]}`))
		switch {
		case err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.note == "" && c.note() != "", tc.note != "" && !strings.Contains(c.note(), tc.note):
			t.Errorf("%s: note %s", tc.name, c.note())
		default:
			if why := tc.check(c); why != "" {
				t.Errorf("%s: %s", tc.name, why)
			}
		}
	}
	// An answer that judges nothing says how many targets it left.
	c, err := checkAchievement(navWithInputs(), targets, json.RawMessage(`{"achievements":[]}`))
	if err != nil || c.note() != "1 targets were not judged" {
		t.Errorf("no answer: %v %s", err, c.note())
	}
}

// Each check of an efficiency answer, on its own: each case cites a step of
// the talk it labels, so only the check named refuses it.
func TestEachLabelCheckStandsAlone(t *testing.T) {
	targets := []sessionjudge.Target{{ID: "t1", Talks: []string{"talk/1"}}, {ID: "t2", Talks: []string{"talk/2"}}}
	whole := `{"target":"t1","talk":"talk/1","from":"","label":"useful","note":"","cite":["c1"]},{"target":"t2","talk":"talk/2","from":"","label":"useful","note":"","cite":["c2"]}`
	for _, tc := range []struct{ name, extra, note string }{
		{"a talk of another target", `{"target":"t1","talk":"talk/2","from":"","label":"repeated","note":"","cite":["s3b"]}`,
			`a label for talk "talk/2" of target "t1", which is not one of its talks, was left out`},
		{"unknown label", `{"target":"t1","talk":"talk/1","from":"s1","label":"not_reviewed","note":"","cite":["s1"]}`,
			`talk talk/1 had label "not_reviewed", left out`},
		{"a citation of nothing", `{"target":"t1","talk":"talk/1","from":"s1","label":"useful","note":"","cite":["s1","made/up"]}`,
			"1 citations of nodes that do not exist were left out"},
	} {
		c, err := checkEfficiency(navWithInputs(), targets, nil, false, json.RawMessage(`{"labels":[`+whole+`,`+tc.extra+`]}`))
		if err != nil || c.note() != tc.note {
			t.Errorf("%s: %v %s", tc.name, err, c.note())
			continue
		}
		var kept []string
		for _, f := range c.frames {
			lb := f.(*sessionjudge.Label)
			kept = append(kept, lb.Target+"/"+lb.Talk+"/"+lb.Label)
		}
		want := "t1/talk/1/useful,t2/talk/2/useful"
		if tc.name == "a citation of nothing" {
			want += ",t1/talk/1/useful"
		}
		if strings.Join(kept, ",") != want {
			t.Errorf("%s: kept %v", tc.name, kept)
		}
	}
}

// Each check of a sub-task answer, on its own: a value outside its field's
// words in any one field, a decision on a sub-task the call was not given,
// and a citation of nothing.
func TestEachSubtaskCheckStandsAlone(t *testing.T) {
	valid := map[string]string{"outcome": "cannot_verify", "report": "unverifiable", "use": "unknown", "dispatch": "reasonable", "work": "not_reviewed"}
	decision := func(id, field, value string, cite string) string {
		f := map[string]any{"ids": []string{id}, "note": "", "cite": []string{}}
		for k, v := range valid {
			f[k] = v
		}
		if field != "" {
			f[field] = value
		}
		if cite != "" {
			f["cite"] = []string{cite}
		}
		b, _ := json.Marshal(f)
		return string(b)
	}
	run := func(answer string) *checked {
		nav := navWithInputs()
		nav.Assign(Assignment{Subtasks: []string{"stream/a"}})
		c, err := checkSubtasks(nav, json.RawMessage(`{"subtasks":[`+answer+`]}`))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if c := run(decision("stream/a", "", "", "")); c.note() != "" || len(c.frames) != 1 {
		t.Fatalf("the valid decision: %s", c.note())
	}
	for _, field := range []string{"outcome", "report", "use", "dispatch", "work"} {
		c := run(decision("stream/a", field, "something_else", ""))
		if len(c.frames) != 0 || !strings.HasPrefix(c.note(), "a decision on [stream/a] with ") ||
			!strings.Contains(c.note(), field+` "something_else"`) || !strings.Contains(c.note(), " was left out; 1 of 1 sub-tasks were not judged") {
			t.Errorf("%s outside its words: %d frames, %s", field, len(c.frames), c.note())
		}
	}
	c := run(decision("stream/a", "", "", "") + "," + decision("stream/b", "", "", ""))
	if len(c.frames) != 1 || c.frames[0].(*sessionjudge.Subtask).ID != "stream/a" || c.note() != "1 decisions on sub-tasks this call does not answer for were left out" {
		t.Errorf("a sub-task the call was not given: %d frames, %s", len(c.frames), c.note())
	}
	// Avoidable work the sub-task's own steps show is kept as given.
	repeated := strings.Replace(strings.Replace(decision("stream/a", "work", "repeated", "s2"), `"cannot_verify"`, `"achieved"`, 1), `"unverifiable"`, `"accurate"`, 1)
	if c := run(repeated); len(c.frames) != 1 || c.frames[0].(*sessionjudge.Subtask).Work != sessionjudge.WorkRepeated || c.note() != "" {
		t.Errorf("avoidable work shown: %s", c.note())
	}
	c = run(decision("stream/a", "", "", "made/up"))
	if len(c.frames) != 1 || c.note() != "1 citations of nodes that do not exist were left out" {
		t.Errorf("a citation of nothing: %d frames, %s", len(c.frames), c.note())
	}
}

// What shows the work is read from citations already checked; a node the
// document does not hold shows nothing, and never stops the check.
func TestACitationOfNothingShowsNothing(t *testing.T) {
	if shows(navWithInputs(), []sessionjudge.Cite{{Node: "made/up"}, {Node: "c1"}}, true, nil) {
		t.Fatal("a citation of nothing showed the work")
	}
}

// An answer is read whole or refused: one that is not a JSON object, one
// without its stage's key, and one whose key holds the wrong type each stop
// the check, in every stage, rather than read as nothing found.
func TestEveryAnswerIsDecodedWhole(t *testing.T) {
	for _, stage := range []struct {
		key   string
		check func(json.RawMessage) error
	}{
		{"targets", func(r json.RawMessage) error { _, err := checkTargets(navWithInputs(), catalogue{}, r); return err }},
		{"achievements", func(r json.RawMessage) error { _, err := checkAchievement(navWithInputs(), nil, r); return err }},
		{"labels", func(r json.RawMessage) error {
			_, err := checkEfficiency(navWithInputs(), nil, nil, false, r)
			return err
		}},
		{"subtasks", func(r json.RawMessage) error { _, err := checkSubtasks(navWithInputs(), r); return err }},
	} {
		for raw, want := range map[string]string{
			`nope`:                        "the answer is not a JSON object",
			`{"other":[]}`:                `the answer has no "` + stage.key + `"`,
			`{"` + stage.key + `":"one"}`: "the answer does not match the schema",
		} {
			if err := stage.check(json.RawMessage(raw)); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s, %s: %v", stage.key, raw, err)
			}
		}
	}
}

// recorded is a conversation where every step has a record, so order can be
// read from where the records landed. Talk m1 starts child a at "start",
// whose result came back in a later record of the same call, after the
// runtime noticed a finish at "note". Talk m2 starts child e at "start2",
// and talk m3 starts child f. Loose steps, in no talk, start children b and
// c, and child d has a notice but no step that started it.
func recorded() *sessionview.Conversation {
	ref := func(row uint64) *sessionflow.Ref { return &sessionflow.Ref{Seq: 1, Row: row} }
	step := func(id, kind, stream string, row uint64) sessionview.Node {
		return sessionview.Node{ID: id, Kind: kind, Stream: stream, Ref: ref(row), Result: "ok"}
	}
	quiet := func(id, stream string, row uint64) sessionview.Node {
		return sessionview.Node{ID: id, Kind: model.KindAgentCall, Stream: stream, Ref: ref(row)}
	}
	refs := func(rows ...uint64) []sessionflow.Ref {
		var out []sessionflow.Ref
		for _, r := range rows {
			out = append(out, sessionflow.Ref{Seq: 1, Row: r})
		}
		return out
	}
	m1 := sessionview.Node{ID: "talk/m1", Kind: "talk", Stream: "main", Children: []sessionview.Node{
		{ID: "in1", Kind: model.KindMessageExternal, Ref: ref(1), Text: "do it"},
		{ID: "start", Kind: model.KindTool, Refs: refs(3, 9), Result: "done"},
		{ID: "note", Kind: model.KindMessageSynthetic, Ref: ref(7)},
		step("mid", model.KindTool, "main", 8),
		step("after", model.KindTool, "main", 10),
		{ID: "note2", Kind: model.KindMessageSynthetic, Ref: ref(12)},
	}}
	ta := sessionview.Node{ID: "talk/a", Kind: "talk", Stream: "a", Children: []sessionview.Node{step("atool", model.KindTool, "a", 4)}}
	m2 := sessionview.Node{ID: "talk/m2", Kind: "talk", Stream: "main", Children: []sessionview.Node{
		{ID: "in2", Kind: model.KindMessageExternal, Ref: ref(25), Text: "go on"},
		{ID: "start2", Kind: model.KindTool, Refs: refs(26, 28), Result: "done"},
		{ID: "unrecorded", Kind: model.KindMessageSynthetic},
		step("late", model.KindTool, "main", 30),
	}}
	// Talk m3 starts child f at a step with no record, so the talk's first
	// record is a step listed before f's notice but recorded after it.
	m3 := sessionview.Node{ID: "talk/m3", Kind: "talk", Stream: "main", Children: []sessionview.Node{
		{ID: "start3", Kind: model.KindTool, Result: "done"},
		step("busy", model.KindTool, "main", 41),
		{ID: "note3", Kind: model.KindMessageSynthetic, Ref: ref(40)},
	}}
	return &sessionview.Conversation{
		Conversation: "conv",
		Streams: []sessionview.Stream{{ID: "stream/main", Name: "main"}, {ID: "stream/a", Name: "a"}, {ID: "stream/b", Name: "b"},
			{ID: "stream/c", Name: "c"}, {ID: "stream/d", Name: "d"}, {ID: "stream/e", Name: "e"}, {ID: "stream/f", Name: "f"}},
		Talks: []sessionview.Node{m1, ta, m2, m3},
		Loose: []sessionview.Node{step("l1", model.KindTool, "a", 20), quiet("ls1", "main", 21), quiet("ls2", "main", 22)},
		Relations: []sessionview.Relation{
			{ID: "r1", Type: model.RelStarts, From: "start", To: "stream/a"},
			{ID: "r2", Type: model.RelReports, From: "note", To: "stream/a"},
			{ID: "r3", Type: model.RelResultOf, From: "start", To: "start"},
			{ID: "r4", Type: model.RelStarts, From: "ls1", To: "stream/b"},
			{ID: "r5", Type: model.RelStarts, From: "ls2", To: "stream/c"},
			{ID: "r6", Type: model.RelReports, From: "note2", To: "stream/d"},
			{ID: "r7", Type: model.RelStarts, From: "start2", To: "stream/e"},
			// Neither of these says start2's result came back: one is of
			// another type, the other a result of another step.
			{ID: "r8", Type: model.RelFollows, From: "late", To: "start2"},
			{ID: "r9", Type: model.RelResultOf, From: "late", To: "in2"},
			// A notice with no record places no report.
			{ID: "r10", Type: model.RelReports, From: "unrecorded", To: "stream/e"},
			{ID: "r11", Type: model.RelStarts, From: "start3", To: "stream/f"},
			{ID: "r12", Type: model.RelReports, From: "note3", To: "stream/f"},
		},
	}
}

// Order is read from places within one talk and from records everywhere
// else, never from places in two talks or of two loose streams.
func TestOrderComesFromTheRecords(t *testing.T) {
	d := recorded()
	nav := NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"after", "start", true},
		// in2 is first in its talk and after is fifth in its own, but
		// in2's record landed later.
		{"in2", "after", true},
		{"after", "in2", false},
		// l1 and ls1 are each first among their stream's loose steps.
		{"ls1", "l1", true},
		{"l1", "ls1", false},
	} {
		if got := recordedAfter(nav, tc.a, tc.b); got != tc.want {
			t.Errorf("%s after %s: %v", tc.a, tc.b, got)
		}
	}
}

// What the agent did next is a step of its own stream recorded after the
// start. A start in no talk has no asking stream, and a cited step in no
// talk or not in the document is no such step.
func TestWhatTheAgentDidNextIsInItsOwnStream(t *testing.T) {
	d := recorded()
	nav := NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
	cite := func(ids ...string) []sessionjudge.Cite {
		var out []sessionjudge.Cite
		for _, id := range ids {
			out = append(out, sessionjudge.Cite{Node: id})
		}
		return out
	}
	if !actedAfter(nav, cite("after"), "start") {
		t.Fatal("a later step of the asking talk was not taken")
	}
	for name, tc := range map[string]struct {
		cites []sessionjudge.Cite
		start string
	}{
		"a start in no talk":       {cite("late"), "ls1"},
		"a cited step in no talk":  {cite("l1"), "start"},
		"a cited step not present": {cite("made/up"), "start"},
	} {
		if actedAfter(nav, tc.cites, tc.start) {
			t.Errorf("%s was taken", name)
		}
	}
	// Two loose steps that each started a sub-task share no stream: one does
	// not show the other's task given wrong.
	if !startsSubtask(nav, "ls1") || !startsSubtask(nav, "ls2") {
		t.Fatal("the loose steps started no sub-task")
	}
	if shownWrong(nav, cite("ls1", "ls2"), "ls1") {
		t.Error("a loose step was taken for the same agent giving a task again")
	}
	if citesStep(nav, cite("made/up"), nil) || citesKind(nav, cite("made/up"), model.KindTool) {
		t.Error("a node not in the document was taken for a step")
	}
}

// A report comes back with the earliest notice that the sub-task ended, or
// else with the starting call's own result. A relation of another type, or
// a result of another step, is no report; a sub-task no step started, or
// one the ledger does not hold, has no report to come back.
func TestAReportComesBackOnlyAsTheRecordSays(t *testing.T) {
	d := recorded()
	nav := NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
	for _, tc := range []struct {
		name, talk, from, subtask string
		want                      bool
	}{
		// The notice at 7 came before the call's result at 9: a part from
		// mid, at 8, begins after the report.
		{"after the notice, before the result", "talk/m1", "mid", "stream/a", true},
		{"before the notice", "talk/m1", "start", "stream/a", false},
		{"no result of the starting call, and a notice with no record", "talk/m2", "late", "stream/e", false},
		// The whole talk the report came back to began before it, whatever
		// record of it is first.
		{"the whole talk the report came back to", "talk/m3", "", "stream/f", false},
		{"a step of that talk after the report", "talk/m3", "busy", "stream/f", true},
		{"no step started it", "talk/m2", "late", "stream/d", false},
		{"no such sub-task", "talk/m2", "late", "stream/zz", false},
	} {
		if got := afterReport(nav, tc.talk, tc.from, tc.subtask); got != tc.want {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
}

// Each kind of evidence on its own: a tool that returned only bytes, a tool
// that failed with no result, and the person's words only where people may
// be cited.
func TestEachKindOfEvidenceStandsAlone(t *testing.T) {
	d := doc()
	failed := true
	d.Talks[0].Children = append(d.Talks[0].Children,
		sessionview.Node{ID: "sized", Kind: model.KindTool, ResultBytes: 10},
		sessionview.Node{ID: "failed", Kind: model.KindTool, Failed: &failed},
		sessionview.Node{ID: "pending", Kind: model.KindTool},
		sessionview.Node{ID: "word", Kind: model.KindMessageExternal, Text: "looks good"})
	nav := NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
	one := func(id string) []sessionjudge.Cite { return []sessionjudge.Cite{{Node: id}} }
	for _, tc := range []struct {
		id     string
		people bool
		want   bool
	}{
		{"sized", false, true},
		{"failed", false, true},
		{"pending", false, false},
		{"word", true, true},
		{"word", false, false},
	} {
		if got := shows(nav, one(tc.id), tc.people, nil); got != tc.want {
			t.Errorf("%s, people %v: %v", tc.id, tc.people, got)
		}
	}
}
