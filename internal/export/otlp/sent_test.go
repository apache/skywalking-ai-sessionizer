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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	collmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"

	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// sentPaths is every file a push sends from zoneWithOneSession, by its path
// under the root, with its digest.
func sentPaths(t *testing.T, z *storage.Zone) map[string]string {
	t.Helper()
	files, err := storage.LandedFiles(z, "sess1")
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(z.Root(), "_conversations", "sess1", "rounds", "r000001-abcdefabcdef.sf")}
	for _, lf := range files {
		paths = append(paths, lf.Path)
	}
	out := map[string]string{}
	for _, p := range paths {
		rel, err := filepath.Rel(z.Root(), p)
		if err != nil {
			t.Fatal(err)
		}
		d, err := storage.FileDigest(p)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.ToSlash(rel)] = d
	}
	return out
}

// spool puts one metrics request in the root's spool, and returns its path
// under the root and its digest.
func spool(t *testing.T, z *storage.Zone) (string, string) {
	t.Helper()
	data, err := proto.Marshal(&collmetricspb.ExportMetricsServiceRequest{})
	if err != nil {
		t.Fatal(err)
	}
	path, err := storage.NewSpool(z).Put("otlp", data, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(z.Root(), path)
	if err != nil {
		t.Fatal(err)
	}
	d, err := storage.FileDigest(path)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.ToSlash(rel), d
}

// A receiver that takes a request and rejects some of its records has taken
// it, and the protocol says not to send it again. So its files are recorded
// as sent and as rejected. The rejected lines survive every later save, or
// the files would read as received whole after the next request.
func TestPushStateRecordsTheEndpointAndTheRejectedFiles(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			z, files := zoneWithOneSession(t)
			rcv := startReceiver(t)
			rcv.Reject(1)
			endpoint := otlp.EndpointOf(protocol, rcv.Options(protocol).Endpoint, false)
			p := &otlp.Pusher{Zone: z, Client: clientFor(t, rcv, protocol), Version: "test", ServiceName: "Claude Code", Endpoint: endpoint}
			st, err := p.Pass()
			if err != nil || len(st.Errors) != 0 || st.Files != len(files) || st.Rejected != 1 {
				t.Fatalf("a partial success: files=%d rejected=%d errors=%v err=%v; want %d files marked and 1 rejected",
					st.Files, st.Rejected, st.Errors, err, len(files))
			}
			first := sentPaths(t, z)
			sent, err := otlp.LoadSent(z.Root())
			if err != nil {
				t.Fatal(err)
			}
			if got := sent.Endpoints(); len(got) != 1 || got[0] != endpoint {
				t.Fatalf("endpoints %v, want [%s]", got, endpoint)
			}
			for rel, digest := range first {
				if d, ok := sent.Digest(rel); !ok || d != digest {
					t.Fatalf("%s: recorded %q (%v), want its digest %s", rel, d, ok, digest)
				}
				if !sent.Rejected(rel) || sent.SentTo(endpoint, rel, digest) {
					t.Fatalf("%s: a file of a request with rejected records must be recorded as rejected, never as sent whole", rel)
				}
			}
			data, err := os.ReadFile(filepath.Join(z.Root(), otlp.StateFile))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), "\nendpoint "+endpoint+"\n") {
				t.Fatalf("push.state carries no endpoint line for %s:\n%s", endpoint, data)
			}

			// A later request that rejects nothing saves push.state again.
			rcv.Reject(0)
			spoolRel, spoolDigest := spool(t, z)
			st, err = p.Pass()
			if err != nil || len(st.Errors) != 0 || st.Metrics != 1 {
				t.Fatalf("the second pass: metrics=%d errors=%v err=%v", st.Metrics, st.Errors, err)
			}
			if sent, err = otlp.LoadSent(z.Root()); err != nil {
				t.Fatal(err)
			}
			for rel := range first {
				if !sent.Rejected(rel) {
					t.Fatalf("%s: the rejected line was lost at the next save", rel)
				}
			}
			if sent.Rejected(spoolRel) || !sent.SentTo(endpoint, spoolRel, spoolDigest) {
				t.Fatalf("%s was taken whole, and must read as sent to %s", spoolRel, endpoint)
			}
			if sent.SentTo(endpoint, spoolRel, strings.Repeat("0", 64)) {
				t.Fatal("a different digest read as sent")
			}
		})
	}
}

