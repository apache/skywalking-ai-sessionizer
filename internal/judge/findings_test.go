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
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
)

func finding(target, at, problem string, cites ...string) findingEntry {
	return findingEntry{Target: target, At: at, Problem: problem, Explanation: "it guessed", Improvement: "read the log first", Cite: cites}
}

// An achievement keeps the findings anchored at a step of its target's work,
// a sub-agent's step and work before the judged window included.
func TestEachAchievementFindingRuleStandsAlone(t *testing.T) {
	for _, tc := range []struct {
		name          string
		talks, window []string
		f             findingEntry
		kept          bool
	}{
		{"a step of the target's talk", both, nil, finding("", "r1", "the cause named was wrong", "in/2"), true},
		{"the person's input in it", both, nil, finding("", "in/2", "the person had to correct it"), true},
		{"a step of a sub-agent of the target", both, nil, finding("", "sa", "the test ran late"), true},
		{"a step before the window", []string{"talk/2"}, []string{"talk/2", "talk/3"}, finding("", "r1", "the cause named was wrong"), true},
		{"a step of other work", both, nil, finding("", "r3", "the reply was empty"), false},
		{"a talk, not a step", both, nil, finding("", "talk/2", "the talk went wrong"), false},
		{"a step that does not exist", both, nil, finding("", "made/up", "it went wrong"), false},
		{"a step that does not exist, in a window", []string{"talk/2"}, []string{"talk/2", "talk/3"}, finding("", "made/up", "it went wrong"), false},
		{"a step in no talk, in a window", []string{"talk/2"}, []string{"talk/2", "talk/3"}, finding("", "l1", "it went wrong"), false},
		{"no problem said", both, nil, finding("", "r1", " "), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nav := deliveries()
			if tc.window != nil {
				nav.Within(&Selection{Talks: tc.window})
			}
			b, _ := json.Marshal([]findingEntry{tc.f})
			raw := `{"achievements":[{"target":"t1","basis":"observed_result","criteria":[{"id":"t1.c1","state":"met","cite":["s3"]},{"id":"t1.c2","state":"unknown","cite":[]}],` +
				`"claims":[],"findings":` + string(b) + `,"note":"","cite":["s3"]}]}`
			target := deliveryTarget()
			target.Talks = tc.talks
			c, err := checkAchievement(nav, []sessionjudge.Target{target}, json.RawMessage(raw))
			if err != nil {
				t.Fatal(err)
			}
			a := c.frames[0].(*sessionjudge.Achievement)
			if kept := len(a.Findings) == 1; kept != tc.kept {
				t.Fatalf("kept %v: %+v", kept, a.Findings)
			}
			if tc.kept {
				f := a.Findings[0]
				if f.At != tc.f.At || f.Ref == nil || f.Problem != tc.f.Problem || f.Explanation != "it guessed" || f.Improvement != "read the log first" ||
					len(f.Cite) != len(tc.f.Cite) || f.Target != "" {
					t.Fatalf("finding %+v", f)
				}
			}
			if tc.kept == strings.Contains(c.note(), "1 findings of target t1 not anchored at a step of its work were left out") {
				t.Fatalf("note %q", c.note())
			}
		})
	}
}

// The efficiency stage writes each finding as a frame of its own, naming its
// target, when it is anchored at a talk the call labels for that target.
func TestAnEfficiencyFindingIsOfWorkTheCallLabels(t *testing.T) {
	targets := []sessionjudge.Target{deliveryTarget(), {ID: "t2", Talks: []string{"talk/3"}}}
	b, _ := json.Marshal([]findingEntry{
		finding("t1", "s2", "the revert was not needed", "s2"),
		finding("t1", "r3", "the thanks needed no reply"),
		finding("t2", "s2", "the revert was not needed"),
		finding("t9", "s2", "the revert was not needed"),
	})
	nav := deliveries()
	nav.Assign(Assignment{Talks: []string{"talk/2", "talk/3"}})
	c, err := checkEfficiency(nav, targets, nil, false, json.RawMessage(`{"labels":[],"findings":`+string(b)+`}`))
	if err != nil {
		t.Fatal(err)
	}
	var kept []*sessionjudge.Finding
	for _, f := range c.frames {
		if x, ok := f.(*sessionjudge.Finding); ok {
			kept = append(kept, x)
		}
	}
	if len(kept) != 1 || kept[0].Target != "t1" || kept[0].At != "s2" {
		t.Fatalf("findings %+v", kept)
	}
	if !strings.Contains(c.note(), "3 findings not anchored at a step of a talk this call labels, for its target, were left out") {
		t.Fatalf("note %q", c.note())
	}
	// A talk of the target that another call labels is not this call's.
	nav.Assign(Assignment{Talks: []string{"talk/3"}})
	c, _ = checkEfficiency(nav, targets, nil, false, json.RawMessage(`{"labels":[],"findings":`+string(b)+`}`))
	if strings.Count(c.note(), "4 findings not anchored") != 1 {
		t.Fatalf("another call's talk: %q", c.note())
	}
}
