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
	"io/fs"
	"sort"
	"unicode/utf8"

	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// ErrRepeat reports a body the session already holds with the same bytes: a
// file landed again after an interrupted pass.
var ErrRepeat = errors.New("providerbody: the session already holds this body")

// ErrTooLarge reports a body that cannot be rebuilt because a piece of it was
// too large to land: its record, or an earlier one it refers to, holds the
// piece's size and digest and not its bytes.
var ErrTooLarge = errors.New("providerbody: a piece of the body was too large to land")

// ErrRemoved reports a body that cannot be rebuilt because retention removed
// a body or a piece it is built from, and the session's store does not keep
// those bytes.
var ErrRemoved = errors.New("providerbody: a body or piece it is built from was removed")

// Store keeps the bytes a removal took out of a session's landed files that
// later bodies may still be built from: the chain tips at the removal, and
// the removed pieces a later body cites. A tip is found by the id of its
// record, never by its bytes alone: an identical body under another id may
// be one the removal could not keep. A piece is found by its SHA-256. A
// missing one is an error that errors.Is fs.ErrNotExist.
type Store interface {
	Tip(id, sha256 string) ([]byte, error)
	Piece(sha256 string) ([]byte, error)
}

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
	// maxRecord is the largest line a record Encode returns may take in a
	// landed file, its newline included; zero means no limit. See Budget.
	maxRecord int64

	records map[string]*held
	pieces  map[string]pieceAt
	// tips holds the latest request body of each chain, by chain, and
	// tipOrder the chains from the one updated longest ago.
	tips     map[string]string
	tipOrder []string

	cache      map[string][]byte
	cacheOrder []string
	cacheBytes int

	// store keeps the bytes of removed tips and pieces. See Use.
	store Store
	// keptBy names, for each removed piece a landed record cites, the first
	// such record: the piece was kept in the store before that record
	// landed, so later records need not keep it again.
	keptBy map[string]string
}

type held struct {
	rec *sessiondata.Record
	m   *Manifest
	// removed says rec is a skeleton: the record keeps its place and its
	// pieces' digests, and none of its bytes.
	removed bool
}

type pieceAt struct {
	id   string
	part int
}

// NewSession returns an empty session.
func NewSession() *Session {
	return &Session{records: map[string]*held{}, pieces: map[string]pieceAt{}, tips: map[string]string{},
		cache: map[string][]byte{}, keptBy: map[string]string{}}
}

// Budget makes Encode fit every record into a landed file of budget bytes on
// its own, the file's header and closing line included. A record over it has
// its largest pieces taken out until it fits. Zero or less means no limit; a
// budget no record can fit still takes out every piece it can.
func (s *Session) Budget(budget int64) {
	s.maxRecord = 0
	if budget > 0 {
		s.maxRecord = max(budget-FileOverhead, 1)
	}
}

// Use gives the session the store that keeps the bytes of its removed tips
// and pieces.
func (s *Session) Use(store Store) { s.store = store }

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
//
// The record may be the skeleton retention left in a removed record's place.
// A skeleton keeps the manifest with every segment but the literal strings,
// and the digest of every piece, and none of the bytes. It is checked as the
// record was, and takes the record's place: its identity, digest, size and
// depth, its pieces and its place among the chain tips. So a body cut after a
// removal is cut as it would be with nothing removed, and the bytes of a
// removed tip or piece come from the store.
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
	removed := rec.Parts[own].State == model.ContentRemoved
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
		// record that says so and does not is damage. One that cannot be
		// rebuilt for a piece too large to land, or for one retention
		// removed, is still a repeat, and so is a skeleton, which holds no
		// bytes to rebuild from.
		if !removed {
			if _, err := s.rebuild(rec, m, nil); err != nil && !errors.Is(err, ErrTooLarge) && !errors.Is(err, ErrRemoved) {
				return err
			}
		}
		return ErrRepeat
	}
	s.records[rec.ID] = &held{rec: rec, m: m, removed: removed}
	for i, p := range rec.Parts[:own] {
		if p.Kind != sessiondata.PartData {
			continue
		}
		// A piece taken out, or removed, is still a piece: a later body that
		// holds it refers to it rather than taking it out again. Its digest
		// is the one the part keeps.
		d := p.SHA256
		if p.State != model.ContentTooLarge && p.State != model.ContentRemoved {
			d = digest(p.Data)
		}
		if d != "" && !hasPiece(s.pieces, d) {
			s.pieces[d] = pieceAt{id: rec.ID, part: i}
		}
	}
	if !removed {
		for _, seg := range m.Segments {
			if seg.Piece != "" && s.removedPiece(seg.Piece) && s.keptBy[seg.Piece] == "" {
				s.keptBy[seg.Piece] = rec.ID
			}
		}
	}
	if m.Role == RoleRequest && m.Chain != "" {
		s.setTip(m.Chain, rec.ID)
	}
	return nil
}

