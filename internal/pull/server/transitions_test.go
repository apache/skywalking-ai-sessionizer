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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/pull/format"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// hookFS is the disk, with a record of every replacement and a hook a test
// sets on each replacement and each new file's install.
type hookFS struct {
	storage.DiskFileSystem
	mu       sync.Mutex
	replaced []string
	// onReplace, when set, runs before a replacement of to, and an error it
	// returns is the replacement's.
	onReplace func(to string) error
	// onInstall, when set, runs before a file is made or installed at to.
	onInstall func(to string)
}

func (h *hookFS) Replace(parent, from, to string) error {
	path := filepath.Join(parent, to)
	h.mu.Lock()
	h.replaced = append(h.replaced, path)
	hook := h.onReplace
	h.mu.Unlock()
	if hook != nil {
		if err := hook(path); err != nil {
			return err
		}
	}
	return h.DiskFileSystem.Replace(parent, from, to)
}

func (h *hookFS) install(to string) {
	h.mu.Lock()
	hook := h.onInstall
	h.mu.Unlock()
	if hook != nil {
		hook(to)
	}
}

func (h *hookFS) Create(path string, mode os.FileMode) (storage.WriteFile, error) {
	h.install(path)
	return h.DiskFileSystem.Create(path, mode)
}

func (h *hookFS) Link(from, to string) error {
	h.install(to)
	return h.DiskFileSystem.Link(from, to)
}

func (h *hookFS) RenameNoReplace(from, to string) error {
	h.install(to)
	return h.DiskFileSystem.RenameNoReplace(from, to)
}

// sawReplace says whether path was replaced since the record was last
// cleared, and clears it.
func (h *hookFS) sawReplace(path string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	seen := false
	for _, p := range h.replaced {
		seen = seen || p == path
	}
	h.replaced = nil
	return seen
}

// setReplace sets the replacement hook while the server runs.
func (h *hookFS) setReplace(fn func(to string) error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onReplace = fn
}

// setInstall sets the install hook while the server runs.
func (h *hookFS) setInstall(fn func(to string)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onInstall = fn
}

func (h *hookFS) clear() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.replaced = nil
}

// caller is the served token as the guard admits it.
func (s *served) caller() Caller { return Caller{Name: "prod", Hash: hashOf(s.token)} }

// A lifecycle fact another pass only saw is committed by a fresh write of
// its whole record before anything relies on it: an expiry before a
// request is refused, a supersession before a freeze declines to reuse,
// and every older snapshot's supersession when a newer one is published.
func TestSeenFactsAreCommittedBeforeTheyAreReliedOn(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	files := &hookFS{}
	s := serveFS(t, root, time.Hour, files)
	_, resp := s.manifest(conversation)
	dir := s.srv.p.snapshot(conversation, resp.Snapshot)
	rec, err := readSnapshot(dir, conversation, resp.Snapshot)
	if err != nil {
		t.Fatal(err)
	}

	// A supersession seen by reuse.
	later := laterTag(t, resp.Snapshot)
	if err := s.srv.global(func() error {
		st, err := readState(s.srv.p)
		if err != nil {
			return err
		}
		st.GreatestTag = later
		if err := replaceRecord(s.srv.p.state(), st, nil); err != nil {
			return err
		}
		superseded := *rec
		superseded.SupersededBy = later
		return replaceRecord(snapshotRecordPath(dir), &superseded, nil)
	}); err != nil {
		t.Fatal(err)
	}
	files.clear()
	tag, err := s.srv.reuse(conversation, &format.Accepted{Digest: format.ManifestDigest(rec.ManifestDigest)})
	if err != nil || tag != "" || !files.sawReplace(snapshotRecordPath(dir)) {
		t.Fatalf("reuse of a superseded snapshot: %q %v", tag, err)
	}

	// A supersession seen when a newer snapshot is published.
	files.clear()
	if err := s.srv.global(func() error { return s.srv.supersede(conversation, laterTag(t, later)) }); err != nil {
		t.Fatal(err)
	}
	if !files.sawReplace(snapshotRecordPath(dir)) {
		t.Fatal("an older snapshot already superseded was not committed again")
	}

	// An expiry seen by a request.
	use, _, err := readLastUse(dir, resp.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	use.State = useExpired
	if err := replaceRecord(lastUsePath(dir), use, nil); err != nil {
		t.Fatal(err)
	}
	files.clear()
	err = s.srv.global(func() error {
		_, err := s.srv.lifecycle(conversation, resp.Snapshot, s.srv.now())
		return err
	})
	if !errors.Is(err, errExpired) || !files.sawReplace(lastUsePath(dir)) {
		t.Fatalf("an expiry seen: %v", err)
	}
}

// laterTag is a canonical tag one second after tag.
func laterTag(t *testing.T, tag string) string {
	t.Helper()
	at, err := storage.ParseTag(tag)
	if err != nil {
		t.Fatal(err)
	}
	return storage.Stamp(at.Add(time.Second))
}

// A freeze whose earlier attempt left its own publication unfinished is
// claimed again and settles that intent itself, holding its owner, rather
// than wait for an owner that is itself.
func TestAReclaimedFreezeSettlesItsOwnIntent(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	s := serve(t, root, time.Hour)
	id := strings.Repeat("ef", 32)
	// The owner is held while the job is set up, so no sweep recovers the
	// intent before the job is claimed again.
	owner, err := storage.TryLockFile(s.srv.p.freezeLock(id))
	if err != nil {
		t.Fatal(err)
	}
	tag := storage.Stamp(time.Now().UTC().Add(-time.Hour))
	if err := s.srv.global(func() error {
		if err := s.srv.conversationSetup(conversation); err != nil {
			return err
		}
		if err := storage.MakeOwnedDir(s.srv.p.build(id), nil); err != nil {
			return err
		}
		if err := replaceRecord(s.srv.p.freeze(id), &freezeJob{Schema: stateSchema, Freeze: id, Conversation: conversation,
			Attempt: 1, Started: true, State: jobBuilding}, nil); err != nil {
			return err
		}
		return replaceRecord(s.srv.p.state(), &state{Schema: stateSchema, GreatestTag: tag,
			Publishing: &publishing{Conversation: conversation, Build: id, Tag: tag, ManifestDigest: strings.Repeat("ab", 32)}}, nil)
	}); err != nil {
		t.Fatal(err)
	}
	_ = owner.Unlock()
	path := "/pull/v1/conversations/" + conversation + "/manifest"
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		a := s.do(http.MethodGet, path, nil, map[string]string{"Asz-Pull-Freeze": id})
		if a.StatusCode == http.StatusOK {
			break
		}
		if a.StatusCode != http.StatusAccepted || time.Now().After(deadline) {
			t.Fatalf("the reclaimed freeze: %d %s", a.StatusCode, a.Body)
		}
	}
	h, err := readHistory(s.srv.p, conversation)
	if err != nil || !h.removed(tag) {
		t.Fatalf("its own abandoned tag: %+v %v", h, err)
	}
}

