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

// Package changes defines the workspace change record, changes/1: which
// files one tool call changed, as a git-style change log.
//
// The same shape is written by three producers. The Claude Code plugin
// writes one for every shell command it observed and for every edit inside
// a subagent. The Claude Code adapter derives one from the patch the runtime
// records for its own editing tools. A scenario writes one directly. A view
// joins each to its step by the tool-use id and shows the hunks, so a reader
// sees one Changes tab whichever producer wrote the record.
//
// A record is one JSON object. Field names are the model's, never a
// runtime's. The hunk shape is a unified diff as JSON: start and count on
// each side, and lines prefixed with "-", "+" or a space. It is also the
// shape Claude Code writes for its own patches, so the adapter copies those
// almost unchanged.
package changes

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// Schema is the record's format version, carried in every record.
const Schema = "changes/1"

// Basis says how a record's changes were observed.
const (
	// BasisToolWindow is a comparison of the workspace before and after a
	// tool ran, made by the plugin.
	BasisToolWindow = "tool_window"
	// BasisRuntimeReported is a patch the runtime itself reported for its
	// own editing tool, copied into this shape.
	BasisRuntimeReported = "runtime_reported"
	// BasisUnattributed is a change found between two observed windows, by
	// something no window covered: a person, an editor, an unhooked tool.
	BasisUnattributed = "unattributed"
	// BasisSkippedReadOnly says the command was classified read-only and
	// no comparison was made. Such a record holds no changes and the
	// absence means "not observed", never "nothing changed".
	BasisSkippedReadOnly = "skipped_read_only"
)

// Operation says what happened to a file.
const (
	OpCreate     = "create"
	OpModify     = "modify"
	OpDelete     = "delete"
	OpTypeChange = "type_change"
)

// Diff says whether hunks are available for a file, and why not.
const (
	DiffAvailable   = "available"
	DiffBinary      = "binary"
	DiffTooLarge    = "too_large"
	DiffUnavailable = "unavailable"
)

// Attribution says which tool windows could have made a file's change.
const (
	AttributionOnlyThisWindow   = "only_this_window"
	AttributionShared           = "shared"
	AttributionOutsideAnyWindow = "outside_any_window"
)

// Coverage says whether the whole declared scope was observed.
const (
	CoverageComplete = "complete"
	CoveragePartial  = "partial"
)

// Outcome states of the tool call a window belongs to.
const (
	OutcomeReturned    = "returned"
	OutcomeFailed      = "failed"
	OutcomeNotExecuted = "not_executed"
	OutcomeUnfinished  = "unfinished"
)

// Record is one change set: what one observation found.
type Record struct {
	Schema string `json:"schema"`
	// ID is stable across producers: a producer prefix and a capture id.
	ID      string `json:"id"`
	Session string `json:"session"`
	// Stream is main or the agent id the tool ran under.
	Stream string `json:"stream"`
	// Tool is the tool-use id the record joins to. Absent on an
	// unattributed record.
	Tool     string `json:"tool,omitempty"`
	ToolName string `json:"tool_name,omitempty"`
	// Time is when the observation ended, RFC 3339.
	Time  string `json:"time"`
	Basis string `json:"basis"`

	Root   *Root   `json:"root,omitempty"`
	Policy *Policy `json:"policy,omitempty"`
	Window *Window `json:"window,omitempty"`
	// Outcome is what the tool call reported, when the producer knows.
	Outcome  *Outcome `json:"outcome,omitempty"`
	Coverage string   `json:"coverage,omitempty"`
	Gaps     []string `json:"gaps,omitempty"`
	// Overlaps lists every other window open on the same root during
	// this one, so a reader can follow a shared change to the other side.
	Overlaps []Overlap `json:"overlaps,omitempty"`

	// ChangedFiles is null when unknown, which is different from zero.
	ChangedFiles *int         `json:"changed_files"`
	Changes      []FileChange `json:"changes"`
}

// Root is the workspace the paths are relative to.
type Root struct {
	Path string `json:"path"`
	ID   string `json:"id,omitempty"`
}

// Policy names the rule sets the observation used, and the rules they
// expanded to, so history explains itself after a rule set changes.
type Policy struct {
	Exclusions string   `json:"exclusions,omitempty"`
	ReadOnly   string   `json:"read_only,omitempty"`
	Expanded   []string `json:"expanded,omitempty"`
}

