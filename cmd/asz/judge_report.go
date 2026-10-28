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
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/internal/judge"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
)

// runReport is what a closed run's files say of its targets and goals,
// gathered for printing.
type runReport struct {
	conversation string
	targets      []sessionjudge.Target
	goals        []sessionjudge.Goal
	achievements map[string]*sessionjudge.Achievement
	outcomes     map[string]*sessionjudge.Outcome
	findings     map[string][]sessionjudge.Finding
	ops          map[string]sessionjudge.Operation
	// regroup is the run's latest regroup, whose grouping by the person's
	// goals the report follows, nil when the person gave no goals.
	regroup *sessionjudge.File
	// sources, conclusions, divergences and rollbacks are what the
	// reference and divergence stages found.
	sources     []sessionjudge.Source
	conclusions []sessionjudge.Conclusion
	divergences map[string]sessionjudge.Divergence
	rollbacks   []sessionjudge.Rollback
	// headers are the stage headers of the run and of the regroups shown with
	// it, in the order given, and subtasks the sub-task stage's file.
	headers  []sessionjudge.Header
	subtasks *sessionjudge.File
	summary  *sessionjudge.Summary
	// unassigned are the file operations no target's calls made.
	unassigned []string
}

func gather(files []*sessionjudge.File, closeFile, regroup *sessionjudge.File) *runReport {
	r := &runReport{achievements: map[string]*sessionjudge.Achievement{}, outcomes: map[string]*sessionjudge.Outcome{},
		findings: map[string][]sessionjudge.Finding{}, ops: map[string]sessionjudge.Operation{},
		regroup: regroup, divergences: map[string]sessionjudge.Divergence{}, summary: closeFile.Header.Summary}
	if r.summary == nil {
		r.summary = &sessionjudge.Summary{}
	}
	for _, f := range files {
		if r.conversation == "" {
			r.conversation = f.Header.Conversation
		}
		r.headers = append(r.headers, f.Header)
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
		case sessionjudge.StageSubtasks:
			r.subtasks = f
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
	for _, op := range closeFile.Operations {
		r.ops[op.ID] = op
	}
	r.unassigned = r.summary.UnassignedOperations
	return r
}

// reportDoc is a run as a person reads it, whatever it is written as: goal
// by goal, each target's status, score, criteria, what was done and what is
// missing; then the overall assessment and the overall result. It names
// nothing by an id or a step's address: those, the steps each line rests on
// and what each target cost are in -json.
type reportDoc struct {
	run          int
	conversation string
	// snapshot names a conversation pulled from another asz as it was asked
	// for; the run's files name it by the source's own identifier.
	snapshot string
	// closed is false for a run with no close yet, which shows nothing else.
	closed     bool
	goals      []goalSection
	assessment []string
	result     []string
	// tally counts the targets by how they ended, most severe first, and
	// final is the run's final score, for the head of a page.
	tally      []tallyItem
	final      string
	finalValue *float64
}

type tallyItem struct{ class, text string }

// goalSection is one goal, or the targets in none: what it is called, and
// for one of the person's goals no target serves, what is known of it.
type goalSection struct {
	label, statement string
	// class is how the person's goal no target serves compares, and status
	// and score what is known of it.
	class, status, score string
	targets              []targetView
}

// targetView is one target as the report says it: how it ended, in words
// and why, its score, its criteria, what was done and what is missing.
// final and record are its two scores, for a page to color.
type targetView struct {
	statement     string
	main          bool
	class         string
	words, why    string
	whySteps      []string
	score         string
	final, record *float64
	criteria      []criterionView
	done, missing []item
}

// item is one line of a list, with the steps of the conversation it rests
// on, which a page links to.
type item struct {
	text  string
	steps []string
}

// criterionView is one criterion: its state, what contradicted or undid it
// since, the reason given for its state, and the steps that show it.
type criterionView struct {
	text, state string
	against     []item
	reason      string
	evidence    []evidenceView
}

func (c criterionView) line() string {
	line := c.text + ": " + c.state
	for _, x := range c.against {
		line += "; " + x.text
	}
	return line
}

// evidenceView is one step a criterion's state rests on: what the step
// holds, as an excerpt, and what the evaluator read in it.
type evidenceView struct {
	step, excerpt, reading string
}

func nodesOf(cites []sessionjudge.Cite) []string {
	var out []string
	for _, c := range cites {
		out = append(out, c.Node)
	}
	return out
}

func (t targetView) status() string {
	if t.why == "" {
		return t.words
	}
	return t.words + ": " + t.why
}

func buildReport(run storage.JudgmentRun, files []*sessionjudge.File) reportDoc {
	doc := reportDoc{run: run.Run}
	var closeFile, regroup *sessionjudge.File
	for _, f := range files {
		if doc.conversation == "" {
			doc.conversation = f.Header.Conversation
		}
		switch f.Header.Stage {
		case sessionjudge.StageClose:
			closeFile = f
		case sessionjudge.StageRegroup:
			if f.Header.Status != sessionjudge.StatusFailed {
				regroup = f
			}
		}
	}
	if closeFile == nil {
		return doc
	}
	r := gather(files, closeFile, regroup)
	doc.closed = true
	doc.goals, doc.assessment, doc.result = r.sections(), r.assessment(), r.result()
	counts := map[string]int{}
	for _, o := range r.outcomes {
		counts[o.Class]++
	}
	for _, c := range sessionjudge.Classes {
		if n := counts[c]; n > 0 {
			doc.tally = append(doc.tally, tallyItem{class: c, text: fmt.Sprintf("%d %s", n, classWords[c])})
		}
	}
	if sc := r.summary.Score; sc != nil && sc.Final != nil {
		doc.final, doc.finalValue = scoreWords100(sc.Final), sc.Final
	}
	return doc
}

// printRun prints a run as a tree, for a terminal.
func printRun(run storage.JudgmentRun, files []*sessionjudge.File) {
	writeText(os.Stdout, buildReport(run, files))
}

// goalView is one goal as the report prints it: the judge's, or the
// person's from a regroup, with how it compares with the judge's grouping.
// A goal only groups targets: how each target did is printed with it.
type goalView struct {
	statement, main, class string
	members                []string
	// given is how much of the judged work the regroup was given, for a
	// person's goal it found nothing worked on.
	given *sessionjudge.Given
	// worked says the conversation worked on one of the person's goals.
	worked bool
}

// goalViews are the goals the report prints, in the order of their ids: the
// person's when a regroup was made, the judge's otherwise.
func (r *runReport) goalViews() []goalView {
	var out []goalView
	if r.regroup == nil {
		for _, g := range r.goals {
			v := goalView{statement: g.Statement, main: g.Main, members: g.Members}
			// A main target the person no longer wanted by the end leaves the
			// goal's main undetermined, as a regroup's main target is only
			// one still wanted.
			if o := r.outcomes[g.Main]; o != nil && (o.Status == sessionjudge.TargetCancelled || o.Status == sessionjudge.TargetReplaced) {
				v.main = ""
			}
			out = append(out, v)
		}
		return out
	}
	for _, g := range r.regroup.Goals {
		out = append(out, goalView{statement: g.Statement, main: g.Main, members: g.Members, class: g.Class, worked: g.Worked, given: g.Given})
	}
	return out
}

// sections are the run's goals, each with its targets, the most severe
// outcome first, and then the targets in no goal.
func (r *runReport) sections() []goalSection {
	placed := map[string]bool{}
	goals := r.goalViews()
	var out []goalSection
	for i, g := range goals {
		sec := goalSection{label: fmt.Sprintf("goal %d", i+1), statement: oneLine(g.statement)}
		if len(g.members) == 0 {
			sec.class, sec.status, sec.score = g.class, noTarget(g), "none"
		}
		members := slices.Clone(g.members)
		sort.SliceStable(members, func(i, j int) bool {
			return severity(classOf(r.outcomes[members[i]])) < severity(classOf(r.outcomes[members[j]]))
		})
		for _, m := range members {
			placed[m] = true
			if o := r.outcomes[m]; o != nil {
				sec.targets = append(sec.targets, r.viewOf(o, m == g.main && len(members) > 1))
			}
		}
		out = append(out, sec)
	}
	loose := goalSection{label: "not in a goal"}
	if len(goals) == 0 {
		loose.label = "targets"
	}
	for _, t := range r.targets {
		if o := r.outcomes[t.ID]; o != nil && !placed[t.ID] {
			loose.targets = append(loose.targets, r.viewOf(o, false))
		}
	}
	if len(loose.targets) > 0 {
		out = append(out, loose)
	}
	return out
}

// noTarget says what is known of one of the person's goals that no target
// serves.
func noTarget(g goalView) string {
	switch {
	case g.class == sessionjudge.CompareMissed:
		return "worked on, but the judge found no target for it"
	case g.class == sessionjudge.CompareAbsent:
		return "not worked on, the evaluator's judgment" + givenWords(g.given)
	case g.class == sessionjudge.CompareUnknown && g.worked:
		return "worked on, and a target that may serve it could not be placed"
	case g.class == sessionjudge.CompareUnknown:
		return "the regroup could not tell whether it was worked on"
	}
	return "no target"
}

// viewOf is one target: its status, its score, its criteria, what was done
// for it and what it is missing.
func (r *runReport) viewOf(o *sessionjudge.Outcome, main bool) targetView {
	a := r.achievements[o.Target]
	t := targetView{statement: oneLine(o.Statement), main: main, class: o.Class, score: scoreWords(o), final: o.ScoreFinal, record: o.ScoreRecord,
		done: r.done(o, a), missing: r.missing(o, a)}
	if o.Class == sessionjudge.ClassCancelled || o.Class == sessionjudge.ClassReplaced {
		t.final, t.record = nil, nil
	}
	t.words, t.why, t.whySteps = r.status(o, a)
	if a != nil {
		t.criteria = r.criteria(o, a)
	}
	return t
}

// classWords says each class in words.
var classWords = map[string]string{sessionjudge.ClassAchieved: "achieved", sessionjudge.ClassPartly: "partly achieved",
	sessionjudge.ClassNotAchieved: "not achieved", sessionjudge.ClassContradicted: "contradicted", sessionjudge.ClassReverted: "reverted",
	sessionjudge.ClassCannotVerify: "cannot be verified", sessionjudge.ClassCancelled: "cancelled", sessionjudge.ClassReplaced: "replaced"}

// status says how a target ended, in words, and why in one sentence, with
// the steps that show why.
func (r *runReport) status(o *sessionjudge.Outcome, a *sessionjudge.Achievement) (words, why string, steps []string) {
	per, whole := r.against(o, a)
	words = classWords[o.Class]
	switch o.Class {
	case sessionjudge.ClassAchieved:
		return words, basisWords(o.Basis), nil
	case sessionjudge.ClassPartly, sessionjudge.ClassNotAchieved:
		return words, metWords(o), nil
	case sessionjudge.ClassCannotVerify:
		return words, "nothing shows the result either way", nil
	case sessionjudge.ClassContradicted, sessionjudge.ClassReverted:
		if len(whole) == 0 && len(per) == 1 {
			return words, "1 criterion contradicted or undone", nil
		}
		if len(whole) == 0 && len(per) > 1 {
			return words, fmt.Sprintf("%d criteria contradicted or undone", len(per)), nil
		}
		var texts []string
		for _, x := range whole {
			texts = append(texts, x.text)
			steps = append(steps, x.steps...)
		}
		return words, strings.Join(texts, "; "), steps
	case sessionjudge.ClassCancelled:
		return words, "the person no longer wanted it, so it is not scored", nil
	case sessionjudge.ClassReplaced:
		return words, "another target took its place, so it is not scored", nil
	}
	return "not judged", "", nil
}

func basisWords(basis string) string {
	switch basis {
	case sessionjudge.BasisAccepted:
		return "the person accepted the result"
	case sessionjudge.BasisObserved:
		return "the conversation shows the result"
	case sessionjudge.BasisDelivered:
		return "the agent delivered the answer"
	case sessionjudge.BasisClaimOnly:
		return "only the agent's own words show it"
	}
	return ""
}

func metWords(o *sessionjudge.Outcome) string {
	if o.Met == nil || o.Criteria == nil {
		return "its criteria are unknown"
	}
	return fmt.Sprintf("%d of %d criteria met", *o.Met, *o.Criteria)
}

// against is what contradicted or undid a target's result standing at the
// end, each said once: per criterion, and for the whole result. Each comes
// with the steps that show it: those the conversation's contradiction or
// undoing cites, and for a source's, the delivery it is about.
func (r *runReport) against(o *sessionjudge.Outcome, a *sessionjudge.Achievement) (map[string][]item, []item) {
	per := map[string][]item{}
	var whole []item
	add := func(criteria []string, x item) {
		if len(criteria) == 0 {
			whole = append(whole, x)
			return
		}
		for _, c := range criteria {
			per[c] = append(per[c], x)
		}
	}
	delivery := func(event string) []string {
		if a == nil {
			return nil
		}
		for _, e := range a.Events {
			if e.ID == event {
				return nodesOf(e.Cite)
			}
		}
		return nil
	}
	if a != nil {
		for _, x := range a.Contradictions {
			if !slices.Contains(o.Standing, x.Event) {
				continue
			}
			criteria := x.Criteria
			if x.Conclusion {
				criteria = nil
			}
			add(criteria, item{"later in the conversation, " + trunc(oneLine(x.Text), 200), nodesOf(x.Cite)})
		}
		for _, e := range a.Events {
			if slices.Contains(o.Undone, e.ID) {
				add(e.Criteria, item{"undone in the conversation, " + trunc(oneLine(e.Text), 200), nodesOf(e.Cite)})
			}
		}
	}
	for _, id := range o.References {
		if d, ok := r.divergences[id]; ok && d.Verdict == sessionjudge.VerdictContradicted && d.Target == o.Target {
			criteria := d.Criteria
			if d.Whole {
				criteria = nil
			}
			add(criteria, item{"the reference says " + trunc(oneLine(r.statementOf(id)), 200), delivery(d.Event)})
		}
		for _, rb := range r.rollbacks {
			if rb.Conclusion == id && rb.Target == o.Target {
				add(rb.Criteria, item{"undone outside the conversation: " + trunc(oneLine(r.statementOf(id)), 200), delivery(rb.Event)})
			}
		}
	}
	return per, whole
}

func (r *runReport) statementOf(id string) string {
	for _, k := range r.conclusions {
		if k.ID == id {
			return k.Statement
		}
	}
	return ""
}

// criteria lists a target's criteria, each with its state, what
// contradicted or undid it since, and what shows it.
func (r *runReport) criteria(o *sessionjudge.Outcome, a *sessionjudge.Achievement) []criterionView {
	per, _ := r.against(o, a)
	var out []criterionView
	for _, k := range a.Criteria {
		c := criterionView{text: trunc(oneLine(k.Text), 160), state: strings.ReplaceAll(k.State, "_", " "), against: per[k.ID], reason: oneLine(k.Reason)}
		for _, ob := range k.Observations {
			c.evidence = append(c.evidence, evidenceView{step: ob.Node, excerpt: oneLine(ob.Excerpt), reading: oneLine(ob.Interpretation)})
		}
		out = append(out, c)
	}
	return out
}

// done lists what was done for a target, by what it was for: what an answer
// checked, ruled out and answered, the files changed, and changes to other
// systems.
func (r *runReport) done(o *sessionjudge.Outcome, a *sessionjudge.Achievement) []item {
	if a == nil {
		return nil
	}
	var out []item
	acc := a.Account
	answer := acc != nil && acc.Answer != nil
	// What was delivered, in order; an answer's delivery is said below with
	// what the answer checked.
	for _, e := range a.Events {
		if e.Kind == sessionjudge.EventUndone || e.Text == "" || answer && e.Artifact == sessionjudge.ArtifactAnswer {
			continue
		}
		what := "delivered"
		if e.Kind == sessionjudge.EventDeliveredAgain {
			what = "delivered again"
		}
		if e.Artifact != "" {
			what += " " + strings.ReplaceAll(e.Artifact, "_", " ")
		}
		out = append(out, item{what + ": " + trunc(oneLine(e.Text), 200), nodesOf(e.Cite)})
	}
	if answer {
		an := acc.Answer
		for _, x := range an.Checked {
			out = append(out, item{trunc(strings.TrimSuffix(oneLine(x.What), "."), 140) + ": " + trunc(oneLine(x.Found), 200), nodesOf(x.Cite)})
		}
		for _, x := range an.RuledOut {
			out = append(out, item{"ruled out " + trunc(strings.TrimSuffix(oneLine(x.What), "."), 140) + ": " + trunc(oneLine(x.Found), 200), nodesOf(x.Cite)})
		}
		if an.Answered.Text != "" {
			out = append(out, item{"answered: " + trunc(oneLine(an.Answered.Text), 200), nodesOf(an.Answered.Cite)})
		}
	}
	grouped := map[string]bool{}
	if acc != nil {
		for _, g := range acc.Files {
			out = append(out, item{text: fmt.Sprintf("%s, %s: %s\n%s", g.Area, g.Category, trunc(oneLine(g.Summary), 120), r.paths(g.Operations))})
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
		out = append(out, item{text: "files: " + r.paths(rest)})
	}
	if len(o.Operations) == 0 && slices.ContainsFunc(a.Events, func(e sessionjudge.Event) bool {
		return e.Artifact == sessionjudge.ArtifactFile || e.Artifact == sessionjudge.ArtifactRevision
	}) {
		out = append(out, item{text: "changed files that no change record or editing tool names"})
	}
	if acc != nil {
		for _, x := range acc.Remote {
			line := fmt.Sprintf("%s on %s through %s", trunc(oneLine(x.Operation), 100), x.System, x.Via)
			if x.Confirmed != nil {
				line += ", confirmed"
			} else {
				line += ", not confirmed"
			}
			out = append(out, item{line, nodesOf(x.Request.Cite)})
		}
	}
	return out
}

// paths names some file operations, at most five, each with what happened:
// a change a record captured says how much changed, and a request of an
// editing tool says whether the tool returned, since a request is only an
// attempt.
func (r *runReport) paths(ids []string) string {
	var names []string
	for i, id := range ids {
		if i == 5 {
			names = append(names, fmt.Sprintf("and %d more", len(ids)-5))
			break
		}
		op, ok := r.ops[id]
		if !ok {
			continue
		}
		path := op.Path
		if path == "" {
			path = "a file not named"
		}
		what := op.Operation
		switch {
		case op.Source == sessionjudge.SourceToolRequest:
			what += ", requested, the tool " + toolWords(op.Status)
		case op.Added != nil && op.Removed != nil:
			what += fmt.Sprintf(" +%d -%d", *op.Added, *op.Removed)
		}
		names = append(names, path+" ("+what+")")
	}
	if len(names) == 0 {
		return plural(len(ids), "file operation")
	}
	return strings.Join(names, ", ")
}

// toolWords says what became of a request to an editing tool.
func toolWords(status string) string {
	switch status {
	case sessionjudge.ObservedReturned:
		return "returned"
	case sessionjudge.ObservedNoResult:
		return "never returned"
	case sessionjudge.ObservedFailed, sessionjudge.ObservedError:
		return "failed"
	}
	return strings.ReplaceAll(status, "_", " ")
}

// missing lists what a target lacks: the criteria not met, the claims the
// conversation contradicts, each round of rework, the person's
// corrections, the problems the stages found and the avoidable work. asz's
// own findings are left out, since each repeats one of these or the status.
func (r *runReport) missing(o *sessionjudge.Outcome, a *sessionjudge.Achievement) []item {
	var out []item
	if a != nil {
		per, whole := r.against(o, a)
		// What contradicted or undid the whole result is missing first: it
		// leaves nothing of the result, whatever its criteria say.
		for _, x := range whole {
			out = append(out, item{"the whole result: " + x.text, x.steps})
		}
		for _, k := range a.Criteria {
			// A criterion met and then contradicted or undone counts as not met.
			if k.State == sessionjudge.CheckMet && len(per[k.ID]) > 0 {
				line := trunc(oneLine(k.Text), 160) + ": met, then"
				var steps []string
				for i, x := range per[k.ID] {
					if i > 0 {
						line += ";"
					}
					line += " " + x.text
					steps = append(steps, x.steps...)
				}
				out = append(out, item{line, steps})
				continue
			}
			if k.State == sessionjudge.CheckMet {
				continue
			}
			line := trunc(oneLine(k.Text), 160) + ": " + strings.ReplaceAll(k.State, "_", " ")
			if k.Reason != "" {
				line += ", " + trunc(oneLine(k.Reason), 160)
			}
			var steps []string
			for _, ob := range k.Observations {
				steps = append(steps, ob.Node)
			}
			for _, x := range per[k.ID] {
				line += "; " + x.text
				steps = append(steps, x.steps...)
			}
			out = append(out, item{line, steps})
		}
		for _, k := range a.Claims {
			if k.State == sessionjudge.CheckContradicted {
				out = append(out, item{"said, and the conversation contradicts it: " + trunc(oneLine(k.Text), 160), nodesOf(k.Cite)})
			}
		}
		for _, w := range a.Rework {
			out = append(out, item{reworkWords(w), []string{w.From}})
		}
	}
	if n := len(o.Corrections); n > 0 {
		out = append(out, item{fmt.Sprintf("the person corrected it %s", times(n)), o.Corrections})
	}
	found := slices.Clone(r.findings[o.Target])
	sort.SliceStable(found, func(i, j int) bool { return refLess(found[i].Ref, found[j].Ref) })
	for _, f := range found {
		if f.Source == sessionjudge.SourceASZ {
			continue
		}
		line := trunc(oneLine(f.Problem), 200)
		if f.Cause != "" {
			line += "\n" + causeWords(f)
		}
		var steps []string
		if f.At != "" {
			steps = []string{f.At}
		}
		out = append(out, item{line, steps})
	}
	if o.Avoidable != nil && o.Avoidable.Tokens() > 0 {
		line := "avoidable work: " + amountWords(*o.Avoidable, r.summary.Currency)
		if o.AvoidableSpan != nil {
			line += fmt.Sprintf(", %.1f minutes", float64(*o.AvoidableSpan)/60000)
		}
		out = append(out, item{text: line})
	}
	return out
}

func times(n int) string {
	if n == 1 {
		return "once"
	}
	return fmt.Sprintf("%d times", n)
}

// reworkWords says a round of rework: what started it, whose doing it was,
// and what the evaluator said it redid.
func reworkWords(w sessionjudge.Rework) string {
	trigger := map[string]string{sessionjudge.TriggerPerson: "the person's correction", sessionjudge.TriggerTool: "a tool's failure",
		sessionjudge.TriggerVerification: "a failed check"}[w.Trigger]
	if trigger == "" {
		trigger = strings.ReplaceAll(w.Trigger, "_", " ")
	}
	cause := map[string]string{sessionjudge.ReworkAgentError: "the agent's mistake", sessionjudge.ReworkRequirementChange: "a changed request",
		sessionjudge.ReworkUnknown: "a cause the conversation does not show"}[w.Cause]
	if cause == "" {
		cause = strings.ReplaceAll(w.Cause, "_", " ")
	}
	line := fmt.Sprintf("redone after %s, from %s", trigger, cause)
	if w.Note != "" {
		line += ": " + trunc(oneLine(w.Note), 160)
	}
	if !w.Finished {
		line += "; not finished"
	}
	return line
}

// causeWords says a finding's cause, whether it is against the reference,
// and for a cause that rests on something not happening, that it is the
// evaluator's judgment, with how much of its span the evaluator was given.
func causeWords(f sessionjudge.Finding) string {
	cause := map[string]string{sessionjudge.CauseMisleadingResult: "a misleading tool result", sessionjudge.CauseMisread: "a result read wrong",
		sessionjudge.CauseMisledBySubtask: "a sub-task's untrue report", sessionjudge.CauseMissingContext: "context the agent could not have had",
		sessionjudge.CauseContextNotUsed: "context the agent had and did not use", sessionjudge.CauseQuestionNotAsked: "a question not asked",
		sessionjudge.CauseAssumptionNotChecked: "an assumption not checked", sessionjudge.CauseUnavailable: "none the conversation supports"}[f.Cause]
	if cause == "" {
		cause = strings.ReplaceAll(f.Cause, "_", " ")
	}
	line := "cause: " + cause
	if f.Conclusion != "" {
		line += ", against the reference"
	}
	if f.Asserted {
		line += "; the evaluator's judgment" + givenWords(f.Given)
	}
	return line
}

// givenWords says how much of an absence claim's steps the evaluator was
// given: what it opened, what it saw only named in a list, and what it was
// not given. It never says the claim holds. A run made before readings were
// kept has none.
func givenWords(g *sessionjudge.Given) string {
	switch {
	case g == nil:
		return "; what it was given was not recorded"
	case g.Counts == nil:
		return "; what it was given is unavailable: " + g.Reason
	}
	c := g.Counts
	where := "where it would show"
	if g.Scope == sessionjudge.GivenWindow {
		where = "of the judged work"
	}
	text := fmt.Sprintf("; of the %s %s, it opened %d, saw %d only in lists and was not given %d", plural(c.Steps, "step"), where, c.Opened,
		c.Listed, c.None)
	if c.Unavailable > 0 {
		text += fmt.Sprintf("; %d hold evidence no one can be given", c.Unavailable)
	}
	return text
}

// assessment is how the run judged the conversation as a whole: the two
// scores, never combined, the efficiency, the sub-tasks, the grouping, the
// references, and the stages.
func (r *runReport) assessment() []string {
	var out []string
	s := r.summary
	if sc := s.Score; sc != nil {
		levels, unscored := map[string]int{}, 0
		for _, o := range r.outcomes {
			if o.Status == sessionjudge.TargetCancelled || o.Status == sessionjudge.TargetReplaced {
				unscored++
				continue
			}
			levels[o.Level]++
		}
		if sc.Achievement != nil {
			line := fmt.Sprintf("by the achievement record: %s over %s: %d achieved, %d partly, %d not achieved, %d cannot be verified",
				scoreWords100(sc.Achievement), plural(sc.Judged, "target"), levels[sessionjudge.LevelAchieved], levels[sessionjudge.LevelPartly],
				levels[sessionjudge.LevelNotAchieved], levels[sessionjudge.LevelCannotVerify])
			if unscored > 0 {
				line += fmt.Sprintf("; %d cancelled or replaced, not scored", unscored)
			}
			out = append(out, line)
		} else {
			out = append(out, fmt.Sprintf("by the achievement record: unavailable, no target could be verified (%d cannot be verified)",
				levels[sessionjudge.LevelCannotVerify]))
		}
		if sc.Final != nil {
			out = append(out, "final, after what contradicted or undid a result: "+scoreWords100(sc.Final))
		}
		if sc.Efficiency != nil {
			out = append(out, fmt.Sprintf("efficiency: %s; %s of the reviewed cost was avoidable, by %s; %s of the targets' cost was reviewed",
				scoreWords100(sc.Efficiency), percent(ptr(1-*sc.Efficiency)), sc.Unit, percent(sc.Reviewed)))
		} else {
			out = append(out, "efficiency: unavailable, no work was labeled")
		}
		if sc.Delivered != nil {
			out = append(out, fmt.Sprintf("delivered: %s of the targets' cost went to targets achieved, by %s", percent(sc.Delivered), sc.Unit))
		} else {
			out = append(out, "delivered: unavailable")
		}
	}
	if line := r.subtaskWords(); line != "" {
		out = append(out, line)
	}
	if line := r.groupingWords(); line != "" {
		out = append(out, line)
	}
	out = append(out, r.referenceWords()...)
	if len(r.unassigned) > 0 {
		out = append(out, "file operations no target's calls made: "+r.paths(r.unassigned))
	}
	out = append(out, r.stageWords())
	return out
}

// subtaskWords says in one line how the sub-tasks went, and how much
// avoidable work their own work going wrong caused.
func (r *runReport) subtaskWords() string {
	f := r.subtasks
	if f == nil || f.Header.Coverage == nil || f.Header.Coverage.Subtasks == nil || *f.Header.Coverage.Subtasks == 0 {
		return ""
	}
	count := func(get func(sessionjudge.Subtask) string, order []string) string {
		n := map[string]int{}
		for _, st := range f.Subtasks {
			n[get(st)]++
		}
		var parts []string
		for _, k := range order {
			if n[k] > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", n[k], strings.ReplaceAll(k, "_", " ")))
			}
		}
		return strings.Join(parts, ", ")
	}
	line := fmt.Sprintf("sub-tasks: %d of %d judged", *f.Header.Coverage.SubtasksJudged, *f.Header.Coverage.Subtasks)
	if len(f.Subtasks) > 0 {
		line += fmt.Sprintf("; results %s; reports %s; use %s; dispatch %s",
			count(func(s sessionjudge.Subtask) string { return s.Outcome }, sessionjudge.Levels),
			count(func(s sessionjudge.Subtask) string { return s.Report }, sessionjudge.Reports),
			count(func(s sessionjudge.Subtask) string { return s.Use }, sessionjudge.Uses),
			count(func(s sessionjudge.Subtask) string { return s.Dispatch }, sessionjudge.Dispatches))
	}
	if b := r.summary.AvoidableBySubtasks; b != nil {
		if caused := (sessionjudge.Amount{Usage: *b}); caused.Tokens() > 0 {
			line += "; their own work going wrong caused " + amountWords(caused, r.summary.Currency) + " of avoidable work"
		}
	}
	return line
}

// groupingWords says how the judge's grouping compared with the person's
// goals, when the person gave them.
func (r *runReport) groupingWords() string {
	if r.regroup == nil {
		return ""
	}
	var agreement sessionjudge.Agreement
	if len(r.regroup.Agreements) > 0 {
		agreement = r.regroup.Agreements[0]
	}
	var parts []string
	for _, c := range sessionjudge.Compares {
		if n := agreement.Classes[c]; n > 0 && c != sessionjudge.CompareAgree {
			parts = append(parts, fmt.Sprintf("%d %s", n, c))
		}
	}
	line := fmt.Sprintf("grouping: the judge agreed with %d of your %s", agreement.Classes[sessionjudge.CompareAgree],
		plural(agreement.Goals, "goal"))
	if len(parts) > 0 {
		line += "; " + strings.Join(parts, ", ")
	}
	if n := len(agreement.Extra); n > 0 {
		line += fmt.Sprintf("; %s serving none of your goals", plural(n, "target"))
	}
	if n := len(agreement.Unknown); n > 0 {
		line += fmt.Sprintf("; %s could not be placed", plural(n, "target"))
	}
	return line
}

// referenceWords says what the run was compared with: a source that could
// not be saved, and how the conversation compared with the conclusions of
// those saved.
func (r *runReport) referenceWords() []string {
	var out []string
	for _, s := range r.sources {
		if s.Outcome != sessionjudge.SourceSaved {
			out = append(out, fmt.Sprintf("reference %s could not be saved: %s; nothing was compared with it", s.Source, trunc(oneLine(s.Error), 120)))
			continue
		}
		// What the copy leaves out of what the source holds.
		var notes []string
		if s.QueryRemoved {
			notes = append(notes, "kept without its query")
		}
		if s.Cut {
			notes = append(notes, "cut to its start")
		}
		if left := judge.LeftOut(s); left != "" {
			notes = append(notes, left)
		}
		if len(notes) > 0 {
			out = append(out, fmt.Sprintf("reference %s: %s", s.Source, strings.Join(notes, "; ")))
		}
	}
	if len(r.conclusions) == 0 {
		return out
	}
	verdicts := map[string]int{}
	for _, k := range r.conclusions {
		if d, ok := r.divergences[k.ID]; ok {
			verdicts[d.Verdict]++
		} else {
			verdicts["not compared"]++
		}
	}
	var parts []string
	for _, v := range []string{sessionjudge.VerdictReached, sessionjudge.VerdictContradicted, sessionjudge.VerdictNotObserved,
		sessionjudge.VerdictUnknown, sessionjudge.VerdictNotApplicable, "not compared"} {
		if n := verdicts[v]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, strings.ReplaceAll(v, "_", " ")))
		}
	}
	line := fmt.Sprintf("reference: %s; %s", plural(len(r.conclusions), "conclusion"), strings.Join(parts, ", "))
	if n := len(r.rollbacks); n > 0 {
		line += fmt.Sprintf("; %s undone outside the conversation", plural(n, "result"))
	}
	return append(out, line)
}

