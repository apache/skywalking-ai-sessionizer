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

package sessiondata

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/apache/skywalking-ai-sessionizer/pkg/changes"
	"github.com/apache/skywalking-ai-sessionizer/pkg/execution"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
)

// endLineMax is the longest closing line a file can have, its newline
// included: the record count takes at most twenty digits.
const endLineMax = len(`{"t":"end","records":,"digest":""}`) + 20 + 64 + 1

// Fit makes r a line of at most limit bytes by taking out its largest
// payloads, one at a time, and reports how many parts it changed. It changes
// r in place. A limit of zero or less fits everything. A part loses its data
// before its text, so a result keeps its readable text, saying truncated,
// while the runtime's structured copy of it goes.
//
// A landed file travels whole, so its size is what a receiver has to store
// at once. A file is cut by narrowing what goes into it, which works down to
// one record. One record can still be larger than the budget: a pasted
// screenshot measured 4.6 MB in a transcript line. So the record lands, and
// its largest payloads do not. A part taken out keeps its place, its kind and
// every identifier, so a reader still sees the step, and it says why its
// content is missing: its state is too_large, or truncated while its text is
// left, its bytes the size of the original, and its sha256 the digest of what
// was taken out first.
//
// A change record and an execution record are evidence a step joins to by
// its tool call, so neither is ever taken out whole: a reader that met
// nothing would read that the call changed no file or ran nowhere. A change
// record is made smaller in its own format instead: its largest files lose
// their hunks and say their diff is too_large, keeping their counts, and then
// its file list goes, keeping its identity and changed_files. Its part says
// truncated. An execution record keeps only sizes and digests, never content.
//
// A record that is still too large with every payload taken out lands as it
// is: its identifiers, label and part list remain.
//
// Data nested deeper than a line may hold, MaxDataDepth, is taken out the
// same way first, under any limit: a reader may refuse the line otherwise. A
// change or execution record is written again in its own format instead,
// which leaves out the fields its format does not name.
func Fit(r *Record, limit int) (int, error) {
	_, n, err := fit(r, limit, true)
	return n, err
}

// MaxLineDepth is the most objects and lists a landed line holds open at
// once. A reader may refuse a deeper line, and the OAP does, so no line is
// landed deeper: a part whose data nests deeper than its share of the line is
// taken out as a payload too large to carry is, whatever its size.
const MaxLineDepth = 256

// MaxDataDepth is a part's data's share of a line: the record, its parts
// and the part itself hold three of its levels.
const MaxDataDepth = MaxLineDepth - 3

// Depth is how many objects and lists data holds open at once at its
// deepest: 0 for a string, a number or a literal.
func Depth(data []byte) int {
	deepest, open, inString, escaped := 0, 0, false, false
	for _, c := range data {
		switch {
		case escaped:
			escaped = false
		case inString:
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
		case c == '"':
			inString = true
		case c == '[' || c == '{':
			open++
			deepest = max(deepest, open)
		case c == ']' || c == '}':
			open--
		}
	}
	return deepest
}

// level takes out the data of every part nested deeper than MaxDataDepth,
// in place, and adds the parts it changed to changed. It changes r only when
// the record then encodes, so a record that cannot be written stays as the
// caller gave it.
func level(r *Record, changed map[int]bool) error {
	parts := append([]Part(nil), r.Parts...)
	deep := map[int]bool{}
	for i := range parts {
		if len(parts[i].Data) > 0 && Depth(parts[i].Data) > MaxDataDepth {
			if !reencode(&parts[i]) {
				omit(&parts[i])
			}
			deep[i] = true
		}
	}
	if len(deep) == 0 {
		return nil
	}
	leveled := *r
	leveled.Parts = parts
	if _, err := encodeRecord(&leveled); err != nil {
		return err
	}
	copy(r.Parts, parts)
	for i := range deep {
		changed[i] = true
	}
	return nil
}

// reencode writes a change record or an execution record again in its own
// format, as its decoder reads it, so a field its format does not name, nested
// however deep, goes and the record stays: neither is ever taken out whole,
// since a step joins to it by its tool call. Its part says truncated, with the
// size and the digest of what it held. It reports false for any other data,
// and for a record its format cannot write shallow enough.
func reencode(p *Part) bool {
	if p.Kind != PartData {
		return false
	}
	var data []byte
	var err error
	if rec, ok := changes.Decode(p.Data); ok {
		data, err = rec.Marshal()
	} else if rec, ok := execution.Decode(p.Data); ok {
		data, err = rec.Marshal()
	} else {
		return false
	}
	if err != nil || Depth(data) > MaxDataDepth {
		return false
	}
	remember(p)
	p.Data, p.State = data, model.ContentTruncated
	return true
}

