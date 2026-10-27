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

package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/pull/format"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// archivePart is where an archive's contiguous prefix is kept in a build's
// work directory while it arrives.
const archivePart = "archive.part"

// ChangedList is a fresh manifest that is not the one a build was fetching:
// other files, or the same files under another head. The build is discarded
// and made again from it, under its lease, which the source now holds.
type ChangedList struct {
	Manifest *format.Manifest
	Response format.Response
}

func (c *ChangedList) Error() string {
	return "pull: the conversation's files changed while they were fetched"
}

// localError is a failure of this machine's own storage while an archive
// arrives: it stops this conversation, and is never tried again over the
// network.
func localError(err error) error {
	return fmt.Errorf("%w: this machine's storage: %w", ErrConversation, err)
}

// RemoteSource is the files of one leased snapshot, fetched as one archive.
// When the snapshot goes away while they are fetched, a fresh manifest is
// asked for within the same acquisition: the same files under the same head
// carry on under its lease; anything else ends the build with a ChangedList.
type RemoteSource struct {
	r            *Remote
	conversation string
	m            *format.Manifest
	resp         format.Response
	tries        *Tries
	// restart is the failed attempt the acquisition last asked a successor
	// of, so a failure seen again is charged once.
	restart uint64
}

// Source is the files of the snapshot m and resp name.
func (r *Remote) Source(conversation string, m *format.Manifest, resp format.Response, tries *Tries) *RemoteSource {
	return &RemoteSource{r: r, conversation: conversation, m: m, resp: resp, tries: tries}
}

// Response is the lease the source holds now, which its caller releases.
func (s *RemoteSource) Response() format.Response { return s.resp }

// Fetch asks the server for one archive of names, waits for it, downloads
// it in ranges into work, checks its size and digest, and extracts it,
// giving each file to put once. One acquisition has one budget of four
// failures across every request it makes, a manifest refresh included, and
// a build made again from a changed list goes on with what is left of it.
func (s *RemoteSource) Fetch(ctx context.Context, work string, names []string, put func(string, io.Reader) error) error {
	listed := map[string]format.Entry{}
	for _, e := range s.m.Files {
		listed[e.File] = e
	}
	asked := make(map[string]format.Entry, len(names))
	for _, n := range names {
		e, ok := listed[n]
		if !ok {
			return fmt.Errorf("pull: %s is not in the manifest", n)
		}
		asked[n] = e
	}
	if s.tries.acquisition == nil {
		s.tries.acquisition = newBudget()
	}
	b := s.tries.acquisition
	part := filepath.Join(work, archivePart)
	defer os.Remove(part)
	for {
		err := s.acquire(ctx, part, names, asked, b, put)
		if !errors.Is(err, errGone) {
			if err == nil {
				s.tries.acquisition = nil
			}
			return err
		}
		// The snapshot went away: a fresh manifest, within this
		// acquisition's budget. Its bytes so far were of the old snapshot,
		// and are dropped.
		if err := s.tries.take("its snapshot went away"); err != nil {
			return err
		}
		m, resp, err := s.r.manifest(ctx, s.conversation, b, s.tries)
		if err != nil {
			return err
		}
		same := m.Digest() == s.m.Digest() && m.Conversation == s.m.Conversation && m.Session == s.m.Session && m.Head == s.m.Head
		s.m, s.resp, s.restart = m, resp, 0
		if !same {
			return &ChangedList{Manifest: m, Response: resp}
		}
	}
}

// acquire makes one archive of names ready, downloads it into part and
// extracts it. Bytes a file's entry does not hold are the remote's fault
// and stop it; a write of this machine's that fails stops the conversation.
func (s *RemoteSource) acquire(ctx context.Context, part string, names []string, asked map[string]format.Entry, b *budget, put func(string, io.Reader) error) error {
	id := format.ArchiveID(names)
	st, err := s.ready(ctx, names, id, b, true)
	if err != nil {
		return err
	}
	if err := s.download(ctx, part, names, id, st, b); err != nil {
		return err
	}
	f, err := os.Open(part)
	if err != nil {
		return localError(err)
	}
	defer f.Close()
	err = format.Extract(f, asked, func(e format.Entry, r io.Reader) error {
		// A member that cannot be read whole, as a tar cut inside it, is
		// the archive's fault, whatever put was doing when it failed.
		member := &readRecorder{r: r}
		if err := put(e.File, member); err != nil {
			if member.err != nil {
				return fmt.Errorf("%w: %s: %w", format.ErrArchive, e.File, member.err)
			}
			return err
		}
		return nil
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, format.ErrArchive), errors.Is(err, storage.ErrChecked):
		return fmt.Errorf("%w: %w", ErrRemote, err)
	case ctx.Err() != nil:
		return err
	}
	return localError(err)
}

