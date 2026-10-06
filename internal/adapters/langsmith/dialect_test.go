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
	"os"
	"slices"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// convertCase renders every arrival of a case, in the order they arrived.
func convertCase(t *testing.T, kase string) []sessiondata.Record {
	t.Helper()
	var out []sessiondata.Record
	seen := map[string]bool{}
	operations := operationsOf(t, kase)
	hints := Resolve(operations)
	for _, op := range operations {
		trace := decode(t, op).Trace
		records, err := Convert(op, !seen[trace], hints)
		if err != nil {
			t.Fatalf("%s: %s: %v", kase, op.RunID, err)
		}
		seen[trace] = true
		out = append(out, records...)
	}
	return out
}

// TestEveryArrivalConverts is the broad one: nothing in the corpus defeats the
// dialect, and nothing it produces is a record with no content at all.
func TestEveryArrivalConverts(t *testing.T) {
	cases, total := 0, 0
	captured(t, func(_, _ string, _ capturedMeta, _ []byte) {})
	for _, kase := range []string{"plain", "three-turns", "tool-error", "parallel-tools",
		"loop", "two-threads", "slow-tool", "large-content", "subagent",
		"subagent-own-thread", "no-thread-key", "shared-thread-key",
		"unsafe-thread-key", "abandoned-run", "traceable-only", "summarized"} {
		records := convertCase(t, kase)
		if len(records) == 0 {
			t.Fatalf("%s: converted to nothing", kase)
		}
		for _, r := range records {
			if len(r.Parts) == 0 {
				t.Errorf("%s: %s has no parts", kase, r.ID)
			}
			if r.Sha == "" || r.Bytes == 0 {
				t.Errorf("%s: %s has no provenance", kase, r.ID)
			}
		}
		cases++
		total += len(records)
	}
	t.Logf("%d cases, %d records", cases, total)
}

// TestOneArrivalCarriesContent is the rule a measurement forced. Two arrivals
// of one call both carrying text produce one call with two assistant messages,
// in a round the parser calls verified — a wrong conversation that passes every
// check. Only the arrival that carries the run's end may carry its content.
func TestOneArrivalCarriesContent(t *testing.T) {
	for _, kase := range []string{"slow-tool", "abandoned-run", "three-turns", "loop"} {
		carrying := map[string]int{}
		for _, r := range convertCase(t, kase) {
			if r.Call == "" && r.Tool == "" {
				continue
			}
			key := r.Call
			if key == "" {
				key = r.ID
			}
			for _, p := range r.Parts {
				if p.Kind == sessiondata.PartText || p.Kind == sessiondata.PartCall ||
					p.Kind == sessiondata.PartResult {
					carrying[key]++
					break
				}
			}
		}
		for call, n := range carrying {
			if n > 1 {
				t.Errorf("%s: call %s carries content on %d arrivals", kase, call, n)
			}
		}
	}
}

// TestUnfinishedRunsLandAsData: work in progress is evidence, and it lands,
// but it says nothing the conversation would have to un-say later.
func TestUnfinishedRunsLandAsData(t *testing.T) {
	data, finished := 0, 0
	for _, op := range operationsOf(t, "abandoned-run") {
		records, err := Convert(op, false, Hints{})
		if err != nil {
			t.Fatal(err)
		}
		open := decode(t, op).End == ""
		for _, r := range records {
			for _, p := range r.Parts {
				if open && p.Kind != sessiondata.PartData {
					t.Errorf("%s arrived unfinished but carries a %s part", r.ID, p.Kind)
				}
			}
			if open {
				data++
			} else {
				finished++
			}
		}
	}
	if data == 0 {
		t.Fatal("no unfinished run landed")
	}
	t.Logf("%d records from unfinished runs, %d from finished", data, finished)
}

// TestToolCarriesTheCallID is the other rule a build forced: a spawn resolves
// by looking the call up by id, so the tool's name leaves a child unattached.
func TestToolCarriesTheCallID(t *testing.T) {
	for _, kase := range []string{"three-turns", "subagent", "parallel-tools"} {
		tools := 0
		for _, r := range convertCase(t, kase) {
			if r.Tool == "" {
				continue
			}
			tools++
			for _, p := range r.Parts {
				if p.Kind == sessiondata.PartResult && p.Of != r.Tool {
					t.Errorf("%s: result joins %q but the record names %q", kase, p.Of, r.Tool)
				}
			}
		}
		if tools == 0 {
			t.Errorf("%s: no tool result", kase)
		}
	}
}

// TestFailureSurvives: a tool that raised has to read as a failed result, not
// as a tool that never returned.
func TestFailureSurvives(t *testing.T) {
	failed := 0
	for _, r := range convertCase(t, "tool-error") {
		for _, p := range r.Parts {
			if p.Kind == sessiondata.PartResult && p.Failed != nil && *p.Failed {
				failed++
			}
		}
	}
	if failed == 0 {
		t.Fatal("the failing tool did not land a failed result")
	}
}

