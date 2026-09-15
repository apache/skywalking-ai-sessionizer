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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// chain builds request bodies the way Claude Code 2.1.260 was measured to:
// the whole message list again on every call, the cache marker on the
// newest message only, a billing header naming the previous request and the
// prompt, and the session in metadata.user_id.
type chain struct {
	session, prompt string
	system          []any
	tools           []any
	msgs            []map[string]any
	prev            string
	reminder        string
}

func newChain(session, prompt, systemText string, tools int) *chain {
	c := &chain{session: session, prompt: prompt, reminder: "<system-reminder>" + strings.Repeat(" instructions", 400) + "</system-reminder>"}
	c.system = []any{
		map[string]any{"type": "text", "text": ""},
		map[string]any{"type": "text", "text": systemText, "cache_control": map[string]any{"type": "ephemeral"}},
	}
	for i := 0; i < tools; i++ {
		c.tools = append(c.tools, map[string]any{
			"name": fmt.Sprintf("Tool%d", i), "description": strings.Repeat(fmt.Sprintf("tool %d does a thing. ", i), 80),
			"input_schema": map[string]any{"type": "object"},
		})
	}
	return c
}

func (c *chain) add(role string, text string) {
	if len(c.msgs) == 0 {
		// The first message carries the injected reminders beside the
		// prompt, several blocks, so it keeps its shape on every call.
		c.msgs = append(c.msgs, map[string]any{"role": role, "content": []any{
			map[string]any{"type": "text", "text": c.reminder},
			map[string]any{"type": "text", "text": text},
		}})
		return
	}
	for _, m := range c.msgs {
		if blocks, ok := m["content"].([]any); ok && len(blocks) == 1 {
			if b, ok := blocks[0].(map[string]any); ok && b["type"] == "text" {
				m["content"] = b["text"]
			}
		}
	}
	c.msgs = append(c.msgs, map[string]any{"role": role, "content": []any{
		map[string]any{"type": "text", "text": text, "cache_control": map[string]any{"type": "ephemeral"}},
	}})
}

func (c *chain) request() []byte {
	header := "x-anthropic-billing-header: cc_version=2.1.260; cch=00000;"
	if c.prev != "" {
		header += " cc_prev_req=" + c.prev + ";"
	}
	header += " cc_prompt_id=" + c.prompt + ";"
	c.system[0].(map[string]any)["text"] = header
	user, _ := json.Marshal(map[string]string{"device_id": "d", "session_id": c.session})
	body := map[string]any{
		"model": "claude-opus-5", "messages": c.msgs, "system": c.system, "tools": c.tools,
		"metadata": map[string]any{"user_id": string(user)}, "max_tokens": 64000,
	}
	return compact(body)
}

// compact encodes the way JSON.stringify does: no spaces, no HTML escapes.
// Go orders map keys alphabetically, which is still one fixed order.
func compact(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'})
}

func response(id, text string) []byte {
	return compact(map[string]any{"id": id, "type": "message", "role": "assistant", "model": "claude-opus-5",
		"content": []any{map[string]any{"type": "text", "text": text}}, "stop_reason": "end_turn"})
}

// encode names a body the way these tests write them: an id and a role from
// a file name, and no keys, since cutting and rebuilding read none.
func encode(s *Session, file string, b []byte) (*sessiondata.Record, error) {
	role := RoleRequest
	if strings.HasSuffix(file, ".response.json") {
		role = RoleResponse
	}
	return s.Encode(Body{ID: strings.TrimSuffix(file, ".json"), Role: role, Src: file, Bytes: b})
}

type body struct {
	file string
	b    []byte
}

