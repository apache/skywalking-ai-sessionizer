// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements.  See the NOTICE file distributed with
// this work for additional information regarding copyright ownership.
// The ASF licenses this file to You under the Apache License, Version 2.0
// (the "License"); you may not use this file except in compliance with
// the License.  You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package judge

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/pkg/changes"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// writes reads a stand-in runtime's one editing tool, Write, whose input
// names its file in "file".
func writes(tool string, input json.RawMessage) []changes.Requested {
	var in struct {
		File string `json:"file"`
	}
	if tool != "Write" || json.Unmarshal(input, &in) != nil || in.File == "" {
		return nil
	}
	return []changes.Requested{{Path: in.File, Operation: changes.RequestWrite}}
}

// bigInput is a request the document clips, read whole from the part of its
// record the step stands on.
var bigInput = `{"file":"big.go","pad":"` + strings.Repeat("x", 3000) + `"}`

// edits is one main talk and a talk of another request: w1 writes a.go and
// returns; w2's request is clipped; e1 is joined to a change record of two
// files, so its request is not read; f1 failed. The second talk's g1 writes
// b.go and never returns. A record no step joins lost its file list. a1 is
// an agent call that bears an editing tool's name and input, which no
// request of a tool is.
func edits() *Nav { return editsWith(nil) }

// editsWith is edits with its document changed first.
func editsWith(change func(d *sessionview.Conversation)) *Nav {
	n := func(id, text string, row uint64) sessionview.Node {
		return sessionview.Node{ID: id, Kind: model.KindTool, Name: "Write", Text: text, Bytes: len(text), Result: "ok",
			Ref: &sessionflow.Ref{Seq: 1, Row: row}}
	}
	three, one, two := 3, 1, 2
	w2 := n("w2", bigInput[:2000], 3)
	w2.Bytes, w2.Ref.Block = len(bigInput), &one
	f1 := n("f1", `{"file":"c.go"}`, 5)
	failed := true
	f1.Failed = &failed
	g1 := n("g1", `{"file":"b.go"}`, 7)
	g1.Result = ""
	d := &sessionview.Conversation{Conversation: "conv", Streams: []sessionview.Stream{{ID: "stream/main", Name: "main"}},
		Talks: []sessionview.Node{
			{ID: "talk/1", Kind: "talk", Stream: "main", Children: []sessionview.Node{
				{ID: "in/1", Kind: model.KindMessageExternal, Text: "write them", Ref: &sessionflow.Ref{Seq: 1, Row: 1}},
				n("w1", `{"file":"a.go"}`, 2), w2, n("e1", `{"file":"x.go"}`, 4), f1,
				{ID: "a1", Kind: model.KindAgentCall, Name: "Write", Text: `{"file":"z.go"}`, Ref: &sessionflow.Ref{Seq: 1, Row: 8}}}},
			{ID: "talk/2", Kind: "talk", Stream: "main", Children: []sessionview.Node{g1}},
		},
		WorkspaceChanges: []sessionview.WorkspaceChange{
			{Step: "e1", Ref: sessionflow.Ref{Seq: 1, Row: 4}, Record: changes.Record{ID: "c1", Root: &changes.Root{Path: "/w"}, ChangedFiles: &two,
				Changes: []changes.FileChange{{Path: "x.go", Operation: changes.OpModify, Attribution: changes.AttributionShared, Additions: &three, Deletions: &one},
					{Path: "y.go", Operation: changes.OpCreate}}}},
			{Ref: sessionflow.Ref{Seq: 1, Row: 6}, Record: changes.Record{ID: "c2", ChangedFiles: &two}},
		},
	}
	read := func(seq, row uint64) (*sessiondata.Record, error) {
		// The record holds another tool's request before w2's.
		if seq == 1 && row == 3 {
			return &sessiondata.Record{Parts: []sessiondata.Part{{Data: json.RawMessage(`{"file":"other.go"}`)}, {Data: json.RawMessage(bigInput)}}}, nil
		}
		return nil, fmt.Errorf("no record %d/%d", seq, row)
	}
	if change != nil {
		change(d)
	}
	return NewNav(d, Compute(d, "claude-code/1", nil, nil), read)
}

