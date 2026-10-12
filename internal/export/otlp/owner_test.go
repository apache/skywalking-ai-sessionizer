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
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"

	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp"
	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp/otlptest"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// watching passes every request on, and first reads, from disk, the owner
// push.state records for one session: what a crash at that moment leaves.
type watching struct {
	otlp.Client
	root, session string
	seen          []string
}

func (w *watching) Export(req *collogspb.ExportLogsServiceRequest) (int64, error) {
	w.look()
	return w.Client.Export(req)
}

func (w *watching) ExportMetrics(req *collmetricspb.ExportMetricsServiceRequest) (int64, error) {
	w.look()
	return w.Client.ExportMetrics(req)
}

func (w *watching) look() {
	sent, err := otlp.LoadSent(w.root)
	if err != nil {
		w.seen = append(w.seen, "error: "+err.Error())
		return
	}
	w.seen = append(w.seen, sent.Owner(w.session))
}

// ownerTest is one root and one receiver, and a pusher for any owner.
type ownerTest struct {
	t        *testing.T
	z        *storage.Zone
	rcv      *otlptest.Receiver
	endpoint string
}

func newOwnerTest(t *testing.T) *ownerTest {
	z, _ := zoneWithOneSession(t)
	rcv := startReceiver(t)
	o := rcv.Options(otlp.ProtocolGRPC)
	return &ownerTest{t: t, z: z, rcv: rcv, endpoint: otlp.EndpointOf(o.Protocol, o.Endpoint, false)}
}

func (o *ownerTest) pusher(service, instance string) *otlp.Pusher {
	return &otlp.Pusher{Zone: o.z, Client: clientFor(o.t, o.rcv, otlp.ProtocolGRPC), Endpoint: o.endpoint, Version: "test",
		ServiceName: service, MetricsService: "Claude Code", Layer: "AI_AGENT", InstanceID: instance, BatchBytes: 1 << 20,
		Runtimes: map[string]string{"test": "Test Agent"}}
}

func (o *ownerTest) pass(service, instance string) *otlp.Stats {
	o.t.Helper()
	st, err := o.pusher(service, instance).Pass()
	if err != nil {
		o.t.Fatal(err)
	}
	return st
}

func (o *ownerTest) sent() *otlp.Sent {
	o.t.Helper()
	sent, err := otlp.LoadSent(o.z.Root())
	if err != nil {
		o.t.Fatal(err)
	}
	return sent
}

// land writes one landed transcript file of one record for a session.
func (o *ownerTest) land(session string, seq uint64) string {
	o.t.Helper()
	path := filepath.Join(o.z.StreamDir(session, "main"), storage.LandedName("transcript", storage.Stamp(time.Unix(int64(seq), 0)), seq))
	err := storage.WriteAtomic(path, storage.PermLanded, func(w io.Writer) error {
		hdr := &sessiondata.Header{Seq: seq, At: "2026-09-04T00:00:00Z", Kind: sessiondata.KindTranscript,
			Adapter: "test/0", Dialect: "test/1", Src: session + ".jsonl", Session: session, Stream: "main"}
		sw, err := sessiondata.NewWriter(w, hdr)
		if err != nil {
			return err
		}
		if err := sw.Write(&sessiondata.Record{Ord: 1, Sha: fmt.Sprint(seq), Bytes: 1,
			Parts: []sessiondata.Part{{Kind: sessiondata.PartText, Text: "hi", State: "available", Bytes: 2}}}); err != nil {
			return err
		}
		return sw.Close()
	})
	if err != nil {
		o.t.Fatal(err)
	}
	return o.rel(path)
}

// round writes round n of a conversation whose header names session.
func (o *ownerTest) round(conv, session string, n int) string {
	o.t.Helper()
	path := filepath.Join(o.z.Root(), "_conversations", conv, "rounds", fmt.Sprintf("r%06d-abcdefabcdef.sf", n))
	text := fmt.Sprintf("{\"t\":\"header\",\"schema\":\"sf/1\",\"conversation\":%q,\"session\":%q,\"round\":%d}\n", conv, session, n) +
		"{\"t\":\"node\",\"id\":\"n1\",\"kind\":\"talk\"}\n" +
		"{\"t\":\"commit\",\"digest\":\"abcdefabcdef\",\"counts\":{\"nodes\":1}}\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		o.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o444); err != nil {
		o.t.Fatal(err)
	}
	return o.rel(path)
}