// A tag whose publication has not finished is refused to every request,
// while its owner lives and after its recovery discards it.
func TestAnUnfinishedPublicationsTagIsRefused(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	s := serve(t, root, time.Hour)
	m, resp := s.manifest(conversation)
	dir := s.srv.p.snapshot(conversation, resp.Snapshot)
	rec, err := readSnapshot(dir, conversation, resp.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	build := strings.Repeat("cd", 32)
	owner, err := storage.TryLockFile(s.srv.p.freezeLock(build))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.srv.global(func() error {
		st, err := readState(s.srv.p)
		if err != nil {
			return err
		}
		st.Publishing = &publishing{Conversation: conversation, Build: build, Tag: resp.Snapshot, ManifestDigest: rec.ManifestDigest}
		return replaceRecord(s.srv.p.state(), st, nil)
	}); err != nil {
		t.Fatal(err)
	}
	archives := "/pull/v1/conversations/" + conversation + "/snapshots/" + resp.Snapshot + "/archives"
	body := map[string]any{"lease": resp.Lease, "files": []string{m.Files[0].File}}
	if a := s.do(http.MethodPost, archives, body, nil); a.StatusCode != http.StatusGone {
		t.Fatalf("while its owner lives: %d %s", a.StatusCode, a.Body)
	}
	_ = owner.Unlock()
	s.srv.sweep()
	if a := s.do(http.MethodPost, archives, body, nil); a.StatusCode != http.StatusGone {
		t.Fatalf("after its recovery: %d %s", a.StatusCode, a.Body)
	}
	if there, _ := exists(dir); there {
		t.Fatal("the recovery left the unfinished tree")
	}
}

// A tree a removal detached and did not delete is deleted by a later pass,
// its tag's removal written again first.
func TestADetachedTreeIsDeletedOnALaterPass(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	s := serve(t, root, time.Hour)
	_, resp := s.manifest(conversation)
	at, err := storage.ParseTag(resp.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	earlier := storage.Stamp(at.Add(-time.Second))
	detached := filepath.Join(s.srv.p.removing(conversation), earlier+"-"+strings.Repeat("0", 64))
	if err := os.MkdirAll(filepath.Join(detached, "_leases"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(detached, "_manifest"), []byte("{}"), 0o444); err != nil {
		t.Fatal(err)
	}
	s.srv.sweep()
	if there, _ := exists(detached); there {
		t.Fatal("the detached tree is still there")
	}
	h, err := readHistory(s.srv.p, conversation)
	if err != nil || !h.removed(earlier) {
		t.Fatalf("the history: %+v %v", h, err)
	}
}

// A request renews its snapshot's use while it is answered, and stops once
// its lease is released: renewal checks the lease each time. Stopping it
// waits until it has stopped.
func TestRenewalEndsWithItsLease(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	// A time to live of two seconds renews every second.
	s := serve(t, root, 2*time.Second)
	_, resp := s.manifest(conversation)
	dir := s.srv.p.snapshot(conversation, resp.Snapshot)
	lastUse := func() string {
		u, _, err := readLastUse(dir, resp.Snapshot)
		if err != nil {
			t.Fatal(err)
		}
		return u.LastUse
	}
	stop := s.srv.renewWhile(context.Background(), conversation, resp.Snapshot, resp.Lease, s.caller())
	before := lastUse()
	s.clock.add(500 * time.Millisecond)
	for deadline := time.Now().Add(5 * time.Second); lastUse() == before; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the use was not renewed")
		}
	}
	if err := s.srv.release(conversation, resp.Snapshot, resp.Lease, s.caller()); err != nil {
		t.Fatal(err)
	}
	released := lastUse()
	s.clock.add(500 * time.Millisecond)
	time.Sleep(2500 * time.Millisecond)
	if lastUse() != released {
		t.Fatal("the use was renewed after its lease was released")
	}
	done := make(chan struct{})
	go func() {
		stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stopping renewal did not return")
	}
}

// blockingWriter is a client that stops reading at the answer's first
// flush, until it is let go.
type blockingWriter struct {
	header  http.Header
	code    int
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingWriter() *blockingWriter {
	return &blockingWriter{header: http.Header{}, entered: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockingWriter) Header() http.Header         { return b.header }
func (b *blockingWriter) WriteHeader(code int)        { b.code = code }
func (b *blockingWriter) Write(p []byte) (int, error) { return len(p), nil }
func (b *blockingWriter) Flush() {
	b.once.Do(func() {
		close(b.entered)
		<-b.release
	})
}

// A request keeps its token's slot until its answer's last flush is out:
// with one slot, another request of the token is refused while an answer
// with no body is still being sent, on the pull routes and on the page.
func TestASlotIsKeptUntilTheAnswerIsSent(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	// The pull server admits through a guard of one slot per token.
	var guard *Guard
	s := serveOpts(t, root, time.Hour, func(o *Options) {
		guard = NewGuard(o.Guard.keeper, 1, nil)
		o.Guard = guard
	})
	_, resp := s.manifest(conversation)
	h := guard.Protect(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }), s.srv.Handler())
	request := func(method, path, body string) *http.Request {
		r, err := http.NewRequest(method, path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+s.token)
		return r
	}
	for name, first := range map[string]*http.Request{
		"pull": request(http.MethodPost, "/pull/v1/conversations/"+conversation+"/snapshots/"+resp.Snapshot+"/done", `{"lease":"`+resp.Lease+`"}`),
		"page": request(http.MethodGet, "/api/status", ""),
	} {
		w := newBlockingWriter()
		done := make(chan struct{})
		go func() {
			h.ServeHTTP(w, first)
			close(done)
		}()
		select {
		case <-w.entered:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: the answer was never flushed", name)
		}
		rec := &recorder{header: http.Header{}}
		h.ServeHTTP(rec, request(http.MethodGet, "/pull/v1/conversations", ""))
		close(w.release)
		<-done
		if rec.code != http.StatusTooManyRequests {
			t.Fatalf("%s: a second request while the first was sent: %d", name, rec.code)
		}
	}
}

// recorder records an answer's status.
type recorder struct {
	header http.Header
	code   int
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) WriteHeader(code int)        { r.code = code }
func (r *recorder) Write(p []byte) (int, error) { return len(p), nil }
func (r *recorder) Flush()                      {}

// An archive accepted while every builder was busy is built once one comes
// free, with no request asking for it; one whose snapshot expired meanwhile
// is never started.
func TestAQueuedArchiveIsBuiltWithNoRequest(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	s := serve(t, root, time.Hour)
	m, resp := s.manifest(conversation)
	dir := s.srv.p.snapshot(conversation, resp.Snapshot)
	archives := "/pull/v1/conversations/" + conversation + "/snapshots/" + resp.Snapshot + "/archives"
	queue := func(names []string) string {
		for range slotsPerKind {
			s.srv.archiveSlots <- struct{}{}
		}
		a := s.do(http.MethodPost, archives, map[string]any{"lease": resp.Lease, "files": names}, nil)
		for range slotsPerKind {
			<-s.srv.archiveSlots
		}
		if a.StatusCode != http.StatusAccepted {
			t.Fatalf("queued: %d %s", a.StatusCode, a.Body)
		}
		return format.ArchiveID(names)
	}
	job := func(id string) *archiveJob {
		var j *archiveJob
		if err := s.srv.global(func() (err error) { j, err = readArchive(archivePath(dir, id, ".json"), resp.Snapshot, id); return }); err != nil {
			t.Fatal(err)
		}
		return j
	}
	id := queue([]string{m.Files[0].File})
	if j := job(id); j.Started {
		t.Fatal("the job started with no builder free")
	}
	s.srv.schedule()
	for deadline := time.Now().Add(10 * time.Second); job(id).State != jobReady; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the queued archive was not built: %+v", job(id))
		}
	}

	other := queue([]string{m.Files[1].File})
	if err := s.srv.global(func() error { return s.srv.expire(dir, resp.Snapshot) }); err != nil {
		t.Fatal(err)
	}
	s.srv.schedule()
	time.Sleep(200 * time.Millisecond)
	if j := job(other); j.Started || j.State != jobBuilding {
		t.Fatalf("work started on an expired snapshot: %+v", j)
	}
}

