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

package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// seen is one request a fake server took: its method, the JSON-RPC method
// and id of its body, and its headers.
type seen struct {
	verb, method string
	id           json.RawMessage
	header       http.Header
	params       map[string]any
}

// fake is a Streamable HTTP server a test scripts: answer gives the HTTP
// response to each request it takes, in order.
type fake struct {
	mu   sync.Mutex
	took []seen
}

func (f *fake) server(t *testing.T, answer func(w http.ResponseWriter, s seen, n int)) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
			Params map[string]any  `json:"params"`
		}
		_ = json.Unmarshal(b, &m)
		s := seen{verb: r.Method, method: m.Method, id: m.ID, header: r.Header.Clone(), params: m.Params}
		f.mu.Lock()
		f.took = append(f.took, s)
		n := len(f.took)
		f.mu.Unlock()
		answer(w, s, n)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// methods lists what the server took, as "verb method".
func (f *fake) methods() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, s := range f.took {
		out = append(out, strings.TrimSpace(s.verb+" "+s.method))
	}
	return strings.Join(out, ", ")
}

func (f *fake) find(method string) []seen {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []seen
	for _, s := range f.took {
		if s.method == method {
			out = append(out, s)
		}
	}
	return out
}

func jsonAnswer(w http.ResponseWriter, s seen, result any) {
	w.Header().Set("Content-Type", "application/json")
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": s.id, "result": result})
	_, _ = w.Write(b)
}

func refuse(w http.ResponseWriter, status int, s seen, code int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if code == 0 {
		return
	}
	body := map[string]any{"jsonrpc": "2.0", "error": map[string]any{"code": code, "message": "refused", "data": data}}
	if s.id != nil {
		body["id"] = s.id
	}
	b, _ := json.Marshal(body)
	_, _ = w.Write(b)
}

var echoTool = map[string]any{"name": "get", "inputSchema": map[string]any{"type": "object"}}

var textResult = map[string]any{"content": []any{map[string]any{"type": "text", "text": "the issue"}}}

// modern is a server of revision 2026-07-28: it lists tools and answers a
// call, each as JSON.
func modern(tools []any) func(http.ResponseWriter, seen, int) {
	return func(w http.ResponseWriter, s seen, _ int) {
		switch s.method {
		case "tools/list":
			jsonAnswer(w, s, map[string]any{"tools": tools})
		case "tools/call":
			jsonAnswer(w, s, textResult)
		default:
			refuse(w, http.StatusNotFound, s, codeMethodNotFound, nil)
		}
	}
}

// legacy is a server of revision 2025-11-25: it refuses a request with no
// session before its handshake, gives session, and answers as JSON.
func legacy(session string, call func(http.ResponseWriter, seen)) func(http.ResponseWriter, seen, int) {
	return func(w http.ResponseWriter, s seen, _ int) {
		switch {
		case s.verb == http.MethodDelete:
			w.WriteHeader(http.StatusMethodNotAllowed)
		case s.method == "initialize":
			if session != "" {
				w.Header().Set("Mcp-Session-Id", session)
			}
			jsonAnswer(w, s, map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}})
		case s.method == "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case s.header.Get("MCP-Protocol-Version") != "2025-11-25":
			refuse(w, http.StatusBadRequest, s, 0, nil)
		case s.method == "tools/list":
			jsonAnswer(w, s, map[string]any{"tools": []any{echoTool}})
		case s.method == "tools/call":
			call(w, s)
		}
	}
}

func callOn(t *testing.T, url string, tool string, args map[string]any) (*Called, error) {
	t.Helper()
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	return Call(ctx, Server{URL: url, Headers: map[string]string{"Authorization": "Basic secret"}}, tool, args)
}

// A modern call carries its version, method and name as headers and its
// metadata in its body, after the tool list it reads first.
func TestAModernCallCarriesWhatTheRevisionAsks(t *testing.T) {
	f := &fake{}
	c, err := callOn(t, f.server(t, modern([]any{echoTool})), "get", map[string]any{"key": "OPS-1"})
	if err != nil || c.Era != EraModern || c.Version != Modern || !strings.Contains(string(c.Result), "the issue") {
		t.Fatalf("%+v %v", c, err)
	}
	if f.methods() != "POST tools/list, POST tools/call" {
		t.Fatalf("took %s", f.methods())
	}
	call := f.find("tools/call")[0]
	meta, _ := call.params["_meta"].(map[string]any)
	if call.header.Get("MCP-Protocol-Version") != Modern || call.header.Get("Mcp-Method") != "tools/call" || call.header.Get("Mcp-Name") != "get" ||
		call.header.Get("Authorization") != "Basic secret" || meta["io.modelcontextprotocol/protocolVersion"] != Modern ||
		meta["io.modelcontextprotocol/clientCapabilities"] == nil {
		t.Fatalf("headers %v, meta %v", call.header, meta)
	}
}

