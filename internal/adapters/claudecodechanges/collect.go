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
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	// Retired names the sessions found that are retired. Their records are
	// not read, and nothing of them is written, a cursor included.
	Retired []string
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
		// Before anything: claiming an older cursor saves it without
		// landing a record, and taking the lock makes the directory.
		if retired, err := storage.Retired(c.Zone, s.ID); err != nil || retired {
			if err != nil {
				st.Errors = append(st.Errors, fmt.Errorf("session %s: %w", s.ID, err))
			} else {
				st.Retired = append(st.Retired, s.ID)
			}
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
//
// One path can also hold one file after another. The plugin removes a file
// once it was not written to for retention.ttl, and writes a new one at the
// same path when the session goes on. Read against the old cursor, the new
// file was a conflict, and nothing more from that path was collected. So
// each file at a path has a cursor of its own: see followFiles.
func cursorFor(dir, root string, src Source) (string, error) {
	// A landed file's name prefix is its kind, and so is its cursor's, so
	// a stream's change file and execution file keep separate places.
	prefix := string(src.Kind)
	path := filepath.Join(dir, prefix+".cursor")
	held, err := storage.LoadCursor(path, storage.CursorAppend, src.Rel)
	if err != nil {
		return "", err
	}
	switch {
	case held.Source == "", held.Source == src.Rel && held.Origin == root:
	case held.Source == src.Rel && held.Origin == "" && describes(held, src.Path):
	default:
		path = filepath.Join(dir, storage.SourceCursorName(prefix, root, src.Rel))
		if held, err = storage.LoadCursor(path, storage.CursorAppend, src.Rel); err != nil {
			return "", err
		}
	}
	return followFiles(dir, prefix, root, src, path, held)
}

// followFiles follows the cursors of one path, from the first file the path
// held to the file it holds now. A cursor records the digest of its file's
// first line. A file that does not begin with that line is a later file, and
// its cursor is named for the path and that line, so it is the same name on
// every pass. The plugin never writes a line twice, so two files it writes
// never begin with the same line. A file rewritten behind its cursor still
// begins with its first line, so it stays with that cursor, and is still a
// conflict there.
//
// The first line is read on every pass, even when the file has the inode the
// cursor recorded and the size it read to: a file system can give a new file
// the inode of the one removed before it, and a new file of the same length
// was taken for the old one and never read. A file that holds no complete
// line yet is not the file of a cursor that read one, so it goes to a cursor
// of its own, which waits for that line.
func followFiles(dir, prefix, root string, src Source, path string, cur *storage.Cursor) (string, error) {
	var head string
	read := false
	seen := map[string]bool{}
	for cur.HeadSHA256 != "" {
		if !read {
			h, err := firstLine(src.Path)
			if errors.Is(err, os.ErrNotExist) {
				// The file went after discovery listed it. TailAppend says
				// so, behind the cursor it had.
				return path, nil
			}
			if err != nil {
				return "", err
			}
			head, read = h, true
		}
		if head == cur.HeadSHA256 {
			return path, nil
		}
		seen[path] = true
		path = filepath.Join(dir, storage.LaterCursorName(prefix, root, src.Rel, cur.HeadSHA256))
		if seen[path] {
			return "", fmt.Errorf("claudecodechanges: the cursors of %s name each other in a loop at %s", src.Rel, path)
		}
		var err error
		if cur, err = storage.LoadCursor(path, storage.CursorAppend, src.Rel); err != nil {
			return "", err
		}
	}
	return path, nil
}

// landedSrc is the src a source's files land with. The first source at a
// path lands under the path. A later file at the path, or the same path under
// a second recorder root, has a cursor of its own, and lands under the path
// marked with its first line, see storage.MarkedSource. Its records number
// their lines from 1 again, so without its own src a reader of the landed
// files took them for lines landed twice. A cursor with no first line has
// read nothing yet, or read before 0.6.0, when its files landed under the
// path.
func landedSrc(src Source, cursorPath string, cur *storage.Cursor) string {
	if filepath.Base(cursorPath) == string(src.Kind)+".cursor" || cur.HeadSHA256 == "" {
		return src.Rel
	}
	return storage.MarkedSource(src.Rel, cur.HeadSHA256)
}

// firstLine is the digest of a file's first line without its newline, or ""
// when the file holds no complete line yet. It is read in pieces, since one
// line can be several megabytes.
func firstLine(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 64<<10)
	for {
		n, err := f.Read(buf)
		if i := bytes.IndexByte(buf[:n], '\n'); i >= 0 {
			h.Write(buf[:i])
			return hex.EncodeToString(h.Sum(nil)), nil
		}
		h.Write(buf[:n])
		if errors.Is(err, io.EOF) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
	}
}