// stageWords says which stages did not complete, and who judged.
func (r *runReport) stageWords() string {
	var notes, evaluators []string
	for _, h := range r.headers {
		if h.Status != sessionjudge.StatusComplete {
			note := h.Stage + " " + h.Status
			if h.Note != "" {
				note += ": " + trunc(oneLine(h.Note), 160)
			}
			notes = append(notes, note)
		}
		// A stage computed by asz, or one with nothing to ask, judged nothing.
		if h.Evaluator.Kind == sessionjudge.EvaluatorComputed || h.Attempts == 0 {
			continue
		}
		name := h.Evaluator.Name
		if h.Evaluator.Model != "" {
			name += " with " + h.Evaluator.Model
		}
		if !slices.Contains(evaluators, name) {
			evaluators = append(evaluators, name)
		}
	}
	line := "every stage completed"
	if len(notes) > 0 {
		line = "stages not complete: " + strings.Join(notes, "; ")
	}
	if len(evaluators) > 0 {
		line += "; judged by " + strings.Join(evaluators, ", ")
	}
	return line
}

// result is what the conversation came to: its targets by how they ended,
// the person's goals nothing served, and what the conversation and its
// judging used.
func (r *runReport) result() []string {
	var out []string
	byClass := map[string][]string{}
	for _, t := range r.targets {
		if o := r.outcomes[t.ID]; o != nil {
			byClass[o.Class] = append(byClass[o.Class], trunc(oneLine(o.Statement), 140))
		}
	}
	words := map[string]string{sessionjudge.ClassAchieved: "achieved", sessionjudge.ClassPartly: "partly achieved",
		sessionjudge.ClassNotAchieved: "not achieved", sessionjudge.ClassContradicted: "contradicted", sessionjudge.ClassReverted: "reverted",
		sessionjudge.ClassCannotVerify: "cannot be verified", sessionjudge.ClassCancelled: "cancelled", sessionjudge.ClassReplaced: "replaced"}
	for _, c := range []string{sessionjudge.ClassAchieved, sessionjudge.ClassPartly, sessionjudge.ClassNotAchieved, sessionjudge.ClassContradicted,
		sessionjudge.ClassReverted, sessionjudge.ClassCannotVerify, sessionjudge.ClassCancelled, sessionjudge.ClassReplaced} {
		if list := byClass[c]; len(list) > 0 {
			out = append(out, fmt.Sprintf("%d of %s %s: %s", len(list), plural(len(r.targets), "target"), words[c], strings.Join(list, "; ")))
		}
	}
	for i, g := range r.goalViews() {
		if len(g.members) > 0 {
			continue
		}
		switch g.class {
		case sessionjudge.CompareAbsent:
			out = append(out, fmt.Sprintf("your goal %d was not worked on, the evaluator's judgment", i+1))
		case sessionjudge.CompareMissed:
			out = append(out, fmt.Sprintf("your goal %d was worked on, but the judge found no target for it", i+1))
		default:
			out = append(out, fmt.Sprintf("for your goal %d, the regroup could not tell whether it was worked on or served", i+1))
		}
	}
	return append(out, r.usageWords())
}

