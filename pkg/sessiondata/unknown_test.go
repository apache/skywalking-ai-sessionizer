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
	"encoding/json"
	"testing"
	"unicode/utf8"

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// The bytes an unknown part keeps come back exactly after a landed file is
// written and read, whether or not they are valid UTF-8, and only bytes that
// are not valid UTF-8 change form.
func TestUnknownPartBytesSurviveALandedFile(t *testing.T) {
	for _, b := range [][]byte{
		[]byte("plain <text> & more, café 中"),
		{0xff, 'x'},
		[]byte("a write cut short: caf\xc3"),
		[]byte("\xef\xbf\xbd is U+FFFD itself, and valid"),
		{},
	} {
		p := sessiondata.Part{Kind: sessiondata.PartUnknown, Text: "why", State: "available", Bytes: len(b)}
		p.SetRaw(b)
		var buf bytes.Buffer
		w, err := sessiondata.NewWriter(&buf, header())
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Write(&sessiondata.Record{Ord: 1, Sha: "abc", Bytes: len(b), Parts: []sessiondata.Part{p}}); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		_, recs, err := sessiondata.All(&buf)
		if err != nil {
			t.Fatal(err)
		}
		got, err := recs[0].Parts[0].Raw()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, b) {
			t.Errorf("kept %x, gave %x", got, b)
		}
		if marked := recs[0].Parts[0].Encoding == sessiondata.EncodingBase64; marked == utf8.Valid(b) {
			t.Errorf("%x: encoding %q", b, recs[0].Parts[0].Encoding)
		}
	}
}

// A part landed before the encoding existed has none, and its string is its
// bytes. Such a file must read as it always did.
func TestUnknownPartLandedBeforeTheEncodingReadsAsItDid(t *testing.T) {
	var p sessiondata.Part
	if err := json.Unmarshal([]byte(`{"k":"unknown","text":"the record is not valid JSON","data":"a < b & c","state":"available","bytes":9}`), &p); err != nil {
		t.Fatal(err)
	}
	got, err := p.Raw()
	if err != nil || string(got) != "a < b & c" {
		t.Fatalf("read %q, %v", got, err)
	}
	p.Encoding = "rot13"
	if _, err := p.Raw(); err == nil {
		t.Error("an encoding the format does not name was read as if it did")
	}
}
