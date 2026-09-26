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

package claudecodechanges

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// Collector lands the plugin's change files into a storage zone.
type Collector struct {
	SourceRoot string
	Zone       *storage.Zone
	MaxDelta   int64
	Now        func() time.Time
}

// Stats summarises one collection pass. The fields are the ones the local
// adapter reports, so a caller can add the two passes together.
type Stats struct {
	Sessions      int
	SourcesSeen   int
	SourcesLanded int
	Records       int
	Bytes         int64
	SourcesGone   int
	Conflicts     int
	Busy          int
	Pending       int
	Indexed       int
	Reindexed     int
	Errors        []error
	// Changed lists the sessions whose landed data or index moved this pass.
	Changed []string
}

// Complete reports whether the pass collected everything available.
func (s *Stats) Complete() bool {
	return s.Pending == 0 && s.Conflicts == 0 && len(s.Errors) == 0
}

// maxDrainRounds bounds how many windows one source may land in a pass.
const maxDrainRounds = 64

// New returns a Collector with sensible defaults.
func New(sourceRoot string, zone *storage.Zone, maxDelta int64) *Collector {
	if maxDelta <= 0 {
		maxDelta = 2 << 20
	}
	return &Collector{SourceRoot: sourceRoot, Zone: zone, MaxDelta: maxDelta, Now: time.Now}
}

// CollectAll discovers sessions and lands whatever is new in each.
func (c *Collector) CollectAll(filter func(Session) bool) (*Stats, error) {
	sessions, warnings, err := DiscoverWithWarnings(c.SourceRoot)
	if err != nil {
		return nil, err
	}
	st := &Stats{}
	st.Errors = append(st.Errors, warnings...)
	for _, s := range sessions {
		if filter != nil && !filter(s) {
			continue
		}
		st.Sessions++
		if err := c.collectSession(s, st); err != nil {
			st.Errors = append(st.Errors, fmt.Errorf("session %s: %w", s.ID, err))
		}
	}
	return st, nil
}

// collectSession lands one session's change files under the session's
// lock, with the session's own sequence, beside whatever the transcript
// adapter landed. The two adapters never run on one session at once: the
// lock is the same, and a busy session is skipped and met again next pass.
func (c *Collector) collectSession(s Session, st *Stats) error {
	sessionDir := c.Zone.SessionDir(s.ID)
	lock, err := storage.LockSession(sessionDir)
	if err != nil {
		if errors.Is(err, storage.ErrSessionBusy) {
			st.Busy++
			return nil
		}
		return err
	}
	defer func() { _ = lock.Unlock() }()

	indexDir := c.Zone.IndexDir(s.ID)
	ixState, err := storage.LoadIndexState(c.Zone.IndexStatePath(s.ID), s.ID)
	if err != nil {
		return err
	}
	statePath := c.Zone.SessionStatePath(s.ID)
	state, err := storage.LoadSessionState(statePath, s.ID)
	if err != nil {
		return err
	}
	if err := state.RecoverNextSeq(sessionDir); err != nil {
		return err
	}
	// Only an index that agrees with its saved state is extended; one that
	// does not is built again. See index.LoadFor for the crash it guards.
	ix, ok, err := index.LoadFor(indexDir, s.ID, ixState)
	if err != nil {
		return err
	}
	if !ok {
		ix = index.New(s.ID)
		ixState = storage.NewIndexState(s.ID)
	}
	// Close any gap between what has landed and what the index covers, for
	// the same reason the transcript adapter does: cursors commit per source
	// and the index once per session, so a crash between them leaves landed
	// files the index does not describe.
	reindexed := false
	if landedTo := state.NextSeq - 1; ixState.IndexedSeq < landedTo {
		n, rerr := index.Rebuild(c.Zone, s.ID, ix, ixState.IndexedSeq)
		if rerr != nil {
			return rerr
		}
		st.Reindexed += n
		ixState.IndexedSeq = landedTo
		reindexed = true
	}

	now := c.Now()
	changed := false
	for _, src := range s.Sources {
		st.SourcesSeen++
		var landedAny bool
		for round := 0; ; round++ {
			landed, more, err := c.collectSource(src, ix, state, st, now)
			if err != nil {
				st.Errors = append(st.Errors, err)
				break
			}
			landedAny = landedAny || landed
			if !more {
				break
			}
			if round >= maxDrainRounds {
				st.Pending++
				break
			}
		}
		if landedAny {
			changed = true
			st.SourcesLanded++
		}
	}
	if changed || reindexed {
		st.Changed = append(st.Changed, s.ID)
		ixState.Schema = index.Schema
		ixState.IndexedSeq = state.NextSeq - 1
		ixState.Entries = len(ix.Entries)
		ixState.Blocks = len(ix.Blocks)
		ixState.Strings = ix.Strings.Len()
		if err := ix.Write(indexDir); err != nil {
			return err
		}
		if err := ixState.Save(c.Zone.IndexStatePath(s.ID), now); err != nil {
			return err
		}
		st.Indexed += ixState.Entries
	}
	state.LastScan = now.UTC().Format(time.RFC3339Nano)
	return state.Save(statePath, now)
}

