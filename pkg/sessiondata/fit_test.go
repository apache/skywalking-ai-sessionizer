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

package sessiondata

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/apache/skywalking-ai-sessionizer/pkg/changes"
	"github.com/apache/skywalking-ai-sessionizer/pkg/execution"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
)

func hexDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func lineOf(t *testing.T, r *Record) int {
	t.Helper()
	b, err := encodeRecord(r)
	if err != nil {
		t.Fatal(err)
	}
	return len(b)
}

func TestFitOmitsTheLargestPayloadFirst(t *testing.T) {
	r := &Record{Ord: 1, Sha: "x", Bytes: 1, Parts: []Part{
		{Kind: PartText, Text: strings.Repeat("a", 5_000), State: "available", Bytes: 5_000},
		{Kind: PartText, Text: strings.Repeat("b", 30_000), State: "available", Bytes: 30_000},
		{Kind: PartData, Data: json.RawMessage(`"` + strings.Repeat("c", 20_000) + `"`), State: "available", Bytes: 20_002},
	}}
	n, err := Fit(r, 30_000)
	if err != nil || n != 1 {
		t.Fatalf("omitted %d part(s), %v; want the one largest", n, err)
	}
	if r.Parts[1].State != model.ContentTooLarge || r.Parts[0].State != "available" || r.Parts[2].State != "available" {
		t.Fatalf("states %s %s %s, want only the 30 KB text omitted", r.Parts[0].State, r.Parts[1].State, r.Parts[2].State)
	}
	if got := lineOf(t, r); got > 30_000 {
		t.Fatalf("the record is %d bytes after Fit, over 30000", got)
	}
}

func TestFitOmitsEqualPayloadsInOrder(t *testing.T) {
	r := &Record{Ord: 1, Sha: "x", Bytes: 1, Parts: []Part{
		{Kind: PartText, Text: strings.Repeat("a", 10_000)},
		{Kind: PartText, Text: strings.Repeat("b", 10_000)},
	}}
	if n, _ := Fit(r, 15_000); n != 1 || r.Parts[0].State != model.ContentTooLarge || r.Parts[1].State == model.ContentTooLarge {
		t.Fatalf("omitted %d; the first of two equal parts must go first", n)
	}
}

func TestFitKeepsSizeAndDigestOfEachPayload(t *testing.T) {
	text := strings.Repeat("t", 4_000)
	spaced := json.RawMessage("{ \"k\" : \"" + strings.Repeat("d", 4_000) + "\" }")
	var compact bytes.Buffer
	_ = json.Compact(&compact, spaced)
	unknown := Part{Kind: PartUnknown, Text: "why it was not described"}
	raw := []byte("\xff" + strings.Repeat("u", 4_000))
	unknown.SetRaw(raw)
	r := &Record{Ord: 1, Sha: "x", Bytes: 1, Parts: []Part{
		{Kind: PartText, Text: text, Bytes: 4_000},
		{Kind: PartResult, Of: "toolu_1", Text: "the readable form", Data: spaced},
		unknown,
	}}
	if n, err := Fit(r, 100); err != nil || n != 3 {
		t.Fatalf("omitted %d part(s), %v; want all three", n, err)
	}
	for i, want := range []struct {
		sha   string
		bytes int
	}{
		{hexDigest([]byte(text)), 4_000},
		// A part with data and text loses both, and its digest is the data's.
		{hexDigest(compact.Bytes()), compact.Len()},
		// An unknown part's digest is of the bytes it kept, not their base64.
		{hexDigest(raw), len(raw)},
	} {
		p := r.Parts[i]
		if p.State != model.ContentTooLarge || len(p.Data) != 0 || p.Encoding != "" {
			t.Fatalf("part %d still holds a payload: state %s, %d data, encoding %q", i, p.State, len(p.Data), p.Encoding)
		}
		// An unknown part's text is the reason its bytes were not described,
		// not content, so it stays.
		if keep := p.Kind == PartUnknown; (p.Text != "") != keep {
			t.Fatalf("part %d has %d bytes of text left", i, len(p.Text))
		}
		if p.SHA256 != want.sha || p.Bytes != want.bytes {
			t.Fatalf("part %d has digest %s and %d bytes, want %s and %d", i, p.SHA256, p.Bytes, want.sha, want.bytes)
		}
	}
	// Identifiers stay, so a reader still joins the result to its call.
	if r.Parts[1].Of != "toolu_1" || r.Parts[1].Kind != PartResult {
		t.Fatalf("the result lost its identity: %+v", r.Parts[1])
	}
}

