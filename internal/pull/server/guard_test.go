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
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
)

// pageRoutes are every route the page answers on: the page, a conversation,
// the three images, an asset, and every /api branch.
var pageRoutes = []string{"/", "/c/c1", "/favicon.svg", "/logo.svg", "/logo-blue.svg", view.AssetPrefix + "conversation-view.js",
	"/api/status", "/api/conversations", "/api/glossary", "/api/c/c1", "/api/c/c1/view", "/api/c/c1/record?seq=1&row=1"}

// guarded is a page over an empty root behind a guard with one token, and
// the token.
func guarded(t *testing.T, limit int, page http.Handler) (*httptest.Server, string, string) {
	t.Helper()
	token, hash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	path := tokensFile(t, "prod "+hash+"\n")
	k, err := LoadKeeper(path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if page == nil {
		page = view.New(storage.NewZone(t.TempDir()), nil).Handler()
	}
	s := httptest.NewUnstartedServer(NewGuard(k, limit, nil).Protect(page, nil))
	s.Config.DisableGeneralOptionsHandler = true
	s.Start()
	t.Cleanup(s.Close)
	return s, token, path
}

// answer is a response read whole and closed.
type answer struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

func get(t *testing.T, s *httptest.Server, method, path, token string) answer {
	t.Helper()
	req, err := http.NewRequest(method, s.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return answer{StatusCode: resp.StatusCode, Header: resp.Header, Body: body}
}

// Every route on a guarded address wants a token, and every answer, a
// refusal included, is kept out of caches: no-store replaces the public
// policy assets and images have.
func TestEveryRouteWantsAToken(t *testing.T) {
	s, token, path := guarded(t, 4, nil)
	changed := []byte(token)
	changed[len(changed)/2] ^= 1
	routes := append(pageRoutes, "/pull/v1/conversations")
	for _, route := range routes {
		for name, sent := range map[string]string{"no token": "", "an unknown token": "asz_nope", "a changed byte": string(changed)} {
			resp := get(t, s, http.MethodGet, route, sent)
			if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("Cache-Control") != "no-store" {
				t.Errorf("%s with %s: %d %q", route, name, resp.StatusCode, resp.Header.Get("Cache-Control"))
			}
		}
		resp := get(t, s, http.MethodGet, route, token)
		if resp.StatusCode == http.StatusUnauthorized || resp.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: %d %q", route, resp.StatusCode, resp.Header.Get("Cache-Control"))
		}
	}
	// Revoked: the file no longer names it, and the keeper reads it again.
	if err := os.WriteFile(path, []byte("# nobody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(reloadEvery)
	for _, route := range routes {
		if resp := get(t, s, http.MethodGet, route, token); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s after revocation: %d", route, resp.StatusCode)
		}
	}
}

// Without tokens the page is as it was: assets and images keep their public
// policy, and no pull route is served.
func TestWithoutTokensThePageIsAsItWas(t *testing.T) {
	s := httptest.NewServer(view.New(storage.NewZone(t.TempDir()), nil).Handler())
	defer s.Close()
	for _, route := range []string{"/favicon.svg", view.AssetPrefix + "conversation-view.js"} {
		resp := get(t, s, http.MethodGet, route, "")
		if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Cache-Control"), "public") {
			t.Errorf("%s: %d %q", route, resp.StatusCode, resp.Header.Get("Cache-Control"))
		}
	}
	if resp := get(t, s, http.MethodGet, "/pull/v1/conversations", ""); resp.StatusCode == http.StatusOK {
		t.Fatal("a pull route was served without tokens")
	}
}

func problemOf(t *testing.T, resp answer) Problem {
	t.Helper()
	var p Problem
	if resp.Header.Get("Content-Type") != "application/problem+json" {
		t.Fatalf("%d with %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if err := json.Unmarshal(resp.Body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Status != resp.StatusCode {
		t.Fatalf("a problem says %d on a %d", p.Status, resp.StatusCode)
	}
	return p
}

// A method is checked after the token and before anything else: the page
// takes GET alone, HEAD included, and pull routes are 404 problems when the
// address serves no pulls.
func TestMethodsAreCheckedAfterTheToken(t *testing.T) {
	s, token, _ := guarded(t, 4, nil)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions} {
		resp := get(t, s, method, "/api/status", token)
		if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != http.MethodGet {
			t.Errorf("%s: %d %q", method, resp.StatusCode, resp.Header.Get("Allow"))
		}
		problemOf(t, resp)
		if resp := get(t, s, method, "/api/status", ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without a token: %d", method, resp.StatusCode)
		}
	}
	resp := get(t, s, http.MethodHead, "/api/status", token)
	body := resp.Body
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != http.MethodGet ||
		resp.Header.Get("Content-Type") != "application/problem+json" || len(body) != 0 {
		t.Fatalf("HEAD: %d %v %q", resp.StatusCode, resp.Header, body)
	}
	resp = get(t, s, http.MethodGet, "/pull/v1/conversations", token)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a pull route without pulls: %d", resp.StatusCode)
	}
	problemOf(t, resp)
}

