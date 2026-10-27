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
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/pull/format"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/source"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// route is one pull route a request named, its parts checked.
type route struct {
	kind         string
	method       string
	conversation string
	tag          string
	archive      string
}

// The kinds of pull route.
const (
	routeList     = "list"
	routeManifest = "manifest"
	routeArchives = "archives"
	routeArchive  = "archive"
	routeData     = "data"
	routeDone     = "done"
)

// errUnsafeName is a name in a path that no machine could hold: 400.
var errUnsafeName = errors.New("server: a name in the path is not one a pull takes")

// parseRoute finds the pull route of a path. An unknown path is nil; a
// known one with a name that is not safe is errUnsafeName.
func parseRoute(r *http.Request) (*route, error) {
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), "/pull/v1/conversations")
	if !ok {
		return nil, nil
	}
	if rest == "" {
		return &route{kind: routeList, method: http.MethodGet}, nil
	}
	parts := strings.Split(strings.TrimPrefix(rest, "/"), "/")
	if !strings.HasPrefix(rest, "/") || len(parts) < 2 {
		return nil, nil
	}
	for i, p := range parts {
		u, err := url.PathUnescape(p)
		if err != nil {
			return nil, errUnsafeName
		}
		parts[i] = u
	}
	conversation := parts[0]
	if err := storage.CheckNamePart(conversation); err != nil {
		return nil, errUnsafeName
	}
	switch {
	case len(parts) == 2 && parts[1] == "manifest":
		return &route{kind: routeManifest, method: http.MethodGet, conversation: conversation}, nil
	case len(parts) >= 4 && parts[1] == "snapshots":
		tag := parts[2]
		if t, err := storage.ParseTag(tag); err != nil || storage.Stamp(t) != tag {
			return nil, errUnsafeName
		}
		rt := &route{conversation: conversation, tag: tag}
		switch {
		case len(parts) == 4 && parts[3] == "archives":
			rt.kind, rt.method = routeArchives, http.MethodPost
		case len(parts) == 4 && parts[3] == "done":
			rt.kind, rt.method = routeDone, http.MethodPost
		case len(parts) == 5 && parts[3] == "archives", len(parts) == 6 && parts[3] == "archives" && parts[5] == "data":
			if !storage.Digest64(parts[4]) {
				return nil, errUnsafeName
			}
			rt.archive, rt.kind, rt.method = parts[4], routeArchive, http.MethodGet
			if len(parts) == 6 {
				rt.kind = routeData
			}
		default:
			return nil, nil
		}
		return rt, nil
	}
	return nil, nil
}

// Handler serves the pull routes, behind the guard that checked the token.
// The method is checked first, then the token's request slot, then the
// route's own checks. Until this process owns the root's pulls every known
// route answers 503 and asks to be tried again in a second. Each request is
// logged once, refused or not.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r, holds, a := withRequestState(r)
		caller, _ := CallerOf(r)
		rt := &route{}
		status := s.handle(w, r, caller, rt, holds)
		name := caller.Name
		if name == "" {
			name = "-"
		}
		tag, archive := rt.tag, rt.archive
		if tag == "" {
			tag = a.snapshot
		}
		if archive == "" {
			archive = a.archive
		}
		s.logf("pull: %s %s %s %s %s %s %s status %d", name, r.Method, rt.kind, rt.conversation, tag, archive, a, status)
	})
}