func hasPiece(m map[string]pieceAt, d string) bool { _, ok := m[d]; return ok }

// removedPiece says the piece with digest d is held by a part retention
// removed.
func (s *Session) removedPiece(d string) bool {
	at, ok := s.pieces[d]
	return ok && s.records[at.id].rec.Parts[at.part].State == model.ContentRemoved
}

// Tips names the latest request body of each chain the session holds, from
// the chain updated longest ago: the bodies a new request is compared with.
func (s *Session) Tips() []string {
	out := make([]string, 0, len(s.tipOrder))
	for _, c := range s.tipOrder {
		out = append(out, s.tips[c])
	}
	return out
}

// RemovedPieces gives the bytes of every removed piece rec cites that no
// landed record before it cites, read from body, the body rec was cut from.
// The store must keep them before rec lands, or nothing could rebuild rec
// later. A piece an earlier landed record cites was kept before that record
// landed.
func (s *Session) RemovedPieces(rec *sessiondata.Record, body []byte) (map[string][]byte, error) {
	m, err := ManifestOf(rec)
	if err != nil {
		return nil, err
	}
	var want []string
	for _, seg := range m.Segments {
		if seg.Piece == "" || !s.removedPiece(seg.Piece) {
			continue
		}
		if by := s.keptBy[seg.Piece]; by != "" && by != rec.ID {
			continue
		}
		want = append(want, seg.Piece)
	}
	if len(want) == 0 {
		return nil, nil
	}
	spans, _, err := pieces(body)
	if err != nil {
		return nil, err
	}
	have := map[string][]byte{}
	for _, sp := range spans {
		piece := body[sp.a:sp.z]
		have[digest(piece)] = piece
	}
	out := make(map[string][]byte, len(want))
	for _, d := range want {
		b, ok := have[d]
		if !ok {
			return nil, fmt.Errorf("providerbody: %s cites piece %s, which its body does not hold", rec.ID, d)
		}
		out[d] = b
	}
	return out, nil
}

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

// BodyFrom rebuilds the body held under id, as Body does, taking the bytes of
// any piece that was too large to land from source: another copy of the
// body, such as the file the runtime wrote. A piece is matched by its
// digest, so a source that is not the same body gives nothing it can use.
func (s *Session) BodyFrom(id string, source []byte) ([]byte, error) {
	h, ok := s.records[id]
	if !ok {
		return nil, fmt.Errorf("providerbody: no body %s", id)
	}
	fill := map[string][]byte{digest(source): source}
	if spans, _, err := pieces(source); err == nil {
		for _, sp := range spans {
			piece := source[sp.a:sp.z]
			fill[digest(piece)] = piece
		}
	}
	b, err := s.rebuild(h.rec, h.m, fill)
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
	var b []byte
	var err error
	if h.removed {
		b, err = s.removedBody(h)
	} else {
		b, err = s.rebuild(h.rec, h.m, nil)
	}
	if err != nil {
		return nil, err
	}
	s.remember(id, b)
	return b, nil
}

