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

package chain

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/verify"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

const upgradeSession = "00000001-0000-4000-8000-000000000001"

// A chain the previous parser wrote continues under this one. Its next round
// revises the nodes whose attributes changed, covers no new landed file, and
// leaves the conversation as a fresh parse of the same files builds it. A
// second parse finds nothing to write.
//
// The ids of an epoch that a reset with an id opens, and of a segment, moved,
// so the round also removes those two nodes and the follows relation that
// named the epoch under their old ids, writes them under the new ones, and
// revises the in_segment relations whose end is the segment.
func TestAnOlderChainContinues(t *testing.T) {
	old := upgradeRoot(t, true)
	r := upgradeParse(t, old, 0)
	if r.Number != 2 || r.FromSeq != r.ThroughSeq+1 || r.More {
		t.Fatalf("the continuing round is round %d over sequences %d..%d, more=%v; want round 2 over no new file",
			r.Number, r.FromSeq, r.ThroughSeq, r.More)
	}
	if r.Unresolved != 0 {
		t.Fatalf("the continuing round writes %d unresolved entries; want none", r.Unresolved)
	}
	f, err := os.Open(r.Path)
	if err != nil {
		t.Fatal(err)
	}
	round, err := sessionflow.Read(f)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	var removed, relations []string
	for _, n := range round.Nodes {
		if n.Tombstone {
			removed = append(removed, n.ID)
		}
	}
	for _, v := range round.Relations {
		if v.Tombstone {
			removed = append(removed, v.ID)
		} else {
			relations = append(relations, v.ID)
		}
	}
	if got, want := strings.Join(removed, " "),
		"epoch/main/s8-boundary segment/at_1_2 rel/follows/epoch_main_s8-boundary/epoch_main_0"; got != want {
		t.Errorf("the continuing round removes %s; want the ids that moved, %s", got, want)
	}
	if got, want := strings.Join(relations, " "), "rel/follows/epoch_main_id_s8-boundary/epoch_main_0 "+
		"rel/in_segment/talk_a0a10ef0666c4dc7e/segment_at_1_2 rel/in_segment/talk_main_s1-cycle/segment_at_1_2 "+
		"rel/in_segment/talk_main_s8-cycle-compact/segment_at_1_2"; got != want {
		t.Errorf("the continuing round writes the relations %s; want %s", got, want)
	}
	if again := upgradeParse(t, old, 0); again.Changed() {
		t.Fatalf("a second parse wrote round %d", again.Number)
	}
	continued := sameAsFresh(t, old, upgradeRoot(t, false))
	for id, n := range continued.Nodes {
		for _, key := range []string{`"state"`, `"committable"`, `"gates_unmet"`, `"title_from"`, `"loops"`,
			`"trigger"`, `"fragments"`} {
			if bytes.Contains(n.Attrs, []byte(key)) {
				t.Errorf("node %s still carries %s: %s", id, key, n.Attrs)
			}
		}
	}
}

// upgradeRoot copies the fixture into a new root, with the old round or
// without it.
func upgradeRoot(t *testing.T, withRound bool) *storage.Zone {
	t.Helper()
	src, dst := filepath.Join("testdata", "round-upgrade"), t.TempDir()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) == ".md" {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if !withRound && strings.HasPrefix(rel, "_conversations") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(dst, filepath.Dir(rel)), 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), data, 0o444)
	})
	if err != nil {
		t.Fatal(err)
	}
	return storage.NewZone(dst)
}

// A large conversation revises many nodes on an upgrade, and a round over no
// new file cannot be narrowed. It is cut by its frames: each round of the
// revision fits the budget, the rounds together leave the conversation as a
// fresh parse builds it, and a parse after them finds nothing to write.
func TestAnUpgradeIsCutToTheBudget(t *testing.T) {
	old := upgradeRoot(t, true)
	rounds := parseAll(t, old, upgradeBudget)
	if len(rounds) < 2 {
		t.Fatalf("the revision took %d round, want it cut into several", len(rounds))
	}
	for _, r := range rounds {
		if r.FromSeq != 5 || r.ThroughSeq != 4 {
			t.Errorf("round %d covers sequences %d..%d, want no new file", r.Number, r.FromSeq, r.ThroughSeq)
		}
		if fi, err := os.Stat(r.Path); err != nil || (fi.Size() > upgradeBudget && r.Nodes+r.Relations+r.Unresolved > 1) {
			t.Errorf("round %d is over the budget: %v", r.Number, err)
		}
	}
	if again := upgradeParse(t, old, upgradeBudget); again.Changed() {
		t.Fatalf("a parse after the revision wrote round %d", again.Number)
	}
	sameAsFresh(t, old, upgradeRoot(t, false))
	chainIntact(t, old)
}

// After an upgrade, the round that lands new evidence would carry every
// revision as well, and one landed file cannot be narrowed. The revisions go
// first, in rounds of their own over no new file, and the new evidence follows
// in a round of its own.
func TestAnUpgradeGoesBeforeNewEvidence(t *testing.T) {
	old, fresh := upgradeRoot(t, true), upgradeRoot(t, false)
	for _, z := range []*storage.Zone{old, fresh} {
		landNextInput(t, z)
	}
	rounds := parseAll(t, old, upgradeBudget)
	last := rounds[len(rounds)-1]
	if len(rounds) < 2 || last.FromSeq != 5 || last.ThroughSeq != 5 {
		t.Fatalf("%d rounds, the last over sequences %d..%d; want revisions, then sequence 5",
			len(rounds), last.FromSeq, last.ThroughSeq)
	}
	for _, r := range rounds[:len(rounds)-1] {
		if r.FromSeq != 5 || r.ThroughSeq != 4 {
			t.Errorf("round %d covers sequences %d..%d before the new evidence, want no new file",
				r.Number, r.FromSeq, r.ThroughSeq)
		}
	}
	sameAsFresh(t, old, fresh)
	chainIntact(t, old)
}

