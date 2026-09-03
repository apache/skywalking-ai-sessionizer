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
	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// epoch is the lifetime of one model context inside a stream.
type epoch struct {
	NodeID   string
	Stream   *streamInfo
	Boundary *index.Entry // nil for a stream's first epoch
	Start    int          // position in the stream's entry list
	End      int          // exclusive
}

// Stage 6 - cut context epochs.
//
// An epoch exists only where a runtime can reset model context in place. A
// stream with no reset has exactly one epoch and its message history grows
// without interruption.
//
// A reset is only ever taken from an explicit boundary record, which carries a
// pointer to the last message before it. It is never inferred from a token
// counter, from elapsed time, or from a mismatch between two message lists.
//
// Two measured facts shape this. The summary the reset produced is timestamped
// EARLIER than the boundary that produced it, so an epoch must never be ordered
// by timestamp - landed order is the only safe order. And child streams never
// reset, so every child stream is exactly one epoch.
func (b *builder) stage6Epochs() {
	b.epochAt = map[[2]uint32]string{}
	for _, s := range b.streams {
		entries := b.entriesOf(s)
		var cuts []int
		for i, e := range entries {
			if e.Flags.Has(index.FlagEpochBoundary) {
				cuts = append(cuts, i)
			}
		}

		start, prev := 0, ""
		emit := func(boundaryAt int, end int) {
			var boundary *index.Entry
			key := "0"
			if boundaryAt >= 0 {
				boundary = entries[boundaryAt]
				key = b.str(boundary.Record)
				if key == "" {
					key = sessionflow.RefID("at", ref(boundary))
				}
			}
			ep := &epoch{
				NodeID: sessionflow.NodeID("epoch", s.Name, key),
				Stream: s, Boundary: boundary, Start: start, End: end,
			}
			s.epochs = append(s.epochs, ep)
			for i := start; i < end; i++ {
				e := entries[i]
				b.epochAt[[2]uint32{e.Seq, e.Row}] = ep.NodeID
			}

			a := map[string]any{"records": end - start}
			var refs []sessionflow.Ref
			if boundary != nil {
				refs = append(refs, ref(boundary))
				a["reset"] = model.ObservedReplayable
				// The continuation point the runtime stated. It may name a record
				// that exists nowhere on disk, so it is checked rather than trusted.
				if boundary.Continues != 0 {
					cont := b.str(boundary.Continues)
					a["continues_from"] = cont
					if _, ok := b.ix.EntryByRecord(cont); !ok {
						b.open("epoch_continuation", cont,
							"a reset names a record that is not in the landed data")
					}
				}
			} else {
				a["reset"] = "none"
			}
			// An epoch is ordered by where it begins, which for the first epoch of
			// a stream is the stream's first record and afterwards is the reset
			// that opened it.
			anchor := ref(entries[start])
			b.node(sessionflow.Node{
				Entity: sessionflow.Entity{ID: ep.NodeID}, Kind: model.KindEpoch,
				Parent: s.NodeID, Stream: s.Name,
				Ref: refPtr(anchor), Refs: refs, Attrs: attrs(a),
			})
			if prev != "" {
				b.relate(model.RelFollows, ep.NodeID, prev, model.ExactUnique,
					"explicit context reset", refs...)
			}
			prev = ep.NodeID
			b.stats.Epochs++
		}

		for _, cut := range cuts {
			// The boundary record opens the epoch that follows it, so the epoch
			// before it ends at the boundary.
			emit(firstBoundary(start, cuts), cut)
			start = cut
		}
		emit(firstBoundary(start, cuts), len(entries))
	}
}

// firstBoundary reports the cut that opens the epoch starting at start, or -1
// when the epoch is a stream's first and no reset opened it.
func firstBoundary(start int, cuts []int) int {
	for _, c := range cuts {
		if c == start {
			return c
		}
	}
	return -1
}

// emitEpochSteps writes the boundary and summary steps themselves.
//
// The summary is a real step: it is the only record of everything the reset
// dropped, so leaving it out makes the conversation look like it began there.
func (b *builder) emitEpochSteps() {
	for _, i := range b.canonical {
		e := &b.ix.Entries[i]
		switch {
		case e.Flags.Has(index.FlagEpochBoundary):
			id := sessionflow.RefID("boundary", ref(e))
			b.node(sessionflow.Node{
				Entity: sessionflow.Entity{ID: id}, Kind: model.KindEpochBoundary,
				Parent: b.epochOf(e), Stream: b.streamName(e), Ref: refPtr(ref(e)),
			})
			b.stats.Steps++
		case e.Flags.Has(index.FlagEpochSummary):
			id := sessionflow.RefID("summary", ref(e))
			b.node(sessionflow.Node{
				Entity: sessionflow.Entity{ID: id}, Kind: model.KindEpochSummary,
				Parent: b.epochOf(e), Stream: b.streamName(e), Ref: refPtr(ref(e)),
			})
			b.stats.Steps++
			// The summary points back at the boundary that produced it through its
			// containment parent, which is exact. Pairing them by timestamp would
			// fail, because the summary is stamped earlier than its own boundary.
			if e.Parent != 0 {
				if bnd, ok := b.ix.EntryByRecord(b.str(e.Parent)); ok && bnd.Flags.Has(index.FlagEpochBoundary) {
					b.relate(model.RelSummarizes, id, sessionflow.RefID("boundary", ref(bnd)),
						model.ExactUnique, "containment parent", ref(e))
				}
			}
		}
	}
}

// epochOf returns the epoch node an entry falls in.
func (b *builder) epochOf(e *index.Entry) string {
	if id, ok := b.epochAt[[2]uint32{e.Seq, e.Row}]; ok {
		return id
	}
	if s, ok := b.byStream[e.Stream]; ok {
		return s.NodeID
	}
	return ""
}
