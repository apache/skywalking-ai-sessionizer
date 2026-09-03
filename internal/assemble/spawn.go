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

// spawnEdge is one join between a call and the child stream it started.
type spawnEdge struct {
	Tool    uint32 // the tool use that started the child
	Batch   uint32 // the group of children this one was started with
	Parent  uint32 // the agent stream that started it, for a nested child
	Stream  uint32 // the child
	Via     string
	Quality string
	Ref     sessionflow.Ref
}

// Stage 5 - join spawns.
//
// Three mechanisms exist and no single field covers all of them, because most
// children carry no pointer back to their parent in their own sidecar file:
//
//	Agent tool   the sidecar names the call that created it, and the parent's
//	             result names the child. The two are redundant, so they check
//	             each other.
//	Skill fork   only the parent's result names the child. The sidecar has no
//	             pointer at all.
//	Workflow     the parent's result names a run, and the run's journal names
//	             each child it starts.
//
// Two cautions, both measured. A completion notification's tool-use id is not
// always resolvable in the file it landed in: a nested child notifies the main
// stream while the call that started it lives in an agent file. Resolution
// therefore searches the whole session, which is why the index keys these
// across streams. And the notification's status element is missing entirely on
// a large class of notifications, so nothing may require it.
func (b *builder) stage5Spawns() {
	// A batch of children is reached from the parent's own launch result, which
	// names both the batch and the tool use that started it. Resolving the batch
	// to that tool keeps the edge between things that exist - the call and the
	// child - instead of inventing a node to stand for the batch.
	batchTool := map[uint32]uint32{}
	for _, i := range b.canonical {
		e := &b.ix.Entries[i]
		if e.Batch == 0 || e.Kind == index.KindJournal {
			continue
		}
		for _, blk := range b.blocksOf(e) {
			if blk.Kind == index.BlockToolResult && blk.ToolID != 0 {
				if _, seen := batchTool[e.Batch]; !seen {
					batchTool[e.Batch] = blk.ToolID
				}
			}
		}
	}

	var edges []spawnEdge
	seen := map[[4]uint32]bool{}

	add := func(ed spawnEdge) {
		if ed.Stream == 0 || (ed.Tool == 0 && ed.Batch == 0 && ed.Parent == 0) {
			// No usable link. A sidecar that names only the agent's type and depth
			// says nothing about who started it, so it must not stand in for an
			// edge and block the journal record that does carry one.
			return
		}
		key := [4]uint32{ed.Tool, ed.Batch, ed.Parent, ed.Stream}
		if seen[key] {
			return
		}
		seen[key] = true
		edges = append(edges, ed)
	}

	for _, i := range b.canonical {
		e := &b.ix.Entries[i]
		if e.Child == 0 {
			continue
		}
		anchor := e.Tool
		if anchor == 0 {
			// A forked child is announced by a result that carries no separate
			// pointer, so the tool id comes from the result's own block.
			for _, blk := range b.blocksOf(e) {
				if blk.Kind == index.BlockToolResult && blk.ToolID != 0 {
					anchor = blk.ToolID
					break
				}
			}
		}
		switch {
		case e.Kind == index.KindJournal:
			// The run journal is the largest source by a wide margin: most children
			// on disk are reached this way and by nothing else. It says which
			// children belong to a run and nothing more - no timestamp, no name, no
			// pointer to the call - so the launch itself still comes from the
			// parent's own result.
			add(spawnEdge{Tool: batchTool[e.Batch], Batch: e.Batch, Stream: e.Child,
				Via: "run journal", Quality: model.StrongInference, Ref: ref(e)})
		case e.Kind == index.KindMeta:
			// A sidecar normally says only what type of agent it is. The one
			// thing it does carry, for a child of a child, is which agent
			// started it - and that is the only evidence of nesting anywhere.
			add(spawnEdge{Tool: anchor, Parent: e.StartedBy, Stream: e.Child,
				Via: "child sidecar", Quality: model.ExactUnique, Ref: ref(e)})
		case e.Trigger == index.TriggerNotification:
			add(spawnEdge{Tool: anchor, Stream: e.Child, Via: "completion notification",
				Quality: model.ExactUnique, Ref: ref(e)})
		default:
			add(spawnEdge{Tool: anchor, Stream: e.Child, Via: "parent tool result",
				Quality: model.ExactUnique, Ref: ref(e)})
		}
	}
	sort.SliceStable(edges, func(i, j int) bool {
		if edges[i].Stream != edges[j].Stream {
			return b.str(edges[i].Stream) < b.str(edges[j].Stream)
		}
		if edges[i].Tool != edges[j].Tool {
			return b.str(edges[i].Tool) < b.str(edges[j].Tool)
		}
		return b.str(edges[i].Batch) < b.str(edges[j].Batch)
	})

	spawned := map[uint32]bool{}
	for _, ed := range edges {
		if _, ok := b.byStream[ed.Stream]; !ok {
			// The journal announces a child before its transcript exists, so this
			// is routine rather than an edge case. It is carried as an open
			// reference and resolves in a later round when the file appears.
			b.open("child_stream", b.str(ed.Stream), "announced, but no transcript has landed yet")
			continue
		}
		b.stats.Spawns++
		if ed.Tool == 0 && ed.Parent != 0 {
			// A nested child, named only by the agent that started it. The call
			// itself lives in that agent's own file and this sidecar does not
			// name it, so the edge runs stream to stream - weaker than a call,
			// and far better than attaching the child to the wrong lineage.
			if from, ok := b.byStream[ed.Parent]; ok {
				b.spawnEdges = append(b.spawnEdges, ed)
				spawned[ed.Stream] = true
				b.stats.SpawnsResolved++
				_ = from
				continue
			}
		}
		if ed.Tool == 0 {
			// A child announced by a run whose launch call is not in the landed
			// data. The child is real, so the gap is recorded rather than the edge
			// quietly dropped.
			b.open("spawn_call", b.str(ed.Batch), "a batch names children but its launch call was never landed")
			continue
		}
		if _, known := b.toolByID[ed.Tool]; !known {
			b.open("spawn_call", b.str(ed.Tool), "a child names a call that was never landed")
			continue
		}
		b.markAgentCall(ed.Tool)
		b.spawnEdges = append(b.spawnEdges, ed)
		spawned[ed.Stream] = true
		b.stats.SpawnsResolved++
	}

	// A child that nothing names does exist, so this is a state to record rather
	// than an assumption to assert. Such a stream stays attached to the session
	// and to no Talk, because guessing a parent would be worse than saying none
	// was found.
	for _, s := range b.streams {
		if s.Role == model.StreamChild && !spawned[s.ID] {
			b.stats.ChildrenOrphan++
			b.open("spawn_of_child", s.Name, "no landed record names the call that started this stream")
		}
	}
}

