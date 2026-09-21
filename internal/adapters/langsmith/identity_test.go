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
	"path/filepath"
	"strings"
	"testing"
)

// TestAThreadKeyIsNeverAPath runs the keys the corpus actually carries through
// the derivation. The storage root joins a session id into a path, so any one
// of these reaching it unchanged would escape the root, hide the session from
// enumeration, or fail to open on Windows.
func TestAThreadKeyIsNeverAPath(t *testing.T) {
	own := DefaultOwnership()
	for _, key := range []string{
		"../outside", "team/customer", "_hidden", "会话-1",
		"..", ".", "con", "nul", "com1", "a\\b", "with space", strings.Repeat("x", 500),
	} {
		owner, ok := own.Owner("asz-harness", map[string]any{"thread_id": key})
		if !ok {
			t.Fatalf("%q: no owner", key)
		}
		id := StorageID(owner)
		switch {
		case id != filepath.Base(id):
			t.Errorf("%q became a path: %q", key, id)
		case strings.HasPrefix(id, "_"):
			t.Errorf("%q would be skipped by enumeration: %q", key, id)
		case strings.ContainsAny(id, `/\:*?"<>| `):
			t.Errorf("%q kept a character a path cannot: %q", key, id)
		case id == "." || id == "..":
			t.Errorf("%q became %q", key, id)
		case len(id) > 64:
			t.Errorf("%q became %d characters", key, len(id))
		}
	}
}

// TestOwnersDoNotCollide is the reason a digest is carried at all. Two keys
// that slug to the same readable text are different conversations and must
// land apart.
func TestOwnersDoNotCollide(t *testing.T) {
	own := DefaultOwnership()
	seen := map[string]string{}
	for _, key := range []string{
		"team/customer", "team-customer", "team_customer", "TEAM/CUSTOMER",
		"会话-1", "会话-2", "../outside", "..-outside",
	} {
		owner, _ := own.Owner("p", map[string]any{"thread_id": key})
		id := StorageID(owner)
		if was, ok := seen[id]; ok {
			t.Errorf("%q and %q both land in %q", was, key, id)
		}
		seen[id] = key
	}
}

// TestProjectIsPartOfOwnership is the two-applications case: one thread key,
// two projects, two conversations.
func TestProjectIsPartOfOwnership(t *testing.T) {
	own := DefaultOwnership()
	one, _ := own.Owner("application-one", map[string]any{"thread_id": "123"})
	two, _ := own.Owner("application-two", map[string]any{"thread_id": "123"})
	if StorageID(one) == StorageID(two) {
		t.Fatal("two applications sharing a thread key landed in one conversation")
	}
	// And an operator who says project does not own a conversation gets the
	// other answer, deliberately.
	merged := Ownership{Keys: own.Keys, Scope: []string{"thread"}}
	a, _ := merged.Owner("application-one", map[string]any{"thread_id": "123"})
	b, _ := merged.Owner("application-two", map[string]any{"thread_id": "123"})
	if StorageID(a) != StorageID(b) {
		t.Fatal("with project out of scope the two should merge")
	}
}

// TestNoKeyIsNotAnIdentity: absence has to be reported, not filled in.
func TestNoKeyIsNotAnIdentity(t *testing.T) {
	own := DefaultOwnership()
	for _, metadata := range []map[string]any{
		nil, {}, {"thread_id": ""}, {"thread_id": 7}, {"other": "x"},
	} {
		if _, ok := own.Owner("p", metadata); ok {
			t.Errorf("%v was treated as an identity", metadata)
		}
	}
	if got := UnassignedID("trace-1"); got == UnassignedID("trace-2") {
		t.Error("two unassigned traces landed together")
	}
}

// TestKeysAreTriedInOrder: an application that sets two keys gets the first
// the ownership names, not whichever the map iterates to.
func TestKeysAreTriedInOrder(t *testing.T) {
	own := DefaultOwnership()
	owner, ok := own.Owner("p", map[string]any{
		"session_id": "second", "thread_id": "first", "conversation_id": "third"})
	if !ok || owner.Thread != "first" {
		t.Fatalf("got %q, want first", owner.Thread)
	}
}
