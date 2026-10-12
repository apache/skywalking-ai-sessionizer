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

// segment is an activity window: the main stream's talks between two quiet
// periods, and the delegated work that started inside them.
type segment struct {
	NodeID string
	Talks  []*talk
	From   int64
	To     int64
}

// Stage 8 - cut the activity windows.
//
// A window is where the work happened, nothing more. It says neither that the
// window is finished nor that it may be committed: a round carries no clock, so
// only a reader, who knows the time, can tell whether the last window is still
// going. A later talk that starts within the idle gap of a window's end joins it
// through a new in_segment relation; the window's own node does not change.
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
	cur := &segment{}
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
			cur = &segment{}
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

	for _, sg := range b.segments {
		// A window carries no attributes: its talks are its in_segment
		// relations, and its time range is theirs.
		b.node(sessionflow.Node{
			Entity: sessionflow.Entity{ID: sg.NodeID}, Kind: model.KindSegment,
			Parent: sessionflow.NodeID("session", b.opt.Session),
			Ref:    refPtr(sg.Talks[0].At),
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
