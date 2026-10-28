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

package main

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/internal/judge"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
)

// runReport is what a closed run's files say of its targets and goals,
// gathered for printing.
type runReport struct {
	targets      []sessionjudge.Target
	goals        []sessionjudge.Goal
	achievements map[string]*sessionjudge.Achievement
	outcomes     map[string]*sessionjudge.Outcome
	findings     map[string][]sessionjudge.Finding
	ops          map[string]sessionjudge.Operation
	goalOut      map[string]sessionjudge.GoalOutcome
	// regroup is the run's latest regroup, whose label view the report
	// follows, nil when the person gave no labels.
	regroup *sessionjudge.File
	// sources, conclusions, divergences and rollbacks are what the
	// reference and divergence stages found.
	sources     []sessionjudge.Source
	conclusions []sessionjudge.Conclusion
	divergences map[string]sessionjudge.Divergence
	rollbacks   []sessionjudge.Rollback
	// unassigned are the file operations no target's calls made.
	unassigned []string
}

func gather(files []*sessionjudge.File, closeFile, regroup *sessionjudge.File) *runReport {
	r := &runReport{achievements: map[string]*sessionjudge.Achievement{}, outcomes: map[string]*sessionjudge.Outcome{},
		findings: map[string][]sessionjudge.Finding{}, ops: map[string]sessionjudge.Operation{}, goalOut: map[string]sessionjudge.GoalOutcome{},
		regroup: regroup, divergences: map[string]sessionjudge.Divergence{}}
	for _, f := range files {
		switch f.Header.Stage {
		case sessionjudge.StageTargets:
			r.targets, r.goals = f.Targets, f.Goals
		case sessionjudge.StageAchievement:
			for i := range f.Achievements {
				a := &f.Achievements[i]
				r.achievements[a.Target] = a
				for _, x := range a.Findings {
					x.Target, x.Source = a.Target, sessionjudge.StageAchievement
					r.findings[a.Target] = append(r.findings[a.Target], x)
				}
			}
		case sessionjudge.StageEfficiency, sessionjudge.StageDivergence:
			for _, x := range f.Findings {
				x.Source = f.Header.Stage
				r.findings[x.Target] = append(r.findings[x.Target], x)
			}
			for _, d := range f.Divergences {
				r.divergences[d.Conclusion] = d
			}
			r.rollbacks = append(r.rollbacks, f.Rollbacks...)
		case sessionjudge.StageReference:
			r.sources, r.conclusions = f.Sources, f.Conclusions
		}
	}
	for i := range closeFile.Outcomes {
		o := &closeFile.Outcomes[i]
		r.outcomes[o.Target] = o
	}
	// A run whose targets stage file cannot be read still has its outcomes,
	// which name every target in order.
	if len(r.targets) == 0 {
		for _, o := range closeFile.Outcomes {
			r.targets = append(r.targets, sessionjudge.Target{ID: o.Target, Statement: o.Statement, Status: o.Status})
		}
	}
	for _, x := range closeFile.Findings {
		r.findings[x.Target] = append(r.findings[x.Target], x)
	}
	for _, op := range closeFile.Operations {
		r.ops[op.ID] = op
	}
	if s := closeFile.Header.Summary; s != nil {
		for _, g := range s.Goals {
			r.goalOut[g.Goal] = g
		}
		r.unassigned = s.UnassignedOperations
	}
	return r
}

// severity orders classes most severe first; a target with none comes last.
func severity(class string) int {
	if i := slices.Index(sessionjudge.Classes, class); i >= 0 {
		return i
	}
	return len(sessionjudge.Classes)
}

// plural prints a count with its noun, one or many.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// scoreOf prints a score out of 100, rounded to a whole number; the stored
// value is not rounded.
func scoreOf(v *float64) string {
	if v == nil {
		return "unavailable"
	}
	return fmt.Sprintf("%.0f/100", math.Round(*v*100))
}

