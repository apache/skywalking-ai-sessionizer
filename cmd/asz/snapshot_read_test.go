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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/client"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// landedIn is a landed file of p's snapshot.
func landedIn(t *testing.T, p pulled) string {
	t.Helper()
	files, err := storage.LandedFiles(storage.NewZone(p.snapshot), p.session)
	if err != nil || len(files) == 0 {
		t.Fatalf("no landed files: %v", err)
	}
	return files[0].Path
}

// cutClosingLine cuts the closing line off a file, so a read of it never
// completes.
func cutClosingLine(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.SplitAfter(bytes.TrimSuffix(b, []byte("\n")), []byte("\n"))
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Join(lines[:len(lines)-1], nil), 0o644); err != nil {
		t.Fatal(err)
	}
}

// flipInterior flips a byte in an interior round of p's chain, keeping its
// head.
func flipInterior(t *testing.T, p pulled) {
	t.Helper()
	files, err := sessionflow.OpenChain(p.snapshot, p.conversation).List()
	if err != nil || len(files) < 3 {
		t.Fatalf("%d rounds: %v", len(files), err)
	}
	path := files[1].Path
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)/2] ^= 0x01
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Every command that reads a snapshot fails on damage it reads, and prints
// no document, no measurement and no part of either.
func TestCommandsOverADamagedSnapshotFail(t *testing.T) {
	for name, hurt := range map[string]func(*testing.T, pulled){
		"a landed file cut short": func(t *testing.T, p pulled) { cutClosingLine(t, landedIn(t, p)) },
		"an interior round":       flipInterior,
	} {
		t.Run(name, func(t *testing.T) {
			p := pulledSnapshot(t)
			hurt(t, p)
			cfg := rootConfig(p.snapshot)
			for _, format := range []string{"", "json"} {
				positional = []string{p.conversation}
				if err := admitCommand("conversation", cfg); err != nil {
					t.Fatal(err)
				}
				docFormat = format
				var err error
				out := stdoutOf(t, func() { err = cmdConversation(cfg, config.Adapter{}, false) })
				docFormat = ""
				releaseAdmission()
				if !errors.Is(err, view.ErrEvidence) || out != "" {
					t.Errorf("conversation %q: %v, printed %q", format, err, out)
				}
			}
			cfgFile := filepath.Join(t.TempDir(), "asz.yaml")
			if err := os.WriteFile(cfgFile, []byte("storage:\n  root: "+p.snapshot+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			code := 0
			var errText string
			out := stdoutOf(t, func() {
				errText = stderrOf(t, func() { code = cmdMeasure([]string{"-config", cfgFile, p.conversation}) })
			})
			if code == 0 || out != "" || !strings.Contains(errText, "does not read whole") {
				t.Errorf("measure: exit %d, printed %q, said %s", code, out, errText)
			}
			// A judge run fails before it reserves a run or starts an
			// evaluator, which here is a program that would answer nothing.
			judgeFile := filepath.Join(t.TempDir(), "judge.yaml")
			if err := os.WriteFile(judgeFile, []byte("storage:\n  root: "+p.snapshot+"\njudge:\n  evaluators:\n    none:\n"+
				"      kind: cli\n      program: claude\n      command: "+os.DevNull+"\n  plan:\n    - stage: targets\n      evaluator: none\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			out = stdoutOf(t, func() {
				errText = stderrOf(t, func() { code = cmdJudge([]string{"-config", judgeFile, p.conversation}) })
			})
			if code == 0 || out != "" || !strings.Contains(errText, "does not read whole") {
				t.Errorf("judge: exit %d, printed %q, said %s", code, out, errText)
			}
			if _, err := os.Stat(filepath.Join(p.snapshot, p.session, storage.JudgmentsDir)); err == nil {
				t.Error("a judge run over damage reserved a run")
			}
			if got := p.leases(t); len(got) != 0 {
				t.Errorf("leases after the failed commands: %v", got)
			}
		})
	}
}

// withStdin runs fn with standard input reading in.
func withStdin(t *testing.T, in string, fn func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = io.WriteString(w, in)
		_ = w.Close()
	}()
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old; _ = r.Close() }()
	fn()
}

