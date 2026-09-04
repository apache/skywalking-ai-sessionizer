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
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
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

// zoneWithOneSession builds a root with one landed file of two records, a
// child's landed file and one round of three frames, the smallest shape that
// exercises both formats. It returns the three files' bytes, in push order.
func zoneWithOneSession(t *testing.T) (*storage.Zone, []string) {
	t.Helper()
	z := storage.NewZone(t.TempDir())
	dir := z.StreamDir("sess1", "main")
	path := filepath.Join(dir, storage.LandedName("transcript", storage.Stamp(time.Unix(0, 0)), 1))
	var files []string
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
	files = append(files, string(data))

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
	files = append(files, string(data))

	round := filepath.Join(z.Root(), "_conversations", "sess1", "rounds", "r000001-abcdefabcdef.sf")
	roundText := "{\"t\":\"header\",\"schema\":\"sf/1\",\"conversation\":\"sess1\",\"session\":\"sess1\",\"round\":1," +
		"\"from_time\":\"2026-09-04T01:00:00Z\",\"through_time\":\"2026-09-04T01:00:00Z\"," +
		"\"session_from_time\":\"2026-09-03T23:00:00Z\",\"session_through_time\":\"2026-09-04T01:00:00Z\"," +
		"\"title\":\"hello\",\"talks\":1,\"steps\":2,\"streams\":1,\"segments\":1,\"unresolved\":0}\n" +
		"{\"t\":\"node\",\"id\":\"n1\",\"kind\":\"talk\"}\n" +
		"{\"t\":\"commit\",\"digest\":\"abcdefabcdef\",\"counts\":{\"nodes\":1}}\n"
	if err := os.MkdirAll(filepath.Dir(round), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(round, []byte(roundText), 0o444); err != nil {
		t.Fatal(err)
	}
	files = append(files, roundText)
	return z, files
}

func TestPushSendsEveryFileOnceWithItsAttributes(t *testing.T) {
	z, files := zoneWithOneSession(t)
	rcv := &receiver{}
	srv := httptest.NewServer(rcv.handler())
	defer srv.Close()
	p := &otlp.Pusher{Zone: z, Client: &otlp.Client{Endpoint: srv.URL}, Version: "test", ServiceName: "Claude Code", Layer: "AI_AGENT", InstanceID: "sender-1"}

	st, err := p.Pass()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Errors) != 0 || st.Files != len(files) || st.Requests != 1 {
		t.Fatalf("first pass: files=%d requests=%d errors=%v, want %d files in one request", st.Files, st.Requests, st.Errors, len(files))
	}

	var all []decodedLog
	var resources []map[string]string
	for _, req := range rcv.reqs {
		res, logs := parseRequest(t, req)
		resources = append(resources, res...)
		all = append(all, logs...)
	}
	if len(all) != len(files) {
		t.Fatalf("receiver decoded %d records, want one per file, %d", len(all), len(files))
	}
	// Every file arrives whole as one body, in order, byte for byte, and the
	// digest on the record is the digest of that body.
	for i, f := range files {
		if all[i].body != f {
			t.Fatalf("record %d body differs:\n got %s\nwant %s", i, all[i].body, f)
		}
		sum := sha256.Sum256([]byte(f))
		if all[i].attrs["asz.file.digest"] != hex.EncodeToString(sum[:]) {
			t.Fatalf("record %d digest %q is not the digest of its body", i, all[i].attrs["asz.file.digest"])
		}
		if all[i].attrs["asz.lines"] != fmt.Sprintf("int:%d", strings.Count(f, "\n")) {
			t.Fatalf("record %d lines = %q, want the line count of the body", i, all[i].attrs["asz.lines"])
		}
	}
	// The sender is named on the resource, and every file of every session
	// lands under the one configured service: one resource per request.
	if len(resources) != 1 {
		t.Fatalf("want one resource for one request, got %d: %v", len(resources), resources)
	}
	res := resources[0]
	if res["telemetry.sdk.name"] != "asz" || res["service.name"] != "Claude Code" || res["service.instance.id"] != "sender-1" || res["service.layer"] != "AI_AGENT" {
		t.Fatalf("resource attributes: %v", res)
	}
	// A landed file says what it is, and where a round's {seq, row} lands:
	// the session and the sequence name the file, and a row is a line of it.
	h := all[0].attrs
	if h["asz.format"] != "sd" || h["asz.format.version"] != "sd/1" || h["asz.file.kind"] != "transcript" || h["asz.file"] == "" ||
		h["asz.session"] != "sess1" || h["asz.seq"] != "int:1" || h["asz.stream"] != "main" || h["asz.run"] != "" {
		t.Fatalf("landed file attributes: %v", h)
	}
	if c := all[1].attrs; c["asz.seq"] != "int:2" || c["asz.stream"] != "a1" || c["asz.session"] != "sess1" {
		t.Fatalf("child file attributes: %v", c)
	}
	// The record time range of a file rides on its record: the main file has
	// one timed record, the child's has none, and the round carries its
	// header's pair.
	if h["asz.from_time"] != "2026-09-04T01:00:00Z" || h["asz.through_time"] != "2026-09-04T01:00:00Z" {
		t.Fatalf("landed file time range: %v", h)
	}
	if c := all[1].attrs; c["asz.from_time"] != "" || c["asz.through_time"] != "" {
		t.Fatalf("a file without timed records must carry no range: %v", c)
	}
	if r := all[2].attrs; r["asz.from_time"] != "2026-09-04T01:00:00Z" || r["asz.through_time"] != "2026-09-04T01:00:00Z" {
		t.Fatalf("round time range: %v", r)
	}
	// The session's own range rides on the round only.
	if r := all[2].attrs; r["asz.session.from_time"] != "2026-09-03T23:00:00Z" || r["asz.session.through_time"] != "2026-09-04T01:00:00Z" {
		t.Fatalf("round session range: %v", r)
	}
	if h["asz.session.from_time"] != "" || all[1].attrs["asz.session.through_time"] != "" {
		t.Fatalf("a landed file must not carry the session range: %v", h)
	}
	// What a list shows rides on the round, copied off its header.
	if r := all[2].attrs; r["asz.conversation.title"] != "hello" || r["asz.conversation.talks"] != "int:1" || r["asz.conversation.steps"] != "int:2" ||
		r["asz.conversation.streams"] != "int:1" || r["asz.conversation.segments"] != "int:1" || r["asz.conversation.unresolved"] != "int:0" {
		t.Fatalf("round list attributes: %v", r)
	}
	if h["asz.conversation.talks"] != "" {
		t.Fatalf("a landed file must not carry list attributes: %v", h)
	}
	if _, ok := h["asz.line"]; ok {
		t.Fatalf("a whole file carries no line address: %v", h)
	}
	// A landed file is stamped with its last record time, and a file without
	// timed records with the session's latest record time, so a receiver
	// that bounds a read by the session's range finds both.
	at := uint64(time.Date(2026, 9, 4, 1, 0, 0, 0, time.UTC).UnixNano())
	if all[0].time != at || all[1].time != at {
		t.Fatalf("landed file times = %d, %d, want %d for both", all[0].time, all[1].time, at)
	}
	// A round is stamped with the session's last activity as of the round.
	if all[2].time != at {
		t.Fatalf("round time = %d, want %d", all[2].time, at)
	}
	r := all[2].attrs
	if r["asz.format"] != "sf" || r["asz.format.version"] != "sf/1" || r["asz.file.kind"] != "round" || r["asz.round"] != "int:1" || r["asz.conversation"] != "sess1" || r["asz.session"] != "sess1" || r["asz.seq"] != "" {
		t.Fatalf("round attributes: %v", r)
	}

	// A second pass sends nothing: every file is write-once and recorded.
	before := len(rcv.reqs)
	st, err = p.Pass()
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 0 || len(rcv.reqs) != before {
		t.Fatalf("second pass re-sent %d files", st.Files)
	}
	if _, err := os.Stat(filepath.Join(z.Root(), "push.state")); err != nil {
		t.Fatal("push.state was not written")
	}
}

