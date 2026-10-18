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
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// A tool result's content is a string or a list of blocks. The text of the
// text blocks is the part's text. Content that is more than text keeps its
// blocks whole as the part's data, since the source can be pruned and the
// landed copy may be the only one. A list of a text and an image, as a tool
// that takes a screenshot returns, kept only the text: the image was in no
// part and named in no drop.
func TestAToolResultKeepsWhatIsNotText(t *testing.T) {
	const image = `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}`
	const caption = `{"type":"text","text":"Took a screenshot"}`
	cases := []struct {
		name, content, toolUseResult string
		text                         string
		data                         string // the part's data, or empty for none
	}{
		{"text and an image", `[` + caption + `,` + image + `]`, ``, "Took a screenshot", `[` + caption + `,` + image + `]`},
		{"text and an image, with a list beside them", `[` + caption + `,` + image + `]`, `,"toolUseResult":[` + caption + `,` + image + `]`, "Took a screenshot", `[` + caption + `,` + image + `]`},
		{"an image alone", `[` + image + `]`, ``, "", `[` + image + `]`},
		{"text blocks alone", `[` + caption + `,{"type":"text","text":"and more"}]`, ``, "Took a screenshot\nand more", ``},
		{"a string", `"Took a screenshot"`, ``, "Took a screenshot", ``},
	}
	var transcript strings.Builder
	for i, tc := range cases {
		transcript.WriteString(`{"type":"user","uuid":"u` + string(rune('1'+i)) + `","sessionId":"` + tSess + `","timestamp":"2026-09-08T10:00:01.000Z",` +
			`"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_s","content":` + tc.content + `}]}` +
			tc.toolUseResult + "}\n")
	}
	src, root := t.TempDir(), t.TempDir()
	mk(t, filepath.Join(src, "-proj-a", tSess+".jsonl"), transcript.String())
	zone := storage.NewZone(root)
	col := claudecode.New(src, zone, 0)
	for i, want := range []int{len(cases), 0} {
		st, err := col.CollectAll(nil)
		if err != nil || !st.Complete() || st.Records != want {
			t.Fatalf("pass %d: %+v err=%v; want %d records", i+1, st, err, want)
		}
	}
	for i := 0; i < 2; i++ {
		r, err := parse.Session(zone, parse.Options{Conversation: tSess, Session: tSess})
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 && r.Number != 0 {
			t.Fatalf("a second parse wrote round %d; nothing changed", r.Number)
		}
	}
	landed := map[string]*sessiondata.Record{}
	files, err := storage.LandedFiles(zone, tSess)
	if err != nil {
		t.Fatal(err)
	}
	for _, lf := range files {
		for _, rec := range readLanded(t, lf.Path) {
			landed[rec.ID] = rec
		}
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := landed["u"+string(rune('1'+i))]
			if rec == nil || len(rec.Parts) != 1 || rec.Parts[0].Kind != sessiondata.PartResult {
				t.Fatalf("the landed record: %+v", rec)
			}
			p := rec.Parts[0]
			if p.Text != tc.text {
				t.Errorf("text %q, want %q", p.Text, tc.text)
			}
			if tc.data == "" {
				if len(p.Data) != 0 {
					t.Errorf("data %s, want none: the text is all the content holds", p.Data)
				}
				return
			}
			var want bytes.Buffer
			if err := json.Compact(&want, []byte(tc.data)); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(p.Data, want.Bytes()) {
				t.Errorf("data %s, want the content whole: %s", p.Data, want.Bytes())
			}
			if p.Bytes != len(tc.content) {
				t.Errorf("bytes %d, want the size of the content, %d", p.Bytes, len(tc.content))
			}
			if len(rec.Dropped) != 0 {
				t.Errorf("dropped %+v; nothing was left out", rec.Dropped)
			}
		})
	}
}

// readLanded reads every record of a landed file, as a reader of the format
// would.
func readLanded(t *testing.T, path string) []*sessiondata.Record {
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
