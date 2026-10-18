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
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecodechanges"
	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp"
	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp/otlptest"
	"github.com/apache/skywalking-ai-sessionizer/internal/retention"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/changes"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// A source line can hold a byte that is not valid UTF-8 inside a JSON
// string: the JSON decoder accepts it, and a write cut short is one way to
// get one. No landed file holds it, since a protocol buffer string must be
// valid UTF-8 and a file holding one could never be sent. The tool input that
// held it is taken out, keeping the call, its size and its digest, and says
// omitted; a change record is written again with U+FFFD in its place; and
// each record's dropped list says why. Collected and parsed twice, the second
// pass lands nothing and changes nothing. The index rebuilt from the files'
// skeletons is the index rebuilt from the files. Every file of the session
// goes over both transports, beside a clean session in the same root.
func TestASourceByteThatIsNotUTF8IsSent(t *testing.T) {
	s := newStage(t, growing)
	b, err := scenario.Build(s.sc, scenario.FormatClaudeCode, s.out, scenario.Options{
		At: time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC), Through: "one",
	})
	if err != nil {
		t.Fatal(err)
	}
	s.session = b.Session
	breakToolInput(t, s.out, s.session)
	recorder := t.TempDir()
	writeBrokenChange(t, recorder, s.session)
	collectChanges := func() int {
		t.Helper()
		st, err := claudecodechanges.New(recorder, s.zone, 0).CollectAll(nil)
		if err != nil || len(st.Errors) > 0 || st.Conflicts != 0 {
			t.Fatalf("the change records: %+v, %v", st, err)
		}
		return st.Records
	}

	// A clean session in the same root, landed first, so its files are sent
	// first too and the broken one lies between them and the rounds.
	clean := newStage(t, strings.Replace(growing, "add1c7ed-0003-4000-8000-000000000003", "add1c7ed-0009-4000-8000-000000000009", 1))
	if _, err := scenario.Build(clean.sc, scenario.FormatClaudeCode, clean.out, scenario.Options{
		At: time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	clean.zone, clean.session = s.zone, "add1c7ed-0009-4000-8000-000000000009"
	if st, err := clean.collector().CollectAll(nil); err != nil || len(st.Errors) > 0 {
		t.Fatal(err, st.Errors)
	}

	st, err := s.collector().CollectAll(nil)
	if err != nil || len(st.Errors) > 0 {
		t.Fatal(err, st.Errors)
	}
	if n := collectChanges(); n != 1 {
		t.Fatalf("%d change records landed; want one", n)
	}
	files, err := storage.LandedFiles(s.zone, s.session)
	if err != nil {
		t.Fatal(err)
	}
	var call, change *sessiondata.Part
	var callDrops, changeDrops []sessiondata.Drop
	for _, lf := range files {
		data, err := os.ReadFile(lf.Path)
		if err != nil {
			t.Fatal(err)
		}
		if !utf8.Valid(data) {
			t.Fatalf("%s holds a byte that is not valid UTF-8", lf.Path)
		}
		for _, rec := range records(t, lf.Path) {
			for i, p := range rec.Parts {
				switch {
				case p.Kind == sessiondata.PartCall && p.Name == "Bash":
					call, callDrops = &rec.Parts[i], rec.Dropped
				case p.Kind == sessiondata.PartData && bytes.Contains(p.Data, []byte(`"changes/1"`)):
					change, changeDrops = &rec.Parts[i], rec.Dropped
				}
			}
		}
	}
	if call == nil || call.ID == "" || len(call.Data) != 0 || call.State != model.ContentOmitted || call.Bytes == 0 || call.SHA256 == "" ||
		len(callDrops) != 1 || callDrops[0].Why != sessiondata.WhyNotUTF8 {
		t.Fatalf("the call whose input held the byte landed as %+v, dropping %+v", call, callDrops)
	}
	if change == nil || change.State != model.ContentTruncated || !bytes.Contains(change.Data, []byte("f\xef\xbf\xbdo.go")) ||
		len(changeDrops) != 1 || changeDrops[0].Why != sessiondata.WhyNotUTF8 {
		t.Fatalf("the change record that held the byte landed as %+v, dropping %+v", change, changeDrops)
	}
	first := s.parse()
	if first.Number == 0 {
		t.Fatal("the first parse wrote no round")
	}

	again, err := s.collector().CollectAll(nil)
	if err != nil || len(again.Errors) > 0 || again.SourcesLanded != 0 || again.Records != 0 {
		t.Fatalf("the second pass landed %d sources and %d records: %v %v", again.SourcesLanded, again.Records, err, again.Errors)
	}
	if n := collectChanges(); n != 0 {
		t.Fatalf("the second pass landed %d change records", n)
	}
	if r := s.parse(); r.Number != 0 {
		t.Fatalf("the second parse wrote round %d over nothing new", r.Number)
	}
	clean.parse()

	// Every file becomes a skeleton in a copy of the root, and the index
	// rebuilt from the skeletons is the one rebuilt from the files.
	removed := storage.NewZone(t.TempDir())
	copyDir(t, s.zone.Root(), removed.Root())
	rem := &retention.Remover{Zone: removed, MetadataOnlyAfter: time.Hour, Now: later}
	if res := rem.Pass(retention.Input{}); res.Files == 0 || len(res.Errors) > 0 {
		t.Fatalf("retention removed %d files: %v", res.Files, res.Errors)
	}
	sameIndex(t, s.zone, removed, s.session)
	verified(t, removed, s.session)

	for _, protocol := range []string{otlp.ProtocolHTTP, otlp.ProtocolGRPC} {
		t.Run(protocol, func(t *testing.T) {
			rcv, err := otlptest.Start()
			if err != nil {
				t.Fatal(err)
			}
			defer rcv.Close()
			c, err := rcv.Client(protocol)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if err := os.Remove(filepath.Join(s.zone.Root(), otlp.StateFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			p := &otlp.Pusher{Zone: s.zone, Client: c, Version: "test", ServiceName: "Claude Code", Endpoint: protocol}
			st, err := p.Pass()
			if err != nil || len(st.Errors) > 0 {
				t.Fatalf("the push failed: %v %v", err, st.Errors)
			}
			cleanFiles, err := storage.LandedFiles(s.zone, clean.session)
			if err != nil {
				t.Fatal(err)
			}
			if st.Files < len(files)+len(cleanFiles)+2 {
				t.Fatalf("the push sent %d files; want every landed file of both sessions and their rounds", st.Files)
			}
			if st, err := p.Pass(); err != nil || len(st.Errors) > 0 || st.Files != 0 {
				t.Fatalf("the next push sent %d files: %v %v", st.Files, err, st.Errors)
			}
		})
	}
}

// breakToolInput puts a raw 0xff byte inside the tool input string of the
// session's transcript.
func breakToolInput(t *testing.T, out, session string) {
	t.Helper()
	var transcript string
	err := filepath.WalkDir(filepath.Join(out, "_source"), func(p string, _ fs.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(p, session+".jsonl") {
			transcript = p
		}
		return err
	})
	if err != nil || transcript == "" {
		t.Fatalf("no transcript: %v", err)
	}
	src, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatal(err)
	}
	var changed []byte
	for _, line := range bytes.SplitAfter(src, []byte("\n")) {
		if bytes.Contains(line, []byte(`"echo one"`)) && bytes.Contains(line, []byte(`"tool_use"`)) {
			line = bytes.Replace(line, []byte(`"echo one"`), []byte("\"echo \xff one\""), 1)
			if !json.Valid(bytes.TrimSpace(line)) {
				t.Fatal("the JSON decoder refuses the line; the test needs one it accepts")
			}
		}
		changed = append(changed, line...)
	}
	if bytes.Equal(changed, src) {
		t.Fatal("no line changed")
	}
	if err := os.WriteFile(transcript, changed, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeBrokenChange writes the recorder's change record for the session's
// Bash call, with a raw 0xff byte inside the path of the file it changed.
func writeBrokenChange(t *testing.T, root, session string) {
	t.Helper()
	r := &changes.Record{Schema: changes.Schema, ID: "c1", CapturedBy: changes.CapturedByASZPlugin, Session: session,
		Stream: "main", Tool: "toolu_1", ToolName: "Bash", Time: "2026-02-01T10:00:04Z", Basis: changes.BasisToolWindow,
		Root: &changes.Root{Path: "/w"}, ChangedFiles: changes.Int(1),
		Changes: []changes.FileChange{{Path: "placeholder.go", Operation: changes.OpModify, Diff: changes.DiffUnavailable}}}
	line, err := r.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	line = bytes.Replace(line, []byte("placeholder.go"), []byte("f\xffo.go"), 1)
	path := filepath.Join(root, "file-changes-market", "output", session, "main.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(line, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// records reads every record of a landed file.
func records(t *testing.T, path string) []*sessiondata.Record {
	t.Helper()
	f, err := sessiondata.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r, err := sessiondata.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var out []*sessiondata.Record
	for {
		rec, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, rec)
	}
}