// spool puts one metrics request derived from a session.
func (o *ownerTest) spool(session string, seq int) string {
	o.t.Helper()
	name := fmt.Sprintf("metrics-%s-%06d-local.pb", session, seq)
	if _, _, err := storage.NewSpool(o.z).PutNamed(name, []byte{}); err != nil {
		o.t.Fatal(err)
	}
	return storage.SpoolDir + "/" + name
}

func (o *ownerTest) rel(path string) string {
	rel, err := filepath.Rel(o.z.Root(), path)
	if err != nil {
		o.t.Fatal(err)
	}
	return filepath.ToSlash(rel)
}

// sessionsSent lists the sessions the receiver got log records of.
func (o *ownerTest) sessionsSent() []string {
	seen := map[string]bool{}
	for _, r := range otlptest.Records(o.rcv.Requests()) {
		seen[otlptest.Attrs(r.Attributes)["asz.session"]] = true
	}
	var out []string
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// The owner is on disk before every request that carries the session goes
// out, the logs and, when only metrics are sent, the metrics as well.
func TestTheOwnerIsSavedBeforeEachRequest(t *testing.T) {
	for _, logs := range []bool{true, false} {
		t.Run(fmt.Sprintf("logs=%v", logs), func(t *testing.T) {
			o := newOwnerTest(t)
			o.spool("sess1", 2)
			p := o.pusher("Claude Code", "sender-1")
			p.NoLogs = !logs
			w := &watching{Client: p.Client, root: o.z.Root(), session: "sess1"}
			p.Client = w
			st, err := p.Pass()
			if err != nil || len(st.Errors) != 0 {
				t.Fatalf("pass: %v, %v", err, st.Errors)
			}
			want := otlp.OwnerOf(o.endpoint, "Claude Code", "sender-1")
			if len(w.seen) == 0 {
				t.Fatal("nothing was sent")
			}
			for i, got := range w.seen {
				if got != want {
					t.Fatalf("request %d went out with the owner on disk %q, want %q", i+1, got, want)
				}
			}
		})
	}
}

// A save of the owner that fails sends nothing of the session, logs or
// metrics, and leaves no owner behind in memory for a later request to trust.
func TestAFailedOwnerSaveSendsNothing(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("a read-only directory does not stop this user writing")
	}
	o := newOwnerTest(t)
	o.spool("sess1", 2)
	// The export lock's directory exists before the root is made read-only,
	// so the pass gets as far as saving push.state.
	l, err := storage.LockExport(o.z.Root())
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Unlock()
	if err := os.Chmod(o.z.Root(), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(o.z.Root(), 0o755) })
	st := o.pass("Claude Code", "sender-1")
	if len(st.Errors) == 0 {
		t.Fatal("a pass that could not save push.state reported no error")
	}
	if n, m := len(o.rcv.Requests()), len(o.rcv.MetricsRequests()); n != 0 || m != 0 {
		t.Fatalf("%d logs and %d metrics request(s) went out with no owner on disk", n, m)
	}
	if _, err := os.Stat(filepath.Join(o.z.Root(), otlp.StateFile)); !os.IsNotExist(err) {
		t.Fatalf("push.state: %v", err)
	}
}