// Window is the two scans a comparison was made between.
type Window struct {
	Before Interval `json:"before"`
	After  Interval `json:"after"`
}

// Interval is one scan's start and end, RFC 3339.
type Interval struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Outcome is what the runtime reported about the tool call.
type Outcome struct {
	State string `json:"state"`
	// ExitCode is null unless the runtime reported one.
	ExitCode *int `json:"exit_code"`
}

// Overlap names another window that was open on the same root.
type Overlap struct {
	Capture  string `json:"capture"`
	Session  string `json:"session"`
	Stream   string `json:"stream"`
	Tool     string `json:"tool,omitempty"`
	ToolName string `json:"tool_name,omitempty"`
	// State is closed or open, as of the time this record was written.
	State string `json:"state"`
}

// FileChange is one changed file.
type FileChange struct {
	// Path is relative to the root, with forward slashes.
	Path      string   `json:"path"`
	Operation string   `json:"operation"`
	Before    Endpoint `json:"before"`
	After     Endpoint `json:"after"`
	Diff      string   `json:"diff"`
	// Attribution and Windows say which windows could have made this
	// change. Both are absent on a runtime-reported record.
	Attribution string   `json:"attribution,omitempty"`
	Windows     []string `json:"windows,omitempty"`
	// Additions and Deletions are null when no diff was made.
	Additions *int   `json:"additions"`
	Deletions *int   `json:"deletions"`
	Hunks     []Hunk `json:"hunks,omitempty"`
}

// Endpoint is a file's state on one side of a change.
type Endpoint struct {
	Present bool `json:"present"`
	// Bytes and SHA256 are null and empty when the file was absent.
	Bytes  *int64 `json:"bytes"`
	SHA256 string `json:"sha256,omitempty"`
	// NoNewlineAtEnd says the text did not end with a newline, which a
	// hunk cannot show on its own.
	NoNewlineAtEnd bool `json:"no_newline_at_end,omitempty"`
}

// Hunk is one block of a unified diff. Lines carry a one-character prefix:
// "-" removed, "+" added, " " unchanged context.
type Hunk struct {
	OldStart int      `json:"old_start"`
	OldLines int      `json:"old_lines"`
	NewStart int      `json:"new_start"`
	NewLines int      `json:"new_lines"`
	Lines    []string `json:"lines"`
}

// EndpointOf describes one side of a file from its bytes. An absent file
// has no bytes and no hash; an empty file has both, at zero and the hash
// of nothing, which is the difference between "gone" and "emptied".
func EndpointOf(b []byte, present bool) Endpoint {
	if !present {
		return Endpoint{Present: false}
	}
	sum := sha256.Sum256(b)
	e := Endpoint{Present: true, Bytes: Int64(int64(len(b))), SHA256: hex.EncodeToString(sum[:])}
	if len(b) > 0 && b[len(b)-1] != '\n' {
		e.NoNewlineAtEnd = true
	}
	return e
}

// Int returns a pointer to n, for the fields that are null when unknown.
func Int(n int) *int { return &n }

// Int64 returns a pointer to n.
func Int64(n int64) *int64 { return &n }

// Decode reads a record and reports whether the bytes are one: a JSON
// object whose schema is this package's. Anything else is not an error,
// it is simply not a change record, because the same data part may hold
// other things.
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
		return fmt.Errorf("changes: schema %q, want %q", r.Schema, Schema)
	case r.ID == "":
		return errors.New("changes: record has no id")
	case r.Session == "":
		return errors.New("changes: record has no session")
	case r.Stream == "":
		return errors.New("changes: record has no stream")
	case r.Time == "":
		return errors.New("changes: record has no time")
	case r.Basis == "":
		return errors.New("changes: record has no basis")
	case r.Basis != BasisUnattributed && r.Tool == "":
		return errors.New("changes: an attributed record names its tool")
	}
	for i, c := range r.Changes {
		if c.Path == "" {
			return fmt.Errorf("changes: change %d has no path", i)
		}
		if c.Operation == "" {
			return fmt.Errorf("changes: %s has no operation", c.Path)
		}
		if c.Diff == "" {
			return fmt.Errorf("changes: %s says nothing about its diff", c.Path)
		}
	}
	return nil
}

// Marshal writes a record as one JSON line without a trailing newline.
func (r *Record) Marshal() ([]byte, error) {
	if r.Changes == nil {
		r.Changes = []FileChange{}
	}
	return json.Marshal(r)
}