func ops(list []sessionjudge.Operation) string {
	var out []string
	for _, o := range list {
		out = append(out, fmt.Sprintf("%s:%s:%s:%s:%s:%s", o.ID, o.Step, o.Path, o.Operation, o.Source, o.Status))
	}
	return strings.Join(out, " ")
}

// The operations are what the change records captured and what the requests
// of the other tool steps named, in the order they landed, each a request
// with whether its tool returned.
func TestOperationsAreWhatTheRecordShows(t *testing.T) {
	nav := edits()
	if got := ops(nav.Operations()); got != "o1:e1:x.go:modify:change_record: o2:e1:y.go:create:change_record: o3::"+
		":unlisted:change_record:" {
		t.Fatalf("without a decoder: %s", got)
	}
	nav.Decode(writes)
	want := "o1:w1:a.go:write:tool_request:returned o2:w2:big.go:write:tool_request:returned o3:e1:x.go:modify:change_record: " +
		"o4:e1:y.go:create:change_record: o5:f1:c.go:write:tool_request:failed o6:::unlisted:change_record: o7:g1:b.go:write:tool_request:no_result"
	if got := ops(nav.Operations()); got != want {
		t.Fatalf("operations\n%s\nwant\n%s", got, want)
	}
	x := nav.Operations()[2]
	if x.Record != "c1" || x.Workspace != "/w" || x.Attribution != changes.AttributionShared || *x.Added != 3 || *x.Removed != 1 ||
		x.Ref == nil || x.Ref.Row != 4 {
		t.Fatalf("a recorded operation %+v", x)
	}
	if w := nav.Operations()[0]; w.Ref == nil || w.Ref.Row != 2 || w.Record != "" {
		t.Fatalf("a requested operation %+v", w)
	}
	// A clipped request whose record cannot be read names nothing.
	nav = edits()
	nav.read = nil
	nav.Decode(writes)
	if got := ops(nav.Operations()); strings.Contains(got, "big.go") || !strings.Contains(got, "a.go") {
		t.Fatalf("an unread request: %s", got)
	}
}

// A step keeps the files its request names that no change record of it
// names: a record whose list was taken out to fit names none, and a capture
// can name some of a step's files and not others. A file a record names, as
// the request names it or joined to the record's workspace, is there once,
// as the change.
func TestARequestKeepsTheFilesNoRecordNames(t *testing.T) {
	nav := editsWith(func(d *sessionview.Conversation) { d.WorkspaceChanges[1].Step = "f1" })
	nav.Decode(writes)
	if got := ops(nav.Operations()); !strings.Contains(got, ":f1:c.go:write:tool_request:failed") || !strings.Contains(got, ":f1::unlisted:change_record:") ||
		strings.Contains(got, ":e1:x.go:write:tool_request") {
		t.Fatalf("a record naming no file: %s", got)
	}
	// e1's record names only y.go; its request names x.go.
	nav = editsWith(func(d *sessionview.Conversation) {
		d.WorkspaceChanges[0].Changes = d.WorkspaceChanges[0].Changes[1:]
	})
	nav.Decode(writes)
	if got := ops(nav.Operations()); !strings.Contains(got, ":e1:x.go:write:tool_request:returned") || !strings.Contains(got, ":e1:y.go:create:change_record:") {
		t.Fatalf("a record naming another file: %s", got)
	}
	// The request names x.go by its full path in the record's workspace.
	nav = editsWith(func(d *sessionview.Conversation) {
		for i := range d.Talks[0].Children {
			if d.Talks[0].Children[i].ID == "e1" {
				d.Talks[0].Children[i].Text = `{"file":"/w/x.go"}`
				d.Talks[0].Children[i].Bytes = len(d.Talks[0].Children[i].Text)
			}
		}
	})
	nav.Decode(writes)
	if got := ops(nav.Operations()); strings.Contains(got, ":e1:/w/x.go:write:tool_request") || !strings.Contains(got, ":e1:x.go:modify:change_record:") {
		t.Fatalf("a full path: %s", got)
	}
}

