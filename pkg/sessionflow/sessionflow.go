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

// Package sessionflow defines how a conversation's structure is written down:
// an append-only chain of immutable rounds, one file each, extension .sf.
//
// Session Flow is the structure - how the conversation went. The detail of what
// was in it is Session Data, and a node here references it rather than carrying
// it.
//
// A round is not a snapshot. It carries only what changed, and the conversation
// is the FOLD of every round from the first to the latest:
//
//	state₀ = empty
//	stateₙ = apply(stateₙ₋₁, roundₙ)
//
// Each round is written once, made read-only, and never revised. Later evidence
// - a tool result that arrives after its call, a child transcript that appears
// after its spawn - produces a new revision of an entity in a LATER round, never
// an edit to an earlier one. That is what lets a round be digested, archived, or
// shipped the instant it is written.
//
// Rounds are linked by digest, not by filename: round N names the digest of
// round N-1. A holder of round 3 that has not seen round 2 can store it but
// cannot apply it.
package sessionflow

import (
	"encoding/json"
	"fmt"
)

// Schema is the round format version. Every round in a chain carries it, and a
// change of interpretation starts a new chain generation rather than continuing
// silently.
const Schema = "sf/1"

// FrameType discriminates the lines of a round file.
type FrameType string

const (
	FrameHeader     FrameType = "header"
	FrameNode       FrameType = "node"
	FrameRelation   FrameType = "relation"
	FrameUnresolved FrameType = "unresolved"
	FrameCommit     FrameType = "commit"
)

// Header is the first line of a round.
//
// It deliberately carries no wall-clock time: a round's bytes must be
// reproducible from its inputs, so that re-deriving it from the same landed
// range, the same previous digest and the same parser version yields the same
// digest. When it was produced belongs outside the digest, in chain state.
type Header struct {
	T      FrameType `json:"t"`
	Schema string    `json:"schema"`

	Conversation string `json:"conversation"`
	Session      string `json:"session"`

	// Round counts from 1. Previous is the digest of round N-1, and is empty
	// only for round 1 - it is a semantic dependency, not audit metadata.
	Round    uint64 `json:"round"`
	Previous string `json:"previous,omitempty"`

	// The landed sequence range this round consumed. FromSeq is inclusive.
	FromSeq    uint64 `json:"from_seq"`
	ThroughSeq uint64 `json:"through_seq"`

	// InputDigest binds the round to the landed evidence it read. It is chained
	// rather than recomputed: H(previous_input_digest || digests of newly landed
	// files), so producing it stays proportional to new data, not to history.
	InputDigest string `json:"input_digest"`

	// Parser and Policy are the interpretation versions. A change to either that
	// alters meaning starts a new chain generation.
	Parser string `json:"parser"`
	Policy string `json:"policy"`

	// FromTime and ThroughTime are the earliest and the latest record time
	// among the landed files this round consumed, as the runtime wrote them,
	// in UTC. They are evidence, not the parser's clock, so the same input
	// reproduces them and they sit inside the digest. Absent when no record
	// in the window carries a time. The session node carries the same pair
	// for the whole conversation so far.
	FromTime    string `json:"from_time,omitempty"`
	ThroughTime string `json:"through_time,omitempty"`
}

// Ref points at one landed record, and optionally one content block within it.
//
// Session scope comes from the header, so a reference carries neither a session
// id nor a digest. Seq selects the landed file and Row the line within it.
type Ref struct {
	Seq   uint64 `json:"seq"`
	Row   uint64 `json:"row"`
	Block *int   `json:"block,omitempty"`
}

// Entity is what every non-header, non-commit frame has in common.
//
// ID must be derived from stable evidence - session, stream, kind and the
// runtime's own identity for the thing - never from a position. A positional id
// shifts when late evidence arrives, and a shifted id cannot supersede its own
// earlier revision.
//
// Revision is the round number that produced this version. Deriving it from
// chain position rather than counting per entity is what keeps a round
// reproducible without replaying the chain.
type Entity struct {
	T        FrameType `json:"t"`
	ID       string    `json:"id"`
	Revision uint64    `json:"revision"`

	// Tombstone removes an entity from the materialised result while leaving it
	// in history. Absence from a later round means unchanged, never deleted, so
	// removal has to be explicit.
	Tombstone bool `json:"tombstone,omitempty"`
}

