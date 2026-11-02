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

package tests_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/judge"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
)

// policyRoot is a built conversation of three requests in an ordinary root,
// the policy over it, and the facts a complete pass gives of it.
type policyRoot struct {
	t      *testing.T
	z      *storage.Zone
	id     string
	p      *judge.Policy
	facts  judge.Facts
	origin string
	exec   func(string, config.Evaluator) (judge.Executor, error)
}

func newPolicyRoot(t *testing.T) *policyRoot {
	t.Helper()
	z, id := builtThree(t)
	// A scenario root is never judged on its own, so the root is made an
	// ordinary one.
	_ = os.RemoveAll(filepath.Join(z.Root(), storage.ScenarioDir))
	pr := &policyRoot{t: t, z: z, id: id, origin: "outside"}
	pr.refresh()
	cfg := runner(z).Config
	cfg.Plan, cfg.MaxSteps, cfg.RunTimeout = plan, 10000, "1h"
	cfg.Server = config.JudgeServer{Enabled: true, Since: "2025-12-01", IdleAfter: "2h", RunsPerDay: 10}
	pr.p = &judge.Policy{Zone: z, Config: cfg,
		NewRunner: func() *judge.Runner {
			r := runner(z)
			r.Config = cfg
			if pr.exec != nil {
				r.Executor = pr.exec
			}
			return r
		},
		Facts:  func() map[string]judge.Facts { return map[string]judge.Facts{id: pr.facts} },
		Origin: func(string) (string, error) { return pr.origin, nil },
		Now:    func() time.Time { return time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC) },
	}
	return pr
}

// refresh gives the facts a complete pass gives: the head and the highest
// landed sequence.
func (pr *policyRoot) refresh() {
	pr.t.Helper()
	files, err := sessionflow.OpenChain(pr.z.Root(), pr.id).List()
	if err != nil || len(files) == 0 {
		pr.t.Fatalf("chain %v %v", files, err)
	}
	_, landed, err := storage.Collected(pr.z, pr.id)
	if err != nil {
		pr.t.Fatal(err)
	}
	last := files[len(files)-1]
	pr.facts = judge.Facts{Outcome: storage.OutcomeComplete, Round: last.Round, Digest: last.Digest, Landed: landed}
}

// restart is a new server over the same root: a policy that has looked at
// nothing yet.
func (pr *policyRoot) restart() {
	old := pr.p
	pr.p = &judge.Policy{Zone: old.Zone, Config: old.Config, NewRunner: old.NewRunner, Facts: old.Facts, Origin: old.Origin, Now: old.Now}
}

// tick gives the policy its turn and waits for the run it started.
func (pr *policyRoot) tick() judge.PolicyStatus {
	pr.t.Helper()
	pr.p.Tick(context.Background())
	pr.p.Wait()
	return pr.p.Status()
}

func (pr *policyRoot) receipts() []storage.Receipt {
	pr.t.Helper()
	rs, err := storage.Receipts(pr.z, pr.id)
	if err != nil {
		pr.t.Fatal(err)
	}
	return rs
}

// A quiet conversation whose collection and parse are complete is judged
// once, in a run its receipt names and its admission names back, and judged
// again only when its views change.
func TestThePolicyJudgesAQuietConversationOnce(t *testing.T) {
	pr := newPolicyRoot(t)
	st := pr.tick()
	if st.LastError != "" {
		t.Fatalf("status %+v", st)
	}
	rs := pr.receipts()
	if len(rs) != 1 || rs[0].Kind != storage.ReceiptRun || rs[0].Run != 1 || len(rs[0].Talks) != 3 || rs[0].Landed != pr.facts.Landed {
		t.Fatalf("receipts %+v", rs)
	}
	runs, err := storage.JudgmentRuns(pr.z, pr.id)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs %+v %v", runs, err)
	}
	a, err := storage.ReadRunRecord(runs[0].Dir)
	if err != nil || a.Trigger != storage.TriggerServer || a.Receipt != 1 || a.Landed != pr.facts.Landed || a.Target == 0 {
		t.Fatalf("admission %+v %v", a, err)
	}
	if st := pr.tick(); st.Due != 0 || st.LeftOut["nothing new to judge"] != 1 {
		t.Fatalf("after its run: %+v", st)
	}
	if len(pr.receipts()) != 1 {
		t.Fatal("judged twice with nothing new")
	}
}

