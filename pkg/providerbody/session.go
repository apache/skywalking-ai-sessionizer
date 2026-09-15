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
	"unicode/utf8"

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// ErrRepeat reports a body the session already holds with the same bytes: a
// file landed again after an interrupted pass.
var ErrRepeat = errors.New("providerbody: the session already holds this body")

// maxTips bounds how many chain tips a new body is compared against. A tip is
// one per chain that is still growing: the main stream and each child running
// at the time.
const maxTips = 64

// otherTips is how many tips of other chains a body is also compared with,
// for a chain whose first message changed shape between two calls.
const otherTips = 8

// cacheLimit bounds the rebuilt bodies kept in memory.
const cacheLimit = 64 << 20

// Session holds the provider_body records of one session, in landing order,
// and rebuilds any of them. Records must be added in the order they landed,
// because a record may refer only to what came before it.
type Session struct {
	records map[string]*held
	pieces  map[string]pieceAt
	// tips holds the latest request body of each chain, by chain, and
	// tipOrder the chains from the one updated longest ago.
	tips     map[string]string
	tipOrder []string

	cache      map[string][]byte
	cacheOrder []string
	cacheBytes int
}

type held struct {
	rec *sessiondata.Record
	m   *Manifest
}

type pieceAt struct {
	id   string
	part int
}

// NewSession returns an empty session.
func NewSession() *Session {
	return &Session{records: map[string]*held{}, pieces: map[string]pieceAt{}, tips: map[string]string{}, cache: map[string][]byte{}}
}

// ManifestOf returns the manifest a provider_body record carries as its last part.
func ManifestOf(rec *sessiondata.Record) (*Manifest, error) {
	if len(rec.Parts) == 0 {
		return nil, errors.New("providerbody: the record has no parts")
	}
	last := rec.Parts[len(rec.Parts)-1]
	var m Manifest
	if last.Kind != sessiondata.PartData || json.Unmarshal(last.Data, &m) != nil || m.Schema != Schema {
		return nil, fmt.Errorf("providerbody: the last part of %s is not a %s manifest", rec.ID, Schema)
	}
	return &m, nil
}

// Holds reports the SHA-256 of the body the session holds under id.
func (s *Session) Holds(id string) (string, bool) {
	h, ok := s.records[id]
	if !ok {
		return "", false
	}
	return h.m.SHA256, true
}

// Manifest returns the manifest of the body held under id.
func (s *Session) Manifest(id string) (*Manifest, bool) {
	h, ok := s.records[id]
	if !ok {
		return nil, false
	}
	return h.m, true
}

// Add takes a record that landed after every record added before it. It
// checks that each reference points at something the session already holds,
// and makes the record's pieces and body available to later records. A
// record whose id the session holds with the same digest returns ErrRepeat.
func (s *Session) Add(rec *sessiondata.Record) error {
	m, err := ManifestOf(rec)
	if err != nil {
		return err
	}
	if rec.ID == "" {
		return errors.New("providerbody: a record without an id")
	}
	if m.Bytes < 0 || m.Bytes > MaxBytes || m.Depth < 0 || m.Depth > MaxDepth {
		return fmt.Errorf("providerbody: %s claims %d bytes at depth %d", rec.ID, m.Bytes, m.Depth)
	}
	// References are checked first, so a record landed again is held to the
	// same rules as the first copy before it may count as a repeat.
	own := len(rec.Parts) - 1
	copies := 0
	for _, seg := range m.Segments {
		switch {
		case seg.Part != nil:
			if *seg.Part < 0 || *seg.Part >= own {
				return fmt.Errorf("providerbody: %s names part %d of %d", rec.ID, *seg.Part, own)
			}
		case seg.Piece != "":
			if _, ok := s.pieces[seg.Piece]; !ok {
				return fmt.Errorf("providerbody: %s names piece %s, which no earlier record holds", rec.ID, seg.Piece)
			}
		case seg.Copy != nil:
			copies++
			base, ok := s.records[seg.Copy.From]
			switch {
			case !ok:
				return fmt.Errorf("providerbody: %s copies from %s, which no earlier record holds", rec.ID, seg.Copy.From)
			case base.m.SHA256 != seg.Copy.SHA256 || seg.Copy.Len < 0 || seg.Copy.Len > base.m.Bytes:
				return fmt.Errorf("providerbody: %s copies from %s with a digest or length it does not have", rec.ID, seg.Copy.From)
			case m.Depth != base.m.Depth+1 || m.Depth > MaxDepth:
				return fmt.Errorf("providerbody: %s claims depth %d over a base of depth %d", rec.ID, m.Depth, base.m.Depth)
			}
		}
	}
	if copies == 0 && m.Depth != 0 {
		return fmt.Errorf("providerbody: %s claims depth %d and copies nothing", rec.ID, m.Depth)
	}
	if h, ok := s.records[rec.ID]; ok {
		if h.m.SHA256 != m.SHA256 {
			return fmt.Errorf("providerbody: %s is held with digest %s and landed again with %s", rec.ID, h.m.SHA256, m.SHA256)
		}
		// A repeat is a repeat only if it rebuilds to what it claims; a
		// record that says so and does not is damage.
		if _, err := s.rebuild(rec, m); err != nil {
			return err
		}
		return ErrRepeat
	}
	s.records[rec.ID] = &held{rec: rec, m: m}
	for i, p := range rec.Parts[:own] {
		if p.Kind != sessiondata.PartData {
			continue
		}
		if d := digest(p.Data); !hasPiece(s.pieces, d) {
			s.pieces[d] = pieceAt{id: rec.ID, part: i}
		}
	}
	if m.Role == RoleRequest && m.Chain != "" {
		s.setTip(m.Chain, rec.ID)
	}
	return nil
}

