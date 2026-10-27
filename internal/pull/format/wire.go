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

package format

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/internal/boundedjson"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// ErrRequest is a request that breaks the protocol. Its text says how.
var ErrRequest = errors.New("pull: invalid request")

func requestError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRequest, fmt.Sprintf(format, args...))
}

// The listing's page sizes.
const (
	DefaultLimit = 50
	MaxLimit     = 500
)

// RangeLimit bounds one range of archive bytes.
const RangeLimit = 8 << 20

// Row is one conversation of a listing, as its head round's header gives
// it. Counts a round written before they existed leaves out stay out.
type Row struct {
	ID                 string  `json:"id"`
	Session            string  `json:"session"`
	Runtime            string  `json:"runtime"`
	Title              string  `json:"title"`
	Round              uint64  `json:"round"`
	Digest             string  `json:"digest"`
	Talks              uint64  `json:"talks"`
	Steps              uint64  `json:"steps"`
	Streams            uint64  `json:"streams"`
	Segments           uint64  `json:"segments"`
	Unresolved         uint64  `json:"unresolved"`
	SessionFromTime    string  `json:"session_from_time,omitempty"`
	SessionThroughTime string  `json:"session_through_time,omitempty"`
	Changes            *uint64 `json:"changes,omitempty"`
	LinesAdded         *uint64 `json:"lines_added,omitempty"`
	LinesRemoved       *uint64 `json:"lines_removed,omitempty"`
	LLMCalls           *uint64 `json:"llm_calls,omitempty"`
	Subagents          *uint64 `json:"subagents,omitempty"`
	BashRuns           *uint64 `json:"bash_runs,omitempty"`
}

// Skipped is a conversation a listing could not read, named as display
// text, never a path or a selector.
type Skipped struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// Listing is one page of a listing.
type Listing struct {
	Schema  string    `json:"schema"`
	Rows    []Row     `json:"rows"`
	Skipped []Skipped `json:"skipped"`
	Next    string    `json:"next,omitempty"`
}

// Filters are the listing's filters as the query gave them: text exactly
// as decoded, a missing one empty.
type Filters struct {
	Since    string `json:"since"`
	Until    string `json:"until"`
	Runtime  string `json:"runtime"`
	Match    string `json:"match"`
	MinTalks uint64 `json:"min_talks"`
	Limit    uint64 `json:"limit"`
}

// Cursor is where the next page of a listing starts: the filters it was
// made with, the bounds they resolved to on the first page, and the last
// row returned. It is an ordering position, not an authorization.
type Cursor struct {
	V       uint64  `json:"v"`
	Filters Filters `json:"filters"`
	Since   string  `json:"since"`
	Until   string  `json:"until"`
	Time    string  `json:"time"`
	ID      string  `json:"id"`
}

// maxCursor bounds a cursor once decoded.
const maxCursor = 4 << 10