// markAgentCall records that a tool use started a child agent, so it is written
// as a request to start one rather than as an ordinary tool step.
//
// The two are the same record. What separates them is that a child stream came
// out of it, which is only known once the spawn join has run.
func (b *builder) markAgentCall(toolID uint32) {
	if t, ok := b.toolByID[toolID]; ok {
		t.StartsAgent = true
	}
}

// emitDelegationSteps writes the steps of the asynchronous return chain.
//
// The canonical order is: a call starts a child, an acknowledgement says it
// started, the child does its work in its own stream, the child produces its
// output, the runtime notifies the parent, and only then does the parent make
// its next provider call. The acknowledgement is not the result; counting it as
// one records an empty output for every asynchronous delegation.
//
// The child's output belongs to the CHILD stream. The parent receives a compact
// boundary that references it and never absorbs its messages or tools. That is
// what stops a rendered conversation repeating every subagent's work inside its
// parent.
func (b *builder) emitDelegationSteps() {
	for _, i := range b.canonical {
		e := &b.ix.Entries[i]
		parent := b.containerOf(e)
		switch {
		case e.Flags.Has(index.FlagLaunchAck):
			id := sessionflow.RefID("ack", ref(e))
			b.node(sessionflow.Node{
				Entity: sessionflow.Entity{ID: id}, Kind: model.KindAgentLaunchAck,
				Parent: parent, Stream: b.streamName(e), Ref: refPtr(ref(e)),
			})
			b.stats.Steps++
		case e.Trigger == index.TriggerNotification:
			id := sessionflow.RefID("notify", ref(e))
			b.node(sessionflow.Node{
				Entity: sessionflow.Entity{ID: id}, Kind: model.KindRuntimeNotification,
				Parent: parent, Stream: b.streamName(e), Ref: refPtr(ref(e)),
			})
			b.stats.Steps++
			b.stats.Notifications++
			b.reportOn(e, id)
		}
	}

	// The child's final answer, owned by the child.
	//
	// There are two records of it and they are not the same thing. The child's
	// own last response is what it said; the run journal's result record is what
	// it RETURNED - a structured value that appears nowhere in the child's
	// transcript. Measured: 2,353 of 2,436 journal results are found nowhere
	// else. Both are evidence of one output, so the step carries both.
	returned := map[uint32]*index.Entry{}
	for _, i := range b.canonical {
		e := &b.ix.Entries[i]
		if e.Kind == index.KindJournal && e.Flags.Has(index.FlagChildResult) && e.Child != 0 {
			if _, seen := returned[e.Child]; !seen {
				returned[e.Child] = e
			}
		}
	}
	for _, s := range b.streams {
		if s.Role != model.StreamChild {
			continue
		}
		last := b.lastAssistant(s)
		result := returned[s.ID]
		if last == nil && result == nil {
			continue
		}
		var refs []sessionflow.Ref
		primary := last
		if primary == nil {
			primary = result
		}
		refs = append(refs, ref(primary))
		if last != nil && result != nil {
			refs = append(refs, ref(result))
		}
		a := map[string]any{}
		if result != nil {
			a["returned_value"] = model.ObservedReplayable
		} else {
			a["returned_value"] = model.Unavailable
		}
		id := sessionflow.NodeID("output", s.Name)
		b.node(sessionflow.Node{
			Entity: sessionflow.Entity{ID: id}, Kind: model.KindAgentOutput,
			Parent: b.containerOf(primary), Stream: s.Name,
			Ref: refPtr(refs[0]), Refs: refs, Attrs: attrs(a),
		})
		b.stats.Steps++
		b.relate(model.RelEndsWith, s.NodeID, id, model.ExactUnique,
			"the last response in the child stream, and what it returned", refs...)
	}
}

