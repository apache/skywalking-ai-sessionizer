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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// httpExchange speaks Streamable HTTP to a remote server: every message a
// POST, every answer a JSON object or an event stream.
type httpExchange struct {
	s      Server
	client *http.Client
	era    string
	// version and session are a legacy server's: the version its handshake
	// agreed and the session it gave, when it gave one.
	version string
	session string
	// sessions are every session id a server gave, the one ended too.
	sessions []string
	next     int
	// renewed says the one new handshake a session that ended allows was
	// made.
	renewed bool
}

// resumes bounds how often a legacy stream that closed before its answer is
// resumed.
const resumes = 3

func openHTTP(s Server) *httpExchange {
	client := s.HTTP
	if client == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.Proxy = http.ProxyFromEnvironment
		// A redirect is never followed: a credential is for the server
		// configured, and no other.
		client = &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return &httpExchange{s: s, client: client}
}

// headers are what a request carries beyond its body: the modern ones for a
// modern request, the version and session for a legacy one after the
// handshake.
func (x *httpExchange) headers(method, name string, extra map[string]string) map[string]string {
	h := map[string]string{}
	switch {
	case x.era == EraModern || x.era == "":
		h["MCP-Protocol-Version"], h["Mcp-Method"] = Modern, method
		if name != "" {
			h["Mcp-Name"] = headerValue(name)
		}
	case x.version != "":
		h["MCP-Protocol-Version"] = x.version
		if x.session != "" {
			h["Mcp-Session-Id"] = x.session
		}
	}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

// send sends one HTTP request to the endpoint: the configured headers, then
// the protocol's own.
func (x *httpExchange) send(ctx context.Context, method string, body []byte, accept string, h map[string]string) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, x.s.URL, r)
	if err != nil {
		return nil, failure(ClassTransport, "the request could not be made")
	}
	for k, v := range x.s.Headers {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", accept)
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := x.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, failure(ClassTimeout, "the exchange did not end in time")
		}
		// The client's own error names the full URL, so it is never kept.
		return nil, failure(ClassTransport, "the request did not reach the server")
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		resp.Body.Close()
		return nil, &Error{Class: ClassTransport, Status: resp.StatusCode, Detail: "the server answered with a redirect, which asz does not follow"}
	}
	return resp, nil
}

const accepts = "application/json, text/event-stream"

// errorBody reads what a refusal of request id says: the JSON-RPC error it
// carries, within the bound of one message. A body that is no JSON-RPC
// error leaves the status to say it; one that answers another request is a
// broken protocol, never a refusal of this one.
func (x *httpExchange) errorBody(resp *http.Response, id int) (*rpcError, error) {
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, int64(x.s.MaxMessage)+1))
	switch {
	case err != nil:
		return nil, &Error{Class: ClassTransport, Status: resp.StatusCode, Detail: "the refusal could not be read"}
	case len(b) > x.s.MaxMessage:
		return nil, &Error{Class: ClassTooLarge, Status: resp.StatusCode, Detail: fmt.Sprintf("a message past %d bytes", x.s.MaxMessage)}
	}
	m, err := readMessage(b)
	if err != nil || m.Error == nil {
		return nil, nil
	}
	if !m.answers(id) {
		return nil, &Error{Class: ClassProtocol, Status: resp.StatusCode, Detail: "a refusal of a request asz did not send"}
	}
	return m.Error, nil
}

// ask sends a request and reads its answer. A refusal comes back as the
// status and the JSON-RPC error it carried, if any, for the caller to read
// by era.
func (x *httpExchange) ask(ctx context.Context, method string, params map[string]any, extra map[string]string) (*message, int, *rpcError, error) {
	x.next++
	id := x.next
	name, _ := params["name"].(string)
	if method != "tools/call" {
		name = ""
	}
	body := x.s.request(id, method, params, x.era != EraLegacy)
	if method == "initialize" {
		// The handshake carries no version header: none was agreed yet.
		body = x.s.request(id, method, params, false)
		extra = nil
	}
	h := x.headers(method, name, extra)
	if method == "initialize" {
		h = map[string]string{}
	}
	resp, err := x.send(ctx, http.MethodPost, body, accepts, h)
	if err != nil {
		// The request may have reached the server before its time ran
		// out, with no answer begun.
		x.cancelLate(ctx, id, method)
		return nil, 0, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		refusal, err := x.errorBody(resp, id)
		return nil, resp.StatusCode, refusal, err
	}
	m, err := x.answer(ctx, resp, id, method)
	if err != nil {
		x.cancelLate(ctx, id, method)
	}
	return m, http.StatusOK, nil, err
}

