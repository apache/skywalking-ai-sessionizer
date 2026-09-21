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
	"time"
)

// Hints are what a whole request knows that one arrival does not.
//
// Only one thing needs them so far, and it is measured rather than imagined: a
// tool that raised has empty outputs, and the call it was answering is named
// nowhere in its run. Not in outputs, not in inputs, not in its metadata. The
// evidence exists, but it is in the model call that asked for the tool, which
// is a different run.
type Hints struct {
	// ToolCall gives the call a tool run answers, by run id, for the runs
	// whose own evidence does not say.
	ToolCall map[string]string
	// SelfContained says which traces this request holds whole: the trace's
	// root is here and no model call is anywhere in it. Only such a trace
	// can be said to have no model call, and only then may a call be made
	// up for a tool that names none.
	//
	// A request holding part of a trace says nothing either way. The model
	// call that asked may simply have landed earlier, and inventing a
	// second call for one real execution is worse than leaving the tool
	// joined to nothing.
	SelfContained map[string]bool
}

// Resolve reads a request's arrivals for what a single one cannot know.
//
// It is not a guess. A tool run is joined only when exactly one call of that
// name in that trace is still unanswered: one candidate is the evidence, and
// several candidates mean the request does not say, so nothing is claimed and
// the result stays unjoined rather than joined to the wrong call.
//
// What it cannot reach is stated rather than worked around. When the model
// call and the failed tool arrive in different requests, the id is not here to
// be found, and the result lands unjoined.
func Resolve(operations []Operation) Hints { return ResolveWith(operations, nil) }

// ResolveWith is Resolve with each run already read, so that an update which
// carries only its outputs is still known to be the tool or the model call
// it is.
func ResolveWith(operations []Operation, known map[string]Run) Hints {
	type candidate struct {
		callID string
		// stream is the run this call was made inside, so a call made by a
		// nested agent is never offered to a tool of the caller.
		stream string
		// done is when the model call ended, which is the only evidence
		// that it happened before the tool ran.
		done time.Time
	}

	// Which runs of this request are tools, so a run's stream can be read
	// from its own ancestry. The stream of a tool run is its caller's,
	// because a tool's call and its result are the caller's evidence.
	runOf := func(op Operation) (Run, bool) {
		if run, ok := known[op.RunID]; ok {
			return run, true
		}
		var run Run
		return run, json.Unmarshal(op.Envelope, &run) == nil
	}
	toolRuns := map[string]bool{}
	here := map[string]bool{}
	for _, op := range operations {
		run, ok := runOf(op)
		if !ok {
			continue
		}
		here[run.ID] = true
		if run.Type == "tool" {
			toolRuns[run.ID] = true
		}
	}
	// Which agent a run belongs to is its ancestry, and an ancestor this
	// request does not carry has no known kind. Reading a missing ancestor
	// as though it were not a tool put two nested agents' runs in one place,
	// where a failure in one could take the other's only candidate.
	streamOf := func(run Run) (string, bool) {
		for _, id := range ancestors(run.Dotted, run.ID) {
			if toolRuns[id] {
				return id, true
			}
			if !here[id] {
				return "", false
			}
		}
		return MainStream, true
	}
	asked := map[string][]candidate{} // trace + "\x1f" + tool name
	claimed := map[string]bool{}
	var pending []Operation

	for _, op := range operations {
		d, err := decodeOperation(op)
		if err != nil {
			continue
		}
		if run, ok := known[op.RunID]; ok {
			d.run = run
		}
		switch d.run.Type {
		case "llm", "chat_model":
			var message providerMessage
			if raw := messageOf(generationOf(d.outputs)); len(raw) > 0 {
				_ = json.Unmarshal(raw, &message)
			}
			for _, call := range message.ToolCalls {
				if call.ID == "" || call.Name == "" {
					continue
				}
				stream, known := streamOf(d.run)
				if !known {
					continue
				}
				key := d.run.TraceID + "\x1f" + call.Name
				asked[key] = append(asked[key], candidate{callID: call.ID,
					stream: stream, done: momentOf(d.run.End, d.run.Start)})
			}
		case "tool":
			var out struct {
				CallID string `json:"tool_call_id"`
			}
			message := rawField(d.outputs, "output")
			if len(message) == 0 {
				message = d.outputs
			}
			_ = json.Unmarshal(message, &out)
			if out.CallID != "" {
				claimed[out.CallID] = true
				continue
			}
			pending = append(pending, op)
		}
	}
	if len(pending) == 0 {
		return Hints{}
	}
	out := Hints{ToolCall: map[string]string{}}
	for _, op := range pending {
		d, err := decodeOperation(op)
		if err != nil {
			continue
		}
		if run, ok := known[op.RunID]; ok {
			d.run = run
		}
		key := d.run.TraceID + "\x1f" + d.run.Name
		// A call the model made after this tool ran cannot be the call this
		// tool answered, and neither can a call made by a different agent.
		// Without both tests a conversation that asks for the same tool more
		// than once joined a failure to whichever call had not been answered
		// yet, which in a loop is the next one rather than the one that
		// failed.
		//
		// The order has to come from the times, not from the dotted order.
		// A dotted order is the shape of the tree, and each branch carries
		// its own start, so a model call that ran late inside a branch that
		// started early sorts before a tool that ran before it.
		//
		// The same arrival can also be delivered twice, so a call counts
		// once however many times it was seen. Otherwise one repeat made an
		// unambiguous join look ambiguous and the result landed unjoined.
		began := momentOf(d.run.Start, d.run.End)
		stream, known := streamOf(d.run)
		if !known {
			continue
		}
		seen := map[string]bool{}
		var free []candidate
		for _, c := range asked[key] {
			if claimed[c.callID] || seen[c.callID] {
				continue
			}
			if c.stream != stream {
				continue
			}
			if began.IsZero() || c.done.IsZero() || c.done.After(began) {
				// No evidence of order, or the call came after. Either way
				// this is not the call to claim.
				continue
			}
			seen[c.callID] = true
			free = append(free, c)
		}
		if len(free) != 1 {
			// Nothing to be sure of. The result lands saying which tool it
			// came from and joined to no call, which reads as a gap rather
			// than as an answer to the wrong question.
			continue
		}
		out.ToolCall[d.run.ID] = free[0].callID
		claimed[free[0].callID] = true
	}
	return out
}

// momentOf reads a run's time, preferring the first and falling back to the
// second. A run with neither has no observed moment, and nothing is inferred
// from that.
func momentOf(preferred, fallback string) time.Time {
	for _, text := range []string{preferred, fallback} {
		if text == "" {
			continue
		}
		if at, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return at
		}
	}
	return time.Time{}
}
