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

package storage

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// ActiveFile is a run's activity lock, inside its directory. It is held for
// the whole life of a run, and of a regroup over a closed run, and its
// contents are never read. Retention probes it before it removes anything of
// the session, so a run never reads a file a removal replaced under it.
const ActiveFile = ".active"

// RunFile says why a run was admitted. It is written once, read-only,
// before the run's first stage, so a run without it was made before
// admissions, or reserved just before a crash.
const RunFile = "run.json"

// The triggers of a run: a person's asz judge, or asz server's policy.
const (
	TriggerCommand = "command"
	TriggerServer  = "server"
)

// RunRecord is what a run's run.json holds.
type RunRecord struct {
	ID           string `json:"id"`
	Conversation string `json:"conversation"`
	Session      string `json:"session"`
	Trigger      string `json:"trigger"`
	// Round and Digest are the head the run judges, which its admission
	// bound.
	Round  uint64 `json:"round"`
	Digest string `json:"digest"`
	// Window is the window header the run's stage files carry, absent for
	// the whole conversation.
	Window json.RawMessage `json:"window,omitempty"`
	// Hide is the effective judge.hide: the flags whose records the run's
	// evaluators were never shown.
	Hide     []string `json:"hide"`
	Admitted string   `json:"admitted"`
	// Receipt, Landed and Target are an automatic run's: the number of the
	// receipt that reserved it, the session's highest landed sequence at
	// admission, and the window's end in unix milliseconds.
	Receipt int    `json:"receipt,omitempty"`
	Landed  uint64 `json:"landed,omitempty"`
	Target  int64  `json:"target,omitempty"`
}

