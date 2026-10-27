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

package client_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/pull/client"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/format"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/server"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario/run"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// hook sees every request before the server does, and answers it itself
// when it returns true.
type hook func(w http.ResponseWriter, r *http.Request, next http.Handler) bool

// remote is a pull server over its own copy of a scenario's root, behind
// the page's guard with one token, with a hook in front of it.
type remote struct {
	t                  *testing.T
	root, conversation string
	token              string
	http               *httptest.Server
	guard              *server.Guard
	mu                 sync.Mutex
	hook               hook
	seen               []string
	// inner is the page and the pull routes of the owner running now.
	inner  http.Handler
	cancel context.CancelFunc
	ran    chan error
}

// start runs an owner of the root's pulls, as a process that starts does.
func (rm *remote) start() {
	srv := server.New(server.Options{Root: rm.root, TTL: time.Hour, Every: time.Hour, Guard: rm.guard})
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan error, 1)
	go func() { ran <- srv.Run(ctx) }()
	rm.mu.Lock()
	defer rm.mu.Unlock()
	rm.inner = rm.guard.Protect(view.New(storage.NewZone(rm.root), nil).Handler(), srv.Handler())
	rm.cancel, rm.ran = cancel, ran
}

// stop stops the owner running now, as a process that ends does.
func (rm *remote) stop() {
	rm.mu.Lock()
	cancel, ran := rm.cancel, rm.ran
	rm.mu.Unlock()
	cancel()
	if err := <-ran; err != nil {
		rm.t.Error(err)
	}
}

// serveRemote builds a scenario's root and serves its pulls, over TLS when
// tlsOn is set. before runs on the root before the server starts.
func serveRemote(t *testing.T, name string, tlsOn bool, before func(root, conversation string)) *remote {
	t.Helper()
	out := t.TempDir()
	rep, err := run.Check(filepath.Join("..", "..", "..", "tests", "scenarios", name+".yaml"),
		run.Options{Out: out, Formats: []scenario.Format{scenario.FormatClaudeCode}})
	if err != nil || rep.Failed {
		t.Fatalf("building %s: %v %v", name, err, rep.Lines)
	}
	root := filepath.Join(out, string(scenario.FormatClaudeCode))
	entries, err := os.ReadDir(filepath.Join(root, "_conversations"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("conversations: %v", err)
	}
	rm := &remote{t: t, root: root, conversation: entries[0].Name()}
	if before != nil {
		before(root, rm.conversation)
	}
	token, hash, err := server.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	rm.token = token
	tokens := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(tokens, []byte(server.TokenLine("prod", hash)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	k, err := server.LoadKeeper(tokens, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rm.guard = server.NewGuard(k, 4, nil)
	rm.start()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rm.mu.Lock()
		rm.seen = append(rm.seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("Range")+" freeze="+r.Header.Get("Asz-Pull-Freeze"))
		h, inner := rm.hook, rm.inner
		rm.mu.Unlock()
		if h != nil && h(w, r, inner) {
			return
		}
		inner.ServeHTTP(w, r)
	})
	if tlsOn {
		rm.http = httptest.NewUnstartedServer(handler)
		rm.http.EnableHTTP2 = true
		rm.http.StartTLS()
	} else {
		rm.http = httptest.NewServer(handler)
	}
	t.Cleanup(func() {
		rm.http.Close()
		rm.stop()
	})
	// The server answers pulls once it owns the root's.
	deadline := time.Now().Add(10 * time.Second)
	for {
		req, _ := http.NewRequest(http.MethodGet, rm.http.URL+"/pull/v1/conversations", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := rm.http.Client().Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the server never served its pulls")
		}
		time.Sleep(20 * time.Millisecond)
	}
	rm.mu.Lock()
	rm.seen = nil
	rm.mu.Unlock()
	return rm
}

func (rm *remote) setHook(h hook) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	rm.hook = h
}

// requests are the requests the server was sent that contain text.
func (rm *remote) requests(text string) []string {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	var out []string
	for _, s := range rm.seen {
		if strings.Contains(s, text) {
			out = append(out, s)
		}
	}
	return out
}

// freezes counts the manifests asked for: requests with no freeze to poll.
func (rm *remote) freezes() int {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	n := 0
	for _, s := range rm.seen {
		if strings.HasSuffix(s, "/manifest  freeze=") {
			n++
		}
	}
	return n
}

// client is a client of the remote, with a token and trust of its own.
func (rm *remote) client(token string, o client.RemoteOptions) *client.Remote {
	rm.t.Helper()
	base, err := url.Parse(rm.http.URL)
	if err != nil {
		rm.t.Fatal(err)
	}
	o.Name, o.Base, o.Token = "prod", base, token
	if o.CA == nil && rm.http.TLS != nil {
		o.CA = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rm.http.Certificate().Raw})
	}
	r, err := client.NewRemote(o)
	if err != nil {
		rm.t.Fatal(err)
	}
	return r
}

// pullOver pulls the remote's conversation into local as the command does:
// a manifest, a build from its source, and the lease's release.
func pullOver(t *testing.T, r *client.Remote, local *client.Local, conversation string) (*client.Result, error) {
	t.Helper()
	ctx := context.Background()
	tries := &client.Tries{}
	m, resp, err := r.Manifest(ctx, conversation, tries)
	if err != nil {
		return nil, err
	}
	src := r.Source(conversation, m, resp, tries)
	res, err := local.Pull(ctx, "prod", conversation, m, src)
	if derr := r.Done(ctx, conversation, src.Response()); derr != nil {
		t.Errorf("done: %v", derr)
	}
	if res != nil && res.Admission != nil {
		if rerr := res.Admission.Release(ctx); rerr != nil {
			t.Error(rerr)
		}
	}
	return res, err
}

