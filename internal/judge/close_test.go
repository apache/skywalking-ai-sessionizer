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
	"slices"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
)

// An input the ledger cannot place cuts no talk: the calls stay split
// between the talk's targets rather than all going to the one it named.
func TestAnInputWithNoPlaceCutsNothing(t *testing.T) {
	l := Compute(twoRequests(), "claude-code/1", nil, nil)
	claims := map[string][]string{"talk/1": {"t1", "t2"}}
	who := ownersOfCalls(l, claims, []*sessionjudge.Input{
		{Node: "nowhere", Talk: "talk/1", Actions: []sessionjudge.Action{{Target: "t2", Action: sessionjudge.ActionCreate}}}})
	for _, i := range l.CallsOf("talk/1") {
		if !slices.Equal(who[i], []string{"t1", "t2"}) {
			t.Fatalf("call %s went to %v", l.Calls[i].ID, who[i])
		}
	}
}

// A part that starts at a step the ledger cannot place is no part.
func TestAPartWithNoPlaceIsNoPart(t *testing.T) {
	l := Compute(twoRequests(), "claude-code/1", nil, nil)
	whole, ps := talkParts(l, []*sessionjudge.Label{{Target: "t1", Talk: "talk/1", From: "nowhere", Label: sessionjudge.WorkRepeated}})
	if whole.work != "" || len(ps) != 0 {
		t.Fatalf("whole %+v, parts %+v", whole, ps)
	}
}

// In a talk two targets share, each target's label covers only the calls
// that served it: t1's useful label does not reach the calls after the
// input that asked for t2.
func TestALabelCoversOnlyItsTargetsCalls(t *testing.T) {
	l := Compute(twoRequests(), "claude-code/1", nil, nil)
	targets := []sessionjudge.Target{
		{ID: "t1", Status: sessionjudge.TargetClosed, Talks: []string{"talk/1"}},
		{ID: "t2", Status: sessionjudge.TargetClosed, Talks: []string{"talk/1"}},
	}
	inputs := []*sessionjudge.Input{
		{Node: "input/1", Talk: "talk/1", Actions: []sessionjudge.Action{{Target: "t1", Action: sessionjudge.ActionCreate}}},
		{Node: "input/2", Talk: "talk/1", Actions: []sessionjudge.Action{{Target: "t2", Action: sessionjudge.ActionCreate}}},
	}
	labels := []*sessionjudge.Label{
		{Target: "t1", Talk: "talk/1", Label: sessionjudge.WorkUseful},
		{Target: "t2", Talk: "talk/1", Label: sessionjudge.WorkRepeated},
	}
	frames, _ := closeFrames(l, targets, inputs, nil, labels, nil, nil)
	work := map[string]int{}
	for _, f := range frames {
		if c, ok := f.(*sessionjudge.Cost); ok && c.Scope == sessionjudge.ScopeWork {
			work[c.Target+" "+c.Work] += c.Usage.Input
		}
	}
	if len(work) != 2 || work["t1 useful"] != 20 || work["t2 repeated"] != 250 {
		t.Fatalf("work %v, want t1 useful 20 and t2 repeated 250", work)
	}
}

// Only an input that sends work cuts a shared talk, and one that names a
// target twice gives it the calls after it whole, never split with itself.
func TestOnlyAnInputThatSendsWorkCutsATalk(t *testing.T) {
	l := Compute(twoRequests(), "claude-code/1", nil, nil)
	claims := map[string][]string{"talk/1": {"t1", "t2"}}
	act := func(target, action string) sessionjudge.Action {
		return sessionjudge.Action{Target: target, Action: action}
	}
	first := &sessionjudge.Input{Node: "input/1", Talk: "talk/1", Actions: []sessionjudge.Action{act("t1", sessionjudge.ActionCreate)}}
	for name, tc := range map[string]struct {
		second []sessionjudge.Action
		want   []string
	}{
		"accepting sends no work": {[]sessionjudge.Action{act("t2", sessionjudge.ActionAccept)}, []string{"t1"}},
		"a target named twice":    {[]sessionjudge.Action{act("t2", sessionjudge.ActionCreate), act("t2", sessionjudge.ActionRefine)}, []string{"t2"}},
	} {
		who := ownersOfCalls(l, claims, []*sessionjudge.Input{first, {Node: "input/2", Talk: "talk/1", Actions: tc.second}})
		for _, i := range l.CallsOf("talk/1") {
			if id := l.Calls[i].ID; (id == "w1" || id == "w2") && !slices.Equal(who[i], tc.want) {
				t.Errorf("%s: %s went to %v, want %v", name, id, who[i], tc.want)
			}
		}
	}
}

