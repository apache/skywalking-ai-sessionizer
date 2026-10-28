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
	"slices"

	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
)

// bound says which targets a conclusion is about: those an input of its
// scope set or acted on. A conclusion with no scope is about none, so it
// can change no score.
func bound(c sessionjudge.Conclusion, targets []sessionjudge.Target, inputs []*sessionjudge.Input) map[string]bool {
	scope := set(c.Scope)
	out := map[string]bool{}
	for _, t := range targets {
		if slices.ContainsFunc(t.Cite, func(ct sessionjudge.Cite) bool { return scope[ct.Node] }) {
			out[t.ID] = true
		}
	}
	for _, in := range inputs {
		if !scope[in.Node] {
			continue
		}
		for _, a := range in.Actions {
			if a.Target != "" {
				out[a.Target] = true
			}
		}
	}
	return out
}

// misledIn says whether a sub-task the sub-task stage found misled the main
// agent was started in a target's talks, before the step a finding is at,
// and the cites name it: one of its own steps, or the step that started it.
// A sub-task that misled other work, or started later, shows nothing of
// this step.
func misledIn(nav *Nav, t *sessionjudge.Target, misled []*sessionjudge.Subtask, at string, cites []sessionjudge.Cite) bool {
	talks := set(t.Talks)
	for _, d := range misled {
		st, ok := nav.ledger.Subtask(d.ID)
		if !ok || !talks[st.Main] || !recordedAfter(nav, at, st.StartedBy) {
			continue
		}
		if slices.ContainsFunc(cites, func(c sessionjudge.Cite) bool {
			return c.Node == st.StartedBy || slices.Contains(st.Talks, nav.TalkOf(c.Node))
		}) {
			return true
		}
	}
	return false
}

// absent are the causes that rest on something not happening: a question
// never asked, a check never made. The record cannot show the evaluator read
// all of where it would have happened, so such a finding is its judgment,
// shown with how much of its span the evaluator was given.
var absent = []string{sessionjudge.CauseQuestionNotAsked, sessionjudge.CauseAssumptionNotChecked}