// lineDigest is the digest firstLine gives for a file that begins with b.
func lineDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
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

// origin is how this collector's root is written into a cursor. The rule
// lives in storage, because asz repack writes it again for a new root.
func (c *Collector) origin() string {
	return storage.CursorOrigin(c.Zone.Root(), c.SourceRoot)
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

	// The first line is taken from the bytes read, not from the file now,
	// which may already be a later one.
	if cur.HeadSHA256 == "" && chunk.Lines[0].Off == 0 {
		cur.HeadSHA256 = lineDigest(chunk.Lines[0].Bytes)
	}
	seq := state.Take()
	name := storage.LandedName(string(src.Kind), storage.Stamp(now), seq)
	hdr := &sessiondata.Header{
		H: 1, Seq: seq, At: now.UTC().Format(time.RFC3339Nano),
		Kind: src.Kind, Adapter: Name + "/" + Version, Dialect: Dialect,
		Src: landedSrc(src, cursorPath, cur), Session: src.Session, Stream: src.Stream,
	}
	// A record is larger than its line, so the file ends where the converted
	// records reach the budget, and the cursor stops before the first line
	// left out.
	cut, err := sessiondata.NewCut(hdr, c.MaxDelta)
	if err != nil {
		state.GiveBack(seq)
		return false, false, err
	}
	var recs []*sessiondata.Record
	for _, ln := range chunk.Lines {
		rec := ConvertKind(src.Kind, ln.Ord, ln.Off, ln.Bytes)
		ok, err := cut.Take(rec)
		if err != nil {
			state.GiveBack(seq)
			return false, false, err
		}
		if !ok {
			break
		}
		recs = append(recs, rec)
	}
	chunk.Keep(len(recs), cur.LastUUID)
	var written int64
	var staged index.Staged
	err = storage.WriteAtomic(filepath.Join(dir, name), storage.PermLanded, func(w io.Writer) error {
		rw, err := sessiondata.NewWriter(w, hdr)
		if err != nil {
			return err
		}
		rw.Budget(c.MaxDelta)
		for row, ln := range chunk.Lines {
			rec := recs[row]
			if err := rw.Write(rec); err != nil {
				return err
			}
			written += int64(len(ln.Bytes))
			staged.Add(index.FromRecord(ix, hdr, rec, uint32(seq), uint32(row+1)))
		}
		return rw.Close()
	})
	if err != nil {
		state.GiveBack(seq)
		return false, false, err
	}
	ix.AppendStaged(&staged)
	// Land before committing the cursor, as the transcript adapter does: a
	// crash between the two lands the same lines again, and assembly and
	// the view drop the repeat by record id.
	digest, err := claudecode.TailDigestAt(src.Path, int64(chunk.NewOffset))
	if err != nil && !os.IsNotExist(err) {
		return true, false, err
	}
	cur.Source = hdr.Src
	cur.Dev, cur.Ino = chunk.Dev, chunk.Ino
	cur.Offset, cur.Ord = chunk.NewOffset, chunk.NewOrd
	cur.LastUUID, cur.TailSHA256 = chunk.LastUUID, digest
	cur.Size, cur.MTime = chunk.Size, chunk.MTime
	cur.LastSeq, cur.State = seq, storage.CursorActive
	st.Records += len(chunk.Lines)
	st.Bytes += written
	return true, chunk.More, cur.Save(cursorPath, now)
}
