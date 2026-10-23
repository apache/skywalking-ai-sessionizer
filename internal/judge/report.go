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
	"slices"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
)

// report adds to the close what the stages recorded of each target's
// result. It amends each target's outcome with its class, its two scores,
// what its rework cost by cause, the person's corrections and its file
// operations; it returns the conversation's file operations and the
// findings asz records itself, a frame each; and it adds to the summary the
// final score, each goal's outcome and the file operations no target made.
// It reads only what the stages kept, never their prose.
//
// frames are the close's frames so far, whose outcomes it amends, and
// findings the efficiency stage's.
func report(nav *Nav, l *Ledger, targets []sessionjudge.Target, goals []sessionjudge.Goal, inputs []*sessionjudge.Input,
	achievements []*sessionjudge.Achievement, findings []*sessionjudge.Finding, labels []*sessionjudge.Label, subtasks map[string]*sessionjudge.Subtask,
	refs references, frames []any, sum *sessionjudge.Summary) []any {
	outcome := map[string]*sessionjudge.Outcome{}
	for _, f := range frames {
		if o, ok := f.(*sessionjudge.Outcome); ok {
			outcome[o.Target] = o
		}
	}
	judged := map[string]*sessionjudge.Achievement{}
	for _, a := range achievements {
		judged[a.Target] = a
	}
	claims := map[string][]string{}
	for _, t := range targets {
		for _, tk := range t.Talks {
			claims[tk] = append(claims[tk], t.ID)
		}
	}
	who := ownersOfCalls(l, claims, inputs)
	// How each call was labeled, as the close prices it, so rework is priced
	// the same way.
	work := labelCalls(l, targets, who, labels, subtasks)

	// Every finding of a target, the stages' and asz's, for the goals.
	found := map[string][]sessionjudge.Finding{}
	for _, a := range achievements {
		for _, f := range a.Findings {
			f.Target, f.Source = a.Target, sessionjudge.StageAchievement
			found[a.Target] = append(found[a.Target], f)
		}
	}
	for _, f := range findings {
		x := *f
		x.T = ""
		if x.Source == "" {
			x.Source = sessionjudge.StageEfficiency
		}
		found[f.Target] = append(found[f.Target], x)
	}

	opsOf, unassigned := operationsOf(nav, l, claims, who)
	sum.UnassignedOperations = unassigned
	// The operations themselves, which outcomes and accounts name by id.
	var out []any
	for _, op := range nav.Operations() {
		out = append(out, &op)
	}
	var finals []float64
	for _, t := range targets {
		o := outcome[t.ID]
		o.Operations = opsOf[t.ID]
		for _, in := range inputs {
			if slices.ContainsFunc(in.Actions, func(a sessionjudge.Action) bool {
				return a.Target == t.ID && a.Action == sessionjudge.ActionCorrect
			}) {
				o.Corrections = append(o.Corrections, in.Node)
			}
		}
		a := judged[t.ID]
		if a == nil {
			continue
		}
		applied, undone := standing(o, a)
		// What a source shows of the deliveries standing applies as what
		// the conversation shows does.
		ra, rundone, used := refs.applying(t, o.Standing, targets, inputs)
		applied, undone, o.References = append(applied, ra...), joinUndone(undone, rundone), used
		o.Class = outcomeClass(t, a, applied, undone)
		scored := o.Class != sessionjudge.ClassCancelled && o.Class != sessionjudge.ClassReplaced
		// A target's scores read with four decimals, as the run's do; the
		// run's mean adds them unrounded.
		if scored {
			if v := recordScore(o.Level, o.Met, o.Criteria, o.Contradicted); v != nil {
				o.ScoreRecord = round4(v)
			}
			if v := finalScore(a, applied, undone); v != nil {
				finals = append(finals, *v)
				o.ScoreFinal = round4(v)
			}
		}
		o.Rework = reworkCost(nav, l, a.Rework, who, t.ID, work[t.ID])
		mine := mechanicalFindings(nav, t, a, scored, applied, undone, o.Corrections)
		for i := range mine {
			out = append(out, &mine[i])
			found[t.ID] = append(found[t.ID], mine[i])
		}
	}
	if sum.Score != nil && len(finals) > 0 {
		total := 0.0
		for _, f := range finals {
			total += f
		}
		sum.Score.Final = ratio(total, float64(len(finals)))
	}
	for _, g := range goals {
		sum.Goals = append(sum.Goals, goalOutcome(nav, g, outcome, found))
	}
	return out
}