// upgradeBudget is small enough that the fixture's revision does not fit one
// round.
const upgradeBudget = 4096

func upgradeParse(t *testing.T, z *storage.Zone, budget int64) *parse.Round {
	t.Helper()
	r, err := parse.Session(z, parse.Options{Conversation: upgradeSession, Session: upgradeSession,
		Reindex: index.Rebuild, MaxRoundBytes: budget})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// parseAll parses until the chain reaches the index, and returns the rounds
// written.
func parseAll(t *testing.T, z *storage.Zone, budget int64) []*parse.Round {
	t.Helper()
	var rounds []*parse.Round
	for {
		r := upgradeParse(t, z, budget)
		if r.Changed() {
			rounds = append(rounds, r)
		}
		if !r.Changed() || !r.More {
			return rounds
		}
		if len(rounds) > 1000 {
			t.Fatal("the chain does not reach the index")
		}
	}
}

// sameAsFresh parses fresh whole and checks that the continued chain folds to
// the same conversation: every node without its revision, every relation and
// every unresolved entry, and the header's Bash count.
func sameAsFresh(t *testing.T, continuedRoot, fresh *storage.Zone) *sessionflow.View {
	t.Helper()
	parseAll(t, fresh, 0)
	continued, err := parse.View(continuedRoot.Root(), upgradeSession)
	if err != nil {
		t.Fatal(err)
	}
	built, err := parse.View(fresh.Root(), upgradeSession)
	if err != nil {
		t.Fatal(err)
	}
	if continued.ThroughSeq != built.ThroughSeq {
		t.Fatalf("the continued chain reaches sequence %d, a fresh parse %d", continued.ThroughSeq, built.ThroughSeq)
	}
	for _, c := range []struct {
		what      string
		got, want map[string]string
	}{
		{"node", withoutRevision(continued.Nodes), withoutRevision(built.Nodes)},
		{"relation", withoutRevision(continued.Relations), withoutRevision(built.Relations)},
		{"unresolved entry", withoutRevision(continued.Unresolved), withoutRevision(built.Unresolved)},
	} {
		if reflect.DeepEqual(c.got, c.want) {
			continue
		}
		for id := range c.want {
			if c.got[id] != c.want[id] {
				t.Errorf("%s %s: continued %s, fresh %s", c.what, id, c.got[id], c.want[id])
			}
		}
		t.Fatalf("the continued chain folds to %d %ss, a fresh parse to %d", len(c.got), c.what, len(c.want))
	}
	if continued.BashRuns == nil || built.BashRuns == nil || *continued.BashRuns != 1 || *built.BashRuns != 1 {
		t.Fatalf("bash_runs: continued %v, fresh %v, want 1 each", continued.BashRuns, built.BashRuns)
	}
	return continued
}

// chainIntact checks the continued chain the way asz verify does.
func chainIntact(t *testing.T, z *storage.Zone) {
	t.Helper()
	rep, err := verify.Chain(z, upgradeSession, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Problems != 0 {
		for _, r := range rep.Rounds {
			for _, p := range r.Problems {
				t.Error(p)
			}
		}
		t.Fatalf("the continued chain has %d problems", rep.Problems)
	}
}

// landNextInput lands one more file in main: a person's next prompt, after the
// last record the fixture holds.
func landNextInput(t *testing.T, z *storage.Zone) {
	t.Helper()
	dir := z.StreamDir(upgradeSession, "main")
	path := filepath.Join(dir, "transcript-20261012T013740.000000000Z-000005.sd")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w, err := sessiondata.NewWriter(f, &sessiondata.Header{
		Seq: 5, At: "2026-10-12T01:37:40Z", Kind: sessiondata.KindTranscript,
		Adapter: "mock/0.2.0", Dialect: "mock/1", Session: upgradeSession, Stream: "main",
		Src: "-Users-dev-01-full-conversation/00000001-0000-4000-8000-000000000001/streams/main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(&sessiondata.Record{
		Ord: 17, Off: 5374, Sha: "0123456789ab", Bytes: 120, ID: "s9-prompt", Parent: "s8-summary",
		Run: "s9-cycle", From: sessiondata.FromExternal, Time: "2026-10-12T01:37:40.000Z",
		Flags: []string{"external_input"},
		Parts: []sessiondata.Part{{Kind: sessiondata.PartText, Text: "Now run the tests.", State: "available", Bytes: 18}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

// withoutRevision renders each entity without its revision, which counts the
// rounds that wrote it rather than saying what it is.
func withoutRevision[E any](entities map[string]*E) map[string]string {
	out := make(map[string]string, len(entities))
	for id, e := range entities {
		var m map[string]any
		b, _ := json.Marshal(e)
		_ = json.Unmarshal(b, &m)
		delete(m, "revision")
		b, _ = json.Marshal(m)
		out[id] = string(b)
	}
	return out
}