// An inherited navigation server loads under its chunk attempt's marker: a
// load that ends well leaves none, and one that reads damage, or finds a
// head other than the run's, leaves it fatal and fails.
func TestAnInheritedServerLoadsUnderItsMarker(t *testing.T) {
	for name, tc := range map[string]struct {
		hurt   func(*testing.T, pulled)
		digest func(string) string
		fails  bool
	}{
		"a sound snapshot":        {nil, nil, false},
		"a landed file cut short": {func(t *testing.T, p pulled) { cutClosingLine(t, landedIn(t, p)) }, nil, true},
		"another head":            {nil, func(d string) string { return strings.Repeat("0", len(d)) }, true},
		"a damaged manifest": {func(t *testing.T, p pulled) {
			path := filepath.Join(p.snapshot, "_manifest")
			_ = os.Chmod(path, 0o644)
			if err := os.WriteFile(path, []byte("{"), 0o444); err != nil {
				t.Fatal(err)
			}
		}, nil, true},
	} {
		t.Run(name, func(t *testing.T) {
			p := pulledSnapshot(t)
			v, err := sessionflow.OpenChain(p.snapshot, p.conversation).Fold()
			if err != nil {
				t.Fatal(err)
			}
			parent, work := runLease(t, p, "j000001")
			defer func() { _ = parent.Release(t.Context()) }()
			log := filepath.Join(work, "2-targets", "c001", "a1", "mcp-calls.jsonl")
			if err := os.WriteFile(log, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			report := reportFor(log, parent.Lease())
			digest := v.Digest
			if tc.digest != nil {
				digest = tc.digest(digest)
			}
			if tc.hurt != nil {
				tc.hurt(t, p)
			}
			code := 0
			var errText string
			withStdin(t, "", func() {
				errText = stderrOf(t, func() {
					code = cmdJudgeMCP([]string{"-root", p.snapshot, "-conversation", p.conversation, "-round", fmt.Sprint(v.Round),
						"-digest", digest, "-log", log, "-parent-lease", parent.Lease(), "-failure-report", report})
				})
			})
			b, rerr := os.ReadFile(report)
			switch {
			case !tc.fails && (code != 0 || rerr == nil):
				t.Fatalf("exit %d, marker %q %v: %s", code, b, rerr, errText)
			case tc.fails && (code == 0 || rerr != nil || !strings.HasPrefix(string(b), "fatal: ")):
				t.Fatalf("exit %d, marker %q %v: %s", code, b, rerr, errText)
			}
		})
	}
}

// statusOf reads the page's status.
func statusOf(t *testing.T, addr string) view.Status {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st view.Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

// A pulled snapshot's page never watches its root, where an ordinary root's
// page does.
func TestASnapshotPageIsNotWatched(t *testing.T) {
	p := pulledSnapshot(t)
	positional = []string{p.conversation}
	if err := admitCommand("view", rootConfig(p.snapshot)); err != nil {
		t.Fatal(err)
	}
	defer releaseAdmission()
	damaged := make(chan error, 1)
	snap, err := snapshotView(func(err error) { damaged <- err })
	if err != nil || snap == nil {
		t.Fatalf("the snapshot: %v", err)
	}
	srv := view.New(storage.NewZone(p.snapshot), nil)
	srv.SetSnapshot(snap)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- servePage(ln, &protection{}, srv, snap, damaged, 10*time.Millisecond, nil, nil) }()
	time.Sleep(200 * time.Millisecond)
	if st := statusOf(t, ln.Addr().String()); st.LastRefresh != 0 {
		t.Fatalf("a snapshot's page was watched: %+v", st)
	}
	damaged <- errors.New("stop")
	if err := <-served; err == nil || err.Error() != "stop" {
		t.Fatalf("the page ended with %v", err)
	}

	// The same files served as an ordinary root are watched, so the check
	// above can see a watcher.
	ordinary := view.New(storage.NewZone(p.snapshot), nil)
	ln, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = servePage(ln, &protection{}, ordinary, nil, nil, 10*time.Millisecond, nil, nil) }()
	defer ln.Close()
	time.Sleep(200 * time.Millisecond)
	if st := statusOf(t, ln.Addr().String()); st.LastRefresh == 0 {
		t.Fatalf("an ordinary root's page was not watched: %+v", st)
	}
}

// A snapshot's page stops in order on damage: it takes no new request,
// cancels the running ones, and returns only once every one has ended, so
// its lease outlives every read.
func TestASnapshotPageStopsInOrder(t *testing.T) {
	started := make(chan struct{})
	var ended atomic.Bool
	h := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		// A read that takes a while to notice.
		time.Sleep(100 * time.Millisecond)
		ended.Store(true)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	damaged := make(chan error, 1)
	done := make(chan error, 1)
	go func() { done <- serveSnapshot(ln, &protection{}, h, damaged) }()
	go func() {
		if resp, err := http.Get("http://" + ln.Addr().String() + "/c/x"); err == nil {
			resp.Body.Close()
		}
	}()
	<-started
	damage := fmt.Errorf("%w: a landed file ends early", view.ErrEvidence)
	damaged <- damage
	if err := <-done; !errors.Is(err, view.ErrEvidence) {
		t.Fatalf("the page ended with %v", err)
	}
	if !ended.Load() {
		t.Fatal("the page returned before a running request ended")
	}

	called := false
	g := &requestGate{h: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })}
	g.stop()
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if called || rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a stopped page served a request: %d %v", rec.Code, called)
	}
}

