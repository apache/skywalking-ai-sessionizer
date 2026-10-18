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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/langsmith"
	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/skeleton"
)

// heldInOrder is heldBodies with the record ids in the order they landed.
func heldInOrder(t *testing.T, zone *storage.Zone, session string) (*providerbody.Session, []string) {
	t.Helper()
	held := providerbody.NewSession()
	files, err := storage.LandedFiles(zone, session)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, lf := range files {
		if lf.Stream != "" || lf.RunID != "" {
			continue
		}
		f, err := sessiondata.Open(lf.Path)
		if err != nil {
			t.Fatal(err)
		}
		r, err := sessiondata.NewReader(f)
		if err != nil {
			f.Close()
			t.Fatal(err)
		}
		for {
			rec, err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := held.Add(rec); err != nil && !errors.Is(err, providerbody.ErrRepeat) {
				t.Fatalf("%s: %v", rec.ID, err)
			}
			order = append(order, rec.ID)
		}
		f.Close()
	}
	return held, order
}

// TestEveryLangChainMessageSentAgainHasOneForm holds the measurement the
// langchain/1 form rests on, over every capture as the collector lands it:
// with nothing left out of a message, a message sent again under the same id
// has the same digest, and a reply comes back equal or altered by the
// framework, never as something the form made different. The numbers are the
// ones measured with the capture script when the form was designed; a new
// capture changes them, and this test says by how much.
func TestEveryLangChainMessageSentAgainHasOneForm(t *testing.T) {
	cases, err := os.ReadDir(corpus)
	if err != nil {
		t.Skipf("no corpus: %v", err)
	}
	type key struct{ kase, id string }
	var calls, messages int
	forms := map[key]map[string]bool{}
	sent := map[key]int{}
	unavailable := map[string]int{}
	outcome := map[string]int{}
	for _, c := range cases {
		if !c.IsDir() {
			continue
		}
		zone, sessions := land(t, c.Name())
		for _, session := range sessions {
			held, order := heldInOrder(t, zone, session)
			// The requests in the order they landed, each with its list.
			var requests []string
			lists := map[string][]providerbody.Entry{}
			for _, id := range order {
				m, _ := held.Manifest(id)
				if m.Role != providerbody.RoleRequest {
					continue
				}
				if m.HistoryForm != providerbody.FormLangChain {
					t.Fatalf("%s/%s: form %q", c.Name(), id, m.HistoryForm)
				}
				calls++
				list, _, err := held.ListOf(id)
				if err != nil {
					unavailable["request "+m.Messages.Unavailable]++
					continue
				}
				requests = append(requests, id)
				lists[id] = list
				messages += len(list)
				for _, e := range list {
					if e.ID == "" {
						continue
					}
					k := key{c.Name(), e.ID}
					if forms[k] == nil {
						forms[k] = map[string]bool{}
					}
					forms[k][e.Digest] = true
					sent[k]++
				}
			}
			// Each reply, looked for in the requests of the calls after its own.
			inOrder := make([][]providerbody.Entry, len(requests))
			for i, id := range requests {
				inOrder[i] = lists[id]
			}
			for i, id := range requests {
				m, ok := held.Manifest(strings.TrimSuffix(id, ":request") + ":response")
				if !ok {
					continue
				}
				r := m.Reply
				if r == nil || r.Unavailable != "" {
					unavailable["reply"]++
					continue
				}
				outcome[replyOutcome(providerbody.Entry{ID: r.ID, Digest: r.Digest}, inOrder, i)]++
			}
		}
	}
	repeated, differing := 0, 0
	for k, n := range sent {
		if n > 1 {
			repeated++
			if len(forms[k]) > 1 {
				differing++
				t.Errorf("%s: message %s was sent with %d forms", k.kase, k.id, len(forms[k]))
			}
		}
	}
	t.Logf("%d calls, %d messages, %d ids, %d sent more than once, replies %v, unavailable %v",
		calls, messages, len(sent), repeated, outcome, unavailable)
	if calls != 79 || messages != 565 || len(sent) != 127 || repeated != 80 || differing != 0 {
		t.Errorf("measured 79 calls, 565 messages, 127 ids and 80 sent more than once; got %d, %d, %d and %d",
			calls, messages, len(sent), repeated)
	}
	if outcome["back"] != 47 || outcome["back changed"] != 3 || outcome["not sent back"] != 29 || len(unavailable) != 0 {
		t.Errorf("measured 47 replies back, 3 back changed and 29 not sent back, none unavailable; got %v, unavailable %v",
			outcome, unavailable)
	}
}

