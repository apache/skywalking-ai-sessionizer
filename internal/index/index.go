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

// Package index holds the derived lookup structure the assembler resolves
// conversation structure against.
//
// The index exists because structure resolution needs identifiers, not content:
// removing duplicate records needs record ids, provider-call grouping needs message ids,
// tool joins need tool-use ids, spawn joins need agent and run ids. None of it
// reads message text. Measured on a real corpus, the index is ~12x smaller than
// the payloads it describes - 16 MB for the largest session, 95 MB for 1.1 GB
// of conversation - which is what lets the whole thing stay resident with
// random access.
//
// It is DERIVED and DISPOSABLE. Deleting it loses nothing; it rebuilds from the
// landed files. Nothing downstream may treat it as authoritative.
package index

// Schema is the on-disk index version. Bump it when Entry or Block changes;
// a mismatch discards the index and rebuilds rather than migrating.
const Schema = 8

// Kind classifies a record without reading it.
type Kind uint8

const (
	KindUnknown Kind = iota
	KindUser
	KindAssistant
	KindAttachment
	KindSystem
	KindMeta     // agent sidecar
	KindJournal  // workflow journal record
	KindManifest // workflow manifest
	KindScript   // workflow script
	KindOther    // an out-of-band record type carrying no uuid
)

// Trigger says what caused a prompt cycle.
//
// A role, not a runtime field: an adapter maps whatever its runtime calls this
// onto one of these. The distinction decides where a Talk begins -
// external input starts a Talk, a runtime notification continues one.
type Trigger uint8

const (
	TriggerNone Trigger = iota
	TriggerExternal
	TriggerNotification
	TriggerOther
)

// Flags carry small facts that a lookup needs but that no identifier expresses.
//
// They are bits rather than fields because each is a yes/no an adapter can
// answer from a single source field, and the alternative - storing the runtime
// value - would put that runtime's vocabulary in the index.
type Flags uint16

const (
	// FlagSynthetic marks an assistant-role record fabricated by the client
	// rather than returned by a provider. It must never be counted as output.
	FlagSynthetic Flags = 1 << iota
	// FlagEpochBoundary marks an explicit model-context reset. This is the ONLY
	// admissible evidence of one; it is never inferred from token counters.
	FlagEpochBoundary
	// FlagEpochSummary marks the carried-forward summary that reset produced.
	FlagEpochSummary
	// FlagStopReason marks a fragment carrying a provider stop reason, which says
	// the call finished and its usage numbers are real.
	//
	// It does NOT identify the last fragment. A main transcript stamps the same
	// stop reason on every fragment of a call, so only line order finds the last
	// one. A call with no stop reason anywhere still has a usage block, but its
	// output count is a streaming stub of a few tokens - so usage availability is
	// gated on this flag, never on the field being present.
	FlagStopReason
	// FlagExternalInput marks a record carrying input that originated outside
	// the agent. Some of these exist ONLY as attachments, with no message twin,
	// so a reader following messages alone loses real human input.
	FlagExternalInput
	// FlagInjection marks material the harness injected into model context. It
	// shapes a response as much as an external turn does, and in some runtimes
	// it is the only record of an input.
	FlagInjection
	// FlagChildResult marks a synchronous child return: a result that repeats
	// the child's own output. The child stream owns that output, so a renderer
	// drops this copy rather than showing the same work twice.
	FlagChildResult
	// FlagError marks a provider or runtime error record.
	FlagError
	// FlagInterrupt marks an externally initiated interruption.
	FlagInterrupt
	// FlagTurnDuration marks a runtime-reported duration for a completed turn.
	FlagTurnDuration
	// FlagCommand marks a command invoked in-line rather than said to the model.
	FlagCommand
	// FlagNotice marks the runtime telling the requester about the session
	// itself - a transport dropped, a fallback taken, a summary of time away.
	FlagNotice
	// FlagLaunchAck marks an acknowledgement that a child agent started. It is
	// NOT a result, and treating it as one attributes an empty output to every
	// asynchronous delegation.
	FlagLaunchAck
)

