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

package changes_test

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/pkg/changes"
)

func lines(h changes.Hunk) string { return strings.Join(h.Lines, "\n") }

func TestDiffOneLineReplaced(t *testing.T) {
	before := changes.SplitLines([]byte("a\nb\nc\nd\ne\nf\ng\nh\n"))
	after := changes.SplitLines([]byte("a\nb\nc\nd\nE\nf\ng\nh\n"))
	hunks, add, del, ok := changes.Diff(before, after)
	if !ok || add != 1 || del != 1 || len(hunks) != 1 {
		t.Fatalf("hunks=%d add=%d del=%d ok=%v", len(hunks), add, del, ok)
	}
	h := hunks[0]
	if h.OldStart != 2 || h.OldLines != 7 || h.NewStart != 2 || h.NewLines != 7 {
		t.Fatalf("header @@ -%d,%d +%d,%d @@", h.OldStart, h.OldLines, h.NewStart, h.NewLines)
	}
	want := " b\n c\n d\n-e\n+E\n f\n g\n h"
	if got := lines(h); got != want {
		t.Fatalf("lines:\n%s\nwant:\n%s", got, want)
	}
}

func TestDiffCreatedAndDeleted(t *testing.T) {
	empty := changes.SplitLines(nil)
	two := changes.SplitLines([]byte("x\ny\n"))
	hunks, add, del, _ := changes.Diff(empty, two)
	if add != 2 || del != 0 || len(hunks) != 1 {
		t.Fatalf("create: hunks=%d add=%d del=%d", len(hunks), add, del)
	}
	if h := hunks[0]; h.OldStart != 0 || h.OldLines != 0 || h.NewStart != 1 || h.NewLines != 2 {
		t.Fatalf("create header @@ -%d,%d +%d,%d @@", h.OldStart, h.OldLines, h.NewStart, h.NewLines)
	}
	hunks, add, del, _ = changes.Diff(two, empty)
	if add != 0 || del != 2 || len(hunks) != 1 {
		t.Fatalf("delete: hunks=%d add=%d del=%d", len(hunks), add, del)
	}
	if h := hunks[0]; h.OldStart != 1 || h.OldLines != 2 || h.NewStart != 0 || h.NewLines != 0 {
		t.Fatalf("delete header @@ -%d,%d +%d,%d @@", h.OldStart, h.OldLines, h.NewStart, h.NewLines)
	}
}

func TestDiffTwoDistantChangesAreTwoHunks(t *testing.T) {
	var sb strings.Builder
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	before := changes.SplitLines([]byte(sb.String()))
	afterText := strings.Replace(strings.Replace(sb.String(), "line 5\n", "LINE 5\n", 1), "line 25\n", "line 25\nline 25b\n", 1)
	after := changes.SplitLines([]byte(afterText))
	hunks, add, del, _ := changes.Diff(before, after)
	if len(hunks) != 2 || add != 2 || del != 1 {
		t.Fatalf("hunks=%d add=%d del=%d", len(hunks), add, del)
	}
	if hunks[0].OldStart != 2 || hunks[1].OldStart != 23 {
		t.Fatalf("starts %d and %d", hunks[0].OldStart, hunks[1].OldStart)
	}
}

func TestDiffNearChangesMergeIntoOneHunk(t *testing.T) {
	before := changes.SplitLines([]byte("1\n2\n3\n4\n5\n6\n7\n8\n9\n"))
	after := changes.SplitLines([]byte("1\nX\n3\n4\n5\n6\nY\n8\n9\n"))
	hunks, _, _, _ := changes.Diff(before, after)
	if len(hunks) != 1 {
		t.Fatalf("hunks=%d, want one: the two changes are five lines apart and share context", len(hunks))
	}
}

func TestDiffRemovedBeforeAddedInABlock(t *testing.T) {
	before := changes.SplitLines([]byte("a\nb\nc\n"))
	after := changes.SplitLines([]byte("a\nB\nC\n"))
	hunks, _, _, _ := changes.Diff(before, after)
	if got, want := lines(hunks[0]), " a\n-b\n-c\n+B\n+C"; got != want {
		t.Fatalf("lines:\n%s\nwant:\n%s", got, want)
	}
}

func TestSplitLinesKeepsEndings(t *testing.T) {
	crlf := changes.SplitLines([]byte("a\r\nb\r\n"))
	if len(crlf.Lines) != 2 || crlf.Lines[0] != "a\r" || crlf.NoNewlineAtEnd {
		t.Fatalf("crlf: %#v", crlf)
	}
	noEOL := changes.SplitLines([]byte("a\nb"))
	if len(noEOL.Lines) != 2 || !noEOL.NoNewlineAtEnd {
		t.Fatalf("no final newline: %#v", noEOL)
	}
	if e := changes.SplitLines(nil); len(e.Lines) != 0 || e.NoNewlineAtEnd {
		t.Fatalf("empty: %#v", e)
	}
	lf := changes.SplitLines([]byte("a\n"))
	crlfOne := changes.SplitLines([]byte("a\r\n"))
	if _, add, del, _ := changes.Diff(lf, crlfOne); add != 1 || del != 1 {
		t.Fatalf("LF and CRLF must compare as different lines: add=%d del=%d", add, del)
	}
}

