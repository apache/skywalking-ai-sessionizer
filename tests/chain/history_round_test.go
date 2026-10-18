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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/verify"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

const historySession = "00000002-0000-4000-8000-000000000002"

// A history root: landed files written by hand, one record each, so a test
// lands late evidence between two parses.
type historyRoot struct {
	t   *testing.T
	z   *storage.Zone
	seq uint64
}

func (r *historyRoot) land(dir, kind, stream string, rec sessiondata.Record) {
	r.t.Helper()
	r.seq++
	path := filepath.Join(dir, storage.LandedName(kind, storage.Stamp(time.Unix(int64(r.seq), 0)), r.seq))
	rec.Ord, rec.Sha, rec.Bytes = 1, "a", 1
	err := storage.WriteAtomic(path, storage.PermLanded, func(w io.Writer) error {
		sw, err := sessiondata.NewWriter(w, &sessiondata.Header{H: 1, Seq: r.seq, At: "2026-10-18T00:00:00Z",
			Kind: sessiondata.Kind(kind), Adapter: "test/0", Dialect: "test/1",
			Src: fmt.Sprintf("src-%d", r.seq), Session: historySession, Stream: stream})
		if err != nil {
			return err
		}
		if err := sw.Write(&rec); err != nil {
			return err
		}
		return sw.Close()
	})
	if err != nil {
		r.t.Fatal(err)
	}
}

func (r *historyRoot) record(rec sessiondata.Record) {
	r.land(r.z.StreamDir(historySession, storage.StreamMain), string(sessiondata.KindTranscript), storage.StreamMain, rec)
}

func (r *historyRoot) body(id, call, role string, fields map[string]any) {
	r.t.Helper()
	m := map[string]any{"schema": providerbody.Schema, "role": role, "sha256": "d", "bytes": 2, "depth": 0,
		"segments": []any{map[string]any{"lit": "{}"}}, "history_form": providerbody.FormLangChain}
	for k, v := range fields {
		m[k] = v
	}
	data, err := json.Marshal(m)
	if err != nil {
		r.t.Fatal(err)
	}
	r.land(r.z.ProviderDir(historySession), string(sessiondata.KindProviderBody), "",
		sessiondata.Record{ID: id, Call: call, Parts: []sessiondata.Part{{Kind: sessiondata.PartData, Data: data, State: "available"}}})
}

func entry(id, digest uint64) map[string]any {
	return map[string]any{"id": fmt.Sprintf("%016x", id), "digest": fmt.Sprintf("%016x", digest)}
}

// turn lands a turn call's record and its request; a nil reply lands no
// response.
func (r *historyRoot) turn(call string, reply map[string]any, entries ...map[string]any) {
	r.record(sessiondata.Record{ID: call + "-r", From: sessiondata.FromAgent, Call: call, Run: "run",
		Flags: []string{"finished", model.ClassTurnCall}, Parts: []sessiondata.Part{{Kind: sessiondata.PartText, Text: "a", State: "available"}}})
	list := make([]any, len(entries))
	for i, e := range entries {
		list[i] = e
	}
	r.body(call+":request", call, providerbody.RoleRequest, map[string]any{"messages": map[string]any{"entries": list}})
	if reply != nil {
		r.body(call+":response", call, providerbody.RoleResponse, map[string]any{"reply": reply})
	}
}

// parse parses until the chain reaches the landed files, the index built
// again from them first, as it is after files land that it does not hold.
func (r *historyRoot) parse(budget int64) []*parse.Round {
	r.t.Helper()
	for _, p := range []string{r.z.IndexDir(historySession), r.z.IndexStatePath(historySession)} {
		if err := os.RemoveAll(p); err != nil {
			r.t.Fatal(err)
		}
	}
	return parseSession(r.t, r.z, historySession, budget)
}

func parseSession(t *testing.T, z *storage.Zone, session string, budget int64) []*parse.Round {
	t.Helper()
	var rounds []*parse.Round
	for {
		rd, err := parse.Session(z, parse.Options{Conversation: session, Session: session, Reindex: index.Rebuild, MaxRoundBytes: budget})
		if err != nil {
			t.Fatal(err)
		}
		if rd.Changed() {
			rounds = append(rounds, rd)
		}
		if !rd.Changed() || !rd.More {
			return rounds
		}
		if len(rounds) > 1000 {
			t.Fatal("the chain does not reach the index")
		}
	}
}