// TestFrameworkRunsSayWhatTheyDropped: the shape is kept, the repeated content
// is not, and the record states the loss rather than leaving it silent.
func TestFrameworkRunsSayWhatTheyDropped(t *testing.T) {
	kept, dropped := 0, 0
	for _, r := range convertCase(t, "loop") {
		if r.Call != "" || r.Tool != "" || len(r.Flags) > 0 && r.Flags[0] == "external_input" {
			continue
		}
		kept++
		if len(r.Dropped) > 0 {
			dropped += r.Dropped[0].Bytes
		}
	}
	if kept == 0 || dropped == 0 {
		t.Fatalf("framework runs: %d records, %d bytes reported dropped", kept, dropped)
	}
	t.Logf("%d framework records, %d bytes of repeated content reported dropped", kept, dropped)
}

// TestAnUnfinishedCallIsNamedForTheRequestItCarries reads the shapes an
// unfinished model call can land in. Its envelope lands whole, so when the
// client put the request's messages or tools inside it, the record is named
// for them and a reader can withhold it. The corpus never does that, and its
// one unfinished call lands with neither name.
func TestAnUnfinishedCallIsNamedForTheRequestItCarries(t *testing.T) {
	flagsOf := func(envelope string) []string {
		t.Helper()
		records, err := Convert(Operation{Op: "post", RunID: "r1", Envelope: json.RawMessage(envelope)}, false, Hints{})
		if err != nil || len(records) == 0 {
			t.Fatalf("%v %v", records, err)
		}
		return records[len(records)-1].Flags
	}
	run := `"id":"r1","trace_id":"t1","parent_run_id":"p1","run_type":"llm","name":"ChatModel","start_time":"2026-09-20T10:46:45Z"`
	for _, tc := range []struct {
		name          string
		envelope      string
		prompt, tools bool
	}{
		{"inputs and extra out of band, as measured", `{` + run + `}`, false, false},
		{"a system message inside", `{` + run + `,"inputs":{"messages":[[{"type":"system","content":"You are a helpful assistant."},{"type":"human","content":"hi"}]]}}`, true, false},
		{"a system field of its own", `{` + run + `,"inputs":{"system":"You are a helpful assistant.","messages":[{"role":"user","content":"hi"}]}}`, true, false},
		{"a completion's prompts", `{` + run + `,"inputs":{"prompts":["System: You are a helpful assistant.\nHuman: hi"]}}`, true, false},
		{"a person's message alone, which is still the request", `{` + run + `,"inputs":{"messages":[{"type":"human","content":"hi"}]}}`, true, false},
		{"tools offered in the parameters", `{` + run + `,"extra":{"invocation_params":{"tools":[{"type":"function","function":{"name":"read"}}]}}}`, false, true},
		{"tools keyed by name", `{` + run + `,"extra":{"invocation_params":{"tools":{"read":{"type":"function"}}}}}`, false, true},
		{"functions offered in the parameters", `{` + run + `,"extra":{"invocation_params":{"functions":[{"name":"read"}]}}}`, false, true},
		{"tools in the inputs", `{` + run + `,"inputs":{"messages":[],"tools":[{"name":"read"}]}}`, true, true},
		{"tools in a configuration of Bedrock's", `{` + run + `,"inputs":{"messages":[],"toolConfig":{"tools":[{"toolSpec":{"name":"read"}}]}}}`, true, true},
		{"tools in a configuration of Gemini's", `{` + run + `,"inputs":{"contents":[],"config":{"tools":[{"function_declarations":[{"name":"read"}]}]}}}`, true, true},
		{"tools in Bedrock's configuration among the parameters", `{` + run + `,"extra":{"invocation_params":{"toolConfig":{"tools":[{"toolSpec":{"name":"read"}}]}}}}`, false, true},
		{"tools among a client's options", `{` + run + `,"extra":{"options":{"tools":[{"name":"read"}]}}}`, false, true},
		{"empty inputs and no tools offered", `{` + run + `,"inputs":{},"extra":{"invocation_params":{"tools":[]}}}`, false, false},
	} {
		flags := flagsOf(tc.envelope)
		if got := slices.Contains(flags, sessiondata.FlagSystemPrompt); got != tc.prompt {
			t.Errorf("%s: system_prompt %v, want %v (%v)", tc.name, got, tc.prompt, flags)
		}
		if got := slices.Contains(flags, sessiondata.FlagToolSchemas); got != tc.tools {
			t.Errorf("%s: tool_schemas %v, want %v (%v)", tc.name, got, tc.tools, flags)
		}
	}
	cases, err := os.ReadDir(corpus)
	if err != nil {
		t.Skipf("no corpus at %s: %v", corpus, err)
	}
	for _, c := range cases {
		if !c.IsDir() {
			continue
		}
		for _, r := range convertCase(t, c.Name()) {
			if slices.Contains(r.Flags, sessiondata.FlagSystemPrompt) || slices.Contains(r.Flags, sessiondata.FlagToolSchemas) {
				t.Errorf("%s: %s is named %v, and the corpus carries the request out of band", c.Name(), r.ID, r.Flags)
			}
		}
	}
}

