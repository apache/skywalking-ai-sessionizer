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

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
)

// Judging is set against the conversation's cost only when both totals are
// whole.
func TestTheJudgingShareNeedsWholeTotals(t *testing.T) {
	whole := func() *sessionjudge.Summary {
		return &sessionjudge.Summary{Money: ptr(4), Judging: &sessionjudge.Amount{Money: ptr(1)}}
	}
	if share, ok := judgingShare(whole()); !ok || share != 0.25 {
		t.Fatalf("whole totals: %v %v", share, ok)
	}
	for name, change := range map[string]func(*sessionjudge.Summary){
		"a conversation call unpriced":      func(s *sessionjudge.Summary) { s.Unpriced = 1 },
		"a conversation call with no usage": func(s *sessionjudge.Summary) { s.Unknown = 1 },
		"a conversation call unfinished":    func(s *sessionjudge.Summary) { s.OutputUnknown = 1 },
		"a judging stage unpriced":          func(s *sessionjudge.Summary) { s.Judging.Unpriced = 1 },
		"a judging request with no usage":   func(s *sessionjudge.Summary) { s.Judging.Unknown = 1 },
		"judging not priced":                func(s *sessionjudge.Summary) { s.Judging.Money = nil },
		"judging unknown":                   func(s *sessionjudge.Summary) { s.Judging = nil },
		"a conversation that cost nothing":  func(s *sessionjudge.Summary) { s.Money = ptr(0) },
	} {
		s := whole()
		change(s)
		if _, ok := judgingShare(s); ok {
			t.Errorf("a share was given with %s", name)
		}
	}
}

// A stage that had nothing to answer says it made no call, not that what it
// consumed is unknown.
func TestAStageSaysWhatItConsumed(t *testing.T) {
	cli := sessionjudge.Evaluator{Kind: sessionjudge.EvaluatorCLI}
	for want, h := range map[string]sessionjudge.Header{
		"computed":                   {Evaluator: sessionjudge.Evaluator{Kind: sessionjudge.EvaluatorComputed}},
		"no call, nothing to answer": {Evaluator: cli, Usage: &sessiondata.Usage{}},
		"usage unknown":              {Evaluator: cli, Chunks: 2, Attempts: 2},
		"in 1, out 2, cache read 3, cache write 4": {Evaluator: cli, Chunks: 1, Attempts: 1, Usage: &sessiondata.Usage{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4}},
		"in 1, out 0, cache read 0, cache write 0; 2 requests with usage unknown, not counted": {Evaluator: cli, Chunks: 1, Attempts: 1,
			Usage: &sessiondata.Usage{Input: 1}, Requests: &sessionjudge.Requests{Count: 3, Unknown: 2}},
	} {
		if got := stageUsage(h); got != want {
			t.Errorf("%q, want %q", got, want)
		}
	}
}

// printed runs f and returns what it wrote to standard output.
func printed(t *testing.T, f func()) string {
	t.Helper()
	return stdoutOf(t, f)
}

// What is printed is read while it is written: a pipe holds a few KB on
// Windows, so a report read only afterwards would wait for ever.
func TestAReportLargerThanAPipeIsRead(t *testing.T) {
	big := strings.Repeat("x", 1<<20)
	if out := printed(t, func() { fmt.Print(big) }); out != big {
		t.Fatalf("read %d bytes of %d", len(out), len(big))
	}
}

