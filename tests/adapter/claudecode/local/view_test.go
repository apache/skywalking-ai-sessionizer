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

package local_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// TestConversationViewIsOneDocument reads the fixture session as the one
// document the page serves and a server holding the same files would build:
// every talk as a tree with its text, the rounds and the files it came
// from, and the session's own range. The counts are the fixture's, fixed by
// its files, and any drift here is a change to the view's schema or to the
// assembly, both of which this test is meant to catch.
func TestConversationViewIsOneDocument(t *testing.T) {
	c := collect(t)
	for _, id := range []string{case1, case2} {
		if _, err := parse.Session(c.zone, parse.Options{Conversation: id, Session: id}); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(view.New(c.zone, nil).Handler())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/api/c/" + case1 + "/view")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	var v sessionview.Conversation
	if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}

	if v.Format != sessionview.Format || v.Version != sessionview.Version || v.Conversation != case1 ||
		len(v.Sessions) != 1 || v.Sessions[0] != case1 || v.Summary.Title != "build and check" {
		t.Fatalf("head of the document: format=%q version=%q conversation=%q sessions=%v title=%q", v.Format, v.Version, v.Conversation, v.Sessions, v.Summary.Title)
	}
	// Every round and every file verified, and the document says so.
	if v.Summary.State != sessionview.StateVerified || len(v.Summary.Problems) != 0 {
		t.Fatalf("verification: state=%q problems=%v", v.Summary.State, v.Summary.Problems)
	}
	// The session began at the first timed record, 2026-01-01T00:00:00Z, and
	// was last active at the context reset, thirty seconds later.
	if v.Summary.From != 1767225600000 || v.Summary.To != 1767225630000 {
		t.Fatalf("session range %d..%d", v.Summary.From, v.Summary.To)
	}
	if v.Head.Round != 1 || v.Head.Digest == "" || v.Parser != "v1" {
		t.Fatalf("head: %+v parser=%q", v.Head, v.Parser)
	}
	if len(v.Rounds) != 1 {
		t.Fatalf("rounds: %+v", v.Rounds)
	}
	r := v.Rounds[0]
	if r.FromSeq != 1 || r.ThroughSeq != 7 || r.Previous != nil || r.Digest != v.Head.Digest || !r.Verified ||
		r.FromTime == nil || *r.FromTime != v.Summary.From || r.ThroughTime == nil || *r.ThroughTime != v.Summary.To {
		t.Fatalf("round: %+v", r)
	}
	// Seven landed files and the round, each with its digest and size.
	if len(v.Files) != 8 {
		t.Fatalf("files: %d, want the fixture's 7 landed files and 1 round", len(v.Files))
	}
	kinds := map[string]int{}
	for _, f := range v.Files {
		kinds[f.Kind]++
		if f.Digest == "" || f.Lines < 3 || f.Bytes == 0 {
			t.Fatalf("file: %+v", f)
		}
		switch f.Format {
		case "sd":
			if f.Seq == nil || f.Round != nil {
				t.Fatalf("landed file addressing: %+v", f)
			}
		case "sf":
			if f.Round == nil || *f.Round != 1 || f.Seq != nil || f.Lines != 50 {
				t.Fatalf("round file: %+v", f)
			}
		default:
			t.Fatalf("file format %q", f.Format)
		}
		// Meta, manifest and script records carry no time, so those files
		// have no range; transcripts and the round have one.
		timed := f.FromTime != nil
		if want := f.Kind == "transcript" || f.Kind == "round"; timed != want {
			t.Fatalf("file %s of kind %s: timed=%v", f.File, f.Kind, timed)
		}
	}
	if kinds["transcript"] != 3 || kinds["agent_meta"] != 1 || kinds["journal"] != 1 || kinds["workflow_manifest"] != 1 || kinds["workflow_script"] != 1 || kinds["round"] != 1 {
		t.Fatalf("file kinds: %v", kinds)
	}
	sm := v.Summary
	if sm.Talks != 3 || sm.Streams != 3 || sm.Segments != 1 || sm.Rounds != 1 || sm.Unresolved != 2 || sm.Kinds["llm.call"] != 6 || sm.RelationTypes["starts"] != 1 {
		t.Fatalf("summary: %+v", sm)
	}
	if len(v.Talks) != 3 || len(v.Relations) != 8 || len(v.Unresolved) != 2 || len(v.Streams) != 3 || len(v.Segments) != 1 {
		t.Fatalf("lists: talks=%d relations=%d unresolved=%d streams=%d segments=%d", len(v.Talks), len(v.Relations), len(v.Unresolved), len(v.Streams), len(v.Segments))
	}
	// The first talk is the person's input, as a tree down to what the
	// call contains, with the text the record carries and its summary on
	// the talk node itself.
	first := v.Talks[0]
	if first.Kind != "talk" || first.Label != "run the build" || first.Runs != 2 || first.Reply != "Build passed and tests are green." || len(first.Children) != 2 {
		t.Fatalf("first talk: kind=%q label=%q runs=%d reply=%q children=%d", first.Kind, first.Label, first.Runs, first.Reply, len(first.Children))
	}
	var input, call *sessionview.Node
	var walk func(n *sessionview.Node)
	walk = func(n *sessionview.Node) {
		if n.Kind == "message.external" && n.Text == "run the build" {
			input = n
		}
		if n.ID == "call/call1" {
			call = n
		}
		for i := range n.Children {
			walk(&n.Children[i])
		}
	}
	walk(&first)
	if input == nil || input.Ref == nil || input.Ref.Seq != 1 || input.Ref.Row != 3 || input.At != 1767225601000 {
		t.Fatalf("the input under the first talk: %+v", input)
	}
	if len(input.Flags) != 1 || input.Flags[0] != "external_input" {
		t.Fatalf("the input's flags from its record: %v", input.Flags)
	}
	// A call's usage is the one record usage_at names, row 7, not a sum
	// over its three fragments.
	if call == nil || call.Usage == nil || call.Usage.Output != 50 || call.Usage.CacheRead != 900 {
		t.Fatalf("call usage: %+v", call)
	}
}
