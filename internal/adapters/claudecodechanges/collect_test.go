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

package claudecodechanges_test

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecodechanges"
	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/repack"
	"github.com/apache/skywalking-ai-sessionizer/internal/retention"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/verify"
	"github.com/apache/skywalking-ai-sessionizer/pkg/changes"
	"github.com/apache/skywalking-ai-sessionizer/pkg/execution"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

const (
	session = "11111111-2222-4333-8444-555555555555"
	agent   = "a0123456789abcdef"
)

func line(id, stream, tool, root string) string {
	r := &changes.Record{
		Schema: changes.Schema, ID: id, CapturedBy: changes.CapturedByASZPlugin, Session: session, Stream: stream, Tool: tool, ToolName: "Bash",
		Time: "2026-09-08T02:00:04Z", Basis: changes.BasisToolWindow, Root: &changes.Root{Path: root},
		ChangedFiles: changes.Int(1),
		Changes: []changes.FileChange{{Path: "a.go", Operation: changes.OpModify,
			Before: changes.Endpoint{Present: true, Bytes: changes.Int64(2), SHA256: "x"},
			After:  changes.Endpoint{Present: true, Bytes: changes.Int64(2), SHA256: "y"},
			Diff:   changes.DiffAvailable, Additions: changes.Int(1), Deletions: changes.Int(1),
			Hunks: []changes.Hunk{{OldStart: 1, OldLines: 1, NewStart: 1, NewLines: 1, Lines: []string{"-a", "+b"}}}}},
	}
	b, _ := r.Marshal()
	return string(b) + "\n"
}

func appendTo(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

func readAll(t *testing.T, path string) (sessiondata.Header, []*sessiondata.Record) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r, err := sessiondata.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var recs []*sessiondata.Record
	for {
		rec, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		recs = append(recs, rec)
	}
	return r.Header(), recs
}

func TestCollectLandsEachStreamUnderTheSession(t *testing.T) {
	src := t.TempDir()
	root := t.TempDir()
	out := filepath.Join(src, "asz-changes-inline", "output", session)
	appendTo(t, filepath.Join(out, "main.jsonl"), line("p1/c1", "main", "toolu_1", "/workspace/project"))
	appendTo(t, filepath.Join(out, agent+".jsonl"), line("p1/c2", agent, "toolu_2", "/workspace/project"))
	// Noise the plugin did not write, in every shape discovery must ignore.
	appendTo(t, filepath.Join(src, "other-plugin", "output", session, "main.jsonl"), line("q/1", "main", "t", "/x"))
	appendTo(t, filepath.Join(out, "notes.txt"), "not a stream\n")
	appendTo(t, filepath.Join(src, "asz-changes-inline", "output", "not-a-session", "main.jsonl"), line("q/2", "main", "t", "/x"))

	zone := storage.NewZone(root)
	c := claudecodechanges.New(src, zone, 0)
	c.Now = func() time.Time { return time.Date(2026, 9, 8, 3, 0, 0, 0, time.UTC) }
	st, err := c.CollectAll(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Errors) != 0 || st.Sessions != 1 || st.SourcesLanded != 2 || st.Records != 2 {
		t.Fatalf("stats: %+v", st)
	}
	if len(st.Changed) != 1 || st.Changed[0] != session {
		t.Fatalf("changed: %v", st.Changed)
	}

	files, err := storage.LandedFiles(zone, session)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("landed %d files, want 2", len(files))
	}
	byStream := map[string]storage.LandedFile{}
	for _, f := range files {
		byStream[f.Stream] = f
		if !strings.HasPrefix(filepath.Base(f.Path), "changes-") {
			t.Fatalf("file name %s does not carry the kind", f.Path)
		}
	}
	hdr, recs := readAll(t, byStream["main"].Path)
	if hdr.Kind != sessiondata.KindChanges || hdr.Dialect != claudecodechanges.Dialect || hdr.Stream != "main" ||
		hdr.Src != "asz-changes-inline/output/"+session+"/main.jsonl" || hdr.Session != session {
		t.Fatalf("header: %+v", hdr)
	}
	if len(recs) != 1 || recs[0].ID != "p1/c1" || recs[0].Tool != "toolu_1" || recs[0].Time != "2026-09-08T02:00:04Z" || recs[0].From != "" {
		t.Fatalf("record: %+v", recs[0])
	}
	if len(recs[0].Parts) != 1 || recs[0].Parts[0].Kind != sessiondata.PartData {
		t.Fatalf("parts: %+v", recs[0].Parts)
	}
	if r, ok := changes.Decode(recs[0].Parts[0].Data); !ok || r.Changes[0].Hunks[0].Lines[1] != "+b" {
		t.Fatalf("the part is not the record byte for byte: %s", recs[0].Parts[0].Data)
	}
	if recs[0].Ord != 1 || recs[0].Off != 0 || recs[0].Sha == "" {
		t.Fatalf("provenance: ord=%d off=%d sha=%q", recs[0].Ord, recs[0].Off, recs[0].Sha)
	}
	if _, recs := readAll(t, byStream[agent].Path); len(recs) != 1 || recs[0].ID != "p1/c2" {
		t.Fatalf("agent stream: %+v", recs)
	}

	// The index knows the records by their kind, and nothing else.
	ix, ok, err := index.Load(zone.IndexDir(session), session)
	if err != nil || !ok {
		t.Fatalf("index: ok=%v err=%v", ok, err)
	}
	if len(ix.Entries) != 2 {
		t.Fatalf("index entries: %d", len(ix.Entries))
	}
	for _, e := range ix.Entries {
		if e.Kind != index.KindChanges {
			t.Fatalf("entry kind %d, want changes", e.Kind)
		}
	}

	// A second pass lands nothing. An appended line lands as a new file.
	st, _ = c.CollectAll(nil)
	if st.Records != 0 || st.SourcesLanded != 0 || len(st.Changed) != 0 {
		t.Fatalf("second pass: %+v", st)
	}
	appendTo(t, filepath.Join(out, "main.jsonl"), line("p1/c3", "main", "toolu_3", "/workspace/project"))
	st, _ = c.CollectAll(nil)
	if st.Records != 1 || st.SourcesLanded != 1 {
		t.Fatalf("third pass: %+v", st)
	}
	files, _ = storage.LandedFiles(zone, session)
	if len(files) != 3 || files[2].Seq != 3 {
		t.Fatalf("after append: %d files, last seq %d", len(files), files[len(files)-1].Seq)
	}
}

