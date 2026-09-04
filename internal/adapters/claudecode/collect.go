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
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// Collector lands Claude Code sources into a storage zone.
type Collector struct {
	SourceRoot string
	Zone       *storage.Zone
	MaxDelta   int64
	Now        func() time.Time
}

// pass carries the state of one session's collection.
//
// The index lives here rather than on Collector because it is per-session
// mutable state: holding it on the shared struct would make two concurrent
// CollectAll calls race on it, silently interleaving one session's entries into
// another's index.
//
// Indexing rides along with landing because the collector already holds every
// byte it writes; a separate pass would re-read the whole corpus to learn
// nothing new.
type pass struct {
	ix    *index.Index
	state *storage.SessionState
	st    *Stats
	now   time.Time
}

// maxDrainRounds bounds how many windows one source may land in a single pass,
// so a source that is growing faster than we read cannot starve the others.
const maxDrainRounds = 64

// Stats summarises one collection pass.
type Stats struct {
	Sessions      int
	SourcesSeen   int
	SourcesLanded int
	Records       int
	Bytes         int64
	SourcesGone   int
	Conflicts     int
	// Busy counts sessions skipped because another collector holds their lock.
	Busy int
	// Pending counts sources that still had data when the per-pass drain limit
	// was reached. A non-zero value means the pass was NOT complete.
	Pending int
	// Indexed is the total index entries across every session touched.
	Indexed int
	// Reindexed counts entries recovered by re-reading landed files because the
	// index had fallen behind. A non-zero value means a previous pass was
	// interrupted after landing but before the index was written.
	Reindexed int
	Errors    []error
	// Changed lists the sessions whose landed data or index moved this pass.
	// A caller that assembles structure afterwards needs only these; parsing
	// every session again measured 3.3 s on a 43-session corpus with nothing
	// new, which is most of a 5 s watch interval.
	Changed []string
}

// Complete reports whether the pass collected everything available.
func (s *Stats) Complete() bool {
	return s.Pending == 0 && s.Conflicts == 0 && len(s.Errors) == 0
}

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
	// An unreadable source directory hides sources; recording it as an error
	// keeps the pass from reporting a clean, complete-looking result.
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

