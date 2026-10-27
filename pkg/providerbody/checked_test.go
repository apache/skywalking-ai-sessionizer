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

package providerbody

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// removedOf is the record a removal leaves in a body's place, as
// pkg/skeleton writes it: every part keeps its kind, size and digest and
// none of its bytes, and the manifest stays. pkg/skeleton imports this
// package, so its own function cannot be used here.
func removedOf(rec *sessiondata.Record) *sessiondata.Record {
	out := *rec
	out.Parts = make([]sessiondata.Part, len(rec.Parts))
	last := len(rec.Parts) - 1
	for i, p := range rec.Parts {
		q := sessiondata.Part{Kind: p.Kind, Bytes: p.Bytes, State: model.ContentRemoved, SHA256: p.SHA256}
		switch {
		case p.State == model.ContentTooLarge:
			q.State = p.State
		case i == last:
			q.Data = p.Data
		case p.Kind == sessiondata.PartData:
			q.SHA256 = digest(p.Data)
		}
		out.Parts[i] = q
	}
	return &out
}

// keptStore is the store a removal fills: the removed body by its id, and
// every piece it held by digest.
type keptStore struct {
	tips   map[string][]byte
	pieces map[string][]byte
}

func (k *keptStore) Tip(id, sha string) ([]byte, error) {
	if b, ok := k.tips[id]; ok && digest(b) == sha {
		return b, nil
	}
	return nil, fs.ErrNotExist
}

func (k *keptStore) Piece(sha string) ([]byte, error) {
	if b, ok := k.pieces[sha]; ok {
		return b, nil
	}
	return nil, fs.ErrNotExist
}

func keep(rec *sessiondata.Record, body []byte) *keptStore {
	k := &keptStore{tips: map[string][]byte{rec.ID: body}, pieces: map[string][]byte{}}
	for _, p := range rec.Parts[:len(rec.Parts)-1] {
		if p.Kind == sessiondata.PartData && p.State != model.ContentTooLarge && p.State != model.ContentRemoved {
			k.pieces[digest(p.Data)] = p.Data
		}
	}
	return k
}

// encodedAfter cuts bodies[at] against bodies[before...], as a collector
// that landed them first would.
func encodedAfter(t *testing.T, bodies []body, at int, before ...int) []*sessiondata.Record {
	t.Helper()
	s := NewSession()
	var out []*sessiondata.Record
	for _, i := range append(before, at) {
		rec, err := encode(s, bodies[i].file, bodies[i].b)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, rec)
	}
	return out
}