// What a person reads: the share of the conversation's cost only beside whole
// totals, and a stage with nothing to answer said to have made no call.
func TestWhatARunPrints(t *testing.T) {
	s := &sessionjudge.Summary{Money: ptr(4), Judging: &sessionjudge.Amount{Money: ptr(1)}, Score: &sessionjudge.Score{}}
	if out := printed(t, func() { printScore(s, nil) }); !strings.Contains(out, "25% of what the conversation cost") {
		t.Fatalf("whole totals:\n%s", out)
	}
	s.OutputUnknown = 1
	if out := printed(t, func() { printScore(s, nil) }); strings.Contains(out, "of what the conversation cost") {
		t.Fatalf("a share beside a total missing output:\n%s", out)
	}
	files := []*sessionjudge.File{{Header: sessionjudge.Header{Stage: "subtasks", Status: sessionjudge.StatusComplete,
		Evaluator: sessionjudge.Evaluator{Name: "opus", Kind: sessionjudge.EvaluatorCLI}, Usage: &sessiondata.Usage{}}}}
	if out := printed(t, func() { printRun(storage.JudgmentRun{Run: 1}, files) }); !strings.Contains(out, "subtasks     complete  by opus, no call, nothing to answer") {
		t.Fatalf("a stage with no call:\n%s", out)
	}
	// What sub-tasks caused is said beside the target's avoidable cost, and
	// per sub-task, the asking agent's work on its report included.
	one, zero := 1, int64(0)
	half := 0.5
	subs := &sessionjudge.File{Header: sessionjudge.Header{Stage: "subtasks", Coverage: &sessionjudge.Coverage{Subtasks: &one, SubtasksJudged: &one}},
		Subtasks: []sessionjudge.Subtask{{ID: "stream/a", Outcome: "not_achieved", Report: "inaccurate", Use: "misled", Dispatch: "reasonable"}}}
	closed := &sessionjudge.File{Header: sessionjudge.Header{Stage: "close", Summary: &sessionjudge.Summary{Score: &sessionjudge.Score{}}},
		Outcomes: []sessionjudge.Outcome{{Target: "t1", Statement: "push it", Avoidable: &sessionjudge.Amount{Usage: sessiondata.Usage{Input: 300}},
			AvoidableBySubtasks: &sessionjudge.Amount{Usage: sessiondata.Usage{Input: 200}}, AvoidableCauseUnknown: &sessionjudge.Amount{Usage: sessiondata.Usage{Input: 50}},
			NotReviewed: &sessionjudge.Amount{}, AvoidableSpan: &zero}},
		Costs: []sessionjudge.Cost{
			{Scope: sessionjudge.ScopeWork, Target: "t1", Work: sessionjudge.WorkMisled, Cause: "stream/a", Usage: sessiondata.Usage{Input: 400}, Share: &half},
			{Scope: sessionjudge.ScopeWork, Target: "t1", Work: sessionjudge.WorkRepeated, Cause: sessionjudge.CauseMain, Usage: sessiondata.Usage{Input: 100}},
			{Scope: sessionjudge.ScopeWork, Target: "t1", Work: sessionjudge.WorkMisled, Cause: sessionjudge.CauseUnknown, Usage: sessiondata.Usage{Input: 50}}}}
	out := printed(t, func() { printRun(storage.JudgmentRun{Run: 1}, []*sessionjudge.File{subs, closed}) })
	for _, want := range []string{"of which sub-tasks caused 200 tokens", "50 tokens of a cause the record does not show", "use misled 1",
		"stream/a caused 200 of avoidable work"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	// The main agent's own avoidable work is no sub-task's, nor is work of
	// a cause the record does not show.
	if strings.Contains(out, "main caused") || strings.Contains(out, "unknown caused") {
		t.Errorf("the main agent listed as a sub-task:\n%s", out)
	}
}

// A -plan pair without "=" names no evaluator. It is refused, never run as
// a stage given to no one.
func TestAPlanPairNeedsAnEvaluator(t *testing.T) {
	plan, err := judgePlan(config.Judge{}, "", "targets=claude, achievement=codex")
	if err != nil || !slices.Equal(plan, []config.JudgeStage{{Stage: "targets", Evaluator: "claude"}, {Stage: "achievement", Evaluator: "codex"}}) {
		t.Fatalf("plan %v, %v", plan, err)
	}
	if plan, err := judgePlan(config.Judge{}, "", "targets=claude,achievement"); err == nil ||
		!strings.Contains(err.Error(), `"achievement" is not stage=evaluator`) || plan != nil {
		t.Fatalf("plan %v, %v", plan, err)
	}
}

// publishStage writes one stage file into a run, as a judge run does.
func publishStage(t *testing.T, z *storage.Zone, runDir string, run, order int, stage string, summary *sessionjudge.Summary) {
	t.Helper()
	w, err := sessionjudge.NewWriter(sessionjudge.Header{Conversation: "s1", Session: "s1", Run: run, Stage: stage,
		Head:      sessionjudge.Head{Round: 3, Digest: strings.Repeat("a", 64)},
		Evaluator: sessionjudge.Evaluator{Name: "asz", Kind: sessionjudge.EvaluatorComputed, Program: "asz"},
		Started:   "2026-10-20T10:00:00Z", Finished: "2026-10-20T10:05:00Z", Status: sessionjudge.StatusComplete, Summary: summary})
	if err != nil {
		t.Fatal(err)
	}
	data, digest, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.PublishStage(z, "s1", runDir, order, stage, digest, data); err != nil {
		t.Fatal(err)
	}
}

// threeRuns makes three runs of session s1: the first has its ledger only,
// the second is closed, and the third has no stage yet.
func threeRuns(t *testing.T) *storage.Zone {
	t.Helper()
	z := storage.NewZone(t.TempDir())
	var dirs []string
	for range 3 {
		_, dir, err := storage.ReserveRun(z, "s1")
		if err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, dir)
	}
	publishStage(t, z, dirs[0], 1, 1, sessionjudge.StageLedger, nil)
	publishStage(t, z, dirs[1], 2, 1, sessionjudge.StageLedger, nil)
	publishStage(t, z, dirs[1], 2, 6, sessionjudge.StageClose, &sessionjudge.Summary{Targets: 2, Achieved: 1, NotAchieved: 1,
		Score: &sessionjudge.Score{Final: ptr(0.5)}})
	return z
}

