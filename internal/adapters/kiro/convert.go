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

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// envelope is what every line of a V3 record file holds.
type envelope struct {
	ID        string          `json:"id"`
	Timestamp string          `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

// payload is the part of a record the dialect reads. Fields of different
// record types share it, because the engine never gives one name two meanings
// that matter here.
type payload struct {
	Type string `json:"type"`

	ExecutionID       string `json:"executionId"`
	ParentExecutionID string `json:"parentExecutionId"`
	Context           struct {
		ExecutionID string `json:"executionId"`
	} `json:"context"`

	Content   json.RawMessage   `json:"content"`
	Images    []json.RawMessage `json:"images"`
	Documents json.RawMessage   `json:"documents"`
	Meta      struct {
		Kiro struct {
			SteeringClearedIDs json.RawMessage `json:"steeringClearedIds"`
			SyntheticReason    json.RawMessage `json:"syntheticUserMessageReason"`
			ServerName         string          `json:"serverName"`
		} `json:"kiro"`
	} `json:"_meta"`

	OperationType      string          `json:"operationType"`
	ReasoningSignature string          `json:"reasoningSignature"`
	ReasoningRedacted  string          `json:"reasoningRedactedContent"`
	SummaryReasoning   json.RawMessage `json:"summaryReasoning"`

	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Args       json.RawMessage `json:"args"`
	Success    *bool           `json:"success"`

	StopReason  string          `json:"stopReason"`
	Key         string          `json:"key"`
	Status      string          `json:"status"`
	ElapsedTime json.RawMessage `json:"elapsedTime"`

	SubSessionID string          `json:"subSessionId"`
	SubAgentName string          `json:"subAgentName"`
	Response     json.RawMessage `json:"response"`
}

// reasoningPlaceholder is what the engine writes as the text of a reasoning
// record whose reasoning it did not keep. The engine itself reads this value,
// and an empty one, as no reasoning when it replays a session (engine
// @kiro/agent 0.66.26), so it is no text here either.
const reasoningPlaceholder = "..."

// Convert turns one Kiro source record into a Session Data record.
//
// This is the only function that reads Kiro's shape. Everything above it sees
// parts and role names. run is the turn the collector found for a record that
// names none of its own; see turnFor.
//
// Two choices are forced by the store and are worth knowing:
//
//   - The record id is the engine's id, then "#", then the record's digest.
//     The engine can give one id to two different records: when it asks
//     twice to approve one tool call, both requests carry one id and both
//     answers another. Measured over 56 sessions and 3,101 records, 46 ids
//     did so, for 23 approvals, and no other id was reused. Assembly
//     removes a repeated id as a replayed copy, so the engine's id alone
//     would drop the second record. The digest keeps the id stable for the
//     same bytes, which is what lets a record landed twice after a crash
//     still be removed as the copy it is.
//   - Nothing is grouped into a provider call. The engine lists the request
//     ids of a turn and ties no record to one, so a record names no call and
//     assembly draws none.
func Convert(src Source, ord, off uint64, raw []byte, run string) *sessiondata.Record {
	sum := sha256.Sum256(raw)
	rec := &sessiondata.Record{
		Ord: ord, Off: off,
		Sha:   hex.EncodeToString(sum[:])[:12],
		Bytes: len(raw),
	}
	rec.From = sessiondata.FromRuntime
	if src.Kind == SrcHeader {
		return convertHeader(rec, raw)
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// A record that will not parse is still evidence, and it is kept
		// whole. It could be the system prompt, so it is named for one: a
		// reader withholding the prompt is better kept from it than shown it.
		rec.Parts = []sessiondata.Part{rawPart(raw, "the record is not valid JSON")}
		rec.Flags = []string{sessiondata.FlagSystemPrompt}
		return rec
	}
	rec.ID = env.ID + "#" + rec.Sha
	rec.Time = env.Timestamp
	var p payload
	if len(env.Payload) == 0 || json.Unmarshal(env.Payload, &p) != nil || p.Type == "" {
		rec.Flags = []string{"notice"}
		rec.Parts = []sessiondata.Part{rawPart(raw, "the record has no payload this dialect can read")}
		return rec
	}
	rec.Run = turnOf(&p)
	if rec.Run == "" {
		rec.Run = run
	}

	switch p.Type {
	case "user":
		convertUser(rec, &p, env.Payload)
	case "agent_note":
		// What the engine tells the model about the session, such as which
		// model is current. It is written just before a prompt.
		rec.Flags = []string{"injected"}
		rec.Parts = []sessiondata.Part{textOf(p.Content)}
	case "session_start":
		// The system prompt the engine composed for the agent. It is written
		// when the first turn ends, which is why it names no turn.
		rec.Flags = []string{"injected", sessiondata.FlagSystemPrompt}
		rec.Parts = []sessiondata.Part{textOf(p.Content)}
	case "steering_inclusion":
		// The steering documents the engine put into context for this turn.
		rec.Flags = []string{"injected"}
		rec.Parts = []sessiondata.Part{dataPart(env.Payload)}
	case "assistant":
		convertAssistant(rec, &p)
	case "tool_call":
		rec.From = sessiondata.FromAgent
		part := sessiondata.Part{Kind: sessiondata.PartCall, ID: p.ToolCallID, Name: p.ToolName,
			Data: p.Args, State: model.ContentAvailable, Bytes: len(p.Args)}
		part.Server, part.ServerTool = mcpName(p.ToolName, p.Meta.Kiro.ServerName)
		rec.Parts = []sessiondata.Part{part}
	case "tool_result":
		rec.From = sessiondata.FromExternal
		part := sessiondata.Part{Kind: sessiondata.PartResult, Of: p.ToolCallID, State: model.ContentAvailable}
		setResult(&part, p.Content)
		if p.Success != nil {
			failed := !*p.Success
			part.Failed = &failed
		}
		rec.Parts = []sessiondata.Part{part}
	case "sub_agent_start":
		if p.SubSessionID == "" {
			unknown(rec, p.Type, env.Payload)
			break
		}
		// The engine records the spawn here and nowhere else: no tool call
		// names it. The child's id is the only key the parent and the
		// child's own file share, so it is the call's id, and the record is
		// the call.
		rec.From = sessiondata.FromAgent
		rec.Tool, rec.Child = p.SubSessionID, p.SubSessionID
		rec.Parts = []sessiondata.Part{{Kind: sessiondata.PartCall, ID: p.SubSessionID, Name: p.SubAgentName,
			Data: env.Payload, State: model.ContentAvailable, Bytes: len(env.Payload)}}
	case "sub_agent_complete":
		if p.SubSessionID == "" {
			unknown(rec, p.Type, env.Payload)
			break
		}
		// The parent's copy of what the child returned, as the result of the
		// call that started it.
		rec.From = sessiondata.FromExternal
		rec.Child = p.SubSessionID
		rec.Flags = []string{"child_result"}
		part := sessiondata.Part{Kind: sessiondata.PartResult, Of: p.SubSessionID, State: model.ContentAvailable}
		setResult(&part, p.Response)
		if p.Status != "" {
			failed := p.Status != "success"
			part.Failed = &failed
		}
		rec.Parts = []sessiondata.Part{part}
	case "tombstone":
		// A tombstone takes the records before it out of the context the
		// model is sent. Its kind says why: a summary, or a checkpoint the
		// person went back to. Either way the context was reset in place.
		// Its effectiveFromMessageId names where the dropped records begin,
		// not the last one before the reset, so it is kept in the data and
		// not read as a continuation.
		rec.Flags = []string{"context_reset"}
		rec.Parts = []sessiondata.Part{dataPart(env.Payload)}
	case "turn_end":
		// A turn that ended any other way than the model finishing is
		// something the requester saw: a turn they cancelled, or one an
		// error stopped.
		if p.StopReason != "end_turn" {
			rec.Flags = []string{"notice"}
		}
		rec.Parts = []sessiondata.Part{dataPart(env.Payload)}
	case "usage_summary":
		// The engine's own measure of the turn, with its credits and the
		// request ids it made. It holds no token counts. elapsedTime is the
		// turn's length in milliseconds, from its start to its end, and is
		// given first under durationMs, the name a turn's measurement is
		// read by.
		rec.Flags = []string{"turn_duration"}
		rec.Parts = []sessiondata.Part{dataPart(env.Payload)}
		var ms json.Number
		if json.Unmarshal(p.ElapsedTime, &ms) == nil && ms != "" && p.ElapsedTime[0] != '"' {
			d, _ := json.Marshal(map[string]json.Number{"durationMs": ms})
			rec.Parts = append([]sessiondata.Part{dataPart(d)}, rec.Parts...)
		}
	case "session_metadata":
		// The error the engine showed the requester. The other key measured,
		// contextUsage, is a percentage of the context window and no step.
		if p.Key == "displayError" {
			rec.Flags = []string{"error"}
		}
		rec.Parts = []sessiondata.Part{dataPart(env.Payload)}
	case "ContextualHookInvoked":
		rec.Flags = []string{"notice"}
		rec.Parts = []sessiondata.Part{dataPart(env.Payload)}
	case "turn_start", "session_event", "pending_interaction", "interaction_resolved":
		// The engine's own bookkeeping: a turn starting, the session pausing
		// after it, and an approval asked and answered for a tool. Each is
		// kept, and none is a step the model has a kind for.
		rec.Parts = []sessiondata.Part{dataPart(env.Payload)}
	default:
		unknown(rec, p.Type, env.Payload)
	}
	return rec
}

// convertUser reads a record in the user role. Three different things are
// written there, and the fields say which.
func convertUser(rec *sessiondata.Record, p *payload, raw json.RawMessage) {
	switch {
	case p.Meta.Kiro.SteeringClearedIDs != nil:
		// The engine noting that the messages a person sent during a turn
		// were consumed. It has no content of its own.
		rec.Parts = []sessiondata.Part{dataPart(raw)}
	case p.Meta.Kiro.SyntheticReason != nil:
		// The text the engine wrote to carry such a message to the model.
		// The person's own words are the steering record before it.
		rec.From = sessiondata.FromExternal
		rec.Flags = []string{"injected"}
		rec.Parts = userParts(p)
	default:
		// A prompt, or a message sent while a turn ran. Kiro Crew writes its
		// own prompts here too, which is input from outside the agent all
		// the same.
		rec.From = sessiondata.FromExternal
		rec.Trigger = model.TriggerExternal
		rec.Flags = []string{"external_input"}
		rec.Parts = userParts(p)
	}
}

// userParts is a user record's text, then its images, then its documents.
func userParts(p *payload) []sessiondata.Part {
	parts := []sessiondata.Part{textOf(p.Content)}
	for _, raw := range p.Images {
		var img struct {
			Data     json.RawMessage `json:"data"`
			MimeType string          `json:"mimeType"`
		}
		var data string
		if json.Unmarshal(raw, &img) != nil || json.Unmarshal(img.Data, &data) != nil {
			parts = append(parts, rawPart(raw, "the image is not one this dialect describes"))
			continue
		}
		parts = append(parts, sessiondata.Part{Kind: sessiondata.PartMedia, Media: img.MimeType,
			Data: img.Data, State: model.ContentAvailable, Bytes: len(data)})
	}
	// Every measured record has an empty list. A document that does arrive
	// travels as it came.
	if len(p.Documents) > 0 && string(p.Documents) != "[]" && string(p.Documents) != "null" {
		parts = append(parts, dataPart(p.Documents))
	}
	return parts
}

// convertAssistant reads a record the agent produced.
func convertAssistant(rec *sessiondata.Record, p *payload) {
	switch p.OperationType {
	case "Summary":
		// The summary a compaction carried forward. The engine sends it to
		// the model as a human message, so it is input here, not output.
		rec.From = sessiondata.FromExternal
		rec.Flags = []string{"reset_summary"}
		rec.Parts = []sessiondata.Part{textOf(p.Content)}
		if len(p.SummaryReasoning) > 0 {
			rec.Parts = append(rec.Parts, dataPart(p.SummaryReasoning))
		}
	case "Reasoning":
		rec.From = sessiondata.FromAgent
		rec.Parts, rec.Dropped = reasoningOf(p)
	default:
		// Say, and Print, which the engine sends back to the model as the
		// agent's own text. So does any other operation it does not name.
		rec.From = sessiondata.FromAgent
		rec.Parts = []sessiondata.Part{textOf(p.Content)}
	}
}

// reasoningOf is a reasoning record's thought, and what it holds that no
// reader can use.
func reasoningOf(p *payload) ([]sessiondata.Part, []sessiondata.Drop) {
	if len(p.Content) > 0 && p.Content[0] != '"' && string(p.Content) != "null" {
		return []sessiondata.Part{rawPart(p.Content, "the reasoning is not text")}, nil
	}
	var text string
	_ = json.Unmarshal(p.Content, &text)
	state := model.ContentAvailable
	if text == "" || text == reasoningPlaceholder {
		text, state = "", model.ContentUnavailable
	}
	if p.ReasoningRedacted != "" {
		state = model.ContentRedacted
	}
	part := sessiondata.Part{Kind: sessiondata.PartReasoning, Text: text, State: state, Bytes: len(text)}
	var dropped []sessiondata.Drop
	if p.ReasoningSignature != "" {
		dropped = append(dropped, sessiondata.Drop{What: "reasoning signature", Bytes: len(p.ReasoningSignature),
			Why: "a provider verifies it; a reader cannot read it"})
	}
	if p.ReasoningRedacted != "" {
		dropped = append(dropped, sessiondata.Drop{What: "redacted reasoning", Bytes: len(p.ReasoningRedacted),
			Why: "the provider redacted it; a reader cannot read it"})
	}
	return []sessiondata.Part{part}, dropped
}

// convertHeader reads session.json: the session's title, and the whole header
// as data.
func convertHeader(rec *sessiondata.Record, raw []byte) *sessiondata.Record {
	var h header
	if err := json.Unmarshal(raw, &h); err != nil {
		rec.Parts = []sessiondata.Part{rawPart(raw, "the header is not valid JSON")}
		return rec
	}
	rec.Label = h.Title
	rec.Parts = []sessiondata.Part{dataPart(raw)}
	return rec
}

// turnOf is the turn a record names itself: the turn it belongs to, the turn
// that started a subagent, or the turn a session event reports on.
func turnOf(p *payload) string {
	switch {
	case p.ExecutionID != "":
		return p.ExecutionID
	case p.ParentExecutionID != "":
		return p.ParentExecutionID
	}
	return p.Context.ExecutionID
}

// wantsTurn reports whether a record belongs to a turn it does not name. A
// person's prompt, the note the engine writes before it, and a message sent
// during a turn carry no turn id in any measured session. The note that
// steering messages were consumed belongs to none.
func wantsTurn(p *payload) bool {
	if turnOf(p) != "" {
		return false
	}
	switch p.Type {
	case "user":
		return p.Meta.Kiro.SteeringClearedIDs == nil
	case "agent_note":
		return true
	}
	return false
}

// unknown keeps a record this dialect does not describe, whole, and says the
// runtime reported something. A later version of the dialect can read it
// without the source.
func unknown(rec *sessiondata.Record, typ string, raw json.RawMessage) {
	rec.From = sessiondata.FromRuntime
	rec.Flags = []string{"notice"}
	rec.Parts = []sessiondata.Part{rawPart(raw, "record type \""+typ+"\" is not one this dialect describes")}
}

// mcpName splits a tool's name into the MCP server and the server's tool. The
// engine names such a tool "mcp_" + the server's name with "-" written "_" +
// "_" + the tool's own name, and records the server's name beside it. Both
// are set only where the name splits exactly that way.
func mcpName(tool, server string) (string, string) {
	if server == "" {
		return "", ""
	}
	rest, ok := strings.CutPrefix(tool, "mcp_"+strings.ReplaceAll(server, "-", "_")+"_")
	if !ok || rest == "" {
		return "", ""
	}
	return server, rest
}

// textOf is a text part from a field that should hold a string. Anything else
// keeps its bytes.
func textOf(raw json.RawMessage) sessiondata.Part {
	if len(raw) == 0 || string(raw) == "null" {
		return sessiondata.Part{Kind: sessiondata.PartText, State: model.ContentAvailable}
	}
	var s string
	if raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return rawPart(raw, "the content is not text")
	}
	return sessiondata.Part{Kind: sessiondata.PartText, Text: s, State: model.ContentAvailable, Bytes: len(s)}
}

// setResult puts a result's content on its part: as text when it is a string,
// and as it came otherwise.
func setResult(part *sessiondata.Part, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var s string
	if raw[0] == '"' && json.Unmarshal(raw, &s) == nil {
		part.Text, part.Bytes = s, len(s)
		return
	}
	part.Data, part.Bytes = raw, len(raw)
}

func dataPart(raw json.RawMessage) sessiondata.Part {
	return sessiondata.Part{Kind: sessiondata.PartData, Data: raw, State: model.ContentAvailable, Bytes: len(raw)}
}

// rawPart keeps bytes the dialect could not describe, every one of them.
func rawPart(b []byte, why string) sessiondata.Part {
	p := sessiondata.Part{Kind: sessiondata.PartUnknown, Text: why, State: model.ContentAvailable, Bytes: len(b)}
	p.SetRaw(b)
	return p
}
