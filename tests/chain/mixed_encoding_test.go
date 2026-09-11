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

package chain_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/repack"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/verify"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

const encodingSession = "add1c7ed-0040-4000-8000-000000000040"

// An upgrade leaves earlier files untouched and adds later files to the same
// chain. The old writer's bytes are a fixture so this test keeps exercising
// that boundary after the old implementation is removed.
func TestMixedWriterEncodingKeepsTheConversation(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "mixed-encoding", "released-writer.sd"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(fixture, []byte(`\u003c`)) || bytes.Contains(fixture, []byte("<")) {
		t.Fatal("released-writer fixture no longer carries the old HTML escapes")
	}
	records := encodingRecords()
	mixed, fresh := encodingStage(t), encodingStage(t)
	oldPath := encodingPath(mixed.zone, 1)
	if err := storage.WriteAtomic(oldPath, storage.PermLanded, func(w io.Writer) error {
		_, err := w.Write(fixture)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	landEncoding(t, fresh.zone, 1, records[:3])
	for _, s := range []*stage{mixed, fresh} {
		if r := s.parse(); r.Number != 1 || r.ThroughSeq != 1 {
			t.Fatalf("first file produced round %d through %d", r.Number, r.ThroughSeq)
		}
		landEncoding(t, s.zone, 2, records[3:])
		// This test supplies landed files directly. Rebuild the derived
		// index so the parser sees the second file, as a collector would.
		if err := os.RemoveAll(s.zone.IndexDir(encodingSession)); err != nil {
			t.Fatal(err)
		}
		if r := s.parse(); r.Number != 2 || r.ThroughSeq != 2 {
			t.Fatalf("new file produced round %d through %d", r.Number, r.ThroughSeq)
		}
		checkEncodingRoot(t, s.zone, 6)
	}
	if unchanged, err := os.ReadFile(oldPath); err != nil || !bytes.Equal(unchanged, fixture) {
		t.Fatalf("appending new evidence changed the old file: %v", err)
	}
	mixedLines, freshLines := encodingLines(t, mixed.zone), encodingLines(t, fresh.zone)
	if len(mixedLines) != 6 || len(freshLines) != 6 {
		t.Fatalf("record counts: mixed=%d, fresh=%d", len(mixedLines), len(freshLines))
	}
	if bytes.Equal(mixedLines[1], freshLines[1]) || !bytes.Contains(mixedLines[4], []byte("new <data> & result")) {
		t.Fatal("test did not produce both old and new data encodings")
	}
	for i := range mixedLines {
		var a, b any
		if err := json.Unmarshal(mixedLines[i], &a); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(freshLines[i], &b); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Errorf("record %d differs in decoded content or provenance", i+1)
		}
	}
	mf, err := sessionflow.OpenChain(mixed.out, encodingSession).Fold()
	if err != nil {
		t.Fatal(err)
	}
	ff, err := sessionflow.OpenChain(fresh.out, encodingSession).Fold()
	if err != nil {
		t.Fatal(err)
	}
	// The chain binds to the bytes of its inputs. All remaining fold fields,
	// including every node, relation, reference and revision, must agree.
	mf.Digest, mf.InputDigest = ff.Digest, ff.InputDigest
	if !reflect.DeepEqual(mf, ff) {
		t.Fatal("mixed and new encodings produced different folds")
	}
	compareEncodingViews(t, mixed.zone, fresh.zone)

	repacked := storage.NewZone(t.TempDir())
	stats, err := repack.Session(mixed.zone, repacked, encodingSession, 1, time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if stats.FilesIn != 2 || stats.FilesOut != 6 || stats.Records != 6 {
		t.Fatalf("unexpected repack: %+v", stats)
	}
	if got := encodingLines(t, repacked); !reflect.DeepEqual(got, mixedLines) {
		t.Fatal("repack changed old or new record bytes")
	}
	s := &stage{t: t, out: repacked.Root(), zone: repacked, session: encodingSession}
	s.parse()
	checkEncodingRoot(t, repacked, 6)
}

func encodingStage(t *testing.T) *stage {
	t.Helper()
	out := t.TempDir()
	return &stage{t: t, out: out, zone: storage.NewZone(out), session: encodingSession}
}

func encodingHeader(seq uint64) *sessiondata.Header {
	return &sessiondata.Header{
		Seq: seq, At: "2026-02-01T10:00:00Z", Kind: sessiondata.KindTranscript,
		Adapter: "mock/0.2.0", Dialect: "mock/1", Src: "mixed-encoding/transcript.jsonl",
		Session: encodingSession, Stream: "main",
	}
}

func encodingPath(z *storage.Zone, seq uint64) string {
	return filepath.Join(z.StreamDir(encodingSession, "main"), storage.LandedName("transcript", "20260201T100000.000000000Z", seq))
}

func landEncoding(t *testing.T, z *storage.Zone, seq uint64, records []*sessiondata.Record) {
	t.Helper()
	err := storage.WriteAtomic(encodingPath(z, seq), storage.PermLanded, func(out io.Writer) error {
		w, err := sessiondata.NewWriter(out, encodingHeader(seq))
		if err != nil {
			return err
		}
		for _, rec := range records {
			if err := w.Write(rec); err != nil {
				return err
			}
		}
		return w.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
}

func encodingRecords() []*sessiondata.Record {
	var records []*sessiondata.Record
	var off uint64
	parent := ""
	for _, phase := range []string{"old", "new"} {
		input := "ask <" + phase + "> & now"
		result := "found <" + phase + "> & done"
		data := json.RawMessage(`{"query":"` + phase + ` <data> & result"}`)
		batch := []*sessiondata.Record{
			{ID: "input-" + phase, Parent: parent, Run: "run-" + phase, From: sessiondata.FromExternal,
				Trigger: model.TriggerExternal, Flags: []string{"external_input"},
				Parts: []sessiondata.Part{{Kind: sessiondata.PartText, Text: input, State: "available", Bytes: len(input)}}},
			{ID: "call-" + phase, Parent: "input-" + phase, Run: "run-" + phase, Call: "llm-" + phase,
				From: sessiondata.FromAgent, Flags: []string{"finished"},
				Parts: []sessiondata.Part{{Kind: sessiondata.PartCall, ID: "tool-" + phase, Name: "Search", Data: data, State: "available", Bytes: len(data)}}},
			{ID: "result-" + phase, Parent: "call-" + phase, Run: "run-" + phase, From: sessiondata.FromExternal,
				Parts: []sessiondata.Part{{Kind: sessiondata.PartResult, Of: "tool-" + phase, Text: result, State: "available", Bytes: len(result)}}},
		}
		for _, rec := range batch {
			rec.Ord, rec.Off = uint64(len(records)+1), off
			rec.Time = fmt.Sprintf("2026-02-01T09:00:%02dZ", rec.Ord)
			// The fixture represents these synthetic source lines. Their
			// provenance stays the same across both landing encodings.
			source := []byte(fmt.Sprintf(`{"id":%q}`, rec.ID))
			sum := sha256.Sum256(source)
			rec.Sha, rec.Bytes = hex.EncodeToString(sum[:]), len(source)
			off += uint64(len(source) + 1)
			records = append(records, rec)
		}
		parent = "result-" + phase
	}
	return records
}

func checkEncodingRoot(t *testing.T, z *storage.Zone, records int) {
	t.Helper()
	sr, err := verify.Session(z, encodingSession)
	if err != nil {
		t.Fatal(err)
	}
	if !sr.OK() || sr.Records != records {
		t.Fatalf("session verification: records=%d, problems=%v", sr.Records, sr.Details())
	}
	cr, err := verify.Chain(z, encodingSession, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !cr.OK() {
		t.Fatalf("chain verification: %v", cr.Details())
	}
}

func encodingLines(t *testing.T, z *storage.Zone) [][]byte {
	t.Helper()
	files, err := storage.LandedFiles(z, encodingSession)
	if err != nil {
		t.Fatal(err)
	}
	var lines [][]byte
	for _, file := range files {
		data, err := os.ReadFile(file.Path)
		if err != nil {
			t.Fatal(err)
		}
		r, err := sessiondata.NewReader(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		for {
			line, err := r.NextRaw()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			lines = append(lines, bytes.Clone(line))
		}
	}
	return lines
}

func encodingView(t *testing.T, z *storage.Zone) *sessionview.Conversation {
	t.Helper()
	c, err := view.New(z, nil).Load(encodingSession)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := c.Build()
	if err != nil {
		t.Fatal(err)
	}
	if doc.Summary.State != sessionview.StateVerified || doc.Head.Round != 2 || len(doc.Rounds) != 2 || len(doc.Files) != 4 {
		t.Fatalf("unexpected view summary: %+v", doc.Summary)
	}
	// Sizes and digests may differ, but each must describe its actual file.
	for _, file := range doc.Files {
		data, err := os.ReadFile(filepath.Join(z.Root(), filepath.FromSlash(file.File)))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		if file.Bytes != int64(len(data)) || file.Digest != hex.EncodeToString(sum[:]) {
			t.Fatalf("view reported wrong bytes or digest for %s", file.File)
		}
		if file.Format == "sf" {
			if file.Round == nil || *file.Round < 1 || *file.Round > uint64(len(doc.Rounds)) {
				t.Fatalf("invalid round file: %+v", file)
			}
			round := doc.Rounds[*file.Round-1]
			if len(round.Digest) != 64 {
				t.Fatalf("invalid round digest: %q", round.Digest)
			}
			want := fmt.Sprintf("r%06d-%s.sf", round.Round, round.Digest[:12])
			if filepath.Base(file.File) != want {
				t.Fatalf("round filename %s does not match %s", file.File, want)
			}
		}
	}
	return doc
}

func compareEncodingViews(t *testing.T, mixed, fresh *storage.Zone) {
	t.Helper()
	a, b := encodingView(t, mixed), encodingView(t, fresh)
	a.Head.Digest = b.Head.Digest
	for i := range a.Rounds {
		a.Rounds[i].Digest = b.Rounds[i].Digest
		a.Rounds[i].InputDigest = b.Rounds[i].InputDigest
		if (a.Rounds[i].Previous == nil) != (b.Rounds[i].Previous == nil) {
			t.Fatal("old encoding changed whether a round has a predecessor")
		}
		a.Rounds[i].Previous = b.Rounds[i].Previous
	}
	for i := range a.Files {
		x, y := &a.Files[i], &b.Files[i]
		switch {
		case x.Format == "sd" && x.Seq != nil && *x.Seq == 1:
			if x.Bytes <= y.Bytes || x.Digest == y.Digest {
				t.Fatal("old and new files did not differ by their expected encoding")
			}
			x.Bytes, x.Digest = y.Bytes, y.Digest
		case x.Format == "sf":
			if x.Round == nil || y.Round == nil || *x.Round != *y.Round {
				t.Fatal("view changed the round files")
			}
			x.File, x.Bytes, x.Digest = y.File, y.Bytes, y.Digest
		}
	}
	oldText := `{"query":"old \u003cdata\u003e \u0026 result"}`
	newText := `{"query":"old <data> & result"}`
	changed := 0
	var adjust func([]sessionview.Node)
	adjust = func(nodes []sessionview.Node) {
		for i := range nodes {
			n := &nodes[i]
			if n.Kind == model.KindTool && n.Ref != nil && n.Ref.Seq == 1 && n.Ref.Row == 2 {
				if n.Text != oldText {
					t.Fatalf("old data-only tool text: got %q, want %q", n.Text, oldText)
				}
				n.Text = newText
				changed++
			}
			adjust(n.Children)
		}
	}
	adjust(a.Talks)
	adjust(a.Loose)
	if changed != 1 {
		t.Fatalf("found %d old data-only tool steps, want 1", changed)
	}
	if !reflect.DeepEqual(a, b) {
		ja, _ := sessionview.MarshalYAML(a)
		jb, _ := sessionview.MarshalYAML(b)
		t.Fatalf("view differed beyond the old data-only step and file bytes:\n%s\nagainst:\n%s", ja, jb)
	}
}