// An accepted archive's list of names is never made again: one gone is a
// storage failure. A list no accepted job owns is replaced by a fresh one
// before a job is accepted.
func TestAnAcceptedListIsNeverMadeAgain(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	s := serve(t, root, time.Hour)
	m, resp := s.manifest(conversation)
	dir := s.srv.p.snapshot(conversation, resp.Snapshot)
	archives := "/pull/v1/conversations/" + conversation + "/snapshots/" + resp.Snapshot + "/archives"

	names := []string{m.Files[0].File}
	s.archive(conversation, resp, names)
	id := format.ArchiveID(names)
	if err := os.Remove(archivePath(dir, id, ".names")); err != nil {
		t.Fatal(err)
	}
	a := s.do(http.MethodPost, archives, map[string]any{"lease": resp.Lease, "files": names}, nil)
	if a.StatusCode != http.StatusInternalServerError || !strings.Contains(string(a.Body), format.ProblemStorageState) {
		t.Fatalf("an accepted list gone: %d %s", a.StatusCode, a.Body)
	}
	if there, _ := exists(archivePath(dir, id, ".names")); there {
		t.Fatal("the accepted list was made again")
	}

	orphan := []string{m.Files[1].File}
	if _, err := s.srv.installNames(dir, archiveRequest{conversation: conversation, tag: resp.Snapshot, id: format.ArchiveID(orphan), names: orphan}); err != nil {
		t.Fatal(err)
	}
	// The leftover is told from a fresh list by its bytes, a space before its
	// end, never by file identity: Windows reads a file's identity only when
	// it is first compared, and Linux can give a new file the old one's.
	leftover := archivePath(dir, format.ArchiveID(orphan), ".names")
	fresh, err := os.ReadFile(leftover)
	if err != nil {
		t.Fatal(err)
	}
	marked := strings.TrimSuffix(string(fresh), "\n") + " \n"
	if err := os.Chmod(leftover, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leftover, []byte(marked), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(leftover, 0o444); err != nil {
		t.Fatal(err)
	}
	s.archive(conversation, resp, orphan)
	if now, err := os.ReadFile(leftover); err != nil || string(now) != string(fresh) {
		t.Fatalf("the leftover list was kept: %q, %v", now, err)
	}
}

// An archive's acceptance checks the request's lease again after its list
// is installed: a lease released meanwhile accepts nothing.
func TestAcceptanceChecksTheLeaseAgain(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	files := &hookFS{}
	s := serveFS(t, root, time.Hour, files)
	m, resp := s.manifest(conversation)
	var once sync.Once
	files.onInstall = func(to string) {
		if strings.HasSuffix(to, ".names") {
			once.Do(func() {
				if err := s.srv.release(conversation, resp.Snapshot, resp.Lease, s.caller()); err != nil {
					t.Error(err)
				}
			})
		}
	}
	names := []string{m.Files[0].File}
	a := s.do(http.MethodPost, "/pull/v1/conversations/"+conversation+"/snapshots/"+resp.Snapshot+"/archives",
		map[string]any{"lease": resp.Lease, "files": names}, nil)
	if a.StatusCode != http.StatusForbidden {
		t.Fatalf("a lease released during acceptance: %d %s", a.StatusCode, a.Body)
	}
	dir := s.srv.p.snapshot(conversation, resp.Snapshot)
	if there, _ := exists(archivePath(dir, format.ArchiveID(names), ".json")); there {
		t.Fatal("a job was accepted")
	}
}

// failArchive saves an archive failed at attempt 1, its names installed,
// and returns its identifier.
func failArchive(t *testing.T, s *served, conversation, tag string, names []string) string {
	t.Helper()
	id := format.ArchiveID(names)
	dir := s.srv.p.snapshot(conversation, tag)
	if _, err := s.srv.installNames(dir, archiveRequest{conversation: conversation, tag: tag, id: id, names: names}); err != nil {
		t.Fatal(err)
	}
	if err := replaceRecord(archivePath(dir, id, ".json"), failedArchive(tag, id, "the disk filled"), nil); err != nil {
		t.Fatal(err)
	}
	return id
}

func failedArchive(tag, id, detail string) *archiveJob {
	return &archiveJob{Schema: stateSchema, Snapshot: tag, Archive: id, Attempt: 1, Started: true, State: jobFailed,
		Problem: &Problem{Type: format.ProblemArchiveFailed, Title: "Internal Server Error", Detail: detail, Status: 500,
			Snapshot: tag, Archive: id, Attempt: 1}}
}

// A restart keeps the failure it replaces: a retained record already there
// must be that failure, and is committed again by a fresh write before the
// successor is saved; another failure there is a storage failure.
func TestARestartCommitsTheFailureItKeeps(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	files := &hookFS{}
	s := serveFS(t, root, time.Hour, files)
	m, resp := s.manifest(conversation)
	dir := s.srv.p.snapshot(conversation, resp.Snapshot)
	archives := "/pull/v1/conversations/" + conversation + "/snapshots/" + resp.Snapshot + "/archives"

	names := []string{m.Files[0].File}
	id := failArchive(t, s, conversation, resp.Snapshot, names)
	retained := archivePath(dir, id, ".failed-1.json")
	if err := newRecord(retained, failedArchive(resp.Snapshot, id, "the disk filled"), nil); err != nil {
		t.Fatal(err)
	}
	files.clear()
	if a := s.do(http.MethodPost, archives, map[string]any{"lease": resp.Lease, "files": names, "restart": 1}, nil); a.StatusCode >= 300 {
		t.Fatalf("a restart: %d %s", a.StatusCode, a.Body)
	}
	if !files.sawReplace(retained) {
		t.Fatal("the retained failure was not committed again")
	}

	other := []string{m.Files[1].File}
	otherID := failArchive(t, s, conversation, resp.Snapshot, other)
	if err := newRecord(archivePath(dir, otherID, ".failed-1.json"), failedArchive(resp.Snapshot, otherID, "another failure"), nil); err != nil {
		t.Fatal(err)
	}
	a := s.do(http.MethodPost, archives, map[string]any{"lease": resp.Lease, "files": other, "restart": 1}, nil)
	if a.StatusCode != http.StatusInternalServerError || !strings.Contains(string(a.Body), format.ProblemStorageState) {
		t.Fatalf("another failure retained: %d %s", a.StatusCode, a.Body)
	}
}

// A replacement Windows finds busy runs the whole transition again: a
// publication whose state write is refused once still publishes.
func TestABusyReplacementRunsTheTransitionAgain(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	files := &hookFS{}
	s := serveFS(t, root, time.Hour, files)
	// Armed once the server serves, so the refusal falls on the
	// publication's reservation, not on the server's start.
	var once sync.Once
	busy := false
	files.setReplace(func(to string) error {
		var err error
		if filepath.Base(to) == "_state" {
			once.Do(func() {
				busy = true
				err = &storage.ControlBusyError{Err: errors.New("the replacement is busy")}
			})
		}
		return err
	})
	if _, resp := s.manifest(conversation); resp.Snapshot == "" || !busy {
		t.Fatalf("no snapshot, or no busy replacement met: %v", busy)
	}
}

// A grant run again after a busy replacement keeps the one lease it made:
// no lease is left open that no answer named.
func TestARetriedGrantKeepsOneLease(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	files := &hookFS{}
	s := serveFS(t, root, time.Hour, files)
	// The publication writes the first use with no lease yet; the clock
	// moves on then, so the grant records a use of its own, which is the
	// replacement found busy.
	var published, busy sync.Once
	var fired atomic.Bool
	files.setReplace(func(to string) error {
		var err error
		if filepath.Base(to) == format.LastUseRecord {
			if entries, _ := os.ReadDir(filepath.Join(filepath.Dir(to), storage.LeasesDir)); len(entries) == 0 {
				published.Do(func() { s.clock.add(time.Second) })
			} else {
				busy.Do(func() {
					fired.Store(true)
					err = &storage.ControlBusyError{Err: errors.New("the replacement is busy")}
				})
			}
		}
		return err
	})
	_, resp := s.manifest(conversation)
	if !fired.Load() {
		t.Fatal("the grant met no busy replacement")
	}
	entries, err := os.ReadDir(leasesDir(s.srv.p.snapshot(conversation, resp.Snapshot)))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != resp.Lease+".json" {
		t.Fatalf("the leases: %v, the answer's %s", entries, resp.Lease)
	}
}

// A control file is read where it is, never through a link: a lease that is
// a link to a matching record elsewhere is refused. A link inside a server
// snapshot's control directories fails its acceptance.
func TestControlsAreNeverReadThroughALink(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	s := serve(t, root, time.Hour)
	m, resp := s.manifest(conversation)
	dir := s.srv.p.snapshot(conversation, resp.Snapshot)
	lease := leasePath(dir, resp.Lease)
	elsewhere := filepath.Join(t.TempDir(), "lease.json")
	b, err := os.ReadFile(lease)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(elsewhere, b, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(lease); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, lease); err != nil {
		t.Skip("no symbolic links here")
	}
	a := s.do(http.MethodPost, "/pull/v1/conversations/"+conversation+"/snapshots/"+resp.Snapshot+"/archives",
		map[string]any{"lease": resp.Lease, "files": []string{m.Files[0].File}}, nil)
	if a.StatusCode != http.StatusInternalServerError {
		t.Fatalf("a lease through a link: %d %s", a.StatusCode, a.Body)
	}
	if err := os.Remove(lease); err != nil {
		t.Fatal(err)
	}
	rec, err := readSnapshot(dir, conversation, resp.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(archivesDir(dir), strings.Repeat("a", 64)+".json")); err != nil {
		t.Fatal(err)
	}
	if _, err := format.Accept(dir, format.ServerSnapshot, format.Want{Conversation: conversation,
		ManifestDigest: format.ManifestDigest(rec.ManifestDigest), Session: rec.Session}); err == nil {
		t.Fatal("a link among the archives accepted")
	}
}

// A failed freeze's saved problem is checked as an answer must be: an empty
// one is a storage failure, never an answer with no status.
func TestAMalformedSavedProblemIsAStorageFailure(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	s := serve(t, root, time.Hour)
	if err := s.srv.global(func() error { return s.srv.conversationSetup(conversation) }); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("ef", 32)
	record := `{"schema":"pull-state/1","freeze":"` + id + `","conversation":"` + conversation +
		`","attempt":1,"started":true,"state":"failed","problem":{},"finished_at":"2026-10-01T00:00:00.000000000Z"}`
	if err := os.WriteFile(s.srv.p.freeze(id), []byte(record), 0o644); err != nil {
		t.Fatal(err)
	}
	a := s.do(http.MethodGet, "/pull/v1/conversations/"+conversation+"/manifest", nil, map[string]string{"Asz-Pull-Freeze": id})
	if a.StatusCode != http.StatusInternalServerError || !strings.Contains(string(a.Body), format.ProblemStorageState) {
		t.Fatalf("an empty saved problem: %d %s", a.StatusCode, a.Body)
	}
}

// A list of names is read as it streams in, and refused at the first name
// past what the snapshot holds, before it is kept.
func TestANamesListPastItsBoundIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.names")
	names := []string{"a/1", "a/2", "a/3"}
	if err := newRecord(path, &archiveNames{Schema: stateSchema, Snapshot: "t", Archive: format.ArchiveID(names), Files: names}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := readNames(path, "t", format.ArchiveID(names), 3); err != nil {
		t.Fatal(err)
	}
	if _, err := readNames(path, "t", format.ArchiveID(names), 2); err == nil {
		t.Fatal("three names read against a bound of two")
	}
}

// A superseded snapshot whose last lease goes while work still holds it is
// expired at once, its last use kept: no new lease is granted on it, and it
// goes once that work ends.
func TestASupersededSnapshotHeldAtItsLastReleaseExpires(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	s := serve(t, root, time.Hour)
	_, resp := s.manifest(conversation)
	dir := s.srv.p.snapshot(conversation, resp.Snapshot)
	later := laterTag(t, resp.Snapshot)
	if err := s.srv.global(func() error {
		st, err := readState(s.srv.p)
		if err != nil {
			return err
		}
		st.GreatestTag = later
		if err := replaceRecord(s.srv.p.state(), st, nil); err != nil {
			return err
		}
		return s.srv.supersede(conversation, later)
	}); err != nil {
		t.Fatal(err)
	}
	before, _, err := readLastUse(dir, resp.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	release, err := s.srv.activeWork(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.srv.release(conversation, resp.Snapshot, resp.Lease, s.caller()); err != nil {
		t.Fatal(err)
	}
	after, _, err := readLastUse(dir, resp.Snapshot)
	if err != nil || after.State != useExpired || after.LastUse != before.LastUse {
		t.Fatalf("the last use: %+v, was %+v: %v", after, before, err)
	}
	if _, err := s.srv.grantLease(conversation, resp.Snapshot, s.caller()); !errors.Is(err, errGone) {
		t.Fatalf("a lease on it: %v", err)
	}
	release()
	s.srv.retain(conversation)
	if there, _ := exists(dir); there {
		t.Fatal("the expired snapshot stayed after its work ended")
	}
}

// A history's removed tags stay in increasing order, however they are
// removed, and one out of order does not read.
func TestAHistoryKeepsItsTagsInOrder(t *testing.T) {
	h := &history{Schema: stateSchema, Conversation: "c", Removed: []string{}}
	for _, tag := range []string{"20261003T000000.000000000Z", "20261001T000000.000000000Z", "20261002T000000.000000000Z", "20261001T000000.000000000Z"} {
		h.remove(tag)
	}
	if strings.Join(h.Removed, ",") != "20261001T000000.000000000Z,20261002T000000.000000000Z,20261003T000000.000000000Z" {
		t.Fatalf("%v", h.Removed)
	}
	p := paths{root: t.TempDir()}
	if err := os.MkdirAll(p.conversation("c"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.Removed = []string{"20261002T000000.000000000Z", "20261001T000000.000000000Z"}
	if err := replaceRecord(p.history("c"), h, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := readHistory(p, "c"); err == nil {
		t.Fatal("a history out of order read")
	}
}

// Each pull request is logged once, with the files it named, the snapshot
// and archive it was answered with where its path names neither, and, for
// a range, what it asked for and what was sent.
func TestARangeIsLoggedWithWhatWasSent(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	var mu sync.Mutex
	var lines []string
	s := serveOpts(t, root, time.Hour, func(o *Options) {
		o.Log = func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			lines = append(lines, fmt.Sprintf(format, args...))
		}
	})
	m, resp := s.manifest(conversation)
	st := s.archive(conversation, resp, []string{m.Files[0].File})
	s.download(conversation, resp, st)
	// The line is written once the answer is sent, which can be just after
	// the client has read it.
	wants := []string{
		fmt.Sprintf("%s %s  files %d status 200", conversation, resp.Snapshot, len(m.Files)),
		fmt.Sprintf("%s %s %s files 1 status", conversation, resp.Snapshot, st.Archive),
		fmt.Sprintf("files 1 range 0-%d sent %d status 206", st.Bytes-1, st.Bytes),
	}
	logged := func(want string) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, l := range lines {
			if strings.Contains(l, want) {
				return true
			}
		}
		return false
	}
	for _, want := range wants {
		for deadline := time.Now().Add(5 * time.Second); !logged(want); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				mu.Lock()
				defer mu.Unlock()
				t.Fatalf("no line says %q in %q", want, lines)
			}
		}
	}
}

// A sweep recovers an abandoned publication within a freeze slot of its
// own: with every slot busy it leaves the recovery to a later pass.
func TestRecoveryWaitsForAFreezeSlot(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	s := serve(t, root, time.Hour)
	if err := s.srv.global(func() error { return s.srv.conversationSetup(conversation) }); err != nil {
		t.Fatal(err)
	}
	tag := storage.Stamp(time.Now().UTC().Add(-time.Hour))
	if err := replaceRecord(s.srv.p.state(), &state{Schema: stateSchema, GreatestTag: tag,
		Publishing: &publishing{Conversation: conversation, Build: strings.Repeat("cd", 32), Tag: tag, ManifestDigest: strings.Repeat("ab", 32)}}, nil); err != nil {
		t.Fatal(err)
	}
	intent := func() *publishing {
		st, err := readState(s.srv.p)
		if err != nil {
			t.Fatal(err)
		}
		return st.Publishing
	}
	for range slotsPerKind {
		s.srv.freezeSlots <- struct{}{}
	}
	s.srv.sweep()
	busy := intent()
	for range slotsPerKind {
		<-s.srv.freezeSlots
	}
	if busy == nil {
		t.Fatal("a recovery ran with every freeze slot busy")
	}
	s.srv.sweep()
	if intent() != nil {
		t.Fatal("the recovery did not run once a slot came free")
	}
}

// A request's renewal ends with the request: cancelled, it renews no more.
func TestRenewalEndsWithItsRequest(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	s := serve(t, root, 2*time.Second)
	_, resp := s.manifest(conversation)
	dir := s.srv.p.snapshot(conversation, resp.Snapshot)
	lastUse := func() string {
		u, _, err := readLastUse(dir, resp.Snapshot)
		if err != nil {
			t.Fatal(err)
		}
		return u.LastUse
	}
	ctx, cancel := context.WithCancel(context.Background())
	stop := s.srv.renewWhile(ctx, conversation, resp.Snapshot, resp.Lease, s.caller())
	defer stop()
	before := lastUse()
	s.clock.add(500 * time.Millisecond)
	for deadline := time.Now().Add(5 * time.Second); lastUse() == before; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the use was not renewed")
		}
	}
	cancel()
	time.Sleep(100 * time.Millisecond)
	cancelled := lastUse()
	s.clock.add(500 * time.Millisecond)
	time.Sleep(2500 * time.Millisecond)
	if lastUse() != cancelled {
		t.Fatal("the use was renewed after its request was cancelled")
	}
}

