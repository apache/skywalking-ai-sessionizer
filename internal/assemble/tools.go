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

// toolUse is one tool use: the request, what came back, and where each is.
//
// A tool use is ONE step, not three. The request and the result are separate
// records, often far apart in the stream, but they pair one to one and a reader
// thinks of them as a single event: it ran this, and got that back. Splitting
// them into sibling steps triples the node count and makes every consumer
// rejoin what the model already knows belongs together.
type toolUse struct {
	ToolID uint32
	Name   string
	Use    *index.Entry
	UseOrd uint16
	Result *index.Entry
	ResOrd uint16
	Stream *streamInfo
	NodeID string

	// Ambiguous means several results carry this id. An exact identifier match
	// does not guarantee a unique match, and where it is not unique the
	// assembler must not choose one.
	Ambiguous bool

	// StartsAgent is set once the spawn join finds that a child stream came out
	// of this call.
	StartsAgent bool
}

// Stage 4 - join tools.
//
// The join key is the tool-use id, which is exact. Its prefix varies - server
// side tools use a different one - so nothing may match on the prefix.
//
// A single record can carry several tool blocks, which is why blocks are
// indexed apart from records. Folding them together would keep one id and lose
// the rest.
func (b *builder) stage4Tools() {
	byTool := map[uint32]*toolUse{}
	b.toolByID = byTool
	for _, s := range b.streams {
		for _, e := range b.entriesOf(s) {
			for _, blk := range b.blocksOf(e) {
				if blk.ToolID == 0 {
					continue
				}
				t, ok := byTool[blk.ToolID]
				if !ok {
					t = &toolUse{
						ToolID: blk.ToolID, Stream: s,
						NodeID: sessionflow.NodeID("tool", b.str(blk.ToolID)),
					}
					byTool[blk.ToolID] = t
					b.tools = append(b.tools, t)
				}
				switch blk.Kind {
				case index.BlockToolUse:
					if t.Use == nil {
						t.Use, t.UseOrd, t.Name = e, blk.Ord, b.str(blk.Name)
					}
				case index.BlockToolResult:
					if t.Result != nil {
						t.Ambiguous = true
						continue
					}
					t.Result, t.ResOrd = e, blk.Ord
				}
			}
		}
	}
	sort.Slice(b.tools, func(i, j int) bool { return b.tools[i].NodeID < b.tools[j].NodeID })

	for _, t := range b.tools {
		if t.Use == nil {
			// A result whose request is not in the landed data. It is real
			// evidence of a call, so it is carried rather than dropped.
			b.open("tool_use", b.str(t.ToolID), "a result arrived for a request that was never landed")
			continue
		}
		b.stats.ToolUses++
		switch {
		case t.Ambiguous:
			b.stats.ToolsUnmatched++
			b.open("tool_result", b.str(t.ToolID), "several results carry this tool-use id")
		case t.Result != nil:
			b.stats.ToolsResolved++
		default:
			b.stats.ToolsUnmatched++
			b.open("tool_result", b.str(t.ToolID), "no result landed for this tool use")
		}
	}
}

// manifestFor finds the manifest describing what a call launched.
func (b *builder) manifestFor(t *toolUse) *index.Entry {
	// The batch is named by the launch RESULT, not by the call: the call says
	// what to run, and only the answer says which run it became.
	if t.Result == nil || t.Result.Batch == 0 {
		return nil
	}
	for _, i := range b.canonical {
		e := &b.ix.Entries[i]
		if e.Kind == index.KindManifest && e.Batch == t.Result.Batch {
			return e
		}
	}
	return nil
}

// emitTool writes one tool step.
//
// Two states must stay expressible rather than being hidden. A tool use whose
// result never arrived - the run was interrupted, or the session ended - is
// recorded with an unavailable result. It is not dropped, and it is not given
// an empty result. And where the runtime does not report execution timing apart
// from record timestamps, timing is unavailable rather than guessed from the
// gap between two records.
func (b *builder) emitTool(t *toolUse, parent string) {
	a := map[string]any{
		"name": t.Name,
		// The request lives in the tool_use block; the reference below locates it.
		"timing": model.Unavailable,
	}
	refs := []sessionflow.Ref{blockRef(t.Use, t.UseOrd)}
	quality := model.Unresolved
	switch {
	case t.Ambiguous:
		a["result"] = model.ContentUnavailable
		quality = model.ExactAmbiguous
	case t.Result != nil:
		refs = append(refs, blockRef(t.Result, t.ResOrd))
		a["result"] = model.ContentAvailable
		quality = model.ExactUnique
	default:
		a["result"] = model.Unavailable
	}
	a["result_join"] = quality

	kind := model.KindTool
	if t.StartsAgent {
		kind = model.KindAgentCall
	}
	// A workflow call has a manifest describing what it launched - the workflow's
	// name and the script it ran. It sits in its own file and nothing else
	// points at it, so the call it describes is where it belongs.
	if m := b.manifestFor(t); m != nil {
		refs = append(refs, ref(m))
		if lbl := b.str(m.Label); lbl != "" {
			a["launched"] = lbl
		}
	}
	// The two references ARE the join: refs[0] is the request, refs[1] is the
	// result. An edge saying the same thing would be the largest relation type
	// in the output by an order of magnitude, and its endpoint would be a record
	// id for which no node exists - a dangling edge restating a fact the node
	// already carries.
	b.node(sessionflow.Node{
		Entity: sessionflow.Entity{ID: t.NodeID}, Kind: kind,
		Parent: parent, Stream: t.Stream.Name,
		Ref: refPtr(refs[0]), Refs: refs, Attrs: attrs(a),
	})
	b.stats.Steps++
}
