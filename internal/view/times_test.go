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

package view

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// TestRecordTimesEndWhereTheReaderStops. A landed file holds two records
// before 1970, a line whose off is a string, and a record after it. Session
// Data says a reader stops at a line it cannot decode and reads no record
// after it, so only the first two have times. A time before 1970 is below
// the 0 that means none, so it still counts as the file's last time and a
// node's last time.
func TestRecordTimesEndWhereTheReaderStops(t *testing.T) {
	const session = "b7f4a0c2-6f36-4c62-9d07-1d5b1f6f0a11"
	z := storage.NewZone(t.TempDir())
	dir := filepath.Join(z.SessionDir(session), "streams", "main")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "transcript-20260101T000000.000000000Z-000001.sd")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w, err := sessiondata.NewWriter(f, &sessiondata.Header{Seq: 1, At: "2026-01-01T00:00:00Z", Kind: sessiondata.KindTranscript,
		Adapter: "test/1", Dialect: "test", Src: "main.jsonl", Session: session, Stream: "main"})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		`{"ord":1,"off":0,"sha":"a","bytes":1,"time":"1969-12-31T23:59:58Z","parts":[]}`,
		`{"ord":2,"off":1,"sha":"b","bytes":1,"time":"1969-12-31T23:59:59Z","parts":[]}`,
		`{"ord":3,"off":"bad","sha":"c","bytes":1,"time":"2026-01-01T00:00:03Z","parts":[]}`,
		`{"ord":4,"off":3,"sha":"d","bytes":1,"time":"2026-01-01T00:00:04Z","parts":[]}`,
	} {
		if err := w.WriteRaw([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	at, lanes := map[[2]uint64]int64{}, map[uint64]string{}
	if err := timesOf(z, session, at, lanes); err != nil {
		t.Fatal(err)
	}
	if len(at) != 2 {
		t.Fatalf("%d records have times, want 2: none after the line that does not decode", len(at))
	}
	landed, err := storage.LandedFiles(z, session)
	if err != nil {
		t.Fatal(err)
	}
	c := &Conversation{at: at, zone: z}
	files, _, err := c.files(landed)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].FromTime == nil || files[0].ThroughTime == nil ||
		*files[0].FromTime != -2000 || *files[0].ThroughTime != -1000 {
		t.Fatalf("the file's times are %+v, want -2000 through -1000", files)
	}
	n := &sessionflow.Node{Entity: sessionflow.Entity{ID: "input/1/2"}, Ref: &sessionflow.Ref{Seq: 1, Row: 2}}
	c.View = &sessionflow.View{Nodes: map[string]*sessionflow.Node{n.ID: n}}
	if lo, hi := c.Span(n); lo != -1000*1e6 || hi != -1000*1e6 {
		t.Errorf("the node's span is %d through %d, want -1s through -1s", lo, hi)
	}
}
