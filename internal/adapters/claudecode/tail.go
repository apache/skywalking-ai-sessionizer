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
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// ErrSourceGone means the source file no longer exists.
//
// This is an expected state, not a failure: Claude Code prunes transcripts, and
// our landed deltas deliberately outlive them.
var ErrSourceGone = errors.New("claudecode: source gone")

// ConflictError means the source changed underneath the cursor. Collection
// stops rather than resuming from a position that no longer means what it did.
type ConflictError struct {
	Path   string
	Reason string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("claudecode: conflict on %s: %s", e.Path, e.Reason)
}

// Line is one source record with its position in the source file.
type Line struct {
	Ord   uint64
	Off   uint64
	Bytes []byte
}

// Chunk is what one tail pass consumed.
type Chunk struct {
	Lines     []Line
	NewOffset uint64
	NewOrd    uint64
	LastUUID  string
	// More reports that the byte budget was reached and the source has further
	// complete lines waiting, so the caller should drain again.
	More bool

	// Dev, Ino and Size are the identity observed when the data was READ.
	// Re-stat'ing the path after landing would hide a rotation that happened
	// during the pass into the cursor, defeating the next pass's check.
	Dev, Ino uint64
	Size     int64
	MTime    int64

	// Moved reports that the file's identity changed but the bytes before the
	// cursor were verified intact. The caller must record the new identity
	// even when nothing landed, or every later pass re-reads the tail window
	// to verify it again.
	Moved bool
}

// statInfo carries the identity and size checks a cursor is validated against.
type statInfo struct {
	dev, ino uint64
	size     int64
	mtime    int64
}

func statSource(path string) (statInfo, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return statInfo{}, ErrSourceGone
		}
		return statInfo{}, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return statInfo{size: fi.Size(), mtime: fi.ModTime().Unix()}, nil
	}
	return statInfo{
		dev: uint64(st.Dev), ino: uint64(st.Ino),
		size: fi.Size(), mtime: fi.ModTime().Unix(),
	}, nil
}