// scores prints a target's final score, with the record's beside it when
// they differ, or why it has none.
func scores(o *sessionjudge.Outcome) string {
	switch {
	case o.Class == sessionjudge.ClassCancelled || o.Class == sessionjudge.ClassReplaced:
		return "not scored, " + o.Class
	case o.ScoreFinal == nil:
		return "score unavailable"
	case o.ScoreRecord != nil && scoreOf(o.ScoreRecord) != scoreOf(o.ScoreFinal):
		return fmt.Sprintf("%s (%s by the record)", scoreOf(o.ScoreFinal), strings.TrimSuffix(scoreOf(o.ScoreRecord), "/100"))
	}
	return scoreOf(o.ScoreFinal)
}

// goalView is one goal as the report prints it: the judge's, with how the
// close found it did, or the person's from a regroup, how it did taken from
// its targets' outcomes the same way, with how it compares with the judge's
// grouping.
type goalView struct {
	id, statement, main, mainReason, worst, class string
	members                                       []string
	evolution                                     []sessionjudge.Evolution
	first                                         *sessionjudge.Finding
	firstOrder                                    string
	// given is how much of the judged work the regroup was given, for a
	// person's goal it found nothing worked on.
	given                *sessionjudge.Given
	scoreMain, scoreMean *float64
	// worked says the conversation worked on one of the person's goals.
	worked bool
}

// goalViews are the goals the report prints: the label view's when a
// regroup was made, the judge's otherwise.
func (r *runReport) goalViews() []goalView {
	var out []goalView
	if r.regroup == nil {
		for _, g := range r.goals {
			o := r.goalOut[g.ID]
			out = append(out, goalView{id: g.ID, statement: g.Statement, main: o.Main, mainReason: g.MainReason, worst: o.Worst,
				members: g.Members, evolution: g.Evolution, first: o.First, firstOrder: o.FirstOrder, scoreMain: o.ScoreMain,
				scoreMean: o.ScoreMean})
		}
		return out
	}
	for _, g := range r.regroup.LabelGoals {
		v := goalView{id: g.ID, statement: g.Statement, main: g.Main, members: g.Members, class: g.Class, worked: g.Worked, given: g.Given}
		worst := len(sessionjudge.Classes)
		total, n := 0.0, 0
		var found []sessionjudge.Finding
		for _, m := range g.Members {
			o := r.outcomes[m]
			if o == nil {
				continue
			}
			worst = min(worst, severity(o.Class))
			if o.ScoreFinal != nil {
				total, n = total+*o.ScoreFinal, n+1
			}
			found = append(found, r.findings[m]...)
		}
		if worst < len(sessionjudge.Classes) {
			v.worst = sessionjudge.Classes[worst]
		}
		if n > 0 {
			mean := total / float64(n)
			v.scoreMean = &mean
		}
		if o := r.outcomes[g.Main]; o != nil {
			v.scoreMain = o.ScoreFinal
		}
		v.first, v.firstOrder = judge.FirstRecorded(found)
		out = append(out, v)
	}
	return out
}