// interleaved is a main chain with a subagent whose calls land between two
// main calls, then a compaction that starts the main chain again. With
// sharedReminder the child's first message starts with the main chain's
// reminder; measured subagents diverge within the first few hundred bytes.
func interleaved(sharedReminder bool) []body {
	var out []body
	main := newChain("s1", "p1", strings.Repeat("You are an agent. ", 300), 12)
	child := newChain("s1", "p1", strings.Repeat("You search a repository. ", 120), 6)
	if !sharedReminder {
		child.reminder = "<system-reminder>" + strings.Repeat(" as you answer", 300) + "</system-reminder>"
	}
	call := func(c *chain, req, resp, text string) {
		c.add("user", text+strings.Repeat(" context", 200))
		out = append(out, body{req + ".request.json", c.request()})
		reply := "answer to " + text
		out = append(out, body{resp + ".response.json", response("msg_"+resp, reply)})
		c.add("assistant", reply)
		c.prev = resp
	}
	call(main, "00000000-0000-4000-8000-000000000001", "req_1", "read the file")
	call(main, "00000000-0000-4000-8000-000000000002", "req_2", "now the other file")
	call(child, "00000000-0000-4000-8000-000000000003", "req_3", "find input_digest")
	call(child, "00000000-0000-4000-8000-000000000004", "req_4", "grep further")
	call(main, "00000000-0000-4000-8000-000000000005", "req_5", "what did it find")
	call(child, "00000000-0000-4000-8000-000000000006", "req_6", "one more search")
	call(main, "00000000-0000-4000-8000-000000000007", "req_7", "summarise")
	main.msgs = nil
	call(main, "00000000-0000-4000-8000-000000000008", "req_8", "This session is being continued")
	call(main, "00000000-0000-4000-8000-000000000009", "req_9", "and then")
	return out
}

