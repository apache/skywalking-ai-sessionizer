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
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/assemble"
	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/verify"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// A conversation grows a round at a time, and the fold of every round is the
// conversation. This is the property the whole chain design exists to provide.
func TestConversationGrowsARoundAtATime(t *testing.T) {
	p := build(t, func(x *transcript) { x.AddTurn("first", false) })
	if p.round.Number != 1 {
		t.Fatalf("first parse wrote round %d", p.round.Number)
	}
	firstNodes := len(p.view.Nodes)

	p.x.AddTurn("second, delegate it", true)
	p.x.Flush()
	p.reparse(t)
	if p.round.Number != 2 {
		t.Fatalf("second parse wrote round %d", p.round.Number)
	}
	if len(p.view.Nodes) <= firstNodes {
		t.Fatalf("round 2 folded to %d nodes, no more than round 1's %d", len(p.view.Nodes), firstNodes)
	}
	if n := len(p.talksOn("main")); n != 2 {
		t.Errorf("talks after two turns: %d, want 2", n)
	}

	// Round 2 carries a delta, not a copy of everything.
	if p.round.Nodes >= len(p.view.Nodes) {
		t.Errorf("round 2 wrote %d nodes against a folded total of %d; it is not a delta",
			p.round.Nodes, len(p.view.Nodes))
	}
	if p.round.FromSeq <= 1 {
		t.Errorf("round 2 starts at landed sequence %d; it re-read round 1's evidence", p.round.FromSeq)
	}
}

// Parsing when nothing changed must write nothing. A watch loop calls this every
// few seconds, and a round per tick would bury the real ones.
func TestUnchangedSessionWritesNoRound(t *testing.T) {
	p := build(t, func(x *transcript) { x.AddTurn("only turn", true) })
	before := p.round.Number

	p.reparse(t)
	if p.round.Changed() {
		t.Fatalf("an unchanged session wrote round %d", p.round.Number)
	}
	if p.view.Round != before {
		t.Errorf("the chain head moved from %d to %d with no new evidence", before, p.view.Round)
	}
}