// printGoals prints the run goal by goal, the most severe first, each with
// its targets, and then the targets in no goal. cost prints a target's cost
// lines.
func printGoals(r *runReport, cost func(*sessionjudge.Outcome)) {
	placed := map[string]bool{}
	goals := r.goalViews()
	sort.SliceStable(goals, func(i, j int) bool { return severity(goals[i].worst) < severity(goals[j].worst) })
	for _, g := range goals {
		fmt.Printf("\n  goal %s  %s\n", g.id, trunc(oneLine(g.statement), 100))
		if len(g.evolution) > 1 {
			var steps []string
			for _, e := range g.evolution {
				steps = append(steps, fmt.Sprintf("%q (%s)", trunc(oneLine(e.Statement), 60), e.Input))
			}
			fmt.Printf("    evolved   %s\n", strings.Join(steps, " → "))
		}
		switch g.class {
		case sessionjudge.CompareMissed:
			fmt.Println("    no target: the conversation worked on it, and the judge found no target for it")
		case sessionjudge.CompareAbsent:
			fmt.Printf("    no target: nothing in the conversation worked on it, the evaluator's judgment%s\n", givenText(g.given, "of the judged work"))
		case sessionjudge.CompareUnknown:
			if g.worked {
				fmt.Println("    no target placed: the conversation worked on it, and a target that may serve it could not be placed")
			} else {
				fmt.Println("    no target: the regroup could not tell whether the conversation worked on it")
			}
		}
		if g.worst != "" {
			fmt.Printf("    worst     %s\n", strings.ReplaceAll(g.worst, "_", " "))
		}
		// The finding recorded first in the goal, which says nothing of work
		// before it that no stage read. One whose cause rests on something
		// not happening shows how much of its span the evaluator was given.
		if f := g.first; f != nil {
			note := ""
			switch g.firstOrder {
			case sessionjudge.FirstOrderPartial:
				note = " (among the findings with a recorded place)"
			case sessionjudge.FirstOrderNone:
				note = " (no finding has a recorded place: the first given)"
			}
			fmt.Printf("    first recorded  %s at %s: %s%s\n", f.Source, f.At, trunc(oneLine(f.Problem), 120), note)
			if f.Asserted {
				fmt.Printf("      cause: %s\n", causeOf(*f))
			}
		}
		switch {
		case g.main != "":
			fmt.Printf("    main      %s%s\n", g.main, reasonOf(g.mainReason))
		case len(g.members) > 0:
			fmt.Printf("    main      undetermined%s\n", reasonOf(g.mainReason))
		}
		members := slices.Clone(g.members)
		sort.SliceStable(members, func(i, j int) bool {
			return severity(classOf(r.outcomes[members[i]])) < severity(classOf(r.outcomes[members[j]]))
		})
		var records []float64
		for _, m := range members {
			placed[m] = true
			if o := r.outcomes[m]; o != nil {
				printTarget(r, o, cost)
				if o.ScoreRecord != nil && o.ScoreFinal != nil {
					records = append(records, *o.ScoreRecord)
				}
			}
		}
		printFindings(r, members)
		// A goal with no target has no score.
		if len(g.members) == 0 {
			continue
		}
		line := "main undetermined"
		if g.main != "" {
			line = fmt.Sprintf("main %s %s", g.main, scoreOf(g.scoreMain))
		}
		if g.scoreMean != nil {
			line += fmt.Sprintf("; all %s %s", plural(len(members), "target"), scoreOf(g.scoreMean))
			if len(records) > 0 {
				mean := 0.0
				for _, v := range records {
					mean += v
				}
				mean /= float64(len(records))
				if rec := scoreOf(&mean); rec != scoreOf(g.scoreMean) {
					line += fmt.Sprintf(" (%s by the record)", strings.TrimSuffix(rec, "/100"))
				}
			}
		}
		fmt.Printf("    scores    %s\n", line)
	}
	var loose []string
	for _, t := range r.targets {
		if !placed[t.ID] {
			loose = append(loose, t.ID)
		}
	}
	if len(loose) > 0 {
		if len(goals) > 0 {
			fmt.Println("\n  not in a goal")
		} else {
			fmt.Println()
		}
		for _, id := range loose {
			if o := r.outcomes[id]; o != nil {
				printTarget(r, o, cost)
			}
		}
		printFindings(r, loose)
	}
	if len(r.unassigned) > 0 {
		fmt.Printf("\n  operations no target's calls made\n    %s\n", r.opsLine(r.unassigned, nil))
	}
	printGrouping(r)
	printReferences(r)
}

// unapplied says why what a conclusion shows of a delivery changed no
// score, or nothing when it did: its source corrected it later, the
// delivery no longer stood at the end, or another conclusion about the
// target disagrees with it.
func (r *runReport) unapplied(conclusion, target, event string) string {
	o := r.outcomes[target]
	var by string
	for _, k := range r.conclusions {
		if k.ID == conclusion {
			by = k.SupersededBy
		}
	}
	switch {
	case o == nil:
		return ""
	case by != "":
		return fmt.Sprintf("; it changes no score, since %s corrects it", by)
	case !slices.Contains(o.Standing, event):
		return fmt.Sprintf("; it changes no score, since %s no longer stands", event)
	case !slices.Contains(o.References, conclusion):
		return fmt.Sprintf("; it changes no score, since another conclusion about %s disagrees", target)
	}
	return ""
}

func (r *runReport) statementOf(id string) string {
	for _, k := range r.conclusions {
		if k.ID == id {
			return k.Statement
		}
	}
	return ""
}

