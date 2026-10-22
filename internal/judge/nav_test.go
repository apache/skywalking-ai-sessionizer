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

// navOverRecords is the test document with a landed record behind the step
// long, and two change records: w1 joined to s1 in talk/1, with two files
// that have hunks and one that has none, and w2 joined to s3b in talk/2.
// read is nil for a navigation that cannot read records.
func navOverRecords(read RecordReader) *Nav {
	d := doc()
	d.Talks[0].Children = append(d.Talks[0].Children, sessionview.Node{ID: "long", Kind: "tool", Result: "r",
		Ref: &sessionflow.Ref{Seq: 3, Row: 1}})
	line := func(text string) []changes.Hunk {
		return []changes.Hunk{{NewStart: 1, NewLines: 1, Lines: []string{text}}}
	}
	d.WorkspaceChanges = []sessionview.WorkspaceChange{
		{Step: "s1", Ref: sessionflow.Ref{Seq: 9, Row: 1}, Record: changes.Record{ID: "w1", Changes: []changes.FileChange{
			{Path: "a.go", Hunks: line("+w1 a.go")}, {Path: "b.go", Hunks: line("+w1 b.go")},
			{Path: "logo.png", Diff: changes.DiffBinary}}}},
		{Step: "s3b", Ref: sessionflow.Ref{Seq: 9, Row: 2}, Record: changes.Record{ID: "w2", Changes: []changes.FileChange{
			{Path: "a.go", Hunks: line("+w2 a.go")}}}},
	}
	return NewNav(d, Compute(d, "claude-code/1", nil, nil), read)
}

// A read of something that is not there is refused with the reason. An
// offset outside an item, a record or a part would otherwise cut the text
// out of range. A navigation without records says it cannot read them, and
// a step, a change record or a file that is not there is named.
func TestAReadOfWhatIsNotThereSaysWhy(t *testing.T) {
	rec := &sessiondata.Record{Parts: []sessiondata.Part{{Kind: "text", Text: "0123456789"}}}
	nav := navOverRecords(func(_, _ uint64) (*sessiondata.Record, error) { return rec, nil })
	blind := navOverRecords(nil)
	item, _ := json.Marshal(nav.TalkRows(nil)[0])
	record, _ := json.Marshal(rec)
	for _, c := range []struct {
		name, tool, args, want string
		nav                    *Nav
	}{
		{"an item read past its end", "talks", fmt.Sprintf(`{"item":1,"offset":%d}`, len(item)+1),
			fmt.Sprintf("offset %d is outside the item's %d bytes", len(item)+1, len(item)), nav},
		{"an item read before its start", "talks", `{"item":1,"offset":-1}`,
			fmt.Sprintf("offset -1 is outside the item's %d bytes", len(item)), nav},
		{"a record read past its end", "record", fmt.Sprintf(`{"seq":3,"row":1,"offset":%d}`, len(record)+1),
			fmt.Sprintf("offset %d is outside the record's %d bytes", len(record)+1, len(record)), nav},
		{"a record read before its start", "record", `{"seq":3,"row":1,"offset":-1}`,
			fmt.Sprintf("offset -1 is outside the record's %d bytes", len(record)), nav},
		{"a part read past its end", "record", `{"seq":3,"row":1,"part":0,"offset":11}`, "offset 11 is outside the part's 10 bytes", nav},
		{"a part read before its start", "record", `{"seq":3,"row":1,"part":0,"offset":-1}`, "offset -1 is outside the part's 10 bytes", nav},
		{"a record where none can be read", "record", `{"seq":3,"row":1}`, "records cannot be read here", blind},
		{"a part where none can be read", "record", `{"seq":3,"row":1,"part":0}`, "records cannot be read here", blind},
		{"a step searched where no record can be read", "search", `{"query":"r","node":"long"}`, "records cannot be read here", blind},
		{"a step that is not there searched", "search", `{"query":"r","node":"nothing"}`, `no step "nothing"`, nav},
		{"a change record that is not there", "diff", `{"id":"w9","path":"a.go"}`, `no workspace change record "w9"`, nav},
		{"a file the record does not name", "diff", `{"id":"w1","path":"c.go"}`, `record w1 names no file "c.go"`, nav},
		{"a file with no hunks", "diff", `{"id":"w1","path":"logo.png"}`, "no hunks for logo.png in w1: the diff is binary", nav},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := c.nav.Call(c.tool, json.RawMessage(c.args)); err == nil || err.Error() != c.want {
				t.Fatalf("%s %s: %v, want %q", c.tool, c.args, err, c.want)
			}
		})
	}
	// The end itself is inside the bound: it reads an empty slice, and
	// nothing follows it.
	for _, c := range []struct{ tool, args string }{
		{"talks", fmt.Sprintf(`{"item":1,"offset":%d}`, len(item))},
		{"record", fmt.Sprintf(`{"seq":3,"row":1,"offset":%d}`, len(record))},
		{"record", `{"seq":3,"row":1,"part":0,"offset":10}`},
	} {
		ans, err := nav.Call(c.tool, json.RawMessage(c.args))
		if err != nil {
			t.Fatalf("%s %s: %v", c.tool, c.args, err)
		}
		if m := ans.(map[string]any); m["slice"] != "" || m["next_offset"] != nil {
			t.Fatalf("%s %s at the end: %+v", c.tool, c.args, m)
		}
	}
}

