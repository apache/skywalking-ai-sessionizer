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

package skeleton_test

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/pkg/changes"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/skeleton"
)

func landed(t *testing.T, hdr *sessiondata.Header, recs ...*sessiondata.Record) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := sessiondata.NewWriter(&buf, hdr)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if err := w.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// read decompresses a skeleton and reads it to its closing line, which the
// reader checks.
func read(t *testing.T, sk []byte) (sessiondata.Header, []*sessiondata.Record) {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(sk))
	if err != nil {
		t.Fatalf("a skeleton is not gzip: %v", err)
	}
	hdr, recs, err := sessiondata.All(zr)
	if err != nil {
		t.Fatalf("a skeleton does not read as Session Data: %v", err)
	}
	return hdr, recs
}

func header(kind sessiondata.Kind) *sessiondata.Header {
	return &sessiondata.Header{H: 1, Schema: sessiondata.Schema, Seq: 7, At: "2026-10-01T00:00:00Z", Kind: kind,
		Adapter: "test/0", Dialect: "test/1", Src: "p/s.jsonl", Session: "s", Stream: "main"}
}

func intp(n int) *int { return &n }

// A skeleton keeps every record, every part in its place and every field the
// index reads, and nothing a person wrote: text, data, media and encoding go,
// and each part says it was removed, or too large to land when it was. Its
// header says what it replaced.
func TestASkeletonKeepsStructureOnly(t *testing.T) {
	failed := true
	change := changes.Record{Schema: changes.Schema, ID: "toolu_1", CapturedBy: changes.CapturedByClaudeCode, Session: "s",
		Stream: "main", Tool: "toolu_1", Time: "2026-10-01T00:00:01Z", Basis: "attributed",
		Root: &changes.Root{Path: "/work"}, Window: &changes.Window{},
		Changes: []changes.FileChange{
			{Path: "a.go", Operation: "modify", Diff: changes.DiffAvailable, Additions: intp(3), Deletions: intp(1),
				Before: changes.Endpoint{Present: true, SHA256: "aa"}, After: changes.Endpoint{Present: true, SHA256: "bb"},
				Windows: []string{"w1"}, Hunks: []changes.Hunk{{Lines: []string{"+secret"}}}},
			{Path: "b.bin", Operation: "modify", Diff: "binary"},
		}}
	changeData, _ := json.Marshal(change)
	unknown := sessiondata.Part{Kind: sessiondata.PartUnknown, State: model.ContentAvailable, Bytes: 4}
	unknown.SetRaw([]byte{0xff, 0xfe, 'a', 'b'})
	rec := &sessiondata.Record{
		Ord: 3, Off: 120, Sha: "abc", Bytes: 900, ID: "m1", Parent: "m0", Call: "c1", Run: "r1", Continues: "m0",
		Tool: "toolu_1", Child: "a1", Batch: "b1", Label: "a title", StartedBy: "main", From: sessiondata.FromAgent,
		Time: "2026-10-01T00:00:01Z", Trigger: "external", Flags: []string{"finished"},
		Usage: &sessiondata.Usage{Input: 10, Output: 5}, Model: "m-1",
		Dropped: []sessiondata.Drop{{What: "signature", Bytes: 12}},
		Parts: []sessiondata.Part{
			{Kind: sessiondata.PartText, Text: "hello", State: model.ContentAvailable, Bytes: 5},
			{Kind: sessiondata.PartReasoning, Text: "thinking", State: model.ContentAvailable, Bytes: 8},
			{Kind: sessiondata.PartCall, ID: "toolu_1", Name: "mcp__s__t", Server: "s", ServerTool: "t",
				Data: json.RawMessage(`{"q":1}`), State: model.ContentAvailable, Bytes: 7},
			{Kind: sessiondata.PartResult, Of: "toolu_1", Failed: &failed, Text: "boom", State: model.ContentAvailable, Bytes: 4},
			{Kind: sessiondata.PartData, Data: changeData, State: model.ContentAvailable, Bytes: len(changeData)},
			{Kind: sessiondata.PartData, Data: json.RawMessage(`{"not":"a changes record"}`), State: model.ContentAvailable, Bytes: 26},
			unknown,
			{Kind: sessiondata.PartData, Media: "image/png", State: model.ContentTooLarge, Bytes: 4 << 20, SHA256: "feed"},
		},
	}
	data := landed(t, header(sessiondata.KindTranscript), rec)

	var out bytes.Buffer
	removed := sessiondata.Removed{At: "2026-10-09T00:00:00Z", Generation: 2, Receiver: sessiondata.ReceiverAccepted, Owner: "o"}
	if err := skeleton.Write(&out, data, removed); err != nil {
		t.Fatal(err)
	}
	hdr, recs := read(t, out.Bytes())
	sum := sha256.Sum256(data)
	want := removed
	want.Digest, want.Size, want.Records = hex.EncodeToString(sum[:]), int64(len(data)), 1
	if hdr.Removed == nil || *hdr.Removed != want {
		t.Fatalf("removed %+v, want %+v", hdr.Removed, want)
	}
	hdr.Removed = nil
	if hdr != *header(sessiondata.KindTranscript) {
		t.Fatalf("the header changed: %+v", hdr)
	}
	if len(recs) != 1 {
		t.Fatalf("%d records, want 1", len(recs))
	}
	got := recs[0]
	if got.Sha != "" {
		t.Fatalf("a transcript record kept its source digest %q", got.Sha)
	}
	kept := *rec
	kept.Sha, kept.Parts = "", nil
	gotNoParts := *got
	gotNoParts.Parts = nil
	if a, b := mustJSON(t, kept), mustJSON(t, gotNoParts); a != b {
		t.Fatalf("the record's own fields changed:\n got %s\nwant %s", b, a)
	}
	if len(got.Parts) != len(rec.Parts) {
		t.Fatalf("%d parts, want %d in place", len(got.Parts), len(rec.Parts))
	}
	for i, p := range got.Parts {
		o := rec.Parts[i]
		state := model.ContentRemoved
		if o.State == model.ContentTooLarge {
			state = o.State
		}
		if p.Kind != o.Kind || p.Bytes != o.Bytes || p.State != state {
			t.Fatalf("part %d: kind %s bytes %d state %s, want %s, %d, %s", i, p.Kind, p.Bytes, p.State, o.Kind, o.Bytes, state)
		}
		if p.Text != "" || p.Media != "" || p.Encoding != "" {
			t.Fatalf("part %d kept content: %+v", i, p)
		}
	}
	if c := got.Parts[2]; c.ID != "toolu_1" || c.Name != "mcp__s__t" || c.Server != "s" || c.ServerTool != "t" || len(c.Data) != 0 {
		t.Fatalf("the call part is %+v", c)
	}
	if r := got.Parts[3]; r.Of != "toolu_1" || r.Failed == nil || !*r.Failed {
		t.Fatalf("the result part is %+v", r)
	}
	cr, ok := changes.Decode(got.Parts[4].Data)
	if !ok || cr.ID != "toolu_1" || cr.CapturedBy != changes.CapturedByClaudeCode || cr.Tool != "toolu_1" || len(cr.Changes) != 2 ||
		cr.Time != change.Time || cr.Root == nil || cr.Root.Path != "/work" || cr.Window != nil || cr.Session != "" {
		t.Fatalf("the changes record is %s", got.Parts[4].Data)
	}
	// Each file keeps what the page groups and counts it by; its diff lines
	// go, so a diff that was available says it is not.
	if c := cr.Changes[0]; c.Path != "a.go" || c.Operation != "modify" || c.Additions == nil || *c.Additions != 3 || *c.Deletions != 1 ||
		c.Diff != changes.DiffUnavailable || len(c.Hunks) != 0 || len(c.Windows) != 0 || c.Before.SHA256 != "aa" || c.After.SHA256 != "bb" {
		t.Fatalf("the first change is %+v", c)
	}
	if c := cr.Changes[1]; c.Additions != nil || c.Deletions != nil || c.Diff != "binary" {
		t.Fatalf("a change with no diff is %+v, want null counts and binary", c)
	}
	if strings.Contains(string(got.Parts[4].Data), "secret") {
		t.Fatalf("the changes record kept a diff line: %s", got.Parts[4].Data)
	}
	if len(got.Parts[5].Data) != 0 || len(got.Parts[6].Data) != 0 {
		t.Fatal("data that is no changes record kept its bytes")
	}
	if p := got.Parts[7]; p.SHA256 != "feed" {
		t.Fatalf("a part too large to land lost its digest: %+v", p)
	}
}

