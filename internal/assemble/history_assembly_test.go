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
	"fmt"
	"runtime"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// A history fixture: a session built record by record, each landing in a file
// of its own, so landing order is the order a test writes them in.
type hfix struct {
	t   *testing.T
	ix  *index.Index
	seq uint32
}

func newHfix(t *testing.T) *hfix { return &hfix{t: t, ix: index.New("s")} }

func (f *hfix) land(hdr *sessiondata.Header, rec *sessiondata.Record) {
	f.seq++
	e, blocks, body := index.FromRecord(f.ix, hdr, rec, f.seq, 1)
	f.ix.AppendRecord(e, blocks, body)
}

func transcriptOf(stream string) *sessiondata.Header {
	return &sessiondata.Header{Kind: sessiondata.KindTranscript, Session: "s", Stream: stream}
}

var bodies = &sessiondata.Header{Kind: sessiondata.KindProviderBody, Session: "s", Src: "."}

func (f *hfix) input(stream, run string) {
	f.land(transcriptOf(stream), &sessiondata.Record{ID: run + "-in", From: sessiondata.FromExternal, Run: run,
		Flags: []string{"external_input"}, Parts: []sessiondata.Part{{Kind: sessiondata.PartText, Text: "q", State: "available"}}})
}

// call lands a model call's record; flags add to "finished".
func (f *hfix) call(stream, run, call string, flags ...string) {
	f.land(transcriptOf(stream), &sessiondata.Record{ID: call + "-r", From: sessiondata.FromAgent, Call: call, Run: run,
		Flags: append([]string{"finished"}, flags...), Parts: []sessiondata.Part{{Kind: sessiondata.PartText, Text: "a", State: "available"}}})
}

func (f *hfix) manifest(id, call, role string, fields map[string]any) {
	m := map[string]any{"schema": providerbody.Schema, "role": role, "sha256": "d", "bytes": 2, "depth": 0,
		"segments": []any{map[string]any{"lit": "{}"}}}
	for k, v := range fields {
		m[k] = v
	}
	data, err := json.Marshal(m)
	if err != nil {
		f.t.Fatal(err)
	}
	f.land(bodies, &sessiondata.Record{ID: id, Call: call, Parts: []sessiondata.Part{{Kind: sessiondata.PartData, Data: data, State: "available"}}})
}

// request lands a call's request with its history fields; a nil list leaves
// the body as one landed before asz wrote any.
func (f *hfix) request(call, form string, fields map[string]any) {
	all := map[string]any{}
	if form != "" {
		all["history_form"] = form
	}
	for k, v := range fields {
		all[k] = v
	}
	f.manifest(call+":request", call, providerbody.RoleRequest, all)
}

func (f *hfix) response(call, form string, reply map[string]any) {
	f.manifest(call+":response", call, providerbody.RoleResponse, map[string]any{"history_form": form, "reply": reply})
}

// e is an entry: an id when id is not 0, and its digest.
func e(id, digest uint64) map[string]any {
	out := map[string]any{"digest": fmt.Sprintf("%016x", digest)}
	if id != 0 {
		out["id"] = fmt.Sprintf("%016x", id)
	}
	return out
}

func whole(entries ...map[string]any) map[string]any {
	list := make([]any, len(entries))
	for i, x := range entries {
		list[i] = x
	}
	return map[string]any{"messages": map[string]any{"entries": list}}
}

// turn lands one LangChain turn: a turn call with its request and its reply.
func (f *hfix) turn(stream, call string, reply map[string]any, entries ...map[string]any) {
	f.call(stream, "run", call, model.ClassTurnCall)
	f.request(call, providerbody.FormLangChain, whole(entries...))
	f.response(call, providerbody.FormLangChain, reply)
}

