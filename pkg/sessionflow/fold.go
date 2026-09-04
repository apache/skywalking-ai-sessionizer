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

package sessionflow

import (
	"fmt"
	"sort"
	"sync"
)

// View is the materialised result of folding a chain.
//
// It is derived and disposable: nothing here is authoritative, the rounds are.
// Deleting a view and refolding must reproduce it exactly, which is the
// property that lets a view be cached, rebuilt or discarded freely.
type View struct {
	Conversation string
	Session      string

	// Round and Digest identify the last round folded in - a view is always a
	// view AS OF a round, never of "the chain" in general.
	Round      uint64
	Digest     string
	ThroughSeq uint64

	// InputDigest, Parser and Policy are recovered from the last round's own
	// header rather than from any state file.
	//
	// This is what makes recovery correct after a crash between publishing a
	// round and saving state. The rounds are immutable and on disk; the state
	// file is a cache that may be stale, missing, or from a different run. A
	// round built on a stale input digest would break the chain that binds each
	// round to the evidence before it.
	InputDigest string
	Parser      string
	Policy      string

	// FromTime and ThroughTime are the head round's own window, from its
	// header. The conversation's range is on the session node.
	FromTime    string
	ThroughTime string

	Nodes      map[string]*Node
	Relations  map[string]*Relation
	Unresolved map[string]*Unresolved

	// kids is a parent-to-children index, built once on first use and dropped
	// whenever a round is applied.
	//
	// Without it, finding one node's children means scanning every node, so
	// walking a subtree of 42 costs 42 times the whole conversation. Measured on
	// a real 53,106-node session that was 17 ms for a median talk and 432 ms for
	// the largest - which reads as "reading is slow" when it is only "this
	// function was written the obvious way".
	kids map[string][]*Node

	// kidsMu guards kids. A view is read by several request goroutines at once,
	// and an index built on first use is a data race that ends the process.
	kidsMu sync.Mutex
}

// Fold semantics.
//
// Three rules, and each exists because its opposite loses information:
//
//  1. LAST WRITER WINS, per entity id. A later revision of the same id fully
//     replaces the earlier one. It does not merge field-by-field: a partial
//     merge cannot express "this attribute is now absent", so a correction that
//     removes something could never be recorded.
//
//  2. ABSENCE MEANS UNCHANGED. A round carries only what it changed. An entity
//     missing from round 7 is exactly the entity round 4 published. Removal is
//     therefore explicit - a revision with Tombstone set - and never inferred
//     from silence.
//
//  3. ORDER IS CHAIN ORDER, NOT TIMESTAMP ORDER. Rounds fold in round order and
//     entities within a round in file order. Timestamps come from the observed
//     runtime and can go backwards across sources; the chain cannot.
//
// The consequence worth stating plainly: fold(rounds 1..N) is the whole
// definition of the conversation as of round N. There is no other state.

// NewView returns an empty view.
func NewView(conversation string) *View {
	return &View{
		Conversation: conversation,
		Nodes:        map[string]*Node{},
		Relations:    map[string]*Relation{},
		Unresolved:   map[string]*Unresolved{},
	}
}

// Apply folds one round into the view.
//
// It refuses a round that does not follow the view's current head: applying
// round 5 to a view that stopped at round 3 would silently skip round 4's
// revisions, and the result would look complete while being wrong.
func (v *View) Apply(r *Round) error {
	if r.Header.Round != v.Round+1 {
		return fmt.Errorf("sessionflow: cannot apply round %d to a view at round %d: rounds must fold in order",
			r.Header.Round, v.Round)
	}
	if v.Round > 0 && r.Header.Previous != v.Digest {
		return fmt.Errorf("sessionflow: round %d names previous %q, but the view's head is %q",
			r.Header.Round, firstN(r.Header.Previous, 12), firstN(v.Digest, 12))
	}
	// Conversation, session, parser and policy are frozen for the life of a
	// chain. A change to any of them is a different interpretation of different
	// data, and folding across it would silently mix the two.
	if v.Round == 0 {
		v.Session, v.Parser, v.Policy = r.Header.Session, r.Header.Parser, r.Header.Policy
		if v.Conversation == "" {
			v.Conversation = r.Header.Conversation
		}
	}
	switch {
	case r.Header.Conversation != v.Conversation:
		return fmt.Errorf("sessionflow: round %d belongs to conversation %q, the chain to %q",
			r.Header.Round, r.Header.Conversation, v.Conversation)
	case r.Header.Session != v.Session:
		return fmt.Errorf("sessionflow: round %d carries session %q, the chain %q",
			r.Header.Round, r.Header.Session, v.Session)
	case r.Header.Parser != v.Parser:
		return fmt.Errorf("sessionflow: round %d was produced by parser %q, the chain by %q; a chain is one interpretation",
			r.Header.Round, r.Header.Parser, v.Parser)
	case r.Header.Policy != v.Policy:
		return fmt.Errorf("sessionflow: round %d was produced under policy %q, the chain under %q",
			r.Header.Round, r.Header.Policy, v.Policy)
	}

	for i := range r.Nodes {
		n := r.Nodes[i]
		if n.Tombstone {
			delete(v.Nodes, n.ID)
			continue
		}
		v.Nodes[n.ID] = &n
	}
	for i := range r.Relations {
		rel := r.Relations[i]
		if rel.Tombstone {
			delete(v.Relations, rel.ID)
			continue
		}
		v.Relations[rel.ID] = &rel
	}
	for i := range r.Unresolved {
		u := r.Unresolved[i]
		if u.Tombstone {
			delete(v.Unresolved, u.ID)
			continue
		}
		// A resolved or terminal entry is kept, not deleted: it records that the
		// gap existed and how it closed. OpenUnresolved filters for what is still
		// outstanding.
		v.Unresolved[u.ID] = &u
	}

	// A round can add, replace or remove a node, so the children index no longer
	// describes the view.
	v.kidsMu.Lock()
	v.kids = nil
	v.kidsMu.Unlock()

	v.Round = r.Header.Round
	v.Digest = r.Commit.Digest
	v.ThroughSeq = r.Header.ThroughSeq
	v.InputDigest = r.Header.InputDigest
	v.FromTime, v.ThroughTime = r.Header.FromTime, r.Header.ThroughTime
	return nil
}