// The agent's stream lands in the same pass as the main stream's fails, so the
// session's index and state are saved. The failed file's record must not be in
// that index, and the number it gave back must go to the next file that lands,
// or every round after it reports that number as a missing file.
func TestAFileThatFailsToLandLeavesNoIndexEntryOrGap(t *testing.T) {
	src := t.TempDir()
	out := filepath.Join(src, "asz-changes-inline", "output", session)
	appendTo(t, filepath.Join(out, "main.jsonl"), line("p1/c1", "main", "toolu_1", "/workspace/project"))
	appendTo(t, filepath.Join(out, agent+".jsonl"), line("p1/c2", agent, "toolu_2", "/workspace/project"))

	zone := storage.NewZone(t.TempDir())
	c := claudecodechanges.New(src, zone, 0)
	at := time.Date(2026, 9, 8, 3, 0, 0, 0, time.UTC)
	c.Now = func() time.Time { return at }
	// A directory with something in it where the main stream's file would go
	// makes its rename fail.
	stream := zone.StreamDir(session, storage.StreamMain)
	var blockers []string
	for seq := uint64(1); seq <= 3; seq++ {
		b := filepath.Join(stream, storage.LandedName("changes", storage.Stamp(at), seq))
		appendTo(t, filepath.Join(b, "keep"), "")
		blockers = append(blockers, b)
	}
	st, err := c.CollectAll(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Errors) == 0 || st.SourcesLanded != 1 {
		t.Fatalf("pass: %+v, want the agent's stream landed and the main stream's failed", st)
	}
	ix, ok, err := index.Load(zone.IndexDir(session), session)
	if err != nil || !ok {
		t.Fatalf("index: ok=%v err=%v", ok, err)
	}
	if _, found := ix.EntryByRecord("p1/c1"); found {
		t.Error("the main stream's record is in the index, and its file did not land")
	}

	for _, b := range blockers {
		if err := os.RemoveAll(b); err != nil {
			t.Fatal(err)
		}
	}
	c.Now = time.Now
	if _, err := c.CollectAll(nil); err != nil {
		t.Fatal(err)
	}
	files, err := storage.LandedFiles(zone, session)
	if err != nil {
		t.Fatal(err)
	}
	seqs := make([]uint64, 0, len(files))
	for _, f := range files {
		seqs = append(seqs, f.Seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	for i, seq := range seqs {
		if seq != uint64(i+1) {
			t.Fatalf("landed numbers %v: %d where %d belongs", seqs, seq, i+1)
		}
	}
	ix, ok, err = index.Load(zone.IndexDir(session), session)
	if err != nil || !ok {
		t.Fatalf("index: ok=%v err=%v", ok, err)
	}
	for _, id := range []string{"p1/c1", "p1/c2"} {
		if _, found := ix.EntryByRecord(id); !found {
			t.Errorf("record %q missing from the index after the retry", id)
		}
	}
}

func TestCollectKeepsALineItCannotRead(t *testing.T) {
	src := t.TempDir()
	out := filepath.Join(src, "asz-changes-inline", "output", session)
	appendTo(t, filepath.Join(out, "main.jsonl"), "{\"schema\":\"something/9\"}\n"+line("p1/c1", "main", "toolu_1", "/w"))
	zone := storage.NewZone(t.TempDir())
	st, err := claudecodechanges.New(src, zone, 0).CollectAll(nil)
	if err != nil || len(st.Errors) != 0 || st.Records != 2 {
		t.Fatalf("stats: %+v err=%v", st, err)
	}
	files, _ := storage.LandedFiles(zone, session)
	_, recs := readAll(t, files[0].Path)
	if recs[0].Parts[0].Kind != sessiondata.PartUnknown || recs[0].ID != "" || recs[1].ID != "p1/c1" {
		t.Fatalf("records: %+v %+v", recs[0], recs[1])
	}
}

func TestMatchJudgesTheWorkspace(t *testing.T) {
	src := t.TempDir()
	out := filepath.Join(src, "asz-changes-inline", "output", session)
	appendTo(t, filepath.Join(out, "main.jsonl"), line("p1/c1", "main", "toolu_1", "/private/tmp/scratch/x"))
	sessions, err := claudecodechanges.Discover(src)
	if err != nil || len(sessions) != 1 || sessions[0].Root != "/private/tmp/scratch/x" {
		t.Fatalf("discover: %+v err=%v", sessions, err)
	}
	m := claudecode.NewMatcher(nil, []string{"/private/tmp/**"})
	if claudecodechanges.Match(m, sessions[0]) {
		t.Fatal("a session under /private/tmp matched the exclusion and was still collected")
	}
	if !claudecodechanges.Match(claudecode.NewMatcher(nil, []string{"/elsewhere/**"}), sessions[0]) {
		t.Fatal("a session outside the exclusion was not collected")
	}
}

// Two recorder directories read by one configuration - a shim's flat
// directory and another - can each hold the same session under the same
// relative path. They are different files: one cursor for both read the
// second against the first's position, a truncation conflict that stopped
// both for good. Each is landed behind a cursor that names its root.
func TestTheSameSessionInTwoRecorderRootsIsTwoSources(t *testing.T) {
	zone := storage.NewZone(t.TempDir())
	rootA, rootB := t.TempDir(), t.TempDir()
	appendTo(t, filepath.Join(rootA, "output", session, "main.jsonl"), line("a/c1", "main", "toolu_1", "/w"))
	appendTo(t, filepath.Join(rootB, "output", session, "main.jsonl"), line("b/c1", "main", "toolu_1", "/w")+line("b/c2", "main", "toolu_2", "/w"))
	all := func(claudecodechanges.Session) bool { return true }
	a := claudecodechanges.New(rootA, zone, 0)
	b := claudecodechanges.New(rootB, zone, 0)
	for name, c := range map[string]*claudecodechanges.Collector{"a": a, "b": b} {
		st, err := c.CollectAll(all)
		if err != nil || len(st.Errors) > 0 || st.Conflicts != 0 {
			t.Fatalf("%s: %+v err=%v", name, st, err)
		}
	}
	appendTo(t, filepath.Join(rootA, "output", session, "main.jsonl"), line("a/c2", "main", "toolu_2", "/w"))
	st, err := a.CollectAll(all)
	if err != nil || len(st.Errors) > 0 || st.Conflicts != 0 || st.Records != 1 {
		t.Fatalf("a line the first root gained: %+v err=%v; want one record and no conflict", st, err)
	}
	if st, err := b.CollectAll(all); err != nil || st.Conflicts != 0 || st.Records != 0 {
		t.Fatalf("the second root, unchanged: %+v err=%v; want nothing and no conflict", st, err)
	}
	files, err := storage.LandedFiles(zone, session)
	if err != nil || len(files) != 3 {
		t.Fatalf("%d landed files, want three: two roots, then the first root's second line (%v)", len(files), err)
	}
}

// A cursor from before 0.5.0 names no root. It belongs to the root whose
// file it describes - its tail digest says which - and not to the first
// root to read the same relative path, which could be the other one. And
// it is written with its root on the next pass, even one that lands
// nothing, so the question is asked once.
func TestALegacyCursorBelongsToTheRootWhoseFileItDescribes(t *testing.T) {
	zone := storage.NewZone(t.TempDir())
	rootA, rootB := t.TempDir(), t.TempDir()
	appendTo(t, filepath.Join(rootA, "output", session, "main.jsonl"), line("a/c1", "main", "toolu_1", "/w"))
	appendTo(t, filepath.Join(rootB, "output", session, "main.jsonl"), line("b/c1", "main", "toolu_1", "/w")+line("b/c2", "main", "toolu_2", "/w"))
	all := func(claudecodechanges.Session) bool { return true }
	a := claudecodechanges.New(rootA, zone, 0)
	if st, err := a.CollectAll(all); err != nil || st.Records != 1 {
		t.Fatalf("a: %+v err=%v", st, err)
	}
	// The cursor as 0.4.0 wrote it: no origin, and no head.
	cursor := filepath.Join(zone.StreamDir(session, "main"), "changes.cursor")
	dropKeys(t, cursor, "origin", "head_sha256")
	// The other root reads the same relative path first. The cursor does
	// not describe its file, so it takes one of its own and lands whole.
	b := claudecodechanges.New(rootB, zone, 0)
	if st, err := b.CollectAll(all); err != nil || st.Conflicts != 0 || st.Records != 2 {
		t.Fatalf("b against a legacy cursor of a: %+v err=%v; want two records and no conflict", st, err)
	}
	if raw, _ := os.ReadFile(cursor); strings.Contains(string(raw), "origin") {
		t.Fatalf("b wrote its root into a's cursor:\n%s", raw)
	}
	// A's pass lands nothing and still writes its root into its cursor.
	if st, err := a.CollectAll(all); err != nil || st.Conflicts != 0 || st.Records != 0 {
		t.Fatalf("a, unchanged: %+v err=%v", st, err)
	}
	if raw, _ := os.ReadFile(cursor); !strings.Contains(string(raw), "origin") {
		t.Fatalf("a's unchanged pass did not claim its cursor:\n%s", raw)
	}
	appendTo(t, filepath.Join(rootA, "output", session, "main.jsonl"), line("a/c2", "main", "toolu_2", "/w"))
	if st, err := a.CollectAll(all); err != nil || st.Conflicts != 0 || st.Records != 1 {
		t.Fatalf("a's next line: %+v err=%v", st, err)
	}
}

// Two files can share their last megabyte and differ before it. A legacy
// cursor claimed on the bytes before its offset alone would let the other
// root skip everything it holds before that window, silently. The file's
// identity decides with the bytes, so the other root lands all of its own.
//
// The two first lines are the same length, so the shared tail sits at the
// same offset in both files and the digests before that offset are equal;
// the test holds that before it collects, or it would prove nothing.
func TestALegacyCursorIsNotClaimedOnASharedTailAlone(t *testing.T) {
	zone := storage.NewZone(t.TempDir())
	rootA, rootB := t.TempDir(), t.TempDir()
	var shared strings.Builder
	for i := 0; shared.Len() < (1<<20)+(64<<10); i++ {
		shared.WriteString(line(fmt.Sprintf("s/c%d", i), "main", fmt.Sprintf("toolu_s%d", i), "/w"))
	}
	sharedLines := strings.Count(shared.String(), "\n")
	fileA := filepath.Join(rootA, "output", session, "main.jsonl")
	fileB := filepath.Join(rootB, "output", session, "main.jsonl")
	first := func(id, tool string) string { return line(id, "main", tool, "/w") }
	if len(first("a/c1", "toolu_a")) != len(first("b/c1", "toolu_b")) {
		t.Fatal("the two first lines differ in length, so the tails would not sit at one offset")
	}
	appendTo(t, fileA, first("a/c1", "toolu_a")+shared.String())
	appendTo(t, fileB, first("b/c1", "toolu_b")+shared.String())
	all := func(claudecodechanges.Session) bool { return true }
	a := claudecodechanges.New(rootA, zone, 0)
	for {
		st, err := a.CollectAll(all)
		if err != nil || len(st.Errors) > 0 || st.Conflicts != 0 {
			t.Fatalf("a: %+v err=%v", st, err)
		}
		if st.Pending == 0 {
			break
		}
	}
	// The cursor as 0.4.0 wrote it: no origin, and no head. With a head,
	// the other root's first line alone would send it to another cursor,
	// and the test would pass whatever the claim decided.
	cursor := filepath.Join(zone.StreamDir(session, "main"), "changes.cursor")
	dropKeys(t, cursor, "origin", "head_sha256")
	held, err := storage.LoadCursor(cursor, storage.CursorAppend, "")
	if err != nil {
		t.Fatal(err)
	}
	offset := int64(held.Offset)
	// The precondition: at a's offset, the bytes before it digest the same
	// in both files, so the bytes alone cannot tell the two apart.
	da, errA := claudecode.TailDigestAt(fileA, offset)
	db, errB := claudecode.TailDigestAt(fileB, offset)
	if errA != nil || errB != nil || offset == 0 || da != db {
		t.Fatalf("the tails at offset %d do not digest the same (%v %v %s %s), so this test would prove nothing", offset, errA, errB, da, db)
	}
	b := claudecodechanges.New(rootB, zone, 0)
	records := 0
	for {
		st, err := b.CollectAll(all)
		if err != nil || len(st.Errors) > 0 || st.Conflicts != 0 {
			t.Fatalf("b: %+v err=%v", st, err)
		}
		records += st.Records
		if st.Pending == 0 {
			break
		}
	}
	if want := 1 + sharedLines; records != want {
		t.Fatalf("b landed %d records, want all %d of its own: a cursor read from a's file was claimed on the shared tail", records, want)
	}
}

// A change record larger than the budget lands as a change record: its
// largest files lose their hunks and say their diff is too_large, and its
// identity, files and counts stay, so it still joins its step and adds up.
func TestALargeChangeRecordKeepsItsIdentity(t *testing.T) {
	src, root := t.TempDir(), t.TempDir()
	r := &changes.Record{
		Schema: changes.Schema, ID: "p1/big", CapturedBy: changes.CapturedByASZPlugin, Session: session, Stream: "main",
		Tool: "toolu_9", ToolName: "Bash", Time: "2026-09-08T02:00:04Z", Basis: changes.BasisToolWindow,
		Root: &changes.Root{Path: "/workspace/project"}, ChangedFiles: changes.Int(2),
	}
	for _, path := range []string{"big.json", "small.go"} {
		n := 2_000
		if path == "small.go" {
			n = 2
		}
		lines := make([]string, n)
		for i := range lines {
			lines[i] = "+" + strings.Repeat("y", 40)
		}
		r.Changes = append(r.Changes, changes.FileChange{Path: path, Operation: changes.OpModify,
			Diff: changes.DiffAvailable, Additions: changes.Int(n), Deletions: changes.Int(0),
			Hunks: []changes.Hunk{{NewStart: 1, NewLines: n, Lines: lines}}})
	}
	b, _ := r.Marshal()
	out := filepath.Join(src, "asz-changes-inline", "output", session)
	appendTo(t, filepath.Join(out, "main.jsonl"), string(b)+"\n")

	const budget = 16 << 10
	zone := storage.NewZone(root)
	if _, err := claudecodechanges.New(src, zone, budget).CollectAll(nil); err != nil {
		t.Fatal(err)
	}
	files, err := storage.LandedFiles(zone, session)
	if err != nil || len(files) != 1 {
		t.Fatalf("landed %d files, %v", len(files), err)
	}
	if fi, _ := os.Stat(files[0].Path); fi.Size() > budget {
		t.Fatalf("the file is %d bytes, over the budget of %d", fi.Size(), budget)
	}
	_, recs := readAll(t, files[0].Path)
	p := recs[0].Parts[0]
	got, ok := changes.Decode(p.Data)
	if !ok || p.State != "truncated" || p.Bytes != len(b) {
		t.Fatalf("the part is %s and decodes %v, want a truncated change record of the original size", p.State, ok)
	}
	if got.ID != "p1/big" || got.Tool != "toolu_9" || len(got.Changes) != 2 ||
		got.Changes[0].Diff != changes.DiffTooLarge || got.Changes[0].Hunks != nil || *got.Changes[0].Additions != 2_000 ||
		got.Changes[1].Diff != changes.DiffAvailable || len(got.Changes[1].Hunks) != 1 {
		t.Fatalf("the change record kept %+v", got.Changes)
	}
	// The index sees a change record, with its lines.
	ix, ok, err := index.Load(zone.IndexDir(session), session)
	if err != nil || !ok {
		t.Fatalf("no index: %v", err)
	}
	adds := uint32(0)
	for _, blk := range ix.Blocks {
		if blk.Kind == index.BlockChanges {
			adds += blk.Adds
		}
	}
	if adds != 2_002 {
		t.Fatalf("the index counts %d added lines, want 2002", adds)
	}
}

// executionLine is one execution record the plugin writes after a call to an
// MCP server.
func executionLine(id string) string {
	ms := int64(12)
	r := &execution.Record{Schema: execution.Schema, ID: id + "/" + execution.BoundaryClientHook,
		ObservedBy: execution.ObservedByASZPlugin, Boundary: execution.BoundaryClientHook,
		Session: session, Stream: "main", Tool: "toolu_" + id, ToolName: "mcp__docs__read", Protocol: execution.ProtocolMCP,
		Server: &execution.Server{Name: "docs", Source: "user"}, Time: "2026-09-08T02:00:04Z",
		DurationMS: &ms, Outcome: execution.OutcomeReturned}
	b, _ := r.Marshal()
	return string(b) + "\n"
}

// The plugin removes an output file once it was not written to for
// retention.ttl, and writes a new one at the same path when the session goes
// on. The old cursor's position means nothing in the new file: a shorter file
// read against it was a truncation conflict, a longer one a digest conflict,
// and either stopped the path for good. A file that does not begin with the
// line its cursor's file began with is another file. It is read from its
// start behind a cursor of its own, and nothing of the file before it is
// landed again.
func TestAFileThePluginWritesAgainIsCollectedFromItsStart(t *testing.T) {
	for _, tc := range []struct {
		kind sessiondata.Kind
		file string
	}{
		{sessiondata.KindChanges, "main.jsonl"},
		{sessiondata.KindExecution, "main.execution.jsonl"},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			src := t.TempDir()
			rel := "file-changes-inline/output/" + session + "/" + tc.file
			path := filepath.Join(src, filepath.FromSlash(rel))
			zone := storage.NewZone(t.TempDir())
			c := claudecodechanges.New(src, zone, 0)
			rec := func(ids ...string) string {
				var out string
				for _, id := range ids {
					if tc.kind == sessiondata.KindExecution {
						out += executionLine(id)
					} else {
						out += line(id, "main", "toolu_"+id, "/w")
					}
				}
				return out
			}
			pass := func(what string, want int) {
				t.Helper()
				st, err := c.CollectAll(nil)
				if err != nil || !st.Complete() || st.Records != want {
					t.Fatalf("%s: %+v err=%v; want %d records and a complete pass", what, st, err, want)
				}
				if st, err := c.CollectAll(nil); err != nil || !st.Complete() || st.Records != 0 {
					t.Fatalf("%s, again: %+v err=%v; want nothing", what, st, err)
				}
			}
			// As the plugin's retention does: the file, then its session's
			// directory, which holds nothing else.
			again := func(ids ...string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Dir(path)); err != nil {
					t.Fatal(err)
				}
				appendTo(t, path, rec(ids...))
			}
			appendTo(t, path, rec("a1", "a2", "a3"))
			pass("the first file", 3)
			// The same length over the same inode, as a file system that
			// gives a new file the number the removed one had: only the
			// first line tells the two apart.
			if err := os.WriteFile(path, []byte(rec("d1", "d2", "d3")), 0o644); err != nil {
				t.Fatal(err)
			}
			pass("a file of the same length over the same inode", 3)
			again("b1")
			pass("a shorter file at the same path", 1)
			appendTo(t, path, rec("b2"))
			pass("the third file's next line", 1)
			again("c1", "c2", "c3", "c4")
			pass("a longer file at the same path", 4)

			s := claudecodechanges.Session{ID: session, Sources: []claudecodechanges.Source{{
				Path: path, Rel: rel, Session: session, Stream: "main", Kind: tc.kind}}}
			if ok, reason, err := c.Covered(s); err != nil || !ok {
				t.Fatalf("the fourth file is landed to its end, but Covered says %v %q %v", ok, reason, err)
			}
			var ids []string
			srcs := map[string]bool{}
			for _, f := range landedFiles(t, zone) {
				hdr, recs := readAll(t, f.Path)
				srcs[hdr.Src] = true
				if hdr.Src != rel && !strings.HasPrefix(hdr.Src, rel+"#") {
					t.Fatalf("a file lands with src %q; want %q, or it with a mark", hdr.Src, rel)
				}
				for _, r := range recs {
					ids = append(ids, strings.TrimSuffix(r.ID, "/"+execution.BoundaryClientHook))
				}
			}
			if got := strings.Join(ids, " "); got != "a1 a2 a3 d1 d2 d3 b1 b2 c1 c2 c3 c4" {
				t.Fatalf("landed %s; want every record once, in the order written", got)
			}
			if len(srcs) != 4 || !srcs[rel] {
				t.Fatalf("the files land with the sources %v; want four, the first file's under its path", srcs)
			}
			cursorsName(t, zone.StreamDir(session, "main"), srcs)
			parseTwice(t, zone)
			verified(t, zone, "as landed", 4)
			ix := rebuiltIndex(t, zone)
			collected, ok, err := index.Load(zone.IndexDir(session), session)
			if err != nil || !ok || !reflect.DeepEqual(collected.Entries, ix.Entries) || !reflect.DeepEqual(collected.Blocks, ix.Blocks) {
				t.Fatalf("the index rebuilt from the landed files differs from the one collection wrote (ok=%v err=%v)", ok, err)
			}

			// Retention keeps only the metadata. A skeleton keeps its file's
			// header, so each source is still told from the others.
			res := (&retention.Remover{Zone: zone, MetadataOnlyAfter: time.Hour,
				Now: func() time.Time { return time.Now().Add(24 * time.Hour) }}).Pass(retention.Input{})
			if len(res.Errors) > 0 || res.Files == 0 {
				t.Fatalf("retention: %+v; the test needs files removed", res)
			}
			verified(t, zone, "after the files were removed", 4)
			if after := rebuiltIndex(t, zone); !reflect.DeepEqual(after, ix) {
				t.Fatal("the index rebuilt from the skeletons differs from the one rebuilt from the files")
			}
		})
	}
}