// TestAModelCallsClassTravelsFromItsStart, through the receiver and two
// collectors, the second reading back the state the first saved. A start
// decides a call's class; its update, which carries no metadata, carries the
// class its start gave; an update whose start's class is not held, as one
// remembered before asz kept it, carries none; and of two starts that
// disagree, internal_call is kept. A reply is landed only beside a request
// of exactly one prompt.
func TestAModelCallsClassTravelsFromItsStart(t *testing.T) {
	zone, post, first := receiveIntoCollector(t)
	const (
		trace  = "31313131-3131-7131-8131-313131313130"
		inner  = "31313131-3131-7131-8131-313131313131"
		turn   = "31313131-3131-7131-8131-313131313132"
		mixed  = "31313131-3131-7131-8131-313131313133"
		plural = "31313131-3131-7131-8131-313131313134"
		lone   = "31313131-3131-7131-8131-313131313135"
		odd    = "31313131-3131-7131-8131-313131313136"
	)
	root := "20260920T100000000000Z" + trace
	owner := `"session_name":"asz"`
	message := `{"lc":1,"type":"constructor","id":["langchain","schema","messages","HumanMessage"],"kwargs":{"content":"q","id":"h1"}}`
	reply := `{"generations":[[{"text":"a","message":{"lc":1,"type":"constructor","id":["langchain","schema","messages","AIMessage"],"kwargs":{"content":"a","id":"r1"}}}]]}`
	llm := func(id, extra, inputs string, end bool) string {
		dotted := root + ".20260920T100001000000Z" + id
		s := `{"id":"` + id + `","trace_id":"` + trace + `","parent_run_id":"` + trace + `","dotted_order":"` + dotted +
			`","run_type":"llm","name":"model","start_time":"2026-09-20T10:00:01Z",` + owner
		if extra != "" {
			s += `,"extra":` + extra
		}
		if inputs != "" {
			s += `,"inputs":` + inputs
		}
		if end {
			s += `,"end_time":"2026-09-20T10:00:02Z","outputs":` + reply
		}
		return s + "}"
	}
	patch := func(id string) string {
		return `{"id":"` + id + `","trace_id":"` + trace + `","end_time":"2026-09-20T10:00:02Z","outputs":` + reply + `}`
	}
	internal := `{"metadata":{"thread_id":"t-class","lc_internal_call":"true"}}`
	plain := `{"metadata":{"thread_id":"t-class"}}`
	one := `{"messages":[[` + message + `]]}`
	post(`{"post":[{"id":"` + trace + `","trace_id":"` + trace + `","dotted_order":"` + root + `","run_type":"chain","name":"graph",` +
		`"start_time":"2026-09-20T10:00:00Z",` + owner + `,"extra":` + plain + `,"inputs":{"messages":[]}},` +
		llm(inner, internal, one, false) + `,` + llm(turn, plain, one, false) + `,` + llm(mixed, internal, one, false) + `]}`)
	if _, err := first.Collect(); err != nil {
		t.Fatal(err)
	}

	// The state as a collector before this change would have left it: the
	// turn call's start remembered without its class.
	statePath := filepath.Join(zone.Root(), langsmith.InboxDir, "open.json")
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	runs, _ := state["runs"].(map[string]any)
	held, _ := runs[turn].(map[string]any)
	if held["class"] != "turn_call" {
		t.Fatalf("the turn call's start was held as %v", held)
	}
	delete(held, "class")
	raw, _ = json.Marshal(state)
	if err := os.WriteFile(statePath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	post(`{"post":[` + llm(mixed, plain, one, false) + `,` + llm(plural, plain, `{"messages":[[`+message+`],[`+message+`]]}`, true) + `,` +
		llm(lone, plain, "", true) + `,` + llm(odd, plain, `{"messages":"invalid"}`, true) +
		`],"patch":[` + patch(inner) + `,` + patch(turn) + `,` + patch(mixed) + `]}`)
	second := &langsmith.Collector{Zone: zone, ProviderBodies: true, Now: func() time.Time { return time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC) }}
	landed, err := second.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(landed.Sessions) != 1 {
		t.Fatalf("sessions %v", landed.Sessions)
	}
	session := landed.Sessions[0]

	// Every record of each call, start first, by its flags.
	flags := map[string][][]string{}
	files, err := storage.LandedFiles(zone, session)
	if err != nil {
		t.Fatal(err)
	}
	for _, lf := range files {
		if lf.Stream == "" {
			continue
		}
		f, err := sessiondata.Open(lf.Path)
		if err != nil {
			t.Fatal(err)
		}
		r, err := sessiondata.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		for {
			rec, err := r.Next()
			if err != nil {
				break
			}
			if rec.Call != "" && rec.From == sessiondata.FromAgent {
				flags[rec.Call] = append(flags[rec.Call], rec.Flags)
			}
		}
		f.Close()
	}
	class := func(f []string) string {
		for _, c := range []string{model.ClassInternalCall, model.ClassTurnCall} {
			if slices.Contains(f, c) {
				return c
			}
		}
		return ""
	}
	classes := func(call string) []string {
		var out []string
		for _, f := range flags[call] {
			out = append(out, class(f))
		}
		return out
	}
	// The index reads the same classes off the same records, by its own bits.
	ix, ok, err := index.Load(zone.IndexDir(session), session)
	if err != nil || !ok {
		t.Fatalf("index: %v %v", ok, err)
	}
	indexed := map[string][]string{}
	for _, e := range ix.Entries {
		if e.Kind != index.KindAssistant || e.Call == 0 {
			continue
		}
		c := ""
		switch {
		case e.Flags.Has(index.FlagInternalCall):
			c = model.ClassInternalCall
		case e.Flags.Has(index.FlagTurnCall):
			c = model.ClassTurnCall
		}
		indexed[ix.Strings.String(e.Call)] = append(indexed[ix.Strings.String(e.Call)], c)
	}
	for call, want := range map[string][]string{
		inner: {model.ClassInternalCall, model.ClassInternalCall},
		turn:  {model.ClassTurnCall, ""},
		// The second start says turn_call of itself; the update carries what
		// the state kept of the two: internal_call.
		mixed: {model.ClassInternalCall, model.ClassTurnCall, model.ClassInternalCall},
	} {
		if got := classes(call); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: classes %v, want %v", call, got, want)
		}
		if got := indexed[call]; fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: indexed classes %v, want %v", call, got, want)
		}
	}

	bodies, _ := heldInOrder(t, zone, session)
	for call, why := range map[string]string{plural: providerbody.PluralPrompts, lone: providerbody.NoRequest,
		odd: providerbody.UnrecognisedShape, turn: ""} {
		m, ok := bodies.Manifest(call + ":response")
		if !ok || m.Reply == nil || m.Reply.Unavailable != why {
			t.Errorf("%s: reply %+v, want unavailable %q", call, m, why)
		}
	}
}

