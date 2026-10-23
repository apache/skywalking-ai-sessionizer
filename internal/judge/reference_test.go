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
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
)

// ticket is a source of five lines, which a call of the reference stage is
// given over worked.
func ticket() *Nav {
	nav, _ := worked()
	nav.Assign(Assignment{Sources: []SourceDoc{{ID: "R1", Source: "OPS-1.md",
		Text: "# OPS-1\nthe cache was ruled out by the hit rate\nroot cause: the clock drifts\nfix: sync the clock\nseen on 0.4.0\n"}}})
	return nav
}

func conclusionJSON(over map[string]any) string {
	c := map[string]any{"id": "r1", "source": "R1", "kind": "final", "statement": "the clock drifts", "expectation": "required",
		"scope": []string{"in/1"}, "context": []any{map[string]any{"source": "R1", "lines": []int{3, 3}, "note": "the cause"}},
		"builds_on": []string{}, "support": "saved", "author": "", "revision": "0.4.0", "environment": "", "conflicts": []string{},
		"ambiguity": "", "not_covered": ""}
	for k, v := range over {
		c[k] = v
	}
	b, _ := json.Marshal(c)
	return string(b)
}

func concluded(t *testing.T, entries ...string) ([]*sessionjudge.Conclusion, string) {
	t.Helper()
	c, err := checkReference(ticket(), json.RawMessage(`{"conclusions":[`+strings.Join(entries, ",")+`]}`))
	if err != nil {
		t.Fatal(err)
	}
	var out []*sessionjudge.Conclusion
	for _, f := range c.frames {
		out = append(out, f.(*sessionjudge.Conclusion))
	}
	return out, c.note()
}

// A conclusion is kept with what it rests on, its scope bound to the inputs
// of the conversation.
func TestAConclusionKeepsWhatItRestsOn(t *testing.T) {
	cs, note := concluded(t, conclusionJSON(nil), conclusionJSON(map[string]any{"id": "r2", "kind": "intermediate", "statement": "the cache is fine",
		"expectation": "explanatory", "scope": []string{"in/1", "in/9", "in/1"}, "builds_on": []string{"r1"}, "conflicts": []string{"r1"}, "support": "linked",
		"context": []any{}}))
	if note != "" {
		t.Fatalf("note %s", note)
	}
	r1, r2 := cs[0], cs[1]
	if r1.ID != "r1" || r1.Kind != "final" || fmt.Sprint(r1.Scope) != "[in/1]" || r1.Support != "saved" || r1.Context[0].Lines != [2]int{3, 3} || r1.Revision != "0.4.0" {
		t.Fatalf("r1 %+v", r1)
	}
	if fmt.Sprint(r2.Scope) != "[in/1]" || fmt.Sprint(r2.Unbound) != "[in/9]" || fmt.Sprint(r2.BuildsOn) != "[r1]" || fmt.Sprint(r2.Conflicts) != "[r1]" || r2.Support != "linked" {
		t.Fatalf("r2 %+v", r2)
	}
}