// A conversation is not judged while something was collected for it in the
// last idle_after, while its collection was not complete, or while it may
// have worked inside the storage root.
func TestAConversationNotQuietCompleteOrOutsideIsNotJudged(t *testing.T) {
	for name, set := range map[string]func(*policyRoot){
		"not quiet yet": func(pr *policyRoot) {
			pr.p.Now = func() time.Time { return time.Date(2026, 1, 1, 0, 30, 0, 0, time.UTC) }
		},
		"collection not complete":                      func(pr *policyRoot) { pr.facts.Outcome = storage.OutcomeIncomplete },
		"worked inside the storage root, or not known": func(pr *policyRoot) { pr.origin = "unknown" },
		"parse not complete":                           func(pr *policyRoot) { pr.facts.Landed++ },
	} {
		t.Run(name, func(t *testing.T) {
			pr := newPolicyRoot(t)
			set(pr)
			if st := pr.tick(); st.Due != 0 || st.LeftOut[name] != 1 {
				t.Fatalf("status %+v", st)
			}
			if len(pr.receipts()) != 0 {
				t.Fatal("admitted")
			}
		})
	}
}

// failing answers the targets stage and fails every later call.
type failing struct{}

func (failing) Describe() sessionjudge.Evaluator { return standIn{}.Describe() }

func (failing) Run(ctx context.Context, t judge.Task) (judge.Result, error) {
	if t.Stage == sessionjudge.StageTargets {
		return standIn{}.Run(ctx, t)
	}
	return judge.Result{}, errors.New("the model is down")
}

// A run that closed partial covers nothing, and is not tried again until
// something new lands for the session.
func TestAPartialRunIsNotTriedAgainUntilSomethingLands(t *testing.T) {
	pr := newPolicyRoot(t)
	pr.exec = func(string, config.Evaluator) (judge.Executor, error) { return failing{}, nil }
	pr.tick()
	if rs := pr.receipts(); len(rs) != 1 {
		t.Fatalf("receipts %+v", rs)
	}
	pr.exec = nil
	if st := pr.tick(); st.LeftOut["the last run did not succeed, and nothing new landed"] != 1 {
		t.Fatalf("status %+v", st)
	}
	if len(pr.receipts()) != 1 {
		t.Fatal("tried again with nothing new")
	}
}

// A window takes whole talks while they fit judge.max_steps, and the rest is
// judged by the next run; a first talk larger than the limit alone is
// skipped, covered by a receipt that calls no model.
func TestMaxStepsCutsTheWindowAndSkipsATalkTooLarge(t *testing.T) {
	pr := newPolicyRoot(t)
	pr.p.Config.MaxSteps = 1
	pr.tick()
	rs := pr.receipts()
	if len(rs) == 0 || rs[0].Kind != storage.ReceiptSkip || len(rs[0].Talks) != 1 {
		t.Fatalf("receipts %+v", rs)
	}
	if runs, _ := storage.JudgmentRuns(pr.z, pr.id); len(runs) != 0 {
		t.Fatalf("a skip made a run: %+v", runs)
	}
	pr2 := newPolicyRoot(t)
	pr2.p.Config.MaxSteps = 10000
	steps := 0
	pr2.tick()
	for _, r := range pr2.receipts() {
		steps = r.Steps
	}
	pr3 := newPolicyRoot(t)
	pr3.p.Config.MaxSteps = steps - 1
	pr3.tick()
	first := pr3.receipts()
	if len(first) != 1 || first[0].Kind != storage.ReceiptRun || len(first[0].Talks) >= 3 {
		t.Fatalf("a cut window: %+v", first)
	}
	pr3.tick()
	second := pr3.receipts()
	if len(second) != 2 || second[1].Talks[0].ID == first[0].Talks[0].ID {
		t.Fatalf("the next part: %+v", second)
	}
}