// handle admits and answers one pull request and returns its status. What
// the request holds is let go after its answer's last flush, its slot and
// its count in the server last, so a stopping server waits for both.
func (s *Server) handle(w http.ResponseWriter, r *http.Request, caller Caller, found *route, holds *requestHolds) int {
	rt, err := parseRoute(r)
	if err != nil {
		return answerProblem(w, problem(http.StatusBadRequest, err.Error()))
	}
	if rt == nil {
		return answerProblem(w, problem(http.StatusNotFound, "no such pull route"))
	}
	*found = *rt
	if r.Method != rt.method {
		w.Header().Set("Allow", rt.method)
		return answerProblem(w, problem(http.StatusMethodNotAllowed, r.Method+" is not served here"))
	}
	if caller.Hash == "" {
		return answerProblem(w, problem(http.StatusUnauthorized, "a token is required"))
	}
	release, ok := s.o.Guard.Admit(caller)
	if !ok {
		return answerProblem(w, tooMany())
	}
	entered := false
	defer func() {
		finishAnswer(r)
		holds.release()
		release()
		if entered {
			s.leave()
		}
	}()
	if !s.owned.Load() {
		w.Header().Set("Retry-After", "1")
		return answerProblem(w, Problem{Type: format.ProblemOwnerWait, Title: "Service Unavailable", Status: http.StatusServiceUnavailable,
			Detail: "another process serves this root's pulls; try again"})
	}
	if entered = s.enter(); !entered {
		w.Header().Set("Retry-After", "1")
		return answerProblem(w, problem(http.StatusServiceUnavailable, "the server is stopping"))
	}
	return s.serve(w, r, rt, caller)
}

// serve answers one admitted pull request and returns its status.
func (s *Server) serve(w http.ResponseWriter, r *http.Request, rt *route, caller Caller) int {
	if rt.kind != routeList && r.URL.RawQuery != "" {
		return answerProblem(w, problem(http.StatusBadRequest, "this route takes no query parameters"))
	}
	switch rt.kind {
	case routeList:
		return s.serveList(w, r)
	case routeManifest:
		return s.serveManifest(w, r, rt, caller)
	case routeArchives:
		return s.serveArchiveStart(w, r, rt, caller)
	case routeArchive, routeData:
		return s.serveArchive(w, r, rt, caller)
	case routeDone:
		return s.serveDone(w, r, rt, caller)
	}
	return answerProblem(w, problem(http.StatusNotFound, "no such pull route"))
}

func answerProblem(w http.ResponseWriter, p Problem) int {
	_ = writeProblem(w, p)
	return p.Status
}

func answerJSON(w http.ResponseWriter, status int, v any) int {
	_ = writeJSON(w, status, v)
	return status
}

func (s *Server) serveList(w http.ResponseWriter, r *http.Request) int {
	// A query that does not parse is refused, never read without the parts
	// that did not.
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return answerProblem(w, problem(http.StatusBadRequest, "the query does not parse: "+err.Error()))
	}
	q, err := source.ParseQuery(values, s.now())
	if err != nil {
		return answerProblem(w, problem(http.StatusBadRequest, err.Error()))
	}
	l, err := source.List(s.o.Root, q)
	if err != nil {
		return answerProblem(w, storageProblem(err))
	}
	return answerJSON(w, http.StatusOK, l)
}

// serveManifest answers a manifest's first GET by starting or joining a
// freeze and saying it builds, and a poll, which names the freeze in
// Asz-Pull-Freeze, by the freeze's state: building, failed with its saved
// problem, or ready, with a fresh lease on the snapshot it found or made.
func (s *Server) serveManifest(w http.ResponseWriter, r *http.Request, rt *route, caller Caller) int {
	freeze := r.Header.Values("Asz-Pull-Freeze")
	if len(freeze) == 0 {
		if rounds, err := sessionflow.OpenChain(s.o.Root, rt.conversation).ListAtMost(format.MaxFiles); err != nil {
			return answerProblem(w, storageProblem(err))
		} else if len(rounds) == 0 {
			return answerProblem(w, problem(http.StatusNotFound, "the conversation has no rounds"))
		}
		id, err := s.startFreeze(rt.conversation)
		if err != nil {
			return answerProblem(w, storageProblem(err))
		}
		return answerJSON(w, http.StatusAccepted, format.ManifestPending{Schema: format.Schema, Conversation: rt.conversation,
			Freeze: id, State: format.StateBuilding})
	}
	if len(freeze) != 1 || !storage.Digest64(freeze[0]) {
		return answerProblem(w, problem(http.StatusBadRequest, "one Asz-Pull-Freeze of 64 lower-case hexadecimal characters is required"))
	}
	id := freeze[0]
	var job *freezeJob
	var owned bool
	err := s.global(func() error {
		j, err := readFreeze(s.p, id)
		if err != nil {
			return err
		}
		job = j
		if j.State == jobBuilding {
			owned, err = storage.ProbeFileLock(s.p.freezeLock(id))
		}
		return err
	})
	switch {
	case errors.Is(err, os.ErrNotExist):
		return answerProblem(w, problem(http.StatusConflict, "no such freeze; ask again without Asz-Pull-Freeze"))
	case err != nil:
		return answerProblem(w, storageProblem(err))
	case job.Conversation != rt.conversation:
		return answerProblem(w, problem(http.StatusBadRequest, "the freeze is of another conversation"))
	}
	switch job.State {
	case jobFailed:
		return answerProblem(w, *job.Problem)
	case jobReady:
		return s.answerManifest(w, r, rt.conversation, job.Snapshot, caller)
	}
	if !owned {
		if err := s.claimFreeze(id); errors.Is(err, errNoCapacity) {
			return answerProblem(w, tooMany())
		} else if err != nil {
			return answerProblem(w, storageProblem(err))
		}
		// A claim of abandoned work can fail it at its recovery limit:
		// the answer is the job as the claim left it.
		if j, err := s.readFreezeLocked(id); err != nil {
			return answerProblem(w, storageProblem(err))
		} else if j.State == jobFailed {
			return answerProblem(w, *j.Problem)
		}
	}
	return answerJSON(w, http.StatusAccepted, format.ManifestPending{Schema: format.Schema, Conversation: rt.conversation,
		Freeze: id, State: format.StateBuilding})
}

