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
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
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
	e, blocks, body := index.FromRecord(ix, hdr, &sessiondata.Record{ID: "u1", Call: "msg_last", From: sessiondata.FromAgent}, 1, 1)
	ix.AppendRecord(e, blocks, body)
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

// A pass that wrote the index and stopped before saving its state leaves the
// index ahead of the state. The next pass builds the index again rather than
// adding those files a second time: each landed record is in it once, in
// landed order, which a round's watermark relies on.
func TestAnIndexAheadOfItsStateIsBuiltAgain(t *testing.T) {
	f := newFixture(t)
	f.request(sessionA, "", 2)
	f.pass(all)
	statePath := f.zone.IndexStatePath(sessionA)
	first, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	f.request(sessionA, "", 3)
	f.pass(all)
	// The crash: the index holds the second pass, its state the first.
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, first, 0o644); err != nil {
		t.Fatal(err)
	}
	f.request(sessionA, "", 4)
	f.pass(all)

	ix, ok, err := index.Load(f.zone.IndexDir(sessionA), sessionA)
	if err != nil || !ok {
		t.Fatalf("index: ok=%v %v", ok, err)
	}
	seen := map[[2]uint32]bool{}
	var last uint32
	for _, e := range ix.Entries {
		if e.Seq < last {
			t.Fatalf("entry at sequence %d follows one at %d", e.Seq, last)
		}
		last = e.Seq
		if seen[[2]uint32{e.Seq, e.Row}] {
			t.Fatalf("sequence %d row %d is indexed twice", e.Seq, e.Row)
		}
		seen[[2]uint32{e.Seq, e.Row}] = true
	}
	if len(seen) != 3 {
		t.Fatalf("%d records indexed, want the 3 bodies landed", len(seen))
	}
}

// retireAll marks a session retired and takes its directory away, as a
// finished retirement leaves it.
func (f *fixture) retireAll(session string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Join(f.zone.Root(), storage.RetiredDir), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(f.zone.RetiredPath(session), nil, 0o444); err != nil {
		f.t.Fatal(err)
	}
	if err := storage.DeleteTree(f.zone.SessionDir(session), nil); err != nil {
		f.t.Fatal(err)
	}
}

// TestARetiredSessionsBodiesAreNotReadAgain. Claude Code never deletes a
// body file, so a retired session's bodies stay in the source directory. A
// pass marks their lines retired without reading them, every pass after it
// passes them over the same way, and a new request naming the session is
// retired too. None of them lands, and the session's directory is not made
// again.
func TestARetiredSessionsBodiesAreNotReadAgain(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("a file that cannot be read needs a user that is not root, and not Windows")
	}
	f := newFixture(t)
	old := f.request(sessionA, "", 2)
	f.pass(all)
	f.retireAll(sessionA)
	// Not to be read: a pass that read it would fail.
	if err := os.Chmod(filepath.Join(f.src, old), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(f.src, old), 0o644) })
	for pass := 0; pass < 2; pass++ {
		st := f.pass(all)
		if st.SourcesLanded != 0 || len(st.Retired) != 1 || st.Retired[0] != sessionA {
			t.Fatalf("pass %d: landed %d, retired %v; want nothing landed and %s retired", pass, st.SourcesLanded, st.Retired, sessionA)
		}
	}
	added := f.request(sessionA, "", 3)
	if st := f.pass(all); st.SourcesLanded != 0 {
		t.Fatalf("a new request of a retired session landed %d", st.SourcesLanded)
	}
	if _, err := os.Stat(f.zone.SessionDir(sessionA)); !os.IsNotExist(err) {
		t.Fatalf("the retired session's directory was made again: %v", err)
	}
	seen, err := os.ReadFile(f.zone.ProviderStatePath())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{old, added} {
		if !strings.Contains(string(seen), name+"\t") || !strings.Contains(lineOf(string(seen), name), "\tretired\t") {
			t.Errorf("%s: its line is %q, want it retired", name, lineOf(string(seen), name))
		}
	}
}

func lineOf(table, name string) string {
	for _, line := range strings.Split(table, "\n") {
		if strings.HasPrefix(line, name+"\t") {
			return line
		}
	}
	return ""
}