// A person's run gives no coverage: the policy judges the conversation as
// if no run had been made.
func TestAManualRunGivesNoCoverage(t *testing.T) {
	pr := newPolicyRoot(t)
	if _, err := runner(pr.z).Run(context.Background(), pr.id, 0, plan); err != nil {
		t.Fatal(err)
	}
	pr.tick()
	if rs := pr.receipts(); len(rs) != 1 || rs[0].Run != 2 || len(rs[0].Talks) != 3 {
		t.Fatalf("receipts %+v", rs)
	}
}

// A receipt is read with the run directory its admission binds to it: a run
// that took the number of a receipt whose directory was never made is not
// that receipt's, and the receipt covers nothing.
func TestAReceiptCountsOnlyThroughItsOwnRun(t *testing.T) {
	pr := newPolicyRoot(t)
	l, err := storage.LockRootContext(context.Background(), pr.z.Root())
	if err != nil {
		t.Fatal(err)
	}
	conv, err := view.New(pr.z, nil).Load(pr.id)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := conv.Build()
	if err != nil {
		t.Fatal(err)
	}
	// The receipt names every talk at its view, as a run that succeeded
	// would.
	var talks []storage.ReceiptTalk
	for id, v := range judge.TalkViews(doc) {
		talks = append(talks, storage.ReceiptTalk{ID: id, View: v})
	}
	rec := &storage.Receipt{Kind: storage.ReceiptRun, Conversation: pr.id, Session: pr.id, Time: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano),
		Round: pr.facts.Round, Digest: pr.facts.Digest, Landed: pr.facts.Landed - 1, Run: 1, Parser: doc.Parser, Policy: doc.Policy, Talks: talks}
	if err := storage.WriteReceipt(pr.z, rec); err != nil {
		t.Fatal(err)
	}
	_ = l.Unlock()
	// A person's run takes number 1.
	if _, err := runner(pr.z).Run(context.Background(), pr.id, 0, plan); err != nil {
		t.Fatal(err)
	}
	pr.tick()
	rs := pr.receipts()
	if len(rs) != 2 || rs[1].Run != 2 || len(rs[1].Talks) != 3 {
		t.Fatalf("receipts %+v", rs)
	}
}

// The policy's scope starts when it was first turned on, written once and
// read back by a later server.
func TestSinceIsWrittenOnce(t *testing.T) {
	pr := newPolicyRoot(t)
	pr.p.Config.Server.Since = ""
	if st := pr.tick(); st.LeftOut["nothing new to judge"] != 1 {
		t.Fatalf("history before the policy was judged: %+v", st)
	}
	first, ok, err := storage.LoadSince(pr.z)
	if err != nil || !ok {
		t.Fatalf("since %v %v", ok, err)
	}
	pr.p.Now = func() time.Time { return time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC) }
	pr.tick()
	if again, _, _ := storage.LoadSince(pr.z); !again.Equal(first) {
		t.Fatalf("since moved from %s to %s", first, again)
	}
}

// The day's allowance counts run receipts, whether their runs exist or not.
func TestTheDailyAllowanceIsSpentByReceipts(t *testing.T) {
	pr := newPolicyRoot(t)
	pr.p.Config.Server.RunsPerDay = 1
	l, err := storage.LockRootContext(context.Background(), pr.z.Root())
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.WriteReceipt(pr.z, &storage.Receipt{Kind: storage.ReceiptRun, Conversation: "other", Session: "other",
		Time: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano), Run: 1}); err != nil {
		t.Fatal(err)
	}
	_ = l.Unlock()
	if st := pr.tick(); st.Admitted != 1 || st.Due != 1 {
		t.Fatalf("status %+v", st)
	}
	if len(pr.receipts()) != 0 {
		t.Fatal("admitted past the allowance")
	}
}