// rawRequest sends text on a fresh connection and reads the first status
// line and headers that come back.
func rawRequest(t *testing.T, s *httptest.Server, text string) (string, http.Header) {
	t.Helper()
	conn, err := net.Dial("tcp", s.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(conn, text); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.Status, resp.Header
}

// OPTIONS * reaches the guard, which authenticates it like any request, and
// a request that waits for 100 Continue without a token gets its 401 and no
// invitation to send its body.
func TestTheGuardAnswersBeforeAnyBody(t *testing.T) {
	s, token, _ := guarded(t, 4, nil)
	status, h := rawRequest(t, s, "OPTIONS * HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer "+token+"\r\n\r\n")
	if !strings.HasPrefix(status, "405") || h.Get("Allow") != "GET, POST" {
		t.Fatalf("OPTIONS *: %s %v", status, h)
	}
	if status, _ := rawRequest(t, s, "OPTIONS * HTTP/1.1\r\nHost: x\r\n\r\n"); !strings.HasPrefix(status, "401") {
		t.Fatalf("OPTIONS * without a token: %s", status)
	}
	status, h = rawRequest(t, s, "POST /pull/v1/x HTTP/1.1\r\nHost: x\r\nContent-Length: 5\r\nExpect: 100-continue\r\n\r\n")
	if !strings.HasPrefix(status, "401") || h.Get("Cache-Control") != "no-store" {
		t.Fatalf("an unauthenticated 100-continue: %s %v", status, h)
	}
}

// A token runs at most its limit of requests at once; one more is a 429
// problem, and a slot freed is taken again.
func TestATokenRunsAtMostItsLimit(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	page := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/block" {
			entered <- struct{}{}
			<-release
		}
		_, _ = io.WriteString(w, "ok")
	})
	s, token, _ := guarded(t, 1, page)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		req, _ := http.NewRequest(http.MethodGet, s.URL+"/block", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		if resp, err := s.Client().Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	<-entered
	resp := get(t, s, http.MethodGet, "/", token)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("a request past the limit: %d", resp.StatusCode)
	}
	problemOf(t, resp)
	close(release)
	wg.Wait()
	if resp := get(t, s, http.MethodGet, "/", token); resp.StatusCode != http.StatusOK {
		t.Fatalf("a freed slot: %d", resp.StatusCode)
	}
}

// A handler's own cache policy is replaced however its header is committed:
// by a flush, by a write that commits it, by an empty answer.
func TestNoStoreReplacesEveryCommit(t *testing.T) {
	page := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Header().Set("Expires", "Thu, 01 Jan 2099 00:00:00 GMT")
		switch r.URL.Path {
		case "/flush":
			w.(http.Flusher).Flush()
			_, _ = io.WriteString(w, "late")
		case "/write":
			_, _ = io.WriteString(w, "implicit")
		case "/controller":
			_ = http.NewResponseController(w).Flush()
		}
	})
	s, token, _ := guarded(t, 4, page)
	for _, route := range []string{"/flush", "/write", "/controller", "/empty"} {
		resp := get(t, s, http.MethodGet, route, token)
		if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Expires") != "" {
			t.Errorf("%s: %v", route, resp.Header)
		}
	}
}

// recorded counts what the bounded writer sends below it.
type recorded struct {
	httptest.ResponseRecorder
	writes  []int
	flushes int
	fail    bool
}

func (r *recorded) Write(p []byte) (int, error) {
	if r.fail {
		return 0, errors.New("the peer went away")
	}
	r.writes = append(r.writes, len(p))
	return r.ResponseRecorder.Write(p)
}

func (r *recorded) Flush() { r.flushes++ }

// Every application write goes down in pieces of at most 32 kibibytes, each
// flushed, and a write that failed fails every later one at once.
func TestTheBoundedWriterWritesInFlushedPieces(t *testing.T) {
	rec := &recorded{ResponseRecorder: *httptest.NewRecorder()}
	b := newBoundedWriter(rec, time.Now)
	if n, err := b.Write(make([]byte, 100<<10)); err != nil || n != 100<<10 {
		t.Fatal(n, err)
	}
	if len(rec.writes) != 4 || rec.writes[0] != writeChunk || rec.writes[3] != 100<<10-3*writeChunk || rec.flushes != 4 {
		t.Fatalf("writes %v, flushes %d", rec.writes, rec.flushes)
	}
	rec.fail = true
	if _, err := b.Write([]byte("x")); !errors.Is(err, errAborted) {
		t.Fatal(err)
	}
	rec.fail = false
	if _, err := b.Write([]byte("x")); !errors.Is(err, errAborted) || len(rec.writes) != 4 {
		t.Fatalf("a write after a failure: %v %v", err, rec.writes)
	}
}

// A reader that stops reading holds one write for its deadline, not the
// handler for ever: the write fails, and the handler sees it.
func TestAStalledReaderStopsTheWrite(t *testing.T) {
	failed := make(chan error, 1)
	page := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, err := w.Write(make([]byte, 64<<20))
		failed <- err
	})
	token, hash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	k, err := LoadKeeper(tokensFile(t, "prod "+hash+"\n"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The clock runs a little short of the write deadline behind, so each
	// deadline falls 300 milliseconds from now.
	behind := func() time.Time { return time.Now().Add(300*time.Millisecond - writeWait) }
	s := httptest.NewServer(NewGuard(k, 4, behind).Protect(page, nil))
	defer s.Close()
	conn, err := net.Dial("tcp", s.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer "+token+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-failed:
		if !errors.Is(err, errAborted) {
			t.Fatalf("the stalled write ended with %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a stalled reader held the handler")
	}
}