// A request on a snapshot that is no longer served is refused before
// anything of the snapshot is read: an expired one with a damaged manifest
// answers 410, never a storage failure.
func TestAnExpiredSnapshotIsRefusedBeforeItsManifestIsRead(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	s := serve(t, root, time.Hour)
	m, resp := s.manifest(conversation)
	dir := s.srv.p.snapshot(conversation, resp.Snapshot)
	if err := s.srv.global(func() error { return s.srv.expire(dir, resp.Snapshot) }); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(dir, format.ManifestFile)
	if err := os.Chmod(manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte("damaged"), 0o444); err != nil {
		t.Fatal(err)
	}
	a := s.do(http.MethodPost, "/pull/v1/conversations/"+conversation+"/snapshots/"+resp.Snapshot+"/archives",
		map[string]any{"lease": resp.Lease, "files": []string{m.Files[0].File}}, nil)
	if a.StatusCode != http.StatusGone {
		t.Fatalf("an expired snapshot: %d %s", a.StatusCode, a.Body)
	}
}

// The last lease of a superseded snapshot expires it in the same
// transition: when its removal then fails, no lease is granted on it, and a
// later pass removes it.
func TestALastReleaseExpiresASupersededSnapshot(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	files := &hookFS{}
	s := serveFS(t, root, time.Hour, files)
	_, resp := s.manifest(conversation)
	dir := s.srv.p.snapshot(conversation, resp.Snapshot)
	later := laterTag(t, resp.Snapshot)
	if err := s.srv.global(func() error {
		st, err := readState(s.srv.p)
		if err != nil {
			return err
		}
		st.GreatestTag = later
		if err := replaceRecord(s.srv.p.state(), st, nil); err != nil {
			return err
		}
		return s.srv.supersede(conversation, later)
	}); err != nil {
		t.Fatal(err)
	}
	failed := errors.New("the disk failed")
	files.setReplace(func(to string) error {
		if to == s.srv.p.history(conversation) {
			return failed
		}
		return nil
	})
	if err := s.srv.release(conversation, resp.Snapshot, resp.Lease, s.caller()); !errors.Is(err, failed) {
		t.Fatalf("a release whose removal fails: %v", err)
	}
	if _, err := s.srv.grantLease(conversation, resp.Snapshot, s.caller()); !errors.Is(err, errGone) {
		t.Fatalf("a lease after the last release: %v", err)
	}
	files.setReplace(nil)
	s.srv.retain(conversation)
	if there, _ := exists(dir); there {
		t.Fatal("the superseded snapshot stayed after its last release")
	}
}

