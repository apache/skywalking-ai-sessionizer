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
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
)

func cited(ids ...string) []sessionjudge.Cite {
	var out []sessionjudge.Cite
	for _, id := range ids {
		out = append(out, sessionjudge.Cite{Node: id})
	}
	return out
}

// reportFiles is a closed run of two goals: g1, a fix whose last delivery
// is contradicted, and g2, a release note undone. t4 is in no goal.
func reportFiles() []*sessionjudge.File {
	half, quarter, zero, one, three, two := 0.5, 0.25, 0.0, 1.0, 3, 2
	minusOne := 1
	targets := &sessionjudge.File{Header: sessionjudge.Header{Stage: sessionjudge.StageTargets, Status: sessionjudge.StatusComplete},
		Targets: []sessionjudge.Target{{ID: "t1", Statement: "Find why checkout times out"}, {ID: "t2", Statement: "Fix the retry limit"},
			{ID: "t3", Statement: "Write the release note"}, {ID: "t4", Statement: "Tidy the README"}},
		Goals: []sessionjudge.Goal{
			{ID: "g2", Statement: "Release 0.4.0", Members: []string{"t3"}, MainReason: "the person named no priority"},
			{ID: "g1", Statement: "Find why checkout times out, and fix it", Members: []string{"t2", "t1"}, Main: "t1", MainReason: "the person asked for the cause first",
				Evolution: []sessionjudge.Evolution{{Input: "in/1", Statement: "find why checkout is slow"}, {Input: "in/2", Statement: "find why it times out"}}},
		}}
	achievement := &sessionjudge.File{Header: sessionjudge.Header{Stage: sessionjudge.StageAchievement, Status: sessionjudge.StatusComplete},
		Achievements: []sessionjudge.Achievement{
			{Target: "t1", Level: sessionjudge.LevelPartly, Basis: sessionjudge.BasisObserved,
				Criteria: []sessionjudge.Check{{ID: "t1.c1", Text: "the cause named", State: sessionjudge.CheckMet, Reason: "the reply names the clock",
					Observations: []sessionjudge.Observation{{Node: "s3", Status: sessionjudge.ObservedReturned, Excerpt: "FAIL TestCache", Interpretation: "the cache test fails"}}},
					{ID: "t1.c2", Text: "fixed", State: sessionjudge.CheckUnknown, Reason: "nothing it cites shows it", GivenReason: "it is fixed"}},
				Claims: []sessionjudge.Check{{Text: "the tests pass", State: sessionjudge.CheckSupported, Cite: cited("s3")},
					{Text: "it is faster", State: sessionjudge.CheckNoEvidence}},
				Events: []sessionjudge.Event{{ID: "t1.e1", Kind: sessionjudge.EventDelivered, Artifact: sessionjudge.ArtifactAnswer, Cite: cited("r1"), Text: "the cache"},
					{ID: "t1.e3", Kind: sessionjudge.EventDeliveredAgain, Artifact: sessionjudge.ArtifactFile, Replaces: []string{"t1.e1"}}},
				Contradictions: []sessionjudge.Contradiction{{Event: "t1.e1", Conclusion: true, Text: "the cache was fine", Cite: cited("in/2")},
					{Event: "t1.e3", Criteria: []string{"t1.c1"}, Text: "not the clock either", Cite: cited("in/3")}},
				Rework: []sessionjudge.Rework{{ID: "t1.r1", Trigger: sessionjudge.TriggerPerson, Cause: sessionjudge.ReworkAgentError, From: "in/2", To: "s3", Finished: true}},
				Account: &sessionjudge.Account{
					Answer: &sessionjudge.Answer{Asked: sessionjudge.Said{Text: "why it times out", Cite: cited("in/1")},
						Checked:  []sessionjudge.Tried{{What: "the pool", Found: "it has room", Cite: cited("s2"), Limits: "one hour of metrics"}},
						RuledOut: []sessionjudge.Tried{{What: "the network", Found: "the ping is fast", Cite: cited("s1")}},
						Answered: sessionjudge.Said{Text: "the clock", Cite: cited("r2")}},
					Files: []sessionjudge.FileGroup{{Area: "internal/clock", Category: "code", Summary: "the clock fix", Operations: []string{"o1"}}},
					Remote: []sessionjudge.Remote{{System: "GitHub", Operation: "merge pull request 12", Via: "gh", Request: sessionjudge.Said{Cite: cited("s5")},
						Confirmed: &sessionjudge.Said{Cite: cited("in/4")}},
						{System: "Slack", Operation: "post the note", Via: "api", Request: sessionjudge.Said{Cite: cited("s6")}}}},
				Findings: []sessionjudge.Finding{{At: "r1", Ref: &sessionjudge.Ref{Seq: 1, Row: 2}, Problem: "it named the cause without reading the log",
					Explanation: "it guessed", Improvement: "read the log first"}}},
			{Target: "t2", Level: sessionjudge.LevelAchieved, Criteria: []sessionjudge.Check{{ID: "t2.c1", Text: "the limit holds", State: sessionjudge.CheckMet}},
				Events: []sessionjudge.Event{{ID: "t2.e1", Kind: sessionjudge.EventDelivered}, {ID: "t2.e2", Kind: sessionjudge.EventUndone, Text: "reverted once", Cite: cited("s4")},
					{ID: "t2.e3", Kind: sessionjudge.EventDeliveredAgain}}},
			{Target: "t3", Level: sessionjudge.LevelAchieved, Events: []sessionjudge.Event{{ID: "t3.e1", Kind: sessionjudge.EventDelivered, Artifact: sessionjudge.ArtifactFile},
				{ID: "t3.e2", Kind: sessionjudge.EventUndone, Text: "the note was deleted", Cite: cited("s9")}}},
		}}
	efficiency := &sessionjudge.File{Header: sessionjudge.Header{Stage: sessionjudge.StageEfficiency, Status: sessionjudge.StatusComplete},
		Findings: []sessionjudge.Finding{{Target: "t2", At: "s7", Ref: &sessionjudge.Ref{Seq: 1, Row: 9}, Problem: "the build ran twice"}}}
	closeFile := &sessionjudge.File{Header: sessionjudge.Header{Stage: sessionjudge.StageClose, Status: sessionjudge.StatusComplete,
		Summary: &sessionjudge.Summary{Score: &sessionjudge.Score{Achievement: ptr(0.625), Final: &quarter, Judged: 4},
			Goals: []sessionjudge.GoalOutcome{
				{Goal: "g1", Main: "t1", Worst: sessionjudge.ClassContradicted, ScoreMain: &zero, ScoreMean: &half,
					First: &sessionjudge.Finding{Target: "t1", Source: sessionjudge.SourceASZ, At: "s1", Problem: "criterion t1.c2 was not met: fixed"}},
				{Goal: "g2", Worst: sessionjudge.ClassReverted, ScoreMean: &zero}}}},
		Outcomes: []sessionjudge.Outcome{
			{Target: "t1", Statement: "Find why checkout times out", Status: sessionjudge.TargetClosed, Level: sessionjudge.LevelPartly, Class: sessionjudge.ClassContradicted,
				Standing: []string{"t1.e3"}, ScoreRecord: &half, ScoreFinal: &zero, Corrections: []string{"in/2"},
				Rework: []sessionjudge.ReworkCost{{Work: sessionjudge.WorkRepair, Cause: sessionjudge.CauseMain, Cost: sessionjudge.Amount{Usage: sessiondata.Usage{Input: 5000}}},
					{Work: sessionjudge.WorkMisled, Cause: "stream/a", Cost: sessionjudge.Amount{Usage: sessiondata.Usage{Input: 1000}}},
					{Work: sessionjudge.WorkNotReviewed, Cost: sessionjudge.Amount{Usage: sessiondata.Usage{Input: 500}}}},
				Operations: []sessionjudge.OperationShare{{ID: "o1"}, {ID: "o2", Share: &half}}},
			{Target: "t2", Statement: "Fix the retry limit", Status: sessionjudge.TargetClosed, Level: sessionjudge.LevelAchieved, Class: sessionjudge.ClassAchieved,
				ScoreRecord: &one, ScoreFinal: &one},
			{Target: "t3", Statement: "Write the release note", Status: sessionjudge.TargetClosed, Level: sessionjudge.LevelAchieved, Class: sessionjudge.ClassReverted,
				Standing: []string{"t3.e1"}, Undone: []string{"t3.e2"}, ScoreRecord: &one, ScoreFinal: &zero},
			{Target: "t4", Statement: "Tidy the README", Status: sessionjudge.TargetCancelled, Class: sessionjudge.ClassCancelled},
		},
		Findings: []sessionjudge.Finding{{Target: "t1", Source: sessionjudge.SourceASZ, At: "s1", Ref: &sessionjudge.Ref{Seq: 1, Row: 1}, Problem: "criterion t1.c2 was not met: fixed"}},
		Operations: []sessionjudge.Operation{
			{ID: "o1", Path: "internal/clock/clock.go", Operation: "modify", Source: sessionjudge.SourceChangeRecord, Added: &three, Removed: &minusOne},
			{ID: "o2", Path: "docs/clock.md", Operation: "write", Source: sessionjudge.SourceToolRequest, Status: sessionjudge.ObservedNoResult},
			{ID: "o3", Path: "scratch.txt", Operation: "write", Source: sessionjudge.SourceToolRequest, Status: sessionjudge.ObservedReturned},
		}}
	closeFile.Header.Summary.UnassignedOperations = []string{"o3"}
	_ = two
	return []*sessionjudge.File{targets, achievement, efficiency, closeFile}
}

