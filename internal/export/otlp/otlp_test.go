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
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// A small wire-format decoder, independent of the encoder, so the test reads
// what a receiver would read rather than what the encoder meant to write.
type field struct {
	num  int
	wire int
	u    uint64
	b    []byte
}

func decode(t *testing.T, b []byte) []field {
	t.Helper()
	var out []field
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 {
			t.Fatalf("bad tag at %d bytes from the end", len(b))
		}
		b = b[n:]
		f := field{num: int(tag >> 3), wire: int(tag & 7)}
		switch f.wire {
		case 0:
			v, n := binary.Uvarint(b)
			f.u, b = v, b[n:]
		case 1:
			f.u, b = binary.LittleEndian.Uint64(b), b[8:]
		case 2:
			l, n := binary.Uvarint(b)
			b = b[n:]
			f.b, b = b[:l], b[l:]
		default:
			t.Fatalf("unexpected wire type %d", f.wire)
		}
		out = append(out, f)
	}
	return out
}

// decodedLog is one record as a receiver sees it.
type decodedLog struct {
	time  uint64
	body  string
	attrs map[string]string
}

func attrsOf(t *testing.T, kvs []field) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, kv := range kvs {
		var key, val string
		for _, f := range decode(t, kv.b) {
			switch f.num {
			case 1:
				key = string(f.b)
			case 2:
				for _, v := range decode(t, f.b) {
					switch v.num {
					case 1:
						val = string(v.b)
					case 3:
						val = "int:" + strconv.FormatInt(int64(v.u), 10)
					}
				}
			}
		}
		out[key] = val
	}
	return out
}

// parseRequest decodes an ExportLogsServiceRequest into resource attributes
// and records.
func parseRequest(t *testing.T, body []byte) (resources []map[string]string, logs []decodedLog) {
	t.Helper()
	for _, rl := range decode(t, body) {
		if rl.num != 1 {
			continue
		}
		var res map[string]string
		for _, f := range decode(t, rl.b) {
			switch f.num {
			case 1: // Resource
				var kvs []field
				for _, a := range decode(t, f.b) {
					if a.num == 1 {
						kvs = append(kvs, a)
					}
				}
				res = attrsOf(t, kvs)
			case 2: // ScopeLogs
				for _, s := range decode(t, f.b) {
					if s.num != 2 {
						continue
					}
					var rec decodedLog
					var kvs []field
					for _, r := range decode(t, s.b) {
						switch r.num {
						case 1:
							rec.time = r.u
						case 5:
							for _, v := range decode(t, r.b) {
								if v.num == 1 {
									rec.body = string(v.b)
								}
							}
						case 6:
							kvs = append(kvs, r)
						}
					}
					rec.attrs = attrsOf(t, kvs)
					logs = append(logs, rec)
				}
			}
		}
		resources = append(resources, res)
	}
	return resources, logs
}

// receiver is an OTLP/HTTP logs endpoint that keeps every request.
type receiver struct {
	mu   sync.Mutex
	reqs [][]byte
	fail bool
}

func (r *receiver) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/logs" || req.Header.Get("Content-Type") != "application/x-protobuf" {
			http.Error(w, "wrong path or content type", http.StatusBadRequest)
			return
		}
		body := make([]byte, 0, 1<<16)
		buf := make([]byte, 1<<16)
		for {
			n, err := req.Body.Read(buf)
			body = append(body, buf[:n]...)
			if err != nil {
				break
			}
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.fail {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		r.reqs = append(r.reqs, body)
		w.WriteHeader(http.StatusOK)
	})
}

