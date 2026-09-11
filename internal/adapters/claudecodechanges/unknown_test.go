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
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecodechanges"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// A line this adapter cannot read lands as an unknown part that keeps every
// byte, including bytes that are not valid UTF-8. The lines go through a
// real collection, and the landed file is read back with Part.Raw, as a
// reader of the format would. A scenario cannot express this. A claude-code
// build writes each of the plugin's lines whole, with json.Marshal, which
// always writes valid UTF-8, and an sd build lands the change records itself.
func TestUnknownPartKeepsEveryByte(t *testing.T) {
	cases := []struct {
		name string
		line string
		text bool // the line is valid UTF-8, so its part keeps the text form
	}{
		{"a line that is not JSON", "\xffx", false},
		// The plugin's write stopped inside a two-byte character, and its
		// next write went on the same line.
		{"a write cut short and joined to the next line",
			`{"schema":"changes/1","id":"p1/c1","tool_name":"caf` + "\xc3" +
				strings.TrimSuffix(line("p1/c2", "main", "toolu_2", "/w"), "\n"), false},
		// The control: valid UTF-8 with the characters JSON escapes. It must
		// land exactly as it did before bytes that are not valid UTF-8 had a
		// form of their own.
		{"valid UTF-8", `{"schema":"something/9","note":"a < b && c > d, café 中"}`, true},
	}
	src := t.TempDir()
	var source strings.Builder
	for _, tc := range cases {
		source.WriteString(tc.line + "\n")
	}
	appendTo(t, filepath.Join(src, "asz-changes-inline", "output", session, "main.jsonl"), source.String())
	zone := storage.NewZone(t.TempDir())
	st, err := claudecodechanges.New(src, zone, 0).CollectAll(nil)
	if err != nil || len(st.Errors) != 0 || st.Records != len(cases) {
		t.Fatalf("stats: %+v err=%v", st, err)
	}
	files, err := storage.LandedFiles(zone, session)
	if err != nil || len(files) != 1 {
		t.Fatalf("landed %d files, err=%v", len(files), err)
	}
	lines := rawLines(t, files[0].Path)
	if len(lines) != len(cases) {
		t.Fatalf("landed %d records, want %d", len(lines), len(cases))
	}

	off := 0
	for i, tc := range cases {
		at := off
		off += len(tc.line) + 1
		t.Run(tc.name, func(t *testing.T) {
			var rec sessiondata.Record
			if err := json.Unmarshal(lines[i], &rec); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256([]byte(tc.line))
			sha := hex.EncodeToString(sum[:])[:12]
			if rec.Sha != sha || rec.Bytes != len(tc.line) || rec.Off != uint64(at) {
				t.Fatalf("provenance sha=%s bytes=%d off=%d, want %s %d %d", rec.Sha, rec.Bytes, rec.Off, sha, len(tc.line), at)
			}
			if len(rec.Parts) != 1 || rec.Parts[0].Kind != sessiondata.PartUnknown {
				t.Fatalf("parts: %s", lines[i])
			}
			p := rec.Parts[0]
			got, err := p.Raw()
			if err != nil {
				t.Fatal(err)
			}
			if src := []byte(tc.line); !bytes.Equal(got, src) {
				n := 0
				for n < len(got) && n < len(src) && got[n] == src[n] {
					n++
				}
				t.Fatalf("from byte %d the part keeps %x, the source held %x", n, head(got[n:]), head(src[n:]))
			}
			if p.State != "available" || p.Bytes != len(tc.line) {
				t.Errorf("state=%s bytes=%d, want available and %d", p.State, p.Bytes, len(tc.line))
			}
			if !tc.text {
				if p.Encoding != sessiondata.EncodingBase64 {
					t.Errorf("bytes that are not valid UTF-8 carry encoding %q, so a reader cannot tell base64 from text", p.Encoding)
				}
				return
			}
			// The whole landed line, byte for byte, as the adapter wrote it
			// before the encoding existed: the line as one JSON string in
			// data, and no encoding key.
			before, _ := json.Marshal(tc.line)
			want, _ := json.Marshal(&sessiondata.Record{
				Ord: uint64(i + 1), Off: uint64(at), Sha: sha, Bytes: len(tc.line),
				Parts: []sessiondata.Part{{
					Kind: sessiondata.PartUnknown, Text: p.Text, Data: before,
					State: "available", Bytes: len(tc.line),
				}},
			})
			if !bytes.Equal(lines[i], want) {
				t.Errorf("valid UTF-8 changed form:\n got %s\nwant %s", lines[i], want)
			}
		})
	}
}

// head keeps a failure message short: eight bytes are enough to show a byte
// that was replaced.
func head(b []byte) []byte {
	if len(b) > 8 {
		return b[:8]
	}
	return b
}

// rawLines returns a landed file's records as the bytes of their lines. The
// reader still checks the closing digest.
func rawLines(t *testing.T, path string) [][]byte {
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
	var out [][]byte
	for {
		line, err := r.NextRaw()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, bytes.Clone(line))
	}
}