func TestPushLeavesFilesForTheNextPassWhenTheReceiverFails(t *testing.T) {
	z, files := zoneWithOneSession(t)
	rcv := &receiver{fail: true}
	srv := httptest.NewServer(rcv.handler())
	defer srv.Close()
	p := &otlp.Pusher{Zone: z, Client: &otlp.Client{Endpoint: srv.URL}, Version: "test", ServiceName: "Claude Code"}

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
	if len(st.Errors) != 0 || st.Files != len(files) {
		t.Fatalf("the next pass must send everything: files=%d errors=%v", st.Files, st.Errors)
	}
}

// A file larger than the budget travels alone, and the files around it are
// batched as before: with a budget smaller than any file, every file is its
// own request.
func TestPushSendsAFileLargerThanTheBudgetAlone(t *testing.T) {
	z, files := zoneWithOneSession(t)
	rcv := &receiver{}
	srv := httptest.NewServer(rcv.handler())
	defer srv.Close()
	p := &otlp.Pusher{Zone: z, Client: &otlp.Client{Endpoint: srv.URL}, Version: "test", ServiceName: "Claude Code", BatchBytes: 1}
	st, err := p.Pass()
	if err != nil || len(st.Errors) != 0 {
		t.Fatal(err, st.Errors)
	}
	if st.Requests != len(files) || st.Files != len(files) {
		t.Fatalf("a one-byte budget must send a request per file: %d requests for %d files, %d marked", st.Requests, len(files), st.Files)
	}
	for i, req := range rcv.reqs {
		_, logs := parseRequest(t, req)
		if len(logs) != 1 || logs[0].body != files[i] {
			t.Fatalf("request %d must carry file %d alone and whole", i, i)
		}
	}
}

// A pusher with neither a service name nor runtime names refuses to run;
// with runtime names, a session is attributed to the runtime its landed
// header names.
func TestServiceIsTheRuntimeThatProducedTheSession(t *testing.T) {
	z, _ := zoneWithOneSession(t)
	p := &otlp.Pusher{Zone: z, Client: &otlp.Client{Endpoint: "http://127.0.0.1:1"}, Version: "test"}
	if err := p.Prepare(); err == nil {
		t.Fatal("Prepare accepted a pusher with no service name and no runtime names")
	}
	rcv := &receiver{}
	srv := httptest.NewServer(rcv.handler())
	defer srv.Close()
	p = &otlp.Pusher{Zone: z, Client: &otlp.Client{Endpoint: srv.URL}, Version: "test",
		Runtimes: map[string]string{"test": "Test Runtime"}}
	if _, err := p.Pass(); err != nil {
		t.Fatal(err)
	}
	res, _ := parseRequest(t, rcv.reqs[0])
	if res[0]["service.name"] != "Test Runtime" {
		t.Fatalf("service.name is %q; the header's adapter test/0 names the runtime Test Runtime", res[0]["service.name"])
	}
}

// Without a configured instance id the sender makes one UUID and keeps it
// for every pass; a configured one is sent as given.
func TestInstanceIDIsAUUIDUnlessConfigured(t *testing.T) {
	z, _ := zoneWithOneSession(t)
	rcv := &receiver{}
	srv := httptest.NewServer(rcv.handler())
	defer srv.Close()
	p := &otlp.Pusher{Zone: z, Client: &otlp.Client{Endpoint: srv.URL}, Version: "test", ServiceName: "Claude Code"}
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
