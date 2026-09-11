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

// Package verify checks that landed data is internally consistent.
//
// The checks here deliberately need only the storage root: the landed files,
// and a stream's cursor where it has one. Claude Code prunes transcripts. The
// great majority of session ids in its own prompt history have none, so a
// check that requires the original source usually cannot run.
package verify

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// Gap is a discontinuity found while walking a stream's landed records.
type Gap struct {
	File     string
	Row      uint32
	Expected uint64
	Got      uint64
}

func (g Gap) String() string {
	return fmt.Sprintf("%s row %d: expected %d, got %d", filepath.Base(g.File), g.Row, g.Expected, g.Got)
}

// EndGap says a stream's landed records stop before the point its cursor says
// the collector read to. Those lines were read, and no landed file holds them.
type EndGap struct {
	Cursor string
	// Ord and Offset are where the cursor says reading stopped: the last line
	// read, and the byte after it.
	Ord, Offset uint64
	// LastOrd and Covered are where the landed records stop.
	LastOrd, Covered uint64
}

func (g EndGap) String() string {
	// The cursor's file name is the same in every stream, so the stream's
	// directory names which one it is.
	name := filepath.ToSlash(filepath.Join(filepath.Base(filepath.Dir(filepath.Dir(g.Cursor))),
		filepath.Base(filepath.Dir(g.Cursor)), filepath.Base(g.Cursor)))
	// A cursor that counts records and not bytes, as the mock's does, has no
	// byte to compare.
	if g.Offset == 0 {
		return fmt.Sprintf("%s: read to line %d, the landed records end at line %d", name, g.Ord, g.LastOrd)
	}
	return fmt.Sprintf("%s: read to line %d and byte %d, the landed records end at line %d and byte %d",
		name, g.Ord, g.Offset, g.LastOrd, g.Covered)
}

// StreamReport is the result of checking one stream or run directory.
type StreamReport struct {
	Dir     string
	Files   int
	Records int

	FirstOrd, LastOrd uint64
	// BytesCovered is the source byte range the landed records account for.
	BytesCovered uint64

	OrdGaps  []Gap // a source line was skipped
	ByteGaps []Gap // a source byte range is unaccounted for
	ShaBad   []Gap // a payload does not match its recorded digest
	// End is set when the landed records stop before where the stream's
	// cursor says the collector read to.
	End *EndGap

	// Relanded counts records that repeat a range already landed.
	//
	// This is not a problem. The collector lands data BEFORE it commits the
	// cursor, so a crash between the two leaves the data on disk and the cursor
	// behind it, and the next pass lands the same bytes again. At-least-once is
	// the deliberate choice; the other order would lose data instead of
	// repeating it. The assembler removes the repeats by record id.
	Relanded int
}

// OK reports whether the stream is contiguous and intact.
func (r *StreamReport) OK() bool {
	return len(r.OrdGaps) == 0 && len(r.ByteGaps) == 0 && len(r.ShaBad) == 0 && r.End == nil
}

