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

package scenario

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// planOf writes a scenario and resolves it, so a test reads the bodies the
// build would write.
func planOf(t *testing.T, body string) *Plan {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	sc, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p, err := sc.Plan(Options{})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return p
}

// requestsOf reads every request body of a plan, decoded.
func bodiesOf(t *testing.T, p *Plan) (requests, responses []map[string]any) {
	t.Helper()
	for _, b := range p.ProviderBodies() {
		var v map[string]any
		if err := json.Unmarshal(b.Bytes, &v); err != nil {
			t.Fatalf("%s: %v", b.Name, err)
		}
		if strings.Contains(b.Name, ".request.") {
			requests = append(requests, v)
		} else {
			responses = append(responses, v)
		}
	}
	return requests, responses
}

func systemTextOf(t *testing.T, req map[string]any) string {
	t.Helper()
	blocks, ok := req["system"].([]any)
	if !ok || len(blocks) < 2 {
		t.Fatalf("system is %v", req["system"])
	}
	// The first block is the billing header; the prompt is the second.
	return blocks[1].(map[string]any)["text"].(string)
}

func toolNamesOf(t *testing.T, req map[string]any) []string {
	t.Helper()
	// Never absent and never null: a stream that advertises nothing still
	// sends the field, as an empty list.
	list, ok := req["tools"].([]any)
	if !ok {
		t.Fatalf("tools is %v, not a list", req["tools"])
	}
	out := []string{}
	for _, v := range list {
		out = append(out, v.(map[string]any)["name"].(string))
	}
	return out
}

// TestAStreamSendsTheToolsItsScenarioWrote. A request body is read as a
// document, so the tools it advertises have to be the ones that agent has.
// Before this, every scenario advertised Read and Bash whatever it did,
// which drew a calendar assistant as a coding agent.
func TestAStreamSendsTheToolsItsScenarioWrote(t *testing.T) {
	p := planOf(t, `provider_bodies: true
system_prompt: You schedule meetings.
tools:
  - name: calendar_freebusy
    description: Returns busy intervals.
  - name: Agent
    description: Starts a subagent.
steps:
  - input: find a slot
  - call:
      text: Delegating.
      agent:
        name: checker
        system_prompt: You check availability.
        tools:
          - name: contacts_lookup
            description: Resolves names to addresses.
        prompt: check the four of them
        steps:
          - call: {text: 14:00 works.}
  - call: {text: 14:00 works for everyone.}
`)
	requests, _ := bodiesOf(t, p)
	var sawMain, sawChild bool
	for _, req := range requests {
		switch systemTextOf(t, req) {
		case "You schedule meetings.":
			sawMain = true
			if got := strings.Join(toolNamesOf(t, req), ","); got != "calendar_freebusy,Agent" {
				t.Errorf("main stream advertises %s", got)
			}
		case "You check availability.":
			sawChild = true
			// A child is started to carry a narrower set, so it must not
			// inherit its parent's.
			if got := strings.Join(toolNamesOf(t, req), ","); got != "contacts_lookup" {
				t.Errorf("child advertises %s", got)
			}
		default:
			t.Errorf("unexpected system prompt %q", systemTextOf(t, req))
		}
	}
	if !sawMain || !sawChild {
		t.Errorf("main %v, child %v", sawMain, sawChild)
	}
}

// TestAScenarioThatWritesNoneKeepsTheStandIn. The stand-in is filler sized
// to make a body worth cutting, and the provider-bodies scenarios pin a file
// count against it. Losing it would change those counts silently.
func TestAScenarioThatWritesNoneKeepsTheStandIn(t *testing.T) {
	p := planOf(t, `provider_bodies: true
steps:
  - input: do it
  - call: {text: Done.}
`)
	requests, _ := bodiesOf(t, p)
	if len(requests) == 0 {
		t.Fatal("no requests")
	}
	if got := toolNamesOf(t, requests[0]); strings.Join(got, ",") != "Read,Bash" {
		t.Errorf("stand-in advertises %v", got)
	}
	if n := len(systemTextOf(t, requests[0])); n < 1024 {
		t.Errorf("stand-in system prompt is %d bytes, too small to cut", n)
	}
}

// TestAResponseCarriesTheUsageTheScenarioDeclared. The transcript records a
// call's usage and the response body reports it; a reader comparing the two
// must see one number. The body carried none at all before.
func TestAResponseCarriesTheUsageTheScenarioDeclared(t *testing.T) {
	p := planOf(t, `provider_bodies: true
steps:
  - input: do it
  - call:
      text: Done.
      usage: {in: 3800, out: 150, cache_read: 14000, cache_write: 2900}
`)
	_, responses := bodiesOf(t, p)
	if len(responses) != 1 {
		t.Fatalf("%d responses", len(responses))
	}
	usage, ok := responses[0]["usage"].(map[string]any)
	if !ok {
		t.Fatalf("no usage: %v", responses[0])
	}
	for field, want := range map[string]float64{
		"input_tokens": 3800, "output_tokens": 150,
		"cache_read_input_tokens": 14000, "cache_creation_input_tokens": 2900,
	} {
		if usage[field] != want {
			t.Errorf("%s is %v, want %v", field, usage[field], want)
		}
	}
}