// historyCost is what the history fields add to one root: their bytes in the
// manifests beside every byte the provider files hold; what they add to the
// index and to the skeletons, each written both ways and measured on disk;
// and how many lists the cap took.
type historyCost struct {
	manifest, landed, index, skeleton int64
	tooLarge, lists                   int
}

// withoutHistory is a record as a collector before the history fields wrote
// it: a manifest with none of them, and no class flag.
func withoutHistory(t *testing.T, rec *sessiondata.Record) *sessiondata.Record {
	t.Helper()
	out := *rec
	out.Flags = slices.DeleteFunc(slices.Clone(rec.Flags), func(f string) bool {
		return f == model.ClassInternalCall || f == model.ClassTurnCall
	})
	m, err := providerbody.ManifestOf(rec)
	if err != nil {
		return &out
	}
	m.HistoryForm, m.Messages, m.System, m.Thread, m.Reply = "", nil, "", nil, nil
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		t.Fatal(err)
	}
	data := bytes.TrimSuffix(buf.Bytes(), []byte{'\n'})
	out.Parts = append([]sessiondata.Part(nil), rec.Parts...)
	last := &out.Parts[len(out.Parts)-1]
	last.Data, last.Bytes = data, len(data)
	return &out
}

// storageCost writes a session's index, and the skeletons of its provider
// files, as landed and with the history taken out, and returns what the
// history adds to each on disk.
func storageCost(t *testing.T, zone *storage.Zone, session string) (indexBytes, skeletonBytes int64) {
	t.Helper()
	files, err := storage.LandedFiles(zone, session)
	if err != nil {
		t.Fatal(err)
	}
	var sizes [2]struct{ index, skeleton int64 }
	for way := range sizes {
		ix := index.New(session)
		for _, lf := range files {
			f, err := sessiondata.Open(lf.Path)
			if err != nil {
				t.Fatal(err)
			}
			r, err := sessiondata.NewReader(f)
			if err != nil {
				t.Fatal(err)
			}
			hdr := r.Header()
			var file bytes.Buffer
			w, err := sessiondata.NewWriter(&file, &hdr)
			if err != nil {
				t.Fatal(err)
			}
			for row := uint32(1); ; row++ {
				rec, err := r.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if way == 1 {
					rec = withoutHistory(t, rec)
				}
				e, blocks, body := index.FromRecord(ix, &hdr, rec, uint32(lf.Seq), row)
				ix.AppendRecord(e, blocks, body)
				if err := w.Write(rec); err != nil {
					t.Fatal(err)
				}
			}
			f.Close()
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if hdr.Kind == sessiondata.KindProviderBody {
				var sk bytes.Buffer
				if err := skeleton.Write(&sk, file.Bytes(), sessiondata.Removed{}); err != nil {
					t.Fatal(err)
				}
				sizes[way].skeleton += int64(sk.Len())
			}
		}
		dir := t.TempDir()
		if err := ix.Write(dir); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(dir, "entries.bin"))
		if err != nil {
			t.Fatal(err)
		}
		sizes[way].index = info.Size()
	}
	return sizes[0].index - sizes[1].index, sizes[0].skeleton - sizes[1].skeleton
}