// answerManifest answers a ready freeze: the snapshot is held for the whole
// answer, a lease granted on it, its manifest read, and its use renewed
// while the manifest is sent.
func (s *Server) answerManifest(w http.ResponseWriter, r *http.Request, conversation, tag string, caller Caller) int {
	expired := problem(http.StatusConflict, "the freeze's snapshot expired; ask again without Asz-Pull-Freeze")
	if status := s.holdSnapshot(r, conversation, tag); status == http.StatusGone {
		return answerProblem(w, expired)
	} else if status != 0 {
		return answerProblem(w, storageProblem(errors.New("the snapshot could not be held")))
	}
	lease, err := s.grantLease(conversation, tag, caller)
	switch {
	case errors.Is(err, errGone):
		return answerProblem(w, expired)
	case errors.Is(err, errForbidden):
		return answerProblem(w, problem(http.StatusForbidden, "the token is no longer in the tokens file"))
	case err != nil:
		return answerProblem(w, storageProblem(err))
	}
	holdUntilSent(r, s.renewWhile(r.Context(), conversation, tag, lease, caller))
	m, err := s.manifestOf(conversation, tag)
	if err != nil {
		return answerProblem(w, storageProblem(err))
	}
	a := auditOf(r)
	a.snapshot, a.files = tag, len(m.Files)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = format.WriteManifestResponse(w, m, format.Response{Snapshot: tag, Lease: lease})
	return http.StatusOK
}

// readFreezeLocked reads a freeze job under the global lock.
func (s *Server) readFreezeLocked(id string) (*freezeJob, error) {
	var j *freezeJob
	err := s.global(func() error {
		var err error
		j, err = readFreeze(s.p, id)
		return err
	})
	return j, err
}

// holdSnapshot holds a snapshot for a request until its answer is sent,
// before anything of it is read: under the global lock, its lifecycle is
// checked and the hold taken in one transition, so no removal runs between
// the two. A snapshot not served is 410.
func (s *Server) holdSnapshot(r *http.Request, conversation, tag string) int {
	hold, err := s.holdServed(conversation, tag)
	if errors.Is(err, errGone) || errors.Is(err, os.ErrNotExist) {
		return http.StatusGone
	}
	if err != nil {
		s.logf("pull: %s/%s could not be held: %v", conversation, tag, err)
		return http.StatusInternalServerError
	}
	holdUntilSent(r, hold)
	return 0
}

// holdServed takes an active-work hold on a snapshot still served, under
// the global lock: removal probes holds under the same lock, so it sees this
// one or has already removed the snapshot.
func (s *Server) holdServed(conversation, tag string) (func(), error) {
	var hold func()
	err := s.global(func() error {
		if hold != nil {
			hold()
			hold = nil
		}
		if _, err := s.lifecycle(conversation, tag, s.now()); err != nil {
			return err
		}
		release, err := s.activeWork(s.p.snapshot(conversation, tag))
		hold = release
		return err
	})
	if err != nil && hold != nil {
		hold()
		hold = nil
	}
	return hold, err
}

