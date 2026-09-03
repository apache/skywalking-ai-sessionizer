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

package sessionflow

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Writer builds one round in memory, computes its digest, and emits it.
//
// The digest covers every line before the commit frame, so it cannot be known
// until the round is complete - which is why the round is buffered rather than
// streamed. A round is a bounded delta, not a snapshot, so this stays small.
type Writer struct {
	buf    bytes.Buffer
	header Header
	counts Counts
	closed bool
}

// NewWriter starts a round.
func NewWriter(h Header) (*Writer, error) {
	h.T = FrameHeader
	h.Schema = Schema
	if err := h.Validate(); err != nil {
		return nil, err
	}
	w := &Writer{header: h}
	if err := w.emit(h); err != nil {
		return nil, err
	}
	return w, nil
}

// Node appends a node revision.
func (w *Writer) Node(n Node) error {
	n.T, n.Revision = FrameNode, w.header.Round
	if n.ID == "" {
		return fmt.Errorf("sessionflow: node has no id; it could never be superseded")
	}
	w.counts.Nodes++
	return w.emit(n)
}

// Relation appends a relation revision.
func (w *Writer) Relation(r Relation) error {
	r.T, r.Revision = FrameRelation, w.header.Round
	if r.ID == "" {
		return fmt.Errorf("sessionflow: relation has no id; it could never be superseded")
	}
	w.counts.Relations++
	return w.emit(r)
}

// Unresolved appends an unresolved-reference revision.
func (w *Writer) Unresolved(u Unresolved) error {
	u.T, u.Revision = FrameUnresolved, w.header.Round
	if u.ID == "" {
		return fmt.Errorf("sessionflow: unresolved entry has no id; it could never be resolved")
	}
	if u.State == "" {
		u.State = UnresolvedOpen
	}
	w.counts.Unresolved++
	return w.emit(u)
}

// Close writes the commit frame and returns the round's bytes and digest.
//
// A round with no entities is valid: it advances the processed-input watermark,
// which is how a pass that found nothing new still records that it looked.
func (w *Writer) Close() (data []byte, digest string, err error) {
	if w.closed {
		return nil, "", fmt.Errorf("sessionflow: round already closed")
	}
	sum := sha256.Sum256(w.buf.Bytes())
	digest = hex.EncodeToString(sum[:])
	if err := w.emit(Commit{T: FrameCommit, Digest: digest, Counts: w.counts}); err != nil {
		return nil, "", err
	}
	w.closed = true
	return w.buf.Bytes(), digest, nil
}

// Counts reports what has been written so far.
func (w *Writer) Counts() Counts { return w.counts }

func (w *Writer) emit(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	w.buf.Write(b)
	w.buf.WriteByte('\n')
	return nil
}

// Round is a parsed round file.
type Round struct {
	Header     Header
	Nodes      []Node
	Relations  []Relation
	Unresolved []Unresolved
	Commit     Commit
}

