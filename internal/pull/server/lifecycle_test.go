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
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/pull/format"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// restart stops a server and serves its root again, as a new process with
// nothing in memory, with the same token.
func (s *served) restart(t *testing.T) *served {
	t.Helper()
	s.http.Close()
	s.cancel()
	if err := <-s.ran; err != nil {
		t.Fatal(err)
	}
	s.ran <- nil
	return serveWith(t, s.srv.o.Root, s.srv.o.TTL, s.token, s.clock)
}

// serveWith serves a root with a known token and clock.
func serveWith(t *testing.T, root string, ttl time.Duration, token string, clock *testClock) *served {
	t.Helper()
	hash := hashOf(token)
	k, err := LoadKeeper(tokensFile(t, "prod "+hash+"\n"), clock.now, nil)
	if err != nil {
		t.Fatal(err)
	}
	guard := NewGuard(k, 4, clock.now)
	srv := New(Options{Root: root, TTL: ttl, Every: time.Hour, Guard: guard, Now: clock.now,
		Log: func(format string, args ...any) { t.Logf(format, args...) }})
	ctx, cancel := context.WithCancel(context.Background())
	s := &served{t: t, srv: srv, token: token, clock: clock, cancel: cancel, ran: make(chan error, 1)}
	go func() { s.ran <- srv.Run(ctx) }()
	for deadline := time.Now().Add(10 * time.Second); !srv.owned.Load(); {
		if time.Now().After(deadline) {
			t.Fatal("the server never owned the root's pulls")
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.http = newHTTP(guard, srv, root)
	t.Cleanup(func() {
		s.http.Close()
		cancel()
		<-s.ran
	})
	return s
}

// A restarted owner holds nothing in memory: a lease, an archive and a
// release the previous one granted work as before, from disk alone.
func TestARestartedOwnerServesWhatItsPredecessorGranted(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	s := serve(t, root, time.Hour)
	m, resp := s.manifest(conversation)
	again := s.restart(t)
	names := []string{m.Files[0].File, m.Files[1].File}
	st := again.archive(conversation, resp, names)
	if b := again.download(conversation, resp, st); uint64(len(b)) != st.Bytes {
		t.Fatalf("%d bytes of %d", len(b), st.Bytes)
	}
	done := "/pull/v1/conversations/" + conversation + "/snapshots/" + resp.Snapshot + "/done"
	if a := again.do(http.MethodPost, done, map[string]string{"lease": resp.Lease}, nil); a.StatusCode != http.StatusNoContent {
		t.Fatalf("done: %d %s", a.StatusCode, a.Body)
	}
}

// A freeze whose worker died is taken again at its next attempt, once; at
// the recovery limit it fails with its typed problem and no worker, and the
// failure is what its polls see.
func TestAnAbandonedFreezeIsRecoveredThenFailed(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	s := serve(t, root, time.Hour)
	abandoned := func(attempt uint64) string {
		t.Helper()
		id, err := newID()
		if err != nil {
			t.Fatal(err)
		}
		if err := s.srv.global(func() error {
			if err := s.srv.conversationSetup(conversation); err != nil {
				return err
			}
			if err := storage.MakeOwnedDir(s.srv.p.build(id), nil); err != nil {
				return err
			}
			return replaceRecord(s.srv.p.freeze(id), &freezeJob{Schema: stateSchema, Freeze: id, Conversation: conversation,
				Attempt: attempt, Started: true, State: jobBuilding}, nil)
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	path := "/pull/v1/conversations/" + conversation + "/manifest"
	poll := func(id string) answer {
		return s.do(http.MethodGet, path, nil, map[string]string{"Asz-Pull-Freeze": id})
	}
	id := abandoned(1)
	for deadline := time.Now().Add(30 * time.Second); ; {
		a := poll(id)
		if a.StatusCode == http.StatusOK {
			break
		}
		if a.StatusCode != http.StatusAccepted || time.Now().After(deadline) {
			t.Fatalf("a recovered freeze: %d %s", a.StatusCode, a.Body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if j, err := readFreeze(s.srv.p, id); err != nil || j.Attempt != 2 || j.State != jobReady {
		t.Fatalf("the recovered job: %+v %v", j, err)
	}
	id = abandoned(2)
	a := poll(id)
	var p Problem
	if a.StatusCode != http.StatusInternalServerError || json.Unmarshal(a.Body, &p) != nil || p.Type != format.ProblemFreezeFailed ||
		p.Attempt != 3 || p.Freeze != id || p.Conversation != conversation {
		t.Fatalf("at the limit: %d %s", a.StatusCode, a.Body)
	}
	if again := poll(id); again.StatusCode != http.StatusInternalServerError {
		t.Fatalf("the failure was not kept: %d", again.StatusCode)
	}
}

// A failed archive keeps its problem until its numbered successor is asked
// for; that restart keeps the failure, builds the next attempt, and asked
// again answers with it; a restart the history does not allow is refused.
func TestAFailedArchiveRestartsByNumber(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	s := serve(t, root, time.Hour)
	m, resp := s.manifest(conversation)
	names := []string{m.Files[0].File}
	id := format.ArchiveID(names)
	dir := s.srv.p.snapshot(conversation, resp.Snapshot)
	if _, err := s.srv.installNames(dir, archiveRequest{conversation: conversation, tag: resp.Snapshot, id: id, names: names}); err != nil {
		t.Fatal(err)
	}
	failed := &archiveJob{Schema: stateSchema, Snapshot: resp.Snapshot, Archive: id, Attempt: 1, Started: true, State: jobFailed,
		Problem: &Problem{Type: format.ProblemArchiveFailed, Title: "Internal Server Error", Detail: "the disk filled", Status: 500,
			Snapshot: resp.Snapshot, Archive: id, Attempt: 1}}
	if err := replaceRecord(archivePath(dir, id, ".json"), failed, nil); err != nil {
		t.Fatal(err)
	}
	base := "/pull/v1/conversations/" + conversation + "/snapshots/" + resp.Snapshot + "/archives"
	a := s.do(http.MethodPost, base, map[string]any{"lease": resp.Lease, "files": names}, nil)
	if a.StatusCode != http.StatusInternalServerError || !strings.Contains(string(a.Body), format.ProblemArchiveFailed) {
		t.Fatalf("a failed archive: %d %s", a.StatusCode, a.Body)
	}
	if a := s.do(http.MethodPost, base, map[string]any{"lease": resp.Lease, "files": names, "restart": 5}, nil); a.StatusCode != http.StatusBadRequest {
		t.Fatalf("a future restart: %d %s", a.StatusCode, a.Body)
	}
	for range 2 {
		a := s.do(http.MethodPost, base, map[string]any{"lease": resp.Lease, "files": names, "restart": 1}, nil)
		if a.StatusCode != http.StatusAccepted && a.StatusCode != http.StatusOK {
			t.Fatalf("a restart: %d %s", a.StatusCode, a.Body)
		}
	}
	if _, err := readArchive(archivePath(dir, id, ".failed-1.json"), resp.Snapshot, id); err != nil {
		t.Fatalf("the failure was not kept: %v", err)
	}
	st := s.archive(conversation, resp, names)
	j, err := readArchive(archivePath(dir, id, ".json"), resp.Snapshot, id)
	if err != nil || j.Attempt != 2 || j.State != jobReady || st.Digest != j.Digest {
		t.Fatalf("the successor: %+v %v", j, err)
	}
}

// A publication an owner left unfinished is discarded whole by the next
// one, which serves meanwhile: its tag is refused until then, then recorded
// removed, its tree gone and its intent cleared.
func TestAnUnfinishedPublicationIsDiscarded(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	s := serve(t, root, time.Hour)
	_, resp := s.manifest(conversation)
	at, _ := storage.ParseTag(resp.Snapshot)
	tag := storage.Stamp(at.Add(time.Second))
	dir := s.srv.p.snapshot(conversation, tag)
	digest := strings.Repeat("ab", 32)
	if err := s.srv.global(func() error {
		st, err := readState(s.srv.p)
		if err != nil {
			return err
		}
		st.GreatestTag = tag
		st.Publishing = &publishing{Conversation: conversation, Build: strings.Repeat("cd", 32), Tag: tag, ManifestDigest: digest}
		if err := replaceRecord(s.srv.p.state(), st, nil); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(dir, "half"), 0o755); err != nil {
			return err
		}
		return newRecord(snapshotRecordPath(dir), &snapshotRecord{Schema: stateSchema, Conversation: conversation,
			Session: "s", Tag: tag, ManifestDigest: digest}, nil)
	}); err != nil {
		t.Fatal(err)
	}
	again := s.restart(t)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		st, err := readState(again.srv.p)
		if err == nil && st.Publishing == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the intent: %+v %v", st, err)
		}
	}
	h, err := readHistory(again.srv.p, conversation)
	if err != nil || !h.removed(tag) {
		t.Fatalf("the history: %+v %v", h, err)
	}
	if there, _ := exists(dir); there {
		t.Fatal("the unfinished tree is still there")
	}
}

// An expired snapshot answers 410 before its archive request's names are
// even looked up, and a new manifest freezes again.
func TestAnExpiredSnapshotIsGoneBeforeItsNamesAreRead(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	s := serve(t, root, time.Minute)
	_, resp := s.manifest(conversation)
	s.clock.add(2 * time.Minute)
	base := "/pull/v1/conversations/" + conversation + "/snapshots/" + resp.Snapshot + "/archives"
	if a := s.do(http.MethodPost, base, map[string]any{"lease": resp.Lease, "files": []string{"no/such"}}, nil); a.StatusCode != http.StatusGone {
		t.Fatalf("an expired snapshot: %d %s", a.StatusCode, a.Body)
	}
	_, again := s.manifest(conversation)
	if again.Snapshot == resp.Snapshot {
		t.Fatal("an expired snapshot was handed out again")
	}
}

// A use moves a snapshot's last use forward; an idle lease does not, and a
// snapshot unused for its time to live expires though its lease is open.
func TestUseRenewsAndIdleLeasesDoNot(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	s := serve(t, root, time.Minute)
	m, resp := s.manifest(conversation)
	names := []string{m.Files[0].File}
	s.clock.add(50 * time.Second)
	s.archive(conversation, resp, names)
	s.clock.add(50 * time.Second)
	poll := "/pull/v1/conversations/" + conversation + "/snapshots/" + resp.Snapshot + "/archives/" + format.ArchiveID(names)
	if a := s.do(http.MethodGet, poll, nil, map[string]string{"Asz-Pull-Lease": resp.Lease}); a.StatusCode != http.StatusOK {
		t.Fatalf("a used snapshot: %d %s", a.StatusCode, a.Body)
	}
	s.clock.add(61 * time.Second)
	if a := s.do(http.MethodGet, poll, nil, map[string]string{"Asz-Pull-Lease": resp.Lease}); a.StatusCode != http.StatusGone {
		t.Fatalf("an idle snapshot: %d %s", a.StatusCode, a.Body)
	}
}

// A later head publishes a new snapshot and supersedes the one before,
// which goes once its last lease is released; the newest stays after its
// own last release.
func TestASupersededSnapshotGoesWithItsLastLease(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	s := serve(t, root, time.Hour)
	_, first := s.manifest(conversation)
	// A second manifest of the same head reuses the first snapshot, with a
	// lease of its own; the snapshot is then marked superseded as a later
	// publication marks it.
	_, second := s.manifest(conversation)
	if second.Snapshot != first.Snapshot {
		t.Fatalf("the same head froze twice: %s %s", first.Snapshot, second.Snapshot)
	}
	at, _ := storage.ParseTag(first.Snapshot)
	later := storage.Stamp(at.Add(time.Second))
	if err := s.srv.global(func() error {
		dir := s.srv.p.snapshot(conversation, first.Snapshot)
		rec, err := readSnapshot(dir, conversation, first.Snapshot)
		if err != nil {
			return err
		}
		rec.SupersededBy = later
		return replaceRecord(snapshotRecordPath(dir), rec, nil)
	}); err != nil {
		t.Fatal(err)
	}
	done := "/pull/v1/conversations/" + conversation + "/snapshots/" + first.Snapshot + "/done"
	if a := s.do(http.MethodPost, done, map[string]string{"lease": first.Lease}, nil); a.StatusCode != http.StatusNoContent {
		t.Fatalf("done: %d %s", a.StatusCode, a.Body)
	}
	if there, _ := exists(s.srv.p.snapshot(conversation, first.Snapshot)); !there {
		t.Fatal("a superseded snapshot went while a lease held it")
	}
	if a := s.do(http.MethodPost, done, map[string]string{"lease": second.Lease}, nil); a.StatusCode != http.StatusNoContent {
		t.Fatalf("the last done: %d %s", a.StatusCode, a.Body)
	}
	if there, _ := exists(s.srv.p.snapshot(conversation, first.Snapshot)); there {
		t.Fatal("a superseded snapshot stayed after its last lease")
	}
}