// The report reads goal by goal, the most severe first: each goal's worst
// class, first finding and main target; each target's scores, reasons and
// what its steps show, what was done by what it was for, the person's
// corrections, its rework and what it still misses; then the findings, and
// the goal's scores.
func TestTheReportReadsGoalByGoal(t *testing.T) {
	out := printed(t, func() { printRun(storage.JudgmentRun{Run: 9}, reportFiles()) })
	want := []string{
		"goal g1  Find why checkout times out, and fix it",
		`evolved   "find why checkout is slow" (in/1) → "find why it times out" (in/2)`,
		"worst     contradicted",
		"first     asz at s1: criterion t1.c2 was not met: fixed",
		"main      t1: the person asked for the cause first",
		"t1  Find why checkout times out",
		"0/100 (50 by the record)",
		"outcome   contradicted; standing t1.e3; 1 round of rework",
		"why       t1.c1 met: the cause named",
		"read: the reply names the clock",
		`[s3 returned] "FAIL TestCache" — read: the cache test fails`,
		"claims    supported: the tests pass [s3]",
		"no evidence: it is faster nothing cited",
		"history   t1.e1 delivered (answer) [r1]: the cache",
		"contradicted the delivered conclusion, by [in/2]: the cache was fine; replaced since",
		"t1.e3 delivered again, replacing t1.e1 (file)\n",
		"contradicted  t1.c1, by [in/3]: not the clock either",
		"did       answer",
		"asked      why it times out [in/1]",
		"checked    the pool: it has room [s2]; holds for: one hour of metrics",
		"ruled out  the network: the ping is fast [s1]",
		"answered   the clock [r2]",
		"files",
		"internal/clock, code: the clock fix",
		"internal/clock/clock.go (modify +3 -1) [o1]",
		"not grouped: docs/clock.md (write, no result, share 0.5) [o2]",
		"remote",
		"GitHub: merge pull request 12 via gh, sent [s5], confirmed [in/4]",
		"Slack: post the note via api, sent [s6], not confirmed",
		"person    corrected it at in/2",
		"rework    t1.r1, person, agent error: from in/2 to s3; finished",
		"cost: repair own mistake 5.0k tokens; misled by subtask, by stream/a 1.0k tokens; not reviewed 500 tokens",
		"gap       t1.c2 unknown: fixed",
		"asz: nothing it cites shows it; the evaluator read: it is fixed",
		"findings",
		"t1 at s1, asz: criterion t1.c2 was not met: fixed",
		"t1 at r1, achievement: it named the cause without reading the log",
		"why, proposed: it guessed",
		"improvement, proposed: read the log first",
		"t2 at s7, efficiency: the build ran twice",
		"scores    main t1 0/100; all 2 targets 50/100 (75 by the record)",
		"goal g2  Release 0.4.0",
		"main      undetermined: the person named no priority",
		"undone    the whole result, by [s9]: the note was deleted",
		"did       files",
		"none recorded: no change record captured them, and no editing tool's request named them",
		"scores    main undetermined; all 1 target 0/100 (100 by the record)",
		"not in a goal",
		"t4  Tidy the README",
		"not scored, cancelled",
		"operations no target's calls made",
		"scratch.txt (write, returned) [o3]",
		"achievement  63/100 over 4 targets judged",
		"final        25/100 over the targets scored, 63 by the record",
	}
	at := 0
	for _, w := range want {
		i := strings.Index(out[at:], w)
		if i < 0 {
			t.Fatalf("no %q after what came before, in:\n%s", w, out)
		}
		at += i
	}
	// A contradiction of an earlier delivery, and an undoing a later delivery
	// followed, are history, not what stands: each is shown once, in the
	// history.
	for _, gone := range []string{"the cache was fine", "t2.e2 undone the whole result [s4]: reverted once"} {
		if strings.Count(out, gone) != 1 || strings.Contains(out, "contradicted  the delivered conclusion") {
			t.Fatalf("%q not shown once as history:\n%s", gone, out)
		}
	}
	// Only a target that delivered files and has none recorded says so.
	if n := strings.Count(out, "none recorded"); n != 1 {
		t.Fatalf("%d targets with files unrecorded, of one:\n%s", n, out)
	}
	if n := strings.Count(out, "undone    "); n != 1 {
		t.Fatalf("%d undoings shown, of one standing:\n%s", n, out)
	}
	// Within a goal, the most severe target comes first.
	if strings.Index(out, "t1  Find why") > strings.Index(out, "t2  Fix the retry") {
		t.Fatalf("t2 before t1:\n%s", out)
	}
}

