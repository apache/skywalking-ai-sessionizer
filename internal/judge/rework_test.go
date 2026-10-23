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
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
)

func round(attempt []string, trigger string, by []string, cause string, redo ...string) reworkEntry {
	return reworkEntry{Attempt: attempt, Trigger: trigger, TriggerCite: by, Cause: cause, Redo: redo, Finished: true, Note: "redone"}
}

// reworked checks an achievement answer with these rounds for a target of
// some talks of deliveries, judged over the talks of window, or the whole
// conversation when window is empty.
func reworked(t *testing.T, talks, window []string, rounds ...reworkEntry) ([]sessionjudge.Rework, string) {
	t.Helper()
	nav := deliveries()
	if len(window) > 0 {
		nav.Within(&Selection{Talks: window})
	}
	target := deliveryTarget()
	target.Talks = talks
	b, _ := json.Marshal(rounds)
	raw := `{"achievements":[{"target":"t1","basis":"observed_result","criteria":[{"id":"t1.c1","state":"met","cite":["s3"]},{"id":"t1.c2","state":"met","cite":["s3"]}],` +
		`"claims":[],"events":[{"key":"e1","kind":"delivered","artifact":"revision","criteria":[],"text":"committed","cite":["s3"]}],"contradictions":[],"rework":` + string(b) + `,"note":"","cite":["s3"]}]}`
	c, err := checkAchievement(nav, []sessionjudge.Target{target}, json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	return c.frames[0].(*sessionjudge.Achievement).Rework, c.note()
}

func bounds(rs []sessionjudge.Rework) string {
	var out []string
	for _, r := range rs {
		out = append(out, fmt.Sprintf("%s:%s:%s..%s", r.ID, r.Cause, r.From, r.To))
	}
	return strings.Join(out, " ")
}

var both = []string{"talk/1", "talk/2"}

// A round keeps its attempt, its trigger and its redo, and is bounded by the
// step that triggered it and the last step of the redo.
func TestARoundOfReworkKeepsWhatBoundsIt(t *testing.T) {
	rs, note := reworked(t, both, nil,
		round([]string{"s1"}, "person", []string{"in/2"}, "agent_error", "s2", "s3"),
		round([]string{"s3"}, "verification", []string{"sa"}, "unknown", "p1"))
	if note != "" {
		t.Fatalf("note %s", note)
	}
	if bounds(rs) != "t1.r1:agent_error:in/2..s3 t1.r2:unknown:sa..p1" {
		t.Fatalf("rounds %s", bounds(rs))
	}
	if r := rs[0]; r.Trigger != "person" || r.Attempt[0].Node != "s1" || r.TriggerCite[0].Node != "in/2" || len(r.Redo) != 2 || !r.Finished || r.Note != "redone" {
		t.Fatalf("round %+v", r)
	}
}

// Each rule of a round on its own: a round without an attempt, a trigger and
// a redo, each shown by what it cites and in recorded order, is left out.
func TestEachReworkRuleStandsAlone(t *testing.T) {
	for _, tc := range []struct {
		name          string
		talks, window []string
		round         reworkEntry
		want          string
	}{
		{"a tool's trigger", both, nil, round([]string{"s1"}, "tool", []string{"s2"}, "agent_error", "s3"), "t1.r1:agent_error:s2..s3"},
		{"a trigger at the attempt's own step", both, nil, round([]string{"s2"}, "tool", []string{"s2"}, "agent_error", "s3"), "t1.r1:agent_error:s2..s3"},
		{"no trigger, no attempt", both, nil, round(nil, "tool", nil, "agent_error", "s3"), ""},
		{"a redo by a sub-agent of the target", both, nil, round([]string{"s1"}, "person", []string{"in/2"}, "agent_error", "sa"), "t1.r1:agent_error:in/2..sa"},
		{"no attempt", both, nil, round(nil, "person", []string{"in/2"}, "agent_error", "s3"), ""},
		{"an attempt by the person", both, nil, round([]string{"in/1"}, "person", []string{"in/2"}, "agent_error", "s3"), ""},
		{"an attempt of other work", []string{"talk/2"}, nil, round([]string{"s1"}, "person", []string{"in/2"}, "agent_error", "s3"), ""},
		{"an attempt before the window", []string{"talk/2"}, []string{"talk/2", "talk/3"}, round([]string{"s1"}, "person", []string{"in/2"}, "agent_error", "s3"),
			"t1.r1:agent_error:in/2..s3"},
		{"an attempt in no talk, in a window", []string{"talk/2"}, []string{"talk/2", "talk/3"}, round([]string{"l1"}, "person", []string{"in/2"}, "agent_error", "s3"), ""},
		{"a person's trigger that is a reply", both, nil, round([]string{"s1"}, "person", []string{"r2"}, "agent_error", "s3"), ""},
		{"a tool's trigger that never returned", []string{"talk/1", "talk/2", "talk/3"}, nil, round([]string{"r1"}, "tool", []string{"p1"}, "agent_error", "r3"), ""},
		{"a tool's trigger that is an input", both, nil, round([]string{"s1"}, "tool", []string{"in/2"}, "agent_error", "s3"), ""},
		{"an unknown trigger", both, nil, round([]string{"s1"}, "review", []string{"in/2"}, "agent_error", "s3"), ""},
		{"a redo of other work", both, nil, round([]string{"s1"}, "person", []string{"in/2"}, "agent_error", "r3"), ""},
		{"a redo by the person", []string{"talk/1", "talk/2", "talk/3"}, nil, round([]string{"s1"}, "person", []string{"in/2"}, "agent_error", "in/3"), ""},
		{"a trigger before the attempt", both, nil, round([]string{"s2"}, "person", []string{"in/2"}, "agent_error", "s3"), ""},
		{"a redo before the trigger", both, nil, round([]string{"r1"}, "tool", []string{"s3"}, "agent_error", "s2"), ""},
		{"an attempt placed by its earliest step", both, nil, round([]string{"s3", "s1"}, "person", []string{"in/2"}, "agent_error", "r2"), "t1.r1:agent_error:in/2..r2"},
		{"a trigger placed by its earliest step", []string{"talk/1", "talk/2", "talk/3"}, nil, round([]string{"s1"}, "person", []string{"in/3", "in/2"}, "agent_error", "s3"),
			"t1.r1:agent_error:in/2..s3"},
		{"a redo with a step before its trigger", both, nil, round([]string{"r1"}, "tool", []string{"s2"}, "agent_error", "s3", "s1"), ""},
		{"a changed requirement from the person", both, nil, round([]string{"s1"}, "person", []string{"in/2"}, "requirement_change", "s3"),
			"t1.r1:requirement_change:in/2..s3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs, note := reworked(t, tc.talks, tc.window, tc.round)
			if bounds(rs) != tc.want {
				t.Errorf("rounds %q, want %q", bounds(rs), tc.want)
			}
			if (tc.want == "") != strings.Contains(note, "1 rounds of rework of target t1 without an attempt, a trigger and a redo in recorded order were left out") {
				t.Errorf("note %q", note)
			}
		})
	}
}

