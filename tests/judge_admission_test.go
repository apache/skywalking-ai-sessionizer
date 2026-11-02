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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp/otlptest"
	"github.com/apache/skywalking-ai-sessionizer/internal/judge"
	"github.com/apache/skywalking-ai-sessionizer/internal/retention"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
)

// stalled answers the targets stage as the stand-in does, and holds every
// later call until its context ends, as a model that never answers would.
type stalled struct{ started chan struct{} }

func (stalled) Describe() sessionjudge.Evaluator { return standIn{}.Describe() }

func (s stalled) Run(ctx context.Context, t judge.Task) (judge.Result, error) {
	if t.Stage == sessionjudge.StageTargets {
		return standIn{}.Run(ctx, t)
	}
	select {
	case s.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return judge.Result{}, ctx.Err()
}

func stalledRunner(z *storage.Zone) (*judge.Runner, chan struct{}) {
	started := make(chan struct{}, 1)
	r := runner(z)
	r.Executor = func(string, config.Evaluator) (judge.Executor, error) { return stalled{started: started}, nil }
	return r, started
}

// onlyRun is the conversation's one run, which must be closed and no
// longer active.
func onlyRun(t *testing.T, z *storage.Zone, id string) storage.JudgmentRun {
	t.Helper()
	runs, err := storage.JudgmentRuns(z, id)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs %+v, %v", runs, err)
	}
	state, held, err := storage.StateOf(&runs[0])
	if err != nil || state != storage.RunClosed || held {
		t.Fatalf("the run is %s, held %v, %v", state, held, err)
	}
	return runs[0]
}

// stageNotes are the stages a run published, each with its status and note.
func stageNotes(t *testing.T, run storage.JudgmentRun) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, f := range run.Files {
		h := readStage(t, f.Path).Header
		out[f.Stage] = h.Status + ": " + h.Note
	}
	return out
}

// A run that would judge more steps than judge.max_steps is refused before
// it is admitted: no run directory, no model call. Within the limit it runs.
func TestARunOverMaxStepsIsRefusedBeforeAnythingIsMade(t *testing.T) {
	z, id := builtThree(t)
	r := runner(z)
	r.Config.MaxSteps = 1
	if _, err := r.Run(context.Background(), id, 0, plan); err == nil || !strings.Contains(err.Error(), "judge.max_steps 1") {
		t.Fatalf("a run over the limit: %v", err)
	}
	if runs, err := storage.JudgmentRuns(z, id); err != nil || len(runs) != 0 {
		t.Fatalf("the refused run left %+v, %v", runs, err)
	}
	r.Config.MaxSteps = 10000
	if _, err := r.Run(context.Background(), id, 0, plan); err != nil {
		t.Fatalf("a run within the limit: %v", err)
	}
}

// A run past judge.run_timeout stops its work, publishes what finished,
// fails the stage it was in and starts no later one, and closes; its
// activity lock is let go.
func TestARunPastItsTimeLimitClosesWithWhatFinished(t *testing.T) {
	z, id := builtThree(t)
	r, _ := stalledRunner(z)
	r.Config.RunTimeout = "300ms"
	run, err := r.Run(context.Background(), id, 0, plan)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != sessionjudge.StatusPartial {
		t.Fatalf("the run closed %s", run.Status)
	}
	notes := stageNotes(t, onlyRun(t, z, id))
	if !strings.HasPrefix(notes["targets"], sessionjudge.StatusComplete) || !strings.HasPrefix(notes["achievement"], sessionjudge.StatusFailed) {
		t.Fatalf("stages %v", notes)
	}
	for _, later := range []string{"subtasks", "efficiency"} {
		if notes[later] != sessionjudge.StatusFailed+": not started: the run passed judge.run_timeout" {
			t.Fatalf("%s: %q", later, notes[later])
		}
	}
}

// An interrupt ends a run the same way, and while it runs no second run or
// regroup of the conversation is admitted.
func TestAnInterruptedRunClosesAndHoldsTheConversationUntilThen(t *testing.T) {
	z, id := builtThree(t)
	r, started := stalledRunner(z)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := r.Run(ctx, id, 0, plan)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Minute):
		t.Fatal("the run never reached its second stage")
	}
	if _, err := runner(z).Run(context.Background(), id, 0, plan); !errors.Is(err, storage.ErrJudgeActive) {
		t.Fatalf("a second run while one is active: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the interrupted run did not close")
	}
	notes := stageNotes(t, onlyRun(t, z, id))
	if notes["efficiency"] != sessionjudge.StatusFailed+": not started: the run was interrupted" {
		t.Fatalf("stages %v", notes)
	}
}

// Judge runs go to the receiver only when export.otlp.judgments asks: off,
// the landed files and rounds go and the closed run stays unsent, and turning
// it on sends it then.
func TestAJudgeRunIsSentOnlyWhenAsked(t *testing.T) {
	_, _, rcv, p := judgedAndPushed(t)
	p.Judgments = false
	if st, err := p.Pass(); err != nil || len(st.Errors) > 0 {
		t.Fatalf("pass %+v %v", st, err)
	}
	if got := sentStages(rcv); len(got) != 0 {
		t.Fatalf("with judgments off the run went: %v", got)
	}
	if len(otlptest.Records(rcv.Requests())) == 0 {
		t.Fatal("with judgments off nothing went at all")
	}
	rcv.Reset()
	p.Judgments = true
	if st, err := p.Pass(); err != nil || len(st.Errors) > 0 {
		t.Fatalf("pass %+v %v", st, err)
	}
	if got := sentStages(rcv); len(got) == 0 || got[len(got)-1] != "close" {
		t.Fatalf("once asked the run went as %v", got)
	}
}

