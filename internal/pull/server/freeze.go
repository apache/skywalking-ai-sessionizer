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

package server

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/internal/pull/format"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/source"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// sourceAttempts is how many times a freeze reads a source that keeps
// changing under it before it gives up.
const sourceAttempts = 4

// recoveryLimit is the attempt at which abandoned work is failed rather than
// started again.
const recoveryLimit = 3

// startFreeze is a manifest's first GET: under the global lock it joins the
// conversation's freeze that is building, or makes a new one at attempt 1,
// not started, with its private directory. It then tries to claim the job;
// a job no slot takes now waits for the scheduler.
func (s *Server) startFreeze(conversation string) (string, error) {
	var id string
	err := s.global(func() error {
		if err := s.conversationSetup(conversation); err != nil {
			return err
		}
		jobs, err := s.freezeJobs()
		if err != nil {
			return err
		}
		for _, j := range jobs {
			if j.Conversation == conversation && j.State == jobBuilding {
				id = j.Freeze
				return nil
			}
		}
		if id, err = s.newFreezeID(); err != nil {
			return err
		}
		if err := storage.MakeOwnedDir(s.p.build(id), s.o.Files); err != nil {
			return err
		}
		return replaceRecord(s.p.freeze(id), &freezeJob{Schema: stateSchema, Freeze: id, Conversation: conversation,
			Attempt: 1, State: jobBuilding}, s.o.Files)
	})
	if err != nil {
		return "", err
	}
	if err := s.claimFreeze(id); err != nil && !errors.Is(err, errNoCapacity) {
		return "", err
	}
	return id, nil
}

// newFreezeID is a freeze identifier no job, lock or build already has.
func (s *Server) newFreezeID() (string, error) {
	for {
		id, err := newID()
		if err != nil {
			return "", err
		}
		taken := false
		for _, p := range []string{s.p.freeze(id), s.p.freezeLock(id), s.p.build(id)} {
			there, err := exists(p)
			if err != nil {
				return "", err
			}
			taken = taken || there
		}
		if !taken {
			return id, nil
		}
	}
}

// freezeJobs reads every freeze job's record. One that does not read is
// reported and passed over: its building directory is still the server's.
func (s *Server) freezeJobs() ([]*freezeJob, error) {
	entries, err := os.ReadDir(s.p.freezes())
	if err != nil {
		return nil, err
	}
	var out []*freezeJob
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !storage.Digest64(id) {
			continue
		}
		j, err := readFreeze(s.p, id)
		if err != nil {
			s.logf("pull: freeze %s: %v", id, err)
			continue
		}
		out = append(out, j)
	}
	return out, nil
}

// claimFreeze takes a freeze job for a worker: a slot, then the job's owner
// lock, both without waiting. Under the global lock a job not started is
// marked started at its attempt before any work; a started one whose owner
// was free was abandoned, and takes its successor attempt, or fails at the
// recovery limit with no worker. errNoCapacity means every slot is busy.
func (s *Server) claimFreeze(id string) error {
	if !takeSlot(s.freezeSlots) {
		return errNoCapacity
	}
	owner, err := storage.TryLockFile(s.p.freezeLock(id))
	if err != nil {
		giveSlot(s.freezeSlots)
		if errors.Is(err, storage.ErrBusy) {
			return nil
		}
		return err
	}
	var job *freezeJob
	err = s.global(func() error {
		j, err := readFreeze(s.p, id)
		if err != nil || j.State != jobBuilding {
			return err
		}
		switch {
		case !j.Started:
			j.Started = true
		case j.Attempt == math.MaxUint64:
			return stateError("freeze %s has no attempt after %d", id, j.Attempt)
		case j.Attempt+1 >= recoveryLimit:
			j.Attempt++
			j.Started, j.State, j.FinishedAt = false, jobFailed, stamp(s.now())
			j.Problem = &Problem{Type: format.ProblemFreezeFailed, Title: "Internal Server Error", Status: http.StatusInternalServerError,
				Detail: "the freeze's recovery reached its bound", Conversation: j.Conversation, Freeze: id, Attempt: j.Attempt}
		default:
			j.Attempt++
		}
		if err := replaceRecord(s.p.freeze(id), j, s.o.Files); err != nil {
			return err
		}
		if j.State == jobBuilding {
			job = j
		}
		return nil
	})
	if err != nil || job == nil || !s.enter() {
		_ = owner.Unlock()
		giveSlot(s.freezeSlots)
		return err
	}
	go func() {
		defer s.leave()
		func() {
			defer giveSlot(s.freezeSlots)
			defer func() { _ = owner.Unlock() }()
			s.runFreeze(job)
		}()
		// A slot just came free: a job waiting for one takes it.
		if s.ctx.Err() == nil {
			s.schedule()
		}
	}()
	return nil
}

