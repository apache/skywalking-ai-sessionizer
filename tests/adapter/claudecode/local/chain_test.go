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

package local_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/verify"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// The tests here simulate what a scenario cannot express: a crash between
// landing and committing a cursor, a lost or stale state file, and several
// collectors or parsers running at once. Every property of assembly itself
// is a scenario under tests/scenarios.

// growing is a scenario with three checkpoints, so a test can land evidence
// in stages.
const growing = `
session: add1c7ed-0003-4000-8000-000000000003
project: -Users-dev-growing-work
steps:
  - input: one
  - call: {text: Working on one, tool: {name: Bash, input: {command: echo one}, result: one}}
  - call: {text: One complete.}
    checkpoint: one
  - input: two, delegate it
  - call:
      agent: {name: helper, prompt: help with two, steps: [{call: {text: helper finished two}}], notify: true}
  - call: {text: Two complete.}
    checkpoint: two
  - input: three
  - call: {text: Working on three, tool: {name: Bash, input: {command: echo three}, result: three}}
  - call: {text: Three complete.}
    checkpoint: three
  - input: four
  - call: {text: Four complete.}
`

// stage is one scenario built into one directory, through whichever
// checkpoint the test has reached, with the real adapter collecting it.
type stage struct {
	t       *testing.T
	sc      *scenario.Scenario
	out     string
	zone    *storage.Zone
	session string
}

func newStage(t *testing.T, text string) *stage {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scenario.yaml")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	sc, err := scenario.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	return &stage{t: t, sc: sc, out: out, zone: storage.NewZone(out)}
}

// through builds the scenario through a checkpoint, or whole when the name
// is empty, and collects it.
func (s *stage) through(checkpoint string) *claudecode.Stats {
	s.t.Helper()
	b, err := scenario.Build(s.sc, scenario.FormatClaudeCode, s.out, scenario.Options{
		At: time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC), Through: checkpoint,
	})
	if err != nil {
		s.t.Fatal(err)
	}
	s.session = b.Session
	st, err := s.collector().CollectAll(nil)
	if err != nil {
		s.t.Fatal(err)
	}
	return st
}

func (s *stage) collector() *claudecode.Collector {
	return claudecode.New(filepath.Join(s.out, "source"), s.zone, 0)
}

func (s *stage) parse() *parse.Round {
	s.t.Helper()
	r, err := parse.Session(s.zone, parse.Options{Conversation: s.session, Session: s.session, Reindex: index.Rebuild})
	if err != nil {
		s.t.Fatal(err)
	}
	return r
}

// An interrupted pass repeats its landing rather than losing it: the same
// records land twice, verification reports the repeat, and the chain moves
// its watermark without changing the conversation.
func TestInterruptedPassRepeatsRatherThanLoses(t *testing.T) {
	s := newStage(t, growing)
	s.through("two")
	before := s.parse()

	rollBackCursors(t, s.zone.SessionDir(s.session))
	if _, err := s.collector().CollectAll(nil); err != nil {
		t.Fatal(err)
	}
	rep, err := verify.Session(s.zone, s.session)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Errorf("a repeated landing was reported as %d problem(s): %v", rep.Problems, rep.Details())
	}
	if rep.Relanded == 0 {
		t.Error("the repeat was not reported at all; it must be visible")
	}
	after := s.parse()
	if !after.Changed() {
		t.Error("re-landed evidence did not advance the chain, so it would be re-read forever")
	}
	if after.Nodes != 0 || after.Relations != 0 || after.Tombstones != 0 {
		t.Errorf("the repeat changed the conversation: %d nodes, %d relations, %d tombstones", after.Nodes, after.Relations, after.Tombstones)
	}
	if after.ThroughSeq <= before.ThroughSeq {
		t.Errorf("watermark did not advance: %d -> %d", before.ThroughSeq, after.ThroughSeq)
	}
	if before.Stats.Talks != after.Stats.Talks || before.Stats.ToolUses != after.Stats.ToolUses {
		t.Errorf("assembly differs after a repeat: talks %d->%d, tools %d->%d", before.Stats.Talks, after.Stats.Talks, before.Stats.ToolUses, after.Stats.ToolUses)
	}
}

// rollBackCursors removes the cursors of the tailed sources, the transcripts
// and journals, as a crash between landing and committing leaves them. A
// snapshot source, such as a child's meta file, is left alone: its records
// carry no identity, so a second landing of one is a second record, and
// that is not the crash this test is about.
func rollBackCursors(t *testing.T, dir string) {
	t.Helper()
	var n int
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".cursor") || strings.HasSuffix(path, "meta.cursor") {
			return err
		}
		n++
		return os.Remove(path)
	})
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no cursors found; the test would not exercise anything")
	}
}

