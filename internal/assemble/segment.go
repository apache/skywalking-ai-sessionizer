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

	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// segment is an activity window that can be committed on its own.
type segment struct {
	NodeID string
	Index  int
	Talks  []*talk
	From   int64
	To     int64
	Gates  map[string]bool
}

// Gate names. All four must pass before a segment may be committed.
const (
	// GateActivityBoundary - the quiet period is real and not an artefact of
	// records arriving out of order.
	GateActivityBoundary = "activity_boundary"
	// GateNoCrossingOpenOperation - no unfinished tool call and no child agent
	// still running. This gate does real work rather than being a safety net: it
	// is what makes "a committed segment is never rewritten" safe to promise.
	// Without it a tool request could sit in a committed segment while its
	// result arrives in the next one.
	GateNoCrossingOpenOperation = "no_crossing_open_operation"
	// GateLatenessWatermark - no late evidence is still expected.
	GateLatenessWatermark = "lateness_watermark"
	// GateConversationIdentity - the conversation this belongs to is known.
	GateConversationIdentity = "conversation_identity"
)

// Stage 8 - propose segment boundaries.
//
// A long quiet period creates a CANDIDATE, not a commit. About a third of gaps
// over ten minutes fall in the middle of a turn - a slow tool, a provider retry
// - so a rule that split on idle time alone would cut roughly one in three
// boundaries in the wrong place.
//
// This stage proposes and reports. Committing is the caller's decision, because
// only the caller knows whether collection has finished.
func (b *builder) stage8Segments() {
	main := b.mainStream()
	if main == nil || len(main.talks) == 0 {
		return
	}
	// Boundaries are cut on the parent lineage only. A child agent runs inside a
	// parent's turn, so its own quiet periods are not activity boundaries for the
	// conversation.
	talks := append([]*talk(nil), main.talks...)
	sort.SliceStable(talks, func(i, j int) bool { return talks[i].First < talks[j].First })

	gap := b.opt.IdleGap.Nanoseconds()
	cur := &segment{Index: 1}
	flush := func() {
		if len(cur.Talks) == 0 {
			return
		}
		// The id comes from where the window OPENS, not from its position in
		// the list. An ordinal changes meaning the moment late evidence inserts
		// an earlier boundary: what was segment 3 becomes segment 4, and the
		// entity that was segment 3 can no longer supersede its own earlier
		// revision. The first talk's first evidence is a landed position, and a
		// landed record never moves.
		cur.NodeID = sessionflow.NodeID("segment", sessionflow.RefID("at", cur.Talks[0].At))
		b.segments = append(b.segments, cur)
	}
	for i, t := range talks {
		// Timestamps step backward in a small share of records - a failed request
		// is stamped when it began, not when it was written - so a negative delta
		// is clamped rather than read as a very large gap in the other direction.
		if i > 0 && t.First != 0 && talks[i-1].Last != 0 && t.First-talks[i-1].Last > gap {
			flush()
			cur = &segment{Index: cur.Index + 1}
		}
		if cur.From == 0 || (t.First != 0 && t.First < cur.From) {
			cur.From = t.First
		}
		if t.Last > cur.To {
			cur.To = t.Last
		}
		cur.Talks = append(cur.Talks, t)
	}
	flush()

	sessionEnd := main.Last
	for _, sg := range b.segments {
		last := sg.Index == len(b.segments)
		sg.Gates = map[string]bool{
			GateActivityBoundary:     !last,
			GateConversationIdentity: b.opt.Conversation != "",
			// Late evidence is still expected while the window sits inside the
			// quiet period at the end of the session.
			GateLatenessWatermark:       !last && sessionEnd-sg.To > gap,
			GateNoCrossingOpenOperation: b.noOpenOperation(sg),
		}
		state := "candidate"
		if last {
			state = "open"
			b.stats.SegmentsOpen++
		} else {
			b.stats.SegmentsCandidate++
		}
		open := []string{}
		for _, g := range []string{GateActivityBoundary, GateNoCrossingOpenOperation,
			GateLatenessWatermark, GateConversationIdentity} {
			if !sg.Gates[g] {
				open = append(open, g)
			}
		}
		b.node(sessionflow.Node{
			Entity: sessionflow.Entity{ID: sg.NodeID}, Kind: model.KindSegment,
			Parent: sessionflow.NodeID("session", b.opt.Session),
			Ref:    refPtr(sg.Talks[0].At),
			Attrs: attrs(map[string]any{
				"state":       state,
				"talks":       len(sg.Talks),
				"gates_unmet": open,
				"committable": len(open) == 0,
			}),
		})
		for _, t := range sg.Talks {
			b.relate(model.RelInSegment, t.NodeID, sg.NodeID, model.ExactUnique,
				"activity window", t.At)
		}
		// A segment cuts across every stream beneath it, so the work a talk
		// delegated belongs to the same window as the talk that asked for it.
		for _, t := range b.talks {
			if t.Stream.Role == model.StreamMain || t.First == 0 {
				continue
			}
			if t.First >= sg.From && t.First <= sg.To {
				b.relate(model.RelInSegment, t.NodeID, sg.NodeID, model.StrongInference,
					"inside the window of the talk that delegated it", t.At)
			}
		}
	}
}

// noOpenOperation reports whether every operation started in a window also
// finished inside it.
//
// The thing this catches is not a request left dangling at the end of the file -
// the runtime writes a result even for a failure, so that is close to
// non-existent. It is a request whose result arrives on the FAR SIDE of the
// quiet period, which is roughly one long gap in eight. Committing the window
// then would freeze the request into a segment that is never rewritten while
// its result lands in the next one.
func (b *builder) noOpenOperation(sg *segment) bool {
	inWindow := func(ts int64) bool { return ts >= sg.From && ts <= sg.To }
	for _, t := range b.tools {
		if t.Use == nil || !inWindow(t.Use.TS) {
			continue
		}
		if t.Result == nil || t.Ambiguous {
			return false
		}
		if !inWindow(t.Result.TS) {
			return false
		}
	}
	for _, u := range b.unres {
		if u.Kind == "child_stream" || u.Kind == "notified_child" {
			return false
		}
	}
	return true
}