// An answer can be an event stream: comments, notifications and a priming
// event come before the response.
func TestAStreamedAnswerIsReadToItsResponse(t *testing.T) {
	f := &fake{}
	url := f.server(t, func(w http.ResponseWriter, s seen, _ int) {
		if s.method == "tools/list" {
			jsonAnswer(w, s, map[string]any{"tools": []any{echoTool}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": s.id, "result": textResult})
		fmt.Fprintf(w, ": keep-alive\n\nid: e1\ndata:\n\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\nid: e2\ndata: %s\n\n", b)
	})
	if c, err := callOn(t, url, "get", nil); err != nil || !strings.Contains(string(c.Result), "the issue") {
		t.Fatalf("%+v %v", c, err)
	}
}

// A tool's x-mcp-header annotations are mirrored as headers before the
// call: a string as it is, an integer in decimal, a boolean as true or
// false, a value that is not plain visible ASCII in the Base64 sentinel, and
// an argument absent or null gets none. An integer needs no declared bounds.
func TestAnnotatedArgumentsAreMirroredAsHeaders(t *testing.T) {
	tool := map[string]any{"name": "get", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{
		"region":  map[string]any{"type": "string", "x-mcp-header": "Region"},
		"count":   map[string]any{"type": "integer", "x-mcp-header": "Count"},
		"flag":    map[string]any{"type": "boolean", "x-mcp-header": "Flag"},
		"padded":  map[string]any{"type": "string", "x-mcp-header": "Padded"},
		"unicode": map[string]any{"type": "string", "x-mcp-header": "Unicode"},
		"looks":   map[string]any{"type": "string", "x-mcp-header": "Looks"},
		"tabbed":  map[string]any{"type": "string", "x-mcp-header": "Tabbed"},
		"gone":    map[string]any{"type": "string", "x-mcp-header": "Gone"},
		"null":    map[string]any{"type": "string", "x-mcp-header": "Null"},
		"nested":  map[string]any{"type": "object", "properties": map[string]any{"deep": map[string]any{"type": "string", "x-mcp-header": "Deep"}}},
	}}}
	f := &fake{}
	_, err := callOn(t, f.server(t, modern([]any{tool})), "get", map[string]any{"region": "us-west1", "count": 42, "flag": true,
		"padded": " x ", "tabbed": "a\tb", "unicode": "世界", "looks": "=?base64?a?=", "null": nil, "nested": map[string]any{"deep": "d"}})
	if err != nil {
		t.Fatal(err)
	}
	h := f.find("tools/call")[0].header
	want := map[string]string{"Mcp-Param-Region": "us-west1", "Mcp-Param-Count": "42", "Mcp-Param-Flag": "true",
		"Mcp-Param-Padded": "=?base64?IHgg?=", "Mcp-Param-Unicode": "=?base64?5LiW55WM?=", "Mcp-Param-Looks": "=?base64?PT9iYXNlNjQ/YT89?=", "Mcp-Param-Tabbed": "=?base64?YQli?=",
		"Mcp-Param-Deep": "d", "Mcp-Param-Gone": "", "Mcp-Param-Null": ""}
	for k, v := range want {
		if got := h.Get(k); got != v {
			t.Errorf("%s: %q, want %q", k, got, v)
		}
	}
	if _, err := callOn(t, f.server(t, modern([]any{tool})), "get", map[string]any{"count": 1 << 54}); err == nil ||
		!strings.Contains(err.Error(), "past the integers a header carries") {
		t.Fatalf("an integer past the safe range: %v", err)
	}
}

// A tool whose annotations break the rules is left out of the list, and a
// call to it finds no valid tool; the other tools stay usable.
func TestAToolWithABadAnnotationIsLeftOut(t *testing.T) {
	bad := func(name string, schema map[string]any) map[string]any {
		return map[string]any{"name": name, "inputSchema": schema}
	}
	tools := []any{echoTool,
		bad("in-items", map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "array",
			"items": map[string]any{"type": "string", "x-mcp-header": "A"}}}}),
		bad("a-number", map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "number", "x-mcp-header": "A"}}}),
		bad("twice", map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string", "x-mcp-header": "A"},
			"b": map[string]any{"type": "string", "x-mcp-header": "a"}}}),
		bad("no-token", map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string", "x-mcp-header": "a b"}}}),
	}
	f := &fake{}
	url := f.server(t, modern(tools))
	l, err := List(context.Background(), Server{URL: url}, 5)
	if err != nil || len(l.Tools) != 1 || l.Tools[0].Name != "get" || l.Invalid != 4 || !l.Complete {
		t.Fatalf("%+v %v", l, err)
	}
	if _, err := callOn(t, url, "twice", nil); err == nil || !strings.Contains(err.Error(), `no valid tool named "twice"`) {
		t.Fatalf("a call to an invalid tool: %v", err)
	}
}