// assembled is every node's attributes by id, and the order of the nodes
// under each parent.
func (f *hfix) assembled(through uint64) (map[string]map[string]any, map[string][]string) {
	f.t.Helper()
	res, err := Session(f.ix, Options{Conversation: "s", Session: "s", ThroughSeq: through})
	if err != nil {
		f.t.Fatal(err)
	}
	attrs := map[string]map[string]any{}
	view := sessionflow.NewView("s")
	for i := range res.Nodes {
		n := &res.Nodes[i]
		var a map[string]any
		_ = json.Unmarshal(n.Attrs, &a)
		if a == nil {
			a = map[string]any{}
		}
		a["kind"], a["parent"] = n.Kind, n.Parent
		attrs[n.ID] = a
		view.Nodes[n.ID] = n
	}
	order := map[string][]string{}
	for _, n := range res.Nodes {
		for _, k := range view.Children(n.ID) {
			order[n.ID] = append(order[n.ID], k.ID)
		}
	}
	return attrs, order
}

func check(t *testing.T, attrs map[string]map[string]any, id string, want map[string]any) {
	t.Helper()
	got, ok := attrs[id]
	if !ok {
		t.Fatalf("no node %s", id)
	}
	for k, v := range want {
		if fmt.Sprint(got[k]) != fmt.Sprint(v) {
			t.Errorf("%s: %s is %v, want %v", id, k, got[k], v)
		}
	}
}

var (
	sys  = e(0, 0x5)
	q1   = e(1, 0x10)
	a1   = e(2, 0x20)
	q2   = e(3, 0x30)
	a2   = e(4, 0x40)
	q3   = e(5, 0x50)
	a1x  = e(2, 0x21) // a1 sent back altered
	none = map[string]any{"unavailable": "no_single_reply"}
)

// TestAHistoryThatGrowsIsKeptAndOneTrimmedChanges: the two verdicts, the
// reply's status on each, and the card with what it counts, where it stands
// and what it names.
func TestAHistoryThatGrowsIsKeptAndOneTrimmedChanges(t *testing.T) {
	f := newHfix(t)
	f.input("main", "run")
	f.turn("main", "c1", a1, sys, q1)
	f.turn("main", "c2", a2, sys, q1, a1, q2)
	f.turn("main", "c3", none, sys, q2, a2, q3) // q1 and a1 trimmed
	attrs, order := f.assembled(0)
	check(t, attrs, "call/c1", map[string]any{"history": "first", "history_messages": 2})
	check(t, attrs, "call/c2", map[string]any{"history": "kept", "history_from": "call/c1", "history_reply": "back", "history_messages": 4})
	check(t, attrs, "call/c3", map[string]any{"history": "changed", "history_from": "call/c2", "history_reply": "back"})
	check(t, attrs, "history/c3", map[string]any{"kind": model.KindHistoryChange, "from_call": "call/c2", "to_call": "call/c3",
		"kept": 2, "changed": 0, "dropped": 2, "added": 2, "order_changed": "no", "reply": "back", "cause": "unavailable",
		"parent": attrs["call/c3"]["parent"]})
	if _, ok := attrs["history/c2"]; ok {
		t.Fatal("a kept history drew a card")
	}
	// The card comes just before its call among the call's siblings.
	siblings := order[fmt.Sprint(attrs["call/c3"]["parent"])]
	at := map[string]int{}
	for i, id := range siblings {
		at[id] = i
	}
	if at["history/c3"]+1 != at["call/c3"] {
		t.Fatalf("siblings %v: the card is not just before its call", siblings)
	}
}

// TestAShorterHistoryChanges: a history that is the one before it with its
// end taken off is not kept, though every message it holds was there.
func TestAShorterHistoryChanges(t *testing.T) {
	f := newHfix(t)
	f.input("main", "run")
	f.turn("main", "c1", a1, sys, q1, q2)
	f.turn("main", "c2", none, sys, q1)
	attrs, _ := f.assembled(0)
	check(t, attrs, "call/c2", map[string]any{"history": "changed", "history_from": "call/c1", "history_reply": "not_found"})
	check(t, attrs, "history/c2", map[string]any{"kept": 2, "changed": 0, "dropped": 1, "added": 0, "order_changed": "no"})
}