// admitSnapshot admits a request the snapshot already holds: under the
// global lock its lifecycle, then its token and lease, and its use is
// recorded; its use is then renewed while it is answered.
func (s *Server) admitSnapshot(r *http.Request, rt *route, lease string, caller Caller) (int, error) {
	err := s.global(func() error {
		if _, err := s.lifecycle(rt.conversation, rt.tag, s.now()); err != nil {
			return err
		}
		if err := s.authorize(rt.conversation, rt.tag, lease, caller); err != nil {
			return err
		}
		return s.use(rt.conversation, rt.tag, s.now())
	})
	switch {
	case errors.Is(err, errGone):
		return http.StatusGone, err
	case errors.Is(err, errForbidden):
		return http.StatusForbidden, err
	case err != nil:
		return http.StatusInternalServerError, err
	}
	holdUntilSent(r, s.renewWhile(r.Context(), rt.conversation, rt.tag, lease, caller))
	return 0, nil
}

func (s *Server) serveArchiveStart(w http.ResponseWriter, r *http.Request, rt *route, caller Caller) int {
	if status := s.holdSnapshot(r, rt.conversation, rt.tag); status != 0 {
		return answerSnapshotRefusal(w, status, errors.New("the snapshot could not be held"))
	}
	manifest, err := s.manifestOf(rt.conversation, rt.tag)
	if errors.Is(err, os.ErrNotExist) {
		return answerProblem(w, problem(http.StatusGone, "no such snapshot"))
	}
	if err != nil {
		return answerProblem(w, storageProblem(err))
	}
	if err := bodyDeadlines(w, s.now()); err != nil {
		return answerProblem(w, problem(http.StatusInternalServerError, err.Error()))
	}
	req, err := format.ReadArchiveRequest(r.Body, len(manifest.Files))
	if err != nil {
		return answerProblem(w, problem(http.StatusBadRequest, err.Error()))
	}
	auditOf(r).files = len(req.Files)
	if status, err := s.admitSnapshot(r, rt, req.Lease, caller); err != nil {
		return answerSnapshotRefusal(w, status, err)
	}
	ar := archiveRequest{conversation: rt.conversation, tag: rt.tag, id: format.ArchiveID(req.Files), names: req.Files,
		restart: req.Restart, lease: req.Lease, caller: caller}
	auditOf(r).archive = ar.id
	job, err := s.startArchive(ar, manifest)
	switch {
	case errors.Is(err, errNoSuchName):
		return answerProblem(w, problem(http.StatusNotFound, err.Error()))
	case errors.Is(err, errRestart):
		return answerProblem(w, problem(http.StatusBadRequest, err.Error()))
	case errors.Is(err, errGone):
		return answerProblem(w, problem(http.StatusGone, "the snapshot is gone"))
	case errors.Is(err, errForbidden):
		return answerSnapshotRefusal(w, http.StatusForbidden, err)
	case errors.Is(err, errPreparing):
		w.Header().Set("Retry-After", "1")
		return answerProblem(w, problem(http.StatusServiceUnavailable, err.Error()))
	case err != nil:
		return answerProblem(w, storageProblem(err))
	}
	return s.answerArchive(w, rt.tag, job)
}

func answerSnapshotRefusal(w http.ResponseWriter, status int, err error) int {
	switch status {
	case http.StatusGone:
		return answerProblem(w, problem(status, "the snapshot is gone"))
	case http.StatusForbidden:
		return answerProblem(w, problem(status, "the lease is not this token's, or is not open"))
	}
	return answerProblem(w, storageProblem(err))
}

// answerArchive answers with an archive job's state.
func (s *Server) answerArchive(w http.ResponseWriter, tag string, job *archiveJob) int {
	switch job.State {
	case jobReady:
		w.Header().Set("ETag", `"`+job.Digest+`"`)
		return answerJSON(w, http.StatusOK, format.ArchiveState{Schema: format.Schema, Snapshot: tag, Archive: job.Archive,
			State: format.StateReady, Bytes: job.Bytes, Digest: job.Digest})
	case jobFailed:
		return answerProblem(w, *job.Problem)
	}
	return answerJSON(w, http.StatusAccepted, format.ArchiveState{Schema: format.Schema, Snapshot: tag, Archive: job.Archive,
		State: format.StateBuilding})
}

