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
	"bytes"
	"context"
	"errors"
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

// snapshotOf binds a built root to its conversation's head, as a pulled
// snapshot's marker and frozen manifest bind a command admitted to one.
func snapshotOf(t *testing.T, z *storage.Zone, id string) *view.Snapshot {
	t.Helper()
	v, err := sessionflow.OpenChain(z.Root(), id).Fold()
	if err != nil {
		t.Fatal(err)
	}
	files, err := storage.LandedFiles(z, v.Session)
	if err != nil {
		t.Fatal(err)
	}
	landed := map[uint64]string{}
	for _, lf := range files {
		rel, err := filepath.Rel(z.Root(), lf.Path)
		if err != nil {
			t.Fatal(err)
		}
		landed[lf.Seq] = filepath.ToSlash(rel)
	}
	return &view.Snapshot{Conversation: id, Session: v.Session, Round: v.Round, Digest: v.Digest, Landed: landed}
}

// strictRunner is a runner over a snapshot, as an admitted judge has one.
// Its navigation servers are given their attempt's marker.
func strictRunner(t *testing.T, z *storage.Zone, id, lease string) *judge.Runner {
	t.Helper()
	r := runner(z)
	r.Snapshot = snapshotOf(t, z, id)
	r.Failures = func() string { return lease }
	r.MCP = func(a judge.MCPArgs) []string { return []string{"stand-in", a.Failure} }
	return r
}

// damage cuts the closing line off a landed file, so a read of it never
// completes.
func damage(z *storage.Zone, rel string) error {
	path := filepath.Join(z.Root(), filepath.FromSlash(rel))
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := bytes.SplitAfter(bytes.TrimSuffix(b, []byte("\n")), []byte("\n"))
	if err := os.Chmod(path, 0o644); err != nil {
		return err
	}
	return os.WriteFile(path, bytes.Join(lines[:len(lines)-1], nil), 0o644)
}

// markersUnder lists the failure markers and gates under a run's work
// directory.
func markersUnder(t *testing.T, dir string) (markers, gates []string) {
	t.Helper()
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case strings.HasPrefix(d.Name(), storage.FailurePrefix):
			markers = append(markers, p)
		case strings.HasPrefix(d.Name(), ".snapshot-check-"):
			gates = append(gates, p)
		}
		return nil
	})
	return markers, gates
}

// reads reads a record the document cites before answering, as an evaluator
// does through its navigation.
type reads struct{ standIn }

func (e reads) Run(ctx context.Context, t judge.Task) (judge.Result, error) {
	if t.Nav != nil {
		if _, err := t.Nav.Call("record", []byte(`{"seq": 1, "row": 1}`)); err != nil {
			return judge.Result{}, err
		}
	}
	return e.standIn.Run(ctx, t)
}