// TestARootModelCallsInputIsNamed reads the first input of a trace whose root
// is the model call itself. With no message list, its arguments land whole on
// the first-input record, and for a model call those arguments are the
// request, system prompt and all. A graph's own arguments are the
// application's, and are named only by their shape, as
// TestADecoratedFunctionIsNamedForWhatItHolds reads them.
func TestARootModelCallsInputIsNamed(t *testing.T) {
	for _, tc := range []struct {
		name          string
		kind, inputs  string
		prompt, tools bool
	}{
		{"a completion asked with prompts", "llm", `{"prompts":["System: You are a helpful assistant.\nHuman: hi"]}`, true, false},
		{"a model call offered tools in its inputs", "chat_model", `{"prompt":"hi","tools":[{"name":"read"}]}`, true, true},
		{"a graph asked a question", "chain", `{"question":"what changed in the docs"}`, false, false},
	} {
		envelope := `{"id":"r1","trace_id":"r1","run_type":"` + tc.kind + `","name":"Root","start_time":"2026-09-20T10:46:45Z","inputs":` + tc.inputs + `}`
		records, err := Convert(Operation{Op: "post", RunID: "r1", Envelope: json.RawMessage(envelope)}, true, Hints{})
		if err != nil {
			t.Fatal(err)
		}
		var input *sessiondata.Record
		for i := range records {
			if records[i].ID == "r1:input" {
				input = &records[i]
			}
		}
		if input == nil {
			t.Fatalf("%s: no first-input record in %v", tc.name, records)
		}
		if got := slices.Contains(input.Flags, sessiondata.FlagSystemPrompt); got != tc.prompt {
			t.Errorf("%s: system_prompt %v, want %v (%v)", tc.name, got, tc.prompt, input.Flags)
		}
		if got := slices.Contains(input.Flags, sessiondata.FlagToolSchemas); got != tc.tools {
			t.Errorf("%s: tool_schemas %v, want %v (%v)", tc.name, got, tc.tools, input.Flags)
		}
	}
}

// TestADecoratedFunctionsResultIsNamedForWhatItHolds reads what a decorated
// function returned. It is the application's own as much as its arguments,
// and a function wrapping a model call can return the request it built.
func TestADecoratedFunctionsResultIsNamedForWhatItHolds(t *testing.T) {
	envelope := `{"id":"r1","trace_id":"t1","parent_run_id":"p1","run_type":"chain","name":"wrapped",` +
		`"start_time":"2026-09-20T10:46:45Z","end_time":"2026-09-20T10:46:46Z","inputs":{"question":"hi"},` +
		`"outputs":{"request":{"messages":[{"role":"system","content":"You are a helpful assistant."}],"tools":[{"name":"read"}]}},` +
		`"extra":{"metadata":{"ls_method":"traceable"}}}`
	records, err := Convert(Operation{Op: "post", RunID: "r1", Envelope: json.RawMessage(envelope)}, false, Hints{})
	if err != nil || len(records) == 0 {
		t.Fatalf("%v %v", records, err)
	}
	var flags []string
	for _, r := range records {
		flags = append(flags, r.Flags...)
	}
	if !slices.Contains(flags, sessiondata.FlagSystemPrompt) || !slices.Contains(flags, sessiondata.FlagToolSchemas) {
		t.Fatalf("the result is named %v", flags)
	}
}

