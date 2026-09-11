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

package claudecode_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// An unknown part keeps every byte it was given, including bytes that are
// not valid UTF-8, and a reader gets them back from the landed file by the
// rule the format page states. A scenario cannot express this. A claude-code
// build writes every record, sidecar, journal line and manifest with
// json.Marshal, which always writes valid UTF-8, and writes whole lines.
// Only a workflow script is written as text, made from the workflow's name.
// A name given as YAML binary can hold such a byte, but it also names the
// run's directory, which macOS refuses to create, and an sd build passes the
// script through json.Marshal. The cases are ways such bytes can reach the
// adapter: a line that is not JSON, a write cut short and joined to the next
// line, a block of a type the dialect does not know, and a workflow script
// saved in another encoding.
func TestUnknownPartKeepsEveryByte(t *testing.T) {
	transcript := claudecode.Source{Kind: claudecode.SrcMainTranscript, Session: "s", Stream: "main"}
	script := claudecode.Source{Kind: claudecode.SrcWorkflowScript, Session: "s", RunID: "wf_r1"}
	block := `{"type":"x-new","v":"` + "\xff" + `"}`
	for _, tc := range []struct {
		name    string
		src     claudecode.Source
		payload string
		kept    string // the bytes the unknown part must keep
		text    bool   // the bytes are valid UTF-8, so they keep the text form
	}{
		{"a line that is not JSON", transcript, "\xffx", "\xffx", false},
		{"a write cut short and joined to the next line", transcript,
			`{"type":"user","message":{"content":"caf` + "\xc3" + `{"type":"user","uuid":"u2"}`,
			`{"type":"user","message":{"content":"caf` + "\xc3" + `{"type":"user","uuid":"u2"}`, false},
		{"a block type the dialect does not know", transcript,
			`{"type":"assistant","uuid":"u1","message":{"content":[` + block + `]}}`, block, false},
		{"a script saved as Latin-1", script, "export const s = 'caf\xe9'", "export const s = 'caf\xe9'", false},
		// The control: valid UTF-8 with the characters JSON escapes. It keeps
		// the form every landed file has used, so existing readers read it.
		{"valid UTF-8", transcript, "not json: a < b && c > d, café 中", "not json: a < b && c > d, café 中", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := claudecode.Convert(tc.src, 1, 0, []byte(tc.payload))
			part := landedUnknown(t, rec)
			got := keptBytes(t, part)
			if !bytes.Equal(got, []byte(tc.kept)) {
				t.Fatalf("the part keeps %x, the source held %x", got, []byte(tc.kept))
			}
			var size int
			_ = json.Unmarshal(part["bytes"], &size)
			var state string
			_ = json.Unmarshal(part["state"], &state)
			if size != len(tc.kept) || state != "available" {
				t.Errorf("bytes=%d state=%s, want %d and available", size, state, len(tc.kept))
			}
			_, marked := part["encoding"]
			if tc.text {
				// Byte for byte what the adapter wrote before the encoding
				// existed: the bytes as one JSON string, and no other key.
				want, _ := json.Marshal(tc.kept)
				if marked || !bytes.Equal(part["data"], want) {
					t.Errorf("valid UTF-8 changed form: data=%s encoding present=%v", part["data"], marked)
				}
			} else if !marked {
				t.Error("bytes that are not valid UTF-8 carry no encoding, so a reader cannot tell base64 from text")
			}
		})
	}
}

// landedUnknown writes a record to a landed file, reads the line back as a
// reader of the format would, and returns its unknown part's fields.
func landedUnknown(t *testing.T, rec *sessiondata.Record) map[string]json.RawMessage {
	t.Helper()
	var buf bytes.Buffer
	w, err := sessiondata.NewWriter(&buf, &sessiondata.Header{
		Seq: 1, At: "2026-09-11T00:00:00Z", Kind: sessiondata.KindTranscript,
		Adapter: claudecode.Name + "/" + claudecode.Version, Dialect: claudecode.Dialect,
		Src: "p/s.jsonl", Session: "s", Stream: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(rec); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := sessiondata.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	line, err := r.NextRaw()
	if err != nil {
		t.Fatal(err)
	}
	var fields struct {
		Parts []map[string]json.RawMessage `json:"parts"`
	}
	if err := json.Unmarshal(line, &fields); err != nil {
		t.Fatal(err)
	}
	for _, p := range fields.Parts {
		if string(p["k"]) == `"unknown"` {
			return p
		}
	}
	t.Fatalf("no unknown part in %s", line)
	return nil
}

// keptBytes applies the format's rule: data is one JSON string, the bytes
// themselves, or their base64 when encoding says base64.
func keptBytes(t *testing.T, part map[string]json.RawMessage) []byte {
	t.Helper()
	var s string
	if err := json.Unmarshal(part["data"], &s); err != nil {
		t.Fatal(err)
	}
	var enc string
	if raw, ok := part["encoding"]; ok {
		_ = json.Unmarshal(raw, &enc)
	}
	switch enc {
	case "":
		return []byte(s)
	case "base64":
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	t.Fatalf("encoding %q is not one the format names", enc)
	return nil
}