// TestAListIsReadOnlyOverABaseThatHolds: a base must be a request, and one
// whose own list holds what is kept from it, however little a later list
// takes of it.
func TestAListIsReadOnlyOverABaseThatHolds(t *testing.T) {
	t.Run("a base that is a response", func(t *testing.T) {
		f := newHfix(t)
		f.input("main", "run")
		f.call("main", "run", "c1", model.ClassTurnCall)
		f.request("c1", providerbody.FormLangChain, whole(sys, q1))
		f.manifest("c1:response", "c1", providerbody.RoleResponse, map[string]any{"history_form": providerbody.FormLangChain,
			"reply": a1, "messages": map[string]any{"entries": []any{sys, q1, a1}}})
		f.call("main", "run", "c2", model.ClassTurnCall)
		f.request("c2", providerbody.FormLangChain, map[string]any{"messages": map[string]any{"base": "c1:response", "keep": 3, "add": []any{q2}}})
		attrs, _ := f.assembled(0)
		check(t, attrs, "call/c2", map[string]any{"history": "unavailable", "history_reason": "messages:unrecognised_shape"})
	})
	t.Run("a base that keeps more than its own base holds", func(t *testing.T) {
		f := newHfix(t)
		f.input("main", "run")
		f.turn("main", "c1", none, sys)
		f.call("main", "run", "c2", model.ClassTurnCall)
		f.request("c2", providerbody.FormLangChain, map[string]any{"messages": map[string]any{"base": "c1:request", "keep": 2, "add": []any{q1}}})
		f.call("main", "run", "c3", model.ClassTurnCall)
		f.request("c3", providerbody.FormLangChain, map[string]any{"messages": map[string]any{"base": "c2:request", "keep": 1, "add": []any{q2}}})
		attrs, _ := f.assembled(0)
		check(t, attrs, "call/c2", map[string]any{"history": "unavailable", "history_reason": "messages:unrecognised_shape"})
		// c3 keeps only the entry c1 does hold, but c2's list does not.
		check(t, attrs, "call/c3", map[string]any{"history": "unavailable", "history_reason": "messages:unrecognised_shape"})
	})
}

// TestWhatACallIsComparedWith holds the table's precedence and the calls a
// predecessor skips or stops at.
func TestWhatACallIsComparedWith(t *testing.T) {
	t.Run("an internal call is not compared and is skipped", func(t *testing.T) {
		f := newHfix(t)
		f.input("main", "run")
		f.turn("main", "c1", a1, sys, q1)
		f.call("main", "run", "s1", model.ClassInternalCall)
		f.request("s1", providerbody.FormLangChain, whole(q1))
		f.turn("main", "c2", a2, sys, q1, a1, q2)
		attrs, _ := f.assembled(0)
		check(t, attrs, "call/s1", map[string]any{"history": "not_compared", "history_reason": "internal_call"})
		check(t, attrs, "call/c2", map[string]any{"history": "kept", "history_from": "call/c1"})
	})
	t.Run("a synthetic call is no predecessor", func(t *testing.T) {
		f := newHfix(t)
		f.input("main", "run")
		f.turn("main", "c1", a1, sys, q1)
		f.call("main", "run", "x1", "synthetic")
		f.turn("main", "c2", a2, sys, q1, a1, q2)
		attrs, _ := f.assembled(0)
		check(t, attrs, "call/c2", map[string]any{"history": "kept", "history_from": "call/c1"})
	})
	t.Run("a call of unknown class, as one collected before the class", func(t *testing.T) {
		f := newHfix(t)
		f.input("main", "run")
		f.turn("main", "c1", a1, sys, q1)
		f.call("main", "run", "u1")
		f.request("u1", providerbody.FormLangChain, whole(sys, q1, a1, q2))
		f.turn("main", "c2", a2, sys, q1, a1, q2)
		attrs, _ := f.assembled(0)
		check(t, attrs, "call/u1", map[string]any{"history": "unavailable", "history_reason": "class_unknown", "history_messages": 4})
		// Never skipped: had it been a turn, c2 would be compared with c1.
		check(t, attrs, "call/c2", map[string]any{"history": "unavailable", "history_reason": "earlier_class_unknown"})
	})
	t.Run("a predecessor with no request, or none written with the fields", func(t *testing.T) {
		f := newHfix(t)
		f.input("main", "run")
		f.call("main", "run", "n1", model.ClassTurnCall)
		f.turn("main", "c1", a1, sys, q1)
		f.call("main", "run", "o1", model.ClassTurnCall)
		f.request("o1", "", nil) // landed before asz wrote any history field
		f.turn("main", "c2", a2, sys, q1, a1, q2)
		f.turn("main", "c3", none, sys, q1, a1, q2, a2, q3)
		attrs, _ := f.assembled(0)
		check(t, attrs, "call/c1", map[string]any{"history": "unavailable", "history_reason": "earlier_no_request"})
		if _, ok := attrs["call/o1"]["history"]; ok {
			t.Fatal("a request landed before the fields got a history")
		}
		// The old call, then a new one that cannot be compared with it, then
		// one kept against that: the middle one still says how long it is.
		check(t, attrs, "call/c2", map[string]any{"history": "unavailable", "history_reason": "earlier_messages:absent", "history_messages": 4})
		check(t, attrs, "call/c3", map[string]any{"history": "kept", "history_from": "call/c2", "history_reply": "back"})
	})
	t.Run("a predecessor's list that is unavailable", func(t *testing.T) {
		f := newHfix(t)
		f.input("main", "run")
		f.call("main", "run", "p1", model.ClassTurnCall)
		f.request("p1", providerbody.FormLangChain, map[string]any{"messages": map[string]any{"unavailable": "plural_prompts"}})
		f.turn("main", "c1", a1, sys, q1)
		attrs, _ := f.assembled(0)
		check(t, attrs, "call/p1", map[string]any{"history": "unavailable", "history_reason": "messages:plural_prompts"})
		check(t, attrs, "call/c1", map[string]any{"history": "unavailable", "history_reason": "earlier_messages:plural_prompts"})
	})
	t.Run("a predecessor of another form", func(t *testing.T) {
		f := newHfix(t)
		f.input("main", "run")
		f.call("main", "run", "k1")
		f.request("k1", providerbody.FormClaudeCode, whole(q1))
		f.turn("main", "c1", a1, sys, q1)
		attrs, _ := f.assembled(0)
		check(t, attrs, "call/c1", map[string]any{"history": "unavailable", "history_reason": "form_mismatch"})
	})
}