// standing finds what applies to a target's result at the end: the
// contradictions tied to the deliveries standing, and the undoings that
// took back a delivery still standing, joined as one. Contradictions of a
// delivery a later one corrected stay in the history, and so does an undoing
// whose deliveries were all delivered again since.
func standing(o *sessionjudge.Outcome, a *sessionjudge.Achievement) ([]sessionjudge.Contradiction, *sessionjudge.Event) {
	var applied []sessionjudge.Contradiction
	var undone *sessionjudge.Event
	var undoings []sessionjudge.Event
	o.Standing, undoings = settle(a.Events)
	for _, x := range a.Contradictions {
		if slices.Contains(o.Standing, x.Event) {
			applied = append(applied, x)
		}
	}
	for i := range undoings {
		o.Undone = append(o.Undone, undoings[i].ID)
		undone = joinUndone(undone, &undoings[i])
	}
	return applied, undone
}

// standingOf names the deliveries standing at the end.
func standingOf(events []sessionjudge.Event) []string {
	stand, _ := settle(events)
	return stand
}

// settle walks a target's events in recorded order. The deliveries standing
// at the end are every delivery, in parts or whole, except those a later
// delivery again replaced: the ones it names, or every one before it when it
// names none. An undoing took back the deliveries it names, or every one
// standing then; it applies at the end while one of them still stands.
func settle(events []sessionjudge.Event) ([]string, []sessionjudge.Event) {
	var stand []string
	type undoing struct {
		event sessionjudge.Event
		of    []string
	}
	var undoings []undoing
	for _, e := range events {
		switch e.Kind {
		case sessionjudge.EventDeliveredAgain:
			if len(e.Replaces) == 0 {
				stand = nil
			}
			stand = append(slices.DeleteFunc(stand, func(id string) bool { return slices.Contains(e.Replaces, id) }), e.ID)
		case sessionjudge.EventDelivered:
			stand = append(stand, e.ID)
		case sessionjudge.EventUndone:
			of := e.Undoes
			if len(of) == 0 {
				of = slices.Clone(stand)
			}
			undoings = append(undoings, undoing{e, of})
		}
	}
	var applies []sessionjudge.Event
	for _, u := range undoings {
		if slices.ContainsFunc(u.of, func(id string) bool { return slices.Contains(stand, id) }) {
			applies = append(applies, u.event)
		}
	}
	return stand, applies
}

// outcomeClass is a target's class: never scored when the person cancelled or
// replaced it, whatever its events; contradicted when what applies shows a
// standing delivery wrong; reverted when it was undone; its level otherwise.
func outcomeClass(t sessionjudge.Target, a *sessionjudge.Achievement, applied []sessionjudge.Contradiction, undone *sessionjudge.Event) string {
	switch {
	case t.Status == sessionjudge.TargetCancelled || t.Status == sessionjudge.TargetReplaced:
		return t.Status
	case len(applied) > 0:
		return sessionjudge.ClassContradicted
	case undone != nil:
		return sessionjudge.ClassReverted
	}
	return a.Level
}

// references are what the reference and divergence stages found: the
// conclusions the sources hold, how the conversation compares with each,
// and the rollbacks the sources report.
type references struct {
	conclusions []sessionjudge.Conclusion
	divergences []*sessionjudge.Divergence
	rollbacks   []*sessionjudge.Rollback
}

