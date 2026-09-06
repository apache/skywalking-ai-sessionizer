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
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"

	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp"
	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp/otlptest"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// Every test runs over both transports: the same request must reach the
// receiver whichever connection carries it.
var protocols = []string{otlp.ProtocolGRPC, otlp.ProtocolHTTP}

// decodedLog is one record as a receiver sees it.
type decodedLog struct {
	time  uint64
	body  string
	attrs map[string]string
}

// parseRequests reads what the receiver accepted: the resource attributes
// of every resource, and every record in order.
func parseRequests(reqs []*collogspb.ExportLogsServiceRequest) (resources []map[string]string, logs []decodedLog) {
	for _, req := range reqs {
		for _, rl := range req.GetResourceLogs() {
			resources = append(resources, otlptest.Attrs(rl.GetResource().GetAttributes()))
			for _, sl := range rl.GetScopeLogs() {
				for _, r := range sl.GetLogRecords() {
					logs = append(logs, decodedLog{time: r.GetTimeUnixNano(), body: r.GetBody().GetStringValue(), attrs: otlptest.Attrs(r.GetAttributes())})
				}
			}
		}
	}
	return resources, logs
}

func startReceiver(t *testing.T) *otlptest.Receiver {
	t.Helper()
	rcv, err := otlptest.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rcv.Close)
	return rcv
}

func clientFor(t *testing.T, rcv *otlptest.Receiver, protocol string) otlp.Client {
	t.Helper()
	c, err := rcv.Client(protocol)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
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
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) { testPushSendsEveryFileOnce(t, protocol) })
	}
}

func testPushSendsEveryFileOnce(t *testing.T, protocol string) {
	z, files := zoneWithOneSession(t)
	rcv := startReceiver(t)
	p := &otlp.Pusher{Zone: z, Client: clientFor(t, rcv, protocol), Version: "test", ServiceName: "Claude Code", Layer: "AI_AGENT", InstanceID: "sender-1"}

	st, err := p.Pass()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Errors) != 0 || st.Files != len(files) || st.Requests != 1 {
		t.Fatalf("first pass: files=%d requests=%d errors=%v, want %d files in one request", st.Files, st.Requests, st.Errors, len(files))
	}

	resources, all := parseRequests(rcv.Requests())
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
	before := len(rcv.Requests())
	st, err = p.Pass()
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 0 || len(rcv.Requests()) != before {
		t.Fatalf("second pass re-sent %d files", st.Files)
	}
	if _, err := os.Stat(filepath.Join(z.Root(), "push.state")); err != nil {
		t.Fatal("push.state was not written")
	}
}

func TestPushLeavesFilesForTheNextPassWhenTheReceiverFails(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) { testPushLeavesFilesForTheNextPass(t, protocol) })
	}
}

func testPushLeavesFilesForTheNextPass(t *testing.T, protocol string) {
	z, files := zoneWithOneSession(t)
	rcv := startReceiver(t)
	rcv.Fail(true)
	p := &otlp.Pusher{Zone: z, Client: clientFor(t, rcv, protocol), Version: "test", ServiceName: "Claude Code"}

	st, err := p.Pass()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Errors) == 0 || st.Files != 0 {
		t.Fatalf("a failed request must mark nothing pushed: files=%d errors=%v", st.Files, st.Errors)
	}
	rcv.Fail(false)
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
	rcv := startReceiver(t)
	p := &otlp.Pusher{Zone: z, Client: clientFor(t, rcv, otlp.ProtocolGRPC), Version: "test", ServiceName: "Claude Code", BatchBytes: 1}
	st, err := p.Pass()
	if err != nil || len(st.Errors) != 0 {
		t.Fatal(err, st.Errors)
	}
	if st.Requests != len(files) || st.Files != len(files) {
		t.Fatalf("a one-byte budget must send a request per file: %d requests for %d files, %d marked", st.Requests, len(files), st.Files)
	}
	for i, req := range rcv.Requests() {
		_, logs := parseRequests([]*collogspb.ExportLogsServiceRequest{req})
		if len(logs) != 1 || logs[0].body != files[i] {
			t.Fatalf("request %d must carry file %d alone and whole", i, i)
		}
	}
}

