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

// Package model is the runtime-agnostic conversation vocabulary.
//
// It is the Go form of docs/en/concepts-and-designs/unified-conversation-model.md,
// and it is deliberately small: names, and the rules that give them meaning.
// No parsing, no I/O, no runtime knowledge. An adapter's vocabulary stops at
// its adapter, so nothing here may name a field of any agent product.
package model

// Structural node kinds - the hierarchy above the leaves.
//
// Segment sits above Session and cuts ACROSS every stream beneath it: it is a
// time window chosen for commit, not a branch of the stream tree.
const (
	KindConversation = "conversation"
	KindSegment      = "segment"
	KindSession      = "session"
	KindStream       = "stream"
	KindEpoch        = "epoch"
	KindTalk         = "talk"
	KindRun          = "run"
)

// Step kinds - the leaves, one per observed occurrence inside a run.
//
// A step that cannot be observed is reported unavailable rather than
// synthesised, so this list names only things a runtime can actually emit.
const (
	// Message family.
	KindMessageExternal  = "message.external"  // input originating outside the agent
	KindMessageAssistant = "message.assistant" // agent output visible to the requester
	// KindMessageSynthetic is an assistant-role record FABRICATED BY THE CLIENT -
	// connection loss, provider error, quota or auth failure - surfaced inline so
	// the requester sees it. No model produced it.
	KindMessageSynthetic = "message.synthetic"

	// Context family.
	KindContextInjection = "context.injection" // material the harness injected into model context

	// Model family.
	KindLLMCall  = "llm.call" // one provider attempt: input manifest plus response
	KindThinking = "thinking" // reasoning content associated with a response

	// Tool family. One tool use is ONE step carrying request, execution and
	// result - not three sibling steps. See the Tool type below.
	KindTool = "tool"

	// Agent family.
	KindAgentCall           = "agent.call"           // a request to start or continue a child agent
	KindAgentLaunchAck      = "agent.launch_ack"     // acknowledgement that a child started - NOT its result
	KindAgentOutput         = "agent.output"         // the child's final output, owned by the CHILD stream
	KindRuntimeNotification = "runtime.notification" // the runtime telling the parent a child completed

	// Epoch family.
	KindEpochBoundary = "epoch.boundary" // an explicit model-context reset
	KindEpochSummary  = "epoch.summary"  // the carried-forward summary produced by that reset

	// Control family.
	KindErrorAPI          = "error.api"
	KindControlInterrupt  = "control.interrupt"
	KindControlPermission = "control.permission"
	KindControlCommand    = "control.command"
	KindTurnDuration      = "turn.duration"
	// KindControlNotice is the runtime telling the requester something about
	// the session itself: a transport dropped, a fallback taken, a summary of
	// what happened while they were away.
	KindControlNotice = "control.notice"
)

// Relation types.
//
// The tree carries direct semantic ownership only: every node has at most one
// containment parent. Everything else is a sparse typed relation, each carrying
// its own correlation quality and source references. Cross-stream flow is never
// expressed as containment - that is what stops a rendered conversation
// duplicating every subagent's work inside its parent.
//
// Each name is a verb, so an edge reads as a sentence from its start to its
// end: "this record is the RESULT OF that tool", "this call STARTS that
// stream". A noun would name a thing rather than a relationship, and would not
// read in either direction.
const (
	// RelStarts: an agent call -> the child stream it created.
	RelStarts = "starts"
	// RelReports: a runtime notification -> the child stream that finished.
	//
	// It points at the child rather than away from it, because that is the
	// question a reader has when they meet a notification: which child is this
	// about?
	RelReports = "reports"
	// RelEndsWith: a child stream -> the step carrying its final output.
	//
	// Containment already says the output is in the stream. This says it is the
	// one the parent was waiting for.
	RelEndsWith = "ends_with"
	// RelResultOf: a record -> the tool use whose result it carried.
	RelResultOf = "result_of"
	// RelFollows: an epoch -> the epoch it continues after a context reset.
	RelFollows = "follows"
	// RelSummarizes: a summary -> the reset that produced it.
	RelSummarizes = "summarizes"
	// RelInSegment places a Talk in the activity window that will commit it.
	//
	// This is a relation and not containment on purpose. A Segment is a time
	// window and a Session outlives many of them, so a Session cannot sit under
	// a Segment in a tree where every node has one parent. The structural spine
	// stays session -> stream -> epoch -> talk -> run -> step, and the segment
	// cuts across it as an edge.
	RelInSegment = "in_segment"

	// RelRetries: an attempt -> the attempt it replaced.
	RelRetries = "retries"
	// RelCancels: an interruption -> the operation it ended.
	RelCancels = "cancels"
	// RelInputOf: a record -> the provider call whose input it was part of.
	RelInputOf = "input_of"
)

// Correlation quality - the qualification on every join between two records.
//
// An exact identifier does NOT guarantee a unique match. Where several
// candidates share a key the relation stays ExactAmbiguous, and the assembler
// must not choose one.
const (
	ExactUnique     = "exact_unique"
	ExactAmbiguous  = "exact_ambiguous"
	StrongInference = "strong_inference"
	WeakInference   = "weak_inference"
	Unresolved      = "unresolved"
	Conflict        = "conflict"
)

// Claim state - how a material claim was arrived at.
const (
	ObservedReplayable = "observed_replayable" // derived from retained evidence that can be replayed
	ObservedReportOnly = "observed_report_only"
	Proposed           = "proposed"
	Unavailable        = "unavailable"
)

// Content state - availability of a referenced payload.
const (
	ContentAvailable   = "available"
	ContentRedacted    = "redacted"
	ContentOmitted     = "omitted"
	ContentHashOnly    = "hash_only"
	ContentSizeOnly    = "size_only"
	ContentTruncated   = "truncated"
	ContentUnavailable = "unavailable"
)

// StreamRole distinguishes lineages within one session.
const (
	StreamMain      = "main"
	StreamChild     = "child"
	StreamAuxiliary = "auxiliary"
	StreamJudge     = "judge"
)

// Trigger says what caused a Talk to begin.
//
// This is the distinction stage 1.7 turns on. A background agent finishing and
// the parent resuming is mechanically a new prompt cycle, but the requester
// said nothing - it is the same interaction continuing, and it belongs INSIDE
// the Talk that spawned it.
const (
	TriggerExternal     = "external"     // input from outside the agent: a Talk begins
	TriggerNotification = "notification" // the runtime reporting a child finished: the Talk continues
	TriggerUnknown      = "unknown"
)

// StartsTalk reports whether a trigger begins a new Talk.
//
// Only external input does. Splitting on every prompt cycle would break one
// interaction into as many fragments as it delegated work.
func StartsTalk(trigger string) bool { return trigger == TriggerExternal }

// CountsAsAgentOutput reports whether a step kind may be counted as output the
// agent produced.
//
// message.synthetic carries the assistant role and sits in the ordered stream
// like any response, but no model produced it. Counted as assistant output it
// corrupts output statistics and attributes text to the agent that the agent
// never wrote.
func CountsAsAgentOutput(kind string) bool { return kind == KindMessageAssistant }

// OwnedByChildStream reports whether a step belongs to the child stream rather
// than to the parent that requested it.
func OwnedByChildStream(kind string) bool { return kind == KindAgentOutput }