// serveArchive answers an archive's poll and its data. A range request is
// checked in this order: its headers, the snapshot and its lease, the
// archive's state, the entity tag against the ready archive, and only then
// the range against its size.
func (s *Server) serveArchive(w http.ResponseWriter, r *http.Request, rt *route, caller Caller) int {
	var start, end uint64
	var etag string
	if rt.kind == routeData {
		var err error
		if start, end, err = format.ParseRange(r.Header.Values("Range")); err != nil {
			return answerProblem(w, problem(http.StatusBadRequest, err.Error()))
		}
		if etag, err = format.ParseIfMatch(r.Header.Values("If-Match")); err != nil {
			return answerProblem(w, problem(http.StatusBadRequest, err.Error()))
		}
		a := auditOf(r)
		a.ranged, a.start, a.end = true, start, end
	}
	lease, err := format.ParseLeaseHeader(r.Header.Values("Asz-Pull-Lease"))
	if err != nil {
		return answerProblem(w, problem(http.StatusBadRequest, err.Error()))
	}
	if status := s.holdSnapshot(r, rt.conversation, rt.tag); status != 0 {
		return answerSnapshotRefusal(w, status, errors.New("the snapshot could not be held"))
	}
	if status, err := s.admitSnapshot(r, rt, lease, caller); err != nil {
		return answerSnapshotRefusal(w, status, err)
	}
	dir := s.p.snapshot(rt.conversation, rt.tag)
	var job *archiveJob
	var owned bool
	err = s.global(func() error {
		j, err := readArchive(archivePath(dir, rt.archive, ".json"), rt.tag, rt.archive)
		if err != nil {
			return err
		}
		job = j
		if j.State == jobBuilding {
			owned, err = storage.ProbeFileLock(archivePath(dir, rt.archive, ".lock"))
		}
		return err
	})
	switch {
	case errors.Is(err, os.ErrNotExist):
		return answerProblem(w, problem(http.StatusNotFound, "no such archive"))
	case err != nil:
		return answerProblem(w, storageProblem(err))
	}
	if job.State == jobBuilding && !owned {
		if err := s.claimArchive(rt.conversation, rt.tag, rt.archive); errors.Is(err, errNoCapacity) {
			return answerProblem(w, tooMany())
		} else if err != nil {
			return answerProblem(w, storageProblem(err))
		}
		// The answer is the job as the claim left it, failed at its
		// recovery limit included.
		err := s.global(func() error {
			j, err := readArchive(archivePath(dir, rt.archive, ".json"), rt.tag, rt.archive)
			if err == nil {
				job = j
			}
			return err
		})
		if err != nil {
			return answerProblem(w, storageProblem(err))
		}
	}
	if rt.kind == routeArchive || job.State != jobReady {
		return s.answerArchive(w, rt.tag, job)
	}
	// The count is read only now, with the snapshot held and its use
	// renewed, and the job found accepted: a list that does not read fails
	// the range.
	files, err := s.archiveFiles(rt.conversation, rt.tag, rt.archive)
	if err != nil {
		return answerProblem(w, storageProblem(err))
	}
	auditOf(r).files = files
	if etag != job.Digest {
		return answerProblem(w, problem(http.StatusPreconditionFailed, "the archive is not the one If-Match names; ask for its state again"))
	}
	if end >= job.Bytes {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", job.Bytes))
		return answerProblem(w, problem(http.StatusRequestedRangeNotSatisfiable, "the range is past the archive's end"))
	}
	return s.sendRange(w, r, dir, job, start, end)
}

// archiveFiles is how many files an archive holds, for the audit: read
// once, streamed, and kept in a bounded memory of counts.
func (s *Server) archiveFiles(conversation, tag, id string) (int, error) {
	key := conversation + "/" + tag + "/" + id
	if n, ok := s.archiveCounts.get(key); ok {
		return n, nil
	}
	m, err := s.manifestOf(conversation, tag)
	if err != nil {
		return 0, err
	}
	names, err := readNames(archivePath(s.p.snapshot(conversation, tag), id, ".names"), tag, id, len(m.Files))
	if err != nil {
		return 0, err
	}
	s.archiveCounts.put(key, len(names.Files))
	return len(names.Files), nil
}

