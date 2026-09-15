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

package claudecodeprovider_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecodeprovider"
	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

const (
	sessionA = "11111111-2222-4333-8444-555555555555"
	sessionB = "66666666-7777-4888-8999-aaaaaaaaaaaa"
)

var base = time.Date(2026, 9, 15, 9, 0, 0, 0, time.UTC)

type fixture struct {
	t    *testing.T
	src  string
	zone *storage.Zone
	col  *claudecodeprovider.Collector
	now  time.Time
	n    int
}

func newFixture(t *testing.T) *fixture {
	dir := t.TempDir()
	f := &fixture{t: t, src: filepath.Join(dir, "bodies"), zone: storage.NewZone(filepath.Join(dir, "root")), now: base.Add(time.Hour)}
	if err := os.MkdirAll(f.src, 0o755); err != nil {
		t.Fatal(err)
	}
	f.col = claudecodeprovider.New(f.src, f.zone, 2<<20)
	f.col.Now = func() time.Time { return f.now }
	return f
}

// request writes a request body of session under a UUID name, naming prev as
// the previous request, with enough history to be worth cutting.
func (f *fixture) request(session, prev string, turns int) string {
	f.n++
	msgs := []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "<system-reminder>" + strings.Repeat(" instructions", 600) + "</system-reminder>"},
		map[string]any{"type": "text", "text": "start"},
	}}}
	for i := 0; i < turns; i++ {
		msgs = append(msgs, map[string]any{"role": "assistant", "content": fmt.Sprintf("answer %d", i)},
			map[string]any{"role": "user", "content": fmt.Sprintf("question %d %s", i, strings.Repeat("x", 400))})
	}
	header := "x-anthropic-billing-header: cc_version=2.1.260;"
	if prev != "" {
		header += " cc_prev_req=" + prev + ";"
	}
	header += " cc_prompt_id=p1;"
	user, _ := json.Marshal(map[string]string{"session_id": session})
	body, _ := json.Marshal(map[string]any{
		"model": "claude-opus-5", "messages": msgs,
		"system":   []any{map[string]any{"type": "text", "text": header}, map[string]any{"type": "text", "text": strings.Repeat("You are an agent. ", 400)}},
		"tools":    []any{map[string]any{"name": "Read", "description": strings.Repeat("reads a file. ", 300)}},
		"metadata": map[string]any{"user_id": string(user)},
	})
	name := fmt.Sprintf("00000000-0000-4000-8000-%012d.request.json", f.n)
	f.write(name, body)
	return name
}

func (f *fixture) response(requestID, messageID string) string {
	body, _ := json.Marshal(map[string]any{"id": messageID, "type": "message", "role": "assistant", "model": "claude-opus-5",
		"content": []any{map[string]any{"type": "text", "text": "ok"}}})
	name := requestID + ".response.json"
	f.write(name, body)
	return name
}

func (f *fixture) write(name string, body []byte) {
	f.t.Helper()
	path := filepath.Join(f.src, name)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		f.t.Fatal(err)
	}
	at := base.Add(time.Duration(f.n) * time.Second)
	if err := os.Chtimes(path, at, at); err != nil {
		f.t.Fatal(err)
	}
	f.n++
}

func all(string) claudecodeprovider.Verdict { return claudecodeprovider.Collect }

func (f *fixture) pass(filter claudecodeprovider.Filter) *claudecodeprovider.Stats {
	f.t.Helper()
	st, err := f.col.CollectAll(filter)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, e := range st.Errors {
		f.t.Errorf("pass error: %v", e)
	}
	return st
}

// landed rebuilds every body landed for a session, by file name.
func (f *fixture) landed(session string) map[string][]byte {
	f.t.Helper()
	files, err := storage.LandedFiles(f.zone, session)
	if err != nil {
		f.t.Fatal(err)
	}
	s := providerbody.NewSession()
	out := map[string][]byte{}
	for _, lf := range files {
		if !strings.HasPrefix(filepath.Base(lf.Path), "provider_body-") {
			continue
		}
		fh, err := os.Open(lf.Path)
		if err != nil {
			f.t.Fatal(err)
		}
		r, err := sessiondata.NewReader(fh)
		if err != nil {
			f.t.Fatal(err)
		}
		for {
			rec, err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				f.t.Fatal(err)
			}
			if err := s.Add(rec); err != nil {
				f.t.Fatalf("%s: %v", rec.ID, err)
			}
			m, _ := providerbody.ManifestOf(rec)
			b, err := s.Body(rec.ID)
			if err != nil {
				f.t.Fatal(err)
			}
			out[m.Src] = b
		}
		fh.Close()
	}
	return out
}

