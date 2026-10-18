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

package assemble

import (
	"fmt"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/index"
)

// m is an entry with an id when id is not 0, else one by its digest alone.
func m(id, digest uint64) index.Message {
	return index.Message{HasID: id != 0, ID: id, Digest: digest}
}

// TestTheCountsExplainAChangedHistory holds the counts and the order test on
// the cases the design names, repeats among them.
func TestTheCountsExplainAChangedHistory(t *testing.T) {
	a, b, c, d := m(1, 10), m(2, 20), m(3, 30), m(4, 40)
	cont := m(0, 99) // "continue", which a person types more than once
	x := m(0, 50)
	for name, tc := range map[string]struct {
		P, N                          []index.Message
		kept, changed, dropped, added int
		order                         string
	}{
		"a trimmed front":                                {[]index.Message{a, b, c}, []index.Message{c, d}, 1, 0, 2, 1, "no"},
		"the last message replaced":                      {[]index.Message{a, b, c}, []index.Message{a, b, d}, 2, 0, 1, 1, "no"},
		"a shortening":                                   {[]index.Message{a, b, c}, []index.Message{a, b}, 2, 0, 1, 0, "no"},
		"a reordering":                                   {[]index.Message{a, b}, []index.Message{b, a}, 2, 0, 0, 0, "yes"},
		"an edit under one id":                           {[]index.Message{a, b}, []index.Message{a, m(2, 21)}, 1, 1, 0, 0, "no"},
		"a moved and edited message":                     {[]index.Message{a, b}, []index.Message{b, m(1, 11)}, 1, 1, 0, 0, "yes"},
		"repeated identical messages, the first dropped": {[]index.Message{cont, x, cont}, []index.Message{x, cont}, 2, 0, 1, 0, "no"},
		"a repeated id reordered":                        {[]index.Message{m(1, 10), m(1, 11)}, []index.Message{m(1, 11), m(1, 10)}, 2, 0, 0, 0, "unavailable"},
		"no id: an edit is a drop and an add":            {[]index.Message{m(0, 1), m(0, 2)}, []index.Message{m(0, 1), m(0, 3)}, 1, 0, 1, 1, "no"},
	} {
		if isPrefix(tc.P, tc.N) {
			t.Errorf("%s: read as kept", name)
		}
		got := counts(tc.P, tc.N)
		want := map[string]any{"kept": tc.kept, "changed": tc.changed, "dropped": tc.dropped, "added": tc.added, "order_changed": tc.order}
		for k, v := range want {
			if fmt.Sprint(got[k]) != fmt.Sprint(v) {
				t.Errorf("%s: %s is %v, want %v (%v)", name, k, got[k], v, got)
			}
		}
		if tc.order == "unavailable" && got["order_changed_reason"] != "repeated_id" {
			t.Errorf("%s: order unavailable for %v, want repeated_id", name, got["order_changed_reason"])
		}
	}
}

// TestTheOrderTestHasABound. Past the pairs it may visit, the order is not
// decided rather than decided slowly.
func TestTheOrderTestHasABound(t *testing.T) {
	same := m(0, 7)
	P := make([]index.Message, 2049)
	N := make([]index.Message, 2049)
	for i := range P {
		P[i], N[i] = same, same
	}
	if _, why := orderChanged(P, N, len(P)); why != "too_large" {
		t.Fatalf("%d pairs: %q, want too_large", len(P)*len(N), why)
	}
	if order, why := orderChanged(P[:2048], N[:2048], 2048); why != "" || order {
		t.Fatalf("%d pairs: %v %q, want decided and unchanged", 2048*2048, order, why)
	}
}

// TestTheEarlierReplyIsFoundWhereItCanBe.
func TestTheEarlierReplyIsFoundWhereItCanBe(t *testing.T) {
	r := m(9, 90)
	altered := m(9, 91)
	other := m(0, 5)
	for want, in := range map[string][]index.Message{
		replyBack:        {other, r},
		replyBackChanged: {altered, other},
		replyNotFound:    {other},
	} {
		if got := find(r, in); got != want {
			t.Errorf("%v: %s, want %s", in, got, want)
		}
	}
	// On a changed history a reply equal to an earlier message, or sharing
	// its id, could be either, and none is claimed.
	h := &historian{}
	for name, P := range map[string][]index.Message{"equal": {r}, "same id": {m(9, 1)}} {
		out := &callHistory{attrs: map[string]any{}}
		h.replyIn(out, r, "", P, []index.Message{r})
		if out.attrs["history_reply"] != "unavailable" || out.attrs["history_reply_reason"] != "ambiguous" {
			t.Errorf("%s: %v", name, out.attrs)
		}
	}
}