// collectSession processes one session single-threaded.
//
// Single-threaded is required, not incidental: the landed sequence must be
// monotonic across every stream in the session, which is what lets the
// assembler track progress with one watermark instead of one per stream.
func (c *Collector) collectSession(s Session, st *Stats) error {
	sessionDir := c.Zone.SessionDir(s.ID)

	// One collector per session. Without this two processes allocate the same
	// sequence numbers to different content, and the assembler's watermark
	// skips whichever it reaches second.
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

	var ix *index.Index
	if ixState.Schema == index.Schema {
		if loaded, ok, lerr := index.Load(indexDir, s.ID); lerr == nil && ok {
			ix = loaded
		}
	}
	if ix == nil {
		ix = index.New(s.ID)
		ixState = storage.NewIndexState(s.ID)
	}

	// Close any gap between what has landed and what the index covers.
	//
	// Cursors commit per source while the index is written once per session, so
	// a crash between them advances the cursors without recording their
	// entries. Nothing would re-land, so the gap would be permanent - and the
	// index would claim, via indexed_seq, to describe data it does not hold.
	// Re-reading the landed files is the only way to close it.
	reindexed := false
	if landedTo := state.NextSeq - 1; ixState.IndexedSeq < landedTo {
		n, rerr := index.Rebuild(c.Zone, s.ID, ix, ixState.IndexedSeq)
		if rerr != nil {
			return rerr
		}
		if n > 0 {
			st.Reindexed += n
		}
		ixState.IndexedSeq = landedTo
		reindexed = true
	}

	p := &pass{ix: ix, state: state, st: st, now: c.Now()}
	now := p.now
	changed := false

	// Two distinct sources must never share a landing directory. A session's
	// files are spread across several source directories, so in principle the
	// same agent id or workflow run id could appear under two of them; landing
	// both into one stream would interleave unrelated records behind a single
	// cursor, and the two would then overwrite each other's position on every
	// pass. Detect it and refuse rather than corrupt.
	claimed := make(map[string]string, len(s.Sources))

	for _, src := range s.Sources {
		st.SourcesSeen++

		_, cursorPath, _ := c.cursorPaths(src)
		if prev, dup := claimed[cursorPath]; dup && prev != src.Rel {
			st.Conflicts++
			st.Errors = append(st.Errors, fmt.Errorf(
				"claudecode: %s and %s both map to %s; refusing to interleave them",
				prev, src.Rel, cursorPath))
			continue
		}
		claimed[cursorPath] = src.Rel

		// Drain a source rather than landing one window per pass. A single
		// window would leave the documented "-once" backfill silently
		// incomplete: a 59 MB transcript needs eight passes at the default
		// budget, and the pass would still report success.
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

	// A recovered index must be persisted even when no source landed this pass;
	// otherwise the rebuild is discarded and repeats on every round forever.
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

// cursorPaths returns the landing directory and cursor path for a source.
func (c *Collector) cursorPaths(src Source) (dir, cursorPath, prefix string) {
	switch src.Kind {
	case SrcMainTranscript, SrcAgentTranscript:
		dir = c.Zone.StreamDir(src.Session, src.Stream)
		prefix = "transcript"
	case SrcAgentMeta:
		dir = c.Zone.StreamDir(src.Session, src.Stream)
		prefix = "meta"
	case SrcJournal:
		dir = c.Zone.RunDir(src.Session, src.RunID)
		prefix = "journal"
	case SrcWorkflowManifest:
		dir = c.Zone.RunDir(src.Session, src.RunID)
		prefix = "manifest"
	case SrcWorkflowScript:
		dir = c.Zone.RunDir(src.Session, src.RunID)
		prefix = "script"
	default:
		return "", "", ""
	}
	// "<kind>.cursor" rather than "<kind>-cursor": a landed file is
	// "<kind>-<ts>-<seq>.jsonl", so a hyphen would make the cursor share a
	// prefix with the data it tracks and turn any prefix scan into a bug.
	return dir, filepath.Join(dir, prefix+".cursor"), prefix
}

func (c *Collector) collectSource(src Source, p *pass) (landed, more bool, err error) {
	dir, cursorPath, prefix := c.cursorPaths(src)
	if dir == "" {
		return false, false, fmt.Errorf("claudecode: unknown source kind for %s", src.Rel)
	}

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

	if src.Kind.Append() {
		return c.landAppend(src, cur, cursorPath, dir, prefix, p)
	}
	landed, err = c.landSnapshot(src, cur, cursorPath, dir, prefix, p)
	return landed, false, err
}

func (c *Collector) landAppend(src Source, cur *storage.Cursor, cursorPath, dir, prefix string,
	p *pass) (bool, bool, error) {
	state, st, now := p.state, p.st, p.now

	chunk, err := TailAppend(src.Path, cur, c.MaxDelta)
	switch {
	case errors.Is(err, ErrSourceGone):
		landed, gerr := c.markGone(cur, cursorPath, st, now)
		return landed, false, gerr
	case err != nil:
		var ce *ConflictError
		if errors.As(err, &ce) {
			cur.State = storage.CursorConflict
			st.Conflicts++
			_ = cur.Save(cursorPath, now)
			return false, false, err
		}
		return false, false, err
	}
	if len(chunk.Lines) == 0 {
		if chunk.Moved {
			// The same bytes under a new identity. Record it now, or the next
			// pass re-reads the tail window of this source to verify it again,
			// which across thousands of unchanged sources is most of a pass.
			cur.Dev, cur.Ino = chunk.Dev, chunk.Ino
			cur.Size, cur.MTime = chunk.Size, chunk.MTime
			return false, false, cur.Save(cursorPath, now)
		}
		return false, false, nil
	}

	seq := state.Take()
	name := storage.LandedName(prefix, storage.Stamp(now), seq)
	hdr := &sessiondata.Header{
		H: 1, Seq: seq, At: now.UTC().Format(time.RFC3339Nano),
		Kind: src.Kind.RecordKind(), Adapter: Name + "/" + Version, Dialect: Dialect,
		Src: src.Rel, Session: src.Session, Stream: src.Stream, Batch: src.RunID,
	}

	var written int64
	err = storage.WriteAtomic(filepath.Join(dir, name), storage.PermLanded, func(w io.Writer) error {
		rw, err := sessiondata.NewWriter(w, hdr)
		if err != nil {
			return err
		}
		for row, ln := range chunk.Lines {
			// Conversion happens HERE, once, while the source is being read.
			// Nothing above this line ever sees a Claude Code shape.
			rec := Convert(src, ln.Ord, ln.Off, ln.Bytes)
			if err := rw.Write(rec); err != nil {
				return err
			}
			written += int64(len(ln.Bytes))
			// The index is built from the CONVERTED record, so the source is
			// parsed once and the index has no dialect in it.
			e, blocks := index.FromRecord(p.ix, hdr, rec, uint32(seq), uint32(row+1))
			p.ix.Append(e, blocks...)
		}
		return rw.Close()
	})
	if err != nil {
		return false, false, err
	}

	// Land before committing the cursor. A crash between the two re-lands the
	// same records next pass, which is safe because the assembler must already
	// drop duplicate records by (file, uuid) - Claude Code itself writes them.
	// The reverse order would lose data instead.
	digest, err := TailDigestAt(src.Path, int64(chunk.NewOffset))
	if err != nil {
		// The source vanished between landing and the digest. The delta is
		// already durable, so record the position we reached rather than
		// discarding a successful write.
		if !os.IsNotExist(err) {
			return true, false, err
		}
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

func (c *Collector) landSnapshot(src Source, cur *storage.Cursor, cursorPath, dir, prefix string,
	p *pass) (bool, error) {
	state, st, now := p.state, p.st, p.now

	data, err := os.ReadFile(src.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return c.markGone(cur, cursorPath, st, now)
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

	seq := state.Take()
	name := storage.LandedName(prefix, storage.Stamp(now), seq)
	hdr := &sessiondata.Header{
		H: 1, Seq: seq, At: now.UTC().Format(time.RFC3339Nano),
		Kind: src.Kind.RecordKind(), Adapter: Name + "/" + Version, Dialect: Dialect,
		Src: src.Rel, Session: src.Session, Stream: src.Stream, Batch: src.RunID,
	}
	err = storage.WriteAtomic(filepath.Join(dir, name), storage.PermLanded, func(w io.Writer) error {
		rw, err := sessiondata.NewWriter(w, hdr)
		if err != nil {
			return err
		}
		rec := Convert(src, 1, 0, body)
		if err := rw.Write(rec); err != nil {
			return err
		}
		e, blocks := index.FromRecord(p.ix, hdr, rec, uint32(seq), 1)
		p.ix.Append(e, blocks...)
		return rw.Close()
	})
	if err != nil {
		return false, err
	}

	info, _ := statSource(src.Path)
	cur.ContentSHA256 = digest
	cur.Size, cur.MTime = info.size, info.mtime
	cur.LastSeq, cur.State = seq, storage.CursorActive

	st.Records++
	st.Bytes += int64(len(body))
	return true, cur.Save(cursorPath, now)
}

func (c *Collector) markGone(cur *storage.Cursor, cursorPath string, st *Stats, now time.Time) (bool, error) {
	st.SourcesGone++
	if cur.State == storage.CursorSourceGone {
		return false, nil
	}
	cur.State = storage.CursorSourceGone
	return false, cur.Save(cursorPath, now)
}

func trimTrailingNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
