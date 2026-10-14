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

package index_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

func build(t *testing.T) *index.Index {
	t.Helper()
	ix := index.New("sess-1")
	in := ix.Strings
	main := in.ID("main")
	// two fragments of one provider call, then its tool result
	ix.Append(index.Entry{
		Seq: 1, Row: 1, Stream: main, Kind: index.KindAssistant,
		TS: 1000, Record: in.ID("u-a1"), Call: in.ID("msg_1"),
	}, index.Block{Ord: 0, Kind: index.BlockToolUse, ToolID: in.ID("toolu_1"), Name: in.ID("Bash")})
	ix.Append(index.Entry{
		Seq: 1, Row: 2, Stream: main, Kind: index.KindAssistant,
		TS: 1001, Record: in.ID("u-a2"), Parent: in.ID("u-a1"), Call: in.ID("msg_1"),
	})
	ix.Append(index.Entry{
		Seq: 1, Row: 3, Stream: main, Kind: index.KindUser,
		TS: 1002, Record: in.ID("u-r1"), Parent: in.ID("u-a1"),
	}, index.Block{Ord: 0, Kind: index.BlockToolResult, ToolID: in.ID("toolu_1")})
	// a child stream
	ix.Append(index.Entry{
		Seq: 2, Row: 1, Stream: in.ID("a1111111111111111"),
		Kind: index.KindUser, TS: 1010, Record: in.ID("u-c1"), Run: in.ID("a1111111111111111"),
	})
	return ix
}

func TestLookups(t *testing.T) {
	ix := build(t)
	if e, ok := ix.EntryByRecord("u-a1"); !ok || e.Row != 1 {
		t.Errorf("EntryByRecord: %+v ok=%v", e, ok)
	}
	if _, ok := ix.EntryByRecord("nope"); ok {
		t.Error("unknown uuid resolved")
	}
	// A provider call is grouped by message id, and both fragments belong to it.
	if got := ix.ProviderCall("msg_1"); len(got) != 2 {
		t.Errorf("ProviderCall: %d entries, want 2", len(got))
	}
	// The tool id appears on both the use and the result.
	if got := ix.ToolBlocks("toolu_1"); len(got) != 2 {
		t.Errorf("ToolBlocks: %d, want 2", len(got))
	}
	if got := ix.Stream("main"); len(got) != 3 {
		t.Errorf("Stream(main): %d, want 3", len(got))
	}
	if got := ix.Streams(); len(got) != 2 {
		t.Errorf("Streams: %v, want 2", got)
	}
}

// TestFirstOccurrenceWins pins the duplicate rule into the index: record
// ids are not unique within a source file, because a resume replays an earlier
// block. The original must never be displaced by its replayed copy.
func TestFirstOccurrenceWins(t *testing.T) {
	ix := index.New("s")
	in := ix.Strings
	ix.Append(index.Entry{Seq: 1, Row: 5, Record: in.ID("dup")})
	ix.Append(index.Entry{Seq: 9, Row: 900, Record: in.ID("dup")})
	e, ok := ix.EntryByRecord("dup")
	if !ok {
		t.Fatal("not found")
	}
	if e.Seq != 1 || e.Row != 5 {
		t.Errorf("got seq=%d row=%d, want seq=1 row=5 (first occurrence must win)", e.Seq, e.Row)
	}
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	ix := build(t)
	if err := ix.Write(dir); err != nil {
		t.Fatal(err)
	}
	got, ok, err := index.Load(dir, "sess-1")
	if err != nil || !ok {
		t.Fatalf("Load: ok=%v err=%v", ok, err)
	}
	if len(got.Entries) != len(ix.Entries) || len(got.Blocks) != len(ix.Blocks) {
		t.Fatalf("entries %d/%d blocks %d/%d", len(got.Entries), len(ix.Entries),
			len(got.Blocks), len(ix.Blocks))
	}
	for i := range ix.Entries {
		if got.Entries[i] != ix.Entries[i] {
			t.Errorf("entry %d:\n got %+v\nwant %+v", i, got.Entries[i], ix.Entries[i])
		}
	}
	// identifiers must survive interning
	if e, ok := got.EntryByRecord("u-a1"); !ok || e.Row != 1 {
		t.Error("uuid lookup did not survive the round trip")
	}
	if got.Strings.String(got.Entries[0].Call) != "msg_1" {
		t.Error("string table did not survive the round trip")
	}
}

