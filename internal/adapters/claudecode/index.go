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

import (
	"encoding/json"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/internal/index"
)

// indexRecord is the subset of a Claude Code record the index needs.
//
// Only identifiers and shape are decoded. Message text, tool inputs and results
// are deliberately left in the payload: structure resolution needs ids, not
// content, and decoding content would cost far more than it buys.
type indexRecord struct {
	Type       string `json:"type"`
	UUID       string `json:"uuid"`
	ParentUUID string `json:"parentUuid"`
	Timestamp  string `json:"timestamp"`
	PromptID   string `json:"promptId"`
	AgentID    string `json:"agentId"`
	Subtype    string `json:"subtype"`

	// LogicalParentUUID appears on context-reset boundaries and on no other
	// record type. It is the explicit pointer to the last message before the
	// reset, which is what makes an epoch observed rather than guessed. A
	// boundary's own parentUuid is null on every boundary measured, so this is
	// the only link back.
	LogicalParentUUID string `json:"logicalParentUuid"`
	IsCompactSummary  bool   `json:"isCompactSummary"`
	IsMeta            bool   `json:"isMeta"`

	// AITitle, Description and WorkflowName are the three places a name appears:
	// on a title record, on a child's sidecar, and on a workflow manifest.
	AITitle      string `json:"aiTitle"`
	Description  string `json:"description"`
	WorkflowName string `json:"workflowName"`
	RunID        string `json:"runId"`
	// Result is present on a run journal's result records, and holds what a
	// child returned. It is NOT in the child's own transcript.
	Result json.RawMessage `json:"result"`

	// ParentAgentID appears on a nested child's sidecar. Without it a child of a
	// child is attributed to the session's main stream, which flattens the whole
	// nesting.
	ParentAgentID string `json:"parentAgentId"`

	Origin origin `json:"origin"`

	Message struct {
		ID         string          `json:"id"`
		Model      string          `json:"model"`
		StopReason *string         `json:"stop_reason"`
		Content    json.RawMessage `json:"content"`
		Usage      *struct {
			Input      int `json:"input_tokens"`
			Output     int `json:"output_tokens"`
			CacheRead  int `json:"cache_read_input_tokens"`
			CacheWrite int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`

	// ToolUseResult must stay raw. It is an OBJECT most of the time and a bare
	// string the rest of the time, so decoding it into a struct fails on those
	// records - and a failed decode of the whole record loses every identifier
	// on it, not just this field.
	ToolUseResult json.RawMessage `json:"toolUseResult"`

	Attachment struct {
		Type        string `json:"type"`
		CommandMode string `json:"commandMode"`
		Origin      origin `json:"origin"`
	} `json:"attachment"`
}

// origin says what caused a prompt cycle. It is carried on the record itself and
// also, for a large share of cases, on the attachment inside it.
type origin struct {
	Kind string `json:"kind"`
}

// toolResult is the object form of a tool result's runtime enrichment.
//
// raw keeps the whole object, because the readable half is stdout and stderr
// and the rest is a per-tool schema - 49 distinct key sets across the corpus -
// that normalisation cannot flatten without losing it.
type toolResult struct {
	raw           json.RawMessage
	AgentID       string `json:"agentId"`
	ToolUseID     string `json:"toolUseId"`
	Status        string `json:"status"`
	RunID         string `json:"runId"`
	TranscriptDir string `json:"transcriptDir"`
	TaskID        string `json:"taskId"`
}

// decodeToolResult reads the enrichment only when it is an object.
func decodeToolResult(raw json.RawMessage) (toolResult, bool) {
	var t toolResult
	if len(raw) == 0 || raw[0] != '{' {
		return t, false
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		return t, false
	}
	t.raw = raw
	return t, true
}

// Launch statuses a tool result can report.
const (
	statusAsyncLaunched = "async_launched" // a child started; this is NOT its result
	statusForked        = "forked"         // a skill fork; the child has no sidecar pointer
	statusCompleted     = "completed"      // a synchronous return; this IS the result
)

// syntheticModel is the model name on an assistant-role record the CLIENT
// produced - a connection loss, a provider error, a quota or authentication
// failure - rather than one a provider returned.
//
// The test is the model name, not a missing request id: about a quarter of these
// records carry a request id, and a genuine call exists that carries none, so
// request id misclassifies in both directions.
const syntheticModel = "<synthetic>"

// triggerOf reads what caused this record's prompt cycle.
//
// The marker sits on the record and, for a substantial share, on the attachment
// inside it instead. Reading only the record misses more than a quarter of them.
// The value is an open set - a third kind exists beyond the two obvious ones -
// so anything unrecognised maps to "other" rather than being treated as absent.
func triggerOf(d *indexRecord) index.Trigger {
	kind := d.Origin.Kind
	if kind == "" {
		kind = d.Attachment.Origin.Kind
	}
	switch kind {
	case "":
		return index.TriggerNone
	case "human":
		return index.TriggerExternal
	case "task-notification":
		return index.TriggerNotification
	}
	return index.TriggerOther
}

// flagsOf reduces the record's shape to the bits a lookup needs.
func flagsOf(d *indexRecord, tur *toolResult, hasTUR bool, src Source) index.Flags {
	var f index.Flags
	if d.Type == "assistant" {
		if d.Message.Model == syntheticModel {
			f |= index.FlagSynthetic
		}
		// A stop reason says the provider finished, which is what makes the usage
		// numbers on this call real. It does NOT identify the last fragment: a
		// main transcript stamps the same stop reason on every fragment of a call.
		// Only line order finds the last one.
		if d.Message.StopReason != nil && *d.Message.StopReason != "" {
			f |= index.FlagStopReason
		}
	}
	if d.Type == "system" {
		// Every subtype is accounted for. An unrecognised one becomes a notice
		// rather than nothing, because a system record always says something
		// about the session and dropping it loses that silently.
		switch d.Subtype {
		case "compact_boundary":
			f |= index.FlagEpochBoundary
		case "api_error":
			f |= index.FlagError
		case "turn_duration":
			f |= index.FlagTurnDuration
		case "local_command":
			f |= index.FlagCommand
		default:
			f |= index.FlagNotice
		}
	}
	if d.IsCompactSummary {
		f |= index.FlagEpochSummary
	}
	if src.Kind == SrcJournal && len(d.Result) > 0 {
		// A run journal records what each child returned. Measured across the
		// corpus, 2,353 of 2,436 of these appear NOWHERE else - not in the
		// child's own transcript - so this is the only copy.
		f |= index.FlagChildResult
	}
	if hasTUR {
		switch tur.Status {
		case statusAsyncLaunched:
			// The acknowledgement is not the result. Counting it as one records an
			// empty output for every asynchronous delegation.
			f |= index.FlagLaunchAck
		case statusCompleted:
			// A synchronous return. This one IS the child's result, and it repeats
			// the child's own output, so a renderer has to drop the copy rather
			// than show the same work twice.
			f |= index.FlagChildResult
		}
	}

	// An attachment is material the harness put into model context. One kind is
	// not: a queued command the person typed, which exists ONLY as an attachment
	// and is otherwise lost.
	//
	// The command mode is what separates them, and both halves of it are needed.
	// Matching the attachment type alone reads completion-notification blobs as
	// things the user typed, and in some sessions there are more of those than
	// genuine prompts.
	//
	// Everything else is injected. That is the DEFAULT rather than a list,
	// because a list has to be right about types that do not exist yet. An
	// earlier version of this code kept one, and it was wrong in both
	// directions at once: 635 records across 13 real attachment types fell
	// through it and were dropped from the conversation entirely, while 5 types
	// on the list appear nowhere in the corpus. Defaulting to injection cannot
	// lose a record; at worst it over-reports one, and a reader is told what it
	// was either way.
	if d.Type == "attachment" {
		if d.Attachment.Type == "queued_command" && d.Attachment.CommandMode == "prompt" {
			f |= index.FlagExternalInput
		} else {
			f |= index.FlagInjection
		}
	}
	if d.Type == "user" && d.Origin.Kind == "human" && !d.IsMeta {
		f |= index.FlagExternalInput
	}
	if d.IsMeta {
		// A meta record is text the harness wrote, placed in the user role. It is
		// injected context, not something a person said.
		f |= index.FlagInjection
		f &^= index.FlagExternalInput
	}
	if src.Kind == SrcAgentMeta || src.Kind == SrcJournal {
		f &^= index.FlagExternalInput
	}
	return f
}

// linksOf extracts the two cross-record links that are not content blocks.
//
// Several mechanisms start a child agent and no single field covers them, so
// each is read where it lives:
//
//	Workflow    the launch result names a run; the run's journal names every
//	            child it starts. This is by far the largest source - most
//	            children on disk are reached this way and no other.
//	Agent tool  the parent's result names the child. The status says whether it
//	            is an acknowledgement or a completed return.
//	Skill fork  only the parent's result names the child.
//	Nesting     a child's sidecar names its parent agent. Without it a child of
//	            a child is attributed to the main stream.
//
// Completion is a further path: a notification names the call it completes and
// the task that finished, and it can land in a different file from that call,
// so both ids are extracted here and resolved across the whole session.
//
// It returns strings; the caller interns them, so this stays a statement about
// Claude Code alone.
func linksOf(d *indexRecord, tur *toolResult, src Source) (anchor, spawn string) {
	switch src.Kind {
	case SrcAgentMeta:
		// The sidecar is not a general parent pointer: it names a parent only for
		// a nested child. For everything else it holds only the agent's type and
		// depth, and the join has to come from the parent side or the journal.
		spawn = d.AgentID
		if spawn == "" {
			spawn = src.Stream
		}
		anchor = tur.ToolUseID
	case SrcJournal:
		// A journal record announces a child before its transcript exists, so an
		// unresolved reference here is expected for a while rather than an error.
		spawn = d.AgentID
	default:
		anchor, spawn = tur.ToolUseID, tur.AgentID
	}

	if a, s := notificationIDs(d.Message.Content); a != "" || s != "" {
		if anchor == "" {
			anchor = a
		}
		if spawn == "" {
			spawn = s
		}
	}
	return anchor, spawn
}

// ParentAgent returns the agent a nested child's sidecar names as its parent.
func ParentAgent(payload []byte) string {
	var d indexRecord
	if err := json.Unmarshal(payload, &d); err != nil {
		return ""
	}
	return d.ParentAgentID
}

// notificationIDs pulls the call id and the task id out of a completion
// notification.
//
// The call id is the primary key. The task id is only sometimes an agent id -
// it is a generic handle whose meaning depends on what was launched, so joining
// on it unconditionally matches the wrong thing most of the time. It is returned
// only when it has an agent id's shape.
//
// The status element is deliberately not read: it is missing entirely on a large
// class of notifications, so anything requiring it would fail on them.
func notificationIDs(content json.RawMessage) (anchor, spawn string) {
	if len(content) == 0 || content[0] != '"' {
		return "", "" // structured content carries no notification
	}
	var text string
	if err := json.Unmarshal(content, &text); err != nil {
		return "", ""
	}
	if !strings.Contains(text, "<task-notification>") {
		return "", ""
	}
	anchor = between(text, "<tool-use-id>", "</tool-use-id>")
	// A single notification can name several tasks. Only one that looks like an
	// agent identifier is usable as a stream name.
	rest := text
	for {
		id := between(rest, "<task-id>", "</task-id>")
		if id == "" {
			break
		}
		if IsAgentID(id) {
			spawn = id
			break
		}
		i := strings.Index(rest, "</task-id>")
		if i < 0 {
			break
		}
		rest = rest[i+len("</task-id>"):]
	}
	return anchor, spawn
}

func between(s, openTag, closeTag string) string {
	i := strings.Index(s, openTag)
	if i < 0 {
		return ""
	}
	rest := s[i+len(openTag):]
	j := strings.Index(rest, closeTag)
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:j])
}
