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

package claudecodeprovider

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
)

// This file is the whole of what the adapter knows about how Claude Code names
// and fills its body files. Everything past it reads the model's words.

// ID is the record id of a body file: its name without the .json extension,
// such as "fcc261af-….request" or "req_011C….response". The role stays in
// the id, so a request and a response never share one.
func ID(file string) string { return strings.TrimSuffix(file, ".json") }

// RoleOf reports the role a file name gives a body, or "" for a name Claude
// Code does not write.
func RoleOf(file string) string {
	switch {
	case strings.HasSuffix(file, ".request.json"):
		return providerbody.RoleRequest
	case strings.HasSuffix(file, ".response.json"):
		return providerbody.RoleResponse
	}
	return ""
}

var (
	promptIDRe = regexp.MustCompile(`cc_prompt_id=([^;\s]+)`)
	prevReqRe  = regexp.MustCompile(`cc_prev_req=([^;\s]+)`)
	uuidRe     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// Lift reads the keys from a body Claude Code wrote under file, in the
// model's words. A value the body does not carry stays empty; nothing is
// inferred.
//
// For Claude Code 2.1.260: metadata.user_id is a JSON string holding
// session_id; the first system block is a billing header holding
// cc_prompt_id, the run, and cc_prev_req, the previous request; a response's
// id is the call; a response file is named by the provider's request id, or
// by a random UUID when Claude Code had none.
func Lift(file string, body []byte) providerbody.Keys {
	var k providerbody.Keys
	role := RoleOf(file)
	if role == providerbody.RoleResponse {
		// The name holds even when the body is not what a response should be.
		if stem := strings.TrimSuffix(file, ".response.json"); !uuidRe.MatchString(stem) {
			k.Request = stem
		}
	}
	var head map[string]json.RawMessage
	if json.Unmarshal(body, &head) != nil {
		return k
	}
	str := func(raw json.RawMessage) string {
		var v string
		_ = json.Unmarshal(raw, &v)
		return v
	}
	k.Model = str(head["model"])
	switch role {
	case providerbody.RoleRequest:
		var meta map[string]json.RawMessage
		_ = json.Unmarshal(head["metadata"], &meta)
		var user struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal([]byte(str(meta["user_id"])), &user) == nil {
			k.Session = user.SessionID
		}
		header := firstSystemText(head["system"])
		if m := promptIDRe.FindStringSubmatch(header); m != nil {
			k.Run = m[1]
		}
		if m := prevReqRe.FindStringSubmatch(header); m != nil {
			k.PreviousRequest = m[1]
		}
	case providerbody.RoleResponse:
		k.Call = str(head["id"])
	}
	return k
}

// BodyOf is a body file as a session cuts it.
func BodyOf(file string, body []byte) providerbody.Body {
	return providerbody.Body{ID: ID(file), Role: RoleOf(file), Src: file, Keys: Lift(file, body), Bytes: body,
		History: HistoryOf(RoleOf(file), body)}
}

// firstSystemText is the text of the first system block, or the system
// prompt itself when it is one string.
func firstSystemText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil && len(blocks) > 0 {
		return blocks[0].Text
	}
	return ""
}

// billingHeader starts the system block Claude Code writes its request
// bookkeeping into. It differs on every request, measured on 2.1.302: it holds
// the request before, the prompt and the turn.
const billingHeader = "x-anthropic-billing-header:"

// HistoryOf reads what a body says about its call's history, in the form
// claude-code/1 (docs/en/formats/session-data.md).
//
// A request's messages are compared as sent, with two exceptions measured on
// 2.1.260 and again on 2.1.302, both for a message sent again unchanged: the
// cache marker moves to the newest message, so cache_control is left out at a
// message's depth and at each of its content blocks'; and a content list of
// one text block is sent again as its plain string, so such a list reads as
// the string, but only when the block holds nothing besides its type, its text
// and the marker. A request that continues a provider thread holds only what
// follows the message it names; the assembler builds the rest.
func HistoryOf(role string, body []byte) *providerbody.History {
	h := &providerbody.History{Form: providerbody.FormClaudeCode}
	v, err := providerbody.Decode(body)
	head, ok := v.(map[string]any)
	if err != nil || !ok {
		h.Unavailable = providerbody.UnrecognisedShape
		if role == providerbody.RoleResponse {
			h.Reply = &providerbody.Reply{Unavailable: providerbody.UnrecognisedShape}
		}
		return h
	}
	switch role {
	case providerbody.RoleRequest:
		readRequest(h, head)
	case providerbody.RoleResponse:
		h.Reply = replyOf(head)
	}
	return h
}

