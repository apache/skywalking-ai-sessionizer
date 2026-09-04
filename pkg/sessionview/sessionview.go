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

// Package sessionview defines the asz.view document: one conversation,
// rebuilt from its Session Flow and its Session Data, as one document that
// holds everything a viewer renders and nothing a viewer must compute.
//
// It is never a file. asz view builds it in memory and serves it as one
// response, and a server that holds the same landed files and rounds, such
// as the SkyWalking OAP, builds the same document and answers a conversation
// query with it. This package defines and owns the shape. A 1.x version adds
// keys and never removes or renames one; a 2.0 may do either.
//
// The document is JSON. Keys are snake_case, as in the two source formats,
// and are written in the order the types below list them. Times are unix
// milliseconds, read from the .sd record a node references; a view is read
// and never digested, so it carries no RFC 3339 strings.
package sessionview

import (
	"encoding/json"

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// Format and Version are the first two keys of every document. A reader
// that does not know the version stops there.
const (
	Format  = "asz.view"
	Version = "1.0"
)

// Conversation is the whole document.
type Conversation struct {
	Format  string `json:"format"`
	Version string `json:"version"`

	// Conversation is the id, and Sessions the sessions that contributed
	// to it: one, equal to the id, for the Claude Code adapter.
	Conversation string   `json:"conversation"`
	Sessions     []string `json:"sessions"`

	// Head is the newest round the document was folded to.
	Head   Head   `json:"head"`
	Parser string `json:"parser"`
	Policy string `json:"policy"`

	Summary Summary `json:"summary"`

	Rounds     []Round      `json:"rounds"`
	Files      []File       `json:"files"`
	Streams    []Stream     `json:"streams"`
	Segments   []Segment    `json:"segments"`
	Talks      []Node       `json:"talks"`
	Relations  []Relation   `json:"relations"`
	Unresolved []Unresolved `json:"unresolved"`
}

// Head identifies the fold the document was built from.
type Head struct {
	Round  uint64 `json:"round"`
	Digest string `json:"digest"`
}

// Summary is what a list or a header shows without opening the rest.
// Verification is content, not an error: State is "verified", "incomplete"
// when a round or a file is missing, or "mismatch" when a digest failed,
// and Problems says which, one line each. The rest of the document holds
// whatever could still be folded.
type Summary struct {
	Title    string   `json:"title"`
	State    string   `json:"state"`
	Problems []string `json:"problems"`

	Talks      int `json:"talks"`
	Steps      int `json:"steps"`
	Streams    int `json:"streams"`
	Segments   int `json:"segments"`
	Rounds     int `json:"rounds"`
	Unresolved int `json:"unresolved"`

	// From and To are when the session began and its last activity, from
	// the session node.
	From int64 `json:"from"`
	To   int64 `json:"to"`

	// Kinds, RelationTypes and Quality size the fold by node kind, by
	// relation type, and by how well each relation is known.
	Kinds         map[string]int `json:"kinds"`
	RelationTypes map[string]int `json:"relation_types"`
	Quality       map[string]int `json:"quality"`
}

// The three verification states.
const (
	StateVerified   = "verified"
	StateIncomplete = "incomplete"
	StateMismatch   = "mismatch"
)

// Round is one round of the chain, from its header. Its file is listed
// under Files like every other file.
type Round struct {
	Round       uint64  `json:"round"`
	Digest      string  `json:"digest"`
	Previous    *string `json:"previous"`
	FromSeq     uint64  `json:"from_seq"`
	ThroughSeq  uint64  `json:"through_seq"`
	InputDigest string  `json:"input_digest"`
	// FromTime and ThroughTime are the record time range of the files the
	// round consumed; null when none carries a time.
	FromTime    *int64 `json:"from_time"`
	ThroughTime *int64 `json:"through_time"`
	// Verified says the round's digest, its link to the round before and
	// its input digest over the landed files all held.
	Verified bool `json:"verified"`
}

// File is one landed file or one round file, as it was on the wire.
type File struct {
	File   string  `json:"file"`
	Format string  `json:"format"` // "sd" or "sf"
	Kind   string  `json:"kind"`
	Seq    *uint64 `json:"seq"`
	Round  *uint64 `json:"round"`
	Stream *string `json:"stream"`
	Run    *string `json:"run"`
	Lines  int     `json:"lines"`
	Bytes  int64   `json:"bytes"`
	Digest string  `json:"digest"`
	// FromTime and ThroughTime are the record time range of the file; null
	// when no record carries a time.
	FromTime    *int64 `json:"from_time"`
	ThroughTime *int64 `json:"through_time"`
}

// Stream is one execution stream, with the step that started it.
type Stream struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Role    string `json:"role"` // "main" or "child"
	Label   string `json:"label"`
	Parent  string `json:"parent"`
	Records int    `json:"records"`
	Steps   int    `json:"steps"`
	Talk    string `json:"talk"`
	NamedBy string `json:"named_by"`
	// OpenedBy lists every step the assembler could tie to the start of
	// this stream, with the quality of each. Several means it did not
	// choose, and neither does a view.
	OpenedBy []Origin `json:"opened_by"`
}

