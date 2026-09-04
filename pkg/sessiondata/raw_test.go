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
	"errors"
	"io"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

func header() *sessiondata.Header {
	return &sessiondata.Header{
		Seq: 7, At: "2026-09-04T00:00:00Z", Kind: sessiondata.KindTranscript,
		Adapter: "test/0", Dialect: "test/1", Src: "s.jsonl", Session: "s", Stream: "main",
	}
}

// A file read as raw lines and written back as raw lines is the same file:
// same records, same bytes, same closing digest. This is what lets a record
// travel or be repacked without ever being re-encoded.
func TestRawRoundTripKeepsBytes(t *testing.T) {
	var a bytes.Buffer
	w, err := sessiondata.NewWriter(&a, header())
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	for i, text := range []string{"one", "two é 中", "three"} {
		p := sessiondata.Part{Kind: sessiondata.PartText, Text: text, State: "available", Bytes: len(text)}
		if i == 1 {
			p = sessiondata.Part{Kind: sessiondata.PartResult, Of: "toolu_1", Failed: &yes, Text: text, State: "available", Bytes: len(text)}
		}
		if err := w.Write(&sessiondata.Record{Ord: uint64(i + 1), Off: uint64(i * 10), Sha: "abc", Bytes: 10, Parts: []sessiondata.Part{p}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := sessiondata.NewReader(bytes.NewReader(a.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	hdr := r.Header()
	w2, err := sessiondata.NewWriter(&b, &hdr)
	if err != nil {
		t.Fatal(err)
	}
	var lines int
	for {
		line, err := r.NextRaw()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := w2.WriteRaw(line); err != nil {
			t.Fatal(err)
		}
		lines++
	}
	if err := w2.Close(); err != nil {
		t.Fatal(err)
	}
	if lines != 3 {
		t.Fatalf("read %d raw lines, want 3", lines)
	}
	if r.End() == nil || r.End().Records != 3 {
		t.Fatalf("closing line not verified: %+v", r.End())
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatalf("raw round trip changed the bytes:\n%s\n---\n%s", a.String(), b.String())
	}
}

// A raw read still rejects a file cut short or tampered with.
func TestRawReadVerifiesTheClosingLine(t *testing.T) {
	var a bytes.Buffer
	w, err := sessiondata.NewWriter(&a, header())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(&sessiondata.Record{Ord: 1, Sha: "abc", Bytes: 1}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Replace(a.Bytes(), []byte(`"ord":1`), []byte(`"ord":2`), 1)
	r, err := sessiondata.NewReader(bytes.NewReader(tampered))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.NextRaw(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.NextRaw(); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("a changed record must fail the digest at the closing line, got %v", err)
	}
}

// The time comes from the record's own field, never from one nested in a
// part, and a record without a time says so.
func TestLineTimeReadsTheRecordsOwnTime(t *testing.T) {
	line := []byte(`{"ord":1,"time":"2026-09-04T01:02:03.5Z","parts":[{"k":"data","data":{"time":"1999-01-01T00:00:00Z"}}]}`)
	ns, ok := sessiondata.LineTime(line)
	if !ok || ns != time.Date(2026, 9, 4, 1, 2, 3, 500000000, time.UTC).UnixNano() {
		t.Fatalf("got %d ok=%v", ns, ok)
	}
	nested := []byte(`{"ord":2,"parts":[{"k":"data","data":{"time":"1999-01-01T00:00:00Z"}}]}`)
	if _, ok := sessiondata.LineTime(nested); ok {
		t.Fatal("a time inside a part must not count as the record's time")
	}
}
