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

// emitSteps writes the leaves of the tree.
//
// Emission is separated from resolution on purpose. A step's containment parent
// is the run, the talk or the epoch that holds it, and none of those are known
// until stage 7 has run. Emitting as each stage resolved would mean either
// guessing a parent or revisiting every node afterwards.
func (b *builder) emitSteps() {
	for _, c := range b.calls {
		b.emitCall(c, b.containerOf(c.Fragments[0]))
	}
	for _, t := range b.tools {
		if t.Use == nil {
			continue
		}
		// A tool block sits inside a provider call's response, so the call
		// contains it. Where the call is not in the landed data the record's own
		// container is used instead.
		parent := sessionflow.NodeID("call", b.str(t.Use.Call))
		if _, ok := b.nodes[parent]; !ok {
			parent = b.containerOf(t.Use)
		}
		b.emitTool(t, parent)
	}
	b.emitEpochSteps()
	b.emitDelegationSteps()
	b.emitInputSteps()
	b.emitControlSteps()
	b.emitSpawnRelations()
}

// emitInputSteps writes the records that carry input into the agent.
//
// Two of these are easy to lose. Some input from outside the agent exists ONLY
// as an attachment, with no message record beside it, so following messages
// alone drops real human turns. And injected material - reminders, catalogues,
// memory, environment state - shapes a response as much as a human turn does,
// and in some cases it is the only record of an input at all. Both are
// first-class steps here.
func (b *builder) emitInputSteps() {
	for _, i := range b.canonical {
		e := &b.ix.Entries[i]
		switch {
		case e.Flags.Has(index.FlagExternalInput):
			b.node(sessionflow.Node{
				Entity: sessionflow.Entity{ID: sessionflow.RefID("input", ref(e))},
				Kind:   model.KindMessageExternal, Parent: b.containerOf(e),
				Stream: b.streamName(e), Ref: refPtr(ref(e)),
			})
			b.stats.Steps++
		case e.Flags.Has(index.FlagInjection):
			b.node(sessionflow.Node{
				Entity: sessionflow.Entity{ID: sessionflow.RefID("inject", ref(e))},
				Kind:   model.KindContextInjection, Parent: b.containerOf(e),
				Stream: b.streamName(e), Ref: refPtr(ref(e)),
			})
			b.stats.Steps++
		}
	}
}

// emitControlSteps writes what the runtime said about the session itself.
//
// A provider error is part of what happened. Dropping it makes a retry look
// like a single call that simply took longer. The same argument covers a turn's
// measured duration, a command invoked in-line, and a notice that the transport
// dropped: each is something the requester saw, and none of them is a message.
//
// Every system record produces one of these. An unrecognised subtype becomes a
// notice rather than nothing, because a runtime that adds one should not make
// this quietly start losing records.
func (b *builder) emitControlSteps() {
	for _, i := range b.canonical {
		e := &b.ix.Entries[i]
		var kind, prefix string
		a := map[string]any{}
		switch {
		case e.Flags.Has(index.FlagError):
			kind, prefix = model.KindErrorAPI, "error"
			a["retry_state"] = model.Unavailable
		case e.Flags.Has(index.FlagTurnDuration):
			// The runtime measured this. It is not the gap between two record
			// timestamps, which would count everything else that happened in
			// between.
			kind, prefix = model.KindTurnDuration, "turn"
			a["measured_by"] = model.ObservedReplayable
		case e.Flags.Has(index.FlagCommand):
			kind, prefix = model.KindControlCommand, "command"
		case e.Flags.Has(index.FlagNotice):
			kind, prefix = model.KindControlNotice, "notice"
		default:
			continue
		}
		b.node(sessionflow.Node{
			Entity: sessionflow.Entity{ID: sessionflow.RefID(prefix, ref(e))},
			Kind:   kind, Parent: b.containerOf(e),
			Stream: b.streamName(e), Ref: refPtr(ref(e)), Attrs: attrs(a),
		})
		b.stats.Steps++
	}
}