// cancelLate cancels a legacy request whose time ran out, other than the
// handshake, which a legacy client must not cancel.
func (x *httpExchange) cancelLate(ctx context.Context, id int, method string) {
	if ctx.Err() != nil && x.era == EraLegacy && method != "initialize" {
		x.cancel(id)
	}
}

// cancel tells a legacy server a request is no longer wanted: closing its
// stream is not a cancellation in those revisions. It waits at most a
// second.
func (x *httpExchange) cancel(id int) {
	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	resp, err := x.send(ctx, http.MethodPost, notification("notifications/cancelled", map[string]any{"requestId": id, "reason": "the fetch ran out of time"}),
		accepts, x.headers("notifications/cancelled", "", nil))
	if err == nil {
		resp.Body.Close()
	}
}

// answer reads the response to request id from what an HTTP response holds:
// one JSON object, or an event stream read event by event until it comes.
// A legacy stream that closes first is resumed with its last event id, the
// call never sent again.
func (x *httpExchange) answer(ctx context.Context, resp *http.Response, id int, method string) (*message, error) {
	media, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" && method == "initialize" {
		x.session, x.sessions = s, append(x.sessions, s)
	}
	switch media {
	case "application/json":
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, int64(x.s.MaxMessage)+1))
		if err != nil {
			return nil, failure(ClassTransport, "the answer could not be read")
		}
		if len(b) > x.s.MaxMessage {
			return nil, failure(ClassTooLarge, "a message past %d bytes", x.s.MaxMessage)
		}
		m, err := readMessage(b)
		if err != nil {
			return nil, err
		}
		if !m.answers(id) {
			return nil, failure(ClassProtocol, "an answer to no request asz sent")
		}
		return m, nil
	case "text/event-stream":
	default:
		resp.Body.Close()
		return nil, failure(ClassProtocol, "an answer of type %q", media)
	}
	last, retry := "", time.Second
	for attempt := 0; ; attempt++ {
		m, err := x.stream(ctx, resp, id, &last, &retry)
		if err != nil || m != nil {
			return m, err
		}
		// The stream ended before the answer.
		if x.era != EraLegacy || last == "" || attempt == resumes {
			return nil, failure(ClassTransport, "the event stream ended before the answer")
		}
		select {
		case <-ctx.Done():
			return nil, failure(ClassTimeout, "the exchange did not end in time")
		case <-time.After(retry):
		}
		h := x.headers("", "", map[string]string{"Last-Event-ID": last})
		resp, err = x.send(ctx, http.MethodGet, nil, "text/event-stream", h)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			if x.endedSession(resp.StatusCode) {
				return nil, sessionEnded()
			}
			return nil, &Error{Class: ClassTransport, Status: resp.StatusCode, Detail: "the event stream could not be resumed"}
		}
	}
}