// The operations tool lists them a row each, the operations of some talks
// when asked; one no step joins is in no talk.
func TestTheOperationsToolListsThem(t *testing.T) {
	nav := edits()
	nav.Decode(writes)
	all, err := nav.Call("operations", nil)
	if err != nil || all.(page).Total != 7 {
		t.Fatalf("all %+v %v", all, err)
	}
	two, err := nav.Call("operations", json.RawMessage(`{"talks":["talk/2"]}`))
	b, _ := json.Marshal(two)
	if err != nil || two.(page).Total != 1 || !strings.Contains(string(b), `"id":"o7"`) || !strings.Contains(string(b), `"status":"no_result"`) {
		t.Fatalf("talk 2 %s %v", b, err)
	}
}

// A target's file groups name its own operations, each once: an operation
// of other work, one named twice and one that does not exist are left out,
// and a group left with none is left out too.
func TestAFileGroupNamesTheTargetsOwnOperations(t *testing.T) {
	nav := edits()
	nav.Decode(writes)
	target := sessionjudge.Target{ID: "t1", Talks: []string{"talk/1"}, Criteria: []sessionjudge.Criterion{{ID: "t1.c1", Text: "written"}}}
	nav.Own([]sessionjudge.Target{target, {ID: "t2", Talks: []string{"talk/2"}}}, nil)
	b, _ := json.Marshal(accountEntry{Files: []fileGroupEntry{
		{Area: "pkg/a", Category: "code", Summary: "the writer", Operations: []string{"o1", "o2"}},
		{Area: "pkg/x", Category: "tests", Summary: "the test", Operations: []string{"o2", "o3", "o7", "o99"}},
		{Area: "pkg/b", Category: "code", Summary: "other work", Operations: []string{"o7"}},
	}})
	raw := `{"achievements":[{"target":"t1","basis":"observed_result","criteria":[{"id":"t1.c1","state":"met","cite":["w1"]}],"claims":[],` +
		`"account":` + string(b) + `,"note":"","cite":["w1"]}]}`
	c, err := checkAchievement(nav, []sessionjudge.Target{target}, json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	a := c.frames[0].(*sessionjudge.Achievement)
	if a.Account == nil || len(a.Account.Files) != 2 || fmt.Sprint(a.Account.Files[0].Operations) != "[o1 o2]" ||
		fmt.Sprint(a.Account.Files[1].Operations) != "[o3]" || a.Account.Files[1].Category != "tests" || a.Account.Files[1].Summary != "the test" {
		t.Fatalf("files %+v", a.Account)
	}
	if !strings.Contains(c.note(), "4 operations in the file groups of target t1 that are not its own, or named twice, were left out") ||
		!strings.Contains(c.note(), "1 parts of the account of target t1") {
		t.Fatalf("note %q", c.note())
	}
}

// A file group names only the operations of calls its target received: in a
// talk two targets share, the calls after an input that directed another
// target are that target's.
func TestAFileGroupNamesOnlyOperationsOfItsCalls(t *testing.T) {
	nav, _ := worked()
	w := workedDecisions()
	// in/2 directs talk/2 to t2, which t1 lists too: c3, which wrote o2, is t2's.
	w.inputs[1].Actions = []sessionjudge.Action{{Target: "t2", Action: sessionjudge.ActionCreate}}
	w.targets[1].Talks = append(w.targets[1].Talks, "talk/2")
	nav.Own(w.targets, w.inputs)
	b, _ := json.Marshal(accountEntry{Files: []fileGroupEntry{{Area: "pkg", Category: "code", Summary: "the fix", Operations: []string{"o1", "o2"}}}})
	raw := `{"achievements":[{"target":"t1","basis":"observed_result","criteria":[{"id":"t1.c1","state":"unknown","cite":[]},{"id":"t1.c2","state":"unknown","cite":[]}],` +
		`"claims":[],"account":` + string(b) + `,"note":"","cite":[]}]}`
	c, err := checkAchievement(nav, w.targets[:1], json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	a := c.frames[0].(*sessionjudge.Achievement)
	if a.Account == nil || fmt.Sprint(a.Account.Files[0].Operations) != "[o1]" ||
		!strings.Contains(c.note(), "1 operations in the file groups of target t1 that are not its own") {
		t.Fatalf("files %+v; %q", a.Account, c.note())
	}
}
