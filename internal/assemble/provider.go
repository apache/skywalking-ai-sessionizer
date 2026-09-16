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

package assemble

import (
	"sort"

	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// Provider bodies, joined to the calls they belong to.
//
// A body is what the runtime sent to its model provider and what came back. The
// round names where each joined body landed and never carries the body itself, so
// a reader loads it when it wants it and the chain stays the size of the
// structure.
//
// The join is here rather than in a reader because every reader would otherwise
// repeat it, and to repeat it a reader has to open every landed body - the largest
// files a session holds - to read one line of each. Resolved once, at parse time,
// it travels in the round.
//
// A response joins by its message id, which is the call's own. A request names no
// call, only the request before it and its prompt, so a request joins to the call
// of its stream whose previous call's response carries that request id, and whose
// prompt is the one it names, when exactly one request and exactly one call carry
// those two ids. A compaction request names no prompt, a retried request carries
// the same two ids twice, and the first calls of two streams under one prompt
// carry the same two ids; none of them is joined. Nothing is joined by position or
// by time.
//
// The join is made again from the evidence each round covers, and a round carries
// the whole call node, so a body landing later attaches in a later round, and a
// join that later evidence makes ambiguous is withdrawn the same way.

// joinProviderBodies fills bodiesOfCall, which emitCall writes onto the call.
func (b *builder) joinProviderBodies() {
	bodies := b.bodiesInWindow()
	if len(bodies) == 0 {
		return
	}
	calls := b.callsInLineOrder()
	if len(calls) == 0 {
		return
	}

	// Responses, by message id.
	byMsg := map[uint32][]int{}
	for i, y := range bodies {
		if y.role == index.BodyRoleResponse && y.msg != 0 {
			byMsg[y.msg] = append(byMsg[y.msg], i)
		}
	}
	// requestOf is the request id a call's response carries, which is what the
	// call after it names as the request before its own.
	requestOf := map[string]uint32{}
	responseOf := map[string]int{}
	for _, k := range calls {
		if k.msg == 0 {
			continue
		}
		switch hits := byMsg[k.msg]; len(hits) {
		case 0:
		case 1:
			requestOf[k.node], responseOf[k.node] = bodies[hits[0]].request, hits[0]
			bodies[hits[0]].joined = true
		default:
			for _, i := range hits {
				bodies[i].ambiguous = true
			}
		}
	}

	// Requests, by the request before them and their prompt.
	type key struct{ previous, prompt uint32 }
	byKey := map[key][]int{}
	for i, y := range bodies {
		if y.role == index.BodyRoleRequest && y.prompt != 0 {
			byKey[key{y.previous, y.prompt}] = append(byKey[key{y.previous, y.prompt}], i)
		}
	}
	// The key each call's request would carry. A call whose previous call has no
	// response carrying its request id has none, and neither has a call in a
	// stream whose landed lines have a gap: a call may be missing between two
	// that look consecutive.
	gapped := map[uint32]bool{}
	for _, k := range calls {
		if _, done := gapped[k.stream]; !done {
			gapped[k.stream] = b.streamHasGap(k.stream)
		}
	}
	callKey := map[string]key{}
	callsByKey := map[key]int{}
	for i, k := range calls {
		if gapped[k.stream] || k.prompt == 0 {
			continue
		}
		var previous uint32
		if i > 0 && calls[i-1].stream == k.stream {
			var ok bool
			if previous, ok = requestOf[calls[i-1].node]; !ok || previous == 0 {
				continue
			}
		}
		callKey[k.node] = key{previous, k.prompt}
		callsByKey[key{previous, k.prompt}]++
	}
	requestFor := map[string]int{}
	for _, k := range calls {
		ck, ok := callKey[k.node]
		if !ok {
			continue
		}
		hits := byKey[ck]
		switch {
		case len(hits) == 0:
		case len(hits) == 1 && callsByKey[ck] == 1 && !bodies[hits[0]].joined && !bodies[hits[0]].ambiguous:
			// One request and one call carry the key: nothing else could be this
			// call's request, and this request no other call's.
			requestFor[k.node] = hits[0]
			bodies[hits[0]].joined = true
		default:
			for _, i := range hits {
				if !bodies[i].joined {
					bodies[i].ambiguous = true
				}
			}
		}
	}

	b.bodiesOfCall = map[string][]sessionflow.ProviderBody{}
	for _, k := range calls {
		var joined []sessionflow.ProviderBody
		if i, ok := requestFor[k.node]; ok {
			joined = append(joined, bodies[i].published())
		}
		if i, ok := responseOf[k.node]; ok {
			joined = append(joined, bodies[i].published())
		}
		if len(joined) > 0 {
			b.bodiesOfCall[k.node] = joined
		}
	}
}

// body is one provider body this round may join.
type body struct {
	role              index.BodyRole
	ref               sessionflow.Ref
	prompt, previous  uint32
	request, msg      uint32
	joined, ambiguous bool
}

func (y *body) published() sessionflow.ProviderBody {
	role := sessionflow.RoleRequest
	if y.role == index.BodyRoleResponse {
		role = sessionflow.RoleResponse
	}
	return sessionflow.ProviderBody{Role: role, Ref: y.ref}
}

// bodiesInWindow is the session's provider bodies this round covers, in landed
// order, one per record: a body landed twice by an interrupted pass is one body,
// and the first landing is the one a round names.
func (b *builder) bodiesInWindow() []body {
	out := make([]body, 0, len(b.ix.Bodies))
	seen := map[uint32]bool{}
	for i := range b.ix.Bodies {
		y := &b.ix.Bodies[i]
		e := &b.ix.Entries[y.Entry]
		if b.opt.ThroughSeq > 0 && uint64(e.Seq) > b.opt.ThroughSeq {
			continue
		}
		if e.Record == 0 || seen[e.Record] {
			continue
		}
		seen[e.Record] = true
		out = append(out, body{
			role: y.Role, ref: sessionflow.Ref{Seq: uint64(e.Seq), Row: uint64(e.Row)},
			prompt: e.Run, previous: y.Previous, request: y.Request, msg: e.Call,
		})
	}
	return out
}

// call is one provider call the bodies join to.
type call struct {
	node, streamName string
	stream, msg      uint32
	prompt           uint32
	at               sessionflow.Ref
}

// callsInLineOrder is the round's calls, in the order their records landed within
// a stream, which is line order there.
//
// A call's prompt is the one its run's own first record names, which is where the
// reader this join replaced took it; a call in a run whose first record names none
// has no prompt and joins no request. A synthetic call was never sent to a provider: it
// takes part in no join, and it is not the call before the next one either. A call
// whose own record carries no message id stays in its stream with no id, so the
// call after it has no previous response to name and is left unjoined rather than
// taken for the first of its stream.
func (b *builder) callsInLineOrder() []*call {
	var out []*call
	for _, c := range b.calls {
		if c.Synthetic || len(c.Fragments) == 0 {
			continue
		}
		first := c.Fragments[0]
		out = append(out, &call{
			node: c.NodeID, stream: first.Stream, streamName: c.Stream.Name,
			msg: c.CallID, prompt: b.promptOf(first),
			at: sessionflow.Ref{Seq: uint64(first.Seq), Row: uint64(first.Row)},
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].streamName != out[j].streamName {
			return out[i].streamName < out[j].streamName
		}
		if out[i].at.Seq != out[j].at.Seq {
			return out[i].at.Seq < out[j].at.Seq
		}
		if out[i].at.Row != out[j].at.Row {
			return out[i].at.Row < out[j].at.Row
		}
		// Two calls on one record would otherwise take their order from the
		// nodes' own, and a request its call from that order, so the same
		// evidence could give two rounds.
		return out[i].node < out[j].node
	})
	return out
}

// streamHasGap reports whether the stream's transcript lines this round covers
// skip a line: the first is not line 1, or a line after a later one is missing.
//
// A line landed twice is a repeat, not a gap, and a replayed block of records
// occupies lines of its own, so every landed record counts here - the duplicates
// stage 1 sets aside included.
func (b *builder) streamHasGap(stream uint32) bool {
	// In landed order, never sorted: lines that arrive out of order are a gap
	// while they are, and the calls this join walks are in that same order.
	var prev uint64
	for i := range b.ix.Entries {
		e := &b.ix.Entries[i]
		if e.Stream != stream || !transcriptKind(e.Kind) {
			continue
		}
		if b.opt.ThroughSeq > 0 && uint64(e.Seq) > b.opt.ThroughSeq {
			continue
		}
		if e.Ord > prev+1 {
			return true
		}
		if e.Ord > prev {
			prev = e.Ord
		}
	}
	return false
}

// promptOf is the prompt of the run holding the call, as the run's own first
// record carries it. A run exists only for records this round covers, so the walk
// stops inside the window: a record landing later cannot change what this round
// made of the evidence it read.
func (b *builder) promptOf(first *index.Entry) uint32 {
	n := b.nodes[b.containerOf(first)]
	if n == nil || n.Kind != model.KindRun || n.Ref == nil {
		return 0
	}
	e := b.entryAt(*n.Ref)
	if e == nil {
		return 0
	}
	return e.Run
}

// entryAt is the entry at a landed position, or nil. The first entry at a
// position wins, as it does everywhere else.
func (b *builder) entryAt(r sessionflow.Ref) *index.Entry {
	if b.byPosition == nil {
		b.byPosition = make(map[[2]uint32]int32, len(b.ix.Entries))
		for i := range b.ix.Entries {
			e := &b.ix.Entries[i]
			k := [2]uint32{e.Seq, e.Row}
			if _, taken := b.byPosition[k]; !taken {
				b.byPosition[k] = int32(i)
			}
		}
	}
	i, ok := b.byPosition[[2]uint32{uint32(r.Seq), uint32(r.Row)}]
	if !ok {
		return nil
	}
	return &b.ix.Entries[i]
}

// transcriptKind reports whether a record came from a stream's transcript, whose
// lines are numbered as one run. A sidecar, a journal, a manifest, a script, a
// change record and a provider body are each their own file, with their own line
// numbers, so counting them together would find gaps that are not there.
func transcriptKind(k index.Kind) bool {
	switch k {
	case index.KindMeta, index.KindJournal, index.KindManifest, index.KindScript,
		index.KindChanges, index.KindProviderBody:
		return false
	}
	return true
}