func TestLoadRejectsForeignAndStale(t *testing.T) {
	dir := t.TempDir()
	// A missing index is not an error - the index is derived, so the caller
	// rebuilds rather than failing.
	if _, ok, err := index.Load(dir, "s"); ok || err != nil {
		t.Errorf("missing index: ok=%v err=%v", ok, err)
	}
	if err := writeFile(filepath.Join(dir, "entries.bin"), "not an index"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := index.Load(dir, "s"); ok || err != nil {
		t.Errorf("foreign file: ok=%v err=%v (must rebuild, not fail)", ok, err)
	}
}

func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o644)
}

// TestDuplicateExcludedFromEveryLookup covers the half of duplicate removal that
// byRecord alone does not: a replayed record carries the same message id, run
// id, batch, stream and tool id as the original, so a check that only guards byRecord
// still doubles a provider call's fragment count, doubles a stream's length,
// and turns a one-to-one tool join into an ambiguous one.
func TestDuplicateExcludedFromEveryLookup(t *testing.T) {
	ix := index.New("s")
	in := ix.Strings
	entry := index.Entry{
		Seq: 1, Row: 1, Stream: in.ID("main"), Kind: index.KindAssistant,
		Record: in.ID("u-a1"), Call: in.ID("msg_1"),
		Run: in.ID("loop-1"), Batch: in.ID("wf_1"),
	}
	block := index.Block{Ord: 0, Kind: index.BlockToolUse, ToolID: in.ID("toolu_1")}
	ix.Append(entry, block)
	// The same record replayed at a later landed position, as a resume does.
	replay := entry
	replay.Seq, replay.Row = 4, 12
	ix.Append(replay, block)

	if got := ix.ProviderCall("msg_1"); len(got) != 1 {
		t.Errorf("ProviderCall: %d fragments, want 1 (the replay is not a second fragment)", len(got))
	}
	if got := ix.Stream("main"); len(got) != 1 {
		t.Errorf("Stream: %d entries, want 1", len(got))
	}
	if got := ix.Run("loop-1"); len(got) != 1 {
		t.Errorf("Run: %d entries, want 1", len(got))
	}
	if got := ix.Batch("wf_1"); len(got) != 1 {
		t.Errorf("Batch: %d entries, want 1", len(got))
	}
	if got := ix.ToolBlocks("toolu_1"); len(got) != 1 {
		t.Errorf("ToolBlocks: %d, want 1 (a replay must not make an exact join ambiguous)", len(got))
	}
	// The duplicate stays in Entries: it is evidence of what the runtime
	// re-emitted, and only the canonical view excludes it.
	if len(ix.Entries) != 2 {
		t.Errorf("Entries: %d, want 2 (the replay is dropped from lookups, not from evidence)", len(ix.Entries))
	}
}

// Entries with no record id have no identity to compare on, so each is
// distinct. Treating them as duplicates of each other would erase most of a
// session: many record types carry no id at all.
func TestEntriesWithoutIdentityAreKeptSeparate(t *testing.T) {
	ix := index.New("s")
	in := ix.Strings
	for i := 0; i < 3; i++ {
		ix.Append(index.Entry{Seq: 1, Row: uint32(i + 1), Stream: in.ID("main"), Kind: index.KindMeta})
	}
	if got := ix.Stream("main"); len(got) != 3 {
		t.Errorf("Stream: %d entries, want 3", len(got))
	}
}

