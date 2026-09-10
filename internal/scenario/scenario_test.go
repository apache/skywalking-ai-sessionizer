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

package scenario

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const oneStep = "steps:\n  - input: hello\n"

// TestLoadSetExpandsADirectory: a directory contributes its scenarios in a
// fixed order, and never an expectation file, which is not a scenario.
func TestLoadSetExpandsADirectory(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"b.yaml", "a.yaml", "c.yaml"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(oneStep), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// An expectation file sits beside every scenario in this project. It
	// would not load as one, so it must be passed over rather than fail.
	if err := os.WriteFile(filepath.Join(dir, "a.expect.yaml"), []byte("rounds: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("not a scenario"), 0o600); err != nil {
		t.Fatal(err)
	}
	set, err := LoadSet([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range set {
		got = append(got, filepath.Base(l.Path))
	}
	want := []string{"a.yaml", "b.yaml", "c.yaml"}
	if len(got) != len(want) {
		t.Fatalf("LoadSet gave %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("LoadSet gave %v, want %v", got, want)
		}
	}
}

// TestLoadSetKeepsTheOrderGivenAndDropsRepeats: a fixed list is emitted in
// the order it was written, and naming one file twice does not emit it
// twice.
func TestLoadSetKeepsTheOrderGivenAndDropsRepeats(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"one.yaml", "two.yaml"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(oneStep), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	two, one := filepath.Join(dir, "two.yaml"), filepath.Join(dir, "one.yaml")
	set, err := LoadSet([]string{two, one, two})
	if err != nil {
		t.Fatal(err)
	}
	if len(set) != 2 || filepath.Base(set[0].Path) != "two.yaml" || filepath.Base(set[1].Path) != "one.yaml" {
		t.Fatalf("LoadSet gave %d entries, first %s", len(set), set[0].Path)
	}
}

// TestLoadSetReportsAnEmptyDirectory rather than feeding nothing.
func TestLoadSetReportsAnEmptyDirectory(t *testing.T) {
	if _, err := LoadSet([]string{t.TempDir()}); err == nil {
		t.Fatal("LoadSet accepted a directory holding no scenarios")
	}
}

// TestSpanIsTheDistanceCovered: a feed shifts a session back by its span so
// the last record lands now, so the span must be first to last, and must
// not be moved by the base time or by a record that carries no time.
func TestSpanIsTheDistanceCovered(t *testing.T) {
	sc, err := Load(filepath.Join("..", "..", "tests", "scenarios", "assembly.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	early, err := sc.Plan(Options{At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	late, err := sc.Plan(Options{At: time.Date(2031, 6, 5, 4, 3, 2, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if early.Span() != late.Span() {
		t.Fatalf("the span moved with the base time: %s then %s", early.Span(), late.Span())
	}
	if early.Span() <= 0 {
		t.Fatalf("span is %s; a scenario with steps covers time", early.Span())
	}
	// Shifting the base back by the span puts the last record at the base.
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	p, err := sc.Plan(Options{At: now.Add(-early.Span())})
	if err != nil {
		t.Fatal(err)
	}
	var last time.Time
	for _, e := range p.Events {
		if e.At.After(last) {
			last = e.At
		}
	}
	if !last.Equal(now) {
		t.Fatalf("the last record is at %s, want %s", last, now)
	}
}

// TestSpanCountsALeadingDelay. A first step that says "after: 1h" starts
// the session an hour after the base. A span measured from the first record
// would leave that hour out, and a feed, which moves the base back one
// span, would stamp its last record an hour ahead of the wall clock.
func TestSpanCountsALeadingDelay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lead.yaml")
	body := "steps:\n  - input: much later\n    after: 1h\n  - input: and then\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	sc, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	probe, err := sc.Plan(Options{At: time.Unix(0, 0).UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if probe.Span() < time.Hour {
		t.Fatalf("span is %s; the leading hour was left out", probe.Span())
	}
	p, err := sc.Plan(Options{At: now.Add(-probe.Span())})
	if err != nil {
		t.Fatal(err)
	}
	var last time.Time
	for _, e := range p.Events {
		if e.At.After(last) {
			last = e.At
		}
	}
	if last.After(now) {
		t.Fatalf("the last record is at %s, which is ahead of %s", last, now)
	}
}

// TestBuildSwitchesCollectorModeAndKeepsWhatWasAppended. Adding --every to
// a directory that already holds a one-shot build must not be refused as a
// foreign configuration, and the export block a person appended for a push
// has to survive the rewrite.
func TestBuildSwitchesCollectorModeAndKeepsWhatWasAppended(t *testing.T) {
	sc, err := Load(filepath.Join("..", "..", "tests", "scenarios", "assembly.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b, err := Build(sc, FormatClaudeCode, out, Options{At: at})
	if err != nil {
		t.Fatal(err)
	}
	const appended = "\nexport:\n  otlp:\n    endpoint: 127.0.0.1:11800\n"
	body, err := os.ReadFile(b.Config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.Config, append(body, appended...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(sc, FormatClaudeCode, out, Options{At: at, Watch: true}); err != nil {
		t.Fatalf("a feed into a one-shot build's directory was refused: %v", err)
	}
	after, err := os.ReadFile(b.Config)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "mode: watch") {
		t.Fatal("the collector is not in watch mode after a feed build")
	}
	if !strings.HasSuffix(string(after), appended) {
		t.Fatal("what was appended to the configuration was lost")
	}
	// And back again, so neither direction is a one-way door.
	if _, err := Build(sc, FormatClaudeCode, out, Options{At: at}); err != nil {
		t.Fatalf("a one-shot build into a feed's directory was refused: %v", err)
	}
}
