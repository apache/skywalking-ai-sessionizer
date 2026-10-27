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
	"crypto/sha256"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
)

// linkTree mirrors from into to with every regular file a hard link, as a pull
// server's frozen snapshot shares a live session's files.
func linkTree(t *testing.T, from, to string) {
	t.Helper()
	if err := filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		if d.Name() == ".lock" {
			return nil
		}
		return os.Link(path, dest)
	}); err != nil {
		t.Fatal(err)
	}
}

// fileState is what a planted file must keep: its bytes and its mode.
type fileState struct {
	sum  [sha256.Size]byte
	mode fs.FileMode
}

func treeState(t *testing.T, root string, dirs ...string) map[string]fileState {
	t.Helper()
	state := map[string]fileState{}
	for _, dir := range dirs {
		if err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			state[rel] = fileState{sha256.Sum256(data), info.Mode()}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return state
}

// Every production pass leaves _remote and a pull server's _pull alone: it
// reads, sends, copies, removes and retires nothing there, even where they
// hold a copy of a live session with the same identifiers and sequences.
// Planted at the top of each, a session's shape is the hardest case for a
// listing; planted where snapshots keep it, the server's copy shares the live
// session's files through hard links, which keep their bytes and modes while
// retention removes and then retires the live session.
func TestProductionPassesLeavePullTreesAlone(t *testing.T) {
	root, b := scenarioRoot(t, "provider-bodies-long.yaml")
	if err := os.RemoveAll(filepath.Join(root, storage.ScenarioDir)); err != nil {
		t.Fatal(err)
	}
	ads := append(bothAdapters(root, ""), config.Adapter{Name: config.AdapterClaudeCodeProvider, Enabled: true,
		SourceRoot: filepath.Join(root, "_source", scenario.ProviderBodyDir)})
	rcv := receiver(t)
	ref := pipelineOver(t, root, ads)
	sendingTo(t, ref, rcv)
	pass := func() {
		t.Helper()
		stdoutOf(t, func() {
			if err := ref.pass(); err != nil {
				t.Fatal(err)
			}
		})
	}
	pass()
	session := filepath.Join(root, b.Session)
	const tag, later = "20261025T103200.000000000Z", "20261025T103201.000000000Z"
	copyTree(t, session, filepath.Join(root, storage.RemoteDir))
	copyTree(t, session, filepath.Join(root, storage.PullDir))
	copyTree(t, session, filepath.Join(root, storage.RemoteDir, "prod", b.Session, tag, b.Session))
	linkTree(t, session, filepath.Join(root, storage.PullDir, "conversations", b.Session, tag, b.Session))
	before := treeState(t, root, storage.RemoteDir, storage.PullDir)

	// Collect, parse, derive metrics and push again, and run the commands that
	// read the whole root. Repack needs a root retention has not touched. The
	// provider collector lists the root's sessions when it rebuilds its state,
	// so the state is taken away first.
	if err := os.Remove(filepath.Join(root, "_provider", "seen.state")); err != nil {
		t.Fatal(err)
	}
	pass()
	cfg := config.Default()
	cfg.Storage.Root = root
	was := positional
	t.Cleanup(func() { positional = was })
	positional = nil
	for name, run := range map[string]func() error{
		"index":  func() error { return cmdIndex(cfg, config.Adapter{}, false) },
		"verify": func() error { return cmdVerify(cfg, config.Adapter{}, false) },
	} {
		var err error
		out := stdoutOf(t, func() { err = run() })
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(out, storage.RemoteDir) || strings.Contains(out, storage.PullDir) {
			t.Fatalf("%s named a pull tree:\n%s", name, out)
		}
		if name == "verify" && !strings.Contains(out, "checked 1 session(s),") {
			t.Fatalf("verify checked more than the live session:\n%s", out)
		}
	}
	dest := filepath.Join(t.TempDir(), "repacked")
	positional = []string{dest}
	var repackErr error
	out := stdoutOf(t, func() { repackErr = cmdRepack(cfg, ads[0], false) })
	if repackErr != nil {
		t.Fatalf("repack: %v\n%s", repackErr, out)
	}
	for _, dir := range []string{storage.RemoteDir, storage.PullDir} {
		if _, err := os.Lstat(filepath.Join(dest, dir)); !os.IsNotExist(err) {
			t.Fatalf("repack copied %s: %v", dir, err)
		}
	}
	// The page lists and reads the live conversation only.
	pageOnly(t, root, b.Session)

	// Remove the landed files, which unlinks them and writes skeletons in
	// their place.
	ref.retain(time.Nanosecond, 0)
	pass()
	// Removal kept the provider pieces the bodies cite in the store. A second
	// snapshot shares the store and the skeletons, then retirement deletes the
	// whole session.
	if _, err := os.Stat(filepath.Join(session, "provider")); err != nil {
		t.Fatalf("removal kept no provider store to share: %v", err)
	}
	second := filepath.Join(storage.PullDir, "conversations", b.Session, later)
	linkTree(t, session, filepath.Join(root, second, b.Session))
	shared := treeState(t, root, second)
	ref.retain(time.Nanosecond, time.Nanosecond)
	pass()
	if _, err := os.Stat(session); !os.IsNotExist(err) {
		t.Fatalf("retirement left the live session: %v", err)
	}

	noChainFor(t, root, storage.RemoteDir, storage.PullDir)
	for _, state := range []string{"push.state", filepath.Join("_metrics", "metrics.state"), filepath.Join("_provider", "seen.state")} {
		data, err := os.ReadFile(filepath.Join(root, state))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if strings.Contains(string(data), storage.RemoteDir) || strings.Contains(string(data), storage.PullDir) {
			t.Fatalf("%s names a pull tree", state)
		}
	}
	after := treeState(t, root, storage.RemoteDir, storage.PullDir)
	for _, planted := range []map[string]fileState{before, shared} {
		for path, was := range planted {
			if now, ok := after[path]; !ok || now != was {
				t.Errorf("%s changed: %+v, then %+v", path, was, now)
			}
		}
	}
	if len(after) != len(before)+len(shared) {
		t.Errorf("%d files in the pull trees, want %d", len(after), len(before)+len(shared))
	}
}

// pageOnly checks that the page lists and serves the live session's
// conversation and nothing of the pull trees.
func pageOnly(t *testing.T, root, session string) {
	t.Helper()
	srv := httptest.NewServer(view.New(storage.NewZone(root), nil).Handler())
	defer srv.Close()
	get := func(path string) (int, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(body)
	}
	status, list := get("/api/conversations")
	if status != http.StatusOK || !strings.Contains(list, session) ||
		strings.Contains(list, storage.RemoteDir) || strings.Contains(list, storage.PullDir) {
		t.Fatalf("the page's list: %d\n%s", status, list)
	}
	for _, part := range []string{"/view"} {
		if status, _ := get("/api/c/" + session + part); status != http.StatusOK {
			t.Fatalf("the live conversation's %s: %d", part, status)
		}
		for _, dir := range []string{storage.RemoteDir, storage.PullDir} {
			if status, _ := get("/api/c/" + dir + part); status == http.StatusOK {
				t.Fatalf("the page served %s as a conversation", dir)
			}
		}
	}
}

// A scenario root's pipeline removes each session once it is all sent. It
// removes nothing of _remote or a pull server's _pull, even where they hold a
// copy of the very session it removes, landed by another build of the same
// scenario with the same identifiers and sequences.
func TestScenarioRemovalLeavesPullTreesAlone(t *testing.T) {
	plain, built := scenarioRoot(t, "growth.yaml")
	if err := os.RemoveAll(filepath.Join(plain, storage.ScenarioDir)); err != nil {
		t.Fatal(err)
	}
	landing := pipelineOver(t, plain, bothAdapters(plain, ""))
	stdoutOf(t, func() {
		if err := landing.pass(); err != nil {
			t.Fatal(err)
		}
	})
	copied := filepath.Join(plain, built.Session)

	root, b := scenarioRoot(t, "growth.yaml")
	if b.Session != built.Session {
		t.Fatalf("two builds of one scenario landed %s and %s", built.Session, b.Session)
	}
	const tag = "20261025T103200.000000000Z"
	copyTree(t, copied, filepath.Join(root, storage.RemoteDir))
	copyTree(t, copied, filepath.Join(root, storage.PullDir))
	copyTree(t, copied, filepath.Join(root, storage.RemoteDir, "prod", b.Session, tag, b.Session))
	copyTree(t, copied, filepath.Join(root, storage.PullDir, "conversations", b.Session, tag, b.Session))
	before := treeState(t, root, storage.RemoteDir, storage.PullDir)
	ref := pipelineOver(t, root, bothAdapters(root, ""))
	rcv := receiver(t)
	sendingTo(t, ref, rcv)
	for i := 0; i < 3; i++ {
		stdoutOf(t, func() {
			if err := ref.pass(); err != nil {
				t.Fatal(err)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(root, b.Session)); !os.IsNotExist(err) {
		t.Fatalf("the scenario's removal did not remove the sent session: %v", err)
	}
	noChainFor(t, root, storage.RemoteDir, storage.PullDir)
	after := treeState(t, root, storage.RemoteDir, storage.PullDir)
	for path, was := range before {
		if now, ok := after[path]; !ok || now != was {
			t.Errorf("%s changed: %+v, then %+v", path, was, now)
		}
	}
	if len(after) != len(before) {
		t.Fatalf("%d files in the pull trees, want %d", len(after), len(before))
	}
}

// noChainFor fails when a conversation chain was built for any of names.
func noChainFor(t *testing.T, root string, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(root, "_conversations", name)); !os.IsNotExist(err) {
			t.Fatalf("a chain was built for %s: %v", name, err)
		}
	}
}