// A run is listed from its close file. A run with no close file yet is
// unfinished, whatever other stages it has, and another stage's header is
// never read as a close.
func TestARunIsListedFromItsCloseFile(t *testing.T) {
	z := threeRuns(t)
	code := 0
	out := stdoutOf(t, func() { code = judgeList(z, "s1", "s1") })
	want := "run 1  unfinished, 1 stage files\n" +
		"run 2  round 3  2026-10-20T10:05:00Z  complete  targets 2: achieved 1, partly 0, not 1, cannot verify 0  score 50/100\n" +
		"run 3  unfinished, 0 stage files\n"
	if code != 0 || out != want {
		t.Fatalf("exit %d, listed:\n%s\nwant:\n%s", code, out, want)
	}
}

// -show prints the run asked for, and a run that is not there is an error,
// never another run printed in its place.
func TestShowPrintsTheRunAskedFor(t *testing.T) {
	z := threeRuns(t)
	code := 0
	out := stdoutOf(t, func() { code = judgeShow(z, "s1", "s1", 2, false) })
	if code != 0 || !strings.HasPrefix(out, "run 2  ") || !strings.Contains(out, "close ") || strings.Contains(out, "not closed") {
		t.Fatalf("exit %d, run 2 printed as:\n%s", code, out)
	}
	out = stdoutOf(t, func() { code = judgeShow(z, "s1", "s1", 1, false) })
	if code != 0 || !strings.HasPrefix(out, "run 1  ") || !strings.Contains(out, "not closed") {
		t.Fatalf("exit %d, run 1 printed as:\n%s", code, out)
	}
	// The run's latest regroup is shown with it, and the report follows the
	// person's goals.
	runs, _ := storage.JudgmentRuns(z, "s1")
	// The third regroup's call failed: it is shown, and the report follows
	// the latest one that compared anything.
	for version, statement := range []string{"the first grouping", "the latest grouping", ""} {
		status := sessionjudge.StatusComplete
		if statement == "" {
			status = sessionjudge.StatusFailed
		}
		w, err := sessionjudge.NewWriter(sessionjudge.Header{Conversation: "s1", Session: "s1", Run: 2, Stage: sessionjudge.StageRegroup,
			Head: sessionjudge.Head{Round: 3, Digest: strings.Repeat("a", 64)}, Evaluator: sessionjudge.Evaluator{Name: "stand-in", Kind: sessionjudge.EvaluatorCLI},
			Labels:  &sessionjudge.LabelFile{File: "goals.yaml", SHA256: strings.Repeat("c", 64), Version: version + 1},
			Started: "2026-10-20T10:00:00Z", Finished: "2026-10-20T10:05:00Z", Status: status})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Write(&sessionjudge.LabelText{From: 1, Text: "goals:\n  - " + statement + "\n"}); err != nil {
			t.Fatal(err)
		}
		if statement != "" {
			if err := w.Write(&sessionjudge.LabelGoal{ID: "P1", Statement: statement, Members: []string{}, Judge: []string{}, Class: sessionjudge.CompareAbsent,
				Cite: []sessionjudge.Cite{}}); err != nil {
				t.Fatal(err)
			}
		}
		data, digest, err := w.Close()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := storage.PublishRegroup(z, "s1", runs[1].Dir, version+1, digest, data); err != nil {
			t.Fatal(err)
		}
	}
	out = stdoutOf(t, func() { code = judgeShow(z, "s1", "s1", 2, false) })
	if code != 0 || !strings.Contains(out, "regroup      complete") || !strings.Contains(out, "regroup      failed") ||
		!strings.Contains(out, "goal P1  the latest grouping") || strings.Contains(out, "the first grouping") {
		t.Fatalf("exit %d, run 2 with its regroups printed as:\n%s", code, out)
	}
	said := stderrOf(t, func() { out = stdoutOf(t, func() { code = judgeShow(z, "s1", "s1", 4, false) }) })
	if code != 1 || out != "" || !strings.Contains(said, "s1 has no run 4") {
		t.Fatalf("exit %d for a run not there, printed %q, said %q", code, out, said)
	}
}

