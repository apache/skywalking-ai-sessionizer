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

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// A context reset is taken only from a mark the framework wrote on purpose.
//
// LangChain's SummarizationMiddleware replaces the history with one message,
// and marks that message with additional_kwargs lc_source = "summarization".
// Measured with langchain 1.4.2, on the summarized capture: the mark is on the
// summary message in every request that carries it, and nowhere else in a
// model call's request.
//
// Nothing is inferred from the shape of the message list. A list that lost its
// oldest messages, or gained a message quoting an earlier answer, can be a
// trim, a summary, a new task or an edit, and the list alone cannot say which.
// langmem and trim_messages write no mark, so they make no reset here.
const (
	summaryMarkKey   = "lc_source"
	summaryMarkValue = "summarization"
)

// resets returns a boundary and its summary for each marked message in a
// model call's request.
//
// Every later request carries the same summary again, so the same records are
// returned for each. Their ids come from the summary message, never from the
// call, so a repeat has the same ids: the placement lands each once while it
// remembers, and the index keeps the first when it does not. The placement
// also puts the stream in the ids, because an epoch belongs to a stream, and a
// summary forwarded into another agent's history resets that agent too.
//
// They are returned before the call, so the reset lands before the call whose
// request first carried the summary, and the summariser's own call, which read
// the old history, lands before it. That holds when a call's request arrives
// with its first record, which every model call in the captured corpus did
// (71 of 71). A request that arrives after its call has begun lands its reset
// after that call's first record, and landed files are never rewritten, so
// that one call counts in the old epoch.
func resets(base sessiondata.Record, d decoded) []sessiondata.Record {
	var out []sessiondata.Record
	for _, m := range requestMessages(d.inputs) {
		fields := m
		if kwargs := rawField(m, "kwargs"); len(kwargs) > 0 {
			// A serialized message keeps its fields under kwargs.
			fields = kwargs
		}
		extra := rawField(fields, "additional_kwargs")
		var mark map[string]any
		if json.Unmarshal(extra, &mark) != nil || mark[summaryMarkKey] != summaryMarkValue {
			continue
		}
		var id string
		_ = json.Unmarshal(rawField(fields, "id"), &id)
		if id == "" {
			// The middleware gives every message an id before it is sent.
			// A marked message without one cannot be told from another
			// summary with the same words, or from itself sent again with
			// different escaping, so naming it by its content could both
			// miss a reset and invent one. It makes none.
			continue
		}
		content := rawField(fields, "content")

		boundary := base
		boundary.ID = id + ":reset"
		boundary.Run = d.run.TraceID
		boundary.From = sessiondata.FromRuntime
		boundary.Time = d.run.Start
		boundary.Flags = []string{"context_reset"}
		// The mark itself, as it arrived, is the evidence for the reset.
		boundary.Parts = []sessiondata.Part{dataPart(extra)}

		summary := base
		summary.ID = id + ":summary"
		summary.Run = d.run.TraceID
		summary.Parent = boundary.ID
		summary.From = sessiondata.FromExternal
		summary.Time = d.run.Start
		summary.Flags = []string{"reset_summary"}
		summary.Parts = contentParts(content)

		out = append(out, boundary, summary)
	}
	return out
}

// requestMessages is a model call's message list. A chat model's inputs hold a
// list of lists, one per prompt, and a completion model's a single list.
func requestMessages(inputs json.RawMessage) []json.RawMessage {
	var lists [][]json.RawMessage
	if json.Unmarshal(rawField(inputs, "messages"), &lists) == nil {
		var out []json.RawMessage
		for _, l := range lists {
			out = append(out, l...)
		}
		return out
	}
	var list []json.RawMessage
	_ = json.Unmarshal(rawField(inputs, "messages"), &list)
	return list
}

// isReset reports a record resets returned.
func isReset(r sessiondata.Record) bool {
	for _, f := range r.Flags {
		if f == "context_reset" || f == "reset_summary" {
			return true
		}
	}
	return false
}