// A diff is read from the change record and the file it names, even when
// another record changed the same path, or the same record changed another.
func TestADiffIsReadFromTheRecordAndFileItNames(t *testing.T) {
	nav := navOverRecords(nil)
	for _, c := range []struct{ id, path, want string }{
		{"w1", "a.go", "+w1 a.go"},
		{"w1", "b.go", "+w1 b.go"},
		{"w2", "a.go", "+w2 a.go"},
	} {
		ans, err := nav.Call("diff", json.RawMessage(fmt.Sprintf(`{"id":%q,"path":%q}`, c.id, c.path)))
		if err != nil {
			t.Fatalf("%s %s: %v", c.id, c.path, err)
		}
		var lines []string
		for _, r := range ans.(page).Rows {
			if l, ok := r.(map[string]any)["line"].(string); ok {
				lines = append(lines, l)
			}
		}
		if got := strings.Join(lines, ","); got != c.want {
			t.Fatalf("%s %s gave %s, want %s", c.id, c.path, got, c.want)
		}
	}
}

// Search and changes asked for some talks answer from those talks only.
func TestSearchAndChangesKeepToTheTalksAsked(t *testing.T) {
	nav := navOverRecords(nil)
	for _, c := range []struct{ tool, args, want string }{
		{"search", `{"query":"done"}`, "s1,s2,s3a,s3b"},
		{"search", `{"query":"done","talks":["talk/2"]}`, "s3b"},
		{"changes", `{}`, "w1,w2"},
		{"changes", `{"talks":["talk/2"]}`, "w2"},
	} {
		ans, err := nav.Call(c.tool, json.RawMessage(c.args))
		if err != nil {
			t.Fatalf("%s %s: %v", c.tool, c.args, err)
		}
		var ids []string
		for _, r := range ans.(page).Rows {
			switch r := r.(type) {
			case hit:
				ids = append(ids, r.Node)
			case changeRow:
				ids = append(ids, r.ID)
			}
		}
		if got := strings.Join(ids, ","); got != c.want {
			t.Fatalf("%s %s gave %s, want %s", c.tool, c.args, got, c.want)
		}
	}
}

// A window answers only for the inputs of its own talks. The inputs tool
// still reads the earlier ones, as context, but the targets stage is not
// given them to answer for.
func TestAWindowAnswersOnlyForItsOwnInputs(t *testing.T) {
	doc, sel, err := Narrow(threeTalks(), Window{FromTalk: "talk/main/2", ToTalk: "talk/main/2"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	l := Compute(doc, "claude-code/1", nil, nil)
	l.Select(sel)
	nav := NewNav(doc, l, nil)
	nav.Within(sel)
	ans, err := nav.Call("inputs", nil)
	if err != nil {
		t.Fatal(err)
	}
	var read []string
	for _, r := range ans.(page).Rows {
		read = append(read, r.(inputRow).Node+":"+r.(inputRow).Window)
	}
	if got := strings.Join(read, ","); got != "input/1:before,input/2:in" {
		t.Fatalf("the inputs tool reads %s", got)
	}
	var given []string
	for _, it := range stageItems(sessionjudge.StageTargets, nav, nil) {
		given = append(given, it.id)
	}
	if got := strings.Join(given, ","); got != "input/2" {
		t.Fatalf("the targets stage is given %s", got)
	}
}

// A row too large for a page keeps, in its note, the address keys it has
// and no other, and only those short enough to keep the note small: a text
// of up to 300 bytes. The row here is a step that started a sub-task of
// thousands of talks.
func TestAnOversizedRowKeepsOnlyItsShortAddresses(t *testing.T) {
	for _, c := range []struct {
		name string
		size int
		want string
	}{
		{"a name of 300 bytes is kept", 300, `{"id":"s2","kind":"tool","name":"` + strings.Repeat("n", 300) + `"}`},
		{"a name of 301 bytes is left out", 301, `{"id":"s2","kind":"tool"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := doc()
			for i := range 2000 {
				d.Talks = append(d.Talks, sessionview.Node{ID: fmt.Sprintf("talk/b/%d", i), Kind: "talk", Stream: "b"})
			}
			d.Talks[1].Children[1].Name = strings.Repeat("n", c.size)
			nav := NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
			ans, err := nav.Call("talk", json.RawMessage(`{"id":"talk/a"}`))
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range ans.(page).Rows {
				if o, ok := r.(oversized); ok {
					if keys, _ := json.Marshal(o.Keys); string(keys) != c.want {
						t.Fatalf("keys %s, want %s", keys, c.want)
					}
					return
				}
			}
			t.Fatal("the step gave no oversized row")
		})
	}
}

// Images are counted only for an input a record stands behind. A talk
// opened by a command typed locally has no input of its own, so its row
// stands on the talk, which no record backs, and it shows no image.
func TestImagesAreCountedOnlyWhereARecordStands(t *testing.T) {
	d := doc()
	d.Talks[0].Children = append([]sessionview.Node{{ID: "in/1", Kind: model.KindMessageExternal, Text: "look",
		Ref: &sessionflow.Ref{Seq: 5, Row: 1}}}, d.Talks[0].Children...)
	d.Talks[3].Label = "/compact"
	nav := NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
	nav.MarkImages(func([]sessionflow.Ref) map[[2]uint64]*sessiondata.Record {
		return map[[2]uint64]*sessiondata.Record{{5, 1}: {Parts: []sessiondata.Part{{Kind: "media", Media: "image/png", Bytes: 75076}}}}
	})
	ans, err := nav.Call("inputs", nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range ans.(page).Rows {
		row := r.(inputRow)
		got = append(got, fmt.Sprintf("%s:%d:%d", row.Node, row.Images, row.ImageBytes))
	}
	if strings.Join(got, ",") != "in/1:1:75076,talk/2:0:0" {
		t.Fatalf("inputs %v", got)
	}
}
