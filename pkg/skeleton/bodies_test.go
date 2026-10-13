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

package skeleton_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/skeleton"
)

// memStore is a store held in memory: the tips a removal kept, by record
// id, and the removed pieces later bodies cited, by digest.
type memStore struct{ tips, pieces map[string][]byte }

func (m *memStore) Tip(id, _ string) ([]byte, error) { return found(m.tips, id) }
func (m *memStore) Piece(sha string) ([]byte, error) { return found(m.pieces, sha) }

func found(m map[string][]byte, sha string) ([]byte, error) {
	if b, ok := m[sha]; ok {
		return b, nil
	}
	return nil, fs.ErrNotExist
}

// conversation is the bodies of two chains whose calls interleave, as a main
// agent and a child do: every request repeats its chain's history, its tools
// and a long reminder, so later requests copy the front of earlier ones and
// cite their pieces.
func conversation() []providerbody.Body {
	type chain struct {
		name, system string
		tools        []any
		msgs         []any
	}
	newChain := func(name string, tools int) *chain {
		c := &chain{name: name, system: strings.Repeat("You are the "+name+" agent. ", 120)}
		for i := 0; i < tools; i++ {
			c.tools = append(c.tools, map[string]any{"name": fmt.Sprintf("Tool%d", i),
				"description": strings.Repeat(fmt.Sprintf("tool %d of %s does a thing. ", i, name), 60)})
		}
		return c
	}
	main, child := newChain("main", 8), newChain("child", 4)
	reminder := "<system-reminder>" + strings.Repeat(" mind the instructions", 120) + "</system-reminder>"
	var out []providerbody.Body
	n := 0
	call := func(c *chain, text string) {
		n++
		if len(c.msgs) == 0 {
			c.msgs = append(c.msgs, map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": reminder},
				map[string]any{"type": "text", "text": text}}})
		} else {
			c.msgs = append(c.msgs, map[string]any{"role": "user", "content": text + strings.Repeat(" more context", 150)})
		}
		req := compact(map[string]any{"model": "m-1", "system": c.system, "tools": c.tools, "messages": c.msgs})
		out = append(out, providerbody.Body{ID: fmt.Sprintf("req-%02d", n), Role: providerbody.RoleRequest, Src: fmt.Sprintf("%02d.json", n), Bytes: req})
		reply := "answer to " + text + strings.Repeat(" in detail", 140)
		out = append(out, providerbody.Body{ID: fmt.Sprintf("resp-%02d", n), Role: providerbody.RoleResponse, Src: fmt.Sprintf("%02d.response.json", n),
			Bytes: compact(map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": reply}}})})
		c.msgs = append(c.msgs, map[string]any{"role": "assistant", "content": reply})
	}
	call(main, "read the file")
	call(main, "now the other file")
	call(child, "search the repository")
	call(child, "grep further")
	call(main, "what did it find")
	call(child, "one more search")
	call(main, "summarise")
	main.msgs = nil
	call(main, "this session is being continued")
	call(main, "and then")
	call(child, "a last look")
	return out
}

func compact(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'})
}

var bodyHeader = &sessiondata.Header{H: 1, Schema: sessiondata.Schema, Kind: sessiondata.KindProviderBody,
	Adapter: "test/0", Dialect: "test/1", Src: ".", Session: "s"}

func jsonOf(t *testing.T, rec *sessiondata.Record) string {
	t.Helper()
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A body cut after a removal is cut exactly as with nothing removed, for a
// removal at any point and for removals one after another: the session holds
// the skeletons of the removed records, a store keeps the chain tips at each
// removal and the removed pieces a later body cites, and every body that is
// not removed rebuilds from what is left. Under a budget some pieces are too
// large to land: a body that cites one is not rebuilt, with or without a
// removal, and neither is a removed tip built from one.
func TestBodiesAreCutAsWithNothingRemoved(t *testing.T) {
	bodies := conversation()
	var cited, citedTooLarge, tooLargeTips int
	for _, budget := range []int64{0, 12 << 10} {
		full := providerbody.NewSession()
		full.Budget(budget)
		var want []string
		for _, b := range bodies {
			rec, err := full.Encode(b)
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, jsonOf(t, rec))
		}
		for _, removals := range [][]int{{1}, {2}, {4}, {7}, {11}, {3, 9}, {2, 4, 6, 8, 10}, {5, 6}, {9, 13}, {19}} {
			t.Run(fmt.Sprint(budget, removals), func(t *testing.T) {
				at := map[int]bool{}
				for _, i := range removals {
					at[i] = true
				}
				store := &memStore{tips: map[string][]byte{}, pieces: map[string][]byte{}}
				cur := providerbody.NewSession()
				cur.Budget(budget)
				var landed []*sessiondata.Record
				skeletal := map[int]bool{}
				for i, b := range bodies {
					if at[i] {
						// The removal: keep the bytes of every chain tip that
						// rebuilds, take out the pieces nothing landed cites
						// any more, and replace every landed record with its
						// skeleton.
						tips := map[string][]byte{}
						for _, id := range cur.Tips() {
							bb, err := cur.Body(id)
							if errors.Is(err, providerbody.ErrTooLarge) {
								if skeletalID(landed, skeletal, id) {
									tooLargeTips++
								}
								continue
							}
							if err != nil {
								t.Fatalf("tip %s does not rebuild at the removal before body %d: %v", id, i, err)
							}
							tips[id] = bb
						}
						store.tips, store.pieces = tips, map[string][]byte{}
						for j := range landed {
							if !skeletal[j] {
								landed[j], skeletal[j] = skeleton.Of(bodyHeader, landed[j]), true
							}
						}
						cur = providerbody.NewSession()
						cur.Budget(budget)
						cur.Use(store)
						for _, r := range landed {
							if err := cur.Add(r); err != nil {
								t.Fatalf("holding %s: %v", r.ID, err)
							}
						}
					}
					rec, err := cur.Encode(b)
					if err != nil {
						t.Fatalf("body %d after removals %v: %v", i, removals, err)
					}
					if got := jsonOf(t, rec); got != want[i] {
						t.Fatalf("body %d is cut differently after removals %v:\n got %s\nwant %s", i, removals, got, want[i])
					}
					citedTooLarge += citesRemovedTooLarge(t, landed, skeletal, rec)
					pieces, err := cur.RemovedPieces(rec, b.Bytes)
					if err != nil {
						t.Fatal(err)
					}
					for d, bb := range pieces {
						if _, ok := store.pieces[d]; ok {
							t.Fatalf("body %d keeps piece %s a landed body already cites", i, d)
						}
						store.pieces[d] = bb
						cited++
					}
					landed = append(landed, rec)
				}
				// What is not removed rebuilds from a session loaded fresh, or
				// fails as it does with nothing removed.
				fresh := providerbody.NewSession()
				fresh.Use(store)
				for _, r := range landed {
					if err := fresh.Add(r); err != nil {
						t.Fatalf("loading %s: %v", r.ID, err)
					}
				}
				for j, r := range landed {
					got, err := fresh.Body(r.ID)
					_, wantErr := full.Body(r.ID)
					switch {
					case skeletal[j]:
						if err == nil && !bytes.Equal(got, bodies[j].Bytes) {
							t.Fatalf("removed body %s rebuilds to other bytes", r.ID)
						}
					case errors.Is(wantErr, providerbody.ErrTooLarge):
						if !errors.Is(err, providerbody.ErrTooLarge) {
							t.Fatalf("body %s cites a piece too large to land, and after the removals: %v", r.ID, err)
						}
					case err != nil || !bytes.Equal(got, bodies[j].Bytes):
						t.Fatalf("body %s does not rebuild after the removals: %v", r.ID, err)
					}
				}
			})
		}
	}
	if cited == 0 {
		t.Fatal("no body after a removal cited a removed piece; the test does not reach the piece store")
	}
	if citedTooLarge == 0 {
		t.Fatal("no body after a removal cited a removed piece that was too large to land")
	}
	if tooLargeTips == 0 {
		t.Fatal("no removal met a removed tip too large to rebuild")
	}
}

// skeletalID says the record id is one of the skeletons in landed.
func skeletalID(landed []*sessiondata.Record, skeletal map[int]bool, id string) bool {
	for j, r := range landed {
		if r.ID == id {
			return skeletal[j]
		}
	}
	return false
}

// citesRemovedTooLarge counts the pieces rec cites that a skeleton in landed
// holds as too large to land.
func citesRemovedTooLarge(t *testing.T, landed []*sessiondata.Record, skeletal map[int]bool, rec *sessiondata.Record) int {
	t.Helper()
	tooLarge := map[string]bool{}
	for j, r := range landed {
		for _, p := range r.Parts {
			if skeletal[j] && p.State == model.ContentTooLarge {
				tooLarge[p.SHA256] = true
			}
		}
	}
	m, err := providerbody.ManifestOf(rec)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, seg := range m.Segments {
		if tooLarge[seg.Piece] {
			n++
		}
	}
	return n
}

// A body its session did not accept is not accepted from its skeleton
// either, and its skeleton indexes as it did: the skeleton keeps the
// references the session checks a body by, and the keys the index reads.
func TestARejectedBodyIsRejectedFromItsSkeleton(t *testing.T) {
	bodies := conversation()
	s := providerbody.NewSession()
	var earlier []*sessiondata.Record
	for _, b := range bodies[:2] {
		rec, err := s.Encode(b)
		if err != nil {
			t.Fatal(err)
		}
		earlier = append(earlier, rec)
	}
	second, err := s.Encode(bodies[2])
	if err != nil {
		t.Fatal(err)
	}
	refers := false
	m, _ := providerbody.ManifestOf(second)
	for _, seg := range m.Segments {
		refers = refers || seg.Piece != "" || seg.Copy != nil
	}
	if !refers {
		t.Fatal("the second request refers to no earlier body; the test needs one that does")
	}
	_, recs := read(t, skeletonOf(t, second))
	sk := recs[0]
	// A session that lost the first call rejects the second request, and
	// its skeleton, for the same reason.
	lost := providerbody.NewSession()
	errBody := lost.Add(second)
	errSkeleton := providerbody.NewSession().Add(sk)
	if errBody == nil || errSkeleton == nil || errBody.Error() != errSkeleton.Error() {
		t.Fatalf("the body is rejected with %v and its skeleton with %v", errBody, errSkeleton)
	}
	// A session that holds the first call takes both, the skeleton once.
	held := providerbody.NewSession()
	for _, rec := range earlier {
		if err := held.Add(skeleton.Of(bodyHeader, rec)); err != nil {
			t.Fatalf("the skeleton of an accepted body: %v", err)
		}
	}
	if err := held.Add(sk); err != nil {
		t.Fatalf("the skeleton of a body that refers to it: %v", err)
	}
	if err := held.Add(sk); !errors.Is(err, providerbody.ErrRepeat) {
		t.Fatalf("a skeleton taken twice: %v", err)
	}
	ix := index.New("s")
	_, _, a := index.FromRecord(ix, bodyHeader, second, 1, 1)
	_, _, b := index.FromRecord(ix, bodyHeader, sk, 1, 1)
	if a == nil || b == nil || *a != *b {
		t.Fatalf("the skeleton indexes as %+v, the body as %+v", b, a)
	}
}

// skeletonOf is the skeleton of a landed file holding rec alone.
func skeletonOf(t *testing.T, rec *sessiondata.Record) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := sessiondata.NewWriter(&buf, bodyHeader)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(rec); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := skeleton.Write(&out, buf.Bytes(), sessiondata.Removed{Generation: 1}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// A manifest as another writer could land it is kept as the session judged
// it. The skeleton writes the manifest as a reader decodes it, so a key in
// another case, or given twice, means what it meant: a literal in any
// spelling goes with its text, a reference beside it stays, and a manifest
// with no segments keeps none. Each body is accepted or rejected from its
// skeleton as it was, and indexes the same.
func TestAnOddManifestIsJudgedTheSameFromItsSkeleton(t *testing.T) {
	missing := strings.Repeat("e", 64)
	for name, c := range map[string]struct {
		keys   string
		accept bool
	}{
		"no segments":           {``, false},
		"a literal and a piece": {`,"segments":[{"lit":"","piece":"` + missing + `"}]`, false},
		"a literal and a copy":  {`,"segments":[{"lit":"x","copy":{"from":"gone","sha256":"` + missing + `","len":1}}]`, false},
		"a literal in capitals": {`,"segments":[{"Lit":"secret body text"},{"part":0}]`, true},
		"part, then Part":       {`,"segments":[{"part":0,"Part":-1}]`, false},
		"Part, then part":       {`,"segments":[{"Part":-1,"part":0}]`, true},
		"keys in capitals":      {`,"Request":"req_9","Previous_Request":"req_8","segments":[{"Lit":"secret body text"},{"part":0}]`, true},
	} {
		t.Run(name, func(t *testing.T) {
			m := `{"schema":"` + providerbody.Schema + `","role":"request","src":"1.json","sha256":"` + missing +
				`","bytes":1,"depth":0,"request":"req_2","previous_request":"req_1"` + c.keys + `}`
			if !c.accept {
				m = strings.Replace(m, `"depth":0`, `"depth":1`, 1)
			}
			piece := json.RawMessage(`"x"`)
			rec := &sessiondata.Record{Ord: 1, Sha: missing[:12], Bytes: 1, ID: "req_2", Parts: []sessiondata.Part{
				{Kind: sessiondata.PartData, Data: piece, State: model.ContentAvailable, Bytes: len(piece)},
				{Kind: sessiondata.PartData, Data: json.RawMessage(m), State: model.ContentAvailable, Bytes: len(m)}}}
			_, recs := read(t, skeletonOf(t, rec))
			sk := recs[0]
			errBody := providerbody.NewSession().Add(rec)
			errSkeleton := providerbody.NewSession().Add(sk)
			if c.accept != (errBody == nil) {
				t.Fatalf("the body is accepted %v, want %v: %v", errBody == nil, c.accept, errBody)
			}
			if (errBody == nil) != (errSkeleton == nil) || (errBody != nil && errBody.Error() != errSkeleton.Error()) {
				t.Fatalf("the body is judged %v and its skeleton %v", errBody, errSkeleton)
			}
			if kept := string(sk.Parts[1].Data); strings.Contains(strings.ToLower(kept), `"lit"`) || strings.Contains(kept, "secret") {
				t.Fatalf("the skeleton kept a literal: %s", kept)
			}
			ix := index.New("s")
			_, _, a := index.FromRecord(ix, bodyHeader, rec, 1, 1)
			_, _, b := index.FromRecord(ix, bodyHeader, sk, 1, 1)
			if a == nil || b == nil || *a != *b {
				t.Fatalf("the skeleton indexes as %+v, the body as %+v", b, a)
			}
		})
	}
}

// A removed body is read from the store by its own record. An identical body
// under another id, which was too large to land, is not made usable by the
// tip a removal kept of the first: with nothing removed it could not be
// rebuilt, so it is not copied from after a removal either.
func TestAnIdenticalBodyDoesNotTakeAnotherTip(t *testing.T) {
	req := conversation()[0]
	whole := providerbody.NewSession()
	first, err := whole.Encode(req)
	if err != nil {
		t.Fatal(err)
	}
	again := req
	again.ID = req.ID + "-again"
	small := providerbody.NewSession()
	small.Budget(4 << 10)
	second, err := small.Encode(again)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(jsonOf(t, second), model.ContentTooLarge) {
		t.Fatal("the second copy landed whole; the test needs a piece of it too large to land")
	}
	store := &memStore{tips: map[string][]byte{req.ID: req.Bytes}, pieces: map[string][]byte{}}
	held := providerbody.NewSession()
	held.Use(store)
	for _, rec := range []*sessiondata.Record{first, second} {
		if err := held.Add(skeleton.Of(bodyHeader, rec)); err != nil {
			t.Fatal(err)
		}
	}
	if b, err := held.Body(req.ID); err != nil || !bytes.Equal(b, req.Bytes) {
		t.Fatalf("the kept tip: %v", err)
	}
	if _, err := held.Body(again.ID); !errors.Is(err, providerbody.ErrTooLarge) {
		t.Fatalf("an identical body too large to land reads %v, want ErrTooLarge", err)
	}
}