// printReferences says what the run was compared with: each source as asz
// saved it, or why it could not be, which leaves its comparison
// unavailable; then each conclusion with how the conversation compared, and
// what it rests on.
func printReferences(r *runReport) {
	if len(r.sources) == 0 {
		return
	}
	fmt.Println("\n  references")
	for _, s := range r.sources {
		if s.Outcome != sessionjudge.SourceSaved {
			fmt.Printf("    %s  %s: not saved, %s; its comparison is unavailable\n", s.ID, s.Source, trunc(oneLine(s.Error), 120))
			continue
		}
		method := s.Method
		if s.MCP != nil {
			method = fmt.Sprintf("mcp %s at %s", s.MCP.Tool, s.MCP.Server)
		}
		line := fmt.Sprintf("    %s  %s: %s, saved %s, %d lines, sha256 %s", s.ID, s.Source, method, s.Captured, s.Lines, s.SHA256[:min(12, len(s.SHA256))])
		if s.QueryRemoved {
			line += ", kept without its query"
		}
		if s.Cut {
			line += ", cut to its start"
		}
		if left := judge.LeftOut(s); left != "" {
			line += "; " + left
		}
		fmt.Println(line)
	}
	for _, k := range r.conclusions {
		d, ok := r.divergences[k.ID]
		verdict := "not compared"
		if ok {
			verdict = strings.ReplaceAll(d.Verdict, "_", " ")
			switch {
			case d.Verdict == sessionjudge.VerdictContradicted && d.Whole:
				verdict += fmt.Sprintf(" %s's delivered conclusion, %s", d.Target, d.Event)
			case d.Verdict == sessionjudge.VerdictContradicted:
				verdict += fmt.Sprintf(" %s of %s, %s", strings.Join(d.Criteria, ", "), d.Target, d.Event)
			case d.Verdict == sessionjudge.VerdictReached:
				verdict += " " + citesOf(d.Cite)
			case d.Asserted:
				verdict += ", the evaluator's judgment" + givenText(d.Given, "of the judged work")
			}
			if d.Verdict == sessionjudge.VerdictContradicted {
				verdict += r.unapplied(k.ID, d.Target, d.Event)
			}
		}
		fmt.Printf("    %s  %-12s %s\n", k.ID, k.Kind, trunc(oneLine(k.Statement), 100))
		fmt.Printf("        %s\n", verdict)
		for _, rb := range r.rollbacks {
			if rb.Conclusion != k.ID {
				continue
			}
			what := "the whole result"
			if len(rb.Criteria) > 0 {
				what = strings.Join(rb.Criteria, ", ")
			}
			fmt.Printf("        rolled back %s of %s, %s, outside the conversation%s\n", what, rb.Target, rb.Event, r.unapplied(k.ID, rb.Target, rb.Event))
		}
		var rests []string
		rests = append(rests, k.Support+" support")
		if len(k.Scope) > 0 {
			rests = append(rests, "about "+strings.Join(k.Scope, ", "))
		} else {
			rests = append(rests, "about no input, so it changes no score")
		}
		if k.SupersededBy != "" {
			rests = append(rests, "superseded by "+k.SupersededBy)
		}
		if len(k.Conflicts) > 0 {
			rests = append(rests, "disagrees with "+strings.Join(k.Conflicts, ", "))
		}
		fmt.Printf("        %s\n", strings.Join(rests, "; "))
	}
}

// printGrouping says whose grouping the report followed, and with a
// regroup, how the judge's compares with the person's goals: each goal's
// class with its targets and the judge's goal each is in, then the targets
// that serve none of the person's goals, and those the regroup could not
// place.
func printGrouping(r *runReport) {
	g := r.regroup
	if g == nil {
		return
	}
	var agreement sessionjudge.Agreement
	if len(g.Agreements) > 0 {
		agreement = g.Agreements[0]
	}
	l := g.Header.Labels
	fmt.Printf("\n  grouping  by the person's goals, %s (sha256 %s), regroup %d; the judge agreed on %d of %s\n",
		l.File, l.SHA256[:min(12, len(l.SHA256))], l.Version, agreement.Classes[sessionjudge.CompareAgree], plural(agreement.Goals, "goal"))
	judgeOf := map[string]string{}
	for _, jg := range r.goals {
		for _, m := range jg.Members {
			judgeOf[m] = jg.ID
		}
	}
	for _, lg := range g.LabelGoals {
		var in []string
		for _, m := range lg.Members {
			where := "in no goal"
			if j := judgeOf[m]; j != "" {
				where = "in " + j
			}
			in = append(in, m+" "+where)
		}
		line := fmt.Sprintf("    %-3s %-8s %s", lg.ID, lg.Class, trunc(oneLine(lg.Statement), 80))
		if len(in) > 0 {
			line += ": " + strings.Join(in, ", ")
		}
		fmt.Println(line)
	}
	if len(agreement.Extra) > 0 {
		fmt.Printf("    serve none of the person's goals: %s\n", strings.Join(agreement.Extra, ", "))
	}
	if len(agreement.Unknown) > 0 {
		fmt.Printf("    not placed: %s\n", strings.Join(agreement.Unknown, ", "))
	}
}