// checkDivergence checks a divergence answer: how the conversation compares
// with each conclusion the call was given, the rollbacks a source reports,
// and why the conversation did not reach a conclusion.
//
// A conclusion reached cites where the conversation reached it: a reply of
// the agent, or a step of its work that shows something happened. A contradiction names a target the
// conclusion is about, a delivery of that target, and either the criteria
// it shows not met or the delivered conclusion itself, which it cites. A
// rollback names the same. A verdict that something was not observed, and a
// cause of absence, rest on reading all of where it would be, which the
// record cannot show: they are the evaluator's judgment. Missing context
// needs evidence of what failed to arrive, and a misleading sub-task one the
// sub-task stage found misled the agent, in the target's work, which the
// finding cites. What does not hold is unknown, or unavailable for a cause.
func checkDivergence(nav *Nav, targets []sessionjudge.Target, inputs []*sessionjudge.Input, achievements []*sessionjudge.Achievement,
	misled []*sessionjudge.Subtask, raw json.RawMessage) (*checked, error) {
	var a struct {
		Divergences []struct {
			Conclusion string   `json:"conclusion"`
			Verdict    string   `json:"verdict"`
			Target     string   `json:"target"`
			Event      string   `json:"event"`
			Criteria   []string `json:"criteria"`
			Whole      bool     `json:"whole"`
			Note       string   `json:"note"`
			Cite       []string `json:"cite"`
		} `json:"divergences"`
		Rollbacks []struct {
			Conclusion string   `json:"conclusion"`
			Target     string   `json:"target"`
			Event      string   `json:"event"`
			Criteria   []string `json:"criteria"`
			Note       string   `json:"note"`
			Cite       []string `json:"cite"`
		} `json:"rollbacks"`
		Findings []struct {
			findingEntry
			Conclusion string `json:"conclusion"`
			Cause      string `json:"cause"`
			// From is where the question or the assumption arose, for a
			// cause that rests on something not happening.
			From string `json:"from"`
		} `json:"findings"`
	}
	if err := decodeAnswer(raw, "divergences", &a); err != nil {
		return nil, err
	}
	c := &checked{}
	conclusions := map[string]sessionjudge.Conclusion{}
	for _, k := range nav.assigned.Conclusions {
		conclusions[k.ID] = k
	}
	byTarget := map[string]*sessionjudge.Target{}
	for i := range targets {
		byTarget[targets[i].ID] = &targets[i]
	}
	// Each target's events, so an event is only ever its own target's.
	events := map[string]map[string]sessionjudge.Event{}
	for _, ach := range achievements {
		events[ach.Target] = map[string]sessionjudge.Event{}
		for _, e := range ach.Events {
			events[ach.Target][e.ID] = e
		}
	}
	// delivery says whether an event is a delivery of a target the
	// conclusion is about, and keeps the criteria of the target it names.
	delivery := func(k sessionjudge.Conclusion, target, event string, criteria []string) (*sessionjudge.Target, []string, bool) {
		t := byTarget[target]
		e, ok := events[target][event]
		if t == nil || !bound(k, targets, inputs)[target] || !ok || (e.Kind != sessionjudge.EventDelivered && e.Kind != sessionjudge.EventDeliveredAgain) {
			return nil, nil, false
		}
		var keep []string
		for _, id := range criteria {
			if slices.ContainsFunc(t.Criteria, func(cr sessionjudge.Criterion) bool { return cr.ID == id && !cr.Dropped }) && !slices.Contains(keep, id) {
				keep = append(keep, id)
			}
		}
		return t, keep, true
	}
	dropped, weakened, unbound, noCriteria := 0, 0, 0, 0
	answered := map[string]bool{}
	for _, x := range a.Divergences {
		k, ok := conclusions[x.Conclusion]
		if !ok || answered[x.Conclusion] {
			continue
		}
		answered[x.Conclusion] = true
		cites := c.cites(nav, x.Cite, &dropped)
		d := &sessionjudge.Divergence{Conclusion: k.ID, Verdict: x.Verdict, Note: x.Note, Cite: cites}
		switch x.Verdict {
		case sessionjudge.VerdictReached:
			// Reaching a conclusion is the agent's work: the person saying
			// it is not the conversation reaching it.
			if !shows(nav, cites, false, nil) && !citesKind(nav, cites, model.KindMessageAssistant) {
				weakened++
				d.Verdict = sessionjudge.VerdictUnknown
			}
		case sessionjudge.VerdictContradicted:
			t, criteria, ok := delivery(k, x.Target, x.Event, x.Criteria)
			whole := ok && x.Whole && slices.ContainsFunc(events[x.Target][x.Event].Cite, func(ct sessionjudge.Cite) bool {
				return slices.ContainsFunc(cites, func(o sessionjudge.Cite) bool { return o.Node == ct.Node })
			})
			switch {
			case !ok:
				unbound++
				d.Verdict = sessionjudge.VerdictUnknown
			case !whole && len(criteria) == 0:
				weakened++
				d.Verdict = sessionjudge.VerdictUnknown
			default:
				d.Target, d.Event, d.Whole = t.ID, x.Event, whole
				if !whole {
					d.Criteria = criteria
				}
			}
		case sessionjudge.VerdictNotObserved:
			d.Asserted, d.Given = true, nav.GivenWindow()
		case sessionjudge.VerdictUnknown, sessionjudge.VerdictNotApplicable:
		default:
			weakened++
			d.Verdict = sessionjudge.VerdictUnknown
		}
		c.frames = append(c.frames, d)
	}
	missing := 0
	for _, k := range nav.assigned.Conclusions {
		if !answered[k.ID] {
			missing++
			c.frames = append(c.frames, &sessionjudge.Divergence{Conclusion: k.ID, Verdict: sessionjudge.VerdictUnknown, Cite: []sessionjudge.Cite{}})
		}
	}
	for _, x := range a.Rollbacks {
		k, ok := conclusions[x.Conclusion]
		cites := c.cites(nav, x.Cite, &dropped)
		var t *sessionjudge.Target
		var criteria []string
		if ok {
			t, criteria, ok = delivery(k, x.Target, x.Event, x.Criteria)
		}
		if !ok {
			unbound++
			continue
		}
		// A rollback that named criteria, none of them the target's, is not
		// one of the whole result.
		if len(x.Criteria) > 0 && len(criteria) == 0 {
			noCriteria++
			continue
		}
		c.frames = append(c.frames, &sessionjudge.Rollback{Conclusion: k.ID, Target: t.ID, Event: x.Event, Criteria: criteria, Note: x.Note, Cite: cites})
	}
	unanchored := 0
	for _, x := range a.Findings {
		t := byTarget[x.Target]
		if t == nil {
			unanchored++
			continue
		}
		talks := set(t.Talks)
		kept, left := checkFindings(c, nav, []findingEntry{x.findingEntry}, func(main string) bool { return talks[main] || nav.beforeWindow(main) }, &dropped)
		unanchored += left
		for _, f := range kept {
			f.Target, f.Source, f.Conclusion, f.Cause = t.ID, sessionjudge.StageDivergence, x.Conclusion, x.Cause
			if _, ok := conclusions[x.Conclusion]; !ok {
				f.Conclusion = ""
			}
			switch {
			case !slices.Contains(sessionjudge.FindingCauses, f.Cause),
				f.Cause == sessionjudge.CauseMissingContext && !shows(nav, f.Cite, false, nil),
				f.Cause == sessionjudge.CauseMisledBySubtask && !misledIn(nav, t, misled, f.At, f.Cite):
				weakened++
				f.Cause = sessionjudge.CauseUnavailable
			case slices.Contains(absent, f.Cause):
				// What was given of the span the question or the assumption
				// stands over is measured from a start asz checks: one in
				// the stream of the finding's step, at or before it.
				var valid bool
				f.Asserted = true
				if f.Given, valid = nav.GivenSpan(x.From, f.At); valid {
					f.From = x.From
				}
			}
			c.frames = append(c.frames, &f)
		}
	}
	if missing > 0 {
		c.problem("%d conclusions were not answered and are unknown", missing)
	}
	if unbound > 0 {
		c.problem("%d contradictions or rollbacks that name no delivery of a target the conclusion is about were left out", unbound)
	}
	if noCriteria > 0 {
		c.problem("%d rollbacks that name none of the target's criteria were left out", noCriteria)
	}
	if weakened > 0 {
		c.problem("%d verdicts or causes the record cannot show were read as unknown or unavailable", weakened)
	}
	if unanchored > 0 {
		c.problem("%d findings not anchored at a step of a target's work were left out", unanchored)
	}
	if dropped > 0 {
		c.problem("%d citations of nodes that do not exist were left out", dropped)
	}
	return c, nil
}
