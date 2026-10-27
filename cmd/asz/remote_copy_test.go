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
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// markCopy makes dir hold the copy marker, as a pull's admission does.
func markCopy(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, storage.CopyMarker), []byte("{}\n"), 0o444); err != nil {
		t.Fatal(err)
	}
}

// Copies pulled from a remote keep the remote's session ids and sequence
// numbers. Every command that writes to its root refuses a root holding them,
// before it writes anything there, a lock directory or an inbox included.
func TestWritersRefuseACopy(t *testing.T) {
	was, saved := rootWait, positional
	rootWait = 100 * time.Millisecond
	t.Cleanup(func() { rootWait, positional = was, saved })
	root := t.TempDir()
	markCopy(t, root)
	cfg := &config.Config{}
	cfg.Storage.Root = root
	cfg.Export.OTLP.Endpoint, cfg.Export.OTLP.ServiceName = "127.0.0.1:1", "Copy Test"
	receiver := config.Adapter{Name: config.AdapterLangSmithIngest, Listen: "127.0.0.1:0"}
	for name, run := range map[string]func() error{
		"collect, at its start": func() error { return admitCommand("collect", cfg) },
		"server, at its start":  func() error { return admitCommand("server", cfg) },
		"parse":                 func() error { return cmdParse(cfg, config.Adapter{}, false) },
		"push":                  func() error { return cmdPush(cfg, config.Adapter{}, false) },
		"repack, from it": func() error {
			positional = []string{filepath.Join(t.TempDir(), "dest")}
			return cmdRepack(cfg, config.Adapter{}, false)
		},
		"the LangSmith receiver": func() error { return startLangSmith(cfg, receiver) },
	} {
		var err error
		stdoutOf(t, func() { err = run() })
		if !errors.Is(err, storage.ErrCopy) {
			t.Fatalf("%s over a root holding copies returned %v", name, err)
		}
	}
	// A pass of a running pipeline refuses it too, should the root become a
	// copy after the command started.
	ref := pipelineOver(t, root, bothAdapters(root, ""))
	var err error
	stdoutOf(t, func() { err = ref.pass() })
	if !errors.Is(err, storage.ErrCopy) {
		t.Fatalf("a pass over a root holding copies returned %v", err)
	}
	items, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Name() != storage.CopyMarker {
		var names []string
		for _, it := range items {
			names = append(names, it.Name())
		}
		t.Fatalf("refused commands wrote into the copy: %v", names)
	}
	// The same commands over a root with no marker are not refused for it.
	plain := t.TempDir()
	cfg.Storage.Root = plain
	for name, run := range map[string]func() error{
		"collect, at its start": func() error { return admitCommand("collect", cfg) },
		"server, at its start":  func() error { return admitCommand("server", cfg) },
	} {
		if err := run(); err != nil {
			t.Fatalf("%s over a plain root returned %v", name, err)
		}
	}
}

// A pull keeps its copies under _remote/ in the storage root. The listings
// of the root's sessions pass over it, so asz index and asz verify never
// take it for a session, even laid out as one.
func TestIndexAndVerifyPassOverRemote(t *testing.T) {
	root, _ := scenarioRoot(t, "fixture.yaml")
	collector := claudecode.New(filepath.Join(root, "_source"), storage.NewZone(root), 0)
	stats, err := collector.CollectAll(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.Errors) != 0 {
		t.Fatalf("collecting the fixture: %v", stats.Errors)
	}
	sessions, err := sessionDirs(root)
	if err != nil || len(sessions) == 0 {
		t.Fatalf("the fixture landed no session: %v", err)
	}
	// The hardest cases for a listing: _remote/ and a pull server's _pull/
	// holding what a session holds, at their top and where snapshots keep it.
	for _, dir := range []string{
		storage.RemoteDir,
		filepath.Join(storage.RemoteDir, "prod", "c1", "20261025T103200.000000000Z", sessions[0]),
		storage.PullDir,
		filepath.Join(storage.PullDir, "conversations", "c1", "20261025T103200.000000000Z", sessions[0]),
	} {
		copyTree(t, filepath.Join(root, sessions[0]), filepath.Join(root, dir))
	}
	cfg := &config.Config{}
	cfg.Storage.Root = root
	was := positional
	positional = nil
	t.Cleanup(func() { positional = was })

	out := stdoutOf(t, func() {
		if err := cmdVerify(cfg, config.Adapter{}, false); err != nil {
			t.Fatal(err)
		}
	})
	if want := fmt.Sprintf("checked %d session(s),", len(sessions)); !strings.Contains(out, want) {
		t.Fatalf("asz verify did not pass over %s; want %q in:\n%s", storage.RemoteDir, want, out)
	}
	out = stdoutOf(t, func() {
		if err := cmdIndex(cfg, config.Adapter{}, false); err != nil {
			t.Fatal(err)
		}
	})
	for _, dir := range []string{storage.RemoteDir, storage.PullDir} {
		if strings.Contains(out, dir) {
			t.Fatalf("asz index listed %s as a session:\n%s", dir, out)
		}
	}
	for _, id := range sessions {
		if !strings.Contains(out, id) {
			t.Fatalf("asz index left out the real session %s:\n%s", id, out)
		}
	}
}

func copyTree(t *testing.T, from, to string) {
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
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0o644)
	}); err != nil {
		t.Fatal(err)
	}
}

// A LangSmith receiver starting on an empty root and a pull admitting the same
// directory race for its root lock: exactly one proceeds. The receiver that
// wins has made its inbox, which the pull then refuses; the pull that wins has
// written its marker, which the receiver then refuses.
func TestReceiverAndBootstrapRaceForAnEmptyDirectory(t *testing.T) {
	was := rootWait
	rootWait = 2 * time.Second
	t.Cleanup(func() { rootWait = was })
	for i := 0; i < 10; i++ {
		root := t.TempDir()
		dir := filepath.Join(root, storage.RemoteDir, "prod", "c1")
		cfg := &config.Config{}
		cfg.Storage.Root = dir
		receiver := config.Adapter{Name: config.AdapterLangSmithIngest, Listen: "127.0.0.1:0"}
		start := make(chan struct{})
		var receiverErr, bootstrapErr error
		var pull *storage.SessionLock
		done := make(chan struct{}, 2)
		go func() {
			<-start
			stdoutOf(t, func() { receiverErr = startLangSmith(cfg, receiver) })
			done <- struct{}{}
		}()
		go func() {
			<-start
			_, pull, bootstrapErr = storage.BootstrapConversation(context.Background(), root, "prod", "c1", nil)
			done <- struct{}{}
		}()
		close(start)
		<-done
		<-done
		if pull != nil {
			if err := pull.Unlock(); err != nil {
				t.Fatal(err)
			}
		}
		_, inboxErr := os.Lstat(filepath.Join(dir, "_langsmith"))
		_, markerErr := os.Lstat(filepath.Join(dir, storage.CopyMarker))
		switch {
		case receiverErr == nil && bootstrapErr != nil:
			if inboxErr != nil || markerErr == nil {
				t.Fatalf("the receiver won, but the inbox is %v and the marker %v", inboxErr, markerErr)
			}
		case receiverErr != nil && bootstrapErr == nil:
			if !errors.Is(receiverErr, storage.ErrCopy) || markerErr != nil || inboxErr == nil {
				t.Fatalf("the pull won, but the receiver said %v, the inbox is %v and the marker %v", receiverErr, inboxErr, markerErr)
			}
		default:
			t.Fatalf("receiver %v and bootstrap %v: exactly one must proceed", receiverErr, bootstrapErr)
		}
	}
}
