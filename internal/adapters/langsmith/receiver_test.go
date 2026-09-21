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

package langsmith

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// started brings up a receiver over a fresh root and returns its base URL.
func started(t *testing.T, token string) (*Receiver, *storage.Zone, string) {
	t.Helper()
	zone := storage.NewZone(t.TempDir())
	at := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	r := &Receiver{Zone: zone, Listen: "127.0.0.1:0", Token: token,
		Now: func() time.Time { at = at.Add(time.Millisecond); return at }}
	if err := r.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(r.Stop)
	return r, zone, "http://" + r.Addr()
}

func send(t *testing.T, base, path, contentType string, body []byte, key string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	if key != "" {
		req.Header.Set("x-api-key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return resp
}

// TestInfoDoesNotAdvertiseCompression is the reason this project needs no zstd
// decoder at all: the client compresses only when told the receiver can read
// it. If that flag ever appeared here, every body would arrive compressed and
// nothing would read it.
func TestInfoDoesNotAdvertiseCompression(t *testing.T) {
	_, _, base := started(t, "")
	resp, err := http.Get(base + "/info")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var info struct {
		Flags  map[string]any `json:"instance_flags"`
		Config map[string]any `json:"batch_ingest_config"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if _, present := info.Flags["zstd_compression_enabled"]; present {
		t.Fatal("the receiver advertised compression it cannot read")
	}
	if info.Config["use_multipart_endpoint"] != true {
		t.Fatal("the batch configuration does not name the endpoint this serves")
	}
}

// TestEveryCapturedRequestIsAcceptedAndLands is the end of the receiving path:
// what one real client sent, over HTTP, into landed Session Data.
func TestEveryCapturedRequestIsAcceptedAndLands(t *testing.T) {
	r, zone, base := started(t, "")
	sent := 0
	captured(t, func(kase, request string, meta capturedMeta, body []byte) {
		resp := send(t, base, "/runs/multipart", meta.Headers["Content-Type"], body, "any-key")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			text, _ := io.ReadAll(resp.Body)
			t.Fatalf("%s/%s: status %d: %s", kase, request, resp.StatusCode, text)
		}
		sent++
	})
	stats := r.Stats()
	if stats.Requests != int64(sent) || stats.Operations == 0 {
		t.Fatalf("accepted %d requests and %d operations, sent %d",
			stats.Requests, stats.Operations, sent)
	}

	at := time.Date(2026, 9, 20, 11, 0, 0, 0, time.UTC)
	collector := &Collector{Zone: zone, Ownership: DefaultOwnership(),
		Now: func() time.Time { at = at.Add(time.Second); return at }}
	landed, err := collector.Collect()
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if landed.Requests != sent {
		t.Fatalf("converted %d of %d requests", landed.Requests, sent)
	}
	if landed.Unassigned == 0 {
		t.Error("the corpus holds a case with no thread key; none landed unassigned")
	}
	t.Logf("%d requests, %d files, %d records, %d sessions, %d unassigned arrivals",
		landed.Requests, landed.Files, landed.Records, len(landed.Sessions),
		landed.Unassigned)

	// The inbox is empty: nothing is kept once it has landed.
	waiting, err := NewInbox(zone).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(waiting) != 0 {
		t.Fatalf("%d requests still waiting", len(waiting))
	}
	assertContiguous(t, zone, landed.Sessions)
}

// assertContiguous holds what asz verify holds: ordinals run 1, 2, 3 with no
// gap across a stream's files, and each record starts at the byte after the
// one before it ended. A landed file that breaks this reads as a shorter
// conversation with nothing saying so.
func assertContiguous(t *testing.T, zone *storage.Zone, sessions []string) {
	t.Helper()
	for _, session := range sessions {
		dir := zone.StreamDir(session, MainStream)
		items, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("%s: %v", session, err)
		}
		var names []string
		for _, it := range items {
			if strings.HasSuffix(it.Name(), ".sd") {
				names = append(names, it.Name())
			}
		}
		if len(names) == 0 {
			t.Errorf("%s: no landed file", session)
			continue
		}
		wantOrd, wantOff := uint64(1), uint64(0)
		for _, name := range names {
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			reader, err := sessiondata.NewReader(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("%s/%s: %v", session, name, err)
			}
			for {
				record, err := reader.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("%s/%s: %v", session, name, err)
				}
				if record.Ord != wantOrd {
					t.Fatalf("%s/%s: ordinal %d, want %d", session, name, record.Ord, wantOrd)
				}
				if record.Off != wantOff {
					t.Fatalf("%s/%s: offset %d, want %d", session, name, record.Off, wantOff)
				}
				wantOrd++
				wantOff += uint64(record.Bytes) + 1
			}
		}
	}
}

// TestARetriedBatchLandsAgainAndTheIndexCanTellIsRepeated: delivery is
// at-least-once by design, so the same bytes twice must land twice with the
// same record ids, which is what lets a reader call the second copy a repeat
// rather than a second answer.
func TestARetriedBatchLandsAgainAndTheIndexCanTellIsRepeated(t *testing.T) {
	_, zone, base := started(t, "")
	var body []byte
	var contentType string
	captured(t, func(kase, _ string, meta capturedMeta, b []byte) {
		if kase == "plain" && body == nil {
			body, contentType = b, meta.Headers["Content-Type"]
		}
	})
	for i := 0; i < 2; i++ {
		resp := send(t, base, "/runs/multipart", contentType, body, "k")
		resp.Body.Close()
	}
	at := time.Date(2026, 9, 20, 11, 0, 0, 0, time.UTC)
	collector := &Collector{Zone: zone,
		Now: func() time.Time { at = at.Add(time.Second); return at }}
	landed, err := collector.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if landed.Files != 2 {
		t.Fatalf("a repeated request landed %d files, want 2", landed.Files)
	}
	ids := map[string]int{}
	for _, session := range landed.Sessions {
		dir := zone.StreamDir(session, MainStream)
		items, _ := os.ReadDir(dir)
		for _, it := range items {
			data, _ := os.ReadFile(filepath.Join(dir, it.Name()))
			reader, err := sessiondata.NewReader(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			for {
				record, err := reader.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				ids[record.ID]++
			}
		}
	}
	repeats := 0
	for _, n := range ids {
		if n == 2 {
			repeats++
		}
	}
	if repeats == 0 {
		t.Fatal("the second copy carried different record ids, so nothing can call it a repeat")
	}
}

// TestAZstdBodyIsRefusedWithTheReason: never advertised, so a client sending
// it has ignored what it was told, and the message has to name the flag or
// whoever runs it is guessing.
func TestAZstdBodyIsRefusedWithTheReason(t *testing.T) {
	_, _, base := started(t, "")
	req, _ := http.NewRequest(http.MethodPost, base+"/runs/multipart",
		strings.NewReader("not really zstd"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	req.Header.Set("Content-Encoding", "zstd")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	text, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest ||
		!strings.Contains(string(text), "zstd_compression_enabled") {
		t.Fatalf("status %d: %s", resp.StatusCode, text)
	}
}

// TestATokenIsRequiredWhenSet, and any key passes when it is not: the client
// insists on sending one and has nothing to prove to a local collector.
func TestATokenIsRequiredWhenSet(t *testing.T) {
	_, _, base := started(t, "the-token")
	// A body with a run in it. An empty one stood here before, and it made
	// the test pass for the wrong reason: it checked that the receiver let a
	// key through, not that it accepted anything.
	body, contentType := oneRunMultipart()
	if resp := send(t, base, "/runs/multipart", contentType, body, "wrong"); true {
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("a wrong key got %d", resp.StatusCode)
		}
	}
	if resp := send(t, base, "/runs/multipart", contentType, body, "the-token"); true {
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("the right key got %d", resp.StatusCode)
		}
	}
}

// TestAMultipartBodyWithNothingInItIsRefused: a 202 says the request was
// kept. A body this receiver understands nothing in was not, so it says so.
func TestAMultipartBodyWithNothingInItIsRefused(t *testing.T) {
	_, _, base := started(t, "")
	resp := send(t, base, "/runs/multipart", "multipart/form-data; boundary=x",
		[]byte("--x--\r\n"), "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

// oneRunMultipart builds the smallest body the client would really send.
func oneRunMultipart() ([]byte, string) {
	const boundary = "asztest"
	const run = `{"id":"44444444-4444-7444-8444-444444444444",` +
		`"trace_id":"44444444-4444-7444-8444-444444444444",` +
		`"dotted_order":"20260920T100000000000Z44444444-4444-7444-8444-444444444444",` +
		`"run_type":"chain","name":"one","start_time":"2026-09-20T10:00:00Z",` +
		`"end_time":"2026-09-20T10:00:01Z","session_name":"asz"}`
	var b bytes.Buffer
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Disposition: form-data; name=\"post.44444444-4444-7444-8444-444444444444\"\r\n")
	b.WriteString("Content-Type: application/json\r\n\r\n")
	b.WriteString(run)
	b.WriteString("\r\n--" + boundary + "--\r\n")
	return b.Bytes(), "multipart/form-data; boundary=" + boundary
}
