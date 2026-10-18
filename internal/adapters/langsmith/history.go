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

	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
)

// historyOf reads what a body says about its call's history, in the form
// langchain/1 (docs/en/formats/session-data.md).
//
// A message is compared as LangChain serialized it, with nothing left out.
// Which fields reach the provider depends on the provider's adapter and its
// mode, which a trace does not say, and leaving out a field one of them sends
// hides a change. Measured on the twenty capture directories under
// tests/apps/langchain/testdata: every one of 80 message ids sent more than
// once has one form. A message is read only in the constructor form, the only
// one a model call's inputs held there; its class is the last element of its
// path, since Python and JavaScript write the path differently, and a chunk
// class keeps its own name, as a loop over model.stream() sends it back.
func historyOf(role string, body []byte) *providerbody.History {
	h := &providerbody.History{Form: providerbody.FormLangChain}
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
		h.Messages, h.Unavailable = requestOf(head)
	case providerbody.RoleResponse:
		h.Reply = replyOf(head)
	}
	return h
}

// requestOf reads a model call's inputs: exactly one prompt, every message in
// the constructor form.
func requestOf(inputs map[string]any) ([]providerbody.Entry, string) {
	prompts, ok := inputs["messages"].([]any)
	if !ok {
		return nil, providerbody.UnrecognisedShape
	}
	if len(prompts) != 1 {
		return nil, providerbody.PluralPrompts
	}
	list, ok := prompts[0].([]any)
	if !ok {
		return nil, providerbody.PluralPrompts
	}
	entries := make([]providerbody.Entry, 0, len(list))
	for _, m := range list {
		e, ok := messageEntry(m)
		if !ok {
			return nil, providerbody.UnrecognisedShape
		}
		entries = append(entries, e)
	}
	return entries, ""
}

// replyOf reads a model call's outputs: exactly one generation list holding
// exactly one generation, whose message is the reply.
func replyOf(outputs map[string]any) *providerbody.Reply {
	gens, ok := outputs["generations"].([]any)
	if !ok || len(gens) != 1 {
		return &providerbody.Reply{Unavailable: providerbody.NoSingleReply}
	}
	list, ok := gens[0].([]any)
	if !ok || len(list) != 1 {
		return &providerbody.Reply{Unavailable: providerbody.NoSingleReply}
	}
	gen, ok := list[0].(map[string]any)
	if !ok {
		return &providerbody.Reply{Unavailable: providerbody.NoSingleReply}
	}
	e, ok := messageEntry(gen["message"])
	if !ok {
		return &providerbody.Reply{Unavailable: providerbody.UnrecognisedShape}
	}
	return &providerbody.Reply{ID: e.ID, Digest: e.Digest}
}

// messageEntry is the entry of one constructor-form message: {"lc": 1, "type":
// "constructor", "id": [..., class], "kwargs": {...}}, read as its class and its
// kwargs without the id. Every field's type is checked before it is read: an
// "lc" of true is not 1.
func messageEntry(raw any) (providerbody.Entry, bool) {
	m, ok := raw.(map[string]any)
	if !ok || m["type"] != "constructor" {
		return providerbody.Entry{}, false
	}
	if lc, ok := m["lc"].(json.Number); !ok || string(lc) != "1" {
		return providerbody.Entry{}, false
	}
	path, ok := m["id"].([]any)
	if !ok || len(path) == 0 {
		return providerbody.Entry{}, false
	}
	for _, p := range path {
		if _, ok := p.(string); !ok {
			return providerbody.Entry{}, false
		}
	}
	kwargs, ok := m["kwargs"].(map[string]any)
	if !ok {
		return providerbody.Entry{}, false
	}
	kept := make(map[string]any, len(kwargs))
	for k, v := range kwargs {
		if k != "id" {
			kept[k] = v
		}
	}
	id, hasID := kwargs["id"].(string)
	if raw, present := kwargs["id"]; present && raw != nil && !hasID {
		return providerbody.Entry{}, false
	}
	form, err := providerbody.Canonical(map[string]any{"class": path[len(path)-1], "kwargs": kept})
	if err != nil {
		return providerbody.Entry{}, false
	}
	return providerbody.EntryOf(id, hasID, form), true
}

// The class of a model call, as a record's flag. A model call LangChain makes
// for its own purpose, such as SummarizationMiddleware's summary, carries
// lc_internal_call in its run's metadata (tests/apps/langchain/testdata/
// summarized), and takes no part in the history of its stream.
const (
	classInternal = model.ClassInternalCall
	classTurn     = model.ClassTurnCall
)

// callClassOf is the class a run's start gives a model call: internal when its
// metadata carries lc_internal_call, a turn otherwise, as a start with no
// metadata at all is. Only a start carries the whole metadata, so an update
// classifies nothing: a key it lacks says nothing.
func callClassOf(op Operation) string {
	if op.Op != "post" {
		return ""
	}
	var extra struct {
		Metadata map[string]json.RawMessage `json:"metadata"`
	}
	if raw, ok := op.Fields["extra"]; ok {
		_ = json.Unmarshal(raw, &extra)
	} else {
		var inline map[string]json.RawMessage
		if json.Unmarshal(op.Envelope, &inline) == nil {
			if raw, ok := inline["extra"]; ok {
				_ = json.Unmarshal(raw, &extra)
			}
		}
	}
	if _, ok := extra.Metadata["lc_internal_call"]; ok {
		return classInternal
	}
	return classTurn
}

// eligibleReply is a response's reply only when its run's request landed and
// read as exactly one prompt: a generation answers the prompts it was asked,
// and the answer to several prompts, or to a request no rule could read, is
// not the reply to a history. A list too large still read as one prompt. The
// request lands from the run's first arrival that carries inputs, before or
// with the response, in this pass or an earlier one, so the session holds it.
func eligibleReply(held *providerbody.Session, run string, reply *providerbody.Reply) *providerbody.Reply {
	m, ok := held.Manifest(run + ":request")
	switch {
	case !ok || m.Messages == nil:
		return &providerbody.Reply{Unavailable: providerbody.NoRequest}
	case m.Messages.Unavailable == providerbody.PluralPrompts || m.Messages.Unavailable == providerbody.UnrecognisedShape:
		return &providerbody.Reply{Unavailable: m.Messages.Unavailable}
	}
	return reply
}
