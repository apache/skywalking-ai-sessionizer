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

package index

import "sort"

// Index is a session's derived lookup structure.
//
// Entries are held in landed order, so a stream's records are already in the
// order the assembler must reason about. Lookup maps are rebuilt on load rather
// than persisted: rebuilding every map for the largest observed session takes
// about 4 ms, which is not worth the format complexity of storing them.
type Index struct {
	Session string
	Strings *Interner
	Entries []Entry
	Blocks  []Block

	byRecord    map[uint32]int32
	byMsg       map[uint32][]int32
	byToolBlock map[uint32][]int32 // tool id -> the content blocks carrying it
	byBatch     map[uint32][]int32
	stream      map[uint32][]int32
	byRun       map[uint32][]int32
	byToolRef   map[uint32][]int32 // tool id -> records that are ABOUT it
	byChild     map[uint32][]int32
	dup         []bool
}

// New returns an empty Index for a session.
func New(session string) *Index {
	return &Index{Session: session, Strings: NewInterner()}
}

// Append adds one record and its blocks, wiring the block range.
func (ix *Index) Append(e Entry, blocks ...Block) {
	ei := uint32(len(ix.Entries))
	e.BlockFirst = uint32(len(ix.Blocks))
	e.BlockCount = uint32(len(blocks))
	for _, b := range blocks {
		b.Entry = ei
		ix.Blocks = append(ix.Blocks, b)
	}
	ix.Entries = append(ix.Entries, e)
	ix.byRecord = nil // invalidate; Build rebuilds on demand
}

// Build constructs the lookup maps. It is idempotent.
func (ix *Index) Build() {
	if ix.byRecord != nil {
		return
	}
	n := len(ix.Entries)
	ix.byRecord = make(map[uint32]int32, n)
	ix.byMsg = make(map[uint32][]int32, n/2+1)
	ix.byBatch = make(map[uint32][]int32)
	ix.stream = make(map[uint32][]int32)
	// Record ids are NOT unique within a source file: a resume replays an
	// earlier block of records. First occurrence wins, and a later copy is
	// excluded from EVERY derived lookup - not just byRecord. A duplicate that
	// reaches byMsg inflates a provider call's fragment count; one that reaches
	// byToolBlock makes an exact join look ambiguous; one that reaches stream
	// duplicates a record in its own stream.
	//
	// The duplicate entries themselves stay in Entries: they are evidence of
	// what the runtime re-emitted, and only the canonical view excludes them.
	dup := make([]bool, len(ix.Entries))
	for i := range ix.Entries {
		e := &ix.Entries[i]
		if e.Record == 0 {
			continue // no identity to compare on, so every such entry is its own record
		}
		if _, seen := ix.byRecord[e.Record]; seen {
			dup[i] = true
			continue
		}
		ix.byRecord[e.Record] = int32(i)
	}
	for i := range ix.Entries {
		if dup[i] {
			continue
		}
		e := &ix.Entries[i]
		if e.Call != 0 {
			ix.byMsg[e.Call] = append(ix.byMsg[e.Call], int32(i))
		}
		if e.Batch != 0 {
			ix.byBatch[e.Batch] = append(ix.byBatch[e.Batch], int32(i))
		}
		if e.Stream != 0 {
			ix.stream[e.Stream] = append(ix.stream[e.Stream], int32(i))
		}
	}
	ix.byRun = make(map[uint32][]int32)
	ix.byToolRef = make(map[uint32][]int32)
	ix.byChild = make(map[uint32][]int32)
	for i := range ix.Entries {
		if dup[i] {
			continue
		}
		e := &ix.Entries[i]
		if e.Run != 0 {
			ix.byRun[e.Run] = append(ix.byRun[e.Run], int32(i))
		}
		if e.Tool != 0 {
			ix.byToolRef[e.Tool] = append(ix.byToolRef[e.Tool], int32(i))
		}
		if e.Child != 0 {
			ix.byChild[e.Child] = append(ix.byChild[e.Child], int32(i))
		}
	}
	ix.dup = dup
	ix.byToolBlock = make(map[uint32][]int32, len(ix.Blocks))
	for i := range ix.Blocks {
		b := &ix.Blocks[i]
		if b.ToolID == 0 || dup[b.Entry] {
			continue
		}
		ix.byToolBlock[b.ToolID] = append(ix.byToolBlock[b.ToolID], int32(i))
	}
}

// EntryByRecord returns the first entry with a record id, and whether it exists.
func (ix *Index) EntryByRecord(recordID string) (*Entry, bool) {
	ix.Build()
	id, ok := ix.Strings.Lookup(recordID)
	if !ok || id == 0 {
		return nil, false
	}
	i, ok := ix.byRecord[id]
	if !ok {
		return nil, false
	}
	return &ix.Entries[i], true
}

// ProviderCall returns the entries of one provider call, in landed order.
//
// Grouping is by message id, never by request id: a synthetic companion record
// can reuse a real call's request id, which would put fabricated content
// into a genuine call.
func (ix *Index) ProviderCall(callID string) []*Entry {
	ix.Build()
	id, ok := ix.Strings.Lookup(callID)
	if !ok || id == 0 {
		return nil
	}
	idxs := ix.byMsg[id]
	out := make([]*Entry, 0, len(idxs))
	for _, i := range idxs {
		out = append(out, &ix.Entries[i])
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Seq != out[b].Seq {
			return out[a].Seq < out[b].Seq
		}
		return out[a].Row < out[b].Row
	})
	return out
}