// ready asks for the archive, by POST when create is set and by its state
// otherwise, and polls it until it is ready. A typed build failure asks for
// its successor by number.
func (s *RemoteSource) ready(ctx context.Context, names []string, id string, b *budget, create bool) (format.ArchiveState, error) {
	archives := s.r.endpoint("conversations", s.conversation, "snapshots", s.resp.Snapshot, "archives")
	state := s.r.endpoint("conversations", s.conversation, "snapshots", s.resp.Snapshot, "archives", id)
	for n := 0; ; {
		var a *answer
		var err error
		if create {
			body := map[string]any{"lease": s.resp.Lease, "files": names}
			if s.restart != 0 {
				body["restart"] = s.restart
			}
			a, err = s.r.metadata(ctx, http.MethodPost, archives, body, nil)
		} else {
			a, err = s.r.metadata(ctx, http.MethodGet, state, nil, map[string]string{"Asz-Pull-Lease": s.resp.Lease})
		}
		var header http.Header
		if err == nil {
			header = a.header
			var st format.ArchiveState
			var next archiveNext
			st, next, err = s.archiveAnswer(a, id, b)
			a.close()
			switch {
			case err == nil && next == archiveReady:
				return st, nil
			case err == nil && next == archivePoll:
				if err := sleep(ctx, pollEvery); err != nil {
					return format.ArchiveState{}, err
				}
				create = false
				continue
			case err == nil && next == archiveRestart:
				create = true
				continue
			}
		}
		if err := again(ctx, b, n, header, err); err != nil {
			return format.ArchiveState{}, err
		}
		n++
	}
}

// What an archive's answer asks of the client next.
type archiveNext int

const (
	archiveRetry archiveNext = iota
	archiveReady
	archivePoll
	archiveRestart
)

func (s *RemoteSource) archiveAnswer(a *answer, id string, b *budget) (format.ArchiveState, archiveNext, error) {
	switch a.status {
	case http.StatusOK, http.StatusAccepted:
		ready := a.status == http.StatusOK
		var st format.ArchiveState
		err := a.decode(func(body io.Reader) error {
			var rerr error
			st, rerr = format.ReadArchiveState(body, s.resp.Snapshot, id, ready)
			return rerr
		})
		if err != nil {
			return st, archiveRetry, err
		}
		if !ready {
			return st, archivePoll, nil
		}
		if a.header.Get("ETag") != `"`+st.Digest+`"` {
			return st, archiveRetry, fmt.Errorf("%w: a ready archive's ETag is not its digest", ErrRemote)
		}
		return st, archiveReady, nil
	}
	if !retryable(a.status) {
		return format.ArchiveState{}, archiveRetry, refusal(a, "the archive")
	}
	p, err := failed(a)
	if err != nil {
		return format.ArchiveState{}, archiveRetry, err
	}
	switch p.Type {
	case format.ProblemStorageState:
		_ = b.charge(errors.New(p.Detail))
		return format.ArchiveState{}, archiveRetry, fmt.Errorf("%w: %s", ErrConversation, p.Detail)
	case format.ProblemArchiveFailed:
		if a.status != http.StatusInternalServerError || p.Snapshot != s.resp.Snapshot || p.Archive != id || p.Attempt == 0 {
			return format.ArchiveState{}, archiveRetry, fmt.Errorf("%w: a failure of another archive", ErrRemote)
		}
		// A failure seen again, as a POST's lost answer replays it, is
		// charged once.
		if p.Attempt != s.restart {
			if err := b.charge(errors.New(p.Detail)); err != nil {
				return format.ArchiveState{}, archiveRetry, err
			}
			s.restart = p.Attempt
		}
		return format.ArchiveState{}, archiveRestart, nil
	}
	return format.ArchiveState{}, archiveRetry, fmt.Errorf("the archive answered %d %s", a.status, p.Detail)
}

// readRecorder keeps a read that failed, so a member of the archive that
// does not read is told from a write here that failed.
type readRecorder struct {
	r   io.Reader
	err error
}

func (r *readRecorder) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		r.err = err
	}
	return n, err
}

// rangeResult is how one range request ended.
type rangeResult int

const (
	rangeDone rangeResult = iota
	// rangeFailed is a failure asking again can fix, charged, with its
	// answer's headers for Retry-After.
	rangeFailed
	// rangeState is an answer that sends the client back to the archive's
	// state: being built again, built with another digest (412), or failed.
	rangeState
)