// Each rule of a conclusion on its own.
func TestEachConclusionRuleStandsAlone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		over  map[string]any
		check func(cs []*sessionjudge.Conclusion) string
		note  string
	}{
		{"no id", map[string]any{"id": " "}, func(cs []*sessionjudge.Conclusion) string { return fmt.Sprint(len(cs)) + " kept" }, "1 conclusions with no unique id"},
		{"no source of the call", map[string]any{"source": "R9"}, nil, "1 conclusions with no unique id"},
		{"no statement", map[string]any{"statement": ""}, nil, "1 conclusions with no unique id"},
		{"a kind asz does not know", map[string]any{"kind": "partial"}, nil, "1 conclusions with no unique id"},
		{"an expectation asz does not know", map[string]any{"expectation": "maybe"}, func(cs []*sessionjudge.Conclusion) string {
			return cs[0].Expectation
		}, "read as explanatory or asserted"},
		{"saved with no line cited", map[string]any{"context": []any{}}, func(cs []*sessionjudge.Conclusion) string { return cs[0].Support }, "read as explanatory or asserted"},
		{"a support asz does not know", map[string]any{"support": "proven"}, func(cs []*sessionjudge.Conclusion) string { return cs[0].Support }, "read as explanatory or asserted"},
		{"lines past the source", map[string]any{"context": []any{map[string]any{"source": "R1", "lines": []int{4, 9}, "note": ""}}, "support": "linked"},
			func(cs []*sessionjudge.Conclusion) string { return fmt.Sprint(len(cs[0].Context)) }, "1 citations of lines the source does not have"},
		{"lines backwards", map[string]any{"context": []any{map[string]any{"source": "R1", "lines": []int{3, 2}, "note": ""}}, "support": "linked"},
			nil, "1 citations of lines the source does not have"},
		{"line zero", map[string]any{"context": []any{map[string]any{"source": "R1", "lines": []int{0, 2}, "note": ""}}, "support": "linked"},
			nil, "1 citations of lines the source does not have"},
		{"lines of another source", map[string]any{"context": []any{map[string]any{"source": "R2", "lines": []int{1, 1}, "note": ""}}, "support": "linked"},
			nil, "1 citations of lines the source does not have"},
		{"builds on no conclusion", map[string]any{"builds_on": []string{"r7"}}, func(cs []*sessionjudge.Conclusion) string { return fmt.Sprint(cs[0].BuildsOn) },
			"1 references to no other conclusion"},
		{"builds on itself", map[string]any{"builds_on": []string{"r1"}}, func(cs []*sessionjudge.Conclusion) string { return fmt.Sprint(cs[0].BuildsOn) },
			"1 references to no other conclusion"},
		{"conflicts with no conclusion", map[string]any{"conflicts": []string{"r7"}}, func(cs []*sessionjudge.Conclusion) string { return fmt.Sprint(cs[0].Conflicts) },
			"1 references to no other conclusion"},
		{"conflicts with itself", map[string]any{"conflicts": []string{"r1"}}, func(cs []*sessionjudge.Conclusion) string { return fmt.Sprint(cs[0].Conflicts) },
			"1 references to no other conclusion"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs, note := concluded(t, conclusionJSON(tc.over))
			if !strings.Contains(note, tc.note) {
				t.Fatalf("note %q, want %q", note, tc.note)
			}
			if tc.check == nil {
				return
			}
			want := map[string]string{"no id": "0 kept", "an expectation asz does not know": "explanatory", "saved with no line cited": "asserted",
				"a support asz does not know": "asserted", "lines past the source": "0", "builds on no conclusion": "[]", "builds on itself": "[]",
				"conflicts with no conclusion": "[]", "conflicts with itself": "[]"}[tc.name]
			if got := tc.check(cs); got != want {
				t.Fatalf("got %s, want %s", got, want)
			}
		})
	}
	// Two conclusions with one id: the first is kept.
	cs, note := concluded(t, conclusionJSON(nil), conclusionJSON(map[string]any{"statement": "again"}))
	if len(cs) != 1 || cs[0].Statement != "the clock drifts" || !strings.Contains(note, "1 conclusions with no unique id") {
		t.Fatalf("twice: %+v %q", cs, note)
	}
	// A builds_on that would close a cycle is left out.
	cs, note = concluded(t, conclusionJSON(map[string]any{"builds_on": []string{"r2"}}), conclusionJSON(map[string]any{"id": "r2", "builds_on": []string{"r1"}}))
	if fmt.Sprint(cs[0].BuildsOn) != "[r2]" || fmt.Sprint(cs[1].BuildsOn) != "[]" || !strings.Contains(note, "1 references to no other conclusion, to a correction from another source, or ones that would close a cycle") {
		t.Fatalf("cycle: %v %v %q", cs[0].BuildsOn, cs[1].BuildsOn, note)
	}
	// A conclusion its source corrected later names the correction; one that
	// would close a chain of corrections does not, nor one naming itself or
	// no conclusion.
	cs, note = concluded(t, conclusionJSON(map[string]any{"superseded_by": " r2 "}), conclusionJSON(map[string]any{"id": "r2", "superseded_by": "r3"}),
		conclusionJSON(map[string]any{"id": "r3", "superseded_by": "r1"}), conclusionJSON(map[string]any{"id": "r4", "superseded_by": "r4"}),
		conclusionJSON(map[string]any{"id": "r5", "superseded_by": "r9"}))
	if cs[0].SupersededBy != "r2" || cs[1].SupersededBy != "r3" || cs[2].SupersededBy != "" || cs[3].SupersededBy != "" || cs[4].SupersededBy != "" ||
		!strings.Contains(note, "3 references to no other conclusion") {
		t.Fatalf("superseded: %q %q %q %q %q; %q", cs[0].SupersededBy, cs[1].SupersededBy, cs[2].SupersededBy, cs[3].SupersededBy, cs[4].SupersededBy, note)
	}
	// Only a conclusion of the same source corrects one.
	two := ticket()
	two.assigned.Sources = append(two.assigned.Sources, SourceDoc{ID: "R2", Source: "OPS-2.md", Text: "the clock is fine\n"})
	c, err := checkReference(two, json.RawMessage(`{"conclusions":[`+conclusionJSON(map[string]any{"superseded_by": "r2"})+","+
		conclusionJSON(map[string]any{"id": "r2", "source": "R2", "context": []any{}, "support": "asserted"})+`]}`))
	if err != nil || c.frames[0].(*sessionjudge.Conclusion).SupersededBy != "" || !strings.Contains(c.note(), "1 references to no other conclusion") {
		t.Fatalf("another source: %v %+v %q", err, c.frames[0], c.note())
	}
	// The source tool gives lines from where it is asked, numbered.
	rows, err := ticket().sourceRows("R1", 3)
	if b, _ := json.Marshal(rows); err != nil || string(b) != `[{"line":3,"text":"root cause: the clock drifts"},{"line":4,"text":"fix: sync the clock"},{"line":5,"text":"seen on 0.4.0"}]` {
		t.Fatalf("rows %s %v", b, err)
	}
	if _, err := ticket().sourceRows("R9", 1); err == nil {
		t.Fatal("a source the call was not given was read")
	}
	// With no id, the source tool lists the sources the call was given, so
	// a call whose prompt was cut short still finds every one; the
	// conclusions tool lists the conclusions a call compares.
	listed := ticket()
	listed.Assign(Assignment{Sources: append(slices.Clone(listed.assigned.Sources), SourceDoc{ID: "R2", Source: "https://jira.example/OPS-2", Text: "one\ntwo"}),
		Conclusions: []sessionjudge.Conclusion{{ID: "r1", Source: "R1", Statement: "the clock drifts"}}})
	for tool, want := range map[string]string{
		"source":      `{"id":"R1","lines":5,"source":"OPS-1.md"},{"id":"R2","lines":2,"source":"https://jira.example/OPS-2"}`,
		"conclusions": `"id":"r1","source":"R1","kind":"","statement":"the clock drifts"`,
	} {
		ans, err := listed.Call(tool, json.RawMessage(`{}`))
		b, _ := json.Marshal(ans)
		if err != nil || !strings.Contains(string(b), want) {
			t.Fatalf("%s: %s %v", tool, b, err)
		}
	}
	// A line too long for one row comes in parts of whole characters, which
	// together are the line.
	wide := strings.Repeat("═", 1500)
	long := ticket()
	long.assigned.Sources[0].Text = "first\n" + wide + "\n"
	rows, _ = long.sourceRows("R1", 2)
	var parts []string
	var whole strings.Builder
	for _, r := range rows {
		m := r.(map[string]any)
		text := m["text"].(string)
		if m["line"] != 2 || len(text) > sourceLineBytes || !utf8.ValidString(text) {
			t.Fatalf("row %v of %d bytes", m["line"], len(text))
		}
		parts = append(parts, fmt.Sprint(m["part"]))
		whole.WriteString(text)
	}
	if strings.Join(parts, " ") != "1 2 3" || whole.String() != wide {
		t.Fatalf("parts %v", parts)
	}
}
