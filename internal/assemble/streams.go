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

// Stage 1 - remove duplicate records.
//
// Nothing downstream is correct before this. Record ids are not unique inside
// one source file: resuming a session replays a block of earlier records. The
// first occurrence wins. Keeping the last would move records away from their
// true position to the point where the replay happened, and break line-order
// reconstruction of every provider call around them.
//
// The index already applies the rule, so this stage takes its canonical view
// and records the count. That count matters: before duplicates are removed a
// measurable share of tool joins look ambiguous when they are not.
func (b *builder) stage1Canonical() {
	all := b.ix.Canonical()
	b.canonical = all
	if b.opt.ThroughSeq > 0 {
		// Everything above the watermark is set aside, not read. The index may
		// already hold records a concurrent collector landed after this round's
		// range was fixed, and a round must describe only the evidence its own
		// header and input digest cover.
		bounded := make([]int32, 0, len(all))
		for _, i := range all {
			if uint64(b.ix.Entries[i].Seq) <= b.opt.ThroughSeq {
				bounded = append(bounded, i)
			}
		}
		b.canonical = bounded
	}
	b.stats.Entries = len(b.canonical)
	b.stats.Duplicates = len(b.ix.Entries) - len(all)
	b.stats.Beyond = len(all) - len(b.canonical)
}

// streamInfo is one execution stream: an ordered lineage inside a session.
type streamInfo struct {
	ID      uint32 // interned stream name
	Name    string
	Role    string
	NodeID  string
	Entries []int32 // canonical entry indices, in landed order
	First   int64   // earliest timestamp seen
	Last    int64

	epochs []*epoch
	talks  []*talk
}

// Stage 2 - partition streams.
//
// A stream is the ordered continuity boundary: model-message continuity is only
// ever evaluated inside one. The landing layout already separates them, because
// a child agent writes its own file.
//
// A child agent always creates or continues a distinct stream. It creates a new
// session only when the runtime explicitly reports a different session
// identifier - never from visual nesting, agent identity or timestamps.
func (b *builder) stage2Streams() {
	sessionNode := sessionflow.NodeID("session", b.opt.Session)
	a := map[string]any{"conversation": b.opt.Conversation}
	// The runtime writes a title and rewrites it as the work goes on - up to
	// ninety times in one session - so the last one is the current one. It is
	// the only name anything in this data has.
	if t := b.latestTitle(); t != "" {
		a["title"] = t
		a["title_from"] = model.ObservedReplayable
	}
	b.node(sessionflow.Node{
		Entity: sessionflow.Entity{ID: sessionNode}, Kind: model.KindSession,
		Attrs: attrs(a),
	})

	for _, i := range b.canonical {
		e := &b.ix.Entries[i]
		if e.Stream == 0 {
			continue // journals, manifests and scripts belong to a run, not a stream
		}
		s, ok := b.byStream[e.Stream]
		if !ok {
			name := b.str(e.Stream)
			role := model.StreamChild
			if name == "main" {
				role = model.StreamMain
			}
			s = &streamInfo{
				ID: e.Stream, Name: name, Role: role,
				NodeID: sessionflow.NodeID("stream", name),
			}
			b.byStream[e.Stream] = s
			b.streams = append(b.streams, s)
		}
		s.Entries = append(s.Entries, i)
		if e.TS != 0 {
			if s.First == 0 || e.TS < s.First {
				s.First = e.TS
			}
			if e.TS > s.Last {
				s.Last = e.TS
			}
		}
	}

	// Sorting by name keeps the output reproducible; discovery order depends on
	// directory iteration, which is not stable.
	sort.Slice(b.streams, func(i, j int) bool { return b.streams[i].Name < b.streams[j].Name })

	for _, s := range b.streams {
		// The first landed position in the stream. It is what puts one stream
		// before another in a rendering, and it is stable because a landed
		// record never moves.
		anchor := ref(&b.ix.Entries[s.Entries[0]])
		sa := map[string]any{"role": s.Role, "records": len(s.Entries)}
		// A child's sidecar says what it was asked to do. Nothing else names a
		// stream, so without this a reader meets a list of agent ids.
		if lbl := b.labelFor(s); lbl != "" {
			sa["label"] = lbl
		}
		b.node(sessionflow.Node{
			Entity: sessionflow.Entity{ID: s.NodeID}, Kind: model.KindStream,
			Parent: sessionNode, Stream: s.Name, Ref: refPtr(anchor),
			Attrs: attrs(sa),
		})
	}
	b.stats.Streams = len(b.streams)
}

// latestTitle returns the last title the runtime wrote for this session.
//
// A title record carries a name and nothing else - no identity, no parent - so
// it can only be found by looking for the name.
func (b *builder) latestTitle() string {
	var title string
	for _, i := range b.canonical {
		e := &b.ix.Entries[i]
		// A title record carries a name and no identity of its own. A sidecar
		// and a manifest also carry names, but those name a stream and a
		// workflow rather than the conversation.
		if e.Label == 0 || e.Record != 0 ||
			e.Kind == index.KindMeta || e.Kind == index.KindManifest {
			continue
		}
		title = b.str(e.Label)
	}
	return title
}

// labelFor returns what a child stream was asked to do, from its sidecar.
func (b *builder) labelFor(s *streamInfo) string {
	for _, i := range b.canonical {
		e := &b.ix.Entries[i]
		if e.Kind == index.KindMeta && e.Label != 0 &&
			(e.Stream == s.ID || e.Child == s.ID) {
			return b.str(e.Label)
		}
	}
	return ""
}

// entriesOf returns a stream's canonical entries in landed order.
func (b *builder) entriesOf(s *streamInfo) []*index.Entry {
	out := make([]*index.Entry, 0, len(s.Entries))
	for _, i := range s.Entries {
		out = append(out, &b.ix.Entries[i])
	}
	return out
}

// mainStream returns the parent lineage, or nil if the session has none.
func (b *builder) mainStream() *streamInfo {
	for _, s := range b.streams {
		if s.Role == model.StreamMain {
			return s
		}
	}
	return nil
}