// Only a tool step whose result came returned: not one that never returned,
// a model API error, an interrupt, a reply or an input.
func TestOnlyAToolStepThatReturnedShowsAResult(t *testing.T) {
	nav := deliveries()
	returned := func(n *cited) bool { return n.returned }
	for id, want := range map[string]string{"s1": "s1", "p1": "", "x1": "", "i1": "", "r1": "", "in/1": ""} {
		if got := place(nav, []sessionjudge.Cite{{Node: id}}, returned, true); got != want {
			t.Errorf("%s: placed at %q, want %q", id, got, want)
		}
	}
}

// A cause the record cannot show is unknown: a cause asz does not know, and
// a changed requirement that no person's input triggered.
func TestAReworkCauseTheRecordCannotShowIsUnknown(t *testing.T) {
	for _, r := range []reworkEntry{
		round([]string{"s1"}, "tool", []string{"s2"}, "requirement_change", "s3"),
		round([]string{"s1"}, "person", []string{"in/2"}, "laziness", "s3"),
	} {
		rs, note := reworked(t, both, nil, r)
		if len(rs) != 1 || rs[0].Cause != sessionjudge.ReworkUnknown ||
			note != "1 rounds of rework of target t1 with a cause the record cannot show were given an unknown cause" {
			t.Errorf("%+v: %s %q", r, bounds(rs), note)
		}
	}
}