func costOf(t *testing.T, zone *storage.Zone, sessions []string) historyCost {
	t.Helper()
	var c historyCost
	for _, session := range sessions {
		held, order := heldInOrder(t, zone, session)
		for _, id := range order {
			m, _ := held.Manifest(id)
			if m.HistoryForm == "" {
				continue
			}
			only, err := json.Marshal(struct {
				Form     string                 `json:"history_form"`
				Messages *providerbody.Messages `json:"messages,omitempty"`
				System   string                 `json:"system,omitempty"`
				Thread   *providerbody.Thread   `json:"thread,omitempty"`
				Reply    *providerbody.Reply    `json:"reply,omitempty"`
			}{m.HistoryForm, m.Messages, m.System, m.Thread, m.Reply})
			if err != nil {
				t.Fatal(err)
			}
			c.manifest += int64(len(only))
			if m.Messages != nil {
				c.lists++
				if m.Messages.Unavailable == providerbody.TooLarge {
					c.tooLarge++
				}
			}
		}
		files, err := storage.LandedFiles(zone, session)
		if err != nil {
			t.Fatal(err)
		}
		for _, lf := range files {
			if lf.Stream == "" && lf.RunID == "" {
				info, err := os.Stat(lf.Path)
				if err != nil {
					t.Fatal(err)
				}
				c.landed += info.Size()
			}
		}
		ix, sk := storageCost(t, zone, session)
		// Both ways write the new format, so what it adds whatever a body
		// holds is counted by hand: the 61 bytes each body row gained over
		// the 13 it had, and the 4 that count the message table.
		c.index += ix + int64(61*len(order)) + 4
		c.skeleton += sk
	}
	return c
}

// TestWhatTheHistoryCosts measures the history fields on every capture, at the
// default budget and at 4 KiB, the smallest budget a scenario uses, and holds
// what the cap may do: nothing at the default. The numbers are on the Session
// Data page.
func TestWhatTheHistoryCosts(t *testing.T) {
	cases, err := os.ReadDir(corpus)
	if err != nil {
		t.Skipf("no corpus: %v", err)
	}
	for _, budget := range []int64{2 << 20, 4 << 10} {
		var lc historyCost
		for _, c := range cases {
			if !c.IsDir() {
				continue
			}
			zone := storage.NewZone(t.TempDir())
			sessions := landIntoBudget(t, zone, c.Name(), budget)
			got := costOf(t, zone, sessions)
			lc.manifest, lc.landed, lc.index = lc.manifest+got.manifest, lc.landed+got.landed, lc.index+got.index
			lc.skeleton += got.skeleton
			lc.tooLarge, lc.lists = lc.tooLarge+got.tooLarge, lc.lists+got.lists
		}
		cc := costOf(t, landThreadsBudget(t, budget), []string{threadsSession})
		t.Logf("budget %d: LangChain history %d bytes of %d landed (%.1f%%), index +%d, skeletons +%d, %d of %d lists too large; "+
			"Claude Code history %d bytes of %d landed (%.1f%%), index +%d, skeletons +%d, %d of %d lists too large",
			budget, lc.manifest, lc.landed, 100*float64(lc.manifest)/float64(lc.landed), lc.index, lc.skeleton, lc.tooLarge, lc.lists,
			cc.manifest, cc.landed, 100*float64(cc.manifest)/float64(cc.landed), cc.index, cc.skeleton, cc.tooLarge, cc.lists)
		// At 4 KiB the cap takes the eleven longest lists of the twenty-turn
		// conversation, 19 to 39 messages, whose bodies copy from no earlier
		// one under so small a budget; at the default it takes none.
		want := map[int64]int{2 << 20: 0, 4 << 10: 11}[budget]
		if lc.tooLarge != want || cc.tooLarge != 0 {
			t.Errorf("at %d bytes the cap took %d LangChain and %d Claude Code lists; measured %d and 0",
				budget, lc.tooLarge, cc.tooLarge, want)
		}
	}
}

