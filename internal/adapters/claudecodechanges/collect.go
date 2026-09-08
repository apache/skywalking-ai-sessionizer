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
	// Changed lists the sessions whose landed data or index moved this pass.
	Changed []string
}

// Complete reports whether the pass collected everything available.
func (s *Stats) Complete() bool {
	return s.Pending == 0 && s.Conflicts == 0 && len(s.Errors) == 0
}

// maxDrainRounds bounds how many windows one source may land in a pass.
const maxDrainRounds = 64

// prefix is the landed file name prefix, which is also the kind.
const prefix = "changes"

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

// collectSource lands one window of one file, and says whether more waits.
func (c *Collector) collectSource(src Source, ix *index.Index, state *storage.SessionState, st *Stats, now time.Time) (landed, more bool, err error) {
	dir := c.Zone.StreamDir(src.Session, src.Stream)
	cursorPath := filepath.Join(dir, prefix+".cursor")
	cur, err := storage.LoadCursor(cursorPath, storage.CursorAppend, src.Rel)
	if err != nil {
		return false, false, err
	}
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
		return false, false, nil
	}

	seq := state.Take()
	name := storage.LandedName(prefix, storage.Stamp(now), seq)
	hdr := &sessiondata.Header{
		H: 1, Seq: seq, At: now.UTC().Format(time.RFC3339Nano),
		Kind: sessiondata.KindChanges, Adapter: Name + "/" + Version, Dialect: Dialect,
		Src: src.Rel, Session: src.Session, Stream: src.Stream,
	}
	var written int64
	err = storage.WriteAtomic(filepath.Join(dir, name), storage.PermLanded, func(w io.Writer) error {
		rw, err := sessiondata.NewWriter(w, hdr)
		if err != nil {
			return err
		}
		for row, ln := range chunk.Lines {
			rec := Convert(ln.Ord, ln.Off, ln.Bytes)
			if err := rw.Write(rec); err != nil {
				return err
			}
			written += int64(len(ln.Bytes))
			e, blocks := index.FromRecord(ix, hdr, rec, uint32(seq), uint32(row+1))
			ix.Append(e, blocks...)
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