// A saved problem's members are checked as an answer's are: one with no
// detail, or a source failure that names an attempt of zero, is a storage
// failure.
func TestASavedProblemsMembersAreChecked(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	s := serve(t, root, time.Hour)
	if err := s.srv.global(func() error { return s.srv.conversationSetup(conversation) }); err != nil {
		t.Fatal(err)
	}
	for name, problem := range map[string]string{
		"no detail":       `{"type":"urn:asz:pull:source-read","title":"Service Unavailable","status":503}`,
		"a null detail":   `{"type":"urn:asz:pull:source-read","title":"Service Unavailable","detail":null,"status":503}`,
		"an attempt of 0": `{"type":"urn:asz:pull:source-read","title":"Service Unavailable","detail":"x","status":503,"attempt":0}`,
		"an empty freeze": `{"type":"urn:asz:pull:source-read","title":"Service Unavailable","detail":"x","status":503,"freeze":""}`,
	} {
		id := fmt.Sprintf("%064x", len(name))
		record := `{"schema":"pull-state/1","freeze":"` + id + `","conversation":"` + conversation +
			`","attempt":1,"started":true,"state":"failed","problem":` + problem + `,"finished_at":"2026-10-01T00:00:00.000000000Z"}`
		if err := os.WriteFile(s.srv.p.freeze(id), []byte(record), 0o644); err != nil {
			t.Fatal(err)
		}
		a := s.do(http.MethodGet, "/pull/v1/conversations/"+conversation+"/manifest", nil, map[string]string{"Asz-Pull-Freeze": id})
		if a.StatusCode != http.StatusInternalServerError || !strings.Contains(string(a.Body), format.ProblemStorageState) {
			t.Errorf("%s: %d %s", name, a.StatusCode, a.Body)
		}
	}
}