// Node is one element of the conversation structure.
type Node struct {
	Entity
	Kind   string          `json:"kind"`
	Parent string          `json:"parent,omitempty"`
	Stream string          `json:"stream,omitempty"`
	Ref    *Ref            `json:"ref,omitempty"`
	Refs   []Ref           `json:"refs,omitempty"`
	Attrs  json.RawMessage `json:"attrs,omitempty"`
}

// Relation is a typed edge that is not containment.
type Relation struct {
	Entity
	Type     string `json:"type"`
	From     string `json:"from"`
	To       string `json:"to"`
	Quality  string `json:"quality"`
	Via      string `json:"via,omitempty"`
	Evidence []Ref  `json:"evidence,omitempty"`
}

// Unresolved is a reference that could not be resolved, carried as data.
//
// A resolved reference is superseded by a later revision whose State says so,
// rather than vanishing: absence never means resolved.
type Unresolved struct {
	Entity
	Kind   string `json:"kind"`
	RefID  string `json:"ref,omitempty"`
	Reason string `json:"reason,omitempty"`
	State  string `json:"state"` // open | resolved | terminal
}

// Unresolved states.
const (
	UnresolvedOpen = "open"
	// UnresolvedResolved supersedes an earlier open entry.
	UnresolvedResolved = "resolved"
	// UnresolvedTerminal means terminal evidence says it will never resolve -
	// a pruned source, a record the runtime never wrote. It is never inferred
	// from elapsed rounds.
	UnresolvedTerminal = "terminal"
)

// Counts is the tally a commit frame publishes, so a reader can detect a
// truncated round without folding it.
type Counts struct {
	Nodes      int `json:"nodes"`
	Relations  int `json:"relations"`
	Unresolved int `json:"unresolved"`
}

// Commit is the last line of a round.
//
// Digest covers every preceding line, so a round verifies itself: recompute
// over lines 1..n-1 and compare. It is also the value the NEXT round names as
// its Previous.
type Commit struct {
	T      FrameType `json:"t"`
	Digest string    `json:"digest"`
	Counts Counts    `json:"counts"`
}

// Validate reports a header that cannot be acted on.
func (h *Header) Validate() error {
	switch {
	case h.Schema != Schema:
		return fmt.Errorf("sessionflow: unsupported schema %q, want %q", h.Schema, Schema)
	case h.Conversation == "":
		return fmt.Errorf("sessionflow: header missing conversation")
	case h.Session == "":
		return fmt.Errorf("sessionflow: header missing session")
	case h.Round == 0:
		return fmt.Errorf("sessionflow: round must count from 1")
	case h.Round > 1 && h.Previous == "":
		return fmt.Errorf("sessionflow: round %d has no previous digest; the chain would be unverifiable", h.Round)
	case h.Round == 1 && h.Previous != "":
		return fmt.Errorf("sessionflow: round 1 must not name a previous digest")
	case h.Parser == "":
		return fmt.Errorf("sessionflow: header missing parser version")
	case h.Policy == "":
		return fmt.Errorf("sessionflow: header missing policy version")
	case h.InputDigest == "":
		// Without it a round says nothing about the evidence it read, and the
		// chain proves only that the rounds are the rounds.
		return fmt.Errorf("sessionflow: header missing input digest")
	case h.FromSeq == 0:
		return fmt.Errorf("sessionflow: landed sequences count from 1, so from_seq must not be 0")
	case h.ThroughSeq < h.FromSeq-1:
		// through == from-1 is the empty range: a round that consumed no new
		// evidence. Anything below that is not a range.
		return fmt.Errorf("sessionflow: round %d consumes sequences %d..%d, which is not a range",
			h.Round, h.FromSeq, h.ThroughSeq)
	}
	return nil
}