// Stream checks one stream or run directory for a given landed kind.
//
// It asserts four properties, all provable from the storage root alone:
//
//   - ORD CONTIGUITY: source line numbers run 1, 2, 3 ... with no gap, and the
//     first is 1. A gap means the tailer skipped a line, or a landed file was
//     lost, which is otherwise invisible: no error, no conflict, just a
//     slightly shorter conversation.
//   - BYTE CONTIGUITY: the first record starts at byte 0, and
//     off[n] + len(payload[n]) + 1 == off[n+1], the +1 being the newline the
//     source had. This proves every byte of the source prefix is accounted
//     for, without needing the source.
//   - THE CURSOR: where the stream has an append cursor, the landed records
//     reach the line and the byte it says the collector read to. Without this
//     a lost last file looks like a stream that simply ends there.
//   - INTEGRITY: each file's own closing digest covers every line before it, so
//     a file edited or cut short after it was written is caught. Reading is what
//     performs that check, so a failure here surfaces as a read error rather
//     than as a count.
//
// It deliberately does NOT treat a REPEAT as a problem. Going backwards - a
// record whose position is at or before one already seen - is what an
// interrupted pass leaves behind, because the collector lands data before it
// commits the cursor. Only going FORWARDS past unaccounted bytes is data loss.
// For the same reason a cursor BEHIND the landed records is not a problem.
func Stream(dir, kind string) (*StreamReport, error) {
	// The cursor is read before the files are listed. The collector lands a
	// file before it saves the cursor, so every record this cursor counts is
	// already on disk when it is read. A collector running beside the check
	// then cannot make the cursor look ahead of the files.
	cursorPath := filepath.Join(dir, kind+".cursor")
	cur, err := storage.LoadCursor(cursorPath, storage.CursorAppend, "")
	if err != nil {
		return nil, err
	}
	files, err := landedFiles(dir, kind)
	if err != nil {
		return nil, err
	}
	rep := &StreamReport{Dir: dir, Files: len(files)}

	// Every source starts at line 1 and byte 0, so the first record is held to
	// that position exactly as a later record is held to the end of the one
	// before it. Starting from whatever the first record said would accept a
	// stream whose first landed file is lost.
	var prevOrd, prevEnd uint64
	first := true

	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		r, err := sessiondata.NewReader(f)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("verify: %s: %w", path, err)
		}
		for row := uint32(1); ; row++ {
			rec, rerr := r.Next()
			if errors.Is(rerr, io.EOF) {
				break
			}
			if rerr != nil {
				f.Close()
				return nil, fmt.Errorf("verify: %s row %d: %w", path, row, rerr)
			}
			rep.Records++

			if first {
				rep.FirstOrd = rec.Ord
				first = false
			}
			if rec.Ord <= prevOrd {
				// A repeat of ground already covered. The bytes are still here;
				// they are here twice.
				rep.Relanded++
			} else {
				// A line gap and a byte gap are independent facts, and a stream can
				// have one without the other, so both are checked.
				if rec.Ord != prevOrd+1 {
					rep.OrdGaps = append(rep.OrdGaps, Gap{path, row, prevOrd + 1, rec.Ord})
				}
				if rec.Off != prevEnd {
					rep.ByteGaps = append(rep.ByteGaps, Gap{path, row, prevEnd, rec.Off})
				}
			}

			// A repeat must not pull the watermark backwards, or every record
			// after it would look like a gap.
			if rec.Ord > prevOrd {
				prevOrd = rec.Ord
				// +1 for the newline the source carried and the record does not.
				prevEnd = rec.Off + uint64(rec.Bytes) + 1
				rep.LastOrd = rec.Ord
			}
		}
		f.Close()
	}
	rep.BytesCovered = prevEnd

	// A snapshot cursor tracks a digest, not lines, and a stream with no cursor
	// has nothing to say where reading stopped. Only an append cursor ahead of
	// the records is a loss. One loss at the end is one problem, whether the
	// cursor counts lines, bytes or both.
	if cur.Kind == storage.CursorAppend && (cur.Ord > rep.LastOrd || cur.Offset > rep.BytesCovered) {
		rep.End = &EndGap{Cursor: cursorPath, Ord: cur.Ord, Offset: cur.Offset,
			LastOrd: rep.LastOrd, Covered: rep.BytesCovered}
	}
	return rep, nil
}

// landedFiles lists a directory's landed files of one kind, in sequence order.
func landedFiles(dir, kind string) ([]string, error) {
	items, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	type sf struct {
		path string
		seq  uint64
	}
	var out []sf
	for _, it := range items {
		name := it.Name()
		if it.IsDir() || !strings.HasPrefix(name, kind+"-") || !strings.HasSuffix(name, ".sd") {
			continue
		}
		base := strings.TrimSuffix(name, ".sd")
		i := strings.LastIndex(base, "-")
		if i < 0 {
			continue
		}
		seq, err := strconv.ParseUint(base[i+1:], 10, 64)
		if err != nil {
			continue
		}
		out = append(out, sf{filepath.Join(dir, name), seq})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seq < out[j].seq })
	paths := make([]string, len(out))
	for i, x := range out {
		paths[i] = x.path
	}
	return paths, nil
}