// A held session sends nothing: not its landed files, not a round that
// names it from another conversation's directory, not its metrics. A request
// that carries other sessions leaves its files out, and nothing of it is
// recorded as sent, so it all goes once its owner is set back.
func TestAHeldSessionWaitsWhole(t *testing.T) {
	o := newOwnerTest(t)
	if st := o.pass("Claude Code", "sender-1"); len(st.Errors) != 0 || st.Files == 0 {
		t.Fatalf("first pass: %+v", st)
	}
	waiting := []string{o.land("sess1", 3), o.round("assembled", "sess1", 1), o.spool("sess1", 3)}
	other := o.land("sess2", 1)
	o.rcv.Reset()

	st := o.pass("Claude Code", "sender-2")
	if len(st.Held) != 1 || st.Held[0].Session != "sess1" {
		t.Fatalf("held %+v, want sess1 once", st.Held)
	}
	if got := o.sessionsSent(); len(got) != 1 || got[0] != "sess2" {
		t.Fatalf("the receiver got records of %v, want sess2 only", got)
	}
	if len(o.rcv.MetricsRequests()) != 0 {
		t.Fatal("a held session's metrics went out")
	}
	sent := o.sent()
	for _, rel := range waiting {
		if _, ok := sent.Digest(rel); ok {
			t.Fatalf("%s of the held session is recorded as sent", rel)
		}
	}
	if _, ok := sent.Digest(other); !ok {
		t.Fatalf("%s is not recorded as sent", other)
	}
	if got, want := sent.Owner("sess2"), otlp.OwnerOf(o.endpoint, "Claude Code", "sender-2"); got != want {
		t.Fatalf("sess2's owner is %q, want %q", got, want)
	}

	o.rcv.Reset()
	st = o.pass("Claude Code", "sender-1")
	if len(st.Held) != 1 || st.Held[0].Session != "sess2" {
		t.Fatalf("back under sender-1: held %+v, want sess2 once", st.Held)
	}
	sent = o.sent()
	for _, rel := range waiting {
		if _, ok := sent.Digest(rel); !ok {
			t.Fatalf("%s did not go once its owner was set back", rel)
		}
	}
	if got := o.sessionsSent(); len(got) != 1 || got[0] != "sess1" {
		t.Fatalf("the receiver got records of %v, want sess1 only", got)
	}
}

// A push.state a build before owners wrote has files and no owner lines.
// The first pass that loads it gives its owner to every session the file
// holds files of, before it sends anything and even when it sends nothing:
// a landed session, a conversation with no session directory, and the
// session a sent round's header names, whatever its conversation is called.
// It marks the file, so this happens once, and holds them from then on.
func TestSessionsSentBeforeOwnersTakeTheFirstPassOwner(t *testing.T) {
	o := newOwnerTest(t)
	o.round("lonely", "lonely", 1)
	o.round("assembled", "sess9", 1)
	// Two conversations whose names collide with the sessions their
	// neighbours' rounds name.
	o.round("alpha", "beta", 1)
	o.round("beta", "gamma", 1)
	o.pass("Claude Code", "sender-1")
	path := filepath.Join(o.z.Root(), otlp.StateFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "\nowners recorded\n") {
		t.Fatalf("a push.state this build wrote does not say owners are recorded:\n%s", data)
	}
	var kept []string
	for _, line := range strings.SplitAfter(string(data), "\n") {
		if !strings.HasPrefix(line, "owner") {
			kept = append(kept, line)
		}
	}
	if err := os.WriteFile(path, []byte(strings.Join(kept, "")), 0o644); err != nil {
		t.Fatal(err)
	}

	o.rcv.Reset()
	if st := o.pass("Claude Code", "sender-2"); st.Files != 0 || len(o.rcv.Requests()) != 0 {
		t.Fatalf("a pass with nothing new sent %d file(s)", st.Files)
	}
	want := otlp.OwnerOf(o.endpoint, "Claude Code", "sender-2")
	for _, s := range []string{"sess1", "lonely", "sess9", "beta", "gamma"} {
		if got := o.sent().Owner(s); got != want {
			t.Fatalf("the earlier session %s's owner is %q, want %q", s, got, want)
		}
	}
	// Once is once: a later pass with nothing to send does not adopt again,
	// so it leaves push.state as it was.
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	p := o.pusher("Claude Code", "sender-2")
	p.Now = func() time.Time { return time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) }
	if _, err := p.Pass(); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatalf("an idle pass after the adoption rewrote push.state:\n%s", after)
	}
	o.land("sess1", 3)
	o.round("lonely", "lonely", 2)
	o.round("assembled", "sess9", 2)
	o.round("beta", "gamma", 2)
	st := o.pass("Claude Code", "sender-3")
	var held []string
	for _, h := range st.Held {
		held = append(held, h.Session)
	}
	sort.Strings(held)
	if strings.Join(held, ",") != "gamma,lonely,sess1,sess9" {
		t.Fatalf("held %v, want gamma, lonely, sess1 and sess9", held)
	}
}