func newLocal(t *testing.T) *client.Local {
	return &client.Local{Root: t.TempDir(), TTL: time.Hour}
}

// A pull over HTTP lists the conversation, makes its snapshot from one
// archive, releases its lease, and a second pull of the same head is
// unchanged; the listing's local column reads that snapshot's head.
func TestAPullOverHTTPMakesItsSnapshotOnce(t *testing.T) {
	rm := serveRemote(t, "provider-bodies", false, nil)
	r := rm.client(rm.token, client.RemoteOptions{})
	l, err := r.List(context.Background(), nil)
	if err != nil || len(l.Rows) != 1 || l.Rows[0].ID != rm.conversation {
		t.Fatalf("%+v %v", l, err)
	}
	local := newLocal(t)
	res, err := pullOver(t, r, local, rm.conversation)
	if err != nil || res.Unchanged || res.Fetched == 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if n := len(rm.requests("POST /pull/v1/conversations/" + rm.conversation + "/snapshots/")); n != 2 {
		t.Fatalf("an archive and a release: %d requests", n)
	}
	again, err := pullOver(t, r, local, rm.conversation)
	if err != nil || !again.Unchanged || again.Tag != res.Tag {
		t.Fatalf("%+v %v", again, err)
	}
	heads := local.NewLocalHeads()
	defer heads.Release(context.Background())
	if round, ok, err := heads.Head(context.Background(), "prod", rm.conversation); err != nil || !ok || round != l.Rows[0].Round {
		t.Fatalf("the local column: %d %v %v", round, ok, err)
	}
}

// cutFirst answers the first request matching match with only half the
// bytes the server sent, under the server's headers, so the client sees a
// body cut short.
func cutFirst(match string) hook {
	var done atomic.Bool
	return func(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
		if !strings.Contains(r.URL.Path, match) || done.Swap(true) {
			return false
		}
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		body := rec.Body.Bytes()
		_, _ = w.Write(body[:len(body)/2])
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
			}
		}
		return true
	}
}

// A range cut short is resumed at its next byte, and the archive is whole.
func TestACutRangeResumesWhereItStopped(t *testing.T) {
	rm := serveRemote(t, "provider-bodies", false, nil)
	rm.setHook(cutFirst("/data"))
	res, err := pullOver(t, rm.client(rm.token, client.RemoteOptions{}), newLocal(t), rm.conversation)
	if err != nil || res.Fetched == 0 {
		t.Fatalf("%+v %v", res, err)
	}
	// The second range starts where the cut left off and ends where the
	// first was to end.
	ranges := rm.requests("/data")
	if len(ranges) != 2 {
		t.Fatalf("the ranges: %v", ranges)
	}
	var start1, end1, start2, end2 uint64
	if _, err := fmt.Sscanf(strings.Fields(ranges[0])[2], "bytes=%d-%d", &start1, &end1); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Sscanf(strings.Fields(ranges[1])[2], "bytes=%d-%d", &start2, &end2); err != nil {
		t.Fatal(err)
	}
	if start1 != 0 || start2 != (end1+1)/2 || end2 != end1 {
		t.Fatalf("the ranges: %v", ranges)
	}
}

// A 412 asks for the archive's descriptor again; the same one carries on.
func TestAStaleIfMatchAsksForTheDescriptorAgain(t *testing.T) {
	rm := serveRemote(t, "provider-bodies", false, nil)
	var done atomic.Bool
	rm.setHook(func(_ http.ResponseWriter, r *http.Request, _ http.Handler) bool {
		if strings.HasSuffix(r.URL.Path, "/data") && !done.Swap(true) {
			r.Header.Set("If-Match", `"`+strings.Repeat("0", 64)+`"`)
		}
		return false
	})
	res, err := pullOver(t, rm.client(rm.token, client.RemoteOptions{}), newLocal(t), rm.conversation)
	if err != nil || res.Fetched == 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if states := rm.requests("GET /pull/v1/conversations/" + rm.conversation + "/snapshots/"); len(states) < 3 {
		t.Fatalf("no descriptor was asked for between the ranges: %v", states)
	}
}

// goneFirst answers the first archive request 410, as when the snapshot
// expired, after running then.
func goneFirst(then func()) hook {
	var done atomic.Bool
	return func(w http.ResponseWriter, r *http.Request, _ http.Handler) bool {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/archives") || done.Swap(true) {
			return false
		}
		if then != nil {
			then()
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusGone)
		fmt.Fprint(w, `{"type":"about:blank","title":"Gone","detail":"the snapshot expired","status":410}`)
		return true
	}
}

// A snapshot that goes away while its files are fetched is fetched again
// under a fresh manifest, when its files are the same, and the fresh lease
// is the one released.
func TestASnapshotThatWentIsFetchedUnderAFreshManifest(t *testing.T) {
	rm := serveRemote(t, "provider-bodies", false, nil)
	rm.setHook(goneFirst(nil))
	res, err := pullOver(t, rm.client(rm.token, client.RemoteOptions{}), newLocal(t), rm.conversation)
	if err != nil || res.Fetched == 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if n := rm.freezes(); n != 2 {
		t.Fatalf("%d manifests were asked for: %v", n, rm.requests(""))
	}
	if n := len(rm.requests("/done")); n != 1 {
		t.Fatalf("%d releases", n)
	}
}

