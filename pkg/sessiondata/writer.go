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
// The closing digest is what makes a landed file self-verifying. It replaces a
// check that is no longer possible: a record used to keep its source bytes, so
// its digest could be recomputed from them. Now the source bytes are converted
// on the way in and not kept, so what can still be proved is that the file has
// not changed since it was written - and that a file cut short mid-write is
// recognised as incomplete rather than read as short.
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
	enc, err := json.Marshal(h)
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
	enc, err := json.Marshal(r)
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
	enc, err := json.Marshal(end)
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