// applying is what the sources show of a target's standing deliveries: each
// contradiction, as one the conversation shows, and the rollbacks, as one
// undoing, of the whole result when one says so. A conclusion its source
// corrected later applies neither, and neither does one that conflicts with
// another conclusion about the same target, since the sources do not agree.
// It names the conclusions that apply.
func (r references) applying(t sessionjudge.Target, standing []string, targets []sessionjudge.Target,
	inputs []*sessionjudge.Input) ([]sessionjudge.Contradiction, *sessionjudge.Event, []string) {
	// about holds the conclusions about the target its sources still
	// stand by.
	about := map[string]bool{}
	byID := map[string]sessionjudge.Conclusion{}
	for _, k := range r.conclusions {
		byID[k.ID] = k
		about[k.ID] = k.SupersededBy == "" && bound(k, targets, inputs)[t.ID]
	}
	conflicted := func(id string) bool {
		for _, k := range r.conclusions {
			if !about[k.ID] {
				continue
			}
			if (k.ID == id && slices.ContainsFunc(k.Conflicts, func(o string) bool { return about[o] })) || (k.ID != id && slices.Contains(k.Conflicts, id)) {
				return true
			}
		}
		return false
	}
	var applied []sessionjudge.Contradiction
	var used []string
	for _, d := range r.divergences {
		if d.Verdict != sessionjudge.VerdictContradicted || d.Target != t.ID || !slices.Contains(standing, d.Event) || !about[d.Conclusion] || conflicted(d.Conclusion) {
			continue
		}
		applied = append(applied, sessionjudge.Contradiction{Event: d.Event, Conclusion: d.Whole, Criteria: d.Criteria,
			Text: fmt.Sprintf("conclusion %s: %s", d.Conclusion, byID[d.Conclusion].Statement), Cite: d.Cite})
		used = append(used, d.Conclusion)
	}
	var undone *sessionjudge.Event
	for _, rb := range r.rollbacks {
		if rb.Target != t.ID || !slices.Contains(standing, rb.Event) || !about[rb.Conclusion] || conflicted(rb.Conclusion) {
			continue
		}
		e := &sessionjudge.Event{ID: rb.Event, Kind: sessionjudge.EventUndone, Criteria: rb.Criteria,
			Text: fmt.Sprintf("conclusion %s reports it rolled back: %s", rb.Conclusion, byID[rb.Conclusion].Statement), Cite: rb.Cite}
		undone = joinUndone(undone, e)
		used = append(used, rb.Conclusion)
	}
	return applied, undone, slices.Compact(used)
}

// joinUndone is two undoings as one: of the whole result when either is,
// and of the criteria of both otherwise.
func joinUndone(a, b *sessionjudge.Event) *sessionjudge.Event {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	}
	joined := *a
	if len(a.Criteria) == 0 || len(b.Criteria) == 0 {
		joined.Criteria = nil
	} else {
		joined.Criteria = slices.Compact(slices.Sorted(slices.Values(append(slices.Clone(a.Criteria), b.Criteria...))))
	}
	joined.Text = strings.TrimPrefix(a.Text+"; "+b.Text, "; ")
	joined.Cite = append(slices.Clone(a.Cite), b.Cite...)
	return &joined
}

// recordScore is a target's score from its level and criteria, from 0 to 1:
// achieved 1, not achieved 0, and partly the share of its criteria met, a
// contradicted claim counting as one more not met; half when the number of
// criteria is unknown. A target that cannot be verified has none.
func recordScore(level string, met, criteria, contradicted *int) *float64 {
	var v float64
	switch level {
	case sessionjudge.LevelAchieved:
		v = 1
	case sessionjudge.LevelNotAchieved:
		v = 0
	case sessionjudge.LevelPartly:
		v = 0.5
		if met != nil && criteria != nil && *criteria > 0 {
			against := 0
			if contradicted != nil {
				against = *contradicted
			}
			v = float64(*met) / float64(*criteria+against)
		}
	default:
		return nil
	}
	return &v
}