// A header mismatch reads the tool list again and sends the call once more,
// since a refused call ran nothing; a second mismatch ends the fetch.
func TestAHeaderMismatchSendsTheCallOnceMore(t *testing.T) {
	for _, mismatches := range []int{1, 2} {
		f := &fake{}
		calls := 0
		url := f.server(t, func(w http.ResponseWriter, s seen, _ int) {
			if s.method == "tools/call" {
				calls++
				if calls <= mismatches {
					refuse(w, http.StatusBadRequest, s, codeHeaderMismatch, nil)
					return
				}
			}
			modern([]any{echoTool})(w, s, 0)
		})
		_, err := callOn(t, url, "get", nil)
		if (mismatches == 1) != (err == nil) || calls != min(mismatches+1, 2) {
			t.Errorf("%d mismatches: %v after %d calls (%s)", mismatches, err, calls, f.methods())
		}
	}
	// Only a 400 that answers this call says it ran nothing: the code with
	// another status, or for another request, sends nothing again.
	for _, c := range []struct {
		name    string
		status  int
		id      json.RawMessage
		garbage bool
	}{{"a 500", http.StatusInternalServerError, nil, false}, {"another request's", http.StatusBadRequest, json.RawMessage(`99`), false},
		{"one with more after it", http.StatusBadRequest, nil, true}} {
		f := &fake{}
		url := f.server(t, func(w http.ResponseWriter, s seen, _ int) {
			if s.method == "tools/call" {
				if c.id != nil {
					s.id = c.id
				}
				if c.garbage {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(c.status)
					fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":%d,"message":"refused"}} garbage`, s.id, codeHeaderMismatch)
					return
				}
				refuse(w, c.status, s, codeHeaderMismatch, nil)
				return
			}
			modern([]any{echoTool})(w, s, 0)
		})
		if _, err := callOn(t, url, "get", nil); err == nil || len(f.find("tools/call")) != 1 {
			t.Errorf("%s: %v (%s)", c.name, err, f.methods())
		}
	}
}

// An error only a modern server sends keeps the modern era: an unsupported
// version that does not list 2026-07-28 ends the fetch, and is never a
// reason for a handshake, even with a legacy version listed. Such an error
// can come with no id.
func TestAModernRefusalNeverFallsBack(t *testing.T) {
	// A refusal larger than 64 KiB is read whole within the bound of a
	// message, and so is still a modern refusal.
	f := &fake{}
	url := f.server(t, func(w http.ResponseWriter, s seen, _ int) {
		refuse(w, http.StatusBadRequest, s, codeUnsupportedVersion, map[string]any{"supported": []string{"2025-11-25"}, "note": strings.Repeat("x", 70<<10)})
	})
	if _, err := callOn(t, url, "get", nil); err == nil || !strings.Contains(err.Error(), "speaks only modern versions asz does not") || len(f.find("initialize")) != 0 {
		t.Errorf("a large refusal: %v (%s)", err, f.methods())
	}
	for _, withID := range []bool{true, false} {
		f := &fake{}
		url := f.server(t, func(w http.ResponseWriter, s seen, _ int) {
			if !withID {
				s.id = nil
			}
			refuse(w, http.StatusBadRequest, s, codeUnsupportedVersion, map[string]any{"supported": []string{"2025-11-25"}})
		})
		_, err := callOn(t, url, "get", nil)
		if err == nil || !strings.Contains(err.Error(), "speaks only modern versions asz does not") || len(f.find("initialize")) != 0 {
			t.Errorf("with id %v: %v (%s)", withID, err, f.methods())
		}
	}
}

