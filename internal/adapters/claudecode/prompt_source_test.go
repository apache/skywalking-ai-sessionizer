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
	"encoding/json"
	"slices"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// TestAPromptSentThroughTheSDK reads the record shapes measured on the local
// corpus. Only a prompt with no origin and no isMeta is a caller's: the SDK
// also writes promptSource on notifications, which keep their origin, and on
// a harness notice, which is meta and is not something anyone asked.
func TestAPromptSentThroughTheSDK(t *testing.T) {
	for _, tc := range []struct {
		name     string
		line     string
		external bool
		trigger  index.Trigger
	}{
		{"headless prompt", `{"type":"user","promptSource":"sdk","message":{"role":"user","content":"read the README"}}`,
			true, index.TriggerExternal},
		{"notification through the SDK", `{"type":"user","promptSource":"sdk","origin":{"kind":"task-notification"},"message":{"role":"user","content":"<task-notification>"}}`,
			false, index.TriggerNotification},
		{"harness notice through the SDK", `{"type":"user","promptSource":"sdk","isMeta":true,"message":{"role":"user","content":"[Cross-session idle notice]"}}`,
			false, index.TriggerNone},
		{"prompt a person typed in VS Code", `{"type":"user","promptSource":"sdk","origin":{"kind":"human"},"message":{"role":"user","content":"hello"}}`,
			true, index.TriggerExternal},
		{"no promptSource and no origin", `{"type":"user","message":{"role":"user","content":"Continue from where you left off."}}`,
			false, index.TriggerNone},
	} {
		var d indexRecord
		if err := json.Unmarshal([]byte(tc.line), &d); err != nil {
			t.Fatal(err)
		}
		if got := flagsOf(&d, nil, false, Source{}).Has(index.FlagExternalInput); got != tc.external {
			t.Errorf("%s: external input %v, want %v", tc.name, got, tc.external)
		}
		if got := triggerOf(&d); got != tc.trigger {
			t.Errorf("%s: trigger %v, want %v", tc.name, got, tc.trigger)
		}
	}
}