// The chain recovers from its state file being lost, and does not trust one
// that is stale: the filesystem is the authority on how far the chain got.
func TestChainRecoversFromLostAndStaleState(t *testing.T) {
	s := newStage(t, growing)
	s.through("one")
	r1 := s.parse()
	if r1.Number != 1 {
		t.Fatalf("first parse wrote round %d", r1.Number)
	}
	chain := sessionflow.OpenChain(s.zone.Root(), s.session)

	if err := os.Remove(chain.StatePath()); err != nil {
		t.Fatal(err)
	}
	s.through("two")
	r2 := s.parse()
	if r2.Number != 2 || r2.FromSeq != r1.ThroughSeq+1 {
		t.Fatalf("after losing the state file the chain wrote round %d from seq %d; want round 2 from %d", r2.Number, r2.FromSeq, r1.ThroughSeq+1)
	}

	stale, err := chain.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	stale.Head, stale.HeadDigest = 1, strings.Repeat("f", 64)
	stale.ThroughSeq, stale.InputDigest = 1, "a-stale-digest"
	if err := chain.SaveState(stale, time.Unix(1750000000, 0)); err != nil {
		t.Fatal(err)
	}
	s.through("three")
	r3 := s.parse()
	if r3.Number != 3 || r3.FromSeq != r2.ThroughSeq+1 {
		t.Fatalf("a stale state file moved the chain to round %d from seq %d; want round 3 from %d", r3.Number, r3.FromSeq, r2.ThroughSeq+1)
	}
	if files, err := chain.Verify(); err != nil || len(files) != 3 {
		t.Fatalf("chain verification: %d rounds, %v", len(files), err)
	}
}

// Two parsers publishing at once do not fork the chain: one writes the
// round, the other finds nothing new.
func TestConcurrentParseDoesNotForkTheChain(t *testing.T) {
	s := newStage(t, growing)
	s.through("two")
	var wg sync.WaitGroup
	rounds := make([]*parse.Round, 4)
	for i := range rounds {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := parse.Session(s.zone, parse.Options{Conversation: s.session, Session: s.session, Reindex: index.Rebuild})
			if err != nil {
				// Losing the chain's lock to another builder is the
				// expected way to write nothing.
				if !strings.Contains(err.Error(), "locked") {
					t.Error(err)
				}
				return
			}
			rounds[i] = r
		}(i)
	}
	wg.Wait()
	written := 0
	for _, r := range rounds {
		if r != nil && r.Changed() {
			written++
		}
	}
	if written != 1 {
		t.Fatalf("%d of 4 concurrent parses wrote a round, want exactly 1", written)
	}
	if files, err := sessionflow.OpenChain(s.zone.Root(), s.session).Verify(); err != nil || len(files) != 1 {
		t.Fatalf("chain: %d rounds, %v", len(files), err)
	}
}

// Several goroutines sharing one collector are safe.
func TestSharedCollectorIsSafe(t *testing.T) {
	s := newStage(t, growing)
	s.through("one")
	col := s.collector()
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = col.CollectAll(nil) }()
	}
	wg.Wait()
}

// The round budget changes where the evidence is cut, never what it says:
// parsed whole and parsed under a one-byte budget, the two folds hold the
// same entities and reach the same index.
func TestRoundBudgetCutsTheChainByWindow(t *testing.T) {
	whole := newStage(t, growing)
	whole.through("")
	one := whole.parse()
	if !one.Changed() || one.More {
		t.Fatalf("one round under the default budget: changed=%v more=%v", one.Changed(), one.More)
	}

	cut := newStage(t, growing)
	cut.through("")
	var rounds []*parse.Round
	for {
		r, err := parse.Session(cut.zone, parse.Options{Conversation: cut.session, Session: cut.session, Reindex: index.Rebuild, MaxRoundBytes: 1})
		if err != nil {
			t.Fatal(err)
		}
		rounds = append(rounds, r)
		if !r.More {
			break
		}
	}
	if len(rounds) != int(one.ThroughSeq) {
		t.Fatalf("a one-byte budget must write one round per landed file: %d rounds for %d files", len(rounds), one.ThroughSeq)
	}
	va, err := parse.View(whole.zone.Root(), whole.session)
	if err != nil {
		t.Fatal(err)
	}
	vb, err := parse.View(cut.zone.Root(), cut.session)
	if err != nil {
		t.Fatal(err)
	}
	if va.ThroughSeq != vb.ThroughSeq || len(va.Nodes) != len(vb.Nodes) || len(va.Relations) != len(vb.Relations) {
		t.Fatalf("folds: whole through %d with %d nodes and %d relations; cut through %d with %d nodes and %d relations",
			va.ThroughSeq, len(va.Nodes), len(va.Relations), vb.ThroughSeq, len(vb.Nodes), len(vb.Relations))
	}
	for id, n := range va.Nodes {
		m := vb.Nodes[id]
		if m == nil || m.Kind != n.Kind || m.Parent != n.Parent {
			t.Fatalf("node %s differs between the folds: whole=%v cut=%v", id, n, m)
		}
	}
}
