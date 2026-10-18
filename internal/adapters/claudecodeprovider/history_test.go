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

package claudecodeprovider_test

import (
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecodeprovider"
	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
)

func request(t *testing.T, body string) *providerbody.History {
	t.Helper()
	h := claudecodeprovider.HistoryOf(providerbody.RoleRequest, []byte(body))
	if h.Form != providerbody.FormClaudeCode {
		t.Fatalf("form %q", h.Form)
	}
	return h
}

// TestAMessageSentAgainUnchangedHasOneForm holds the two exceptions the form
// makes, measured on Claude Code 2.1.260 and 2.1.302: the cache marker moves
// to the newest message, and a message of one text block is sent again as its
// string. Anything else in a message counts.
func TestAMessageSentAgainUnchangedHasOneForm(t *testing.T) {
	marked := request(t, `{"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}],"cache_control":{"x":1}}]}`)
	plain := request(t, `{"messages":[{"role":"user","content":"hi"}]}`)
	if marked.Messages[0] != plain.Messages[0] {
		t.Fatalf("a message with the marker and the same message collapsed differ: %v %v", marked.Messages, plain.Messages)
	}
	for name, body := range map[string]string{
		"a citation":   `{"messages":[{"role":"user","content":[{"type":"text","text":"hi","citations":[]}]}]}`,
		"two blocks":   `{"messages":[{"role":"user","content":[{"type":"text","text":"h"},{"type":"text","text":"i"}]}]}`,
		"another role": `{"messages":[{"role":"system","content":"hi"}]}`,
	} {
		if got := request(t, body); got.Messages[0] == plain.Messages[0] {
			t.Fatalf("%s reads as the plain message", name)
		}
	}
	// The marker is left out only where it moves: at a message's depth and at
	// its blocks'. One deeper, inside a tool result, is the result's own.
	inner := request(t, `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"text","text":"hi","cache_control":{}}]}]}]}`)
	bare := request(t, `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"text","text":"hi"}]}]}]}`)
	if inner.Messages[0] == bare.Messages[0] {
		t.Fatal("a marker inside a tool result was left out")
	}
	if m := request(t, `{"messages":[{"role":"user","content":"hi"}]}`).Messages[0]; m.ID != "" {
		t.Fatalf("a Claude Code message has an id %q; Claude Code sends none", m.ID)
	}
}

// TestARequestNamesItsThread. A request that starts a thread or carries none
// holds its whole list; one that continues names the message it goes on
// from. A thread no rule here describes leaves the list unavailable.
func TestARequestNamesItsThread(t *testing.T) {
	if h := request(t, `{"messages":[]}`); h.Thread != nil || h.Unavailable != "" || h.Messages == nil {
		t.Fatalf("no thread: %+v", h)
	}
	if h := request(t, `{"thread":{"type":"create"},"messages":[]}`); h.Thread == nil || h.Thread.Type != providerbody.ThreadCreate {
		t.Fatalf("create: %+v", h.Thread)
	}
	h := request(t, `{"thread":{"type":"continue","previous_message_id":"msg_1"},"messages":[{"role":"user","content":"x"}]}`)
	if h.Thread == nil || h.Thread.Type != providerbody.ThreadContinue || h.Thread.Previous != "msg_1" || len(h.Messages) != 1 {
		t.Fatalf("continue: %+v %+v", h.Thread, h.Messages)
	}
	long := strings.Repeat("m", providerbody.MaxPrevious+1)
	for name, body := range map[string]string{
		"another type":       `{"thread":{"type":"fork"},"messages":[]}`,
		"no message id":      `{"thread":{"type":"continue"},"messages":[]}`,
		"a long message id":  `{"thread":{"type":"continue","previous_message_id":"` + long + `"},"messages":[]}`,
		"not an object":      `{"thread":"create","messages":[]}`,
		"no messages":        `{"thread":{"type":"create"}}`,
		"a message string":   `{"messages":["hi"]}`,
		"not an object body": `[]`,
		"not json":           `{`,
	} {
		if h := request(t, body); h.Unavailable != providerbody.UnrecognisedShape || h.Messages != nil {
			t.Fatalf("%s: %+v", name, h)
		}
	}
}

// TestTheSystemPromptLeavesOutItsBillingHeader. The header differs on every
// request, measured on 2.1.302: it holds the request before, the prompt and
// the turn. Everything else in the system prompt counts, its markers too.
func TestTheSystemPromptLeavesOutItsBillingHeader(t *testing.T) {
	a := request(t, `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_prev_req=a;"},{"type":"text","text":"You are"}],"messages":[]}`)
	b := request(t, `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_prev_req=b;"},{"type":"text","text":"You are"}],"messages":[]}`)
	if a.System == "" || a.System != b.System {
		t.Fatalf("two system prompts that differ only in the header: %q %q", a.System, b.System)
	}
	if c := request(t, `{"system":[{"type":"text","text":"You are","cache_control":{"type":"ephemeral"}}],"messages":[]}`); c.System == a.System {
		t.Fatal("a marker on a system block was left out")
	}
	if h := request(t, `{"system":[{"type":"text","text":"x-anthropic-billing-header: x;"}],"messages":[]}`); h.System != "" {
		t.Fatalf("a system prompt of only the header has a digest %q", h.System)
	}
	// A system prompt written as one string is digested as that string, and
	// only a text block is a billing header.
	form, _ := providerbody.Canonical("hi")
	if h := request(t, `{"system":"hi","messages":[]}`); h.System != providerbody.EntryOf("", false, form).Digest {
		t.Fatalf("a string system prompt has the digest %q", h.System)
	}
	if h := request(t, `{"system":[{"type":"other","text":"x-anthropic-billing-header: x;"}],"messages":[]}`); h.System == "" {
		t.Fatal("a block that is not text was taken for the billing header")
	}
}

// TestTheReplyIsTheMessageAResponseProduced. Its form is a request message's
// form, so a reply compares with the message sent back; the response's id is
// never in a message, so the reply has none.
func TestTheReplyIsTheMessageAResponseProduced(t *testing.T) {
	resp := claudecodeprovider.HistoryOf(providerbody.RoleResponse,
		[]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{}}`))
	sent := request(t, `{"messages":[{"role":"assistant","content":"hi"}]}`)
	if resp.Reply == nil || resp.Reply.Unavailable != "" || resp.Reply.ID != "" || resp.Reply.Digest != sent.Messages[0].Digest {
		t.Fatalf("reply %+v, sent %+v", resp.Reply, sent.Messages[0])
	}
	for name, body := range map[string]string{
		"unfinished": `{"type":"message","role":"assistant","content":[],"stop_reason":null}`,
		"an error":   `{"type":"error","error":{"type":"overloaded_error"}}`,
		"no content": `{"type":"message","role":"assistant","stop_reason":"end_turn"}`,
		"not json":   `{`,
	} {
		if h := claudecodeprovider.HistoryOf(providerbody.RoleResponse, []byte(body)); h.Reply == nil || h.Reply.Unavailable == "" {
			t.Fatalf("%s: reply %+v", name, h.Reply)
		}
	}
}
