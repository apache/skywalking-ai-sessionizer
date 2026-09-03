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

package sessionflow_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// build writes one round and returns its bytes and digest.
func build(t *testing.T, h sessionflow.Header, fill func(*sessionflow.Writer)) ([]byte, string) {
	t.Helper()
	w, err := sessionflow.NewWriter(h)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if fill != nil {
		fill(w)
	}
	data, digest, err := w.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	return data, digest
}

func header(round uint64, prev string, from, through uint64) sessionflow.Header {
	return sessionflow.Header{
		Conversation: "conv-alpha", Session: "session-01-hello",
		Round: round, Previous: prev,
		FromSeq: from, ThroughSeq: through,
		InputDigest: "d0", Parser: "v1", Policy: "v1",
	}
}

// TestChainConstantsAreFrozen covers the four header fields that must not
// change across a chain. A change to any of them means the rounds describe
// different data or a different interpretation of it, and folding across the
// change would silently mix the two.
func TestChainConstantsAreFrozen(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*sessionflow.Header)
		want string
	}{
		{"session", func(h *sessionflow.Header) { h.Session = "another-session" }, "the chain"},
		{"parser", func(h *sessionflow.Header) { h.Parser = "v2" }, "one interpretation"},
		{"policy", func(h *sessionflow.Header) { h.Policy = "v2" }, "under policy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			c := sessionflow.OpenChain(root, "conv-alpha")
			d1, g1 := build(t, header(1, "", 1, 10), nil)
			if _, err := c.Publish(1, g1, d1); err != nil {
				t.Fatal(err)
			}
			h := header(2, g1, 11, 20)
			tc.edit(&h)
			d2, g2 := build(t, h, nil)
			if _, err := c.Publish(2, g2, d2); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Fold(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("a changed %s folded without complaint, err=%v", tc.name, err)
			}
		})
	}
}

// A reference must point at a real landed position inside the range the round
// says it read.
func TestReferencesMustBeInRange(t *testing.T) {
	for _, tc := range []struct {
		name string
		ref  sessionflow.Ref
		want string
	}{
		{"zero position", sessionflow.Ref{Seq: 0, Row: 0}, "not a position"},
		{"past the watermark", sessionflow.Ref{Seq: 99, Row: 1}, "past the round's declared"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.ref
			data, _ := build(t, header(1, "", 1, 10), func(w *sessionflow.Writer) {
				_ = w.Node(sessionflow.Node{Entity: sessionflow.Entity{ID: "n/1"}, Kind: "tool", Ref: &r})
			})
			if _, err := sessionflow.Read(bytes.NewReader(data)); err == nil ||
				!strings.Contains(err.Error(), tc.want) {
				t.Fatalf("accepted %+v, err=%v", tc.ref, err)
			}
		})
	}
}

// Two frames claiming one id in a single round is ambiguous: the fold would
// keep whichever came last, with nothing saying which was meant.
func TestDuplicateIDInOneRoundRejected(t *testing.T) {
	data, _ := build(t, header(1, "", 1, 10), func(w *sessionflow.Writer) {
		_ = w.Node(sessionflow.Node{Entity: sessionflow.Entity{ID: "talk/1"}, Kind: "talk"})
		_ = w.Node(sessionflow.Node{Entity: sessionflow.Entity{ID: "talk/1"}, Kind: "run"})
	})
	if _, err := sessionflow.Read(bytes.NewReader(data)); err == nil ||
		!strings.Contains(err.Error(), "appears twice") {
		t.Fatalf("a repeated id was accepted, err=%v", err)
	}
}

