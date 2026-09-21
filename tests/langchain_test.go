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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecodechanges"
	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/langsmith"
	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/verify"
	view_ "github.com/apache/skywalking-ai-sessionizer/internal/view"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// corpus is what the LangChain demo application captured: what one real client
// sent, over the wire, for each shape a conversation can take.
const corpus = "apps/langchain/testdata"

// TestALangChainConversationAssembles carries a real capture the whole way:
// over HTTP into the receiver, out of the inbox into landed files, through the
// parser into a round chain, and past the verifier.
//
// It is the test that would have caught every mistake found while this was
// being designed, because none of them showed up until a conversation was
// actually built.
func TestALangChainConversationAssembles(t *testing.T) {
	for _, tc := range []struct {
		kase    string
		talks   int
		tools   int
		streams int
	}{
		{"plain", 1, 0, 1},
		{"three-turns", 3, 1, 1},
		{"tool-error", 1, 1, 1},
		{"parallel-tools", 1, 2, 1},
		{"loop", 1, 5, 1},
		// One of this case's three tools ran an agent of its own, one made a
		// single model call with a prompt of its own, and one is a leaf. The
		// first is an agent call; the other two are tool steps. All three
		// contexts get a stream and a talk, because neither nested prompt
		// continues the caller's.
		{"subagent", 3, 2, 3},
		// A decorated function with no graph and no model call. It is a
		// conversation all the same: a question came in, an answer went
		// out. Its tool makes no step, because a traced function calls its
		// tools itself and nothing on the wire carries the call - see
		// TestATracedToolsCallIsNotInvented.
		{"traceable-only", 1, 0, 1},
	} {
		t.Run(tc.kase, func(t *testing.T) {
			zone, sessions := land(t, tc.kase)
			if len(sessions) != 1 {
				t.Fatalf("%s landed %d sessions, want 1", tc.kase, len(sessions))
			}
			session := sessions[0]
			round, err := parse.Session(zone, parse.Options{
				Conversation: session, Session: session, Reindex: index.Rebuild})
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if round == nil {
				t.Fatal("no round was written")
			}
			view := fold(t, zone, session)
			counts := map[string]int{}
			for _, node := range view.Nodes {
				counts[node.Kind]++
			}
			if got := counts[model.KindTalk]; got != tc.talks {
				t.Errorf("%d talks, want %d", got, tc.talks)
			}
			if got := counts[model.KindTool]; got != tc.tools {
				t.Errorf("%d tool steps, want %d", got, tc.tools)
			}
			if got := counts[model.KindStream]; got != tc.streams {
				t.Errorf("%d streams, want %d", got, tc.streams)
			}
			if counts[model.KindLLMCall] == 0 && tc.kase != "traceable-only" {
				t.Error("no model call")
			}
			if counts[model.KindMessageExternal] == 0 {
				t.Error("nothing opened the conversation")
			}
			if len(view.Unresolved) != 0 {
				for _, u := range view.Unresolved {
					t.Errorf("unresolved %s: %s", u.Kind, u.Reason)
				}
			}
			report, err := verify.Session(zone, session)
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			if report.Problems != 0 {
				t.Errorf("verify found %d problems", report.Problems)
			}
			t.Logf("%s: %d nodes, %d talks, %d model calls, %d tool steps, %d relations",
				tc.kase, len(view.Nodes), counts[model.KindTalk], counts[model.KindLLMCall],
				counts[model.KindTool], len(view.Relations))
		})
	}
}

// TestTheHumanMessageOpensTheTurn: the message that began a turn lives inside
// the root run's inputs and has no run of its own, so the dialect has to lift
// it out. Without the trigger and the flag it lands as evidence that makes no
// node, and the conversation reads as one that never began.
func TestTheHumanMessageOpensTheTurn(t *testing.T) {
	zone, sessions := land(t, "three-turns")
	session := sessions[0]
	if _, err := parse.Session(zone, parse.Options{
		Conversation: session, Session: session, Reindex: index.Rebuild}); err != nil {
		t.Fatal(err)
	}
	view := fold(t, zone, session)
	inputs := 0
	for _, node := range view.Nodes {
		if node.Kind == model.KindMessageExternal {
			inputs++
		}
	}
	if inputs != 3 {
		t.Fatalf("%d human messages over three turns, want 3", inputs)
	}
}

// TestUnfinishedWorkIsAConversationToo: a process that died mid-turn leaves
// runs nothing will ever complete. That is evidence, and it has to assemble
// and verify like any other — a conversation that is incomplete, not one that
// is broken.
func TestUnfinishedWorkIsAConversationToo(t *testing.T) {
	zone, sessions := land(t, "abandoned-run")
	session := sessions[0]
	if _, err := parse.Session(zone, parse.Options{
		Conversation: session, Session: session, Reindex: index.Rebuild}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	report, err := verify.Session(zone, session)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if report.Problems != 0 {
		t.Errorf("verify found %d problems in a conversation that was cut short", report.Problems)
	}
	view := fold(t, zone, session)
	t.Logf("a turn that never finished: %d nodes, %d unresolved",
		len(view.Nodes), len(view.Unresolved))
}

// land replays one case's captured requests through the receiver and the
// collector, and returns the root and the sessions they landed in.
func land(t *testing.T, kase string) (*storage.Zone, []string) {
	t.Helper()
	zone := storage.NewZone(t.TempDir())
	at := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	receiver := &langsmith.Receiver{Zone: zone, Listen: "127.0.0.1:0",
		Now: func() time.Time { at = at.Add(time.Millisecond); return at }}
	if err := receiver.Start(); err != nil {
		t.Fatalf("receiver: %v", err)
	}
	defer receiver.Stop()

	dir := filepath.Join(corpus, kase)
	requests, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("no corpus for %s: %v", kase, err)
	}
	for _, r := range requests {
		raw, err := os.ReadFile(filepath.Join(dir, r.Name(), "meta.json"))
		if err != nil {
			continue
		}
		var meta struct {
			Headers map[string]string `json:"headers"`
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(filepath.Join(dir, r.Name(), "body.bin"))
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(http.MethodPost,
			"http://"+receiver.Addr()+"/runs/multipart", bytes.NewReader(body))
		req.Header.Set("Content-Type", meta.Headers["Content-Type"])
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", r.Name(), err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("%s: status %d", r.Name(), resp.StatusCode)
		}
	}
	collected := time.Date(2026, 9, 20, 11, 0, 0, 0, time.UTC)
	collector := &langsmith.Collector{Zone: zone, ProviderBodies: true,
		Now: func() time.Time { collected = collected.Add(time.Second); return collected }}
	landed, err := collector.Collect()
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	return zone, landed.Sessions
}

// fold reads the conversation back out of its rounds.
func fold(t *testing.T, zone *storage.Zone, session string) *sessionflow.View {
	t.Helper()
	view, err := sessionflow.OpenChain(zone.Root(), session).Fold()
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	return view
}