// reportOn links a completion notification to the child stream it is about.
//
// The edge points at the child, because that is the question a reader has when
// they meet a notification: which child finished?
//
// The notification names two things and only one of them is a reliable key. The
// task id is a generic handle whose meaning depends on what was launched, so it
// names an agent only a small share of the time. The CALL id it also carries is
// exact, and the call already has an edge to the child, so resolving through the
// call works whenever resolving through the task id does not.
func (b *builder) reportOn(e *index.Entry, notifyID string) {
	if e.Child != 0 {
		if child, ok := b.byStream[e.Child]; ok {
			b.relate(model.RelReports, notifyID, child.NodeID, model.ExactUnique,
				"task id on the notification", ref(e))
			return
		}
		b.stats.NotifyUnmatched++
		b.open("notified_child", b.str(e.Child),
			"a notification names a child whose stream has not landed")
		return
	}
	if e.Tool == 0 {
		return
	}
	for _, ed := range b.spawnEdges {
		if ed.Tool != e.Tool {
			continue
		}
		if child, ok := b.byStream[ed.Stream]; ok {
			b.relate(model.RelReports, notifyID, child.NodeID, model.StrongInference,
				"the call the notification completes", ref(e))
		}
	}
}

// lastAssistant returns a stream's final model response.
func (b *builder) lastAssistant(s *streamInfo) *index.Entry {
	for i := len(s.Entries) - 1; i >= 0; i-- {
		e := &b.ix.Entries[s.Entries[i]]
		if e.Kind == index.KindAssistant && !e.Flags.Has(index.FlagSynthetic) {
			return e
		}
	}
	return nil
}

func (b *builder) streamName(e *index.Entry) string {
	if s, ok := b.byStream[e.Stream]; ok {
		return s.Name
	}
	return ""
}

// emitSpawnRelations writes the edges stage 5 resolved.
//
// Cross-stream flow is a relation and never containment. A child agent's steps
// stay in the child's own stream and the parent holds a reference to them. That
// is what keeps a rendered conversation from repeating every subagent's work
// inside the parent that asked for it.
func (b *builder) emitSpawnRelations() {
	for _, ed := range b.spawnEdges {
		child, ok := b.byStream[ed.Stream]
		if !ok {
			continue
		}
		from := sessionflow.NodeID("tool", b.str(ed.Tool))
		if _, exists := b.nodes[from]; !exists {
			continue
		}
		b.relate(model.RelStarts, from, child.NodeID, ed.Quality, ed.Via, ed.Ref)
	}
}