// A legacy server is given its handshake: the session it gives is carried,
// with the version agreed, on every later request, and ended with a DELETE
// whose 405 fails nothing. A server that gives no session gets none.
func TestALegacyServerKeepsItsSession(t *testing.T) {
	for _, session := range []string{"s-1", ""} {
		f := &fake{}
		c, err := callOn(t, f.server(t, legacy(session, func(w http.ResponseWriter, s seen) { jsonAnswer(w, s, textResult) })), "get", nil)
		if err != nil || c.Era != EraLegacy || c.Version != "2025-11-25" || (session != "") != slices.Equal(c.Sessions, []string{session}) {
			t.Fatalf("session %q: %+v %v (%s)", session, c, err, f.methods())
		}
		wantTook := "POST tools/list, POST initialize, POST notifications/initialized, POST tools/list, POST tools/call"
		if session != "" {
			wantTook += ", DELETE"
		}
		if f.methods() != wantTook {
			t.Fatalf("session %q took %s", session, f.methods())
		}
		for _, s := range f.took[2:] {
			if s.header.Get("MCP-Protocol-Version") != "2025-11-25" || s.header.Get("Mcp-Session-Id") != session {
				t.Errorf("session %q: %s %s carried %v", session, s.verb, s.method, s.header)
			}
		}
		call := f.find("tools/call")[0]
		if _, has := call.params["_meta"]; has {
			t.Errorf("a legacy call carried modern metadata")
		}
	}
}

// A legacy stream that closes before its answer is resumed with its last
// event id, and the call is never sent again.
func TestALegacyStreamIsResumed(t *testing.T) {
	f := &fake{}
	var callID json.RawMessage
	url := f.server(t, legacy("s-1", func(w http.ResponseWriter, s seen) {
		callID = s.id
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "id: p1\nretry: 10\ndata:\n\n")
	}))
	inner := url
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if r.Header.Get("Last-Event-ID") != "p1" || r.Header.Get("Mcp-Session-Id") != "s-1" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": callID, "result": textResult})
			fmt.Fprintf(w, "id: p2\ndata: %s\n\n", b)
			return
		}
		proxy, _ := http.NewRequest(r.Method, inner, r.Body)
		proxy.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(proxy)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(srv.Close)
	c, err := callOn(t, srv.URL, "get", nil)
	if err != nil || !strings.Contains(string(c.Result), "the issue") || len(f.find("tools/call")) != 1 {
		t.Fatalf("%+v %v (%s)", c, err, f.methods())
	}
}

// A session the server ended before the call is renewed and the call made;
// one ended after the call was sent is renewed as the revision asks, and
// the fetch fails, since the tool may have run. The call is sent once.
func TestAnEndedSessionNeverSendsTheCallAgain(t *testing.T) {
	for _, after := range []bool{false, true} {
		f := &fake{}
		ended := false
		base := legacy("s-1", func(w http.ResponseWriter, s seen) { jsonAnswer(w, s, textResult) })
		url := f.server(t, func(w http.ResponseWriter, s seen, n int) {
			end := (after && s.method == "tools/call") || (!after && s.method == "tools/list" && s.header.Get("Mcp-Session-Id") != "")
			if end && !ended {
				ended = true
				w.WriteHeader(http.StatusNotFound)
				return
			}
			base(w, s, n)
		})
		_, err := callOn(t, url, "get", nil)
		if after != (err != nil) || len(f.find("tools/call")) != 1 || len(f.find("initialize")) != 2 {
			t.Errorf("ended after the call %v: %v (%s)", after, err, f.methods())
		}
		// Both sessions are the caller's to redact from what the failure
		// says.
		var e *Error
		if after && (!errors.As(err, &e) || len(e.Sessions) != 2 || strings.Contains(err.Error(), "s-1")) {
			t.Errorf("the failure's sessions: %+v", e)
		}
	}
}