// A saved list of names is checked name by name before a name is kept: one
// no manifest could name is refused.
func TestANamesListWithAnInvalidNameIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.names")
	names := []string{"../escape", "a/1"}
	if err := newRecord(path, &archiveNames{Schema: stateSchema, Snapshot: "t", Archive: format.ArchiveID(names), Files: names}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := readNames(path, "t", format.ArchiveID(names), 2); err == nil {
		t.Fatal("a name no manifest could name was read")
	}
}

// Whatever a conversation's _removing holds is deleted by a later pass,
// whatever its name: a tree renamed, and a file.
func TestAnyLeftoverInRemovingIsDeleted(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	s := serve(t, root, time.Hour)
	s.manifest(conversation)
	removing := s.srv.p.removing(conversation)
	if err := os.MkdirAll(filepath.Join(removing, "leftover", "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(removing, "stray"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.srv.sweep()
	entries, err := os.ReadDir(removing)
	if err != nil || len(entries) != 0 {
		t.Fatalf("left in _removing: %v %v", entries, err)
	}
}

// A builder that finishes builds what waits for a builder, with no request
// asking: an archive queued while both builders were busy is built once
// one comes free.
func TestAFinishedBuilderBuildsWhatWaits(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	files := &hookFS{}
	s := serveFS(t, root, time.Hour, files)
	m, resp := s.manifest(conversation)
	dir := s.srv.p.snapshot(conversation, resp.Snapshot)
	archives := "/pull/v1/conversations/" + conversation + "/snapshots/" + resp.Snapshot + "/archives"
	first, second := []string{m.Files[0].File}, []string{m.Files[1].File}
	// One builder is taken here; the other builds the first archive, held
	// as it installs its bytes, outside every lock, until the second
	// archive is queued.
	s.srv.archiveSlots <- struct{}{}
	defer func() { <-s.srv.archiveSlots }()
	queued := make(chan struct{})
	firstBytes := archivePath(dir, format.ArchiveID(first), ".tar.gz")
	files.setInstall(func(to string) {
		if to == firstBytes {
			<-queued
		}
	})
	if a := s.do(http.MethodPost, archives, map[string]any{"lease": resp.Lease, "files": first}, nil); a.StatusCode != http.StatusAccepted {
		t.Fatalf("the first: %d %s", a.StatusCode, a.Body)
	}
	if a := s.do(http.MethodPost, archives, map[string]any{"lease": resp.Lease, "files": second}, nil); a.StatusCode != http.StatusAccepted {
		t.Fatalf("the second: %d %s", a.StatusCode, a.Body)
	}
	close(queued)
	secondJob := archivePath(dir, format.ArchiveID(second), ".json")
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		var j *archiveJob
		err := s.srv.global(func() (err error) { j, err = readArchive(secondJob, resp.Snapshot, format.ArchiveID(second)); return })
		if err == nil && j.State == jobReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the second archive waits: %+v %v", j, err)
		}
	}
}