func TestEncodedBodiesRebuildExactlyAfterLanding(t *testing.T) {
	bodies := interleaved(false)
	s := NewSession()
	var recs []*sessiondata.Record
	raw, landed := 0, 0
	for _, b := range bodies {
		rec, err := encode(s, b.file, b.b)
		if err != nil {
			t.Fatalf("%s: %v", b.file, err)
		}
		recs = append(recs, rec)
		raw += len(b.b)
	}

	// Land every record through the writer, read the file back into a new
	// session, and rebuild every body from what was read.
	var buf bytes.Buffer
	w, err := sessiondata.NewWriter(&buf, &sessiondata.Header{Kind: sessiondata.KindProviderBody, Session: "s1", Src: ".", Dialect: "test"})
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		if err := w.Write(rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	landed = buf.Len()
	r, err := sessiondata.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	fresh := NewSession()
	for i := 0; ; i++ {
		rec, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := fresh.Add(rec); err != nil {
			t.Fatalf("add %s: %v", rec.ID, err)
		}
		got, err := fresh.Body(rec.ID)
		if err != nil {
			t.Fatalf("rebuild %s: %v", rec.ID, err)
		}
		if !bytes.Equal(got, bodies[i].b) {
			t.Fatalf("%s rebuilt to different bytes", rec.ID)
		}
	}
	if landed*2 > raw {
		t.Fatalf("landed %d bytes for %d raw; the repeats were not removed", landed, raw)
	}
}

func TestACopyFollowsItsOwnChainAcrossInterleavedCalls(t *testing.T) {
	// Call 5 is the main chain's, after two child calls; it copies call 2.
	// Call 6 is the child's, after a main call; it copies call 4. The first
	// call after compaction starts a new chain.
	want := map[string]string{
		"00000000-0000-4000-8000-000000000002.request": "00000000-0000-4000-8000-000000000001.request",
		"00000000-0000-4000-8000-000000000004.request": "00000000-0000-4000-8000-000000000003.request",
		"00000000-0000-4000-8000-000000000005.request": "00000000-0000-4000-8000-000000000002.request",
		"00000000-0000-4000-8000-000000000006.request": "00000000-0000-4000-8000-000000000004.request",
		"00000000-0000-4000-8000-000000000007.request": "00000000-0000-4000-8000-000000000005.request",
		"00000000-0000-4000-8000-000000000009.request": "00000000-0000-4000-8000-000000000008.request",
	}
	for _, shared := range []bool{false, true} {
		s := NewSession()
		base := map[string]string{}
		for _, b := range interleaved(shared) {
			rec, err := encode(s, b.file, b.b)
			if err != nil {
				t.Fatal(err)
			}
			m, _ := ManifestOf(rec)
			for _, seg := range m.Segments {
				if seg.Copy != nil {
					base[rec.ID] = seg.Copy.From
				}
			}
		}
		for id, from := range want {
			if base[id] != from {
				t.Errorf("shared reminder %v: %s copies from %q, want %q", shared, id, base[id], from)
			}
		}
		// With its own reminder, a chain's first call shares too little with
		// anything to copy. With the main chain's reminder, it may copy that
		// front from the main chain, and the main chain keeps its own link.
		if !shared {
			for _, id := range []string{"00000000-0000-4000-8000-000000000001.request", "00000000-0000-4000-8000-000000000003.request"} {
				if from, ok := base[id]; ok {
					t.Errorf("%s is the first call of its chain and copies from %s", id, from)
				}
			}
		}
	}
}

func TestAChainOfCopiesStopsAtMaxDepth(t *testing.T) {
	s := NewSession()
	c := newChain("s1", "p1", strings.Repeat("system ", 2000), 4)
	deepest := 0
	for i := 0; i < MaxDepth+5; i++ {
		c.add("user", fmt.Sprintf("turn %d %s", i, strings.Repeat("x", 3000)))
		rec, err := encode(s, fmt.Sprintf("00000000-0000-4000-8000-%012d.request.json", i), c.request())
		if err != nil {
			t.Fatal(err)
		}
		m, _ := ManifestOf(rec)
		if m.Depth > MaxDepth {
			t.Fatalf("depth %d past %d", m.Depth, MaxDepth)
		}
		deepest = max(deepest, m.Depth)
		c.add("assistant", "ok")
	}
	if deepest != MaxDepth {
		t.Fatalf("the chain reached depth %d, want %d", deepest, MaxDepth)
	}
}

func TestABodyThatCannotBeCutLandsWholeAndSaysWhy(t *testing.T) {
	for name, raw := range map[string][]byte{
		"not JSON":      []byte(`{"model":"claude-opus-5","messages":[`),
		"not UTF-8":     []byte("{\"model\":\"\xff\"}"),
		"not an object": []byte(`["a"]`),
		"not compact":   []byte("{\"tools\": [ {\"name\": \"a\"} ]}"),
	} {
		s := NewSession()
		rec, err := encode(s, "00000000-0000-4000-8000-000000000001.request.json", raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		m, _ := ManifestOf(rec)
		got, err := s.Body(rec.ID)
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatalf("%s: rebuilt %q, %v", name, got, err)
		}
		if name == "not compact" {
			// Whitespace between tokens is no problem: literals keep it.
			continue
		}
		if m.Why == "" || rec.Parts[0].Kind != sessiondata.PartUnknown {
			t.Fatalf("%s: landed cut, want whole with a reason", name)
		}
	}
}

func TestAddRefusesAReferenceToSomethingNotYetHeld(t *testing.T) {
	bodies := interleaved(false)
	s := NewSession()
	var recs []*sessiondata.Record
	for _, b := range bodies[:6] {
		rec, err := encode(s, b.file, b.b)
		if err != nil {
			t.Fatal(err)
		}
		recs = append(recs, rec)
	}
	fresh := NewSession()
	// The third request copies an earlier one; added first, it names
	// something the session does not hold.
	if err := fresh.Add(recs[2]); err == nil {
		t.Fatal("a record whose base was never added was accepted")
	}
	if err := fresh.Add(recs[0]); err != nil {
		t.Fatal(err)
	}
	if err := fresh.Add(recs[0]); !errors.Is(err, ErrRepeat) {
		t.Fatalf("the same record added twice: %v, want ErrRepeat", err)
	}
	if _, err := encode(s, bodies[0].file, append([]byte(nil), bodies[1].b...)); err == nil {
		t.Fatal("a body read again under the same name with other bytes was accepted")
	}
}

func TestPiecesAreToolsAndLongStrings(t *testing.T) {
	long := strings.Repeat("a", MinPiece)
	b := compact(map[string]any{
		"messages": []any{map[string]any{"content": long, "role": "user"}, map[string]any{"content": "short", "role": "user"}},
		"tools":    []any{map[string]any{"description": long, "name": "A"}, map[string]any{"name": "B"}},
	})
	spans, _, err := pieces(b)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, sp := range spans {
		p := string(b[sp.a:sp.z])
		if len(p) > 20 {
			p = p[:20]
		}
		got = append(got, p)
	}
	want := []string{`"aaaaaaaaaaaaaaaaaaa`, `{"description":"aaaa`, `{"name":"B"}`}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("pieces %q, want %q", got, want)
	}
}

