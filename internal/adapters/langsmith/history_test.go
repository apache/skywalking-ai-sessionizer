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

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
)

const ai = `{"lc":1,"type":"constructor","id":["langchain","schema","messages","AIMessage"],"kwargs":{"content":"hi","id":"r1"}}`

func inputs(messages ...string) []byte {
	return []byte(`{"messages":[[` + strings.Join(messages, ",") + `]]}`)
}

// TestALangChainMessageIsReadOnlyInTheFormMeasured. Every field's type is
// checked before it is read, a shape no capture held is unavailable rather
// than guessed at, and a chunk keeps its own class.
func TestALangChainMessageIsReadOnlyInTheFormMeasured(t *testing.T) {
	h := historyOf(providerbody.RoleRequest, inputs(ai))
	if h.Form != providerbody.FormLangChain || h.Unavailable != "" || len(h.Messages) != 1 || h.Messages[0].ID == "" {
		t.Fatalf("history %+v", h)
	}
	for name, body := range map[string][]byte{
		"lc true":           inputs(`{"lc":true,"type":"constructor","id":["AIMessage"],"kwargs":{}}`),
		"lc 1.0":            inputs(`{"lc":1.0,"type":"constructor","id":["AIMessage"],"kwargs":{}}`),
		"a path of null":    inputs(`{"lc":1,"type":"constructor","id":[null],"kwargs":{}}`),
		"an empty path":     inputs(`{"lc":1,"type":"constructor","id":[],"kwargs":{}}`),
		"kwargs a list":     inputs(`{"lc":1,"type":"constructor","id":["AIMessage"],"kwargs":[]}`),
		"an id of a number": inputs(`{"lc":1,"type":"constructor","id":["AIMessage"],"kwargs":{"id":7}}`),
		"a plain message":   inputs(`{"type":"ai","content":"hi"}`),
		"a message 42":      inputs(`42`),
		"no messages":       []byte(`{"prompts":["hi"]}`),
		"not json":          []byte(`{`),
	} {
		if h := historyOf(providerbody.RoleRequest, body); h.Unavailable != providerbody.UnrecognisedShape {
			t.Fatalf("%s: %+v", name, h)
		}
	}
	for name, body := range map[string][]byte{
		"two prompts":     []byte(`{"messages":[[` + ai + `],[` + ai + `]]}`),
		"a prompt object": []byte(`{"messages":[{}]}`),
		"no prompt":       []byte(`{"messages":[]}`),
	} {
		if h := historyOf(providerbody.RoleRequest, body); h.Unavailable != providerbody.PluralPrompts {
			t.Fatalf("%s: %+v", name, h)
		}
	}
	// A null id is no id, as LangChain writes a message it gave none.
	if h := historyOf(providerbody.RoleRequest, inputs(`{"lc":1,"type":"constructor","id":["HumanMessage"],"kwargs":{"content":"q","id":null}}`)); h.Unavailable != "" || h.Messages[0].ID != "" {
		t.Fatalf("a null id: %+v", h)
	}
	chunk := historyOf(providerbody.RoleRequest, inputs(`{"lc":1,"type":"constructor","id":["langchain","schema","messages","AIMessageChunk"],"kwargs":{"content":"hi","id":"r1"}}`))
	if chunk.Unavailable != "" || chunk.Messages[0].Digest == h.Messages[0].Digest {
		t.Fatalf("a chunk is not read in its own class: %+v", chunk)
	}
	// Python and JavaScript write the path apart; the class is the same.
	js := historyOf(providerbody.RoleRequest, inputs(`{"lc":1,"type":"constructor","id":["langchain_core","messages","AIMessage"],"kwargs":{"content":"hi","id":"r1"}}`))
	if js.Messages[0] != h.Messages[0] {
		t.Fatalf("the JavaScript path reads apart: %v %v", js.Messages, h.Messages)
	}
}

// TestEveryFieldOfALangChainMessageCounts. Nothing LangChain recorded is left
// out of a message, since which fields reach a provider depends on its adapter
// and its mode: each pair differs in one field a provider may send, or LangChain
// may keep for itself, and reads as a different message.
func TestEveryFieldOfALangChainMessageCounts(t *testing.T) {
	msg := func(class, kwargs string) string {
		return `{"lc":1,"type":"constructor","id":["langchain","schema","messages","` + class + `"],"kwargs":` + kwargs + `}`
	}
	for name, pair := range map[string][2]string{
		"the role of a chat message": {msg("ChatMessage", `{"content":"x","role":"user"}`), msg("ChatMessage", `{"content":"x","role":"assistant"}`)},
		"a system message sent as the developer": {msg("SystemMessage", `{"content":"x"}`),
			msg("SystemMessage", `{"content":"x","additional_kwargs":{"__openai_role__":"developer"}}`)},
		"a function call's arguments": {msg("AIMessage", `{"content":"","additional_kwargs":{"function_call":{"name":"f","arguments":"{}"}}}`),
			msg("AIMessage", `{"content":"","additional_kwargs":{"function_call":{"name":"f","arguments":"{\"a\":1}"}}}`)},
		"a raw tool call under the same parsed one": {
			msg("AIMessage", `{"content":"","tool_calls":[{"name":"f","args":{},"id":"c"}],"additional_kwargs":{"tool_calls":[{"id":"c","function":{"name":"f","arguments":"{}"}}]}}`),
			msg("AIMessage", `{"content":"","tool_calls":[{"name":"f","args":{},"id":"c"}],"additional_kwargs":{"tool_calls":[{"id":"c","function":{"name":"f","arguments":"{ }"}}]}}`)},
		"a tool message's artifact": {msg("ToolMessage", `{"content":"x","tool_call_id":"c","artifact":1}`),
			msg("ToolMessage", `{"content":"x","tool_call_id":"c","artifact":2}`)},
		"the response's id": {msg("AIMessage", `{"content":"x","response_metadata":{"id":"resp_1"}}`),
			msg("AIMessage", `{"content":"x","response_metadata":{"id":"resp_2"}}`)},
		"a citation in a text block": {
			msg("AIMessage", `{"content":[{"type":"text","text":"x","annotations":[{"url":"https://a"}]}]}`),
			msg("AIMessage", `{"content":[{"type":"text","text":"x","annotations":[{"url":"https://b"}]}]}`)},
	} {
		a, b := historyOf(providerbody.RoleRequest, inputs(pair[0])), historyOf(providerbody.RoleRequest, inputs(pair[1]))
		if a.Unavailable != "" || b.Unavailable != "" || a.Messages[0] == b.Messages[0] {
			t.Fatalf("%s reads as the same message: %+v %+v", name, a, b)
		}
	}
}

