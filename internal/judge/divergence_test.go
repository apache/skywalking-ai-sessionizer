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

// diverged checks a divergence answer over worked: r1 is about in/1, which
// set t1; r2 about in/3, which set t2; r3 about no input; r4 about in/2,
// which corrected t1 and which t2 cites.
func diverged(t *testing.T, misled bool, answer string) *checked {
	t.Helper()
	w := workedDecisions()
	nav, _ := worked()
	nav.Assign(Assignment{Conclusions: []sessionjudge.Conclusion{
		{ID: "r1", Kind: "final", Statement: "the clock drifts", Scope: []string{"in/1"}},
		{ID: "r2", Kind: "final", Statement: "the disk is slow", Scope: []string{"in/3"}},
		{ID: "r3", Kind: "intermediate", Statement: "an aside", Scope: []string{}},
		{ID: "r4", Kind: "final", Statement: "the correction was wrong", Scope: []string{"in/2"}}}})
	// The sub-task stage found a sub-task misled the agent, one this
	// conversation does not hold, so it is no target's.
	var found []*sessionjudge.Subtask
	if misled {
		found = []*sessionjudge.Subtask{{ID: "stream/z"}}
	}
	c, err := checkDivergence(nav, w.targets, w.inputs, w.achievements, found, json.RawMessage(answer))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func verdict(conclusion, v, target, event string, criteria []string, whole bool, cites ...string) map[string]any {
	return map[string]any{"conclusion": conclusion, "verdict": v, "target": target, "event": event, "criteria": criteria, "whole": whole, "note": "", "cite": cites}
}

func divergenceAnswer(divs []map[string]any, rollbacks, findings []map[string]any) string {
	b, _ := json.Marshal(map[string]any{"divergences": divs, "rollbacks": rollbacks, "findings": findings})
	return string(b)
}

func divergencesOf(c *checked) map[string]*sessionjudge.Divergence {
	out := map[string]*sessionjudge.Divergence{}
	for _, f := range c.frames {
		if d, ok := f.(*sessionjudge.Divergence); ok {
			out[d.Conclusion] = d
		}
	}
	return out
}

// Each rule of a verdict on its own: the other two conclusions are not
// applicable, and the case's conclusion gets the verdict given.
func TestEachVerdictRuleStandsAlone(t *testing.T) {
	na := func(id string) map[string]any { return verdict(id, "not_applicable", "", "", nil, false) }
	for _, tc := range []struct {
		name string
		div  map[string]any
		want string
		note string
	}{
		{"reached, at a reply", verdict("r1", "reached", "", "", nil, false, "r2"), "reached", ""},
		{"reached, at nothing", verdict("r1", "reached", "", "", nil, false, "in/1"), "unknown", "1 verdicts or causes the record cannot show"},
		{"contradicted, some criteria", verdict("r1", "contradicted", "t1", "t1.e3", []string{"t1.c1", "t1.c9"}, false, "in/3"), "contradicted t1 t1.e3 [t1.c1] false", ""},
		{"contradicted, the conclusion delivered", verdict("r1", "contradicted", "t1", "t1.e3", nil, true, "s3", "in/3"), "contradicted t1 t1.e3 [] true", ""},
		{"contradicted, whole with no delivery cited", verdict("r1", "contradicted", "t1", "t1.e3", nil, true, "in/3"), "unknown", "1 verdicts or causes the record cannot show"},
		{"contradicted, a target it is not about", verdict("r2", "contradicted", "t1", "t1.e3", []string{"t1.c1"}, false), "unknown", "1 contradictions or rollbacks that name no delivery"},
		{"contradicted, an undoing", verdict("r1", "contradicted", "t1", "t1.e2", []string{"t1.c1"}, false), "unknown", "1 contradictions or rollbacks that name no delivery"},
		{"contradicted, another target's delivery", verdict("r1", "contradicted", "t1", "t2.e1", []string{"t1.c1"}, false), "unknown", "1 contradictions or rollbacks that name no delivery"},
		{"contradicted, no such event", verdict("r1", "contradicted", "t1", "t1.e9", []string{"t1.c1"}, false), "unknown", "1 contradictions or rollbacks that name no delivery"},
		{"contradicted, no such target", verdict("r1", "contradicted", "t9", "t9.e1", []string{"t9.c1"}, false), "unknown", "1 contradictions or rollbacks that name no delivery"},
		{"contradicted, about no input", verdict("r3", "contradicted", "t1", "t1.e3", []string{"t1.c1"}, false), "unknown", "1 contradictions or rollbacks that name no delivery"},
		{"contradicted, about an input that changed the target", verdict("r4", "contradicted", "t1", "t1.e3", []string{"t1.c1"}, false, "in/3"), "contradicted t1 t1.e3 [t1.c1] false", ""},
		{"contradicted, about an input the target cites", verdict("r4", "contradicted", "t2", "t2.e1", []string{"t2.c1"}, false, "in/3"), "contradicted t2 t2.e1 [t2.c1] false", ""},
		{"not observed", verdict("r1", "not_observed", "", "", nil, false), "not_observed asserted", ""},
		{"a verdict asz does not know", verdict("r1", "partly", "", "", nil, false), "unknown", "1 verdicts or causes the record cannot show"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var divs []map[string]any
			for _, id := range []string{"r1", "r2", "r3", "r4"} {
				if id == tc.div["conclusion"] {
					divs = append(divs, tc.div)
				} else {
					divs = append(divs, na(id))
				}
			}
			c := diverged(t, false, divergenceAnswer(divs, nil, nil))
			d := divergencesOf(c)[tc.div["conclusion"].(string)]
			got := d.Verdict
			switch {
			case d.Verdict == sessionjudge.VerdictContradicted:
				got = fmt.Sprintf("%s %s %s %v %v", d.Verdict, d.Target, d.Event, d.Criteria, d.Whole)
				if d.Criteria == nil {
					got = fmt.Sprintf("%s %s %s [] %v", d.Verdict, d.Target, d.Event, d.Whole)
				}
			case d.Asserted:
				got += " asserted"
			}
			if got != tc.want {
				t.Errorf("verdict %q, want %q", got, tc.want)
			}
			if (tc.note == "") != (c.note() == "") || !strings.Contains(c.note(), tc.note) {
				t.Errorf("note %q, want %q", c.note(), tc.note)
			}
		})
	}
	// A conclusion left out is unknown; one answered twice keeps its first
	// verdict; a conclusion the call was not given is passed over.
	c := diverged(t, false, divergenceAnswer([]map[string]any{verdict("r1", "reached", "", "", nil, false, "r2"),
		verdict("r1", "not_observed", "", "", nil, false), verdict("r9", "reached", "", "", nil, false, "r2")}, nil, nil))
	ds := divergencesOf(c)
	if ds["r1"].Verdict != "reached" || ds["r2"].Verdict != "unknown" || ds["r3"].Verdict != "unknown" || ds["r9"] != nil ||
		!strings.Contains(c.note(), "3 conclusions were not answered and are unknown") {
		t.Fatalf("divergences %+v %+v %+v; %q", ds["r1"], ds["r2"], ds["r3"], c.note())
	}
}