// asz verify read only <kind>.cursor, the first file's, so a lost landed file
// of a later file at the path was not found. Each source is held to its own
// cursor.
func TestALostFileOfALaterFileIsFound(t *testing.T) {
	src := t.TempDir()
	path := filepath.Join(src, "file-changes-inline", "output", session, "main.jsonl")
	zone := storage.NewZone(t.TempDir())
	c := claudecodechanges.New(src, zone, 0)
	collect := func(want int) {
		t.Helper()
		if st, err := c.CollectAll(nil); err != nil || !st.Complete() || st.Records != want {
			t.Fatalf("%+v err=%v; want %d records", st, err, want)
		}
	}
	appendTo(t, path, line("a1", "main", "toolu_a1", "/w")+line("a2", "main", "toolu_a2", "/w"))
	collect(2)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	appendTo(t, path, line("b1", "main", "toolu_b1", "/w"))
	collect(1)
	appendTo(t, path, line("b2", "main", "toolu_b2", "/w"))
	collect(1)
	parseTwice(t, zone)
	verified(t, zone, "as landed", 2)
	files := landedFiles(t, zone)
	if err := os.Remove(files[len(files)-1].Path); err != nil {
		t.Fatal(err)
	}
	rep, err := verify.Session(zone, session)
	if err != nil {
		t.Fatal(err)
	}
	if rep.EndGaps != 1 || rep.Problems != 1 || !strings.Contains(strings.Join(rep.Details(), " "), "changes-") {
		t.Fatalf("verify finds %v; want the later file's cursor ahead of its landed records", rep.Details())
	}
}

