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

package claudecode

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// Covered reports whether collecting s now would land nothing, because every
// source of it is landed to its end. A scenario removal asks this before it
// deletes a session's sources. A source deleted with a line not yet landed
// would lose that line.
//
// Each source is read against the adapter's own cursor, as a pass reads it,
// and nothing is written. The caller holds the session's lock, so no pass
// moves a cursor meanwhile.
//
// ok is false with a reason, and no error, when a pass would land something
// now: the session only has to wait. An error means a source cannot be
// covered until a person looks at it. That is a cursor in conflict, a source
// that conflicts with its cursor, a source that is gone, two sources that map
// to one cursor, or a read that failed.
func (c *Collector) Covered(s Session) (ok bool, reason string, err error) {
	// Checked for every source before any is read. A pass refuses the second
	// of two sources that map to one cursor, so that source is never landed,
	// and the session must say so even while another source is still
	// waiting to land.
	claimed := make(map[string]string, len(s.Sources))
	for _, src := range s.Sources {
		dir, cursorPath, _ := c.cursorPaths(src)
		if dir == "" {
			return false, "", fmt.Errorf("claudecode: unknown source kind for %s", src.Rel)
		}
		if prev, dup := claimed[cursorPath]; dup && prev != src.Rel {
			return false, "", fmt.Errorf("claudecode: %s and %s both map to %s, so one of them is never landed", prev, src.Rel, cursorPath)
		}
		claimed[cursorPath] = src.Rel
	}
	for _, src := range s.Sources {
		_, cursorPath, _ := c.cursorPaths(src)
		if src.Kind.Append() {
			ok, reason, err = CoveredAppend(src.Path, src.Rel, cursorPath, c.MaxDelta)
		} else {
			ok, reason, err = coveredSnapshot(src, cursorPath)
		}
		if err != nil || !ok {
			return ok, reason, err
		}
	}
	return true, "", nil
}

// CoveredAppend reports whether an append source is landed to its end: a pass
// would read no line from it now. Both adapters tail their sources with
// TailAppend, so both apply this one rule. The results mean what they mean
// for Covered.
//
// TailAppend decides, not a comparison of inodes. The same bytes under a new
// identity are a move, not new data. A line not yet ended by a newline is
// left behind by a pass, so a source that ends in one is not covered. A
// 0-byte source with no cursor is covered, since there is nothing to land.
func CoveredAppend(path, rel, cursorPath string, maxBytes int64) (bool, string, error) {
	cur, err := storage.LoadCursor(cursorPath, storage.CursorAppend, rel)
	if err != nil {
		return false, "", err
	}
	if cur.State == storage.CursorConflict {
		return false, "", fmt.Errorf("%s conflicts with its cursor, and collection stopped there", rel)
	}
	if maxBytes <= 0 {
		maxBytes = 2 << 20
	}
	chunk, err := TailAppend(path, cur, maxBytes)
	if err != nil {
		if errors.Is(err, ErrSourceGone) {
			return false, "", fmt.Errorf("%s is gone, so whether all of it landed cannot be told", rel)
		}
		return false, "", err
	}
	if len(chunk.Lines) > 0 || int64(chunk.NewOffset) != chunk.Size {
		return false, rel + " is not landed to its end yet", nil
	}
	return true, "", nil
}

// coveredSnapshot reports whether a snapshot source would land nothing. That
// is so when its body is empty, which lands nothing and saves no cursor, or
// when its digest is the one its cursor recorded. It is the test landSnapshot
// applies.
func coveredSnapshot(src Source, cursorPath string) (bool, string, error) {
	cur, err := storage.LoadCursor(cursorPath, storage.CursorSnapshot, src.Rel)
	if err != nil {
		return false, "", err
	}
	if cur.State == storage.CursorConflict {
		return false, "", fmt.Errorf("%s conflicts with its cursor, and collection stopped there", src.Rel)
	}
	data, err := os.ReadFile(src.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, "", fmt.Errorf("%s is gone, so whether it landed cannot be told", src.Rel)
		}
		return false, "", err
	}
	body := trimTrailingNewline(data)
	if len(body) == 0 {
		return true, "", nil
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) == cur.ContentSHA256 {
		return true, "", nil
	}
	return false, src.Rel + " is not landed to its end yet", nil
}