// With a regroup, the report follows the person's goals, and says how the
// judge's grouping compared.
func TestTheReportFollowsThePersonsGoals(t *testing.T) {
	regroup := &sessionjudge.File{Header: sessionjudge.Header{Stage: sessionjudge.StageRegroup, Status: sessionjudge.StatusPartial,
		Labels: &sessionjudge.LabelFile{File: "goals.yaml", SHA256: "41be12345678abcdef", Version: 2}},
		LabelGoals: []sessionjudge.LabelGoal{
			{ID: "P1", Statement: "Fix checkout, release and tidy", Members: []string{"t1", "t2", "t3"}, Main: "t2", Judge: []string{"g1", "g2"},
				Class: sessionjudge.CompareSplit, Worked: true},
			{ID: "P2", Statement: "Design a logo", Members: []string{}, Judge: []string{}, Class: sessionjudge.CompareMissed, Worked: true},
			{ID: "P3", Statement: "Write the docs", Members: []string{}, Judge: []string{}, Class: sessionjudge.CompareAbsent},
			{ID: "P4", Statement: "Fix the build", Members: []string{}, Judge: []string{}, Class: sessionjudge.CompareUnknown, Worked: true},
			{ID: "P5", Statement: "Update the logo", Members: []string{}, Judge: []string{}, Class: sessionjudge.CompareUnknown},
		},
		Agreements: []sessionjudge.Agreement{{Goals: 5, Classes: map[string]int{"split": 1, "missed": 1, "absent": 1, "unknown": 2}, Extra: []string{"t4"}}}}
	out := printed(t, func() { printRun(storage.JudgmentRun{Run: 9}, append(reportFiles(), regroup)) })
	want := []string{
		"regroup      partial",
		"goal P1  Fix checkout, release and tidy",
		"worst     contradicted",
		"first     asz at s1: criterion t1.c2 was not met: fixed",
		"main      t2",
		"scores    main t2 100/100; all 3 targets 33/100",
		"goal P2  Design a logo",
		"no target: the conversation worked on it, and the judge found no target for it",
		"goal P3  Write the docs",
		"no target: nothing in the conversation worked on it",
		"goal P4  Fix the build",
		"no target placed: the conversation worked on it, and a target that may serve it could not be placed",
		"goal P5  Update the logo",
		"no target: the regroup could not tell whether the conversation worked on it",
		"not in a goal",
		"t4  Tidy the README",
		"grouping  by the person's goals, goals.yaml (sha256 41be12345678), regroup 2; the judge agreed on 0 of 5 goals",
		"P1  split    Fix checkout, release and tidy: t1 in g1, t2 in g1, t3 in g2",
		"P2  missed   Design a logo\n",
		"P3  absent   Write the docs\n",
		"serve none of the person's goals: t4",
	}
	at := 0
	for _, w := range want {
		i := strings.Index(out[at:], w)
		if i < 0 {
			t.Fatalf("no %q after what came before, in:\n%s", w, out)
		}
		at += i
	}
	// The judge's own goals are not printed beside the person's.
	if strings.Contains(out, "goal g1") || strings.Contains(out, "goal g2") {
		t.Fatalf("the judge's goals printed:\n%s", out)
	}
}