func readRequest(h *providerbody.History, head map[string]any) {
	if raw, ok := head["thread"]; ok {
		t, ok := threadOf(raw)
		if !ok {
			h.Unavailable = providerbody.UnrecognisedShape
			return
		}
		h.Thread = t
	}
	h.System = systemOf(head["system"])
	list, ok := head["messages"].([]any)
	if !ok {
		h.Unavailable = providerbody.UnrecognisedShape
		return
	}
	entries := make([]providerbody.Entry, 0, len(list))
	for _, raw := range list {
		e, ok := entryOf(raw)
		if !ok {
			h.Unavailable = providerbody.UnrecognisedShape
			return
		}
		entries = append(entries, e)
	}
	h.Messages = entries
}

// threadOf reads a request's thread: one that creates a thread, or continues
// one from a message id of a provider's size. Any other type says the request
// holds something no rule here describes.
func threadOf(raw any) (*providerbody.Thread, bool) {
	t, ok := raw.(map[string]any)
	if !ok {
		return nil, false
	}
	switch typ, _ := t["type"].(string); typ {
	case providerbody.ThreadCreate:
		return &providerbody.Thread{Type: typ}, true
	case providerbody.ThreadContinue:
		prev, _ := t["previous_message_id"].(string)
		if prev == "" || len(prev) > providerbody.MaxPrevious {
			return nil, false
		}
		return &providerbody.Thread{Type: typ, Previous: prev}, true
	}
	return nil, false
}

// systemOf is the digest of the system prompt without its billing header, or
// "" when nothing else is there. A list of blocks loses the text blocks that
// start with the header and keeps the rest as sent, their cache markers too,
// which do not move: one form each across every request measured. Any other
// value is digested as it is.
func systemOf(raw any) string {
	value := raw
	switch s := raw.(type) {
	case nil:
		return ""
	case string:
		if s == "" {
			return ""
		}
	case []any:
		var rest []any
		for _, b := range s {
			if block, ok := b.(map[string]any); ok && block["type"] == "text" {
				if text, _ := block["text"].(string); strings.HasPrefix(text, billingHeader) {
					continue
				}
			}
			rest = append(rest, b)
		}
		if len(rest) == 0 {
			return ""
		}
		value = rest
	}
	form, err := providerbody.Canonical(value)
	if err != nil {
		return ""
	}
	return providerbody.EntryOf("", false, form).Digest
}

// entryOf is one message's entry. Claude Code sends no message id.
func entryOf(raw any) (providerbody.Entry, bool) {
	msg, ok := raw.(map[string]any)
	if !ok {
		return providerbody.Entry{}, false
	}
	form, err := providerbody.Canonical(messageForm(msg))
	if err != nil {
		return providerbody.Entry{}, false
	}
	return providerbody.EntryOf("", false, form), true
}

// messageForm is a message as compared: a copy, with the cache marker left out
// of the message and of each content block, and a lone text block read as its
// string.
func messageForm(msg map[string]any) map[string]any {
	out := make(map[string]any, len(msg))
	for k, v := range msg {
		if k != "cache_control" {
			out[k] = v
		}
	}
	blocks, ok := out["content"].([]any)
	if !ok {
		return out
	}
	content := make([]any, len(blocks))
	for i, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok {
			content[i] = b
			continue
		}
		kept := make(map[string]any, len(block))
		for k, v := range block {
			if k != "cache_control" {
				kept[k] = v
			}
		}
		content[i] = kept
	}
	out["content"] = content
	if len(content) == 1 {
		if block, ok := content[0].(map[string]any); ok && len(block) == 2 && block["type"] == "text" {
			if text, ok := block["text"].(string); ok {
				out["content"] = text
			}
		}
	}
	return out
}

// replyOf is the message a response produced: its role and content, without
// the envelope that belongs to the response. The response's id is never in a
// message, so the reply carries none. A response with no stop reason did not
// finish, and one that is not an assistant message is not a reply.
func replyOf(head map[string]any) *providerbody.Reply {
	content, ok := head["content"].([]any)
	if !ok || head["role"] != "assistant" || head["type"] != "message" || head["stop_reason"] == nil {
		return &providerbody.Reply{Unavailable: providerbody.NoSingleReply}
	}
	e, ok := entryOf(map[string]any{"role": "assistant", "content": content})
	if !ok {
		return &providerbody.Reply{Unavailable: providerbody.UnrecognisedShape}
	}
	return &providerbody.Reply{Digest: e.Digest}
}
