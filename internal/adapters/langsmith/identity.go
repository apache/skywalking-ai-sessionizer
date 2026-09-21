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

package langsmith

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"unicode"
)

// Ownership is how the evidence in a run says which conversation it belongs to.
//
// A bare thread key is not an identity. Two applications can both use the
// project "production" and the thread "123", and merging them would put two
// people's histories, tools and costs in one conversation while every
// individual join stayed valid. So ownership is a namespace the operator
// names, and the default names the two dimensions the client always carries.
//
// Nothing here invents an identity. A trace that supplies no key is not given
// one: see Owner's second return value, and what the collector does with it.
type Ownership struct {
	// Keys are the metadata keys that may carry the thread, in the order they
	// are tried. The client's own documentation names these three, and an
	// application that uses another says so here.
	Keys []string
	// Scope names the dimensions that together own a conversation, in order.
	// "project" is the client's session_name; anything else is a metadata
	// key. Dropping "project" merges every project that shares a thread key,
	// which is a choice an operator can make and this must not make for them.
	Scope []string
}

// DefaultOwnership is project and thread, with the three keys the client
// documents.
func DefaultOwnership() Ownership {
	return Ownership{
		Keys:  []string{"thread_id", "session_id", "conversation_id"},
		Scope: []string{"project", "thread"},
	}
}

// Owner is which conversation a run belongs to, as the run says it.
type Owner struct {
	// Values are the dimensions in Scope order, as they were supplied.
	Values []string
	// Thread is the thread key itself, kept apart because it is the one a
	// reader recognises.
	Thread string
}

// Owner reads the ownership of one run.
//
// The second return is false when no key was supplied. That is a state, not a
// failure: such evidence is real and is held unassigned rather than landed
// under something made up.
func (o Ownership) Owner(project string, metadata map[string]any) (Owner, bool) {
	thread := ""
	for _, key := range o.Keys {
		if v, ok := metadata[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				thread = s
				break
			}
		}
	}
	if thread == "" {
		return Owner{}, false
	}
	out := Owner{Thread: thread}
	for _, dimension := range o.Scope {
		switch dimension {
		case "project":
			out.Values = append(out.Values, project)
		case "thread":
			out.Values = append(out.Values, thread)
		default:
			s, _ := metadata[dimension].(string)
			out.Values = append(out.Values, s)
		}
	}
	return out, true
}

// StorageID is the session directory an owner's evidence lands under.
//
// A supplied key is never a path. The storage root joins a session id straight
// into a filesystem path, and the corpus shows what applications really send:
// "../outside", "team/customer", "_hidden" and "会话-1" all arrived as thread
// keys. So the id is derived rather than used: a readable slug for whoever
// reads a directory listing, and a digest of the whole namespace so two owners
// can never collide even when their slugs do.
//
// The "ls-" prefix does three jobs at once. It says which adapter landed the
// session, it keeps the name off the leading underscore that session
// enumeration skips, and it keeps every Windows device name (con, nul, com1)
// from ever being a directory.
func StorageID(owner Owner) string {
	sum := sha256.Sum256(tuple(owner.Values))
	slug := slugOf(owner.Values)
	if slug == "" {
		return "ls-" + hex.EncodeToString(sum[:6])
	}
	return "ls-" + slug + "-" + hex.EncodeToString(sum[:6])
}

// UnassignedID is where a trace with no supplied identity lands.
//
// It is named for the trace because there is nothing else to name it for, and
// the name says plainly that ownership was not supplied rather than implying
// the trace is one.
func UnassignedID(traceID string) string {
	sum := sha256.Sum256([]byte(traceID))
	return "ls-unassigned-" + hex.EncodeToString(sum[:6])
}

// tuple encodes the dimensions so their boundaries cannot be forged.
//
// Joining with a separator is ambiguous, and not in a way a hash protects
// against: with a plain join, the owners ["a\x1fb", "c"] and ["a", "b\x1fc"]
// produce the same bytes and so the same session. A supplied value can contain
// any byte, so the length of each dimension goes in front of it and nothing a
// value holds can look like the end of one.
func tuple(values []string) []byte {
	var b []byte
	for _, v := range values {
		b = strconv.AppendInt(b, int64(len(v)), 10)
		b = append(b, ':')
		b = append(b, v...)
	}
	return b
}

// maxSlug keeps a session directory readable and well inside the shortest
// path limit worth worrying about. The digest carries the identity, so
// truncation here costs nothing but legibility.
const maxSlug = 48

// slugOf renders the namespace for a human reading a directory listing.
//
// Anything that is not a letter, a digit or a dash becomes a dash, letters
// fold to lower case, and runs of dashes collapse. A value in a script with no
// ASCII at all slugs to nothing, which is why the digest and not this is what
// makes the name unique.
func slugOf(values []string) string {
	var b strings.Builder
	for i, v := range values {
		if i > 0 {
			b.WriteByte('-')
		}
		for _, r := range v {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
				b.WriteRune(r)
			case unicode.IsUpper(r) && r < unicode.MaxASCII:
				b.WriteRune(unicode.ToLower(r))
			default:
				b.WriteByte('-')
			}
		}
	}
	out := b.String()
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	out = strings.Trim(out, "-")
	if len(out) > maxSlug {
		out = strings.Trim(out[:maxSlug], "-")
	}
	return out
}