// The whole point: folding the chain must equal parsing everything from scratch.
// A rebuild from the index is the reference implementation - simple and
// obviously correct - and the fold is checked against it.
func TestFoldEqualsAFullParse(t *testing.T) {
	p := build(t, func(x *transcript) { x.AddTurn("one", false) })
	for _, turn := range []struct {
		prompt string
		agent  bool
	}{{"two", true}, {"three", false}, {"four", true}} {
		p.x.AddTurn(turn.prompt, turn.agent)
		p.x.Flush()
		p.reparse(t)
	}
	if p.view.Round < 4 {
		t.Fatalf("expected at least 4 rounds, got %d", p.view.Round)
	}

	ix, ok, err := index.Load(p.zone.IndexDir(p.x.session), p.x.session)
	if err != nil || !ok {
		t.Fatalf("index: ok=%v err=%v", ok, err)
	}
	full, err := assemble.Session(ix, assemble.Options{
		Conversation: p.x.session, Session: p.x.session,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(full.Nodes) != len(p.view.Nodes) {
		t.Errorf("a full parse has %d nodes, the fold of %d rounds has %d",
			len(full.Nodes), p.view.Round, len(p.view.Nodes))
	}
	for _, n := range full.Nodes {
		folded, ok := p.view.Nodes[n.ID]
		if !ok {
			t.Errorf("node %s is in a full parse but not in the fold", n.ID)
			continue
		}
		if folded.Kind != n.Kind || folded.Parent != n.Parent || string(folded.Attrs) != string(n.Attrs) {
			t.Errorf("node %s differs:\n  full parse %s parent=%s attrs=%s\n  fold       %s parent=%s attrs=%s",
				n.ID, n.Kind, n.Parent, n.Attrs, folded.Kind, folded.Parent, folded.Attrs)
		}
	}
	for id := range p.view.Nodes {
		var found bool
		for _, n := range full.Nodes {
			if n.ID == id {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("node %s is in the fold but not in a full parse", id)
		}
	}
	if len(full.Relations) != len(p.view.Relations) {
		t.Errorf("relations: full parse %d, fold %d", len(full.Relations), len(p.view.Relations))
	}
}

// The same landed evidence must produce the same round bytes. Without this a
// chain can only be trusted, never re-derived.
//
// The evidence is collected once and copied, rather than collected twice. A
// landed sequence is assigned as sources are found, and the order sources are
// found in is not fixed, so two independent collections of the same session
// legitimately number their files differently. Determinism is a property of
// parsing given evidence, not of collection.
func TestParsingIsReproducible(t *testing.T) {
	src := t.TempDir()
	x := newTranscript(t, src)
	x.AddTurn("one", true)
	x.AddTurn("two", false)
	x.AddCompaction()
	x.AddTurn("three", true)
	x.Flush()

	first := storage.NewZone(t.TempDir())
	if _, err := claudecode.New(src, first, 0).CollectAll(nil); err != nil {
		t.Fatal(err)
	}
	second := storage.NewZone(t.TempDir())
	copyTree(t, first.Root(), second.Root())

	digest := func(z *storage.Zone) string {
		r, err := parse.Session(z, parse.Options{Conversation: x.session, Session: x.session})
		if err != nil {
			t.Fatal(err)
		}
		return r.Digest
	}
	a, b := digest(first), digest(second)
	if a == "" {
		t.Fatal("no round was written")
	}
	if a != b {
		t.Fatalf("two parses of identical evidence produced different rounds:\n  %s\n  %s", a, b)
	}
}

// copyTree duplicates a landing zone so the same evidence can be parsed twice.
func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, body, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A round is immutable once written, and the chain checks itself.
func TestPublishedRoundsAreImmutableAndLinked(t *testing.T) {
	p := build(t, func(x *transcript) { x.AddTurn("one", true) })
	p.x.AddTurn("two", false)
	p.x.Flush()
	p.reparse(t)

	chain := sessionflow.OpenChain(p.zone.Root(), p.x.session)
	files, err := chain.Verify()
	if err != nil {
		t.Fatalf("chain verification: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d rounds, want 2", len(files))
	}
	for _, f := range files {
		fi, err := os.Stat(f.Path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o222 != 0 {
			t.Errorf("round %d is writable: %v", f.Round, fi.Mode())
		}
	}
}

// A reference that resolves later is superseded in a later round, not edited in
// an earlier one - and the record that it was once open survives.
func TestLateEvidenceSupersedesInALaterRound(t *testing.T) {
	// A child announced by a run journal before its transcript exists. This is
	// routine: the journal leads the filesystem.
	p := build(t, func(x *transcript) {
		x.AddTurn("start the work", false)
		x.AddOrphanTool()
	})

	var openAtFirst bool
	for _, u := range p.view.OpenUnresolved() {
		if u.Kind == "tool_result" {
			openAtFirst = true
		}
	}
	if !openAtFirst {
		t.Fatal("the missing result was not recorded as unresolved")
	}
	firstRound := p.view.Round

	// The result arrives.
	p.x.AddOrphanResult()
	p.x.Flush()
	p.reparse(t)

	if p.view.Round <= firstRound {
		t.Fatal("no round was written when the result arrived")
	}
	for _, u := range p.view.OpenUnresolved() {
		if u.Kind == "tool_result" {
			t.Errorf("still open after the result arrived: %s", u.ID)
		}
	}
	// The entry is superseded, not deleted: a reader must be able to see that
	// the gap existed and how it closed.
	var resolved int
	for _, u := range p.view.Unresolved {
		if u.Kind == "tool_result" && u.State == sessionflow.UnresolvedResolved {
			resolved++
		}
	}
	if resolved != 1 {
		t.Errorf("%d resolved entries kept, want 1 - absence cannot say a gap closed", resolved)
	}
}

// Every node has at most one containment parent, and that parent exists.
func TestContainmentIsASingleParentTree(t *testing.T) {
	p := build(t, func(x *transcript) {
		x.AddTurn("one", true)
		x.AddCompaction()
		x.AddTurn("two", true)
		x.AddSkillFork()
	})

	for id, n := range p.view.Nodes {
		if n.Parent == "" {
			if n.Kind != model.KindSession {
				t.Errorf("%s (%s) has no parent but is not the session", id, n.Kind)
			}
			continue
		}
		if _, ok := p.view.Nodes[n.Parent]; !ok {
			t.Errorf("%s (%s) names a parent that does not exist: %s", id, n.Kind, n.Parent)
		}
	}

	// No cycles: every node reaches a root within the depth of the hierarchy.
	for id := range p.view.Nodes {
		seen := map[string]bool{}
		cur := id
		for range 32 {
			if cur == "" || seen[cur] {
				break
			}
			seen[cur] = true
			cur = p.view.Nodes[cur].Parent
		}
		if cur != "" && seen[cur] {
			t.Errorf("containment cycle reached from %s", id)
		}
	}
}

// Cross-stream flow is never containment. A child's steps stay under the child.
func TestCrossStreamFlowIsNeverContainment(t *testing.T) {
	p := build(t, func(x *transcript) { x.AddTurn("delegate", true) })

	for id, n := range p.view.Nodes {
		if n.Stream == "" || n.Parent == "" {
			continue
		}
		parent := p.view.Nodes[n.Parent]
		if parent == nil || parent.Stream == "" {
			continue
		}
		if parent.Stream != n.Stream {
			t.Errorf("%s is on stream %s but contained by %s on stream %s",
				id, n.Stream, n.Parent, parent.Stream)
		}
	}
}

// An interrupted pass lands data twice, because the collector lands before it
// commits the cursor. That is the deliberate choice - the other order loses data
// instead of repeating it - so verification must report the repeat rather than
// call it a gap, and assembly must produce the same conversation either way.
func TestInterruptedPassRepeatsRatherThanLoses(t *testing.T) {
	src := t.TempDir()
	z := storage.NewZone(t.TempDir())
	x := newTranscript(t, src)
	x.AddTurn("one", true)
	x.AddTurn("two", false)
	x.Flush()

	if _, err := claudecode.New(src, z, 0).CollectAll(nil); err != nil {
		t.Fatal(err)
	}
	before, err := parse.Session(z, parse.Options{Conversation: x.session, Session: x.session})
	if err != nil {
		t.Fatal(err)
	}

	// Roll every cursor back to the start, which is the on-disk state a crash
	// between landing a file and committing its cursor leaves behind.
	rollBackCursors(t, z.SessionDir(x.session))
	if _, err := claudecode.New(src, z, 0).CollectAll(nil); err != nil {
		t.Fatal(err)
	}

	rep, err := verify.Session(z, x.session)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Errorf("a repeated landing was reported as %d problem(s): %v", rep.Problems, rep.Details())
	}
	if rep.Relanded == 0 {
		t.Error("the repeat was not reported at all; it must be visible")
	}

	// The repeat is new EVIDENCE, so it advances the watermark and a round is
	// written. That round is empty: the records were already known, so no entity
	// changed. Both halves matter. Without the round the chain would re-read the
	// same evidence every pass; without it being empty the conversation would
	// have doubled.
	after, err := parse.Session(z, parse.Options{Conversation: x.session, Session: x.session})
	if err != nil {
		t.Fatal(err)
	}
	if !after.Changed() {
		t.Error("re-landed evidence did not advance the chain, so it would be re-read forever")
	}
	if after.Nodes != 0 || after.Relations != 0 || after.Tombstones != 0 {
		t.Errorf("the repeat changed the conversation: %d nodes, %d relations, %d tombstones",
			after.Nodes, after.Relations, after.Tombstones)
	}
	if after.ThroughSeq <= before.ThroughSeq {
		t.Errorf("watermark did not advance: %d -> %d", before.ThroughSeq, after.ThroughSeq)
	}
	if before.Stats.Talks != after.Stats.Talks || before.Stats.ToolUses != after.Stats.ToolUses {
		t.Errorf("assembly differs after a repeat: talks %d->%d, tools %d->%d",
			before.Stats.Talks, after.Stats.Talks, before.Stats.ToolUses, after.Stats.ToolUses)
	}
}

// rollBackCursors resets every cursor in a session to its starting position.
func rollBackCursors(t *testing.T, dir string) {
	t.Helper()
	var n int
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".cursor") {
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

// Landed files plus the round chain are the whole conversation. Nothing else
// has to travel with them.
//
// This is what makes an archive, a backup or a bundle sent to someone else
// usable rather than merely readable: the index rebuilds from the landed files,
// so the conversation can be re-derived with no source files and no collector.
// And re-deriving it must not fork the chain - the same evidence produces the
// same entities, so there is nothing new to write.
func TestLandedFilesAndRoundsAreSelfSufficient(t *testing.T) {
	src := t.TempDir()
	z := storage.NewZone(t.TempDir())
	x := newTranscript(t, src)
	x.AddTurn("one", true)
	x.AddCompaction()
	x.AddTurn("two", false)
	x.Flush()

	if _, err := claudecode.New(src, z, 0).CollectAll(nil); err != nil {
		t.Fatal(err)
	}
	opts := parse.Options{
		Conversation: x.session, Session: x.session, Reindex: claudecode.RebuildIndex,
	}
	if _, err := parse.Session(z, opts); err != nil {
		t.Fatal(err)
	}
	before, err := parse.View(z.Root(), x.session)
	if err != nil {
		t.Fatal(err)
	}

	// Reduce the zone to what a bundle would carry, and take the sources away.
	for _, gone := range []string{
		z.IndexDir(x.session),
		z.IndexStatePath(x.session),
		z.SessionStatePath(x.session),
		filepath.Join(z.Root(), "_conversations", x.session, "conversation.state"),
	} {
		if err := os.RemoveAll(gone); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(src); err != nil {
		t.Fatal(err)
	}

	// It must still read.
	after, err := parse.View(z.Root(), x.session)
	if err != nil {
		t.Fatalf("the bundle could not be read: %v", err)
	}
	if after.Round != before.Round || after.Digest != before.Digest {
		t.Fatalf("the fold changed: round %d/%s became %d/%s",
			before.Round, before.Digest[:8], after.Round, after.Digest[:8])
	}
	if len(after.Nodes) != len(before.Nodes) {
		t.Errorf("nodes: %d before, %d after", len(before.Nodes), len(after.Nodes))
	}

	// And it must still parse, rebuilding the index from the landed files alone.
	again, err := parse.Session(z, opts)
	if err != nil {
		t.Fatalf("the bundle could not be parsed: %v", err)
	}
	if again.Changed() {
		t.Errorf("re-deriving from the bundle forked the chain: round %d wrote %d nodes",
			again.Number, again.Nodes)
	}
	if again.Stats.Talks != countTalks(before) {
		t.Errorf("re-derived %d talks, the chain holds %d", again.Stats.Talks, countTalks(before))
	}
}

func countTalks(v *sessionflow.View) int {
	var n int
	for _, node := range v.Nodes {
		if node.Kind == model.KindTalk {
			n++
		}
	}
	return n
}
