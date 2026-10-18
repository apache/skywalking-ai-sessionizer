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

package assemble

import (
	"encoding/json"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// nestedReturn lands a session whose main agent starts the agent "p", which
// starts the agent "c" and waits for it: the result of p's call names c, and
// is not the acknowledgement of a background launch. c's sidecar names the
// agent that started it, which is sidecar.Parent, and the call, which is
// sidecar.Tool. It returns the value c's output says it returned, and whether
// a result_of relation leads from that output to p's call.
//
// It is a Go test because a scenario's Claude Code writer would have to make
// up the shape: in a local root of 2026-10-18, no record of a Claude Code
// sub-agent's transcript named a child, and each of the 31 nested children was
// named by its sidecar alone or with a notification. Session Data can carry
// it, so assembly must read it.
func nestedReturn(t *testing.T, sidecar sidecarNames) (returned any, resultOf bool) {
	t.Helper()
	f := newHfix(t)
	start := func(stream, run, tool, child string) {
		f.land(transcriptOf(stream), &sessiondata.Record{ID: tool + "-use", From: sessiondata.FromAgent,
			Call: tool + "-call", Run: run, Flags: []string{"finished"},
			Parts: []sessiondata.Part{{Kind: sessiondata.PartCall, ID: tool, Name: "Agent", State: "available"}}})
		f.land(transcriptOf(stream), &sessiondata.Record{ID: tool + "-result", From: sessiondata.FromExternal,
			Run: run, Child: child,
			Parts: []sessiondata.Part{{Kind: sessiondata.PartResult, Of: tool, Text: "done", State: "available"}}})
	}
	meta := func(stream string, names sidecarNames) {
		f.land(&sessiondata.Header{Kind: sessiondata.KindAgentMeta, Session: "s", Stream: stream},
			&sessiondata.Record{Child: stream, StartedBy: names.Parent, Tool: names.Tool, From: sessiondata.FromRuntime})
	}
	f.input("main", "r1")
	start("main", "r1", "t1", "p")
	meta("p", sidecarNames{})
	f.input("p", "rp")
	start("p", "rp", "t2", "c")
	f.input("c", "rc")
	f.call("c", "rc", "c-call")
	meta("c", sidecar)

	res, err := Session(f.ix, Options{Conversation: "s", Session: "s"})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range res.Nodes {
		if n.ID == "output/c" {
			var a map[string]any
			_ = json.Unmarshal(n.Attrs, &a)
			returned = a["returned_value"]
		}
	}
	for _, r := range res.Relations {
		if r.Type == model.RelResultOf && r.From == "output/c" && r.To == "tool/t2" {
			resultOf = true
		}
	}
	return returned, resultOf
}

// sidecarNames is what a child's sidecar names: the agent that started the
// child, and the call that did.
type sidecarNames struct{ Parent, Tool string }

// TestANestedChildReturnsToTheCallThatStartedIt. A sidecar that names the agent
// which started a nested child names no call, so it does not compete with the
// call whose result names the child. Counted as a second call, it took the
// output's result_of away and made its returned value unavailable, though one
// call started the child and received what it returned. A sidecar that also
// names that call is the same call again. A sidecar that names another agent
// than the one that made the call disagrees with it, and then nothing says
// which of them started the child, whether that agent's stream landed or not.
func TestANestedChildReturnsToTheCallThatStartedIt(t *testing.T) {
	for _, c := range []struct {
		sidecar sidecarNames
		returns bool
	}{
		{sidecarNames{}, true},
		{sidecarNames{Parent: "p"}, true},
		{sidecarNames{Parent: "p", Tool: "t2"}, true},
		{sidecarNames{Parent: "main"}, false},
		{sidecarNames{Parent: "missing"}, false},
	} {
		returned, resultOf := nestedReturn(t, c.sidecar)
		want := model.Unavailable
		if c.returns {
			want = model.ObservedReplayable
		}
		if returned != want || resultOf != c.returns {
			t.Errorf("sidecar naming %+v: returned_value %v, result_of %v; want %v and %v",
				c.sidecar, returned, resultOf, want, c.returns)
		}
	}
}
