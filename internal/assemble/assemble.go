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

// Package assemble turns a session's derived index into conversation
// structure.
//
// It reads the index, not the payloads. Structure resolution needs identifiers,
// not content: removing duplicate records needs record ids, grouping a provider
// call needs message ids, joining a tool needs tool-use ids, joining a spawn
// needs agent and run ids. None of it reads message text. Payloads are read
// only when the result is rendered, and only for the records that appear in it.
//
// The whole session is assembled every time. That is what makes the output
// reproducible: the same index always produces the same entities, so a round
// can be re-derived rather than only trusted. Assembly resolves against a few
// megabytes of index and takes milliseconds, so rebuilding costs little.
package assemble

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// Options configures one assembly.
type Options struct {
	// Conversation is supplied, never inferred. An adapter reports it, or an
	// application supplies it. Grouping by person, account, timestamp proximity
	// or session id is not permitted, so this has no default here; the caller
	// applies its adapter's documented mapping.
	Conversation string
	Session      string

	// IdleGap is how long a session must be quiet before a segment becomes a
	// candidate for commit. A candidate is not a commit: about a third of long
	// gaps fall in the middle of a turn, so the gates in stage 8 decide.
	IdleGap time.Duration

	// ThroughSeq bounds assembly to landed sequences at or below it.
	//
	// Without it, assembly reads whatever the index happens to hold and the
	// caller lowers its declared watermark afterwards - so a round could contain
	// nodes and references drawn from evidence its own header, and its own input
	// digest, do not cover. Concurrent collection makes that likely rather than
	// theoretical: the index grows while assembly walks it.
	//
	// Zero means no bound, which is only correct when nothing else is writing.
	ThroughSeq uint64
}

// DefaultIdleGap is the quiet period that makes a segment a close candidate.
//
// Ten minutes is where the measurement puts the worst trade: about 39% of gaps
// that long fall inside a Talk rather than between two, against 22% at twenty
// minutes. It stays the default because the gates in stage 8 are what actually
// decide, and a shorter gap proposes more candidates for them to judge. It is
// carried in the round's policy version, so a chain built under one gap can
// never be extended under another.
const DefaultIdleGap = 10 * time.Minute

// Stats records what assembly saw. Every count carries its denominator, because
// "3 unresolved" means nothing and "3 of 8,045 tool uses" means something.
type Stats struct {
	Entries    int
	Duplicates int
	// Beyond counts indexed records above the round's watermark, which this
	// assembly deliberately did not read.
	Beyond int

	Streams int
	Epochs  int
	Talks   int
	Runs    int
	Steps   int

	ProviderCalls     int
	CallFragments     int
	CallsWithoutEnd   int // never received a terminal fragment: usage is unavailable
	SyntheticMessages int

	ToolUses       int
	ToolsResolved  int
	ToolsUnmatched int

	Spawns          int
	SpawnsResolved  int
	ChildrenOrphan  int
	Notifications   int
	NotifyUnmatched int

	SegmentsOpen      int
	SegmentsCandidate int
}

// Result is one assembly of a whole session.
type Result struct {
	Nodes      []sessionflow.Node
	Relations  []sessionflow.Relation
	Unresolved []sessionflow.Unresolved
	Stats      Stats

	// ThroughSeq is the highest landed sequence the assembly covered. It is what
	// the round header records, and what tells the next round where to resume.
	ThroughSeq uint64
}

// runKey scopes an agent loop to the stream it was written in.
type runKey struct {
	stream uint32
	run    uint32
}

// builder holds the state the eight stages share.
type builder struct {
	ix  *index.Index
	opt Options

	nodes map[string]*sessionflow.Node
	rels  map[string]*sessionflow.Relation
	unres map[string]*sessionflow.Unresolved
	stats Stats

	// canonical is the session's entries in landed order with replayed copies
	// removed. Every stage works from this, never from Entries directly.
	canonical []int32

	streams  []*streamInfo
	byStream map[uint32]*streamInfo
	calls    []*providerCall
	tools    []*toolUse
	talks    []*talk
	segments []*segment

	// spawnEdges are resolved in stage 5 and written after stage 7, because the
	// nodes they connect do not exist until then.
	spawnEdges []spawnEdge

	// container maps a landed record, by (seq, row), to the node that holds it.
	container map[[2]uint32]string

	toolByID map[uint32]*toolUse
	epochAt  map[[2]uint32]string
	talkByID map[string]*talk

	// talkOfRun maps a run to the Talk that owns it. A Talk owns the run that
	// started it and every run that followed from the work it delegated.
	//
	// The key is (stream, run) and not the run alone. A run id is NOT unique
	// across a session: a child stream is written under an id that also appears
	// in the parent, and one was observed in three different child streams.
	// Keying on the id alone puts a child's records inside the parent's turn and
	// leaves the child with no run of its own.
	talkOfRun map[runKey]string
	// runNode maps a run to its node id, keyed the same way.
	runNode map[runKey]string
	// runAt is each run's first landed position, which is what orders it.
	runAt map[runKey]sessionflow.Ref
}

// Session assembles one session into conversation structure.
func Session(ix *index.Index, opt Options) (*Result, error) {
	if opt.Conversation == "" {
		return nil, fmt.Errorf("assemble: conversation identity is supplied, never inferred")
	}
	if opt.IdleGap == 0 {
		opt.IdleGap = DefaultIdleGap
	}
	b := &builder{
		ix: ix, opt: opt,
		nodes:     map[string]*sessionflow.Node{},
		rels:      map[string]*sessionflow.Relation{},
		unres:     map[string]*sessionflow.Unresolved{},
		byStream:  map[uint32]*streamInfo{},
		talkOfRun: map[runKey]string{},
		runNode:   map[runKey]string{},
		runAt:     map[runKey]sessionflow.Ref{},
	}
	ix.Build()

	// The order is forced: each stage depends on the one before it.
	b.stage1Canonical() // duplicates removed
	b.stage2Streams()   // lineages partitioned
	b.stage3Calls()     // provider calls grouped
	b.stage4Tools()     // tool uses joined to their results
	b.stage5Spawns()    // children joined to the calls that started them
	b.stage6Epochs()    // context resets cut
	b.stage7Talks()     // talks and runs built, steps placed
	b.stage8Segments()  // commit windows proposed
	b.emitSteps()       // leaves written, now that their containers are known

	return b.result(), nil
}

