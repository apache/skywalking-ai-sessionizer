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

package judge

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
)

// TestMCPServerStandIn is a local MCP server when ASZ_MCP_STAND_IN is set:
// it speaks the modern revision over its standard input and output. With
// "fail" it writes what ASZ_MCP_STAND_IN_STDERR holds on its standard error
// and ends before it answers the call; otherwise it lists one tool and
// answers a call with the text the call's key names.
func TestMCPServerStandIn(_ *testing.T) {
	mode := os.Getenv("ASZ_MCP_STAND_IN")
	if mode == "" {
		return
	}
	in, out := bufio.NewReader(os.Stdin), os.Stdout
	answer := func(id json.RawMessage, result any) {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
		out.Write(append(b, '\n'))
	}
	for {
		line, err := in.ReadBytes('\n')
		if err != nil {
			os.Exit(0)
		}
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.Unmarshal(line, &m)
		switch m.Method {
		case "server/discover":
			answer(m.ID, map[string]any{"supportedVersions": []string{"2026-07-28"}})
		case "tools/list":
			answer(m.ID, map[string]any{"tools": []any{map[string]any{"name": "jira_get_issue", "description": "Get a Jira issue",
				"inputSchema": map[string]any{"type": "object"}}}})
		case "tools/call":
			if mode == "fail" {
				fmt.Fprint(os.Stderr, os.Getenv("ASZ_MCP_STAND_IN_STDERR"))
				os.Exit(4)
			}
			answer(m.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": fmt.Sprintf("issue %v", m.Params.Arguments["key"])}}})
		}
	}
}

// mcpFake is a remote MCP server of revision 2026-07-28, or, with a
// session, of 2025-11-25, which answers a call with result and records the
// calls it took.
type mcpFake struct {
	session string
	result  func(args map[string]any) map[string]any
	// refusal, when set, is the message of a JSON-RPC error a call is
	// answered with instead.
	refusal string
	mu      sync.Mutex
	calls   []map[string]any
	headers []http.Header
}

func (f *mcpFake) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.Unmarshal(b, &m)
		reply := func(v map[string]any) {
			w.Header().Set("Content-Type", "application/json")
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": v})
			w.Write(b)
		}
		legacy := r.Header.Get("MCP-Protocol-Version") == "2025-11-25"
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusMethodNotAllowed)
		case f.session != "" && m.Method == "initialize":
			w.Header().Set("Mcp-Session-Id", f.session)
			reply(map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}})
		case f.session != "" && m.Method == "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case f.session != "" && !legacy:
			w.WriteHeader(http.StatusBadRequest)
		case m.Method == "tools/list":
			reply(map[string]any{"tools": []any{map[string]any{"name": "get", "description": "Get an issue", "inputSchema": map[string]any{"type": "object"}},
				map[string]any{"name": "bad", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{
					"a": map[string]any{"type": "number", "x-mcp-header": "A"}}}}}})
		case m.Method == "tools/call" && f.refusal != "":
			w.Header().Set("Content-Type", "application/json")
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32603, "message": f.refusal}})
			w.Write(b)
		case m.Method == "tools/call":
			f.mu.Lock()
			f.calls, f.headers = append(f.calls, m.Params.Arguments), append(f.headers, r.Header.Clone())
			f.mu.Unlock()
			reply(f.result(m.Params.Arguments))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// throughMCP fetches one source through an MCP entry.
func throughMCP(t *testing.T, mc config.MCPFetch, src string) savedSource {
	t.Helper()
	cfg := config.References{MaxBytes: 1 << 20, Timeout: "20s", Fetch: []config.Fetcher{{Prefix: "https://jira.example/browse/", MCP: &mc}}}
	return fetched(t, cfg, src)[0]
}

// leaks says which of some secrets a saved source keeps anywhere: its text,
// its fetched copy, its manifest and the frames the reference stage writes.
func leaks(s savedSource, secrets ...string) []string {
	frames, _ := json.Marshal(sourceFrames(s))
	all := s.text + "\n" + string(s.data) + "\n" + string(frames)
	var out []string
	for _, x := range secrets {
		if strings.Contains(all, x) {
			out = append(out, x)
		}
	}
	return out
}