// stream reads one event stream until the answer to id: comments passed
// over, an event with an id and no data a priming event, notifications
// passed over, a legacy server's requests answered. It returns nil, nil when
// the stream ended first.
func (x *httpExchange) stream(ctx context.Context, resp *http.Response, id int, last *string, retry *time.Duration) (*message, error) {
	defer resp.Body.Close()
	br := bufio.NewReaderSize(resp.Body, 64<<10)
	var data []byte
	eventID := ""
	for {
		l, err := readLine(br, x.s.MaxMessage)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, nil
			}
			if ctx.Err() != nil {
				return nil, failure(ClassTimeout, "the exchange did not end in time")
			}
			var e *Error
			if errors.As(err, &e) {
				return nil, e
			}
			return nil, nil
		}
		switch {
		case len(l) == 0:
			// The end of an event.
			if eventID != "" {
				*last = eventID
			}
			if len(data) > 0 {
				m, err := readMessage(data)
				if err != nil {
					return nil, err
				}
				if done, err := x.handle(ctx, m, id); err != nil || done {
					return m, err
				}
			}
			data, eventID = nil, ""
		case l[0] == ':':
		default:
			field, value, _ := strings.Cut(string(l), ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "data":
				if len(data) > 0 {
					data = append(data, '\n')
				}
				data = append(data, value...)
				if len(data) > x.s.MaxMessage {
					return nil, failure(ClassTooLarge, "a message past %d bytes", x.s.MaxMessage)
				}
			case "id":
				eventID = value
			case "retry":
				if ms, err := strconv.Atoi(value); err == nil && ms >= 0 && ms <= 60_000 {
					*retry = time.Duration(ms) * time.Millisecond
				}
			}
		}
	}
}

// handle takes one message of a stream: the answer to id ends it, a
// notification is passed over, a legacy server's ping is answered and any
// other request it sends refused, and a modern server sends none.
func (x *httpExchange) handle(ctx context.Context, m *message, id int) (bool, error) {
	switch {
	case m.isNotification():
		return false, nil
	case m.isRequest() && x.era == EraLegacy:
		var b []byte
		if m.Method == "ping" {
			b = reply(m.ID, map[string]any{}, nil)
		} else {
			b = reply(m.ID, nil, &rpcError{Code: codeMethodNotFound, Message: "asz offers no " + m.Method})
		}
		resp, err := x.send(ctx, http.MethodPost, b, accepts, x.headers("", "", nil))
		if err != nil {
			return false, err
		}
		resp.Body.Close()
		if x.endedSession(resp.StatusCode) {
			return false, sessionEnded()
		}
		return false, nil
	case m.isRequest():
		return false, failure(ClassProtocol, "a modern server sent a request")
	case m.answers(id):
		return true, nil
	}
	return false, failure(ClassProtocol, "an answer to no request asz sent")
}