// A client refuses an endpoint of the wrong shape for its transport, and
// an unreachable receiver is reported by the pass, not by the client.
func TestClientRefusesAnEndpointOfTheWrongShape(t *testing.T) {
	if _, err := otlp.NewClient(otlp.Options{Protocol: otlp.ProtocolGRPC, Endpoint: "http://127.0.0.1:11800"}); err == nil {
		t.Fatal("a grpc client accepted a URL")
	}
	if _, err := otlp.NewClient(otlp.Options{Protocol: otlp.ProtocolHTTP, Endpoint: "127.0.0.1:12800"}); err == nil {
		t.Fatal("an http client accepted host:port")
	}
	if _, err := otlp.NewClient(otlp.Options{Protocol: "tcp", Endpoint: "127.0.0.1:1"}); err == nil {
		t.Fatal("an unknown protocol was accepted")
	}
	if _, err := otlp.NewClient(otlp.Options{}); err == nil {
		t.Fatal("an empty endpoint was accepted")
	}
	z, _ := zoneWithOneSession(t)
	for _, o := range []otlp.Options{
		{Protocol: otlp.ProtocolGRPC, Endpoint: "127.0.0.1:1", Timeout: 2 * time.Second},
		{Protocol: otlp.ProtocolHTTP, Endpoint: "http://127.0.0.1:1", Timeout: 2 * time.Second},
	} {
		c, err := otlp.NewClient(o)
		if err != nil {
			t.Fatal(err)
		}
		p := &otlp.Pusher{Zone: z, Client: c, Version: "test", ServiceName: "Claude Code"}
		st, err := p.Pass()
		if err != nil {
			t.Fatal(err)
		}
		if len(st.Errors) == 0 || st.Files != 0 {
			t.Fatalf("%s: an unreachable receiver must fail the pass and mark nothing: files=%d errors=%v", o.Protocol, st.Files, st.Errors)
		}
		_ = c.Close()
	}
}

// A pusher with neither a service name nor runtime names refuses to run;
// with runtime names, a session is attributed to the runtime its landed
// header names.
func TestServiceIsTheRuntimeThatProducedTheSession(t *testing.T) {
	z, _ := zoneWithOneSession(t)
	rcv := startReceiver(t)
	p := &otlp.Pusher{Zone: z, Client: clientFor(t, rcv, otlp.ProtocolGRPC), Version: "test"}
	if err := p.Prepare(); err == nil {
		t.Fatal("Prepare accepted a pusher with no service name and no runtime names")
	}
	if err := (&otlp.Pusher{Zone: z, Version: "test", ServiceName: "x"}).Prepare(); err == nil {
		t.Fatal("Prepare accepted a pusher with no client")
	}
	p = &otlp.Pusher{Zone: z, Client: clientFor(t, rcv, otlp.ProtocolGRPC), Version: "test",
		Runtimes: map[string]string{"test": "Test Runtime"}}
	if _, err := p.Pass(); err != nil {
		t.Fatal(err)
	}
	res, _ := parseRequests(rcv.Requests())
	if res[0]["service.name"] != "Test Runtime" {
		t.Fatalf("service.name is %q; the header's adapter test/0 names the runtime Test Runtime", res[0]["service.name"])
	}
}

// Without a configured instance id the sender names itself by the person
// and the machine, user@host, and keeps that for every pass; a configured
// one is sent as given.
func TestInstanceIsUserAtHostUnlessConfigured(t *testing.T) {
	z, _ := zoneWithOneSession(t)
	rcv := startReceiver(t)
	p := &otlp.Pusher{Zone: z, Client: clientFor(t, rcv, otlp.ProtocolGRPC), Version: "test", ServiceName: "Claude Code"}
	if err := p.Prepare(); err != nil {
		t.Fatal(err)
	}
	id := p.InstanceID
	host, _ := os.Hostname()
	if !strings.HasSuffix(id, "@"+host) || strings.HasPrefix(id, "@") {
		t.Fatalf("instance id %q is not user@%s", id, host)
	}
	if _, err := p.Pass(); err != nil {
		t.Fatal(err)
	}
	if p.InstanceID != id {
		t.Fatalf("instance id changed between Prepare and Pass: %s -> %s", id, p.InstanceID)
	}
	res, _ := parseRequests(rcv.Requests())
	if res[0]["service.instance.id"] != id {
		t.Fatalf("resource carries %q, want %q", res[0]["service.instance.id"], id)
	}
}

// fakeClock drives a pusher's Now and Sleep: sleeping moves the clock.
type fakeClock struct {
	now   time.Time
	slept time.Duration
	calls int
}

func (c *fakeClock) wire(p *otlp.Pusher) {
	p.Now = func() time.Time { return c.now }
	p.Sleep = func(d time.Duration) { c.calls++; c.slept += d; c.now = c.now.Add(d) }
}

// A rate limit paces the requests of a pass and delivers everything; no
// limit never waits.
func TestRateLimitPacesAPass(t *testing.T) {
	z, files := zoneWithOneSession(t)
	rcv := startReceiver(t)
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	// Every file alone, and a minute's budget smaller than any request, so
	// every request after the first waits for a full bucket.
	p := &otlp.Pusher{Zone: z, Client: clientFor(t, rcv, otlp.ProtocolGRPC), Version: "test", ServiceName: "Claude Code",
		BatchBytes: 1, MaxBytesPerMinute: 64}
	clock.wire(p)
	st, err := p.Pass()
	if err != nil || len(st.Errors) != 0 {
		t.Fatal(err, st.Errors)
	}
	if st.Files != len(files) || st.Requests != len(files) {
		t.Fatalf("a paced pass must still send everything: files=%d requests=%d", st.Files, st.Requests)
	}
	if st.Paused != clock.slept || clock.calls != len(files)-1 || st.Paused != time.Duration(len(files)-1)*time.Minute {
		t.Fatalf("paused %s over %d waits, want %d minutes: one full bucket per request after the first", st.Paused, clock.calls, len(files)-1)
	}
	if st.Wire == 0 || st.Wire < st.Bytes {
		t.Fatalf("wire bytes %d must count the encoded requests, at least the %d file bytes", st.Wire, st.Bytes)
	}

	z2, _ := zoneWithOneSession(t)
	free := &fakeClock{now: clock.now}
	p = &otlp.Pusher{Zone: z2, Client: clientFor(t, rcv, otlp.ProtocolGRPC), Version: "test", ServiceName: "Claude Code", BatchBytes: 1}
	free.wire(p)
	if st, err := p.Pass(); err != nil || st.Paused != 0 || free.calls != 0 {
		t.Fatalf("no limit must never wait: paused %s over %d waits, err %v", st.Paused, free.calls, err)
	}
}

