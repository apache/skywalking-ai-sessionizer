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

// Package parse drives one round of assembly and publishes it to a
// conversation's round chain.
//
// Each round does the same three things: assemble the whole session from the
// index, compare that against what the chain already says, and write only the
// difference. Assembling everything each time is what makes a round
// reproducible - the same index always produces the same entities - and
// comparing before writing is what keeps a round small.
package parse

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/assemble"
	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// Parser is the interpretation version written into every round header.
//
// A change here that alters meaning starts a new chain generation. It does not
// retroactively correct earlier rounds and must not be folded together with
// them, so the version travels in the header where a reader can see the mix.
const Parser = "v1"

// policyBase is the version of the choices that are settings rather than
// interpretation: the segment gates, and how a commit window is proposed.
const policyBase = "v1"

// DefaultIdleGap is the quiet period that makes a segment a close candidate.
//
// It is part of the POLICY, not a free parameter, because changing it changes
// which segments a round proposes. A chain is one interpretation of one body of
// evidence, so a round produced under a different gap must not fold together
// with rounds produced under this one.
const DefaultIdleGap = assemble.DefaultIdleGap

// policyFor renders the effective policy, settings included.
//
// A version string that stayed "v1" while a setting changed underneath it would
// be worse than no version at all: the chain would look consistent and would
// not be.
func policyFor(idle time.Duration) string {
	if idle == 0 {
		idle = DefaultIdleGap
	}
	return fmt.Sprintf("%s+idle=%s", policyBase, idle)
}

// DefaultMaxRoundBytes bounds a round file. A round travels whole as one log
// record when a root is exported, so it is cut at the same budget as a landed
// file. A round's size follows its input window, so the parser narrows the
// window until the round fits and leaves the rest of the evidence to the next
// round.
const DefaultMaxRoundBytes = 2 << 20

// Options configures one parse round.
type Options struct {
	Conversation string
	Session      string
	IdleGap      time.Duration

	// MaxRoundBytes is the largest round file to publish. Zero means
	// DefaultMaxRoundBytes. A round covering a single landed file is
	// published whole even when it is larger, as a single source record
	// larger than the landing budget is landed whole.
	MaxRoundBytes int64

	// Now supplies the clock for chain state. Rounds themselves carry no time,
	// so this never reaches a digest.
	Now func() time.Time

	// Reindex rebuilds a session's index by re-reading its landed files.
	//
	// It is what makes the index genuinely disposable. Without it the index can
	// only be built while collecting, so landed data separated from its source -
	// an archive, a bundle sent to someone else, a restored backup - could be
	// read but never parsed again, even though every byte needed is present.
	//
	// Nil means index.Rebuild, which reads the converted records and needs no
	// adapter. A transport that keeps landed data somewhere other than the
	// zone supplies its own.
	Reindex func(z *storage.Zone, session string, ix *index.Index, afterSeq uint64) (int, error)
}

// Round reports what one parse round did.
type Round struct {
	Session      string
	Conversation string

	// Number is 0 when nothing changed and no round was written.
	Number uint64
	Digest string
	Path   string

	FromSeq    uint64
	ThroughSeq uint64

	// More reports that the round was cut at the byte budget and evidence
	// past ThroughSeq is indexed already. A caller that wants the chain to
	// reach the index calls Session again until More is false.
	More bool

	Nodes      int
	Relations  int
	Unresolved int
	Tombstones int

	Stats assemble.Stats
}

// Changed reports whether the round wrote anything.
func (r *Round) Changed() bool { return r.Number != 0 }