// A source fetched through a remote MCP server keeps what was sent and how,
// never a secret: its URL without the query, the server without its path,
// the arguments with a secret one named by its variable. Its text is the
// text blocks joined, a credential redacted however it was written: plain,
// escaped inside JSON, decoded from Basic, or a number the result holds.
func TestAnMCPSourceIsSavedWithoutItsSecrets(t *testing.T) {
	t.Setenv("ASZ_TEST_AUTH", "Basic "+base64.StdEncoding.EncodeToString([]byte("me@example.com:tok-123")))
	t.Setenv("ASZ_TEST_PIN", "12345")
	f := &mcpFake{result: func(map[string]any) map[string]any {
		return map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "Summary: OPS-1, by me@example.com"},
			map[string]any{"type": "text", "text": `{"auth":"tok\u002d123","m":1.50}`},
			map[string]any{"type": "image", "data": "iVBORw0KGgo=", "mimeType": "image/png"},
			map[string]any{"type": "text", "text": ""}},
			"structuredContent": map[string]any{"token": "tok-123", "pin": json.Number("12345"), "m": json.Number("1.50")}}
	}}
	srv := f.start(t)
	s := throughMCP(t, config.MCPFetch{URL: srv.URL + "/mcp/hidden-path", Headers: map[string]string{"Authorization": "ASZ_TEST_AUTH"}, Tool: "get",
		Arguments: map[string]any{"issue": "{rest}", "whole": "{source}", "limit": 100}, ArgumentsEnv: map[string]string{"pin": "ASZ_TEST_PIN"}},
		"https://jira.example/browse/OPS-1?token=q1#frag")
	m := s.manifest
	if m.Outcome != sessionjudge.SourceSaved || m.Method != sessionjudge.MethodMCP || m.Source != "https://jira.example/browse/OPS-1" || !m.QueryRemoved {
		t.Fatalf("manifest %+v", m)
	}
	if x := m.MCP; x == nil || x.Server != srv.URL || x.Era != "modern" || x.Tool != "get" || x.BlocksOmitted != 1 || !x.StructuredOmitted || x.Redactions == 0 ||
		x.Arguments["issue"] != "OPS-1" || x.Arguments["whole"] != "https://jira.example/browse/OPS-1" || x.Arguments["pin"] != "[env ASZ_TEST_PIN]" {
		t.Fatalf("mcp %+v", m.MCP)
	}
	if got := f.calls[0]; got["issue"] != "OPS-1" || got["pin"] != "12345" || f.headers[0].Get("Authorization") != os.Getenv("ASZ_TEST_AUTH") {
		t.Fatalf("the server took %v %v", got, f.headers[0])
	}
	if s.text != "Summary: OPS-1, by [redacted]\n\n{\"auth\":\"[redacted]\",\"m\":1.50}" {
		t.Fatalf("text %q", s.text)
	}
	if !json.Valid(s.data) || !strings.Contains(string(s.data), `"m":1.50`) || !strings.Contains(string(s.data), `"pin":"[redacted]"`) {
		t.Fatalf("fetched %s", s.data)
	}
	if l := leaks(s, "tok-123", "me@example.com", "12345", `tok\u002d123`, "hidden-path", "token=q1", "frag"); len(l) > 0 {
		t.Fatalf("kept %v", l)
	}
}

