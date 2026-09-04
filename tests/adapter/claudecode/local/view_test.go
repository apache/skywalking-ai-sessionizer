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

	if v.Schema != sessionview.Schema || v.ID != case1 || v.Session != case1 || v.Title != "build and check" {
		t.Fatalf("head of the document: schema=%q id=%q session=%q title=%q", v.Schema, v.ID, v.Session, v.Title)
	}
	// The session began at the first timed record, 2026-01-01T00:00:00Z, and
	// was last active at the context reset, thirty seconds later.
	if v.From != 1767225600000 || v.To != 1767225630000 {
		t.Fatalf("session range %d..%d", v.From, v.To)
	}
	if v.Head.Round != 1 || v.Head.ThroughSeq != 7 || v.Head.Digest == "" || v.Head.Parser != "v1" {
		t.Fatalf("head: %+v", v.Head)
	}
	if len(v.Rounds) != 1 || v.Rounds[0].FromSeq != 1 || v.Rounds[0].ThroughSeq != 7 ||
		v.Rounds[0].From != v.From || v.Rounds[0].SessionTo != v.To || v.Rounds[0].Digest != v.Head.Digest ||
		v.Rounds[0].Lines != 50 || v.Rounds[0].FileDigest == "" {
		t.Fatalf("rounds: %+v", v.Rounds)
	}
	if len(v.Files) != 7 {
		t.Fatalf("files: %d, want the fixture's 7", len(v.Files))
	}
	kinds := map[string]int{}
	for _, f := range v.Files {
		kinds[f.Kind]++
		if f.Digest == "" || f.Lines < 3 || f.Bytes == 0 || f.Version != "sd/1" || f.Format != "sd" {
			t.Fatalf("file: %+v", f)
		}
		// Meta, manifest and script records carry no time, so those files
		// have no range; transcripts and the journal have one.
		timed := f.From != 0
		if want := f.Kind == "transcript"; timed != want {
			t.Fatalf("file %s of kind %s: timed=%v", f.File, f.Kind, timed)
		}
	}
	if kinds["transcript"] != 3 || kinds["agent_meta"] != 1 || kinds["journal"] != 1 || kinds["workflow_manifest"] != 1 || kinds["workflow_script"] != 1 {
		t.Fatalf("file kinds: %v", kinds)
	}
	if v.Counts.Talks != 3 || v.Counts.Streams != 3 || v.Counts.Relations != 8 || v.Counts.Unresolved != 2 || v.Counts.Nodes != 38 {
		t.Fatalf("counts: %+v", v.Counts)
	}
	if len(v.Talks) != 3 || len(v.Relations) != 8 || len(v.Unresolved) != 2 || len(v.Streams) != 3 {
		t.Fatalf("lists: talks=%d relations=%d unresolved=%d streams=%d", len(v.Talks), len(v.Relations), len(v.Unresolved), len(v.Streams))
	}
	// The first talk is the person's input, as a tree down to what the
	// call contains, with the text the record carries.
	first := v.Talks[0]
	if first.Label != "run the build" || first.Runs != 2 || first.Tree == nil || first.Tree.Kind != "talk" {
		t.Fatalf("first talk: %+v", first)
	}
	var input *sessionview.Node
	var walk func(n *sessionview.Node)
	walk = func(n *sessionview.Node) {
		if n.Kind == "message.external" && n.Text == "run the build" {
			input = n
		}
		for i := range n.Children {
			walk(&n.Children[i])
		}
	}
	walk(first.Tree)
	if input == nil || input.Ref == nil || input.Ref.Seq != 1 || input.Ref.Row != 3 || input.At != 1767225601000 {
		t.Fatalf("the input under the first talk: %+v", input)
	}
	if first.Reply != "Build passed and tests are green." {
		t.Fatalf("reply: %q", first.Reply)
	}
}