// EncodeCursor writes a cursor as unpadded base64url of its JSON. A cursor
// that would not fit is an error.
func EncodeCursor(c Cursor) (string, error) {
	c.V = 1
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	if len(b) > maxCursor {
		return "", requestError("the filters are too large for a cursor")
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// DecodeCursor reads a cursor: canonical unpadded base64url, at most four
// kibibytes once decoded, one object with exactly its members and their
// types, scanned within the bounded checks.
func DecodeCursor(text string) (Cursor, error) {
	var c Cursor
	if base64.RawURLEncoding.DecodedLen(len(text)) > maxCursor {
		return c, requestError("after is too long")
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(text)
	if err != nil || base64.RawURLEncoding.EncodeToString(b) != text {
		return c, requestError("after is not canonical unpadded base64url")
	}
	s := boundedjson.New(bytes.NewReader(b))
	err = exactMembers(s, map[string]func() error{
		"v": func() error {
			v, err := uint64Value(s)
			if err == nil && v != 1 {
				err = errors.New("not 1")
			}
			c.V = v
			return err
		},
		"filters": func() error { return readFilters(s, &c.Filters) },
		"since":   func() (err error) { c.Since, err = stringValue(s); return },
		"until":   func() (err error) { c.Until, err = stringValue(s); return },
		"time":    func() (err error) { c.Time, err = stringValue(s); return },
		"id":      func() (err error) { c.ID, err = stringValue(s); return },
	})
	if err == nil {
		err = valueEnded(s)
	}
	if err != nil {
		return Cursor{}, requestError("after: %v", err)
	}
	// A cursor is written after a page's last row, so it always names one.
	if err := storage.CheckNamePart(c.ID); err != nil {
		return Cursor{}, requestError("after: id: %v", err)
	}
	return c, nil
}

func readFilters(s *boundedjson.Scanner, f *Filters) error {
	return exactMembers(s, map[string]func() error{
		"since":     func() (err error) { f.Since, err = stringValue(s); return },
		"until":     func() (err error) { f.Until, err = stringValue(s); return },
		"runtime":   func() (err error) { f.Runtime, err = stringValue(s); return },
		"match":     func() (err error) { f.Match, err = stringValue(s); return },
		"min_talks": func() (err error) { f.MinTalks, err = uint64Value(s); return },
		"limit":     func() (err error) { f.Limit, err = uint64Value(s); return },
	})
}

// exactMembers reads an object holding exactly the named members, each
// read by its function: an unknown, a missing or a null member is refused,
// and the scanner refuses one given twice.
func exactMembers(s *boundedjson.Scanner, read map[string]func() error) error {
	if err := openObject(s); err != nil {
		return err
	}
	seen := map[string]bool{}
	for {
		token, err := s.Next()
		if err != nil {
			return err
		}
		if token.Kind == boundedjson.ObjectEnd {
			break
		}
		fn, ok := read[token.Text]
		if !ok {
			return fmt.Errorf("%s is not a member here", token.Text)
		}
		if err := fn(); err != nil {
			return fmt.Errorf("%s: %w", token.Text, err)
		}
		seen[token.Text] = true
	}
	for name := range read {
		if !seen[name] {
			return fmt.Errorf("%s is missing", name)
		}
	}
	return nil
}

// valueEnded requires the value to have ended, with nothing but whitespace after.
func valueEnded(s *boundedjson.Scanner) error {
	if _, err := s.Next(); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("a value follows")
		}
		return err
	}
	return nil
}

// ArchiveRequest is the body of an archive's creation: the lease the client
// holds, the names it wants, and the failed attempt it asks to restart
// after, if any.
type ArchiveRequest struct {
	Lease   string
	Files   []string
	Restart uint64
}

// ReadArchiveRequest reads an archive's creation as it streams in: one
// object with lease and a non-empty files list, and restart when it
// restarts a failed attempt.
// Every name is safe and given once; there are at most most of them, the
// snapshot's file count. Whether each is in the manifest is the
// server's to check.
func ReadArchiveRequest(r io.Reader, most int) (ArchiveRequest, error) {
	var a ArchiveRequest
	s := boundedjson.New(r)
	err := requestMembers(s, []string{"lease", "files"}, map[string]func() error{
		"lease": func() (err error) { a.Lease, err = leaseValue(s); return },
		"files": func() error { return readNames(s, most, &a.Files) },
		"restart": func() error {
			n, err := uint64Value(s)
			if err == nil && n == 0 {
				err = errors.New("not a positive attempt")
			}
			a.Restart = n
			return err
		},
	})
	if err == nil {
		err = valueEnded(s)
	}
	if err != nil {
		return ArchiveRequest{}, requestError("%v", err)
	}
	return a, nil
}

// ReadDoneRequest reads a lease's release: one object with lease alone.
func ReadDoneRequest(r io.Reader) (string, error) {
	var lease string
	s := boundedjson.New(r)
	err := requestMembers(s, []string{"lease"}, map[string]func() error{
		"lease": func() (err error) { lease, err = leaseValue(s); return },
	})
	if err == nil {
		err = valueEnded(s)
	}
	if err != nil {
		return "", requestError("%v", err)
	}
	return lease, nil
}

// requestMembers reads a request body's object: the required members, the
// optional ones read lists, and nothing else.
func requestMembers(s *boundedjson.Scanner, required []string, read map[string]func() error) error {
	if err := openObject(s); err != nil {
		return err
	}
	seen := map[string]bool{}
	for {
		token, err := s.Next()
		if err != nil {
			return err
		}
		if token.Kind == boundedjson.ObjectEnd {
			break
		}
		fn, ok := read[token.Text]
		if !ok {
			return fmt.Errorf("%s is not a member of this request", token.Text)
		}
		if err := fn(); err != nil {
			return fmt.Errorf("%s: %w", token.Text, err)
		}
		seen[token.Text] = true
	}
	for _, name := range required {
		if !seen[name] {
			return fmt.Errorf("%s is missing", name)
		}
	}
	return nil
}

func leaseValue(s *boundedjson.Scanner) (string, error) {
	lease, err := stringValue(s)
	if err == nil && !storage.Digest64(lease) {
		err = errors.New("not 64 lower-case hexadecimal characters")
	}
	return lease, err
}

// readNames reads a non-empty list of safe names, each once, counting each
// before it is kept.
func readNames(s *boundedjson.Scanner, most int, out *[]string) error {
	token, err := s.Next()
	if err != nil {
		return err
	}
	if token.Kind != boundedjson.Array {
		return errors.New("expected an array")
	}
	seen := map[string]bool{}
	for {
		token, err := s.Next()
		if err != nil {
			return err
		}
		if token.Kind == boundedjson.ArrayEnd {
			break
		}
		if token.Kind != boundedjson.String {
			return errors.New("expected a name")
		}
		if len(*out) >= most {
			return fmt.Errorf("more than the snapshot's %d files", most)
		}
		if err := storage.CheckRelativePath(token.Text); err != nil {
			return err
		}
		if seen[token.Text] {
			return fmt.Errorf("%q is asked for twice", token.Text)
		}
		seen[token.Text] = true
		*out = append(*out, token.Text)
	}
	if len(*out) == 0 {
		return errors.New("no file is asked for")
	}
	return nil
}

// ParseRange reads a range request's Range header: exactly one closed range
// of bytes, start not after end, at most RangeLimit bytes.
func ParseRange(values []string) (start, end uint64, err error) {
	if len(values) != 1 {
		return 0, 0, requestError("one Range header is required")
	}
	spec, ok := strings.CutPrefix(values[0], "bytes=")
	if !ok || strings.ContainsAny(spec, ", \t") {
		return 0, 0, requestError("Range is not one closed range of bytes")
	}
	first, last, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, requestError("Range is not one closed range of bytes")
	}
	if start, err = decimal(first); err != nil {
		return 0, 0, requestError("Range: %v", err)
	}
	if end, err = decimal(last); err != nil {
		return 0, 0, requestError("Range: %v", err)
	}
	if end < start {
		return 0, 0, requestError("Range ends before it starts")
	}
	if end-start >= RangeLimit {
		return 0, 0, requestError("Range is larger than %d bytes", RangeLimit)
	}
	return start, end, nil
}