// Session parses one session and appends a round to its conversation chain.
//
// It is safe to call when nothing has changed: the comparison finds no
// difference and no round is written, so a watch loop does not grow the chain
// with empty rounds.
func Session(z *storage.Zone, opt Options) (*Round, error) {
	if opt.Reindex == nil {
		opt.Reindex = index.Rebuild
	}
	if opt.Conversation == "" {
		return nil, fmt.Errorf("parse: conversation identity is supplied, never inferred")
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}

	ix, ok, err := index.Load(z.IndexDir(opt.Session), opt.Session)
	if err != nil {
		return nil, err
	}
	if !ok {
		ix, err = rebuild(z, opt)
		if err != nil {
			return nil, err
		}
	}

	// The index must not be read past what it actually covers. Its own state
	// records that; parsing beyond it would describe records the index does not
	// hold.
	ixState, err := storage.LoadIndexState(z.IndexStatePath(opt.Session), opt.Session)
	if err != nil {
		return nil, err
	}

	// The watermark is fixed BEFORE assembly and assembly is bounded by it.
	// Deciding afterwards would let a round contain nodes drawn from evidence
	// its own header and input digest do not cover, which a concurrent
	// collector makes likely rather than theoretical.
	through := ixState.IndexedSeq
	if through == 0 {
		for i := range ix.Entries {
			if s := uint64(ix.Entries[i].Seq); s > through {
				through = s
			}
		}
	}

	policy := policyFor(opt.IdleGap)
	chain := sessionflow.OpenChain(z.Root(), opt.Conversation)

	// Publishing is a read followed by a write - decide the next round from what
	// is on disk, then create it. Two builders doing that at once would both
	// read round N and both write an N+1, and because the digest is part of the
	// filename their files would not even collide.
	lock, err := chain.Lock()
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Unlock() }()

	// The filesystem is the authority on how far the chain got. A crash between
	// publishing a round and saving state leaves a round on disk that state does
	// not mention, and folding must see it.
	view, err := chain.Fold()
	if err != nil {
		return nil, err
	}
	if view.Round > 0 {
		if view.Parser != Parser || view.Policy != policy {
			return nil, fmt.Errorf(
				"parse: this chain was built by parser %q under policy %q; this build is %q/%q. "+
					"A chain is one interpretation of one body of evidence, so it cannot be extended "+
					"across a change to either", view.Parser, view.Policy, Parser, policy)
		}
		if view.Session != opt.Session {
			return nil, fmt.Errorf("parse: this chain carries session %q, not %q", view.Session, opt.Session)
		}
	}

	out := &Round{
		Session: opt.Session, Conversation: opt.Conversation,
		FromSeq: view.ThroughSeq + 1, ThroughSeq: through,
	}
	if through < view.ThroughSeq {
		return nil, fmt.Errorf("parse: the chain covers landed sequence %d but the index only reaches %d",
			view.ThroughSeq, through)
	}

	budget := opt.MaxRoundBytes
	if budget <= 0 {
		budget = DefaultMaxRoundBytes
	}
	indexed := through
	round := view.Round + 1
	var (
		d           *delta
		data        []byte
		digest      string
		inputDigest string
	)
	for {
		res, err := assemble.Session(ix, assemble.Options{
			Conversation: opt.Conversation,
			Session:      opt.Session,
			IdleGap:      opt.IdleGap,
			ThroughSeq:   through,
		})
		if err != nil {
			return nil, err
		}
		out.Stats = res.Stats
		d = diff(view, res)

		// A round is written when EVIDENCE advanced, not only when the
		// structure changed. Evidence that produced no new entity - a replayed
		// block, a record type nothing reads - still has to move the chain's
		// watermark, or the next round re-reads it forever and "no round" comes
		// to mean two different things: nothing new arrived, and nothing new
		// mattered.
		if d.empty() && through <= view.ThroughSeq {
			out.ThroughSeq = view.ThroughSeq
			return out, nil
		}

		// The input digest chains from the LAST ROUND's own header, not from
		// the state file. State is a cache and may be stale or absent after a
		// crash; chaining from it would break the link binding each round to
		// the evidence before it.
		inputDigest, err = inputDigestFor(z, opt.Session, view.InputDigest, view.ThroughSeq, through)
		if err != nil {
			return nil, err
		}
		fromTime, throughTime := windowTimes(ix, view.ThroughSeq, through)
		sessionFrom, sessionThrough := sessionRange(res, opt.Session)
		w, err := sessionflow.NewWriter(sessionflow.Header{
			Conversation: opt.Conversation, Session: opt.Session,
			Round: round, Previous: view.Digest,
			FromSeq: view.ThroughSeq + 1, ThroughSeq: through,
			InputDigest: inputDigest, Parser: Parser, Policy: policy,
			FromTime: fromTime, ThroughTime: throughTime,
			SessionFromTime: sessionFrom, SessionThroughTime: sessionThrough,
		})
		if err != nil {
			return nil, err
		}
		for _, n := range d.nodes {
			if err := w.Node(n); err != nil {
				return nil, err
			}
		}
		for _, r := range d.relations {
			if err := w.Relation(r); err != nil {
				return nil, err
			}
		}
		for _, u := range d.unresolved {
			if err := w.Unresolved(u); err != nil {
				return nil, err
			}
		}
		data, digest, err = w.Close()
		if err != nil {
			return nil, err
		}
		// A round over the budget is built again over a narrower window,
		// halved each time, until it fits or covers a single landed file.
		// Nothing is lost: the evidence past the window is still indexed and
		// the next round starts where this one stops.
		if int64(len(data)) <= budget || through <= out.FromSeq {
			break
		}
		through = out.FromSeq + (through-out.FromSeq)/2
	}
	out.ThroughSeq = through
	out.More = through < indexed
	path, err := chain.Publish(round, digest, data)
	if err != nil {
		return nil, err
	}

	// State is saved after the round is on disk. The reverse order would leave
	// state pointing at a round that does not exist.
	state := &sessionflow.State{Schema: 1, Conversation: opt.Conversation}
	state.Head, state.HeadDigest = round, digest
	state.ThroughSeq, state.InputDigest = through, inputDigest
	state.Parser, state.Policy = Parser, policy
	if err := chain.SaveState(state, opt.Now()); err != nil {
		return nil, err
	}

	out.Number, out.Digest, out.Path = round, digest, path
	out.Nodes, out.Relations, out.Unresolved = len(d.nodes), len(d.relations), len(d.unresolved)
	out.Tombstones = d.tombstones
	return out, nil
}