// ToolBlocks returns every block carrying a tool-use id.
func (ix *Index) ToolBlocks(toolUseID string) []*Block {
	ix.Build()
	id, ok := ix.Strings.Lookup(toolUseID)
	if !ok || id == 0 {
		return nil
	}
	out := make([]*Block, 0, 2)
	for _, i := range ix.byToolBlock[id] {
		out = append(out, &ix.Blocks[i])
	}
	return out
}

// Stream returns every entry of one execution stream, in landed order.
func (ix *Index) Stream(name string) []*Entry {
	ix.Build()
	id, ok := ix.Strings.Lookup(name)
	if !ok || id == 0 {
		return nil
	}
	idxs := ix.stream[id]
	out := make([]*Entry, 0, len(idxs))
	for _, i := range idxs {
		out = append(out, &ix.Entries[i])
	}
	return out
}

// Streams returns the names of every indexed stream.
func (ix *Index) Streams() []string {
	ix.Build()
	out := make([]string, 0, len(ix.stream))
	for id := range ix.stream {
		out = append(out, ix.Strings.String(id))
	}
	sort.Strings(out)
	return out
}

// Batch returns every entry the runtime filed under one orchestration.
func (ix *Index) Batch(batchID string) []*Entry {
	return ix.resolve(ix.byBatch, batchID)
}

// IsDuplicate reports whether an entry is a replayed copy of an earlier one.
//
// Duplicates stay in Entries because they are evidence of what the runtime
// re-emitted; every canonical view excludes them. An assembler iterating
// Entries directly must consult this, or a resume doubles the conversation.
func (ix *Index) IsDuplicate(i int) bool {
	ix.Build()
	return i < len(ix.dup) && ix.dup[i]
}

// Canonical returns the entry indices of the session in landed order, with
// replayed copies excluded.
func (ix *Index) Canonical() []int32 {
	ix.Build()
	out := make([]int32, 0, len(ix.Entries))
	for i := range ix.Entries {
		if !ix.dup[i] {
			out = append(out, int32(i))
		}
	}
	return out
}

// Run returns every entry of one agent loop, in landed order.
func (ix *Index) Run(runID string) []*Entry {
	return ix.resolve(ix.byRun, runID)
}

// AboutTool returns every record that names a tool use outside its content
// blocks - a child's sidecar naming the call that created it, a notification
// naming the call it completes.
func (ix *Index) AboutTool(toolUseID string) []*Entry {
	return ix.resolve(ix.byToolRef, toolUseID)
}

// AboutChild returns every record that names a child agent stream, whether it
// says one started or that one finished.
//
// This is the parent side of the join, and it deliberately spans the whole
// session: a nested child notifies the main stream while the call that created
// it lives in an agent file, so resolution scoped to one file would miss it.
func (ix *Index) AboutChild(stream string) []*Entry {
	return ix.resolve(ix.byChild, stream)
}

func (ix *Index) resolve(m map[uint32][]int32, key string) []*Entry {
	ix.Build()
	id, ok := ix.Strings.Lookup(key)
	if !ok || id == 0 {
		return nil
	}
	idxs := m[id]
	out := make([]*Entry, 0, len(idxs))
	for _, i := range idxs {
		out = append(out, &ix.Entries[i])
	}
	return out
}

// Ancestor walks BACKWARD from an entry to the nearest ancestor satisfying
// want, following the containment parent.
//
// The direction is the whole point. Forward traversal of the parent graph is
// ambiguous - it is a DAG, and parallel tool dispatch forks it, so a forward
// walk follows one branch and silently drops the other. Backward is
// single-valued: a record has exactly one containment parent.
//
// maxHops bounds a cycle in corrupt data. It returns nil rather than looping.
func (ix *Index) Ancestor(e *Entry, maxHops int, want func(*Entry) bool) *Entry {
	ix.Build()
	cur := e
	for hop := 0; hop <= maxHops; hop++ {
		if want(cur) {
			return cur
		}
		if cur.Parent == 0 {
			return nil
		}
		i, ok := ix.byRecord[cur.Parent]
		if !ok {
			return nil
		}
		next := &ix.Entries[i]
		if next == cur {
			return nil
		}
		cur = next
	}
	return nil
}

// RunOf resolves the agent loop an entry belongs to.
//
// Run ids sit on trigger records; a model response carries none, but reaches
// one through its containment parents. Line proximity would agree in the simple
// case and diverge exactly where it matters - a forked chain, a replayed block,
// an interrupt that re-parents to an earlier record - attributing records to
// the wrong turn.
func (ix *Index) RunOf(e *Entry) (*Entry, bool) {
	a := ix.Ancestor(e, ancestorHops, func(x *Entry) bool { return x.Run != 0 })
	return a, a != nil
}

// ancestorHops bounds the backward walk. Measured hop counts to a run id reach
// 7 in most cases and past it in 140 of 44,973 records, so the bound is far
// above both and low enough that corrupt data cannot spin.
const ancestorHops = 64