// usageWords says what the conversation used and what judging it used. A
// total that leaves something out says what, so it is never read as whole.
func (r *runReport) usageWords() string {
	s := r.summary
	line := "the conversation used " + amountWords(sessionjudge.Amount{Usage: s.Usage, Money: s.Money, Unpriced: s.Unpriced}, s.Currency)
	if s.Unknown > 0 {
		line += ", not counting " + plural(s.Unknown, "call") + " whose usage is unknown"
	}
	if s.OutputUnknown > 0 {
		line += ", not counting the output of " + plural(s.OutputUnknown, "call") + " that never finished"
	}
	if s.Money != nil && s.Unpriced > 0 {
		line += ", with " + plural(s.Unpriced, "call") + " not priced"
	}
	if s.Judging == nil {
		return line + "; what judging used is unknown, as an evaluator did not report it"
	}
	line += "; judging used " + amountWords(*s.Judging, s.Currency)
	if s.Judging.Unknown > 0 {
		line += ", not counting " + plural(s.Judging.Unknown, "model request") + " whose usage is unknown"
	}
	if s.Judging.Money != nil && s.Judging.Unpriced > 0 {
		line += ", with " + plural(s.Judging.Unpriced, "stage") + " not priced"
	}
	if share, ok := judgingShare(s); ok {
		line += fmt.Sprintf(", %.0f%% of what the conversation cost", share*100)
	}
	if s.JudgingReportedUSD != nil {
		line += fmt.Sprintf(", %.2f USD as the evaluators reported", *s.JudgingReportedUSD)
	}
	if s.JudgingMs > 0 {
		m := float64(s.JudgingMs) / 60000
		u := s.Judging.Usage
		line += fmt.Sprintf(", over %.1f minutes: %s input and %s output tokens a minute", m, tokenWords(float64(u.Input+u.CacheRead+u.CacheWrite)/m),
			tokenWords(float64(u.Output)/m))
	}
	return line
}

