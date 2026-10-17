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

package kiro

import (
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

// Collector lands Kiro V3 sessions into a storage zone.
//
// A session removed whole, as Kiro Crew removes the sessions of a task's
// steps, is not found again and not visited again. Its landed files stay, and
// they are the record of it.
type Collector struct {
	SourceRoot string
	Zone       *storage.Zone
	MaxDelta   int64
	Now        func() time.Time
}

// pass carries one session's collection state. The index lives here and not
// on the Collector because it is one session's mutable state.
type pass struct {
	ix    *index.Index
	state *storage.SessionState
	st    *Stats
	now   time.Time
}

// maxDrainRounds bounds how many windows one source may land in a single pass,
// so a source that grows faster than it is read cannot starve the others.
const maxDrainRounds = 64

// Stats summarises one collection pass.
type Stats struct {
	Sessions      int
	SourcesSeen   int
	SourcesLanded int
	Records       int
	Bytes         int64
	// SourcesGone counts files discovery listed that were gone when the pass
	// came to read them.
	SourcesGone int
	Conflicts   int
	// Busy counts sessions skipped because another collector holds their lock.
	Busy int
	// Pending counts sources that still had data when the per-pass drain
	// limit was reached. A non-zero value means the pass was NOT complete.
	Pending int
	// Held counts sources whose last records wait for the turn they belong
	// to. It is not an error: the records land on a later pass.
	Held int
	// Indexed is the total index entries across every session touched.
	// Reindexed counts entries recovered from landed files because the index
	// had fallen behind them.
	Indexed   int
	Reindexed int
	// Skipped is what discovery found and does not collect.
	Skipped Skipped
	Errors  []error
	// Retired names the sessions found that are retired. Their files, the
	// sub-executions included, are not read, and nothing of them is written.
	Retired []string
	// Changed lists the sessions whose landed data or index moved this pass.
	Changed []string
}

// Complete reports whether the pass collected everything available.
func (s *Stats) Complete() bool {
	return s.Pending == 0 && s.Conflicts == 0 && len(s.Errors) == 0
}

// New returns a Collector with the default byte budget when none is given.
func New(sourceRoot string, zone *storage.Zone, maxDelta int64) *Collector {
	if maxDelta <= 0 {
		maxDelta = 2 << 20
	}
	return &Collector{SourceRoot: sourceRoot, Zone: zone, MaxDelta: maxDelta, Now: time.Now}
}

// CollectAll discovers sessions and lands whatever is new in each.
func (c *Collector) CollectAll(filter func(Session) bool) (*Stats, error) {
	sessions, skipped, warnings, err := Discover(c.SourceRoot)
	if err != nil {
		return nil, err
	}
	st := &Stats{Skipped: skipped}
	// A directory that could not be read hides sessions, so the pass must
	// not look complete.
	st.Errors = append(st.Errors, warnings...)
	for _, s := range sessions {
		if filter != nil && !filter(s) {
			continue
		}
		// Before its lock, which would make its directory again. A
		// sub-execution carries its parent's id, so it goes with it.
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

// collectSession processes one session single-threaded, under its lock, so
// the landed sequence stays monotonic across every stream of the session.
func (c *Collector) collectSession(s Session, st *Stats) error {
	lock, err := storage.LockSession(c.Zone.SessionDir(s.ID))
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
	if err := state.RecoverNextSeq(c.Zone.SessionDir(s.ID)); err != nil {
		return err
	}
	// Only an index that agrees with its saved state is extended; one that
	// does not is built again.
	ix, ok, err := index.LoadFor(indexDir, s.ID, ixState)
	if err != nil {
		return err
	}
	if !ok {
		ix = index.New(s.ID)
		ixState = storage.NewIndexState(s.ID)
	}
	// Cursors commit per source and the index once per session, so a crash
	// between them leaves landed records the index does not hold. Nothing
	// would land them again, so they are read back from the landed files.
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

	p := &pass{ix: ix, state: state, st: st, now: c.Now()}
	changed := false
	for _, src := range s.Sources {
		st.SourcesSeen++
		// Drain a source rather than land one window per pass, so a single
		// pass over existing history is complete.
		var landedAny bool
		for round := 0; ; round++ {
			landed, more, err := c.collectSource(src, p)
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

	// A recovered index is saved even when nothing landed, or the rebuild
	// would repeat on every pass.
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
		if err := ixState.Save(c.Zone.IndexStatePath(s.ID), p.now); err != nil {
			return err
		}
		st.Indexed += ixState.Entries
	}
	state.LastScan = p.now.UTC().Format(time.RFC3339Nano)
	return state.Save(statePath, p.now)
}

// cursorPaths returns the landing directory, the cursor path and the landed
// file prefix for a source.
//
// The header lands in the main stream's directory under its own prefix and
// cursor, as a sidecar does, so the main stream's records and the header's
// snapshots never share a cursor.
func (c *Collector) cursorPaths(src Source) (dir, cursorPath, prefix string) {
	dir = c.Zone.StreamDir(src.Session, src.Stream)
	prefix = "transcript"
	if src.Kind == SrcHeader {
		prefix = "meta"
	}
	return dir, filepath.Join(dir, prefix+".cursor"), prefix
}

// recordKind is the landed kind of a source. The header is what the runtime
// recorded about the session's agent, which is what an agent_meta file holds.
func recordKind(k SrcKind) sessiondata.Kind {
	if k == SrcHeader {
		return sessiondata.KindAgentMeta
	}
	return sessiondata.KindTranscript
}

func (c *Collector) collectSource(src Source, p *pass) (landed, more bool, err error) {
	dir, cursorPath, prefix := c.cursorPaths(src)
	kind := storage.CursorSnapshot
	if src.Kind.Append() {
		kind = storage.CursorAppend
	}
	cur, err := storage.LoadCursor(cursorPath, kind, src.Rel)
	if err != nil {
		return false, false, err
	}
	if cur.State == storage.CursorConflict {
		p.st.Conflicts++
		return false, false, nil // a conflict is sticky until resolved
	}
	// Discovery found the file, so it is not gone, whatever its cursor last
	// said.
	if cur.State == storage.CursorSourceGone {
		cur.State = storage.CursorActive
		if err := cur.Save(cursorPath, p.now); err != nil {
			return false, false, err
		}
	}
	if src.Kind.Append() {
		return c.landAppend(src, cur, cursorPath, dir, prefix, p)
	}
	landed, err = c.landSnapshot(src, cur, cursorPath, dir, prefix, p)
	return landed, false, err
}

func (c *Collector) landAppend(src Source, cur *storage.Cursor, cursorPath, dir, prefix string,
	p *pass) (bool, bool, error) {
	st, now := p.st, p.now

	chunk, err := claudecode.TailAppend(src.Path, cur, c.MaxDelta)
	switch {
	case errors.Is(err, claudecode.ErrSourceGone):
		return false, false, c.markGone(cur, cursorPath, st, now)
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
			// The same bytes under a new identity. Record it now, or every
			// later pass verifies the tail window again.
			cur.Dev, cur.Ino = chunk.Dev, chunk.Ino
			cur.Size, cur.MTime = chunk.Size, chunk.MTime
			return false, false, cur.Save(cursorPath, now)
		}
		return false, false, nil
	}

	runs, keep, err := turnFor(chunk.Lines, src.Path, chunk.NewOffset, chunk.MTime, now)
	if err != nil {
		return false, false, err
	}
	lines, newOffset, newOrd, more := chunk.Lines, chunk.NewOffset, chunk.NewOrd, chunk.More
	held := keep < len(lines)
	if held {
		// The rest waits for its turn. The cursor stops before it, so the
		// next pass reads it again with whatever followed.
		lines, newOffset, newOrd, more = lines[:keep], lines[keep].Off, lines[keep].Ord-1, false
	}
	if len(lines) == 0 {
		if held {
			st.Held++
		}
		return false, false, nil
	}

	seq := p.state.Take()
	name := storage.LandedName(prefix, storage.Stamp(now), seq)
	hdr := &sessiondata.Header{
		H: 1, Seq: seq, At: now.UTC().Format(time.RFC3339Nano),
		Kind: recordKind(src.Kind), Adapter: Name + "/" + Version, Dialect: Dialect,
		Src: src.Rel, Session: src.Session, Stream: src.Stream,
	}
	// A record is larger than its line, so the file ends where the converted
	// records reach the budget, and the cursor stops before the first line
	// left out.
	cut, err := sessiondata.NewCut(hdr, c.MaxDelta)
	if err != nil {
		p.state.GiveBack(seq)
		return false, false, err
	}
	var recs []*sessiondata.Record
	for row, ln := range lines {
		rec := Convert(src, ln.Ord, ln.Off, ln.Bytes, runs[row])
		ok, err := cut.Take(rec)
		if err != nil {
			p.state.GiveBack(seq)
			return false, false, err
		}
		if !ok {
			break
		}
		recs = append(recs, rec)
	}
	switch {
	case len(recs) < len(lines):
		newOffset, newOrd, more = lines[len(recs)].Off, lines[len(recs)].Ord-1, true
		lines = lines[:len(recs)]
	case held:
		// Counted once, by the call that lands the last line before what is
		// held, not by every call a cut makes on the way.
		st.Held++
	}
	var written int64
	var staged index.Staged
	err = storage.WriteAtomic(filepath.Join(dir, name), storage.PermLanded, func(w io.Writer) error {
		rw, err := sessiondata.NewWriter(w, hdr)
		if err != nil {
			return err
		}
		rw.Budget(c.MaxDelta)
		for row, ln := range lines {
			rec := recs[row]
			if err := rw.Write(rec); err != nil {
				return err
			}
			written += int64(len(ln.Bytes))
			// The index is built from the converted record, so it holds no
			// Kiro field.
			staged.Add(index.FromRecord(p.ix, hdr, rec, uint32(seq), uint32(row+1)))
		}
		return rw.Close()
	})
	if err != nil {
		p.state.GiveBack(seq)
		return false, false, err
	}
	p.ix.AppendStaged(&staged)

	// Land before committing the cursor. A crash between the two lands the
	// same records again, which assembly removes by record id.
	digest, err := claudecode.TailDigestAt(src.Path, int64(newOffset))
	if err != nil && !os.IsNotExist(err) {
		return true, false, err
	}
	cur.Dev, cur.Ino = chunk.Dev, chunk.Ino
	cur.Offset, cur.Ord = newOffset, newOrd
	cur.TailSHA256 = digest
	cur.Size, cur.MTime = chunk.Size, chunk.MTime
	cur.LastSeq, cur.State = seq, storage.CursorActive

	st.Records += len(lines)
	st.Bytes += written
	return true, more, cur.Save(cursorPath, now)
}

func (c *Collector) landSnapshot(src Source, cur *storage.Cursor, cursorPath, dir, prefix string,
	p *pass) (bool, error) {
	st, now := p.st, p.now

	data, err := os.ReadFile(src.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, c.markGone(cur, cursorPath, st, now)
		}
		return false, err
	}
	body := trimTrailingNewline(data)
	if len(body) == 0 {
		return false, nil
	}
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	if digest == cur.ContentSHA256 {
		return false, nil // unchanged
	}

	seq := p.state.Take()
	name := storage.LandedName(prefix, storage.Stamp(now), seq)
	hdr := &sessiondata.Header{
		H: 1, Seq: seq, At: now.UTC().Format(time.RFC3339Nano),
		Kind: recordKind(src.Kind), Adapter: Name + "/" + Version, Dialect: Dialect,
		Src: src.Rel, Session: src.Session, Stream: src.Stream,
	}
	var staged index.Staged
	err = storage.WriteAtomic(filepath.Join(dir, name), storage.PermLanded, func(w io.Writer) error {
		rw, err := sessiondata.NewWriter(w, hdr)
		if err != nil {
			return err
		}
		rw.Budget(c.MaxDelta)
		rec := Convert(src, 1, 0, body, "")
		if err := rw.Write(rec); err != nil {
			return err
		}
		staged.Add(index.FromRecord(p.ix, hdr, rec, uint32(seq), 1))
		return rw.Close()
	})
	if err != nil {
		p.state.GiveBack(seq)
		return false, err
	}
	p.ix.AppendStaged(&staged)

	cur.ContentSHA256 = digest
	if fi, err := os.Stat(src.Path); err == nil {
		cur.Size, cur.MTime = fi.Size(), fi.ModTime().Unix()
	}
	cur.LastSeq, cur.State = seq, storage.CursorActive

	st.Records++
	st.Bytes += int64(len(body))
	return true, cur.Save(cursorPath, now)
}

func (c *Collector) markGone(cur *storage.Cursor, cursorPath string, st *Stats, now time.Time) error {
	st.SourcesGone++
	if cur.State == storage.CursorSourceGone {
		return nil
	}
	cur.State = storage.CursorSourceGone
	return cur.Save(cursorPath, now)
}

func trimTrailingNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