// Read parses a round and verifies it.
//
// A round is a shipped artifact, so reading is where a malformed one has to be
// caught - after this point it is folded into a conversation and nothing else
// looks at it. The checks are deliberately strict: anything a well-formed
// producer never emits is rejected rather than tolerated, because tolerating it
// means some consumer somewhere silently disagrees about what the round said.
func Read(r io.Reader) (*Round, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	var (
		out       Round
		hashed    bytes.Buffer
		sawHead   bool
		sawCommit bool
		line      int
	)
	ids := map[string]string{}
	claim := func(kind, id string) error {
		if id == "" {
			return fmt.Errorf("sessionflow: line %d: %s frame has no id", line, kind)
		}
		if prev, dup := ids[id]; dup {
			return fmt.Errorf("sessionflow: line %d: id %q appears twice in one round (as %s and %s)",
				line, id, prev, kind)
		}
		ids[id] = kind
		return nil
	}

	for {
		raw, err := readLine(br)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(raw) == 0 {
			continue
		}
		line++
		var probe struct {
			T FrameType `json:"t"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			return nil, fmt.Errorf("sessionflow: line %d: undecodable frame: %w", line, err)
		}
		if sawCommit {
			return nil, fmt.Errorf("sessionflow: line %d: content after the commit frame", line)
		}
		if line == 1 && probe.T != FrameHeader {
			return nil, fmt.Errorf("sessionflow: first frame is %q, not a header", probe.T)
		}
		if line > 1 && probe.T == FrameHeader {
			return nil, fmt.Errorf("sessionflow: line %d: a second header", line)
		}

		switch probe.T {
		case FrameHeader:
			if err := json.Unmarshal(raw, &out.Header); err != nil {
				return nil, err
			}
			if err := out.Header.Validate(); err != nil {
				return nil, err
			}
			sawHead = true
		case FrameNode:
			var n Node
			if err := json.Unmarshal(raw, &n); err != nil {
				return nil, err
			}
			if err := claim("node", n.ID); err != nil {
				return nil, err
			}
			if err := checkRevision(line, n.Revision, out.Header.Round); err != nil {
				return nil, err
			}
			if err := checkRefs(line, n.Ref, n.Refs, &out.Header); err != nil {
				return nil, err
			}
			out.Nodes = append(out.Nodes, n)
		case FrameRelation:
			var v Relation
			if err := json.Unmarshal(raw, &v); err != nil {
				return nil, err
			}
			if err := claim("relation", v.ID); err != nil {
				return nil, err
			}
			if err := checkRevision(line, v.Revision, out.Header.Round); err != nil {
				return nil, err
			}
			if !v.Tombstone && (v.From == "" || v.To == "" || v.Type == "") {
				return nil, fmt.Errorf("sessionflow: line %d: relation %q is missing an endpoint or a type", line, v.ID)
			}
			if err := checkRefs(line, nil, v.Evidence, &out.Header); err != nil {
				return nil, err
			}
			out.Relations = append(out.Relations, v)
		case FrameUnresolved:
			var u Unresolved
			if err := json.Unmarshal(raw, &u); err != nil {
				return nil, err
			}
			if err := claim("unresolved", u.ID); err != nil {
				return nil, err
			}
			if err := checkRevision(line, u.Revision, out.Header.Round); err != nil {
				return nil, err
			}
			switch u.State {
			case UnresolvedOpen, UnresolvedResolved, UnresolvedTerminal:
			default:
				if !u.Tombstone {
					return nil, fmt.Errorf("sessionflow: line %d: unresolved entry %q has state %q", line, u.ID, u.State)
				}
			}
			out.Unresolved = append(out.Unresolved, u)
		case FrameCommit:
			if err := json.Unmarshal(raw, &out.Commit); err != nil {
				return nil, err
			}
			sawCommit = true
			continue // the commit frame is not covered by its own digest
		default:
			return nil, fmt.Errorf("sessionflow: line %d: unknown frame type %q", line, probe.T)
		}
		hashed.Write(raw)
		hashed.WriteByte('\n')
	}
	if !sawHead {
		return nil, fmt.Errorf("sessionflow: round has no header")
	}
	if !sawCommit {
		return nil, fmt.Errorf("sessionflow: round has no commit frame; it is truncated")
	}
	sum := sha256.Sum256(hashed.Bytes())
	if got := hex.EncodeToString(sum[:]); got != out.Commit.Digest {
		return nil, fmt.Errorf("sessionflow: digest mismatch: computed %s, round claims %s",
			got[:12], firstN(out.Commit.Digest, 12))
	}
	want := Counts{Nodes: len(out.Nodes), Relations: len(out.Relations), Unresolved: len(out.Unresolved)}
	if want != out.Commit.Counts {
		return nil, fmt.Errorf("sessionflow: counts mismatch: read %+v, round claims %+v", want, out.Commit.Counts)
	}
	return &out, nil
}

// checkRevision enforces that an entity names the round that produced it.
//
// Revision is derived from chain position rather than counted per entity, which
// is what lets a round be re-derived without replaying the chain. A frame whose
// revision disagrees with its header was not produced by that round.
func checkRevision(line int, got, round uint64) error {
	if got != round {
		return fmt.Errorf("sessionflow: line %d: revision %d in round %d", line, got, round)
	}
	return nil
}

// checkRefs rejects references outside the range the header declares it read.
//
// A round says which landed sequences it consumed. A reference past that range
// describes evidence the round did not claim to have seen, and its input digest
// therefore does not cover it.
func checkRefs(line int, one *Ref, many []Ref, h *Header) error {
	all := many
	if one != nil {
		all = append(append([]Ref(nil), *one), many...)
	}
	for _, r := range all {
		if r.Seq == 0 && r.Row == 0 {
			return fmt.Errorf("sessionflow: line %d: a reference to seq 0 row 0 is not a position", line)
		}
		if r.Seq > h.ThroughSeq {
			return fmt.Errorf("sessionflow: line %d: reference to landed sequence %d, past the round's declared %d",
				line, r.Seq, h.ThroughSeq)
		}
	}
	return nil
}

func readLine(br *bufio.Reader) ([]byte, error) {
	line, err := br.ReadBytes('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && len(line) > 0 {
			return line, nil
		}
		return nil, err
	}
	return line[:len(line)-1], nil
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