// TestTheEarlierReplysStatus: a retry finds no reply, a reply of another form
// is not compared, a kept history's reply is looked for only after the
// earlier history, and on a changed one a reply that could be an earlier
// message is ambiguous.
func TestTheEarlierReplysStatus(t *testing.T) {
	t.Run("a retry sends the same request again", func(t *testing.T) {
		f := newHfix(t)
		f.input("main", "run")
		f.turn("main", "c1", a1, sys, q1)
		f.turn("main", "c2", none, sys, q1)
		attrs, _ := f.assembled(0)
		check(t, attrs, "call/c2", map[string]any{"history": "kept", "history_reply": "not_found"})
	})
	t.Run("a reply of another form", func(t *testing.T) {
		f := newHfix(t)
		f.input("main", "run")
		f.call("main", "run", "c1", model.ClassTurnCall)
		f.request("c1", providerbody.FormLangChain, whole(sys, q1))
		f.response("c1", providerbody.FormClaudeCode, a1)
		f.turn("main", "c2", none, sys, q1, a1, q2)
		attrs, _ := f.assembled(0)
		check(t, attrs, "call/c2", map[string]any{"history": "kept", "history_reply": "unavailable", "history_reply_reason": "form_mismatch"})
	})
	t.Run("a kept history's reply is looked for after the earlier one", func(t *testing.T) {
		f := newHfix(t)
		f.input("main", "run")
		f.turn("main", "c1", a1, sys, a1) // the earlier history already holds a message equal to the reply
		f.turn("main", "c2", none, sys, a1, q2)
		attrs, _ := f.assembled(0)
		check(t, attrs, "call/c2", map[string]any{"history": "kept", "history_reply": "not_found"})
	})
	t.Run("a reply sent back altered", func(t *testing.T) {
		f := newHfix(t)
		f.input("main", "run")
		f.turn("main", "c1", a1, sys, q1)
		f.turn("main", "c2", none, sys, q1, a1x, q2)
		attrs, _ := f.assembled(0)
		check(t, attrs, "call/c2", map[string]any{"history": "kept", "history_reply": "back_changed"})
	})
	t.Run("a changed history's reply equal to an earlier message", func(t *testing.T) {
		f := newHfix(t)
		f.input("main", "run")
		f.turn("main", "c1", a1, sys, q1, a1)
		f.turn("main", "c2", none, sys, a1, q2)
		attrs, _ := f.assembled(0)
		check(t, attrs, "call/c2", map[string]any{"history": "changed", "history_reply": "unavailable", "history_reply_reason": "ambiguous"})
		check(t, attrs, "history/c2", map[string]any{"reply": "unavailable", "reply_reason": "ambiguous"})
	})
}