// A provider body's join keys are read from its manifest while landing, and they
// survive being written and read back. A record whose manifest is not a provider
// body's is indexed like any other: it is evidence, and it joins to nothing.
func TestProviderBodiesAreIndexedAndSurviveTheRoundTrip(t *testing.T) {
	manifest := func(role, request, previous string) json.RawMessage {
		m := map[string]any{
			"schema": "provider_body/1", "role": role, "sha256": "d", "bytes": 2, "depth": 0,
			"segments": []any{map[string]any{"lit": "{}"}},
		}
		if request != "" {
			m["request"] = request
		}
		if previous != "" {
			m["previous_request"] = previous
		}
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	hdr := &sessiondata.Header{Kind: sessiondata.KindProviderBody, Session: "sess-1", Stream: "main"}
	ix := index.New("sess-1")
	for row, rec := range []*sessiondata.Record{
		{Ord: 1, ID: "one.request", Run: "cycle-1", Parts: []sessiondata.Part{
			{Kind: sessiondata.PartData, Data: manifest("request", "", "req-0")},
		}},
		{Ord: 2, ID: "one.response", Call: "msg_1", Parts: []sessiondata.Part{
			{Kind: sessiondata.PartData, Data: manifest("response", "req-1", "")},
		}},
		{Ord: 3, ID: "not-a-body", Parts: []sessiondata.Part{
			{Kind: sessiondata.PartData, Data: json.RawMessage(`{"schema":"other/1"}`)},
		}},
	} {
		e, blocks, body := index.FromRecord(ix, hdr, rec, 4, uint32(row+1))
		ix.AppendRecord(e, blocks, body)
	}
	if len(ix.Bodies) != 2 {
		t.Fatalf("indexed %d bodies, want 2", len(ix.Bodies))
	}

	dir := t.TempDir()
	if err := ix.Write(dir); err != nil {
		t.Fatal(err)
	}
	got, ok, err := index.Load(dir, "sess-1")
	if err != nil || !ok {
		t.Fatalf("Load: ok=%v err=%v", ok, err)
	}
	if len(got.Bodies) != 2 {
		t.Fatalf("read back %d bodies, want 2", len(got.Bodies))
	}
	request, response := got.Bodies[0], got.Bodies[1]
	if request.Role != index.BodyRoleRequest || got.Strings.String(request.Previous) != "req-0" {
		t.Errorf("request: %+v", request)
	}
	if response.Role != index.BodyRoleResponse || got.Strings.String(response.Request) != "req-1" {
		t.Errorf("response: %+v", response)
	}
	// the entry each body belongs to, and the source line the gap check reads
	if e := got.Entries[request.Entry]; e.Ord != 1 || got.Strings.String(e.Run) != "cycle-1" {
		t.Errorf("request entry: %+v", e)
	}
	if e := got.Entries[response.Entry]; e.Ord != 2 || got.Strings.String(e.Call) != "msg_1" {
		t.Errorf("response entry: %+v", e)
	}
}

// Through keeps the entries at or below a sequence and builds its lookups over
// them alone, leaving the index it came from as it was.
func TestThroughBoundsEveryLookup(t *testing.T) {
	ix := build(t)
	cut := ix.Through(1)
	if _, ok := cut.EntryByRecord("u-c1"); ok {
		t.Error("a record landed at sequence 2 was found through sequence 1")
	}
	if got := cut.Streams(); len(got) != 1 {
		t.Errorf("Streams through 1: %v, want main only", got)
	}
	if got := cut.ProviderCall("msg_1"); len(got) != 2 {
		t.Errorf("ProviderCall through 1: %d, want 2", len(got))
	}
	if got := cut.ToolBlocks("toolu_1"); len(got) != 2 {
		t.Errorf("ToolBlocks through 1: %d, want 2", len(got))
	}
	if _, ok := ix.EntryByRecord("u-c1"); !ok {
		t.Error("Through changed the index it was called on")
	}
	if ix.Through(0) != ix || ix.Through(2) != ix || ix.Through(9) != ix {
		t.Error("a bound at or past the last entry must return the index itself")
	}
}

// An index whose sequences go back was extended twice over the same files, by
// a pass that wrote the index and stopped before saving its state. It is not
// read: the caller builds it again from the landed files.
func TestAnIndexOutOfLandedOrderIsNotRead(t *testing.T) {
	ix := index.New("s")
	in := ix.Strings
	for _, seq := range []uint32{1, 2, 1, 2} {
		ix.Append(index.Entry{Seq: seq, Row: 1, Record: in.ID("r")})
	}
	dir := t.TempDir()
	if err := ix.Write(dir); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := index.Load(dir, "s"); err != nil || ok {
		t.Fatalf("an index out of landed order was read: ok=%v %v", ok, err)
	}
}

// The first lookup on an index builds its maps, whichever lookup it is.
func TestAFirstLookupBuildsTheMaps(t *testing.T) {
	fresh := func() *index.Index {
		ix := index.New("s")
		in := ix.Strings
		ix.Append(index.Entry{Seq: 1, Row: 1, Record: in.ID("a"), Run: in.ID("r"), Batch: in.ID("b"),
			Tool: in.ID("t"), Child: in.ID("c")})
		ix.Append(index.Entry{Seq: 2, Row: 1, Record: in.ID("z")})
		return ix.Through(1)
	}
	for name, lookup := range map[string]func(*index.Index) []*index.Entry{
		"Run":        func(ix *index.Index) []*index.Entry { return ix.Run("r") },
		"Batch":      func(ix *index.Index) []*index.Entry { return ix.Batch("b") },
		"AboutTool":  func(ix *index.Index) []*index.Entry { return ix.AboutTool("t") },
		"AboutChild": func(ix *index.Index) []*index.Entry { return ix.AboutChild("c") },
	} {
		if got := lookup(fresh()); len(got) != 1 {
			t.Errorf("%s as the first lookup found %d entries, want 1", name, len(got))
		}
	}
}

// Through keeps the blocks and bodies of the entries it keeps, and only those.
// A record appended to the prefix does not write over the index it came from.
func TestThroughKeepsTheBlocksAndBodiesOfItsEntries(t *testing.T) {
	ix := index.New("s")
	in := ix.Strings
	ix.AppendRecord(index.Entry{Seq: 1, Row: 1, Record: in.ID("a"), Kind: index.KindProviderBody},
		[]index.Block{{Kind: index.BlockOther}}, &index.Body{Role: index.BodyRoleRequest})
	ix.AppendRecord(index.Entry{Seq: 2, Row: 1, Record: in.ID("b"), Kind: index.KindProviderBody},
		[]index.Block{{Kind: index.BlockOther}, {Kind: index.BlockOther}}, &index.Body{Role: index.BodyRoleResponse})
	cut := ix.Through(1)
	if len(cut.Entries) != 1 || len(cut.Blocks) != 1 || len(cut.Bodies) != 1 || cut.Bodies[0].Entry != 0 {
		t.Fatalf("through 1: %d entries, %d blocks, %d bodies", len(cut.Entries), len(cut.Blocks), len(cut.Bodies))
	}
	cut.AppendRecord(index.Entry{Seq: 2, Row: 1, Record: in.ID("c")}, []index.Block{{Kind: index.BlockText}},
		&index.Body{Role: index.BodyRoleRequest})
	if ix.Entries[1].Record != in.ID("b") || ix.Blocks[1].Kind != index.BlockOther || ix.Bodies[1].Role != index.BodyRoleResponse {
		t.Fatal("appending to the prefix wrote over the index it came from")
	}
}

// A landed record bounds neither a label's length nor how many parts it has,
// so the index does not either: a title of 64 KiB or more is written and read
// back, and the part after the 65,536th keeps its own position rather than
// wrapping to the first one's.
func TestALongLabelAndManyPartsSurviveTheRoundTrip(t *testing.T) {
	label := strings.Repeat("a title ", 70000/8)
	parts := make([]sessiondata.Part, 65538)
	for i := range parts {
		parts[i] = sessiondata.Part{Kind: sessiondata.PartText, Text: "x"}
	}
	hdr := &sessiondata.Header{Kind: sessiondata.KindTranscript, Session: "sess-1", Stream: "main"}
	ix := index.New("sess-1")
	e, blocks, body := index.FromRecord(ix, hdr, &sessiondata.Record{Ord: 1, ID: "m1", Label: label, Parts: parts}, 1, 1)
	ix.AppendRecord(e, blocks, body)

	dir := t.TempDir()
	if err := ix.Write(dir); err != nil {
		t.Fatal(err)
	}
	got, ok, err := index.Load(dir, "sess-1")
	if err != nil || !ok {
		t.Fatalf("Load: ok=%v err=%v", ok, err)
	}
	if l := got.Strings.String(got.Entries[0].Label); l != label {
		t.Fatalf("the label read back is %d bytes, want %d", len(l), len(label))
	}
	if len(got.Blocks) != len(parts) {
		t.Fatalf("%d blocks read back, want %d", len(got.Blocks), len(parts))
	}
	for i, b := range got.Blocks {
		if int(b.Ord) != i {
			t.Fatalf("block %d reads back at position %d", i, b.Ord)
		}
	}
}
