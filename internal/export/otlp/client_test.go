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
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"

	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp"
)

// httpReceiver serves one handler, and returns an HTTP client for it.
func httpReceiver(t *testing.T, h http.HandlerFunc) otlp.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := otlp.NewClient(otlp.Options{Protocol: otlp.ProtocolHTTP, Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// An answer with no body is a full success whatever its type, since that is
// how a proxy in front of a receiver often answers.
func TestAnEmptyAnswerIsASuccessWhateverItsType(t *testing.T) {
	c := httpReceiver(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
	})
	if n, err := c.Export(&collogspb.ExportLogsServiceRequest{}); err != nil || n != 0 {
		t.Fatalf("logs: rejected %d, err %v; want a full success", n, err)
	}
	if n, err := c.ExportMetrics(&collmetricspb.ExportMetricsServiceRequest{}); err != nil || n != 0 {
		t.Fatalf("metrics: rejected %d, err %v; want a full success", n, err)
	}
}

// A protobuf answer is decoded under either name for the type, with any
// parameters ignored, and its partial success is counted.
func TestAProtobufAnswerGivesItsPartialSuccess(t *testing.T) {
	for _, media := range []string{"application/x-protobuf", "application/protobuf", "application/protobuf; charset=binary"} {
		c := httpReceiver(t, func(w http.ResponseWriter, r *http.Request) {
			var out []byte
			if r.URL.Path == "/v1/metrics" {
				out, _ = proto.Marshal(&collmetricspb.ExportMetricsServiceResponse{
					PartialSuccess: &collmetricspb.ExportMetricsPartialSuccess{RejectedDataPoints: 4}})
			} else {
				out, _ = proto.Marshal(&collogspb.ExportLogsServiceResponse{
					PartialSuccess: &collogspb.ExportLogsPartialSuccess{RejectedLogRecords: 3}})
			}
			w.Header().Set("Content-Type", media)
			_, _ = w.Write(out)
		})
		if n, err := c.Export(&collogspb.ExportLogsServiceRequest{}); err != nil || n != 3 {
			t.Fatalf("%s: logs rejected %d, err %v; want 3", media, n, err)
		}
		if n, err := c.ExportMetrics(&collmetricspb.ExportMetricsServiceRequest{}); err != nil || n != 4 {
			t.Fatalf("%s: metrics rejected %d, err %v; want 4", media, n, err)
		}
	}
}

// A page answered with 200, by a fallback route or a login page, is not an
// OTLP response. It is an error, and a pass marks nothing sent, so the files
// go again once the receiver answers properly.
func TestAPageAnsweredWith200IsNotASuccess(t *testing.T) {
	c := httpReceiver(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body>Sign in</body></html>"))
	})
	_, err := c.Export(&collogspb.ExportLogsServiceRequest{})
	if err == nil || !strings.Contains(err.Error(), "with text/html, not an OTLP response") {
		t.Fatalf("an HTML page answered with 200 gave %v; want an error naming text/html", err)
	}

	z, _ := zoneWithOneSession(t)
	p := &otlp.Pusher{Zone: z, Client: c, Version: "test", ServiceName: "Claude Code", Endpoint: "http://receiver"}
	st, err := p.Pass()
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 0 || len(st.Errors) == 0 {
		t.Fatalf("files=%d errors=%v; want nothing marked and the error reported", st.Files, st.Errors)
	}
	sent, err := otlp.LoadSent(z.Root())
	if err != nil {
		t.Fatal(err)
	}
	for rel := range sentPaths(t, z) {
		if _, ok := sent.Digest(rel); ok {
			t.Fatalf("%s was recorded as sent", rel)
		}
	}
}

// A success whose body is cut off before it all arrives is not a success:
// what the receiver took is unknown. It is an error, whatever the type, and
// a pass marks nothing sent, so the files go again.
func TestASuccessCutOffBeforeItsBodyIsNotASuccess(t *testing.T) {
	for _, media := range []string{"application/x-protobuf", "text/html"} {
		t.Run(media, func(t *testing.T) {
			// The answer promises 100 bytes, and the connection closes
			// before the first of them.
			c := httpReceiver(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				conn, buf, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: " + media + "\r\nContent-Length: 100\r\n\r\n")
				_ = buf.Flush()
				_ = conn.Close()
			})
			if _, err := c.Export(&collogspb.ExportLogsServiceRequest{}); err == nil || !strings.Contains(err.Error(), "did not arrive whole") {
				t.Fatalf("logs: a cut answer gave %v; want an error saying its body did not arrive whole", err)
			}
			if _, err := c.ExportMetrics(&collmetricspb.ExportMetricsServiceRequest{}); err == nil || !strings.Contains(err.Error(), "did not arrive whole") {
				t.Fatalf("metrics: a cut answer gave %v; want an error saying its body did not arrive whole", err)
			}

			z, _ := zoneWithOneSession(t)
			p := &otlp.Pusher{Zone: z, Client: c, Version: "test", ServiceName: "Claude Code", Endpoint: "http://receiver"}
			st, err := p.Pass()
			if err != nil {
				t.Fatal(err)
			}
			if st.Files != 0 || len(st.Errors) == 0 {
				t.Fatalf("files=%d errors=%v; want nothing marked and the error reported", st.Files, st.Errors)
			}
			sent, err := otlp.LoadSent(z.Root())
			if err != nil {
				t.Fatal(err)
			}
			for rel := range sentPaths(t, z) {
				if _, ok := sent.Digest(rel); ok {
					t.Fatalf("%s was recorded as sent", rel)
				}
			}
		})
	}
}