// framesOf is every node frame the rounds wrote, by id, in order.
func framesOf(t *testing.T, z *storage.Zone, session string, rounds []*parse.Round) map[string][]sessionflow.Node {
	t.Helper()
	ch := sessionflow.OpenChain(z.Root(), session)
	out := map[string][]sessionflow.Node{}
	for _, rd := range rounds {
		r, err := ch.Open(rd.Path)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range r.Nodes {
			out[n.ID] = append(out[n.ID], n)
		}
	}
	return out
}

func attrsOf(t *testing.T, n *sessionflow.Node) map[string]any {
	t.Helper()
	var a map[string]any
	if err := json.Unmarshal(n.Attrs, &a); err != nil {
		t.Fatal(err)
	}
	return a
}

func foldOf(t *testing.T, z *storage.Zone, session string) *sessionflow.View {
	t.Helper()
	v, err := parse.View(z.Root(), session)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func verified(t *testing.T, z *storage.Zone, session string) {
	t.Helper()
	rep, err := verify.Chain(z, session, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rep.Rounds {
		for _, p := range r.Problems {
			t.Error(p)
		}
	}
	if rep.Problems != 0 {
		t.Fatalf("the chain has %d problems", rep.Problems)
	}
}

// sameNodes says the two folds hold the same nodes and relations, each
// revision number aside.
func sameNodes(t *testing.T, got, want *sessionflow.View) {
	t.Helper()
	g, w := withoutRevision(got.Nodes), withoutRevision(want.Nodes)
	for id := range w {
		if g[id] != w[id] {
			t.Errorf("node %s: %s, fresh %s", id, g[id], w[id])
		}
	}
	for id := range g {
		if _, ok := w[id]; !ok {
			t.Errorf("node %s is not in a fresh parse: %s", id, g[id])
		}
	}
	if !reflect.DeepEqual(withoutRevision(got.Relations), withoutRevision(want.Relations)) {
		t.Error("the relations differ from a fresh parse")
	}
}

// A card is published, revised and removed by rounds of the chain as its
// evidence lands: a response that lands late gives the earlier reply, and a
// late record of the earlier call's class takes that call out of the stream's
// history, so the round after it removes the card with a tombstone. The fold
// ends as a fresh parse of the same files builds it.
func TestACardIsRevisedAndRemovedAsItsEvidenceLands(t *testing.T) {
	r := &historyRoot{t: t, z: storage.NewZone(t.TempDir())}
	sys, q1, a1, q2, q3 := entry(1, 1), entry(2, 2), entry(3, 3), entry(4, 4), entry(5, 5)
	r.record(sessiondata.Record{ID: "in", From: sessiondata.FromExternal, Run: "run", Flags: []string{"external_input"},
		Parts: []sessiondata.Part{{Kind: sessiondata.PartText, Text: "q", State: "available"}}})
	r.turn("c1", a1, sys, q1)
	r.turn("x1", nil, q3) // read as a turn, so it and c2 each draw a card
	r.turn("c2", nil, sys, q1, a1, q2)

	first := r.parse(0)
	card := framesOf(t, r.z, historySession, first)["history/c2"]
	if len(card) != 1 || card[0].Tombstone {
		t.Fatalf("the first rounds wrote the card as %+v, want it once", card)
	}
	if a := attrsOf(t, &card[0]); a["from_call"] != "call/x1" || a["reply"] != model.Unavailable || a["reply_reason"] != "no_reply" {
		t.Fatalf("the first card: %v", a)
	}

	// x1's response lands: its reply, a1, is in c2's history.
	r.body("x1:response", "x1", providerbody.RoleResponse, map[string]any{"reply": a1})
	second := r.parse(0)
	card = framesOf(t, r.z, historySession, second)["history/c2"]
	if len(card) != 1 || card[0].Tombstone {
		t.Fatalf("the late response's rounds wrote the card as %+v, want one revision", card)
	}
	if a := attrsOf(t, &card[0]); a["reply"] != "back" || a["reply_reason"] != nil {
		t.Fatalf("the revised card: %v", a)
	}
	if card[0].Revision < 2 {
		t.Fatalf("the revised card is revision %d", card[0].Revision)
	}

	// A later arrival of x1, under its own record id, says it was the
	// framework's: c2 is now compared with c1, whose history it keeps.
	r.record(sessiondata.Record{ID: "x1-late", From: sessiondata.FromAgent, Call: "x1", Run: "run",
		Flags: []string{"finished", model.ClassInternalCall}})
	third := r.parse(0)
	frames := framesOf(t, r.z, historySession, third)
	// x1 drew a card of its own, against c1, and is no longer compared.
	for _, id := range []string{"history/c2", "history/x1"} {
		if card := frames[id]; len(card) != 1 || !card[0].Tombstone {
			t.Fatalf("the late class's rounds wrote %s as %+v, want a tombstone", id, card)
		}
	}
	tombstones := 0
	for _, rd := range third {
		tombstones += rd.Tombstones
	}
	if tombstones != 2 {
		t.Fatalf("the late class's rounds wrote %d tombstones, want the two cards'", tombstones)
	}
	call := frames["call/c2"]
	if len(call) != 1 {
		t.Fatalf("the late class's rounds wrote call/c2 %d times", len(call))
	}
	if a := attrsOf(t, &call[0]); a["history"] != "kept" || a["history_from"] != "call/c1" {
		t.Fatalf("call/c2 after the late class: %v", a)
	}

	fold := foldOf(t, r.z, historySession)
	if cards := fold.NodesByKind(model.KindHistoryChange); len(cards) != 0 {
		t.Fatalf("the fold still holds %d card(s)", len(cards))
	}
	verified(t, r.z, historySession)
	again := &historyRoot{t: t, z: storage.NewZone(t.TempDir())}
	if err := os.CopyFS(again.z.SessionDir(historySession), os.DirFS(r.z.SessionDir(historySession))); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(again.z.SessionDir(historySession), "index"), again.z.SessionStatePath(historySession)} {
		if err := os.RemoveAll(p); err != nil {
			t.Fatal(err)
		}
	}
	again.parse(0)
	sameNodes(t, fold, foldOf(t, again.z, historySession))
}

// A chain whose rounds carry no history, as a parser before the history was
// compared wrote it, continues under this one: the next rounds, one frame
// each under a budget of one byte, write every call's history and the card
// over no new file, and the fold ends as a fresh parse builds it.
func TestAChainWithNoHistoryGetsItInRoundsOverNoNewFile(t *testing.T) {
	sc, err := scenario.Load("../scenarios/history-forget.yaml")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	b, err := scenario.Build(sc, scenario.FormatSD, root, scenario.Options{At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	z := storage.NewZone(root)
	session := b.Session
	parseSession(t, z, session, 0)
	fresh := foldOf(t, z, session)
	cards := fresh.NodesByKind(model.KindHistoryChange)
	if len(cards) == 0 {
		t.Fatal("the scenario draws no card")
	}

	// The round an older parser would have written: no card, and no call
	// that says what its history was.
	ch := sessionflow.OpenChain(z.Root(), session)
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
	stripped := 0
	for _, id := range sortedKeys(fresh.Nodes) {
		n := *fresh.Nodes[id]
		switch n.Kind {
		case model.KindHistoryChange:
			n.Tombstone = true
		case model.KindLLMCall:
			a := attrsOf(t, &n)
			if _, ok := a["history"]; !ok {
				continue
			}
			for k := range a {
				if strings.HasPrefix(k, "history") || k == "system_changed" {
					delete(a, k)
				}
			}
			if n.Attrs, err = json.Marshal(a); err != nil {
				t.Fatal(err)
			}
			stripped++
		default:
			continue
		}
		if err := w.Node(n); err != nil {
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
	old := foldOf(t, z, session)
	if len(old.NodesByKind(model.KindHistoryChange)) != 0 {
		t.Fatal("the older chain still holds a card")
	}

	rounds := parseSession(t, z, session, 1)
	if want := stripped + len(cards); len(rounds) != want {
		t.Fatalf("the history took %d round(s), want %d, one a frame", len(rounds), want)
	}
	for _, rd := range rounds {
		if rd.FromSeq != rd.ThroughSeq+1 {
			t.Errorf("round %d covers sequences %d..%d, want no new file", rd.Number, rd.FromSeq, rd.ThroughSeq)
		}
		if rd.Nodes != 1 || rd.Relations+rd.Unresolved+rd.Tombstones != 0 {
			t.Errorf("round %d holds %d nodes, %d relations, %d unresolved entries and %d tombstones, want one node",
				rd.Number, rd.Nodes, rd.Relations, rd.Unresolved, rd.Tombstones)
		}
	}
	if again := parseSession(t, z, session, 1); len(again) != 0 {
		t.Fatalf("a parse after the history wrote %d round(s)", len(again))
	}
	sameNodes(t, foldOf(t, z, session), fresh)
	verified(t, z, session)
}