func TestFitLandsARecordThatCannotShrinkAsItIs(t *testing.T) {
	r := &Record{Ord: 1, Sha: "x", Bytes: 1, Label: strings.Repeat("l", 10_000),
		Parts: []Part{{Kind: PartText, Text: "short"}}}
	n, err := Fit(r, 1_000)
	if err != nil || n != 1 || r.Label == "" {
		t.Fatalf("omitted %d, %v; want the one payload omitted and the record kept", n, err)
	}
}

// The one file a budget does not hold: a record still larger than the budget
// with every payload taken out lands as it is, alone in its file. The docs
// state this exception, so a receiver is not sized by a promise the collector
// does not keep. The file is read back to show what it carries.
func TestAFileOfOneRecordWithNothingLeftToTakeOutIsOverTheBudget(t *testing.T) {
	const budget = 4 << 10
	h := &Header{Kind: KindTranscript, Session: "s", Src: "a.jsonl", Dialect: "d", Stream: "main", Seq: 1, At: "2026-01-01T00:00:00Z"}
	label, text := strings.Repeat("l", 10_000), strings.Repeat("a", 5_000)
	r := &Record{Ord: 1, Sha: "x", Bytes: 1, Label: label, Parts: []Part{{Kind: PartText, Text: text}}}
	cut, err := NewCut(h, budget)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := cut.Take(r); !ok || err != nil {
		t.Fatalf("the first record of a file was taken %v, %v", ok, err)
	}
	var buf bytes.Buffer
	w, err := NewWriter(&buf, h)
	if err != nil {
		t.Fatal(err)
	}
	w.Budget(budget)
	if err := w.Write(r); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	size := buf.Len()
	if size <= budget {
		t.Fatalf("the file is %d bytes, within the budget of %d; the label should have kept it over", size, budget)
	}
	_, recs, err := All(&buf)
	if err != nil || len(recs) != 1 {
		t.Fatalf("read back %d records, %v", len(recs), err)
	}
	got := recs[0]
	if got.Label != label {
		t.Fatalf("the label read back is %d bytes, want %d", len(got.Label), len(label))
	}
	if p := got.Parts[0]; p.State != model.ContentTooLarge || p.Text != "" || p.SHA256 != hexDigest([]byte(text)) || p.Bytes != len(text) {
		t.Fatalf("the text part is %s with %d bytes of text, digest %q and size %d; want too_large, empty, the text's digest and size",
			p.State, len(p.Text), p.SHA256, p.Bytes)
	}
	t.Logf("a record with a %d-byte label landed as a file of %d bytes under a budget of %d", len(label), size, budget)
}

func TestFitChangesNothingWithoutALimitOrWhenItFits(t *testing.T) {
	for _, max := range []int{0, -1, 1 << 20} {
		r := &Record{Ord: 1, Sha: "x", Bytes: 1, Parts: []Part{{Kind: PartText, Text: strings.Repeat("a", 5_000)}}}
		if n, _ := Fit(r, max); n != 0 || r.Parts[0].Text == "" {
			t.Fatalf("max %d omitted %d part(s)", max, n)
		}
	}
}