// Retention takes nothing of a session while a run or regroup of it is
// active, at either level: the run reads its landed files and writes into
// its directory. Once the run lets its lock go the session is retired.
func TestRetentionWaitsForAnActiveRun(t *testing.T) {
	z, id := builtFixture(t)
	// A scenario root is never retired, so the root is made an ordinary one.
	_ = os.RemoveAll(filepath.Join(z.Root(), storage.ScenarioDir))
	active, err := storage.AdmitRun(context.Background(), z, id, func() (storage.RunAdmission, error) {
		return storage.RunAdmission{Trigger: storage.TriggerCommand}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	later := func() time.Time { return time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC) }
	for _, rem := range []*retention.Remover{
		{Zone: z, MetadataOnlyAfter: time.Hour, Now: later},
		{Zone: z, MetadataOnlyAfter: time.Hour, RetireAfter: time.Hour, Now: later},
	} {
		res := rem.Pass(retention.Input{})
		if len(res.Errors) > 0 || len(res.Retired) != 0 || len(res.Removed) != 0 {
			t.Fatalf("with a run active: %+v", res)
		}
	}
	if files, err := storage.LandedFiles(z, id); err != nil || len(files) == 0 || files[0].Skeleton {
		t.Fatalf("the landed files: %+v, %v", files, err)
	}
	if err := active.Release(); err != nil {
		t.Fatal(err)
	}
	rem := &retention.Remover{Zone: z, MetadataOnlyAfter: time.Hour, RetireAfter: time.Hour, Now: later}
	if res := rem.Pass(retention.Input{}); len(res.Errors) > 0 || len(res.Retired) != 1 {
		t.Fatalf("once the run ended: %+v", res)
	}
}

// stuck holds every call until its context ends, the targets stage's too.
type stuck struct{}

func (stuck) Describe() sessionjudge.Evaluator { return standIn{}.Describe() }

func (stuck) Run(ctx context.Context, _ judge.Task) (judge.Result, error) {
	<-ctx.Done()
	return judge.Result{}, ctx.Err()
}

// A run whose time limit passes during its targets stage publishes every
// later stage failed, not started, rather than leaving them out.
func TestARunStoppedInItsTargetsStageStillListsTheRest(t *testing.T) {
	z, id := builtThree(t)
	r := runner(z)
	r.Executor = func(string, config.Evaluator) (judge.Executor, error) { return stuck{}, nil }
	r.Config.RunTimeout = "300ms"
	if _, err := r.Run(context.Background(), id, 0, plan); err != nil {
		t.Fatal(err)
	}
	run := onlyRun(t, z, id)
	notes := stageNotes(t, run)
	if !strings.HasPrefix(notes["targets"], sessionjudge.StatusFailed) {
		t.Fatalf("targets: %q", notes["targets"])
	}
	for _, later := range []string{"achievement", "subtasks", "efficiency"} {
		if notes[later] != sessionjudge.StatusFailed+": not started: the run passed judge.run_timeout" {
			t.Fatalf("%s: %q", later, notes[later])
		}
	}
	// A stage builds on nothing it did not read: no targets answered.
	for _, f := range run.Files {
		for _, d := range readStage(t, f.Path).Header.BuildsOn {
			if d.Digest == "" {
				t.Fatalf("%s builds on %s with no digest", f.Stage, d.Stage)
			}
		}
	}
}

// The time limit counts from the admission, so a wait to bind a pulled
// run's lease is bounded by it too, and the run is left stopped.
func TestTheTimeLimitBoundsTheLeaseBinding(t *testing.T) {
	z, id := builtThree(t)
	r := runner(z)
	r.Config.RunTimeout = "300ms"
	r.Reserved = func(ctx context.Context, _ int) error {
		<-ctx.Done()
		return ctx.Err()
	}
	done := make(chan error, 1)
	go func() {
		_, err := r.Run(context.Background(), id, 0, plan)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a binding past the limit: %v", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the binding outlived the run's time limit")
	}
	runs, err := storage.JudgmentRuns(z, id)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs %+v, %v", runs, err)
	}
	if s, held, err := storage.StateOf(&runs[0]); err != nil || s != storage.RunStopped || held {
		t.Fatalf("the run is %s, held %v, %v", s, held, err)
	}
}

// Recovering a removal an earlier pass left waits for an active run too.
func TestARemovalsRecoveryWaitsForAnActiveRun(t *testing.T) {
	z, id := builtFixture(t)
	_ = os.RemoveAll(filepath.Join(z.Root(), storage.ScenarioDir))
	active, err := storage.AdmitRun(context.Background(), z, id, func() (storage.RunAdmission, error) {
		return storage.RunAdmission{Trigger: storage.TriggerCommand}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	files, err := storage.LandedFiles(z, id)
	if err != nil || len(files) == 0 {
		t.Fatalf("landed %+v, %v", files, err)
	}
	intent := &storage.Removing{Generation: 1, Through: files[len(files)-1].Seq, Phase: storage.RemovingSkeletons}
	if err := storage.SaveRemoving(z.RemovingPath(id), intent); err != nil {
		t.Fatal(err)
	}
	rem := &retention.Remover{Zone: z}
	if res := rem.Recover(); len(res.Errors) > 0 || len(res.Removed) != 0 {
		t.Fatalf("with a run active: %+v", res)
	}
	if left, err := storage.LoadRemoving(z.RemovingPath(id)); err != nil || left == nil {
		t.Fatalf("the intent: %+v, %v", left, err)
	}
	if err := active.Release(); err != nil {
		t.Fatal(err)
	}
	if res := rem.Recover(); len(res.Errors) > 0 || len(res.Removed) != 1 {
		t.Fatalf("once the run ended: %+v", res)
	}
}