// counts keeps archives' file counts, at most countsKept of them: once
// full it starts again empty, so a long-running server never holds a count
// for every archive it has served.
type counts struct {
	mu sync.Mutex
	m  map[string]int
}

const countsKept = 1024

func (c *counts) get(key string) (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, ok := c.m[key]
	return n, ok
}

func (c *counts) put(key string, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) >= countsKept {
		c.m = map[string]int{}
	}
	c.m[key] = n
}

// sendRange streams one range of a ready archive. The request holds the
// snapshot, and renews its use, until the range is sent.
func (s *Server) sendRange(w http.ResponseWriter, r *http.Request, dir string, job *archiveJob, start, end uint64) int {
	f, err := storage.OpenEvidence(archivePath(dir, job.Archive, ".tar.gz"), nil)
	if err != nil {
		return answerProblem(w, storageProblem(err))
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || uint64(info.Size()) != job.Bytes {
		return answerProblem(w, storageProblem(fmt.Errorf("archive %s is not the size its record says", job.Archive)))
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Length", strconv.FormatUint(end-start+1, 10))
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, job.Bytes))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("ETag", `"`+job.Digest+`"`)
	w.WriteHeader(http.StatusPartialContent)
	auditOf(r).sent, _ = io.Copy(w, io.NewSectionReader(f, int64(start), int64(end-start+1)))
	return http.StatusPartialContent
}

// renewWhile records a snapshot's use while a request is answered: at the
// server's interval, or at half the time to live when that is sooner, so a
// request still at work never sees its snapshot expire. Each renewal checks
// again, under the global lock, that the snapshot is served, the token is
// still in the tokens file and the lease is open; the first that fails ends
// renewal, as the request's end or cancellation does. stop ends it and
// waits until it has.
func (s *Server) renewWhile(ctx context.Context, conversation, tag, lease string, caller Caller) (stop func()) {
	every := s.o.Every
	if half := s.o.TTL / 2; half > 0 && half < every {
		every = half
	}
	done, ended := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ended)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-s.ctx.Done():
				return
			case <-t.C:
			}
			err := s.global(func() error {
				if _, err := s.lifecycle(conversation, tag, s.now()); err != nil {
					return err
				}
				if err := s.authorize(conversation, tag, lease, caller); err != nil {
					return err
				}
				return s.use(conversation, tag, s.now())
			})
			if err != nil {
				return
			}
		}
	}()
	return func() {
		close(done)
		<-ended
	}
}

func (s *Server) serveDone(w http.ResponseWriter, r *http.Request, rt *route, caller Caller) int {
	if err := bodyDeadlines(w, s.now()); err != nil {
		return answerProblem(w, problem(http.StatusInternalServerError, err.Error()))
	}
	lease, err := format.ReadDoneRequest(r.Body)
	if err != nil {
		return answerProblem(w, problem(http.StatusBadRequest, err.Error()))
	}
	switch err := s.release(rt.conversation, rt.tag, lease, caller); {
	case errors.Is(err, errGone):
		return answerProblem(w, problem(http.StatusGone, "the snapshot is gone"))
	case errors.Is(err, errForbidden):
		return answerProblem(w, problem(http.StatusForbidden, "the lease is not this token's"))
	case err != nil:
		return answerProblem(w, storageProblem(err))
	}
	w.WriteHeader(http.StatusNoContent)
	return http.StatusNoContent
}