func TestWriterBudgetFitsEachRecordIntoItsFile(t *testing.T) {
	const budget = 8 << 10
	var buf bytes.Buffer
	w, err := NewWriter(&buf, &Header{Kind: KindTranscript, Session: "s", Src: "a.jsonl", Dialect: "d", Stream: "main"})
	if err != nil {
		t.Fatal(err)
	}
	w.Budget(budget)
	rec := &Record{Ord: 1, Sha: "x", Bytes: 1, Parts: []Part{{Kind: PartText, Text: strings.Repeat("a", 20_000)}}}
	if err := w.Write(rec); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if buf.Len() > budget {
		t.Fatalf("a file of one record is %d bytes, over the budget of %d", buf.Len(), budget)
	}
	// The caller's record is the one that landed, for its index.
	if rec.Parts[0].State != model.ContentTooLarge {
		t.Fatalf("the caller's record still says %s", rec.Parts[0].State)
	}
	_, recs, err := All(&buf)
	if err != nil || len(recs) != 1 || recs[0].Parts[0].SHA256 != hexDigest([]byte(strings.Repeat("a", 20_000))) {
		t.Fatalf("read back %d records, %v", len(recs), err)
	}
}

func TestWriterBudgetLeavesProviderBodiesToTheirCutter(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewWriter(&buf, &Header{Kind: KindProviderBody, Session: "s", Src: ".", Dialect: "d"})
	if err != nil {
		t.Fatal(err)
	}
	w.Budget(1 << 10)
	rec := &Record{Ord: 1, Sha: "x", Bytes: 1, Parts: []Part{{Kind: PartData, Data: json.RawMessage(`"` + strings.Repeat("p", 5_000) + `"`)}}}
	if err := w.Write(rec); err != nil {
		t.Fatal(err)
	}
	if rec.Parts[0].State == model.ContentTooLarge {
		t.Fatal("the writer omitted a provider body's part; only its cutter may")
	}
}