func classOf(o *sessionjudge.Outcome) string {
	if o == nil {
		return ""
	}
	return o.Class
}

func reasonOf(reason string) string {
	if reason == "" {
		return ""
	}
	return ": " + trunc(oneLine(reason), 120)
}

// printTarget prints one target: its scores, how it ended, why, what was
// done, the person's corrections, its rework, what it still misses, and
// what it cost. What the evaluator wrote, such as a reason, a summary or how
// it read a step, is marked as read, apart from what asz checked.
func printTarget(r *runReport, o *sessionjudge.Outcome, cost func(*sessionjudge.Outcome)) {
	fmt.Printf("\n    %s  %-80s %s\n", o.Target, trunc(oneLine(o.Statement), 80), scores(o))
	a := r.achievements[o.Target]
	line := strings.ReplaceAll(o.Class, "_", " ")
	if line == "" {
		line = "not judged"
	}
	if len(o.Standing) > 0 {
		line += "; standing " + strings.Join(o.Standing, ", ")
	}
	if a != nil && len(a.Rework) > 0 {
		line += "; " + plural(len(a.Rework), "round") + " of rework"
	}
	fmt.Printf("        outcome   %s\n", line)
	fmt.Printf("        record    %s\n", verdict(*o))
	if a == nil {
		cost(o)
		return
	}
	var met, missing []sessionjudge.Check
	for _, k := range a.Criteria {
		if k.State == sessionjudge.CheckMet {
			met = append(met, k)
		} else {
			missing = append(missing, k)
		}
	}
	for i, k := range met {
		label := "why"
		if i > 0 {
			label = ""
		}
		printCheck(label, k)
	}
	for i, k := range a.Claims {
		label := "claims"
		if i > 0 {
			label = ""
		}
		fmt.Printf("        %-9s %s: %s %s\n", label, strings.ReplaceAll(k.State, "_", " "), trunc(oneLine(k.Text), 100), citesOf(k.Cite))
	}
	printHistory(a, o)
	for _, x := range a.Contradictions {
		if slices.Contains(o.Standing, x.Event) {
			fmt.Printf("        contradicted  %s, by %s: %s\n", disproved(x), citesOf(x.Cite), trunc(oneLine(x.Text), 120))
		}
	}
	// What a source shows of a standing delivery, which applies to the
	// final score as what the conversation shows does.
	for _, id := range o.References {
		if d, ok := r.divergences[id]; ok && d.Verdict == sessionjudge.VerdictContradicted && d.Target == o.Target {
			what := "the delivered conclusion"
			if !d.Whole {
				what = strings.Join(d.Criteria, ", ")
			}
			fmt.Printf("        reference %s contradicts %s of %s: %s\n", id, what, d.Event, trunc(oneLine(r.statementOf(id)), 120))
		}
		for _, rb := range r.rollbacks {
			if rb.Conclusion == id && rb.Target == o.Target {
				what := "the whole result"
				if len(rb.Criteria) > 0 {
					what = strings.Join(rb.Criteria, ", ")
				}
				fmt.Printf("        reference %s rolled back %s of %s, outside the conversation: %s\n", id, what, rb.Event, trunc(oneLine(r.statementOf(id)), 120))
			}
		}
	}
	// The undoings that apply: those that took back a delivery still
	// standing.
	for _, e := range a.Events {
		if !slices.Contains(o.Undone, e.ID) {
			continue
		}
		what := "the whole result"
		if len(e.Criteria) > 0 {
			what = strings.Join(e.Criteria, ", ")
		}
		fmt.Printf("        undone    %s, by %s: %s\n", what, citesOf(e.Cite), trunc(oneLine(e.Text), 120))
	}
	printAccount(r, a, o)
	if len(o.Corrections) > 0 {
		fmt.Printf("        person    corrected it at %s\n", strings.Join(o.Corrections, ", "))
	}
	for i, w := range a.Rework {
		label := "rework"
		if i > 0 {
			label = ""
		}
		state := "finished"
		if !w.Finished {
			state = "not finished"
		}
		fmt.Printf("        %-9s %s, %s, %s: from %s to %s; %s%s\n", label, w.ID, w.Trigger, strings.ReplaceAll(w.Cause, "_", " "), w.From, w.To, state, readAs(w.Note))
	}
	// What the rounds cost, each call once, by how the stages labeled it.
	if len(o.Rework) > 0 {
		var parts []string
		for _, c := range o.Rework {
			label := strings.ReplaceAll(c.Work, "_", " ")
			switch c.Cause {
			case "", sessionjudge.CauseMain:
			case sessionjudge.CauseUnknown:
				label += ", cause unknown"
			default:
				label += ", by " + c.Cause
			}
			parts = append(parts, fmt.Sprintf("%s %s tokens", label, tokensOf(float64(c.Cost.Tokens()))))
		}
		fmt.Printf("                  cost: %s\n", strings.Join(parts, "; "))
	}
	for i, k := range missing {
		label := "gap"
		if i > 0 {
			label = ""
		}
		printCheck(label, k)
	}
	cost(o)
}