// A round must verify itself: the commit digest covers every line before it.
func TestRoundSelfVerifies(t *testing.T) {
	data, digest := build(t, header(1, "", 1, 10), func(w *sessionflow.Writer) {
		if err := w.Node(sessionflow.Node{Entity: sessionflow.Entity{ID: "talk/1"}, Kind: "talk"}); err != nil {
			t.Fatal(err)
		}
	})
	r, err := sessionflow.Read(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if r.Commit.Digest != digest {
		t.Fatalf("digest %s != %s", r.Commit.Digest, digest)
	}
	if r.Commit.Counts.Nodes != 1 || len(r.Nodes) != 1 {
		t.Fatalf("counts %+v, nodes %d", r.Commit.Counts, len(r.Nodes))
	}
	if r.Nodes[0].Revision != 1 {
		t.Fatalf("revision %d, want the round number", r.Nodes[0].Revision)
	}
}

// Editing a published round must be detected, not folded.
func TestTamperedRoundRejected(t *testing.T) {
	data, _ := build(t, header(1, "", 1, 10), func(w *sessionflow.Writer) {
		_ = w.Node(sessionflow.Node{Entity: sessionflow.Entity{ID: "talk/1"}, Kind: "talk"})
	})
	edited := bytes.Replace(data, []byte(`"kind":"talk"`), []byte(`"kind":"tool"`), 1)
	if bytes.Equal(edited, data) {
		t.Fatal("test did not edit anything")
	}
	if _, err := sessionflow.Read(bytes.NewReader(edited)); err == nil ||
		!strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("edited round accepted, err=%v", err)
	}
}

// A round cut short mid-write must not read as a complete one.
func TestTruncatedRoundRejected(t *testing.T) {
	data, _ := build(t, header(1, "", 1, 10), func(w *sessionflow.Writer) {
		_ = w.Node(sessionflow.Node{Entity: sessionflow.Entity{ID: "talk/1"}, Kind: "talk"})
	})
	cut := data[:bytes.LastIndexByte(data[:len(data)-1], '\n')+1]
	if _, err := sessionflow.Read(bytes.NewReader(cut)); err == nil ||
		!strings.Contains(err.Error(), "truncated") {
		t.Fatalf("truncated round accepted, err=%v", err)
	}
}

// Same previous digest + same inputs + same parser version = same bytes.
// Without this a chain cannot be re-derived, only trusted.
func TestRoundIsDeterministic(t *testing.T) {
	fill := func(w *sessionflow.Writer) {
		_ = w.Node(sessionflow.Node{Entity: sessionflow.Entity{ID: "talk/1"}, Kind: "talk"})
		_ = w.Relation(sessionflow.Relation{
			Entity: sessionflow.Entity{ID: sessionflow.RelationID("spawn", "run/1", "stream/child")},
			Type:   "spawn", From: "run/1", To: "stream/child", Quality: "exact_unique",
		})
	}
	a, da := build(t, header(2, "abc", 11, 20), fill)
	b, db := build(t, header(2, "abc", 11, 20), fill)
	if !bytes.Equal(a, b) || da != db {
		t.Fatalf("same inputs produced different rounds:\n%s\n%s", a, b)
	}
}