// TestAnAttachmentIsNamedForWhatItCarries reads the snapshot and deferred
// tools shapes measured on Claude Code and an Agent SDK runtime, and the
// shapes a later version could change them to. A reader withholds by these
// names, so a snapshot is named for the prompt whatever keys it holds, and
// tools of any shape name the schemas: in a snapshot under a key that says
// tool, and in a deferred tools record as a list or an object under any key
// but the type and the input copies.
func TestAnAttachmentIsNamedForWhatItCarries(t *testing.T) {
	for _, tc := range []struct {
		name          string
		attachment    string
		prompt, tools bool
	}{
		{"the prompt alone, as measured", `{"type":"prompt_snapshot","reminderFold":false,"systemPrompt":["You are a helpful assistant."]}`, true, false},
		{"the prompt and the schemas, as measured", `{"type":"prompt_snapshot","cliPrefix":"You are a Claude agent.","systemPrompt":["You are a helpful assistant."],"tools":[{"name":"Read","description":"Read a file.","schema":{"type":"object"}}]}`, true, true},
		{"the settings beside the prompt, as measured", `{"type":"prompt_snapshot","systemPrompt":["You are a helpful assistant."],"inlineTools":false,"systemTurns":true,"contextRendering":"announced"}`, true, false},
		{"the prompt as one string", `{"type":"prompt_snapshot","systemPrompt":"You are a helpful assistant."}`, true, false},
		{"the prompt under a key not known", `{"type":"prompt_snapshot","content":"You are a helpful assistant."}`, true, false},
		{"the schemas keyed by name", `{"type":"prompt_snapshot","systemPrompt":["You are a helpful assistant."],"tools":{"Read":{"type":"object"}}}`, true, true},
		{"the schemas under a renamed key", `{"type":"prompt_snapshot","systemPrompt":["You are a helpful assistant."],"toolSchemas":[{"name":"Read"}]}`, true, true},
		{"the schemas written as one string", `{"type":"prompt_snapshot","systemPrompt":["You are a helpful assistant."],"tools":"[{\"name\":\"Read\"}]"}`, true, true},
		{"the settings that name tools, as measured", `{"type":"prompt_snapshot","systemPrompt":["You are a helpful assistant."],"inlineTools":false,"echoWireToolInputs":true,"toolChangeHeader":true}`, true, false},
		{"empty values with white space inside", `{"type":"prompt_snapshot","systemPrompt":[ ],"tools":{ }}`, true, false},
		{"the deferred tools, as measured", `{"type":"deferred_tools_record","entries":[{"name":"WebFetch","description":"Fetch a page.","input_schema":{"type":"object"},"defer_loading":true}],"toolInputCopies":[]}`, false, true},
		{"no deferred tools, as most are", `{"type":"deferred_tools_record","entries":[],"toolInputCopies":[{"id":"t1","copy":"{}"}]}`, false, false},
		{"the deferred tools under a renamed key", `{"type":"deferred_tools_record","tools":[{"name":"WebFetch","input_schema":{"type":"object"}}],"toolInputCopies":[]}`, false, true},
		{"the tools delta, names only", `{"type":"deferred_tools_delta","addedNames":["WebFetch"],"addedLines":["WebFetch"]}`, false, false},
		{"the keys on another type", `{"type":"agent_listing_delta","systemPrompt":["not a snapshot"],"tools":[{"name":"Read"}],"entries":[{"name":"Read"}]}`, false, false},
	} {
		payload := []byte(`{"type":"attachment","attachment":` + tc.attachment + `}`)
		var d indexRecord
		if err := json.Unmarshal(payload, &d); err != nil {
			t.Fatal(err)
		}
		if !flagsOf(&d, nil, false, Source{}).Has(index.FlagInjection) {
			t.Errorf("%s: not an injection", tc.name)
		}
		names := sentFlags(&d, payload)
		if got := slices.Contains(names, sessiondata.FlagSystemPrompt); got != tc.prompt {
			t.Errorf("%s: system_prompt %v, want %v", tc.name, got, tc.prompt)
		}
		if got := slices.Contains(names, sessiondata.FlagToolSchemas); got != tc.tools {
			t.Errorf("%s: tool_schemas %v, want %v", tc.name, got, tc.tools)
		}
	}
}

// TestADamagedLineIsStillNamed reads lines that do not decode. Their bytes
// land whole, as every such line's do. One whose bytes name the snapshot type
// is named for both things a snapshot can carry, one naming the deferred
// tools type for the schemas, and a damaged line of any other type for
// nothing.
func TestADamagedLineIsStillNamed(t *testing.T) {
	for _, tc := range []struct {
		name, line string
		want       []string
	}{
		{"a snapshot", `{"type":"attachment","uuid":"u1","attachment":{"type":"prompt_snapshot","systemPrompt":["You are a helpful assistant.\q"]}}`,
			[]string{sessiondata.FlagSystemPrompt, sessiondata.FlagToolSchemas}},
		{"deferred tools", `{"type":"attachment","uuid":"u2","attachment":{"type":"deferred_tools_record","entries":[{"name":"\q"}]}}`,
			[]string{sessiondata.FlagToolSchemas}},
		{"another type", `{"type":"attachment","uuid":"u3","attachment":{"type":"todo_reminder","content":"\q"}}`, nil},
	} {
		rec := Convert(Source{}, 1, 0, []byte(tc.line))
		if len(rec.Parts) != 1 || rec.Parts[0].Kind != sessiondata.PartUnknown {
			t.Fatalf("%s: the damaged line did not land whole: %+v", tc.name, rec.Parts)
		}
		if !slices.Equal(rec.Flags, tc.want) {
			t.Errorf("%s: named %v, want %v", tc.name, rec.Flags, tc.want)
		}
	}
}