// lastRoundHidden hides a three-round chain's last round before its server
// starts, and restore puts it back: the head moves from round 2 to round 3.
func lastRoundHidden(t *testing.T) (hide func(root, conversation string), restore func()) {
	var hidden, last string
	hide = func(root, conversation string) {
		files, err := sessionflow.OpenChain(root, conversation).List()
		if err != nil || len(files) != 3 {
			t.Fatalf("%d rounds: %v", len(files), err)
		}
		last = files[2].Path
		hidden = filepath.Join(t.TempDir(), filepath.Base(last))
		if err := os.Rename(last, hidden); err != nil {
			t.Fatal(err)
		}
	}
	restore = func() {
		if err := os.Rename(hidden, last); err != nil {
			t.Error(err)
		}
	}
	return hide, restore
}

// A fresh manifest whose files differ discards the build, and the pull makes
// it again from that manifest, under its lease, publishing the new head
// alone.
func TestAChangedListIsBuiltAgainInOnePull(t *testing.T) {
	hide, restore := lastRoundHidden(t)
	rm := serveRemote(t, "three-rounds", false, hide)
	rm.setHook(goneFirst(restore))
	r := rm.client(rm.token, client.RemoteOptions{})
	local := newLocal(t)
	ctx := context.Background()
	tries := &client.Tries{}
	m, resp, err := r.Manifest(ctx, rm.conversation, tries)
	if err != nil || m.Head.Round != 2 {
		t.Fatalf("round %v: %v", m, err)
	}
	// One pull, its conversation held throughout: the build of round 2 is
	// discarded when the files change, and round 3 is built in its place.
	src := r.Source(rm.conversation, m, resp, tries)
	res, err := local.Pull(ctx, "prod", rm.conversation, m, src)
	if err != nil {
		t.Fatalf("%v %v", err, rm.requests(""))
	}
	defer func() {
		if err := res.Admission.Release(ctx); err != nil {
			t.Error(err)
		}
	}()
	if res.Admission.Marker.Round != 3 || rm.freezes() != 2 {
		t.Fatalf("round %d after %d freezes", res.Admission.Marker.Round, rm.freezes())
	}
	dir, err := storage.ConversationDir(local.Root, "prod", rm.conversation)
	if err != nil {
		t.Fatal(err)
	}
	published := 0
	for _, e := range entries(t, dir) {
		if _, err := storage.ParseTag(e); err == nil {
			published++
		}
	}
	if published != 1 {
		t.Fatalf("%d snapshots published", published)
	}
	if err := r.Done(ctx, rm.conversation, src.Response()); err != nil {
		t.Fatal(err)
	}
}

// failsFirstRemoval fails the first removal inside _building, as a disk
// that fails does.
type failsFirstRemoval struct {
	storage.DiskFileSystem
	once sync.Once
}

var errDiskFailed = errors.New("the disk failed")

func (f *failsFirstRemoval) Remove(path string) error {
	failed := false
	if strings.Contains(path, string(filepath.Separator)+storage.BuildingDir+string(filepath.Separator)) {
		f.once.Do(func() { failed = true })
	}
	if failed {
		return errDiskFailed
	}
	return f.DiskFileSystem.Remove(path)
}

// A changed list that comes with a local failure, the discarded build's
// cleanup failing, ends the pull with that failure: nothing is built again
// over it, and nothing is published.
func TestAChangedListWithALocalFailureStops(t *testing.T) {
	hide, restore := lastRoundHidden(t)
	rm := serveRemote(t, "three-rounds", false, hide)
	rm.setHook(goneFirst(restore))
	local := newLocal(t)
	local.Files = &failsFirstRemoval{}
	if _, err := pullOver(t, rm.client(rm.token, client.RemoteOptions{}), local, rm.conversation); !errors.Is(err, errDiskFailed) {
		t.Fatalf("%v", err)
	}
	dir, err := storage.ConversationDir(local.Root, "prod", rm.conversation)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries(t, dir) {
		if _, err := storage.ParseTag(e); err == nil {
			t.Fatalf("a snapshot was published: %s", e)
		}
	}
}