// landedFiles lists the session's landed files, in sequence order.
func landedFiles(t *testing.T, zone *storage.Zone) []storage.LandedFile {
	t.Helper()
	files, err := storage.LandedFiles(zone, session)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// parseTwice parses the session twice. The second parse has nothing new to
// read, so it writes no round.
func parseTwice(t *testing.T, zone *storage.Zone) {
	t.Helper()
	for i := 0; i < 2; i++ {
		r, err := parse.Session(zone, parse.Options{Conversation: session, Session: session})
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 && r.Number != 0 {
			t.Fatalf("a second parse wrote round %d; nothing changed", r.Number)
		}
	}
}

// verified checks the session with asz verify: no problem, no record counted
// as landed again, and one report for each source of the plugin's kind.
func verified(t *testing.T, zone *storage.Zone, when string, sources int) {
	t.Helper()
	rep, err := verify.Session(zone, session)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() || rep.Relanded != 0 || len(rep.Streams) != sources {
		t.Fatalf("%s: verify finds %v, %d records landed again and %d sources; want none, none and %d",
			when, rep.Details(), rep.Relanded, len(rep.Streams), sources)
	}
}

// rebuiltIndex builds the session's index again from its landed files.
func rebuiltIndex(t *testing.T, zone *storage.Zone) *index.Index {
	t.Helper()
	ix := index.New(session)
	if _, err := index.Rebuild(zone, session, ix, 0); err != nil {
		t.Fatal(err)
	}
	return ix
}

// cursorsName checks that every cursor that read a line, 0.6.0 being what
// wrote it, records the file's first line, and names the src its files land
// with: each source has its own.
func cursorsName(t *testing.T, dir string, srcs map[string]bool) {
	t.Helper()
	items, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	named := map[string]bool{}
	for _, it := range items {
		if !strings.HasSuffix(it.Name(), ".cursor") {
			continue
		}
		cur, err := storage.LoadCursor(filepath.Join(dir, it.Name()), storage.CursorAppend, "")
		if err != nil {
			t.Fatal(err)
		}
		if cur.Offset == 0 {
			continue
		}
		if cur.HeadSHA256 == "" {
			t.Fatalf("the cursor %s read to %d and records no first line", it.Name(), cur.Offset)
		}
		if !srcs[cur.Source] || named[cur.Source] {
			t.Fatalf("the cursor %s names %q; want one cursor for each of %v", it.Name(), cur.Source, srcs)
		}
		named[cur.Source] = true
	}
	if len(named) != len(srcs) {
		t.Fatalf("the cursors name %v; want %v", named, srcs)
	}
}

// A cursor 0.5.0 wrote names its root and records no first line. It goes on
// as it did: lines appended to its file land, and nothing is landed twice.
// It is not given a first line afterwards, so a file the plugin writes again
// at its path is still a conflict, as the storage root page says.
func TestACursorWithoutAFirstLineGoesOnAsBefore(t *testing.T) {
	src := t.TempDir()
	path := filepath.Join(src, "file-changes-inline", "output", session, "main.jsonl")
	zone := storage.NewZone(t.TempDir())
	c := claudecodechanges.New(src, zone, 0)
	appendTo(t, path, line("a1", "main", "toolu_a1", "/w")+line("a2", "main", "toolu_a2", "/w"))
	if st, err := c.CollectAll(nil); err != nil || !st.Complete() || st.Records != 2 {
		t.Fatalf("the first pass: %+v err=%v", st, err)
	}
	cursor := filepath.Join(zone.StreamDir(session, "main"), "changes.cursor")
	dropKeys(t, cursor, "head_sha256")
	appendTo(t, path, line("a3", "main", "toolu_a3", "/w"))
	for i, want := range []int{1, 0} {
		if st, err := c.CollectAll(nil); err != nil || !st.Complete() || st.Records != want {
			t.Fatalf("pass %d after the cursor of 0.5.0: %+v err=%v; want %d records", i+1, st, err, want)
		}
	}
	parseTwice(t, zone)
	if held, err := storage.LoadCursor(cursor, storage.CursorAppend, ""); err != nil || held.HeadSHA256 != "" || held.Origin == "" {
		t.Fatalf("the cursor: %+v %v; want its root and still no first line", held, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	appendTo(t, path, line("b1", "main", "toolu_b1", "/w"))
	if st, _ := c.CollectAll(nil); st == nil || st.Conflicts != 1 || st.Records != 0 {
		t.Fatalf("a file written again behind the cursor of 0.5.0: %+v; want the conflict the page states", st)
	}
}

// dropKeys rewrites a cursor without the keys given, as an older asz wrote it.
func dropKeys(t *testing.T, path string, keys ...string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, l := range strings.Split(string(raw), "\n") {
		key, _, _ := strings.Cut(l, " ")
		if !slices.Contains(keys, key) {
			kept = append(kept, l)
		}
	}
	if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A repacked root goes on from where the old one stopped. Only the plain
// cursor was carried over, so a collector pointed at the new root read a
// later file at the same path from its start and landed all of it again.
// And a new file never mixes two sources: its header names one src.
func TestARepackCarriesTheCursorOfALaterFile(t *testing.T) {
	src := t.TempDir()
	path := filepath.Join(src, "file-changes-inline", "output", session, "main.jsonl")
	zone := storage.NewZone(t.TempDir())
	appendTo(t, path, line("a1", "main", "toolu_a1", "/w")+line("a2", "main", "toolu_a2", "/w"))
	if st, err := claudecodechanges.New(src, zone, 0).CollectAll(nil); err != nil || !st.Complete() || st.Records != 2 {
		t.Fatalf("the first file: %+v err=%v", st, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	appendTo(t, path, line("b1", "main", "toolu_b1", "/w"))
	if st, err := claudecodechanges.New(src, zone, 0).CollectAll(nil); err != nil || !st.Complete() || st.Records != 1 {
		t.Fatalf("the later file: %+v err=%v", st, err)
	}
	for _, budget := range []int64{1, 2 << 20} {
		dst := storage.NewZone(t.TempDir())
		if _, err := repack.Session(zone, dst, session, budget, time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if st, err := claudecodechanges.New(src, dst, 0).CollectAll(nil); err != nil || !st.Complete() || st.Records != 0 {
				t.Fatalf("budget %d, pass %d over the repacked root: %+v err=%v; want nothing landed again", budget, i+1, st, err)
			}
		}
		parseTwice(t, dst)
		verified(t, dst, fmt.Sprintf("repacked under a budget of %d", budget), 2)
		// The first file's two records land in the second new file under a
		// budget of one byte, and in the first under the large one.
		want := map[int64]string{1: "last_seq      2\n", 2 << 20: "last_seq      1\n"}[budget]
		if raw, err := os.ReadFile(filepath.Join(dst.StreamDir(session, "main"), "changes.cursor")); err != nil || !strings.Contains(string(raw), want) {
			t.Fatalf("budget %d: the first file's cursor must name the new file of its own last record, %q: %v\n%s", budget, want, err, raw)
		}
	}
}

// The rule that tells a later file from the one a cursor read must not hide a
// file rewritten behind its cursor. Such a file still begins with its first
// line, so it is the cursor's file, and it is a conflict that stays.
func TestAFileRewrittenBehindItsCursorIsStillAConflict(t *testing.T) {
	first := line("a1", "main", "toolu_1", "/w")
	for name, rewrite := range map[string]string{
		"cut short":      first,
		"a line changed": first + line("a2", "main", "toolu_9", "/w") + line("a3", "main", "toolu_3", "/w") + line("a4", "main", "toolu_4", "/w"),
	} {
		t.Run(name, func(t *testing.T) {
			src := t.TempDir()
			path := filepath.Join(src, "file-changes-inline", "output", session, "main.jsonl")
			zone := storage.NewZone(t.TempDir())
			c := claudecodechanges.New(src, zone, 0)
			appendTo(t, path, first+line("a2", "main", "toolu_2", "/w")+line("a3", "main", "toolu_3", "/w"))
			if st, err := c.CollectAll(nil); err != nil || !st.Complete() || st.Records != 3 {
				t.Fatalf("first pass: %+v err=%v", st, err)
			}
			if err := os.WriteFile(path, []byte(rewrite), 0o644); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if st, _ := c.CollectAll(nil); st == nil || st.Conflicts != 1 || st.Records != 0 {
					t.Fatalf("pass %d over the rewritten file: %+v; want one conflict and nothing landed", i+1, st)
				}
			}
			parseTwice(t, zone)
			verified(t, zone, "after the conflict", 1)
		})
	}
}

// A file the plugin is creating holds no complete line for a moment. It is not
// the file a cursor read, which had one, and nothing in it can land yet, so
// the pass waits for the line rather than calling the path a conflict.
func TestAFileStillWritingItsFirstLineWaits(t *testing.T) {
	src := t.TempDir()
	path := filepath.Join(src, "file-changes-inline", "output", session, "main.jsonl")
	zone := storage.NewZone(t.TempDir())
	c := claudecodechanges.New(src, zone, 0)
	appendTo(t, path, line("a1", "main", "toolu_1", "/w")+line("a2", "main", "toolu_2", "/w"))
	if st, err := c.CollectAll(nil); err != nil || !st.Complete() || st.Records != 2 {
		t.Fatalf("first pass: %+v err=%v", st, err)
	}
	next := line("b1", "main", "toolu_5", "/w")
	for _, part := range []string{"", next[:20]} {
		if err := os.WriteFile(path, []byte(part), 0o644); err != nil {
			t.Fatal(err)
		}
		if st, err := c.CollectAll(nil); err != nil || !st.Complete() || st.Records != 0 {
			t.Fatalf("a first line of %d bytes, not ended: %+v err=%v; want nothing and no conflict", len(part), st, err)
		}
	}
	appendTo(t, path, next[20:])
	if st, err := c.CollectAll(nil); err != nil || !st.Complete() || st.Records != 1 {
		t.Fatalf("the first line, ended: %+v err=%v; want it landed", st, err)
	}
	if st, err := c.CollectAll(nil); err != nil || !st.Complete() || st.Records != 0 {
		t.Fatalf("again: %+v err=%v; want nothing", st, err)
	}
	parseTwice(t, zone)
	verified(t, zone, "after the later file landed", 2)
}
