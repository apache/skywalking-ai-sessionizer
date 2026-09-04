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

package claudecode

import "github.com/apache/skywalking-ai-sessionizer/pkg/model"

// Dialect names this runtime's vocabulary and the version of the mapping below.
//
// It is separate from the adapter name. `claude-code-local` says how records
// were acquired; this says how they are read. A push receiver for the same
// runtime would be a different adapter and the same dialect.
const Dialect = "claude-code/1"

// glossary is what Claude Code calls the things the model names.
//
// It lives beside the extraction code on purpose. Every entry with a Native
// value corresponds to a field the converter reads, so a rename in the
// runtime changes both together, and a test checks that the model's whole
// vocabulary is accounted for here.
//
// An empty Native is not a gap. A Talk, a Segment, a correlation quality - the
// runtime records none of them, and saying so is more useful than a plausible
// word that would send a reader looking for a field that does not exist.
var glossary = model.NewGlossary(Dialect,
	// ---- the roles a record plays ----
	model.Term{Unified: model.RoleID, Native: "uuid", Where: "record"},
	model.Term{Unified: model.RoleParent, Native: "parentUuid", Where: "record"},
	model.Term{Unified: model.RoleCall, Native: "message.id", Where: "record",
		Note: "not requestId, which a client-fabricated record can reuse"},
	model.Term{Unified: model.RoleRun, Native: "promptId", Where: "record",
		Note: "on user records only; a model response reaches one through its parents"},
	model.Term{Unified: model.RoleBatch, Native: "toolUseResult.runId", Where: "a workflow launch result",
		Note: "also the name of the directory the batch's records are filed under"},
	model.Term{Unified: model.RoleStream, Native: "agentId", Where: "a child record, and its filename",
		Note: "main has no name of its own; it is the session's own transcript"},
	model.Term{Unified: model.RoleContinues, Native: "logicalParentUuid", Where: "a compact_boundary record",
		Note: "the only link back; a boundary's own parentUuid is null"},
	model.Term{Unified: model.RoleTool, Native: "tool_use_id", Where: "a tool_result block, a notification, a sidecar"},
	model.Term{Unified: model.RoleChild, Native: "toolUseResult.agentId", Where: "a launch result, a journal record"},
	model.Term{Unified: model.RoleTime, Native: "timestamp", Where: "record"},
	model.Term{Unified: model.RoleTrigger, Native: "origin.kind", Where: "record, and attachment.origin.kind"},

	// ---- structure the model derived; the runtime records none of it ----
	model.Term{Unified: model.KindConversation, Note: "supplied, never inferred; defaults to the session id"},
	model.Term{Unified: model.KindSegment, Note: "an activity window chosen for commit"},
	model.Term{Unified: model.KindSession, Native: "sessionId", Where: "record, and the transcript filename"},
	model.Term{Unified: model.KindStream, Native: "agentId", Where: "a child record",
		Note: "the parent lineage is the session transcript itself"},
	model.Term{Unified: model.KindEpoch, Note: "the span between two compact_boundary records"},
	model.Term{Unified: model.KindTalk, Note: "one readable interaction; derived from where a run was triggered"},
	model.Term{Unified: model.KindRun, Native: "promptId", Where: "record",
		Note: "one loop per distinct promptId"},

	// ---- steps ----
	model.Term{Unified: model.KindMessageExternal, Native: "origin.kind == \"human\"", Where: "a user record",
		Note: "also a queued_command attachment whose commandMode is \"prompt\""},
	model.Term{Unified: model.KindMessageAssistant, Native: "text block", Where: "an assistant record"},
	model.Term{Unified: model.KindMessageSynthetic, Native: "message.model == \"<synthetic>\"", Where: "an assistant record",
		Note: "the client wrote it; no model produced it"},
	model.Term{Unified: model.KindContextInjection, Native: "attachment", Where: "an attachment record"},
	model.Term{Unified: model.KindLLMCall, Native: "message.id", Where: "assistant records sharing one",
		Note: "one call is several records; 2.09 on average"},
	model.Term{Unified: model.KindThinking, Native: "thinking block", Where: "an assistant record"},
	model.Term{Unified: model.KindTool, Native: "tool_use + tool_result", Where: "two records",
		Note: "one step here, two blocks there"},
	model.Term{Unified: model.KindAgentCall, Native: "Agent, Skill or Workflow tool_use", Where: "an assistant record"},
	model.Term{Unified: model.KindAgentLaunchAck, Native: "toolUseResult.status == \"async_launched\"", Where: "a tool result",
		Note: "an acknowledgement, not a result"},
	model.Term{Unified: model.KindAgentOutput, Note: "the last model response in a child stream"},
	model.Term{Unified: model.KindRuntimeNotification, Native: "<task-notification>", Where: "a user record's content"},
	model.Term{Unified: model.KindEpochBoundary, Native: "system/compact_boundary", Where: "a system record"},
	model.Term{Unified: model.KindEpochSummary, Native: "isCompactSummary", Where: "a user record"},
	model.Term{Unified: model.KindErrorAPI, Native: "system/api_error", Where: "a system record"},
	model.Term{Unified: model.KindControlInterrupt, Native: "toolUseResult.interrupted", Where: "a tool result"},
	model.Term{Unified: model.KindControlPermission, Native: "permission-mode", Where: "a record type"},
	model.Term{Unified: model.KindControlCommand, Native: "system/local_command", Where: "a system record"},
	model.Term{Unified: model.KindTurnDuration, Native: "system/turn_duration", Where: "a system record",
		Note: "the runtime measured it; it is not the gap between two record timestamps"},
	model.Term{Unified: model.KindControlNotice, Native: "system/away_summary, bridge_status, informational, model_refusal_fallback",
		Where: "a system record", Note: "anything the runtime says about the session itself"},

	// ---- relations ----
	model.Term{Unified: model.RelStarts, Native: "toolUseResult.agentId, or a run journal", Where: "a launch result"},
	model.Term{Unified: model.RelReports, Native: "<task-notification>", Where: "a user record's content"},
	model.Term{Unified: model.RelEndsWith, Note: "the last model response in a child stream"},
	model.Term{Unified: model.RelResultOf, Native: "tool_use_id", Where: "a tool_result block",
		Note: "also stated redundantly as sourceToolAssistantUUID"},
	model.Term{Unified: model.RelFollows, Native: "logicalParentUuid", Where: "a compact_boundary record"},
	model.Term{Unified: model.RelSummarizes, Native: "parentUuid", Where: "an isCompactSummary record"},
	model.Term{Unified: model.RelInSegment, Note: "an activity window chosen for commit"},
	model.Term{Unified: model.RelRetries, Note: "unavailable; retry identity is not recorded"},
	model.Term{Unified: model.RelCancels, Native: "toolUseResult.interrupted", Where: "a tool result"},
	model.Term{Unified: model.RelInputOf, Note: "unavailable; the serialized provider request is not written locally"},

	// ---- qualification; all of it is this project's judgement ----
	model.Term{Unified: model.ExactUnique, Note: "an identifier matched exactly one candidate"},
	model.Term{Unified: model.ExactAmbiguous, Note: "an identifier matched several; nothing chose one"},
	model.Term{Unified: model.StrongInference, Note: "no identifier, but a well-founded structural argument"},
	model.Term{Unified: model.WeakInference, Note: "plausible only"},
	model.Term{Unified: model.Unresolved, Note: "no usable evidence"},
	model.Term{Unified: model.Conflict, Note: "sources disagree"},

	model.Term{Unified: model.ContentAvailable, Note: "the bytes are in a landed record"},
	model.Term{Unified: model.ContentRedacted, Native: "redacted_thinking", Where: "a content block"},
	model.Term{Unified: model.ContentOmitted, Note: "deliberately not carried"},
	model.Term{Unified: model.ContentHashOnly, Note: "only a digest of the bytes was kept"},
	model.Term{Unified: model.ContentSizeOnly, Note: "only the length was kept"},
	model.Term{Unified: model.ContentTruncated, Note: "part of the bytes, with the full length reported"},
	model.Term{Unified: model.ContentUnavailable, Note: "the runtime never wrote it, or the source was pruned"},

	model.Term{Unified: model.TriggerExternal, Native: "origin.kind == \"human\"", Where: "record",
		Note: "also a run whose records state no origin at all - a locally typed command"},
	model.Term{Unified: model.TriggerNotification, Native: "origin.kind == \"task-notification\"", Where: "record"},
	model.Term{Unified: model.TriggerUnknown, Note: "no record of the run states a trigger"},
)

// Glossary returns what Claude Code calls the things the model names.
func Glossary() *model.Glossary { return glossary }

// RuntimeName is what the runtime this adapter reads is called. It is the
// service every record is attributed to when no service name is configured,
// so a receiver lists conversations by the agent that produced them.
const RuntimeName = "Claude Code"