// TestALangChainReplyIsTheOneGenerationsMessage.
func TestALangChainReplyIsTheOneGenerationsMessage(t *testing.T) {
	one := historyOf(providerbody.RoleResponse, []byte(`{"generations":[[{"text":"hi","message":`+ai+`}]]}`))
	sent := historyOf(providerbody.RoleRequest, inputs(ai))
	if one.Reply == nil || one.Reply.Unavailable != "" || one.Reply.ID != sent.Messages[0].ID || one.Reply.Digest != sent.Messages[0].Digest {
		t.Fatalf("reply %+v, sent %+v", one.Reply, sent.Messages[0])
	}
	for name, body := range map[string]string{
		"two generations":      `{"generations":[[{"message":` + ai + `},{"message":` + ai + `}]]}`,
		"two generation lists": `{"generations":[[{"message":` + ai + `}],[{"message":` + ai + `}]]}`,
		"a null generation":    `{"generations":[[null]]}`,
		"no generations":       `{"output":"hi"}`,
		"no message":           `{"generations":[[{"text":"hi"}]]}`,
	} {
		if h := historyOf(providerbody.RoleResponse, []byte(body)); h.Reply == nil || h.Reply.Unavailable == "" {
			t.Fatalf("%s: reply %+v", name, h.Reply)
		}
	}
}

func operation(op string, metadata map[string]any) Operation {
	extra, _ := json.Marshal(map[string]any{"metadata": metadata})
	return Operation{Op: op, RunID: "r", Envelope: json.RawMessage(`{"id":"r"}`), Fields: map[string]json.RawMessage{"extra": extra}}
}

// TestAModelCallsClassComesFromItsStart. Only a start carries the whole
// metadata, so an update classifies nothing; its record carries the class
// the start gave, held with the run, or none when the start's is not held.
// Two starts that disagree keep internal_call, the side that compares nothing.
func TestAModelCallsClassComesFromItsStart(t *testing.T) {
	if c := callClassOf(operation("post", map[string]any{"lc_internal_call": "true"})); c != classInternal {
		t.Fatalf("an internal start: %q", c)
	}
	if c := callClassOf(operation("post", map[string]any{"ls_model_name": "m"})); c != classTurn {
		t.Fatalf("a start: %q", c)
	}
	if c := callClassOf(Operation{Op: "post", RunID: "r", Envelope: json.RawMessage(`{"id":"r"}`)}); c != classTurn {
		t.Fatalf("a start with no metadata: %q", c)
	}
	if c := callClassOf(operation("patch", map[string]any{"lc_internal_call": "true"})); c != "" {
		t.Fatalf("an update classified a call: %q", c)
	}

	now := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	p := &pending{Runs: map[string]pendingRun{}}
	p.started(Run{ID: "r"}, "", classTurn, "s", now)
	p.started(Run{ID: "r"}, "", classInternal, "s", now)
	p.started(Run{ID: "r"}, "", classTurn, "s", now)
	p.started(Run{ID: "r"}, "", "", "s", now)
	if c := p.Runs["r"].Class; c != classInternal {
		t.Fatalf("the class held after a conflict: %q", c)
	}

	flagsOf := func(op Operation, class string) []string {
		records, err := ConvertRun(op, Run{ID: "r", TraceID: "r", Type: "llm"}, "", class, false, Hints{})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range records {
			if r.Call == "r" {
				return r.Flags
			}
		}
		t.Fatal("no model call record")
		return nil
	}
	if f := flagsOf(operation("patch", nil), classInternal); !slices.Contains(f, classInternal) {
		t.Fatalf("an update did not carry its start's class: %v", f)
	}
	if f := flagsOf(operation("patch", nil), ""); slices.Contains(f, classInternal) || slices.Contains(f, classTurn) {
		t.Fatalf("an update whose start's class is not held was classified: %v", f)
	}
	if f := flagsOf(operation("post", map[string]any{"lc_internal_call": "true"}), classTurn); !slices.Contains(f, classInternal) || slices.Contains(f, classTurn) {
		t.Fatalf("a start did not classify itself: %v", f)
	}
}