// readLine reads one line of an event stream within limit bytes, its line
// ending taken off.
func readLine(br *bufio.Reader, limit int) ([]byte, error) {
	var b []byte
	for {
		frag, err := br.ReadSlice('\n')
		b = append(b, frag...)
		if len(b) > limit {
			return nil, failure(ClassTooLarge, "a line past %d bytes", limit)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			if len(b) > 0 && errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		break
	}
	b = bytes.TrimSuffix(b, []byte("\n"))
	return bytes.TrimSuffix(b, []byte("\r")), nil
}

// handshake gives a legacy server its handshake: initialize, its session
// kept when it gives one, then the initialized notification with the
// version and the session.
func (x *httpExchange) handshake(ctx context.Context) error {
	x.era, x.version, x.session = EraLegacy, "", ""
	m, status, refusal, err := x.ask(ctx, "initialize", x.s.initializeParams(), nil)
	switch {
	case err != nil:
		return err
	case status != http.StatusOK:
		return refused(status, refusal)
	case m.Error != nil:
		return serverError(m.Error)
	}
	if x.version, err = negotiated(m.Result); err != nil {
		return err
	}
	resp, err := x.send(ctx, http.MethodPost, notification("notifications/initialized", nil), accepts, x.headers("", "", nil))
	if err != nil {
		return err
	}
	resp.Body.Close()
	if x.endedSession(resp.StatusCode) {
		// Nothing was called yet: the fetch goes on in a new session.
		return x.renew(ctx)
	}
	if resp.StatusCode/100 != 2 {
		return &Error{Class: ClassTransport, Status: resp.StatusCode, Detail: "the server refused the end of the handshake"}
	}
	return nil
}

// refused is a request the server refused, by its status and the error it
// carried.
func refused(status int, e *rpcError) *Error {
	if e != nil {
		err := serverError(e)
		err.Status = status
		return err
	}
	return &Error{Class: ClassTransport, Status: status}
}

// legacyRefusal says whether a refusal of the first modern request shows a
// server of the initialize-based revisions: a 400, 404 or 405 with no error
// only a modern server sends.
func legacyRefusal(status int, e *rpcError) bool {
	return (status == http.StatusBadRequest || status == http.StatusNotFound || status == http.StatusMethodNotAllowed) && !modernError(e)
}

// modernRefusal reads a modern server's refusal: an unsupported version
// that does not list the modern revision, or a capability asz does not
// offer, fails the fetch, and is never a reason to fall back.
func modernRefusal(status int, e *rpcError) error {
	switch {
	case e != nil && e.Code == codeUnsupportedVersion && !supportsModern(e):
		err := refused(status, e)
		err.Detail = "the server speaks only modern versions asz does not"
		return err
	case e != nil && e.Code == codeMissingCapability:
		err := refused(status, e)
		err.Detail = "the server asks for a capability asz does not offer"
		return err
	}
	return refused(status, e)
}

// pages lists the tools, page by page, at most limit pages, finding the era
// with its first request: a modern server answers the modern request, and a
// legacy one refuses it as legacyRefusal says, and is given its handshake.
func (x *httpExchange) pages(ctx context.Context, limit int) ([]json.RawMessage, bool, error) {
	var tools []json.RawMessage
	var cursor *string
	for page := 0; page < limit; page++ {
		m, status, refusal, err := x.ask(ctx, "tools/list", pageParams(cursor), nil)
		if ended(err) {
			if err := x.renew(ctx); err != nil {
				return nil, false, err
			}
			page--
			continue
		}
		if err != nil {
			return nil, false, err
		}
		if status != http.StatusOK {
			switch {
			case x.era == "" && legacyRefusal(status, refusal):
				if err := x.handshake(ctx); err != nil {
					return nil, false, err
				}
				page--
				continue
			case x.era == "":
				return nil, false, modernRefusal(status, refusal)
			case x.endedSession(status):
				if err := x.renew(ctx); err != nil {
					return nil, false, err
				}
				page--
				continue
			}
			return nil, false, refused(status, refusal)
		}
		if x.era == "" {
			x.era = EraModern
		}
		if m.Error != nil {
			return nil, false, serverError(m.Error)
		}
		list, next, err := listPage(m.Result)
		if err != nil {
			return nil, false, err
		}
		tools = append(tools, list...)
		if next == nil {
			return tools, true, nil
		}
		cursor = next
	}
	return tools, false, nil
}

// endedSession says whether a refusal ends a legacy session: a 404 to a
// request that carried it.
func (x *httpExchange) endedSession(status int) bool {
	return status == http.StatusNotFound && x.era == EraLegacy && x.session != ""
}

// renew makes the one new handshake a session that ended allows: the
// revision requires one without the old session before anything else. A
// session that ended again is dropped, so nothing carries it after.
func (x *httpExchange) renew(ctx context.Context) error {
	if x.renewed {
		x.session = ""
		return &Error{Class: ClassTransport, Status: http.StatusNotFound, Detail: "the server ended the session again"}
	}
	x.renewed = true
	return x.handshake(ctx)
}

// sessionEnded is the failure of a request whose session the server ended
// while its answer was read: a resumed stream or a reply refused with 404.
func sessionEnded() *Error {
	return &Error{Class: ClassTransport, Status: http.StatusNotFound, Detail: "the server ended the session", ended: true}
}

// ended says whether a failure is a session the server ended.
func ended(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.ended
}

// valid keeps the tools whose x-mcp-header annotations hold, on a modern
// server, and counts those left out; a legacy server's tools carry no rule
// asz applies.
func (x *httpExchange) valid(raw []json.RawMessage) ([]Tool, map[string][]annotation, int) {
	var tools []Tool
	annots := map[string][]annotation{}
	invalid := 0
	for _, r := range raw {
		var t Tool
		if json.Unmarshal(r, &t) != nil || t.Name == "" {
			invalid++
			continue
		}
		if x.era == EraModern {
			a, err := annotations(t.InputSchema)
			if err != nil {
				invalid++
				continue
			}
			annots[t.Name] = a
		}
		tools = append(tools, t)
	}
	return tools, annots, invalid
}

func (x *httpExchange) list(ctx context.Context, limit int) (*Listed, error) {
	raw, complete, err := x.pages(ctx, limit)
	if err != nil {
		return nil, err
	}
	tools, _, invalid := x.valid(raw)
	return &Listed{Era: x.era, Version: x.versionSpoken(), Tools: tools, Invalid: invalid, Complete: complete}, nil
}

// lookupPages bounds the pages a call's tool is looked for in.
const lookupPages = 20

// call calls one tool. A modern call is preceded by the tool list, so the
// tool's x-mcp-header annotations are mirrored as headers before it is
// sent; a header mismatch makes the list read again and the call sent once
// more, since a refused call ran nothing. A legacy call needs no list. A
// session that ended before the call is renewed and the call made; one
// that ended after it ends the fetch, since the tool may have run.
func (x *httpExchange) call(ctx context.Context, tool string, args map[string]any) (*Called, error) {
	var params map[string]string
	find := func() error {
		// The first page finds the era; a legacy call needs no more of the
		// list, and a modern one reads on only for a tool not on it.
		raw, complete, err := x.pages(ctx, 1)
		if err != nil {
			return err
		}
		if x.era != EraModern {
			return nil
		}
		if !complete && !slices.ContainsFunc(raw, func(r json.RawMessage) bool { return named(r, tool) }) {
			if raw, complete, err = x.pages(ctx, lookupPages); err != nil {
				return err
			}
		}
		tools, annots, _ := x.valid(raw)
		for _, t := range tools {
			if t.Name == tool {
				params, err = paramHeaders(annots[tool], args)
				return err
			}
		}
		if !complete {
			return failure(ClassProtocol, "no valid tool named %q in the server's first %d pages of tools", tool, lookupPages)
		}
		return failure(ClassProtocol, "the server lists no valid tool named %q", tool)
	}
	if err := find(); err != nil {
		return nil, err
	}
	for retried := false; ; {
		m, status, refusal, err := x.ask(ctx, "tools/call", map[string]any{"name": tool, "arguments": args}, params)
		switch {
		case ended(err) || x.endedSession(status):
			// The call was sent: whether the tool ran is not known. A new
			// handshake ends the session as the revision asks, and the
			// call is never sent again.
			if err := x.renew(ctx); err != nil {
				return nil, err
			}
			return nil, &Error{Class: ClassTransport, Status: http.StatusNotFound,
				Detail: "the server ended the session after the call was sent, so whether the tool ran is not known"}
		case err != nil:
			return nil, err
		case status == http.StatusOK && m.Error != nil:
			return nil, serverError(m.Error)
		case status == http.StatusOK:
			if err := complete(m.Result); err != nil {
				return nil, err
			}
			return &Called{Era: x.era, Version: x.versionSpoken(), Result: m.Result}, nil
		case x.era == EraModern && status == http.StatusBadRequest && refusal != nil && refusal.Code == codeHeaderMismatch && !retried:
			retried = true
			if err := find(); err != nil {
				return nil, err
			}
		default:
			return nil, refused(status, refusal)
		}
	}
}

// named says whether a listed tool has a name.
func named(raw json.RawMessage, name string) bool {
	var t struct {
		Name string `json:"name"`
	}
	return json.Unmarshal(raw, &t) == nil && t.Name == name
}

func (x *httpExchange) secrets() []string { return x.sessions }

func (x *httpExchange) versionSpoken() string {
	if x.era == EraModern {
		return Modern
	}
	return x.version
}

// close ends a legacy session with a DELETE carrying it; a 405 says the
// server does not end sessions so, and no failure here undoes a fetch.
func (x *httpExchange) close() {
	if x.era != EraLegacy || x.session == "" {
		return
	}
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if resp, err := x.send(ctx, http.MethodDelete, nil, accepts, x.headers("", "", nil)); err == nil {
		resp.Body.Close()
	}
}