// ReadRunRecord reads a run's run.json; nil, with no error, when the
// run has none.
func ReadRunRecord(runDir string) (*RunRecord, error) {
	data, err := os.ReadFile(filepath.Join(runDir, RunFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var a RunRecord
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, fmt.Errorf("storage: %s: %w", filepath.Join(runDir, RunFile), err)
	}
	return &a, nil
}

// ErrJudgeActive means a run or a regroup of the conversation is active: a
// conversation has one at a time.
var ErrJudgeActive = errors.New("storage: a judge run or regroup of the conversation is active")

// ErrRemovalPending means a removal of the session's files has not finished,
// so nothing is admitted until the pass that finishes it has run.
var ErrRemovalPending = errors.New("storage: a removal of the session's files is in progress")

// Active is a run, or a regroup over a closed run, holding its activity
// lock.
type Active struct {
	Number int
	Dir    string
	lock   *SessionLock
}

// Release lets the run's activity lock go. Its holder calls it once every
// worker of the run has returned and its cleanup has ended.
func (a *Active) Release() error {
	if a == nil {
		return nil
	}
	return a.lock.Unlock()
}

// JudgeActive reports whether any run or regroup of a session is active. It
// never waits and never makes anything. Retention asks it under the root
// lock, as every admission takes the activity lock under it, so no run can
// start between the answer and a removal.
func JudgeActive(z *Zone, session string) (bool, error) {
	work, err := activeRun(z, session)
	return work != "", err
}

// activeRun names the run of a session whose activity lock is held, or "".
func activeRun(z *Zone, session string) (string, error) {
	runs, err := JudgmentRuns(z, session)
	if err != nil {
		return "", err
	}
	for _, run := range runs {
		held, err := ProbeFileLock(filepath.Join(run.Dir, ActiveFile))
		if err != nil {
			return "", err
		}
		if held {
			return filepath.Base(run.Dir), nil
		}
	}
	return "", nil
}

// refuseJudging is what refuses any admission: a session retired or marked,
// a removal of its files not finished, or a run or regroup active. The
// caller holds the root lock.
func refuseJudging(z *Zone, session string) error {
	if retired, err := Retired(z, session); err != nil || retired {
		if err == nil {
			err = ErrRetiredSession
		}
		return err
	}
	intent, err := LoadRemoving(z.RemovingPath(session))
	if err != nil {
		return err
	}
	if intent != nil {
		return fmt.Errorf("%w: %s", ErrRemovalPending, session)
	}
	work, err := activeRun(z, session)
	if err != nil {
		return err
	}
	if work != "" {
		return fmt.Errorf("%w: %s of session %s", ErrJudgeActive, work, session)
	}
	return nil
}

// AdmitRun admits a run of a session's conversation in one decision, under
// the root lock, waited for until ctx ends. decide runs while the lock is
// held, and never takes it: told the number the run will have, it gives what
// the run judges, which run.json records. The run's directory is then reserved, its activity lock taken and
// run.json written before the root lock is released, so the run is
// active from the moment it can be seen.
func AdmitRun(ctx context.Context, z *Zone, session string, decide func(next int) (RunRecord, error)) (*Active, error) {
	l, err := LockRootContext(ctx, z.Root())
	if err != nil {
		return nil, err
	}
	defer func() { _ = l.Unlock() }()
	if err := refuseJudging(z, session); err != nil {
		return nil, err
	}
	runs, err := JudgmentRuns(z, session)
	if err != nil {
		return nil, err
	}
	next := 1
	if len(runs) > 0 {
		next = runs[len(runs)-1].Run + 1
	}
	adm, err := decide(next)
	if err != nil {
		return nil, err
	}
	parent := filepath.Join(z.SessionDir(session), JudgmentsDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, err
	}
	dir := filepath.Join(parent, fmt.Sprintf("j%06d", next))
	if err := os.Mkdir(dir, 0o755); err != nil {
		return nil, err
	}
	SyncAfterRename(parent)
	// The directory is new, so nothing else holds its lock.
	lock, err := TryLockFile(filepath.Join(dir, ActiveFile))
	if err != nil {
		return nil, err
	}
	if adm.ID == "" {
		adm.ID = rand.Text()
	}
	adm.Session = session
	if adm.Hide == nil {
		adm.Hide = []string{}
	}
	adm.Admitted = time.Now().UTC().Format(time.RFC3339)
	data, err := json.Marshal(adm)
	if err == nil {
		err = WriteAtomic(filepath.Join(dir, RunFile), 0o444, func(w io.Writer) error {
			_, err := w.Write(append(data, '\n'))
			return err
		})
	}
	if err != nil {
		_ = lock.Unlock()
		return nil, err
	}
	return &Active{Number: next, Dir: dir, lock: lock}, nil
}

// AdmitRegroup admits a regroup of a closed run, under the root lock waited
// for until ctx ends: the refusals of a run's admission, then the run's
// activity lock, taken before the regroup reads any stored file of it, then
// the next version, the one after every regroup written and every regroup
// begun, whose work directory it makes, so two regroups never share one.
func AdmitRegroup(ctx context.Context, z *Zone, session, runDir string) (int, *Active, error) {
	l, err := LockRootContext(ctx, z.Root())
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = l.Unlock() }()
	if err := refuseJudging(z, session); err != nil {
		return 0, nil, err
	}
	lock, err := TryLockFile(filepath.Join(runDir, ActiveFile))
	if errors.Is(err, ErrBusy) {
		return 0, nil, fmt.Errorf("%w: %s of session %s", ErrJudgeActive, filepath.Base(runDir), session)
	}
	if err != nil {
		return 0, nil, err
	}
	version, err := nextRegroup(runDir)
	if err != nil {
		_ = lock.Unlock()
		return 0, nil, err
	}
	n, _ := strconv.Atoi(runDirRe.FindStringSubmatch(filepath.Base(runDir))[1])
	return version, &Active{Number: n, Dir: runDir, lock: lock}, nil
}

// RunState is what a run's directory says of it.
type RunState string

// The states of a run.
const (
	// RunClosed has its close file.
	RunClosed RunState = "closed"
	// RunRunning holds its activity lock and has no close file.
	RunRunning RunState = "running"
	// RunStopped has run.json, no close file and a free lock: it
	// stopped before it closed, and nothing resumes or closes it.
	RunStopped RunState = "stopped"
	// RunUnknown has neither run.json nor a close file: made before
	// admissions, or reserved just before a crash.
	RunUnknown RunState = "unknown"
)

// StateOf says what state a run is in, and whether its activity lock is
// held, which for a closed run means a regroup of it is running. It never
// waits and never makes anything. The lock is probed first and the run's
// files read again after it, into run: a run publishes its close file
// before it lets the lock go, so a run found not held that has closed shows
// its close file then, and is never taken for stopped.
func StateOf(run *JudgmentRun) (RunState, bool, error) {
	held, err := ProbeFileLock(filepath.Join(run.Dir, ActiveFile))
	if err != nil {
		return "", false, err
	}
	if err := run.read(); err != nil {
		return "", false, err
	}
	switch {
	case run.Closed():
		return RunClosed, held, nil
	case held:
		return RunRunning, true, nil
	}
	if _, err := os.Lstat(filepath.Join(run.Dir, RunFile)); err == nil {
		return RunStopped, false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", false, err
	}
	return RunUnknown, false, nil
}