// changeRecord is a changes/1 record of files, each with one hunk of n lines.
func changeRecord(t *testing.T, files, n int) json.RawMessage {
	t.Helper()
	rec := changes.Record{Schema: changes.Schema, ID: "c1", CapturedBy: changes.CapturedByASZPlugin,
		Session: "s", Stream: "main", Tool: "toolu_1", Time: "2026-01-01T00:00:00Z", Basis: "tool_window"}
	for f := 0; f < files; f++ {
		lines := make([]string, n)
		for i := range lines {
			lines[i] = "+" + strings.Repeat("x", 60)
		}
		rec.Changes = append(rec.Changes, changes.FileChange{Path: fmt.Sprintf("f%d.go", f), Operation: changes.OpModify,
			Diff: changes.DiffAvailable, Additions: changes.Int(n), Deletions: changes.Int(0),
			Hunks: []changes.Hunk{{NewStart: 1, NewLines: n, Lines: lines}}})
	}
	data, err := rec.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A change record keeps its identity, files and counts: its largest files
// lose their hunks first, and only what is still too large is left out.
func TestFitShrinksAChangeRecordBeforeLeavingItOut(t *testing.T) {
	data := changeRecord(t, 3, 100)
	r := &Record{Ord: 1, Sha: "x", Bytes: 1, Tool: "toolu_1", Parts: []Part{{Kind: PartData, Data: data, State: "available", Bytes: len(data)}}}
	if _, err := Fit(r, len(data)/2); err != nil {
		t.Fatal(err)
	}
	p := r.Parts[0]
	got, ok := changes.Decode(p.Data)
	if !ok || p.State != model.ContentTruncated || p.SHA256 != hexDigest(data) || p.Bytes != len(data) {
		t.Fatalf("the change record is %s, decodes %v, digest %q; want truncated, decodable, with the original's digest", p.State, ok, p.SHA256)
	}
	dropped := 0
	for _, c := range got.Changes {
		if c.Diff == changes.DiffTooLarge && c.Hunks == nil && c.Additions != nil && *c.Additions == 100 {
			dropped++
		}
	}
	if dropped == 0 || dropped == len(got.Changes) || len(got.Changes) != 3 {
		t.Fatalf("%d of %d files lost their hunks; want some, with their counts kept", dropped, len(got.Changes))
	}
	// Under a budget even the record without hunks does not fit, its file
	// list goes too, and the record stays: who captured it, its id and its
	// tool, so a reader still sees that the call changed files. It is never
	// taken out whole, and one part changed several times counts once.
	r2 := &Record{Ord: 1, Sha: "x", Bytes: 1, Parts: []Part{{Kind: PartData, Data: data}}}
	n, err := Fit(r2, 100)
	if err != nil || n != 1 {
		t.Fatalf("Fit changed %d part(s), %v; want the one", n, err)
	}
	p2 := r2.Parts[0]
	left, ok := changes.Decode(p2.Data)
	if !ok || p2.State != model.ContentTruncated || p2.SHA256 != hexDigest(data) || left.ID != "c1" || left.Tool != "toolu_1" || len(left.Changes) != 0 {
		t.Fatalf("the change record is %s, decodes %v, with %d files; want it truncated to its identity", p2.State, ok, len(left.Changes))
	}
}

// An execution record keeps sizes and digests only, and is never taken out,
// however small the budget: a step joins to it by its tool call.
func TestFitNeverTakesOutAnExecutionRecord(t *testing.T) {
	data := json.RawMessage(`{"schema":"execution/1","id":"e1","observed_by":"asz-plugin","boundary":"mcp","session":"s","stream":"main","tool":"toolu_1","protocol":"mcp","time":"2026-01-01T00:00:00Z","outcome":"returned","arguments":{"state":"size_only","bytes":10,"sha256":"ab"}}`)
	r := &Record{Ord: 1, Sha: "x", Bytes: 1, Tool: "toolu_1", Parts: []Part{{Kind: PartData, Data: data, State: "available"}}}
	if n, err := Fit(r, 50); err != nil || n != 0 || r.Parts[0].State != "available" || len(r.Parts[0].Data) == 0 {
		t.Fatalf("Fit changed %d part(s), %v, state %s", n, err, r.Parts[0].State)
	}
}

// A record exactly at the limit lands whole; one byte over, it does not.
func TestFitAtTheExactLimit(t *testing.T) {
	r := &Record{Ord: 1, Sha: "x", Bytes: 1, Parts: []Part{{Kind: PartText, Text: strings.Repeat("a", 5_000)}}}
	n := lineOf(t, r)
	if c, _ := Fit(r, n); c != 0 {
		t.Fatalf("a record of exactly %d bytes lost %d part(s)", n, c)
	}
	if c, _ := Fit(r, n-1); c != 1 || r.Parts[0].State != model.ContentTooLarge {
		t.Fatalf("a record one byte over its limit lost %d part(s)", c)
	}
}

// A file is cut by what its records take once encoded: small records, each
// far under the budget, fill files of at most the budget, and the first
// record of a file is always taken.
func TestCutPacksByEncodedSize(t *testing.T) {
	const budget = 2 << 10
	h := &Header{Kind: KindTranscript, Session: "s", Src: "a.jsonl", Dialect: "d", Stream: "main", Seq: 1, At: "2026-01-01T00:00:00Z"}
	var recs []*Record
	for i := 0; i < 100; i++ {
		recs = append(recs, &Record{Ord: uint64(i + 1), Sha: "x", Bytes: 2, ID: fmt.Sprintf("r%03d", i), Parts: []Part{{Kind: PartText, Text: "{}"}}})
	}
	files := 0
	for len(recs) > 0 {
		cut, err := NewCut(h, budget)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		w, err := NewWriter(&buf, h)
		if err != nil {
			t.Fatal(err)
		}
		w.Budget(budget)
		n := 0
		for n < len(recs) {
			ok, err := cut.Take(recs[n])
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				break
			}
			if err := w.Write(recs[n]); err != nil {
				t.Fatal(err)
			}
			n++
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if n == 0 || buf.Len() > budget {
			t.Fatalf("file %d took %d records in %d bytes, budget %d", files, n, buf.Len(), budget)
		}
		recs = recs[n:]
		files++
	}
	if files < 2 {
		t.Fatalf("100 records went into %d file(s) of %d bytes", files, budget)
	}
	// One record larger than the budget is still taken, fitted.
	cut, _ := NewCut(h, budget)
	big := &Record{Ord: 1, Sha: "x", Bytes: 1, Parts: []Part{{Kind: PartText, Text: strings.Repeat("a", 10_000)}}}
	if ok, err := cut.Take(big); !ok || err != nil || big.Parts[0].State != model.ContentTooLarge {
		t.Fatalf("a large first record was taken %v, %v, state %s", ok, err, big.Parts[0].State)
	}
}

// A line that is already encoded is cut by the rule a record is: lines fill
// files of at most the budget, header and closing line included, and the
// first line of a file is always taken, unchanged, even when it alone is
// larger than the budget.
func TestCutTakesARawLineByTheSameRule(t *testing.T) {
	const budget = 2 << 10
	h := &Header{Kind: KindTranscript, Session: "s", Src: "a.jsonl", Dialect: "d", Stream: "main", Seq: 1, At: "2026-01-01T00:00:00Z"}
	var lines [][]byte
	for i := 0; i < 100; i++ {
		line, err := encodeRecord(&Record{Ord: uint64(i + 1), Sha: "x", Bytes: 2, ID: fmt.Sprintf("r%03d", i), Parts: []Part{{Kind: PartText, Text: "{}"}}})
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	files := 0
	for len(lines) > 0 {
		cut, err := NewCut(h, budget)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		w, err := NewWriter(&buf, h)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for n < len(lines) && cut.TakeRaw(lines[n]) {
			if err := w.WriteRaw(lines[n]); err != nil {
				t.Fatal(err)
			}
			n++
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if n == 0 || buf.Len() > budget {
			t.Fatalf("file %d took %d lines in %d bytes, budget %d", files, n, buf.Len(), budget)
		}
		lines = lines[n:]
		files++
	}
	if files < 2 {
		t.Fatalf("100 lines went into %d file(s) of %d bytes", files, budget)
	}
	cut, err := NewCut(h, budget)
	if err != nil {
		t.Fatal(err)
	}
	big := bytes.Repeat([]byte("a"), 10_000)
	if !cut.TakeRaw(big) || cut.TakeRaw([]byte("{}")) {
		t.Fatal("a large first line was not taken alone")
	}
}

// A part with text and data loses its data first, the larger and the
// structured copy, and keeps its readable text, saying truncated; only a
// tighter limit takes the text too. The digest stays the data's.
func TestFitTakesDataBeforeText(t *testing.T) {
	data := json.RawMessage(`{"content":"` + strings.Repeat("c", 20_000) + `"}`)
	mk := func() *Record {
		return &Record{Ord: 1, Sha: "x", Bytes: 1, Parts: []Part{
			{Kind: PartResult, Of: "toolu_1", Text: "File created successfully", Data: data, State: "available", Bytes: 25},
		}}
	}
	r := mk()
	if _, err := Fit(r, 1_000); err != nil {
		t.Fatal(err)
	}
	if p := r.Parts[0]; p.State != model.ContentTruncated || p.Text != "File created successfully" || len(p.Data) != 0 || p.SHA256 != hexDigest(data) {
		t.Fatalf("the result is %s with text %q and digest %q; want truncated, its text kept, the data's digest", p.State, p.Text, p.SHA256)
	}
	r = mk()
	if _, err := Fit(r, 50); err != nil {
		t.Fatal(err)
	}
	if p := r.Parts[0]; p.State != model.ContentTooLarge || p.Text != "" || p.SHA256 != hexDigest(data) {
		t.Fatalf("under a tighter limit the result is %s with text %q and digest %q; want too_large, the data's digest", p.State, p.Text, p.SHA256)
	}
}

// A file of one record is at most the budget, whether the record fits as it
// is or is fitted, for every text size around the budget. Cut fits a record
// as Writer.Budget does, its newline counted.
func TestACutRecordAloneFitsItsFile(t *testing.T) {
	h := &Header{Kind: KindTranscript, Session: "s", Src: "a.jsonl", Dialect: "d", Stream: "main", Seq: 1, At: "2026-01-01T00:00:00Z"}
	const budget = 1024
	for n := 400; n < 1200; n++ {
		cut, err := NewCut(h, budget)
		if err != nil {
			t.Fatal(err)
		}
		r := &Record{Ord: 1, Sha: "x", Bytes: 1, Parts: []Part{{Kind: PartText, Text: strings.Repeat("a", n)}}}
		if ok, err := cut.Take(r); !ok || err != nil {
			t.Fatal(ok, err)
		}
		var buf bytes.Buffer
		w, _ := NewWriter(&buf, h)
		w.Budget(budget)
		_ = w.Write(r)
		_ = w.Close()
		if buf.Len() > budget {
			t.Fatalf("a text of %d bytes made a file of %d bytes", n, buf.Len())
		}
	}
}

// A change record or an execution record that carries a field nested deeper
// than a line may hold is never taken out whole: a step joins to it by its
// tool call. It is written again in its own format, which leaves out the field
// its format does not name, and says truncated, with the size and the digest
// of what it held.
func TestADeepFieldLeavesAChangeOrExecutionRecordInPlace(t *testing.T) {
	deep := strings.Repeat("[", 300) + "0" + strings.Repeat("]", 300)
	change := changeRecord(t, 1, 2)
	observed := `{"schema":"execution/1","id":"e1","observed_by":"asz-plugin","boundary":"mcp","session":"s","stream":"main","tool":"toolu_1","protocol":"mcp","time":"2026-01-01T00:00:00Z","outcome":"returned"}`
	for name, data := range map[string]json.RawMessage{
		"change":    json.RawMessage(strings.TrimSuffix(string(change), "}") + `,"extra":` + deep + `}`),
		"execution": json.RawMessage(strings.TrimSuffix(observed, "}") + `,"extra":` + deep + `}`),
	} {
		t.Run(name, func(t *testing.T) {
			r := &Record{Ord: 1, Tool: "toolu_1", Parts: []Part{{Kind: PartData, Data: data}}}
			line, n, err := fit(r, 0, true)
			if err != nil || n != 1 {
				t.Fatalf("fit changed %d part(s), %v", n, err)
			}
			if d := Depth(line); d > MaxLineDepth {
				t.Fatalf("the line holds %d levels", d)
			}
			p := r.Parts[0]
			if p.State != model.ContentTruncated || p.SHA256 != hexDigest(data) || p.Bytes != len(data) || strings.Contains(string(p.Data), "extra") {
				t.Fatalf("the record is %s with digest %q and %d bytes: %s", p.State, p.SHA256, p.Bytes, p.Data)
			}
			if name == "change" {
				if rec, ok := changes.Decode(p.Data); !ok || rec.ID != "c1" || rec.Tool != "toolu_1" || len(rec.Changes) != 1 {
					t.Fatalf("the change record lost its identity or files: %s", p.Data)
				}
			} else if rec, ok := execution.Decode(p.Data); !ok || rec.ID != "e1" || rec.Tool != "toolu_1" || rec.Outcome != "returned" {
				t.Fatalf("the execution record lost its identity: %s", p.Data)
			}
		})
	}
}

// A change record or an execution record holding a byte that is not valid
// UTF-8 is never taken out whole either. It is written again in its own
// format, which reads the byte as U+FFFD, and says truncated, with the size
// and the digest of what it held. The record's dropped list says why.
func TestABadByteLeavesAChangeOrExecutionRecordInPlace(t *testing.T) {
	change := strings.Replace(string(changeRecord(t, 1, 2)), `"f0.go"`, "\"f0\xff.go\"", 1)
	observed := "{\"schema\":\"execution/1\",\"id\":\"e1\",\"observed_by\":\"asz-plugin\",\"boundary\":\"mcp\",\"session\":\"s\",\"stream\":\"main\",\"tool\":\"toolu_1\",\"protocol\":\"mcp\",\"time\":\"2026-01-01T00:00:00Z\",\"outcome\":\"returned\",\"cwd\":\"a\xffb\"}"
	for name, data := range map[string]json.RawMessage{
		"change":    json.RawMessage(change),
		"execution": json.RawMessage(observed),
	} {
		t.Run(name, func(t *testing.T) {
			r := &Record{Ord: 1, Tool: "toolu_1", Parts: []Part{{Kind: PartData, Data: data}}}
			line, n, err := fit(r, 0, true)
			if err != nil || n != 1 {
				t.Fatalf("fit changed %d part(s), %v", n, err)
			}
			if !utf8.Valid(line) {
				t.Fatalf("the line holds a byte that is not valid UTF-8: %q", line)
			}
			p := r.Parts[0]
			if p.State != model.ContentTruncated || p.SHA256 != hexDigest(data) || p.Bytes != len(data) {
				t.Fatalf("the record is %s with digest %q and %d bytes: %s", p.State, p.SHA256, p.Bytes, p.Data)
			}
			if want := []Drop{{What: "the data of part 0", Bytes: len(data), Why: WhyNotUTF8}}; !reflect.DeepEqual(r.Dropped, want) {
				t.Fatalf("the record drops %+v; want %+v", r.Dropped, want)
			}
			if name == "change" {
				if rec, ok := changes.Decode(p.Data); !ok || rec.ID != "c1" || len(rec.Changes) != 1 || rec.Changes[0].Path != "f0\xef\xbf\xbd.go" {
					t.Fatalf("the change record lost its identity or files: %s", p.Data)
				}
			} else if rec, ok := execution.Decode(p.Data); !ok || rec.ID != "e1" || rec.Tool != "toolu_1" || rec.Outcome != "returned" {
				t.Fatalf("the execution record lost its identity: %s", p.Data)
			}
		})
	}
}

// A byte that is not valid UTF-8 is named in the record's dropped list
// whatever else is wrong with its data. Data nested too deep as well is taken
// out as too deep data is, too_large, and a change or execution record with a
// deep field as well is written again in its own format, truncated; each
// record still says the byte was there.
func TestABadByteIsNamedWhenItsDataIsAlsoTooDeep(t *testing.T) {
	nest := func(inner string) string { return strings.Repeat("[", 300) + inner + strings.Repeat("]", 300) }
	change := strings.Replace(string(changeRecord(t, 1, 2)), `"f0.go"`, "\"f0\xff.go\"", 1)
	change = strings.TrimSuffix(change, "}") + `,"extra":` + nest("0") + `}`
	observed := "{\"schema\":\"execution/1\",\"id\":\"e1\",\"observed_by\":\"asz-plugin\",\"boundary\":\"mcp\",\"session\":\"s\",\"stream\":\"main\",\"tool\":\"toolu_1\",\"protocol\":\"mcp\",\"time\":\"2026-01-01T00:00:00Z\",\"outcome\":\"returned\",\"cwd\":\"a\xffb\",\"extra\":" +
		nest("0") + "}"
	for name, c := range map[string]struct {
		data  string
		state string
	}{
		"data":      {nest("\"\xff\""), model.ContentTooLarge},
		"change":    {change, model.ContentTruncated},
		"execution": {observed, model.ContentTruncated},
	} {
		t.Run(name, func(t *testing.T) {
			data := json.RawMessage(c.data)
			r := &Record{Ord: 1, Tool: "toolu_1", Parts: []Part{{Kind: PartData, Data: data}}}
			line, n, err := fit(r, 0, true)
			if err != nil || n != 1 {
				t.Fatalf("fit changed %d part(s), %v", n, err)
			}
			if !utf8.Valid(line) || Depth(line) > MaxLineDepth {
				t.Fatalf("the line is not valid UTF-8 or nests too deep: %q", line)
			}
			if p := r.Parts[0]; p.State != c.state || p.SHA256 != hexDigest(data) || p.Bytes != len(data) {
				t.Fatalf("the part is %s with digest %q and %d bytes", p.State, p.SHA256, p.Bytes)
			}
			if want := []Drop{{What: "the data of part 0", Bytes: len(data), Why: WhyNotUTF8}}; !reflect.DeepEqual(r.Dropped, want) {
				t.Fatalf("the record drops %+v; want %+v", r.Dropped, want)
			}
		})
	}
}
