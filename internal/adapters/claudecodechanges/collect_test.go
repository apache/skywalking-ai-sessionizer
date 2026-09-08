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
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecodechanges"
	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/changes"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

const (
	session = "11111111-2222-4333-8444-555555555555"
	agent   = "a0123456789abcdef"
)

func line(id, stream, tool, root string) string {
	r := &changes.Record{
		Schema: changes.Schema, ID: id, Session: session, Stream: stream, Tool: tool, ToolName: "Bash",
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
