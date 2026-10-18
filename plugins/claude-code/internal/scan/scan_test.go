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

package scan_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/plugins/claude-code/internal/scan"
	"github.com/apache/skywalking-ai-sessionizer/plugins/claude-code/internal/scope"
)

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func steady() func() time.Time {
	t := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	return func() time.Time {
		t = t.Add(2 * time.Second)
		return t
	}
}

func TestScanKeepsBytesAndRecordsSteps(t *testing.T) {
	ws := t.TempDir()
	write(t, filepath.Join(ws, "a.txt"), "a\n")
	write(t, filepath.Join(ws, "sub", "b.txt"), "b\n")
	write(t, filepath.Join(ws, "node_modules", "x.js"), "x")
	write(t, filepath.Join(ws, "big.bin"), string(make([]byte, 100)))
	rules, _ := scope.Compile(scope.StandardV1, nil, nil)
	r, err := scan.Open(t.TempDir(), ws)
	if err != nil {
		t.Fatal(err)
	}
	o := scan.Options{Rules: rules, SizeCap: 50, Now: steady()}
	m, step, err := r.Scan(o)
	if err != nil {
		t.Fatal(err)
	}
	if m.N != 1 || len(m.Files) != 3 || len(step.Changes) != 3 {
		t.Fatalf("first scan: n=%d files=%v step=%v", m.N, m.Files, step.Changes)
	}
	if _, ok := m.Files["node_modules/x.js"]; ok {
		t.Fatal("an excluded directory was scanned")
	}
	if b, ok := r.Content(m.Files["a.txt"].Hash); !ok || string(b) != "a\n" {
		t.Fatal("the bytes of a.txt were not kept")
	}
	if _, ok := r.Content(m.Files["big.bin"].Hash); ok {
		t.Fatal("the bytes of a file over the cap were kept")
	}

	// Change, delete, create; the unchanged file keeps its hash.
	write(t, filepath.Join(ws, "a.txt"), "A\n")
	if err := os.Remove(filepath.Join(ws, "sub", "b.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(ws, "c.txt"), "c\n")
	m2, step2, err := r.Scan(o)
	if err != nil {
		t.Fatal(err)
	}
	if m2.N != 2 || len(step2.Changes) != 3 {
		t.Fatalf("second scan: %+v", step2)
	}
	if c := step2.Changes["a.txt"]; c.Before != m.Files["a.txt"].Hash || c.After != m2.Files["a.txt"].Hash || c.Before == c.After {
		t.Fatalf("a.txt step: %+v", c)
	}
	if c := step2.Changes["sub/b.txt"]; c.After != "" || c.Before == "" {
		t.Fatalf("deleted b.txt step: %+v", c)
	}
	if c := step2.Changes["c.txt"]; c.Before != "" || c.After == "" {
		t.Fatalf("created c.txt step: %+v", c)
	}
	if m2.Files["big.bin"].Hash != m.Files["big.bin"].Hash {
		t.Fatal("an unchanged file changed its hash")
	}
	steps, err := r.Steps(0, 2)
	if err != nil || len(steps) != 2 || steps[1].N != 2 {
		t.Fatalf("steps: %v err=%v", steps, err)
	}
	head, _ := r.Head()
	if head.N != 2 {
		t.Fatalf("head %d", head.N)
	}

	// Dropping the bytes keeps the manifest, so the next scan still knows
	// what was there and starts a fresh chain.
	if err := r.DropBytes(); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Content(m2.Files["a.txt"].Hash); ok {
		t.Fatal("bytes survived the drop")
	}
	if steps, _ := r.Steps(0, 2); len(steps) != 0 {
		t.Fatal("steps survived the drop")
	}
	m3, step3, _ := r.Scan(o)
	if m3.N != 3 || len(step3.Changes) != 0 {
		t.Fatalf("after the drop: n=%d changes=%v", m3.N, step3.Changes)
	}
}

func TestScanStopsAtItsTimeCap(t *testing.T) {
	ws := t.TempDir()
	for i := 0; i < 20; i++ {
		write(t, filepath.Join(ws, "f"+string(rune('a'+i))+".txt"), "x\n")
	}
	r, _ := scan.Open(t.TempDir(), ws)
	// A clock that jumps a minute on every reading: the cap is passed at
	// the first file.
	tick := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	now := func() time.Time {
		tick = tick.Add(time.Minute)
		return tick
	}
	m, step, err := r.Scan(scan.Options{Timeout: time.Second, SizeCap: 1 << 20, Now: now})
	if !errors.Is(err, scan.ErrTimeout) {
		t.Fatalf("err %v, want the time cap", err)
	}
	if !step.Partial || m.N != 1 {
		t.Fatalf("partial=%v n=%d", step.Partial, m.N)
	}
}

// A directory or a file the scan cannot read is still there. A tool that ran
// chmod 000 on it made every file under it look deleted, with the step
// complete, and restoring access then made them look created. What cannot be
// read keeps what the last scan saw, and the step names it as not observed.
func TestWhatAScanCannotReadIsNotDeleted(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission bits do not stop this reader here")
	}
	ws := t.TempDir()
	write(t, filepath.Join(ws, "a.txt"), "a\n")
	write(t, filepath.Join(ws, "sub", "b.txt"), "b\n")
	write(t, filepath.Join(ws, "sub", "deep", "c.txt"), "c\n")
	write(t, filepath.Join(ws, "locked.txt"), "l\n")
	// Older than the scan before, so the scan takes its hash from what it
	// saw: chmod changes neither the size, the modification time nor the
	// inode it compares.
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(ws, "locked.txt"), old, old); err != nil {
		t.Fatal(err)
	}
	r, err := scan.Open(t.TempDir(), ws)
	if err != nil {
		t.Fatal(err)
	}
	o := scan.Options{SizeCap: 1 << 20, Now: steady()}
	m1, _, err := r.Scan(o)
	if err != nil || len(m1.Files) != 4 {
		t.Fatalf("first scan: %v %v", m1.Files, err)
	}
	for _, p := range []string{"sub", "locked.txt"} {
		if err := os.Chmod(filepath.Join(ws, p), 0); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(ws, "sub"), 0o755)
		_ = os.Chmod(filepath.Join(ws, "locked.txt"), 0o644)
	})
	write(t, filepath.Join(ws, "a.txt"), "A\n")
	m2, step, err := r.Scan(o)
	if err != nil {
		t.Fatal(err)
	}
	if len(step.Changes) != 1 || step.Changes["a.txt"].After == "" {
		t.Fatalf("the step records %v; want only the change to a.txt", step.Changes)
	}
	if got := strings.Join(step.Unread, " "); got != "locked.txt sub" {
		t.Fatalf("the step names %q as not read, want the directory and the file", got)
	}
	for _, p := range []string{"sub/b.txt", "sub/deep/c.txt", "locked.txt"} {
		if m2.Files[p] != m1.Files[p] {
			t.Fatalf("%s: the manifest has %+v, want what the last scan saw, %+v", p, m2.Files[p], m1.Files[p])
		}
	}
	if err := os.Chmod(filepath.Join(ws, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(ws, "locked.txt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, step, err := r.Scan(o); err != nil || len(step.Changes) != 0 || len(step.Unread) != 0 {
		t.Fatalf("access back: changes %v unread %v err %v; want nothing", step.Changes, step.Unread, err)
	}
}

func TestPendingNotes(t *testing.T) {
	r, _ := scan.Open(t.TempDir(), t.TempDir())
	if err := r.PutPending(&scan.Pending{Capture: "toolu_1", Tool: "toolu_1", Before: 3, At: "2026-09-08T10:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if err := r.PutPending(&scan.Pending{Capture: "toolu_2", Tool: "toolu_2", Before: 4, At: "2026-09-08T10:40:00Z"}); err != nil {
		t.Fatal(err)
	}
	stale, err := r.StalePending(30*time.Minute, time.Date(2026, 9, 8, 10, 45, 0, 0, time.UTC))
	if err != nil || len(stale) != 1 || stale[0].Tool != "toolu_1" {
		t.Fatalf("stale: %+v err=%v", stale, err)
	}
	p, err := r.TakePending("toolu_1")
	if err != nil || p.Before != 3 {
		t.Fatalf("take: %+v err=%v", p, err)
	}
	if _, err := r.TakePending("toolu_1"); !os.IsNotExist(err) {
		t.Fatal("a note taken twice")
	}
}
