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

// Package providerbody stores the bodies a runtime exchanged with its model
// provider, one record per body, with what a session already holds taken out
// and every body rebuildable byte for byte.
//
// Every call sends the whole message list of its chain again, so a body is
// mostly bytes an earlier body of the same session already had. A record keeps
// only what is new. Its last part is a manifest: the segments that, joined in
// order, give the body back. A segment is literal bytes, a part of the record,
// a piece an earlier record holds, or the front of an earlier body.
//
// A piece is a JSON value cut out of the body whole: each tool definition, and
// every string of MinPiece bytes or more anywhere else. A body is compact JSON,
// so every value is one contiguous run of its bytes. Pieces are named by their
// SHA-256, and an earlier body by its record id and SHA-256, never by file and
// row, because asz repack renumbers files and rows and carries records whole.
package providerbody

import (
	"crypto/sha256"
	"encoding/hex"
)

// Schema is the manifest's schema.
const Schema = "provider_body/1"

// Roles of a body.
const (
	RoleRequest  = "request"
	RoleResponse = "response"
)

const (
	// MinPiece is the smallest string cut out as a piece. Measured on two
	// captures of Claude Code 2.1.260, 1 KiB kept 25.9% and 23.2% of the raw
	// bytes; 256 bytes kept 1 to 4 points less with three to four times as
	// many parts.
	MinPiece = 1 << 10
	// MinCopy is the shortest front of an earlier body worth a copy segment.
	// The first body of a chain shares only a few hundred bytes with anything
	// earlier, and a copy of that saves nothing.
	MinCopy = 4 << 10
	// MaxDepth bounds a chain of copies. Rebuilding a body rebuilds its base
	// first, so a body whose base is this deep lands without a copy and
	// starts a new chain. Not measured on a long session yet.
	MaxDepth = 32
	// MaxBytes bounds the size a body may claim, and a rebuild stops as soon
	// as it passes the size its record claims. A record is small, but its
	// references can name large pieces many times over, so without a bound
	// a damaged record could make a rebuild hold far more than any body.
	MaxBytes = 256 << 20
)

// Manifest is the last part of a provider_body record. Its keys are the
// model's words; a runtime's own names for these values stop at its adapter.
type Manifest struct {
	Schema string `json:"schema"`
	Role   string `json:"role"`
	// Src is where the body came from, relative to the adapter's source
	// root, as a header's src is.
	Src string `json:"src"`
	// SHA256 and Bytes describe the whole body, which the segments give back.
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
	// Depth counts the copies between this body and one with none.
	Depth int `json:"depth"`
	// Chain names the chain a request belongs to: the first sixteen
	// hexadecimal characters of the SHA-256 of its first message, with the
	// moving cache marker taken out. It is storage, not structure: it picks
	// the body a copy comes from, and says nothing a join may rely on.
	Chain string `json:"chain,omitempty"`
	// Why says the body is kept whole, in part 0, and why it was not cut.
	Why string `json:"why,omitempty"`

	// The values the body carries that join it, as its adapter read them,
	// so a reader can join a body without rebuilding it. Empty when the
	// body does not carry one.
	Model           string `json:"model,omitempty"`
	Session         string `json:"session,omitempty"`
	Run             string `json:"run,omitempty"`
	Call            string `json:"call,omitempty"`
	Request         string `json:"request,omitempty"`
	PreviousRequest string `json:"previous_request,omitempty"`

	Segments []Segment `json:"segments"`
}

// Segment is one run of a body's bytes. Exactly one field is set.
type Segment struct {
	// Lit holds bytes that are no piece, as a string: the keys, the
	// punctuation, the short values.
	Lit string `json:"lit,omitempty"`
	// Part is a part of this record: its data, or an unknown part's bytes.
	Part *int `json:"part,omitempty"`
	// Piece is the SHA-256 of a piece an earlier record of the session holds.
	Piece string `json:"piece,omitempty"`
	// Copy is the front of an earlier body of the session.
	Copy *Copy `json:"copy,omitempty"`
}

// Copy names the first Len bytes of an earlier body.
type Copy struct {
	From   string `json:"from"`
	SHA256 string `json:"sha256"`
	Len    int    `json:"len"`
}

// Keys are the values a body carries that join it to a session and a call,
// in the model's words. An adapter reads them from its runtime's bodies.
type Keys struct {
	Model string
	// Session is the session the body names.
	Session string
	// Run is the run, one prompt cycle, a request was sent in.
	Run string
	// Call is the provider call a response answers, and the call a request
	// was sent to when its adapter knows it. A runtime that names a request
	// before the provider has answered cannot know it.
	Call string
	// Request is the provider's id for the request a response answers.
	Request string
	// PreviousRequest is the provider request id of the call before a
	// request in its chain.
	PreviousRequest string
}

// Body is one body for a session to cut: its record id, which is unique in the
// session and stable however often it is read, its role, where it came from,
// the keys its adapter read, and its bytes.
type Body struct {
	ID    string
	Role  string
	Src   string
	Keys  Keys
	Bytes []byte
}

// Digest is the SHA-256 of b in hexadecimal, as a manifest and a copy name
// a body and a piece.
func Digest(b []byte) string { return digest(b) }

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
