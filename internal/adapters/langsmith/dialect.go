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
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"sort"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// Run is a run's envelope, in the client's own words. Nothing above the
// adapter sees these names: the dialect renders them into the model's roles
// and the runtime's vocabulary stops here.
type Run struct {
	ID       string          `json:"id"`
	TraceID  string          `json:"trace_id"`
	ParentID string          `json:"parent_run_id"`
	Dotted   string          `json:"dotted_order"`
	Type     string          `json:"run_type"`
	Name     string          `json:"name"`
	Start    string          `json:"start_time"`
	End      string          `json:"end_time"`
	Session  string          `json:"session_name"`
	Tags     []string        `json:"tags"`
	Error    json.RawMessage `json:"error"`
}

// Finished reports whether this arrival carries the run's end.
//
// It decides which arrival carries content, which is the rule that keeps a
// conversation from saying the model answered twice: every text block of every
// fragment becomes a message, so a post and a patch that both carry text emit
// the assistant message twice, in a round that calls itself verified. Measured,
// not reasoned. An arrival with no end lands its bytes as data instead.
func (r Run) Finished() bool { return r.End != "" }

// decoded is one arrival with its fields read.
type decoded struct {
	op      Operation
	run     Run
	extra   map[string]any
	failure string
	// inputs and outputs stay as the bytes they arrived as. Decoding them
	// into map[string]any and encoding them again turns every number into a
	// float64: a tool argument of 10006 comes back as 10006 but one of
	// 12345678901234567890 does not, and a part that carries the source's
	// own JSON must carry the source's own bytes.
	inputs   json.RawMessage
	outputs  json.RawMessage
	metadata map[string]any
	// alone says this request held the whole trace and no model call in it.
	// Then nothing else can be carrying what this run's fields hold.
	alone bool
}

func decodeOperation(op Operation) (decoded, error) {
	d := decoded{op: op}
	if err := json.Unmarshal(op.Envelope, &d.run); err != nil {
		return d, err
	}
	// A field the client sent out of band wins over the same field inside the
	// envelope: the envelope carries it only when it was small enough not to
	// be split out.
	d.inputs = rawField(op.Envelope, "inputs")
	d.outputs = rawField(op.Envelope, "outputs")
	unmarshalInto(op.Envelope, "extra", &d.extra)
	if raw, ok := op.Fields["inputs"]; ok {
		d.inputs = raw
	}
	if raw, ok := op.Fields["outputs"]; ok {
		d.outputs = raw
	}
	if raw, ok := op.Fields["extra"]; ok {
		_ = json.Unmarshal(raw, &d.extra)
	}
	if raw, ok := op.Fields["error"]; ok {
		var text string
		if json.Unmarshal(raw, &text) == nil {
			d.failure = text
		} else {
			d.failure = string(raw)
		}
	} else if len(d.run.Error) > 0 && string(d.run.Error) != "null" {
		d.failure = string(d.run.Error)
	}
	d.metadata, _ = d.extra["metadata"].(map[string]any)
	if d.metadata == nil {
		d.metadata = map[string]any{}
	}
	return d, nil
}

func unmarshalInto(envelope json.RawMessage, key string, into *map[string]any) {
	var holder map[string]json.RawMessage
	if json.Unmarshal(envelope, &holder) != nil {
		return
	}
	if raw, ok := holder[key]; ok {
		_ = json.Unmarshal(raw, into)
	}
}