// A rollback names a delivery of a target its conclusion is about, and the
// target's own criteria.
func TestARollbackNamesADeliveryItIsAbout(t *testing.T) {
	all := []map[string]any{verdict("r1", "reached", "", "", nil, false, "r2"), verdict("r2", "not_applicable", "", "", nil, false),
		verdict("r3", "not_applicable", "", "", nil, false), verdict("r4", "not_applicable", "", "", nil, false)}
	c := diverged(t, false, divergenceAnswer(all, []map[string]any{
		{"conclusion": "r1", "target": "t1", "event": "t1.e3", "criteria": []string{"t1.c2", "t1.c7"}, "note": "rolled back in 0.4.1", "cite": []string{"s3"}},
		{"conclusion": "r2", "target": "t1", "event": "t1.e3", "criteria": []string{}, "note": "", "cite": []string{"s3"}},
		{"conclusion": "r9", "target": "t1", "event": "t1.e3", "criteria": []string{}, "note": "", "cite": []string{}},
		// Criteria, none of them the target's, are not the whole result.
		{"conclusion": "r1", "target": "t1", "event": "t1.e3", "criteria": []string{"t1.c7"}, "note": "", "cite": []string{"s3"}},
	}, nil))
	var rbs []*sessionjudge.Rollback
	for _, f := range c.frames {
		if r, ok := f.(*sessionjudge.Rollback); ok {
			rbs = append(rbs, r)
		}
	}
	if len(rbs) != 1 || rbs[0].Conclusion != "r1" || fmt.Sprint(rbs[0].Criteria) != "[t1.c2]" || rbs[0].Note != "rolled back in 0.4.1" ||
		!strings.Contains(c.note(), "2 contradictions or rollbacks that name no delivery") ||
		!strings.Contains(c.note(), "1 rollbacks that name none of the target's criteria were left out") {
		t.Fatalf("rollbacks %+v; %q", rbs, c.note())
	}
}