// A receiver asking to slow down stops the pass, marks nothing, names the
// wait it gave, and empties the budget; the next pass sends everything.
func TestThrottledReceiverStopsThePass(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			z, files := zoneWithOneSession(t)
			rcv := startReceiver(t)
			rcv.Throttle(true, 7*time.Second)
			clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
			p := &otlp.Pusher{Zone: z, Client: clientFor(t, rcv, protocol), Version: "test", ServiceName: "Claude Code",
				BatchBytes: 1, MaxBytesPerMinute: 1 << 20}
			clock.wire(p)
			st, err := p.Pass()
			if err != nil {
				t.Fatal(err)
			}
			if !st.Throttled || st.Files != 0 || st.Requests != 1 || len(st.Errors) != 1 {
				t.Fatalf("a throttled pass must stop at the first request and mark nothing: throttled=%v files=%d requests=%d errors=%v", st.Throttled, st.Files, st.Requests, st.Errors)
			}
			var throttled *otlp.Throttled
			if !errors.As(st.Errors[0], &throttled) {
				t.Fatalf("the error is not Throttled: %v", st.Errors[0])
			}
			if protocol == otlp.ProtocolHTTP && st.RetryAfter != 7*time.Second {
				t.Fatalf("retry after %s, the receiver said 7s", st.RetryAfter)
			}
			if protocol == otlp.ProtocolGRPC && st.RetryAfter != 0 {
				t.Fatalf("gRPC names no wait, got %s", st.RetryAfter)
			}
			rcv.Throttle(false, 0)
			st, err = p.Pass()
			if err != nil || len(st.Errors) != 0 || st.Files != len(files) {
				t.Fatalf("the next pass must send everything: files=%d errors=%v err=%v", st.Files, st.Errors, err)
			}
			if st.Paused == 0 {
				t.Fatal("after a throttle the budget is empty, so the next request must wait")
			}
		})
	}
}

// A pass goes session by session, the session landed first going first,
// each session's files followed by its rounds.
func TestPassSendsSessionsOldestFirstWithTheirRounds(t *testing.T) {
	z, _ := zoneWithOneSession(t)
	// A second session with a smaller name, landed later: name order would
	// put it first, landing order puts it second.
	dir := z.StreamDir("sess0", "main")
	path := filepath.Join(dir, storage.LandedName("transcript", storage.Stamp(time.Unix(0, 0)), 1))
	err := storage.WriteAtomic(path, storage.PermLanded, func(w io.Writer) error {
		hdr := &sessiondata.Header{Seq: 1, At: "2026-09-05T00:00:00Z", Kind: sessiondata.KindTranscript,
			Adapter: "test/0", Dialect: "test/1", Src: "-Users-me-proj/sess0.jsonl", Session: "sess0", Stream: "main"}
		sw, err := sessiondata.NewWriter(w, hdr)
		if err != nil {
			return err
		}
		if err := sw.Write(&sessiondata.Record{Ord: 1, Sha: "z", Bytes: 1, Time: "2026-09-05T01:00:00Z", Parts: []sessiondata.Part{{Kind: sessiondata.PartText, Text: "later", State: "available", Bytes: 5}}}); err != nil {
			return err
		}
		return sw.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
	round := filepath.Join(z.Root(), "_conversations", "sess0", "rounds", "r000001-000000000000.sf")
	if err := os.MkdirAll(filepath.Dir(round), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(round, []byte("{\"t\":\"header\",\"schema\":\"sf/1\",\"conversation\":\"sess0\",\"session\":\"sess0\",\"round\":1}\n{\"t\":\"commit\",\"digest\":\"000000000000\"}\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	rcv := startReceiver(t)
	p := &otlp.Pusher{Zone: z, Client: clientFor(t, rcv, otlp.ProtocolGRPC), Version: "test", ServiceName: "Claude Code", BatchBytes: 1}
	if st, err := p.Pass(); err != nil || len(st.Errors) != 0 {
		t.Fatal(err, st.Errors)
	}
	var order []string
	for _, r := range otlptest.Records(rcv.Requests()) {
		a := otlptest.Attrs(r.GetAttributes())
		order = append(order, a["asz.session"]+"/"+a["asz.format"])
	}
	want := []string{"sess1/sd", "sess1/sd", "sess1/sf", "sess0/sd", "sess0/sf"}
	if strings.Join(order, " ") != strings.Join(want, " ") {
		t.Fatalf("sent %v, want %v: the session landed first goes first, its rounds after its files", order, want)
	}
}