// A run compared with sources says what each was, how it was saved or why
// it was not, each conclusion with how the conversation compared, and, at
// the target, what applies to its final score.
func TestTheReportSaysWhatTheSourcesShowed(t *testing.T) {
	files := reportFiles()
	files[3].Outcomes[0].References = []string{"r2"}
	ref := &sessionjudge.File{Header: sessionjudge.Header{Stage: sessionjudge.StageReference, Status: sessionjudge.StatusPartial},
		Sources: []sessionjudge.Source{{ID: "R1", Source: "OPS-1.md", Method: "file", Outcome: "saved", Captured: "2026-10-23T09:00:00Z", Lines: 40,
			SHA256: "7a1c2b3d4e5f60718293", Cut: true}, {ID: "R2", Source: "https://jira.example/OPS-2", Outcome: "failed", Error: "the server answered 401"}},
		Conclusions: []sessionjudge.Conclusion{{ID: "r1", Kind: "intermediate", Statement: "the queue was full", Support: "saved", Scope: []string{"in/1"}},
			{ID: "r2", Kind: "final", Statement: "max_attempts is never checked", Support: "asserted", Scope: []string{"in/1"}, Conflicts: []string{"r3"}},
			{ID: "r3", Kind: "final", Statement: "an aside", Support: "linked"},
			{ID: "r4", Kind: "final", Statement: "the first fix was wrong", Support: "linked", Scope: []string{"in/1"}},
			{ID: "r5", Kind: "final", Statement: "the cache is fine", Support: "linked", Scope: []string{"in/1"}},
			{ID: "r6", Kind: "intermediate", Statement: "the database failed", Support: "linked", Scope: []string{"in/1"}, SupersededBy: "r2"}}}
	div := &sessionjudge.File{Header: sessionjudge.Header{Stage: sessionjudge.StageDivergence, Status: sessionjudge.StatusComplete},
		Divergences: []sessionjudge.Divergence{{Conclusion: "r1", Verdict: "reached", Cite: cited("s3")},
			{Conclusion: "r2", Verdict: "contradicted", Target: "t1", Event: "t1.e3", Whole: true}, {Conclusion: "r3", Verdict: "not_observed", Asserted: true},
			{Conclusion: "r4", Verdict: "contradicted", Target: "t1", Event: "t1.e1", Criteria: []string{"t1.c1"}},
			{Conclusion: "r5", Verdict: "contradicted", Target: "t1", Event: "t1.e3", Whole: true},
			{Conclusion: "r6", Verdict: "contradicted", Target: "t1", Event: "t1.e3", Whole: true}},
		Rollbacks: []sessionjudge.Rollback{{Conclusion: "r2", Target: "t1", Event: "t1.e3", Criteria: []string{"t1.c2"}}},
		Findings: []sessionjudge.Finding{{Target: "t1", At: "s2", Ref: &sessionjudge.Ref{Seq: 1, Row: 2}, Problem: "it trusted the ping", Conclusion: "r2",
			Cause: "unsupported_assumption", Asserted: true}}}
	out := printed(t, func() { printRun(storage.JudgmentRun{Run: 9}, append(files, ref, div)) })
	want := []string{
		"reference r2 contradicts the delivered conclusion of t1.e3: max_attempts is never checked",
		"reference r2 rolled back t1.c2 of t1.e3, outside the conversation: max_attempts is never checked",
		"t1 at s2, divergence: it trusted the ping",
		"cause: unsupported assumption, against r2; the evaluator's judgment, since the record cannot show it was looked for everywhere",
		"references",
		"R1  OPS-1.md: file, saved 2026-10-23T09:00:00Z, 40 lines, sha256 7a1c2b3d4e5f, cut to its start",
		"R2  https://jira.example/OPS-2: not saved, the server answered 401; its comparison is unavailable",
		"r1  intermediate the queue was full",
		"reached [s3]",
		"saved support; about in/1",
		"r2  final        max_attempts is never checked",
		"contradicted t1's delivered conclusion, t1.e3\n",
		"rolled back t1.c2 of t1, t1.e3, outside the conversation\n",
		"asserted support; about in/1; disagrees with r3",
		"r3  final        an aside",
		"not observed, the evaluator's judgment",
		"linked support; about no input, so it changes no score",
		// What a source shows of a delivery a later one replaced, or that
		// another conclusion disagrees with, changes no score, and says why.
		"contradicted t1.c1 of t1, t1.e1; it changes no score, since t1.e1 no longer stands\n",
		"contradicted t1's delivered conclusion, t1.e3; it changes no score, since another conclusion about t1 disagrees\n",
		// A conclusion its source corrected later says so, and by which.
		"contradicted t1's delivered conclusion, t1.e3; it changes no score, since r2 corrects it\n",
		"linked support; about in/1; superseded by r2\n",
	}
	at := 0
	for _, w := range want {
		i := strings.Index(out[at:], w)
		if i < 0 {
			t.Fatalf("no %q after what came before, in:\n%s", w, out)
		}
		at += i
	}
}

// A text cut short is cut between characters.
func TestATextIsCutBetweenCharacters(t *testing.T) {
	for in, want := range map[string]string{"abcdef": "abc…", "ab═d": "ab…", "a═cd": "a…", "abcd": "abcd", "": "-"} {
		if got := trunc(in, 4); got != want || !utf8.ValidString(got) {
			t.Errorf("trunc(%q) = %q, want %q", in, got, want)
		}
	}
}

// A regroup takes the evaluator named, or the targets stage's.
func TestARegroupTakesTheTargetsStagesEvaluator(t *testing.T) {
	plan := []config.JudgeStage{{Stage: sessionjudge.StageTargets, Evaluator: "opus"}, {Stage: sessionjudge.StageAchievement, Evaluator: "sonnet"}}
	if got := regrouperOf("", plan); got != "opus" {
		t.Errorf("unnamed: %q", got)
	}
	if got := regrouperOf("codex", plan); got != "codex" {
		t.Errorf("named: %q", got)
	}
}
