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

//go:build !windows

package main

import (
	"context"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// A pull interrupted once its snapshot was published, as its lease is let
// go, starts no judging: the command fails, no run is made, and the
// snapshot's lease is let go. The interrupt is a signal the test sends
// itself, which Windows does not have.
func TestAnInterruptedPullJudgesNothing(t *testing.T) {
	root, ids := pulledRoot(t, 1)
	cfg, local := pullsOf(t, root, func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/done") {
			return false
		}
		if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
			t.Error(err)
		}
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
		return true
	})
	f, err := os.OpenFile(cfg, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("judge:\n  evaluators:\n    none:\n      kind: cli\n      program: claude\n      command: " + os.DevNull +
		"\n  plan:\n    - stage: targets\n      evaluator: none\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	var code int
	_ = stdoutOf(t, func() {
		_ = stderrOf(t, func() {
			code = cmdJudge([]string{"-config", cfg, "-pull", "-remote-conversation", "prod/" + ids[0]})
		})
	})
	if code == 0 {
		t.Fatal("an interrupted pull was judged")
	}
	dir := filepath.Join(local, storage.RemoteDir, "prod", ids[0])
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch d.Name() {
		case storage.JudgmentsDir:
			if items, _ := os.ReadDir(path); len(items) != 0 {
				t.Errorf("a run was made: %s", path)
			}
		case storage.LeasesDir:
			if items, _ := os.ReadDir(path); len(items) != 0 {
				t.Errorf("a lease was left: %s", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// An interrupt while the startup sweep waits starts nothing after it: the
// sweep, once let through, fails the command, and no listing or judging
// follows.
func TestAnInterruptedSweepStartsNothing(t *testing.T) {
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
	// The sweep waits for this gate to remove the expired snapshot.
	gate, err := storage.LockOwnedWait(context.Background(), filepath.Join(filepath.Dir(p.snapshot), storage.AdmitDir))
	if err != nil {
		t.Fatal(err)
	}
	ended := make(chan int, 1)
	var printed string
	go func() {
		var code int
		printed = stdoutOf(t, func() {
			_ = stderrOf(t, func() {
				code = cmdJudge([]string{"-config", cfg, "-list", "-remote-conversation", "prod/" + p.conversation})
			})
		})
		ended <- code
	}()
	time.Sleep(500 * time.Millisecond)
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if err := gate.Unlock(); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-ended:
		if code == 0 || strings.Contains(printed, "judge runs") {
			t.Fatalf("an interrupted sweep went on: exit %d: %s", code, printed)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the command did not end")
	}
}

// An interrupt while the admission of a snapshot waits starts nothing after
// it: the admission, once let through, is let go, and the command fails.
// Another command holds the conversation, so the sweep passes over it
// without waiting, and the admission waits for its gate. It is so for the
// selected snapshot and for a listing of every snapshot.
func TestAnInterruptedAdmissionStartsNothing(t *testing.T) {
	for name, suffix := range map[string]string{"the selected snapshot": ":latest", "every snapshot": ""} {
		t.Run(name, func(t *testing.T) { interruptedAdmission(t, suffix) })
	}
}

func interruptedAdmission(t *testing.T, suffix string) {
	p := pulledSnapshot(t)
	held, err := storage.LockOwnedWait(context.Background(), filepath.Join(filepath.Dir(p.snapshot), storage.PullDir))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Unlock() }()
	gate, err := storage.LockOwnedWait(context.Background(), filepath.Join(filepath.Dir(p.snapshot), storage.AdmitDir))
	if err != nil {
		t.Fatal(err)
	}
	ended := make(chan int, 1)
	var printed, said string
	go func() {
		var code int
		printed = stdoutOf(t, func() {
			said = stderrOf(t, func() {
				code = cmdJudge([]string{"-config", pulledConfig(t, p), "-list", "-remote-conversation", "prod/" + p.conversation + suffix})
			})
		})
		ended <- code
	}()
	time.Sleep(500 * time.Millisecond)
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if err := gate.Unlock(); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-ended:
		if code == 0 || strings.Contains(printed, "judge runs") || !strings.Contains(said, "stopped") {
			t.Fatalf("an interrupted admission went on: exit %d: %s %s", code, printed, said)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the command did not end")
	}
	if got := p.leases(t); len(got) != 0 {
		t.Fatalf("a lease was left: %v", got)
	}
}
