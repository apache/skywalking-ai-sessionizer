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

// Package execution defines the tool execution record, execution/1: what
// one observer saw a tool call do after the model asked for it.
//
// A transcript holds what the model asked for and what came back. It does
// not say which server ran the call, how long the call took, or what the
// server was sent, which can differ from what the model wrote. An observer
// around the call can say those things. Today the one observer is the Claude
// Code plugin, whose hooks run after every call to an MCP server.
//
// A record says which observer wrote it and where that observer stands. One
// call can have several records: a retry, a bridge that calls several
// servers, or two observers of the same call. Each has an id of its own, and
// all of them name the call by its tool-use id, which is the only join. A
// record's field names are the model's, never a runtime's.
package execution

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Schema is the record's format version, carried in every record.
const Schema = "execution/1"

// Who observed the call.
const (
	// ObservedByASZPlugin is the asz plugin, from the runtime's own hooks.
	ObservedByASZPlugin = "asz-plugin"
)

// Where the observer stands. Two observers at different places can see
// different parameters for one call, so a record says which place it saw.
const (
	// BoundaryClientHook is the runtime's hook after the call: what the
	// runtime sent the server once every hook before the call had run, and
	// what the runtime made of the answer. Measured on Claude Code 2.1.282:
	// a hook that rewrote the input before the call changed what the server
	// received and what this hook sees, while the transcript kept the
	// model's own input.
	BoundaryClientHook = "client_hook"
)

// Protocol is what the call went over.
const (
	ProtocolMCP = "mcp"
)

// Outcome is what the observer saw the call end with.
const (
	// OutcomeReturned is an answer the runtime did not treat as an error.
	OutcomeReturned = "returned"
	// OutcomeFailed is a call the runtime reported as failed: an error the
	// server returned, a lost connection, or a timeout. Measured on Claude
	// Code 2.1.282, an MCP result marked isError reaches the failure hook,
	// not the success hook.
	OutcomeFailed = "failed"
	// OutcomeInterrupted is a call a person stopped.
	OutcomeInterrupted = "interrupted"
)

// Content states for what a record says about the parameters and the
// answer. Only their size is kept today, and the record says so.
const (
	ContentSizeOnly = "size_only"
)

// Record is one observation of one tool call.
type Record struct {
	Schema string `json:"schema"`
	// ID is this observation's own identity, unique among the records of a
	// session. It is not the call's id: one call can have several records.
	ID         string `json:"id"`
	ObservedBy string `json:"observed_by"`
	Boundary   string `json:"boundary"`
	Session    string `json:"session"`
	// Stream is main or the agent id the call ran under.
	Stream string `json:"stream"`
	// Tool is the tool-use id of the call this record observed, the join to
	// the call's step. ToolName is the name the runtime called it by.
	Tool     string `json:"tool"`
	ToolName string `json:"tool_name,omitempty"`
	// Cwd is the working directory the observer reported, which the
	// collector's session filters are judged by.
	Cwd      string  `json:"cwd,omitempty"`
	Protocol string  `json:"protocol"`
	Server   *Server `json:"server,omitempty"`
	// Time is when the observation ended, RFC 3339.
	Time string `json:"time"`
	// DurationMS is the time the observer measured, in milliseconds. It is
	// the runtime's own measure around the call, so it includes any waiting
	// before the call started, and it is not the server's own time. Absent
	// when the observer did not say.
	DurationMS *int64 `json:"duration_ms,omitempty"`
	Outcome    string `json:"outcome"`
	// Arguments are what the observer saw sent. Result is what came back,
	// for a call that returned.
	Arguments *Content `json:"arguments,omitempty"`
	Result    *Content `json:"result,omitempty"`
}

// Server is the server a call went to, as the observer names it.
type Server struct {
	Name string `json:"name"`
	// Source is where the runtime's configuration of the server came from,
	// in the runtime's own word: for Claude Code "user", "project", "local",
	// or "dynamic" for a server given on the command line.
	Source string `json:"source,omitempty"`
}

// Content is what a record keeps of the parameters or the answer: only
// their size and a digest, never their text.
type Content struct {
	State string `json:"state"`
	// Bytes is the size of the value as compact JSON with sorted keys.
	Bytes int `json:"bytes"`
	// SHA256 is the digest of the same bytes. Two observations of one value
	// have one digest, whichever order its keys were written in, so it can
	// tell whether the parameters were changed on the way without keeping
	// them.
	SHA256 string `json:"sha256,omitempty"`
}

// Measure keeps the size and the digest of a JSON value. A value that is not
// JSON, or is followed by more than white space, is measured as the bytes it
// is.
func Measure(raw []byte) *Content {
	b := raw
	dec := json.NewDecoder(bytes.NewReader(raw))
	// A number keeps its own text. Decoded as a float64, two integers above
	// 2^53 would become one value, and a changed argument would keep its
	// digest.
	dec.UseNumber()
	var v any
	if dec.Decode(&v) == nil {
		if _, err := dec.Token(); errors.Is(err, io.EOF) {
			if c, err := canonical(v); err == nil {
				b = c
			}
		}
	}
	sum := sha256.Sum256(b)
	return &Content{State: ContentSizeOnly, Bytes: len(b), SHA256: hex.EncodeToString(sum[:])}
}

// canonical is compact JSON with every object's keys sorted. A decoded
// object is a map, and encoding/json writes a map's keys in sorted order, so
// encoding the decoded value is enough. A number is written as its own text.
func canonical(v any) ([]byte, error) {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return []byte(strings.TrimSuffix(sb.String(), "\n")), nil
}

// Decode reads a record and reports whether the bytes are one: a JSON
// object whose schema is this package's.
func Decode(raw []byte) (*Record, bool) {
	if len(raw) == 0 || raw[0] != '{' {
		return nil, false
	}
	var r Record
	if err := json.Unmarshal(raw, &r); err != nil || r.Schema != Schema {
		return nil, false
	}
	return &r, true
}

// Validate reports a record that cannot be used.
func (r *Record) Validate() error {
	switch {
	case r.Schema != Schema:
		return fmt.Errorf("execution: schema %q, want %q", r.Schema, Schema)
	case r.ID == "":
		return errors.New("execution: record has no id")
	case r.ObservedBy == "":
		return errors.New("execution: record does not say who observed it")
	case r.Boundary == "":
		return errors.New("execution: record does not say where it was observed")
	case r.Session == "":
		return errors.New("execution: record has no session")
	case r.Stream == "":
		return errors.New("execution: record has no stream")
	case r.Tool == "":
		return errors.New("execution: record names no tool call")
	case r.Protocol == "":
		return errors.New("execution: record has no protocol")
	case r.Time == "":
		return errors.New("execution: record has no time")
	case r.Outcome == "":
		return errors.New("execution: record has no outcome")
	}
	return nil
}

// Marshal writes a record as one JSON line without a trailing newline.
func (r *Record) Marshal() ([]byte, error) {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		return nil, err
	}
	return []byte(strings.TrimSuffix(sb.String(), "\n")), nil
}
