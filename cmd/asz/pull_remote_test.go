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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/client"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/format"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/server"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// pulledRoot is a root of n conversations, a day apart, parsed whole.
func pulledRoot(t *testing.T, n int) (root string, ids []string) {
	t.Helper()
	root = t.TempDir()
	for i := 1; i <= n; i++ {
		text := fmt.Sprintf("session: %08x-0000-4000-8000-%012x\ntitle: conversation %d\nsteps:\n"+
			"  - input: build the parser\n  - call: {text: The parser is built.}\n", i, i, i)
		path := filepath.Join(t.TempDir(), "scenario.yaml")
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		sc, err := scenario.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		at := time.Date(2026, 10, i, 0, 0, 0, 0, time.UTC)
		b, err := scenario.Build(sc, scenario.FormatSD, root, scenario.Options{At: at})
		if err != nil {
			t.Fatal(err)
		}
		z := storage.NewZone(root)
		for {
			r, err := parse.Session(z, parse.Options{Conversation: b.Session, Session: b.Session})
			if err != nil {
				t.Fatal(err)
			}
			if !r.Changed() || !r.More {
				break
			}
		}
		ids = append(ids, b.Session)
	}
	return root, ids
}

// pullsOf serves root's pulls with one token, which the returned
// configuration names as remote prod, its storage root a new one. hook,
// when set, sees each request first and answers it when it returns true.
func pullsOf(t *testing.T, root string, hook func(http.ResponseWriter, *http.Request) bool) (cfg, local string) {
	t.Helper()
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
	pulls := server.New(server.Options{Root: root, TTL: time.Hour, Every: time.Hour, Guard: guard})
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan error, 1)
	go func() { ran <- pulls.Run(ctx) }()
	inner := guard.Protect(http.NotFoundHandler(), pulls.Handler())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hook != nil && hook(w, r) {
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		srv.Close()
		cancel()
		if err := <-ran; err != nil {
			t.Error(err)
		}
	})
	t.Setenv("ASZ_TEST_PULL_TOKEN", token)
	local = t.TempDir()
	cfg = filepath.Join(t.TempDir(), "asz.yaml")
	if err := os.WriteFile(cfg, []byte("storage:\n  root: "+local+"\nremotes:\n  - name: prod\n    url: "+srv.URL+
		"\n    token_env: ASZ_TEST_PULL_TOKEN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg, local
}

// pullCommand runs asz pull with args, and returns its exit status and
// what it printed.
func pullCommand(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var code int
	var stderr string
	stdout := stdoutOf(t, func() {
		stderr = stderrOf(t, func() { code = cmdPull(args) })
	})
	return code, stdout, stderr
}

// The command a listing prints for its next page asks for it with every
// filter this page had, and the configuration it read: followed as it is,
// it lists the next page.
func TestTheNextPagesCommandKeepsItsFilters(t *testing.T) {
	root, ids := pulledRoot(t, 3)
	cfg, _ := pullsOf(t, root, nil)
	code, out, errs := pullCommand(t, "-config", cfg, "-list", "-remote", "prod", "-limit", "1", "-min-talks", "1")
	if code != 0 || !strings.Contains(out, ids[2]) {
		t.Fatalf("exit %d: %s %s", code, out, errs)
	}
	_, next, ok := strings.Cut(out, "more from prod: ")
	if !ok {
		t.Fatalf("no next page: %s", out)
	}
	words := shellFields(next)
	if len(words) < 3 || words[0] != "asz" || words[1] != "pull" {
		t.Fatalf("the next page's command: %s", next)
	}
	code, out, errs = pullCommand(t, words[2:]...)
	if code != 0 || !strings.Contains(out, ids[1]) || strings.Contains(out, ids[2]) {
		t.Fatalf("following %q: exit %d: %s %s", next, code, out, errs)
	}
}

// shellFields reads a line back into words as a shell reads what shellWord
// writes: words between spaces, single quotes around a quoted one, and a
// backslash before a quote outside them. A Windows path is quoted.
func shellFields(line string) []string {
	var words []string
	var word []rune
	quoted, escaped, inWord := false, false, false
	for _, r := range line {
		switch {
		case escaped:
			word, escaped = append(word, r), false
		case quoted && r == '\'':
			quoted = false
		case quoted:
			word = append(word, r)
		case r == '\'':
			quoted, inWord = true, true
		case r == '\\':
			escaped, inWord = true, true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if inWord {
				words, word, inWord = append(words, string(word)), nil, false
			}
		default:
			word, inWord = append(word, r), true
		}
	}
	if inWord {
		words = append(words, string(word))
	}
	return words
}

// A merged listing is ordered by the instant each row's time names: a
// fraction of a second later is later, an offset is read as the same
// instant, and a row with no time comes last.
func TestAMergedListingIsOrderedByInstant(t *testing.T) {
	row := func(remote, id, at string) listedRow {
		return listedRow{remote: remote, Row: format.Row{ID: id, SessionThroughTime: at}}
	}
	rows := []listedRow{
		row("a", "none", ""),
		row("a", "whole", "2026-10-26T10:00:00Z"),
		row("b", "fraction", "2026-10-26T10:00:00.1Z"),
		row("b", "offset", "2026-10-26T12:00:00+02:00"),
		row("a", "later", "2026-10-26T11:00:00+01:00"),
	}
	newestFirst(rows)
	var got []string
	for _, r := range rows {
		got = append(got, r.remote+"/"+r.ID)
	}
	if want := "b/fraction a/later a/whole b/offset a/none"; strings.Join(got, " ") != want {
		t.Fatalf("%v, want %s", got, want)
	}
}

// Damage here fails a listing, though it goes on: a copy the startup sweep
// cannot read, and a listed conversation whose snapshot here does not
// admit a reader, its local column saying so.
func TestDamageHereFailsAListing(t *testing.T) {
	root, ids := pulledRoot(t, 1)
	cfg, local := pullsOf(t, root, nil)
	if code, out, errs := pullCommand(t, "-config", cfg, "prod/"+ids[0]); code != 0 {
		t.Fatalf("the pull: exit %d: %s %s", code, out, errs)
	}
	// A file where a remote's directory belongs: the sweep cannot read it.
	stray := filepath.Join(local, storage.RemoteDir, "stray")
	if err := os.WriteFile(stray, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := pullCommand(t, "-config", cfg, "-list")
	if code != 1 || !strings.Contains(out, "prod/"+ids[0]) {
		t.Fatalf("a sweep that failed: exit %d: %s %s", code, out, errs)
	}
	if err := os.Remove(stray); err != nil {
		t.Fatal(err)
	}
	listed := func(what string) {
		t.Helper()
		code, out, errs := pullCommand(t, "-config", cfg, "-list")
		if code != 1 || !strings.Contains(out, "prod/"+ids[0]) {
			t.Fatalf("%s: exit %d: %s %s", what, code, out, errs)
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "prod/"+ids[0]) && !strings.Contains(line, " ? ") {
				t.Fatalf("%s: its local column: %q", what, line)
			}
		}
	}
	// A remote's directory that is a link: its snapshots are not where
	// their path says.
	if runtime.GOOS != "windows" {
		remote := filepath.Join(local, storage.RemoteDir, "prod")
		moved := filepath.Join(local, storage.RemoteDir, "prod-real")
		if err := os.Rename(remote, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(moved, remote); err != nil {
			t.Fatal(err)
		}
		listed("a remote's directory that is a link")
		if err := os.Remove(remote); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(moved, remote); err != nil {
			t.Fatal(err)
		}
	}
	// A snapshot here whose marker does not read is said and passed over:
	// the listing still succeeds, with no snapshot here.
	dir := filepath.Join(local, storage.RemoteDir, "prod", ids[0])
	var tag string
	for _, e := range entriesOf(t, dir) {
		if _, err := storage.ParseTag(e); err == nil {
			tag = e
		}
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
	broken(filepath.Join(dir, tag, storage.CopyMarker))
	code, out, errs = pullCommand(t, "-config", cfg, "-list")
	if code != 0 || !strings.Contains(errs, tag) {
		t.Fatalf("a damaged snapshot: exit %d: %s %s", code, out, errs)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "prod/"+ids[0]) && !strings.Contains(line, " - ") {
			t.Fatalf("a damaged snapshot: its local column: %q", line)
		}
	}
	// The conversation's own marker that does not read fails the listing.
	broken(filepath.Join(dir, storage.CopyMarker))
	listed("a damaged conversation")
}

func entriesOf(t *testing.T, dir string) []string {
	t.Helper()
	items, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, it := range items {
		out = append(out, it.Name())
	}
	return out
}

// A pull stopped after its snapshot is published lets its lease here go
// within the stop's grace: with the conversation's gate held by another
// command, the release stops when the pull does, and leaves the lease for a
// later sweep.
func TestAStoppedPullsReleaseEndsWithTheGrace(t *testing.T) {
	root, ids := pulledRoot(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var local string
	var gate *storage.SessionLock
	held := make(chan error, 1)
	cfg, localRoot := pullsOf(t, root, func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/done") {
			return false
		}
		// The release of the remote's lease comes after publication: the
		// gate is taken here, and the pull stopped.
		dir, err := storage.ConversationDir(local, "prod", ids[0])
		if err == nil {
			gate, err = storage.LockOwnedWait(context.Background(), filepath.Join(dir, storage.AdmitDir))
		}
		held <- err
		cancel()
		w.WriteHeader(http.StatusNoContent)
		return true
	})
	local = localRoot
	c, err := loadConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r, err := openRemote(c, "prod", &remoteSlots{})
	if err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	go func() {
		_, _, err := pullOne(ctx, &client.Local{Root: local, TTL: time.Hour}, r, ids[0])
		ended <- err
	}()
	// A pull that fails before its release never reaches the gate.
	select {
	case err := <-held:
		if err != nil {
			t.Fatal(err)
		}
	case err := <-ended:
		t.Fatalf("the pull ended before its release: %v", err)
	}
	defer func() { _ = gate.Unlock() }()
	select {
	case err := <-ended:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the release: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the release waited for the gate past the stop")
	}
}

// A word of the next page's command is quoted when a shell would read it
// otherwise, and left as it is when not.
func TestAShellWordReadsBackAsItIs(t *testing.T) {
	for word, want := range map[string]string{
		"prod":                       "prod",
		"-min-talks":                 "-min-talks",
		"/etc/asz.yaml":              "/etc/asz.yaml",
		"":                           "''",
		"the parser":                 "'the parser'",
		"it's":                       `'it'\''s'`,
		"$HOME":                      "'$HOME'",
		"a;b":                        "'a;b'",
		"2026-10-26T10:":             "2026-10-26T10:",
		`C:\Users\RUNNER~1\asz.yaml`: `'C:\Users\RUNNER~1\asz.yaml'`,
	} {
		if got := shellWord(word); got != want {
			t.Errorf("%q: %s, want %s", word, got, want)
		}
		if back := shellFields("asz " + shellWord(word) + "\n"); len(back) != 2 || back[1] != word {
			t.Errorf("%q reads back as %q", word, back)
		}
	}
}
