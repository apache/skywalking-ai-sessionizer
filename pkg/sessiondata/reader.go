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
	"errors"
	"fmt"
	"hash"
	"io"
	"time"
)

// Reader reads a .sd file back.
//
// It uses bufio.Reader rather than bufio.Scanner on purpose: real records reach
// about a megabyte, far past Scanner's 64 KB default, where Scanner stops
// silently rather than loudly.
type Reader struct {
	br   *bufio.Reader
	hdr  Header
	h    hash.Hash
	n    int
	end  *End
	done bool
}

// NewReader consumes the header and returns a Reader at the first record.
func NewReader(r io.Reader) (*Reader, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	line, err := readLine(br)
	if err != nil {
		return nil, fmt.Errorf("sessiondata: read header: %w", err)
	}
	rd := &Reader{br: br, h: sha256.New()}
	rd.h.Write(line)
	rd.h.Write([]byte{'\n'})
	if err := json.Unmarshal(line, &rd.hdr); err != nil {
		return nil, fmt.Errorf("sessiondata: decode header: %w", err)
	}
	if err := rd.hdr.Validate(); err != nil {
		return nil, err
	}
	return rd, nil
}

// Header returns the file's header.
func (r *Reader) Header() Header { return r.hdr }

// Next returns the next record, or io.EOF once the closing line is reached.
//
// Reaching EOF without a closing line is an error, not an end: a file cut short
// mid-write would otherwise read as a shorter conversation with nothing saying
// so.
func (r *Reader) Next() (*Record, error) {
	if r.done {
		return nil, io.EOF
	}
	line, err := readLine(r.br)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("sessiondata: %s has no closing line; it is incomplete", r.hdr.Src)
		}
		return nil, err
	}
	if isEnd(line) {
		var e End
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("sessiondata: decode closing line: %w", err)
		}
		r.end, r.done = &e, true
		if got := hex.EncodeToString(r.h.Sum(nil)); got != e.Digest {
			return nil, fmt.Errorf("sessiondata: %s: digest mismatch, computed %s and the file claims %s",
				r.hdr.Src, got[:12], firstN(e.Digest, 12))
		}
		if r.n != e.Records {
			return nil, fmt.Errorf("sessiondata: %s holds %d records, the file claims %d",
				r.hdr.Src, r.n, e.Records)
		}
		return nil, io.EOF
	}
	r.h.Write(line)
	r.h.Write([]byte{'\n'})
	r.n++
	var rec Record
	if err := json.Unmarshal(line, &rec); err != nil {
		return nil, fmt.Errorf("sessiondata: decode record: %w", err)
	}
	return &rec, nil
}

// NextRaw returns the next record as the bytes of its line, without decoding
// it, or io.EOF once the closing line is reached. The closing line is
// verified the same way Next verifies it.
//
// It exists for the paths that must carry a record unchanged: repacking a
// zone under another file budget, and sending records over a wire where the
// receiver lands them byte for byte so the file digests still match. A record
// re-encoded from its decoded form could differ in bytes and break both.
func (r *Reader) NextRaw() ([]byte, error) {
	if r.done {
		return nil, io.EOF
	}
	line, err := readLine(r.br)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("sessiondata: %s has no closing line; it is incomplete", r.hdr.Src)
		}
		return nil, err
	}
	if isEnd(line) {
		var e End
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("sessiondata: decode closing line: %w", err)
		}
		r.end, r.done = &e, true
		if got := hex.EncodeToString(r.h.Sum(nil)); got != e.Digest {
			return nil, fmt.Errorf("sessiondata: %s: digest mismatch, computed %s and the file claims %s",
				r.hdr.Src, got[:12], firstN(e.Digest, 12))
		}
		if r.n != e.Records {
			return nil, fmt.Errorf("sessiondata: %s holds %d records, the file claims %d",
				r.hdr.Src, r.n, e.Records)
		}
		return nil, io.EOF
	}
	r.h.Write(line)
	r.h.Write([]byte{'\n'})
	r.n++
	out := make([]byte, len(line))
	copy(out, line)
	return out, nil
}

// End returns the closing line, once the file has been read to its end.
func (r *Reader) End() *End { return r.end }

// LineTime returns a record's time from the bytes of its line, as unix
// nanoseconds, without decoding the record. It reports false when the record
// carries no time.
//
// A page turning thousands of node references into moments must not pay for
// a full decode of every record, most of which is content it will never
// show. The record's own fields are encoded before its parts, so the first
// time field that appears before the parts is the record's own and never one
// nested inside a part.
func LineTime(line []byte) (int64, bool) {
	limit := bytes.Index(line, []byte(`"parts":`))
	if limit < 0 {
		limit = len(line)
	}
	i := bytes.Index(line[:limit], []byte(`"time":"`))
	if i < 0 {
		return 0, false
	}
	start := i + len(`"time":"`)
	end := bytes.IndexByte(line[start:], '"')
	if end < 0 {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339Nano, string(line[start:start+end]))
	if err != nil {
		return 0, false
	}
	return t.UnixNano(), true
}

// isEnd reports whether a line is the closing one, without a full decode.
func isEnd(line []byte) bool {
	return len(line) > 8 && string(line[:9]) == `{"t":"end`
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// All reads every record in a file.
func All(r io.Reader) (Header, []*Record, error) {
	rd, err := NewReader(r)
	if err != nil {
		return Header{}, nil, err
	}
	var out []*Record
	for {
		rec, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return rd.hdr, out, nil
		}
		if err != nil {
			return rd.hdr, out, err
		}
		out = append(out, rec)
	}
}

func readLine(br *bufio.Reader) ([]byte, error) {
	line, err := br.ReadBytes('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && len(line) > 0 {
			return line, nil
		}
		return nil, err
	}
	return line[:len(line)-1], nil
}