// decimal reads a whole number written plainly: digits only, no sign, no
// leading zero but in 0 itself, within 64 bits.
func decimal(s string) (uint64, error) {
	if s == "" || (len(s) > 1 && s[0] == '0') || strings.TrimLeft(s, "0123456789") != "" {
		return 0, fmt.Errorf("%q is not a decimal number", s)
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is past the 64-bit range", s)
	}
	return n, nil
}

// ParseIfMatch reads a range request's If-Match: exactly one quoted strong
// entity tag, which is an archive's byte digest.
func ParseIfMatch(values []string) (string, error) {
	if len(values) != 1 {
		return "", requestError("one If-Match header is required")
	}
	v := values[0]
	if len(v) != 66 || v[0] != '"' || v[65] != '"' || !storage.Digest64(v[1:65]) {
		return "", requestError("If-Match is not one quoted strong archive digest")
	}
	return v[1:65], nil
}

// ParseLeaseHeader reads the one Asz-Pull-Lease header a GET of an archive
// carries.
func ParseLeaseHeader(values []string) (string, error) {
	if len(values) != 1 || !storage.Digest64(values[0]) {
		return "", requestError("one Asz-Pull-Lease header of 64 lower-case hexadecimal characters is required")
	}
	return values[0], nil
}

// ManifestPending is the manifest route's answer while its freeze builds.
type ManifestPending struct {
	Schema       string `json:"schema"`
	Conversation string `json:"conversation"`
	Freeze       string `json:"freeze"`
	State        string `json:"state"`
}

// ArchiveState is an archive's answer: building, whether queued or active,
// or ready with its size and byte digest.
type ArchiveState struct {
	Schema   string `json:"schema"`
	Snapshot string `json:"snapshot"`
	Archive  string `json:"archive"`
	State    string `json:"state"`
	Bytes    uint64 `json:"bytes,omitempty"`
	Digest   string `json:"digest,omitempty"`
}

// The problem types a client acts on by name.
const (
	ProblemOwnerWait     = "urn:asz:pull:owner-wait"
	ProblemStorageState  = "urn:asz:pull:storage-state"
	ProblemSourceRead    = "urn:asz:pull:source-read"
	ProblemSourceChange  = "urn:asz:pull:source-changing"
	ProblemSourceInvalid = "urn:asz:pull:source-invalid"
	ProblemFreezeFailed  = "urn:asz:pull:freeze-build-failed"
	ProblemArchiveFailed = "urn:asz:pull:archive-build-failed"
)

// The states a job's answer gives.
const (
	StateBuilding = "building"
	StateReady    = "ready"
)