// TestAHistoryOverTheBoundIsNotCompared: a whole list over the entry bound,
// and a list against a base whose whole list is over it, which keeps less.
func TestAHistoryOverTheBoundIsNotCompared(t *testing.T) {
	defer func(entries, links int) { maxHistoryEntries, maxHistoryLinks = entries, links }(maxHistoryEntries, maxHistoryLinks)
	maxHistoryEntries = 3
	f := newHfix(t)
	f.input("main", "run")
	f.turn("main", "c1", a1, sys, q1, a1, q2) // four entries, one over
	f.call("main", "run", "c2", model.ClassTurnCall)
	f.request("c2", providerbody.FormLangChain, map[string]any{"messages": map[string]any{"base": "c1:request", "keep": 1, "add": []any{q3}}})
	f.response("c2", providerbody.FormLangChain, none)
	attrs, _ := f.assembled(0)
	check(t, attrs, "call/c1", map[string]any{"history": "unavailable", "history_reason": "messages:too_large"})
	// c2 keeps one entry of c1's list and adds one: its own history fits.
	check(t, attrs, "call/c2", map[string]any{"history_messages": 2, "history": "unavailable", "history_reason": "earlier_messages:too_large"})
}

// A Claude Code turn: entries carry no id, and a request may start or
// continue a provider thread.
func (f *hfix) ccTurn(stream, call string, thread map[string]any, system string, reply map[string]any, entries ...map[string]any) {
	f.call(stream, "run", call)
	fields := whole(entries...)
	if thread != nil {
		fields["thread"] = thread
	}
	if system != "" {
		fields["system"] = system
	}
	f.request(call, providerbody.FormClaudeCode, fields)
	if reply != nil {
		f.response(call, providerbody.FormClaudeCode, reply)
	}
}

var (
	u1, r1, u2, r2, u3 = e(0, 0x101), e(0, 0x102), e(0, 0x103), e(0, 0x104), e(0, 0x105)
	create             = map[string]any{"type": "create"}
	sysA               = "000000000000000a"
)

func cont(call string) map[string]any { return map[string]any{"type": "continue", "previous": call} }