// replyOutcome is how the reply of the call at own comes back in the requests
// of the calls after it, requests being every call's list in landed order:
// equal, under its id altered, or not at all. Its own call's request, and an
// earlier one's, are never looked in.
func replyOutcome(reply providerbody.Entry, requests [][]providerbody.Entry, own int) string {
	result := "not sent back"
	for _, list := range requests[own+1:] {
		for _, e := range list {
			switch {
			case e == reply:
				return "back"
			case reply.ID != "" && e.ID == reply.ID:
				result = "back changed"
			}
		}
	}
	return result
}

// TestAReplyIsLookedForOnlyAfterItsCall holds the measurement's own rule: a
// reply found only in its own call's request is not sent back.
func TestAReplyIsLookedForOnlyAfterItsCall(t *testing.T) {
	r := providerbody.Entry{ID: "0000000000000001", Digest: "000000000000000a"}
	altered := providerbody.Entry{ID: r.ID, Digest: "000000000000000b"}
	other := providerbody.Entry{Digest: "000000000000000c"}
	// The call is the second of three: an earlier request and its own hold the
	// reply; only the third is looked in.
	for want, third := range map[string][]providerbody.Entry{
		"not sent back": {other},
		"back":          {other, r},
		"back changed":  {altered},
	} {
		requests := [][]providerbody.Entry{{r}, {r}, third}
		if got := replyOutcome(r, requests, 1); got != want {
			t.Fatalf("%v: %s, want %s", third, got, want)
		}
	}
}

