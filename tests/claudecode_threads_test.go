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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecodeprovider"
	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// threads is a redacted capture of Claude Code 2.1.302 continuing provider
// threads; its README says what it holds and how it was made.
const threads = "../internal/adapters/claudecodeprovider/testdata/threads"

const threadsSession = "2bb28d1c-72a7-4260-a010-f439329e4e76"

// landThreads collects the capture as asz collect does, transcripts first so
// each response finds the call that claims it, and parses it.
func landThreads(t *testing.T) (*storage.Zone, *sessionflow.View) {
	t.Helper()
	zone := landThreadsBudget(t, 2<<20)
	if _, err := parse.Session(zone, parse.Options{Conversation: threadsSession, Session: threadsSession,
		Reindex: index.Rebuild}); err != nil {
		t.Fatal(err)
	}
	return zone, fold(t, zone, threadsSession)
}

// landThreadsBudget collects the capture into a new root under a budget.
func landThreadsBudget(t *testing.T, budget int64) *storage.Zone {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(threads)); err != nil {
		t.Fatal(err)
	}
	var times map[string]int64
	raw, err := os.ReadFile(filepath.Join(dir, "mtimes.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &times); err != nil {
		t.Fatal(err)
	}
	var last time.Time
	for name, ns := range times {
		at := time.Unix(0, ns)
		if err := os.Chtimes(filepath.Join(dir, "bodies", name), at, at); err != nil {
			t.Fatal(err)
		}
		if at.After(last) {
			last = at
		}
	}
	zone := storage.NewZone(filepath.Join(dir, "root"))
	now := func() time.Time { return last.Add(time.Hour) }
	local := claudecode.New(filepath.Join(dir, "projects"), zone, budget)
	local.Now = now
	if _, err := local.CollectAll(func(claudecode.Session) bool { return true }); err != nil {
		t.Fatal(err)
	}
	bodies := claudecodeprovider.New(filepath.Join(dir, "bodies"), zone, budget)
	bodies.Now = now
	st, err := bodies.CollectAll(func(string) claudecodeprovider.Verdict { return claudecodeprovider.Collect })
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range st.Errors {
		t.Errorf("collect: %v", e)
	}
	return zone
}