// result flattens the builder into sorted, reproducible output.
//
// Sorting by id is what makes two assemblies of the same index produce the same
// bytes. Map iteration order in Go is deliberately random, so emitting in map
// order would make every round differ from the last for no reason.
func (b *builder) result() *Result {
	r := &Result{Stats: b.stats}
	for _, n := range b.nodes {
		r.Nodes = append(r.Nodes, *n)
	}
	sort.Slice(r.Nodes, func(i, j int) bool { return r.Nodes[i].ID < r.Nodes[j].ID })
	for _, v := range b.rels {
		r.Relations = append(r.Relations, *v)
	}
	sort.Slice(r.Relations, func(i, j int) bool { return r.Relations[i].ID < r.Relations[j].ID })
	for _, u := range b.unres {
		r.Unresolved = append(r.Unresolved, *u)
	}
	sort.Slice(r.Unresolved, func(i, j int) bool { return r.Unresolved[i].ID < r.Unresolved[j].ID })

	// The result's watermark is the bound it was given, not the highest sequence
	// it happened to touch. A round that read evidence 1..57 covers 1..57 even
	// if the last few records held nothing structural - otherwise the chain
	// would silently re-read them next round.
	r.ThroughSeq = b.opt.ThroughSeq
	if r.ThroughSeq == 0 {
		for _, i := range b.canonical {
			if s := uint64(b.ix.Entries[i].Seq); s > r.ThroughSeq {
				r.ThroughSeq = s
			}
		}
	}
	return r
}

// node adds or replaces a node.
func (b *builder) node(n sessionflow.Node) *sessionflow.Node {
	cp := n
	b.nodes[n.ID] = &cp
	return &cp
}

// relate records a typed edge, keyed by its endpoints so the same edge observed
// twice folds to one.
//
// Evidence that is not a real landed position is dropped rather than recorded.
// A placeholder reference is worse than no reference: it says the claim came
// from somewhere, and points at a record that does not exist.
func (b *builder) relate(typ, from, to, quality, via string, evidence ...sessionflow.Ref) {
	kept := evidence[:0]
	for _, e := range evidence {
		if e.Seq != 0 {
			kept = append(kept, e)
		}
	}
	evidence = kept
	id := sessionflow.RelationID(typ, from, to)
	if prev, ok := b.rels[id]; ok {
		// Two sources asserting the same edge is confirmation, not a conflict:
		// keep the stronger qualification and merge the evidence.
		//
		// A conflict, once recorded, stays. It says the sources disagree, and no
		// amount of further agreement makes the disagreement untrue - hiding it
		// behind a stronger-looking observation is exactly the failure the
		// qualification exists to prevent.
		switch {
		case prev.Quality == model.Conflict || quality == model.Conflict:
			prev.Quality = model.Conflict
			if prev.Via != via && via != "" {
				prev.Via = prev.Via + "; " + via
			}
		case qualityRank(quality) > qualityRank(prev.Quality):
			prev.Quality, prev.Via = quality, via
		}
		prev.Evidence = append(prev.Evidence, evidence...)
		return
	}
	b.rels[id] = &sessionflow.Relation{
		Entity: sessionflow.Entity{ID: id}, Type: typ, From: from, To: to,
		Quality: quality, Via: via, Evidence: evidence,
	}
}

// qualityRank orders the qualifications by how much they license a consumer to
// do, so that merging two observations keeps the stronger one.
//
// Conflict is deliberately absent. It is not a weak qualification to be
// outranked - it is the statement that sources disagree, which more evidence
// cannot resolve and a stronger-looking observation must not hide.
func qualityRank(q string) int {
	switch q {
	case model.ExactUnique:
		return 5
	case model.ExactAmbiguous:
		return 4
	case model.StrongInference:
		return 3
	case model.WeakInference:
		return 2
	}
	return 0
}

// open records a reference that could not be resolved.
//
// Unresolved entries are data, not noise. An assembler that drops what it could
// not resolve presents a partial conversation as a complete one.
func (b *builder) open(kind, ref, reason string) {
	id := sessionflow.UnresolvedID(kind, ref)
	if _, ok := b.unres[id]; ok {
		return
	}
	b.unres[id] = &sessionflow.Unresolved{
		Entity: sessionflow.Entity{ID: id}, Kind: kind, RefID: ref,
		Reason: reason, State: sessionflow.UnresolvedOpen,
	}
}

// ref points at one landed record.
func ref(e *index.Entry) sessionflow.Ref {
	return sessionflow.Ref{Seq: uint64(e.Seq), Row: uint64(e.Row)}
}

// blockRef points at one content block within a landed record.
func blockRef(e *index.Entry, ord uint16) sessionflow.Ref {
	o := int(ord)
	return sessionflow.Ref{Seq: uint64(e.Seq), Row: uint64(e.Row), Block: &o}
}

// attrs marshals a map deterministically. encoding/json sorts map keys, so the
// same map always produces the same bytes - which a digest chain requires.
func attrs(m map[string]any) json.RawMessage {
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}

// str resolves an interned id to its string.
func (b *builder) str(id uint32) string { return b.ix.Strings.String(id) }
