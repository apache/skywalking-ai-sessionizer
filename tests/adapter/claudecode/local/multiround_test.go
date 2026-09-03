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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/assemble"
	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// TestMultiRoundChain drives one session through the sequence a real chain
// takes, and checks the properties that only appear once a chain is longer than
// one round. A single-round chain proves almost nothing: every rule about
// superseding, watermarks and recovery is about round N against round N-1.
//
//	R1  an initial prefix
//	R2  late evidence that REVISES entities R1 already published
//	R3  evidence that changes no entity but must still move the watermark
//	    recovery with the state file deleted and with it made stale
//	    a concurrent publish attempt
func TestMultiRoundChain(t *testing.T) {
	src := t.TempDir()
	z := storage.NewZone(t.TempDir())
	x := newTranscript(t, src)
	opts := parse.Options{
		Conversation: growSession, Session: growSession, Reindex: index.Rebuild,
	}
	collect := func() {
		t.Helper()
		if _, err := claudecode.New(src, z, 0).CollectAll(nil); err != nil {
			t.Fatal(err)
		}
	}

	// R1 - a turn whose tool result has not arrived yet.
	x.AddTurn("start the work", true)
	x.AddOrphanTool()
	x.Flush()
	collect()
	r1, err := parse.Session(z, opts)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Number != 1 {
		t.Fatalf("first parse wrote round %d", r1.Number)
	}
	v1, err := parse.View(z.Root(), growSession)
	if err != nil {
		t.Fatal(err)
	}
	toolID := unfinishedTool(t, v1)

	// R2 - the missing result arrives, revising a node R1 already published.
	x.AddOrphanResult()
	x.AddTurn("keep going", false)
	x.Flush()
	collect()
	r2, err := parse.Session(z, opts)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Number != 2 {
		t.Fatalf("second parse wrote round %d", r2.Number)
	}
	if r2.FromSeq != r1.ThroughSeq+1 {
		t.Errorf("round 2 starts at %d, round 1 ended at %d", r2.FromSeq, r1.ThroughSeq)
	}
	v2, err := parse.View(z.Root(), growSession)
	if err != nil {
		t.Fatal(err)
	}
	tool := v2.Nodes[toolID]
	if tool == nil {
		t.Fatalf("the tool node vanished at round 2")
	}
	if tool.Revision != 2 {
		t.Errorf("the tool node is at revision %d; a revision must name the round that produced it", tool.Revision)
	}
	if strings.Contains(string(tool.Attrs), `"result":"unavailable"`) {
		t.Error("the arriving result did not supersede the earlier revision")
	}
	// Round 2 carried the revision, and only what changed.
	if r2.Nodes >= len(v2.Nodes) {
		t.Errorf("round 2 wrote %d nodes against a folded %d; it is not a delta", r2.Nodes, len(v2.Nodes))
	}

	// R3 - a replayed block. No entity changes, but the evidence is new, so the
	// watermark must move or the chain re-reads it forever.
	x.AddReplay(6)
	x.Flush()
	collect()
	r3, err := parse.Session(z, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !r3.Changed() {
		t.Fatal("new evidence produced no round, so the chain would re-read it on every pass")
	}
	if r3.Nodes != 0 || r3.Relations != 0 || r3.Tombstones != 0 {
		t.Errorf("the replay changed the conversation: %d nodes, %d relations, %d tombstones",
			r3.Nodes, r3.Relations, r3.Tombstones)
	}
	if r3.ThroughSeq <= r2.ThroughSeq {
		t.Errorf("watermark did not advance: %d -> %d", r2.ThroughSeq, r3.ThroughSeq)
	}

	// Recovery - the state file is a cache. Deleting it must change nothing, and
	// a STALE one must not be believed over the rounds themselves.
	chain := sessionflow.OpenChain(z.Root(), growSession)
	statePath := filepath.Join(z.Root(), "_conversations", growSession, "conversation.state")
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	x.AddTurn("after losing the state file", true)
	x.Flush()
	collect()
	r4, err := parse.Session(z, opts)
	if err != nil {
		t.Fatalf("parse could not recover from a missing state file: %v", err)
	}
	if r4.Number != 4 {
		t.Fatalf("after recovery the chain wrote round %d, not 4", r4.Number)
	}

	// Now make the state file lie about everything mutable, and check the round
	// is unaffected - it must be derived from the last round's own header.
	stale, err := chain.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	stale.Head, stale.HeadDigest = 1, strings.Repeat("f", 64)
	stale.ThroughSeq, stale.InputDigest = 1, "a-stale-digest"
	if err := chain.SaveState(stale, time.Unix(1750000000, 0)); err != nil {
		t.Fatal(err)
	}
	x.AddTurn("after a stale state file", false)
	x.Flush()
	collect()
	r5, err := parse.Session(z, opts)
	if err != nil {
		t.Fatalf("parse trusted a stale state file: %v", err)
	}
	if r5.Number != 5 {
		t.Fatalf("a stale state file moved the chain to round %d, not 5", r5.Number)
	}
	if r5.FromSeq != r4.ThroughSeq+1 {
		t.Errorf("a stale state file changed the consumed range: round 5 starts at %d, round 4 ended at %d",
			r5.FromSeq, r4.ThroughSeq)
	}

	// The chain must verify as a chain, and must be exactly five rounds.
	files, err := chain.Verify()
	if err != nil {
		t.Fatalf("chain verification: %v", err)
	}
	if len(files) != 5 {
		t.Fatalf("got %d rounds, want 5", len(files))
	}
	for _, f := range files {
		fi, serr := os.Stat(f.Path)
		if serr != nil {
			t.Fatal(serr)
		}
		if fi.Mode().Perm()&0o222 != 0 {
			t.Errorf("round %d is writable: %v", f.Round, fi.Mode())
		}
	}

	// THE ACCEPTANCE PROPERTY: folding every round equals parsing all the
	// evidence those rounds consumed.
	assertFoldEqualsFullParse(t, z, growSession, opts)
}

// unfinishedTool returns the id of the tool whose result has not arrived.
func unfinishedTool(t *testing.T, v *sessionflow.View) string {
	t.Helper()
	for id, n := range v.Nodes {
		if n.Kind == model.KindTool && strings.Contains(string(n.Attrs), `"result":"unavailable"`) {
			return id
		}
	}
	t.Fatal("no unfinished tool in round 1; the test needs one to revise")
	return ""
}

// assertFoldEqualsFullParse checks the property the whole design rests on.
func assertFoldEqualsFullParse(t *testing.T, z *storage.Zone, session string, opts parse.Options) {
	t.Helper()
	v, err := parse.View(z.Root(), session)
	if err != nil {
		t.Fatal(err)
	}
	ix, ok, err := index.Load(z.IndexDir(session), session)
	if err != nil || !ok {
		t.Fatalf("index: ok=%v err=%v", ok, err)
	}
	full, err := assemble.Session(ix, assemble.Options{
		Conversation: opts.Conversation, Session: session, ThroughSeq: v.ThroughSeq,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Nodes) != len(v.Nodes) {
		t.Errorf("a full parse through seq %d has %d nodes; the fold of %d rounds has %d",
			v.ThroughSeq, len(full.Nodes), v.Round, len(v.Nodes))
	}
	for _, n := range full.Nodes {
		folded, present := v.Nodes[n.ID]
		if !present {
			t.Errorf("node %s is in a full parse but not in the fold", n.ID)
			continue
		}
		if folded.Kind != n.Kind || folded.Parent != n.Parent || string(folded.Attrs) != string(n.Attrs) {
			t.Errorf("node %s differs between a full parse and the fold:\n  parse %s parent=%s attrs=%s\n  fold  %s parent=%s attrs=%s",
				n.ID, n.Kind, n.Parent, n.Attrs, folded.Kind, folded.Parent, folded.Attrs)
		}
	}
	if len(full.Relations) != len(v.Relations) {
		t.Errorf("relations: a full parse has %d, the fold has %d", len(full.Relations), len(v.Relations))
	}
}

// Two builders parsing one conversation at once must produce one chain, not a
// fork. Publishing is a read followed by a write, so without a lock both read
// round N and both write an N+1 - and because the digest is part of the
// filename, their files do not even collide.
func TestConcurrentParseDoesNotForkTheChain(t *testing.T) {
	src := t.TempDir()
	z := storage.NewZone(t.TempDir())
	x := newTranscript(t, src)
	x.AddTurn("one", true)
	x.AddTurn("two", false)
	x.Flush()
	if _, err := claudecode.New(src, z, 0).CollectAll(nil); err != nil {
		t.Fatal(err)
	}

	const builders = 6
	var wg sync.WaitGroup
	results := make([]*parse.Round, builders)
	errs := make([]error, builders)
	for i := range builders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = parse.Session(z, parse.Options{
				Conversation: growSession, Session: growSession,
				Reindex: index.Rebuild,
			})
		}()
	}
	wg.Wait()

	var wrote int
	for i, r := range results {
		if errs[i] != nil {
			continue // losing the race is a legitimate outcome
		}
		if r.Changed() {
			wrote++
		}
	}
	if wrote != 1 {
		t.Errorf("%d builders wrote a round; exactly one should have", wrote)
	}

	chain := sessionflow.OpenChain(z.Root(), growSession)
	files, err := chain.Verify()
	if err != nil {
		t.Fatalf("the chain did not survive concurrent parsing: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("the chain has %d rounds after one parse worth of work", len(files))
	}
	if _, err := chain.Fold(); err != nil {
		t.Fatalf("fold: %v", err)
	}
}