// A finding of divergence names a cause only the evidence supports: missing
// context needs what failed to arrive, a misleading sub-task one the
// sub-task stage found, and a cause of absence is the evaluator's judgment.
func TestADivergenceFindingNamesACauseItCanShow(t *testing.T) {
	all := []map[string]any{verdict("r1", "not_observed", "", "", nil, false), verdict("r2", "not_applicable", "", "", nil, false),
		verdict("r3", "not_applicable", "", "", nil, false), verdict("r4", "not_applicable", "", "", nil, false)}
	f := func(target, at, cause string, cites ...string) map[string]any {
		return map[string]any{"target": target, "at": at, "conclusion": "r1", "cause": cause, "problem": "it went another way", "explanation": "",
			"improvement": "check the clock first", "cite": cites}
	}
	for _, tc := range []struct {
		name   string
		misled bool
		f      map[string]any
		want   string
	}{
		{"a cause it supports", false, f("t1", "r1", "wrong_interpretation", "s1"), "wrong_interpretation"},
		{"a cause asz does not know", false, f("t1", "r1", "bad_luck"), "unavailable"},
		{"missing context with nothing that shows it", false, f("t1", "r1", "missing_context", "r2"), "unavailable"},
		{"missing context shown", false, f("t1", "r1", "missing_context", "s2"), "missing_context"},
		{"a sub-task no stage found misleading", false, f("t1", "r1", "misled_by_subtask"), "unavailable"},
		{"a sub-task that misled other work", true, f("t1", "r1", "misled_by_subtask", "s1"), "unavailable"},
		{"a question never asked", false, f("t1", "in/2", "clarification_not_sought"), "clarification_not_sought asserted"},
		{"a premise never checked", false, f("t1", "s1", "unsupported_assumption"), "unsupported_assumption asserted"},
		{"other work", false, f("t1", "r3", "wrong_interpretation"), "left out"},
		{"no such target", false, f("t9", "r1", "wrong_interpretation"), "left out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := diverged(t, tc.misled, divergenceAnswer(all, nil, []map[string]any{tc.f}))
			got := "left out"
			for _, fr := range c.frames {
				if x, ok := fr.(*sessionjudge.Finding); ok {
					if x.Source != sessionjudge.StageDivergence || x.Conclusion != "r1" || x.Target != tc.f["target"] || x.Improvement != "check the clock first" {
						t.Fatalf("finding %+v", x)
					}
					got = x.Cause
					if x.Asserted {
						got += " asserted"
					}
				}
			}
			if got != tc.want {
				t.Errorf("cause %q, want %q; %q", got, tc.want, c.note())
			}
		})
	}
}

// A misleading sub-task is a finding's cause only when the sub-task stage
// found it misled the agent, it was started in the target's talks, and the
// finding cites it: one of its steps, or the step that started it.
func TestAMisleadingSubtaskIsTheTargetsAndCited(t *testing.T) {
	target := deliveryTarget()
	inputs := []*sessionjudge.Input{{Node: "in/1", Actions: []sessionjudge.Action{{Target: "t1", Action: sessionjudge.ActionCreate}}}}
	achievements := []*sessionjudge.Achievement{{Target: "t1", Events: []sessionjudge.Event{{ID: "t1.e1", Kind: sessionjudge.EventDelivered, Artifact: "file",
		Cite: []sessionjudge.Cite{{Node: "s3"}}}}}}
	stream := []*sessionjudge.Subtask{{ID: "stream/a"}}
	// p1 comes after s3, which started the sub-task; s1 before it.
	for _, tc := range []struct {
		name   string
		talks  []string
		misled []*sessionjudge.Subtask
		at     string
		cites  []string
		want   string
	}{
		{"its own step cited", nil, stream, "p1", []string{"sa"}, "misled_by_subtask"},
		{"the step that started it cited", nil, stream, "p1", []string{"s3"}, "misled_by_subtask"},
		{"nothing of it cited", nil, stream, "p1", []string{"s1"}, "unavailable"},
		{"at a step before it started", nil, stream, "s1", []string{"sa"}, "unavailable"},
		{"started in another target's talk", []string{"talk/1", "talk/3"}, stream, "r3", []string{"sa"}, "unavailable"},
		{"not found misleading", nil, nil, "p1", []string{"sa"}, "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nav := deliveries()
			nav.Assign(Assignment{Conclusions: []sessionjudge.Conclusion{{ID: "r1", Kind: "final", Statement: "the cache is fine", Scope: []string{"in/1"}}}})
			tg := target
			if tc.talks != nil {
				tg.Talks = tc.talks
			}
			answer := divergenceAnswer([]map[string]any{verdict("r1", "not_observed", "", "", nil, false)}, nil, []map[string]any{{"target": "t1",
				"at": tc.at, "conclusion": "r1", "cause": "misled_by_subtask", "problem": "it trusted the report", "explanation": "", "improvement": "", "cite": tc.cites}})
			c, err := checkDivergence(nav, []sessionjudge.Target{tg}, inputs, achievements, tc.misled, json.RawMessage(answer))
			if err != nil {
				t.Fatal(err)
			}
			got := "left out"
			for _, fr := range c.frames {
				if x, ok := fr.(*sessionjudge.Finding); ok {
					got = x.Cause
				}
			}
			if got != tc.want {
				t.Errorf("cause %q, want %q; %q", got, tc.want, c.note())
			}
		})
	}
}
