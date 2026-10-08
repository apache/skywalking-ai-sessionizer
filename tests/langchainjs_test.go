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

package tests_test

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// landedLine is what these tests read of a landed record.
type landedLine struct {
	raw   string
	Flags []string `json:"flags"`
	Parts []struct {
		K    string `json:"k"`
		Text string `json:"text"`
	} `json:"parts"`
}

// transcriptLines is every record a session landed on its streams, the
// provider bodies left out.
func transcriptLines(t *testing.T, zone *storage.Zone, session string) []landedLine {
	t.Helper()
	files, err := storage.LandedFiles(zone, session)
	if err != nil {
		t.Fatal(err)
	}
	var out []landedLine
	for _, f := range files {
		if f.Stream == "" {
			continue
		}
		raw, err := os.ReadFile(f.Path)
		if err != nil {
			t.Fatal(err)
		}
		lines := bytes.Split(raw, []byte("\n"))
		for _, line := range lines[1:] {
			if len(line) == 0 || bytes.HasPrefix(line, []byte(`{"t":"end"`)) {
				continue
			}
			l := landedLine{raw: string(line)}
			if err := json.Unmarshal(line, &l); err != nil {
				t.Fatal(err)
			}
			out = append(out, l)
		}
	}
	return out
}

// bodiesHold reports whether a provider body file of a session holds a text.
func bodiesHold(t *testing.T, zone *storage.Zone, session, text string) bool {
	t.Helper()
	files, err := storage.LandedFiles(zone, session)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Stream != "" || f.RunID != "" {
			continue
		}
		raw, err := os.ReadFile(f.Path)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(text)) {
			return true
		}
	}
	return false
}

// TestALangGraphJSConversationLandsWhatWasSaid. LangGraph JS serializes every
// message as a constructor whose kind is named only by the class that ends
// its id, with its fields under kwargs. Read by role and type alone, as the
// Python client writes them, the person's question was never found in the
// graph's first input, so nothing opened the conversation, and the tool's
// result landed as the serialized message rather than as what the tool said.
// Measured on a capture of a LangGraph JS agent with one tool.
func TestALangGraphJSConversationLandsWhatWasSaid(t *testing.T) {
	zone, sessions := land(t, "js-graph")
	if len(sessions) != 1 {
		t.Fatalf("%d sessions, want the one thread: %v", len(sessions), sessions)
	}
	var question, result string
	for _, l := range transcriptLines(t, zone, sessions[0]) {
		for _, p := range l.Parts {
			switch {
			case slices.Contains(l.Flags, "external_input") && p.K == string(sessiondata.PartText):
				question = p.Text
			case p.K == string(sessiondata.PartResult):
				result = p.Text
			}
		}
		// The graph's first input holds the system message before the
		// question. Only the question is lifted from it, and the graph's own
		// runs repeat what the model call was sent, so no record on the
		// streams holds the system message at all.
		if strings.Contains(l.raw, "You are a docs assistant") {
			t.Errorf("a record on the streams holds the system prompt: %.200s", l.raw)
		}
	}
	if !bodiesHold(t, zone, sessions[0], "You are a docs assistant") {
		t.Error("no provider body holds the system prompt, so the capture does not test where it lands")
	}
	if question != "what changed in the setup page" {
		t.Errorf("the question landed as %q", question)
	}
	if result != "The setup page gained a section on hiding." {
		t.Errorf("the tool's result landed as %q", result)
	}
}

// TestALangChainJSTracedFunctionKeepsItsArguments. LangSmith JS writes no
// ls_method on a decorated function. It says, as the Python client does, that
// the LangSmith client made the run, in extra.runtime.library. Read by
// ls_method alone, the function was taken for a graph's own run and its
// arguments and result were dropped as a repeat; its system message is then
// named where it lands. LangSmith JS also passes the function's metadata to
// none of the runs inside it, so the model call it made supplies no thread,
// and lands under its trace, as any run that supplied none does.
func TestALangChainJSTracedFunctionKeepsItsArguments(t *testing.T) {
	zone, sessions := land(t, "js-traceable")
	var thread, unassigned string
	for _, s := range sessions {
		if strings.HasPrefix(s, "ls-unassigned-") {
			unassigned = s
		} else {
			thread = s
		}
	}
	if thread == "" || unassigned == "" || len(sessions) != 2 {
		t.Fatalf("sessions %v, want the thread's and the model call's own", sessions)
	}
	kept := false
	for _, l := range transcriptLines(t, zone, thread) {
		if strings.Contains(l.raw, "You are a careful reviewer.") {
			kept = strings.Contains(l.raw, `"limit"`)
			if !slices.Contains(l.Flags, sessiondata.FlagSystemPrompt) {
				t.Errorf("the function's arguments hold the system prompt unnamed: %.200s", l.raw)
			}
		}
	}
	if !kept {
		t.Error("the function's arguments were not kept")
	}
	answered := false
	for _, l := range transcriptLines(t, zone, unassigned) {
		for _, p := range l.Parts {
			answered = answered || p.Text == "Two pages changed: the setup page and the formats page."
		}
	}
	if !answered {
		t.Error("the model call's reply did not land")
	}
}
