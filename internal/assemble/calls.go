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

// providerCall is one provider attempt: an ordered set of record fragments and
// the response they carry between them.
type providerCall struct {
	CallID    uint32
	Stream    *streamInfo
	Fragments []*index.Entry // in landed order

	// Finished says the provider reported a stop reason somewhere in the call.
	// It is the only honest test for whether the usage numbers mean anything: a
	// call that never finished still carries a usage block, but its output count
	// is a streaming stub of a few tokens.
	Finished bool

	Synthetic bool
	NodeID    string
}

// Last returns the final fragment in line order.
//
// Line order, not the stop reason. A main transcript stamps the same stop reason
// on every fragment of a call, so a stop-reason test picks the first fragment
// there and the last one on a child stream. Only line order works on both.
func (c *providerCall) Last() *index.Entry {
	if len(c.Fragments) == 0 {
		return nil
	}
	return c.Fragments[len(c.Fragments)-1]
}

// Stage 3 - group provider calls.
//
// Group by message id, in LINE ORDER. Two rules here are the opposite of the
// obvious choice, and both were forced by measurement.
//
// First, never rebuild a call by walking the parent pointer FORWARD. The parent
// graph is a directed acyclic graph, not a chain: dispatching several tools at
// once forks it, so a forward walk follows one branch and drops the other tool
// use with its result. On tool-bearing calls that produces a message list the
// provider API would itself reject. Walking BACKWARD is fine and stage 7 relies
// on it - a record has exactly one containment parent, so backward is
// single-valued. The ban is on the direction, not on the edge.
//
// Second, group by message id and not by request id. A record the client
// fabricated can carry a real call's request id, so grouping by request id puts
// invented content inside a real provider call.
func (b *builder) stage3Calls() {
	byCall := map[uint32]*providerCall{}
	for _, s := range b.streams {
		for _, e := range b.entriesOf(s) {
			if e.Kind != index.KindAssistant || e.Call == 0 {
				continue
			}
			c, ok := byCall[e.Call]
			if !ok {
				c = &providerCall{
					CallID: e.Call, Stream: s,
					NodeID: sessionflow.NodeID("call", b.str(e.Call)),
				}
				byCall[e.Call] = c
				b.calls = append(b.calls, c)
			}
			c.Fragments = append(c.Fragments, e)
			if e.Flags.Has(index.FlagSynthetic) {
				c.Synthetic = true
			}
			if e.Flags.Has(index.FlagStopReason) {
				c.Finished = true
			}
		}
	}

	sort.Slice(b.calls, func(i, j int) bool { return b.calls[i].NodeID < b.calls[j].NodeID })
	for _, c := range b.calls {
		sort.SliceStable(c.Fragments, func(i, j int) bool {
			if c.Fragments[i].Seq != c.Fragments[j].Seq {
				return c.Fragments[i].Seq < c.Fragments[j].Seq
			}
			return c.Fragments[i].Row < c.Fragments[j].Row
		})
		b.stats.CallFragments += len(c.Fragments)
		if c.Synthetic {
			b.stats.SyntheticMessages++
		}
		if !c.Finished {
			// A call that never reports a stop reason is normal, not an error, and
			// it is NOT still running: almost all of them sit in the middle of a
			// file with the conversation continuing past them, and their tools did
			// execute. Nothing waits for it and nothing is marked open. Only its
			// usage is unavailable.
			b.stats.CallsWithoutEnd++
		}
	}
	b.stats.ProviderCalls = len(b.calls)
}

// usageRule explains, in the emitted data, where usage comes from.
//
// It comes from the last fragment in line order, and never from adding the
// fragments up. A main transcript repeats the call's final usage on every
// fragment, so adding them multiplies the real number by the fragment count -
// up to nine times over. A child stream instead writes streaming partials that
// climb 1, 2, 3, where the last value is the whole count. Taking the last
// fragment is correct for both; summing is wrong for both.
const usageRule = "last_fragment_in_line_order"

// emitCall writes the provider call node and the steps inside it.
//
// A synthetic record carries the assistant role and sits in the stream like any
// response, but no model produced it. It is emitted as its own kind so it can
// never be counted as agent output, and kept rather than dropped because it
// marks exactly where a conversation was interrupted.
func (b *builder) emitCall(c *providerCall, parent string) {
	refs := make([]sessionflow.Ref, 0, len(c.Fragments))
	for _, f := range c.Fragments {
		refs = append(refs, ref(f))
	}
	a := map[string]any{
		"fragments":  len(c.Fragments),
		"usage_from": usageRule,
	}
	if last := c.Last(); last != nil && c.Finished {
		a["usage"] = model.ObservedReplayable
		a["usage_at"] = map[string]any{"seq": last.Seq, "row": last.Row}
	} else {
		// The usage block on an unfinished call is present but meaningless, so it
		// is reported unavailable rather than published as a real measurement.
		a["usage"] = model.Unavailable
		a["stop_reason"] = model.Unavailable
	}
	b.node(sessionflow.Node{
		Entity: sessionflow.Entity{ID: c.NodeID}, Kind: model.KindLLMCall,
		Parent: parent, Stream: c.Stream.Name,
		Ref: &refs[0], Refs: refs, Attrs: attrs(a),
	})

	// The response's own content becomes steps inside the call. Their position
	// is (record, block), which is stable because a landed record never changes.
	kind := model.KindMessageAssistant
	if c.Synthetic {
		kind = model.KindMessageSynthetic
	}
	for _, f := range c.Fragments {
		for _, blk := range b.blocksOf(f) {
			switch blk.Kind {
			case index.BlockText:
				id := sessionflow.RefID("msg", blockRef(f, blk.Ord))
				b.node(sessionflow.Node{
					Entity: sessionflow.Entity{ID: id}, Kind: kind, Parent: c.NodeID,
					Stream: c.Stream.Name, Ref: refPtr(blockRef(f, blk.Ord)),
				})
				b.stats.Steps++
			case index.BlockThinking:
				id := sessionflow.RefID("think", blockRef(f, blk.Ord))
				b.node(sessionflow.Node{
					Entity: sessionflow.Entity{ID: id}, Kind: model.KindThinking, Parent: c.NodeID,
					Stream: c.Stream.Name, Ref: refPtr(blockRef(f, blk.Ord)),
				})
				b.stats.Steps++
			}
		}
	}
}

// blocksOf returns a record's content blocks.
func (b *builder) blocksOf(e *index.Entry) []index.Block {
	if e.BlockCount == 0 {
		return nil
	}
	return b.ix.Blocks[e.BlockFirst : e.BlockFirst+e.BlockCount]
}

func refPtr(r sessionflow.Ref) *sessionflow.Ref { return &r }
