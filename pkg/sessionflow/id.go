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

// RelationID builds a relation id.
//
// A relation is keyed by its endpoints and type, not by the evidence that
// revealed it. Two records asserting the same edge are the same relation
// observed twice, and must fold to one - keying by evidence would keep both.
func RelationID(typ, from, to string) string {
	return join([]string{"rel", typ, from, to})
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
	s := join([]string{kind, strconv.FormatUint(r.Seq, 10), strconv.FormatUint(r.Row, 10)})
	if r.Block != nil {
		s += ":" + strconv.Itoa(*r.Block)
	}
	return s
}

func join(parts []string) string {
	clean := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		clean = append(clean, strings.ReplaceAll(p, "/", "_"))
	}
	return strings.Join(clean, "/")
}

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