// A round file must never be replaced: later rounds' digests depend on it.
func TestPublishRefusesToReplace(t *testing.T) {
	root := t.TempDir()
	c := sessionflow.OpenChain(root, "conv-alpha")
	data, digest := build(t, header(1, "", 1, 10), nil)
	if _, err := c.Publish(1, digest, data); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	if _, err := c.Publish(1, digest, data); err == nil {
		t.Fatal("republishing round 1 was accepted")
	}
	// A second builder that read the same head would produce a round 1 with a
	// DIFFERENT digest, so its filename would not collide. The head check is
	// what refuses it; a name check alone would let the chain fork.
	other, otherDigest := build(t, header(1, "", 1, 10), func(w *sessionflow.Writer) {
		_ = w.Node(sessionflow.Node{Entity: sessionflow.Entity{ID: "talk/2"}, Kind: "talk"})
	})
	if otherDigest == digest {
		t.Fatal("test setup produced the same digest twice")
	}
	if _, err := c.Publish(1, otherDigest, other); err == nil {
		t.Fatal("a forking round 1 with a different digest was accepted")
	}
	files, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(files[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o222 != 0 {
		t.Fatalf("published round is writable: %v", fi.Mode())
	}
}

// chainOf publishes n rounds, each naming its predecessor's digest.
func chainOf(t *testing.T, root string, n int, fill func(round uint64, w *sessionflow.Writer)) *sessionflow.Chain {
	t.Helper()
	c := sessionflow.OpenChain(root, "conv-alpha")
	prev := ""
	for i := 1; i <= n; i++ {
		round := uint64(i)
		from, through := uint64(i*10-9), uint64(i*10)
		data, digest := build(t, header(round, prev, from, through), func(w *sessionflow.Writer) {
			if fill != nil {
				fill(round, w)
			}
		})
		if _, err := c.Publish(round, digest, data); err != nil {
			t.Fatalf("publish round %d: %v", i, err)
		}
		prev = digest
	}
	return c
}

func TestChainVerifies(t *testing.T) {
	c := chainOf(t, t.TempDir(), 4, func(_ uint64, w *sessionflow.Writer) {
		_ = w.Node(sessionflow.Node{Entity: sessionflow.Entity{ID: "talk/1"}, Kind: "talk"})
	})
	files, err := c.Verify()
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(files) != 4 {
		t.Fatalf("got %d rounds", len(files))
	}
}

// A missing round breaks the chain: its successors are unverifiable.
func TestChainDetectsMissingRound(t *testing.T) {
	root := t.TempDir()
	c := chainOf(t, root, 3, nil)
	files, _ := c.List()
	if err := os.Chmod(files[1].Path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(files[1].Path); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Verify(); err == nil || !strings.Contains(err.Error(), "expected round 2") {
		t.Fatalf("gap accepted, err=%v", err)
	}
}

// A round that names the wrong predecessor is a forked chain, not a chain.
func TestChainDetectsBrokenLink(t *testing.T) {
	root := t.TempDir()
	c := chainOf(t, root, 2, nil)
	// Publish a round 3 that names a predecessor digest nobody produced.
	data, digest := build(t, header(3, strings.Repeat("f", 64), 21, 30), nil)
	if _, err := c.Publish(3, digest, data); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Verify(); err == nil || !strings.Contains(err.Error(), "names previous") {
		t.Fatalf("broken link accepted, err=%v", err)
	}
}

// Skipping landed evidence between rounds must be caught: no digest reveals it.
func TestChainDetectsSkippedEvidence(t *testing.T) {
	root := t.TempDir()
	c := sessionflow.OpenChain(root, "conv-alpha")
	d1, g1 := build(t, header(1, "", 1, 10), nil)
	if _, err := c.Publish(1, g1, d1); err != nil {
		t.Fatal(err)
	}
	d2, g2 := build(t, header(2, g1, 15, 20), nil) // 11..14 never consumed
	if _, err := c.Publish(2, g2, d2); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Verify(); err == nil || !strings.Contains(err.Error(), "evidence was skipped") {
		t.Fatalf("gap in consumed evidence accepted, err=%v", err)
	}
}

// The whole point of the design: fold(1..N) is the conversation.
func TestFoldSupersedesAndKeepsUnchanged(t *testing.T) {
	root := t.TempDir()
	c := chainOf(t, root, 3, func(round uint64, w *sessionflow.Writer) {
		switch round {
		case 1:
			// A tool call whose result has not arrived yet.
			_ = w.Node(sessionflow.Node{Entity: sessionflow.Entity{ID: "tool/t1"}, Kind: "tool",
				Attrs: json.RawMessage(`{"name":"Bash","result":"unavailable"}`)})
			_ = w.Node(sessionflow.Node{Entity: sessionflow.Entity{ID: "talk/1"}, Kind: "talk"})
			_ = w.Unresolved(sessionflow.Unresolved{Entity: sessionflow.Entity{ID: sessionflow.UnresolvedID("tool_result", "t1")},
				Kind: "tool_result", RefID: "tool/t1"})
		case 2:
			// Round 2 touches nothing: talk/1 and tool/t1 must survive.
			_ = w.Node(sessionflow.Node{Entity: sessionflow.Entity{ID: "talk/2"}, Kind: "talk"})
		case 3:
			// Late evidence supersedes the earlier revision in a LATER round.
			_ = w.Node(sessionflow.Node{Entity: sessionflow.Entity{ID: "tool/t1"}, Kind: "tool",
				Attrs: json.RawMessage(`{"name":"Bash","result":"ok"}`)})
			_ = w.Unresolved(sessionflow.Unresolved{Entity: sessionflow.Entity{ID: sessionflow.UnresolvedID("tool_result", "t1")},
				Kind: "tool_result", RefID: "tool/t1", State: sessionflow.UnresolvedResolved})
		}
	})

	v, err := c.Fold()
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if v.Round != 3 {
		t.Fatalf("view at round %d", v.Round)
	}
	if len(v.Nodes) != 3 {
		t.Fatalf("got %d nodes, want 3 (talk/1 survived rounds it was absent from)", len(v.Nodes))
	}
	if got := string(v.Nodes["tool/t1"].Attrs); !strings.Contains(got, `"result":"ok"`) {
		t.Fatalf("tool/t1 not superseded: %s", got)
	}
	if rev := v.Nodes["tool/t1"].Revision; rev != 3 {
		t.Fatalf("revision %d, want 3 (the round that produced it)", rev)
	}
	if n := len(v.OpenUnresolved()); n != 0 {
		t.Fatalf("%d still open after resolution", n)
	}
	if len(v.Unresolved) != 1 {
		t.Fatal("the resolved entry was deleted; it must remain as a record that the gap existed")
	}
}

// Removal must be explicit. Absence is "unchanged", never "deleted".
func TestTombstoneRemoves(t *testing.T) {
	root := t.TempDir()
	c := chainOf(t, root, 2, func(round uint64, w *sessionflow.Writer) {
		if round == 1 {
			_ = w.Node(sessionflow.Node{Entity: sessionflow.Entity{ID: "talk/1"}, Kind: "talk"})
		} else {
			_ = w.Node(sessionflow.Node{Entity: sessionflow.Entity{ID: "talk/1", Tombstone: true}})
		}
	})
	v, err := c.Fold()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := v.Nodes["talk/1"]; ok {
		t.Fatal("tombstoned node survived the fold")
	}
}

// Folding out of order would silently skip a round's revisions.
func TestFoldRefusesOutOfOrder(t *testing.T) {
	v := sessionflow.NewView("conv-alpha")
	data, _ := build(t, header(2, "abc", 11, 20), nil)
	r, err := sessionflow.Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Apply(r); err == nil || !strings.Contains(err.Error(), "must fold in order") {
		t.Fatalf("out-of-order apply accepted, err=%v", err)
	}
}

// Round 1 has no predecessor; every later round must have one.
func TestHeaderValidation(t *testing.T) {
	ok := func(f func(*sessionflow.Header)) sessionflow.Header {
		h := sessionflow.Header{
			Conversation: "c", Session: "s", Round: 1, FromSeq: 1, ThroughSeq: 10,
			InputDigest: "d", Parser: "v1", Policy: "v1",
		}
		f(&h)
		return h
	}
	for _, tc := range []struct {
		name string
		h    sessionflow.Header
		want string
	}{
		{"round 0", ok(func(h *sessionflow.Header) { h.Round = 0 }), "count from 1"},
		{"no previous", ok(func(h *sessionflow.Header) { h.Round = 2 }), "no previous digest"},
		{"round 1 with previous", ok(func(h *sessionflow.Header) { h.Previous = "x" }), "must not name a previous"},
		{"no conversation", ok(func(h *sessionflow.Header) { h.Conversation = "" }), "missing conversation"},
		{"no session", ok(func(h *sessionflow.Header) { h.Session = "" }), "missing session"},
		{"no parser", ok(func(h *sessionflow.Header) { h.Parser = "" }), "missing parser"},
		{"no policy", ok(func(h *sessionflow.Header) { h.Policy = "" }), "missing policy"},
		{"no input digest", ok(func(h *sessionflow.Header) { h.InputDigest = "" }), "missing input digest"},
		{"from_seq 0", ok(func(h *sessionflow.Header) { h.FromSeq = 0 }), "count from 1"},
		{"backwards range", ok(func(h *sessionflow.Header) { h.FromSeq, h.ThroughSeq = 20, 10 }), "not a range"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := sessionflow.NewWriter(tc.h); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("accepted %+v, err=%v", tc.h, err)
			}
		})
	}
}

// An entity with no id could never be superseded by a later revision.
func TestEntityRequiresID(t *testing.T) {
	w, err := sessionflow.NewWriter(header(1, "", 1, 10))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Node(sessionflow.Node{Kind: "talk"}); err == nil {
		t.Fatal("node without an id accepted")
	}
	if err := w.Relation(sessionflow.Relation{Type: "spawn"}); err == nil {
		t.Fatal("relation without an id accepted")
	}
}

// An empty round is valid: it records that a pass looked and found nothing new.
func TestEmptyRoundIsValid(t *testing.T) {
	data, _ := build(t, header(1, "", 1, 10), nil)
	r, err := sessionflow.Read(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("empty round rejected: %v", err)
	}
	if r.Commit.Counts != (sessionflow.Counts{}) {
		t.Fatalf("counts %+v", r.Commit.Counts)
	}
}

// The input digest chains rather than recomputing, and does not depend on the
// order sources were discovered in.
func TestInputDigestChains(t *testing.T) {
	a := sessionflow.ChainInputDigest("seed", []string{"h1", "h2"})
	b := sessionflow.ChainInputDigest("seed", []string{"h2", "h1"})
	if a != b {
		t.Fatal("input digest depends on discovery order")
	}
	if sessionflow.ChainInputDigest(a, []string{"h3"}) == sessionflow.ChainInputDigest("seed", []string{"h1", "h2", "h3"}) {
		t.Fatal("chained and flat digests collide; the chain is not binding the predecessor")
	}
	if sessionflow.ChainInputDigest("seed", nil) == sessionflow.ChainInputDigest("other", nil) {
		t.Fatal("an empty round does not carry its predecessor's input digest")
	}
}

// Chain state is the mutable head; rounds stay immutable and time-free.
func TestChainStateRoundTrips(t *testing.T) {
	root := t.TempDir()
	c := sessionflow.OpenChain(root, "conv-alpha")
	s, err := c.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if s.Head != 0 {
		t.Fatalf("fresh state at head %d", s.Head)
	}
	s.Head, s.HeadDigest, s.ThroughSeq = 7, strings.Repeat("a", 64), 120
	s.InputDigest, s.Parser, s.Policy = "idg", "v1", "v1"
	if err := c.SaveState(s, time.Unix(1750000000, 0)); err != nil {
		t.Fatal(err)
	}
	got, err := c.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if got.Head != 7 || got.HeadDigest != s.HeadDigest || got.ThroughSeq != 120 || got.Parser != "v1" {
		t.Fatalf("round-trip lost data: %+v", got)
	}
	if got.UpdatedAt == "" {
		t.Fatal("state did not record when it was written")
	}
}

// The head pointer is a cache; the filesystem is the authority. A crash between
// publishing and saving state must not lose a round.
func TestListSeesRoundsStateDoesNot(t *testing.T) {
	root := t.TempDir()
	c := chainOf(t, root, 2, nil)
	files, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d rounds with no state file written at all", len(files))
	}
	if _, err := os.Stat(filepath.Join(root, "_conversations", "conv-alpha", "conversation.state")); !os.IsNotExist(err) {
		t.Fatal("test setup wrote state; it should not have")
	}
}

// Guard the constant the rest of the tree writes rounds with.
func TestPublishedRoundPermIsLanded(t *testing.T) {
	if storage.PermLanded&0o222 != 0 {
		t.Fatalf("PermLanded is writable: %v", storage.PermLanded)
	}
}