// A legacy server's ping is answered and any other request it sends is
// refused; a modern server sends none, so one ends the fetch.
func TestARequestFromTheServerIsAnsweredByEra(t *testing.T) {
	streamed := func(requests ...string) func(http.ResponseWriter, seen) {
		return func(w http.ResponseWriter, s seen) {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, r := range requests {
				fmt.Fprintf(w, "data: %s\n\n", r)
			}
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": s.id, "result": textResult})
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
	}
	ping := `{"jsonrpc":"2.0","id":"r1","method":"ping"}`
	sampling := `{"jsonrpc":"2.0","id":"r2","method":"sampling/createMessage","params":{}}`
	f := &fake{}
	url := f.server(t, legacy("s-1", streamed(ping, sampling)))
	if _, err := callOn(t, url, "get", nil); err != nil {
		t.Fatalf("legacy: %v (%s)", err, f.methods())
	}
	var answered []string
	for _, s := range f.took {
		if s.verb == http.MethodPost && s.method == "" && s.id != nil {
			answered = append(answered, string(s.id))
		}
	}
	if strings.Join(answered, " ") != `"r1" "r2"` {
		t.Fatalf("answered %v (%s)", answered, f.methods())
	}
	m := &fake{}
	murl := m.server(t, func(w http.ResponseWriter, s seen, _ int) {
		if s.method == "tools/call" {
			streamed(ping)(w, s)
			return
		}
		modern([]any{echoTool})(w, s, 0)
	})
	if _, err := callOn(t, murl, "get", nil); err == nil || !strings.Contains(err.Error(), "a modern server sent a request") {
		t.Fatalf("modern: %v", err)
	}
}

// A result asks no more of asz than to read it: none of type, as every
// legacy result, is complete; one that asks for input, or of a type asz does
// not know, ends the fetch.
func TestAResultIsReadByItsType(t *testing.T) {
	for _, c := range []struct {
		resultType any
		fails      string
	}{
		{nil, ""}, {"complete", ""}, {"input_required", "asked for input"}, {"other", "a type asz does not know"},
	} {
		f := &fake{}
		url := f.server(t, func(w http.ResponseWriter, s seen, _ int) {
			if s.method == "tools/call" {
				r := map[string]any{"content": []any{}}
				if c.resultType != nil {
					r["resultType"] = c.resultType
				}
				jsonAnswer(w, s, r)
				return
			}
			modern([]any{echoTool})(w, s, 0)
		})
		_, err := callOn(t, url, "get", nil)
		if (c.fails == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.fails)) {
			t.Errorf("type %v: %v", c.resultType, err)
		}
	}
}