// bodyDeadlines sets a request body's read deadline, and the answer's write
// deadline Go sends an awaited 100 Continue under, before the body is read.
func bodyDeadlines(w http.ResponseWriter, now time.Time) error {
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(now.Add(bodyWait)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	if err := rc.SetWriteDeadline(now.Add(writeWait)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}

// schedule claims every building job that has no owner, freezes and
// archives, as slots allow: work a request accepted runs whether or not
// that request is still there.
func (s *Server) schedule() {
	jobs, err := s.freezeJobs()
	if err != nil {
		s.sweepLog("scheduling freezes", err)
		return
	}
	for _, j := range jobs {
		if j.State != jobBuilding {
			continue
		}
		if err := s.claimFreeze(j.Freeze); errors.Is(err, errNoCapacity) {
			break
		} else if err != nil {
			s.sweepLog("freeze "+j.Freeze, err)
		}
	}
	s.scheduleArchives()
}

// scheduleArchives claims every building archive job no builder owns.
func (s *Server) scheduleArchives() {
	conversations, err := s.conversationsOn()
	if err != nil {
		s.sweepLog("scheduling archives", err)
		return
	}
	for _, c := range conversations {
		tags, err := s.tagsOf(c)
		if err != nil {
			s.sweepLog("scheduling archives of "+c, err)
			continue
		}
		for _, tag := range tags {
			dir := s.p.snapshot(c, tag)
			entries, err := os.ReadDir(archivesDir(dir))
			if err != nil {
				continue
			}
			for _, e := range entries {
				id, ok := strings.CutSuffix(e.Name(), ".json")
				if !ok || !storage.Digest64(id) {
					continue
				}
				j, err := readArchive(archivePath(dir, id, ".json"), tag, id)
				if err != nil || j.State != jobBuilding {
					continue
				}
				if busy, err := storage.ProbeFileLock(archivePath(dir, id, ".lock")); err != nil || busy {
					continue
				}
				if err := s.claimArchive(c, tag, id); errors.Is(err, errNoCapacity) {
					return
				} else if err != nil {
					s.sweepLog("archive "+id, err)
				}
			}
		}
	}
}

// sweep settles a publication whose owner is gone, commits expiries,
// removes due snapshots and what earlier removals left detached, and
// removes finished freeze jobs once they are no longer asked for: ready
// ones whose snapshot is gone, failed ones a time to live after they ended.
// A private directory no building job owns goes too.
func (s *Server) sweep() {
	if err := s.settleIntent(true); err != nil {
		s.sweepLog("settling a publication", err)
	}
	conversations, err := s.conversationsOn()
	if err != nil {
		s.sweepLog("listing conversations", err)
		return
	}
	for _, c := range conversations {
		s.retain(c)
	}
	jobs, err := s.freezeJobs()
	if err != nil {
		s.sweepLog("listing freezes", err)
		return
	}
	building := map[string]bool{}
	for _, j := range jobs {
		if j.State == jobBuilding {
			building[j.Freeze] = true
			continue
		}
		done, _ := parseStamp(j.FinishedAt)
		gone := j.State == jobFailed && !s.now().Before(done.Add(s.o.TTL))
		if j.State == jobReady {
			there, err := exists(s.p.snapshot(j.Conversation, j.Snapshot))
			gone = err == nil && !there
		}
		if gone {
			s.removeFreeze(j.Freeze)
		}
	}
	builds, err := os.ReadDir(s.p.building())
	if err != nil {
		s.sweepLog("listing builds", err)
		return
	}
	for _, b := range builds {
		if !building[b.Name()] {
			s.removeBuild(b.Name())
		}
	}
}

// removeFreeze removes a finished job: its private directory and its
// record under its owner lock, then the lock file once it is let go. With
// no record, nothing can claim the job again.
func (s *Server) removeFreeze(id string) {
	owner, err := storage.TryLockFile(s.p.freezeLock(id))
	if err != nil {
		return
	}
	err = storage.DeleteOwned(s.p.building(), id, s.o.Files)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err == nil {
		err = s.global(func() error {
			if err := os.Remove(s.p.freeze(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return nil
		})
	}
	_ = owner.Unlock()
	if err == nil {
		err = os.Remove(s.p.freezeLock(id))
	}
	s.sweepLog("removing freeze "+id, err)
}

// removeBuild deletes a private directory no live worker owns.
func (s *Server) removeBuild(id string) {
	if busy, err := storage.ProbeFileLock(s.p.freezeLock(id)); err != nil || busy {
		return
	}
	if err := storage.DeleteOwned(s.p.building(), id, s.o.Files); err != nil {
		s.sweepLog("removing build "+id, err)
	}
}