// download reads a ready archive into part in ranges of at most eight
// mebibytes, keeping only a contiguous prefix, and checks its size and
// digest. A range that stops early is asked again from its next byte to
// the end it had. An answer that sends the client back to the archive's
// state fetches its descriptor again, and a changed one starts again from
// byte zero.
func (s *RemoteSource) download(ctx context.Context, part string, names []string, id string, st format.ArchiveState, b *budget) error {
	// A part an earlier acquisition of this build left was of another
	// snapshot.
	if err := os.Remove(part); err != nil && !errors.Is(err, os.ErrNotExist) {
		return localError(err)
	}
	f, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return localError(err)
	}
	defer f.Close()
	target := s.r.endpoint("conversations", s.conversation, "snapshots", s.resp.Snapshot, "archives", id, "data")
	var have, end uint64
	ranged := false
	for n := 0; have < st.Bytes; {
		if !ranged {
			end, ranged = min(have+format.RangeLimit, st.Bytes)-1, true
		}
		got, result, header, err := s.rangeOf(ctx, target, id, have, end, st, f, b)
		have += got
		if err == nil && have == end+1 {
			ranged = false
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, ErrRemote) || errors.Is(err, ErrConversation) || errors.Is(err, errGone) {
			return err
		}
		if result == rangeState {
			if err != nil {
				if cerr := b.charge(err); cerr != nil {
					return cerr
				}
			}
			fresh, err := s.ready(ctx, names, id, b, false)
			if err != nil {
				return err
			}
			if fresh.Bytes != st.Bytes || fresh.Digest != st.Digest {
				st, have, ranged = fresh, 0, false
				if err := restartPart(f); err != nil {
					return localError(err)
				}
			}
			continue
		}
		if err == nil {
			err = fmt.Errorf("a range was cut at byte %d", have)
		}
		if err := again(ctx, b, n, header, err); err != nil {
			return err
		}
		n++
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return localError(err)
	}
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return localError(err)
	}
	if uint64(size) != st.Bytes || hex.EncodeToString(h.Sum(nil)) != st.Digest {
		return fmt.Errorf("%w: the archive's bytes are not its digest", ErrRemote)
	}
	return nil
}

// restartPart drops a prefix whose archive changed.
func restartPart(f *os.File) error { return dropFrom(f, 0) }

// dropFrom drops the part's bytes from offset on, and writes there next.
func dropFrom(f *os.File, offset uint64) error {
	if err := f.Truncate(int64(offset)); err != nil {
		return err
	}
	_, err := f.Seek(int64(offset), io.SeekStart)
	return err
}

// partWriter appends to the part and keeps a write that failed, so a
// failure of this machine's storage is told from a body that stopped.
type partWriter struct {
	f   *os.File
	err error
}

func (p *partWriter) Write(b []byte) (int, error) {
	n, err := p.f.Write(b)
	if err != nil {
		p.err = err
	}
	return n, err
}

