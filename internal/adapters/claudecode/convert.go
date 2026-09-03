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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// Convert turns one Claude Code source record into a Session Data record.
//
// This is the only function that reads Claude Code's shape. Everything above it
// - the index, assembly, the structure chain, a reader - sees parts and role
// names and never a runtime field.
//
// Three rules govern what happens to bytes:
//
//   - Understood content becomes a part.
//   - Content the dialect does not recognise becomes an `unknown` part that
//     KEEPS THE BYTES. A later version of this dialect can interpret it without
//     the source, which is what makes converting on read safe.
//   - Content deliberately not carried is named in Dropped with its size, so
//     the loss is stated rather than silent.
func Convert(src Source, ord, off uint64, payload []byte) *sessiondata.Record {
	sum := sha256.Sum256(payload)
	rec := &sessiondata.Record{
		Ord: ord, Off: off,
		Sha:   hex.EncodeToString(sum[:])[:12],
		Bytes: len(payload),
	}

	// A manifest and a script are both written by the harness, and saying so is
	// what lets a reader tell them from anything a person or a model produced.
	rec.From = sessiondata.FromRuntime
	// A script is JavaScript. Nothing here can describe it, so it stays bytes.
	if src.Kind == SrcWorkflowScript {
		rec.Parts = []sessiondata.Part{rawPart(payload, "the source is a program, not data")}
		return rec
	}
	// A workflow manifest is one JSON document rather than a stream of records:
	// structured, readable, and not prose. It still names the workflow it ran,
	// so it is decoded far enough to take that before the document travels
	// whole.
	if src.Kind == SrcWorkflowManifest {
		var d indexRecord
		if err := json.Unmarshal(payload, &d); err == nil {
			rec.Label = d.WorkflowName
			rec.Batch = firstOf(d.RunID, src.RunID)
		}
		rec.Parts = []sessiondata.Part{{
			Kind: sessiondata.PartData, Data: json.RawMessage(payload),
			State: model.ContentAvailable, Bytes: len(payload),
		}}
		return rec
	}

	var d indexRecord
	if err := json.Unmarshal(payload, &d); err != nil {
		// A record that will not parse is still evidence. Its position is known
		// and its bytes are kept; nothing about it is guessed.
		rec.Parts = []sessiondata.Part{rawPart(payload, "the record is not valid JSON")}
		return rec
	}

	tur, hasTUR := decodeToolResult(d.ToolUseResult)
	rec.ID, rec.Parent = d.UUID, d.ParentUUID
	rec.Call, rec.Run = d.Message.ID, d.PromptID
	rec.Continues = d.LogicalParentUUID
	rec.Time = d.Timestamp
	rec.Trigger = triggerName(triggerOf(&d))
	tool, child := linksOf(&d, &tur, src)
	rec.Tool, rec.Child = tool, child
	rec.Batch = tur.RunID
	if rec.Batch == "" {
		rec.Batch = src.RunID
	}
	rec.StartedBy = d.ParentAgentID
	rec.Label = firstOf(d.AITitle, d.Description, d.WorkflowName)
	rec.Flags = flagNames(flagsOf(&d, &tur, hasTUR, src))
	rec.From = producerOf(&d, src)
	if u := d.Message.Usage; u != nil {
		rec.Usage = &sessiondata.Usage{
			Input: u.Input, Output: u.Output,
			CacheRead: u.CacheRead, CacheWrite: u.CacheWrite,
		}
	}

	rec.Parts, rec.Dropped = partsOf(&d, &tur, hasTUR, payload)
	if len(rec.Parts) == 0 {
		// A record with no message and no attachment still said something - a
		// journal announcing a child, a manifest, an artefact the host keeps for
		// itself. It is structured and readable, just not prose, so it travels
		// as data rather than as something unrecognised.
		rec.Parts = []sessiondata.Part{{
			Kind: sessiondata.PartData, Data: json.RawMessage(payload),
			State: model.ContentAvailable, Bytes: len(payload),
		}}
	}
	return rec
}