// printHistory prints a target's deliveries in recorded order, each with
// the contradictions of it that no longer apply, since a later delivery
// replaced it. What stands at the end is printed apart, as what applies.
func printHistory(a *sessionjudge.Achievement, o *sessionjudge.Outcome) {
	for i, e := range a.Events {
		label := "history"
		if i > 0 {
			label = ""
		}
		line := e.ID + " " + strings.ReplaceAll(e.Kind, "_", " ")
		switch {
		case e.Kind == sessionjudge.EventUndone && len(e.Criteria) > 0:
			line += " " + strings.Join(e.Criteria, ", ")
		case e.Kind == sessionjudge.EventUndone:
			line += " the whole result"
		case len(e.Replaces) > 0:
			line += ", replacing " + strings.Join(e.Replaces, ", ")
		}
		if len(e.Undoes) > 0 {
			line += " of " + strings.Join(e.Undoes, ", ")
		}
		if e.Artifact != "" {
			line += " (" + e.Artifact + ")"
		}
		if len(e.Cite) > 0 {
			line += " " + citesOf(e.Cite)
		}
		if e.Text != "" {
			line += ": " + trunc(oneLine(e.Text), 100)
		}
		fmt.Printf("        %-9s %s\n", label, line)
		for _, x := range a.Contradictions {
			if x.Event == e.ID && !slices.Contains(o.Standing, e.ID) {
				fmt.Printf("                    contradicted %s, by %s: %s; replaced since\n", disproved(x), citesOf(x.Cite), trunc(oneLine(x.Text), 100))
			}
		}
	}
}

// disproved says what a contradiction shows wrong: the delivered conclusion,
// or some criteria.
func disproved(x sessionjudge.Contradiction) string {
	if x.Conclusion {
		return "the delivered conclusion"
	}
	return strings.Join(x.Criteria, ", ")
}

// printCheck prints one criterion: its state and reason, asz's when asz
// changed the state with the evaluator's beside it, then what its steps
// show.
func printCheck(label string, k sessionjudge.Check) {
	state := strings.ReplaceAll(k.State, "_", " ")
	fmt.Printf("        %-9s %s %s: %s\n", label, k.ID, state, trunc(oneLine(k.Text), 80))
	if k.GivenReason != "" {
		fmt.Printf("                    asz: %s; the evaluator read: %s\n", k.Reason, trunc(oneLine(k.GivenReason), 120))
	} else if k.Reason != "" {
		fmt.Printf("                    read: %s\n", trunc(oneLine(k.Reason), 140))
	}
	for _, ob := range k.Observations {
		status := ""
		if ob.Status != "" {
			status = " " + strings.ReplaceAll(ob.Status, "_", " ")
		}
		excerpt := ""
		if ob.Excerpt != "" {
			excerpt = fmt.Sprintf(" %q", trunc(oneLine(ob.Excerpt), 80))
		}
		fmt.Printf("                    [%s%s]%s%s\n", ob.Node, status, excerpt, readAs(ob.Interpretation))
	}
}