// A run over a snapshot reads under its attempts' markers, removes each once
// its read ended well, and judges what an ordinary root's run judges.
func TestASnapshotRunReadsUnderMarkers(t *testing.T) {
	z, id := builtFixture(t)
	r := strictRunner(t, z, id, "lease1")
	r.Executor = func(string, config.Evaluator) (judge.Executor, error) { return reads{}, nil }
	run, err := r.Run(context.Background(), id, 0, plan)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != sessionjudge.StatusComplete {
		t.Fatalf("run %s: %s", run.Status, notes(t, run))
	}
	markers, gates := markersUnder(t, run.Dir)
	if len(markers) != 0 || len(gates) == 0 {
		t.Fatalf("markers %v, gates %v", markers, gates)
	}
	for _, g := range gates {
		if filepath.Base(g) != ".snapshot-check-lease1.lock" || !strings.HasPrefix(filepath.Base(filepath.Dir(g)), "c") {
			t.Fatalf("a gate %s outside a chunk directory, or of another lease", g)
		}
	}
	ordinary := runner(z)
	ordinary.Executor = r.Executor
	want, err := ordinary.Run(context.Background(), id, 0, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(want.Files) != len(run.Files) || want.Status != run.Status {
		t.Fatalf("a snapshot run published %d files %s, an ordinary one %d %s", len(run.Files), run.Status, len(want.Files), want.Status)
	}
}

// ignoresDamage damages the snapshot before its first read, then answers
// as though nothing happened, as an evaluator that ignores a tool's error
// does.
type ignoresDamage struct {
	standIn
	z    *storage.Zone
	file string
	// lose removes the fatal marker, as a report that could not be
	// written leaves the parent nothing to read.
	lose bool
}

func (e ignoresDamage) Run(ctx context.Context, t judge.Task) (judge.Result, error) {
	if t.Stage == sessionjudge.StageTargets && filepath.Base(t.Dir) == "a1" {
		if err := damage(e.z, e.file); err != nil {
			return judge.Result{}, err
		}
		if _, err := t.Nav.Call("record", []byte(`{"seq": 1, "row": 1}`)); err == nil {
			return judge.Result{}, errors.New("a damaged record was read")
		}
		if e.lose {
			markers, _ := filepath.Glob(filepath.Join(t.Dir, storage.FailurePrefix+"*"))
			for _, m := range markers {
				if err := os.Remove(m); err != nil {
					return judge.Result{}, err
				}
			}
		}
	}
	return e.standIn.Run(ctx, t)
}

// An in-process evaluator that ignores a fatal read and answers well still
// fails the run, at once and without another attempt, and publishes nothing
// of the stage it answered.
func TestAFatalReadAnEvaluatorIgnoresFailsTheRun(t *testing.T) {
	z, id := builtFixture(t)
	r := strictRunner(t, z, id, "lease1")
	r.Executor = func(string, config.Evaluator) (judge.Executor, error) {
		return ignoresDamage{z: z, file: r.Snapshot.Landed[1]}, nil
	}
	if _, err := r.Run(context.Background(), id, 0, plan); !errors.Is(err, view.ErrEvidence) {
		t.Fatalf("the run: %v", err)
	}
	runs, err := storage.JudgmentRuns(z, id)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs %v %v", runs, err)
	}
	markers, _ := markersUnder(t, runs[0].Dir)
	if len(markers) != 1 || filepath.Base(filepath.Dir(markers[0])) != "a1" {
		t.Fatalf("markers %v", markers)
	}
	if b, _ := os.ReadFile(markers[0]); !strings.HasPrefix(string(b), "fatal: ") {
		t.Fatalf("the marker says %q", b)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(markers[0])), "a2")); err == nil {
		t.Fatal("a fatal read was tried again")
	}
	for _, f := range runs[0].Files {
		if f.Stage == sessionjudge.StageTargets || f.Stage == sessionjudge.StageClose {
			t.Fatalf("published %s after a fatal read", f.Path)
		}
	}

	// The run fails just the same when the fatal read's report is lost.
	z, id = builtFixture(t)
	r = strictRunner(t, z, id, "lease1")
	r.Executor = func(string, config.Evaluator) (judge.Executor, error) {
		return ignoresDamage{z: z, file: r.Snapshot.Landed[1], lose: true}, nil
	}
	if _, err := r.Run(context.Background(), id, 0, plan); !errors.Is(err, view.ErrEvidence) {
		t.Fatalf("a fatal read without its report: %v", err)
	}
}

// leavesMarker stands for a navigation server: it leaves what it was given
// to leave at its attempt's marker, then answers well and exits 0.
type leavesMarker struct {
	standIn
	leave func(path string) error
}

func (e leavesMarker) Run(ctx context.Context, t judge.Task) (judge.Result, error) {
	if t.Stage == sessionjudge.StageTargets && len(t.MCP) == 2 {
		if err := e.leave(t.MCP[1]); err != nil {
			return judge.Result{}, err
		}
	}
	return e.standIn.Run(ctx, t)
}