func TestBodiesLandInTheirSessionsAndRebuildExactly(t *testing.T) {
	f := newFixture(t)
	r1 := f.request(sessionA, "", 3)
	p1 := f.response("req_a1", "msg_a1")
	r2 := f.request(sessionA, "req_a1", 4)
	b1 := f.request(sessionB, "", 2)
	// A response no request names and no transcript holds, as the one that
	// names a session is.
	f.response("req_title", "msg_title")
	st := f.pass(all)
	if st.SourcesLanded != 4 || st.Waiting != 1 {
		t.Fatalf("landed %d, waiting %d; want 4 and the unclaimed response waiting", st.SourcesLanded, st.Waiting)
	}
	for session, names := range map[string][]string{sessionA: {r1, p1, r2}, sessionB: {b1}} {
		got := f.landed(session)
		if len(got) != len(names) {
			t.Fatalf("%s holds %d bodies, want %d", session, len(got), len(names))
		}
		for _, n := range names {
			raw, _ := os.ReadFile(filepath.Join(f.src, n))
			if !bytes.Equal(got[n], raw) {
				t.Fatalf("%s rebuilt to other bytes", n)
			}
		}
	}
	if !st.Complete() {
		t.Fatal("a pass that left only an unclaimed response was reported incomplete")
	}
	// Nothing new: the next pass reads nothing and lands nothing.
	if st := f.pass(all); st.SourcesLanded != 0 {
		t.Fatalf("a second pass landed %d", st.SourcesLanded)
	}
}

func TestAResponseNoLaterRequestNamesIsClaimedByItsTranscript(t *testing.T) {
	f := newFixture(t)
	f.request(sessionA, "", 2)
	last := f.response("req_last", "msg_last")
	if st := f.pass(all); st.Waiting != 1 {
		t.Fatalf("waiting %d, want 1 before the transcript holds the call", st.Waiting)
	}
	// The local adapter lands the transcript and indexes the call.
	ix := index.New(sessionA)
	hdr := &sessiondata.Header{Kind: sessiondata.KindTranscript, Session: sessionA, Stream: "main"}
	e, blocks := index.FromRecord(ix, hdr, &sessiondata.Record{ID: "u1", Call: "msg_last", From: sessiondata.FromAgent}, 1, 1)
	ix.Append(e, blocks...)
	if err := ix.Write(f.zone.IndexDir(sessionA)); err != nil {
		t.Fatal(err)
	}
	st := f.pass(all)
	if st.SourcesLanded != 1 || st.Waiting != 0 {
		t.Fatalf("landed %d, waiting %d; want the response claimed", st.SourcesLanded, st.Waiting)
	}
	if _, ok := f.landed(sessionA)[last]; !ok {
		t.Fatal("the response did not land in the session whose transcript holds its call")
	}
}

func TestAHalfWrittenFileWaitsAndIsCalledUnreadableOnlyLater(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(f.src, "00000000-0000-4000-8000-000000000099.request.json")
	if err := os.WriteFile(path, []byte(`{"model":"claude-opus-5","messages":[`), 0o644); err != nil {
		t.Fatal(err)
	}
	f.now = time.Now()
	if st := f.pass(all); st.Unreadable != 0 || st.SourcesLanded != 0 {
		t.Fatalf("a file written a moment ago was judged: %+v", st)
	}
	f.now = time.Now().Add(claudecodeprovider.Settle + time.Minute)
	if st := f.pass(all); st.Unreadable != 1 {
		t.Fatalf("unreadable %d after the settle time, want 1", st.Unreadable)
	}
}

func TestLostStateLandsNothingTwice(t *testing.T) {
	f := newFixture(t)
	f.request(sessionA, "", 2)
	f.response("req_a1", "msg_a1")
	f.request(sessionA, "req_a1", 3)
	f.pass(all)
	before, _ := storage.LandedFiles(f.zone, sessionA)
	// A crash after landing and before the table was saved, or a table
	// deleted by hand: it rebuilds from the landed records.
	if err := os.Remove(f.zone.ProviderStatePath()); err != nil {
		t.Fatal(err)
	}
	if st := f.pass(all); st.SourcesLanded != 0 || st.Conflicts != 0 {
		t.Fatalf("after the state was lost a pass landed %d with %d conflicts", st.SourcesLanded, st.Conflicts)
	}
	after, _ := storage.LandedFiles(f.zone, sessionA)
	if len(after) != len(before) {
		t.Fatalf("landed files went from %d to %d", len(before), len(after))
	}
}