// An inherited server's reads after loading are held to its marker as well:
// a record it reads damaged leaves the marker fatal, and the server ends.
func TestAnInheritedServersReadIsHeldToItsMarker(t *testing.T) {
	p := pulledSnapshot(t)
	v, err := sessionflow.OpenChain(p.snapshot, p.conversation).Fold()
	if err != nil {
		t.Fatal(err)
	}
	parent, work := runLease(t, p, "j000001")
	defer func() { _ = parent.Release(t.Context()) }()
	log := filepath.Join(work, "2-targets", "c001", "a1", "mcp-calls.jsonl")
	if err := os.WriteFile(log, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	report := reportFor(log, parent.Lease())
	files, err := storage.LandedFiles(storage.NewZone(p.snapshot), p.session)
	if err != nil {
		t.Fatal(err)
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// Standard error is the server's from its start to its end, drained:
	// swapped while it runs, its failure would race the swap.
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = io.Copy(io.Discard, errR) }()
	oldIn, oldOut, oldErr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = inR, outW, errW
	defer func() { os.Stdin, os.Stdout, os.Stderr = oldIn, oldOut, oldErr; _ = errW.Close() }()
	code := make(chan int, 1)
	go func() {
		code <- cmdJudgeMCP([]string{"-root", p.snapshot, "-conversation", p.conversation, "-round", fmt.Sprint(v.Round),
			"-digest", v.Digest, "-log", log, "-parent-lease", parent.Lease(), "-failure-report", report})
		_ = outW.Close()
	}()
	replies := json.NewDecoder(outR)
	var reply map[string]any
	if _, err := io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	// The server answers once it has loaded, and its marker is gone.
	if err := replies.Decode(&reply); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(report); err == nil {
		t.Fatal("a load that ended well left its marker")
	}
	cutClosingLine(t, files[0].Path)
	call := fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"record","arguments":{"seq":%d,"row":1}}}`, files[0].Seq)
	if _, err := io.WriteString(inW, call+"\n"); err != nil {
		t.Fatal(err)
	}
	if err := replies.Decode(&reply); err != nil {
		t.Fatal(err)
	}
	exit := <-code
	_ = inW.Close()
	b, err := os.ReadFile(report)
	if exit == 0 || err != nil || !strings.HasPrefix(string(b), "fatal: ") {
		t.Fatalf("exit %d, marker %q %v, reply %v", exit, b, err, reply)
	}
}

// A run's lease admits a navigation server only to that run's own work
// directory, reached through real directories: a work directory that is a
// link to another run's is refused, never followed.
func TestALinkedWorkDirectoryAdmitsNoServer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("links need privileges on Windows")
	}
	p := pulledSnapshot(t)
	a, workA := runLease(t, p, "j000001")
	defer func() { _ = a.Release(t.Context()) }()
	b, workB := runLease(t, p, "j000002")
	defer func() { _ = b.Release(t.Context()) }()
	logB := filepath.Join(workB, "2-targets", "c001", "a1", "mcp-calls.jsonl")
	if err := os.WriteFile(logB, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(workA); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(workB, workA); err != nil {
		t.Fatal(err)
	}
	for _, log := range []string{logB, filepath.Join(workA, "2-targets", "c001", "a1", "mcp-calls.jsonl")} {
		var code int
		stderrOf(t, func() { _, code = admitNavigation(p.snapshot, "", a.Lease(), log, reportFor(log, a.Lease())) })
		if code == 0 {
			releaseAdmission()
			t.Fatalf("run A's lease admitted a server logging to %s", log)
		}
	}
}

// The page over an admitted snapshot stops on a real damaged read with a
// reader still running, and its lease is held until that reader ends: a
// sweep that finds the snapshot expired meanwhile leaves it, and removes it
// once the command has let go.
func TestAnAdmittedPageHoldsItsLeaseUntilItsReadersEnd(t *testing.T) {
	p := pulledSnapshot(t)
	positional = []string{p.conversation}
	if err := admitCommand("view", rootConfig(p.snapshot)); err != nil {
		t.Fatal(err)
	}
	damaged := make(chan error, 1)
	snap, err := snapshotView(func(err error) {
		select {
		case damaged <- err:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := view.New(storage.NewZone(p.snapshot), nil)
	srv.SetSnapshot(snap)
	started := make(chan struct{})
	var ended atomic.Bool
	pages := srv.Handler()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			close(started)
			<-r.Context().Done()
			time.Sleep(300 * time.Millisecond)
			ended.Store(true)
			return
		}
		pages.ServeHTTP(w, r)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- serveSnapshot(ln, &protection{}, h, damaged) }()
	base := "http://" + ln.Addr().String()
	go func() {
		if resp, err := http.Get(base + "/slow"); err == nil {
			resp.Body.Close()
		}
	}()
	<-started
	cutClosingLine(t, landedIn(t, p))
	resp, err := http.Get(base + "/api/c/" + p.conversation + "/view")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("a damaged read answered %d", resp.StatusCode)
	}
	later := &client.Local{Root: p.root, TTL: 2 * time.Second, Now: func() time.Time { return time.Now().Add(time.Hour) }}
	if _, err := later.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.snapshot); err != nil {
		t.Fatalf("a sweep removed a snapshot a running page holds: %v", err)
	}
	if err := <-done; !errors.Is(err, view.ErrEvidence) {
		t.Fatalf("the page ended with %v", err)
	}
	if !ended.Load() {
		t.Fatal("the page returned before its reader ended")
	}
	if got := p.leases(t); len(got) != 1 {
		t.Fatalf("leases while the command has not let go: %v", got)
	}
	releaseAdmission()
	if _, err := later.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.snapshot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the expired snapshot outlived its last reader: %v", err)
	}
}