// cursorFor is where one source's position is kept.
//
// Two installations of the plugin can write the same session and stream: the
// old plugin name and the new one both land under the same data directory,
// and a session id is the same in both. They are different files, and one
// cursor for both is wrong in a way that stops collection for good - once
// the newer file passes the older one's length, the older is read against a
// position past its end, which is a truncation conflict, and after that
// neither file is collected again.
//
// The first source to arrive keeps the plain name, so nothing already
// landed is landed twice. Any other source gets its own, named for the
// source itself so it is the same name on every pass.
//
// A source is its path relative to its recorder root, and the root: two
// recorder directories read by one collector configuration can each hold
// the same session under the same relative path - a shim's flat directory
// and another one - and they are different files. A cursor written before
// 0.5.0 names no root. It belongs to the root whose file it was read from,
// which the cursor says twice: the file's identity, and the bytes before
// its offset. A root whose file is another takes a cursor of its own
// rather than reading its file against another's position, which is the
// conflict that stopped both for good.
func cursorFor(dir, root string, src Source) (string, error) {
	// A landed file's name prefix is its kind, and so is its cursor's, so
	// a stream's change file and execution file keep separate places.
	prefix := string(src.Kind)
	plain := filepath.Join(dir, prefix+".cursor")
	held, err := storage.LoadCursor(plain, storage.CursorAppend, src.Rel)
	if err != nil {
		return "", err
	}
	switch {
	case held.Source == "", held.Source == src.Rel && held.Origin == root:
		return plain, nil
	case held.Source == src.Rel && held.Origin == "" && describes(held, src.Path):
		return plain, nil
	}
	sum := sha256.Sum256([]byte(root + "\n" + src.Rel))
	return filepath.Join(dir, prefix+"-"+hex.EncodeToString(sum[:])[:12]+".cursor"), nil
}

// describes reports whether a cursor that names no root was read from this
// file: nothing consumed yet, or the file is the one the cursor recorded -
// the same device and the same inode, since two filesystems can give one
// inode number to unrelated files - and the digest of the window before
// the offset is the cursor's. The bytes alone are not enough: two files
// can share their last megabyte and differ before it, and a cursor claimed
// on the bytes alone would skip what the other file holds before them. A
// file that cannot be read is not claimed. A legacy cursor whose file was
// since copied, restored or reached through another mount, so its identity
// changed, is not claimed either: that source is read again from its start
// behind a cursor of its own, and the index keeps the first record it
// holds for an id.
func describes(cur *storage.Cursor, path string) bool {
	if cur.Offset == 0 {
		return true
	}
	if cur.TailSHA256 == "" || cur.Ino == 0 {
		return false
	}
	dev, ino, err := claudecode.Identity(path)
	if err != nil || ino == 0 || ino != cur.Ino || dev != cur.Dev {
		return false
	}
	got, err := claudecode.TailDigestAt(path, int64(cur.Offset))
	return err == nil && got == cur.TailSHA256
}