// TestAThreadsHistoryIsBuiltFromWhatItContinues holds what the capture showed
// and the form rests on. A process's first request starts a thread and holds
// its whole list; every later request of the process continues the thread
// from the previous response's message and holds only the new messages. With
// each continue's history built from the call it continues, that call's
// reply and its own messages, every call's history is the history before it
// with the reply next, through every resume, until the compaction.
func TestAThreadsHistoryIsBuiltFromWhatItContinues(t *testing.T) {
	zone, view := landThreads(t)
	held, order := heldInOrder(t, zone, threadsSession)

	// What asz at f6e3274 gave on the unredacted capture, before anything
	// is read of the history: without these, nothing below is compared.
	calls := view.NodesByKind(model.KindLLMCall)
	joined := 0
	type call struct {
		node              *sessionflow.Node
		request, response string
	}
	var all []call
	byMessage := map[string]*call{}
	for _, n := range calls {
		bodies, err := sessionflow.ProviderBodiesOf(n.Attrs)
		if err != nil {
			t.Fatal(err)
		}
		c := call{node: n}
		for _, b := range bodies {
			joined++
			id := recordIDAt(t, zone, threadsSession, b.Ref)
			if b.Role == sessionflow.RoleRequest {
				c.request = id
			} else {
				c.response = id
			}
		}
		all = append(all, c)
	}
	if len(calls) != 13 || len(order) != 27 || joined != 26 {
		t.Fatalf("%d calls, %d bodies landed, %d joined; measured 13, 27 and 26", len(calls), len(order), joined)
	}
	// Which ones: the compaction's request lands and joins no call, and its
	// response, which no transcript record names, waits.
	compaction := compactionBodies(t)
	landedIDs := map[string]bool{}
	for _, id := range order {
		landedIDs[id] = true
	}
	joinedIDs := map[string]bool{}
	for _, c := range all {
		joinedIDs[c.request], joinedIDs[c.response] = true, true
	}
	if !landedIDs[compaction.request] || joinedIDs[compaction.request] || landedIDs[compaction.response] {
		t.Fatalf("the compaction's request landed %v and joined %v, its response landed %v; measured landed, unjoined, waiting",
			landedIDs[compaction.request], joinedIDs[compaction.request], landedIDs[compaction.response])
	}
	threadsOf := map[string]int{}
	for _, id := range order {
		m, _ := held.Manifest(id)
		if m.Role != providerbody.RoleRequest {
			continue
		}
		if m.HistoryForm != providerbody.FormClaudeCode {
			t.Fatalf("%s: form %q", id, m.HistoryForm)
		}
		kind := "none"
		if m.Thread != nil {
			kind = m.Thread.Type
		}
		threadsOf[kind]++
	}
	if threadsOf["create"] != 9 || threadsOf["continue"] != 4 || threadsOf["none"] != 1 {
		t.Fatalf("threads %v; measured 9 created, 4 continued and the compaction's request with none", threadsOf)
	}

	// The history of each call, built as the design says.
	reply := func(c *call) providerbody.Entry {
		m, ok := held.Manifest(c.response)
		if !ok || m.Reply == nil || m.Reply.Unavailable != "" {
			t.Fatalf("%s has no reply", c.node.ID)
		}
		return providerbody.Entry{ID: m.Reply.ID, Digest: m.Reply.Digest}
	}
	var historyOf func(c *call, depth int) []providerbody.Entry
	historyOf = func(c *call, depth int) []providerbody.Entry {
		own, _, err := held.ListOf(c.request)
		if err != nil {
			t.Fatalf("%s: %v", c.request, err)
		}
		m, _ := held.Manifest(c.request)
		if m.Thread == nil || m.Thread.Type != providerbody.ThreadContinue {
			return own
		}
		from := byMessage[m.Thread.Previous]
		if from == nil || depth > 64 {
			t.Fatalf("%s continues %s, which no call here is", c.request, m.Thread.Previous)
		}
		return append(append(historyOf(from, depth+1), reply(from)), own...)
	}

	// Consecutive calls of each stream, in the order their first fragments
	// landed. The calls are looked up by message once they are in that order.
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i].node.Ref, all[j].node.Ref
		return a.Seq < b.Seq || (a.Seq == b.Seq && a.Row < b.Row)
	})
	for i := range all {
		byMessage[strings.TrimPrefix(all[i].node.ID, "call/")] = &all[i]
	}
	last := map[string]*call{}
	kept := 0
	var others [][2]*call
	for i := range all {
		c := &all[i]
		p := last[c.node.Stream]
		last[c.node.Stream] = c
		if p == nil {
			continue
		}
		P, N := historyOf(p, 0), historyOf(c, 0)
		if len(N) > len(P) && equalEntries(N[:len(P)], P) && N[len(P)] == reply(p) {
			kept++
		} else {
			others = append(others, [2]*call{p, c})
		}
	}
	if kept != 10 || len(others) != 1 {
		t.Fatalf("%d pairs kept with the reply next, %d not; measured 10 and the one across the compaction", kept, len(others))
	}
	// The one other pair spans the compaction: the explicit reset of its
	// stream lies between its two calls.
	p, n := others[0][0].node, others[0][1].node
	before := func(a, b sessionflow.Ref) bool { return a.Seq < b.Seq || (a.Seq == b.Seq && a.Row < b.Row) }
	spans := false
	for _, b := range view.NodesByKind(model.KindEpochBoundary) {
		if b.Stream == n.Stream && b.Ref != nil && before(*p.Ref, *b.Ref) && before(*b.Ref, *n.Ref) {
			spans = true
		}
	}
	if !spans {
		t.Fatalf("the pair %s, %s that is not kept spans no reset", p.ID, n.ID)
	}

	// And the assembler says the same: ten kept with the reply back, the
	// first call of each stream and the first after the compaction with no
	// history before them, and no card.
	got := map[string]int{}
	for _, c := range all {
		var a struct {
			History string `json:"history"`
			Reply   string `json:"history_reply"`
		}
		_ = json.Unmarshal(c.node.Attrs, &a)
		got[a.History+"/"+a.Reply]++
	}
	if fmt.Sprint(got) != fmt.Sprint(map[string]int{"first/": 3, "kept/back": 10}) {
		t.Fatalf("history %v; want 3 first and 10 kept with the reply back", got)
	}
	if cards := view.NodesByKind(model.KindHistoryChange); len(cards) != 0 {
		t.Fatalf("%d cards; no history here changes", len(cards))
	}
}

func equalEntries(a, b []providerbody.Entry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// compactionBodies names the compaction's two bodies, as the capture's own
// index of the files it wrote says.
func compactionBodies(t *testing.T) struct{ request, response string } {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(threads, "bodies", "index.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ request, response string }
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var row struct {
			QuerySource  string `json:"query_source"`
			RequestFile  string `json:"request_file"`
			ResponseFile string `json:"response_file"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		if row.QuerySource == "compact" {
			if out.request != "" {
				t.Fatal("the capture holds two compactions")
			}
			out.request = strings.TrimSuffix(row.RequestFile, ".json")
			out.response = strings.TrimSuffix(row.ResponseFile, ".json")
		}
	}
	if out.request == "" {
		t.Fatal("the capture holds no compaction")
	}
	return out
}
