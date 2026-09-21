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

package langsmith

import "github.com/apache/skywalking-ai-sessionizer/pkg/model"

// glossary is what LangChain and its tracing client call the things the model
// names.
//
// It lives beside the code that reads them, so a rename on the wire is made in
// both places at once. An empty Native is not a gap: a Talk, a Segment and a
// correlation quality are things this project derived, and saying the runtime
// has no word for them is more useful than a plausible one that would send a
// reader looking for a field that is not there.
//
// Two words here mean something different from what a reader might expect, and
// both are worth the note. The client's "session" is the project, not a
// conversation. And its "run" is any step of the graph, so most runs are not
// what this model calls a Run.
var glossary = model.NewGlossary(Dialect,
	// ---- the roles a record plays ----
	model.Term{Unified: model.RoleID, Native: "id", Where: "a run",
		Note: "the record is the arrival, so a run that is posted and later patched has two"},
	model.Term{Unified: model.RoleParent, Native: "parent_run_id", Where: "a run",
		Note: "the child names the parent; dotted_order carries the whole ancestry"},
	model.Term{Unified: model.RoleCall, Native: "id", Where: "a run of type llm",
		Note: "shared by every arrival about that run"},
	model.Term{Unified: model.RoleRun, Native: "trace_id", Where: "a run",
		Note: "one invocation of the graph, which is one turn"},
	model.Term{Unified: model.RoleBatch, Note: "no equivalent: nothing here launches a group of agents at once"},
	model.Term{Unified: model.RoleContinues, Note: "no equivalent: the client has no context reset to resume from"},
	model.Term{Unified: model.RoleTool, Native: "tool_call_id", Where: "outputs.output of a run of type tool",
		Note: "absent when the tool raised; then it is read from the model call that asked for it"},
	model.Term{Unified: model.RoleChild, Note: "no equivalent: a nested agent is a run beneath a tool, named as nothing else"},
	model.Term{Unified: model.RoleTime, Native: "start_time and end_time", Where: "a run"},
	model.Term{Unified: model.RoleTrigger, Note: "no equivalent: derived from a trace's root run"},
	model.Term{Unified: model.RoleModel, Native: "extra.metadata.ls_model_name", Where: "a run of type llm"},
	model.Term{Unified: model.RoleStream, Native: "", Where: "",
		Note: "no equivalent: derived from whether a nested run's model calls continue the parent's history"},

	// ---- structure ----
	model.Term{Unified: model.KindConversation, Native: "extra.metadata.thread_id", Where: "a run",
		Note: "with session_id and conversation_id as the other spellings; supplied, never inferred"},
	model.Term{Unified: model.KindSegment, Note: "an activity window chosen for commit"},
	model.Term{Unified: model.KindSession, Native: "session_name", Where: "a run",
		Note: "the client's word for the PROJECT; a conversation is the thread key, and a session here is the two together"},
	model.Term{Unified: model.KindStream, Note: "no equivalent: a nested agent is a run like any other"},
	model.Term{Unified: model.KindEpoch, Note: "no equivalent: the client has no context reset"},
	model.Term{Unified: model.KindTalk, Note: "one readable interaction; one trace is one turn"},
	model.Term{Unified: model.KindRun, Native: "trace_id", Where: "a run",
		Note: "the client calls every graph step a run; this is the invocation they share"},

	// ---- steps ----
	model.Term{Unified: model.KindMessageExternal, Native: "inputs.messages", Where: "the root run of a trace",
		Note: "the message that opened the turn has no run of its own"},
	model.Term{Unified: model.KindMessageAssistant, Native: "outputs.generations[].message.kwargs.content",
		Where: "a run of type llm"},
	model.Term{Unified: model.KindMessageSynthetic, Note: "no equivalent: nothing here fabricates a message on the model's behalf"},
	model.Term{Unified: model.KindContextInjection, Note: "no equivalent"},
	model.Term{Unified: model.KindLLMCall, Native: "run_type: llm", Where: "a run"},
	model.Term{Unified: model.KindThinking, Note: "no equivalent yet: a provider's reasoning arrives inside the message content"},
	model.Term{Unified: model.KindTool, Native: "run_type: tool", Where: "a run"},
	model.Term{Unified: model.KindAgentCall, Native: "a tool call whose run has model calls beneath it",
		Note: "the wire has no word for delegation; it is read from the nested context"},
	model.Term{Unified: model.KindAgentLaunchAck, Note: "no equivalent: nothing acknowledges a delegation separately"},
	model.Term{Unified: model.KindAgentOutput, Native: "outputs.output.content", Where: "the delegating tool run"},
	model.Term{Unified: model.KindRuntimeNotification, Note: "no equivalent"},
	model.Term{Unified: model.KindEpochBoundary, Note: "no equivalent: the client has no context reset"},
	model.Term{Unified: model.KindEpochSummary, Note: "no equivalent"},
	model.Term{Unified: model.KindErrorAPI, Native: "error", Where: "a run",
		Note: "a string with the exception and its traceback"},
	model.Term{Unified: model.KindControlInterrupt, Note: "no equivalent"},
	model.Term{Unified: model.KindControlPermission, Note: "no equivalent: nothing here asks before running a tool"},
	model.Term{Unified: model.KindControlCommand, Note: "no equivalent"},
	model.Term{Unified: model.KindTurnDuration, Note: "no equivalent as a record; a run's own start and end give it"},

	// ---- relations ----
	model.Term{Unified: model.RelStarts, Note: "derived from a delegating tool and the runs beneath it"},
	model.Term{Unified: model.RelReports, Note: "derived"},
	model.Term{Unified: model.RelEndsWith, Note: "derived from a nested agent's last response"},
	model.Term{Unified: model.RelResultOf, Native: "tool_call_id", Where: "outputs.output of a run of type tool"},
	model.Term{Unified: model.RelFollows, Note: "derived from order within a trace"},
	model.Term{Unified: model.RelSummarizes, Note: "no equivalent"},
	model.Term{Unified: model.RelInSegment, Note: "derived"},
	model.Term{Unified: model.RelRetries, Note: "no equivalent yet: the client does not say a run retried another"},
	model.Term{Unified: model.RelCancels, Note: "no equivalent"},
	model.Term{Unified: model.RelInputOf, Native: "inputs", Where: "a run"},

	// ---- qualification; all derived, none of it on the wire ----
	model.Term{Unified: model.ExactUnique, Note: "one call id, named by the tool run that answered it"},
	model.Term{Unified: model.ExactAmbiguous, Note: "derived"},
	model.Term{Unified: model.StrongInference, Note: "derived: a failed tool joined to the one unanswered call of its name"},
	model.Term{Unified: model.WeakInference, Note: "derived"},
	model.Term{Unified: model.Unresolved, Note: "derived: a failed tool whose model call arrived in another request"},
	model.Term{Unified: model.Conflict, Note: "derived"},

	// ---- what a part holds ----
	model.Term{Unified: model.ContentAvailable, Note: "the client sends inputs and outputs whole, as their own parts"},
	model.Term{Unified: model.ContentRedacted, Note: "no equivalent: nothing on the wire says it redacted anything"},
	model.Term{Unified: model.ContentOmitted, Note: "a graph's own run: its repeated message list is not landed"},
	model.Term{Unified: model.ContentHashOnly, Note: "no equivalent"},
	model.Term{Unified: model.ContentSizeOnly, Note: "no equivalent"},
	model.Term{Unified: model.ContentTruncated, Note: "no equivalent: nothing measured arrived truncated"},
	model.Term{Unified: model.ContentUnavailable, Note: "a run that arrived unfinished: its content is not there yet"},

	// ---- what began a turn ----
	model.Term{Unified: model.TriggerExternal, Note: "the root run of a trace carries the message that opened it"},
	model.Term{Unified: model.TriggerNotification, Note: "no equivalent"},
	model.Term{Unified: model.TriggerUnknown, Note: "derived"},
)

// Glossary is what this runtime calls the things the model names.
func Glossary() *model.Glossary { return glossary }