// Whether a block is text is read before anything is redacted, so a Basic
// user named text changes nothing; structured content alone is the text, a
// secret that holds a quote redacted in it.
func TestStructuredContentAloneIsTheText(t *testing.T) {
	t.Setenv("ASZ_TEST_AUTH", "Basic "+base64.StdEncoding.EncodeToString([]byte("text:pw-9")))
	t.Setenv("ASZ_TEST_QUOTE", `a"b`)
	f := &mcpFake{result: func(map[string]any) map[string]any {
		return map[string]any{"structuredContent": map[string]any{"k": `a"b`, "pw": "pw-9"}}
	}}
	srv := f.start(t)
	mc := config.MCPFetch{URL: srv.URL, Headers: map[string]string{"Authorization": "ASZ_TEST_AUTH"}, Tool: "get", ArgumentsEnv: map[string]string{"q": "ASZ_TEST_QUOTE"}}
	s := throughMCP(t, mc, "https://jira.example/browse/OPS-2")
	if s.manifest.Outcome != sessionjudge.SourceSaved || s.text != `{"k":"[redacted]","pw":"[redacted]"}` || s.manifest.MCP.StructuredOmitted {
		t.Fatalf("%+v %q", s.manifest, s.text)
	}
	f.result = func(map[string]any) map[string]any {
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": "the text"}}}
	}
	if s := throughMCP(t, mc, "https://jira.example/browse/OPS-2"); s.text != "the [redacted]" {
		t.Fatalf("a text block, its user named text: %q", s.text)
	}
	// A secret of characters longer than a byte, beside what is safe.
	t.Setenv("ASZ_TEST_QUOTE", "café-9")
	f.result = func(map[string]any) map[string]any {
		return map[string]any{"structuredContent": map[string]any{"k": "café-9", "safe": "ok"}}
	}
	if s := throughMCP(t, mc, "https://jira.example/browse/OPS-2"); s.text != `{"k":"[redacted]","safe":"ok"}` || strings.Contains(string(s.data), "café") {
		t.Fatalf("a secret not in ASCII: %q %s", s.text, s.data)
	}
}

// A secret that only shows once the text blocks are joined by a blank line
// is caught by the last pass over the joined text, and counted.
func TestASecretJoinedFromTwoBlocksIsCaught(t *testing.T) {
	t.Setenv("ASZ_TEST_SPLIT", "part-a\n\npart-b")
	f := &mcpFake{result: func(map[string]any) map[string]any {
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": "ends part-a"}, map[string]any{"type": "text", "text": "part-b starts"}}}
	}}
	srv := f.start(t)
	s := throughMCP(t, config.MCPFetch{URL: srv.URL, Tool: "get", ArgumentsEnv: map[string]string{"k": "ASZ_TEST_SPLIT"}}, "https://jira.example/browse/OPS-5")
	if s.text != "ends [redacted] starts" || s.manifest.MCP.Redactions != 1 || LeftOut(s.manifest) != "1 secret redacted" {
		t.Fatalf("%q %+v", s.text, s.manifest.MCP)
	}
}

// A session a legacy server gave is a secret too, and a proxy's password.
func TestASessionAndAProxyPasswordAreSecrets(t *testing.T) {
	f := &mcpFake{session: "sess-4242", result: func(map[string]any) map[string]any {
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": "session sess-4242 through pw-proxy"}}}
	}}
	srv := f.start(t)
	old := proxyFor
	proxyFor = func(*http.Request) (*url.URL, error) {
		return url.Parse("http://proxy-user:pw-proxy@proxy.example:3128")
	}
	t.Cleanup(func() { proxyFor = old })
	s := throughMCP(t, config.MCPFetch{URL: srv.URL, Tool: "get"}, "https://jira.example/browse/OPS-3")
	if s.manifest.MCP.Era != "legacy" || s.text != "session [redacted] through [redacted]" {
		t.Fatalf("%+v %q", s.manifest.MCP, s.text)
	}
	// A failure that names the session keeps it no more than a result.
	f.refusal = "session sess-4242 expired"
	if m := throughMCP(t, config.MCPFetch{URL: srv.URL, Tool: "get"}, "https://jira.example/browse/OPS-3").manifest; m.Error != "server error: JSON-RPC -32603: session [redacted] expired" {
		t.Fatalf("a failure: %+v", m)
	}
}