// finalScore applies to the record's criteria each contradiction of the
// deliveries standing, and their undoing, once. A contradiction of the
// delivered conclusion, or an undoing of the whole result, leaves nothing;
// one of some criteria, or an undoing of them, counts them as not met, and
// the score is taken again from the criteria.
func finalScore(a *sessionjudge.Achievement, applied []sessionjudge.Contradiction, undone *sessionjudge.Event) *float64 {
	notMet := map[string]bool{}
	for _, x := range applied {
		if x.Conclusion {
			return new(float64)
		}
		for _, id := range x.Criteria {
			notMet[id] = true
		}
	}
	if undone != nil {
		if len(undone.Criteria) == 0 {
			return new(float64)
		}
		for _, id := range undone.Criteria {
			notMet[id] = true
		}
	}
	checks := slices.Clone(a.Criteria)
	met := 0
	for i := range checks {
		if notMet[checks[i].ID] {
			checks[i].State = sessionjudge.CheckNotMet
		}
		if checks[i].State == sessionjudge.CheckMet {
			met++
		}
	}
	contradicted := 0
	for _, k := range a.Claims {
		if k.State == sessionjudge.CheckContradicted {
			contradicted++
		}
	}
	n := len(checks)
	return recordScore(levelWith(checks, a.Claims), &met, &n, &contradicted)
}

// reworkCost is what a target's rounds of rework cost: the calls of every
// round, each counted once however many rounds hold it, priced by the label
// the stages gave each call, with the target's share of a shared call and a
// sub-task's own label first, as the target's cost is. A round holds the
// target's calls from its trigger to the last step of its redo: a call is
// recorded before the steps it asked for, so the call that asked for that
// step is held too.
func reworkCost(nav *Nav, l *Ledger, rounds []sessionjudge.Rework, who map[int][]string, target string, w *targetWork) []sessionjudge.ReworkCost {
	var out []sessionjudge.ReworkCost
	var calls [][]int
	index := map[[2]string]int{}
	for _, g := range w.groups {
		for _, i := range g.calls {
			id := l.Calls[i].ID
			if !slices.ContainsFunc(rounds, func(r sessionjudge.Rework) bool {
				return recordedAfter(nav, id, r.From) && recordedAfter(nav, r.To, id)
			}) {
				continue
			}
			k := [2]string{g.work, g.cause}
			at, ok := index[k]
			if !ok {
				at = len(out)
				index[k] = at
				out = append(out, sessionjudge.ReworkCost{Work: g.work, Cause: g.cause})
				calls = append(calls, nil)
			}
			calls[at] = append(calls[at], i)
		}
	}
	for at := range out {
		out[at].Cost = amountFor(l, calls[at], who, target)
	}
	return out
}

// Own records the file operations each target's calls made, as the close
// will give them, so an achievement call's file groups name only those: a
// talk two targets share can hold calls only one of them received.
func (n *Nav) Own(targets []sessionjudge.Target, inputs []*sessionjudge.Input) {
	claims := map[string][]string{}
	for _, t := range targets {
		for _, tk := range t.Talks {
			claims[tk] = append(claims[tk], t.ID)
		}
	}
	opsOf, _ := operationsOf(n, n.ledger, claims, ownersOfCalls(n.ledger, claims, inputs))
	n.owned = map[string]map[string]bool{}
	for t, shares := range opsOf {
		n.owned[t] = map[string]bool{}
		for _, s := range shares {
			n.owned[t][s.ID] = true
		}
	}
}

// operationsOf gives each target the file operations of its calls, with its
// share of a call it shared, and lists the operations no target's calls
// made: those no step joins, and those of calls no target received. A step
// no model call asked for, as some runtimes record, belongs to the targets
// of its talk, split equally.
func operationsOf(nav *Nav, l *Ledger, claims map[string][]string, who map[int][]string) (map[string][]sessionjudge.OperationShare, []string) {
	index := map[string]int{}
	for i, c := range l.Calls {
		index[c.ID] = i
	}
	out := map[string][]sessionjudge.OperationShare{}
	var unassigned []string
	for _, op := range nav.Operations() {
		var ts []string
		if i, ok := index[l.producedBy[op.Step]]; ok {
			ts = who[i]
		} else if op.Step != "" {
			ts = claims[mainTalkOf(nav, op.Step)]
		}
		if len(ts) == 0 {
			unassigned = append(unassigned, op.ID)
			continue
		}
		for _, id := range ts {
			s := sessionjudge.OperationShare{ID: op.ID}
			if len(ts) > 1 {
				share := 1 / float64(len(ts))
				s.Share = &share
			}
			out[id] = append(out[id], s)
		}
	}
	return out, unassigned
}

