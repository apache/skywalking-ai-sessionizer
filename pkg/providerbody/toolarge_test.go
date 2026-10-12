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
	"errors"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

const testBudget = 16 << 10

// landed writes a record as a collector does and reads it back, so a test
// checks what a reader gets.
func landed(t *testing.T, rec *sessiondata.Record) *sessiondata.Record {
	t.Helper()
	var buf bytes.Buffer
	w, err := sessiondata.NewWriter(&buf, &sessiondata.Header{Kind: sessiondata.KindProviderBody, Session: "s1", Src: ".", Dialect: "d"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(rec); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if int64(buf.Len()) > testBudget {
		t.Fatalf("the landed file is %d bytes, over the budget of %d", buf.Len(), testBudget)
	}
	_, recs, err := sessiondata.All(&buf)
	if err != nil || len(recs) != 1 {
		t.Fatalf("read back %d records: %v", len(recs), err)
	}
	return recs[0]
}

// omitted returns the record's parts that were too large to land.
func omitted(rec *sessiondata.Record) []sessiondata.Part {
	var out []sessiondata.Part
	for _, p := range rec.Parts {
		if p.State == model.ContentTooLarge {
			out = append(out, p)
		}
	}
	return out
}

// pasted is a chain whose first message holds a 40 KB string, as a pasted
// screenshot's base64 or a long tool result is.
func pasted() (*chain, string) {
	c := newChain("s1", "p1", strings.Repeat("You are an agent. ", 30), 2)
	big := "look at this: " + strings.Repeat("iVBORw0KGgo", 40_000/11)
	c.add("user", big)
	return c, big
}

func TestABodyOverTheBudgetOmitsItsLargestPiece(t *testing.T) {
	s := NewSession()
	s.Budget(testBudget)
	c, big := pasted()
	req := c.request()
	rec, err := encode(s, "00000000-0000-4000-8000-000000000001.request.json", req)
	if err != nil {
		t.Fatal(err)
	}
	piece := compact(big)
	gone := omitted(rec)
	if len(gone) != 1 || gone[0].SHA256 != Digest(piece) || gone[0].Bytes != len(piece) || len(gone[0].Data) != 0 {
		t.Fatalf("omitted %d part(s), want the one 40 KB piece with its size and digest", len(gone))
	}
	if _, err := ManifestOf(rec); err != nil {
		t.Fatalf("the manifest did not survive: %v", err)
	}
	// The cutter cannot rebuild what it did not land, and says why; the file
	// the runtime wrote gives the piece back.
	if _, err := s.Body(rec.ID); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Body returned %v, want ErrTooLarge", err)
	}
	if got, err := s.BodyFrom(rec.ID, req); err != nil || !bytes.Equal(got, req) {
		t.Fatalf("BodyFrom did not give the body back: %v", err)
	}
	// A reader of the landed file sees the same.
	r := NewSession()
	back := landed(t, rec)
	if err := r.Add(back); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Body(back.ID); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("a reader's Body returned %v, want ErrTooLarge", err)
	}
	if got, err := r.BodyFrom(back.ID, req); err != nil || !bytes.Equal(got, req) {
		t.Fatalf("a reader's BodyFrom did not give the body back: %v", err)
	}
	// Landing the file again is a repeat, not damage.
	if err := r.Add(back); !errors.Is(err, ErrRepeat) {
		t.Fatalf("landed again: %v, want ErrRepeat", err)
	}
}

func TestALaterBodyNamesTheOmittedPiece(t *testing.T) {
	s := NewSession()
	s.Budget(testBudget)
	c, big := pasted()
	first, err := encode(s, "00000000-0000-4000-8000-000000000001.request.json", c.request())
	if err != nil {
		t.Fatal(err)
	}
	c.add("assistant", "I see a screenshot.")
	c.add("user", "what is in it?")
	req := c.request()
	rec, err := encode(s, "00000000-0000-4000-8000-000000000002.request.json", req)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := ManifestOf(rec)
	names := false
	for _, seg := range m.Segments {
		names = names || seg.Piece == Digest(compact(big))
		// The first body cannot be rebuilt, so nothing copies from it.
		if seg.Copy != nil && seg.Copy.From == first.ID {
			t.Fatalf("copies from %s, which cannot be rebuilt", first.ID)
		}
	}
	if !names || len(omitted(rec)) != 0 {
		t.Fatalf("the later body omits %d part(s) and names the piece %v; want it named, not omitted again", len(omitted(rec)), names)
	}
	if _, err := s.Body(rec.ID); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Body returned %v, want ErrTooLarge", err)
	}
	if got, err := s.BodyFrom(rec.ID, req); err != nil || !bytes.Equal(got, req) {
		t.Fatalf("BodyFrom did not give the body back: %v", err)
	}
	r := NewSession()
	for _, x := range []*sessiondata.Record{first, rec} {
		if err := r.Add(landed(t, x)); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := r.BodyFrom(rec.ID, req); err != nil || !bytes.Equal(got, req) {
		t.Fatalf("a reader's BodyFrom did not give the later body back: %v", err)
	}
}