// Whatever a navigation server leaves at its marker fails the run: a child
// killed mid-read leaves it empty, a fatal one leaves its report, and
// anything else there is not ruled out either.
func TestAMarkerAServerLeavesFailsTheRun(t *testing.T) {
	for name, leave := range map[string]func(string) error{
		"pending":    func(p string) error { return os.WriteFile(p, nil, 0o600) },
		"fatal":      func(p string) error { return os.WriteFile(p, []byte("fatal: damaged\n"), 0o600) },
		"malformed":  func(p string) error { return os.WriteFile(p, []byte{0xff, 0, 1}, 0o600) },
		"unreadable": func(p string) error { return os.WriteFile(p, []byte("fatal: x\n"), 0o000) },
		"directory":  func(p string) error { return os.Mkdir(p, 0o755) },
	} {
		t.Run(name, func(t *testing.T) {
			z, id := builtFixture(t)
			r := strictRunner(t, z, id, "lease1")
			r.Executor = func(string, config.Evaluator) (judge.Executor, error) { return leavesMarker{leave: leave}, nil }
			_, err := r.Run(context.Background(), id, 0, plan)
			if err == nil || !strings.Contains(err.Error(), "did not finish") {
				t.Fatalf("the run: %v", err)
			}
			runs, _ := storage.JudgmentRuns(z, id)
			if _, err := os.Stat(filepath.Join(runs[0].Dir, storage.WorkDir, "2-targets", "c001", "a2")); err == nil {
				t.Fatal("a chunk with a marker was tried again")
			}
		})
	}
}

// failsFirst fails the first attempt of the targets stage without reading,
// then leaves a pending marker from the second.
type failsFirst struct{ standIn }

func (e failsFirst) Run(ctx context.Context, t judge.Task) (judge.Result, error) {
	if t.Stage == sessionjudge.StageTargets {
		if filepath.Base(t.Dir) == "a1" {
			return judge.Result{}, errors.New("the model went away")
		}
		if err := os.WriteFile(t.MCP[1], nil, 0o600); err != nil {
			return judge.Result{}, err
		}
	}
	return e.standIn.Run(ctx, t)
}

// A retry gets a marker of its own, and the check before its answer covers
// every attempt the chunk was given.
func TestARetryIsCheckedWithItsChunk(t *testing.T) {
	z, id := builtFixture(t)
	r := strictRunner(t, z, id, "lease1")
	r.Executor = func(string, config.Evaluator) (judge.Executor, error) { return failsFirst{}, nil }
	if _, err := r.Run(context.Background(), id, 0, plan); err == nil || !strings.Contains(err.Error(), filepath.Join("a2", storage.FailurePrefix+"lease1")) {
		t.Fatalf("the run: %v", err)
	}
}

// siblings pauses chunk two's read, its pending marker in place, until chunk
// one has answered and been checked.
type siblings struct {
	standIn
	stage      string
	twoPending chan struct{}
}