// rebuild reconstructs a missing index from the landed files themselves.
//
// The rebuilt index is written back, so this cost is paid once rather than on
// every round. It is safe to write because the index is derived: if it is wrong
// or stale, deleting it and coming back here reproduces it.
func rebuild(z *storage.Zone, opt Options) (*index.Index, error) {
	if opt.Reindex == nil {
		return nil, fmt.Errorf("parse: no index for session %s, and no way to rebuild one", opt.Session)
	}
	ix := index.New(opt.Session)
	n, err := opt.Reindex(z, opt.Session, ix, 0)
	if err != nil {
		return nil, fmt.Errorf("parse: rebuild index for %s: %w", opt.Session, err)
	}
	if n == 0 {
		return nil, fmt.Errorf("parse: no landed records for session %s", opt.Session)
	}
	if err := ix.Write(z.IndexDir(opt.Session)); err != nil {
		return nil, err
	}
	st := storage.NewIndexState(opt.Session)
	st.Schema, st.Entries = index.Schema, len(ix.Entries)
	for i := range ix.Entries {
		if s := uint64(ix.Entries[i].Seq); s > st.IndexedSeq {
			st.IndexedSeq = s
		}
	}
	if err := st.Save(z.IndexStatePath(opt.Session), opt.Now()); err != nil {
		return nil, err
	}
	return ix, nil
}

// inputDigestFor extends the chain's input digest with the evidence this round
// newly consumed.
//
// Only files above the previous watermark are read, so the cost is proportional
// to what is new rather than to the whole conversation. A landed file is written
// once and never appended to, so its digest is fixed from the moment it appears.
func inputDigestFor(z *storage.Zone, session, previous string, after, through uint64) (string, error) {
	files, err := storage.LandedFiles(z, session)
	if err != nil {
		return "", err
	}
	var added []string
	for _, f := range files {
		if f.Seq <= after || f.Seq > through {
			continue
		}
		d, err := storage.FileDigest(f.Path)
		if err != nil {
			return "", err
		}
		added = append(added, d)
	}
	return sessionflow.ChainInputDigest(previous, added), nil
}

// windowTimes is the earliest and the latest record time among the landed
// files a round consumes, from the index. They are what the runtime wrote,
// so they are evidence and reproduce with the round. Both are empty when no
// record in the window carries a time.
func windowTimes(ix *index.Index, after, through uint64) (from, throughTime string) {
	var lo, hi int64
	ok := false
	for i := range ix.Entries {
		e := &ix.Entries[i]
		if uint64(e.Seq) <= after || uint64(e.Seq) > through || e.TS == 0 {
			continue
		}
		if !ok || e.TS < lo {
			lo = e.TS
		}
		if !ok || e.TS > hi {
			hi = e.TS
		}
		ok = true
	}
	if !ok {
		return "", ""
	}
	return sessiondata.FormatTime(lo), sessiondata.FormatTime(hi)
}