// supersededWithOneLease is a served snapshot with one open lease, then
// superseded by a later tag.
func supersededWithOneLease(t *testing.T, s *served, conversation string) (format.Response, string) {
	t.Helper()
	_, resp := s.manifest(conversation)
	later := laterTag(t, resp.Snapshot)
	if err := s.srv.global(func() error {
		st, err := readState(s.srv.p)
		if err != nil {
			return err
		}
		st.GreatestTag = later
		if err := replaceRecord(s.srv.p.state(), st, nil); err != nil {
			return err
		}
		return s.srv.supersede(conversation, later)
	}); err != nil {
		t.Fatal(err)
	}
	return resp, s.srv.p.snapshot(conversation, resp.Snapshot)
}

// A grant on a superseded snapshot whose leases are all released, as a
// release cut short by a busy replacement leaves it, expires the snapshot
// and is refused: no lease keeps a due snapshot.
func TestAGrantOnADueSnapshotIsRefused(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	s := serve(t, root, time.Hour)
	resp, dir := supersededWithOneLease(t, s, conversation)
	if err := s.srv.global(func() error {
		l, err := readLease(dir, resp.Snapshot, resp.Lease)
		if err != nil {
			return err
		}
		l.State = leaseReleased
		return replaceRecord(leasePath(dir, resp.Lease), l, nil)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.srv.grantLease(conversation, resp.Snapshot, s.caller()); !errors.Is(err, errGone) {
		t.Fatalf("a lease on a due snapshot: %v", err)
	}
	if use, _, err := readLastUse(dir, resp.Snapshot); err != nil || use.State != useExpired {
		t.Fatalf("the due snapshot: %+v %v", use, err)
	}
}

// The supersession a last release relies on is committed again before the
// snapshot expires: when that write fails, nothing expires.
func TestALastReleaseCommitsItsSupersessionFirst(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	files := &hookFS{}
	s := serveFS(t, root, time.Hour, files)
	resp, dir := supersededWithOneLease(t, s, conversation)
	failed := errors.New("the disk failed")
	files.setReplace(func(to string) error {
		if to == snapshotRecordPath(dir) {
			return failed
		}
		return nil
	})
	if err := s.srv.release(conversation, resp.Snapshot, resp.Lease, s.caller()); !errors.Is(err, failed) {
		t.Fatalf("a release whose supersession does not commit: %v", err)
	}
	if use, _, err := readLastUse(dir, resp.Snapshot); err != nil || use.State == useExpired {
		t.Fatalf("expired without its supersession committed: %+v %v", use, err)
	}
}

// A claim decides on the snapshot in the transition that saves it: a
// snapshot that expires while a busy replacement of the job lets the lock
// go starts no build.
func TestAClaimChecksTheSnapshotWhereItIsSaved(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	files := &hookFS{}
	s := serveFS(t, root, time.Hour, files)
	m, resp := s.manifest(conversation)
	dir := s.srv.p.snapshot(conversation, resp.Snapshot)
	names := []string{m.Files[0].File}
	id := format.ArchiveID(names)
	// Both builders are taken, so the POST queues its job unclaimed.
	s.srv.archiveSlots <- struct{}{}
	s.srv.archiveSlots <- struct{}{}
	a := s.do(http.MethodPost, "/pull/v1/conversations/"+conversation+"/snapshots/"+resp.Snapshot+"/archives",
		map[string]any{"lease": resp.Lease, "files": names}, nil)
	<-s.srv.archiveSlots
	<-s.srv.archiveSlots
	if a.StatusCode != http.StatusAccepted {
		t.Fatalf("the POST: %d %s", a.StatusCode, a.Body)
	}
	jobPath := archivePath(dir, id, ".json")
	var once sync.Once
	files.setReplace(func(to string) error {
		if to != jobPath {
			return nil
		}
		var busy error
		once.Do(func() {
			use, _, err := readLastUse(dir, resp.Snapshot)
			if err == nil {
				use.State = useExpired
				err = replaceRecord(lastUsePath(dir), use, nil)
			}
			if err != nil {
				t.Error(err)
			}
			busy = &storage.ControlBusyError{Err: errors.New("the job is busy")}
		})
		return busy
	})
	if err := s.srv.claimArchive(conversation, resp.Snapshot, id); err != nil {
		t.Fatal(err)
	}
	files.setReplace(nil)
	j, err := readArchive(jobPath, resp.Snapshot, id)
	if err != nil || j.Started {
		t.Fatalf("the job of an expired snapshot: %+v %v", j, err)
	}
}

// A range's file count is read once its request is admitted, and a list of
// names that does not read fails the range: a wrong lease is refused with
// nothing of the archive read, and a damaged list is never counted as none.
func TestARangesCountIsReadOnceAdmitted(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	s := serve(t, root, time.Hour)
	m, resp := s.manifest(conversation)
	names := []string{m.Files[0].File}
	st := s.archive(conversation, resp, names)
	dir := s.srv.p.snapshot(conversation, resp.Snapshot)
	list := archivePath(dir, format.ArchiveID(names), ".names")
	if err := os.Chmod(list, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(list, []byte("{broken"), 0o444); err != nil {
		t.Fatal(err)
	}
	data := "/pull/v1/conversations/" + conversation + "/snapshots/" + resp.Snapshot + "/archives/" + st.Archive + "/data"
	headers := func(lease string) map[string]string {
		return map[string]string{"Range": "bytes=0-0", "If-Match": `"` + st.Digest + `"`, "Asz-Pull-Lease": lease}
	}
	if a := s.do(http.MethodGet, data, nil, headers(strings.Repeat("0", 64))); a.StatusCode != http.StatusForbidden {
		t.Fatalf("another lease: %d %s", a.StatusCode, a.Body)
	}
	if a := s.do(http.MethodGet, data, nil, headers(resp.Lease)); a.StatusCode != http.StatusInternalServerError {
		t.Fatalf("a list that does not read: %d %s", a.StatusCode, a.Body)
	}
}

// The memory of counts holds at most countsKept of them.
func TestTheCountsKeptAreBounded(t *testing.T) {
	var c counts
	for i := range countsKept + 10 {
		c.put(fmt.Sprint(i), i)
	}
	if len(c.m) > countsKept {
		t.Fatalf("%d counts kept", len(c.m))
	}
	if n, ok := c.get(fmt.Sprint(countsKept + 9)); !ok || n != countsKept+9 {
		t.Fatalf("the newest count: %d %v", n, ok)
	}
}