// Has reports whether every bit in f is set.
func (v Flags) Has(f Flags) bool { return v&f == f }

// Entry is one indexed record.
//
// Every field is fixed width with no pointers or strings - identifiers are
// interned to uint32 - so the entry array is one contiguous allocation and the
// persisted form is a flat block.
type Entry struct {
	// Landed location: which landed file, and which line within it. This is the
	// only position the index needs - resolution never reads source positions,
	// and ordering by (Seq, Row) already reproduces source order because landed
	// sequences are monotonic and deltas are contiguous.
	//
	// Source position and integrity live in the landed record's own envelope,
	// beside the bytes they describe. Duplicating them here would cost 12 of
	// 69 bytes per entry for data one dereference away that nothing reads.
	Seq uint32
	Row uint32

	Stream uint32 // interned: "main" or an agent id
	// Batch groups records the runtime filed together under one orchestration -
	// a set of child agents started as a unit. It is a join key for resolving
	// which call started which child, and nothing in the model corresponds to
	// it.
	Batch uint32
	Kind  Kind
	TS    int64 // record timestamp, unix nanoseconds; 0 when absent

	Trigger Trigger
	Flags   Flags

	// Interned join keys, named for the ROLE they play rather than for any one
	// runtime's field. An adapter maps its own identifiers onto these; nothing
	// here may carry a runtime's vocabulary, because a rename in that runtime
	// must not reach past its adapter.
	//
	// 0 means absent, which is common: many record types carry no identity.
	Record uint32 // this record's own id
	Parent uint32 // its containment parent
	Call   uint32 // the provider call this record is a fragment of
	// Run is one loop the agent performed in response to a trigger.
	//
	// The runtime supplies the grouping; the assembler builds a Run node per
	// distinct value. It is not one model call - a run averages thirteen of
	// them - and the agent does not start its own: every run measured was
	// triggered from outside the loop, by input or by the runtime reporting
	// that a child had finished.
	Run uint32

	// Continues is the record a new model context resumes from, across a reset.
	// Present only on a reset boundary, where it is the only link back: the
	// boundary's own parent is empty.
	Continues uint32
	// Tool is the tool use this record is about, where the runtime says so
	// outside the content blocks - a child's sidecar naming the call that
	// created it, or a notification naming the call it completes.
	Tool uint32
	// Child is the child agent stream this record names, whether it is
	// announcing that one started or that one finished. It is what makes the
	// join to a child possible from the parent's side.
	Child uint32
	// StartedBy is the stream that started this record's own stream, for a child
	// of a child. Without it a nested child attaches to the parent lineage
	// rather than to the agent that started it.
	StartedBy uint32
	// Label is a name the runtime gave something - a conversation's title, what
	// a child agent was asked to do, a workflow's name. It is the only naming
	// evidence in the data; everything else has to be derived.
	Label uint32

	// Blocks indexes into Index.Blocks: [BlockFirst, BlockFirst+BlockCount).
	BlockFirst uint32
	BlockCount uint32
}

// BlockKind classifies a joinable content block.
type BlockKind uint8

const (
	BlockUnknown BlockKind = iota
	BlockToolUse
	BlockToolResult
	// BlockText is visible message text.
	BlockText
	// BlockThinking is reasoning content attached to a model response.
	BlockThinking
	// BlockOther is any other content element: an image, a document, a
	// runtime-specific block. It is recorded so the content array's shape stays
	// faithful, because block positions are how a step is located inside a
	// record.
	BlockOther
)

// Block is one joinable content block.
//
// Blocks are indexed separately rather than folded into Entry because a single
// record can carry several - a line usually holds one content block, but not
// always - and collapsing them would silently drop tool ids and break the join
// they exist to serve.
type Block struct {
	Entry  uint32 // index into Index.Entries
	Ord    uint16 // position within the record's content array
	Kind   BlockKind
	ToolID uint32 // interned tool_use id
	Name   uint32 // interned tool name, for tool_use
}
