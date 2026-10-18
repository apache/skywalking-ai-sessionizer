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

package otlp_test

import (
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// zoneOfThree lands three files in one stream of one session, in sequence
// order, and returns their paths relative to the root. The file at bad holds
// a byte that is not valid UTF-8, written raw, as the writer never lands it:
// a writer outside asz could.
func zoneOfThree(t *testing.T, bad int) (*storage.Zone, []string) {
	t.Helper()
	z := storage.NewZone(t.TempDir())
	dir := z.StreamDir("sess1", "main")
	var rels []string
	for seq := 1; seq <= 3; seq++ {
		path := filepath.Join(dir, storage.LandedName("transcript", storage.Stamp(time.Unix(0, 0)), uint64(seq)))
		err := storage.WriteAtomic(path, storage.PermLanded, func(w io.Writer) error {
			hdr := &sessiondata.Header{Seq: uint64(seq), At: "2026-09-04T00:00:00Z", Kind: sessiondata.KindTranscript,
				Adapter: "test/0", Dialect: "test/1", Src: "sess1.jsonl", Session: "sess1", Stream: "main"}
			sw, err := sessiondata.NewWriter(w, hdr)
			if err != nil {
				return err
			}
			text := fmt.Sprintf("record %d", seq)
			if seq == bad {
				text = "record \xff"
			}
			line := fmt.Sprintf(`{"ord":%d,"off":0,"sha":"a","bytes":1,"time":"2026-09-04T01:00:00Z","parts":[{"k":"text","text":"%s"}]}`, seq, text)
			if err := sw.WriteRaw([]byte(line)); err != nil {
				return err
			}
			return sw.Close()
		})
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(z.Root(), path)
		rels = append(rels, filepath.ToSlash(rel))
	}
	return z, rels
}

// A file that cannot be encoded as a log record, since a protocol buffer
// string must be valid UTF-8, fails every request it is in. It is reported on
// its own, under its name, on every pass, and stays out of the request: the
// files beside it go, whether they share one request or each has its own.
func TestAFileThatCannotBeSentIsReportedOnItsOwn(t *testing.T) {
	for _, protocol := range protocols {
		for _, batch := range []int64{0, 1} {
			t.Run(fmt.Sprintf("%s/batch %d", protocol, batch), func(t *testing.T) {
				z, rels := zoneOfThree(t, 2)
				rcv := startReceiver(t)
				p := &otlp.Pusher{Zone: z, Client: clientFor(t, rcv, protocol), Version: "test", ServiceName: "Claude Code",
					Endpoint: protocol, BatchBytes: batch}
				for pass := 1; pass <= 2; pass++ {
					st, err := p.Pass()
					if err != nil {
						t.Fatal(err)
					}
					want := 2
					if pass == 2 {
						want = 0
					}
					if st.Files != want || len(st.Errors) != 1 || !strings.HasPrefix(st.Errors[0].Error(), rels[1]+": ") {
						t.Fatalf("pass %d: files=%d errors=%v; want %d sent and one error naming %s", pass, st.Files, st.Errors, want, rels[1])
					}
				}
				_, logs := parseRequests(rcv.Requests())
				if len(logs) != 2 || !strings.Contains(logs[0].body, "record 1") || !strings.Contains(logs[1].body, "record 3") {
					t.Fatalf("the receiver holds %d records; want the first and the third file", len(logs))
				}
				sent, err := otlp.LoadSent(z.Root())
				if err != nil {
					t.Fatal(err)
				}
				for i, rel := range rels {
					if _, ok := sent.Digest(rel); ok != (i != 1) {
						t.Fatalf("%s recorded sent: %v", rel, ok)
					}
				}
			})
		}
	}
}

// A request that fails is reported as the request's error, and the file
// that was being batched when it went goes in the next request of the same
// pass. It was left out of the pass, under the failed request's error with
// its own name on it.
func TestAFailedRequestDoesNotSkipTheFileAfterIt(t *testing.T) {
	var requests atomic.Int32
	c := httpReceiver(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if requests.Add(1) == 1 {
			http.Error(w, "busy", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	z, rels := zoneOfThree(t, 0)
	p := &otlp.Pusher{Zone: z, Client: c, Version: "test", ServiceName: "Claude Code", Endpoint: "http://receiver", BatchBytes: 1}
	st, err := p.Pass()
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 2 || st.Requests != 3 || len(st.Errors) != 1 || strings.HasPrefix(st.Errors[0].Error(), rels[1]) {
		t.Fatalf("files=%d requests=%d errors=%v; want the second and third file sent, and the error not named for the second", st.Files, st.Requests, st.Errors)
	}
	sent, err := otlp.LoadSent(z.Root())
	if err != nil {
		t.Fatal(err)
	}
	for i, rel := range rels {
		if _, ok := sent.Digest(rel); ok != (i != 0) {
			t.Fatalf("%s recorded sent: %v", rel, ok)
		}
	}
	if st, err := p.Pass(); err != nil || st.Files != 1 || len(st.Errors) != 0 {
		t.Fatalf("the next pass: files=%d errors=%v, %v; want the first file sent", st.Files, st.Errors, err)
	}
}