// Digest is the digest of the bytes this arrival came as: the envelope and
// every field, in a fixed order, so two collectors reading one request produce
// one digest and a record that claims a source it did not come from is
// detectable.
func (o Operation) Digest() (string, int) {
	sum := sha256.New()
	sum.Write(o.Envelope)
	names := make([]string, 0, len(o.Fields))
	for name := range o.Fields {
		names = append(names, name)
	}
	// Every field the arrival came with, in one order, whether this dialect
	// understands it or not. A digest that covered only the known ones said
	// two different arrivals were the same one.
	//
	// The length of each piece goes in front of it, because writing a name
	// and then its value runs them together: the fields {"a":1,"b":2} and
	// {"a1b":2} both write a1b2, and two arrivals with the same envelope
	// then share a record id, which the index reads as a repeat and drops.
	// It is the same mistake a session id made with a separator, and it has
	// the same answer.
	sort.Strings(names)
	for _, name := range names {
		writeSized(sum, []byte(name))
		writeSized(sum, o.Fields[name])
	}
	return hex.EncodeToString(sum.Sum(nil))[:12], o.Bytes
}

// writeSized writes a piece so its boundary cannot be forged.
func writeSized(w io.Writer, b []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(b)))
	_, _ = w.Write(size[:])
	_, _ = w.Write(b)
}

// RecordID is the receipt: this arrival, never another.
//
// The call is shared by every arrival about a run, but the id cannot be,
// because the index keeps the first entry for an id and a later arrival would
// be hidden behind the earlier one. In the streaming capture the tool's result
// exists only in the patch, so one id would have shown a tool call that never
// returned.
func RecordID(op Operation) string {
	if op.Op == "post" {
		return op.RunID
	}
	digest, _ := op.Digest()
	return op.RunID + ":" + op.Op + ":" + digest
}

// Convert renders one arrival into the records it is evidence for.
//
// Most arrivals are one record. A trace's first arrival is two, because the
// human message that opened the turn lives inside the root run's inputs and
// has no run of its own.
func Convert(op Operation, firstOfTrace bool, hints Hints) ([]sessiondata.Record, error) {
	var envelope Run
	if err := json.Unmarshal(op.Envelope, &envelope); err != nil {
		return nil, err
	}
	return ConvertRun(op, envelope, "", firstOfTrace, hints)
}

// ConvertRun is Convert with the run already read, and with whatever an
// earlier arrival supplied that this one left out. An update carries only
// what changed, so on its own it says neither what kind of run it is nor
// which conversation it belongs to.
func ConvertRun(op Operation, run Run, method string, firstOfTrace bool, hints Hints) ([]sessiondata.Record, error) {
	d, err := decodeOperation(op)
	if err != nil {
		return nil, err
	}
	d.run = run
	// An update carries no metadata, so how the run was traced comes from
	// what its own start said.
	if method != "" {
		if _, ok := d.metadata["ls_method"]; !ok {
			d.metadata["ls_method"] = method
		}
	}
	d.alone = hints.SelfContained[d.run.TraceID]
	digest, bytes := op.Digest()
	base := sessiondata.Record{Sha: digest, Bytes: bytes}

	var out []sessiondata.Record
	if firstOfTrace && d.run.ParentID == "" {
		if input, ok := humanInput(base, d); ok {
			out = append(out, input)
		}
	}
	switch d.run.Type {
	case "llm", "chat_model":
		out = append(out, resets(base, d)...)
		out = append(out, modelCall(base, d))
	case "tool":
		// A tool that names no call is landed answering none.
		//
		// A decorated function calls its tools itself, so no model asks and
		// nothing in the trace carries a call for them. Making one up from
		// the tool run gave those traces a tool step, and it could not be
		// made safely: ls_method is inherited, so a decorated function
		// wrapping a graph marks that graph's own tools as traced too, and
		// no test on one request tells a trace with no model call from a
		// trace whose model call has not arrived yet. Every version of the
		// test invented a second call for one real execution somewhere.
		//
		// So it is not made. The tool's arguments and its result are landed
		// either way; what is missing is the step joining them, and a
		// missing step is better than one that says a call happened twice.
		hinted := hints.ToolCall[d.run.ID]
		out = append(out, toolResult(base, d, hinted))
	default:
		out = append(out, framework(base, d))
	}
	// A client that grows a field must not lose it here. The record says it
	// arrived and this dialect did not understand it, which is the contract
	// an unknown part exists for. Erasing it was worse than useless: the
	// request is removed from the inbox once it lands, so those bytes were
	// gone for good and nothing said so.
	if unknown := unknownParts(d); len(unknown) > 0 && len(out) > 0 {
		last := len(out) - 1
		out[last].Parts = append(out[last].Parts, unknown...)
	}
	return out, nil
}

