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
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
)

// Entity ids.
//
// An id must be derivable from evidence that does not move. Position is the
// tempting choice and the wrong one: a node keyed by "the 14th record in the
// stream" gets a different key the moment an earlier source is backfilled, and
// a changed key cannot supersede its own earlier revision - the fold would hold
// both, one stale and one current, with no way to tell them apart.
//
// So ids are built from the runtime's own identity for a thing, scoped by
// session and stream. Where the runtime supplies no identity, the id is derived
// from the landed location of the evidence, which is itself immutable once
// written.

// NodeID builds a node id from stable parts.
//
// Parts are joined rather than hashed so an id stays legible in a round file
// and in an error message. Legibility matters more than length here: these are
// read by people debugging why two revisions did not merge.
func NodeID(kind string, parts ...string) string {
	return join(append([]string{kind}, parts...))
}

// NodeIDIn builds the id of a node named inside another node, as a run is
// inside its talk: the other node's id, nested, and then the parts.
func NodeIDIn(kind, outer string, parts ...string) string {
	written := []string{escape(kind), nest(outer)}
	for _, p := range parts {
		written = append(written, escape(p))
	}
	return joinWritten(written)
}

// LocatedID builds a node id from stable parts and then the landed location
// of its evidence, for use where the runtime gives no identity.
func LocatedID(kind string, r Ref, parts ...string) string {
	all := append(append([]string{kind}, parts...), strconv.FormatUint(r.Seq, 10), strconv.FormatUint(r.Row, 10))
	s := join(all)
	if r.Block != nil {
		s += ":" + strconv.Itoa(*r.Block)
	}
	return s
}

// RelationID builds a relation id.
//
// A relation is keyed by its endpoints and type, not by the evidence that
// revealed it. Two records asserting the same edge are the same relation
// observed twice, and must fold to one - keying by evidence would keep both.
func RelationID(typ, from, to string) string {
	return joinWritten([]string{escape("rel"), escape(typ), nest(from), nest(to)})
}

// UnresolvedID builds an unresolved-entry id.
//
// Keyed by what is missing, so the later revision that resolves it lands on the
// same key. Keyed by the round that noticed, every round would open a new one.
func UnresolvedID(kind, ref string) string {
	return join([]string{"unres", kind, ref})
}

// RefID names a landed location, for use where the runtime gives no identity.
func RefID(kind string, r Ref) string {
	return LocatedID(kind, r)
}

// join escapes each part, so two different lists of parts never give one id.
// Writing a "/" in a part as "_" made the tool ids "a/b" and "a_b" one node,
// and one call took the other's place. Only "%" and "/" are escaped, so a part
// that holds neither is written as it is.
func join(parts []string) string {
	written := make([]string, len(parts))
	for i, p := range parts {
		written[i] = escape(p)
	}
	return joinWritten(written)
}

// joinWritten joins parts already written as an id writes them, and leaves
// out an empty one.
func joinWritten(parts []string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "/")
}

func escape(part string) string { return partEscaper.Replace(part) }

// nest writes an id as one part of another. Its "/" becomes "_", so the outer
// id stays as readable as the ids it names: the run in talk "talk/main/8d53"
// is "run/talk_main_8d53/8d53". A "_" the id already holds becomes "%5F"
// first, or the talk of the main stream's cycle "a", talk/main/a, and the one
// talk of a child stream named "main_a", talk/main_a, nest alike, and their
// runs take each other's place. A "%" becomes "%25" as well, or an id nested
// twice reads alike: run/talk_main_a/a and run/talk_main%5Fa/a. The nested id
// is not escaped again, since it holds no "/" and each "%" in it begins an
// escape.
func nest(id string) string { return nestEscaper.Replace(id) }

var (
	partEscaper = strings.NewReplacer("%", "%25", "/", "%2F")
	nestEscaper = strings.NewReplacer("%", "%25", "_", "%5F", "/", "_")
)

// ChainInputDigest extends an input digest with newly consumed evidence.
//
// It is a chain, not a recomputation over everything read so far:
//
//	digestₙ = H(digestₙ₋₁ || sorted digests of the evidence round n consumed)
//
// Recomputing over the full history would make each round cost proportional to
// the whole conversation, so a long-running one would slow down without bound -
// exactly the property an incremental design exists to avoid. Chaining keeps
// each round proportional to what is new, and still binds every round
// transitively to all evidence before it.
//
// Inputs are sorted so that discovery order - which depends on directory
// iteration and is not stable - cannot change the digest.
func ChainInputDigest(previous string, added []string) string {
	sorted := append([]string(nil), added...)
	sort.Strings(sorted)
	h := sha256.New()
	h.Write([]byte(previous))
	for _, a := range sorted {
		h.Write([]byte{0})
		h.Write([]byte(a))
	}
	return hex.EncodeToString(h.Sum(nil))
}
