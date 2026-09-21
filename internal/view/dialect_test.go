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
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// TestTheConversationsDialectWinsOverASidecars.
//
// A session holds more than its conversation. The plugin's file changes land
// beside it in a dialect of their own, and they can land first - a change is
// captured as a tool runs, and the runtime sends the conversation afterwards.
// Whichever file came first used to decide the whole session, so the page
// offered the changes vocabulary and none of the runtime's words at all.
func TestTheConversationsDialectWinsOverASidecars(t *testing.T) {
	zone := storage.NewZone(t.TempDir())
	const session = "s1"
	// Sequence 1 is the sidecar, sequence 2 the conversation: the order that
	// used to give the wrong answer.
	landHeader(t, zone, session, "main", 1, sessiondata.KindChanges, "changes/1")
	landHeader(t, zone, session, "main", 2, sessiondata.KindTranscript, "runtime/1")

	if got := dialectOf(zone, session); got != "runtime/1" {
		t.Errorf("the session reads as %q, want the conversation's runtime/1", got)
	}
}

// TestASidecarIsStillAnAnswerWhenItIsAllThereIs: a session that has only its
// changes so far still says which vocabulary it was read in, because the
// alternative is the page offering none.
func TestASidecarIsStillAnAnswerWhenItIsAllThereIs(t *testing.T) {
	zone := storage.NewZone(t.TempDir())
	const session = "s2"
	landHeader(t, zone, session, "main", 1, sessiondata.KindChanges, "changes/1")
	if got := dialectOf(zone, session); got != "changes/1" {
		t.Errorf("the session reads as %q, want changes/1", got)
	}
}

// landHeader writes a landed file with one record, for its header alone.
func landHeader(t *testing.T, z *storage.Zone, session, stream string, seq uint64,
	kind sessiondata.Kind, dialect string) {
	t.Helper()
	dir := z.StreamDir(session, stream)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := storage.LandedName(string(kind), storage.Stamp(time.Unix(int64(seq), 0).UTC()), seq)
	path := filepath.Join(dir, name)
	header := &sessiondata.Header{Seq: seq, At: time.Unix(int64(seq), 0).UTC().Format(time.RFC3339Nano),
		Kind: kind, Adapter: "test/1", Dialect: dialect, Src: "test", Session: session, Stream: stream}
	err := storage.WriteExclusive(path, storage.PermLanded, func(w io.Writer) error {
		writer, err := sessiondata.NewWriter(w, header)
		if err != nil {
			return err
		}
		record := &sessiondata.Record{Ord: 1, Off: 0, Sha: "0", Bytes: 1,
			ID: string(kind), Time: "2026-09-20T10:00:00Z"}
		if err := writer.Write(record); err != nil {
			return err
		}
		return writer.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
}