// A failure keeps its class and what asz built of it, never a secret, the
// server's path or the source's query: a tool that failed, a server that
// refused, a variable not set, a local server's standard error, and an
// http or command fetch.
func TestAFailureKeepsNoSecret(t *testing.T) {
	t.Setenv("ASZ_TEST_AUTH", "Bearer tok-123")
	f := &mcpFake{result: func(map[string]any) map[string]any {
		return map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "denied for tok-123"}}}
	}}
	srv := f.start(t)
	mc := config.MCPFetch{URL: srv.URL + "/mcp/hidden-path", Headers: map[string]string{"Authorization": "ASZ_TEST_AUTH"}, Tool: "get"}
	s := throughMCP(t, mc, "https://jira.example/browse/OPS-4?token=q1")
	if m := s.manifest; m.Outcome != sessionjudge.SourceFailed || m.Error != "tool error: denied for [redacted]" || m.MCP == nil || m.MCP.Server != srv.URL {
		t.Fatalf("a tool that failed: %+v", m)
	}
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"bad token tok-123 at %s/mcp/hidden-path"}}`, "http://"+r.Host)
	}))
	defer refusing.Close()
	mc.URL = refusing.URL + "/mcp/hidden-path"
	if m := throughMCP(t, mc, "https://jira.example/browse/OPS-4").manifest; m.Error != "server error: JSON-RPC -32602: bad token [redacted] at "+refusing.URL {
		t.Fatalf("a server that refused: %+v", m)
	}
	// The same URL with its slashes escaped, as a JSON writer may spell it.
	escaping := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32602,
			"message": `{"endpoint":"` + strings.ReplaceAll("http://"+r.Host+"/mcp/hidden-path", "/", `\/`) + `"}`}})
		w.Write(b)
	}))
	defer escaping.Close()
	mc.URL = escaping.URL + "/mcp/hidden-path"
	if m := throughMCP(t, mc, "https://jira.example/browse/OPS-4").manifest; !strings.Contains(m.Error, `{"endpoint":"[redacted]"}`) ||
		strings.Contains(m.Error, "hidden-path") {
		t.Fatalf("a server that refused, its URL escaped: %+v", m)
	}
	mc.Headers = map[string]string{"Authorization": "ASZ_TEST_UNSET"}
	if m := throughMCP(t, mc, "https://jira.example/browse/OPS-4").manifest; !strings.Contains(m.Error, "ASZ_TEST_UNSET, which holds the header Authorization, is not set") {
		t.Fatalf("a variable not set: %+v", m)
	}
	// A local server's standard error is redacted whole before it is cut,
	// so a literal across the cut is not shown in part: the error keeps 300
	// bytes, and the literal that holds the secret crosses that bound.
	t.Setenv("ASZ_MCP_STAND_IN", "fail")
	t.Setenv("ASZ_TEST_TOKEN", "tok-123")
	t.Setenv("ASZ_MCP_STAND_IN_STDERR", strings.Repeat("x", 240)+` {"t":"tok\u002d123"} `+strings.Repeat("y", 5000)+" tok-123")
	stand := config.MCPFetch{Command: []string{os.Args[0], "-test.run=TestMCPServerStandIn"}, SecretEnv: []string{"ASZ_TEST_TOKEN"}, Tool: "jira_get_issue"}
	m := throughMCP(t, stand, "https://jira.example/browse/OPS-4").manifest
	if m.Outcome != sessionjudge.SourceFailed || !strings.Contains(m.Error, `{"t":"[red`) || strings.Contains(m.Error, "tok") ||
		!strings.HasPrefix(m.MCP.Server, "judge.test") {
		t.Fatalf("a local server's standard error: %+v", m)
	}
	// A command that repeats the source spelled another way keeps no query.
	t.Setenv("ASZ_FETCH_STAND_IN", "1")
	command := config.References{Timeout: "10s", Fetch: []config.Fetcher{{Prefix: "https://tickets.example/",
		Command: []string{os.Args[0], "-test.run=TestFetchCommandStandIn", "--"}}}}
	if m := fetched(t, command, "https://tickets.example/echo?token=q1")[0].manifest; m.Outcome != sessionjudge.SourceFailed ||
		strings.Contains(m.Error, "token=q1") || !strings.Contains(m.Error, `"[redacted]"`) {
		t.Fatalf("a command's standard error: %+v", m)
	}
	// A proxy that refuses can repeat its credential; a fetch over http
	// keeps none of it.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		buf.WriteString("HTTP/1.1 407 denied for proxy-user and pw-proxy\r\n\r\n")
		buf.Flush()
		conn.Close()
	}))
	defer proxy.Close()
	old := proxyFor
	proxyFor = func(*http.Request) (*url.URL, error) {
		u, _ := url.Parse(proxy.URL)
		u.User = url.UserPassword("proxy-user", "pw-proxy")
		return u, nil
	}
	m = fetched(t, config.References{Timeout: "10s"}, "https://jira.example/browse/OPS-6?token=q1")[0].manifest
	if m.Outcome != sessionjudge.SourceFailed || !strings.Contains(m.Error, "denied for [redacted] and [redacted]") || strings.Contains(m.Error, "pw-proxy") {
		t.Fatalf("a proxy that refused: %+v", m)
	}
	// A page reached directly that redirects through the proxy: the
	// redirect's proxy is the fetch's too.
	redirecting := httptest.NewServer(http.RedirectHandler("https://jira.example/browse/OPS-7", http.StatusFound))
	defer redirecting.Close()
	direct, _ := url.Parse(redirecting.URL)
	through := proxyFor
	proxyFor = func(r *http.Request) (*url.URL, error) {
		if r.URL.Host == direct.Host {
			return nil, nil
		}
		return through(r)
	}
	m = fetched(t, config.References{Timeout: "10s"}, redirecting.URL+"/page")[0].manifest
	proxyFor = old
	if m.Outcome != sessionjudge.SourceFailed || !strings.Contains(m.Error, "denied for [redacted] and [redacted]") || strings.Contains(m.Error, "pw-proxy") {
		t.Fatalf("a redirect through a proxy that refused: %+v", m)
	}
	// A page that refused, and a server that cannot be reached, keep no
	// query.
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer page.Close()
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	for _, src := range []string{page.URL + "/page?token=q1", gone.URL + "/page?token=q1"} {
		s := fetched(t, config.References{Timeout: "10s"}, src)[0]
		if m := s.manifest; m.Outcome != sessionjudge.SourceFailed || strings.Contains(m.Source+m.Error, "token=q1") || !m.QueryRemoved {
			t.Errorf("%s: %+v", src, m)
		}
	}
}

