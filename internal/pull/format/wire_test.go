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

package format_test

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/pull/format"
)

var lease = strings.Repeat("ab", 32)

// A cursor reads back as it was written, and nothing else reads as one: not
// another spelling of its base64, not another shape of its object.
func TestACursorReadsOnlyAsItWasWritten(t *testing.T) {
	c := format.Cursor{Filters: format.Filters{Match: "x", Limit: 50}, Since: "2026-10-01T00:00:00Z", Time: "2026-10-02T00:00:00Z", ID: "c1"}
	text, err := format.EncodeCursor(c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := format.DecodeCursor(text)
	c.V = 1
	if err != nil || got != c {
		t.Fatalf("%+v %v", got, err)
	}
	object := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	filters := `{"since":"","until":"","runtime":"","match":"","min_talks":0,"limit":50}`
	for name, text := range map[string]string{
		"padding":          text + "==",
		"standard base64":  strings.ReplaceAll(strings.ReplaceAll(text, "-", "+"), "_", "/") + "+",
		"not base64":       "!!",
		"too long":         object(`{"v":1,"filters":` + filters + `,"since":"","until":"","time":"","id":"` + strings.Repeat("x", 5000) + `"}`),
		"another version":  object(`{"v":2,"filters":` + filters + `,"since":"","until":"","time":"","id":"c1"}`),
		"a missing member": object(`{"v":1,"filters":` + filters + `,"since":"","until":"","time":""}`),
		"an extra member":  object(`{"v":1,"filters":` + filters + `,"since":"","until":"","time":"","id":"c1","x":1}`),
		"a null member":    object(`{"v":1,"filters":` + filters + `,"since":null,"until":"","time":"","id":"c1"}`),
		"an escaped twin":  object(`{"v":1,"filters":` + filters + `,"since":"","\u0073ince":"","until":"","time":"","id":"c1"}`),
		"a filter missing": object(`{"v":1,"filters":{"since":"","until":"","runtime":"","match":"","limit":50},"since":"","until":"","time":"","id":"c1"}`),
		"a string count":   object(`{"v":1,"filters":{"since":"","until":"","runtime":"","match":"","min_talks":"0","limit":50},"since":"","until":"","time":"","id":"c1"}`),
		"a value after":    object(`{"v":1,"filters":` + filters + `,"since":"","until":"","time":"","id":"c1"} 1`),
		"an unsafe id":     object(`{"v":1,"filters":` + filters + `,"since":"","until":"","time":"","id":"../x"}`),
		"an empty id":      object(`{"v":1,"filters":` + filters + `,"since":"","until":"","time":"","id":""}`),
	} {
		if _, err := format.DecodeCursor(text); !errors.Is(err, format.ErrRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// An archive request is one object with a lease and the names wanted, each
// safe and once, and a positive restart when it restarts; nothing else.
func TestAnArchiveRequestIsStrict(t *testing.T) {
	a, err := format.ReadArchiveRequest(strings.NewReader(`{"lease":"`+lease+`","files":["a/b","c"],"restart":2}`), 5)
	if err != nil || a.Lease != lease || len(a.Files) != 2 || a.Restart != 2 {
		t.Fatalf("%+v %v", a, err)
	}
	for name, body := range map[string]string{
		"no lease":         `{"files":["a"]}`,
		"no files":         `{"lease":"` + lease + `"}`,
		"empty files":      `{"lease":"` + lease + `","files":[]}`,
		"a null":           `{"lease":"` + lease + `","files":["a"],"restart":null}`,
		"restart zero":     `{"lease":"` + lease + `","files":["a"],"restart":0}`,
		"an unknown":       `{"lease":"` + lease + `","files":["a"],"x":1}`,
		"an escaped twin":  `{"lease":"` + lease + `","files":["a"],"le\u0061se":"` + lease + `"}`,
		"a name twice":     `{"lease":"` + lease + `","files":["a","a"]}`,
		"an unsafe name":   `{"lease":"` + lease + `","files":["../a"]}`,
		"too many":         `{"lease":"` + lease + `","files":["a","b","c","d","e","f"]}`,
		"a short lease":    `{"lease":"ab","files":["a"]}`,
		"a value after":    `{"lease":"` + lease + `","files":["a"]}{}`,
		"not an object":    `["a"]`,
		"a bad surrogate":  `{"lease":"` + lease + `","files":["\ud800"]}`,
		"a number as name": `{"lease":"` + lease + `","files":[1]}`,
	} {
		if _, err := format.ReadArchiveRequest(strings.NewReader(body), 5); !errors.Is(err, format.ErrRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if got, err := format.ReadDoneRequest(strings.NewReader(`{"lease":"` + lease + `"}`)); err != nil || got != lease {
		t.Fatalf("done: %q %v", got, err)
	}
	for _, body := range []string{`{}`, `{"lease":"` + lease + `","files":["a"]}`, `{"lease":null}`} {
		if _, err := format.ReadDoneRequest(strings.NewReader(body)); !errors.Is(err, format.ErrRequest) {
			t.Errorf("done %s: %v", body, err)
		}
	}
}

// A range is one closed range of at most eight mebibytes, written plainly;
// If-Match is one quoted strong archive digest.
func TestRangeAndIfMatchAreStrict(t *testing.T) {
	if s, e, err := format.ParseRange([]string{"bytes=0-8388607"}); err != nil || s != 0 || e != 8388607 {
		t.Fatalf("%d %d %v", s, e, err)
	}
	for _, v := range [][]string{nil, {"bytes=0-1", "bytes=2-3"}, {"bytes=0-8388608"}, {"bytes=5-4"}, {"bytes=5-"},
		{"bytes=-5"}, {"bytes=0-1,2-3"}, {"bytes=01-2"}, {"bytes= 0-1"}, {"items=0-1"}, {"bytes=0-18446744073709551616"}} {
		if _, _, err := format.ParseRange(v); !errors.Is(err, format.ErrRequest) {
			t.Errorf("%q: %v", v, err)
		}
	}
	digest := strings.Repeat("0f", 32)
	if got, err := format.ParseIfMatch([]string{`"` + digest + `"`}); err != nil || got != digest {
		t.Fatalf("%q %v", got, err)
	}
	for _, v := range [][]string{nil, {"*"}, {`W/"` + digest + `"`}, {digest}, {`"` + digest + `", "` + digest + `"`},
		{`"` + digest + `"`, `"` + digest + `"`}, {`"` + strings.ToUpper(digest) + `"`}} {
		if _, err := format.ParseIfMatch(v); !errors.Is(err, format.ErrRequest) {
			t.Errorf("%q: %v", v, err)
		}
	}
}