// A provider body keeps its manifest without the literal strings, its record
// digest, and every piece's identity: the digest of its data, or the one a
// piece too large to land kept, with its state.
func TestABodySkeletonKeepsItsManifestAndPieces(t *testing.T) {
	m := providerbody.Manifest{Schema: providerbody.Schema, Role: providerbody.RoleRequest, Src: "x/1.json",
		SHA256: strings.Repeat("a", 64), Bytes: 99, Depth: 1, Chain: "c0ffee", Model: "m-1", Session: "s",
		Call: "c1", Request: "req_2", PreviousRequest: "req_1",
		Segments: []providerbody.Segment{
			{Copy: &providerbody.Copy{From: "req_1", SHA256: strings.Repeat("b", 64), Len: 40}},
			{Lit: `,"metadata":{"user_id":"a person's literal"},`}, {Part: intp(0)}, {Lit: ","}, {Piece: strings.Repeat("c", 64)},
			{Part: intp(1)}, {Lit: "}"}}}
	md, _ := json.Marshal(m)
	piece := json.RawMessage(`"a long piece of a prompt"`)
	rec := &sessiondata.Record{Ord: 1, Sha: "aaaaaaaaaaaa", Bytes: 99, ID: "req_2", Parts: []sessiondata.Part{
		{Kind: sessiondata.PartData, Data: piece, State: model.ContentAvailable, Bytes: len(piece)},
		{Kind: sessiondata.PartData, State: model.ContentTooLarge, Bytes: 5 << 20, SHA256: "beef"},
		{Kind: sessiondata.PartData, Data: md, State: model.ContentAvailable, Bytes: len(md)},
	}}
	data := landed(t, header(sessiondata.KindProviderBody), rec)
	var out bytes.Buffer
	if err := skeleton.Write(&out, data, sessiondata.Removed{Generation: 1, Receiver: sessiondata.ReceiverUnconfirmed}); err != nil {
		t.Fatal(err)
	}
	_, recs := read(t, out.Bytes())
	got := recs[0]
	if got.Sha != rec.Sha {
		t.Fatalf("a body's record digest is %q, want %q", got.Sha, rec.Sha)
	}
	sum := sha256.Sum256(piece)
	if got.Parts[0].SHA256 != hex.EncodeToString(sum[:]) || len(got.Parts[0].Data) != 0 {
		t.Fatalf("the piece is %+v", got.Parts[0])
	}
	if got.Parts[1].SHA256 != "beef" || got.Parts[1].State != model.ContentTooLarge {
		t.Fatalf("the piece too large to land is %+v", got.Parts[1])
	}
	gm, err := providerbody.ManifestOf(got)
	if err != nil {
		t.Fatal(err)
	}
	want := m
	want.Segments = []providerbody.Segment{m.Segments[0], m.Segments[2], m.Segments[4], m.Segments[5]}
	if mustJSON(t, *gm) != mustJSON(t, want) {
		t.Fatalf("the manifest is %+v, want %+v", *gm, want)
	}
	if strings.Contains(string(got.Parts[2].Data), "literal") || strings.Contains(string(got.Parts[2].Data), `"lit"`) {
		t.Fatalf("the manifest kept a literal string: %s", got.Parts[2].Data)
	}
}

// A skeleton is made only of a landed file that reads whole, and never of a
// skeleton.
func TestASkeletonIsMadeOfAWholeLandedFile(t *testing.T) {
	rec := &sessiondata.Record{Ord: 1, Bytes: 2, Parts: []sessiondata.Part{{Kind: sessiondata.PartText, Text: "hi", State: model.ContentAvailable, Bytes: 2}}}
	data := landed(t, header(sessiondata.KindTranscript), rec)
	cut := data[:bytes.LastIndexByte(data[:len(data)-1], '\n')+1]
	if err := skeleton.Write(io.Discard, cut, sessiondata.Removed{}); err == nil {
		t.Fatal("a landed file with no closing line became a skeleton")
	}
	var out bytes.Buffer
	if err := skeleton.Write(&out, data, sessiondata.Removed{Generation: 1}); err != nil {
		t.Fatal(err)
	}
	zr, _ := gzip.NewReader(&out)
	plain, _ := io.ReadAll(zr)
	if err := skeleton.Write(io.Discard, plain, sessiondata.Removed{}); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("a skeleton of a skeleton: %v", err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
