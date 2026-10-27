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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/internal/pull/format"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// errNoSuchName is an archive request naming a file the snapshot does not
// hold: 404.
var errNoSuchName = errors.New("server: a name the snapshot does not hold")

// errRestart is a restart the job's history does not allow: 400.
var errRestart = errors.New("server: no such restart")

// errPreparing is an archive whose name list another request is installing
// now: 503, tried again in a second.
var errPreparing = errors.New("server: the archive is being prepared; try again")

// archiveRequest is an archive's creation, checked: the request, its
// identifier, and the lease and token it came with.
type archiveRequest struct {
	conversation, tag, id string
	names                 []string
	restart               uint64
	lease                 string
	caller                Caller
}

// startArchive accepts an archive's creation. The names are checked against
// the manifest outside the global lock. A job already accepted has its
// saved list compared outside it too, and an explicit restart applied under
// it. A new job takes the archive's owner lock under the global lock, then
// outside it installs the immutable list of names once, fresh and durable,
// over any list no accepted job owns; only then, under the global lock
// again, with the request's token and lease and the snapshot's lifecycle
// checked again and the list still the file it installed, is the job saved
// at attempt 1, not started. It then tries to claim the job. Nothing that
// grows with the list is done under the global lock.
func (s *Server) startArchive(r archiveRequest, manifest *format.Manifest) (*archiveJob, error) {
	entries := make(map[string]bool, len(manifest.Files))
	for _, e := range manifest.Files {
		entries[e.File] = true
	}
	for _, n := range r.names {
		if !entries[n] {
			return nil, fmt.Errorf("%w: %s", errNoSuchName, n)
		}
	}
	slices.Sort(r.names)
	dir := s.p.snapshot(r.conversation, r.tag)
	namesPath := archivePath(dir, r.id, ".names")
	jobPath := archivePath(dir, r.id, ".json")
	count := len(manifest.Files)
	admitted := func() error {
		if _, err := s.lifecycle(r.conversation, r.tag, s.now()); err != nil {
			return err
		}
		return s.authorize(r.conversation, r.tag, r.lease, r.caller)
	}
	var job *archiveJob
	var owner *storage.SessionLock
	err := s.global(func() error {
		job, owner = nil, nil
		if err := admitted(); err != nil {
			return err
		}
		j, err := readArchive(jobPath, r.tag, r.id)
		if err == nil {
			job = j
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if r.restart != 0 {
			return errRestart
		}
		l, err := storage.TryLockFile(archivePath(dir, r.id, ".lock"))
		if errors.Is(err, storage.ErrBusy) {
			return errPreparing
		}
		if err != nil {
			return err
		}
		owner = l
		return nil
	})
	if err != nil {
		return nil, err
	}
	if job != nil {
		// An accepted job's list never changes: one missing or not the
		// names asked for is damage, never made again.
		saved, err := readNames(namesPath, r.tag, r.id, count)
		if errors.Is(err, os.ErrNotExist) {
			return nil, stateError("archive %s was accepted and its names are gone", r.id)
		}
		if err != nil {
			return nil, err
		}
		if !slices.Equal(saved.Files, r.names) {
			return nil, stateError("archive %s's saved names are another set", r.id)
		}
		if r.restart != 0 {
			err := s.global(func() error {
				if err := admitted(); err != nil {
					return err
				}
				j, err := readArchive(jobPath, r.tag, r.id)
				if err != nil {
					return err
				}
				job = j
				return s.restartArchive(dir, r, j, func(next *archiveJob) { job = next })
			})
			if err != nil {
				return nil, err
			}
		}
	} else {
		defer func() { _ = owner.Unlock() }()
		installed, err := s.installNames(dir, r)
		if err != nil {
			return nil, err
		}
		err = s.global(func() error {
			job = nil
			if err := admitted(); err != nil {
				return err
			}
			if _, err := readArchive(jobPath, r.tag, r.id); !errors.Is(err, os.ErrNotExist) {
				if err == nil {
					err = stateError("archive %s was accepted while its owner was held", r.id)
				}
				return err
			}
			if now, err := os.Lstat(namesPath); err != nil || !os.SameFile(now, installed) {
				return stateError("archive %s's names are not the file installed for it", r.id)
			}
			j := &archiveJob{Schema: stateSchema, Snapshot: r.tag, Archive: r.id, Attempt: 1, State: jobBuilding}
			if err := replaceRecord(jobPath, j, s.o.Files); err != nil {
				return err
			}
			job = j
			return nil
		})
		if err != nil {
			return nil, err
		}
		_ = owner.Unlock()
	}
	if job.State == jobBuilding {
		if err := s.claimArchive(r.conversation, r.tag, r.id); err != nil && !errors.Is(err, errNoCapacity) {
			return nil, err
		}
	}
	return job, nil
}

// installNames installs an archive's list of names, fresh and durable, for
// a job not yet accepted, and returns the file it installed. A list there
// already is a leftover no accepted job owns, and is removed first: it may
// be one whose sync failed, which no later sync makes durable. The caller
// holds the archive's owner and the snapshot.
func (s *Server) installNames(dir string, r archiveRequest) (os.FileInfo, error) {
	path := archivePath(dir, r.id, ".names")
	info, err := os.Lstat(path)
	switch {
	case err == nil && !info.Mode().IsRegular():
		return nil, stateError("%s is not a regular file", path)
	case err == nil:
		if err := s.files().Remove(path); err != nil {
			return nil, err
		}
		if err := s.files().SyncDir(archivesDir(dir)); err != nil {
			return nil, err
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	if err := newRecord(path, &archiveNames{Schema: stateSchema, Snapshot: r.tag, Archive: r.id, Files: r.names}, s.o.Files); err != nil {
		return nil, err
	}
	return os.Lstat(path)
}

// restartArchive applies an explicit restart numbered N: a current failure
// at N is kept as .failed-N.json, then its successor saved at N+1, building
// and not started; a current attempt past N whose failure N was kept is
// answered as it is; anything else is refused. The caller holds the global
// lock.
func (s *Server) restartArchive(dir string, r archiveRequest, j *archiveJob, set func(*archiveJob)) error {
	n := r.restart
	retained := archivePath(dir, r.id, ".failed-"+strconv.FormatUint(n, 10)+".json")
	switch {
	case j.Attempt == n && j.State == jobFailed:
		if err := s.keepFailure(retained, j); err != nil {
			return err
		}
		if n == math.MaxUint64 {
			return stateError("archive %s has no attempt after %d", r.id, n)
		}
		next := &archiveJob{Schema: stateSchema, Snapshot: r.tag, Archive: r.id, Attempt: n + 1, State: jobBuilding}
		if err := replaceRecord(archivePath(dir, r.id, ".json"), next, s.o.Files); err != nil {
			return err
		}
		set(next)
		return nil
	case j.Attempt > n:
		kept, err := readArchive(retained, r.tag, r.id)
		if errors.Is(err, os.ErrNotExist) {
			return errRestart
		}
		if err != nil {
			return err
		}
		if kept.Attempt != n || kept.State != jobFailed {
			return stateError("%s is not the failure of attempt %d", retained, n)
		}
		return nil
	case j.Attempt == n:
		return stateError("archive %s is at attempt %d and not failed, beside a restart of it", r.id, n)
	}
	return errRestart
}

// keepFailure keeps a failed job's record as its retained failure before its
// successor is saved: installed when it is not there, and when it is, the
// same record committed again by a fresh write, since one whose sync failed
// is not made durable by being there. A retained record that is another
// failure is damage.
func (s *Server) keepFailure(path string, j *archiveJob) error {
	kept, err := readArchive(path, j.Snapshot, j.Archive)
	if errors.Is(err, os.ErrNotExist) {
		return newRecord(path, j, s.o.Files)
	}
	if err != nil {
		return err
	}
	a, _ := json.Marshal(kept)
	b, _ := json.Marshal(j)
	if !bytes.Equal(a, b) {
		return stateError("%s is not the failure it keeps", path)
	}
	return replaceRecord(path, kept, s.o.Files)
}

// claimArchive takes an archive job for a builder: a slot, the job's owner
// lock and an active-work hold on the snapshot, none waiting. Under the
// global lock the snapshot must still be served; a job not started is
// marked started; a started one whose owner was free was abandoned, and
// takes its successor or fails at the recovery limit. A snapshot no longer
// served starts no work.
func (s *Server) claimArchive(conversation, tag, id string) error {
	if !takeSlot(s.archiveSlots) {
		return errNoCapacity
	}
	dir := s.p.snapshot(conversation, tag)
	owner, err := storage.TryLockFile(archivePath(dir, id, ".lock"))
	if err != nil {
		giveSlot(s.archiveSlots)
		if errors.Is(err, storage.ErrBusy) {
			return nil
		}
		return err
	}
	// The builder's hold is taken with the snapshot's lifecycle check: a
	// snapshot no longer served starts no work.
	release, err := s.holdServed(conversation, tag)
	if err != nil {
		_ = owner.Unlock()
		giveSlot(s.archiveSlots)
		if errors.Is(err, errGone) {
			return nil
		}
		return err
	}
	var job *archiveJob
	jobPath := archivePath(dir, id, ".json")
	err = s.global(func() error {
		job = nil
		// The claim is decided where it is saved: a snapshot that stopped
		// being served since the hold, or while a busy replacement let the
		// lock go, starts nothing.
		if _, err := s.lifecycle(conversation, tag, s.now()); err != nil {
			return err
		}
		j, err := readArchive(jobPath, tag, id)
		if err != nil || j.State != jobBuilding {
			return err
		}
		switch {
		case !j.Started:
			j.Started = true
		case j.Attempt == math.MaxUint64:
			return stateError("archive %s has no attempt after %d", id, j.Attempt)
		case j.Attempt+1 >= recoveryLimit:
			j.Attempt++
			j.Started, j.State = false, jobFailed
			j.Problem = &Problem{Type: format.ProblemArchiveFailed, Title: "Internal Server Error", Status: http.StatusInternalServerError,
				Detail: "the archive's recovery reached its bound", Snapshot: tag, Archive: id, Attempt: j.Attempt}
		default:
			j.Attempt++
		}
		if err := replaceRecord(jobPath, j, s.o.Files); err != nil {
			return err
		}
		if j.State == jobBuilding {
			job = j
		}
		return nil
	})
	if errors.Is(err, errGone) {
		err = nil
	}
	if err != nil || job == nil || !s.enter() {
		release()
		_ = owner.Unlock()
		giveSlot(s.archiveSlots)
		return err
	}
	go func() {
		defer s.leave()
		func() {
			defer giveSlot(s.archiveSlots)
			defer func() { _ = owner.Unlock() }()
			defer release()
			s.buildArchive(conversation, tag, job)
		}()
		// A builder just came free: work waiting for one takes it.
		if s.ctx.Err() == nil {
			s.schedule()
		}
	}()
	return nil
}

// buildArchive writes an archive: what an earlier attempt left is
// discarded, the names read from their immutable list, the frozen files
// written in order with every size and digest checked, the result checked
// again by the extraction validator, synced, made read-only and installed,
// and only then the job saved ready with its size and digest.
func (s *Server) buildArchive(conversation, tag string, job *archiveJob) {
	dir := s.p.snapshot(conversation, tag)
	fail := func(err error) {
		s.finishArchive(dir, tag, job, func(j *archiveJob) {
			j.State = jobFailed
			j.Problem = &Problem{Type: format.ProblemArchiveFailed, Title: "Internal Server Error", Status: http.StatusInternalServerError,
				Detail: err.Error(), Snapshot: tag, Archive: job.Archive, Attempt: job.Attempt}
		})
	}
	if err := s.discardArchiveOutput(dir, job.Archive); err != nil {
		fail(err)
		return
	}
	manifest, err := s.manifestOf(conversation, tag)
	if err != nil {
		fail(err)
		return
	}
	names, err := readNames(archivePath(dir, job.Archive, ".names"), tag, job.Archive, len(manifest.Files))
	if err != nil {
		fail(err)
		return
	}
	entries := map[string]format.Entry{}
	for _, e := range manifest.Files {
		entries[e.File] = e
	}
	var files []format.ArchiveFile
	want := map[string]format.Entry{}
	for _, n := range names.Files {
		e := entries[n]
		want[n] = e
		path := filepath.Join(dir, filepath.FromSlash(n))
		files = append(files, format.ArchiveFile{Entry: e, Open: func() (io.ReadCloser, error) { return storage.OpenEvidence(path, nil) }})
	}
	random, err := newID()
	if err != nil {
		fail(err)
		return
	}
	temp := archivePath(dir, ".tmp-"+job.Archive+"-"+random[:16], "")
	size, digest, err := s.writeArchive(temp, files, want)
	if err != nil {
		_ = os.Remove(temp)
		fail(err)
		return
	}
	if err := storage.InstallFile(temp, archivePath(dir, job.Archive, ".tar.gz"), s.o.Files); err != nil {
		_ = os.Remove(temp)
		fail(err)
		return
	}
	s.finishArchive(dir, tag, job, func(j *archiveJob) {
		j.State, j.Bytes, j.Digest = jobReady, uint64(size), digest
	})
}

// writeArchive writes the archive to temp and checks it as a client will.
func (s *Server) writeArchive(temp string, files []format.ArchiveFile, want map[string]format.Entry) (int64, string, error) {
	f, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, "", err
	}
	size, digest, err := format.WriteArchive(f, files)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, "", err
	}
	in, err := os.Open(temp)
	if err != nil {
		return 0, "", err
	}
	defer in.Close()
	if err := format.Extract(in, want, func(format.Entry, io.Reader) error { return nil }); err != nil {
		return 0, "", err
	}
	return size, digest, os.Chmod(temp, 0o444)
}

// discardArchiveOutput removes an archive's bytes and temporaries that no
// ready record names, before a builder starts.
func (s *Server) discardArchiveOutput(dir, id string) error {
	entries, err := os.ReadDir(archivesDir(dir))
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if name == id+".tar.gz" || strings.HasPrefix(name, ".tmp-"+id+"-") {
			if !e.Type().IsRegular() {
				return stateError("%s is not a regular file", filepath.Join(archivesDir(dir), name))
			}
			if err := os.Remove(filepath.Join(archivesDir(dir), name)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Server) finishArchive(dir, tag string, job *archiveJob, set func(*archiveJob)) {
	path := archivePath(dir, job.Archive, ".json")
	var saved *archiveJob
	err := s.global(func() error {
		saved = nil
		j, err := readArchive(path, tag, job.Archive)
		if err != nil {
			return err
		}
		if j.State != jobBuilding || j.Attempt != job.Attempt {
			return stateError("archive %s changed under its builder", job.Archive)
		}
		set(j)
		if err := replaceRecord(path, j, s.o.Files); err != nil {
			return err
		}
		saved = j
		return nil
	})
	if err != nil {
		s.logf("pull: archive %s: its result could not be saved: %v", job.Archive, err)
		return
	}
	if saved.State == jobFailed {
		s.logf("pull: archive %s of %s failed at attempt %d: %s", job.Archive, tag, saved.Attempt, saved.Problem.Detail)
	}
}
