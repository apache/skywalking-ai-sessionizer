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
	"fmt"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// listOf is the history a chain's request carries: one entry per message,
// its digest standing in for the form an adapter would compute.
func listOf(c *chain) []Entry {
	out := make([]Entry, 0, len(c.msgs))
	for i := range c.msgs {
		out = append(out, EntryOf("", false, []byte(fmt.Sprintf("message %d of %s", i, c.prompt))))
	}
	return out
}

// grow encodes n requests of one chain, each with its list, and returns their
// ids, the records it landed, and the list each was given.
func grow(t *testing.T, s *Session, c *chain, n int, form string) ([]string, []*sessiondata.Record, [][]Entry) {
	t.Helper()
	var ids []string
	var recs []*sessiondata.Record
	var lists [][]Entry
	for i := 0; i < n; i++ {
		c.add("user", fmt.Sprintf("question %d %s", i, strings.Repeat("x", 300)))
		id := fmt.Sprintf("%s-%03d.request", c.prompt, i)
		h := &History{Form: form, Messages: listOf(c)}
		rec, err := s.Encode(Body{ID: id, Role: RoleRequest, Src: id + ".json", Bytes: c.request(), History: h})
		if err != nil {
			t.Fatal(err)
		}
		ids, recs, lists = append(ids, id), append(recs, rec), append(lists, h.Messages)
		c.add("assistant", fmt.Sprintf("answer %d", i))
	}
	return ids, recs, lists
}

