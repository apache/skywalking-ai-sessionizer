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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// Every command that writes to a storage root holds it for a pass. A
// watching pass that finds it held does nothing this period; a single pass
// waits, and says so when it is not released; once it is, the pass works.
func TestAPassTakesTheRootLock(t *testing.T) {
	root, _ := scenarioRoot(t, "fixture.yaml")
	held, err := storage.LockRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	ref := pipelineOver(t, root, bothAdapters(root, ""))
	out := stdoutOf(t, func() {
		for range 2 {
			if err := ref.pass(); err != nil {
				t.Fatalf("a watching pass over a held root: %v", err)
			}
		}
	})
	if n := strings.Count(out, "waiting  : another command holds"); n != 1 {
		t.Fatalf("two watching passes over a held root said they wait %d times:\n%s", n, out)
	}
	if sessions, _ := sessionDirs(root); len(sessions) != 0 {
		t.Fatalf("a pass over a held root landed %d session(s)", len(sessions))
	}
	ref.last, ref.scenarioWait = true, 100*time.Millisecond
	if err := ref.pass(); err == nil || !strings.Contains(err.Error(), "holds this storage root") {
		t.Fatalf("a single pass over a held root returned %v", err)
	}
	if err := held.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := ref.pass(); err != nil {
		t.Fatal(err)
	}
	if sessions, _ := sessionDirs(root); len(sessions) == 0 {
		t.Fatal("once the root was released the pass landed nothing")
	}
}

// asz parse and asz push wait for a pass that holds the root, and say so when
// it is not released.
func TestParseAndPushWaitForTheRoot(t *testing.T) {
	was := rootWait
	rootWait = 100 * time.Millisecond
	t.Cleanup(func() { rootWait = was })
	root := t.TempDir()
	held, err := storage.LockRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Unlock()
	cfg := &config.Config{}
	cfg.Storage.Root = root
	cfg.Export.OTLP.Endpoint, cfg.Export.OTLP.ServiceName = "127.0.0.1:1", "Lock Test"
	for name, run := range map[string]func() error{
		"parse": func() error { return cmdParse(cfg, config.Adapter{}, false) },
		"push":  func() error { return cmdPush(cfg, config.Adapter{}, false) },
	} {
		var err error
		stdoutOf(t, func() { err = run() })
		if err == nil || !strings.Contains(err.Error(), "another command held this storage root") {
			t.Fatalf("%s over a held root returned %v", name, err)
		}
	}
}

// asz repack holds the root it reads, waiting for it, and the root it
// writes, without waiting: a command that holds a new or empty root is
// writing into it. A destination another command wrote into while the
// repack waited is refused, as one that was populated from the start.
func TestRepackHoldsBothRoots(t *testing.T) {
	was, saved := rootWait, positional
	rootWait = 100 * time.Millisecond
	t.Cleanup(func() { rootWait, positional = was, saved })
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "dest")
	cfg := &config.Config{}
	cfg.Storage.Root = src
	positional = []string{dst}
	repack := func() error {
		var err error
		stdoutOf(t, func() { err = cmdRepack(cfg, config.Adapter{}, false) })
		return err
	}

	held, err := storage.LockRoot(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := repack(); err == nil || !strings.Contains(err.Error(), "another command held this storage root") {
		t.Fatalf("a repack from a held root returned %v", err)
	}
	_ = held.Unlock()

	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if held, err = storage.LockRoot(dst); err != nil {
		t.Fatal(err)
	}
	if err := repack(); err == nil || !strings.Contains(err.Error(), "another command is writing") {
		t.Fatalf("a repack into a held root returned %v", err)
	}
	_ = held.Unlock()

	// What only the root lock left behind is not content.
	if err := repack(); err != nil {
		t.Fatalf("a repack into a root holding only the lock's directory: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dst, "sess1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := repack(); err == nil || !strings.Contains(err.Error(), "is not empty") {
		t.Fatalf("a repack into a populated root returned %v", err)
	}

	// A destination inside the root would be read as a session of it.
	nested := filepath.Join(src, "repacked")
	positional = []string{nested}
	if err := repack(); err == nil || !strings.Contains(err.Error(), "inside the storage root") {
		t.Fatalf("a repack into a directory inside the root returned %v", err)
	}
	if pathExists(nested) {
		t.Fatal("a refused repack made its destination")
	}
	// The same directory reached through a symbolic link to the root.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(src, link); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	positional = []string{filepath.Join(link, "repacked")}
	if err := repack(); err == nil || !strings.Contains(err.Error(), "inside the storage root") {
		t.Fatalf("a repack into the root through a symbolic link returned %v", err)
	}
	if pathExists(nested) {
		t.Fatal("a refused repack made its destination through the link")
	}
	positional = []string{link}
	if err := repack(); err == nil || !strings.Contains(err.Error(), "storage root itself") {
		t.Fatalf("a repack into the root itself through a symbolic link returned %v", err)
	}
}

// asz scenario build writes a root a pipeline may be watching, so each
// build holds that root, and waits for a pass that holds it.
func TestScenarioBuildWaitsForTheRoot(t *testing.T) {
	was := rootWait
	rootWait = 100 * time.Millisecond
	t.Cleanup(func() { rootWait = was })
	out := t.TempDir()
	held, err := storage.LockRoot(out)
	if err != nil {
		t.Fatal(err)
	}
	build := func() (int, string) {
		var code int
		stderr := written(t, &os.Stderr, func() {
			stdoutOf(t, func() {
				code = cmdScenario([]string{"build", "--format", "sd", "--out", out, filepath.Join("..", "..", "tests", "scenarios", "fixture.yaml")})
			})
		})
		return code, stderr
	}
	if code, stderr := build(); code == 0 || !strings.Contains(stderr, "another command held this storage root") {
		t.Fatalf("a build into a held root exited %d: %s", code, stderr)
	}
	if sessions, _ := sessionDirs(out); len(sessions) != 0 {
		t.Fatalf("a build into a held root wrote %d session(s)", len(sessions))
	}
	_ = held.Unlock()
	if code, stderr := build(); code != 0 {
		t.Fatalf("a build into a free root exited %d: %s", code, stderr)
	}
	if sessions, _ := sessionDirs(out); len(sessions) == 0 {
		t.Fatal("a build into a free root wrote nothing")
	}
}