// A reference with a user or a password in its URL stops the run before it
// writes anything, and the error does not repeat it.
func TestAReferenceWithAUserIsRefused(t *testing.T) {
	if err := CheckReferences([]string{"notes.md", "https://jira.example/browse/OPS-1?x=a@b"}); err != nil {
		t.Fatalf("an @ in a query: %v", err)
	}
	err := CheckReferences([]string{"notes.md", "https://me:pw-1@jira.example/browse/OPS-1"})
	if err == nil || !strings.Contains(err.Error(), "reference 2") || strings.Contains(err.Error(), "pw-1") {
		t.Fatalf("%v", err)
	}
}

// The tools of an entry's server are listed with its settings alone, a
// remote one's and a local one's; a tool whose header annotations break the
// rules is counted, not listed.
func TestAnEntrysToolsAreListed(t *testing.T) {
	srv := (&mcpFake{}).start(t)
	cfg := config.References{Timeout: "20s", Fetch: []config.Fetcher{{Prefix: "https://jira.example/browse/", MCP: &config.MCPFetch{URL: srv.URL}}}}
	var out strings.Builder
	if err := ReferenceTools(context.Background(), cfg, "https://jira.example/browse/OPS-1", "test", &out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "speaks 2026-07-28 (modern); 1 tools") || !strings.Contains(got, "\nget\n  Get an issue\n") ||
		!strings.Contains(got, "1 tools left out") {
		t.Fatalf("remote:\n%s", got)
	}
	t.Setenv("ASZ_MCP_STAND_IN", "list")
	cfg.Fetch[0].MCP = &config.MCPFetch{Command: []string{os.Args[0], "-test.run=TestMCPServerStandIn"}}
	out.Reset()
	if err := ReferenceTools(context.Background(), cfg, "https://jira.example/browse/OPS-1", "test", &out); err != nil ||
		!strings.Contains(out.String(), "jira_get_issue\n  Get a Jira issue") {
		t.Fatalf("local: %v\n%s", err, out.String())
	}
	// With no tool configured, a fetch says to list them.
	s := throughMCP(t, config.MCPFetch{URL: srv.URL}, "https://jira.example/browse/OPS-1")
	if !strings.Contains(s.manifest.Error, "run asz judge -reference-tools https://jira.example/browse/OPS-1") {
		t.Fatalf("no tool: %+v", s.manifest)
	}
	_ = time.Second
}
