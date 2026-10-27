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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/pull/format"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/pulltest"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario/run"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
)

// production is a production root holding one scenario's conversation, as
// Claude Code writes it and the collector lands it.
func production(t *testing.T, name string) (root, conversation string) {
	t.Helper()
	out := t.TempDir()
	rep, err := run.Check(filepath.Join("..", "..", "..", "tests", "scenarios", name+".yaml"),
		run.Options{Out: out, Formats: []scenario.Format{scenario.FormatClaudeCode}})
	if err != nil || rep.Failed {
		t.Fatalf("building %s: %v %v", name, err, rep.Lines)
	}
	root = filepath.Join(out, string(scenario.FormatClaudeCode))
	entries, err := os.ReadDir(filepath.Join(root, "_conversations"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("conversations: %v", err)
	}
	return root, entries[0].Name()
}

// testClock is a clock a test moves.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// served is a pull server over a root behind a guard, its address and two
// tokens it takes: token, which do sends, and other, of another name.
type served struct {
	t      *testing.T
	srv    *Server
	http   *httptest.Server
	token  string
	other  string
	clock  *testClock
	cancel context.CancelFunc
	ran    chan error
}

func serve(t *testing.T, root string, ttl time.Duration) *served {
	t.Helper()
	return serveFS(t, root, ttl, nil)
}

// serveFS is serve writing through files.
func serveFS(t *testing.T, root string, ttl time.Duration, files storage.FileSystem) *served {
	t.Helper()
	return serveOpts(t, root, ttl, func(o *Options) { o.Files = files })
}

// serveOpts is serve with options set by set before the server starts.
func serveOpts(t *testing.T, root string, ttl time.Duration, set func(*Options)) *served {
	t.Helper()
	token, hash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	other, otherHash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	clock := &testClock{t: time.Now().UTC()}
	k, err := LoadKeeper(tokensFile(t, "prod "+hash+"\ndev "+otherHash+"\n"), clock.now, nil)
	if err != nil {
		t.Fatal(err)
	}
	guard := NewGuard(k, 4, clock.now)
	o := Options{Root: root, TTL: ttl, Every: time.Hour, Guard: guard, Now: clock.now}
	set(&o)
	srv := New(o)
	ctx, cancel := context.WithCancel(context.Background())
	s := &served{t: t, srv: srv, token: token, other: other, clock: clock, cancel: cancel, ran: make(chan error, 1)}
	go func() { s.ran <- srv.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for !srv.owned.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the server never owned the root's pulls")
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.http = newHTTP(guard, srv, root)
	t.Cleanup(func() {
		s.http.Close()
		cancel()
		if err := <-s.ran; err != nil {
			t.Error(err)
		}
	})
	return s
}

// newHTTP serves the page and the pull routes of a root behind a guard.
func newHTTP(guard *Guard, srv *Server, root string) *httptest.Server {
	page := view.New(storage.NewZone(root), nil).Handler()
	return httptest.NewServer(guard.Protect(page, srv.Handler()))
}

// do sends one request with the token, unless headers name another, and
// reads the answer whole.
func (s *served) do(method, path string, body any, headers map[string]string) answer {
	s.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			s.t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.http.URL+path, r)
	if err != nil {
		s.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := s.http.Client().Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		s.t.Fatal(err)
	}
	return answer{StatusCode: resp.StatusCode, Header: resp.Header, Body: b}
}

// manifest asks for a conversation's manifest and polls its freeze until it
// is ready, and returns the answer.
func (s *served) manifest(conversation string) (*format.Manifest, format.Response) {
	s.t.Helper()
	path := "/pull/v1/conversations/" + conversation + "/manifest"
	a := s.do(http.MethodGet, path, nil, nil)
	if a.StatusCode != http.StatusAccepted {
		s.t.Fatalf("the first GET: %d %s", a.StatusCode, a.Body)
	}
	var pending format.ManifestPending
	if err := json.Unmarshal(a.Body, &pending); err != nil || pending.Conversation != conversation || !storage.Digest64(pending.Freeze) {
		s.t.Fatalf("pending %s: %v", a.Body, err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		a = s.do(http.MethodGet, path, nil, map[string]string{"Asz-Pull-Freeze": pending.Freeze})
		switch a.StatusCode {
		case http.StatusOK:
			m, resp, err := format.ReadManifestResponse(bytes.NewReader(a.Body), conversation)
			if err != nil {
				s.t.Fatal(err)
			}
			return m, resp
		case http.StatusAccepted:
		default:
			s.t.Fatalf("a poll: %d %s", a.StatusCode, a.Body)
		}
		if time.Now().After(deadline) {
			s.t.Fatal("the freeze never ended")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// archive asks for an archive of names and polls it until it is ready.
func (s *served) archive(conversation string, r format.Response, names []string) format.ArchiveState {
	s.t.Helper()
	base := "/pull/v1/conversations/" + conversation + "/snapshots/" + r.Snapshot + "/archives"
	a := s.do(http.MethodPost, base, map[string]any{"lease": r.Lease, "files": names}, nil)
	deadline := time.Now().Add(60 * time.Second)
	for {
		var st format.ArchiveState
		switch a.StatusCode {
		case http.StatusOK:
			if err := json.Unmarshal(a.Body, &st); err != nil || st.State != format.StateReady {
				s.t.Fatalf("ready %s: %v", a.Body, err)
			}
			return st
		case http.StatusAccepted:
			if err := json.Unmarshal(a.Body, &st); err != nil {
				s.t.Fatal(err)
			}
		default:
			s.t.Fatalf("an archive: %d %s", a.StatusCode, a.Body)
		}
		if time.Now().After(deadline) {
			s.t.Fatal("the archive never ended")
		}
		time.Sleep(20 * time.Millisecond)
		a = s.do(http.MethodGet, base+"/"+st.Archive, nil, map[string]string{"Asz-Pull-Lease": r.Lease})
	}
}

// download reads a ready archive in ranges of at most the range limit.
func (s *served) download(conversation string, r format.Response, st format.ArchiveState) []byte {
	s.t.Helper()
	path := "/pull/v1/conversations/" + conversation + "/snapshots/" + r.Snapshot + "/archives/" + st.Archive + "/data"
	var out []byte
	for uint64(len(out)) < st.Bytes {
		end := min(uint64(len(out))+format.RangeLimit, st.Bytes) - 1
		a := s.do(http.MethodGet, path, nil, map[string]string{"Asz-Pull-Lease": r.Lease,
			"Range": fmt.Sprintf("bytes=%d-%d", len(out), end), "If-Match": `"` + st.Digest + `"`})
		if a.StatusCode != http.StatusPartialContent || a.Header.Get("ETag") != `"`+st.Digest+`"` ||
			a.Header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", len(out), end, st.Bytes) {
			s.t.Fatalf("a range: %d %v %s", a.StatusCode, a.Header, a.Body)
		}
		out = append(out, a.Body...)
	}
	return out
}

// A plain client pulls a conversation whole: the listing names it, the
// manifest is what the root holds, the archive of every file extracts into
// a tree that accepts as whole evidence, and the lease is released, twice.
// A second manifest of the same head is the same snapshot.
func TestAPlainClientPullsAConversation(t *testing.T) {
	root, conversation := production(t, "provider-bodies")
	s := serve(t, root, time.Hour)
	a := s.do(http.MethodGet, "/pull/v1/conversations", nil, nil)
	var listing format.Listing
	if a.StatusCode != http.StatusOK || json.Unmarshal(a.Body, &listing) != nil || len(listing.Rows) != 1 || listing.Rows[0].ID != conversation {
		t.Fatalf("the listing: %d %s", a.StatusCode, a.Body)
	}
	m, resp := s.manifest(conversation)
	want, err := pulltest.Manifest(root, conversation)
	if err != nil {
		t.Fatal(err)
	}
	if m.Digest() != want.Digest() || !storage.Digest64(resp.Lease) {
		t.Fatalf("the manifest: %s, the root holds %s", m.Digest(), want.Digest())
	}
	var names []string
	entries := map[string]format.Entry{}
	for _, e := range m.Files {
		names = append(names, e.File)
		entries[e.File] = e
	}
	st := s.archive(conversation, resp, names)
	if st.Archive != format.ArchiveID(names) {
		t.Fatalf("archive %s for names hashing to %s", st.Archive, format.ArchiveID(names))
	}
	b := s.download(conversation, resp, st)
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != st.Digest || uint64(len(b)) != st.Bytes {
		t.Fatal("the archive's bytes are not its digest")
	}
	tree := t.TempDir()
	err = format.Extract(bytes.NewReader(b), entries, func(e format.Entry, r io.Reader) error {
		path := filepath.Join(tree, filepath.FromSlash(e.File))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		data, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		return os.WriteFile(path, data, 0o444)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := pulltest.SaveManifest(tree, m); err != nil {
		t.Fatal(err)
	}
	if _, err := format.Accept(tree, format.Building, format.Want{Conversation: conversation}); err != nil {
		t.Fatalf("the pulled tree: %v", err)
	}
	done := "/pull/v1/conversations/" + conversation + "/snapshots/" + resp.Snapshot + "/done"
	for range 2 {
		if a := s.do(http.MethodPost, done, map[string]string{"lease": resp.Lease}, nil); a.StatusCode != http.StatusNoContent || len(a.Body) != 0 {
			t.Fatalf("done: %d %s", a.StatusCode, a.Body)
		}
	}
	_, again := s.manifest(conversation)
	if again.Snapshot != resp.Snapshot || again.Lease == resp.Lease {
		t.Fatalf("the same head froze again: %s, then %s", resp.Snapshot, again.Snapshot)
	}
}

// Each route says what is wrong with a request, as the protocol names it.
func TestEveryPullRouteRefusesAsItMust(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	s := serve(t, root, time.Hour)
	_, resp := s.manifest(conversation)
	m, err := s.srv.manifestOf(conversation, resp.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{m.Files[0].File}
	st := s.archive(conversation, resp, names)
	snap := "/pull/v1/conversations/" + conversation + "/snapshots/" + resp.Snapshot
	data := snap + "/archives/" + st.Archive + "/data"
	other := strings.Repeat("0", 64)
	// A lease is its token's: another valid token holding it is refused.
	dev := map[string]string{"Authorization": "Bearer " + s.other}
	for name, tc := range map[string]struct {
		method, path string
		body         any
		headers      map[string]string
		status       int
	}{
		"an unknown conversation": {http.MethodGet, "/pull/v1/conversations/nope/manifest", nil, nil, http.StatusNotFound},
		"an unsafe conversation":  {http.MethodGet, "/pull/v1/conversations/con/manifest", nil, nil, http.StatusBadRequest},
		"a query on a manifest":   {http.MethodGet, "/pull/v1/conversations/" + conversation + "/manifest?x=1", nil, nil, http.StatusBadRequest},
		"an unknown freeze":       {http.MethodGet, "/pull/v1/conversations/" + conversation + "/manifest", nil, map[string]string{"Asz-Pull-Freeze": other}, http.StatusConflict},
		"a POST to a manifest":    {http.MethodPost, "/pull/v1/conversations/" + conversation + "/manifest", nil, nil, http.StatusMethodNotAllowed},
		"an unknown route":        {http.MethodGet, "/pull/v1/nope", nil, nil, http.StatusNotFound},
		"another lease":           {http.MethodPost, snap + "/archives", map[string]any{"lease": other, "files": names}, nil, http.StatusForbidden},
		"a name not held":         {http.MethodPost, snap + "/archives", map[string]any{"lease": resp.Lease, "files": []string{"a/b"}}, nil, http.StatusNotFound},
		"an unknown member":       {http.MethodPost, snap + "/archives", map[string]any{"lease": resp.Lease, "files": names, "x": 1}, nil, http.StatusBadRequest},
		"a restart of nothing":    {http.MethodPost, snap + "/archives", map[string]any{"lease": resp.Lease, "files": []string{m.Files[1].File}, "restart": 1}, nil, http.StatusBadRequest},
		"an unknown archive":      {http.MethodGet, snap + "/archives/" + other, nil, map[string]string{"Asz-Pull-Lease": resp.Lease}, http.StatusNotFound},
		"no lease header":         {http.MethodGet, snap + "/archives/" + st.Archive, nil, nil, http.StatusBadRequest},
		"a stale If-Match":        {http.MethodGet, data, nil, map[string]string{"Asz-Pull-Lease": resp.Lease, "Range": "bytes=0-0", "If-Match": `"` + other + `"`}, http.StatusPreconditionFailed},
		"a range past the end":    {http.MethodGet, data, nil, map[string]string{"Asz-Pull-Lease": resp.Lease, "Range": fmt.Sprintf("bytes=%d-%d", st.Bytes, st.Bytes), "If-Match": `"` + st.Digest + `"`}, http.StatusRequestedRangeNotSatisfiable},
		"two ranges":              {http.MethodGet, data, nil, map[string]string{"Asz-Pull-Lease": resp.Lease, "Range": "bytes=0-0,2-3", "If-Match": `"` + st.Digest + `"`}, http.StatusBadRequest},
		"a stale If-Match past the end": {http.MethodGet, data, nil, map[string]string{"Asz-Pull-Lease": resp.Lease,
			"Range": fmt.Sprintf("bytes=%d-%d", st.Bytes, st.Bytes), "If-Match": `"` + other + `"`}, http.StatusPreconditionFailed},
		"hide on the listing":      {http.MethodGet, "/pull/v1/conversations?hide=thinking", nil, nil, http.StatusBadRequest},
		"a weak If-Match":          {http.MethodGet, data, nil, map[string]string{"Asz-Pull-Lease": resp.Lease, "Range": "bytes=0-0", "If-Match": `W/"` + st.Digest + `"`}, http.StatusBadRequest},
		"a gone snapshot":          {http.MethodPost, "/pull/v1/conversations/" + conversation + "/snapshots/20200101T000000.000000000Z/done", map[string]string{"lease": resp.Lease}, nil, http.StatusGone},
		"another token's done":     {http.MethodPost, snap + "/done", map[string]string{"lease": other}, nil, http.StatusForbidden},
		"a lease of another token": {http.MethodPost, snap + "/archives", map[string]any{"lease": resp.Lease, "files": names}, dev, http.StatusForbidden},
		"a done of another token":  {http.MethodPost, snap + "/done", map[string]string{"lease": resp.Lease}, dev, http.StatusForbidden},
		"a state of another token": {http.MethodGet, snap + "/archives/" + st.Archive, nil,
			map[string]string{"Authorization": "Bearer " + s.other, "Asz-Pull-Lease": resp.Lease}, http.StatusForbidden},
		"a HEAD of a manifest":        {http.MethodHead, "/pull/v1/conversations/" + conversation + "/manifest", nil, nil, http.StatusMethodNotAllowed},
		"a GET of archives":           {http.MethodGet, snap + "/archives", nil, nil, http.StatusMethodNotAllowed},
		"a malformed tag":             {http.MethodPost, "/pull/v1/conversations/" + conversation + "/snapshots/x/done", map[string]string{"lease": resp.Lease}, nil, http.StatusBadRequest},
		"a repeated listing filter":   {http.MethodGet, "/pull/v1/conversations?limit=1&limit=2", nil, nil, http.StatusBadRequest},
		"a query that does not parse": {http.MethodGet, "/pull/v1/conversations?limit=%ZZ", nil, nil, http.StatusBadRequest},
	} {
		a := s.do(tc.method, tc.path, tc.body, tc.headers)
		if a.StatusCode != tc.status {
			t.Errorf("%s: %d, want %d: %s", name, a.StatusCode, tc.status, a.Body)
		}
		if tc.method != http.MethodHead && a.Header.Get("Content-Type") != "application/problem+json" {
			t.Errorf("%s: %q", name, a.Header.Get("Content-Type"))
		}
	}
}

// A snapshot unused for its time to live expires, for good: its routes
// answer 410, and the sweep removes it and records its tag removed.
func TestASnapshotExpiresAndIsRemoved(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	s := serve(t, root, time.Minute)
	_, resp := s.manifest(conversation)
	s.clock.add(2 * time.Minute)
	done := "/pull/v1/conversations/" + conversation + "/snapshots/" + resp.Snapshot + "/done"
	if a := s.do(http.MethodPost, done, map[string]string{"lease": resp.Lease}, nil); a.StatusCode != http.StatusGone {
		t.Fatalf("an expired snapshot: %d %s", a.StatusCode, a.Body)
	}
	s.srv.sweep()
	if there, _ := exists(s.srv.p.snapshot(conversation, resp.Snapshot)); there {
		t.Fatal("the expired snapshot was not removed")
	}
	h, err := readHistory(s.srv.p, conversation)
	if err != nil || !h.removed(resp.Snapshot) {
		t.Fatalf("the history: %+v %v", h, err)
	}
	_, again := s.manifest(conversation)
	if again.Snapshot <= resp.Snapshot {
		t.Fatalf("a later freeze reused tag %s after %s", again.Snapshot, resp.Snapshot)
	}
}

// One process serves a root's pulls at a time: another's pull routes answer
// 503 and say to try again in a second, until the first stops.
func TestOneProcessServesARootsPulls(t *testing.T) {
	root, conversation := production(t, "three-rounds")
	first := serve(t, root, time.Hour)
	token, hash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	k, err := LoadKeeper(tokensFile(t, "prod "+hash+"\n"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	guard := NewGuard(k, 4, nil)
	second := New(Options{Root: root, TTL: time.Hour, Every: time.Hour, Guard: guard})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = second.Run(ctx) }()
	h := httptest.NewServer(guard.Protect(http.NotFoundHandler(), second.Handler()))
	defer h.Close()
	req, _ := http.NewRequest(http.MethodGet, h.URL+"/pull/v1/conversations/"+conversation+"/manifest", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "1" {
		t.Fatalf("a waiting process: %d %v", resp.StatusCode, resp.Header)
	}
	first.cancel()
	<-first.ran
	first.ran <- nil
	deadline := time.Now().Add(10 * time.Second)
	for !second.owned.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the second process never took over")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