// rangeOf asks for one range and appends what arrives to f, at most the
// range, after its status and headers are checked against the request and
// the descriptor. A body that stops early is a cut, kept to where it
// stopped. It returns the bytes kept, how the request ended, and the
// answer's headers. Every body is read under a bound: a range's while bytes
// keep arriving, any other answer's within the metadata time.
func (s *RemoteSource) rangeOf(ctx context.Context, target, id string, start, end uint64, st format.ArchiveState, f *os.File, b *budget) (uint64, rangeResult, http.Header, error) {
	if err := s.r.take(ctx); err != nil {
		return 0, rangeFailed, nil, err
	}
	defer s.r.give()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := s.r.request(ctx, http.MethodGet, target, nil, map[string]string{
		"Asz-Pull-Lease": s.resp.Lease,
		"Range":          fmt.Sprintf("bytes=%d-%d", start, end),
		"If-Match":       `"` + st.Digest + `"`,
	})
	if err != nil {
		return 0, rangeFailed, nil, err
	}
	resp, err := s.r.http.Do(req)
	if err != nil {
		return 0, rangeFailed, nil, transportError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		bound := time.AfterFunc(metadataWait, cancel)
		defer bound.Stop()
		result, err := s.rangeRefused(&answer{status: resp.StatusCode, header: resp.Header, body: resp.Body, done: func() {}}, id, start, end, b)
		return 0, result, resp.Header, err
	}
	length := end - start + 1
	switch {
	case resp.Header.Get("Content-Encoding") != "":
		return 0, rangeFailed, nil, fmt.Errorf("%w: a range was encoded", ErrRemote)
	case resp.Header.Get("ETag") != `"`+st.Digest+`"`:
		return 0, rangeFailed, nil, fmt.Errorf("%w: a range of another archive", ErrRemote)
	case resp.Header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", start, end, st.Bytes):
		return 0, rangeFailed, nil, fmt.Errorf("%w: a range that is not the one asked for", ErrRemote)
	case resp.ContentLength != int64(length):
		return 0, rangeFailed, nil, fmt.Errorf("%w: a range of another length", ErrRemote)
	}
	// Sixty seconds without a byte stops this request alone.
	stall := time.AfterFunc(stallWait, cancel)
	defer stall.Stop()
	out := &partWriter{f: f}
	n, err := io.Copy(out, io.LimitReader(&stallReader{r: resp.Body, timer: stall}, int64(length)))
	if out.err != nil {
		return uint64(n), rangeFailed, nil, localError(out.err)
	}
	// An error that comes with the range's last bytes leaves the range not
	// known whole: it is dropped, as one at its end is. One before its end
	// is a cut, its bytes kept.
	if err != nil && uint64(n) == length {
		if terr := dropFrom(f, start); terr != nil {
			return 0, rangeFailed, nil, localError(terr)
		}
		return 0, rangeFailed, nil, fmt.Errorf("a range's end: %w", err)
	}
	if err != nil {
		return uint64(n), rangeFailed, nil, err
	}
	if uint64(n) != length {
		return uint64(n), rangeFailed, nil, fmt.Errorf("a range was cut at byte %d", start+uint64(n))
	}
	// A byte past the range stops the remote. An error the transport
	// reports at the body's end, such as a length that does not match its
	// frames, fails the range: its bytes are dropped, and it is asked again
	// whole.
	var extra [1]byte
	m, err := resp.Body.Read(extra[:])
	if m > 0 {
		return uint64(n), rangeFailed, nil, fmt.Errorf("%w: a range longer than it said", ErrRemote)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		if terr := dropFrom(f, start); terr != nil {
			return 0, rangeFailed, nil, localError(terr)
		}
		return 0, rangeFailed, nil, fmt.Errorf("a range's end: %w", err)
	}
	return uint64(n), rangeDone, nil, nil
}

// rangeRefused reads an answer to a range that is not its bytes, checked as
// any answer is: a building or failed archive and a stale If-Match send
// the client back to the archive's state, storage-state stops the
// conversation, and anything not tried again is the refusal of its scope.
func (s *RemoteSource) rangeRefused(a *answer, id string, start, end uint64, b *budget) (rangeResult, error) {
	switch {
	case a.status == http.StatusOK:
		return rangeFailed, fmt.Errorf("%w: a range answered 200", ErrRemote)
	case a.status == http.StatusAccepted:
		// Being built again: the answer must name this snapshot and archive.
		if err := a.decode(func(body io.Reader) error {
			_, err := format.ReadArchiveState(body, s.resp.Snapshot, id, false)
			return err
		}); err != nil {
			return rangeFailed, err
		}
		return rangeState, nil
	case a.status == http.StatusPreconditionFailed:
		if _, err := problemOf(a); err != nil {
			return rangeFailed, err
		}
		return rangeState, errors.New("the archive is not the one asked for")
	case !retryable(a.status):
		return rangeFailed, refusal(a, fmt.Sprintf("the range %d-%d", start, end))
	}
	p, err := failed(a)
	if err != nil {
		return rangeFailed, err
	}
	switch p.Type {
	case format.ProblemStorageState:
		return rangeFailed, fmt.Errorf("%w: %s", ErrConversation, p.Detail)
	case format.ProblemArchiveFailed:
		// Checked as the archive's own answer is, and charged once for its
		// attempt: the state, asked for next, answers the same failure.
		if a.status != http.StatusInternalServerError || p.Snapshot != s.resp.Snapshot || p.Archive != id || p.Attempt == 0 {
			return rangeFailed, fmt.Errorf("%w: a failure of another archive", ErrRemote)
		}
		if p.Attempt != s.restart {
			if err := b.charge(errors.New(p.Detail)); err != nil {
				return rangeFailed, err
			}
			s.restart = p.Attempt
		}
		return rangeState, nil
	}
	return rangeFailed, fmt.Errorf("a range answered %d %s", a.status, p.Detail)
}

// stallReader restarts its timer on every byte that arrives.
type stallReader struct {
	r     io.Reader
	timer *time.Timer
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.timer.Reset(stallWait)
	}
	return n, err
}