// readAs marks text the evaluator wrote, apart from what asz checked.
func readAs(text string) string {
	if text == "" {
		return ""
	}
	return " — read: " + trunc(oneLine(text), 120)
}

func citesOf(cites []sessionjudge.Cite) string {
	var ids []string
	for _, c := range cites {
		ids = append(ids, c.Node)
	}
	if len(ids) == 0 {
		return "nothing cited"
	}
	return "[" + strings.Join(ids, " ") + "]"
}

// printAccount prints what was done for a target, by what it was for: an
// answer, the files it changed, grouped as the evaluator grouped them with
// the rest listed apart, and its changes to other systems. A target that
// delivered files the record names none of says so, rather than nothing.
func printAccount(r *runReport, a *sessionjudge.Achievement, o *sessionjudge.Outcome) {
	acc := a.Account
	var lines []string
	if acc != nil && acc.Answer != nil {
		an := acc.Answer
		lines = append(lines, "answer")
		lines = append(lines, fmt.Sprintf("  asked      %s %s", trunc(oneLine(an.Asked.Text), 100), citesOf(an.Asked.Cite)))
		for _, x := range an.Checked {
			lines = append(lines, fmt.Sprintf("  checked    %s: %s %s%s", trunc(oneLine(x.What), 60), trunc(oneLine(x.Found), 80), citesOf(x.Cite), limitsOf(x.Limits)))
		}
		for _, x := range an.RuledOut {
			lines = append(lines, fmt.Sprintf("  ruled out  %s: %s %s%s", trunc(oneLine(x.What), 60), trunc(oneLine(x.Found), 80), citesOf(x.Cite), limitsOf(x.Limits)))
		}
		lines = append(lines, fmt.Sprintf("  answered   %s %s", trunc(oneLine(an.Answered.Text), 100), citesOf(an.Answered.Cite)))
	}
	share := map[string]*float64{}
	for _, s := range o.Operations {
		share[s.ID] = s.Share
	}
	if len(o.Operations) > 0 {
		lines = append(lines, "files")
		grouped := map[string]bool{}
		if acc != nil {
			for _, g := range acc.Files {
				lines = append(lines, fmt.Sprintf("  %s, %s: %s", g.Area, g.Category, trunc(oneLine(g.Summary), 100)))
				lines = append(lines, "    "+r.opsLine(g.Operations, share))
				for _, id := range g.Operations {
					grouped[id] = true
				}
			}
		}
		var rest []string
		for _, s := range o.Operations {
			if !grouped[s.ID] {
				rest = append(rest, s.ID)
			}
		}
		if len(rest) > 0 {
			label := "  not grouped: "
			if len(grouped) == 0 {
				label = "  "
			}
			lines = append(lines, label+r.opsLine(rest, share))
		}
	}
	if len(o.Operations) == 0 && slices.ContainsFunc(a.Events, func(e sessionjudge.Event) bool {
		return e.Artifact == sessionjudge.ArtifactFile || e.Artifact == sessionjudge.ArtifactRevision
	}) {
		lines = append(lines, "files", "  none recorded: no change record captured them, and no editing tool's request named them")
	}
	if acc != nil && len(acc.Remote) > 0 {
		lines = append(lines, "remote")
		for _, x := range acc.Remote {
			l := fmt.Sprintf("  %s: %s via %s, sent %s", x.System, trunc(oneLine(x.Operation), 80), x.Via, citesOf(x.Request.Cite))
			if x.Response != nil {
				l += ", answered " + citesOf(x.Response.Cite)
			}
			if x.Confirmed != nil {
				l += ", confirmed " + citesOf(x.Confirmed.Cite)
			} else {
				l += ", not confirmed"
			}
			lines = append(lines, l)
		}
	}
	for i, l := range lines {
		label := "did"
		if i > 0 {
			label = ""
		}
		fmt.Printf("        %-9s %s\n", label, l)
	}
}