func hasPiece(m map[string]pieceAt, d string) bool { _, ok := m[d]; return ok }

// setTip makes id the latest body of its chain, and forgets the chain
// updated longest ago once more than maxTips are held.
func (s *Session) setTip(chain, id string) {
	if _, ok := s.tips[chain]; ok {
		for i, c := range s.tipOrder {
			if c == chain {
				s.tipOrder = append(s.tipOrder[:i], s.tipOrder[i+1:]...)
				break
			}
		}
	}
	s.tips[chain] = id
	s.tipOrder = append(s.tipOrder, chain)
	if len(s.tipOrder) > maxTips {
		delete(s.tips, s.tipOrder[0])
		s.tipOrder = s.tipOrder[1:]
	}
}

// Body rebuilds the body held under id and checks its digest. The bytes
// are the caller's own.
func (s *Session) Body(id string) ([]byte, error) {
	b, err := s.body(id)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), b...), nil
}

// body is Body without the copy, for this package only: a later body's copy
// must read the base as it was rebuilt, and nothing here changes it.
func (s *Session) body(id string) ([]byte, error) {
	if b, ok := s.cache[id]; ok {
		return b, nil
	}
	h, ok := s.records[id]
	if !ok {
		return nil, fmt.Errorf("providerbody: no body %s", id)
	}
	b, err := s.rebuild(h.rec, h.m)
	if err != nil {
		return nil, err
	}
	s.remember(id, b)
	return b, nil
}

func (s *Session) remember(id string, b []byte) {
	s.cache[id] = b
	s.cacheOrder = append(s.cacheOrder, id)
	s.cacheBytes += len(b)
	for s.cacheBytes > cacheLimit && len(s.cacheOrder) > 1 {
		old := s.cacheOrder[0]
		s.cacheOrder = s.cacheOrder[1:]
		s.cacheBytes -= len(s.cache[old])
		delete(s.cache, old)
	}
}

// rebuild joins a record's segments. The record need not be added yet, so a
// record can be checked before it lands.
func (s *Session) rebuild(rec *sessiondata.Record, m *Manifest) ([]byte, error) {
	if m.Bytes < 0 || m.Bytes > MaxBytes {
		return nil, fmt.Errorf("providerbody: %s claims %d bytes", rec.ID, m.Bytes)
	}
	over := func(int) error {
		return fmt.Errorf("providerbody: %s rebuilds past the %d bytes it claims", rec.ID, m.Bytes)
	}
	// The claimed size is only a claim until the digest agrees, so it sets
	// no more than a first capacity.
	out := make([]byte, 0, min(m.Bytes, 1<<20))
	for _, seg := range m.Segments {
		switch {
		case seg.Lit != "":
			if len(out)+len(seg.Lit) > m.Bytes {
				return nil, over(len(seg.Lit))
			}
			out = append(out, seg.Lit...)
		case seg.Part != nil:
			if *seg.Part < 0 || *seg.Part >= len(rec.Parts)-1 {
				return nil, fmt.Errorf("providerbody: %s names part %d it does not have", rec.ID, *seg.Part)
			}
			b, err := partBytes(rec.Parts[*seg.Part])
			if err != nil {
				return nil, err
			}
			if len(out)+len(b) > m.Bytes {
				return nil, over(len(b))
			}
			out = append(out, b...)
		case seg.Piece != "":
			at, ok := s.pieces[seg.Piece]
			if !ok {
				return nil, fmt.Errorf("providerbody: %s names piece %s, which no earlier record holds", rec.ID, seg.Piece)
			}
			b, err := partBytes(s.records[at.id].rec.Parts[at.part])
			if err != nil {
				return nil, err
			}
			if len(out)+len(b) > m.Bytes {
				return nil, over(len(b))
			}
			out = append(out, b...)
		case seg.Copy != nil:
			base, err := s.body(seg.Copy.From)
			if err != nil {
				return nil, err
			}
			if seg.Copy.Len < 0 || seg.Copy.Len > len(base) {
				return nil, fmt.Errorf("providerbody: %s copies %d bytes of a %d byte body", rec.ID, seg.Copy.Len, len(base))
			}
			if len(out)+seg.Copy.Len > m.Bytes {
				return nil, over(seg.Copy.Len)
			}
			out = append(out, base[:seg.Copy.Len]...)
		}
	}
	if len(out) != m.Bytes || digest(out) != m.SHA256 {
		return nil, fmt.Errorf("providerbody: %s rebuilds to %d bytes that do not match its digest", rec.ID, len(out))
	}
	return out, nil
}