// fit is Fit, also returning the record's line, so a writer encodes it once.
// It takes out data nested too deep first, unless leveled is false: a
// provider body's cutter does that itself, knowing which of its parts later
// bodies refer to.
func fit(r *Record, limit int, leveled bool) ([]byte, int, error) {
	changed := map[int]bool{}
	if leveled {
		if err := level(r, changed); err != nil {
			return nil, 0, err
		}
	}
	line, err := encodeRecord(r)
	if err != nil || limit <= 0 {
		return line, len(changed), err
	}
	for len(line) > limit {
		i := largest(r.Parts)
		if i < 0 {
			break
		}
		if !shrink(&r.Parts[i]) {
			omit(&r.Parts[i])
		}
		changed[i] = true
		if line, err = encodeRecord(r); err != nil {
			break
		}
	}
	return line, len(changed), err
}

// largest is the part with the most payload Fit can take out, the earlier of
// two of the same size, so the same record always loses the same parts. It
// is -1 when nothing is left to take out.
func largest(parts []Part) int {
	best, size := -1, 0
	for i, p := range parts {
		if n := Payload(p); n > size {
			best, size = i, n
		}
	}
	return best
}

// Payload is how much Fit can take out of a part in its next step: its data
// while it has some, and then its text, unless it is an unknown part, whose
// text is the reason its bytes were not described rather than content. An
// execution record, and a change record with nothing left to shrink, have
// none, so a record whose parts all have none is as small as Fit makes it.
func Payload(p Part) int {
	if p.State == model.ContentTooLarge {
		return 0
	}
	if len(p.Data) > 0 {
		if p.Kind == PartData {
			if _, ok := execution.Decode(p.Data); ok {
				return 0
			}
			if rec, ok := changes.Decode(p.Data); ok && !shrinkable(rec) {
				return 0
			}
		}
		return len(p.Data)
	}
	if p.Kind == PartUnknown {
		return 0
	}
	return len(p.Text)
}

func shrinkable(rec *changes.Record) bool {
	return len(rec.Changes)+len(rec.Overlaps)+len(rec.Gaps) > 0
}

// shrink makes a change record smaller by one step: the hunks of its
// largest file, or, once no file has hunks, its file list. It reports false
// for any other part.
func shrink(p *Part) bool {
	if p.Kind != PartData {
		return false
	}
	rec, ok := changes.Decode(p.Data)
	if !ok || (!rec.DropHunks() && !rec.DropFiles()) {
		return false
	}
	data, err := rec.Marshal()
	if err != nil {
		return false
	}
	remember(p)
	p.Data, p.State = data, model.ContentTruncated
	return true
}

// omit takes one payload out of a part and says so: its data first, while it
// has some, so a result keeps its readable text and says truncated, and then
// its text. A part with nothing left says too_large.
func omit(p *Part) {
	remember(p)
	if len(p.Data) > 0 {
		p.Data, p.Encoding = nil, ""
	} else {
		p.Text = ""
	}
	p.State = model.ContentTooLarge
	if p.Text != "" && p.Kind != PartUnknown {
		p.State = model.ContentTruncated
	}
}

// remember keeps the size and the digest of what a part carried, before Fit
// first changes it: of its data when it had some, which goes first.
func remember(p *Part) {
	if p.SHA256 != "" {
		return
	}
	b := carried(p)
	sum := sha256.Sum256(b)
	p.SHA256 = hex.EncodeToString(sum[:])
	if p.Bytes == 0 {
		p.Bytes = len(b)
	}
}

// carried is what a part holds: an unknown part's bytes, other data as the
// writer lands it, compacted, or the text of a part with no data.
func carried(p *Part) []byte {
	if len(p.Data) == 0 {
		return []byte(p.Text)
	}
	if p.Kind == PartUnknown {
		if raw, err := p.Raw(); err == nil {
			return raw
		}
	}
	var compact bytes.Buffer
	if json.Compact(&compact, p.Data) != nil {
		return p.Data
	}
	return compact.Bytes()
}

// Cut decides which records go into one landed file of a byte budget. A
// collector reads a source by its own bytes, and a record is larger than the
// line it came from, by its envelope, so a file is cut by what its records
// take once encoded.
type Cut struct {
	// room is what the file has left for record lines, newlines included,
	// and limit what it had before the first.
	limit, room int
}

// NewCut starts a file with header h under a budget of bytes. A budget of
// zero or less takes every record.
func NewCut(h *Header, budget int64) (*Cut, error) {
	c := &Cut{}
	if budget <= 0 {
		return c, nil
	}
	hh := *h
	hh.H, hh.Schema = 1, Schema
	head, err := encodeLine(&hh)
	if err != nil {
		return nil, err
	}
	c.limit = max(int(budget)-len(head)-1-endLineMax, 1)
	c.room = c.limit
	return c, nil
}

// Take fits r as a Writer under the same budget does, and reports whether it
// goes into this file: the first record always does, a later one only while
// the file stays within the budget. A record not taken starts the next file.
func (c *Cut) Take(r *Record) (bool, error) {
	if c.limit <= 0 {
		_, _, err := fit(r, 0, true)
		return err == nil, err
	}
	// A line alone in its file takes its newline too, as Writer.Budget counts.
	line, _, err := fit(r, max(c.limit-1, 1), true)
	if err != nil {
		return false, err
	}
	need := len(line) + 1
	if c.room < c.limit && need > c.room {
		return false, nil
	}
	c.room -= need
	return true, nil
}