// TestTheSameScenarioWritesTheSameBytesTwice. Every digest in the round
// chain binds these bytes, so a build that varied would break verification
// on the second pass rather than the first.
func TestTheSameScenarioWritesTheSameBytesTwice(t *testing.T) {
	body := `provider_bodies: true
system_prompt: You schedule meetings.
tools:
  - name: calendar_freebusy
    description: Returns busy intervals.
    input_schema:
      type: object
      properties:
        time_min: {type: string}
steps:
  - input: find a slot
  - call: {text: 14:00 works.}
`
	first := planOf(t, body).ProviderBodies()
	second := planOf(t, body).ProviderBodies()
	// The bytes as they would be written, not a decoded copy: key order,
	// escaping and number formatting all reach the digest, and a comparison
	// that decodes first can see none of them. Requests and responses both.
	if len(first) == 0 {
		t.Fatal("no bodies, so this would pass on anything")
	}
	if len(first) != len(second) {
		t.Fatalf("%d then %d bodies", len(first), len(second))
	}
	for i := range first {
		if first[i].Name != second[i].Name {
			t.Errorf("body %d is %s then %s", i, first[i].Name, second[i].Name)
			continue
		}
		if !bytes.Equal(first[i].Bytes, second[i].Bytes) {
			t.Errorf("%s differs:\n%s\n%s", first[i].Name, first[i].Bytes, second[i].Bytes)
		}
	}
}

// TestAValueThatCannotBeWrittenIsRefused. YAML writes an infinity and a NaN
// as plain scalars, and the JSON encoder refuses both. The body writer cannot
// report that, so such a value used to leave an empty body and surface much
// later as a digest that did not match.
func TestAValueThatCannotBeWrittenIsRefused(t *testing.T) {
	for name, body := range map[string]string{
		"a tool's input schema": `provider_bodies: true
system_prompt: You ping things.
tools:
  - name: ping
    description: Pings.
    input_schema: {type: object, timeout: .inf}
steps:
  - input: go
  - call: {text: Done.}
`,
		"a tool's input": `steps:
  - input: go
  - call: {text: Done., tool: {id: t1, name: Bash, input: {timeout: .nan}}}
`,
	} {
		path := filepath.Join(t.TempDir(), "s.yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(path)
		if err == nil {
			t.Errorf("%s: loaded without complaint", name)
			continue
		}
		if !strings.Contains(err.Error(), "cannot be written as JSON") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestAToolNeedsNoDescription. A runtime may declare a tool without one, and
// a scenario has to be able to write what a runtime sends.
func TestAToolNeedsNoDescription(t *testing.T) {
	p := planOf(t, `provider_bodies: true
system_prompt: You ping things.
tools:
  - name: ping
steps:
  - input: go
  - call: {text: Done.}
`)
	requests, _ := bodiesOf(t, p)
	if len(requests) == 0 {
		t.Fatal("no requests")
	}
	if got := toolNamesOf(t, requests[0]); strings.Join(got, ",") != "ping" {
		t.Errorf("advertises %v", got)
	}
}

// TestAStreamMustAdvertiseWhatItCalls. The tools array is sent on every
// request of a stream, whatever that request goes on to call, so a model can
// never call a tool the request did not offer it. A scenario that writes its
// tools has to write all of them, and starting a child is a call like any
// other: the response carries a tool_use block named Agent, Skill or Workflow.
func TestAStreamMustAdvertiseWhatItCalls(t *testing.T) {
	for name, body := range map[string]string{
		"a tool": `system_prompt: You work in a repository.
tools: [{name: Read, description: Reads a file.}]
steps:
  - input: go
  - call: {text: Running., tool: {id: t1, name: Bash, input: {command: ls}}}
`,
		"a child": `system_prompt: You work in a repository.
tools: [{name: Read, description: Reads a file.}]
steps:
  - input: go
  - call:
      agent: {name: helper, prompt: help, steps: [{call: {text: Done.}}]}
`,
	} {
		path := filepath.Join(t.TempDir(), "s.yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "not advertised") {
			t.Errorf("%s: %v", name, err)
		}
	}

	// A stream that writes no tools keeps the stand-in and is not checked.
	planOf(t, `steps:
  - input: go
  - call: {text: Running., tool: {id: t1, name: Bash, input: {command: ls}}}
`)
}

// TestWrittenAndEmptyIsNotUnwritten. A scenario may say its agent advertises
// no tools at all, which is a request a runtime sends. Written-and-empty used
// to fall back to the stand-in's Read and Bash, so that agent could not be
// described.
func TestWrittenAndEmptyIsNotUnwritten(t *testing.T) {
	p := planOf(t, `provider_bodies: true
system_prompt: You answer from what you were given.
tools: []
steps:
  - input: go
  - call: {text: Done.}
`)
	requests, _ := bodiesOf(t, p)
	if len(requests) == 0 {
		t.Fatal("no requests")
	}
	if got := toolNamesOf(t, requests[0]); len(got) != 0 {
		t.Errorf("a tool-free agent advertises %v", got)
	}
	// The field is sent as an empty list, not as null.
	if body := p.ProviderBodies()[0].Bytes; !bytes.Contains(body, []byte(`"tools":[]`)) {
		t.Errorf("tools is not an empty list on the wire: %s", body)
	}
	if got := systemTextOf(t, requests[0]); got != "You answer from what you were given." {
		t.Errorf("system prompt is %q", got)
	}
}

// TestHalfADeclarationIsRefused. Each field falls back on its own, so half a
// declaration would leave an authored prompt beside the stand-in's Read and
// Bash -- the very thing writing them is meant to stop.
func TestHalfADeclarationIsRefused(t *testing.T) {
	for name, body := range map[string]string{
		"prompt without tools": "system_prompt: You work alone.\nsteps:\n  - input: go\n  - call: {text: Done.}\n",
		"tools without prompt": "tools: [{name: Read, description: Reads a file.}]\nsteps:\n  - input: go\n  - call: {text: Done.}\n",
	} {
		path := filepath.Join(t.TempDir(), "s.yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "both or neither") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