// What does not hold ends the fetch: an answer to another request, a
// message past the bound, a redirect, which is never followed.
func TestWhatDoesNotHoldEndsTheFetch(t *testing.T) {
	for _, c := range []struct {
		name   string
		answer func(http.ResponseWriter, seen)
		fails  string
	}{
		{"an answer to another id", func(w http.ResponseWriter, s seen) {
			s.id = json.RawMessage(`999`)
			jsonAnswer(w, s, textResult)
		}, "an answer to no request"},
		{"a message past the bound", func(w http.ResponseWriter, s seen) {
			jsonAnswer(w, s, map[string]any{"content": []any{map[string]any{"type": "text", "text": strings.Repeat("x", 2048)}}})
		}, "too large"},
		{"an answer whose id is a string", func(w http.ResponseWriter, s seen) {
			s.id = json.RawMessage(`"` + string(s.id) + `"`)
			jsonAnswer(w, s, textResult)
		}, "an answer to no request"},
		{"a refusal past the bound", func(w http.ResponseWriter, s seen) {
			refuse(w, http.StatusBadRequest, s, codeHeaderMismatch, strings.Repeat("x", 2048))
		}, "too large"},
		{"a redirect", func(w http.ResponseWriter, _ seen) {
			w.Header().Set("Location", "https://elsewhere.example/")
			w.WriteHeader(http.StatusFound)
		}, "redirect"},
	} {
		f := &fake{}
		url := f.server(t, func(w http.ResponseWriter, s seen, _ int) {
			if s.method == "tools/call" {
				c.answer(w, s)
				return
			}
			modern([]any{echoTool})(w, s, 0)
		})
		_, err := Call(context.Background(), Server{URL: url, MaxMessage: 1024}, "get", nil)
		var e *Error
		if !errors.As(err, &e) || !strings.Contains(err.Error(), c.fails) || strings.Contains(err.Error(), url) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

// The era is found once: a modern server's later refusal is a refusal, never
// a reason for a handshake.
func TestTheEraIsFoundOnce(t *testing.T) {
	f := &fake{}
	url := f.server(t, func(w http.ResponseWriter, s seen, _ int) {
		if s.method == "tools/call" {
			refuse(w, http.StatusBadRequest, s, 0, nil)
			return
		}
		modern([]any{echoTool})(w, s, 0)
	})
	if _, err := callOn(t, url, "get", nil); err == nil || len(f.find("initialize")) != 0 {
		t.Fatalf("%v (%s)", err, f.methods())
	}
}

// A legacy call that runs out of time is cancelled with a notification, its
// session and version carried; the call is never sent again.
func TestALegacyCallOutOfTimeIsCancelled(t *testing.T) {
	// The answer's stream begun, or no answer begun at all: either way the
	// call may have reached the server.
	for _, begun := range []bool{true, false} {
		f := &fake{}
		url := f.server(t, legacy("s-1", func(w http.ResponseWriter, _ seen) {
			if begun {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
			}
			time.Sleep(500 * time.Millisecond)
		}))
		ctx, stop := context.WithTimeout(context.Background(), 300*time.Millisecond)
		_, err := Call(ctx, Server{URL: url}, "get", nil)
		stop()
		var e *Error
		if !errors.As(err, &e) || e.Class != ClassTimeout {
			t.Fatalf("begun %v: %v", begun, err)
		}
		deadline := time.Now().Add(2 * time.Second)
		for len(f.find("notifications/cancelled")) == 0 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		c := f.find("notifications/cancelled")
		if len(c) != 1 || c[0].header.Get("Mcp-Session-Id") != "s-1" || c[0].header.Get("MCP-Protocol-Version") != "2025-11-25" ||
			len(f.find("tools/call")) != 1 {
			t.Fatalf("begun %v: cancelled %+v (%s)", begun, c, f.methods())
		}
	}
}

// A session that ends on any request that carries it is renewed: on the
// stream of a call resumed, the call is not sent again and the fetch fails;
// at the end of the handshake, before anything was called, the fetch goes
// on in the new session.
func TestASessionEndedAnywhereIsRenewed(t *testing.T) {
	f := &fake{}
	base := legacy("s-1", func(w http.ResponseWriter, _ seen) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "id: p1\nretry: 10\ndata:\n\n")
	})
	url := f.server(t, func(w http.ResponseWriter, s seen, n int) {
		if s.verb == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		base(w, s, n)
	})
	_, err := callOn(t, url, "get", nil)
	if err == nil || !strings.Contains(err.Error(), "ended the session after the call was sent") || len(f.find("tools/call")) != 1 ||
		len(f.find("initialize")) != 2 {
		t.Fatalf("resumed: %v (%s)", err, f.methods())
	}

	f = &fake{}
	ended := false
	base = legacy("s-1", func(w http.ResponseWriter, s seen) { jsonAnswer(w, s, textResult) })
	url = f.server(t, func(w http.ResponseWriter, s seen, n int) {
		if s.method == "notifications/initialized" && !ended {
			ended = true
			w.WriteHeader(http.StatusNotFound)
			return
		}
		base(w, s, n)
	})
	if _, err := callOn(t, url, "get", nil); err != nil || len(f.find("initialize")) != 2 || len(f.find("tools/call")) != 1 {
		t.Fatalf("at the handshake: %v (%s)", err, f.methods())
	}
}

// A list follows its pages, an empty cursor included, and says when it
// stopped at the bound.
func TestAListFollowsItsPages(t *testing.T) {
	f := &fake{}
	url := f.server(t, func(w http.ResponseWriter, s seen, _ int) {
		cursor, _ := s.params["cursor"].(string)
		_, has := s.params["cursor"]
		switch {
		case !has:
			jsonAnswer(w, s, map[string]any{"tools": []any{echoTool}, "nextCursor": ""})
		case cursor == "":
			jsonAnswer(w, s, map[string]any{"tools": []any{map[string]any{"name": "two", "inputSchema": map[string]any{}}}, "nextCursor": "c3"})
		default:
			jsonAnswer(w, s, map[string]any{"tools": []any{map[string]any{"name": "three", "inputSchema": map[string]any{}}}})
		}
	})
	l, err := List(context.Background(), Server{URL: url}, 5)
	if err != nil || len(l.Tools) != 3 || !l.Complete {
		t.Fatalf("%+v %v", l, err)
	}
	if l, err := List(context.Background(), Server{URL: url}, 2); err != nil || len(l.Tools) != 2 || l.Complete {
		t.Fatalf("bounded: %+v %v", l, err)
	}
}
