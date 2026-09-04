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

// Package sessionview defines the Conversation View: one conversation,
// rebuilt from its Session Flow and its Session Data, as one document.
//
// It is never a file. asz view builds it in memory and serves it as one
// response, and a server that holds the same landed files and rounds, such
// as the SkyWalking OAP, builds the same document and answers a conversation
// query with it. This package defines and owns the shape; every reader
// shares it, and a change to it is a change to the schema below.
//
// Times in a view are unix milliseconds, because a view is read and never
// digested. Session Data and Session Flow carry RFC 3339 strings, because
// their bytes are.
package sessionview

import (
	"encoding/json"

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// Schema is the version of the document shape.
const Schema = "cv/1"

// Conversation is the whole view.
type Conversation struct {
	Schema  string `json:"schema"`
	ID      string `json:"conversation"`
	Session string `json:"session"`
	// Title is the last name the runtime gave the session, or empty.
	Title string `json:"title,omitempty"`
	// From and To are when the session began and its last activity, from
	// the session node.
	From int64 `json:"from"`
	To   int64 `json:"to"`

	Head   Head    `json:"head"`
	Rounds []Round `json:"rounds"`
	Files  []File  `json:"files"`

	Counts        Counts         `json:"counts"`
	Kinds         map[string]int `json:"kinds"`
	RelationTypes map[string]int `json:"relation_types"`
	Quality       map[string]int `json:"quality"`

	Streams    []Stream     `json:"streams"`
	Segments   []Segment    `json:"segments"`
	Talks      []Talk       `json:"talks"`
	Relations  []Relation   `json:"relations"`
	Unresolved []Unresolved `json:"unresolved"`
}

// Head identifies the fold the view was built from.
type Head struct {
	Round       uint64 `json:"round"`
	Digest      string `json:"digest"`
	ThroughSeq  uint64 `json:"through_seq"`
	InputDigest string `json:"input_digest"`
	Parser      string `json:"parser"`
	Policy      string `json:"policy"`
}

// Round is one round of the chain, from its header and its file.
type Round struct {
	Round       uint64 `json:"round"`
	Digest      string `json:"digest"`
	Previous    string `json:"previous,omitempty"`
	FromSeq     uint64 `json:"from_seq"`
	ThroughSeq  uint64 `json:"through_seq"`
	InputDigest string `json:"input_digest"`
	// From and To are the record time range of the files the round
	// consumed; SessionFrom and SessionTo the session's range as of the
	// round. Zero when the header carries none.
	From        int64  `json:"from,omitempty"`
	To          int64  `json:"to,omitempty"`
	SessionFrom int64  `json:"session_from,omitempty"`
	SessionTo   int64  `json:"session_to,omitempty"`
	Lines       int    `json:"lines"`
	Bytes       int64  `json:"bytes"`
	FileDigest  string `json:"file_digest"`
	File        string `json:"file"`
}

// File is one landed file of the session.
type File struct {
	File    string `json:"file"`
	Format  string `json:"format"`
	Version string `json:"version"`
	Kind    string `json:"kind"`
	Seq     uint64 `json:"seq"`
	Stream  string `json:"stream,omitempty"`
	Run     string `json:"run,omitempty"`
	Lines   int    `json:"lines"`
	Bytes   int64  `json:"bytes"`
	Digest  string `json:"digest"`
	// From and To are the record time range of the file. Zero when no
	// record carries a time.
	From int64 `json:"from,omitempty"`
	To   int64 `json:"to,omitempty"`
}

// Counts sizes the fold.
type Counts struct {
	Nodes      int `json:"nodes"`
	Relations  int `json:"relations"`
	Unresolved int `json:"unresolved"`
	Talks      int `json:"talks"`
	Steps      int `json:"steps"`
	Streams    int `json:"streams"`
	Segments   int `json:"segments"`
}

// Stream is one execution stream, with the step that started it.
type Stream struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Role    string `json:"role"`
	Label   string `json:"label"`
	NamedBy string `json:"named_by"`
	Records int    `json:"records"`
	Parent  string `json:"parent"`
	Talk    string `json:"talk"`
	Steps   int    `json:"steps"`
	// OpenedBy lists every step the assembler could tie to the start of
	// this stream, with the quality of each. Several means it did not
	// choose, and neither does a view.
	OpenedBy []Origin `json:"opened_by"`
}

// Origin is one step that may have started a stream.
type Origin struct {
	Step    string `json:"step"`
	Stream  string `json:"stream"`
	Quality string `json:"quality"`
	Talk    string `json:"talk"`
}

// Segment is one activity window, with the span of the talks placed in it.
type Segment struct {
	ID          string `json:"id"`
	State       string `json:"state"`
	Talks       int    `json:"talks"`
	From        int64  `json:"from"`
	To          int64  `json:"to"`
	Committable bool   `json:"committable"`
}

// Talk is one readable interaction: its summary, and its tree of runs and
// steps when the view carries trees.
type Talk struct {
	ID      string `json:"id"`
	Stream  string `json:"stream"`
	Label   string `json:"label"`
	Runs    int    `json:"runs"`
	Steps   int    `json:"steps"`
	Tools   int    `json:"tools"`
	From    int64  `json:"from"`
	To      int64  `json:"to"`
	Child   bool   `json:"child"`
	Segment string `json:"segment,omitempty"`
	// Reply is the first 2,000 bytes of the talk's last assistant message.
	Reply string `json:"reply,omitempty"`
	Tree  *Node  `json:"tree,omitempty"`
}

// Node is one entity of the fold with what its record says: the readable
// text of the part it stands on, clipped to 2,000 bytes, with the full size
// in Bytes. The whole record is read by address, {seq, row}, from the file.
type Node struct {
	ID     string            `json:"id"`
	Kind   string            `json:"kind"`
	Parent string            `json:"parent,omitempty"`
	Stream string            `json:"stream,omitempty"`
	At     int64             `json:"at"`
	Ref    *sessionflow.Ref  `json:"ref,omitempty"`
	Refs   []sessionflow.Ref `json:"refs,omitempty"`
	Attrs  json.RawMessage   `json:"attrs,omitempty"`
	Text   string            `json:"text,omitempty"`
	Name   string            `json:"name,omitempty"`
	Failed *bool             `json:"failed,omitempty"`
	State  string            `json:"state,omitempty"`
	Bytes  int               `json:"bytes,omitempty"`

	Result      string `json:"result,omitempty"`
	ResultState string `json:"result_state,omitempty"`
	ResultBytes int    `json:"result_bytes,omitempty"`

	DurationMS  int64  `json:"duration_ms,omitempty"`
	DurationHow string `json:"duration_measured_by,omitempty"`

	RequestToResultMS int64  `json:"request_to_result_ms,omitempty"`
	RequestToResultBy string `json:"request_to_result_join,omitempty"`

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
