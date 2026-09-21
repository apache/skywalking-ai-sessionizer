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
	"encoding/json"
	"os"
	"testing"
)

// identityCases is the table both implementations are held to.
const identityCases = "../../../plugins/langchain/testdata/identity.json"

// TestTheShimAndTheReceiverNameASessionTheSame is the one place two languages
// have to agree exactly.
//
// The receiver lands a conversation and the LangChain shim lands what a tool
// call changed, and they meet only in the name of the session directory. If
// the two derivations ever drift, a change record goes to a session nobody is
// assembling and the loss is silent: no error, no gap, just a conversation
// that never mentions the files it wrote.
func TestTheShimAndTheReceiverNameASessionTheSame(t *testing.T) {
	raw, err := os.ReadFile(identityCases)
	if err != nil {
		t.Skipf("no identity table: %v", err)
	}
	var cases []struct {
		Values []string `json:"values"`
		ID     string   `json:"id"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("the identity table is empty")
	}
	for _, tc := range cases {
		got := StorageID(Owner{Values: tc.Values})
		if got != tc.ID {
			t.Errorf("%q\n  Go     %s\n  Python %s", tc.Values, got, tc.ID)
		}
	}
	t.Logf("%d owners named identically in both languages", len(cases))
}

// ownershipCases is the second table: not just how an owner is named, but how
// one is derived from a run in the first place.
const ownershipCases = "../../../plugins/langchain/testdata/ownership.json"

// TestTheShimAndTheReceiverDeriveTheSameOwner.
//
// Naming an owner identically is not enough if the two sides disagree about
// which owner a run has. The receiver reads its thread keys and its scope
// from the configuration; the shim used to assume the defaults, so a root
// configured with any other key landed its change records under a session
// name no conversation had. Nothing failed - the records were simply never
// joined, and the conversation never mentioned the files it wrote.
func TestTheShimAndTheReceiverDeriveTheSameOwner(t *testing.T) {
	raw, err := os.ReadFile(ownershipCases)
	if err != nil {
		t.Skipf("no ownership table: %v", err)
	}
	var cases []struct {
		Project  string         `json:"project"`
		Metadata map[string]any `json:"metadata"`
		Keys     []string       `json:"keys"`
		Scope    []string       `json:"scope"`
		Values   []string       `json:"values"`
		ID       string         `json:"id"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("the ownership table is empty")
	}
	supplied := 0
	for _, tc := range cases {
		ownership := DefaultOwnership()
		if len(tc.Keys) > 0 {
			ownership.Keys = tc.Keys
		}
		if len(tc.Scope) > 0 {
			ownership.Scope = tc.Scope
		}
		owner, ok := ownership.Owner(tc.Project, tc.Metadata)
		if !ok {
			if tc.Values != nil {
				t.Errorf("%v: Go found no owner, Python found %v", tc.Metadata, tc.Values)
			}
			continue
		}
		supplied++
		if tc.Values == nil {
			t.Errorf("%v: Go found owner %v, Python found none", tc.Metadata, owner.Values)
			continue
		}
		if !equal(owner.Values, tc.Values) {
			t.Errorf("%v\n  Go     %v\n  Python %v", tc.Metadata, owner.Values, tc.Values)
			continue
		}
		if got := StorageID(owner); got != tc.ID {
			t.Errorf("%v\n  Go     %s\n  Python %s", tc.Values, got, tc.ID)
		}
	}
	t.Logf("%d runs of %d supplied an identity, derived identically in both languages",
		supplied, len(cases))
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
