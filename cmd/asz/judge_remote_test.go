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
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/pull/server"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario/run"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// pulledConfig is a configuration file whose storage root holds p's
// snapshot, with no remote configured: judging what is here needs none.
func pulledConfig(t *testing.T, p pulled) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "asz.yaml")
	if err := os.WriteFile(path, []byte("storage:\n  root: "+p.root+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// -remote-conversation selects a pulled snapshot under the configured root:
// the newest without a tag or with :latest, one by its tag, and none is a
// failure that says to pull first. What is printed names the snapshot.
func TestARemoteConversationSelectsItsSnapshot(t *testing.T) {
	p := pulledSnapshot(t)
	cfg := pulledConfig(t, p)
	tag := filepath.Base(p.snapshot)
	name := "prod/" + p.conversation
	for _, sel := range []string{name + ":latest", name + ":" + tag} {
		var code int
		out := stdoutOf(t, func() { code = cmdJudge([]string{"-config", cfg, "-list", "-remote-conversation", sel}) })
		if code != 0 || strings.TrimSpace(out) != name+":"+tag+" has no judge runs" {
			t.Fatalf("%s: %d %q", sel, code, out)
		}
	}
	if len(p.leases(t)) != 0 {
		t.Fatalf("a lease outlived its command: %v", p.leases(t))
	}
	var code int
	errOut := stderrOf(t, func() { code = cmdJudge([]string{"-config", cfg, "-list", "-remote-conversation", "prod/nope:latest"}) })
	if code != 1 || !strings.Contains(errOut, "run asz pull prod/nope first") {
		t.Fatalf("no snapshot: %d %q", code, errOut)
	}
}

// -list with no tag lists the runs of every unexpired snapshot, each named
// by its snapshot, REMOTE/CONVERSATION:TAG, and its number. A damaged
// snapshot is said and passed over; damage to the conversation's own
// controls fails the listing.
func TestARemoteListNamesEachRunBySnapshot(t *testing.T) {
	p := pulledSnapshot(t)
	if _, _, err := storage.ReserveRun(storage.NewZone(p.snapshot), p.session); err != nil {
		t.Fatal(err)
	}
	var code int
	out := stdoutOf(t, func() {
		code = cmdJudge([]string{"-config", pulledConfig(t, p), "-list", "-remote-conversation", "prod/" + p.conversation})
	})
	want := "prod/" + p.conversation + ":" + filepath.Base(p.snapshot) + " run 1  unfinished"
	if code != 0 || !strings.HasPrefix(strings.TrimSpace(out), want) {
		t.Fatalf("%d %q, want %q", code, out, want)
	}
	if got := p.leases(t); len(got) != 0 {
		t.Fatalf("a snapshot listed kept its lease: %v", got)
	}
	// With :latest, the selected snapshot's runs are named by it too.
	out = stdoutOf(t, func() {
		code = cmdJudge([]string{"-config", pulledConfig(t, p), "-list", "-remote-conversation", "prod/" + p.conversation + ":latest"})
	})
	if code != 0 || !strings.HasPrefix(strings.TrimSpace(out), want) {
		t.Fatalf("with :latest: %d %q, want %q", code, out, want)
	}
	broken := func(path string) {
		t.Helper()
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{broken"), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	listed := func() (int, string) {
		var said string
		var code int
		_ = stdoutOf(t, func() {
			said = stderrOf(t, func() {
				code = cmdJudge([]string{"-config", pulledConfig(t, p), "-list", "-remote-conversation", "prod/" + p.conversation})
			})
		})
		return code, said
	}
	broken(filepath.Join(p.snapshot, storage.CopyMarker))
	if code, said := listed(); code != 0 || !strings.Contains(said, filepath.Base(p.snapshot)) {
		t.Fatalf("a damaged snapshot: exit %d: %s", code, said)
	}
	broken(filepath.Join(filepath.Dir(p.snapshot), storage.CopyMarker))
	if code, said := listed(); code != 1 {
		t.Fatalf("a damaged conversation: exit %d: %s", code, said)
	}
}

// A pulled conversation is selected under a root that is one: a configured
// root that is itself a snapshot is refused.
func TestARemoteSelectorRefusesACopyAsItsRoot(t *testing.T) {
	p := pulledSnapshot(t)
	path := filepath.Join(t.TempDir(), "asz.yaml")
	if err := os.WriteFile(path, []byte("storage:\n  root: "+p.snapshot+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var code int
	errOut := stderrOf(t, func() {
		code = cmdJudge([]string{"-config", path, "-list", "-remote-conversation", "prod/" + p.conversation + ":latest"})
	})
	if code != 1 || !strings.Contains(errOut, "copies pulled from a remote") {
		t.Fatalf("%d %q", code, errOut)
	}
}

// A selector is checked before anything is read: given with a positional
// conversation, a -pull that is not of the head or not of a new run, and a
// selector that does not parse are usage errors.
func TestARemoteSelectorIsCheckedFirst(t *testing.T) {
	p := pulledSnapshot(t)
	cfg := pulledConfig(t, p)
	name := "prod/" + p.conversation
	for why, args := range map[string][]string{
		"both":                    {"-remote-conversation", name, p.conversation},
		"-pull of a tag":          {"-pull", "-remote-conversation", name + ":" + filepath.Base(p.snapshot)},
		"-pull with -list":        {"-pull", "-list", "-remote-conversation", name},
		"-pull with -show":        {"-pull", "-show", "-remote-conversation", name},
		"-pull with -show -run 1": {"-pull", "-show", "-run", "1", "-remote-conversation", name},
		"-pull with -regroup -1":  {"-pull", "-regroup", "-1", "-remote-conversation", name},
		"-pull alone":             {"-pull", p.conversation},
		"no slash":                {"-remote-conversation", "prod"},
		"not a tag":               {"-remote-conversation", name + ":yesterday"},
		"an empty tag":            {"-remote-conversation", name + ":"},
		"an unsafe remote":        {"-remote-conversation", "../" + p.conversation},
		"an unsafe session":       {"-remote-conversation", "prod/" + strings.Repeat("x", 300)},
	} {
		var code int
		_ = stderrOf(t, func() { code = cmdJudge(append([]string{"-config", cfg}, args...)) })
		if code != 2 {
			t.Errorf("%s: exit %d", why, code)
		}
	}
}

// -pull pulls the head and judges the snapshot the pull ended with: the run
// is made in that snapshot, named by it, and no lease outlives the command.
func TestAPullFirstJudgesWhatItPulled(t *testing.T) {
	out := t.TempDir()
	rep, err := run.Check(filepath.Join("..", "..", "tests", "scenarios", "three-rounds.yaml"),
		run.Options{Out: out, Formats: []scenario.Format{scenario.FormatClaudeCode}})
	if err != nil || rep.Failed {
		t.Fatalf("building the source: %v %v", err, rep.Lines)
	}
	src := filepath.Join(out, string(scenario.FormatClaudeCode))
	entries, err := os.ReadDir(filepath.Join(src, "_conversations"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("conversations: %v", err)
	}
	conversation := entries[0].Name()
	token, hash, err := server.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	tokens := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(tokens, []byte(server.TokenLine("prod", hash)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keeper, err := server.LoadKeeper(tokens, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	guard := server.NewGuard(keeper, 4, nil)
	pulls := server.New(server.Options{Root: src, TTL: time.Hour, Every: time.Hour, Guard: guard})
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan error, 1)
	go func() { ran <- pulls.Run(ctx) }()
	httpServer := httptest.NewServer(guard.Protect(http.NotFoundHandler(), pulls.Handler()))
	defer func() {
		httpServer.Close()
		cancel()
		if err := <-ran; err != nil {
			t.Error(err)
		}
	}()
	t.Setenv("ASZ_TEST_PULL_TOKEN", token)
	local := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "asz.yaml")
	if err := os.WriteFile(cfg, []byte("storage:\n  root: "+local+"\nremotes:\n  - name: prod\n    url: "+httpServer.URL+
		"\n    token_env: ASZ_TEST_PULL_TOKEN\njudge:\n  evaluators:\n    none:\n      kind: cli\n      program: claude\n      command: "+os.DevNull+
		"\n  plan:\n    - stage: targets\n      evaluator: none\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var code int
	printed := stdoutOf(t, func() {
		_ = stderrOf(t, func() {
			code = cmdJudge([]string{"-config", cfg, "-pull", "-remote-conversation", "prod/" + conversation})
		})
	})
	dir := filepath.Join(local, storage.RemoteDir, "prod", conversation)
	tags, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var tag string
	for _, e := range tags {
		if _, err := storage.ParseTag(e.Name()); err == nil {
			tag = e.Name()
		}
	}
	if tag == "" || !strings.Contains(printed, "run 1 of prod/"+conversation+":"+tag) {
		t.Fatalf("exit %d, tag %q, printed %q", code, tag, printed)
	}
	session, err := os.ReadDir(filepath.Join(dir, tag))
	if err != nil {
		t.Fatal(err)
	}
	judged := false
	for _, e := range session {
		if _, err := os.Stat(filepath.Join(dir, tag, e.Name(), storage.JudgmentsDir, "j000001")); err == nil {
			judged = true
		}
	}
	if !judged {
		t.Fatal("the run is not in the snapshot the pull ended with")
	}
	leases, err := os.ReadDir(filepath.Join(dir, tag, storage.LeasesDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 0 {
		t.Fatalf("leases outlived the command: %d", len(leases))
	}
}

// The newest unexpired tag is the one judged: an entry under that tag that
// is not a snapshot, a file where its directory should be, fails the
// selection, and the older snapshot is not read in its place. A listing of
// every snapshot says the damaged one and goes on.
func TestADamagedNewestTagIsNeverPassedOver(t *testing.T) {
	p := pulledSnapshot(t)
	if _, _, err := storage.ReserveRun(storage.NewZone(p.snapshot), p.session); err != nil {
		t.Fatal(err)
	}
	newer := storage.Stamp(time.Now().UTC().Add(time.Second))
	if err := os.WriteFile(filepath.Join(filepath.Dir(p.snapshot), newer), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var code int
	var said string
	out := stdoutOf(t, func() {
		said = stderrOf(t, func() {
			code = cmdJudge([]string{"-config", pulledConfig(t, p), "-list", "-remote-conversation", "prod/" + p.conversation})
		})
	})
	if code != 0 || !strings.Contains(out, filepath.Base(p.snapshot)+" run 1") || !strings.Contains(said, newer) {
		t.Fatalf("every snapshot: exit %d: %s %s", code, out, said)
	}
	out = stdoutOf(t, func() {
		_ = stderrOf(t, func() {
			code = cmdJudge([]string{"-config", pulledConfig(t, p), "-list", "-remote-conversation", "prod/" + p.conversation + ":latest"})
		})
	})
	if code != 1 || strings.Contains(out, filepath.Base(p.snapshot)) {
		t.Fatalf("a damaged newest tag: exit %d: %s", code, out)
	}
	h := pulledHandler(t, p)
	if rec := get(t, h, "/api/c/remote:prod:"+p.conversation+"/view"); rec.Code == http.StatusOK {
		t.Fatalf("the page read past a damaged newest tag: %d", rec.Code)
	}
}

// A judge of a pulled conversation sweeps the copies here first, as a pull
// does: an expired snapshot of any remote is removed, and a conversation
// another command holds is said and passed over.
func TestARemoteSelectorSweepsFirst(t *testing.T) {
	p := pulledSnapshot(t)
	cfg := filepath.Join(t.TempDir(), "asz.yaml")
	if err := os.WriteFile(cfg, []byte("storage:\n  root: "+p.root+"\nremote_snapshot_ttl: 2s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tag, err := storage.ParseTag(filepath.Base(p.snapshot))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(tag.Add(2100 * time.Millisecond)))
	// Another conversation, held by another command.
	other := filepath.Join(p.root, storage.RemoteDir, "prod", "held")
	if err := os.MkdirAll(filepath.Join(other, storage.PullDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := storage.MakeLockFile(filepath.Join(other, storage.PullDir), nil); err != nil {
		t.Fatal(err)
	}
	lock, err := storage.LockOwnedWait(context.Background(), filepath.Join(other, storage.PullDir))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Unlock() }()
	var said string
	_ = stdoutOf(t, func() {
		said = stderrOf(t, func() {
			_ = cmdJudge([]string{"-config", cfg, "-list", "-remote-conversation", "prod/" + p.conversation})
		})
	})
	if _, err := os.Lstat(p.snapshot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the expired snapshot stayed: %v", err)
	}
	if !strings.Contains(said, "prod/held is held by another command") {
		t.Fatalf("the held conversation was not said: %s", said)
	}
}

// The selected conversation's own controls that do not read fail the
// command though no snapshot of it is left: its sweep's failure is the
// command's.
func TestASweepFailureOfTheSelectedConversationFails(t *testing.T) {
	p := pulledSnapshot(t)
	cfg := filepath.Join(t.TempDir(), "asz.yaml")
	if err := os.WriteFile(cfg, []byte("storage:\n  root: "+p.root+"\nremote_snapshot_ttl: 2s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tag, err := storage.ParseTag(filepath.Base(p.snapshot))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(tag.Add(2100 * time.Millisecond)))
	list := func() (int, string) {
		var code int
		var said string
		_ = stdoutOf(t, func() {
			said = stderrOf(t, func() {
				code = cmdJudge([]string{"-config", cfg, "-list", "-remote-conversation", "prod/" + p.conversation})
			})
		})
		return code, said
	}
	// The first sweep removes the expired snapshot.
	if code, said := list(); code != 0 {
		t.Fatalf("the first listing: exit %d: %s", code, said)
	}
	if _, err := os.Lstat(p.snapshot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the expired snapshot stayed: %v", err)
	}
	outer := filepath.Join(filepath.Dir(p.snapshot), storage.CopyMarker)
	if err := os.Chmod(outer, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outer, []byte("{broken"), 0o444); err != nil {
		t.Fatal(err)
	}
	if code, said := list(); code != 1 {
		t.Fatalf("a damaged conversation with no snapshot: exit %d: %s", code, said)
	}
}

// A lease this machine cannot write fails a run listing: it is never taken
// for a damaged snapshot and passed over.
func TestARunListingFailsOnALeaseItCannotWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a directory's mode does not refuse a write there")
	}
	p := pulledSnapshot(t)
	leases := filepath.Join(p.snapshot, storage.LeasesDir)
	if err := os.Chmod(leases, 0o555); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(leases, 0o755) }()
	var code int
	_ = stdoutOf(t, func() {
		_ = stderrOf(t, func() {
			code = cmdJudge([]string{"-config", pulledConfig(t, p), "-list", "-remote-conversation", "prod/" + p.conversation})
		})
	})
	if code != 1 {
		t.Fatalf("a lease that cannot be written: exit %d", code)
	}
}