// With a sub-task stage and no efficiency stage, the sub-tasks' work is
// still labeled and its avoidable part counted.
func TestSubtaskWorkCountsWithoutLabels(t *testing.T) {
	l := Compute(doc(), "claude-code/1", nil, nil)
	targets := []sessionjudge.Target{{ID: "t1", Status: sessionjudge.TargetClosed, Talks: []string{"talk/1"}}}
	subtasks := map[string]*sessionjudge.Subtask{"stream/a": {ID: "stream/a", Dispatch: sessionjudge.DispatchReasonable, Work: sessionjudge.WorkRepeated}}
	frames, sum := closeFrames(l, targets, nil, nil, nil, subtasks, nil)
	repeated := 0
	for _, f := range frames {
		if c, ok := f.(*sessionjudge.Cost); ok && c.Scope == sessionjudge.ScopeWork && c.Work == sessionjudge.WorkRepeated && c.Cause == "stream/a" {
			repeated += c.Calls
		}
	}
	if repeated == 0 || sum.Avoidable == nil || sum.Avoidable.Input == 0 {
		t.Fatalf("repeated calls %d, avoidable %+v", repeated, sum.Avoidable)
	}
}

// The close file prices the sub-tasks the main talks gave; one no step can
// be shown to have started, as child d of doc, is no main talk's.
func TestASubtaskNoMainTalkGaveIsNotPriced(t *testing.T) {
	frames, _ := closeFrames(Compute(doc(), "claude-code/1", nil, nil), nil, nil, nil, nil, nil, nil)
	var priced []string
	for _, f := range frames {
		if c, ok := f.(*sessionjudge.Cost); ok && c.Scope == sessionjudge.ScopeSubtask {
			priced = append(priced, c.ID)
		}
	}
	if !slices.Contains(priced, "stream/a") || slices.Contains(priced, "stream/d") {
		t.Fatalf("priced %v", priced)
	}
}

// Work done on the report of a sub-task the ledger does not hold is the main
// agent's doing: no sub-task above it can be named.
func TestWorkOnTheReportOfAnUnknownSubtaskIsTheMainAgents(t *testing.T) {
	l := Compute(doc(), "claude-code/1", nil, nil)
	targets := []sessionjudge.Target{{ID: "t1", Status: sessionjudge.TargetClosed, Talks: []string{"talk/1"}}}
	labels := []*sessionjudge.Label{{Target: "t1", Talk: "talk/1", Label: sessionjudge.WorkMisled, Subtask: "stream/zz"}}
	subtasks := map[string]*sessionjudge.Subtask{"stream/zz": {ID: "stream/zz", Dispatch: sessionjudge.DispatchMisdirected, Work: sessionjudge.WorkUseful}}
	frames, _ := closeFrames(l, targets, nil, nil, labels, subtasks, nil)
	for _, f := range frames {
		if c, ok := f.(*sessionjudge.Cost); ok && c.Work == sessionjudge.WorkMisled && c.Cause != sessionjudge.CauseMain {
			t.Fatalf("misled work charged to %q", c.Cause)
		}
	}
}

// A partly achieved target missing its criteria counts or its met counts
// scores one half; and a cost with calls not priced, with usage unknown, or
// with no money at all, makes tokens the score's unit.
func TestAScoreReadsOnlyWhatAnOutcomeHolds(t *testing.T) {
	two, one := 2, 1
	for name, o := range map[string]*sessionjudge.Outcome{
		"no met count":      {Level: sessionjudge.LevelPartly, Criteria: &two},
		"no criteria count": {Level: sessionjudge.LevelPartly, Met: &one},
	} {
		var s scoreSum
		s.add(o, exactAmounts{})
		if s.value != 0.5 {
			t.Errorf("%s: %v", name, s.value)
		}
	}
	money := 1.0
	priced := sessionjudge.Amount{Usage: sessiondata.Usage{Input: 5}, Money: &money}
	for name, tc := range map[string]struct {
		cost sessionjudge.Amount
		unit string
	}{
		"every call priced": {priced, sessionjudge.UnitMoney},
		"a call not priced": {sessionjudge.Amount{Usage: priced.Usage, Money: &money, Unpriced: 1}, sessionjudge.UnitTokens},
		"a usage unknown":   {sessionjudge.Amount{Usage: priced.Usage, Money: &money, Unknown: 1}, sessionjudge.UnitTokens},
		"no money":          {sessionjudge.Amount{Usage: priced.Usage}, sessionjudge.UnitTokens},
	} {
		var s scoreSum
		s.add(&sessionjudge.Outcome{Level: sessionjudge.LevelAchieved, Cost: tc.cost}, exactAmounts{cost: [2]float64{1, 5}})
		if got := s.score(false).Unit; got != tc.unit {
			t.Errorf("%s: unit %s, want %s", name, got, tc.unit)
		}
	}
}

// The time of several spans counts what overlaps once, and leaves out a
// span with no start or one that ends before it starts. Any range is a
// span, one before the epoch too.
func TestSpansAreJoinedOnce(t *testing.T) {
	for _, tc := range []struct {
		spans [][2]int64
		want  int64
	}{
		{[][2]int64{{1, 5}, {3, 8}, {10, 12}}, 9},
		{[][2]int64{{0, 5}}, 0},
		{[][2]int64{{10, 5}}, 0},
		{[][2]int64{{-10, -5}}, 5},
	} {
		if got := union(tc.spans); got != tc.want {
			t.Errorf("%v: %d, want %d", tc.spans, got, tc.want)
		}
	}
}