// reloaded is a session holding recs as a reader holds them: written and read
// back as landed lines.
func reloaded(t *testing.T, recs []*sessiondata.Record) *Session {
	t.Helper()
	var buf bytes.Buffer
	w, err := sessiondata.NewWriter(&buf, &sessiondata.Header{Kind: sessiondata.KindProviderBody, Session: "s", Src: ".", Dialect: "d"})
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
	r, err := sessiondata.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSession()
	for range recs {
		back, err := r.Next()
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Add(back); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// TestAListIsWrittenAgainstTheBodyItsCopyComesFrom. A request's list shares
// its front with the list of the body its bytes are copied from, so it keeps
// what it shares and writes only what is new, and reads back whole.
func TestAListIsWrittenAgainstTheBodyItsCopyComesFrom(t *testing.T) {
	s := NewSession()
	c := newChain("s", "p", "system prompt", 2)
	ids, _, _ := grow(t, s, c, 5, FormClaudeCode)
	for i, id := range ids {
		m, _ := s.Manifest(id)
		switch base := copiedFrom(m); {
		case i == 0:
			if base != "" || m.Messages.Relative() {
				t.Fatalf("the first request copies from %q and writes its list %+v", base, m.Messages)
			}
		case base == "" || m.Messages.Base != base:
			t.Fatalf("%s copies from %q and writes its list against %q", id, base, m.Messages.Base)
		case len(m.Messages.Add) != 2:
			t.Fatalf("%s adds %d entries, want the 2 new messages", id, len(m.Messages.Add))
		}
		list, form, err := s.ListOf(id)
		if err != nil || form != FormClaudeCode {
			t.Fatalf("%s: %v, form %q", id, err, form)
		}
		if want := 2*i + 1; len(list) != want {
			t.Fatalf("%s reads back %d entries, want %d", id, len(list), want)
		}
	}
}

// TestAListIsWholeWhenItsBaseCannotBeKept. A base that carries no list, or one
// of another form, gives nothing to keep, so the list is written whole.
func TestAListIsWholeWhenItsBaseCannotBeKept(t *testing.T) {
	for _, tc := range []struct {
		name string
		base *History
	}{
		{"no list", nil},
		{"another form", &History{Form: FormLangChain}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewSession()
			c := newChain("s", "p", "system prompt", 2)
			c.add("user", "first "+strings.Repeat("x", 300))
			if tc.base != nil {
				tc.base.Messages = listOf(c)
			}
			if _, err := s.Encode(Body{ID: "a.request", Role: RoleRequest, Bytes: c.request(), History: tc.base}); err != nil {
				t.Fatal(err)
			}
			c.add("assistant", "answer")
			c.add("user", "second "+strings.Repeat("x", 300))
			if _, err := s.Encode(Body{ID: "b.request", Role: RoleRequest, Bytes: c.request(),
				History: &History{Form: FormClaudeCode, Messages: listOf(c)}}); err != nil {
				t.Fatal(err)
			}
			m, _ := s.Manifest("b.request")
			if copiedFrom(m) != "a.request" {
				t.Fatalf("the second request copies from %q; the test needs a copy", copiedFrom(m))
			}
			if m.Messages.Relative() || len(m.Messages.Entries) != 3 {
				t.Fatalf("written %+v, want the 3 entries whole", m.Messages)
			}
		})
	}
}

// TestAListIsNeverDeeperThanItsBody. A body past the depth limit copies
// nothing, and its list is written whole with it, so a list's chain is its
// body's chain and every list reads back.
func TestAListIsNeverDeeperThanItsBody(t *testing.T) {
	s := NewSession()
	c := newChain("s", "p", "system prompt", 2)
	ids, recs, lists := grow(t, s, c, MaxDepth+3, FormClaudeCode)
	read := reloaded(t, recs)
	whole := 0
	for i, id := range ids {
		m, _ := read.Manifest(id)
		if m.Messages.Relative() != (copiedFrom(m) != "") {
			t.Fatalf("%s at depth %d: list relative %v, copy from %q", id, m.Depth, m.Messages.Relative(), copiedFrom(m))
		}
		if !m.Messages.Relative() {
			whole++
		}
		list, _, err := read.ListOf(id)
		if err != nil || fmt.Sprint(list) != fmt.Sprint(lists[i]) {
			t.Fatalf("%s reads back %v, %v; it was given %v", id, list, err, lists[i])
		}
	}
	if whole != 2 {
		t.Fatalf("%d lists written whole, want the first and the one past the depth limit", whole)
	}
}

// TestABodyThatFallsBackToOnePartWritesItsListWhole. A body whose cut chose a
// copy, but whose record was still over the budget with every piece taken
// out, lands whole in one part and copies nothing, so the list written with
// the copy is written again whole.
func TestABodyThatFallsBackToOnePartWritesItsListWhole(t *testing.T) {
	front := `{"messages":[{"role":"user","content":"` + strings.Repeat("a", 6000) + `"}`
	many := strings.Repeat(`,{"role":"user","content":"short"}`, 600)
	bodies := []Body{
		{ID: "a.request", Role: RoleRequest, Bytes: []byte(front + `]}`),
			History: &History{Form: FormClaudeCode, Messages: []Entry{EntryOf("", false, []byte("a"))}}},
		{ID: "b.request", Role: RoleRequest, Bytes: []byte(front + many + `]}`),
			History: &History{Form: FormClaudeCode, Messages: []Entry{EntryOf("", false, []byte("a")), EntryOf("", false, []byte("b"))}}},
	}
	for _, budget := range []int64{0, 16 << 10} {
		s := NewSession()
		s.Budget(budget)
		for _, b := range bodies {
			if _, err := s.Encode(b); err != nil {
				t.Fatal(err)
			}
		}
		m, _ := s.Manifest("b.request")
		switch budget {
		case 0:
			// With no budget the cut stands: a copy, and the list against it.
			if copiedFrom(m) != "a.request" || !m.Messages.Relative() {
				t.Fatalf("with no budget: copy %q, list %+v; the test needs a copy", copiedFrom(m), m.Messages)
			}
		default:
			if copiedFrom(m) != "" || m.Messages.Relative() || len(m.Messages.Entries) != 2 {
				t.Fatalf("under the budget: copy %q, list %+v, want the list whole", copiedFrom(m), m.Messages)
			}
		}
	}
}

// TestABodyLandedWholeWritesItsListWhole. A body that cannot be cut lands in
// one part and copies nothing, so its list has no base to be written against.
func TestABodyLandedWholeWritesItsListWhole(t *testing.T) {
	s := NewSession()
	h := &History{Form: FormLangChain, Messages: []Entry{EntryOf("m1", true, []byte("one"))}}
	rec, err := s.Encode(Body{ID: "a.request", Role: RoleRequest, Bytes: []byte("not json"), History: h})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := ManifestOf(rec)
	if m.Messages == nil || m.Messages.Relative() || len(m.Messages.Entries) != 1 || m.HistoryForm != FormLangChain {
		t.Fatalf("manifest %+v", m.Messages)
	}
}

// TestAListOverTheCapIsUnavailable. The budget never takes a manifest out, so
// a list that would take more than a quarter of it says it is too large, with
// how many messages it held, rather than making a record no file can hold. A
// quarter of the file's budget, its header and closing line included.
func TestAListOverTheCapIsUnavailable(t *testing.T) {
	entries := func(n int) []Entry {
		out := make([]Entry, n)
		for i := range out {
			out[i] = EntryOf(fmt.Sprint(i), true, []byte(fmt.Sprint("message ", i)))
		}
		return out
	}
	// An entry with an id takes 53 bytes, and a comma between two: 18 take
	// 973 bytes with the brackets, under a quarter of 4,096; 19 take 1,027.
	for n, want := range map[int]string{18: "", 19: TooLarge} {
		s := NewSession()
		s.Budget(4096)
		if b, _ := json.Marshal(entries(n)); (int64(len(b)) > 1024) != (want == TooLarge) {
			t.Fatalf("%d entries take %d bytes; the test's numbers are wrong", n, len(b))
		}
		rec, err := s.Encode(Body{ID: "a.request", Role: RoleRequest, Bytes: []byte(`{"messages":[]}`),
			History: &History{Form: FormLangChain, Messages: entries(n)}})
		if err != nil {
			t.Fatal(err)
		}
		m, _ := ManifestOf(rec)
		if m.Messages.Unavailable != want || (want == TooLarge && m.Messages.Count != n) {
			t.Fatalf("%d entries written %+v, want %q", n, m.Messages, want)
		}
		if got := RecordBytes(rec); got > 4096-FileOverhead {
			t.Fatalf("the record takes %d bytes, over the budget", got)
		}
		if _, _, err := s.ListOf("a.request"); (err == nil) != (want == "") {
			t.Fatalf("%d entries read back: %v", n, err)
		}
	}
}

// TestAListAgainstALaterBaseIsUnavailable. A base is a body that landed before
// the list's own; a list that names one added after it does not read, though
// both bodies are held and the later one's list is whole.
func TestAListAgainstALaterBaseIsUnavailable(t *testing.T) {
	_, early, _ := grow(t, NewSession(), newChain("s", "p", "system prompt", 2), 1, FormClaudeCode)
	_, later, _ := grow(t, NewSession(), newChain("s", "q", "another prompt", 2), 1, FormClaudeCode)
	// The early request names the later one as its base, and lands first.
	first := *early[0]
	first.Parts = append([]sessiondata.Part(nil), early[0].Parts...)
	m, _ := ManifestOf(early[0])
	m.Messages = &Messages{Base: later[0].ID, Keep: 1, Add: []Entry{}}
	first.Parts[len(first.Parts)-1] = manifestPart(m)
	read := NewSession()
	for _, rec := range []*sessiondata.Record{&first, later[0]} {
		if err := read.Add(rec); err != nil {
			t.Fatal(err)
		}
	}
	if list, _, err := read.ListOf(later[0].ID); err != nil || len(list) != 1 {
		t.Fatalf("the later request's own list: %v %v; the test needs it whole", list, err)
	}
	if _, _, err := read.ListOf(first.ID); err == nil {
		t.Fatal("a list against a base added after it read back")
	}
}

// TestAListAgainstABaseThatDoesNotHoldIsUnavailable. A reader checks what a
// list keeps from its base, and a list that keeps more than the base holds,
// or keeps from a body of another form, reads as unavailable, never as a
// shorter list.
func TestAListAgainstABaseThatDoesNotHoldIsUnavailable(t *testing.T) {
	s := NewSession()
	c := newChain("s", "p", "system prompt", 2)
	ids, _, _ := grow(t, s, c, 2, FormClaudeCode)
	h := s.records[ids[1]]
	h.m.Messages.Keep = 99
	if _, _, err := s.ListOf(ids[1]); err == nil {
		t.Fatal("a list keeping more than its base holds read back")
	}
	h.m.Messages.Keep = 1
	h.m.HistoryForm = FormLangChain
	if _, _, err := s.ListOf(ids[1]); err == nil {
		t.Fatal("a list against a base of another form read back")
	}
}

// TestAMessageListReadsAsExactlyOneShape.
func TestAMessageListReadsAsExactlyOneShape(t *testing.T) {
	e := `{"id":"0123456789abcdef","digest":"fedcba9876543210"}`
	for _, ok := range []string{
		`{"entries":[]}`, `{"entries":[` + e + `]}`, `{"base":"a","keep":1,"add":[]}`,
		`{"unavailable":"too_large","count":3}`, `{"unavailable":"plural_prompts"}`,
	} {
		var m Messages
		if err := json.Unmarshal([]byte(ok), &m); err != nil {
			t.Fatalf("%s: %v", ok, err)
		}
		back, err := json.Marshal(m)
		if err != nil || string(back) != ok {
			t.Fatalf("%s wrote back as %s, %v", ok, back, err)
		}
	}
	// Anything else is a list that is unavailable, never a manifest that does
	// not read: the body still joins its call.
	for _, bad := range []string{
		`{}`, `{"entries":[],"base":"a","keep":1,"add":[]}`, `{"base":"a","keep":0,"add":[]}`,
		`{"base":"","keep":1,"add":[]}`, `{"base":"a","keep":1}`, `{"unavailable":""}`,
		`{"entries":[{"digest":"short"}]}`, `{"entries":[{"id":"xyz","digest":"fedcba9876543210"}]}`,
		`[]`, `"entries"`,
	} {
		var m Messages
		if err := json.Unmarshal([]byte(bad), &m); err != nil || m.Unavailable != UnrecognisedShape {
			t.Fatalf("%s read as %+v, %v", bad, m, err)
		}
	}
}

// TestCanonicalEscapesOnlyWhatJSONRequires. Go's encoder escapes U+2028 and
// U+2029; another implementation following the format page would not, and
// a digest has to come out the same in both.
func TestCanonicalEscapesOnlyWhatJSONRequires(t *testing.T) {
	v, err := Decode([]byte(`{"z":1.50,"a":["  <&>","\"\\\n\t\u0001é"],"m":null,"b":true}`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Canonical(v)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\"a\":[\"  <&>\",\"\\\"\\\\\\n\\t\\u0001é\"],\"b\":true,\"m\":null,\"z\":1.50}"
	if string(got) != want {
		t.Fatalf("canonical\n got %s\nwant %s", got, want)
	}
	for _, bad := range []string{`{} {}`, `{"messages":[]} ]`, `{"messages":[]}}`, `[] ]`} {
		if _, err := Decode([]byte(bad)); err == nil {
			t.Fatalf("%s read as one value", bad)
		}
	}
}

// TestAManifestWhoseHistoryDoesNotReadStillJoins. A damaged history key costs
// the history, never the manifest: the body still joins its call and
// rebuilds, and says nothing of its call's history.
func TestAManifestWhoseHistoryDoesNotReadStillJoins(t *testing.T) {
	s := NewSession()
	rec, err := s.Encode(Body{ID: "a.response", Role: RoleResponse, Keys: Keys{Call: "msg_1"}, Bytes: []byte(`{"id":"msg_1"}`),
		History: &History{Form: FormClaudeCode, Reply: &Reply{Digest: "0123456789abcdef"}}})
	if err != nil {
		t.Fatal(err)
	}
	last := &rec.Parts[len(rec.Parts)-1]
	for _, damaged := range []string{`"reply":"x"`, `"thread":7`, `"system":[]`, `"Reply":"x"`, `"THREAD":7`} {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(last.Data, &fields)
		k, v, _ := strings.Cut(damaged, ":")
		fields[strings.Trim(k, `"`)] = json.RawMessage(v)
		data, _ := json.Marshal(fields)
		copied := *rec
		copied.Parts = append([]sessiondata.Part(nil), rec.Parts...)
		copied.Parts[len(copied.Parts)-1].Data = data
		m, err := ManifestOf(&copied)
		if err != nil || m.Call != "msg_1" || m.HistoryForm != "" || m.Reply != nil {
			t.Fatalf("%s: %+v, %v", damaged, m, err)
		}
	}
	// A list that does not read costs only the list.
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(last.Data, &fields)
	fields["MESSAGES"] = json.RawMessage(`3`)
	data, _ := json.Marshal(fields)
	copied := *rec
	copied.Parts = append([]sessiondata.Part(nil), rec.Parts...)
	copied.Parts[len(copied.Parts)-1].Data = data
	m, err := ManifestOf(&copied)
	if err != nil || m.HistoryForm != FormClaudeCode || m.Reply == nil || m.Messages == nil || m.Messages.Unavailable != UnrecognisedShape {
		t.Fatalf("a list that does not read: %+v, %v", m, err)
	}
}