// A repeat names a body already held, and claims its digest. The strict
// reader rebuilds the repeat from its own representation: here it is built
// on a body retention removed and the store does not keep, so it fails,
// though the body held under its id rebuilds.
func TestAddCheckedRebuildsARepeatFromItsOwnRepresentation(t *testing.T) {
	bodies := interleaved(false)
	cut := encodedAfter(t, bodies, 2, 0) // the second call, cut against the first
	first, second := cut[0], cut[1]
	whole := encodedAfter(t, bodies, 2)[0] // the second call, cut against nothing
	if second.ID != whole.ID || len(second.Parts) == len(whole.Parts) {
		t.Fatal("the fixture needs the same body in two representations")
	}
	gone := removedOf(first)

	strict := NewSession()
	for _, rec := range []*sessiondata.Record{gone, whole} {
		if repeat, _, err := strict.AddChecked(rec, rec == gone); repeat || err != nil {
			t.Fatalf("%s: repeat %v, %v", rec.ID, repeat, err)
		}
	}
	if _, err := strict.Body(whole.ID); err != nil {
		t.Fatalf("the body held under the id rebuilds: %v", err)
	}
	repeat, _, err := strict.AddChecked(second, false)
	if !repeat || !errors.Is(err, ErrRemoved) {
		t.Fatalf("the repeat built on a removed body: repeat %v, %v; want a repeat that fails with ErrRemoved", repeat, err)
	}

	// The ordinary reader keeps its behavior: a repeat it cannot rebuild for
	// a removal is still a repeat.
	ordinary := NewSession()
	for _, rec := range []*sessiondata.Record{gone, whole} {
		if err := ordinary.Add(rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := ordinary.Add(second); !errors.Is(err, ErrRepeat) {
		t.Fatalf("ordinary Add of the repeat: %v, want ErrRepeat", err)
	}

	// With the removed body kept, the repeat rebuilds.
	kept := NewSession()
	kept.Use(keep(first, bodies[0].b))
	for _, rec := range []*sessiondata.Record{gone, whole} {
		if _, _, err := kept.AddChecked(rec, rec == gone); err != nil {
			t.Fatal(err)
		}
	}
	if repeat, _, err := kept.AddChecked(second, false); !repeat || err != nil {
		t.Fatalf("with the store: repeat %v, %v", repeat, err)
	}
}

// A new record is rebuilt before it is added, so one built on a removed body
// the store does not keep fails, and is not there for a later record.
func TestAddCheckedRebuildsANewRecord(t *testing.T) {
	bodies := interleaved(false)
	cut := encodedAfter(t, bodies, 2, 0)
	first, second := cut[0], cut[1]

	strict := NewSession()
	if _, _, err := strict.AddChecked(removedOf(first), true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := strict.AddChecked(second, false); !errors.Is(err, ErrRemoved) {
		t.Fatalf("a new record built on a removed body: %v, want ErrRemoved", err)
	}
	if _, ok := strict.Holds(second.ID); ok {
		t.Fatal("a record that failed was added")
	}
	// Add checks references only, as it always did.
	ordinary := NewSession()
	if err := ordinary.Add(removedOf(first)); err != nil {
		t.Fatal(err)
	}
	if err := ordinary.Add(second); err != nil {
		t.Fatalf("ordinary Add: %v", err)
	}
	// A record whose representation does not rebuild to its digest fails too.
	lie := NewSession()
	if _, _, err := lie.AddChecked(first, false); err != nil {
		t.Fatal(err)
	}
	m, _ := ManifestOf(second)
	m.Segments = []Segment{{Lit: "[]"}}
	bad := *second
	bad.Parts = append(append([]sessiondata.Part(nil), second.Parts[:len(second.Parts)-1]...), manifestPart(m))
	if _, _, err := lie.AddChecked(&bad, false); err == nil {
		t.Fatal("a record that does not rebuild to its digest was added")
	}
}

// A body holding a piece too large to land never could be rebuilt. It is the
// one failure a strict reader allows, and the record is still added for the
// ones after it.
func TestAddCheckedAllowsAPieceTooLargeToLand(t *testing.T) {
	s := NewSession()
	s.Budget(testBudget)
	c, _ := pasted()
	rec, err := encode(s, "00000000-0000-4000-8000-000000000001.request.json", c.request())
	if err != nil {
		t.Fatal(err)
	}
	if len(omitted(rec)) != 1 {
		t.Fatal("the fixture needs a piece too large to land")
	}
	back := landed(t, rec)
	strict := NewSession()
	if repeat, rebuild, err := strict.AddChecked(back, false); repeat || err != nil || !errors.Is(rebuild, ErrTooLarge) {
		t.Fatalf("a body with a piece too large to land: repeat %v, rebuild %v, %v", repeat, rebuild, err)
	}
	if _, ok := strict.Holds(back.ID); !ok {
		t.Fatal("the body was not added for the records after it")
	}
	if repeat, rebuild, err := strict.AddChecked(back, false); !repeat || err != nil || !errors.Is(rebuild, ErrTooLarge) {
		t.Fatalf("its repeat: repeat %v, rebuild %v, %v", repeat, rebuild, err)
	}
	// A skeleton of it holds no bytes, so it is checked for structure only.
	after := NewSession()
	if _, _, err := after.AddChecked(removedOf(back), true); err != nil {
		t.Fatalf("the skeleton: %v", err)
	}
}

// A body evicted from the cache is rebuilt again when a later one copies
// it, so the later record is checked against the dependencies as they are
// now, not as they were when the first was added.
func TestAddCheckedRebuildsAnEvictedDependency(t *testing.T) {
	bodies := interleaved(false)
	// The main chain: calls 1 and 2, then call 5, which copies call 2.
	cut := encodedAfter(t, bodies, 8, 0, 2)
	first, second, fifth := cut[0], cut[1], cut[2]
	m, err := ManifestOf(fifth)
	if err != nil {
		t.Fatal(err)
	}
	copies := false
	for _, seg := range m.Segments {
		copies = copies || seg.Copy != nil && seg.Copy.From == second.ID
	}
	if !copies {
		t.Fatal("the fixture needs call 5 to copy call 2")
	}
	for _, damaged := range []bool{false, true} {
		store := keep(first, bodies[0].b)
		s := NewSession()
		s.Use(store)
		for _, rec := range []*sessiondata.Record{removedOf(first), second} {
			if _, _, err := s.AddChecked(rec, rec.Parts[len(rec.Parts)-1].State == model.ContentRemoved); err != nil {
				t.Fatalf("%s: %v", rec.ID, err)
			}
		}
		// Evict everything rebuilt so far, then take the removed body out of
		// the store when the dependency is damaged.
		s.cache, s.cacheOrder, s.cacheBytes = map[string][]byte{}, nil, 0
		if damaged {
			delete(store.tips, first.ID)
			clear(store.pieces)
		}
		_, _, err := s.AddChecked(fifth, false)
		if damaged && !errors.Is(err, ErrRemoved) {
			t.Fatalf("with its dependency gone: %v, want ErrRemoved", err)
		}
		if !damaged && err != nil {
			t.Fatalf("with its dependency kept: %v", err)
		}
	}
}

// A record is held to its body's manifest: its size and digest prefix are
// the body's, and it holds bytes exactly when it was read from a landed
// file. A landed record that says it was removed would otherwise pass
// without being rebuilt.
func TestAddCheckedHoldsARecordToItsPlace(t *testing.T) {
	bodies := interleaved(false)
	first := encodedAfter(t, bodies, 0)[0]
	for name, change := range map[string]func(*sessiondata.Record){
		"size":   func(r *sessiondata.Record) { r.Bytes++ },
		"digest": func(r *sessiondata.Record) { r.Sha = "000000000000" },
	} {
		bad := *first
		change(&bad)
		if _, _, err := NewSession().AddChecked(&bad, false); err == nil {
			t.Errorf("a record with another %s was added", name)
		}
		if err := NewSession().Add(&bad); err != nil {
			t.Errorf("ordinary Add of a record with another %s: %v", name, err)
		}
	}
	if _, _, err := NewSession().AddChecked(removedOf(first), false); err == nil {
		t.Fatal("a landed record that says it was removed was added")
	}
	if _, _, err := NewSession().AddChecked(first, true); err == nil {
		t.Fatal("a skeleton's record that keeps its bytes was added")
	}
}

// The cache of rebuilt bodies stays near its target as a strict reader adds
// records, and a body it evicted is rebuilt when a later record is built on
// it: every request of this chain copies the one before.
func TestAddCheckedEvictsAndRebuildsPastTheCacheTarget(t *testing.T) {
	collector := NewSession()
	c := newChain("s1", "p1", strings.Repeat("You are an agent with a long system prompt. ", 50_000), 0)
	var recs []*sessiondata.Record
	for i := range 40 {
		c.add("user", fmt.Sprintf("message %d", i))
		rec, err := collector.Encode(Body{ID: fmt.Sprintf("req-%02d", i), Role: RoleRequest, Src: "x", Bytes: c.request()})
		if err != nil {
			t.Fatal(err)
		}
		recs = append(recs, rec)
		c.add("assistant", fmt.Sprintf("answer %d", i))
	}
	copies := 0
	for _, rec := range recs {
		m, _ := ManifestOf(rec)
		for _, seg := range m.Segments {
			if seg.Copy != nil {
				copies++
			}
		}
	}
	if copies == 0 {
		t.Fatal("the fixture needs requests built on earlier ones")
	}
	strict := NewSession()
	largest := 0
	for _, rec := range recs {
		if _, _, err := strict.AddChecked(rec, false); err != nil {
			t.Fatalf("%s: %v", rec.ID, err)
		}
		largest = max(largest, rec.Bytes)
	}
	if strict.cacheBytes > cacheLimit+largest {
		t.Fatalf("the cache holds %d bytes, past its target of %d", strict.cacheBytes, cacheLimit)
	}
	for _, rec := range recs[:6] {
		if _, cached := strict.cache[rec.ID]; cached {
			t.Fatalf("%s was never evicted", rec.ID)
		}
	}
	// Request 5 copies request 4, both evicted: rebuilding it rebuilds its
	// base first.
	if got, err := strict.Body(recs[5].ID); err != nil || len(got) != recs[5].Bytes {
		t.Fatalf("an evicted body built on an evicted one was not rebuilt: %v", err)
	}
}