// givenText says how much of an absence claim's steps the evaluator was
// given, beside the claim: what it asked for and was answered, what it saw
// only named in a list, and what it was not given. It never says the claim
// holds. A run made before readings were kept has none.
func givenText(g *sessionjudge.Given, where string) string {
	switch {
	case g == nil:
		return "; what it was given was not recorded"
	case g.Counts == nil:
		return "; what it was given is unavailable: " + g.Reason
	}
	c := g.Counts
	text := fmt.Sprintf("; of the %s %s it opened %d, saw %d only in lists and was not given %d", plural(c.Steps, "step"), where, c.Opened, c.Listed, c.None)
	if c.Unavailable > 0 {
		text += fmt.Sprintf("; %d hold evidence no one can be given", c.Unavailable)
	}
	return text
}

// causeOf says a finding's cause and the conclusion it is against. A cause
// that rests on something not happening is the evaluator's judgment, said
// with how much of its span the evaluator was given.
func causeOf(f sessionjudge.Finding) string {
	line := strings.ReplaceAll(f.Cause, "_", " ")
	if f.Conclusion != "" {
		line += ", against " + f.Conclusion
	}
	if f.Asserted {
		line += "; the evaluator's judgment, since the record cannot show it was looked for everywhere" +
			givenText(f.Given, fmt.Sprintf("from %s to %s", f.From, f.At))
	}
	return line
}

func limitsOf(limits string) string {
	if limits == "" {
		return ""
	}
	return "; holds for: " + trunc(oneLine(limits), 80)
}

// opsLine prints some operations by path and what happened, at most a dozen,
// with their ids. A request names an attempt, so it says whether its tool
// returned; a share says the call was shared.
func (r *runReport) opsLine(ids []string, share map[string]*float64) string {
	var parts []string
	for i, id := range ids {
		if i == 12 {
			parts = append(parts, fmt.Sprintf("and %d more", len(ids)-12))
			break
		}
		op, ok := r.ops[id]
		if !ok {
			parts = append(parts, id)
			continue
		}
		path := op.Path
		if path == "" {
			path = "files not listed"
		}
		what := op.Operation
		switch {
		case op.Source == sessionjudge.SourceToolRequest:
			what += ", " + strings.ReplaceAll(op.Status, "_", " ")
		case op.Added != nil && op.Removed != nil:
			what += fmt.Sprintf(" +%d -%d", *op.Added, *op.Removed)
		}
		if s := share[id]; s != nil {
			what += fmt.Sprintf(", share %.2g", *s)
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", path, what))
	}
	return strings.Join(parts, ", ") + " [" + strings.Join(ids, " ") + "]"
}

// printFindings prints the findings of some targets in the order their
// steps landed, those whose place is unknown last. An explanation and an
// improvement are proposals.
func printFindings(r *runReport, targets []string) {
	var all []sessionjudge.Finding
	for _, t := range targets {
		all = append(all, r.findings[t]...)
	}
	if len(all) == 0 {
		return
	}
	sort.SliceStable(all, func(i, j int) bool { return refLess(all[i].Ref, all[j].Ref) })
	fmt.Println("\n    findings")
	for _, f := range all {
		fmt.Printf("      %s at %s, %s: %s\n", f.Target, f.At, f.Source, trunc(oneLine(f.Problem), 120))
		if f.Cause != "" {
			fmt.Printf("        cause: %s\n", causeOf(f))
		}
		if f.Explanation != "" {
			fmt.Printf("        why, proposed: %s\n", trunc(oneLine(f.Explanation), 120))
		}
		if f.Improvement != "" {
			fmt.Printf("        improvement, proposed: %s\n", trunc(oneLine(f.Improvement), 120))
		}
	}
}

func refLess(a, b *sessionjudge.Ref) bool {
	switch {
	case a == nil:
		return false
	case b == nil:
		return true
	case a.Seq != b.Seq:
		return a.Seq < b.Seq
	case a.Row != b.Row:
		return a.Row < b.Row
	}
	block := func(r *sessionjudge.Ref) int {
		if r.Block == nil {
			return -1
		}
		return *r.Block
	}
	return block(a) < block(b)
}
