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

// Package mcpclient calls one tool of a Model Context Protocol server, the
// way asz fetches a reference a person names: a remote server over
// Streamable HTTP, or a local one it starts and speaks to over stdio. It
// speaks revision 2026-07-28, where every request carries its version and
// there is no handshake, and the initialize-based revisions 2025-11-25,
// 2025-06-18, 2025-03-26 and 2024-11-05, finding which a server speaks as
// each transport says. It offers the server nothing: no sampling, roots or
// elicitation.
package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Modern is the revision without a handshake that asz speaks.
const Modern = "2026-07-28"

// Legacy are the initialize-based revisions asz speaks, newest first.
var Legacy = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// The eras a server is found to speak.
const (
	EraModern = "modern"
	EraLegacy = "legacy"
)

// Server is where a tool is called: a remote one at URL, or a local one asz
// starts with Command.
type Server struct {
	// URL is a remote server's MCP endpoint, and Headers what every request
	// to it carries, its values resolved.
	URL     string
	Headers map[string]string
	// HTTP sends the requests; nil is a client that follows no redirect,
	// with the environment's proxy.
	HTTP *http.Client
	// Command starts a local server, with Env added to asz's environment,
	// and Stderr takes what it writes on its standard error.
	Command []string
	Env     []string
	Stderr  io.Writer
	// MaxMessage bounds one message from the server; 0 is 16 MiB.
	MaxMessage int
	// Probe bounds how long a local server is given to answer the modern
	// probe before it is taken for a legacy one; 0 is 10 seconds.
	Probe time.Duration
	// Version is asz's own, for the client information a server is told.
	Version string
}

// Called is what one tool call answered: the era and version spoken, and
// the call's result object as the server sent it. Sessions are the session
// ids the server gave, secrets the caller redacts and never keeps.
type Called struct {
	Era      string
	Version  string
	Result   json.RawMessage
	Sessions []string
}

// Tool is one tool a server lists.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Listed is what a server lists of its tools: the valid ones, how many were
// left out for annotations that break the rules, and whether the list is
// complete or stopped at the page bound.
type Listed struct {
	Era      string
	Version  string
	Tools    []Tool
	Invalid  int
	Complete bool
	Sessions []string
}

// The classes of a failure. A failure says its class and a detail asz built
// from typed values: never a raw message that could hold a URL or a secret.
// Only a server's or a tool's own text is carried as it came, for the caller
// to redact.
const (
	ClassTimeout     = "timeout"
	ClassTransport   = "transport"
	ClassProtocol    = "protocol"
	ClassServerError = "server error"
	ClassToolError   = "tool error"
	ClassTooLarge    = "too large"
)

// Error is a failure of a call or a listing.
type Error struct {
	Class string
	// Status is an HTTP status, when one was answered.
	Status int
	// Code and Message are a JSON-RPC error's, as the server gave them.
	Code    int
	Message string
	Detail  string
	// Sessions are the session ids the server gave, which its text may
	// repeat: secrets the caller redacts, never printed here.
	Sessions []string
	// ended marks a session the server ended while an answer was read.
	ended bool
}

func (e *Error) Error() string {
	s := e.Class
	if e.Status != 0 {
		s += fmt.Sprintf(": HTTP %d", e.Status)
	}
	if e.Code != 0 {
		s += fmt.Sprintf(": JSON-RPC %d", e.Code)
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	return s
}

func failure(class, format string, args ...any) *Error {
	return &Error{Class: class, Detail: fmt.Sprintf(format, args...)}
}

// timedOut turns a context that ended into the timeout class.
func timedOut(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return failure(ClassTimeout, "the exchange did not end in time")
	}
	return err
}

// Call calls one tool once. The era is found by the first exchange and holds
// for the rest of it; a call that may have run is never sent again.
func Call(ctx context.Context, s Server, tool string, args map[string]any) (*Called, error) {
	x, err := open(ctx, s)
	if err != nil {
		return nil, err
	}
	defer x.close()
	c, err := x.call(ctx, tool, args)
	if c != nil {
		c.Sessions = x.secrets()
	}
	return c, withSessions(timedOut(ctx, err), x)
}

// List lists a server's tools, following at most pages pages of them.
func List(ctx context.Context, s Server, pages int) (*Listed, error) {
	x, err := open(ctx, s)
	if err != nil {
		return nil, err
	}
	defer x.close()
	l, err := x.list(ctx, pages)
	if l != nil {
		l.Sessions = x.secrets()
	}
	return l, withSessions(timedOut(ctx, err), x)
}

// withSessions gives a failure the session ids its exchange was given.
func withSessions(err error, x exchange) error {
	var e *Error
	if errors.As(err, &e) {
		e.Sessions = x.secrets()
	}
	return err
}

// exchange is one conversation with a server: what each transport keeps
// between its requests.
type exchange interface {
	call(ctx context.Context, tool string, args map[string]any) (*Called, error)
	list(ctx context.Context, pages int) (*Listed, error)
	// secrets are the session ids the server gave.
	secrets() []string
	close()
}

func open(ctx context.Context, s Server) (exchange, error) {
	if s.MaxMessage <= 0 {
		s.MaxMessage = 16 << 20
	}
	if s.Probe <= 0 {
		s.Probe = 10 * time.Second
	}
	switch {
	case s.URL != "" && len(s.Command) == 0:
		return openHTTP(s), nil
	case s.URL == "" && len(s.Command) > 0:
		return openStdio(ctx, s)
	}
	return nil, errors.New("mcpclient: a server is a URL or a command, exactly one")
}