// Origin is one step that may have started a stream.
type Origin struct {
	Step    string `json:"step"`
	Stream  string `json:"stream"`
	Talk    string `json:"talk"`
	Quality string `json:"quality"`
}

// Segment is one activity window, with the span of the talks placed in it.
type Segment struct {
	ID          string `json:"id"`
	State       string `json:"state"`
	Committable bool   `json:"committable"`
	Talks       int    `json:"talks"`
	From        int64  `json:"from"`
	To          int64  `json:"to"`
}

// Node is one entity of the fold with what its record says, and its
// children: a talk holds runs, a run holds steps, a call holds what it
// produced. Text is the readable text of the part the node stands on,
// clipped to 2,000 bytes, with the full size in Bytes; a viewer wanting the
// whole record reads it by address. A talk adds its summary: Label, Reply,
// Runs, Steps, Tools, From, To, Child and Segment. A tool or agent call adds
// Name and what came back.
type Node struct {
	ID     string            `json:"id"`
	Kind   string            `json:"kind"`
	Parent string            `json:"parent,omitempty"`
	Stream string            `json:"stream,omitempty"`
	At     int64             `json:"at"`
	Ref    *sessionflow.Ref  `json:"ref,omitempty"`
	Refs   []sessionflow.Ref `json:"refs,omitempty"`
	Attrs  json.RawMessage   `json:"attrs,omitempty"`

	Text  string `json:"text,omitempty"`
	State string `json:"state,omitempty"`
	Bytes int    `json:"bytes,omitempty"`

	// Usage, Flags and Dropped are what else the referenced record says,
	// copied once: on an llm.call the token counts from the one record its
	// usage_at names; the record's flags; and what the conversion left out.
	Usage   *sessiondata.Usage `json:"usage,omitempty"`
	Flags   []string           `json:"flags,omitempty"`
	Dropped []sessiondata.Drop `json:"dropped,omitempty"`

	// A talk adds these.
	Label   string `json:"label,omitempty"`
	Reply   string `json:"reply,omitempty"`
	Runs    int    `json:"runs,omitempty"`
	Steps   int    `json:"steps,omitempty"`
	Tools   int    `json:"tools,omitempty"`
	From    int64  `json:"from,omitempty"`
	To      int64  `json:"to,omitempty"`
	Child   bool   `json:"child,omitempty"`
	Segment string `json:"segment,omitempty"`

	// A tool or agent call adds these.
	Name              string `json:"name,omitempty"`
	Failed            *bool  `json:"failed,omitempty"`
	Result            string `json:"result,omitempty"`
	ResultState       string `json:"result_state,omitempty"`
	ResultBytes       int    `json:"result_bytes,omitempty"`
	RequestToResultMS int64  `json:"request_to_result_ms,omitempty"`
	RequestToResultBy string `json:"request_to_result_join,omitempty"`

	// A turn.duration step adds these.
	DurationMS  int64  `json:"duration_ms,omitempty"`
	DurationHow string `json:"duration_measured_by,omitempty"`

	Children []Node `json:"children,omitempty"`
	Edges    []Edge `json:"edges,omitempty"`
}

// Edge is one relation seen from a node.
type Edge struct {
	Type    string `json:"type"`
	Other   string `json:"other"`
	Dir     string `json:"dir"` // "out" or "in"
	Quality string `json:"quality"`
	Via     string `json:"via,omitempty"`
}

// Relation is one relation of the fold.
type Relation struct {
	ID       string            `json:"id"`
	Type     string            `json:"type"`
	From     string            `json:"from"`
	To       string            `json:"to"`
	Quality  string            `json:"quality"`
	Via      string            `json:"via,omitempty"`
	Evidence []sessionflow.Ref `json:"evidence,omitempty"`
}

// Unresolved is one reference the assembler could not resolve, open or
// since resolved.
type Unresolved struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Ref    string `json:"ref"`
	Reason string `json:"reason"`
	State  string `json:"state"`
}