func TestIsText(t *testing.T) {
	if !changes.IsText([]byte("plain\n")) || changes.IsText([]byte("a\x00b")) || changes.IsText([]byte{0xff, 0xfe}) {
		t.Fatal("text detection")
	}
}

// TestDiffIsAValidEditScript applies the hunks back to the old text on
// random inputs and checks the new text comes out, which is the one
// property every diff must have whatever its shape.
func TestDiffIsAValidEditScript(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	words := []string{"a", "b", "c", "d", "e"}
	for round := 0; round < 300; round++ {
		n := rng.Intn(40)
		old := make([]string, n)
		for i := range old {
			old[i] = words[rng.Intn(len(words))]
		}
		cur := append([]string{}, old...)
		for k := rng.Intn(10); k > 0; k-- {
			switch rng.Intn(3) {
			case 0:
				if len(cur) > 0 {
					i := rng.Intn(len(cur))
					cur = append(cur[:i], cur[i+1:]...)
				}
			case 1:
				i := rng.Intn(len(cur) + 1)
				cur = append(cur[:i], append([]string{words[rng.Intn(len(words))]}, cur[i:]...)...)
			case 2:
				if len(cur) > 0 {
					cur[rng.Intn(len(cur))] = words[rng.Intn(len(words))]
				}
			}
		}
		hunks, _, _, ok := changes.Diff(changes.Text{Lines: old}, changes.Text{Lines: cur})
		if !ok {
			t.Fatal("not ok")
		}
		got := apply(old, hunks)
		if strings.Join(got, "\n") != strings.Join(cur, "\n") {
			t.Fatalf("round %d: applying the hunks to\n%v\ngave\n%v\nwant\n%v\nhunks %+v", round, old, got, cur, hunks)
		}
	}
}

// apply is a small patch: it walks the old lines and the hunks together.
func apply(old []string, hunks []changes.Hunk) []string {
	var out []string
	pos := 0 // next old line to copy, 0-based
	for _, h := range hunks {
		start := h.OldStart - 1
		if h.OldLines == 0 {
			start = h.OldStart
		}
		out = append(out, old[pos:start]...)
		pos = start
		for _, l := range h.Lines {
			switch l[0] {
			case ' ':
				out = append(out, old[pos])
				pos++
			case '-':
				pos++
			case '+':
				out = append(out, l[1:])
			}
		}
	}
	return append(out, old[pos:]...)
}

func TestRecordRoundTrip(t *testing.T) {
	r := &changes.Record{
		Schema: changes.Schema, ID: "p1/c42", Session: "S1", Stream: "main", Tool: "toolu_1", ToolName: "Bash",
		Time: "2026-09-08T02:00:04Z", Basis: changes.BasisToolWindow,
		Root:         &changes.Root{Path: "/workspace/project"},
		ChangedFiles: changes.Int(1),
		Changes: []changes.FileChange{{
			Path: "server.go", Operation: changes.OpModify,
			Before: changes.Endpoint{Present: true, Bytes: changes.Int64(10), SHA256: "old"},
			After:  changes.Endpoint{Present: true, Bytes: changes.Int64(10), SHA256: "new"},
			Diff:   changes.DiffAvailable, Additions: changes.Int(1), Deletions: changes.Int(1),
			Hunks: []changes.Hunk{{OldStart: 12, OldLines: 1, NewStart: 12, NewLines: 1, Lines: []string{"-timeout := 10", "+timeout := 30"}}},
		}},
	}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := r.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	// Keys come out in the order the type lists them, schema first.
	if !strings.HasPrefix(string(data), `{"schema":"changes/1","id":"p1/c42",`) {
		t.Fatalf("key order: %s", data[:60])
	}
	if strings.Contains(string(data), "\n") {
		t.Fatal("a record is one line")
	}
	back, ok := changes.Decode(data)
	if !ok || back.ID != r.ID || len(back.Changes) != 1 || back.Changes[0].Hunks[0].Lines[1] != "+timeout := 30" {
		t.Fatalf("decode: ok=%v %+v", ok, back)
	}
	if _, ok := changes.Decode([]byte(`{"schema":"other/1"}`)); ok {
		t.Fatal("another schema decoded as a change record")
	}
	// The null-when-unknown fields are written as null, never dropped.
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(data, &raw)
	if string(raw["changed_files"]) != "1" {
		t.Fatalf("changed_files: %s", raw["changed_files"])
	}
	empty := &changes.Record{Schema: changes.Schema, ID: "x", Session: "S", Stream: "main", Time: "t", Basis: changes.BasisUnattributed}
	data, _ = empty.Marshal()
	_ = json.Unmarshal(data, &raw)
	if string(raw["changed_files"]) != "null" || string(raw["changes"]) != "[]" {
		t.Fatalf("unknown count and no changes: %s", data)
	}
}

func TestValidate(t *testing.T) {
	r := &changes.Record{Schema: changes.Schema, ID: "x", Session: "S", Stream: "main", Time: "t", Basis: changes.BasisToolWindow}
	if err := r.Validate(); err == nil {
		t.Fatal("an attributed record without a tool must not validate")
	}
	r.Tool = "toolu_1"
	r.Changes = []changes.FileChange{{Path: "a", Operation: changes.OpModify}}
	if err := r.Validate(); err == nil {
		t.Fatal("a change that says nothing about its diff must not validate")
	}
}