// TestANestedAgentGetsItsOwnStream is the property the flattened first
// version got wrong.
//
// A tool whose implementation runs an agent of its own produces model calls
// that continue nothing in the caller's message list. Landing them in main
// makes a stream whose calls do not follow from one another, which is what a
// stream is defined to rule out, and the reader sees one agent doing work
// that two did.
//
// The capture this runs on has three tools. Two ran something: one a whole
// second graph, one a single model call. The third is a leaf, inside the
// second graph. So the conversation has to come out as one main stream, two
// child streams, two agent calls that start them, and one tool step that
// stays a tool step - and the leaf has to sit in the child, not in main.
func TestANestedAgentGetsItsOwnStream(t *testing.T) {
	zone, sessions := land(t, "subagent")
	session := sessions[0]
	if _, err := parse.Session(zone, parse.Options{
		Conversation: session, Session: session, Reindex: index.Rebuild}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	view := fold(t, zone, session)

	streams := map[string]string{} // node id -> role
	for _, n := range view.Nodes {
		if n.Kind != model.KindStream {
			continue
		}
		var a struct {
			Role string `json:"role"`
		}
		if err := json.Unmarshal(n.Attrs, &a); err != nil {
			t.Fatal(err)
		}
		streams[n.ID] = a.Role
	}
	var children []string
	for id, role := range streams {
		if role != storage.StreamMain {
			children = append(children, id)
		}
	}
	sort.Strings(children)
	if len(children) != 2 {
		t.Fatalf("%d nested streams, want 2: %v", len(children), children)
	}

	// Each child has to be reached from the call that started it. A child
	// nothing names is an orphan, and assembly says so rather than guessing.
	started := map[string]bool{}
	for _, r := range view.Relations {
		if r.Type == model.RelStarts {
			started[r.To] = true
		}
	}
	for _, id := range children {
		if !started[id] {
			t.Errorf("%s is a child stream that no call starts", id)
		}
	}

	// The nested work belongs to the child. A model call of the second graph
	// appearing under main is the flattening this test exists to catch.
	byStream := map[string]int{}
	for _, n := range view.Nodes {
		if n.Kind == model.KindLLMCall {
			byStream[n.Stream]++
		}
	}
	// Three top-level turns in the outer graph, two in the nested one, and
	// one the single-call tool made.
	if byStream[storage.StreamMain] != 3 {
		t.Errorf("%d model calls in main, want 3", byStream[storage.StreamMain])
	}
	nested := 0
	for s, n := range byStream {
		if s != storage.StreamMain {
			nested += n
		}
	}
	if nested != 3 {
		t.Errorf("%d model calls in child streams, want 3", nested)
	}
	if len(view.Unresolved) != 0 {
		for _, u := range view.Unresolved {
			t.Errorf("unresolved %s: %s", u.Kind, u.Reason)
		}
	}
}

// TestAConversationHasAName.
//
// A list of conversations with no names in it is a list of identifiers.
// Nothing on this wire carries a name: the client names runs - "LangGraph",
// "agent", "should_continue" - and those name the program, so every
// conversation would be called the same thing.
//
// What a reader recognises is what was asked. So the name is the first
// question of the conversation, and when it began without words - a traced
// function called with arguments - the name of the run that started it.
//
// It is landed once. The fold takes the last name it finds, so naming every
// turn would change a conversation's name under a reader each time it
// answered.
func TestAConversationHasAName(t *testing.T) {
	for _, tc := range []struct{ kase, want string }{
		{"three-turns", "Is prod-1 healthy?"},
		{"subagent", "Is prod-1 healthy?"},
		// No messages at all: a decorated function, called with arguments.
		{"traceable-only", "troubleshoot"},
	} {
		t.Run(tc.kase, func(t *testing.T) {
			zone, sessions := land(t, tc.kase)
			session := sessions[0]
			if _, err := parse.Session(zone, parse.Options{
				Conversation: session, Session: session, Reindex: index.Rebuild}); err != nil {
				t.Fatal(err)
			}
			view := fold(t, zone, session)
			var got struct {
				Title   string `json:"title"`
				Through string `json:"through_time"`
			}
			for _, n := range view.Nodes {
				if n.Kind == model.KindSession {
					if err := json.Unmarshal(n.Attrs, &got); err != nil {
						t.Fatal(err)
					}
				}
			}
			if got.Title != tc.want {
				t.Errorf("named %q, want %q", got.Title, tc.want)
			}
			// The name carries no time of its own. A record's time is
			// evidence of when something happened, and a name did not
			// happen: taking the collector's clock for it moved the
			// conversation's last moment to whenever it was collected.
			if strings.HasPrefix(got.Through, "2026-09-20T11:") {
				t.Errorf("the conversation ends at %s, which is when it was collected", got.Through)
			}
		})
	}
}

// TestTalksComeOutInTheOrderTheyHappened.
//
// The document says its talks are in time order, and a reader is told to
// read them in that order. They were ordered by landed position instead,
// which orders records within one stream and means nothing across streams:
// one request carries a whole trace and its files are written per stream, so
// a sub-agent's file can land before the file of the turn that delegated to
// it. The conversation then opened with the sub-agent answering a question
// nobody had asked yet.
func TestTalksComeOutInTheOrderTheyHappened(t *testing.T) {
	zone, sessions := land(t, "subagent")
	session := sessions[0]
	if _, err := parse.Session(zone, parse.Options{
		Conversation: session, Session: session, Reindex: index.Rebuild}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	server := view_.New(zone, nil)
	conversation, err := server.Load(session)
	if err != nil {
		t.Fatal(err)
	}
	talks := conversation.Talks()
	if len(talks) < 2 {
		t.Fatalf("%d talks, want the main turn and its children", len(talks))
	}
	var last int64
	for i, talk := range talks {
		from, _ := conversation.Span(talk)
		t.Logf("%d %-46s from=%d", i, talk.ID, from)
		if from == 0 {
			continue
		}
		if from < last {
			t.Errorf("%s begins at %d, after a talk that begins at %d", talk.ID, from, last)
		}
		last = from
	}
	// And the first one is the turn the person began, not a child's.
	if !strings.Contains(talks[0].Stream, storage.StreamMain) {
		t.Errorf("the conversation opens with %q, which is not the main stream", talks[0].Stream)
	}
}

// TestAPlainCallInsideAToolIsNotASubagent.
//
// A tool whose body is one model call builds a prompt of its own, so it needs
// a stream of its own: the continuity check must not run across it. But it
// is not a child agent - it is the same agent, carrying a different prompt -
// and it was being reported as one, so this program showed two subagents
// where it has one.
//
// The evidence is what ran directly under the tool. A chain of any kind is a
// program, and the tool is an agent call; only model calls make it a plain
// call, and the tool stays a tool step whose stream is auxiliary.
func TestAPlainCallInsideAToolIsNotASubagent(t *testing.T) {
	zone, sessions := land(t, "subagent")
	session := sessions[0]
	if _, err := parse.Session(zone, parse.Options{
		Conversation: session, Session: session, Reindex: index.Rebuild}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	view := fold(t, zone, session)

	roles := map[string]int{}
	auxiliaryStream := ""
	for _, n := range view.Nodes {
		if n.Kind != model.KindStream {
			continue
		}
		var a struct {
			Role string `json:"role"`
		}
		if err := json.Unmarshal(n.Attrs, &a); err != nil {
			t.Fatal(err)
		}
		roles[a.Role]++
		if a.Role == model.StreamAuxiliary {
			auxiliaryStream = n.Stream
		}
	}
	if roles[model.StreamMain] != 1 || roles[model.StreamChild] != 1 || roles[model.StreamAuxiliary] != 1 {
		t.Fatalf("roles %v, want one main, one child and one auxiliary", roles)
	}
	if !strings.HasPrefix(auxiliaryStream, "summarise-") {
		t.Errorf("the auxiliary stream is %q, want the one summarise opened", auxiliaryStream)
	}

	// The call that opened it is a tool step; the call that opened the child
	// agent is an agent call.
	kinds := map[string]string{}
	for _, n := range view.Nodes {
		if n.Kind == model.KindTool || n.Kind == model.KindAgentCall {
			kinds[n.ID] = n.Kind
		}
	}
	var summarise, delegate string
	for id, kind := range kinds {
		switch {
		case strings.Contains(id, "call_summarise"):
			summarise = kind
		case strings.Contains(id, "call_delegate_to_analyst"):
			delegate = kind
		}
	}
	if summarise != model.KindTool {
		t.Errorf("summarise is %q, want a tool step", summarise)
	}
	if delegate != model.KindAgentCall {
		t.Errorf("delegate_to_analyst is %q, want an agent call", delegate)
	}

	// Its stream is still reached from the call that started it, and it
	// still has a talk - but no agent output, because no agent ran.
	started, output := false, false
	for _, r := range view.Relations {
		if r.Type == model.RelStarts && r.To == "stream/"+auxiliaryStream {
			started = true
		}
	}
	for _, n := range view.Nodes {
		if n.Kind == model.KindAgentOutput && n.Stream == auxiliaryStream {
			output = true
		}
	}
	if !started {
		t.Error("nothing says which call started the auxiliary stream")
	}
	if output {
		t.Error("an agent output was written for a stream no agent ran in")
	}

	// And the page says the same. Its talk rows marked every talk outside
	// main as a child's, so it showed two subagents while the round said one.
	server := view_.NewWithGlossaries(zone, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/c/"+session+"/view", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	var doc struct {
		Talks []struct {
			Stream string `json:"stream"`
			Child  bool   `json:"child"`
		} `json:"talks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("the page's document: %v", err)
	}
	for _, talk := range doc.Talks {
		isChild := strings.HasPrefix(talk.Stream, "delegate-to-analyst-")
		if talk.Child != isChild {
			t.Errorf("the page marks the talk in %s child=%v, want %v", talk.Stream, talk.Child, isChild)
		}
	}

	// And the count a list of conversations shows is the agents.
	if view.Subagents == nil || *view.Subagents != 1 {
		got := "nil"
		if view.Subagents != nil {
			got = strconv.Itoa(*view.Subagents)
		}
		t.Errorf("subagents = %s, want 1: a plain call is not one", got)
	}
}

// receiveInto starts a receiver over a fresh root and returns a poster and a
// collector for it, so a test can send the arrivals it describes rather than
// only replay a capture.
func receiveInto(t *testing.T) (*storage.Zone, func(body string), func() langsmith.Landed) {
	t.Helper()
	zone := storage.NewZone(t.TempDir())
	at := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	receiver := &langsmith.Receiver{Zone: zone, Listen: "127.0.0.1:0",
		Now: func() time.Time { at = at.Add(time.Millisecond); return at }}
	if err := receiver.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(receiver.Stop)
	post := func(body string) {
		t.Helper()
		resp, err := http.Post("http://"+receiver.Addr()+"/runs/batch",
			"application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("the receiver answered %d", resp.StatusCode)
		}
	}
	collected := time.Date(2026, 9, 20, 11, 0, 0, 0, time.UTC)
	collector := &langsmith.Collector{Zone: zone, ProviderBodies: true,
		Now: func() time.Time { collected = collected.Add(time.Second); return collected }}
	collect := func() langsmith.Landed {
		t.Helper()
		landed, err := collector.Collect()
		if err != nil {
			t.Fatal(err)
		}
		// A request held for a run that never arrives converts nothing, and
		// a test that does not notice passes on nothing. The tests that
		// want a request to wait build their own collector.
		if landed.Waiting != 0 {
			t.Fatalf("%d request(s) are waiting for a run that has not arrived; the fixture names one it never sends", landed.Waiting)
		}
		if landed.Unreadable != 0 {
			t.Fatalf("%d request(s) were set aside as unreadable; the fixture is malformed", landed.Unreadable)
		}
		return landed
	}
	return zone, post, collect
}

// receiveIntoCollector is receiveInto for a test that expects a request to
// wait, and drives the collector itself.
func receiveIntoCollector(t *testing.T) (*storage.Zone, func(body string), *langsmith.Collector) {
	t.Helper()
	zone := storage.NewZone(t.TempDir())
	at := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	receiver := &langsmith.Receiver{Zone: zone, Listen: "127.0.0.1:0",
		Now: func() time.Time { at = at.Add(time.Millisecond); return at }}
	if err := receiver.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(receiver.Stop)
	post := func(body string) {
		t.Helper()
		resp, err := http.Post("http://"+receiver.Addr()+"/runs/batch",
			"application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("the receiver answered %d", resp.StatusCode)
		}
	}
	collected := time.Date(2026, 9, 20, 11, 0, 0, 0, time.UTC)
	return zone, post, &langsmith.Collector{Zone: zone, ProviderBodies: true,
		Now: func() time.Time { collected = collected.Add(time.Second); return collected }}
}

// TestAProgramArrivingLaterMakesAPlainCallAChild.
//
// A tool's first nested run can be a model call, with the program it is part
// of arriving in a later request. The link that opened the stream then said
// auxiliary. What is landed cannot be changed, so the arriving program lands
// the link again without the flag - and in assembly a program anywhere under
// the tool wins, because later evidence can make a plain call a child agent
// and never the other way.
func TestAProgramArrivingLaterMakesAPlainCallAChild(t *testing.T) {
	zone, post, collect := receiveInto(t)
	const trace = "21212121-2121-7121-8121-212121212120"
	const toolID = "21212121-2121-7121-8121-212121212121"
	const early = "21212121-2121-7121-8121-212121212122"
	const inner = "21212121-2121-7121-8121-212121212123"
	const late = "21212121-2121-7121-8121-212121212124"
	owner := `"session_name":"asz","extra":{"metadata":{"thread_id":"t-revise"}}`
	root := "20260920T100000000000Z" + trace
	toolDotted := root + ".20260920T100001000000Z" + toolID
	innerDotted := toolDotted + ".20260920T100003000000Z" + inner

	// The root, the tool, and a model call directly under the tool.
	post(`{"post":[{"id":"` + trace + `","trace_id":"` + trace + `",` +
		`"dotted_order":"` + root + `","run_type":"chain","name":"agent",` +
		`"start_time":"2026-09-20T10:00:00Z","end_time":"2026-09-20T10:00:10Z",` + owner + `,` +
		`"inputs":{"messages":[{"role":"user","content":"delegate this"}]}},` +
		`{"id":"` + toolID + `","trace_id":"` + trace + `","parent_run_id":"` + trace + `",` +
		`"dotted_order":"` + toolDotted + `","run_type":"tool","name":"delegate",` +
		`"start_time":"2026-09-20T10:00:01Z","end_time":"2026-09-20T10:00:09Z",` + owner + `,` +
		`"outputs":{"output":{"type":"tool","tool_call_id":"call_delegate_1","content":"done"}}},` +
		`{"id":"` + early + `","trace_id":"` + trace + `","parent_run_id":"` + toolID + `",` +
		`"dotted_order":"` + toolDotted + `.20260920T100002000000Z` + early + `",` +
		`"run_type":"llm","name":"ChatOpenAI","start_time":"2026-09-20T10:00:02Z",` +
		`"end_time":"2026-09-20T10:00:02Z",` + owner + `,` +
		`"outputs":{"generations":[[{"message":{"kwargs":{"content":"quick answer"}}}]]}}]}`)
	landed := collect()
	session := landed.Sessions[0]
	if _, err := parse.Session(zone, parse.Options{
		Conversation: session, Session: session, Reindex: index.Rebuild}); err != nil {
		t.Fatal(err)
	}
	if role := roleOf(t, fold(t, zone, session), "delegate-"); role != model.StreamAuxiliary {
		t.Fatalf("after one plain call the stream is %q, want auxiliary", role)
	}

	// Then the program: a chain under the same tool, with a model call of
	// its own.
	post(`{"post":[{"id":"` + inner + `","trace_id":"` + trace + `","parent_run_id":"` + toolID + `",` +
		`"dotted_order":"` + innerDotted + `","run_type":"chain","name":"analyst",` +
		`"start_time":"2026-09-20T10:00:03Z","end_time":"2026-09-20T10:00:08Z",` + owner + `},` +
		`{"id":"` + late + `","trace_id":"` + trace + `","parent_run_id":"` + inner + `",` +
		`"dotted_order":"` + innerDotted + `.20260920T100004000000Z` + late + `",` +
		`"run_type":"llm","name":"ChatOpenAI","start_time":"2026-09-20T10:00:04Z",` +
		`"end_time":"2026-09-20T10:00:07Z",` + owner + `,` +
		`"outputs":{"generations":[[{"message":{"kwargs":{"content":"considered answer"}}}]]}}]}`)
	collect()
	if _, err := parse.Session(zone, parse.Options{
		Conversation: session, Session: session}); err != nil {
		t.Fatal(err)
	}
	view := fold(t, zone, session)
	if role := roleOf(t, view, "delegate-"); role != model.StreamChild {
		t.Errorf("after the program arrived the stream is %q, want child", role)
	}
	if view.Subagents == nil || *view.Subagents != 1 {
		t.Errorf("subagents = %v, want 1", view.Subagents)
	}
	for _, n := range view.Nodes {
		if strings.Contains(n.ID, "call_delegate_1") && n.Kind != model.KindAgentCall {
			t.Errorf("the call is %q, want an agent call now that a program ran under it", n.Kind)
		}
	}
}

// roleOf finds the role of the stream whose name has the prefix.
func roleOf(t *testing.T, view *sessionflow.View, prefix string) string {
	t.Helper()
	for _, n := range view.Nodes {
		if n.Kind != model.KindStream || !strings.HasPrefix(n.Stream, prefix) {
			continue
		}
		var a struct {
			Role string `json:"role"`
		}
		if err := json.Unmarshal(n.Attrs, &a); err != nil {
			t.Fatal(err)
		}
		return a.Role
	}
	t.Fatalf("no stream named %s...", prefix)
	return ""
}

// TestASecondTurnReachesTheConversation.
//
// The pipeline parses from the index and never extends it: a parse rebuilds
// an index only when there is none. The collector that lands the files has
// to index them, and this one did not. So a LangChain conversation was
// parsed once - the first parse found no index and built one - and every
// turn after that landed, was never indexed, and never reached the chain.
// The conversation was frozen at its first turn, and nothing said so.
//
// Every earlier test parsed a session once, after everything had landed,
// which is exactly the case that could not show it.
func TestASecondTurnReachesTheConversation(t *testing.T) {
	zone, post, collect := receiveInto(t)
	owner := `"session_name":"asz","extra":{"metadata":{"thread_id":"t-two-turns"}}`
	turn := func(id, question, at string) string {
		return `{"post":[{"id":"` + id + `","trace_id":"` + id + `",` +
			`"dotted_order":"20260920T10000` + at + `000000Z` + id + `",` +
			`"run_type":"chain","name":"agent","start_time":"2026-09-20T10:00:0` + at + `Z",` +
			`"end_time":"2026-09-20T10:00:0` + at + `Z",` + owner + `,` +
			`"inputs":{"messages":[{"role":"user","content":"` + question + `"}]},` +
			`"outputs":{"messages":[{"role":"assistant","content":"ok"}]}}]}`
	}
	const first = "51515151-5151-7151-8151-515151515151"
	const second = "51515151-5151-7151-8151-515151515152"

	post(turn(first, "first question", "1"))
	session := collect().Sessions[0]
	if _, err := parse.Session(zone, parse.Options{Conversation: session, Session: session}); err != nil {
		t.Fatal(err)
	}
	if talks := countTalks(fold(t, zone, session)); talks != 1 {
		t.Fatalf("%d talks after one turn, want 1", talks)
	}

	// The second turn: landed by the collector, parsed by the pipeline's own
	// call, which takes the index as it finds it.
	post(turn(second, "second question", "5"))
	collect()
	round, err := parse.Session(zone, parse.Options{Conversation: session, Session: session})
	if err != nil {
		t.Fatal(err)
	}
	if !round.Changed() {
		t.Fatal("the second turn landed and the parse wrote no round")
	}
	view := fold(t, zone, session)
	if talks := countTalks(view); talks != 2 {
		t.Errorf("%d talks after two turns, want 2: the second never reached the conversation", talks)
	}
	if view.Round != 2 {
		t.Errorf("the chain is at round %d after two parses, want 2", view.Round)
	}
}

func countTalks(view *sessionflow.View) int {
	n := 0
	for _, node := range view.Nodes {
		if node.Kind == model.KindTalk {
			n++
		}
	}
	return n
}

// TestARevisionArrivingInItsOwnPassIsNotDropped.
//
// The tool, the model call under it and the program under it can arrive in
// three separate requests. The second lands a link saying auxiliary; the
// third lands a link saying child. Both were named <tool>:started, and the
// index keeps the first record for an id, so the correction was written to
// disk and never read. The two say different things, so they are different
// records.
func TestARevisionArrivingInItsOwnPassIsNotDropped(t *testing.T) {
	zone, post, collect := receiveInto(t)
	const trace = "61616161-6161-7161-8161-616161616160"
	const toolID = "61616161-6161-7161-8161-616161616161"
	const early = "61616161-6161-7161-8161-616161616162"
	const inner = "61616161-6161-7161-8161-616161616163"
	owner := `"session_name":"asz","extra":{"metadata":{"thread_id":"t-three-passes"}}`
	root := "20260920T100000000000Z" + trace
	toolDotted := root + ".20260920T100001000000Z" + toolID
	parseNow := func(session string) *sessionflow.View {
		t.Helper()
		if _, err := parse.Session(zone, parse.Options{Conversation: session, Session: session}); err != nil {
			t.Fatal(err)
		}
		return fold(t, zone, session)
	}

	// Pass one: the root and the tool, nothing under it yet.
	post(`{"post":[{"id":"` + trace + `","trace_id":"` + trace + `","dotted_order":"` + root + `",` +
		`"run_type":"chain","name":"agent","start_time":"2026-09-20T10:00:00Z",` +
		`"end_time":"2026-09-20T10:00:10Z",` + owner + `,` +
		`"inputs":{"messages":[{"role":"user","content":"delegate this"}]}},` +
		`{"id":"` + toolID + `","trace_id":"` + trace + `","parent_run_id":"` + trace + `",` +
		`"dotted_order":"` + toolDotted + `","run_type":"tool","name":"delegate",` +
		`"start_time":"2026-09-20T10:00:01Z","end_time":"2026-09-20T10:00:09Z",` + owner + `,` +
		`"outputs":{"output":{"type":"tool","tool_call_id":"call_delegate_1","content":"done"}}}]}`)
	session := collect().Sessions[0]
	parseNow(session)

	// Pass two: a model call directly under the tool. A plain call, so far.
	post(`{"post":[{"id":"` + early + `","trace_id":"` + trace + `","parent_run_id":"` + toolID + `",` +
		`"dotted_order":"` + toolDotted + `.20260920T100002000000Z` + early + `",` +
		`"run_type":"llm","name":"ChatOpenAI","start_time":"2026-09-20T10:00:02Z",` +
		`"end_time":"2026-09-20T10:00:02Z",` + owner + `,` +
		`"outputs":{"generations":[[{"message":{"kwargs":{"content":"quick"}}}]]}}]}`)
	collect()
	if role := roleOf(t, parseNow(session), "delegate-"); role != model.StreamAuxiliary {
		t.Fatalf("after a plain call the stream is %q, want auxiliary", role)
	}

	// Pass three: a program under the same tool.
	post(`{"post":[{"id":"` + inner + `","trace_id":"` + trace + `","parent_run_id":"` + toolID + `",` +
		`"dotted_order":"` + toolDotted + `.20260920T100003000000Z` + inner + `",` +
		`"run_type":"chain","name":"analyst","start_time":"2026-09-20T10:00:03Z",` +
		`"end_time":"2026-09-20T10:00:08Z",` + owner + `}]}`)
	collect()
	view := parseNow(session)
	if role := roleOf(t, view, "delegate-"); role != model.StreamChild {
		t.Errorf("after the program arrived in its own pass the stream is %q, want child", role)
	}
	if view.Subagents == nil || *view.Subagents != 1 {
		t.Errorf("subagents = %v, want 1", view.Subagents)
	}
}

// TestACrashBetweenIndexAndStateDoesNotDoubleCount.
//
// The index is written before its state is saved. A crash between the two
// leaves an index that already holds what the state says it does not, and
// extending it from the state's watermark added those records a second
// time. A record with an id is kept once; a name has no id, so its copy was
// kept too, and the stream's record count no longer matched the files.
func TestACrashBetweenIndexAndStateDoesNotDoubleCount(t *testing.T) {
	zone, post, collect := receiveInto(t)
	owner := `"session_name":"asz","extra":{"metadata":{"thread_id":"t-crash"}}`
	turn := func(id, question, at string) string {
		return `{"post":[{"id":"` + id + `","trace_id":"` + id + `",` +
			`"dotted_order":"20260920T10000` + at + `000000Z` + id + `",` +
			`"run_type":"chain","name":"agent","start_time":"2026-09-20T10:00:0` + at + `Z",` +
			`"end_time":"2026-09-20T10:00:0` + at + `Z",` + owner + `,` +
			`"inputs":{"messages":[{"role":"user","content":"` + question + `"}]}}]}`
	}
	post(turn("71717171-7171-7171-8171-717171717171", "first", "1"))
	session := collect().Sessions[0]

	// The crash: the index on disk is what the collector wrote, the state
	// is what it had before. The schema is the current one - a state from
	// another schema is discarded by every writer already, which is not the
	// crash and would not show it.
	crashed(t, zone, session)

	post(turn("71717171-7171-7171-8171-717171717172", "second", "5"))
	collect()
	if _, err := parse.Session(zone, parse.Options{Conversation: session, Session: session}); err != nil {
		t.Fatal(err)
	}
	view := fold(t, zone, session)

	onDisk := 0
	files, err := storage.LandedFiles(zone, session)
	if err != nil {
		t.Fatal(err)
	}
	for _, lf := range files {
		f, err := os.Open(lf.Path)
		if err != nil {
			t.Fatal(err)
		}
		if reader, err := sessiondata.NewReader(f); err == nil {
			for {
				if _, err := reader.Next(); err != nil {
					break
				}
				onDisk++
			}
		}
		f.Close()
	}
	for _, n := range view.Nodes {
		if n.Kind != model.KindStream || n.Stream != storage.StreamMain {
			continue
		}
		var a struct {
			Records int `json:"records"`
		}
		if err := json.Unmarshal(n.Attrs, &a); err != nil {
			t.Fatal(err)
		}
		if a.Records != onDisk {
			t.Errorf("main says %d records, the files hold %d: something was counted twice", a.Records, onDisk)
		}
	}
}

// TestACrashRecoveredByAnotherWriterDoesNotDoubleCount.
//
// Every collector that lands into a session extends the same index, and
// each writes the index before saving its state. A guard in one of them is
// not enough: crash after the langsmith collector wrote the index but before
// it saved the state, and if the changes collector runs first on restart, it
// appends the already-indexed records again and then saves counts that
// match - and the langsmith guard sees nothing wrong afterwards. So every
// writer takes the index through one guard, index.LoadFor.
func TestACrashRecoveredByAnotherWriterDoesNotDoubleCount(t *testing.T) {
	zone, post, collect := receiveInto(t)
	owner := `"session_name":"asz","extra":{"metadata":{"thread_id":"t-other-writer"}}`
	turn := func(id, question, at string) string {
		return `{"post":[{"id":"` + id + `","trace_id":"` + id + `",` +
			`"dotted_order":"20260920T10000` + at + `000000Z` + id + `",` +
			`"run_type":"chain","name":"agent","start_time":"2026-09-20T10:00:0` + at + `Z",` +
			`"end_time":"2026-09-20T10:00:0` + at + `Z",` + owner + `,` +
			`"inputs":{"messages":[{"role":"user","content":"` + question + `"}]}}]}`
	}
	post(turn("a1a1a1a1-a1a1-71a1-81a1-a1a1a1a1a1a1", "first", "1"))
	session := collect().Sessions[0]

	// The crash: the index holds the turn, the state does not.
	crashed(t, zone, session)

	// The changes collector is the first writer after the restart. It has a
	// change record for this session, so it takes the session's index.
	data := t.TempDir()
	out := filepath.Join(data, "file-changes-skywalking-ai-sessionizer", "output", session)
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(map[string]any{
		"schema": "changes/1", "id": "call_write_1", "captured_by": "test", "cwd": data,
		"at": "2026-09-20T10:00:02Z", "tool": "write_report", "changes": []any{},
	})
	if err := os.WriteFile(filepath.Join(out, "main.jsonl"), append(line, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := claudecodechanges.New(data, zone, 2<<20).CollectAll(nil); err != nil {
		t.Fatal(err)
	}

	// Then a second turn through the langsmith collector, and a parse.
	post(turn("a1a1a1a1-a1a1-71a1-81a1-a1a1a1a1a1a2", "second", "5"))
	collect()
	if _, err := parse.Session(zone, parse.Options{Conversation: session, Session: session}); err != nil {
		t.Fatal(err)
	}
	view := fold(t, zone, session)

	// The stream's record count is its conversation records. The change
	// record the other writer landed sits in the same directory under its
	// own kind and is joined to a step, not counted as one.
	onDisk := map[string]int{}
	files, err := storage.LandedFiles(zone, session)
	if err != nil {
		t.Fatal(err)
	}
	for _, lf := range files {
		f, err := os.Open(lf.Path)
		if err != nil {
			t.Fatal(err)
		}
		if reader, err := sessiondata.NewReader(f); err == nil && reader.Header().Kind == sessiondata.KindTranscript {
			stream := reader.Header().Stream
			for {
				if _, err := reader.Next(); err != nil {
					break
				}
				onDisk[stream]++
			}
		}
		f.Close()
	}
	for _, n := range view.Nodes {
		if n.Kind != model.KindStream {
			continue
		}
		var a struct {
			Records int `json:"records"`
		}
		if err := json.Unmarshal(n.Attrs, &a); err != nil {
			t.Fatal(err)
		}
		if a.Records != onDisk[n.Stream] {
			t.Errorf("%s says %d records, the files hold %d: something was counted twice",
				n.Stream, a.Records, onDisk[n.Stream])
		}
	}
}

// crashed leaves a session as a crash between writing its index and saving
// the index's state does: the index holds what the state says it does not.
// The schema stays current, because a state from another schema is
// discarded by every writer already and shows nothing.
func crashed(t *testing.T, zone *storage.Zone, session string) {
	t.Helper()
	before := storage.NewIndexState(session)
	before.Schema = index.Schema
	if err := before.Save(zone.IndexStatePath(session), time.Now()); err != nil {
		t.Fatal(err)
	}
}

// TestEveryModelCallCarriesWhatItWasSent.
//
// A call's record keeps what the model said. What it was told is landed
// beside it as a provider body and joined to the call, which both the
// request and the response name. Every call in every stream has to carry
// both - including the first call of a nested agent, in a stream of its own.
//
// Counting roles is not enough: two calls' requests swapped, or a call gone
// missing, still count. So the number of calls is the capture's, and each
// call's request and response are rebuilt from the landed records and
// compared byte for byte with what that run carried on the wire.
func TestEveryModelCallCarriesWhatItWasSent(t *testing.T) {
	for _, tc := range []struct {
		kase  string
		calls int
	}{{"subagent", 6}, {"long-conversation", 20}, {"three-turns", 4}, {"large-content", 2}} {
		t.Run(tc.kase, func(t *testing.T) {
			wire := wireBodies(t, tc.kase)
			zone, sessions := land(t, tc.kase)
			session := sessions[0]
			if _, err := parse.Session(zone, parse.Options{
				Conversation: session, Session: session, Reindex: index.Rebuild}); err != nil {
				t.Fatalf("parse: %v", err)
			}
			view := fold(t, zone, session)
			held := heldBodies(t, zone, session)

			calls := 0
			for _, n := range view.Nodes {
				if n.Kind != model.KindLLMCall {
					continue
				}
				calls++
				runID := strings.TrimPrefix(n.ID, "call/")
				bodies, err := sessionflow.ProviderBodiesOf(n.Attrs)
				if err != nil {
					t.Fatalf("%s: %v", n.ID, err)
				}
				got := map[string]sessionflow.Ref{}
				for _, b := range bodies {
					got[b.Role] = b.Ref
				}
				if len(got) != 2 || len(bodies) != 2 {
					t.Errorf("%s in %s carries %d bodies, want one request and one response", n.ID, n.Stream, len(bodies))
					continue
				}
				for role, want := range map[string][]byte{
					sessionflow.RoleRequest:  wire[runID].inputs,
					sessionflow.RoleResponse: wire[runID].outputs,
				} {
					ref, ok := got[role]
					if !ok {
						t.Errorf("%s carries no %s", n.ID, role)
						continue
					}
					id := recordIDAt(t, zone, session, ref)
					if m, ok := held.Manifest(id); !ok || m.Call != runID {
						t.Errorf("%s: the manifest of %s names call %q", n.ID, id, m.Call)
					}
					if wantID := runID + ":" + role; id != wantID {
						t.Errorf("%s's %s is record %q, want %q: joined to another call's body", n.ID, role, id, wantID)
						continue
					}
					body, err := held.Body(id)
					if err != nil {
						t.Errorf("%s: rebuilding %s: %v", n.ID, id, err)
						continue
					}
					if !bytes.Equal(body, want) {
						t.Errorf("%s's %s rebuilds to %d bytes that are not the %d the wire carried", n.ID, role, len(body), len(want))
					}
				}
			}
			if calls != tc.calls {
				t.Errorf("%d model calls, want the capture's %d", calls, tc.calls)
			}
		})
	}
}

// wireBody is what one model call carried on the wire.
type wireBody struct{ inputs, outputs []byte }

// wireBodies reads, from a capture's own requests, each model run's inputs
// and outputs as they were sent: the inputs from its first arrival carrying
// them, the outputs from the arrival carrying its end.
func wireBodies(t *testing.T, kase string) map[string]wireBody {
	t.Helper()
	out := map[string]wireBody{}
	dir := filepath.Join(corpus, kase)
	requests, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("no corpus for %s: %v", kase, err)
	}
	for _, r := range requests {
		raw, err := os.ReadFile(filepath.Join(dir, r.Name(), "meta.json"))
		if err != nil {
			continue
		}
		var meta struct {
			Headers map[string]string `json:"headers"`
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(filepath.Join(dir, r.Name(), "body.bin"))
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := langsmith.ParseMultipart(bytes.NewReader(body), meta.Headers["Content-Type"])
		if err != nil {
			t.Fatal(err)
		}
		for _, op := range parsed.Operations {
			var run struct {
				ID   string `json:"id"`
				Type string `json:"run_type"`
				End  string `json:"end_time"`
			}
			if err := json.Unmarshal(op.Envelope, &run); err != nil {
				t.Fatal(err)
			}
			if run.Type != "llm" && run.Type != "chat_model" {
				continue
			}
			w := out[run.ID]
			if in, ok := op.Fields["inputs"]; ok && w.inputs == nil {
				w.inputs = append([]byte(nil), in...)
			}
			if o, ok := op.Fields["outputs"]; ok && run.End != "" {
				w.outputs = append([]byte(nil), o...)
			}
			out[run.ID] = w
		}
	}
	return out
}

// heldBodies rebuilds what a session holds, from its landed body records,
// the way a reader would.
func heldBodies(t *testing.T, zone *storage.Zone, session string) *providerbody.Session {
	t.Helper()
	held := providerbody.NewSession()
	files, err := storage.LandedFiles(zone, session)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Stream != "" || f.RunID != "" {
			continue
		}
		fh, err := os.Open(f.Path)
		if err != nil {
			t.Fatal(err)
		}
		reader, err := sessiondata.NewReader(fh)
		if err != nil {
			fh.Close()
			t.Fatal(err)
		}
		for {
			rec, err := reader.Next()
			if err != nil {
				break
			}
			if err := held.Add(rec); err != nil {
				t.Fatalf("%s: %v", f.Path, err)
			}
		}
		fh.Close()
	}
	return held
}

// recordIDAt reads the id of the landed record at a position.
func recordIDAt(t *testing.T, zone *storage.Zone, session string, ref sessionflow.Ref) string {
	t.Helper()
	files, err := storage.LandedFiles(zone, session)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Seq != ref.Seq {
			continue
		}
		fh, err := os.Open(f.Path)
		if err != nil {
			t.Fatal(err)
		}
		defer fh.Close()
		reader, err := sessiondata.NewReader(fh)
		if err != nil {
			t.Fatal(err)
		}
		for row := uint64(1); ; row++ {
			rec, err := reader.Next()
			if err != nil {
				break
			}
			if row == ref.Row {
				return rec.ID
			}
		}
	}
	t.Fatalf("no record at seq %d row %d", ref.Seq, ref.Row)
	return ""
}

// TestARequestJoinsItsOwnCallWhenItsInputsArriveLate.
//
// A call's completion can arrive before its inputs, and the next call's
// start can arrive between the two. A request used to be keyed by the call
// before it, taken from a cursor that moved only when a body landed, which
// was nothing here - and the second call's request then joined the first
// call. A request names its call, whenever its inputs arrive.
func TestARequestJoinsItsOwnCallWhenItsInputsArriveLate(t *testing.T) {
	zone, post, collect := receiveInto(t)
	const trace = "e1e1e1e1-e1e1-71e1-81e1-e1e1e1e1e1e0"
	const a = "e1e1e1e1-e1e1-71e1-81e1-e1e1e1e1e1e1"
	const b = "e1e1e1e1-e1e1-71e1-81e1-e1e1e1e1e1e2"
	owner := `"session_name":"asz","extra":{"metadata":{"thread_id":"t-late-inputs"}}`
	root := "20260920T100000000000Z" + trace
	call := func(id, at string) string {
		return `"id":"` + id + `","trace_id":"` + trace + `","parent_run_id":"` + trace + `",` +
			`"dotted_order":"` + root + `.20260920T10000` + at + `000000Z` + id + `",` +
			`"run_type":"llm","name":"ChatOpenAI",` + owner
	}
	inputsA := `{"messages":[[{"role":"user","content":"first"}]]}`
	inputsB := `{"messages":[[{"role":"user","content":"first"},{"role":"assistant","content":"A"},{"role":"user","content":"second"}]]}`
	outputs := func(text string) string {
		return `{"generations":[[{"message":{"kwargs":{"content":"` + text + `"}}}]]}`
	}
	// The root, then A's completion with no inputs.
	post(`{"post":[{"id":"` + trace + `","trace_id":"` + trace + `","dotted_order":"` + root + `",` +
		`"run_type":"chain","name":"agent","start_time":"2026-09-20T10:00:00Z",` + owner + `,` +
		`"inputs":{"messages":[{"role":"user","content":"first"}]}}],` +
		`"patch":[{` + call(a, "1") + `,"end_time":"2026-09-20T10:00:02Z","outputs":` + outputs("A") + `}]}`)
	collect()
	// B's start, with inputs.
	post(`{"post":[{` + call(b, "3") + `,"start_time":"2026-09-20T10:00:03Z","end_time":"2026-09-20T10:00:04Z",` +
		`"inputs":` + inputsB + `,"outputs":` + outputs("B") + `}]}`)
	collect()
	// A's start, late, with its inputs.
	post(`{"post":[{` + call(a, "1") + `,"start_time":"2026-09-20T10:00:01Z","inputs":` + inputsA + `}]}`)
	landed := collect()
	session := landed.Sessions[0]

	m := manifestsOf(t, zone, session)
	if got := m[a+":request"].Call; got != a {
		t.Errorf("A's request, landed last, names %q as its call", got)
	}
	bodiesAreOwn(t, zone, session, 2)
}

func countKind(view *sessionflow.View, kind string) int {
	n := 0
	for _, node := range view.Nodes {
		if node.Kind == kind {
			n++
		}
	}
	return n
}

// manifestsOf reads every landed body's manifest, by record id.
func manifestsOf(t *testing.T, zone *storage.Zone, session string) map[string]*providerbody.Manifest {
	t.Helper()
	out := map[string]*providerbody.Manifest{}
	files, err := storage.LandedFiles(zone, session)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Stream != "" || f.RunID != "" {
			continue
		}
		fh, err := os.Open(f.Path)
		if err != nil {
			t.Fatal(err)
		}
		if reader, err := sessiondata.NewReader(fh); err == nil {
			for {
				rec, err := reader.Next()
				if err != nil {
					break
				}
				if m, err := providerbody.ManifestOf(rec); err == nil {
					out[rec.ID] = m
				}
			}
		}
		fh.Close()
	}
	return out
}

// TestAReplayAfterALostShapeLandsNothingTwice.
//
// Bodies land before the shape is saved. A crash between the two replays the
// request: its bodies are repeats, and every request still joins its own
// call. The shape holds nothing a body joins by - a request names its call -
// so a lost shape cannot move a join, as it did when the shape kept the
// stream's order.
func TestAReplayAfterALostShapeLandsNothingTwice(t *testing.T) {
	zone, post, collect := receiveInto(t)
	const trace = "f1f1f1f1-f1f1-71f1-81f1-f1f1f1f1f1f0"
	owner := `"session_name":"asz","extra":{"metadata":{"thread_id":"t-replay"}}`
	root := "20260920T100000000000Z" + trace
	call := func(id, at, content string) string {
		return `{"post":[{"id":"` + id + `","trace_id":"` + trace + `","parent_run_id":"` + trace + `",` +
			`"dotted_order":"` + root + `.20260920T10000` + at + `000000Z` + id + `",` +
			`"run_type":"llm","name":"ChatOpenAI","start_time":"2026-09-20T10:00:0` + at + `Z",` +
			`"end_time":"2026-09-20T10:00:0` + at + `Z",` + owner + `,` +
			`"inputs":{"messages":[[{"role":"user","content":"` + content + `"}]]},` +
			`"outputs":{"generations":[[{"message":{"kwargs":{"content":"ok"}}}]]}}]}`
	}
	post(`{"post":[{"id":"` + trace + `","trace_id":"` + trace + `","dotted_order":"` + root + `",` +
		`"run_type":"chain","name":"agent","start_time":"2026-09-20T10:00:00Z",` + owner + `,` +
		`"inputs":{"messages":[{"role":"user","content":"go"}]}}]}`)
	const a, b, c = "f1f1f1f1-f1f1-71f1-81f1-f1f1f1f1f1f1", "f1f1f1f1-f1f1-71f1-81f1-f1f1f1f1f1f2", "f1f1f1f1-f1f1-71f1-81f1-f1f1f1f1f1f3"
	post(call(a, "1", "a"))
	session := collect().Sessions[0]
	shapePath := filepath.Join(zone.SessionDir(session), "langsmith.shape.json")
	before, err := os.ReadFile(shapePath)
	if err != nil {
		t.Fatal(err)
	}
	second := call(b, "2", "b")
	post(second)
	collect()
	// The crash: B's bodies landed, the shape did not. The client retries.
	if err := os.WriteFile(shapePath, before, 0o644); err != nil {
		t.Fatal(err)
	}
	post(second)
	collect()
	post(call(c, "3", "c"))
	collect()
	m := manifestsOf(t, zone, session)
	if len(m) != 6 {
		t.Errorf("%d bodies landed for three calls, want 6: a replay is a repeat", len(m))
	}
	if got := bodyRecordsLanded(t, zone, session); got != 6 {
		t.Errorf("%d body records landed for three calls, want 6: a replay lands no second record", got)
	}
	for _, id := range []string{a, b, c} {
		if got := m[id+":request"].Call; got != id {
			t.Errorf("%s's request names %q as its call", id, got)
		}
	}
	bodiesAreOwn(t, zone, session, 3)
}

// TestTwoRequestsNamingOneCallJoinNeither.
//
// A request joins by the call it names when exactly one request names it,
// which is the rule every join follows. This receiver lands one request per
// call, so the second one here is landed by hand, as another collector might
// land it. The call then carries its response and no request, rather than
// one of the two.
func TestTwoRequestsNamingOneCallJoinNeither(t *testing.T) {
	zone, post, collect := receiveInto(t)
	const trace = "d2d2d2d2-d2d2-72d2-82d2-d2d2d2d2d2d0"
	const a = "d2d2d2d2-d2d2-72d2-82d2-d2d2d2d2d2d1"
	owner := `"session_name":"asz","extra":{"metadata":{"thread_id":"t-two-requests"}}`
	root := "20260920T100000000000Z" + trace
	post(`{"post":[{"id":"` + trace + `","trace_id":"` + trace + `","dotted_order":"` + root + `",` +
		`"run_type":"chain","name":"agent","start_time":"2026-09-20T10:00:00Z",` + owner + `,` +
		`"inputs":{"messages":[{"role":"user","content":"go"}]}},` +
		`{"id":"` + a + `","trace_id":"` + trace + `","parent_run_id":"` + trace + `",` +
		`"dotted_order":"` + root + `.20260920T100001000000Z` + a + `","run_type":"llm","name":"ChatOpenAI",` +
		`"start_time":"2026-09-20T10:00:01Z","end_time":"2026-09-20T10:00:02Z",` + owner + `,` +
		`"inputs":{"messages":[[{"role":"user","content":"go"}]]},` +
		`"outputs":{"generations":[[{"message":{"kwargs":{"content":"A"}}}]]}}]}`)
	session := collect().Sessions[0]
	bodiesAreOwn(t, zone, session, 1)

	// A second request naming A, in a provider body file of its own, under
	// the session's lock and sequence as any collector would land it.
	dir := zone.SessionDir(session)
	lock, err := storage.LockSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	state, err := storage.LoadSessionState(zone.SessionStatePath(session), session)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.RecoverNextSeq(dir); err != nil {
		t.Fatal(err)
	}
	rec, err := providerbody.NewSession().Encode(providerbody.Body{
		ID: a + ":request-again", Role: providerbody.RoleRequest, Src: "by-hand",
		Keys:  providerbody.Keys{Session: session, Call: a},
		Bytes: []byte(`{"messages":[[{"role":"user","content":"go, again"}]]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	seq := state.Take()
	header := &sessiondata.Header{
		Seq: seq, At: time.Now().UTC().Format(time.RFC3339Nano),
		Kind: sessiondata.KindProviderBody, Adapter: langsmith.Name + "/" + langsmith.Version,
		Dialect: langsmith.Dialect, Src: ".", Session: session,
	}
	path := filepath.Join(zone.ProviderDir(session), storage.LandedName(string(sessiondata.KindProviderBody), "20260920T100003.000000000Z", seq))
	err = storage.WriteExclusive(path, storage.PermLanded, func(w io.Writer) error {
		writer, err := sessiondata.NewWriter(w, header)
		if err != nil {
			return err
		}
		if err := writer.Write(rec); err != nil {
			return err
		}
		return writer.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Save(zone.SessionStatePath(session), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	// A collector indexes what it lands. This landed nothing through one, so
	// the index is dropped and parsing rebuilds it from the landed files,
	// which is what makes the index disposable.
	if err := os.RemoveAll(zone.IndexDir(session)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(zone.IndexStatePath(session)); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	if _, err := parse.Session(zone, parse.Options{Conversation: session, Session: session}); err != nil {
		t.Fatal(err)
	}
	if got := len(manifestsOf(t, zone, session)); got != 3 {
		t.Fatalf("%d bodies landed, want the call's two and the one by hand", got)
	}
	view := fold(t, zone, session)
	if got := countKind(view, model.KindLLMCall); got != 1 {
		t.Fatalf("%d calls in the fold, want the one", got)
	}
	for _, n := range view.Nodes {
		if n.Kind != model.KindLLMCall {
			continue
		}
		bodies, _ := sessionflow.ProviderBodiesOf(n.Attrs)
		if len(bodies) != 1 || bodies[0].Role != sessionflow.RoleResponse {
			t.Fatalf("%s carries %v, want its response alone: two requests name it", n.ID, bodies)
		}
		if id := recordIDAt(t, zone, session, bodies[0].Ref); id != a+":response" {
			t.Errorf("%s carries %q as its response", n.ID, id)
		}
	}
}

// TestAChangedRedeliveryIsCountedNotLanded: a finished run delivered again
// with other outputs does not replace what landed, and is not hidden either.
func TestAChangedRedeliveryIsCountedNotLanded(t *testing.T) {
	zone, post, collect := receiveInto(t)
	// The run is its own root: a dotted order naming a run that never
	// arrives is waited for, and a test that waits converts nothing.
	const id = "a2a2a2a2-a2a2-71a2-81a2-a2a2a2a2a2a1"
	owner := `"session_name":"asz","extra":{"metadata":{"thread_id":"t-conflict"}}`
	root := "20260920T100000000000Z" + id
	run := func(outputs string) string {
		return `{"post":[{"id":"` + id + `","trace_id":"` + id + `","dotted_order":"` + root + `",` +
			`"run_type":"llm","name":"ChatOpenAI","start_time":"2026-09-20T10:00:00Z",` +
			`"end_time":"2026-09-20T10:00:01Z",` + owner + `,` +
			`"inputs":{"messages":[[{"role":"user","content":"hi"}]]},` +
			`"outputs":{"generations":[[{"message":{"kwargs":{"content":"` + outputs + `"}}}]]}}]}`
	}
	post(run("first"))
	first := collect()
	post(run("changed"))
	second := collect()
	if first.BodyConflicts != 0 || second.BodyConflicts != 1 {
		t.Errorf("conflicts %d then %d, want 0 then 1", first.BodyConflicts, second.BodyConflicts)
	}
	// Counted, and not landed: one response, and it is the first one.
	responses := 0
	for id, m := range manifestsOf(t, zone, first.Sessions[0]) {
		if m.Role != providerbody.RoleResponse {
			continue
		}
		responses++
		if m.SHA256 != providerbody.Digest([]byte(`{"generations":[[{"message":{"kwargs":{"content":"first"}}}]]}`)) {
			t.Errorf("%s is not the first response landed", id)
		}
	}
	if responses != 1 {
		t.Errorf("%d responses landed, want the first and only the first", responses)
	}
	if got := bodyRecordsLanded(t, zone, first.Sessions[0]); got != 2 {
		t.Errorf("%d body records landed, want the first request and the first response only", got)
	}
}

// TestAModelCallSentTwiceLandsItsBodiesOnceAndFinished.
//
// A model call can be posted open, with a streaming stub in its outputs,
// and patched closed with the real answer. Its request is landed from the
// first arrival carrying inputs, its response only from the arrival carrying
// its end, and the repeat of the request is a repeat: two bodies, and the
// response is the finished answer and not the stub.
//
// The slow-tool capture does not show this - there it is the tool that
// arrives twice, and its model calls arrive once, finished - so a test on
// it passed with the rule broken. This one sends the arrivals itself.
func TestAModelCallSentTwiceLandsItsBodiesOnceAndFinished(t *testing.T) {
	zone, post, collect := receiveInto(t)
	const trace = "d1d1d1d1-d1d1-71d1-81d1-d1d1d1d1d1d0"
	const call = "d1d1d1d1-d1d1-71d1-81d1-d1d1d1d1d1d1"
	owner := `"session_name":"asz","extra":{"metadata":{"thread_id":"t-twice"}}`
	root := "20260920T100000000000Z" + trace
	inputs := `{"messages":[[{"role":"user","content":"count to three"}]]}`
	stub := `{"generations":[[{"message":{"kwargs":{"content":"on"}}}]]}`
	answer := `{"generations":[[{"message":{"kwargs":{"content":"one, two, three"}}}]]}`

	post(`{"post":[{"id":"` + trace + `","trace_id":"` + trace + `","dotted_order":"` + root + `",` +
		`"run_type":"chain","name":"agent","start_time":"2026-09-20T10:00:00Z",` + owner + `,` +
		`"inputs":{"messages":[{"role":"user","content":"count to three"}]}},` +
		`{"id":"` + call + `","trace_id":"` + trace + `","parent_run_id":"` + trace + `",` +
		`"dotted_order":"` + root + `.20260920T100001000000Z` + call + `",` +
		`"run_type":"llm","name":"ChatOpenAI","start_time":"2026-09-20T10:00:01Z",` + owner + `,` +
		`"inputs":` + inputs + `,"outputs":` + stub + `}]}`)
	collect()
	post(`{"patch":[{"id":"` + call + `","trace_id":"` + trace + `","parent_run_id":"` + trace + `",` +
		`"dotted_order":"` + root + `.20260920T100001000000Z` + call + `",` +
		`"run_type":"llm","name":"ChatOpenAI","end_time":"2026-09-20T10:00:04Z",` + owner + `,` +
		`"inputs":` + inputs + `,"outputs":` + answer + `},` +
		`{"id":"` + trace + `","end_time":"2026-09-20T10:00:05Z"}]}`)
	landed := collect()
	session := landed.Sessions[0]

	var roles []string
	responseIs := ""
	files, err := storage.LandedFiles(zone, session)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Stream != "" || f.RunID != "" {
			continue
		}
		fh, err := os.Open(f.Path)
		if err != nil {
			t.Fatal(err)
		}
		if reader, err := sessiondata.NewReader(fh); err == nil {
			for {
				rec, err := reader.Next()
				if err != nil {
					break
				}
				m, err := providerbody.ManifestOf(rec)
				if err != nil {
					t.Fatal(err)
				}
				roles = append(roles, m.Role)
				if m.Role == providerbody.RoleResponse {
					responseIs = m.SHA256
				}
			}
		}
		fh.Close()
	}
	sort.Strings(roles)
	if strings.Join(roles, ",") != "request,response" {
		t.Fatalf("bodies landed: %v, want exactly one request and one response", roles)
	}
	if responseIs != providerbody.Digest([]byte(answer)) {
		if responseIs == providerbody.Digest([]byte(stub)) {
			t.Fatal("the response landed is the streaming stub, not the finished answer")
		}
		t.Fatalf("the response landed is neither the stub nor the answer")
	}
}

// TestProviderBodiesCanBeTurnedOff: with the setting off nothing is landed
// beside the conversation, and the session says it holds no bodies.
func TestProviderBodiesCanBeTurnedOff(t *testing.T) {
	zone, post, _ := receiveInto(t)
	post(`{"post":[{"id":"c1c1c1c1-c1c1-71c1-81c1-c1c1c1c1c1c1","trace_id":"c1c1c1c1-c1c1-71c1-81c1-c1c1c1c1c1c1",` +
		`"dotted_order":"20260920T100000000000Zc1c1c1c1-c1c1-71c1-81c1-c1c1c1c1c1c1","run_type":"llm","name":"ChatOpenAI",` +
		`"start_time":"2026-09-20T10:00:00Z","end_time":"2026-09-20T10:00:01Z","session_name":"asz",` +
		`"extra":{"metadata":{"thread_id":"t-off"}},"inputs":{"messages":[[{"role":"user","content":"hi"}]]},` +
		`"outputs":{"generations":[[{"message":{"kwargs":{"content":"hello"}}}]]}}]}`)
	off := &langsmith.Collector{Zone: zone, ProviderBodies: false}
	landed, err := off.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if landed.Bodies != 0 {
		t.Errorf("%d bodies landed with the setting off", landed.Bodies)
	}
	if _, err := os.Stat(zone.ProviderDir(landed.Sessions[0])); !os.IsNotExist(err) {
		t.Error("a provider body directory exists with the setting off")
	}
}

// TestARequestJoinsItsOwnCallInEverySchedule.
//
// A request names its call, the run's own id, and the collector keeps and
// derives nothing about the order of a stream's calls. It did, twice - a
// copy kept in its own state, then an order read from the session's index -
// and the assembler disagreed with it in each schedule below, so a request
// joined another call. In every schedule every request has to name its own
// call and join it, whatever landed first:
//
//   - a session landed before the collector kept anything;
//   - a call whose first fragment landed in main before its ancestry
//     arrived, so the assembler places it by that fragment;
//   - a request held for a later pass and then replayed, landing after a
//     call that arrived while it waited;
//   - a call delivered again after another has followed it.
func TestARequestJoinsItsOwnCallInEverySchedule(t *testing.T) {
	owner := `"session_name":"asz","extra":{"metadata":{"thread_id":"t-order"}}`
	trace := func(n string) string {
		return "b" + n + "b" + n + "b" + n + "b" + n + "-b" + n + "b" + n + "-7" + n + "b" + n + "-8" + n + "b" + n + "-b" + n + "b" + n + "b" + n + "b" + n + "b" + n + "b0"
	}
	call := func(tr, id, parent, dotted, at string, inputs, outputs string) string {
		body := `{"id":"` + id + `","trace_id":"` + tr + `","parent_run_id":"` + parent + `",` +
			`"dotted_order":"` + dotted + `","run_type":"llm","name":"ChatOpenAI",` + owner
		if inputs != "" {
			body += `,"start_time":"2026-09-20T10:00:0` + at + `Z","inputs":` + inputs
		}
		if outputs != "" {
			body += `,"end_time":"2026-09-20T10:00:0` + at + `.9Z","outputs":` + outputs
		}
		return body + `}`
	}
	in := func(text string) string { return `{"messages":[[{"role":"user","content":"` + text + `"}]]}` }
	out := func(text string) string {
		return `{"generations":[[{"message":{"kwargs":{"content":"` + text + `"}}}]]}`
	}
	t.Run("a session landed before the collector kept any order", func(t *testing.T) {
		zone, post, collect := receiveInto(t)
		tr := trace("1")
		root := "20260920T100000000000Z" + tr
		a, b := tr[:len(tr)-1]+"1", tr[:len(tr)-1]+"2"
		post(`{"post":[{"id":"` + tr + `","trace_id":"` + tr + `","dotted_order":"` + root + `","run_type":"chain","name":"agent","start_time":"2026-09-20T10:00:00Z",` + owner + `,"inputs":{"messages":[{"role":"user","content":"go"}]}},` +
			call(tr, a, tr, root+".20260920T100001000000Z"+a, "1", in("a"), out("A")) + `]}`)
		session := collect().Sessions[0]
		// An older session: whatever the collector might have kept is gone.
		if err := os.Remove(filepath.Join(zone.SessionDir(session), "langsmith.shape.json")); err != nil {
			t.Fatal(err)
		}
		post(`{"post":[` + call(tr, b, tr, root+".20260920T100002000000Z"+b, "2", in("b"), out("B")) + `]}`)
		collect()
		if got := manifestsOf(t, zone, session)[b+":request"].Call; got != b {
			t.Errorf("B's request names %q as its call", got)
		}
		bodiesAreOwn(t, zone, session, 2)
	})

	t.Run("a call whose first fragment landed before its ancestry", func(t *testing.T) {
		zone, post, collector := receiveIntoCollector(t)
		tr := trace("2")
		root := "20260920T100000000000Z" + tr
		tool := tr[:len(tr)-1] + "5"
		a, b := tr[:len(tr)-1]+"1", tr[:len(tr)-1]+"2"
		toolDotted := root + ".20260920T100001000000Z" + tool
		// A's completion, inside a tool nobody has sent. It waits as long as
		// it may, then lands in main.
		post(`{"patch":[` + call(tr, a, tool, toolDotted+".20260920T100002000000Z"+a, "2", "", out("A")) + `]}`)
		var landed langsmith.Landed
		for i := 0; i < 6 && landed.Placed == 0; i++ {
			var err error
			if landed, err = collector.Collect(); err != nil {
				t.Fatal(err)
			}
		}
		if landed.Placed != 1 {
			t.Fatalf("A was not landed after waiting as long as it may: %+v", landed)
		}
		session := landed.Sessions[0]
		files, err := storage.LandedFiles(zone, session)
		if err != nil {
			t.Fatal(err)
		}
		inMain := 0
		for _, f := range files {
			if f.Stream == langsmith.MainStream {
				inMain++
			} else if f.Stream != "" {
				t.Errorf("A landed in stream %q before its ancestry, want main", f.Stream)
			}
		}
		if inMain == 0 {
			t.Fatal("A's completion landed in no stream")
		}
		// Now the ancestry, A's own inputs, and B inside the same tool.
		post(`{"post":[{"id":"` + tr + `","trace_id":"` + tr + `","dotted_order":"` + root + `","run_type":"chain","name":"agent","start_time":"2026-09-20T10:00:00Z","end_time":"2026-09-20T10:00:09Z",` + owner + `,"inputs":{"messages":[{"role":"user","content":"go"}]}},` +
			`{"id":"` + tool + `","trace_id":"` + tr + `","parent_run_id":"` + tr + `","dotted_order":"` + toolDotted + `","run_type":"tool","name":"delegate","start_time":"2026-09-20T10:00:01Z","end_time":"2026-09-20T10:00:08Z",` + owner + `,"outputs":{"output":{"type":"tool","tool_call_id":"call_d","content":"done"}}}]}`)
		if _, err := collector.Collect(); err != nil {
			t.Fatal(err)
		}
		post(`{"post":[` + call(tr, a, tool, toolDotted+".20260920T100002000000Z"+a, "2", in("a"), "") + `,` +
			call(tr, b, tool, toolDotted+".20260920T100003000000Z"+b, "3", in("b"), out("B")) + `]}`)
		if _, err := collector.Collect(); err != nil {
			t.Fatal(err)
		}
		bodiesAreOwn(t, zone, session, 2)
	})

	t.Run("a request replayed after a call that arrived while it waited", func(t *testing.T) {
		zone, post, collector := receiveIntoCollector(t)
		tr := trace("3")
		root := "20260920T100000000000Z" + tr
		a, b := tr[:len(tr)-1]+"1", tr[:len(tr)-1]+"2"
		// A waits for the root. The root arrives with B's completion; A
		// then lands after B, which is the order the assembler sees.
		post(`{"post":[` + call(tr, a, tr, root+".20260920T100001000000Z"+a, "1", in("a"), out("A")) + `]}`)
		if landed, err := collector.Collect(); err != nil || landed.Waiting != 1 {
			t.Fatalf("A should be waiting for the root: %+v %v", landed, err)
		}
		post(`{"post":[{"id":"` + tr + `","trace_id":"` + tr + `","dotted_order":"` + root + `","run_type":"chain","name":"agent","start_time":"2026-09-20T10:00:00Z",` + owner + `,"inputs":{"messages":[{"role":"user","content":"go"}]}}],` +
			`"patch":[` + call(tr, b, tr, root+".20260920T100002000000Z"+b, "2", "", out("B")) + `]}`)
		landed, err := collector.Collect()
		if err != nil {
			t.Fatal(err)
		}
		session := landed.Sessions[0]
		post(`{"post":[` + call(tr, b, tr, root+".20260920T100002000000Z"+b, "2", in("b"), "") + `]}`)
		if _, err := collector.Collect(); err != nil {
			t.Fatal(err)
		}
		m := manifestsOf(t, zone, session)
		for _, id := range []string{a, b} {
			if got := m[id+":request"].Call; got != id {
				t.Errorf("%s's request names %q as its call", id, got)
			}
		}
		bodiesAreOwn(t, zone, session, 2)
	})

	t.Run("a call delivered again after another has followed it", func(t *testing.T) {
		zone, post, collect := receiveInto(t)
		tr := trace("4")
		root := "20260920T100000000000Z" + tr
		a, b, c := tr[:len(tr)-1]+"1", tr[:len(tr)-1]+"2", tr[:len(tr)-1]+"3"
		one := func(id, at, text string) string {
			return `{"post":[` + call(tr, id, tr, root+".20260920T10000"+at+"000000Z"+id, at, in(text), out(strings.ToUpper(text))) + `]}`
		}
		post(`{"post":[{"id":"` + tr + `","trace_id":"` + tr + `","dotted_order":"` + root + `","run_type":"chain","name":"agent","start_time":"2026-09-20T10:00:00Z",` + owner + `,"inputs":{"messages":[{"role":"user","content":"go"}]}}]}`)
		post(one(a, "1", "a"))
		session := collect().Sessions[0]
		post(one(b, "2", "b"))
		collect()
		post(one(a, "1", "a")) // A again, as a client retries
		collect()
		post(one(c, "3", "c"))
		collect()
		m := manifestsOf(t, zone, session)
		if got := m[c+":request"].Call; got != c {
			t.Errorf("C's request names %q as its call", got)
		}
		if len(m) != 6 {
			t.Errorf("%d bodies landed for three calls, want 6", len(m))
		}
		if got := bodyRecordsLanded(t, zone, session); got != 6 {
			t.Errorf("%d body records landed for three calls, want 6: a redelivery lands no second record", got)
		}
		bodiesAreOwn(t, zone, session, 3)
	})
}

// bodiesAreOwn parses the session and checks that every call in the fold
// carries its own request and its own response, and no other call's.
func bodiesAreOwn(t *testing.T, zone *storage.Zone, session string, calls int) {
	t.Helper()
	if _, err := parse.Session(zone, parse.Options{Conversation: session, Session: session}); err != nil {
		t.Fatal(err)
	}
	view := fold(t, zone, session)
	if got := countKind(view, model.KindLLMCall); got != calls {
		t.Errorf("%d calls in the fold, want %d", got, calls)
	}
	for _, n := range view.Nodes {
		if n.Kind != model.KindLLMCall {
			continue
		}
		bodies, _ := sessionflow.ProviderBodiesOf(n.Attrs)
		// One of each role, and each the call's own record: two references
		// to its own response would otherwise pass as a request and a
		// response.
		byRole := map[string]int{}
		for _, y := range bodies {
			byRole[y.Role]++
			want := strings.TrimPrefix(n.ID, "call/") + ":" + y.Role
			if id := recordIDAt(t, zone, session, y.Ref); id != want {
				t.Errorf("%s carries %q, which is another call's", n.ID, id)
			}
		}
		if len(bodies) != 2 || byRole[sessionflow.RoleRequest] != 1 || byRole[sessionflow.RoleResponse] != 1 {
			t.Errorf("%s carries %v, want its request and its response, one each", n.ID, bodies)
		}
	}
}

// bodyRecordsLanded counts the provider body records a session holds, as
// landed: a record landed twice counts twice here and once in manifestsOf,
// which keys by id.
func bodyRecordsLanded(t *testing.T, zone *storage.Zone, session string) int {
	t.Helper()
	files, err := storage.LandedFiles(zone, session)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, f := range files {
		if f.Stream != "" || f.RunID != "" {
			continue
		}
		fh, err := os.Open(f.Path)
		if err != nil {
			t.Fatal(err)
		}
		if reader, err := sessiondata.NewReader(fh); err == nil {
			for {
				if _, err := reader.Next(); err != nil {
					break
				}
				n++
			}
		}
		fh.Close()
	}
	return n
}