// TestAThreadsHistoryIsBuiltFromWhatItContinues covers the thread cases: a
// continue from the call before, a resumed create, a rewind to an older
// call, a kept continue of an older call, and the threads that cannot be
// followed.
func TestAThreadsHistoryIsBuiltFromWhatItContinues(t *testing.T) {
	t.Run("continued, then resumed", func(t *testing.T) {
		f := newHfix(t)
		f.input("main", "run")
		f.ccTurn("main", "c1", create, sysA, r1, u1)
		f.ccTurn("main", "c2", cont("c1"), "", r2, u2)
		f.ccTurn("main", "c3", create, sysA, none, u1, r1, u2, r2, u3)
		attrs, _ := f.assembled(0)
		check(t, attrs, "call/c2", map[string]any{"history": "kept", "history_messages": 3, "history_reply": "back", "system_changed": "no"})
		check(t, attrs, "call/c3", map[string]any{"history": "kept", "history_messages": 5, "history_reply": "back", "system_changed": "no"})
	})
	t.Run("a rewind to an older call", func(t *testing.T) {
		f := newHfix(t)
		f.input("main", "run")
		f.ccTurn("main", "c1", create, sysA, r1, u1)
		f.ccTurn("main", "c2", cont("c1"), "", r2, u2)
		f.ccTurn("main", "c3", cont("c1"), "", none, u3) // c2's message and reply are gone
		attrs, _ := f.assembled(0)
		check(t, attrs, "call/c3", map[string]any{"history": "changed", "history_from": "call/c2", "history_messages": 3})
		check(t, attrs, "history/c3", map[string]any{"kept": 2, "dropped": 1, "added": 1})
	})
	t.Run("a kept continue of a call older than its predecessor", func(t *testing.T) {
		f := newHfix(t)
		f.input("main", "run")
		f.ccTurn("main", "c1", create, sysA, r1, u1)
		f.ccTurn("main", "c2", cont("c1"), "", r2, u2)
		f.ccTurn("main", "c3", cont("c1"), "", none, u2, r2, u3) // sends c2's again
		attrs, _ := f.assembled(0)
		check(t, attrs, "call/c3", map[string]any{"history": "kept", "history_messages": 5, "history_reply": "back"})
	})
	t.Run("threads that cannot be followed", func(t *testing.T) {
		f := newHfix(t)
		f.input("main", "run")
		f.ccTurn("main", "c1", create, sysA, nil, u1) // no response: no reply
		f.ccTurn("main", "c2", cont("c1"), "", r2, u2)
		f.ccTurn("main", "c3", cont("gone"), "", r2, u3)
		f.input("side", "srun")
		f.ccTurn("side", "s1", cont("c1"), "", nil, u1) // another stream's call
		f.ccTurn("main", "c4", map[string]any{"type": "fork"}, "", nil, u1)
		attrs, _ := f.assembled(0)
		check(t, attrs, "call/c2", map[string]any{"history": "unavailable", "history_reason": "thread_reply:no_reply"})
		check(t, attrs, "call/c3", map[string]any{"history": "unavailable", "history_reason": "thread_unresolved"})
		check(t, attrs, "call/s1", map[string]any{"history": "unavailable", "history_reason": "thread_unresolved"})
		check(t, attrs, "call/c4", map[string]any{"history": "unavailable", "history_reason": "messages:unrecognised_shape"})
	})
	t.Run("an unknown system goes on through the thread", func(t *testing.T) {
		f := newHfix(t)
		f.input("main", "run")
		f.ccTurn("main", "c1", create, sysA, r1, u1)
		f.ccTurn("main", "c2", cont("c1"), "000000000000000b", r2, u2) // a system block of its own
		f.ccTurn("main", "c3", cont("c2"), "", r1, u3)
		f.ccTurn("main", "c4", create, sysA, none, u1, r1, u2, r2, u3, r1)
		attrs, _ := f.assembled(0)
		check(t, attrs, "call/c2", map[string]any{"history": "kept", "system_changed": "unavailable"})
		check(t, attrs, "call/c3", map[string]any{"history": "kept", "system_changed": "unavailable"})
		check(t, attrs, "call/c4", map[string]any{"history": "kept", "system_changed": "unavailable"})
	})
}

// TestALimitOneWalkReachesIsNotKeptAgainstAnotherCall. With the link bound
// at one, the chain's tail is too large; a call nearer its start is not, and
// stays measurable though the tail, landed first, was measured first.
func TestALimitOneWalkReachesIsNotKeptAgainstAnotherCall(t *testing.T) {
	defer func(links int) { maxHistoryLinks = links }(maxHistoryLinks)
	maxHistoryLinks = 1
	f := newHfix(t)
	f.input("main", "run")
	f.ccTurn("main", "c3", cont("c2"), "", none, u3)
	f.ccTurn("main", "c1", create, sysA, r1, u1)
	f.ccTurn("main", "c2", cont("c1"), "", r2, u2)
	attrs, _ := f.assembled(0)
	check(t, attrs, "call/c3", map[string]any{"history": "unavailable", "history_reason": "messages:too_large"})
	check(t, attrs, "call/c2", map[string]any{"history": "kept", "history_messages": 3})
}

// TestASystemWalkAtTheLinkBoundReachesItsStart. A thread of exactly as many
// links as the bound is measured, so its system is read at its start, though
// its last call lands first and is walked with nothing known; and that walk
// leaves nothing behind for the calls it passed.
func TestASystemWalkAtTheLinkBoundReachesItsStart(t *testing.T) {
	defer func(links int) { maxHistoryLinks = links }(maxHistoryLinks)
	maxHistoryLinks = 2
	f := newHfix(t)
	f.input("main", "run")
	f.ccTurn("main", "c3", cont("c2"), "", none, u3)
	f.ccTurn("main", "c1", create, sysA, r1, u1)
	f.ccTurn("main", "c2", cont("c1"), "", r2, u2)
	attrs, _ := f.assembled(0)
	check(t, attrs, "call/c1", map[string]any{"history": "changed", "history_from": "call/c3", "system_changed": "no"})
	check(t, attrs, "call/c2", map[string]any{"history": "kept", "history_from": "call/c1", "system_changed": "no"})
}