// push.state records which receivers the files went to, not which file went
// to which. A root sent to two receivers proves nothing about either.
func TestSentToNeedsTheOneReceiverPushStateNames(t *testing.T) {
	z, files := zoneWithOneSession(t)
	rcv := startReceiver(t)
	a := otlp.EndpointOf(otlp.ProtocolGRPC, rcv.Options(otlp.ProtocolGRPC).Endpoint, false)
	p := &otlp.Pusher{Zone: z, Client: clientFor(t, rcv, otlp.ProtocolGRPC), Version: "test", ServiceName: "Claude Code", Endpoint: a}
	if st, err := p.Pass(); err != nil || st.Files != len(files) {
		t.Fatalf("files=%d err=%v", st.Files, err)
	}
	sent, err := otlp.LoadSent(z.Root())
	if err != nil {
		t.Fatal(err)
	}
	for rel, digest := range sentPaths(t, z) {
		if !sent.SentTo(a, rel, digest) {
			t.Fatalf("%s must read as sent to %s", rel, a)
		}
		if sent.SentTo("grpc://elsewhere:11800", rel, digest) || sent.SentTo("", rel, digest) {
			t.Fatalf("%s reads as sent to a receiver it never reached", rel)
		}
	}

	// The same root, sent on to a second receiver.
	b := otlp.EndpointOf(otlp.ProtocolHTTP, rcv.Options(otlp.ProtocolHTTP).Endpoint, false)
	spoolRel, spoolDigest := spool(t, z)
	p = &otlp.Pusher{Zone: z, Client: clientFor(t, rcv, otlp.ProtocolHTTP), Version: "test", ServiceName: "Claude Code", Endpoint: b}
	if st, err := p.Pass(); err != nil || st.Metrics != 1 {
		t.Fatalf("metrics=%d err=%v", st.Metrics, err)
	}
	if sent, err = otlp.LoadSent(z.Root()); err != nil {
		t.Fatal(err)
	}
	if got := sent.Endpoints(); len(got) != 2 {
		t.Fatalf("endpoints %v, want both receivers", got)
	}
	if sent.SentTo(b, spoolRel, spoolDigest) || sent.SentTo(a, spoolRel, spoolDigest) {
		t.Fatal("with two receivers recorded, which file reached which cannot be told")
	}
}