// problemHook answers every request whose path contains match with status,
// as a problem, with Retry-After when after is set.
func problemHook(match string, status int, after string, calls *atomic.Int32) hook {
	return func(w http.ResponseWriter, r *http.Request, _ http.Handler) bool {
		if !strings.Contains(r.URL.Path, match) {
			return false
		}
		calls.Add(1)
		if after != "" {
			w.Header().Set("Retry-After", after)
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"type":"about:blank","title":"x","detail":"y","status":%d}`, status)
		return true
	}
}

// A refusal stops its scope: authentication and a redirect stop the
// remote, a refused conversation stops that conversation, four failures
// and a Retry-After past a minute stop the remote.
func TestRefusalsStopTheirScope(t *testing.T) {
	rm := serveRemote(t, "provider-bodies", false, nil)
	ctx := context.Background()
	if _, err := rm.client("wrong", client.RemoteOptions{}).List(ctx, nil); !errors.Is(err, client.ErrRemote) {
		t.Fatalf("a wrong token: %v", err)
	}
	r := rm.client(rm.token, client.RemoteOptions{})
	if _, _, err := r.Manifest(ctx, "nope", &client.Tries{}); !errors.Is(err, client.ErrConversation) {
		t.Fatalf("an unknown conversation: %v", err)
	}
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) }))
	defer other.Close()
	rm.setHook(func(w http.ResponseWriter, r *http.Request, _ http.Handler) bool {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
		return true
	})
	if _, err := r.List(ctx, nil); !errors.Is(err, client.ErrRemote) || elsewhere.Load() != 0 {
		t.Fatalf("a redirect: %v, followed %d", err, elsewhere.Load())
	}
	var calls atomic.Int32
	rm.setHook(problemHook("/conversations", http.StatusServiceUnavailable, "", &calls))
	if _, err := r.List(ctx, nil); !errors.Is(err, client.ErrRemote) || calls.Load() != 4 {
		t.Fatalf("always 503: %v after %d", err, calls.Load())
	}
	calls.Store(0)
	rm.setHook(problemHook("/conversations", http.StatusTooManyRequests, "120", &calls))
	if _, err := r.List(ctx, nil); !errors.Is(err, client.ErrRemote) || calls.Load() != 1 {
		t.Fatalf("Retry-After 120: %v after %d", err, calls.Load())
	}
	calls.Store(0)
	once := problemHook("/conversations", http.StatusServiceUnavailable, "1", &calls)
	rm.setHook(func(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
		return calls.Load() == 0 && once(w, r, next)
	})
	start := time.Now()
	if _, err := r.List(ctx, nil); err != nil || time.Since(start) < time.Second {
		t.Fatalf("Retry-After 1: %v after %s", err, time.Since(start))
	}
}

// tamper rewrites the first data answer with change before it is sent.
func tamper(change func(h http.Header, status *int, body *[]byte)) hook {
	var done atomic.Bool
	return func(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
		if !strings.HasSuffix(r.URL.Path, "/data") || done.Swap(true) {
			return false
		}
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		status, body := rec.Code, rec.Body.Bytes()
		change(rec.Header(), &status, &body)
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
		return true
	}
}

// A range answer that is not the range asked for stops the remote and
// publishes nothing.
func TestARangeThatIsNotTheOneAskedForStopsTheRemote(t *testing.T) {
	for name, change := range map[string]func(h http.Header, status *int, body *[]byte){
		"encoded":       func(h http.Header, _ *int, _ *[]byte) { h.Set("Content-Encoding", "gzip") },
		"another range": func(h http.Header, _ *int, _ *[]byte) { h.Set("Content-Range", "bytes 1-2/3") },
		"another etag":  func(h http.Header, _ *int, _ *[]byte) { h.Set("ETag", `"`+strings.Repeat("0", 64)+`"`) },
		"a whole file":  func(_ http.Header, status *int, _ *[]byte) { *status = http.StatusOK },
	} {
		t.Run(name, func(t *testing.T) {
			rm := serveRemote(t, "provider-bodies", false, nil)
			rm.setHook(tamper(change))
			local := newLocal(t)
			if _, err := pullOver(t, rm.client(rm.token, client.RemoteOptions{}), local, rm.conversation); !errors.Is(err, client.ErrRemote) {
				t.Fatalf("%v", err)
			}
			dir, err := storage.ConversationDir(local.Root, "prod", rm.conversation)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries(t, dir) {
				if _, err := storage.ParseTag(e); err == nil {
					t.Fatalf("a snapshot was published: %s", e)
				}
			}
		})
	}
}

// One origin and token run at most two requests at once, across every
// remote that shares its slots.
func TestAnOriginRunsTwoRequestsAtOnce(t *testing.T) {
	rm := serveRemote(t, "provider-bodies", false, nil)
	var running, most atomic.Int32
	rm.setHook(func(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
		n := running.Add(1)
		defer running.Add(-1)
		for {
			m := most.Load()
			if n <= m || most.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
		next.ServeHTTP(w, r)
		return true
	})
	slots := make(chan struct{}, 2)
	a := rm.client(rm.token, client.RemoteOptions{Slots: slots})
	b := rm.client(rm.token, client.RemoteOptions{Slots: slots})
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := a
			if i%2 == 1 {
				r = b
			}
			if _, err := r.List(context.Background(), nil); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if most.Load() != 2 {
		t.Fatalf("%d requests ran at once", most.Load())
	}
}

// TLS is verified: the server's authority pulls, another authority and a
// name the certificate does not hold stop the remote before any request,
// and server_name checks the certificate against the name it gives.
func TestTLSIsVerified(t *testing.T) {
	rm := serveRemote(t, "provider-bodies", true, nil)
	ctx := context.Background()
	r := rm.client(rm.token, client.RemoteOptions{})
	if _, err := pullOver(t, r, newLocal(t), rm.conversation); err != nil {
		t.Fatal(err)
	}
	// Every httptest server has the same certificate, so the other
	// authority is made here.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "another authority"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	otherCA := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	before := len(rm.requests(""))
	if _, err := rm.client(rm.token, client.RemoteOptions{CA: otherCA}).List(ctx, nil); !errors.Is(err, client.ErrRemote) {
		t.Fatalf("another authority: %v", err)
	}
	if _, err := rm.client(rm.token, client.RemoteOptions{ServerName: "asz.invalid"}).List(ctx, nil); !errors.Is(err, client.ErrRemote) {
		t.Fatalf("another name: %v", err)
	}
	if after := len(rm.requests("")); after != before {
		t.Fatalf("%d requests reached the server", after-before)
	}
	// The test certificate holds example.com.
	if _, err := rm.client(rm.token, client.RemoteOptions{ServerName: "example.com"}).List(ctx, nil); err != nil {
		t.Fatalf("server_name: %v", err)
	}
	if rm.http.TLS == nil || !slices.Contains(rm.http.TLS.NextProtos, "h2") {
		t.Fatal("the test server does not offer HTTP/2")
	}
}

// A remote's name, not its URL, names its copies: the same server reached
// at another URL finds its snapshot unchanged.
func TestARemotesCopiesOutliveItsURL(t *testing.T) {
	rm := serveRemote(t, "provider-bodies", false, nil)
	local := newLocal(t)
	first, err := pullOver(t, rm.client(rm.token, client.RemoteOptions{}), local, rm.conversation)
	if err != nil {
		t.Fatal(err)
	}
	moved, err := url.Parse(strings.Replace(rm.http.URL, "127.0.0.1", "localhost", 1))
	if err != nil {
		t.Fatal(err)
	}
	r, err := client.NewRemote(client.RemoteOptions{Name: "prod", Base: moved, Token: rm.token})
	if err != nil {
		t.Fatal(err)
	}
	again, err := pullOver(t, r, local, rm.conversation)
	if err != nil || !again.Unchanged || again.Tag != first.Tag {
		t.Fatalf("%+v %v", again, err)
	}
}

// A server that restarts between a manifest and its archive serves the
// lease its predecessor granted, and the pull completes.
func TestAPullOutlivesTheServersRestart(t *testing.T) {
	rm := serveRemote(t, "provider-bodies", false, nil)
	var restarted atomic.Bool
	rm.setHook(func(w http.ResponseWriter, r *http.Request, _ http.Handler) bool {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/archives") && !restarted.Swap(true) {
			rm.stop()
			rm.start()
			// The request is answered by the new owner, once it owns
			// the root's pulls, through the client's retries.
			rm.mu.Lock()
			inner := rm.inner
			rm.mu.Unlock()
			inner.ServeHTTP(w, r)
			return true
		}
		return false
	})
	res, err := pullOver(t, rm.client(rm.token, client.RemoteOptions{}), newLocal(t), rm.conversation)
	if err != nil || res.Fetched == 0 || !restarted.Load() {
		t.Fatalf("%+v %v", res, err)
	}
}

// Two pulls of one conversation at once, into two roots, through one
// origin's two request slots, both complete.
func TestTwoPullsAtOnceBothComplete(t *testing.T) {
	rm := serveRemote(t, "provider-bodies", false, nil)
	slots := make(chan struct{}, 2)
	var wg sync.WaitGroup
	tags := make([]string, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := pullOver(t, rm.client(rm.token, client.RemoteOptions{Slots: slots}), newLocal(t), rm.conversation)
			if err != nil {
				t.Error(err)
				return
			}
			tags[i] = res.Tag
		}()
	}
	wg.Wait()
	if tags[0] == "" || tags[1] == "" {
		t.Fatalf("%v", tags)
	}
}

// A pull cancelled while its archive arrives publishes nothing and leaves
// nothing a later pull trips on.
func TestACancelledPullPublishesNothing(t *testing.T) {
	rm := serveRemote(t, "provider-bodies", false, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rm.setHook(func(_ http.ResponseWriter, r *http.Request, _ http.Handler) bool {
		if !strings.HasSuffix(r.URL.Path, "/data") {
			return false
		}
		cancel()
		<-r.Context().Done()
		return true
	})
	r := rm.client(rm.token, client.RemoteOptions{})
	local := newLocal(t)
	tries := &client.Tries{}
	m, resp, err := r.Manifest(ctx, rm.conversation, tries)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := local.Pull(ctx, "prod", rm.conversation, m, r.Source(rm.conversation, m, resp, tries)); !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	dir, err := storage.ConversationDir(local.Root, "prod", rm.conversation)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries(t, dir) {
		if _, err := storage.ParseTag(e); err == nil {
			t.Fatalf("a snapshot was published: %s", e)
		}
	}
	rm.setHook(nil)
	if _, err := pullOver(t, r, local, rm.conversation); err != nil {
		t.Fatal(err)
	}
}

// An archive acquisition has one budget of four failures across its
// requests: ranges that keep failing stop the remote at the fourth.
func TestAnArchivesFailuresShareOneBudget(t *testing.T) {
	rm := serveRemote(t, "provider-bodies", false, nil)
	var calls atomic.Int32
	rm.setHook(problemHook("/data", http.StatusBadGateway, "", &calls))
	_, err := pullOver(t, rm.client(rm.token, client.RemoteOptions{}), newLocal(t), rm.conversation)
	if !errors.Is(err, client.ErrRemote) || calls.Load() != 4 {
		t.Fatalf("%v after %d ranges", err, calls.Load())
	}
}

// A range answer with a byte past its length, on a connection kept open,
// never reaches the archive: Go reads a body to its Content-Length, and
// what follows breaks the next answer on that connection, which is tried
// again.
func TestAByteBeyondARangeNeverReachesTheArchive(t *testing.T) {
	rm := serveRemote(t, "provider-bodies", false, nil)
	var done atomic.Bool
	rm.setHook(func(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
		if !strings.HasSuffix(r.URL.Path, "/data") || done.Swap(true) {
			return false
		}
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return true
		}
		fmt.Fprintf(buf, "HTTP/1.1 %d %s\r\n", rec.Code, http.StatusText(rec.Code))
		_ = rec.Header().Write(buf)
		fmt.Fprintf(buf, "\r\n%sX", rec.Body.Bytes())
		_ = buf.Flush()
		go func() {
			time.Sleep(2 * time.Second)
			conn.Close()
		}()
		return true
	})
	res, err := pullOver(t, rm.client(rm.token, client.RemoteOptions{}), newLocal(t), rm.conversation)
	if err != nil || res.Fetched == 0 {
		t.Fatalf("%+v %v", res, err)
	}
}

// answer writes an answer of status with a JSON body: a problem when the
// status is 400 or more.
func answer(w http.ResponseWriter, status int, body string) {
	if status >= 400 {
		w.Header().Set("Content-Type", "application/problem+json")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	fmt.Fprint(w, body)
}

func plainProblem(status int) string {
	return fmt.Sprintf(`{"type":"about:blank","title":"x","detail":"y","status":%d}`, status)
}

// archiveOf is the snapshot and archive a request's path names.
func archiveOf(r *http.Request) (snapshot, archive string) {
	parts := strings.Split(r.URL.Path, "/")
	for i := range parts {
		if parts[i] == "snapshots" && i+1 < len(parts) {
			snapshot = parts[i+1]
		}
		if parts[i] == "archives" && i+1 < len(parts) {
			archive = parts[i+1]
		}
	}
	return snapshot, archive
}

// A build made again from a changed list goes on with what is left of its
// acquisition's budget: three failures, the snapshot gone, a changed list,
// and one more failure stop the remote.
func TestAChangedListGoesOnWithItsAcquisitionsBudget(t *testing.T) {
	hide, restore := lastRoundHidden(t)
	rm := serveRemote(t, "three-rounds", false, hide)
	var calls atomic.Int32
	rm.setHook(func(w http.ResponseWriter, r *http.Request, _ http.Handler) bool {
		if !strings.HasSuffix(r.URL.Path, "/data") {
			return false
		}
		switch n := calls.Add(1); {
		case n <= 3:
			answer(w, http.StatusBadGateway, plainProblem(http.StatusBadGateway))
		case n == 4:
			restore()
			answer(w, http.StatusGone, plainProblem(http.StatusGone))
		case n == 5:
			answer(w, http.StatusServiceUnavailable, plainProblem(http.StatusServiceUnavailable))
		default:
			return false
		}
		return true
	})
	if _, err := pullOver(t, rm.client(rm.token, client.RemoteOptions{}), newLocal(t), rm.conversation); !errors.Is(err, client.ErrRemote) || calls.Load() != 5 {
		t.Fatalf("%v after %d ranges", err, calls.Load())
	}
}

// A refresh outside an archive acquisition, its freeze gone, starts a fresh
// budget: three failures, a 409, and one more failure still pull.
func TestARefreshOutsideAnAcquisitionStartsAFreshBudget(t *testing.T) {
	rm := serveRemote(t, "provider-bodies", false, nil)
	var calls atomic.Int32
	rm.setHook(func(w http.ResponseWriter, r *http.Request, _ http.Handler) bool {
		if !strings.HasSuffix(r.URL.Path, "/manifest") {
			return false
		}
		switch n := calls.Add(1); {
		case n <= 3, n == 5:
			answer(w, http.StatusServiceUnavailable, plainProblem(http.StatusServiceUnavailable))
		case n == 4:
			answer(w, http.StatusConflict, plainProblem(http.StatusConflict))
		default:
			return false
		}
		return true
	})
	r := rm.client(rm.token, client.RemoteOptions{})
	ctx := context.Background()
	_, resp, err := r.Manifest(ctx, rm.conversation, &client.Tries{})
	if err != nil {
		t.Fatalf("%v after %d manifests", err, calls.Load())
	}
	if err := r.Done(ctx, rm.conversation, resp); err != nil {
		t.Fatal(err)
	}
}

// A poll that answers its freeze's saved source failure drops the freeze:
// the next request asks for a new one, never that freeze again.
func TestASavedSourceFailureDropsItsFreeze(t *testing.T) {
	rm := serveRemote(t, "provider-bodies", false, nil)
	freeze := strings.Repeat("a", 64)
	var pending atomic.Bool
	var polls atomic.Int32
	rm.setHook(func(w http.ResponseWriter, r *http.Request, _ http.Handler) bool {
		if !strings.HasSuffix(r.URL.Path, "/manifest") {
			return false
		}
		switch {
		case r.Header.Get("Asz-Pull-Freeze") == freeze:
			polls.Add(1)
			answer(w, http.StatusServiceUnavailable, `{"type":"`+format.ProblemSourceRead+`","title":"Service Unavailable",`+
				`"detail":"the source could not be read","status":503,"conversation":"`+rm.conversation+`","freeze":"`+freeze+`","attempt":1}`)
		case !pending.Swap(true):
			answer(w, http.StatusAccepted, `{"schema":"pull/1","conversation":"`+rm.conversation+`","freeze":"`+freeze+`","state":"building"}`)
		default:
			return false
		}
		return true
	})
	r := rm.client(rm.token, client.RemoteOptions{})
	ctx := context.Background()
	_, resp, err := r.Manifest(ctx, rm.conversation, &client.Tries{})
	if err != nil || polls.Load() != 1 {
		t.Fatalf("%v after %d polls of the failed freeze", err, polls.Load())
	}
	if err := r.Done(ctx, rm.conversation, resp); err != nil {
		t.Fatal(err)
	}
}

// A fresh manifest that lists the same files under another head is not the
// manifest the build was fetching: the build is made again from it, and
// evidence that does not hold its head publishes nothing.
func TestTheSameFilesUnderAnotherHeadAreBuiltAgain(t *testing.T) {
	rm := serveRemote(t, "three-rounds", false, nil)
	var gone atomic.Bool
	var changed atomic.Bool
	rm.setHook(func(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/archives") && !gone.Swap(true):
			answer(w, http.StatusGone, plainProblem(http.StatusGone))
			return true
		case gone.Load() && strings.HasSuffix(r.URL.Path, "/manifest") && !changed.Load():
			rec := httptest.NewRecorder()
			next.ServeHTTP(rec, r)
			body := rec.Body.Bytes()
			if rec.Code == http.StatusOK {
				var m struct {
					Round struct {
						Digest string `json:"digest"`
					} `json:"round"`
				}
				if err := json.Unmarshal(body, &m); err != nil || len(m.Round.Digest) != 64 {
					t.Errorf("the manifest's head: %v", err)
				}
				// A digit past the first twelve, which a tag shows.
				d := []byte(m.Round.Digest)
				if d[20] == '0' {
					d[20] = '1'
				} else {
					d[20] = '0'
				}
				body = bytes.Replace(body, []byte(m.Round.Digest), d, 1)
				changed.Store(true)
			}
			for k, v := range rec.Header() {
				w.Header()[k] = v
			}
			w.Header().Del("Content-Length")
			w.WriteHeader(rec.Code)
			_, _ = w.Write(body)
			return true
		}
		return false
	})
	local := newLocal(t)
	if _, err := pullOver(t, rm.client(rm.token, client.RemoteOptions{}), local, rm.conversation); err == nil || !changed.Load() {
		t.Fatalf("evidence under a head it does not hold: %v", err)
	}
	dir, err := storage.ConversationDir(local.Root, "prod", rm.conversation)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries(t, dir) {
		if _, err := storage.ParseTag(e); err == nil {
			t.Fatalf("a snapshot was published: %s", e)
		}
	}
}

// A range's answer that is not its bytes is read as any answer is, by its
// status and its problem.
func TestARangesRefusalIsReadAsAnyAnswer(t *testing.T) {
	type when struct {
		name   string
		answer func(w http.ResponseWriter, r *http.Request)
		check  func(t *testing.T, rm *remote, err error, calls int32)
	}
	for _, c := range []when{{
		name: "storage-state stops the conversation at once",
		answer: func(w http.ResponseWriter, _ *http.Request) {
			answer(w, http.StatusInternalServerError, `{"type":"`+format.ProblemStorageState+`","title":"x","detail":"y","status":500}`)
		},
		check: func(t *testing.T, _ *remote, err error, calls int32) {
			if !errors.Is(err, client.ErrConversation) || calls != 1 {
				t.Fatalf("%v after %d ranges", err, calls)
			}
		},
	}, {
		name: "a failed build sends the client to the archive's state",
		answer: func(w http.ResponseWriter, r *http.Request) {
			snapshot, archive := archiveOf(r)
			answer(w, http.StatusInternalServerError, `{"type":"`+format.ProblemArchiveFailed+`","title":"x","detail":"y","status":500,`+
				`"snapshot":"`+snapshot+`","archive":"`+archive+`","attempt":1}`)
		},
		check: func(t *testing.T, rm *remote, err error, _ int32) {
			if err != nil {
				t.Fatal(err)
			}
			var data []int
			states := 0
			for i, s := range rm.requests("") {
				switch {
				case strings.Contains(s, "/data "):
					data = append(data, i)
				case len(data) == 1 && strings.HasPrefix(s, "GET ") && strings.Contains(s, "/archives/"):
					states++
				}
			}
			if len(data) != 2 || states == 0 {
				t.Fatalf("no state was asked for between the ranges: %v", rm.requests(""))
			}
		},
	}, {
		name: "a failed build of another archive stops the remote",
		answer: func(w http.ResponseWriter, r *http.Request) {
			snapshot, _ := archiveOf(r)
			answer(w, http.StatusInternalServerError, `{"type":"`+format.ProblemArchiveFailed+`","title":"x","detail":"y","status":500,`+
				`"snapshot":"`+snapshot+`","archive":"`+strings.Repeat("b", 64)+`","attempt":1}`)
		},
		check: func(t *testing.T, _ *remote, err error, _ int32) {
			if !errors.Is(err, client.ErrRemote) {
				t.Fatal(err)
			}
		},
	}, {
		name: "a failed build as a 412 stops the remote",
		answer: func(w http.ResponseWriter, r *http.Request) {
			snapshot, archive := archiveOf(r)
			answer(w, http.StatusPreconditionFailed, `{"type":"`+format.ProblemArchiveFailed+`","title":"x","detail":"y","status":412,`+
				`"snapshot":"`+snapshot+`","archive":"`+archive+`","attempt":1}`)
		},
		check: func(t *testing.T, _ *remote, err error, _ int32) {
			if !errors.Is(err, client.ErrRemote) {
				t.Fatal(err)
			}
		},
	}, {
		name: "a failed build as a 410 stops the remote",
		answer: func(w http.ResponseWriter, r *http.Request) {
			snapshot, archive := archiveOf(r)
			answer(w, http.StatusGone, `{"type":"`+format.ProblemArchiveFailed+`","title":"x","detail":"y","status":410,`+
				`"snapshot":"`+snapshot+`","archive":"`+archive+`","attempt":1}`)
		},
		check: func(t *testing.T, _ *remote, err error, _ int32) {
			if !errors.Is(err, client.ErrRemote) {
				t.Fatal(err)
			}
		},
	}, {
		name: "a failed build with no attempt stops the remote",
		answer: func(w http.ResponseWriter, r *http.Request) {
			snapshot, archive := archiveOf(r)
			answer(w, http.StatusInternalServerError, `{"type":"`+format.ProblemArchiveFailed+`","title":"x","detail":"y","status":500,`+
				`"snapshot":"`+snapshot+`","archive":"`+archive+`"}`)
		},
		check: func(t *testing.T, _ *remote, err error, _ int32) {
			if !errors.Is(err, client.ErrRemote) {
				t.Fatal(err)
			}
		},
	}, {
		name: "a 202 of another archive stops the remote",
		answer: func(w http.ResponseWriter, r *http.Request) {
			snapshot, _ := archiveOf(r)
			answer(w, http.StatusAccepted, `{"schema":"pull/1","snapshot":"`+snapshot+`","archive":"`+strings.Repeat("b", 64)+`","state":"building"}`)
		},
		check: func(t *testing.T, _ *remote, err error, _ int32) {
			if !errors.Is(err, client.ErrRemote) {
				t.Fatal(err)
			}
		},
	}, {
		name: "a 412 without a problem stops the remote",
		answer: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusPreconditionFailed)
			fmt.Fprint(w, "stale")
		},
		check: func(t *testing.T, _ *remote, err error, _ int32) {
			if !errors.Is(err, client.ErrRemote) {
				t.Fatal(err)
			}
		},
	}, {
		name: "a Retry-After past a minute stops the remote at once",
		answer: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "120")
			answer(w, http.StatusTooManyRequests, plainProblem(http.StatusTooManyRequests))
		},
		check: func(t *testing.T, _ *remote, err error, calls int32) {
			if !errors.Is(err, client.ErrRemote) || calls != 1 {
				t.Fatalf("%v after %d ranges", err, calls)
			}
		},
	}} {
		t.Run(c.name, func(t *testing.T) {
			rm := serveRemote(t, "provider-bodies", false, nil)
			var calls atomic.Int32
			rm.setHook(func(w http.ResponseWriter, r *http.Request, _ http.Handler) bool {
				if !strings.HasSuffix(r.URL.Path, "/data") || calls.Add(1) > 1 {
					return false
				}
				c.answer(w, r)
				return true
			})
			_, err := pullOver(t, rm.client(rm.token, client.RemoteOptions{}), newLocal(t), rm.conversation)
			c.check(t, rm, err, calls.Load())
		})
	}
}

// A failed build answered to a range counts against the acquisition's
// budget: three failures and a failed build stop the remote, though the
// archive is ready again.
func TestAFailedBuildOnARangeIsCharged(t *testing.T) {
	rm := serveRemote(t, "provider-bodies", false, nil)
	var calls atomic.Int32
	rm.setHook(func(w http.ResponseWriter, r *http.Request, _ http.Handler) bool {
		if !strings.HasSuffix(r.URL.Path, "/data") {
			return false
		}
		switch n := calls.Add(1); {
		case n <= 3:
			answer(w, http.StatusBadGateway, plainProblem(http.StatusBadGateway))
		case n == 4:
			snapshot, archive := archiveOf(r)
			answer(w, http.StatusInternalServerError, `{"type":"`+format.ProblemArchiveFailed+`","title":"x","detail":"y","status":500,`+
				`"snapshot":"`+snapshot+`","archive":"`+archive+`","attempt":1}`)
		default:
			return false
		}
		return true
	})
	if _, err := pullOver(t, rm.client(rm.token, client.RemoteOptions{}), newLocal(t), rm.conversation); !errors.Is(err, client.ErrRemote) || calls.Load() != 4 {
		t.Fatalf("%v after %d ranges", err, calls.Load())
	}
}

// A range answered with a Retry-After of a second waits that second.
func TestARangesRetryAfterIsWaited(t *testing.T) {
	rm := serveRemote(t, "provider-bodies", false, nil)
	var calls atomic.Int32
	var first, second atomic.Int64
	rm.setHook(func(w http.ResponseWriter, r *http.Request, _ http.Handler) bool {
		if !strings.HasSuffix(r.URL.Path, "/data") {
			return false
		}
		switch calls.Add(1) {
		case 1:
			first.Store(time.Now().UnixNano())
			w.Header().Set("Retry-After", "2")
			answer(w, http.StatusServiceUnavailable, plainProblem(http.StatusServiceUnavailable))
			return true
		case 2:
			second.Store(time.Now().UnixNano())
		}
		return false
	})
	if _, err := pullOver(t, rm.client(rm.token, client.RemoteOptions{}), newLocal(t), rm.conversation); err != nil {
		t.Fatal(err)
	}
	if waited := time.Duration(second.Load() - first.Load()); waited < 2*time.Second {
		t.Fatalf("waited %s", waited)
	}
}

// A retryable answer whose problem does not read stops the remote at once,
// for a listing and a release as for any route.
func TestARetryableAnswersProblemIsChecked(t *testing.T) {
	rm := serveRemote(t, "provider-bodies", false, nil)
	r := rm.client(rm.token, client.RemoteOptions{})
	ctx := context.Background()
	_, resp, err := r.Manifest(ctx, rm.conversation, &client.Tries{})
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"/conversations", "/done"} {
		var calls atomic.Int32
		rm.setHook(func(w http.ResponseWriter, req *http.Request, _ http.Handler) bool {
			if !strings.HasSuffix(req.URL.Path, route) {
				return false
			}
			calls.Add(1)
			answer(w, http.StatusServiceUnavailable, `{"type":`)
			return true
		})
		if route == "/conversations" {
			_, err = r.List(ctx, nil)
		} else {
			err = r.Done(ctx, rm.conversation, resp)
		}
		if !errors.Is(err, client.ErrRemote) || calls.Load() != 1 {
			t.Fatalf("%s: %v after %d", route, err, calls.Load())
		}
	}
	rm.setHook(nil)
	if err := r.Done(ctx, rm.conversation, resp); err != nil {
		t.Fatal(err)
	}
}

// A server that refuses the handshake for want of a client certificate
// stops the remote at its first request, never tried again.
func TestAMissingClientCertificateStopsTheRemoteAtOnce(t *testing.T) {
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		answer(w, http.StatusOK, `{"schema":"pull/1","conversations":[]}`)
	}))
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.StartTLS()
	defer srv.Close()
	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	r, err := client.NewRemote(client.RemoteOptions{Name: "prod", Base: base, Token: "t",
		CA: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.List(context.Background(), nil); !errors.Is(err, client.ErrRemote) || conns.Load() != 1 {
		t.Fatalf("%v after %d connections", err, conns.Load())
	}
}