// partsOf breaks a record's content into parts.
func partsOf(d *indexRecord, tur *toolResult, hasTUR bool, payload []byte) ([]sessiondata.Part, []sessiondata.Drop) {
	var parts []sessiondata.Part
	var dropped []sessiondata.Drop

	// An attachment carries its text outside the message, in a shape that
	// differs per attachment type. What is readable becomes text; the rest is
	// kept whole rather than picked over.
	if d.Type == "attachment" {
		text, obj := attachmentContent(payload)
		p := sessiondata.Part{Kind: sessiondata.PartText, Text: text,
			State: model.ContentAvailable, Bytes: len(text)}
		if text == "" {
			// Most attachment types are a set of fields rather than a sentence -
			// names added and removed, a count, a list. They are still injected
			// context and a reader still wants them, so they travel as data
			// rather than as something we failed to recognise.
			p = sessiondata.Part{Kind: sessiondata.PartData, Data: obj,
				State: model.ContentAvailable, Bytes: len(obj)}
		}
		parts = append(parts, p)
		return parts, dropped
	}

	content := d.Message.Content
	if len(content) == 0 {
		return parts, dropped
	}
	// Content is sometimes a bare string rather than a list of blocks.
	if content[0] == '"' {
		var text string
		if err := json.Unmarshal(content, &text); err == nil {
			parts = append(parts, sessiondata.Part{
				Kind: sessiondata.PartText, Text: text,
				State: model.ContentAvailable, Bytes: len(text),
			})
			return parts, dropped
		}
	}

	var blocks []json.RawMessage
	if err := json.Unmarshal(content, &blocks); err != nil {
		return append(parts, rawPart(content, "the content is neither text nor a list of blocks")), dropped
	}
	for _, raw := range blocks {
		p, drop := partOf(raw, tur, hasTUR)
		parts = append(parts, p)
		if drop != nil {
			dropped = append(dropped, *drop)
		}
	}
	return parts, dropped
}

// blockOf is the shape of one content block, decoded far enough to classify it.
type blockOf struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Signature string          `json:"signature"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   *bool           `json:"is_error"`
	Source    struct {
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	} `json:"source"`
}

// partOf converts one content block.
func partOf(raw json.RawMessage, tur *toolResult, hasTUR bool) (sessiondata.Part, *sessiondata.Drop) {
	var b blockOf
	if err := json.Unmarshal(raw, &b); err != nil {
		return rawPart(raw, "the block is not an object"), nil
	}
	switch b.Type {
	case "text":
		return sessiondata.Part{Kind: sessiondata.PartText, Text: b.Text,
			State: model.ContentAvailable, Bytes: len(b.Text)}, nil

	case "thinking", "redacted_thinking":
		// Reasoning is usually withheld: the block arrives with a signature and
		// no text at all. Calling that "available" claims we have something we
		// do not, which is the one thing this model is not allowed to do.
		state := model.ContentAvailable
		switch {
		case b.Type == "redacted_thinking":
			state = model.ContentRedacted
		case b.Thinking == "":
			state = model.ContentUnavailable
		}
		p := sessiondata.Part{Kind: sessiondata.PartReasoning, Text: b.Thinking,
			State: state, Bytes: len(b.Thinking)}
		if b.Signature == "" {
			return p, nil
		}
		// A signature is material a provider verifies when a block is handed
		// back to it. No reader can use it, and it is the second largest thing
		// in the corpus - 13.3% of all bytes, wrapping reasoning text that
		// totals 0.1 MB. It is named here rather than silently discarded.
		return p, &sessiondata.Drop{
			What: "reasoning signature", Bytes: len(b.Signature),
			Why: "a provider verifies it; a reader cannot read it",
		}

	case "tool_use", "server_tool_use":
		return sessiondata.Part{Kind: sessiondata.PartCall, ID: b.ID, Name: b.Name,
			Data: b.Input, State: model.ContentAvailable, Bytes: len(b.Input)}, nil

	case "tool_result":
		p := sessiondata.Part{Kind: sessiondata.PartResult, Of: b.ToolUseID,
			State: model.ContentAvailable}
		p.Text, p.Bytes = resultText(b.Content)
		if b.IsError != nil {
			p.Failed = b.IsError
		}
		// A result whose content is neither a string nor text blocks - an image,
		// a reference to another tool's output - reads as empty otherwise. The
		// raw content travels instead of being described, because the source it
		// came from can be pruned and this would be the only copy.
		if p.Text == "" && len(b.Content) > 0 {
			p.Data = b.Content
			p.Bytes = len(b.Content)
		}
		// The runtime also gives a structured form of the same result on the
		// parent lineage - stdout and stderr split apart, a patch, a status.
		// It is a different view of the same output, so it travels beside the
		// text rather than replacing it.
		if hasTUR && tur.raw != nil {
			p.Data = tur.raw
		}
		return p, nil

	case "image":
		return sessiondata.Part{Kind: sessiondata.PartMedia, Media: b.Source.MediaType,
			Data: mustJSON(b.Source.Data), State: model.ContentAvailable,
			Bytes: len(b.Source.Data)}, nil
	}
	// Anything else keeps its bytes. A block type that did not exist when this
	// was written must not be lost because of it.
	return rawPart(raw, "block type "+quote(b.Type)+" is not one this dialect describes"), nil
}

// resultText pulls the readable text out of a tool result's content, which is
// sometimes a string and sometimes a list of blocks.
func resultText(content json.RawMessage) (string, int) {
	if len(content) == 0 {
		return "", 0
	}
	if content[0] == '"' {
		var s string
		if err := json.Unmarshal(content, &s); err == nil {
			return s, len(s)
		}
	}
	var blocks []blockOf
	if err := json.Unmarshal(content, &blocks); err == nil {
		var out []string
		for _, b := range blocks {
			if b.Text != "" {
				out = append(out, b.Text)
			}
		}
		if len(out) > 0 {
			joined := strings.Join(out, "\n")
			return joined, len(joined)
		}
	}
	return "", len(content)
}

// attachmentContent returns an attachment's prose, or its whole object when it
// has none.
//
// There are dozens of attachment types and their shapes differ. Two fields
// carry prose; the rest carry structure - names added and removed, a count, a
// list of skills. Rather than guess at each shape, the prose is taken where it
// exists and the object travels whole where it does not.
func attachmentContent(payload []byte) (string, json.RawMessage) {
	var d struct {
		Attachment json.RawMessage `json:"attachment"`
	}
	if err := json.Unmarshal(payload, &d); err != nil {
		return "", nil
	}
	var a struct {
		Text    string          `json:"text"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(d.Attachment, &a); err != nil {
		return "", d.Attachment
	}
	if a.Text != "" {
		return a.Text, d.Attachment
	}
	if len(a.Content) > 0 && a.Content[0] == '"' {
		var s string
		if err := json.Unmarshal(a.Content, &s); err == nil {
			return s, d.Attachment
		}
	}
	return "", d.Attachment
}