// mechanicalFindings are the findings asz records itself from what the
// stages decided: each criterion not met, at its first cited evidence; each
// contradiction of a standing delivery, and an undoing, at the event;
// each round of rework at its trigger; and each correction at the person's
// input, unless a round of rework began there. A target the person
// cancelled or replaced was not wanted at the end, so what its result
// missed is no problem; its rework and corrections still are.
func mechanicalFindings(nav *Nav, t sessionjudge.Target, a *sessionjudge.Achievement, scored bool, applied []sessionjudge.Contradiction,
	undone *sessionjudge.Event, corrections []string) []sessionjudge.Finding {
	// Every anchor here was shown by what it cites when its stage was
	// checked, so none is empty.
	var out []sessionjudge.Finding
	add := func(at, problem string, cites []sessionjudge.Cite) {
		f := sessionjudge.Finding{T: sessionjudge.FrameFinding, Target: t.ID, Source: sessionjudge.SourceASZ, At: at, Problem: problem, Cite: cites}
		if n := nav.Node(at); n != nil && n.Ref != nil {
			f.Ref = &sessionjudge.Ref{Seq: n.Ref.Seq, Row: n.Ref.Row, Block: n.Ref.Block}
		}
		out = append(out, f)
	}
	if scored {
		anyStep := func(*cited) bool { return true }
		for _, k := range a.Criteria {
			if k.State == sessionjudge.CheckNotMet {
				add(place(nav, k.Cite, anyStep, true), fmt.Sprintf("criterion %s was not met: %s", k.ID, k.Text), k.Cite)
			}
		}
		at := map[string]string{}
		for _, e := range a.Events {
			at[e.ID] = eventPlace(nav, e.Artifact, e.Cite)
		}
		for _, x := range applied {
			add(at[x.Event], fmt.Sprintf("the delivered result %s was contradicted: %s", x.Event, x.Text), x.Cite)
		}
		if undone != nil {
			add(at[undone.ID], fmt.Sprintf("the delivered result was undone: %s", undone.Text), undone.Cite)
		}
	}
	triggers := map[string]bool{}
	for _, r := range a.Rework {
		triggers[r.From] = true
		add(r.From, fmt.Sprintf("round %s of rework began, triggered by %s, cause %s", r.ID, r.Trigger, r.Cause), r.TriggerCite)
	}
	for _, in := range corrections {
		if !triggers[in] {
			add(in, "the person corrected the target", []sessionjudge.Cite{{Node: in}})
		}
	}
	return out
}

// goalOutcome is how one goal did. Its main target stays only while it is
// still wanted; one cancelled or replaced by the end leaves it
// undetermined. Its worst class is the most severe of its targets', and its
// first finding the one whose step came first in recorded order.
func goalOutcome(nav *Nav, g sessionjudge.Goal, outcome map[string]*sessionjudge.Outcome, found map[string][]sessionjudge.Finding) sessionjudge.GoalOutcome {
	out := sessionjudge.GoalOutcome{Goal: g.ID}
	if o := outcome[g.Main]; o != nil && o.Status != sessionjudge.TargetCancelled && o.Status != sessionjudge.TargetReplaced {
		out.Main, out.ScoreMain = g.Main, o.ScoreFinal
	}
	worst := len(sessionjudge.Classes)
	total, n := 0.0, 0
	for _, m := range g.Members {
		o := outcome[m]
		if o == nil {
			continue
		}
		if i := slices.Index(sessionjudge.Classes, o.Class); i >= 0 && i < worst {
			worst = i
		}
		if o.ScoreFinal != nil {
			total, n = total+*o.ScoreFinal, n+1
		}
		for _, f := range found[m] {
			if out.First == nil || recordedAfter(nav, out.First.At, f.At) {
				first := f
				out.First = &first
			}
		}
	}
	if worst < len(sessionjudge.Classes) {
		out.Worst = sessionjudge.Classes[worst]
	}
	if n > 0 {
		out.ScoreMean = ratio(total, float64(n))
	}
	return out
}
