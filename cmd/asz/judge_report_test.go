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
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

func cited(ids ...string) []sessionjudge.Cite {
	var out []sessionjudge.Cite
	for _, id := range ids {
		out = append(out, sessionjudge.Cite{Node: id})
	}
	return out
}

// reportFiles is a closed run of two goals: g1, a release note undone, and
// g2, a fix whose last delivery is contradicted, the more severe outcome
// in the later goal. t4 is in no goal.
func reportFiles() []*sessionjudge.File {
	half, quarter, zero, one, three, two := 0.5, 0.25, 0.0, 1.0, 3, 2
	minusOne := 1
	targets := &sessionjudge.File{Header: sessionjudge.Header{Stage: sessionjudge.StageTargets, Status: sessionjudge.StatusComplete},
		Targets: []sessionjudge.Target{{ID: "t1", Statement: "Find why checkout times out"}, {ID: "t2", Statement: "Fix the retry limit"},
			{ID: "t3", Statement: "Write the release note"}, {ID: "t4", Statement: "Tidy the README"}},
		Goals: []sessionjudge.Goal{
			{ID: "g1", Statement: "Release 0.4.0", Members: []string{"t3"}, MainReason: "the person named no priority"},
			{ID: "g2", Statement: "Find why checkout times out, and fix it", Members: []string{"t2", "t1"}, Main: "t1", MainReason: "the person asked for the cause first",
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
		Summary: &sessionjudge.Summary{Score: &sessionjudge.Score{Achievement: ptr(0.625), Final: &quarter, Judged: 4}}},
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

// The report reads goal by goal, in the order of their ids, though the later
// goal holds the more severe outcome. Each target says its status, score,
// criteria, what was done and what is missing, in plain words with no id or
// step address; then come the overall assessment and the overall result. A
// goal only groups targets: it prints no class, finding or score of its own.
func TestTheReportReadsGoalByGoal(t *testing.T) {
	out := printed(t, func() { printRun(storage.JudgmentRun{Run: 9}, reportFiles()) })
	want := []string{
		"goal 1: Release 0.4.0",
		"└─ target: Write the release note",
		"   ├─ status: reverted: undone in the conversation, the note was deleted",
		"   ├─ score: 0 of 100 (100 by the achievement record)",
		"   │  1. changed files that no change record or editing tool names",
		// A result undone whole is what it is missing.
		"      1. the whole result: undone in the conversation, the note was deleted",
		"goal 2: Find why checkout times out, and fix it",
		"├─ target: Find why checkout times out (the main one)",
		// Nothing is said of the criterion's state before it was contradicted.
		"│  ├─ status: contradicted: 1 criterion contradicted or undone\n",
		"│  ├─ score: 0 of 100 (50 by the achievement record)",
		"│  │  1. the cause named: met; later in the conversation, not the clock either",
		"│  │  2. fixed: unknown",
		"│  │  1. the pool: it has room",
		"│  │  2. ruled out the network: the ping is fast",
		"│  │  3. answered: the clock",
		"│  │  4. internal/clock, code: the clock fix",
		"│  │     internal/clock/clock.go (modify +3 -1)",
		// A request of an editing tool is an attempt, said with what became of it.
		"│  │  5. files: docs/clock.md (write, requested, the tool never returned)",
		"│  │  6. merge pull request 12 on GitHub through gh, confirmed",
		"│  │  7. post the note on Slack through api, not confirmed",
		// A criterion met and contradicted since is missing too.
		"│     1. the cause named: met, then later in the conversation, not the clock either",
		"│     2. fixed: unknown, nothing it cites shows it",
		"│     3. redone after the person's correction, from the agent's mistake",
		"│     4. the person corrected it once",
		"│     5. it named the cause without reading the log",
		"└─ target: Fix the retry limit",
		"   ├─ status: achieved",
		"      1. the build ran twice",
		"not in a goal",
		"└─ target: Tidy the README",
		"   ├─ status: cancelled: the person no longer wanted it, so it is not scored",
		"overall assessment",
		"1. by the achievement record: 63 of 100 over 4 targets: 2 achieved, 1 partly, 0 not achieved, 0 cannot be verified; 1 cancelled or replaced, not scored",
		"2. final, after what contradicted or undid a result: 25 of 100",
		// An operation is not a change: it says what became of it.
		"file operations no target's calls made: scratch.txt (write, requested, the tool returned)",
		"overall result",
		"1. 1 of 4 targets achieved: Fix the retry limit",
		"2. 1 of 4 targets contradicted: Find why checkout times out",
		"3. 1 of 4 targets reverted: Write the release note",
		"4. 1 of 4 targets cancelled: Tidy the README",
	}
	at := 0
	for _, w := range want {
		i := strings.Index(out[at:], w)
		if i < 0 {
			t.Fatalf("no %q after what came before, in:\n%s", w, out)
		}
		at += i
	}
	if id := regexp.MustCompile(`\b([tg]\d|t\d\.[cer]\d|s\d|in/\d)\b`).FindString(out); id != "" {
		t.Fatalf("named by an id or an address, %q:\n%s", id, out)
	}
	for _, gone := range []string{"worst", "first recorded", "scores "} {
		if strings.Contains(out, gone) {
			t.Fatalf("a goal printed %q:\n%s", gone, out)
		}
	}
}

// Among several targets, a goal's main one is marked, unless the person no
// longer wanted it by the end, as a goal of the person's has none then.
func TestACancelledMainTargetLeavesItsGoalUndetermined(t *testing.T) {
	if out := printed(t, func() { printRun(storage.JudgmentRun{Run: 9}, reportFiles()) }); !strings.Contains(out, "target: Find why checkout times out (the main one)") {
		t.Fatalf("no main target marked:\n%s", out)
	}
	files := reportFiles()
	files[3].Outcomes[0].Status, files[3].Outcomes[0].Class = sessionjudge.TargetCancelled, sessionjudge.ClassCancelled
	if out := printed(t, func() { printRun(storage.JudgmentRun{Run: 9}, files) }); strings.Contains(out, "(the main one)") {
		t.Fatalf("a cancelled main target:\n%s", out)
	}
}

// With a regroup, the report follows the person's goals, and says how the
// judge's grouping compared.
func TestTheReportFollowsThePersonsGoals(t *testing.T) {
	regroup := &sessionjudge.File{Header: sessionjudge.Header{Stage: sessionjudge.StageRegroup, Status: sessionjudge.StatusPartial,
		Goals: &sessionjudge.GoalsFile{File: "goals.yaml", SHA256: "41be12345678abcdef", Version: 2}},
		Goals: []sessionjudge.Goal{
			{ID: "P1", Statement: "Fix checkout, release and tidy", Members: []string{"t1", "t2", "t3"}, Main: "t2", Judge: []string{"g2", "g1"},
				Class: sessionjudge.CompareSplit, Worked: true},
			{ID: "P2", Statement: "Design a logo", Members: []string{}, Judge: []string{}, Class: sessionjudge.CompareMissed, Worked: true},
			{ID: "P3", Statement: "Write the docs", Members: []string{}, Judge: []string{}, Class: sessionjudge.CompareAbsent},
			{ID: "P4", Statement: "Fix the build", Members: []string{}, Judge: []string{}, Class: sessionjudge.CompareUnknown, Worked: true},
			{ID: "P5", Statement: "Update the logo", Members: []string{}, Judge: []string{}, Class: sessionjudge.CompareUnknown},
		},
		Agreements: []sessionjudge.Agreement{{Goals: 5, Classes: map[string]int{"split": 1, "missed": 1, "absent": 1, "unknown": 2}, Extra: []string{"t4"}}}}
	out := printed(t, func() { printRun(storage.JudgmentRun{Run: 9}, append(reportFiles(), regroup)) })
	want := []string{
		"goal 1: Fix checkout, release and tidy",
		"├─ target: Find why checkout times out",
		"├─ target: Write the release note",
		"└─ target: Fix the retry limit (the main one)",
		"goal 2: Design a logo",
		"├─ status: worked on, but the judge found no target for it",
		"└─ score: none",
		"goal 3: Write the docs",
		"├─ status: not worked on, the evaluator's judgment; what it was given was not recorded",
		"goal 4: Fix the build",
		"├─ status: worked on, and a target that may serve it could not be placed",
		"goal 5: Update the logo",
		"├─ status: the regroup could not tell whether it was worked on",
		"not in a goal",
		"└─ target: Tidy the README",
		"grouping: the judge agreed with 0 of your 5 goals; 1 missed, 1 split, 2 unknown, 1 absent; 1 target serving none of your goals",
		"stages not complete: regroup partial",
		"your goal 2 was worked on, but the judge found no target for it",
		"your goal 3 was not worked on, the evaluator's judgment",
		"for your goal 4, the regroup could not tell whether it was worked on or served",
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
	if strings.Contains(out, "goal 1: Release 0.4.0") || strings.Contains(out, "goal 2: Find why") {
		t.Fatalf("the judge's goals printed:\n%s", out)
	}
}

// A run compared with sources says, at the target, what a source shows of
// its result standing at the end, and of each criterion; a cause resting on
// something not happening, with how much of its span the evaluator was
// given; and, in the assessment, a source that could not be saved and how
// the conversation compared with the conclusions.
func TestTheReportSaysWhatTheSourcesShowed(t *testing.T) {
	files := reportFiles()
	files[3].Outcomes[0].References = []string{"r2"}
	ref := &sessionjudge.File{Header: sessionjudge.Header{Stage: sessionjudge.StageReference, Status: sessionjudge.StatusPartial},
		Sources: []sessionjudge.Source{{ID: "R1", Source: "OPS-1.md", Method: "file", Outcome: "saved", Captured: "2026-10-23T09:00:00Z", Lines: 40,
			SHA256: "7a1c2b3d4e5f60718293", Cut: true}, {ID: "R2", Source: "https://jira.example/OPS-2", Outcome: "failed", Error: "the server answered 401"},
			{ID: "R3", Source: "https://jira.example/OPS-3", QueryRemoved: true, Method: "mcp", Outcome: "saved", Captured: "2026-10-23T09:00:00Z", Lines: 12,
				SHA256: "0123456789abcdef", MCP: &sessionjudge.SourceMCP{Server: "https://mcp.example.com", Tool: "getJiraIssue", BlocksOmitted: 1,
					StructuredOmitted: true, Redactions: 2}}},
		Conclusions: []sessionjudge.Conclusion{{ID: "r1", Kind: "intermediate", Statement: "the queue was full", Support: "saved", Scope: []string{"in/1"}},
			{ID: "r2", Kind: "final", Statement: "max_attempts is never checked", Support: "asserted", Scope: []string{"in/1"}, Conflicts: []string{"r3"}},
			{ID: "r3", Kind: "final", Statement: "an aside", Support: "linked"},
			{ID: "r4", Kind: "final", Statement: "the first fix was wrong", Support: "linked", Scope: []string{"in/1"}},
			{ID: "r5", Kind: "final", Statement: "the cache is fine", Support: "linked", Scope: []string{"in/1"}},
			{ID: "r6", Kind: "intermediate", Statement: "the database failed", Support: "linked", Scope: []string{"in/1"}, SupersededBy: "r2"}}}
	div := &sessionjudge.File{Header: sessionjudge.Header{Stage: sessionjudge.StageDivergence, Status: sessionjudge.StatusComplete},
		Divergences: []sessionjudge.Divergence{{Conclusion: "r1", Verdict: "reached", Cite: cited("s3")},
			{Conclusion: "r2", Verdict: "contradicted", Target: "t1", Event: "t1.e3", Whole: true}, {Conclusion: "r3", Verdict: "not_observed", Asserted: true,
				Given: &sessionjudge.Given{Scope: sessionjudge.GivenWindow, Counts: &sessionjudge.GivenCounts{Steps: 40, Opened: 12, Listed: 21, None: 7, Unavailable: 2}}},
			{Conclusion: "r4", Verdict: "contradicted", Target: "t1", Event: "t1.e1", Criteria: []string{"t1.c1"}},
			{Conclusion: "r5", Verdict: "contradicted", Target: "t1", Event: "t1.e3", Whole: true},
			{Conclusion: "r6", Verdict: "contradicted", Target: "t1", Event: "t1.e3", Whole: true}},
		Rollbacks: []sessionjudge.Rollback{{Conclusion: "r2", Target: "t1", Event: "t1.e3", Criteria: []string{"t1.c2"}}},
		Findings: []sessionjudge.Finding{{Target: "t1", At: "s2", Ref: &sessionjudge.Ref{Seq: 1, Row: 2}, Problem: "it trusted the ping", Conclusion: "r2",
			Cause: "assumption_not_checked", Asserted: true, From: "s1",
			Given: &sessionjudge.Given{Scope: sessionjudge.GivenSpan, Counts: &sessionjudge.GivenCounts{Steps: 3, Opened: 3}}}}}
	out := printed(t, func() { printRun(storage.JudgmentRun{Run: 9}, append(files, ref, div)) })
	want := []string{
		"├─ target: Find why checkout times out (the main one)",
		"│  ├─ status: contradicted: the reference says max_attempts is never checked",
		"│  │  1. the cause named: met; later in the conversation, not the clock either",
		"│  │  2. fixed: unknown; undone outside the conversation: max_attempts is never checked",
		"it trusted the ping",
		"cause: an assumption not checked, against the reference; the evaluator's judgment; of the 3 steps where it would show, " +
			"it opened 3, saw 0 only in lists and was not given 0",
		"reference OPS-1.md: cut to its start",
		"reference https://jira.example/OPS-2 could not be saved: the server answered 401; nothing was compared with it",
		"reference https://jira.example/OPS-3: kept without its query; 1 content block that is not text left out, " +
			"the structured content beside the text left out, 2 secrets redacted",
		"reference: 6 conclusions; 1 reached, 4 contradicted, 1 not observed; 1 result undone outside the conversation",
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

// The conclusions read as Markdown: one table of every goal's targets, a
// cell's bar and tag escaped, and what each target is missing, with no
// detail. The page opens as a file: Horizon's tokens, a badge per target,
// its evidence behind its summary, and every cited step the conversation
// holds shown on the page and linked there; a text that looks like a tag
// stays text. Steps not read are not linked, and the page says why.
func TestTheReportIsWrittenAsMarkdownAndAPage(t *testing.T) {
	files := reportFiles()
	files[3].Outcomes[1].Statement = "Fix __init__.py | limit <now> &copy; as https://example.com/?a=1&b=2_x|y says, not http://[::1]:8080/__init__.py or https://example.com/\\|y"
	files[1].Achievements[0].Findings[0].Problem = "- it named the cause without reading the log"
	doc := buildReport(storage.JudgmentRun{Run: 9}, files)
	var md strings.Builder
	if err := writeReport(&md, doc, formatMarkdown, pageAssets{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"| Goal | Target | Status | Score |",
		"| Goal 2: Find why checkout times out, and fix it | Fix \\_\\_init\\_\\_.py \\| limit \\<now\\> \\&copy; as `https://example.com/?a=1&b=2_x\\|y` says, not `http://[::1]:8080/__init__.py` or https://example.com/\\\\\\|y | achieved | 100 of 100 |",
		// A line that starts as a list item stays text.
		"\\- it named the cause without reading the log",
		"**Missing: Find why checkout times out**", "## Overall assessment", "## Overall result"} {
		if !strings.Contains(md.String(), want) {
			t.Fatalf("no %q in:\n%s", want, md.String())
		}
	}
	if strings.Contains(md.String(), "**Criteria**") || strings.Contains(md.String(), "**Done**") {
		t.Fatalf("the summary holds the details:\n%s", md.String())
	}
	nodes := map[string]sessionview.Node{"s3": {ID: "s3", Kind: "tool", Name: "Bash", At: 1700000000000, Text: `{"command":"go test"}`,
		Result: "FAIL TestCache <b>"}, "s9": {ID: "s9", Kind: "tool", Name: "Bash", At: 1700000001000, Text: `{"command":"rm notes.md"}`}}
	var page strings.Builder
	if err := writeReport(&page, doc, formatHTML, pageAssets{theme: ":root { --sw-ok: green; }", logo: []byte("<svg/>")}, nodes, nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{":root { --sw-ok: green; }", `<span class="badge class-contradicted">contradicted</span>`,
		// The page carries the asz page's header: the logo, held in the page, and the project's name.
		`<img class="logo-dark" src="data:image/svg&#43;xml;base64,PHN2Zy8&#43;" alt="SkyWalking">`, "<span>AI Sessionizer</span>",
		// Each target's details are a link away from the table, and a cited step opens in a dialog.
		`<a class="button" href="#goal-1-target-1">Check details</a>`, `<details class="target" id="goal-1-target-1">`,
		`<dialog class="step-dialog" id="step-dialog"`, `dialog.showModal()`, `html:has(dialog[open]) { overflow: hidden; }`,
		`<a class="step" href="#step-1">the step</a>`, `<div class="step-card" id="step-1">`, "tool Bash",
		"2023-11-14 22:13:20 UTC", "FAIL TestCache &lt;b&gt;", "Fix __init__.py | limit &lt;now&gt;",
		// The step that undid a result is shown and linked from the status it decides.
		`undone in the conversation, the note was deleted <a class="step" href="#step-`, "rm notes.md"} {
		if !strings.Contains(page.String(), want) {
			t.Fatalf("no %q in:\n%s", want, page.String())
		}
	}
	if strings.Contains(page.String(), "<now>") || strings.Contains(page.String(), "<b>") {
		t.Fatalf("a text read as a tag:\n%s", page.String())
	}
	page.Reset()
	if err := writeReport(&page, doc, formatHTML, pageAssets{}, nil, errors.New("round 1 of the conversation is now x")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page.String(), `href="#step-`) || strings.Contains(page.String(), "<dialog") || !strings.Contains(page.String(), "The steps the evidence rests on are not shown: round 1 of the conversation is now x") {
		t.Fatalf("steps not read:\n%s", page.String())
	}
	if err := writeReport(io.Discard, doc, "pdf", pageAssets{}, nil, nil); err == nil {
		t.Fatal("a form asz does not know was written")
	}
	// A pulled conversation is named inside the page, escaped, and nothing
	// comes before the page's doctype.
	doc.snapshot = "prod/review&copy;:20261028"
	page.Reset()
	if err := writeReport(&page, doc, formatHTML, pageAssets{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(page.String(), "<!doctype html>") || !strings.Contains(page.String(), "prod/review&amp;copy;:20261028") {
		t.Fatalf("the snapshot's name:\n%s", page.String())
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

// A claim of absence says how much of its steps the evaluator was given, or
// why that cannot be said; a run made before readings were kept says it has
// none. It never says the claim holds.
func TestAReadingStandsBesideItsClaim(t *testing.T) {
	for _, c := range []struct {
		given *sessionjudge.Given
		want  string
	}{
		{nil, "; what it was given was not recorded"},
		{&sessionjudge.Given{Scope: sessionjudge.GivenWindow, Reason: "the navigation log has no closing line"},
			"; what it was given is unavailable: the navigation log has no closing line"},
		{&sessionjudge.Given{Scope: sessionjudge.GivenWindow, Counts: &sessionjudge.GivenCounts{Steps: 1, Listed: 1}},
			"; of the 1 step of the judged work, it opened 0, saw 1 only in lists and was not given 0"},
	} {
		if got := givenWords(c.given); got != c.want {
			t.Errorf("%q, want %q", got, c.want)
		}
	}
}