// runFreeze freezes a job's conversation: the source is read and frozen in
// the job's private directory, at most four times while it keeps changing,
// accepted as whole evidence, then either matched to the newest snapshot
// or published as a new one. The job is saved ready with its tag, or failed
// with the problem a request would have been answered with.
func (s *Server) runFreeze(job *freezeJob) {
	build := s.p.build(job.Freeze)
	// An earlier attempt of this job may have left its publication
	// unfinished: this worker holds its owner, so it settles it first, and
	// never waits on its own intent.
	if err := s.settleOwnIntent(job); err != nil {
		s.failFreeze(job, storageProblem(err))
		return
	}
	for range sourceAttempts {
		if s.ctx.Err() != nil {
			return
		}
		if err := s.freshBuild(job.Freeze); err != nil {
			s.failFreeze(job, storageProblem(err))
			return
		}
		m, err := source.Freeze(s.ctx, s.o.Root, job.Conversation, build, s.o.Files)
		if errors.Is(err, source.ErrChanging) {
			continue
		}
		if err != nil {
			s.failFreeze(job, sourceProblem(err))
			return
		}
		tree := filepath.Join(build, "tree")
		accepted, err := format.Accept(tree, format.Building, format.Want{Conversation: job.Conversation})
		if err != nil {
			s.failFreeze(job, Problem{Type: format.ProblemSourceInvalid, Title: "Unprocessable Entity", Status: http.StatusUnprocessableEntity,
				Detail: err.Error()})
			return
		}
		tag, err := s.reuse(job.Conversation, accepted)
		if err == nil && tag == "" {
			tag, err = s.publish(job, tree, m, accepted)
		}
		if err != nil {
			s.failFreeze(job, storageProblem(err))
			return
		}
		s.readyFreeze(job, tag)
		return
	}
	s.failFreeze(job, Problem{Type: format.ProblemSourceChange, Title: "Service Unavailable", Status: http.StatusServiceUnavailable,
		Detail: fmt.Sprintf("the source kept changing through %d attempts; try again", sourceAttempts)})
}

// settleOwnIntent discards the publication an earlier attempt of job left
// unfinished, when the intent is its own. The caller holds its owner.
func (s *Server) settleOwnIntent(job *freezeJob) error {
	var intent *publishing
	if err := s.global(func() error {
		st, err := readState(s.p)
		if err == nil && st.Publishing != nil && st.Publishing.Build == job.Freeze {
			intent = st.Publishing
		}
		return err
	}); err != nil || intent == nil {
		return err
	}
	return s.discardPublication(intent)
}

// freshBuild discards what a job's private directory holds and makes it
// again, empty, under the job's own owner lock. An empty one, as the job's
// start made it, is kept as it is.
func (s *Server) freshBuild(id string) error {
	entries, err := os.ReadDir(s.p.build(id))
	switch {
	case err == nil && len(entries) == 0:
		return storage.RealDirectory(s.p.build(id))
	case err == nil:
		if err := storage.DeleteOwned(s.p.building(), id, s.o.Files); err != nil {
			return err
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	return storage.MakeOwnedDir(s.p.build(id), s.o.Files)
}

// sourceProblem is the problem a source failure is answered with.
func sourceProblem(err error) Problem {
	switch {
	case errors.Is(err, source.ErrNoChain):
		return problem(http.StatusNotFound, "the conversation has no rounds")
	case errors.Is(err, source.ErrRetired):
		return problem(http.StatusGone, "the conversation's session was retired")
	case errors.Is(err, source.ErrRemoving):
		return problem(http.StatusConflict, "a removal of the session's files is in progress; try again")
	case errors.Is(err, source.ErrInvalid):
		return Problem{Type: format.ProblemSourceInvalid, Title: "Unprocessable Entity", Status: http.StatusUnprocessableEntity, Detail: err.Error()}
	case errors.Is(err, source.ErrRead):
		return Problem{Type: format.ProblemSourceRead, Title: "Service Unavailable", Status: http.StatusServiceUnavailable, Detail: err.Error()}
	}
	return storageProblem(err)
}

// storageProblem is a failure of the server's own state.
func storageProblem(err error) Problem {
	return Problem{Type: format.ProblemStorageState, Title: "Internal Server Error", Status: http.StatusInternalServerError, Detail: err.Error()}
}

// failFreeze saves a job failed with its problem. A failure that cannot be
// saved is logged; the owner is let go, and the next claim recovers it.
func (s *Server) failFreeze(job *freezeJob, p Problem) {
	p.Conversation, p.Freeze = "", ""
	if p.Type == format.ProblemFreezeFailed {
		p.Conversation, p.Freeze = job.Conversation, job.Freeze
	}
	s.finishFreeze(job, func(j *freezeJob) {
		j.State, j.Problem = jobFailed, &p
	})
}

// readyFreeze saves a job ready with the snapshot it found or made.
func (s *Server) readyFreeze(job *freezeJob, tag string) {
	s.finishFreeze(job, func(j *freezeJob) {
		j.State, j.Snapshot = jobReady, tag
	})
	if err := storage.DeleteOwned(s.p.building(), job.Freeze, s.o.Files); err != nil {
		s.logf("pull: freeze %s: its private directory: %v", job.Freeze, err)
	}
}

func (s *Server) finishFreeze(job *freezeJob, set func(*freezeJob)) {
	err := s.global(func() error {
		j, err := readFreeze(s.p, job.Freeze)
		if err != nil {
			return err
		}
		if j.State != jobBuilding || j.Attempt != job.Attempt {
			return stateError("freeze %s changed under its worker", job.Freeze)
		}
		set(j)
		j.FinishedAt = stamp(s.now())
		return replaceRecord(s.p.freeze(job.Freeze), j, s.o.Files)
	})
	if err != nil {
		s.logf("pull: freeze %s: its result could not be saved: %v", job.Freeze, err)
	}
}