// TailAppend reads new complete lines from an append-only source.
//
// It advances only to the last complete newline; an unterminated tail is an
// in-flight write and is deliberately left for the next pass. Reading it would
// land a record the writer has not finished producing.
func TailAppend(path string, cur *storage.Cursor, maxBytes int64) (*Chunk, error) {
	info, err := statSource(path)
	if err != nil {
		return nil, err
	}

	fresh := cur.Ino == 0 && cur.Offset == 0
	moved := false
	if !fresh {
		if info.size < int64(cur.Offset) {
			return nil, &ConflictError{Path: path, Reason: fmt.Sprintf("truncated: size %d < offset %d", info.size, cur.Offset)}
		}
		// A different inode under the same path usually means the file was
		// replaced. It also happens when the same file is reached through
		// another filesystem: a bind mount into a container, a restored backup,
		// a copied source tree. Measured: every source collected on a host
		// conflicted inside a container when the inode alone decided. So the
		// bytes decide. The window before the cursor is verified below, and
		// only a mismatch is a conflict.
		if cur.Ino != 0 && info.ino != 0 && cur.Ino != info.ino {
			if cur.TailSHA256 == "" {
				return nil, &ConflictError{Path: path, Reason: "inode changed and the cursor has no tail digest to verify the bytes with"}
			}
			moved = true
		}
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrSourceGone
		}
		return nil, err
	}
	defer f.Close()

	// Nothing new: return before verifying the tail digest. Verifying on every
	// pass would re-read a 1 MiB window for every one of several thousand
	// unchanged sources. A rewrite is only actionable when we are about to read
	// past it, so the check belongs on the path that actually reads.
	if info.size == int64(cur.Offset) && !moved {
		return &Chunk{NewOffset: cur.Offset, NewOrd: cur.Ord, LastUUID: cur.LastUUID,
			Dev: info.dev, Ino: info.ino, Size: info.size, MTime: info.mtime}, nil
	}

	if !fresh && cur.TailSHA256 != "" {
		sum, err := tailDigest(f, int64(cur.Offset))
		if err != nil {
			return nil, err
		}
		if sum != cur.TailSHA256 {
			return nil, &ConflictError{Path: path, Reason: "tail digest mismatch (source rewritten behind cursor)"}
		}
	}
	if info.size == int64(cur.Offset) {
		// Nothing new, but the identity changed and the bytes checked out.
		return &Chunk{NewOffset: cur.Offset, NewOrd: cur.Ord, LastUUID: cur.LastUUID,
			Dev: info.dev, Ino: info.ino, Size: info.size, MTime: info.mtime, Moved: true}, nil
	}

	buf, err := readWindow(f, int64(cur.Offset), info.size, maxBytes)
	if err != nil {
		return nil, err
	}

	// Consume only up to the last complete newline in the window.
	end := bytes.LastIndexByte(buf, '\n')
	if end < 0 && int64(len(buf)) >= maxBytes && int64(cur.Offset)+int64(len(buf)) < info.size {
		// The window is full and there is more file beyond it, so this single
		// line is larger than the byte budget. Reading only the budget would
		// stall this source forever, silently, with no error - so grow past the
		// budget to find the line's end. The budget bounds throughput, not the
		// maximum record we are willing to store.
		buf, err = readWindow(f, int64(cur.Offset), info.size, 0)
		if err != nil {
			return nil, err
		}
		end = bytes.LastIndexByte(buf, '\n')
	}
	if end < 0 {
		// Still no complete line: an in-flight write. Nothing safe to land yet.
		return &Chunk{NewOffset: cur.Offset, NewOrd: cur.Ord, LastUUID: cur.LastUUID,
			Dev: info.dev, Ino: info.ino, Size: info.size, MTime: info.mtime, Moved: moved}, nil
	}
	consumed := buf[:end+1]

	ch := &Chunk{NewOrd: cur.Ord, LastUUID: cur.LastUUID,
		Dev: info.dev, Ino: info.ino, Size: info.size, MTime: info.mtime, Moved: moved}
	off := cur.Offset
	ord := cur.Ord
	for _, raw := range bytes.SplitAfter(consumed, []byte("\n")) {
		if len(raw) == 0 {
			continue
		}
		ord++
		body := bytes.TrimSuffix(raw, []byte("\n"))
		ch.Lines = append(ch.Lines, Line{Ord: ord, Off: off, Bytes: body})
		if u := extractUUID(body); u != "" {
			ch.LastUUID = u
		}
		off += uint64(len(raw))
	}
	ch.NewOffset = off
	ch.NewOrd = ord
	ch.More = int64(off) < info.size
	return ch, nil
}

// readWindow reads at most maxBytes starting at from.
func readWindow(f *os.File, from, size, maxBytes int64) ([]byte, error) {
	want := size - from
	if maxBytes > 0 && want > maxBytes {
		want = maxBytes
	}
	buf := make([]byte, want)
	n, err := f.ReadAt(buf, from)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:n], nil
}

// tailDigest hashes the bytes immediately before offset.
//
// A window rather than the whole consumed prefix: re-hashing [0,offset) would
// mean re-reading tens of megabytes every pass. The window is sized above the
// largest observed transcript line so it always spans a record boundary.
func tailDigest(f *os.File, offset int64) (string, error) {
	if offset == 0 {
		return "", nil
	}
	start := offset - storage.TailWindow
	if start < 0 {
		start = 0
	}
	buf := make([]byte, offset-start)
	if _, err := f.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:]), nil
}

// TailDigestAt computes the tail digest for a file at a given offset.
func TailDigestAt(path string, offset int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return tailDigest(f, offset)
}

var uuidKey = []byte(`"uuid":"`)

// extractUUID pulls a record's uuid without a full JSON parse.
//
// Many record types carry no uuid at all, so an empty result is normal. The
// value is diagnostic only - position is authoritative, never this.
func extractUUID(line []byte) string {
	i := bytes.Index(line, uuidKey)
	if i < 0 {
		return ""
	}
	rest := line[i+len(uuidKey):]
	j := bytes.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return string(rest[:j])
}