func TestARemovedSessionLandsAgain(t *testing.T) {
	f := newFixture(t)
	r := f.request(sessionA, "", 2)
	f.pass(all)
	if err := os.RemoveAll(f.zone.SessionDir(sessionA)); err != nil {
		t.Fatal(err)
	}
	if st := f.pass(all); st.SourcesLanded != 1 {
		t.Fatalf("landed %d after the session was removed, want 1", st.SourcesLanded)
	}
	if _, ok := f.landed(sessionA)[r]; !ok {
		t.Fatal("the body is not back in the session")
	}
}

func TestTheFilterExcludesAndHolds(t *testing.T) {
	f := newFixture(t)
	f.request(sessionA, "", 2)
	f.request(sessionB, "", 2)
	filter := func(s string) claudecodeprovider.Verdict {
		if s == sessionA {
			return claudecodeprovider.Exclude
		}
		return claudecodeprovider.Wait
	}
	st := f.pass(filter)
	if st.SourcesLanded != 0 || st.Waiting != 1 {
		t.Fatalf("landed %d, waiting %d; want nothing landed and the unjudged session waiting", st.SourcesLanded, st.Waiting)
	}
	if st := f.pass(all); st.SourcesLanded != 2 {
		t.Fatalf("landed %d once both sessions are collected, want 2", st.SourcesLanded)
	}
}