func partBytes(p sessiondata.Part) ([]byte, error) {
	if p.Kind == sessiondata.PartUnknown {
		return p.Raw()
	}
	return p.Data, nil
}

// Encode cuts a body against what the session holds, checks that the record
// rebuilds the body after a round trip through the Session Data writer and
// reader, and adds it. A body that cannot be cut, or does not come back whole,
// lands whole in one unknown part, and the manifest says why. A body the
// session already holds returns ErrRepeat.
func (s *Session) Encode(b Body) (*sessiondata.Record, error) {
	body := b.Bytes
	sum := digest(body)
	if h, ok := s.records[b.ID]; ok {
		if h.m.SHA256 == sum {
			return nil, ErrRepeat
		}
		return nil, fmt.Errorf("providerbody: %s is held with digest %s and read again with %s", b.ID, h.m.SHA256, sum)
	}
	k := b.Keys
	m := &Manifest{
		Schema: Schema, Role: b.Role, Src: b.Src, SHA256: sum, Bytes: len(body),
		Model: k.Model, Session: k.Session, Run: k.Run, Call: k.Call, Request: k.Request, PreviousRequest: k.PreviousRequest,
	}
	rec := &sessiondata.Record{Ord: 1, Sha: sum[:12], Bytes: len(body), ID: b.ID, Model: k.Model}
	switch b.Role {
	case RoleRequest:
		rec.Run = k.Run
	case RoleResponse:
		rec.Call = k.Call
	}

	parts, why := s.cut(body, m)
	if why == "" {
		rec.Parts = append(parts, manifestPart(m))
		if err := s.roundTrip(rec, m); err != nil {
			why = err.Error()
		}
	}
	if why != "" {
		whole := sessiondata.Part{Kind: sessiondata.PartUnknown, Text: why, State: "available", Bytes: len(body)}
		whole.SetRaw(body)
		zero := 0
		m.Depth, m.Why, m.Segments = 0, why, []Segment{{Part: &zero}}
		rec.Parts = []sessiondata.Part{whole, manifestPart(m)}
		if err := s.roundTrip(rec, m); err != nil {
			return nil, err
		}
	}
	if err := s.Add(rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// cut builds the parts and segments of a body, or says why it cannot.
func (s *Session) cut(body []byte, m *Manifest) ([]sessiondata.Part, string) {
	if !utf8.Valid(body) {
		return nil, "the body is not valid UTF-8"
	}
	if !json.Valid(body) {
		return nil, "the body is not valid JSON"
	}
	spans, first, err := pieces(body)
	if err != nil {
		return nil, err.Error()
	}

	pos := 0
	if m.Role == RoleRequest && first.z > first.a {
		m.Chain = chainOf(body[first.a:first.z])
		if c := s.base(body, spans, m.Chain); c != nil {
			m.Segments = append(m.Segments, Segment{Copy: c})
			m.Depth = s.records[c.From].m.Depth + 1
			pos = c.Len
		}
	}
	var parts []sessiondata.Part
	local := map[string]int{}
	for _, sp := range spans {
		if sp.a < pos {
			continue
		}
		if sp.a > pos {
			m.Segments = append(m.Segments, Segment{Lit: string(body[pos:sp.a])})
		}
		piece := body[sp.a:sp.z]
		d := digest(piece)
		switch i, ok := local[d]; {
		case ok:
			m.Segments = append(m.Segments, Segment{Part: &i})
		case hasPiece(s.pieces, d):
			m.Segments = append(m.Segments, Segment{Piece: d})
		default:
			i := len(parts)
			local[d] = i
			parts = append(parts, sessiondata.Part{
				Kind: sessiondata.PartData, Data: json.RawMessage(append([]byte(nil), piece...)),
				State: "available", Bytes: len(piece),
			})
			m.Segments = append(m.Segments, Segment{Part: &i})
		}
		pos = sp.z
	}
	if pos < len(body) {
		m.Segments = append(m.Segments, Segment{Lit: string(body[pos:])})
	}
	return parts, ""
}

// base picks the body to copy the front of: the tip of the body's own chain,
// and the most recently updated tips of other chains, whichever shares the
// longest front. It says how much to copy. The copy ends before any piece it
// would cut in two, and on a character boundary, since the rest continues
// as a literal string.
func (s *Session) base(body []byte, spans []span, chain string) *Copy {
	var candidates []string
	if id, ok := s.tips[chain]; ok {
		candidates = append(candidates, id)
	}
	for i := len(s.tipOrder) - 1; i >= 0 && len(candidates) < otherTips+1; i-- {
		if c := s.tipOrder[i]; c != chain {
			candidates = append(candidates, s.tips[c])
		}
	}
	var best *Copy
	for _, id := range candidates {
		h := s.records[id]
		if h.m.Depth+1 > MaxDepth {
			continue
		}
		b, err := s.body(id)
		if err != nil {
			continue
		}
		n := commonPrefix(b, body)
		if best == nil || n > best.Len {
			best = &Copy{From: id, SHA256: h.m.SHA256, Len: n}
		}
	}
	if best == nil {
		return nil
	}
	for _, sp := range spans {
		if sp.z > best.Len {
			if sp.a < best.Len {
				best.Len = sp.a
			}
			break
		}
	}
	for best.Len > 0 && best.Len < len(body) && !utf8.RuneStart(body[best.Len]) {
		best.Len--
	}
	if best.Len < MinCopy {
		return nil
	}
	return best
}

// chainOf names the chain a request belongs to by its first message. That
// message is the same on every call of a chain, apart from the cache marker,
// which moves to the newest message, and a one-block text list becoming a
// plain string once the marker has left it. Both are taken out first. A
// compaction starts a new first message, and so a new chain.
func chainOf(first []byte) string {
	var msg map[string]any
	if json.Unmarshal(first, &msg) != nil {
		return digest(first)[:16]
	}
	if blocks, ok := msg["content"].([]any); ok {
		for _, b := range blocks {
			if block, ok := b.(map[string]any); ok {
				delete(block, "cache_control")
			}
		}
		if len(blocks) == 1 {
			if block, ok := blocks[0].(map[string]any); ok && block["type"] == "text" && len(block) == 2 {
				msg["content"] = block["text"]
			}
		}
	}
	b, _ := json.Marshal(msg)
	return digest(b)[:16]
}

func commonPrefix(a, b []byte) int {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

func manifestPart(m *Manifest) sessiondata.Part {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(m)
	data := bytes.TrimSuffix(buf.Bytes(), []byte{'\n'})
	return sessiondata.Part{Kind: sessiondata.PartData, Data: data, State: "available", Bytes: len(data)}
}

// roundTrip writes the record with the Session Data writer, reads it back,
// and rebuilds the body from what was read. Only that proves the bytes a
// reader will get, since the writer compacts every data part.
func (s *Session) roundTrip(rec *sessiondata.Record, m *Manifest) error {
	var buf bytes.Buffer
	hdr := &sessiondata.Header{Kind: sessiondata.KindProviderBody, Session: "check", Src: ".", Dialect: "check"}
	w, err := sessiondata.NewWriter(&buf, hdr)
	if err != nil {
		return err
	}
	if err := w.Write(rec); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	r, err := sessiondata.NewReader(&buf)
	if err != nil {
		return err
	}
	back, err := r.Next()
	if err != nil {
		return err
	}
	bm, err := ManifestOf(back)
	if err != nil {
		return err
	}
	if _, err := s.rebuild(back, bm); err != nil {
		return fmt.Errorf("the record does not rebuild the body after a round trip: %w", err)
	}
	_ = m
	return nil
}

// RecordBytes is the size a record takes as a line of a landed file, its
// newline included. A writer cutting files at a byte budget adds these up.
func RecordBytes(rec *sessiondata.Record) int64 {
	var buf bytes.Buffer
	w, err := sessiondata.NewWriter(&buf, &sessiondata.Header{Kind: sessiondata.KindProviderBody, Session: "s", Src: ".", Dialect: "d"})
	if err != nil {
		return 0
	}
	start := int64(buf.Len())
	_ = w.Write(rec)
	_ = w.Close()
	// The closing line is measured with the record and taken off with it.
	end := int64(buf.Len()) - int64(len(`{"t":"end","records":1,"digest":""}`)+64+1)
	return end - start
}

// FileOverhead is what a landed provider_body file holds besides its
// records, at most: its header and its closing line.
const FileOverhead = 512