// severity orders classes most severe first; a target with none comes last.
func severity(class string) int {
	if i := slices.Index(sessionjudge.Classes, class); i >= 0 {
		return i
	}
	return len(sessionjudge.Classes)
}

func classOf(o *sessionjudge.Outcome) string {
	if o == nil {
		return ""
	}
	return o.Class
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

func scoreWords100(v *float64) string {
	if v == nil {
		return "unavailable"
	}
	return fmt.Sprintf("%.0f of 100", math.Round(*v*100))
}

// scoreWords says a target's final score, with what the achievement record
// gives beside it when the two differ, or why it has none. The record counts
// a contradicted claim as a criterion not met, so it is not the criteria's
// score alone.
func scoreWords(o *sessionjudge.Outcome) string {
	switch {
	case o.Class == sessionjudge.ClassCancelled || o.Class == sessionjudge.ClassReplaced:
		return "none, not scored"
	case o.ScoreFinal == nil:
		return "unavailable"
	case o.ScoreRecord != nil && scoreOf(o.ScoreRecord) != scoreOf(o.ScoreFinal):
		return fmt.Sprintf("%s (%.0f by the achievement record)", scoreWords100(o.ScoreFinal), math.Round(*o.ScoreRecord*100))
	}
	return scoreWords100(o.ScoreFinal)
}

func percent(f *float64) string {
	if f == nil {
		return "unavailable"
	}
	return fmt.Sprintf("%.0f%%", *f*100)
}

// tokenWords prints a count of tokens in words: thousands and millions to
// one decimal place, never an abbreviation.
func tokenWords(f float64) string {
	switch {
	case f >= 1e6:
		return fmt.Sprintf("%.1f million", f/1e6)
	case f >= 1e3:
		return fmt.Sprintf("%.1f thousand", f/1e3)
	}
	return fmt.Sprintf("%.0f", f)
}

// amountWords prints tokens, and money where any call in the amount was
// priced.
func amountWords(a sessionjudge.Amount, currency string) string {
	out := tokenWords(float64(a.Tokens())) + " tokens"
	if a.Money != nil {
		out += fmt.Sprintf(", %.2f %s", *a.Money, currency)
	}
	return out
}

// judgingShare is what judging cost against what the conversation cost. It
// is given only when both are whole: never against a priced part of either,
// nor a total missing calls with no usage or calls that never finished.
func judgingShare(s *sessionjudge.Summary) (float64, bool) {
	if s.Judging == nil || s.Judging.Money == nil || s.Money == nil || *s.Money <= 0 || s.Judging.Unpriced > 0 ||
		s.Judging.Unknown > 0 || s.Unpriced > 0 || s.Unknown > 0 || s.OutputUnknown > 0 {
		return 0, false
	}
	return *s.Judging.Money / *s.Money, true
}

func ptr(f float64) *float64 { return &f }

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

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