// unknownParts are the fields this dialect has no reading for.
func unknownParts(d decoded) []sessiondata.Part {
	var names []string
	for name := range d.op.Fields {
		if !KnownFields[name] {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	out := make([]sessiondata.Part, 0, len(names))
	for _, name := range names {
		raw := d.op.Fields[name]
		out = append(out, sessiondata.Part{Kind: sessiondata.PartUnknown, Name: name,
			Data: raw, State: "available", Bytes: len(raw)})
	}
	return out
}

// humanInput is the message that opened the turn.
//
// From alone does not make it visible: an input step needs external_input, and
// a Talk boundary needs a trigger. A record with neither is landed evidence
// that produces no node, which reads as a conversation that never began.
//
// Three shapes reach here, all measured on one capture. A graph's root run
// names the speaker with role. A serialized message names it with type
// instead, and carries no role at all. And a traced function has no message
// list: its arguments are the input, and if they are not carried the
// conversation has no beginning and no question in it.
func humanInput(base sessiondata.Record, d decoded) (sessiondata.Record, bool) {
	// Named for the run that carried it, not for the trace. They are the
	// same string for a trace with a root, and a client can send a run with
	// no trace at all - and then every such run's opening message shared
	// one id, which the index reads as a repeat and drops.
	base.ID = d.run.ID + ":input"
	base.Run = d.run.TraceID
	base.From = sessiondata.FromExternal
	base.Time = d.run.Start
	base.Trigger = model.TriggerExternal
	base.Flags = []string{"external_input"}

	if messages := rawField(d.inputs, "messages"); len(messages) > 0 {
		var list []json.RawMessage
		if json.Unmarshal(messages, &list) != nil {
			return sessiondata.Record{}, false
		}
		// Only what is new. An application that keeps its own history sends
		// the whole conversation as the turn's input, so taking every human
		// message reported an earlier turn's question as though it had just
		// been asked - every turn, for ever. What opened this turn is the
		// run of human messages at the end, after the last thing anyone
		// else said.
		first := len(list)
		for first > 0 && speakerIsHuman(list[first-1]) {
			first--
		}
		var parts []sessiondata.Part
		for _, m := range list[first:] {
			parts = append(parts, contentParts(rawField(m, "content"))...)
		}
		if len(parts) == 0 {
			return sessiondata.Record{}, false
		}
		if first > 0 {
			// The rest of the list is the history this turn was given. It is
			// not landed here: it belongs to the turns that produced it, and
			// those turns are already in the conversation.
			base.Dropped = []sessiondata.Drop{{
				What: "the conversation history this turn was given", Bytes: 0,
				Why: "earlier turns already carry these messages",
			}}
		}
		base.Parts = parts
		return base, true
	}

	// No message list. The run's own arguments are what it was asked, so
	// they land as they arrived rather than being flattened into prose.
	if len(d.inputs) == 0 || string(d.inputs) == "null" || string(d.inputs) == "{}" {
		return sessiondata.Record{}, false
	}
	base.Parts = []sessiondata.Part{dataPart(d.inputs)}
	return base, true
}

// tracedDirectly reports that this run was traced by decorating a function,
// rather than by running inside a graph.
//
// Measured: every run of a decorated trace carries this, and no run of a
// graph trace carries it at all. It matters because the two say different
// things about who called a tool - in a graph the model did and its call is
// landed evidence, and here the surrounding function did and nothing else
// records it.
func tracedDirectly(d decoded) bool {
	method, _ := d.metadata["ls_method"].(string)
	return method == "traceable"
}

// speakerIsHuman reads whichever of the two fields names the speaker.
//
// A message the caller wrote says role; the same message read back out of the
// graph's state says type and has no role. Checking only one of them loses
// every turn of a conversation whose client serialises its messages.
func speakerIsHuman(message json.RawMessage) bool {
	var m struct {
		Role string `json:"role"`
		Type string `json:"type"`
	}
	if json.Unmarshal(message, &m) != nil {
		return false
	}
	for _, named := range []string{m.Role, m.Type} {
		if named == "user" || named == "human" {
			return true
		}
	}
	return false
}

// modelCall is one provider call. Its content lands once, on the arrival that
// carries the end.
func modelCall(base sessiondata.Record, d decoded) sessiondata.Record {
	base.ID = RecordID(d.op)
	base.Call = d.run.ID
	base.Run = d.run.TraceID
	base.Parent = d.run.ParentID
	base.From = sessiondata.FromAgent
	base.Time = timeOf(d)
	base.Model, _ = d.metadata["ls_model_name"].(string)
	if d.run.Finished() {
		base.Flags = append(base.Flags, "finished")
		message := generationOf(d.outputs)
		base.Parts = modelParts(message)
		base.Usage = usageOf(message)
	} else {
		base.Parts = []sessiondata.Part{dataPart(d.op.Envelope)}
	}
	if d.failure != "" {
		base.Flags = append(base.Flags, "error")
		base.Parts = append(base.Parts, textPart(d.failure))
	}
	return base
}

// toolResult is what a tool returned.
//
// Tool carries the CALL id, never the tool's name: a spawn is resolved by
// looking the call up by id, so a name leaves a child unattached and the
// result joined to nothing.
//
// Two output shapes are measured. A graph's tool node returns a whole tool
// message, with the call it answers and a status. A traced function returns
// whatever it returns, under output, most often a plain string - and reading
// only the first shape landed those results with no content at all.
func toolResult(base sessiondata.Record, d decoded, hinted string) sessiondata.Record {
	base.ID = RecordID(d.op)
	base.Label = d.run.Name
	base.Run = d.run.TraceID
	base.Parent = d.run.ParentID
	base.From = sessiondata.FromExternal
	base.Time = timeOf(d)

	// The tool message sits under output when there is one, and is the whole
	// outputs object when the runtime put it there directly.
	message := rawField(d.outputs, "output")
	if len(message) == 0 {
		message = d.outputs
	}
	var out struct {
		CallID string `json:"tool_call_id"`
		Status string `json:"status"`
	}
	_ = json.Unmarshal(message, &out)
	callID := out.CallID
	if callID == "" {
		// A tool that raised has empty outputs and names its call nowhere.
		// The trace knows, and Resolve reads it there when it can be sure.
		callID = hinted
	}
	base.Tool = callID
	failed := d.failure != "" || out.Status == "error"
	if d.run.Finished() {
		base.Flags = append(base.Flags, "finished")
		content := rawField(message, "content")
		if len(content) == 0 {
			// Not a tool message: whatever the function returned is the
			// result, as it arrived.
			content = message
		}
		parts := contentParts(content)
		result := sessiondata.Part{Kind: sessiondata.PartResult, Of: callID,
			State: "available"}
		switch {
		case len(parts) == 1 && parts[0].Kind == sessiondata.PartText:
			result.Text = parts[0].Text
			result.Bytes = len(result.Text)
		case len(parts) > 0:
			// More than one block, or a block that is not text. The content
			// is kept as it arrived. Keeping only the first part lost the
			// rest, and a result of two text blocks landed empty, because a
			// text part carries no data for the first part to be taken from.
			result.Data = content
			result.Bytes = len(content)
		case d.failure != "":
			result.Text = d.failure
			result.Bytes = len(result.Text)
		}
		if failed {
			yes := true
			result.Failed = &yes
		}
		base.Parts = []sessiondata.Part{result}
	} else {
		base.Parts = []sessiondata.Part{dataPart(d.op.Envelope)}
	}
	if failed {
		base.Flags = append(base.Flags, "error")
	}
	return base
}

// framework is a run the graph made for itself: a node, a prompt, a branch.
//
// Measured on the corpus, these are most of the runs and most of the bytes,
// because each carries content its model call already carries - the whole
// message list, or the tool call it is routing. Landing that would nearly
// quadruple a conversation for nothing new.
//
// So the question is who made the run. A graph's own machinery is
// bookkeeping and its fields are dropped, with the record saying how much
// went. A function someone decorated is not: its arguments and its return
// value live nowhere else, and for a traced application that is the whole
// conversation. Dropping those landed such traces with no content at all.
//
// The runtime says which it is, on every arrival, and that is measured: every
// run of a decorated trace carries ls_method and no run of a graph trace
// does. Testing the shape of the fields instead does not work. The repeats
// are not all message lists and they are not under one name - one capture
// held 164 KB of the message list under "output" and another 104 KB of a
// routing value that quoted a tool call - so every test on the content kept
// something large that was already landed elsewhere.
func framework(base sessiondata.Record, d decoded) sessiondata.Record {
	base.ID = RecordID(d.op)
	base.Run = d.run.TraceID
	base.Parent = d.run.ParentID
	base.Time = timeOf(d)
	base.Label = d.run.Name
	kept := map[string]any{
		"id": d.run.ID, "trace_id": d.run.TraceID, "run_type": d.run.Type,
		"name": d.run.Name, "dotted_order": d.run.Dotted,
	}
	if d.run.ParentID != "" {
		kept["parent_run_id"] = d.run.ParentID
	}
	shape := map[string]any{}
	for key, value := range d.metadata {
		if strings.HasPrefix(key, "langgraph_") || strings.HasPrefix(key, "ls_") {
			shape[key] = value
		}
	}
	if len(shape) > 0 {
		kept["metadata"] = shape
	}
	encoded, _ := json.Marshal(kept)
	parts := []sessiondata.Part{dataPart(encoded)}
	dropped := 0
	own := tracedDirectly(d)
	for _, field := range []struct {
		name string
		raw  json.RawMessage
	}{{"inputs", d.inputs}, {"outputs", d.outputs}} {
		switch {
		case len(field.raw) == 0 || string(field.raw) == "null":
		// A decorated function can still be handed the conversation, and
		// then its arguments are the repeat everything else was. Only when
		// that is all they are: a field holding a message list and an
		// answer beside it was dropped whole, and the answer it carried
		// existed nowhere else.
		//
		// And only when something else could be holding them. A decorated
		// function in a trace with no model call in it may well return
		// messages it built itself, and there is then no other record for
		// them to be a repeat of.
		case own && d.alone:
			parts = append(parts, sessiondata.Part{Kind: sessiondata.PartData,
				Name: field.name, Data: field.raw, State: "available", Bytes: len(field.raw)})
		case own && !isOnlyMessages(field.raw):
			parts = append(parts, sessiondata.Part{Kind: sessiondata.PartData,
				Name: field.name, Data: field.raw, State: "available", Bytes: len(field.raw)})
		default:
			dropped += len(field.raw)
		}
	}
	base.Parts = parts
	if dropped > 0 {
		base.Dropped = []sessiondata.Drop{{
			What: "inputs and outputs", Bytes: dropped,
			Why: "a graph's own run repeats content its model call already carries",
		}}
	}
	if d.failure != "" {
		base.From = sessiondata.FromRuntime
		base.Flags = append(base.Flags, "error")
		base.Parts = append(base.Parts, textPart(d.failure))
	}
	return base
}

// isOnlyMessages reports whether a field is a message list and nothing else.
//
// It is the test for a decorated function's own arguments and results, where
// anything beside the messages is the function's and is recorded nowhere
// else. A graph's own run is not asked this: everything it carries is a
// repeat, whatever shape it is in.
func isOnlyMessages(raw json.RawMessage) bool {
	var holder map[string]json.RawMessage
	if json.Unmarshal(raw, &holder) != nil {
		// Not an object: a bare message list is a repeat, anything else is
		// the function's own value.
		return repeatsMessages(raw)
	}
	if len(holder) == 0 {
		return false
	}
	for key, value := range holder {
		if key != "messages" || !repeatsMessages(value) {
			return false
		}
	}
	return true
}

// repeatsMessages reports whether a field carries conversation messages
// anywhere inside it.
//
// The test is the shape of a message, not the name of the field that holds
// it. A graph's own node holds the list under "messages", but a prompt run
// holds the same list under "output" and a sequence holds one message there,
// so a check on the key kept 164 KB of one capture - the largest repeat in
// it - while believing it had dropped every repeat.
//
// A message is an object with content, named by a type or a role that the
// runtime uses for one. Requiring the name to be one of those is what keeps
// a traced function's own return value, which can have a content field of
// its own and mean something else entirely.
func repeatsMessages(raw json.RawMessage) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	var walk func(any) bool
	walk = func(v any) bool {
		switch t := v.(type) {
		case map[string]any:
			if list, ok := t["messages"]; ok {
				// An empty list is not a repeat of anything.
				if inner, isList := list.([]any); !isList || len(inner) > 0 {
					return true
				}
			}
			if _, ok := t["content"]; ok {
				for _, named := range []string{"type", "role"} {
					if kind, ok := t[named].(string); ok && messageKinds[kind] {
						return true
					}
				}
			}
			for _, inner := range t {
				if walk(inner) {
					return true
				}
			}
		case []any:
			for _, inner := range t {
				if walk(inner) {
					return true
				}
			}
		}
		return false
	}
	return walk(value)
}

// messageKinds are the names a runtime gives a conversation message, as
// measured over the captured corpus: ai, human, tool and system as a type,
// and user as a role. The rest are here because the same vocabularies use
// them and a capture that happens not to contain one proves nothing.
var messageKinds = map[string]bool{
	"ai": true, "human": true, "tool": true, "system": true,
	"function": true, "chat": true, "developer": true,
	"user": true, "assistant": true,
}

// timeOf is when this arrival says the run was: its end when it has one, its
// start otherwise. A record's time is evidence, never the clock.
func timeOf(d decoded) string {
	if d.run.End != "" {
		return d.run.End
	}
	return d.run.Start
}

// generationOf digs out what the model produced, without decoding it, so
// what it carries stays the bytes the provider sent.
//
// It returns the generation itself rather than the message inside it,
// because not every model returns a message. A chat model does; an ordinary
// one reports its completion as text on the generation, and requiring a
// message landed those answers as an empty object.
func generationOf(outputs json.RawMessage) json.RawMessage {
	generations := rawField(outputs, "generations")
	if len(generations) == 0 {
		return nil
	}
	first := rawIndex(generations, 0)
	inner := rawIndex(first, 0)
	if len(inner) == 0 {
		// Some runs carry one generation rather than a list of lists.
		inner = first
	}
	return inner
}

// messageOf is the chat message inside a generation, when there is one.
func messageOf(generation json.RawMessage) json.RawMessage {
	message := rawField(generation, "message")
	if len(message) == 0 {
		return nil
	}
	if kwargs := rawField(message, "kwargs"); len(kwargs) > 0 {
		return kwargs
	}
	return message
}

// providerMessage is what the model said. Everything a reader has to get back
// unchanged stays raw.
type providerMessage struct {
	Content   json.RawMessage `json:"content"`
	ToolCalls []struct {
		ID   string          `json:"id"`
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	} `json:"tool_calls"`
	Usage *struct {
		Input   int `json:"input_tokens"`
		Output  int `json:"output_tokens"`
		Details struct {
			CacheRead  int `json:"cache_read"`
			CacheWrite int `json:"cache_creation"`
		} `json:"input_token_details"`
	} `json:"usage_metadata"`
}

// modelParts renders what the model said: its text, then each tool call it
// asked for, in the order it asked.
//
// A call's arguments land as the bytes they arrived as. Reading them into a
// map and writing them out again made every number a float64, so a tool
// argument that was an exact integer came back rounded, and a reader
// replaying the call would send the model something it never sent.
func modelParts(generation json.RawMessage) []sessiondata.Part {
	message := messageOf(generation)
	if len(message) == 0 {
		// An ordinary model, which answers with text and asks for nothing.
		return contentParts(rawField(generation, "text"))
	}
	var m providerMessage
	_ = json.Unmarshal(message, &m)
	parts := contentParts(m.Content)
	for _, call := range m.ToolCalls {
		args := call.Args
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		parts = append(parts, sessiondata.Part{Kind: sessiondata.PartCall, ID: call.ID,
			Name: call.Name, Data: args, State: "available", Bytes: len(args)})
	}
	if len(parts) == 0 {
		parts = []sessiondata.Part{dataPart(json.RawMessage(`{}`))}
	}
	return parts
}

// contentParts renders a message's content, whichever shape it came in.
//
// A provider that answers with text sends a string. One that answers with
// blocks sends a list, and a block that is not text - a thought, an image -
// is kept as it arrived rather than flattened into prose it is not.
func contentParts(content json.RawMessage) []sessiondata.Part {
	if len(content) == 0 || string(content) == "null" {
		return nil
	}
	var text string
	if json.Unmarshal(content, &text) == nil {
		if text == "" {
			return nil
		}
		return []sessiondata.Part{textPart(text)}
	}
	var blocks []json.RawMessage
	if json.Unmarshal(content, &blocks) != nil {
		// Neither a string nor a list: an object the runtime returned, kept
		// whole because nothing here knows what it means.
		return []sessiondata.Part{dataPart(content)}
	}
	var parts []sessiondata.Part
	for _, block := range blocks {
		var inner string
		if json.Unmarshal(block, &inner) == nil {
			if inner != "" {
				parts = append(parts, textPart(inner))
			}
			continue
		}
		var typed struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(block, &typed) == nil && typed.Type == "text" && typed.Text != "" {
			parts = append(parts, textPart(typed.Text))
			continue
		}
		parts = append(parts, dataPart(block))
	}
	return parts
}

// usageOf reads what the provider reported. Only a finished call has it: an
// unfinished one carries a streaming stub whose output count means nothing.
func usageOf(generation json.RawMessage) *sessiondata.Usage {
	message := messageOf(generation)
	if len(message) == 0 {
		return nil
	}
	var m providerMessage
	if json.Unmarshal(message, &m) != nil || m.Usage == nil {
		return nil
	}
	return &sessiondata.Usage{
		Input: m.Usage.Input, Output: m.Usage.Output,
		CacheRead: m.Usage.Details.CacheRead, CacheWrite: m.Usage.Details.CacheWrite,
	}
}

// rawField takes one key out of a JSON object without decoding the rest.
func rawField(raw json.RawMessage, key string) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var holder map[string]json.RawMessage
	if json.Unmarshal(raw, &holder) != nil {
		return nil
	}
	return holder[key]
}

// rawIndex takes one element out of a JSON list without decoding the rest.
func rawIndex(raw json.RawMessage, i int) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) != nil || i >= len(list) {
		return nil
	}
	return list[i]
}

func textPart(text string) sessiondata.Part {
	return sessiondata.Part{Kind: sessiondata.PartText, Text: text,
		State: "available", Bytes: len(text)}
}

func dataPart(data json.RawMessage) sessiondata.Part {
	return sessiondata.Part{Kind: sessiondata.PartData, Data: data,
		State: "available", Bytes: len(data)}
}
