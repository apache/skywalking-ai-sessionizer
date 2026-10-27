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
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/pull/format"
)

// A listing reads back as a server writes it, and an answer that breaks the
// schema is refused as an invalid answer.
func TestAListingReadsAsWrittenAndNothingElse(t *testing.T) {
	seven := uint64(7)
	l := format.Listing{Schema: format.Schema, Rows: []format.Row{
		{ID: "c1", Session: "s1", Runtime: "claude-code", Title: "Build it", Round: 3, Digest: digest, Talks: 2, Steps: 9,
			Streams: 1, Segments: 1, SessionFromTime: "2026-10-01T00:00:00Z", SessionThroughTime: "2026-10-01T01:00:00.5Z", Changes: &seven},
		{ID: "c2", Session: "s2", Round: 1, Digest: digest},
	}, Skipped: []format.Skipped{{ID: `bad\xff`, Reason: "its head does not read"}}, Next: "abc"}
	b, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	got, err := format.ReadListing(strings.NewReader(string(b)))
	if err != nil || !reflect.DeepEqual(*got, l) {
		t.Fatalf("%+v %v", got, err)
	}
	empty, err := format.ReadListing(strings.NewReader(`{"schema":"pull/1","rows":[],"skipped":[],"later":{"x":[1]}}`))
	if err != nil || len(empty.Rows) != 0 || empty.Next != "" || empty.Skipped == nil {
		t.Fatalf("an empty page: %+v %v", empty, err)
	}
	row := `{"id":"c1","session":"s1","runtime":"","title":"","round":1,"digest":"` + digest + `","talks":0,"steps":0,"streams":0,"segments":0,"unresolved":0}`
	for name, text := range map[string]string{
		"no skipped":           `{"schema":"pull/1","rows":[]}`,
		"another schema":       `{"schema":"pull/2","rows":[],"skipped":[]}`,
		"a value after":        `{"schema":"pull/1","rows":[],"skipped":[]} {}`,
		"a null next":          `{"schema":"pull/1","rows":[],"skipped":[],"next":null}`,
		"an empty next":        `{"schema":"pull/1","rows":[],"skipped":[],"next":""}`,
		"a row not an object":  `{"schema":"pull/1","rows":[1],"skipped":[]}`,
		"a row without talks":  `{"schema":"pull/1","rows":[` + strings.Replace(row, `"talks":0,`, "", 1) + `],"skipped":[]}`,
		"an unsafe id":         `{"schema":"pull/1","rows":[` + strings.Replace(row, `"c1"`, `"../c1"`, 1) + `],"skipped":[]}`,
		"round 0":              `{"schema":"pull/1","rows":[` + strings.Replace(row, `"round":1`, `"round":0`, 1) + `],"skipped":[]}`,
		"a negative count":     `{"schema":"pull/1","rows":[` + strings.Replace(row, `"steps":0`, `"steps":-1`, 1) + `],"skipped":[]}`,
		"a bad time":           `{"schema":"pull/1","rows":[` + strings.Replace(row, `"unresolved":0`, `"unresolved":0,"session_through_time":"yesterday"`, 1) + `],"skipped":[]}`,
		"a null count":         `{"schema":"pull/1","rows":[` + strings.Replace(row, `"unresolved":0`, `"unresolved":0,"changes":null`, 1) + `],"skipped":[]}`,
		"a repeated member":    `{"schema":"pull/1","rows":[` + strings.Replace(row, `"talks":0`, `"talks":0,"talks":1`, 1) + `],"skipped":[]}`,
		"an empty reason":      `{"schema":"pull/1","rows":[],"skipped":[{"id":"x","reason":""}]}`,
		"a skipped without id": `{"schema":"pull/1","rows":[],"skipped":[{"reason":"x"}]}`,
	} {
		if _, err := format.ReadListing(strings.NewReader(text)); !errors.Is(err, format.ErrAnswer) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A pending manifest is the conversation asked for, building, under a
// freeze identifier.
func TestAPendingManifestNamesWhatWasAskedFor(t *testing.T) {
	ok := `{"schema":"pull/1","conversation":"c1","freeze":"` + digest + `","state":"building","more":1}`
	p, err := format.ReadManifestPending(strings.NewReader(ok), "c1")
	if err != nil || p.Freeze != digest {
		t.Fatalf("%+v %v", p, err)
	}
	for name, text := range map[string]string{
		"another conversation": strings.Replace(ok, `"c1"`, `"c2"`, 1),
		"ready":                strings.Replace(ok, "building", "ready", 1),
		"a short freeze":       strings.Replace(ok, digest, "ab", 1),
		"no state":             strings.Replace(ok, `,"state":"building"`, "", 1),
	} {
		if _, err := format.ReadManifestPending(strings.NewReader(text), "c1"); !errors.Is(err, format.ErrAnswer) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// An archive's state names the snapshot and archive asked for; a ready one
// carries its size and byte digest.
func TestAnArchiveStateNamesWhatWasAskedFor(t *testing.T) {
	const tag = "20261001T000000.000000001Z"
	ready := `{"schema":"pull/1","snapshot":"` + tag + `","archive":"` + digest + `","state":"ready","bytes":42,"digest":"` + lease + `"}`
	st, err := format.ReadArchiveState(strings.NewReader(ready), tag, digest, true)
	if err != nil || st.Bytes != 42 || st.Digest != lease {
		t.Fatalf("%+v %v", st, err)
	}
	building := `{"schema":"pull/1","snapshot":"` + tag + `","archive":"` + digest + `","state":"building"}`
	if _, err := format.ReadArchiveState(strings.NewReader(building), tag, digest, false); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		text  string
		ready bool
	}{
		"another snapshot":  {strings.Replace(ready, tag, "20261001T000000.000000002Z", 1), true},
		"another archive":   {strings.Replace(ready, `"archive":"`+digest, `"archive":"`+lease, 1), true},
		"empty":             {strings.Replace(ready, `"bytes":42`, `"bytes":0`, 1), true},
		"past a file":       {strings.Replace(ready, `"bytes":42`, `"bytes":9223372036854775808`, 1), true},
		"no digest":         {strings.Replace(ready, `,"digest":"`+lease+`"`, "", 1), true},
		"building as ready": {building, true},
		"ready as building": {ready, false},
	} {
		if _, err := format.ReadArchiveState(strings.NewReader(tc.text), tag, digest, tc.ready); !errors.Is(err, format.ErrAnswer) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A problem's status is the status it came with, and a named type comes
// with its own one.
func TestAProblemIsReadAgainstItsStatus(t *testing.T) {
	text := `{"type":"` + format.ProblemArchiveFailed + `","title":"x","detail":"y","status":500,"snapshot":"s","archive":"a","attempt":2,"extra":[]}`
	p, err := format.ReadProblem(strings.NewReader(text), 500)
	if err != nil || p.Type != format.ProblemArchiveFailed || p.Attempt != 2 || p.Archive != "a" {
		t.Fatalf("%+v %v", p, err)
	}
	for name, tc := range map[string]struct {
		text   string
		status int
	}{
		"another status":        {text, 503},
		"under 400":             {strings.Replace(text, "500", "200", 1), 200},
		"attempt 0":             {strings.Replace(text, `"attempt":2`, `"attempt":0`, 1), 500},
		"no detail":             {strings.Replace(text, `"detail":"y",`, "", 1), 500},
		"a value after":         {text + "1", 500},
		"a status string":       {strings.Replace(text, `"status":500`, `"status":"500"`, 1), 500},
		"a failed build as 412": {strings.Replace(text, `"status":500`, `"status":412`, 1), 412},
		"a failed build as 410": {strings.Replace(text, `"status":500`, `"status":410`, 1), 410},
	} {
		if _, err := format.ReadProblem(strings.NewReader(tc.text), tc.status); !errors.Is(err, format.ErrAnswer) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