// The one adoption reads every sent round's header. One it cannot read
// fails the pass before anything is sent and leaves push.state unmarked, so
// the next pass adopts again rather than leave the round's session unowned.
func TestAnUnreadableRoundStopsTheAdoption(t *testing.T) {
	o := newOwnerTest(t)
	rel := o.round("assembled", "sess9", 1)
	o.pass("Claude Code", "sender-1")
	path := filepath.Join(o.z.Root(), otlp.StateFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, line := range strings.SplitAfter(string(data), "\n") {
		if !strings.HasPrefix(line, "owner") {
			kept = append(kept, line)
		}
	}
	legacy := strings.Join(kept, "")
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	round := filepath.Join(o.z.Root(), filepath.FromSlash(rel))
	good, err := os.ReadFile(round)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(round, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(round, []byte("not a header\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o.land("sess1", 3)
	o.rcv.Reset()
	if _, err := o.pusher("Claude Code", "sender-2").Pass(); err == nil || !strings.Contains(err.Error(), "header cannot be read") {
		t.Fatalf("a pass over an unreadable sent round returned %v", err)
	}
	if len(o.rcv.Requests()) != 0 {
		t.Fatal("a pass that could not adopt sent something")
	}
	if now, _ := os.ReadFile(path); string(now) != legacy {
		t.Fatalf("a pass that could not adopt changed push.state:\n%s", now)
	}
	if err := os.WriteFile(round, good, 0o644); err != nil {
		t.Fatal(err)
	}
	o.pass("Claude Code", "sender-2")
	if got, want := o.sent().Owner("sess9"), otlp.OwnerOf(o.endpoint, "Claude Code", "sender-2"); got != want {
		t.Fatalf("once the round could be read, sess9's owner is %q, want %q", got, want)
	}
}

// The owner names the configured service, empty when asz names each
// session's service by its runtime, never the name a session resolves to: a
// round of a conversation with no session directory and a session whose
// transcript has not landed resolve to other names, and are not held for it.
func TestTheOwnerNamesTheConfiguredService(t *testing.T) {
	o := newOwnerTest(t)
	o.round("lonely", "lonely", 1)
	o.pass("", "sender-1")
	want := otlp.OwnerOf(o.endpoint, "", "sender-1")
	for _, s := range []string{"sess1", "lonely"} {
		if got := o.sent().Owner(s); got != want {
			t.Fatalf("%s's owner is %q, want %q", s, got, want)
		}
	}
	o.land("lonely", 1)
	o.round("lonely", "lonely", 2)
	if st := o.pass("", "sender-1"); len(st.Held) != 0 || len(st.Errors) != 0 {
		t.Fatalf("a second pass under the same configuration held %+v: %v", st.Held, st.Errors)
	}
	if st := o.pass("Claude Code", "sender-1"); len(st.Held) == 0 {
		t.Fatal("a configured service name did not hold the sessions sent without one")
	}
}

// A held session holds its own files only. A round in its conversation's
// directory goes by the session its header names, so it is not stranded
// between two owners when that session belongs to another.
func TestARoundGoesByItsHeaderSession(t *testing.T) {
	o := newOwnerTest(t)
	o.pass("Claude Code", "sender-1")
	o.land("sessT", 1)
	if st := o.pass("Claude Code", "sender-2"); len(st.Held) != 1 || st.Held[0].Session != "sess1" {
		t.Fatalf("held %+v, want sess1", st.Held)
	}
	rel := o.round("sess1", "sessT", 2)
	st := o.pass("Claude Code", "sender-2")
	if len(st.Held) != 1 || st.Held[0].Session != "sess1" {
		t.Fatalf("held %+v, want sess1 once", st.Held)
	}
	if _, ok := o.sent().Digest(rel); !ok {
		t.Fatalf("%s names sessT, owned by sender-2, and was not sent by it", rel)
	}
}

// A metrics request that cannot be decoded sends nothing, and gives its
// session no owner: nothing of it was tried.
func TestAnUnreadableRequestGivesNoOwner(t *testing.T) {
	o := newOwnerTest(t)
	name := "metrics-sessM-000001-local.pb"
	if _, _, err := storage.NewSpool(o.z).PutNamed(name, []byte("not a request")); err != nil {
		t.Fatal(err)
	}
	p := o.pusher("Claude Code", "sender-1")
	p.NoLogs = true
	st, err := p.Pass()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Errors) == 0 || len(o.rcv.MetricsRequests()) != 0 {
		t.Fatalf("a request that cannot be decoded: errors %v, %d sent", st.Errors, len(o.rcv.MetricsRequests()))
	}
	if owner := o.sent().Owner("sessM"); owner != "" {
		t.Fatalf("a session nothing was tried for has the owner %q", owner)
	}
}
