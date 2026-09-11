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
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
)

// Writer emits a .sd file: a header line, one line per record, and a closing
// line carrying a digest of everything before it.
//
// The digest proves the file has not changed since it was written. It also
// makes a file cut short mid-write fail as incomplete. A record's source
// digest has a different purpose: it identifies the source record before
// conversion, including any envelope fields the conversion leaves out.
type Writer struct {
	bw *bufio.Writer
	h  hash.Hash
	n  int
}

// End is the last line of a .sd file.
type End struct {
	T       string `json:"t"`
	Records int    `json:"records"`
	Digest  string `json:"digest"`
}

// NewWriter returns a Writer that emits h as the first line.
func NewWriter(w io.Writer, h *Header) (*Writer, error) {
	h.H, h.Schema = 1, Schema
	if err := h.Validate(); err != nil {
		return nil, err
	}
	sum := sha256.New()
	bw := bufio.NewWriterSize(io.MultiWriter(w, sum), 1<<20)
	enc, err := encodeLine(h)
	if err != nil {
		return nil, fmt.Errorf("sessiondata: encode header: %w", err)
	}
	if _, err := bw.Write(enc); err != nil {
		return nil, err
	}
	if err := bw.WriteByte('\n'); err != nil {
		return nil, err
	}
	return &Writer{bw: bw, h: sum}, nil
}

// Write appends one record.
func (w *Writer) Write(r *Record) error {
	enc, err := encodeRecord(r)
	if err != nil {
		return fmt.Errorf("sessiondata: encode record %d: %w", r.Ord, err)
	}
	if _, err := w.bw.Write(enc); err != nil {
		return err
	}
	if err := w.bw.WriteByte('\n'); err != nil {
		return err
	}
	w.n++
	return nil
}

// WriteRaw appends one record that is already encoded, as the bytes of its
// line without the newline. It is the counterpart of Reader.NextRaw: the
// bytes go into the file and the digest exactly as given.
func (w *Writer) WriteRaw(line []byte) error {
	if _, err := w.bw.Write(line); err != nil {
		return err
	}
	if err := w.bw.WriteByte('\n'); err != nil {
		return err
	}
	w.n++
	return nil
}

// Count reports how many records have been written.
func (w *Writer) Count() int { return w.n }

// Close writes the closing line and flushes.
//
// The digest covers the header and every record, so it is computed before the
// line carrying it is written - the same arrangement a round file uses.
func (w *Writer) Close() error {
	if err := w.bw.Flush(); err != nil {
		return err
	}
	end := End{T: "end", Records: w.n, Digest: hex.EncodeToString(w.h.Sum(nil))}
	enc, err := encodeLine(end)
	if err != nil {
		return err
	}
	if _, err := w.bw.Write(enc); err != nil {
		return err
	}
	if err := w.bw.WriteByte('\n'); err != nil {
		return err
	}
	return w.bw.Flush()
}

// encodeLine leaves <, > and & readable in the fields the writer owns.
func encodeLine(v any) ([]byte, error) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte{'\n'}), nil
}

var dataSlot = []byte(`"data":null`)

// encodeRecord keeps each part's data apart from whitespace between tokens.
// The JSON encoder still rewrites RawMessage values, including U+2028 and
// U+2029 under Go's JSON v2 engine, even with HTML escaping disabled. Insert
// the compacted data after encoding the other fields so its spelling survives.
func encodeRecord(r *Record) ([]byte, error) {
	if r == nil {
		return encodeLine(r)
	}
	var held [][]byte
	size := 0
	cp := *r
	cp.Parts = append([]Part(nil), r.Parts...)
	for i := range cp.Parts {
		if len(cp.Parts[i].Data) == 0 {
			continue
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, cp.Parts[i].Data); err != nil {
			return nil, fmt.Errorf("part %d: %w", i, err)
		}
		held = append(held, compact.Bytes())
		size += compact.Len()
		cp.Parts[i].Data = json.RawMessage("null")
	}
	line, err := encodeLine(&cp)
	if err != nil || len(held) == 0 {
		return line, err
	}
	// A string cannot contain this literal field: its quotes are escaped.
	if n := bytes.Count(line, dataSlot); n != len(held) {
		return nil, fmt.Errorf("found %d places for the data of %d parts", n, len(held))
	}
	out := make([]byte, 0, len(line)+size)
	for _, data := range held {
		i := bytes.Index(line, dataSlot) + len(`"data":`)
		out = append(out, line[:i]...)
		out = append(out, data...)
		line = line[i+len("null"):]
	}
	out = append(out, line...)
	// A part is inside three record containers. JSON near the depth limit
	// can be valid on its own but unreadable after insertion into a record.
	if !json.Valid(out) {
		return nil, fmt.Errorf("encoded record is not valid JSON")
	}
	return out, nil
}