// TestADecoratedFunctionIsNamedForWhatItHolds reads a decorated function's
// own values. They are the application's, not a request, so they are named
// only when they hold the shapes a function wrapping a model call is handed:
// a system message, a system prompt under a key a provider takes it by, and
// tools offered as objects. A list of names under tools is not a schema. A
// graph's or a function's own first input is read the same way.
func TestADecoratedFunctionIsNamedForWhatItHolds(t *testing.T) {
	for _, tc := range []struct {
		name          string
		root          bool
		inputs        string
		prompt, tools bool
	}{
		{"a wrapper handed messages and tools", false, `{"messages":[{"role":"system","content":"You are a helpful assistant."},{"role":"user","content":"hi"}],"tools":[{"name":"read"}]}`, true, true},
		{"a wrapper handed a person's message", false, `{"messages":[{"role":"user","content":"hi"}],"limit":3}`, false, false},
		{"a function asked a question", false, `{"question":"what changed in the docs"}`, false, false},
		{"a root function handed a system message", true, `{"prompt":[{"type":"system","content":"You are a helpful assistant."}],"question":"hi"}`, true, false},
		{"a root function handed a system prompt as a value", true, `{"system":"You are a helpful assistant.","question":"hi"}`, true, false},
		{"a wrapper handed a request as Anthropic takes it", false, `{"system":"You are a helpful assistant.","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"read","input_schema":{"type":"object"}}]}`, true, true},
		{"a wrapper handed a request as OpenAI's Responses take it", false, `{"instructions":"You are a helpful assistant.","input":"hi"}`, true, false},
		{"a wrapper handed tools in a configuration of Gemini's", false, `{"contents":[],"config":{"tools":[{"function_declarations":[{"name":"read"}]}]}}`, false, true},
		{"a question that names tools", true, `{"question":"which tools do I need","tools":["hammer","saw"]}`, false, false},
		{"documents that name functions", false, `{"docs":[{"metadata":{"functions":["billing"]}}]}`, false, false},
		{"a flag named tools", false, `{"tools":false,"system":""}`, false, false},
		{"a prompt under a JavaScript client's key", false, `{"systemPrompt":"You are a helpful assistant.","input":"hi"}`, true, false},
		{"a prompt under AutoGen's key", false, `{"system_message":"You are a helpful assistant."}`, true, false},
		{"a prompt as Cohere's preamble", false, `{"preamble":"You are a helpful assistant.","message":"hi"}`, true, false},
		{"a role written in capitals", false, `{"messages":[{"role":"SYSTEM","content":"You are a helpful assistant."}]}`, true, false},
		{"tools beside the name of a built-in one", false, `{"tools":[{"name":"read","input_schema":{}},"web_search"]}`, false, true},
		{"a message as LangChain's pair of role and content", false, `{"messages":[["system","You are a helpful assistant."],["human","hi"]]}`, true, false},
		{"a message of the developer role, beside a value of the function's own", false, `{"messages":[{"role":"developer","content":"You are a helpful assistant."}],"limit":3}`, true, false},
		{"a prompt as Gemini takes it", false, `{"contents":[],"system_instruction":{"parts":[{"text":"You are a helpful assistant."}]}}`, true, false},
		{"tools keyed by their names", false, `{"tools":{"read":{"description":"Read a file.","parameters":{"type":"object"}}}}`, false, true},
		{"a prompt as Bedrock's agents take it", false, `{"instruction":"You are a helpful assistant.","inputText":"hi"}`, true, false},
		{"a prompt under system_instructions", false, `{"system_instructions":"You are a helpful assistant."}`, true, false},
		{"tools as Gemini's function declarations", false, `{"function_declarations":[{"name":"read","parameters":{"type":"object"}}]}`, false, true},
		{"tools kept as definitions", false, `{"tool_definitions":[{"name":"read"}]}`, false, true},
		{"a pair of words of the application's own", false, `{"tags":["system","linux"],"user":{"roles":["developer","tester"]}}`, false, false},
		{"an event of the application's own", false, `{"events":[{"type":"system","message":"Alice joined"}]}`, false, false},
		{"a root function handed a system message with nothing in it", true, `{"prompt":[{"type":"system","content":""}],"question":"hi"}`, false, false},
		{"one pair alone in a message list", false, `{"messages":[["developer","You are a helpful assistant."]]}`, true, false},
	} {
		parent := `,"parent_run_id":"p1"`
		if tc.root {
			parent = ""
		}
		envelope := `{"id":"r1","trace_id":"t1"` + parent + `,"run_type":"chain","name":"wrapped","start_time":"2026-09-20T10:46:45Z","end_time":"2026-09-20T10:46:46Z","inputs":` + tc.inputs + `,"extra":{"metadata":{"ls_method":"traceable"}}}`
		records, err := Convert(Operation{Op: "post", RunID: "r1", Envelope: json.RawMessage(envelope)}, tc.root, Hints{})
		if err != nil || len(records) == 0 {
			t.Fatalf("%s: %v %v", tc.name, records, err)
		}
		var flags []string
		for _, r := range records {
			flags = append(flags, r.Flags...)
		}
		if got := slices.Contains(flags, sessiondata.FlagSystemPrompt); got != tc.prompt {
			t.Errorf("%s: system_prompt %v, want %v (%v)", tc.name, got, tc.prompt, flags)
		}
		if got := slices.Contains(flags, sessiondata.FlagToolSchemas); got != tc.tools {
			t.Errorf("%s: tool_schemas %v, want %v (%v)", tc.name, got, tc.tools, flags)
		}
	}
}