func TestAddRefusesNegativeSizesAndABadRepeat(t *testing.T) {
	bodies := interleaved(false)
	s := NewSession()
	var recs []*sessiondata.Record
	for _, b := range bodies[:3] {
		rec, err := encode(s, b.file, b.b)
		if err != nil {
			t.Fatal(err)
		}
		recs = append(recs, rec)
	}
	withManifest := func(rec *sessiondata.Record, edit func(*Manifest)) *sessiondata.Record {
		m, _ := ManifestOf(rec)
		edit(m)
		cp := *rec
		cp.Parts = append(append([]sessiondata.Part(nil), rec.Parts[:len(rec.Parts)-1]...), manifestPart(m))
		return &cp
	}
	fresh := NewSession()
	if err := fresh.Add(withManifest(recs[0], func(m *Manifest) { m.Bytes = -1 })); err == nil {
		t.Fatal("a record claiming -1 bytes was accepted")
	}
	if err := fresh.Add(recs[0]); err != nil {
		t.Fatal(err)
	}
	if err := fresh.Add(recs[1]); err != nil {
		t.Fatal(err)
	}
	negative := withManifest(recs[2], func(m *Manifest) {
		for i := range m.Segments {
			if m.Segments[i].Copy != nil {
				m.Segments[i].Copy.Len = -1
			}
		}
	})
	if err := fresh.Add(negative); err == nil {
		t.Fatal("a copy of -1 bytes was accepted")
	}
	// A repeat that claims the body's digest but does not rebuild to it.
	lie := withManifest(recs[0], func(m *Manifest) { m.Segments = []Segment{{Lit: "[]"}} })
	if err := fresh.Add(lie); err == nil || errors.Is(err, ErrRepeat) {
		t.Fatalf("a repeat that does not rebuild was taken for a repeat: %v", err)
	}
}

func TestABodyHandedOutCannotChangeWhatLaterBodiesCopy(t *testing.T) {
	bodies := interleaved(false)
	s := NewSession()
	first, err := encode(s, bodies[0].file, bodies[0].b)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Body(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := range got {
		got[i] = 'x'
	}
	again, err := s.Body(first.ID)
	if err != nil || !bytes.Equal(again, bodies[0].b) {
		t.Fatal("changing a body a caller was given changed the session's copy")
	}
}

func TestAddAndRebuildWithstandAnyClaim(t *testing.T) {
	bodies := interleaved(false)
	s := NewSession()
	first, err := encode(s, bodies[0].file, bodies[0].b)
	if err != nil {
		t.Fatal(err)
	}
	withManifest := func(rec *sessiondata.Record, edit func(*Manifest)) *sessiondata.Record {
		m, _ := ManifestOf(rec)
		edit(m)
		cp := *rec
		cp.Parts = append(append([]sessiondata.Part(nil), rec.Parts[:len(rec.Parts)-1]...), manifestPart(m))
		return &cp
	}
	fresh := NewSession()
	// A size past MaxBytes is refused before anything is allocated.
	huge := withManifest(first, func(m *Manifest) { m.Bytes = 1 << 62; m.Segments = []Segment{{Lit: "{}"}} })
	huge.ID = "huge.request"
	if err := fresh.Add(huge); err == nil {
		t.Fatal("a body claiming 2^62 bytes was accepted")
	}
	// A repeat with a bogus copy is refused, not taken for a repeat.
	if err := fresh.Add(first); err != nil {
		t.Fatal(err)
	}
	bogus := withManifest(first, func(m *Manifest) {
		m.Depth = 1
		m.Segments = []Segment{{Copy: &Copy{From: first.ID, SHA256: "bogus", Len: 2}}}
	})
	if err := fresh.Add(bogus); err == nil || errors.Is(err, ErrRepeat) {
		t.Fatalf("a repeat with a copy of the wrong digest: %v", err)
	}
	deep := withManifest(first, func(m *Manifest) { m.Depth = 3 })
	if err := fresh.Add(deep); err == nil || errors.Is(err, ErrRepeat) {
		t.Fatalf("a record that copies nothing and claims depth 3: %v", err)
	}
}

func TestARebuildStopsAtItsClaim(t *testing.T) {
	bodies := interleaved(false)
	s := NewSession()
	first, err := encode(s, bodies[0].file, bodies[0].b)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := ManifestOf(first)
	var piece string
	for _, seg := range m.Segments {
		if seg.Part != nil {
			piece = digest(first.Parts[*seg.Part].Data)
			break
		}
	}
	segs := make([]Segment, 10000)
	for i := range segs {
		segs[i] = Segment{Piece: piece}
	}
	bomb := &Manifest{Schema: Schema, Role: RoleRequest, Src: "x", SHA256: digest([]byte("{}")), Bytes: 2, Segments: segs}
	rec := &sessiondata.Record{Ord: 1, ID: "bomb.request", Parts: []sessiondata.Part{manifestPart(bomb)}}
	fresh := NewSession()
	if err := fresh.Add(first); err != nil {
		t.Fatal(err)
	}
	if err := fresh.Add(rec); err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Body("bomb.request"); err == nil {
		t.Fatal("a record naming a piece 10,000 times rebuilt")
	}
	if n := RecordBytes(first); n <= 0 {
		t.Fatalf("RecordBytes is %d", n)
	}
}
