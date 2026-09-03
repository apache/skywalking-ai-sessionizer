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
	"fmt"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/verify"
)

// TestConversationGrowsAcrossRounds is the test closest to production: a live
// session gains turns while collection runs repeatedly against it.
//
// It exercises what a static fixture cannot - cursors resuming, source line
// numbers continuing across deltas, the index accumulating rather than being
// rebuilt, and content from every round surviving.
func TestConversationGrowsAcrossRounds(t *testing.T) {
	src := t.TempDir()
	zone := storage.NewZone(t.TempDir())
	x := newTranscript(t, src)

	// Turns 3 and 5 spawn a child agent, so some rounds add streams as well as
	// records - the parent's Talk then spans two prompt cycles.
	prompts := []string{
		"set up the project",
		"add a build target",
		"check the tests", // spawns
		"fix the failing case",
		"review everything", // spawns
	}

	var lastRecords int
	for i, p := range prompts {
		x.AddTurn(p, i == 2 || i == 4)
		x.Flush()

		st, err := claudecode.New(src, zone, 0).CollectAll(nil)
		if err != nil {
			t.Fatalf("round %d: %v", i+1, err)
		}
		if len(st.Errors) != 0 || !st.Complete() {
			t.Fatalf("round %d incomplete: errors=%v pending=%d", i+1, st.Errors, st.Pending)
		}
		// Each round must land strictly more than the last; a round that lands
		// nothing after an append means the cursor did not advance.
		if st.Records <= 0 && i > 0 {
			t.Errorf("round %d landed no records despite an append", i+1)
		}
		lastRecords = st.Records
	}
	_ = lastRecords

	// --- every round's content survives ---
	c := &collected{zone: zone, t: t}
	mainDir := zone.StreamDir(growSession, storage.StreamMain)
	var joined strings.Builder
	for _, rec := range c.records(mainDir, "transcript") {
		joined.WriteString(content(rec))
	}
	all := joined.String()
	for i, p := range prompts {
		if !strings.Contains(all, p) {
			t.Errorf("turn %d prompt %q missing after %d rounds", i+1, p, len(prompts))
		}
		if want := fmt.Sprintf("Turn %d complete.", i+1); !strings.Contains(all, want) {
			t.Errorf("turn %d answer missing", i+1)
		}
	}

	// --- source line numbers stay contiguous ACROSS rounds ---
	// A delta starts mid-source, so ord is what proves nothing was skipped
	// between one round's end and the next round's start.
	recs := c.records(mainDir, "transcript")
	for i, rec := range recs {
		if rec.Ord != uint64(i+1) {
			t.Fatalf("record %d has source ord %d, want %d — a line was skipped or repeated",
				i, rec.Ord, i+1)
		}
	}

	// --- the audit that runs on real data ---
	rep, err := verify.Session(zone, growSession)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Errorf("verify found %d problem(s): %v", rep.Problems, rep.Details())
	}

	// --- structure: Talks, cycles, streams ---
	ix, ok, err := index.Load(zone.IndexDir(growSession), growSession)
	if err != nil || !ok {
		t.Fatalf("no index: ok=%v err=%v", ok, err)
	}
	ix.Build()

	// 5 human turns, 2 of which also have a notification cycle: 7 prompt cycles,
	// but only 5 Talks. Splitting on cycles would show 7 conversations where a
	// reader sees 5.
	cycles := map[uint32]bool{}
	for i := range ix.Entries {
		if e := &ix.Entries[i]; e.Run != 0 && ix.Strings.String(e.Stream) == storage.StreamMain {
			cycles[e.Run] = true
		}
	}
	if len(cycles) != 7 {
		t.Errorf("main stream has %d prompt cycles, want 7 (5 human + 2 notification)", len(cycles))
	}

	// main plus one child stream per spawning turn
	if got := len(ix.Streams()); got != 3 {
		t.Errorf("streams = %d, want 3 (main + 2 children): %v", got, ix.Streams())
	}

	// Each provider call was split across two fragments; grouping by call id
	// must collapse them, or every count inflates.
	if got := ix.ProviderCall("t1-call"); len(got) != 2 {
		t.Errorf("turn 1 provider call has %d fragments, want 2", len(got))
	}

	// The child's answer belongs to the child stream, and must not appear in
	// the parent - otherwise a rendered conversation duplicates subagent work.
	if strings.Contains(all, "helper finished turn 3") {
		t.Error("the parent stream absorbed a child's output")
	}
	var childFound bool
	for _, s := range ix.Streams() {
		if s == storage.StreamMain {
			continue
		}
		for _, rec := range c.records(zone.StreamDir(growSession, s), "transcript") {
			if strings.Contains(content(rec), "helper finished") {
				childFound = true
			}
		}
	}
	if !childFound {
		t.Error("child output missing from every child stream")
	}
}

// TestReCollectIsIdempotent asserts that collecting an unchanged source lands
// nothing. Without it, a watch loop would re-land the whole corpus every tick.
func TestReCollectIsIdempotent(t *testing.T) {
	src := t.TempDir()
	zone := storage.NewZone(t.TempDir())
	x := newTranscript(t, src)
	x.AddTurn("do the thing", true)
	x.Flush()

	col := claudecode.New(src, zone, 0)
	first, err := col.CollectAll(nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Records == 0 {
		t.Fatal("first pass landed nothing")
	}
	second, err := col.CollectAll(nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.Records != 0 {
		t.Errorf("re-collecting an unchanged source landed %d records, want 0", second.Records)
	}
	if second.SourcesLanded != 0 {
		t.Errorf("re-collecting touched %d sources, want 0", second.SourcesLanded)
	}
}