func (e siblings) Run(ctx context.Context, t judge.Task) (judge.Result, error) {
	if t.Stage != e.stage {
		return e.standIn.Run(ctx, t)
	}
	chunk := filepath.Base(filepath.Dir(t.Dir))
	switch chunk {
	case "c002":
		if err := os.WriteFile(t.MCP[1], nil, 0o600); err != nil {
			return judge.Result{}, err
		}
		close(e.twoPending)
		// Chunk one's answer is written once it passed its check.
		one := filepath.Join(filepath.Dir(filepath.Dir(t.Dir)), "c001", "a1", "output.json")
		deadline := time.Now().Add(30 * time.Second)
		for !exists(one) {
			if time.Now().After(deadline) {
				return judge.Result{}, errors.New("chunk one never answered")
			}
			time.Sleep(5 * time.Millisecond)
		}
		if err := os.Remove(t.MCP[1]); err != nil {
			return judge.Result{}, err
		}
	case "c001":
		select {
		case <-e.twoPending:
		case <-time.After(30 * time.Second):
			return judge.Result{}, errors.New("chunk two never began")
		}
	}
	return e.standIn.Run(ctx, t)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// A chunk is checked for its own markers only: one chunk answers and is
// accepted while its sibling's read is pending, and never looks at it.
func TestASiblingsPendingReadIsNotChecked(t *testing.T) {
	z, id := builtThree(t)
	r := strictRunner(t, z, id, "lease1")
	r.Config.Mode, r.Config.SingleBytes, r.Config.ChunkBytes, r.Config.Concurrency = "stream", 1, 1, 2
	ev := siblings{stage: sessionjudge.StageEfficiency, twoPending: make(chan struct{})}
	r.Executor = func(string, config.Evaluator) (judge.Executor, error) { return ev, nil }
	run, err := r.Run(context.Background(), id, 0, plan)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != sessionjudge.StatusComplete {
		t.Fatalf("run %s: %s", run.Status, notes(t, run))
	}
	select {
	case <-ev.twoPending:
	default:
		t.Fatal("the stage had no second chunk")
	}
}

// A regroup reads under markers its own lease names, in its own work
// directory, beside a run that read under another.
func TestARegroupReadsUnderItsOwnLease(t *testing.T) {
	z, id := builtFixture(t)
	r := strictRunner(t, z, id, "lease1")
	r.Executor = func(string, config.Evaluator) (judge.Executor, error) { return reads{}, nil }
	run, err := r.Run(context.Background(), id, 0, plan)
	if err != nil {
		t.Fatal(err)
	}
	labels := filepath.Join(t.TempDir(), "goals.yaml")
	if err := os.WriteFile(labels, []byte("goals:\n  - build the parser\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := judge.ReadGoals(labels, "")
	if err != nil {
		t.Fatal(err)
	}
	runs, _ := storage.JudgmentRuns(z, id)
	r.Failures = func() string { return "lease2" }
	if _, _, err := r.Regroup(context.Background(), id, runs[0], l, "stand-in"); err != nil {
		t.Fatal(err)
	}
	markers, gates := markersUnder(t, filepath.Join(run.Dir, storage.WorkDir, "g1-regroup"))
	if len(markers) != 0 || len(gates) != 1 || filepath.Base(gates[0]) != ".snapshot-check-lease2.lock" {
		t.Fatalf("the regroup's markers %v, gates %v", markers, gates)
	}
}

// lateChild leaves a fatal marker at the targets chunk's path once that
// chunk was accepted, as a child that outlived its chunk's check does, and
// holds the next stage until it has.
type lateChild struct {
	standIn
	late chan struct{}
}

func (e lateChild) Run(ctx context.Context, t judge.Task) (judge.Result, error) {
	switch t.Stage {
	case sessionjudge.StageTargets:
		marker := t.MCP[1]
		go func() {
			defer close(e.late)
			answer := filepath.Join(filepath.Dir(marker), "output.json")
			for deadline := time.Now().Add(30 * time.Second); !exists(answer) && time.Now().Before(deadline); {
				time.Sleep(5 * time.Millisecond)
			}
			_ = os.WriteFile(marker, []byte("fatal: after the check\n"), 0o600)
		}()
	case sessionjudge.StageAchievement:
		<-e.late
	}
	return e.standIn.Run(ctx, t)
}

// A child that fails after its chunk was checked affects only itself: the
// accepted answer stands, and the run goes on to publish and close.
func TestALateChildChangesNothingAccepted(t *testing.T) {
	z, id := builtFixture(t)
	r := strictRunner(t, z, id, "lease1")
	ev := lateChild{late: make(chan struct{})}
	r.Executor = func(string, config.Evaluator) (judge.Executor, error) { return ev, nil }
	run, err := r.Run(context.Background(), id, 0, plan)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != sessionjudge.StatusComplete {
		t.Fatalf("run %s: %s", run.Status, notes(t, run))
	}
	markers, _ := markersUnder(t, run.Dir)
	if len(markers) != 1 || !strings.Contains(markers[0], filepath.Join("2-targets", "c001", "a1")) {
		t.Fatalf("markers %v", markers)
	}
}
