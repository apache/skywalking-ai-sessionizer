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

package capture_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/pkg/changes"
	"github.com/apache/skywalking-ai-sessionizer/plugins/claude-code/internal/capture"
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

// A clock that moves a second per scan, so the stat cache's racy window
// never hides a change made between two scans in the same instant.
func clock() func() time.Time {
	t := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	return func() time.Time {
		t = t.Add(2 * time.Second)
		return t
	}
}

func opts(now func() time.Time) scan.Options {
	rules, _ := scope.Compile(scope.StandardV1, nil, nil)
	return scan.Options{Rules: rules, SizeCap: 1 << 20, Now: now}
}

// TestTwoWindowsInterleaved is the B1 B2 A1 A2 case: two tools open on one
// root at once. A change in the step only one window spans is that
// window's alone; a change in the step both span is shared, and each
// record names the other.
func TestTwoWindowsInterleaved(t *testing.T) {
	ws := t.TempDir()
	data := t.TempDir()
	write(t, filepath.Join(ws, "f1.txt"), "one\n")
	write(t, filepath.Join(ws, "f2.txt"), "two\n")
	write(t, filepath.Join(ws, "f3.txt"), "three\n")
	now := clock()
	r, err := scan.Open(data, ws)
	if err != nil {
		t.Fatal(err)
	}
	reg := &capture.Registry{}
	o := opts(now)

	// Baseline, then B1.
	if _, _, err := r.Scan(o); err != nil {
		t.Fatal(err)
	}
	m, _, _ := r.Scan(o)
	reg.Open(capture.Window{Capture: "t1", Session: "S", Stream: "main", Tool: "t1", ToolName: "Bash", Before: m.N, OpenedAt: m.To})
	// Tool 1 writes f1, then B2.
	write(t, filepath.Join(ws, "f1.txt"), "ONE\n")
	m, _, _ = r.Scan(o)
	reg.Open(capture.Window{Capture: "t2", Session: "S", Stream: "a1", Tool: "t2", ToolName: "Bash", Before: m.N, OpenedAt: m.To})
	// Somebody writes f2 while both are open, then A1.
	write(t, filepath.Join(ws, "f2.txt"), "TWO\n")
	m, _, _ = r.Scan(o)
	reg.Close("t1", m.N, m.To, false)
	// Tool 2 writes f3, then A2.
	write(t, filepath.Join(ws, "f3.txt"), "THREE\n")
	m, _, _ = r.Scan(o)
	reg.Close("t2", m.N, m.To, false)

	ctx := func(tool string) capture.Context {
		return capture.Context{Session: "S", Stream: "main", Tool: tool, ToolName: "Bash", Outcome: &changes.Outcome{State: changes.OutcomeReturned}}
	}
	r1, err := capture.Build(r, reg, reg.Get("t1"), ctx("t1"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := capture.Build(r, reg, reg.Get("t2"), ctx("t2"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := r1.Validate(); err != nil {
		t.Fatal(err)
	}
	byPath := func(rec *changes.Record) map[string]changes.FileChange {
		m := map[string]changes.FileChange{}
		for _, c := range rec.Changes {
			m[c.Path] = c
		}
		return m
	}
	c1, c2 := byPath(r1), byPath(r2)
	if len(c1) != 2 || len(c2) != 2 {
		t.Fatalf("window 1 saw %v, window 2 saw %v", keys(c1), keys(c2))
	}
	if c1["f1.txt"].Attribution != changes.AttributionOnlyThisWindow || len(c1["f1.txt"].Windows) != 1 {
		t.Errorf("f1 in window 1: %+v", c1["f1.txt"])
	}
	if c1["f2.txt"].Attribution != changes.AttributionShared || len(c1["f2.txt"].Windows) != 2 {
		t.Errorf("f2 in window 1: %+v", c1["f2.txt"])
	}
	if c2["f2.txt"].Attribution != changes.AttributionShared || c2["f3.txt"].Attribution != changes.AttributionOnlyThisWindow {
		t.Errorf("window 2: f2 %+v, f3 %+v", c2["f2.txt"], c2["f3.txt"])
	}
	if len(r1.Overlaps) != 1 || r1.Overlaps[0].Capture != "t2" || r1.Overlaps[0].Stream != "a1" || r1.Overlaps[0].State != "closed" {
		t.Errorf("window 1 overlaps: %+v", r1.Overlaps)
	}
	if len(r2.Overlaps) != 1 || r2.Overlaps[0].Capture != "t1" {
		t.Errorf("window 2 overlaps: %+v", r2.Overlaps)
	}
	// The hunks come from the kept bytes.
	f1 := c1["f1.txt"]
	if f1.Operation != changes.OpModify || f1.Diff != changes.DiffAvailable || len(f1.Hunks) != 1 || f1.Hunks[0].Lines[0] != "-one" || f1.Hunks[0].Lines[1] != "+ONE" {
		t.Errorf("f1 hunks: %+v", f1)
	}
	if *f1.Additions != 1 || *f1.Deletions != 1 || *r1.ChangedFiles != 2 || r1.Coverage != changes.CoverageComplete {
		t.Errorf("counts: %+v", r1)
	}
	if r1.Window.Before.From == "" || r1.Window.After.To == "" || r1.Time != r1.Window.After.To {
		t.Errorf("window times: %+v time %s", r1.Window, r1.Time)
	}
}

func keys(m map[string]changes.FileChange) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestGapAndDeleteAndCreate(t *testing.T) {
	ws := t.TempDir()
	data := t.TempDir()
	write(t, filepath.Join(ws, "old.txt"), "gone\n")
	now := clock()
	r, _ := scan.Open(data, ws)
	o := opts(now)
	if _, _, err := r.Scan(o); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(ws, "old.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(ws, "new.txt"), "hello\n")
	write(t, filepath.Join(ws, "bin.dat"), "a\x00b")
	_, step, err := r.Scan(o)
	if err != nil {
		t.Fatal(err)
	}
	rec := capture.BuildGap(r, step, "S", "main", nil, 1<<20)
	if err := rec.Validate(); err != nil {
		t.Fatal(err)
	}
	if rec.Basis != changes.BasisUnattributed || rec.Tool != "" || *rec.ChangedFiles != 3 {
		t.Fatalf("gap record: %+v", rec)
	}
	for _, c := range rec.Changes {
		if c.Attribution != changes.AttributionOutsideAnyWindow {
			t.Errorf("%s attribution %s", c.Path, c.Attribution)
		}
		switch c.Path {
		case "old.txt":
			if c.Operation != changes.OpDelete || c.After.Present || !c.Before.Present || *c.Deletions != 1 {
				t.Errorf("delete: %+v", c)
			}
		case "new.txt":
			if c.Operation != changes.OpCreate || c.Before.Present || *c.Additions != 1 || c.Hunks[0].NewStart != 1 {
				t.Errorf("create: %+v", c)
			}
		case "bin.dat":
			if c.Diff != changes.DiffBinary || c.Hunks != nil || c.Additions != nil {
				t.Errorf("binary: %+v", c)
			}
		default:
			t.Errorf("unexpected %s", c.Path)
		}
	}
}

func TestRegistryRoundTripAndPrune(t *testing.T) {
	r, _ := scan.Open(t.TempDir(), t.TempDir())
	reg := &capture.Registry{}
	reg.Open(capture.Window{Capture: "a", Before: 1, OpenedAt: "2026-09-08T10:00:00Z"})
	reg.Open(capture.Window{Capture: "b", Before: 2, OpenedAt: "2026-09-08T10:00:10Z"})
	reg.Close("a", 3, "2026-09-08T10:00:20Z", false)
	if err := reg.Save(r); err != nil {
		t.Fatal(err)
	}
	back, err := capture.LoadRegistry(r)
	if err != nil || len(back.Windows) != 2 || back.Get("a").After != 3 || len(back.OpenWindows()) != 1 {
		t.Fatalf("round trip: %+v err=%v", back, err)
	}
	now := time.Date(2026, 9, 8, 11, 0, 0, 0, time.UTC)
	back.Prune(30*time.Minute, now)
	if len(back.Windows) != 1 || back.Windows[0].Capture != "b" {
		t.Fatalf("prune kept %+v; an open window stays, a closed one older than the age goes", back.Windows)
	}
	if !back.LastActivity().Equal(time.Date(2026, 9, 8, 10, 0, 10, 0, time.UTC)) {
		t.Fatalf("last activity %v", back.LastActivity())
	}
}
