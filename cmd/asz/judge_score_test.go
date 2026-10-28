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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/judge"
	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
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
// totals, and as judging only the stages that asked an evaluator.
func TestWhatARunPrints(t *testing.T) {
	s := &sessionjudge.Summary{Money: ptr(4), Judging: &sessionjudge.Amount{Money: ptr(1)}, Score: &sessionjudge.Score{}}
	r := &runReport{summary: s}
	if got := r.usageWords(); !strings.Contains(got, "25% of what the conversation cost") {
		t.Fatalf("whole totals: %s", got)
	}
	s.OutputUnknown = 1
	if got := r.usageWords(); strings.Contains(got, "of what the conversation cost") {
		t.Fatalf("a share beside a total missing output: %s", got)
	}
	// A total that leaves something out says what.
	s.Judging.Unknown = 1
	if got := r.usageWords(); !strings.Contains(got, "not counting the output of 1 call that never finished") ||
		!strings.Contains(got[strings.Index(got, "judging used"):], "not counting 1 model request whose usage is unknown") {
		t.Fatalf("a partial total said as whole: %s", got)
	}
	cli := sessionjudge.Evaluator{Name: "opus", Kind: sessionjudge.EvaluatorCLI}
	r.headers = []sessionjudge.Header{
		{Stage: sessionjudge.StageLedger, Status: sessionjudge.StatusComplete, Evaluator: sessionjudge.Evaluator{Name: "asz", Kind: sessionjudge.EvaluatorComputed}},
		{Stage: sessionjudge.StageSubtasks, Status: sessionjudge.StatusComplete, Evaluator: cli},
		{Stage: sessionjudge.StageTargets, Status: sessionjudge.StatusComplete, Evaluator: sessionjudge.Evaluator{Name: "claude", Kind: sessionjudge.EvaluatorCLI,
			Model: "claude-fable-5-1"}, Attempts: 1},
	}
	if got := r.stageWords(); got != "every stage completed; judged by claude with claude-fable-5-1" {
		t.Fatalf("stages: %s", got)
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
	out := stdoutOf(t, func() { code = judgeList(z, "s1", "s1", false) })
	want := "run 1  unfinished, 1 stage files\n" +
		"run 2  round 3  2026-10-20T10:05:00Z  complete  targets 2: achieved 1, partly 0, not 1, cannot verify 0  score 50/100\n" +
		"run 3  unfinished, 0 stage files\n"
	if code != 0 || out != want {
		t.Fatalf("exit %d, listed:\n%s\nwant:\n%s", code, out, want)
	}
}

// -show prints the run asked for, or the latest closed one, and a run that is
// not there is an error, never another run printed in its place.
func TestShowPrintsTheRunAskedFor(t *testing.T) {
	z := threeRuns(t)
	code := 0
	out := stdoutOf(t, func() { code = judgeShow(z, "s1", "s1", 2, showOptions{format: formatText}) })
	if code != 0 || !strings.HasPrefix(out, "run 2 of conversation s1") || !strings.Contains(out, "overall result") || strings.Contains(out, "not closed") {
		t.Fatalf("exit %d, run 2 printed as:\n%s", code, out)
	}
	// With no run named, the latest closed one: run 3 is not closed.
	out = stdoutOf(t, func() { code = judgeShow(z, "s1", "s1", 0, showOptions{format: formatText}) })
	if code != 0 || !strings.HasPrefix(out, "run 2 of conversation s1") {
		t.Fatalf("exit %d, the latest closed run printed as:\n%s", code, out)
	}
	out = stdoutOf(t, func() { code = judgeShow(z, "s1", "s1", 1, showOptions{format: formatText}) })
	if code != 0 || !strings.HasPrefix(out, "run 1 ") || !strings.Contains(out, "the run is not closed") {
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
			Goals:   &sessionjudge.GoalsFile{File: "goals.yaml", SHA256: strings.Repeat("c", 64), Version: version + 1},
			Started: "2026-10-20T10:00:00Z", Finished: "2026-10-20T10:05:00Z", Status: status})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Write(&sessionjudge.GoalsText{From: 1, Text: "goals:\n  - " + statement + "\n"}); err != nil {
			t.Fatal(err)
		}
		if statement != "" {
			if err := w.Write(&sessionjudge.Goal{ID: "P1", Statement: statement, Members: []string{}, Judge: []string{}, Class: sessionjudge.CompareAbsent,
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
	out = stdoutOf(t, func() { code = judgeShow(z, "s1", "s1", 2, showOptions{format: formatText}) })
	if code != 0 || !strings.Contains(out, "stages not complete: regroup failed") ||
		!strings.Contains(out, "goal 1: the latest grouping") || strings.Contains(out, "the first grouping") {
		t.Fatalf("exit %d, run 2 with its regroups printed as:\n%s", code, out)
	}
	said := stderrOf(t, func() {
		out = stdoutOf(t, func() { code = judgeShow(z, "s1", "s1", 4, showOptions{format: formatText}) })
	})
	if code != 1 || out != "" || !strings.Contains(said, "s1 has no run 4") {
		t.Fatalf("exit %d for a run not there, printed %q, said %q", code, out, said)
	}
}

// A page over a pulled snapshot reads none of its steps, whether the snapshot
// was selected or given as the storage root: its evidence is read only
// through its checks.
func TestAPageOverASnapshotReadsNoSteps(t *testing.T) {
	p := pulledSnapshot(t)
	if pageSteps(p.root, nil) == nil {
		t.Fatal("a storage root's steps were not read")
	}
	if _, err := admitRoot("judge", rootConfig(p.snapshot), p.snapshot); err != nil {
		t.Fatal(err)
	}
	defer releaseAdmission()
	if pageSteps(p.snapshot, nil) != nil {
		t.Fatal("a snapshot given as the root was read without its checks")
	}
}

// The navigation server reads the conversation at the head its run judges,
// named by a round and that round's digest. A chain parsed again can reach
// the same round number with other content, so a digest that no longer
// matches stops the server before it answers anything.
// servedFixture is the fixture scenario built and parsed in a root, and its
// conversation's document at round 1, which a navigation server serves.
func servedFixture(t *testing.T) (root, session string, doc *sessionview.Conversation) {
	t.Helper()
	root = t.TempDir()
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
	doc, err = c.BuildWithheld(nil)
	if err != nil {
		t.Fatal(err)
	}
	return root, b.Session, doc
}

// serveMCP runs the navigation server with some requests waiting on its
// input, which then ends.
func serveMCP(t *testing.T, args []string, requests ...string) (code int, out, said string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range requests {
		_, _ = w.WriteString(req + "\n")
	}
	_ = w.Close()
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old; _ = r.Close() }()
	said = stderrOf(t, func() {
		out = stdoutOf(t, func() { code = cmdJudgeMCP(args) })
	})
	return code, out, said
}

func TestTheNavigationServesOnlyTheHeadItsRunJudges(t *testing.T) {
	root, session, doc := servedFixture(t)
	head := doc.Head.Digest
	serve := func(digest string) (int, string, string) {
		return serveMCP(t, []string{"-root", root, "-conversation", session, "-round", "1", "-digest", digest},
			`{"jsonrpc":"2.0","id":7,"method":"ping"}`)
	}
	if code, out, said := serve(head); code != 0 || !strings.Contains(out, `"id":7`) {
		t.Fatalf("at its head: exit %d, answered %q, said %q", code, out, said)
	}
	stale := strings.Repeat("0", 64)
	code, out, said := serve(stale)
	if code != 1 || out != "" || !strings.Contains(said, "round 1 of "+session+" is now "+head+", not the "+stale+" the run judges") {
		t.Fatalf("at a digest the round no longer has: exit %d, answered %q, said %q", code, out, said)
	}
}

// The navigation server logs each answer it gave with its attempt's name and
// its receipt, and closes the log with how many it gave. The run reads that
// log back strictly: its own attempt's receipts count, and another
// attempt's log gives none.
func TestTheServersLogHoldsItsAttemptsReceipts(t *testing.T) {
	root, session, doc := servedFixture(t)
	dir := t.TempDir()
	task, log := filepath.Join(dir, "task.json"), filepath.Join(dir, "mcp-calls.jsonl")
	attempt := "j000001/5-divergence/c001/a1/test"
	b, _ := json.Marshal(judge.Assignment{Attempt: attempt})
	if err := os.WriteFile(task, b, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, said := serveMCP(t, []string{"-root", root, "-conversation", session, "-round", "1", "-digest", doc.Head.Digest,
		"-task", task, "-log", log},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"talks","arguments":{}}}`)
	if code != 0 || !strings.Contains(out, `"id":1`) {
		t.Fatalf("exit %d, answered %q, said %q", code, out, said)
	}
	written, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(written)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], `"closing":true`) || !strings.Contains(lines[1], `"answers":1`) {
		t.Fatalf("log:\n%s", written)
	}
	given := func(attempt string) *sessionjudge.Given {
		nav := judge.NewNav(doc, judge.Compute(doc, "claude-code/1", nil, nil), nil)
		nav.TakeLog(log, attempt)
		return nav.GivenWindow()
	}
	if g := given(attempt); g.Counts == nil || g.Counts.Listed == 0 {
		t.Fatalf("the attempt's own log: %+v %+v", g, g.Counts)
	}
	if g := given("another"); g.Counts != nil || !strings.Contains(g.Reason, "not this attempt's") {
		t.Fatalf("another attempt's log: %+v", g)
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
// one apart.
func TestTheScoreReadsAsItAdds(t *testing.T) {
	one := 1.0
	r := &runReport{summary: &sessionjudge.Summary{Score: &sessionjudge.Score{Judged: 2, Achievement: &one}}, outcomes: map[string]*sessionjudge.Outcome{
		"t1": {Target: "t1", Status: sessionjudge.TargetReplaced, Level: sessionjudge.LevelNotAchieved},
		"t2": {Target: "t2", Status: sessionjudge.TargetClosed, Level: sessionjudge.LevelAchieved},
		"t3": {Target: "t3", Status: sessionjudge.TargetClosed, Level: sessionjudge.LevelAchieved},
	}}
	want := "by the achievement record: 100 of 100 over 2 targets: 2 achieved, 0 partly, 0 not achieved, 0 cannot be verified; 1 cancelled or replaced, not scored"
	if got := r.assessment()[0]; got != want {
		t.Fatalf("score: %s", got)
	}
}