// The navigation server reads the conversation at the head its run judges,
// named by a round and that round's digest. A chain parsed again can reach
// the same round number with other content, so a digest that no longer
// matches stops the server before it answers anything.
func TestTheNavigationServesOnlyTheHeadItsRunJudges(t *testing.T) {
	root := t.TempDir()
	sc, err := scenario.Load(filepath.Join("..", "..", "tests", "scenarios", "fixture.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := scenario.Build(sc, scenario.FormatSD, root, scenario.Options{At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	z := storage.NewZone(root)
	for {
		r, err := parse.Session(z, parse.Options{Conversation: b.Session, Session: b.Session})
		if err != nil {
			t.Fatal(err)
		}
		if !r.Changed() || !r.More {
			break
		}
	}
	c, err := view.New(z, nil).LoadAt(b.Session, 1)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := c.BuildWithheld(nil)
	if err != nil {
		t.Fatal(err)
	}
	head := doc.Head.Digest
	// serve runs the server with one request waiting on its input.
	serve := func(digest string) (code int, out, said string) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.WriteString(`{"jsonrpc":"2.0","id":7,"method":"ping"}` + "\n")
		_ = w.Close()
		old := os.Stdin
		os.Stdin = r
		defer func() { os.Stdin = old; _ = r.Close() }()
		said = stderrOf(t, func() {
			out = stdoutOf(t, func() {
				code = cmdJudgeMCP([]string{"-root", root, "-conversation", b.Session, "-round", "1", "-digest", digest})
			})
		})
		return code, out, said
	}
	if code, out, said := serve(head); code != 0 || !strings.Contains(out, `"id":7`) {
		t.Fatalf("at its head: exit %d, answered %q, said %q", code, out, said)
	}
	stale := strings.Repeat("0", 64)
	code, out, said := serve(stale)
	if code != 1 || out != "" || !strings.Contains(said, "round 1 of "+b.Session+" is now "+head+", not the "+stale+" the run judges") {
		t.Fatalf("at a digest the round no longer has: exit %d, answered %q, said %q", code, out, said)
	}
}

// The navigation server is started only with everything it reads by: the
// root, the conversation, and the round and digest of the head it serves.
// Without any one of them it says how it is used and stops.
func TestTheNavigationServerNeedsItsHead(t *testing.T) {
	full := map[string]string{"-root": t.TempDir(), "-conversation": "c1", "-round": "1", "-digest": strings.Repeat("a", 64)}
	for missing := range full {
		var args []string
		for flag, value := range full {
			if flag != missing {
				args = append(args, flag, value)
			}
		}
		var code int
		said := stderrOf(t, func() { code = cmdJudgeMCP(args) })
		if code != 2 || !strings.Contains(said, "Usage:") {
			t.Errorf("without %s: exit %d, said %q", missing, code, said)
		}
	}
}

// The score line counts the targets the score is over, and names a replaced
// one apart; and a share of a call reads the same in a target's cost as in
// what is said about its calls.
func TestTheScoreAndCallsReadAsTheyAdd(t *testing.T) {
	one := 1.0
	s := &sessionjudge.Summary{Score: &sessionjudge.Score{Judged: 2, Achievement: &one}, Achieved: 2, NotAchieved: 1}
	outcomes := []sessionjudge.Outcome{
		{Target: "t1", Status: sessionjudge.TargetReplaced, Level: sessionjudge.LevelNotAchieved},
		{Target: "t2", Status: sessionjudge.TargetClosed, Level: sessionjudge.LevelAchieved},
		{Target: "t3", Status: sessionjudge.TargetClosed, Level: sessionjudge.LevelAchieved},
	}
	out := printed(t, func() { printScore(s, outcomes) })
	if !strings.Contains(out, "over 2 targets judged: achieved 2, partly 0, not achieved 0; 0 cannot be verified; 1 cancelled or replaced, not scored") {
		t.Fatalf("score:\n%s", out)
	}
	for f, want := range map[float64]string{60.5: "60.5", 2.5: "2.5", 0.5: "0.5", 61: "61"} {
		if got := share(f); got != want {
			t.Errorf("share(%v) = %s, want %s", f, got, want)
		}
	}
}
