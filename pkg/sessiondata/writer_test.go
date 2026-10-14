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

package sessiondata_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// These are byte expectations, not decoded-value comparisons. Both Go JSON
// engines must retain data's spelling even where their encoders would change it.
func TestWriterDataBytesDoNotDependOnJSONEngine(t *testing.T) {
	cases := []struct {
		name string
		data string
		want string
	}{
		{"html", `{"html":"</script><tag>&"}`, `{"html":"</script><tag>&"}`},
		{"line separators", "{\"text\":\"one\u2028two\u2029three\"}", "{\"text\":\"one\u2028two\u2029three\"}"},
		{"escape spelling", `{"s":"\u003C\u003e\u0026\/\u2028\u2029\u0001\u00e9\u00E9\ud800\ud83d\ude00"}`, `{"s":"\u003C\u003e\u0026\/\u2028\u2029\u0001\u00e9\u00E9\ud800\ud83d\ude00"}`},
		{"number spelling", `[1E400,-0,1.0,1e+09,9007199254740993]`, `[1E400,-0,1.0,1e+09,9007199254740993]`},
		{"duplicate keys and order", `{"z":1,"a":2,"z":3}`, `{"z":1,"a":2,"z":3}`},
		{"invalid utf8", "{\"text\":\"\xff\xfe\xc0\xaf\"}", "{\"text\":\"\xff\xfe\xc0\xaf\"}"},
		{"compact whitespace", " \r\n { \"x\" : [ true , null , \" a  b \\n\\t\" ] }\t", `{"x":[true,null," a  b \n\t"]}`},
		{"string", `"<text>&\u2028"`, `"<text>&\u2028"`},
		{"null", `null`, `null`},
		{"boolean", `true`, `true`},
		{"empty object", `{}`, `{}`},
		{"empty array", `[]`, `[]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &sessiondata.Record{Ord: 1, Parts: []sessiondata.Part{{Kind: sessiondata.PartData, Data: json.RawMessage(tc.data)}}}
			file := writeRecords(t, rec)
			line := bytes.Split(file, []byte{'\n'})[1]
			want := `{"ord":1,"off":0,"sha":"","bytes":0,"parts":[{"k":"data","data":` + tc.want + `}]}`
			if !bytes.Equal(line, []byte(want)) {
				t.Fatalf("record bytes changed:\n got %q\nwant %q", line, want)
			}
			got := readRecords(t, file)
			if len(got) != 1 || !bytes.Equal(got[0].Parts[0].Data, []byte(tc.want)) {
				t.Fatalf("reader changed data: %+v", got)
			}
		})
	}
}

func TestWriterDataSlotsDoNotMatchStrings(t *testing.T) {
	slot := `"data":null`
	rec := &sessiondata.Record{
		Ord: 1, Label: slot, Flags: []string{slot},
		Parts: []sessiondata.Part{
			{Kind: sessiondata.PartText, Text: slot},
			{Kind: sessiondata.PartData, Data: json.RawMessage{}},
			{Kind: sessiondata.PartData, Text: slot, Data: json.RawMessage(`null`)},
			{Kind: sessiondata.PartData, Data: json.RawMessage(`{"data":null,"text":"\"data\":null"}`)},
			{Kind: sessiondata.PartCall, ID: slot, Name: slot, Data: json.RawMessage(`"\"data\":null"`)},
			{Kind: sessiondata.PartData, Data: json.RawMessage(`{"final":"<>&"}`)},
		},
		Dropped: []sessiondata.Drop{{What: slot, Why: slot}},
	}
	file := writeRecords(t, rec)
	got := readRecords(t, file)[0]
	// JSON omits both nil and empty data. Every nonempty data value, including
	// null, remains present and in its original part.
	want := *rec
	want.Parts = append([]sessiondata.Part(nil), rec.Parts...)
	want.Parts[1].Data = nil
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("parts or surrounding fields changed:\n got %+v\nwant %+v", *got, want)
	}
	line := bytes.Split(file, []byte{'\n'})[1]
	var shape struct {
		Parts []map[string]json.RawMessage `json:"parts"`
	}
	if err := json.Unmarshal(line, &shape); err != nil {
		t.Fatal(err)
	}
	for i, part := range shape.Parts {
		_, present := part["data"]
		if present != (i >= 2) {
			t.Errorf("part %d data present=%v", i, present)
		}
	}
}

func TestWriterLeavesCallerDataUnchanged(t *testing.T) {
	backing := []byte(" { \"x\" : \"<>&\" } trailing storage")
	original := bytes.Clone(backing)
	data := backing[:len(` { "x" : "<>&" }`)]
	rec := &sessiondata.Record{Ord: 1, Parts: []sessiondata.Part{
		{Kind: sessiondata.PartData, Data: data},
		{Kind: sessiondata.PartData, Data: json.RawMessage{}},
		{Kind: sessiondata.PartText, Text: "text"},
	}}
	parts := append([]sessiondata.Part(nil), rec.Parts...)
	_ = writeRecords(t, rec)
	if !bytes.Equal(backing, original) || !reflect.DeepEqual(rec.Parts, parts) {
		t.Fatalf("writer changed the caller's parts or their backing storage: %+v", rec.Parts)
	}
}

func TestWriterRejectsInvalidDataWithoutWritingARecord(t *testing.T) {
	cases := []struct {
		name string
		data string
	}{
		{"whitespace only", " \t\r\n"},
		{"truncated", `{"x":`},
		{"two values", `{} {}`},
		{"invalid escape", `"\x"`},
		{"raw tab", "\"a\tb\""},
		{"raw nul", "\"a\x00b\""},
		{"byte order mark", "\ufeff{}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var file bytes.Buffer
			w, err := sessiondata.NewWriter(&file, header())
			if err != nil {
				t.Fatal(err)
			}
			first := &sessiondata.Record{Ord: 1, Parts: []sessiondata.Part{{Kind: sessiondata.PartText, Text: "before"}}}
			if err := w.Write(first); err != nil {
				t.Fatal(err)
			}
			bad := &sessiondata.Record{Ord: 2, Parts: []sessiondata.Part{
				{Kind: sessiondata.PartData, Data: json.RawMessage(` { "valid" : "<>&" } `)},
				{Kind: sessiondata.PartData, Data: json.RawMessage(tc.data)},
			}}
			before := append([]sessiondata.Part(nil), bad.Parts...)
			err = w.Write(bad)
			if err == nil || !strings.Contains(err.Error(), "encode record 2: part 1:") {
				t.Fatalf("expected record and part context, got %v", err)
			}
			if !reflect.DeepEqual(bad.Parts, before) {
				t.Fatal("failed encoding changed the caller's parts")
			}
			if w.Count() != 1 {
				t.Fatalf("failed record was counted: %d", w.Count())
			}
			last := &sessiondata.Record{Ord: 3, Parts: []sessiondata.Part{{Kind: sessiondata.PartData, Data: json.RawMessage(`{"after":true}`)}}}
			if err := w.Write(last); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			want := writeRecords(t, first, last)
			if !bytes.Equal(file.Bytes(), want) {
				t.Fatal("failed encoding changed the file or closing digest")
			}
			if got := readRecords(t, file.Bytes()); len(got) != 2 {
				t.Fatalf("read %d records, want 2", len(got))
			}
		})
	}
}

// No landed line holds more than MaxLineDepth objects and lists open at
// once, since a reader may refuse it and the OAP does. Data is inside three
// of them, the record, its parts and the part, so data nested deeper than
// MaxDataDepth is taken out as a payload too large to carry is: the part
// keeps its place and kind, says too_large, or truncated while its text is
// left, and keeps the size and the SHA-256 of what it held. Data at the
// limit lands as it is, and so does data past Go's own decoder's limit, taken
// out the same way.
func TestDataNestedTooDeepForALineIsTakenOut(t *testing.T) {
	nested := func(depth int) json.RawMessage {
		return json.RawMessage(strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth))
	}
	for _, depth := range []int{sessiondata.MaxDataDepth, sessiondata.MaxDataDepth + 1, 10001} {
		t.Run(fmt.Sprint(depth), func(t *testing.T) {
			data := nested(depth)
			var file bytes.Buffer
			w, err := sessiondata.NewWriter(&file, header())
			if err != nil {
				t.Fatal(err)
			}
			rec := &sessiondata.Record{Ord: 1, Parts: []sessiondata.Part{
				{Kind: sessiondata.PartData, Data: data},
				{Kind: sessiondata.PartResult, Of: "toolu_1", Text: "readable", Data: data},
			}}
			if err := w.Write(rec); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			for _, line := range bytes.Split(bytes.TrimSpace(file.Bytes()), []byte("\n")) {
				if d := sessiondata.Depth(line); d > sessiondata.MaxLineDepth {
					t.Fatalf("a line holds %d levels, past %d", d, sessiondata.MaxLineDepth)
				}
			}
			got := readRecords(t, file.Bytes())
			if len(got) != 1 {
				t.Fatalf("read %d records, want 1", len(got))
			}
			whole, result := got[0].Parts[0], got[0].Parts[1]
			if depth <= sessiondata.MaxDataDepth {
				if !bytes.Equal(whole.Data, data) || whole.State != "" || !bytes.Equal(result.Data, data) {
					t.Fatalf("data at the limit did not land as it was: %+v", whole)
				}
				return
			}
			sum := sha256.Sum256(data)
			if len(whole.Data) != 0 || whole.State != model.ContentTooLarge || whole.Bytes != len(data) ||
				whole.SHA256 != hex.EncodeToString(sum[:]) || whole.Kind != sessiondata.PartData {
				t.Fatalf("the deep data part is %+v", whole)
			}
			if len(result.Data) != 0 || result.State != model.ContentTruncated || result.Text != "readable" || result.Of != "toolu_1" {
				t.Fatalf("the deep result part is %+v", result)
			}
			if len(rec.Parts[0].Data) != 0 || rec.Parts[0].State != model.ContentTooLarge {
				t.Fatal("the caller's record does not say what landed")
			}
		})
	}
}

// A record that cannot be written is left as the caller gave it, though one
// of its parts was too deep and another is not JSON.
func TestARecordThatCannotBeWrittenIsNotLeveled(t *testing.T) {
	deep := json.RawMessage(strings.Repeat("[", 300) + "0" + strings.Repeat("]", 300))
	rec := &sessiondata.Record{Ord: 1, Parts: []sessiondata.Part{
		{Kind: sessiondata.PartData, Data: deep},
		{Kind: sessiondata.PartData, Data: json.RawMessage(`{"x":`)},
	}}
	before := append([]sessiondata.Part(nil), rec.Parts...)
	var file bytes.Buffer
	w, err := sessiondata.NewWriter(&file, header())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(rec); err == nil {
		t.Fatal("a record with data that is not JSON was written")
	}
	if !reflect.DeepEqual(rec.Parts, before) || w.Count() != 0 {
		t.Fatal("a record that was not written was changed, or counted")
	}
}

func TestWriterDoesNotHTMLEscapeItsStringFields(t *testing.T) {
	var file bytes.Buffer
	h := header()
	h.Src = "<source>&.jsonl"
	h.Session = "<session>&"
	w, err := sessiondata.NewWriter(&file, h)
	if err != nil {
		t.Fatal(err)
	}
	rec := &sessiondata.Record{Ord: 1, Label: "<label>&", Parts: []sessiondata.Part{{Kind: sessiondata.PartText, Text: "<text>&\nnext\tline\u2028end\u2029"}}}
	if err := w.Write(rec); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	for _, escaped := range []string{`\u003c`, `\u003e`, `\u0026`} {
		if bytes.Contains(file.Bytes(), []byte(escaped)) {
			t.Errorf("writer added HTML escape %s", escaped)
		}
	}
	if bytes.Count(file.Bytes(), []byte{'\n'}) != 3 {
		t.Fatal("string content created an extra JSON line")
	}
	r, err := sessiondata.NewReader(bytes.NewReader(file.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if r.Header().Src != h.Src || r.Header().Session != h.Session {
		t.Fatalf("header changed: %+v", r.Header())
	}
	got := readRecords(t, file.Bytes())[0]
	if !reflect.DeepEqual(*got, *rec) {
		t.Fatalf("string fields changed: %+v", got)
	}
}

func TestWriterInvalidUTF8StringUsesGoReplacement(t *testing.T) {
	rec := &sessiondata.Record{Ord: 1, Label: "\xff", Parts: []sessiondata.Part{{Kind: sessiondata.PartData, Data: json.RawMessage("\"\xff\"")}}}
	file := writeRecords(t, rec)
	line := bytes.Split(file, []byte{'\n'})[1]
	// Go's default engine writes the replacement character literally. The
	// older engine escapes it. This known difference applies to strings,
	// while data keeps its original bytes in both builds.
	if !bytes.Contains(line, []byte(`"label":"�"`)) && !bytes.Contains(line, []byte(`"label":"\ufffd"`)) {
		t.Fatalf("unexpected invalid UTF-8 string encoding: %q", line)
	}
	got := readRecords(t, file)[0]
	if got.Label != "�" || !bytes.Equal(got.Parts[0].Data, rec.Parts[0].Data) {
		t.Fatalf("unexpected replacement or data change: %+v", got)
	}
}

func writeRecords(t *testing.T, records ...*sessiondata.Record) []byte {
	t.Helper()
	var file bytes.Buffer
	w, err := sessiondata.NewWriter(&file, header())
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range records {
		if err := w.Write(rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return file.Bytes()
}

func readRecords(t *testing.T, file []byte) []*sessiondata.Record {
	t.Helper()
	r, err := sessiondata.NewReader(bytes.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	var records []*sessiondata.Record
	for {
		rec, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, rec)
	}
	if r.End() == nil || r.End().Records != len(records) {
		t.Fatalf("closing record count was not verified: %+v", r.End())
	}
	return records
}
