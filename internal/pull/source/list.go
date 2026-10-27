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

package source

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/apache/skywalking-ai-sessionizer/internal/pull/format"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// Query is a listing's request: its filters as given, the bounds they
// resolved to, and where the page starts.
type Query struct {
	Filters      format.Filters
	Since, Until time.Time
	// After is the last row of the page before, when there was one.
	After *format.Cursor
}

// queryNames are the listing's parameters; any other is refused.
var queryNames = map[string]bool{"since": true, "until": true, "runtime": true, "match": true, "min_talks": true, "limit": true, "after": true}

// ParseQuery reads the listing's parameters at now. Each comes at most
// once. A cursor must carry exactly the filters given with it, and its
// bounds are the ones the first page resolved, never resolved again.
func ParseQuery(values url.Values, now time.Time) (Query, error) {
	var q Query
	for name, v := range values {
		if !queryNames[name] {
			return q, requestError("%s is not a parameter of the listing", name)
		}
		if len(v) != 1 {
			return q, requestError("%s is given %d times", name, len(v))
		}
	}
	f := format.Filters{Since: values.Get("since"), Until: values.Get("until"), Runtime: values.Get("runtime"),
		Match: values.Get("match"), Limit: format.DefaultLimit}
	if values.Has("min_talks") {
		n, err := whole(values.Get("min_talks"))
		if err != nil {
			return q, requestError("min_talks is not a count")
		}
		f.MinTalks = n
	}
	if values.Has("limit") {
		n, err := whole(values.Get("limit"))
		if err != nil || n < 1 || n > format.MaxLimit {
			return q, requestError("limit is a whole number from 1 to %d", format.MaxLimit)
		}
		f.Limit = n
	}
	q.Filters = f
	if values.Has("after") {
		c, err := format.DecodeCursor(values.Get("after"))
		if err != nil {
			return q, err
		}
		if c.Filters != f {
			return q, requestError("after was made with other filters")
		}
		if q.Since, err = cursorBound(c.Since); err != nil {
			return q, err
		}
		if q.Until, err = cursorBound(c.Until); err != nil {
			return q, err
		}
		if c.Time != "" {
			if _, err := time.Parse(time.RFC3339Nano, c.Time); err != nil {
				return q, requestError("after: time: %v", err)
			}
		}
		q.After = &c
	} else {
		var err error
		if q.Since, err = bound("since", f.Since, now); err != nil {
			return q, err
		}
		if q.Until, err = bound("until", f.Until, now); err != nil {
			return q, err
		}
	}
	if !q.Since.IsZero() && !q.Until.IsZero() && q.Since.After(q.Until) {
		return q, requestError("since is after until")
	}
	// A cursor of these filters must fit, whatever row it ends on.
	probe := format.Cursor{Filters: f, Since: stamp(q.Since), Until: stamp(q.Until),
		Time: "2006-01-02T15:04:05.999999999Z", ID: strings.Repeat("x", 255)}
	if _, err := format.EncodeCursor(probe); err != nil {
		return q, err
	}
	return q, nil
}

// whole reads a count written plainly: digits only, with no sign and no
// leading zero but in 0 itself.
func whole(s string) (uint64, error) {
	if s == "" || len(s) > 1 && s[0] == '0' || strings.TrimLeft(s, "0123456789") != "" {
		return 0, fmt.Errorf("%q is not written as a whole number", s)
	}
	return strconv.ParseUint(s, 10, 64)
}

func requestError(text string, args ...any) error {
	return fmt.Errorf("%w: %s", format.ErrRequest, fmt.Sprintf(text, args...))
}

// bound resolves since or until: a positive duration counted back from now,
// or an RFC 3339 time.
func bound(name, text string, now time.Time) (time.Time, error) {
	if text == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(text); err == nil {
		if d <= 0 {
			return time.Time{}, requestError("%s is a duration that is not positive", name)
		}
		return now.Add(-d).UTC(), nil
	}
	t, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return time.Time{}, requestError("%s is neither a duration nor an RFC 3339 time", name)
	}
	return t.UTC(), nil
}

func cursorBound(text string) (time.Time, error) {
	if text == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return time.Time{}, requestError("after: a bound: %v", err)
	}
	return t.UTC(), nil
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// row is a listing row with its activity time read.
type row struct {
	format.Row
	at time.Time
}

// before orders rows newest activity first, then by identifier; a row with
// no activity time comes after every row that has one.
func before(a, b row) int {
	switch {
	case !a.at.IsZero() && b.at.IsZero():
		return -1
	case a.at.IsZero() && !b.at.IsZero():
		return 1
	case !a.at.Equal(b.at):
		return b.at.Compare(a.at)
	}
	return strings.Compare(a.ID, b.ID)
}

