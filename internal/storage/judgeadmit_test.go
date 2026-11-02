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

package storage_test

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

func admit(z *storage.Zone, session string, a storage.RunAdmission) (*storage.Active, error) {
	return storage.AdmitRun(context.Background(), z, session, func() (storage.RunAdmission, error) { return a, nil })
}

// A conversation has one run or regroup at a time: a second admission is
// refused while the first holds its activity lock, a regroup of a closed run
// too, and both are admitted once it lets the lock go.
func TestAConversationHasOneRunAtATime(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	first, err := admit(z, "s1", storage.RunAdmission{Trigger: storage.TriggerCommand})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admit(z, "s1", storage.RunAdmission{}); !errors.Is(err, storage.ErrJudgeActive) {
		t.Fatalf("a second run while the first is active: %v", err)
	}
	if active, err := storage.JudgeActive(z, "s1"); err != nil || !active {
		t.Fatalf("the probe says active %v, %v", active, err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if active, err := storage.JudgeActive(z, "s1"); err != nil || active {
		t.Fatalf("after the release the probe says active %v, %v", active, err)
	}
	second, err := admit(z, "s1", storage.RunAdmission{})
	if err != nil || second.Number != 2 {
		t.Fatalf("the next run: %+v, %v", second, err)
	}
	if _, _, err := storage.AdmitRegroup(context.Background(), z, "s1", first.Dir); !errors.Is(err, storage.ErrJudgeActive) {
		t.Fatalf("a regroup while a run is active: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
	version, regroup, err := storage.AdmitRegroup(context.Background(), z, "s1", first.Dir)
	if err != nil || version != 1 {
		t.Fatalf("a regroup once nothing is active: %d, %v", version, err)
	}
	if _, err := admit(z, "s1", storage.RunAdmission{}); !errors.Is(err, storage.ErrJudgeActive) {
		t.Fatalf("a run while a regroup is active: %v", err)
	}
	if err := regroup.Release(); err != nil {
		t.Fatal(err)
	}
}

// A run is not admitted while a removal of its session's files has not
// finished: its intent is there until the pass that finishes it. Nothing is
// reserved.
func TestAdmissionWaitsForARemovalToFinish(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	if err := os.MkdirAll(z.SessionDir("s1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := storage.SaveRemoving(z.RemovingPath("s1"), &storage.Removing{Generation: 1, Through: 3, Phase: storage.RemovingSkeletons}); err != nil {
		t.Fatal(err)
	}
	if _, err := admit(z, "s1", storage.RunAdmission{}); !errors.Is(err, storage.ErrRemovalPending) {
		t.Fatalf("a run during a removal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(z.SessionDir("s1"), storage.JudgmentsDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a run directory was made: %v", err)
	}
}

// The selection function's refusal reserves nothing, and what it gives is
// what admission.json records, read-only, beside the run's activity lock.
func TestAdmissionRecordsWhatTheRunJudges(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	refused := errors.New("not due")
	if _, err := storage.AdmitRun(context.Background(), z, "s1", func() (storage.RunAdmission, error) {
		return storage.RunAdmission{}, refused
	}); !errors.Is(err, refused) {
		t.Fatalf("a refusal: %v", err)
	}
	if runs, err := storage.JudgmentRuns(z, "s1"); err != nil || len(runs) != 0 {
		t.Fatalf("a refused run left %v, %v", runs, err)
	}
	window := json.RawMessage(`{"from_talk":"t2","to_talk":"t3","talks":["t2","t3"],"before":1,"end":1000}`)
	active, err := admit(z, "s1", storage.RunAdmission{Conversation: "c1", Trigger: storage.TriggerServer, Round: 7,
		Digest: "abc", Window: window, Hide: []string{"system_prompt"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = active.Release() }()
	a, err := storage.ReadAdmission(active.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == "" || a.Session != "s1" || a.Conversation != "c1" || a.Trigger != storage.TriggerServer || a.Round != 7 ||
		a.Digest != "abc" || string(a.Window) != string(window) || len(a.Hide) != 1 || a.Admitted == "" {
		t.Fatalf("admission.json holds %+v", a)
	}
	info, err := os.Stat(filepath.Join(active.Dir, storage.AdmissionFile))
	if err != nil || info.Mode().Perm()&0o222 != 0 {
		t.Fatalf("admission.json is writable: %v, %v", info.Mode(), err)
	}
}

// A run is closed with its close file, running while its lock is held,
// stopped with admission.json and a free lock, and unknown without
// admission.json, as a run made before admissions. The probe makes nothing.
func TestARunsState(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	running, err := admit(z, "s1", storage.RunAdmission{})
	if err != nil {
		t.Fatal(err)
	}
	state := func(n int) (storage.RunState, bool) {
		t.Helper()
		runs, err := storage.JudgmentRuns(z, "s1")
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range runs {
			if r.Run == n {
				s, held, err := storage.StateOf(&r)
				if err != nil {
					t.Fatal(err)
				}
				return s, held
			}
		}
		t.Fatalf("no run %d", n)
		return "", false
	}
	if s, _ := state(1); s != storage.RunRunning {
		t.Fatalf("a run holding its lock is %s", s)
	}
	if err := running.Release(); err != nil {
		t.Fatal(err)
	}
	if s, _ := state(1); s != storage.RunStopped {
		t.Fatalf("a run that let its lock go unclosed is %s", s)
	}
	// A run made before admissions: a directory, nothing in it.
	old := filepath.Join(z.SessionDir("s1"), storage.JudgmentsDir, "j000002")
	if err := os.Mkdir(old, 0o755); err != nil {
		t.Fatal(err)
	}
	if s, _ := state(2); s != storage.RunUnknown {
		t.Fatalf("a run without admission.json is %s", s)
	}
	if _, err := os.Stat(filepath.Join(old, storage.ActiveFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the probe made the lock file: %v", err)
	}
	if _, err := storage.PublishStage(context.Background(), z, "s1", old, 9, "close", "0123456789abcdef", []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	if s, held := state(2); s != storage.RunClosed || held {
		t.Fatalf("a run with its close file is %s, held %v", s, held)
	}
	_, regroup, err := storage.AdmitRegroup(context.Background(), z, "s1", old)
	if err != nil {
		t.Fatal(err)
	}
	if s, held := state(2); s != storage.RunClosed || !held {
		t.Fatalf("a closed run being regrouped is %s, held %v", s, held)
	}
	if err := regroup.Release(); err != nil {
		t.Fatal(err)
	}
}

// An admission and a publish wait for the root while another holder in the
// process has it, and stop waiting when their context ends; the root locks
// that do not wait find it busy at once, as they do when another process
// holds it.
func TestAJudgeWaitsForTheRootUntilItsContextEnds(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	held, err := storage.LockRootInCopy(z.Root())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.LockRootInCopy(z.Root()); !errors.Is(err, storage.ErrRootBusy) {
		t.Fatalf("a second lock in the process: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := storage.AdmitRun(ctx, z, "s1", func() (storage.RunAdmission, error) { return storage.RunAdmission{}, nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("an admission while the root is held: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		a, err := admit(z, "s1", storage.RunAdmission{})
		if err == nil {
			err = a.Release()
		}
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	if err := held.Unlock(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the admission that waited: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the admission did not take the root once it was let go")
	}
}

// A wait whose context has ended takes nothing, even when the root is free:
// a select that finds both ready may pick either.
func TestAnEndedWaitNeverTakesTheRoot(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 200; i++ {
		if l, err := storage.LockRootContext(ctx, z.Root()); !errors.Is(err, context.Canceled) {
			if l != nil {
				_ = l.Unlock()
			}
			t.Fatalf("attempt %d with an ended context: %v", i+1, err)
		}
	}
	l, err := storage.LockRootInCopy(z.Root())
	if err != nil {
		t.Fatalf("the root after the ended waits: %v", err)
	}
	_ = l.Unlock()
}

// A run listed before it closed is read again after its lock is probed, so
// a run that closed in between is closed, never stopped.
func TestARunListedBeforeItClosedIsClosed(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	active, err := admit(z, "s1", storage.RunAdmission{})
	if err != nil {
		t.Fatal(err)
	}
	runs, err := storage.JudgmentRuns(z, "s1")
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs %+v, %v", runs, err)
	}
	if _, err := storage.PublishStage(context.Background(), z, "s1", active.Dir, 9, "close", "0123456789abcdef", []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	if err := active.Release(); err != nil {
		t.Fatal(err)
	}
	run := runs[0]
	if s, held, err := storage.StateOf(&run); err != nil || s != storage.RunClosed || held || !run.Closed() {
		t.Fatalf("a run listed before it closed is %s, held %v, %v", s, held, err)
	}
}