// TestAListKeepingTheFrontOfALongerOneCopiesOnlyThatFront. A list over a
// base that holds many entries takes the front it keeps without copying the
// rest.
func TestAListKeepingTheFrontOfALongerOneCopiesOnlyThatFront(t *testing.T) {
	const many = 50000
	f := newHfix(t)
	f.input("main", "run")
	f.turn("main", "c1", none, sys)
	add := make([]any, many)
	for i := range add {
		add[i] = e(uint64(100+i), uint64(100+i))
	}
	f.call("main", "run", "c2", model.ClassTurnCall)
	f.request("c2", providerbody.FormLangChain, map[string]any{"messages": map[string]any{"base": "c1:request", "keep": 1, "add": add}})
	f.call("main", "run", "c3", model.ClassTurnCall)
	f.request("c3", providerbody.FormLangChain, map[string]any{"messages": map[string]any{"base": "c2:request", "keep": 2, "add": []any{q1}}})
	b := &builder{ix: f.ix}
	h := &historian{b: b, bodyOf: map[uint32]int{}, lists: map[int]ownList{}}
	for i := range f.ix.Bodies {
		h.bodyOf[f.ix.Entries[f.ix.Bodies[i].Entry].Record] = i
	}
	row := len(f.ix.Bodies) - 1
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	list, why := h.own(row)
	runtime.ReadMemStats(&after)
	if why != "" || len(list) != 3 || list[1].ID != 100 || list[2] != f.ix.Messages[len(f.ix.Messages)-1] {
		t.Fatalf("c3's list is %v (%q)", list, why)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 64<<10 {
		t.Fatalf("reading three entries allocated %d bytes", grew)
	}
}

// TestLateEvidenceRevisesTheComparison: a response that lands late gives the
// reply, and the history a thread builds from it; a late record of a call's
// class takes the call out of its stream's history, and the card it drew
// goes.
func TestLateEvidenceRevisesTheComparison(t *testing.T) {
	f := newHfix(t)
	f.input("main", "run")
	f.call("main", "run", "c1")
	f.request("c1", providerbody.FormClaudeCode, map[string]any{"thread": create, "messages": map[string]any{"entries": []any{u1}}})
	f.ccTurn("main", "c2", cont("c1"), "", nil, u2)
	before := uint64(f.seq)
	f.response("c1", providerbody.FormClaudeCode, r1)
	early, _ := f.assembled(before)
	check(t, early, "call/c2", map[string]any{"history": "unavailable", "history_reason": "thread_reply:no_reply"})
	late, _ := f.assembled(0)
	check(t, late, "call/c2", map[string]any{"history": "kept", "history_reply": "back"})

	g := newHfix(t)
	g.input("main", "run")
	g.turn("main", "c1", a1, sys, q1)
	g.turn("main", "x1", none, q3) // read as a turn, so c2 is compared with it
	g.turn("main", "c2", none, sys, q1, a1, q2)
	cut := uint64(g.seq)
	// A later arrival of x1, under its own record id, says it was the framework's.
	g.land(transcriptOf("main"), &sessiondata.Record{ID: "x1-late", From: sessiondata.FromAgent, Call: "x1", Run: "run",
		Flags: []string{"finished", model.ClassInternalCall}})
	early, _ = g.assembled(cut)
	check(t, early, "call/c2", map[string]any{"history": "changed", "history_from": "call/x1"})
	if _, ok := early["history/c2"]; !ok {
		t.Fatal("no card before the late record")
	}
	late, _ = g.assembled(0)
	check(t, late, "call/c2", map[string]any{"history": "kept", "history_from": "call/c1"})
	if _, ok := late["history/c2"]; ok {
		t.Fatal("the card stayed after its pair was no longer changed")
	}
}