// rawPart keeps bytes the dialect could not describe.
func rawPart(b []byte, why string) sessiondata.Part {
	return sessiondata.Part{
		Kind: sessiondata.PartUnknown, Data: mustJSON(string(b)),
		Text: why, State: model.ContentAvailable, Bytes: len(b),
	}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

func quote(s string) string { return `"` + s + `"` }

// firstOf returns the first value that is not empty.
func firstOf(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// producerOf says who wrote a record.
//
// A record's own type is a poor guide: every tool result in the corpus sits on
// a record typed "user", and only a small share of those are a person. What
// matters downstream is who produced it.
func producerOf(d *indexRecord, src Source) sessiondata.From {
	switch src.Kind {
	case SrcAgentMeta, SrcJournal, SrcWorkflowManifest, SrcWorkflowScript:
		return sessiondata.FromRuntime
	}
	switch d.Type {
	case "assistant":
		return sessiondata.FromAgent
	case "system", "attachment":
		return sessiondata.FromRuntime
	case "user":
		return sessiondata.FromExternal
	}
	return sessiondata.FromRuntime
}

// triggerName renders a trigger in the model's vocabulary.
func triggerName(t index.Trigger) string {
	switch t {
	case index.TriggerExternal:
		return model.TriggerExternal
	case index.TriggerNotification:
		return model.TriggerNotification
	case index.TriggerOther:
		return model.TriggerUnknown
	}
	return ""
}

// flagNames renders the flags a record carries.
var flagNames = func(f index.Flags) []string {
	var out []string
	for _, m := range []struct {
		bit  index.Flags
		name string
	}{
		{index.FlagSynthetic, "synthetic"},
		{index.FlagEpochBoundary, "context_reset"},
		{index.FlagEpochSummary, "reset_summary"},
		{index.FlagStopReason, "finished"},
		{index.FlagExternalInput, "external_input"},
		{index.FlagInjection, "injected"},
		{index.FlagChildResult, "child_result"},
		{index.FlagError, "error"},
		{index.FlagLaunchAck, "launch_ack"},
		{index.FlagTurnDuration, "turn_duration"},
		{index.FlagCommand, "command"},
		{index.FlagNotice, "notice"},
	} {
		if f.Has(m.bit) {
			out = append(out, m.name)
		}
	}
	return out
}
