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
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// A split upgrade publishes its revisions in several rounds, one parse each.
// A crash after the first, which also loses conversation.state, is picked up
// by the next parse from the rounds on disk: it publishes the rest, and the
// chain ends as a fresh parse builds it.
func TestASplitUpgradeContinuesAfterLosingItsState(t *testing.T) {
	old := upgradeRoot(t, true)
	first := upgradeParse(t, old, upgradeBudget)
	if !first.Changed() || !first.More {
		t.Fatalf("the first part: changed %v, more %v; want a part with more to come", first.Changed(), first.More)
	}
	if err := os.Remove(sessionflow.OpenChain(old.Root(), upgradeSession).StatePath()); err != nil {
		t.Fatal(err)
	}
	if rest := parseAll(t, old, upgradeBudget); len(rest) == 0 {
		t.Fatal("nothing was published after the state was lost")
	}
	sameAsFresh(t, old, upgradeRoot(t, false))
	chainIntact(t, old)
}

// A round written by hand, as an older parser could have, removes a node and
// a relation the evidence holds, adds a node, a relation and an open
// unresolved entry the evidence does not, and changes a node's attributes.
// The next parse, one frame a round, puts the conversation back: it adds what
// was removed, removes what was added, revises the node and resolves the
// entry, in rounds over no new file, and the chain verifies.
func TestASplitRevisionAddsRemovesAndResolves(t *testing.T) {
	z := upgradeRoot(t, false)
	parseAll(t, z, 0)
	fold, err := parse.View(z.Root(), upgradeSession)
	if err != nil {
		t.Fatal(err)
	}
	removedNode, revisedNode := pickNode(t, fold, model.KindTool), pickNode(t, fold, model.KindLLMCall)
	removedRel := sortedKeys(fold.Relations)[0]

	ch := sessionflow.OpenChain(z.Root(), upgradeSession)
	files, err := ch.List()
	if err != nil {
		t.Fatal(err)
	}
	head, err := ch.Open(files[len(files)-1].Path)
	if err != nil {
		t.Fatal(err)
	}
	h := head.Header
	h.Round, h.Previous, h.FromSeq = h.Round+1, head.Commit.Digest, h.ThroughSeq+1
	h.InputDigest = sessionflow.ChainInputDigest(h.InputDigest, nil)
	w, err := sessionflow.NewWriter(h)
	if err != nil {
		t.Fatal(err)
	}
	n := *fold.Nodes[removedNode]
	n.Tombstone = true
	r := *fold.Relations[removedRel]
	r.Tombstone = true
	changed := *fold.Nodes[revisedNode]
	changed.Attrs = json.RawMessage(`{"written_by":"an older parser"}`)
	for _, err := range []error{
		w.Node(n),
		w.Node(changed),
		w.Node(sessionflow.Node{Entity: sessionflow.Entity{ID: "test/extra"}, Kind: model.KindMessageExternal, Stream: "main"}),
		w.Relation(r),
		w.Relation(sessionflow.Relation{Entity: sessionflow.Entity{ID: "test/extra-follows"}, Type: model.RelFollows,
			From: "test/extra", To: revisedNode, Quality: model.ExactUnique}),
		w.Unresolved(sessionflow.Unresolved{Entity: sessionflow.Entity{ID: "test/open"}, Kind: model.RoleParent,
			RefID: "nothing", Reason: "written by an older parser"}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	data, digest, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ch.Publish(h.Round, digest, data); err != nil {
		t.Fatal(err)
	}

	rounds := parseAll(t, z, 1)
	if len(rounds) < 3 {
		t.Fatalf("the revision took %d round(s), want it cut frame by frame", len(rounds))
	}
	tombstones := 0
	for _, rd := range rounds {
		if rd.FromSeq != rd.ThroughSeq+1 {
			t.Errorf("round %d covers sequences %d..%d, want no new file", rd.Number, rd.FromSeq, rd.ThroughSeq)
		}
		if frames := rd.Nodes + rd.Relations + rd.Unresolved; frames != 1 {
			t.Errorf("round %d holds %d frames, want one under a budget of one byte", rd.Number, frames)
		}
		tombstones += rd.Tombstones
	}
	if tombstones != 2 {
		t.Errorf("the revision wrote %d tombstones, want the added node and relation", tombstones)
	}
	if again := upgradeParse(t, z, 1); again.Changed() {
		t.Fatalf("a parse after the revision wrote round %d", again.Number)
	}

	got, err := parse.View(z.Root(), upgradeSession)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(withoutRevision(got.Nodes), withoutRevision(fold.Nodes)) ||
		!reflect.DeepEqual(withoutRevision(got.Relations), withoutRevision(fold.Relations)) {
		t.Fatal("the nodes or relations differ from what the evidence builds")
	}
	want := withoutRevision(fold.Unresolved)
	open := got.Unresolved["test/open"]
	if open == nil || open.State != sessionflow.UnresolvedResolved {
		t.Fatalf("the hand-written open entry is %+v, want it resolved", open)
	}
	gotUnresolved := withoutRevision(got.Unresolved)
	delete(gotUnresolved, "test/open")
	if !reflect.DeepEqual(gotUnresolved, want) {
		t.Fatal("the other unresolved entries differ from what the evidence builds")
	}
	chainIntact(t, z)
}

func pickNode(t *testing.T, v *sessionflow.View, kind string) string {
	t.Helper()
	for _, id := range sortedKeys(v.Nodes) {
		if v.Nodes[id].Kind == kind {
			return id
		}
	}
	t.Fatalf("no %s node", kind)
	return ""
}

func sortedKeys[E any](m map[string]*E) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