// List lists one page of a live root's conversations. Each row is read off
// its head round's header and closing commit and its landed files' headers,
// never by folding. A conversation that cannot be read is named in skipped;
// a retired one, or one whose files a removal is taking, is left out. A
// root whose conversations cannot be listed fails the request.
func List(root string, q Query) (*format.Listing, error) {
	dirs, err := os.ReadDir(filepath.Join(root, "_conversations"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	out := &format.Listing{Schema: format.Schema, Rows: []format.Row{}, Skipped: []format.Skipped{}}
	var rows []row
	for _, d := range dirs {
		name := d.Name()
		r, err := readRow(root, name)
		switch {
		case errors.Is(err, errOmitted):
			continue
		case err != nil:
			out.Skipped = append(out.Skipped, format.Skipped{ID: display(name), Reason: err.Error()})
			continue
		}
		if keep(r, q) {
			rows = append(rows, r)
		}
	}
	slices.SortFunc(rows, before)
	slices.SortFunc(out.Skipped, func(a, b format.Skipped) int { return strings.Compare(a.ID, b.ID) })
	start := 0
	if q.After != nil {
		var at time.Time
		if q.After.Time != "" {
			at, _ = time.Parse(time.RFC3339Nano, q.After.Time)
		}
		position := row{Row: format.Row{ID: q.After.ID}, at: at}
		start, _ = slices.BinarySearchFunc(rows, position, before)
		if start < len(rows) && before(rows[start], position) == 0 {
			start++
		}
	}
	end := min(len(rows), start+int(q.Filters.Limit))
	for _, r := range rows[start:end] {
		out.Rows = append(out.Rows, r.Row)
	}
	if end < len(rows) {
		last := rows[end-1]
		next, err := format.EncodeCursor(format.Cursor{Filters: q.Filters, Since: stamp(q.Since), Until: stamp(q.Until),
			Time: stamp(last.at), ID: last.ID})
		if err != nil {
			return nil, err
		}
		out.Next = next
	}
	return out, nil
}

// keep applies the filters to a row.
func keep(r row, q Query) bool {
	f := q.Filters
	if (!q.Since.IsZero() || !q.Until.IsZero()) && r.at.IsZero() {
		return false
	}
	if !q.Since.IsZero() && r.at.Before(q.Since) {
		return false
	}
	if !q.Until.IsZero() && !r.at.Before(q.Until) {
		return false
	}
	if f.Runtime != "" && r.Runtime != f.Runtime {
		return false
	}
	if f.Match != "" && !strings.Contains(fold(r.Title), fold(f.Match)) {
		return false
	}
	return r.Talks >= f.MinTalks
}

// fold is text under Unicode simple case folding: each character becomes
// the smallest of the characters that fold together with it. Nothing is
// normalized, and no locale applies.
func fold(s string) string {
	var b strings.Builder
	for _, r := range s {
		smallest := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < smallest {
				smallest = f
			}
		}
		b.WriteRune(smallest)
	}
	return b.String()
}

// errOmitted leaves a conversation out of a listing without a word: its
// session is retired, or a removal is taking its files.
var errOmitted = errors.New("source: omitted")

// readRow reads one conversation's row.
func readRow(root, conversation string) (row, error) {
	if !utf8.ValidString(conversation) {
		return row{}, errors.New("the identifier is not valid UTF-8")
	}
	if err := storage.CheckNamePart(conversation); err != nil {
		return row{}, fmt.Errorf("the identifier is not one a pull can hold: %w", err)
	}
	chain := sessionflow.OpenChain(root, conversation)
	rounds, err := chain.ListAtMost(format.MaxFiles)
	if err != nil {
		return row{}, fmt.Errorf("its rounds cannot be listed: %w", err)
	}
	if len(rounds) == 0 {
		return row{}, errOmitted
	}
	last := rounds[len(rounds)-1]
	h, commit, err := headerAndCommit(last.Path)
	if err != nil {
		return row{}, err
	}
	switch {
	case h.Conversation != conversation:
		return row{}, fmt.Errorf("its head round is of conversation %q", h.Conversation)
	case h.Round != last.Round || h.Round == 0:
		return row{}, fmt.Errorf("its head round's file is round %d, and its header says %d", last.Round, h.Round)
	case !storage.Digest64(commit) || !strings.HasPrefix(commit, last.Digest):
		return row{}, errors.New("its head round's commit does not match its file name")
	}
	if err := storage.CheckNamePart(h.Session); err != nil || strings.HasPrefix(h.Session, "_") {
		return row{}, fmt.Errorf("session %q is not one a pull can hold", h.Session)
	}
	zone := storage.NewZone(root)
	retired, err := storage.Retired(zone, h.Session)
	if err != nil {
		return row{}, fmt.Errorf("its session's state cannot be read: %w", err)
	}
	_, removing, err := storage.RemovalGeneration(zone, h.Session)
	if err != nil {
		return row{}, fmt.Errorf("its session's removal state cannot be read: %w", err)
	}
	if retired || removing {
		return row{}, errOmitted
	}
	r := row{Row: format.Row{ID: conversation, Session: h.Session, Title: h.Title, Round: h.Round, Digest: commit,
		Talks: count(h.Talks), Steps: count(h.Steps), Streams: count(h.Streams), Segments: count(h.Segments),
		Unresolved: count(h.Unresolved), SessionFromTime: h.SessionFromTime, SessionThroughTime: h.SessionThroughTime,
		Changes: optional(h.Changes), LinesAdded: optional(h.LinesAdded), LinesRemoved: optional(h.LinesRemoved),
		LLMCalls: optional(h.LLMCalls), Subagents: optional(h.Subagents), BashRuns: optional(h.BashRuns)}}
	for _, t := range []string{h.SessionFromTime, h.SessionThroughTime} {
		if t == "" {
			continue
		}
		if _, err := time.Parse(time.RFC3339Nano, t); err != nil {
			return row{}, fmt.Errorf("its head round's session time %q is not an RFC 3339 time", t)
		}
	}
	if h.SessionThroughTime != "" {
		r.at, _ = time.Parse(time.RFC3339Nano, h.SessionThroughTime)
	}
	if r.Runtime, err = runtimeOf(zone, h.Session); err != nil {
		return row{}, err
	}
	return r, nil
}

func count(n int) uint64 {
	if n < 0 {
		return 0
	}
	return uint64(n)
}

func optional(n *int) *uint64 {
	if n == nil {
		return nil
	}
	v := count(*n)
	return &v
}

// maxHeaderLine bounds the header line a listing reads of a round.
const maxHeaderLine = 1 << 20

// maxCommitLine bounds the commit line a listing reads off a round's end.
const maxCommitLine = 64 << 10

// headerAndCommit reads a round's header, its first line, and its commit
// digest, off its last line, and nothing between: a listing never reads a
// round whole.
func headerAndCommit(path string) (*sessionflow.Header, string, error) {
	f, err := storage.OpenEvidence(path, nil)
	if err != nil {
		return nil, "", fmt.Errorf("its head round cannot be read: %w", err)
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64<<10)
	line, err := br.ReadSlice('\n')
	for errors.Is(err, bufio.ErrBufferFull) && len(line) < maxHeaderLine {
		var more []byte
		more, err = br.ReadSlice('\n')
		line = append(slices.Clone(line), more...)
	}
	if err != nil {
		return nil, "", fmt.Errorf("its head round's header cannot be read: %w", err)
	}
	var h sessionflow.Header
	if err := json.Unmarshal(line, &h); err != nil || h.T != sessionflow.FrameHeader {
		return nil, "", errors.New("its head round's header does not read")
	}
	if err := h.Validate(); err != nil {
		return nil, "", fmt.Errorf("its head round's header: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		return nil, "", fmt.Errorf("its head round cannot be read: %w", err)
	}
	tail := min(info.Size(), maxCommitLine)
	buf := make([]byte, tail)
	if _, err := f.ReadAt(buf, info.Size()-tail); err != nil && !errors.Is(err, io.EOF) {
		return nil, "", fmt.Errorf("its head round's commit cannot be read: %w", err)
	}
	buf = bytes.TrimSuffix(buf, []byte("\n"))
	if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
		buf = buf[i+1:]
	}
	var c sessionflow.Commit
	if err := json.Unmarshal(buf, &c); err != nil || c.T != sessionflow.FrameCommit {
		return nil, "", errors.New("its head round's commit does not read")
	}
	return &h, c.Digest, nil
}

// runtimeOf is a session's dialect, chosen as the page chooses it: the
// first transcript's, or the first one seen when no transcript has one.
// Landed headers are read in order and nothing more of them.
func runtimeOf(zone *storage.Zone, session string) (string, error) {
	files, err := storage.LandedFiles(zone, session)
	if err != nil {
		return "", fmt.Errorf("its landed files cannot be listed: %w", err)
	}
	var fallback string
	for _, lf := range files {
		h, err := landedHeader(lf.Path)
		if err != nil {
			return "", err
		}
		if h.Dialect == "" {
			continue
		}
		if h.Kind == sessiondata.KindTranscript {
			return h.Dialect, nil
		}
		if fallback == "" {
			fallback = h.Dialect
		}
	}
	return fallback, nil
}

func landedHeader(path string) (sessiondata.Header, error) {
	f, err := storage.OpenEvidence(path, nil)
	if err != nil {
		return sessiondata.Header{}, fmt.Errorf("a landed file cannot be read: %w", err)
	}
	defer f.Close()
	// A skeleton, which retention leaves in a landed file's place, is
	// compressed.
	var r io.Reader = f
	if sessiondata.IsSkeleton(path) {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return sessiondata.Header{}, fmt.Errorf("a skeleton's header does not read: %w", err)
		}
		defer zr.Close()
		r = zr
	}
	rd, err := sessiondata.NewReader(r)
	if err != nil {
		return sessiondata.Header{}, fmt.Errorf("a landed file's header does not read: %w", err)
	}
	return rd.Header(), nil
}

// display writes a name for a person to read: every byte that is not valid
// UTF-8 as \xHH, so the answer stays valid UTF-8.
func display(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); {
		r, size := utf8.DecodeRuneInString(name[i:])
		if r == utf8.RuneError && size <= 1 {
			fmt.Fprintf(&b, `\x%02X`, name[i])
			i++
			continue
		}
		b.WriteString(name[i : i+size])
		i += size
	}
	return b.String()
}