// sessionRange reads the session's own time range off the session node the
// assembler built, so the header repeats exactly what the node carries.
func sessionRange(res *assemble.Result, session string) (from, through string) {
	id := sessionflow.NodeID("session", session)
	for i := range res.Nodes {
		if res.Nodes[i].ID != id || len(res.Nodes[i].Attrs) == 0 {
			continue
		}
		var a struct {
			From    string `json:"from_time"`
			Through string `json:"through_time"`
		}
		_ = json.Unmarshal(res.Nodes[i].Attrs, &a)
		return a.From, a.Through
	}
	return "", ""
}

// delta is what one round must write.
type delta struct {
	nodes      []sessionflow.Node
	relations  []sessionflow.Relation
	unresolved []sessionflow.Unresolved
	tombstones int
}

func (d *delta) empty() bool {
	return len(d.nodes) == 0 && len(d.relations) == 0 && len(d.unresolved) == 0
}

// diff compares a fresh assembly against what the chain already says.
//
// Only what changed is written. An entity the assembly produced unchanged is
// left out, because absence in a round means unchanged - which is exactly what
// keeps a round proportional to new work rather than to the whole conversation.
//
// An entity the chain holds that the assembly no longer produces gets a
// tombstone. Silence would mean unchanged, so removal has to be said.
func diff(view *sessionflow.View, res *assemble.Result) *delta {
	d := &delta{}

	seenNode := map[string]bool{}
	for _, n := range res.Nodes {
		seenNode[n.ID] = true
		if prev, ok := view.Nodes[n.ID]; ok && sameNode(prev, &n) {
			continue
		}
		d.nodes = append(d.nodes, n)
	}
	for id, prev := range view.Nodes {
		if !seenNode[id] {
			d.nodes = append(d.nodes, sessionflow.Node{Entity: sessionflow.Entity{ID: id, Tombstone: true}})
			d.tombstones++
			_ = prev
		}
	}

	seenRel := map[string]bool{}
	for _, r := range res.Relations {
		seenRel[r.ID] = true
		if prev, ok := view.Relations[r.ID]; ok && sameRelation(prev, &r) {
			continue
		}
		d.relations = append(d.relations, r)
	}
	for id := range view.Relations {
		if !seenRel[id] {
			d.relations = append(d.relations, sessionflow.Relation{Entity: sessionflow.Entity{ID: id, Tombstone: true}})
			d.tombstones++
		}
	}

	seenUnres := map[string]bool{}
	for _, u := range res.Unresolved {
		seenUnres[u.ID] = true
		if prev, ok := view.Unresolved[u.ID]; ok && sameUnresolved(prev, &u) {
			continue
		}
		d.unresolved = append(d.unresolved, u)
	}
	// An entry the assembly stopped producing has been resolved. It is superseded
	// with that state rather than removed: a reader must be able to see that the
	// gap existed and how it closed, and absence cannot say that.
	for id, prev := range view.Unresolved {
		if seenUnres[id] || prev.State != sessionflow.UnresolvedOpen {
			continue
		}
		done := *prev
		done.State = sessionflow.UnresolvedResolved
		d.unresolved = append(d.unresolved, done)
	}

	sortDelta(d)
	return d
}

// Comparing an assembled entity against a folded one has to ignore the two
// fields that are not about the entity at all.
//
// Revision is derived from chain position, so comparing it would make every
// entity look changed in every round. The frame tag is set by the writer, so a
// folded entity carries it and a freshly assembled one does not - comparing it
// would ALSO make every entity look changed, and every round would rewrite the
// whole conversation.
func normalize(e *sessionflow.Entity) {
	e.Revision, e.T = 0, ""
}

func sameNode(a, b *sessionflow.Node) bool {
	x, y := *a, *b
	normalize(&x.Entity)
	normalize(&y.Entity)
	return jsonEqual(x, y)
}

func sameRelation(a, b *sessionflow.Relation) bool {
	x, y := *a, *b
	normalize(&x.Entity)
	normalize(&y.Entity)
	return jsonEqual(x, y)
}

func sameUnresolved(a, b *sessionflow.Unresolved) bool {
	x, y := *a, *b
	normalize(&x.Entity)
	normalize(&y.Entity)
	return jsonEqual(x, y)
}

func jsonEqual(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(x) == string(y)
}