// TestEveryLangChainCallSaysWhatItsHistoryWasComparedWith holds what the
// assembler makes of every capture: a conversation that only grows keeps its
// history call after call with the reply sent back; a reply the framework
// sends back with the agent's name added is back_changed; the summariser's
// own calls and a tool's plain calls are not compared; a marked summary is a
// reset, so the call after it is the first of its epoch; and no capture
// changes a history without a mark, so no card is drawn.
func TestEveryLangChainCallSaysWhatItsHistoryWasComparedWith(t *testing.T) {
	want := map[string]map[string]int{
		"abandoned-run":       {"first": 1},
		"js-graph":            {"first": 1, "kept/back": 1},
		"js-traceable":        {"first": 1},
		"large-content":       {"first": 1, "kept/back": 1},
		"long-conversation":   {"first": 1, "kept/back": 19},
		"loop":                {"first": 1, "kept/back": 5},
		"no-thread-key":       {"first": 1, "kept/back": 1},
		"parallel-tools":      {"first": 1, "kept/back": 1},
		"plain":               {"first": 1},
		"shared-thread-key":   {"first": 1, "kept/back": 2},
		"slow-tool":           {"first": 1, "kept/back": 1},
		"streaming":           {"first": 2, "kept/back": 2, "kept/back_changed": 1},
		"subagent":            {"first": 2, "kept/back": 2, "kept/back_changed": 1, "not_compared/auxiliary_stream": 1},
		"subagent-own-thread": {"first": 2, "kept/back": 1, "kept/back_changed": 1},
		"summarized":          {"first": 3, "kept/back": 4, "not_compared/internal_call": 2},
		"three-turns":         {"first": 1, "kept/back": 3},
		"tool-error":          {"first": 1},
		"traceable-only":      {},
		"two-threads":         {"first": 2, "kept/back": 2},
		"unsafe-thread-key":   {"first": 4},
	}
	cases, err := os.ReadDir(corpus)
	if err != nil {
		t.Skipf("no corpus: %v", err)
	}
	seen := 0
	for _, c := range cases {
		if !c.IsDir() {
			continue
		}
		seen++
		zone, sessions := land(t, c.Name())
		got := map[string]int{}
		for _, s := range sessions {
			if _, err := parse.Session(zone, parse.Options{Conversation: s, Session: s, Reindex: index.Rebuild}); err != nil {
				t.Fatal(err)
			}
			view := fold(t, zone, s)
			checkPredecessors(t, c.Name(), zone, s, view)
			for _, n := range view.Nodes {
				var a struct {
					History string `json:"history"`
					Reason  string `json:"history_reason"`
					Reply   string `json:"history_reply"`
				}
				_ = json.Unmarshal(n.Attrs, &a)
				switch n.Kind {
				case model.KindHistoryChange:
					t.Errorf("%s: a card %s; no capture changes a history without a mark", c.Name(), n.ID)
				case model.KindLLMCall:
					key := a.History
					if a.Reason != "" {
						key += "/" + a.Reason
					}
					if a.Reply != "" {
						key += "/" + a.Reply
					}
					got[key]++
				}
			}
		}
		w, ok := want[c.Name()]
		if !ok {
			t.Errorf("%s: a new capture; measure it and add it here", c.Name())
			continue
		}
		if fmt.Sprint(got) != fmt.Sprint(w) {
			t.Errorf("%s: %v, want %v", c.Name(), got, w)
		}
	}
	if seen != len(want) {
		t.Errorf("%d captures, %d expected", seen, len(want))
	}
}

// checkPredecessors holds each call's comparison to the calls around it: a
// compared call names the nearest earlier call of its stream that is not the
// framework's own, a first call has none before it but across a reset, and
// a call's length is its own request's list.
func checkPredecessors(t *testing.T, kase string, zone *storage.Zone, session string, view *sessionflow.View) {
	t.Helper()
	held, _ := heldInOrder(t, zone, session)
	before := func(a, b *sessionflow.Ref) bool { return a.Seq < b.Seq || (a.Seq == b.Seq && a.Row < b.Row) }
	byStream := map[string][]*sessionflow.Node{}
	for _, n := range view.Nodes {
		if n.Kind == model.KindLLMCall {
			byStream[n.Stream] = append(byStream[n.Stream], n)
		}
	}
	resets := view.NodesByKind(model.KindEpochBoundary)
	for stream, calls := range byStream {
		sort.Slice(calls, func(i, j int) bool { return before(calls[i].Ref, calls[j].Ref) })
		var last *sessionflow.Node
		for _, n := range calls {
			var a struct {
				History  string `json:"history"`
				Reason   string `json:"history_reason"`
				From     string `json:"history_from"`
				Messages *int   `json:"history_messages"`
			}
			_ = json.Unmarshal(n.Attrs, &a)
			// A request whose list reads says its length, and only such a one.
			list, _, err := held.ListOf(strings.TrimPrefix(n.ID, "call/") + ":request")
			switch {
			case err == nil && a.Messages == nil:
				t.Errorf("%s: %s says no length; its request holds %d messages", kase, n.ID, len(list))
			case err == nil && *a.Messages != len(list):
				t.Errorf("%s: %s says %d messages; its request holds %d", kase, n.ID, *a.Messages, len(list))
			case err != nil && a.Messages != nil:
				t.Errorf("%s: %s says %d messages; its request's list does not read: %v", kase, n.ID, *a.Messages, err)
			}
			switch a.History {
			case "kept", "changed":
				if last == nil || a.From != last.ID {
					t.Errorf("%s: %s is compared with %s; the call before it in %s is %v", kase, n.ID, a.From, stream, last)
				}
			case "first":
				if last != nil {
					spans := false
					for _, r := range resets {
						if r.Stream == stream && before(last.Ref, r.Ref) && before(r.Ref, n.Ref) {
							spans = true
						}
					}
					if !spans {
						t.Errorf("%s: %s is first, and %s comes before it with no reset between", kase, n.ID, last.ID)
					}
				}
			}
			if a.Reason != model.ClassInternalCall {
				last = n
			}
		}
	}
}