// Fold reads every round of a chain in order and returns the view.
func (c *Chain) Fold() (*View, error) {
	files, err := c.List()
	if err != nil {
		return nil, err
	}
	v := NewView(c.id)
	for _, rf := range files {
		r, err := c.Open(rf.Path)
		if err != nil {
			return nil, err
		}
		if err := v.Apply(r); err != nil {
			return nil, err
		}
	}
	return v, nil
}

// OpenUnresolved returns the entries still outstanding, sorted by id.
//
// Reporting these is not optional. An assembler that silently drops what it
// could not resolve presents a partial conversation as a complete one; carrying
// them as data is what makes the gaps visible to a reader.
func (v *View) OpenUnresolved() []*Unresolved {
	var out []*Unresolved
	for _, u := range v.Unresolved {
		if u.State == UnresolvedOpen {
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// NodesByKind returns the view's nodes of one kind, sorted by id.
func (v *View) NodesByKind(kind string) []*Node {
	var out []*Node
	for _, n := range v.Nodes {
		if n.Kind == kind {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Children returns the nodes whose containment parent is id, in the order they
// occurred.
//
// Order comes from each node's own landed position, not from its id and not
// from a counter. A landed record never moves, so the ordering is stable across
// rounds: late evidence that inserts an earlier node puts it in the right place
// without renumbering anything. Where two nodes share a position, or neither
// has one, the id breaks the tie so the result stays reproducible.
//
// Containment only. Cross-stream flow - starting a child, reporting its
// completion, retrying - is a relation and never a parent, so a child agent's
// steps never appear beneath the call that made them.
// It is safe to call from several goroutines at once. The index is built on
// first use and dropped whenever a round changes the view, so both the build
// and the read are taken under the same lock that Apply uses to drop it.
// Without that, two readers race on the map and the process dies.
//
// The returned slice is never modified after it is built. Applying a round
// replaces the whole index rather than editing it, so a caller may keep
// reading a slice it was given.
func (v *View) Children(id string) []*Node {
	v.kidsMu.Lock()
	defer v.kidsMu.Unlock()
	v.indexLocked()
	return v.kids[id]
}

// indexLocked builds the parent-to-children map, once. The caller holds kidsMu.
func (v *View) indexLocked() {
	if v.kids != nil {
		return
	}
	v.kids = make(map[string][]*Node, len(v.Nodes))
	for _, n := range v.Nodes {
		if n.Parent != "" {
			v.kids[n.Parent] = append(v.kids[n.Parent], n)
		}
	}
	for _, list := range v.kids {
		sort.Slice(list, func(i, j int) bool { return Before(list[i], list[j]) })
	}
}

// Roots returns the nodes with no parent inside the view, in order.
func (v *View) Roots() []*Node {
	var out []*Node
	for _, n := range v.Nodes {
		if n.Parent == "" || v.Nodes[n.Parent] == nil {
			out = append(out, n)
		}
	}
	return InOrder(out)
}

// Before reports whether a occurred before b.
//
// This is the display order the model calls a deterministic projection. It does
// not prove causality and must not be read as evidence for it.
func Before(a, b *Node) bool {
	ap, bp := a.Ref, b.Ref
	switch {
	case ap != nil && bp != nil:
		if ap.Seq != bp.Seq {
			return ap.Seq < bp.Seq
		}
		if ap.Row != bp.Row {
			return ap.Row < bp.Row
		}
		if ap.Block != nil && bp.Block != nil && *ap.Block != *bp.Block {
			return *ap.Block < *bp.Block
		}
	case ap != nil:
		return true // a is positioned, b is not
	case bp != nil:
		return false
	}
	return a.ID < b.ID
}

// InOrder returns nodes sorted the way they occurred.
func InOrder(nodes []*Node) []*Node {
	out := append([]*Node(nil), nodes...)
	sort.Slice(out, func(i, j int) bool { return Before(out[i], out[j]) })
	return out
}