// A push.state in today's format, with pushed lines only, still loads. The
// receiver it names is unknown, so it proves nothing about any receiver. A
// pusher with no endpoint leaves the same.
func TestPushStateWithNoEndpointNamesTheReceiverUnknown(t *testing.T) {
	root := t.TempDir()
	today := "schema 1\nupdated_at 2026-09-10T00:00:00Z\n" +
		"pushed a/streams/main/x.sd 1111\npushed _conversations/a/rounds/r000001-aaaaaaaaaaaa.sf 2222\n"
	if err := os.WriteFile(filepath.Join(root, otlp.StateFile), []byte(today), 0o644); err != nil {
		t.Fatal(err)
	}
	sent, err := otlp.LoadSent(root)
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := sent.Digest("a/streams/main/x.sd"); !ok || d != "1111" {
		t.Fatalf("a push.state in today's format did not load: %q %v", d, ok)
	}
	if got := sent.Endpoints(); len(got) != 1 || got[0] != "unknown" {
		t.Fatalf("endpoints %v, want [unknown]", got)
	}
	if sent.SentTo("unknown", "a/streams/main/x.sd", "1111") || sent.SentTo("grpc://oap:11800", "a/streams/main/x.sd", "1111") {
		t.Fatal("a file sent to an unknown receiver reads as sent to one")
	}

	z, _ := zoneWithOneSession(t)
	rcv := startReceiver(t)
	p := &otlp.Pusher{Zone: z, Client: clientFor(t, rcv, otlp.ProtocolGRPC), Version: "test", ServiceName: "Claude Code"}
	if _, err := p.Pass(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(z.Root(), otlp.StateFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "\nendpoint ") {
		t.Fatalf("a pusher with no endpoint wrote one:\n%s", data)
	}
	if sent, err = otlp.LoadSent(z.Root()); err != nil {
		t.Fatal(err)
	}
	if got := sent.Endpoints(); len(got) != 1 || got[0] != "unknown" {
		t.Fatalf("endpoints %v, want [unknown]", got)
	}

	empty, err := otlp.LoadSent(t.TempDir())
	if err != nil || len(empty.Endpoints()) != 0 {
		t.Fatalf("a root that sent nothing: endpoints %v, err %v", empty.Endpoints(), err)
	}
	if _, ok := empty.Digest("a/streams/main/x.sd"); ok {
		t.Fatal("a root that sent nothing reads as having sent a file")
	}
}

// ForgetGone drops only the lines of the files it is told are owned and that
// are gone. Every other line, and every endpoint line, stays.
func TestForgetGoneDropsOnlyOwnedFilesThatAreGone(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a", "kept.sd"), []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	state := "schema 1\nupdated_at 2026-09-10T00:00:00Z\nendpoint grpc://oap:11800\n" +
		"pushed a/kept.sd d1\npushed a/gone.sd d2\npushed b/gone.sd d3\n" +
		"rejected a/gone.sd\nrejected b/gone.sd\n"
	if err := os.WriteFile(filepath.Join(root, otlp.StateFile), []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}
	owned := func(rel string) bool { return strings.HasPrefix(rel, "a/") }
	if err := otlp.ForgetGone(root, owned, time.Now()); err != nil {
		t.Fatal(err)
	}
	sent, err := otlp.LoadSent(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sent.Digest("a/kept.sd"); !ok {
		t.Fatal("the line of a file that is still there was dropped")
	}
	if _, ok := sent.Digest("a/gone.sd"); ok || sent.Rejected("a/gone.sd") {
		t.Fatal("the lines of an owned file that is gone stayed")
	}
	if _, ok := sent.Digest("b/gone.sd"); !ok || !sent.Rejected("b/gone.sd") {
		t.Fatal("the lines of a file nobody owns were dropped")
	}
	if got := sent.Endpoints(); len(got) != 1 || got[0] != "grpc://oap:11800" {
		t.Fatalf("endpoints %v, want the one recorded", got)
	}

	bare := t.TempDir()
	if err := otlp.ForgetGone(bare, owned, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(bare, otlp.StateFile)); !os.IsNotExist(err) {
		t.Fatalf("ForgetGone made a push.state in a root that had none: %v", err)
	}
}

// A receiver is named by its transport and address, so the same receiver
// reached the same way always has the same name.
func TestEndpointOfNamesTheReceiver(t *testing.T) {
	for _, c := range []struct {
		protocol, endpoint string
		tls                bool
		want               string
	}{
		{otlp.ProtocolGRPC, "oap:11800", false, "grpc://oap:11800"},
		{"", "oap:11800", false, "grpc://oap:11800"},
		{otlp.ProtocolGRPC, "oap:11800", true, "grpcs://oap:11800"},
		{otlp.ProtocolHTTP, "http://oap:12800/", false, "http://oap:12800"},
		{otlp.ProtocolHTTP, "https://oap:12800//", true, "https://oap:12800"},
		{otlp.ProtocolGRPC, "", false, ""},
	} {
		if got := otlp.EndpointOf(c.protocol, c.endpoint, c.tls); got != c.want {
			t.Errorf("EndpointOf(%q, %q, %v) = %q, want %q", c.protocol, c.endpoint, c.tls, got, c.want)
		}
	}
}