// origin is how this collector's root is written into a cursor, with
// forward slashes so it reads the same on every pass. A root inside the
// storage root - a scenario's, which a test copies whole to another place
// - is written relative to it, so the copy keeps its cursors; any other
// root is written absolute, since a source directory that moves is a
// different source.
func (c *Collector) origin() string {
	root := filepath.Clean(c.SourceRoot)
	if rel, err := filepath.Rel(c.Zone.Root(), root); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(root)
}

// collectSource lands one window of one file, and says whether more waits.
func (c *Collector) collectSource(src Source, ix *index.Index, state *storage.SessionState, st *Stats, now time.Time) (landed, more bool, err error) {
	dir := c.Zone.StreamDir(src.Session, src.Stream)
	cursorPath, err := cursorFor(dir, c.origin(), src)
	if err != nil {
		return false, false, err
	}
	cur, err := storage.LoadCursor(cursorPath, storage.CursorAppend, src.Rel)
	if err != nil {
		return false, false, err
	}
	// A cursor from before 0.5.0 names no root. It is written once with
	// this one, even on a pass that lands nothing, so the next root to
	// read the same relative path finds it claimed.
	claim := cur.Origin == ""
	cur.Origin = c.origin()
	if cur.State == storage.CursorConflict {
		st.Conflicts++
		return false, false, nil
	}
	chunk, err := claudecode.TailAppend(src.Path, cur, c.MaxDelta)
	switch {
	case errors.Is(err, claudecode.ErrSourceGone):
		st.SourcesGone++
		if cur.State == storage.CursorSourceGone {
			return false, false, nil
		}
		cur.State = storage.CursorSourceGone
		return false, false, cur.Save(cursorPath, now)
	case err != nil:
		var ce *claudecode.ConflictError
		if errors.As(err, &ce) {
			cur.State = storage.CursorConflict
			st.Conflicts++
			_ = cur.Save(cursorPath, now)
		}
		return false, false, err
	}
	if len(chunk.Lines) == 0 {
		if chunk.Moved {
			cur.Dev, cur.Ino = chunk.Dev, chunk.Ino
			cur.Size, cur.MTime = chunk.Size, chunk.MTime
			return false, false, cur.Save(cursorPath, now)
		}
		if claim {
			return false, false, cur.Save(cursorPath, now)
		}
		return false, false, nil
	}

	seq := state.Take()
	name := storage.LandedName(string(src.Kind), storage.Stamp(now), seq)
	hdr := &sessiondata.Header{
		H: 1, Seq: seq, At: now.UTC().Format(time.RFC3339Nano),
		Kind: src.Kind, Adapter: Name + "/" + Version, Dialect: Dialect,
		Src: src.Rel, Session: src.Session, Stream: src.Stream,
	}
	var written int64
	err = storage.WriteAtomic(filepath.Join(dir, name), storage.PermLanded, func(w io.Writer) error {
		rw, err := sessiondata.NewWriter(w, hdr)
		if err != nil {
			return err
		}
		for row, ln := range chunk.Lines {
			rec := ConvertKind(src.Kind, ln.Ord, ln.Off, ln.Bytes)
			if err := rw.Write(rec); err != nil {
				return err
			}
			written += int64(len(ln.Bytes))
			e, blocks, body := index.FromRecord(ix, hdr, rec, uint32(seq), uint32(row+1))
			ix.AppendRecord(e, blocks, body)
		}
		return rw.Close()
	})
	if err != nil {
		return false, false, err
	}
	// Land before committing the cursor, as the transcript adapter does: a
	// crash between the two lands the same lines again, and assembly and
	// the view drop the repeat by record id.
	digest, err := claudecode.TailDigestAt(src.Path, int64(chunk.NewOffset))
	if err != nil && !os.IsNotExist(err) {
		return true, false, err
	}
	cur.Dev, cur.Ino = chunk.Dev, chunk.Ino
	cur.Offset, cur.Ord = chunk.NewOffset, chunk.NewOrd
	cur.LastUUID, cur.TailSHA256 = chunk.LastUUID, digest
	cur.Size, cur.MTime = chunk.Size, chunk.MTime
	cur.LastSeq, cur.State = seq, storage.CursorActive
	st.Records += len(chunk.Lines)
	st.Bytes += written
	return true, chunk.More, cur.Save(cursorPath, now)
}