// removedBody reads a removed body from the store. One the store does not
// keep that holds or cites a piece too large to land fails as it did before
// its removal, with ErrTooLarge: it could not be rebuilt then, so no removal
// kept it. A copy needs no check, since a body is only copied from one that
// rebuilds.
func (s *Session) removedBody(h *held) ([]byte, error) {
	b, err := s.stored(func(sha string) ([]byte, error) { return s.store.Tip(h.rec.ID, sha) }, h.m.SHA256, "body "+h.rec.ID)
	if !errors.Is(err, ErrRemoved) {
		return b, err
	}
	for _, seg := range h.m.Segments {
		var p *sessiondata.Part
		switch {
		case seg.Part != nil && *seg.Part >= 0 && *seg.Part < len(h.rec.Parts)-1:
			p = &h.rec.Parts[*seg.Part]
		case seg.Piece != "":
			if at, ok := s.pieces[seg.Piece]; ok {
				p = &s.records[at.id].rec.Parts[at.part]
			}
		}
		if p != nil && p.State == model.ContentTooLarge {
			return nil, fmt.Errorf("%w: %d bytes with digest %s", ErrTooLarge, p.Bytes, p.SHA256)
		}
	}
	return nil, err
}

// stored reads bytes a removal kept from the store, and checks they are what
// their digest says.
func (s *Session) stored(get func(string) ([]byte, error), sha, what string) ([]byte, error) {
	if s.store == nil {
		return nil, fmt.Errorf("%w: %s, and the session has no store", ErrRemoved, what)
	}
	b, err := get(sha)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s is not kept", ErrRemoved, what)
	}
	if err != nil {
		return nil, err
	}
	if digest(b) != sha {
		return nil, fmt.Errorf("providerbody: the stored bytes of %s do not match their digest", what)
	}
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
// record can be checked before it lands. fill holds, by digest, the bytes of
// pieces that were too large to land, when the caller has them: the body it
// is encoding. Without them such a piece fails with ErrTooLarge.
func (s *Session) rebuild(rec *sessiondata.Record, m *Manifest, fill map[string][]byte) ([]byte, error) {
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
			b, err := s.partBytes(rec.Parts[*seg.Part], fill)
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
			b, err := s.partBytes(s.records[at.id].rec.Parts[at.part], fill)
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

// partBytes gives the bytes a part holds of a body. A piece too large to land
// comes from fill, the body being encoded, when the caller has it. A removed
// piece comes from fill too, and otherwise from the store, which kept it from
// the first later body that cited it.
func (s *Session) partBytes(p sessiondata.Part, fill map[string][]byte) ([]byte, error) {
	switch {
	case p.State == model.ContentTooLarge:
		if b, ok := fill[p.SHA256]; ok {
			return b, nil
		}
		return nil, fmt.Errorf("%w: %d bytes with digest %s", ErrTooLarge, p.Bytes, p.SHA256)
	case p.State == model.ContentRemoved:
		if p.SHA256 == "" {
			return nil, fmt.Errorf("%w: a part with no digest", ErrRemoved)
		}
		if b, ok := fill[p.SHA256]; ok {
			return b, nil
		}
		return s.stored(func(sha string) ([]byte, error) { return s.store.Piece(sha) }, p.SHA256, "piece "+p.SHA256)
	case p.Kind == sessiondata.PartUnknown:
		return p.Raw()
	}
	return p.Data, nil
}

// Encode cuts a body against what the session holds, checks that the record
// rebuilds the body after a round trip through the Session Data writer and
// reader, and adds it. A body that cannot be cut, or does not come back whole,
// lands whole in one unknown part, whose text says why. A body the session
// already holds returns ErrRepeat.
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
		Model: k.Model, Session: k.Session, Call: k.Call, Request: k.Request, PreviousRequest: k.PreviousRequest,
	}
	rec := &sessiondata.Record{Ord: 1, Sha: sum[:12], Bytes: len(body), ID: b.ID, Model: k.Model}
	switch b.Role {
	case RoleRequest:
		// A request names the call it was sent to when its adapter knows
		// it. One that does not is joined by its run and the request
		// before it.
		rec.Run, rec.Call = k.Run, k.Call
	case RoleResponse:
		rec.Call = k.Call
	}

	// The bytes of every piece of this body, by digest, so the round trip
	// can check a record whose pieces did not fit.
	fill := map[string][]byte{sum: body}
	parts, why := s.cut(body, m, fill)
	if why == "" {
		rec.Parts = append(parts, manifestPart(m))
		s.fit(rec, fill)
		// What is left after the pieces go is the manifest, and a body of
		// many short strings keeps nearly all of itself there. Whole, the
		// body is one part, which can be taken out.
		if s.maxRecord > 0 && RecordBytes(rec) > s.maxRecord {
			why = "the record is larger than the budget with every piece taken out"
		} else if err := s.roundTrip(rec, fill); err != nil {
			why = err.Error()
		}
	}
	if why != "" {
		whole := sessiondata.Part{Kind: sessiondata.PartUnknown, Text: why, State: "available", Bytes: len(body)}
		whole.SetRaw(body)
		zero := 0
		// Why the body was not cut is the whole part's text; the manifest
		// does not repeat it.
		m.Depth, m.Segments = 0, []Segment{{Part: &zero}}
		rec.Parts = []sessiondata.Part{whole, manifestPart(m)}
		s.fit(rec, fill)
		if err := s.roundTrip(rec, fill); err != nil {
			return nil, err
		}
	}
	if err := s.Add(rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// cut builds the parts and segments of a body, or says why it cannot. It
// adds every piece of the body to fill.
func (s *Session) cut(body []byte, m *Manifest, fill map[string][]byte) ([]sessiondata.Part, string) {
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
		fill[d] = piece
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

// fit takes out the largest parts a record holds of its body, never its
// manifest, until the record is a line of at most the budget's bytes. A record
// of one body is cut by leaving out what earlier records hold, so a body whose
// new pieces are larger than the budget is the one case left: the first
// request after a person pasted a screenshot, measured at 4.6 MB. A part taken
// out keeps its place and its size, and its sha256 is the piece's digest, so
// every segment still names it and later bodies refer to it as to any piece.
// The body then cannot be rebuilt, and a reader is told why with ErrTooLarge.
//
// A part whose data nests deeper than a landed line may hold is taken out the
// same way first, whatever the budget: a reader may refuse the line otherwise.
func (s *Session) fit(rec *sessiondata.Record, fill map[string][]byte) {
	own := len(rec.Parts) - 1
	for i := range rec.Parts[:own] {
		if len(rec.Parts[i].Data) > 0 && sessiondata.Depth(rec.Parts[i].Data) > sessiondata.MaxDataDepth {
			s.takeOut(&rec.Parts[i], fill)
		}
	}
	if s.maxRecord <= 0 || RecordBytes(rec) <= s.maxRecord {
		return
	}
	order := make([]int, 0, own)
	for i := range rec.Parts[:own] {
		if len(rec.Parts[i].Data) > 0 {
			order = append(order, i)
		}
	}
	sort.SliceStable(order, func(a, b int) bool { return len(rec.Parts[order[a]].Data) > len(rec.Parts[order[b]].Data) })
	for _, i := range order {
		if s.takeOut(&rec.Parts[i], fill) && RecordBytes(rec) <= s.maxRecord {
			return
		}
	}
}

// takeOut takes a part's bytes out of a record, keeping its place, its size
// and the digest of the piece, and adds the bytes to fill. It reports false
// for a part whose bytes it cannot read.
func (s *Session) takeOut(p *sessiondata.Part, fill map[string][]byte) bool {
	b, err := s.partBytes(*p, fill)
	if err != nil {
		return false
	}
	d := digest(b)
	fill[d] = b
	p.Data, p.Encoding = nil, ""
	p.State, p.SHA256, p.Bytes = model.ContentTooLarge, d, len(b)
	return true
}

// roundTrip writes the record with the Session Data writer, reads it back,
// and rebuilds the body from what was read. Only that proves the bytes a
// reader will get, since the writer compacts every data part. fill gives the
// bytes of the pieces that were too large to land, so a record that omits
// them is still proven to rebuild the body once they are given.
func (s *Session) roundTrip(rec *sessiondata.Record, fill map[string][]byte) error {
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
	if _, err := s.rebuild(back, bm, fill); err != nil {
		return fmt.Errorf("the record does not rebuild the body after a round trip: %w", err)
	}
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