// What asz server cannot judge with on its own is refused before it starts.
func TestThePolicyRefusesWhatNobodyWatches(t *testing.T) {
	good := runner(nil).Config
	good.Plan, good.MaxSteps, good.RunTimeout = plan, 3000, "2h"
	good.Server = config.JudgeServer{Enabled: true, IdleAfter: "2h", RunsPerDay: 10}
	if err := judge.CheckPolicy(good); err != nil {
		t.Fatalf("a good configuration: %v", err)
	}
	for name, change := range map[string]func(*config.Judge){
		"idle under 10m": func(j *config.Judge) { j.Server.IdleAfter = "5m" },
		"no step limit":  func(j *config.Judge) { j.MaxSteps = 0 },
		"no time limit":  func(j *config.Judge) { j.RunTimeout = "0" },
		"a reference stage": func(j *config.Judge) {
			j.Plan = append(append([]config.JudgeStage{}, plan...), config.JudgeStage{Stage: "reference", Evaluator: "stand-in"})
		},
		"kiro-cli": func(j *config.Judge) {
			j.Evaluators = map[string]config.Evaluator{"stand-in": {Kind: "cli", Program: "kiro-cli"}}
		},
		"an unknown evaluator": func(j *config.Judge) { j.Plan = []config.JudgeStage{{Stage: "targets", Evaluator: "nobody"}} },
	} {
		j := good
		change(&j)
		if err := judge.CheckPolicy(j); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// rewriteReceipt changes a receipt file in place, as an older asz would have
// written it.
func rewriteReceipt(t *testing.T, z *storage.Zone, conversation string, number int, change func(*storage.Receipt)) {
	t.Helper()
	path := filepath.Join(z.Root(), storage.JudgeDir, "runs", conversation, fmt.Sprintf("%06d.json", number))
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var r storage.Receipt
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	change(&r)
	b, _ = json.Marshal(r)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o444); err != nil {
		t.Fatal(err)
	}
}

// Views under two interpretations of the chain cannot be compared: a talk
// judged under an older one is judged again only when a record of its own
// landed after it was judged.
func TestAnUpgradeJudgesATalkAgainOnlyForItsOwnRecords(t *testing.T) {
	pr := newPolicyRoot(t)
	pr.tick()
	rewriteReceipt(t, pr.z, pr.id, 1, func(r *storage.Receipt) {
		r.Parser = "v0"
		for i := range r.Talks {
			r.Talks[i].View = "a view taken under v0"
		}
	})
	pr.restart()
	if st := pr.tick(); st.LeftOut["nothing new to judge"] != 1 {
		t.Fatalf("after an upgrade alone: %+v", st)
	}
	rewriteReceipt(t, pr.z, pr.id, 1, func(r *storage.Receipt) { r.Landed = 0 })
	pr.restart()
	pr.tick()
	if rs := pr.receipts(); len(rs) != 2 {
		t.Fatalf("with records of their own landed since: %+v", rs)
	}
}

// Two servers on one root never run two automatic runs at once: the
// admission takes the root's automatic lock without waiting, and one held
// refuses it before its receipt is written.
func TestTwoServersOnOneRootTakeTurns(t *testing.T) {
	pr := newPolicyRoot(t)
	held, err := storage.TryLockAutomatic(pr.z)
	if err != nil {
		t.Fatal(err)
	}
	if st := pr.tick(); !strings.Contains(st.LastError, "another server's automatic run") {
		t.Fatalf("status %+v", st)
	}
	if len(pr.receipts()) != 0 {
		t.Fatal("a receipt was written")
	}
	_ = held.Unlock()
	pr.tick()
	if len(pr.receipts()) != 1 {
		t.Fatal("not admitted once the lock was let go")
	}
}

// The admission asks again, under the root lock, what the list decided
// from: a pass that found a line unfinished meanwhile keeps the run out, and
// no receipt is written.
func TestTheAdmissionReadsTheFactsAgain(t *testing.T) {
	pr := newPolicyRoot(t)
	calls := 0
	pr.p.Facts = func() map[string]judge.Facts {
		calls++
		f := pr.facts
		if calls > 1 {
			f.Outcome = storage.OutcomeIncomplete
		}
		return map[string]judge.Facts{pr.id: f}
	}
	pr.tick()
	if calls < 2 || len(pr.receipts()) != 0 {
		t.Fatalf("admitted after the facts changed: %d calls, receipts %+v", calls, pr.receipts())
	}
}

// A skipped talk stays listed after a restart: the skips are read from the
// receipts.
func TestSkippedTalksAreReadFromTheReceipts(t *testing.T) {
	pr := newPolicyRoot(t)
	pr.p.Config.MaxSteps = 1
	pr.tick()
	pr.restart()
	pr.p.Config.MaxSteps = 1
	if st := pr.tick(); len(st.Skipped) == 0 {
		t.Fatalf("after a restart: %+v", st)
	}
}