// The client reads at most 1 MiB of an answer, far more than any OTLP
// response holds. An answer cut off just after that much is not a success
// either, though what arrived is a whole OTLP response, and nor is a whole
// answer longer than that: what came after the first MiB was never read.
func TestASuccessLongerThanTheClientReadsIsNotASuccess(t *testing.T) {
	const limit = 1 << 20
	logs := &collogspb.ExportLogsServiceResponse{PartialSuccess: &collogspb.ExportLogsPartialSuccess{}}
	for n := limit - 16; proto.Size(logs) != limit; n++ {
		logs.PartialSuccess.ErrorMessage = strings.Repeat("x", n)
	}
	prefix, err := proto.Marshal(logs)
	if err != nil || len(prefix) != limit {
		t.Fatalf("a response of %d bytes, %v", len(prefix), err)
	}
	for name, promised := range map[string]int{"cut off after the limit": limit + 100, "longer than the limit": limit + 1} {
		t.Run(name, func(t *testing.T) {
			c := httpReceiver(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				conn, buf, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_, _ = fmt.Fprintf(buf, "HTTP/1.1 200 OK\r\nContent-Type: application/x-protobuf\r\nContent-Length: %d\r\n\r\n", promised)
				_, _ = buf.Write(prefix)
				if promised == limit+1 {
					_ = buf.WriteByte(0)
				}
				_ = buf.Flush()
				_ = conn.Close()
			})
			if _, err := c.Export(&collogspb.ExportLogsServiceRequest{}); err == nil {
				t.Fatal("logs: the answer was taken as a success")
			}
			if _, err := c.ExportMetrics(&collmetricspb.ExportMetricsServiceRequest{}); err == nil {
				t.Fatal("metrics: the answer was taken as a success")
			}
		})
	}
}

// A redirect is not followed. A receiver that moved is configured again, and
// a redirect to a login page, followed, would answer 200 with a page.
func TestARedirectIsNotFollowed(t *testing.T) {
	var followed atomic.Int32
	c := httpReceiver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			followed.Add(1)
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>login</html>"))
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	})
	if _, err := c.Export(&collogspb.ExportLogsServiceRequest{}); err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("a redirect gave %v; want an error naming the 302", err)
	}
	if n := followed.Load(); n != 0 {
		t.Fatalf("the redirect's target was requested %d times", n)
	}
}

// The client keeps its own connections. Closing it leaves another client's
// idle connection open, and closing the default transport's idle connections,
// as an httptest server does when it closes, leaves the client's open: the
// receiver sees each go on over the connection it had.
func TestTheClientKeepsItsOwnConnections(t *testing.T) {
	var mu sync.Mutex
	var from []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		from = append(from, r.RemoteAddr)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	last := func() string {
		mu.Lock()
		defer mu.Unlock()
		return from[len(from)-1]
	}
	other := &http.Client{Transport: http.DefaultTransport}
	get := func() string {
		t.Helper()
		resp, err := other.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return last()
	}
	export := func(c otlp.Client) string {
		t.Helper()
		if _, err := c.Export(&collogspb.ExportLogsServiceRequest{}); err != nil {
			t.Fatal(err)
		}
		return last()
	}

	before := get()
	c, err := otlp.NewClient(otlp.Options{Protocol: otlp.ProtocolHTTP, Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	export(c)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if after := get(); after != before {
		t.Fatalf("closing the client closed another client's connection: %s, then %s", before, after)
	}

	c, err = otlp.NewClient(otlp.Options{Protocol: otlp.ProtocolHTTP, Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	first := export(c)
	http.DefaultTransport.(interface{ CloseIdleConnections() }).CloseIdleConnections()
	if again := export(c); again != first {
		t.Fatalf("closing the default transport's idle connections closed the client's: %s, then %s", first, again)
	}
}
