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

package storage

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// CursorKind distinguishes the two tracking strategies. Sources that only grow
// are tracked by position; sources that are rewritten whole are tracked by
// content digest.
type CursorKind string

const (
	// CursorAppend tracks a growing JSONL source by byte offset and line ordinal.
	CursorAppend CursorKind = "append"
	// CursorSnapshot tracks a rewritten JSON source by content digest, landing a
	// new immutable version whenever the digest changes.
	CursorSnapshot CursorKind = "snapshot"
)

// Cursor states.
const (
	// CursorActive is the normal state.
	CursorActive = "active"
	// CursorSourceGone means the source file was deleted. This is expected, not
	// an error: Claude Code prunes transcripts, and our landed deltas outlive them.
	CursorSourceGone = "source_gone"
	// CursorConflict means the source changed underneath us - rotated, truncated,
	// or rewritten behind the cursor. Collection stops rather than resuming from
	// a position that no longer means what it meant.
	CursorConflict = "conflict"
)

// TailWindow is how many bytes before the cursor are covered by TailSHA256.
//
// Sized above the largest transcript line observed in a real corpus (979,632
// bytes) so the window always spans at least one complete record boundary.
// Digesting the whole consumed prefix instead would mean re-reading up to 59 MB
// on every pass.
const TailWindow int64 = 1 << 20

// Cursor records how much of one source file has been consumed.
//
// For an append cursor Offset is both "bytes consumed" and "where the next read
// starts" - one value, not two. It advances only to the last complete newline;
// an unterminated tail is an in-flight write and is deliberately left behind.
type Cursor struct {
	Schema int
	Kind   CursorKind
	Source string // path relative to the adapter's source root

	// Append fields.
	Dev        uint64
	Ino        uint64
	Offset     uint64
	Ord        uint64 // source line consumed through; the next batch starts at Ord+1
	LastUUID   string // last uuid seen - diagnostic only, since many lines carry none
	TailSHA256 string

	// Snapshot fields.
	ContentSHA256 string

	Size      int64
	MTime     int64
	LastSeq   uint64
	UpdatedAt string
	State     string
}

// NewCursor returns a zero cursor of the given kind for a source path.
func NewCursor(kind CursorKind, source string) *Cursor {
	return &Cursor{Schema: 1, Kind: kind, Source: source, State: CursorActive}
}

// LoadCursor reads a cursor file. A missing file yields a fresh cursor, which
// is how a newly discovered source begins.
func LoadCursor(path string, kind CursorKind, source string) (*Cursor, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return NewCursor(kind, source), nil
		}
		return nil, err
	}
	defer f.Close()

	c := NewCursor(kind, source)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch key {
		case "schema":
			c.Schema, _ = strconv.Atoi(val)
		case "kind":
			c.Kind = CursorKind(val)
		case "source":
			c.Source = val
		case "dev":
			c.Dev, _ = strconv.ParseUint(val, 10, 64)
		case "ino":
			c.Ino, _ = strconv.ParseUint(val, 10, 64)
		case "offset":
			c.Offset, _ = strconv.ParseUint(val, 10, 64)
		case "ord":
			c.Ord, _ = strconv.ParseUint(val, 10, 64)
		case "last_uuid":
			c.LastUUID = val
		case "tail_sha256":
			c.TailSHA256 = val
		case "content_sha256":
			c.ContentSHA256 = val
		case "size":
			c.Size, _ = strconv.ParseInt(val, 10, 64)
		case "mtime":
			c.MTime, _ = strconv.ParseInt(val, 10, 64)
		case "last_seq":
			c.LastSeq, _ = strconv.ParseUint(val, 10, 64)
		case "updated_at":
			c.UpdatedAt = val
		case "state":
			c.State = val
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("storage: read cursor %s: %w", path, err)
	}
	return c, nil
}

// Save writes the cursor atomically.
func (c *Cursor) Save(path string, now time.Time) error {
	c.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
	return WriteAtomic(path, PermState, func(w io.Writer) error {
		bw := bufio.NewWriter(w)
		fmt.Fprintf(bw, "schema        %d\n", c.Schema)
		fmt.Fprintf(bw, "kind          %s\n", c.Kind)
		fmt.Fprintf(bw, "source        %s\n", c.Source)
		if c.Kind == CursorAppend {
			fmt.Fprintf(bw, "dev           %d\n", c.Dev)
			fmt.Fprintf(bw, "ino           %d\n", c.Ino)
			fmt.Fprintf(bw, "offset        %d\n", c.Offset)
			fmt.Fprintf(bw, "ord           %d\n", c.Ord)
			if c.LastUUID != "" {
				fmt.Fprintf(bw, "last_uuid     %s\n", c.LastUUID)
			}
			if c.TailSHA256 != "" {
				fmt.Fprintf(bw, "tail_sha256   %s\n", c.TailSHA256)
			}
		} else {
			fmt.Fprintf(bw, "content_sha256 %s\n", c.ContentSHA256)
		}
		fmt.Fprintf(bw, "size          %d\n", c.Size)
		fmt.Fprintf(bw, "mtime         %d\n", c.MTime)
		fmt.Fprintf(bw, "last_seq      %d\n", c.LastSeq)
		fmt.Fprintf(bw, "updated_at    %s\n", c.UpdatedAt)
		fmt.Fprintf(bw, "state         %s\n", c.State)
		return bw.Flush()
	})
}