func TestABodyLandedWholeIsOmittedWhole(t *testing.T) {
	s := NewSession()
	s.Budget(testBudget)
	raw := []byte(`{"model":"claude-opus-5","messages":[` + strings.Repeat(`"x",`, 10_000))
	rec, err := encode(s, "00000000-0000-4000-8000-000000000001.request.json", raw)
	if err != nil {
		t.Fatal(err)
	}
	whole := rec.Parts[0]
	if whole.Kind != sessiondata.PartUnknown || whole.State != model.ContentTooLarge || whole.SHA256 != Digest(raw) || whole.Text == "" {
		t.Fatalf("the whole part is %s %s with digest %q, want an unknown part omitted with the body's digest and its reason kept", whole.Kind, whole.State, whole.SHA256)
	}
	if got, err := s.BodyFrom(rec.ID, raw); err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("BodyFrom did not give the body back: %v", err)
	}
	landed(t, rec)
}

// Under a budget no record fits, every piece goes, the manifest stays, and
// the body lands whole in one part left out, so the manifest is one segment.
// A budget of a few bytes still limits: it does not mean no limit.
func TestTheManifestIsNeverOmitted(t *testing.T) {
	for _, budget := range []int64{FileOverhead + 1, 100} {
		s := NewSession()
		s.Budget(budget)
		c, _ := pasted()
		req := c.request()
		rec, err := encode(s, "00000000-0000-4000-8000-000000000001.request.json", req)
		if err != nil {
			t.Fatal(err)
		}
		m, err := ManifestOf(rec)
		if err != nil {
			t.Fatalf("budget %d: the manifest was omitted: %v", budget, err)
		}
		if whole := rec.Parts[0]; len(rec.Parts) != 2 || whole.Kind != sessiondata.PartUnknown || whole.State != model.ContentTooLarge || len(m.Segments) != 1 {
			t.Fatalf("budget %d: %d parts, the first %s %s, %d segments; want the whole body left out", budget, len(rec.Parts), whole.Kind, whole.State, len(m.Segments))
		}
		if got, err := s.BodyFrom(rec.ID, req); err != nil || !bytes.Equal(got, req) {
			t.Fatalf("budget %d: BodyFrom did not give the body back: %v", budget, err)
		}
	}
}

// A body of many short strings has no piece to leave out: they stay in the
// manifest as literals. Whole, it is one part, which can be.
func TestABodyOfShortStringsLandsWholeAndLeftOut(t *testing.T) {
	s := NewSession()
	s.Budget(testBudget)
	var b strings.Builder
	b.WriteString(`{"model":"claude-opus-5","messages":[`)
	for i := 0; i < 2_000; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"role":"user","content":"a short message of no length"}`)
	}
	b.WriteString(`]}`)
	req := []byte(b.String())
	rec, err := encode(s, "00000000-0000-4000-8000-000000000001.request.json", req)
	if err != nil {
		t.Fatal(err)
	}
	if RecordBytes(rec) > testBudget-FileOverhead {
		t.Fatalf("the record is %d bytes, over the budget", RecordBytes(rec))
	}
	if whole := rec.Parts[0]; whole.Kind != sessiondata.PartUnknown || whole.State != model.ContentTooLarge || whole.SHA256 != Digest(req) {
		t.Fatalf("the first part is %s %s, want the whole body left out with its digest", whole.Kind, whole.State)
	}
	if got, err := s.BodyFrom(rec.ID, req); err != nil || !bytes.Equal(got, req) {
		t.Fatalf("BodyFrom did not give the body back: %v", err)
	}
	landed(t, rec)
}

func TestWithoutABudgetNothingIsOmitted(t *testing.T) {
	s := NewSession()
	c, _ := pasted()
	req := c.request()
	rec, err := encode(s, "00000000-0000-4000-8000-000000000001.request.json", req)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(omitted(rec)); n != 0 {
		t.Fatalf("omitted %d part(s) with no budget", n)
	}
	if got, err := s.Body(rec.ID); err != nil || !bytes.Equal(got, req) {
		t.Fatalf("Body did not give the body back: %v", err)
	}
}