// TestWhatIsLeftOfARetiredSessionIsNotWritten. A retirement that has not
// finished leaves the session's directory, here with no index, which a pass
// would build again for a session it lands into. It is not built. A response
// that comes after the retirement, for a request the retired session's
// request names, is known to be retired by that request, and does not wait.
func TestWhatIsLeftOfARetiredSessionIsNotWritten(t *testing.T) {
	f := newFixture(t)
	f.request(sessionA, "req_9", 2)
	f.pass(all)
	if err := os.MkdirAll(filepath.Join(f.zone.Root(), storage.RetiredDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.zone.RetiredPath(sessionA), nil, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := storage.DeleteTree(f.zone.IndexDir(sessionA), nil); err != nil {
		t.Fatal(err)
	}
	f.pass(all)
	if _, err := os.Stat(f.zone.IndexDir(sessionA)); !os.IsNotExist(err) {
		t.Fatalf("the retired session's index was built again: %v", err)
	}
	resp := f.response("req_9", "msg_9")
	if st := f.pass(all); st.SourcesLanded != 0 || st.Waiting != 0 {
		t.Fatalf("the response landed %d and waits %d; want it retired", st.SourcesLanded, st.Waiting)
	}
	seen, err := os.ReadFile(f.zone.ProviderStatePath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lineOf(string(seen), resp), "\tretired\t") {
		t.Errorf("the response's line is %q, want it retired", lineOf(string(seen), resp))
	}
}

// TestAMarkerThatCannotBeReadChangesNothing. While a session's marker cannot
// be read, the session is not written, and nothing is recorded of it: its
// landed body keeps its line, and a new body waits unrecorded. Once the
// marker reads again, the new body lands, and the landed one still reports a
// change to its bytes, as a live session's body does.
func TestAMarkerThatCannotBeReadChangesNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a file where a directory should be reads as not there on Windows")
	}
	f := newFixture(t)
	old := f.request(sessionA, "", 2)
	f.pass(all)
	before, err := os.ReadFile(f.zone.ProviderStatePath())
	if err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(f.zone.Root(), storage.RetiredDir)
	if err := os.WriteFile(blocked, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	added := f.request(sessionA, "", 3)
	st, err := f.col.CollectAll(all)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Errors) == 0 || st.SourcesLanded != 0 {
		t.Fatalf("errors %v, landed %d: a marker that cannot be read must be an error, and nothing land", st.Errors, st.SourcesLanded)
	}
	after, err := os.ReadFile(f.zone.ProviderStatePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("the table changed while the marker could not be read:\nbefore %s\nafter  %s", before, after)
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if st := f.pass(all); st.SourcesLanded != 1 {
		t.Fatalf("landed %d once the marker read again, want the new body", st.SourcesLanded)
	}
	if _, ok := f.landed(sessionA)[added]; !ok {
		t.Fatal("the new body did not land")
	}
	f.write(old, []byte(`{"changed":true}`))
	st, err = f.col.CollectAll(all)
	if err != nil {
		t.Fatal(err)
	}
	if st.Conflicts == 0 {
		t.Fatal("a landed body changed after the marker could not be read is no longer reported")
	}
}

// A pull's copy and a server's snapshot hold landed provider bodies with a
// local session's names and headers, and here one damaged file each. A rebuilt
// table passes over them: it reads none of their files, so it reports none of
// their damage, and with the local session gone its body lands again from its
// source.
func TestARebuiltTablePassesOverPullTrees(t *testing.T) {
	f := newFixture(t)
	f.request(sessionA, "", 2)
	f.pass(all)
	session := f.zone.SessionDir(sessionA)
	for _, dir := range []string{storage.RemoteDir, storage.PullDir} {
		if err := filepath.WalkDir(session, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(session, path)
			if err != nil {
				return err
			}
			dest := filepath.Join(f.zone.Root(), dir, rel)
			if d.IsDir() {
				return os.MkdirAll(dest, 0o755)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(dest, data, 0o444)
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{storage.RemoteDir, storage.PullDir} {
		damaged := filepath.Join(f.zone.Root(), dir, "provider_body", storage.LandedName("provider_body", storage.Stamp(base), 99))
		if err := os.WriteFile(damaged, []byte("not session data"), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	if err := storage.DeleteTree(session, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.zone.ProviderStatePath()); err != nil {
		t.Fatal(err)
	}
	st := f.pass(all)
	if len(st.Errors) != 0 {
		t.Fatalf("a rebuilt table read a pull tree: %v", st.Errors)
	}
	if st.SourcesLanded == 0 {
		t.Fatal("a rebuilt table took a pull's copy for the root's own landed body")
	}
}