// zoneWithOneSession builds a root with one landed file of two records and
// one round of three frames, the smallest shape that exercises both formats.
func zoneWithOneSession(t *testing.T) (*storage.Zone, []string) {
	t.Helper()
	z := storage.NewZone(t.TempDir())
	dir := z.StreamDir("sess1", "main")
	path := filepath.Join(dir, storage.LandedName("transcript", storage.Stamp(time.Unix(0, 0)), 1))
	var lines []string
	err := storage.WriteAtomic(path, storage.PermLanded, func(w io.Writer) error {
		hdr := &sessiondata.Header{Seq: 1, At: "2026-09-04T00:00:00Z", Kind: sessiondata.KindTranscript,
			Adapter: "test/0", Dialect: "test/1", Src: "-Users-me-proj/sess1.jsonl", Session: "sess1", Stream: "main"}
		sw, err := sessiondata.NewWriter(w, hdr)
		if err != nil {
			return err
		}
		if err := sw.Write(&sessiondata.Record{Ord: 1, Sha: "a", Bytes: 1, Time: "2026-09-04T01:00:00Z", Parts: []sessiondata.Part{{Kind: sessiondata.PartText, Text: "hello", State: "available", Bytes: 5}}}); err != nil {
			return err
		}
		if err := sw.Write(&sessiondata.Record{Ord: 2, Sha: "b", Bytes: 1, Parts: []sessiondata.Part{{Kind: sessiondata.PartText, Text: "no time", State: "available", Bytes: 7}}}); err != nil {
			return err
		}
		return sw.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	lines = append(lines, strings.Split(strings.TrimRight(string(data), "\n"), "\n")...)

	// A child agent that ran in another directory: its file names that
	// directory, but it belongs to the session's project all the same.
	child := filepath.Join(z.StreamDir("sess1", "a1"), storage.LandedName("transcript", storage.Stamp(time.Unix(0, 0)), 2))
	err = storage.WriteAtomic(child, storage.PermLanded, func(w io.Writer) error {
		hdr := &sessiondata.Header{Seq: 2, At: "2026-09-04T00:00:00Z", Kind: sessiondata.KindTranscript,
			Adapter: "test/0", Dialect: "test/1", Src: "-private-tmp-scratch/sess1/subagents/agent-a1.jsonl", Session: "sess1", Stream: "a1"}
		sw, err := sessiondata.NewWriter(w, hdr)
		if err != nil {
			return err
		}
		if err := sw.Write(&sessiondata.Record{Ord: 1, Sha: "c", Bytes: 1, Parts: []sessiondata.Part{{Kind: sessiondata.PartText, Text: "child", State: "available", Bytes: 5}}}); err != nil {
			return err
		}
		return sw.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(child)
	lines = append(lines, strings.Split(strings.TrimRight(string(data), "\n"), "\n")...)

	round := filepath.Join(z.Root(), "_conversations", "sess1", "rounds", "r000001-abcdefabcdef.sf")
	roundText := "{\"t\":\"header\",\"schema\":\"sf/1\",\"conversation\":\"sess1\",\"session\":\"sess1\",\"round\":1}\n" +
		"{\"t\":\"node\",\"id\":\"n1\",\"kind\":\"talk\"}\n" +
		"{\"t\":\"commit\",\"digest\":\"abcdefabcdef\",\"counts\":{\"nodes\":1}}\n"
	if err := os.MkdirAll(filepath.Dir(round), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(round, []byte(roundText), 0o444); err != nil {
		t.Fatal(err)
	}
	lines = append(lines, strings.Split(strings.TrimRight(roundText, "\n"), "\n")...)
	return z, lines
}

func TestPushSendsEveryLineOnceWithItsAttributes(t *testing.T) {
	z, lines := zoneWithOneSession(t)
	rcv := &receiver{}
	srv := httptest.NewServer(rcv.handler())
	defer srv.Close()
	p := &otlp.Pusher{Zone: z, Client: &otlp.Client{Endpoint: srv.URL}, Version: "test", Layer: "AI-AGENT", InstanceID: "sender-1"}

	st, err := p.Pass()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Errors) != 0 || st.Files != 3 || st.Records != len(lines) {
		t.Fatalf("first pass: files=%d records=%d errors=%v, want 3 files and %d records", st.Files, st.Records, st.Errors, len(lines))
	}

	var all []decodedLog
	var resources []map[string]string
	for _, req := range rcv.reqs {
		res, logs := parseRequest(t, req)
		resources = append(resources, res...)
		all = append(all, logs...)
	}
	if len(all) != len(lines) {
		t.Fatalf("receiver decoded %d records, want %d", len(all), len(lines))
	}
	// Every line arrives as a body, in order, unchanged.
	for i, l := range lines {
		if all[i].body != l {
			t.Fatalf("record %d body differs:\n got %s\nwant %s", i, all[i].body, l)
		}
	}
	// The sender is named on the resource, the service comes from the project
	// directory of the main transcript, and the child's file in another
	// directory lands under the same service: one resource for the session.
	if len(resources) != 1 {
		t.Fatalf("want one resource for one session, got %d: %v", len(resources), resources)
	}
	res := resources[0]
	if res["telemetry.sdk.name"] != "asz" || res["service.name"] != "-Users-me-proj" || res["service.instance.id"] != "sender-1" || res["service.layer"] != "AI-AGENT" {
		t.Fatalf("resource attributes: %v", res)
	}
	if all[1].attrs["asz.session"] != "sess1" {
		t.Fatalf("the session rides on the record: %v", all[1].attrs)
	}
	// Format, version, file kind and line kinds ride on each record.
	h := all[0].attrs
	if h["asz.format"] != "sd" || h["asz.format.version"] != "sd/1" || h["asz.file.kind"] != "transcript" || h["asz.line.kind"] != "header" || h["asz.line"] != "int:0" {
		t.Fatalf("landed header attributes: %v", h)
	}
	if all[1].attrs["asz.line.kind"] != "record" || all[3].attrs["asz.line.kind"] != "end" {
		t.Fatalf("landed line kinds: %v / %v", all[1].attrs, all[3].attrs)
	}
	// A record with a time is stamped with it; one without gets the file's time.
	if all[1].time != uint64(time.Date(2026, 9, 4, 1, 0, 0, 0, time.UTC).UnixNano()) {
		t.Fatalf("record time = %d", all[1].time)
	}
	if all[2].time != uint64(time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC).UnixNano()) {
		t.Fatalf("record without a time must carry the file's time, got %d", all[2].time)
	}
	r := all[7].attrs
	if r["asz.format"] != "sf" || r["asz.format.version"] != "sf/1" || r["asz.file.kind"] != "round" || r["asz.line.kind"] != "header" || r["asz.round"] != "int:1" || r["asz.conversation"] != "sess1" {
		t.Fatalf("round header attributes: %v", r)
	}
	if all[8].attrs["asz.line.kind"] != "node" || all[9].attrs["asz.line.kind"] != "commit" {
		t.Fatalf("round line kinds: %v / %v", all[8].attrs, all[9].attrs)
	}

	// A second pass sends nothing: both files are write-once and recorded.
	before := len(rcv.reqs)
	st, err = p.Pass()
	if err != nil {
		t.Fatal(err)
	}
	if st.Records != 0 || len(rcv.reqs) != before {
		t.Fatalf("second pass re-sent %d records", st.Records)
	}
	if _, err := os.Stat(filepath.Join(z.Root(), "push.state")); err != nil {
		t.Fatal("push.state was not written")
	}
}

func TestPushLeavesFilesForTheNextPassWhenTheReceiverFails(t *testing.T) {
	z, lines := zoneWithOneSession(t)
	rcv := &receiver{fail: true}
	srv := httptest.NewServer(rcv.handler())
	defer srv.Close()
	p := &otlp.Pusher{Zone: z, Client: &otlp.Client{Endpoint: srv.URL}, Version: "test"}

	st, err := p.Pass()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Errors) == 0 || st.Files != 0 {
		t.Fatalf("a failed request must mark nothing pushed: files=%d errors=%v", st.Files, st.Errors)
	}
	rcv.mu.Lock()
	rcv.fail = false
	rcv.mu.Unlock()
	st, err = p.Pass()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Errors) != 0 || st.Records != len(lines) {
		t.Fatalf("the next pass must send everything: records=%d errors=%v", st.Records, st.Errors)
	}
}

func TestPushSplitsLargeBatches(t *testing.T) {
	z, lines := zoneWithOneSession(t)
	rcv := &receiver{}
	srv := httptest.NewServer(rcv.handler())
	defer srv.Close()
	p := &otlp.Pusher{Zone: z, Client: &otlp.Client{Endpoint: srv.URL}, Version: "test", BatchBytes: 1}
	st, err := p.Pass()
	if err != nil || len(st.Errors) != 0 {
		t.Fatal(err, st.Errors)
	}
	if st.Requests < len(lines) {
		t.Fatalf("a one-byte budget must send a request per line: %d requests for %d lines", st.Requests, len(lines))
	}
	if st.Files != 3 {
		t.Fatalf("files marked pushed = %d, want 3", st.Files)
	}
}

// Without a configured instance id the sender makes one UUID and keeps it
// for every pass; a configured one is sent as given.
func TestInstanceIDIsAUUIDUnlessConfigured(t *testing.T) {
	z, _ := zoneWithOneSession(t)
	rcv := &receiver{}
	srv := httptest.NewServer(rcv.handler())
	defer srv.Close()
	p := &otlp.Pusher{Zone: z, Client: &otlp.Client{Endpoint: srv.URL}, Version: "test"}
	if err := p.Prepare(); err != nil {
		t.Fatal(err)
	}
	id := p.InstanceID
	if len(id) != 36 || id[14] != '4' || strings.Count(id, "-") != 4 {
		t.Fatalf("instance id %q is not a version 4 UUID", id)
	}
	if _, err := p.Pass(); err != nil {
		t.Fatal(err)
	}
	if p.InstanceID != id {
		t.Fatalf("instance id changed between Prepare and Pass: %s -> %s", id, p.InstanceID)
	}
	res, _ := parseRequest(t, rcv.reqs[0])
	if res[0]["service.instance.id"] != id {
		t.Fatalf("resource carries %q, want %q", res[0]["service.instance.id"], id)
	}
}
