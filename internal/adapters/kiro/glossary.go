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

package kiro

import "github.com/apache/skywalking-ai-sessionizer/pkg/model"

// glossary is what Kiro's V3 engine calls the things the model names.
//
// It lives beside the code that reads them, so a rename in the store is made in
// both places at once. A test checks that the model's whole vocabulary is
// accounted for. An empty Native says the engine has no word for something,
// which is more useful than a plausible one that sends a reader looking for a
// field that is not there.
//
// Two words here mean something different from what a reader might expect.
// Kiro's "execution" is one turn, which is a run here. And Kiro writes a
// subagent's records in a file of its own, a "sub-execution", which is a
// child stream here.
var glossary = model.NewGlossary(Dialect,
	// ---- the roles a record plays ----
	model.Term{Unified: model.RoleID, Native: "id", Where: "every record",
		Note: "followed by # and the first 12 hex digits of the record's SHA-256; the engine gives one id to an approval it asks twice"},
	model.Term{Unified: model.RoleParent, Note: "no equivalent: a record names no record before it"},
	model.Term{Unified: model.RoleCall, Note: "no equivalent: a turn lists its request ids in usage_summary, and no record names one"},
	model.Term{Unified: model.RoleBatch, Note: "no equivalent: subagents started together share only their turn"},
	model.Term{Unified: model.RoleContinues, Note: "no equivalent: a tombstone's effectiveFromMessageId names where the dropped records begin, not the last one kept"},
	model.Term{Unified: model.RoleChild, Native: "subSessionId", Where: "sub_agent_start and sub_agent_complete",
		Note: "also the name of the child's file under sub-executions"},
	model.Term{Unified: model.RoleTime, Native: "timestamp", Where: "every record",
		Note: "not in line order; line order is the order"},
	model.Term{Unified: model.RoleModel, Note: "not read: no record says which model answered; session.json names the model chosen"},
	model.Term{Unified: model.RoleTrigger, Note: "derived: a user record that is neither a steering note nor written by the engine"},

	// ---- structure ----
	model.Term{Unified: model.KindConversation, Native: "id", Where: "session.json",
		Note: "one session is one conversation; Kiro Crew names no chain between the sessions of one chat"},
	model.Term{Unified: model.KindSegment, Note: "an activity window"},
	model.Term{Unified: model.KindSession, Native: "session", Where: "sessions/<workspace key>/sess_<id>"},
	model.Term{Unified: model.KindStream, Native: "sub-execution", Where: "sub-executions/<subSessionId>.jsonl",
		Note: "the main stream is messages.jsonl"},
	model.Term{Unified: model.KindEpoch, Note: "the span between two tombstones"},
	model.Term{Unified: model.KindTalk, Note: "one readable interaction: a prompt and the turn it opened"},
	model.Term{Unified: model.KindRun, Native: "executionId", Where: "turn_start and the records of the turn",
		Note: "a prompt carries none; it belongs to the first turn named after it in the file"},

	// ---- steps ----
	model.Term{Unified: model.KindMessageExternal, Native: "user", Where: "messages.jsonl",
		Note: "also a message sent during a turn, source steer; Kiro Crew writes its own prompts here too"},
	model.Term{Unified: model.KindMessageAssistant, Native: "assistant, operationType Say", Where: "messages.jsonl"},
	model.Term{Unified: model.KindMessageSynthetic, Note: "not read: records the engine writes on recovery end their id in -synthetic, are a tool result, a child's end or a turn's end, and none was measured"},
	model.Term{Unified: model.KindContextInjection, Native: "agent_note, session_start, steering_inclusion",
		Where: "messages.jsonl",
		Note:  "and a user record with syntheticUserMessageReason, the engine's text around a steering message"},
	model.Term{Unified: model.KindLLMCall, Note: "no equivalent: the engine records what the model said and not which request said it"},
	model.Term{Unified: model.KindThinking, Native: "assistant, operationType Reasoning", Where: "messages.jsonl",
		Note: "the text ... means the engine kept no reasoning"},
	model.Term{Unified: model.KindTool, Native: "tool_call and tool_result", Where: "messages.jsonl and sub-executions",
		Note: "joined by toolCallId"},
	model.Term{Unified: model.KindAgentCall, Native: "sub_agent_start", Where: "messages.jsonl",
		Note: "no tool call names the spawn; the record is the call"},
	model.Term{Unified: model.KindAgentLaunchAck, Note: "no equivalent: nothing acknowledges a subagent separately"},
	model.Term{Unified: model.KindAgentOutput, Native: "sub_agent_complete", Where: "messages.jsonl",
		Note: "the response the parent received; the child's own last record is in its file"},
	model.Term{Unified: model.KindRuntimeNotification, Note: "no equivalent: a subagent's end is the result of its call"},
	model.Term{Unified: model.KindEpochBoundary, Native: "tombstone", Where: "messages.jsonl",
		Note: "kind summarization or checkpoint_revert"},
	model.Term{Unified: model.KindEpochSummary, Native: "assistant, operationType Summary", Where: "messages.jsonl",
		Note: "nothing names the tombstone it follows"},
	model.Term{Unified: model.KindErrorAPI, Native: "session_metadata, key displayError", Where: "messages.jsonl"},
	model.Term{Unified: model.KindControlInterrupt, Note: "not read: a turn the person cancelled ends with stopReason cancelled, which lands as a notice"},
	model.Term{Unified: model.KindControlPermission, Note: "not read: pending_interaction and interaction_resolved are landed and are no step"},
	model.Term{Unified: model.KindControlCommand, Note: "no equivalent: a slash command leaves no record of its own"},
	model.Term{Unified: model.KindTurnDuration, Native: "usage_summary", Where: "messages.jsonl",
		Note: "elapsedTime, with the turn's credits; no token counts"},
	model.Term{Unified: model.KindControlNotice, Native: "turn_end, ContextualHookInvoked", Where: "messages.jsonl",
		Note: "a turn_end whose stopReason is not end_turn; also a record with no type, or a type this adapter does not describe"},

	// ---- relations ----
	model.Term{Unified: model.RelStarts, Native: "subSessionId", Where: "sub_agent_start"},
	model.Term{Unified: model.RelReports, Note: "no equivalent"},
	model.Term{Unified: model.RelEndsWith, Note: "derived from a child's last response"},
	model.Term{Unified: model.RelResultOf, Native: "toolCallId", Where: "tool_result",
		Note: "subSessionId for a subagent's end"},
	model.Term{Unified: model.RelFollows, Note: "derived from line order"},
	model.Term{Unified: model.RelSummarizes, Note: "not read: nothing joins a summary to its tombstone"},
	model.Term{Unified: model.RelInSegment, Note: "derived"},
	model.Term{Unified: model.RelRetries, Note: "no equivalent"},
	model.Term{Unified: model.RelCancels, Note: "no equivalent"},
	model.Term{Unified: model.RelInputOf, Note: "no equivalent"},

	// ---- qualification; all derived, none of it in the store ----
	model.Term{Unified: model.ExactUnique, Note: "one toolCallId or subSessionId on both records"},
	model.Term{Unified: model.ExactAmbiguous, Note: "derived"},
	model.Term{Unified: model.StrongInference, Note: "derived"},
	model.Term{Unified: model.WeakInference, Note: "derived"},
	model.Term{Unified: model.Unresolved, Note: "derived"},
	model.Term{Unified: model.Conflict, Note: "derived"},

	// ---- what a part holds ----
	model.Term{Unified: model.ContentAvailable, Note: "the engine keeps text and tool results whole, apart from large tool output"},
	model.Term{Unified: model.ContentRedacted, Native: "reasoningRedactedContent", Where: "a Reasoning record"},
	model.Term{Unified: model.ContentOmitted, Note: "no equivalent"},
	model.Term{Unified: model.ContentHashOnly, Note: "no equivalent"},
	model.Term{Unified: model.ContentSizeOnly, Note: "no equivalent"},
	model.Term{Unified: model.ContentTruncated, Note: "not read: the model is sent a preview of a large tool output, and that preview is the result here; the whole output is in tool-outputs, which is not collected"},
	model.Term{Unified: model.ContentTooLarge, Note: "no equivalent: asz omits a payload whose record alone is larger than a landed file's byte budget"},
	model.Term{Unified: model.ContentRemoved, Note: "no equivalent: asz removes the payloads of a conversation idle longer than storage.retention from its own root"},
	model.Term{Unified: model.ContentUnavailable, Native: "...", Where: "the content of a Reasoning record"},

	// ---- what began a turn ----
	model.Term{Unified: model.TriggerExternal, Native: "user", Where: "messages.jsonl"},
	model.Term{Unified: model.TriggerNotification, Note: "no equivalent"},
	model.Term{Unified: model.TriggerUnknown, Note: "derived"},
)

// Glossary is what this runtime calls the things the model names.
func Glossary() *model.Glossary { return glossary }