func TestAFileChangedAfterLandingIsAConflict(t *testing.T) {
	f := newFixture(t)
	r := f.request(sessionA, "", 2)
	f.pass(all)
	path := filepath.Join(f.src, r)
	raw, _ := os.ReadFile(path)
	if err := os.WriteFile(path, bytes.Replace(raw, []byte("start"), []byte("other"), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	for pass := 1; pass <= 2; pass++ {
		// The conflict fails every pass it stays, not only the one that found it.
		st, err := f.col.CollectAll(all)
		if err != nil {
			t.Fatal(err)
		}
		if st.Conflicts != 1 || st.SourcesLanded != 0 || st.Complete() {
			t.Fatalf("pass %d over a changed body: %+v", pass, st)
		}
	}
	// Putting the bytes back ends it.
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if st := f.pass(all); st.Conflicts != 0 || !st.Complete() {
		t.Fatalf("the body put back still conflicts: %+v", st)
	}
}

func TestASessionIDThatIsNoSessionIDLandsNothing(t *testing.T) {
	f := newFixture(t)
	f.request("../outside", "", 2)
	if err := os.MkdirAll(filepath.Join(filepath.Dir(f.zone.Root()), "outside"), 0o755); err != nil {
		t.Fatal(err)
	}
	st := f.pass(all)
	if st.SourcesLanded != 0 || st.Waiting != 1 {
		t.Fatalf("a request naming ../outside: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(f.zone.Root()), "outside", "provider_body")); !os.IsNotExist(err) {
		t.Fatalf("something was written outside the root: %v", err)
	}
}

// A session whose earlier provider file is gone, lost or kept in another
// storage stage, still takes new bodies. They are cut only against what is
// left, so each rebuilds from the files that remain.
func TestASessionMissingAnEarlierFileStillTakesNewBodies(t *testing.T) {
	f := newFixture(t)
	f.request(sessionA, "", 3)
	f.pass(all)
	f.response("req_a1", "msg_a1")
	f.request(sessionA, "req_a1", 4)
	f.pass(all)
	files, _ := storage.LandedFiles(f.zone, sessionA)
	var provider []string
	for _, lf := range files {
		if strings.HasPrefix(filepath.Base(lf.Path), "provider_body-") {
			provider = append(provider, lf.Path)
		}
	}
	if len(provider) != 2 {
		t.Fatalf("%d provider files, want one per pass", len(provider))
	}
	if err := os.Chmod(filepath.Dir(provider[0]), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(provider[0]); err != nil {
		t.Fatal(err)
	}
	next := f.request(sessionA, "req_a2", 5)
	f.response("req_a2", "msg_a2")
	// The new request, and the response it names as its previous one.
	if st := f.pass(all); st.SourcesLanded != 2 {
		t.Fatalf("landed %d after an earlier file was lost, want the new request and its named response", st.SourcesLanded)
	}
	// Rebuilt from the files that remain, the new body is whole. The body
	// of the second file refers to the lost one and does not rebuild.
	s := providerbody.NewSession()
	var rebuilt []byte
	files, _ = storage.LandedFiles(f.zone, sessionA)
	for _, lf := range files {
		if !strings.HasPrefix(filepath.Base(lf.Path), "provider_body-") {
			continue
		}
		fh, err := os.Open(lf.Path)
		if err != nil {
			t.Fatal(err)
		}
		_, recs, err := sessiondata.All(fh)
		fh.Close()
		if err != nil {
			t.Fatal(err)
		}
		for _, rec := range recs {
			if s.Add(rec) != nil {
				continue
			}
			if m, _ := providerbody.ManifestOf(rec); m.Src == next {
				if rebuilt, err = s.Body(rec.ID); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	raw, _ := os.ReadFile(filepath.Join(f.src, next))
	if !bytes.Equal(rebuilt, raw) {
		t.Fatal("the new body does not rebuild from the files that remain")
	}
}

// A pass that stopped after landing and before saving the index, and whose
// table is gone too, still leaves the index covering what is landed.
func TestARebuiltTableClosesTheIndexGap(t *testing.T) {
	f := newFixture(t)
	f.request(sessionA, "", 2)
	f.pass(all)
	files, _ := storage.LandedFiles(f.zone, sessionA)
	landedTo := files[len(files)-1].Seq
	if err := os.RemoveAll(f.zone.IndexDir(sessionA)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.zone.ProviderStatePath()); err != nil {
		t.Fatal(err)
	}
	if st := f.pass(all); st.SourcesLanded != 0 {
		t.Fatalf("landed %d after the table was rebuilt", st.SourcesLanded)
	}
	ix, err := storage.LoadIndexState(f.zone.IndexStatePath(sessionA), sessionA)
	if err != nil || ix.IndexedSeq != landedTo {
		t.Fatalf("the index covers %d, and %d is landed: %v", ix.IndexedSeq, landedTo, err)
	}
}

// A file ends before a body that would take it past the budget.
func TestAFileEndsBeforeTheBudget(t *testing.T) {
	f := newFixture(t)
	f.col.MaxDelta = 12 << 10
	f.request(sessionA, "", 2)
	f.request(sessionB, "", 2)
	f.request(sessionA, "", 3)
	f.request(sessionA, "", 4)
	f.pass(all)
	files, _ := storage.LandedFiles(f.zone, sessionA)
	for _, lf := range files {
		info, err := os.Stat(lf.Path)
		if err != nil {
			t.Fatal(err)
		}
		fh, _ := os.Open(lf.Path)
		_, recs, _ := sessiondata.All(fh)
		fh.Close()
		if len(recs) > 1 && info.Size() > f.col.MaxDelta {
			t.Fatalf("%s holds %d bodies in %d bytes, past a %d byte budget", filepath.Base(lf.Path), len(recs), info.Size(), f.col.MaxDelta)
		}
	}
}

// A session that was busy when its index gap was to be closed has it closed
// by a later pass, with no new body of it arriving.
func TestABusySessionHasItsIndexGapClosedLater(t *testing.T) {
	f := newFixture(t)
	f.request(sessionA, "", 2)
	f.pass(all)
	files, _ := storage.LandedFiles(f.zone, sessionA)
	landedTo := files[len(files)-1].Seq
	if err := os.RemoveAll(f.zone.IndexDir(sessionA)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.zone.ProviderStatePath()); err != nil {
		t.Fatal(err)
	}
	lock, err := storage.LockSession(f.zone.SessionDir(sessionA))
	if err != nil {
		t.Fatal(err)
	}
	if st := f.pass(all); st.Busy == 0 {
		t.Fatal("the pass did not meet the held lock")
	}
	_ = lock.Unlock()
	f.pass(all)
	ix, err := storage.LoadIndexState(f.zone.IndexStatePath(sessionA), sessionA)
	if err != nil || ix.IndexedSeq != landedTo {
		t.Fatalf("the index covers %d, and %d is landed: %v", ix.IndexedSeq, landedTo, err)
	}
}

// One damaged provider file does not stop another session's bodies.
func TestADamagedFileDoesNotStopOtherSessions(t *testing.T) {
	f := newFixture(t)
	f.request(sessionA, "", 2)
	f.pass(all)
	files, _ := storage.LandedFiles(f.zone, sessionA)
	damaged := files[len(files)-1].Path
	if err := os.Chmod(damaged, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(damaged, 100); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.zone.ProviderStatePath()); err != nil {
		t.Fatal(err)
	}
	b := f.request(sessionB, "", 2)
	st, err := f.col.CollectAll(all)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Errors) == 0 {
		t.Fatal("the damaged file was not reported")
	}
	if _, ok := f.landed(sessionB)[b]; !ok {
		t.Fatal("a damaged file in one session kept another session's body from landing")
	}
}
