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

package claudecodeprovider

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// prefix is the landed file name prefix, which is also the kind.
const prefix = string(sessiondata.KindProviderBody)

// Settle is how long a file that is not yet one JSON value is left alone
// before it is called unreadable. Claude Code writes a body with one
// asynchronous write and no rename, so a pass can meet it half written.
const Settle = 2 * time.Minute

// Window bounds the sessions a response is looked for in: those with a
// request whose file was written within this long of the response's.
const Window = 24 * time.Hour

// Verdict is what a session filter says about a session.
type Verdict int

const (
	// Collect lands the session's bodies.
	Collect Verdict = iota
	// Exclude leaves them out: include and exclude say so.
	Exclude
	// Wait holds them: nothing yet says where the session was run, so the
	// filter cannot judge it.
	Wait
)

// Filter judges a session by its id.
type Filter func(session string) Verdict

// Collector lands provider bodies into a storage zone.
type Collector struct {
	SourceRoot string
	Zone       *storage.Zone
	MaxDelta   int64
	Now        func() time.Time
}

// Stats summarises one collection pass. The fields the local adapter reports
// mean the same here, so a caller can add the passes together.
type Stats struct {
	Sessions      int
	SourcesSeen   int
	SourcesLanded int
	Records       int
	Bytes         int64
	Conflicts     int
	Busy          int
	Pending       int
	Indexed       int
	Reindexed     int
	Errors        []error
	Changed       []string
	// Waiting counts bodies no session claims yet, and Unreadable the files
	// that are not one JSON value. Neither makes a pass incomplete: a body
	// the runtime wrote for no call a transcript records, such as the one
	// that names a session, is never claimed.
	Waiting    int
	Unreadable int
}

// Complete reports whether the pass collected everything it could.
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

// nameRe is the file names Claude Code writes: a request under a random UUID,
// a response under the provider's request id, or a UUID when it had none.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+\.(request|response)\.json$`)

// file is one body file found in the source directory.
type file struct {
	name  string
	path  string
	size  int64
	mtime int64
	body  []byte
	keys  providerbody.Keys
	e     *entry
}

// CollectAll lands whatever bodies are new, into the sessions they belong to.
func (c *Collector) CollectAll(filter Filter) (*Stats, error) {
	st := &Stats{}
	items, err := os.ReadDir(c.SourceRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return st, nil
		}
		return nil, err
	}
	lock, err := storage.LockProvider(c.Zone.Root())
	if err != nil {
		if errors.Is(err, storage.ErrProviderBusy) {
			st.Busy++
			return st, nil
		}
		return nil, err
	}
	defer func() { _ = lock.Unlock() }()
	sn, err := loadSeen(c.Zone)
	if err != nil {
		return nil, err
	}
	now := c.Now()

	// A line whose file is gone and whose session is gone describes nothing
	// any more: a body pruned from the directory after its session was
	// removed.
	present := make(map[string]bool, len(items))
	for _, it := range items {
		present[it.Name()] = true
	}
	for name, e := range sn.entries {
		if !present[name] && (e.session == "" || !c.sessionHolds(e.session)) {
			delete(sn.entries, name)
			sn.dirty = true
		}
	}

	var fresh []*file
	for _, it := range items {
		if it.IsDir() || !nameRe.MatchString(it.Name()) {
			continue
		}
		info, err := it.Info()
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				// Not gone between the listing and the stat: unreachable, and
				// a pass that skipped it must not look complete.
				st.Errors = append(st.Errors, fmt.Errorf("claudecodeprovider: %s: %w", it.Name(), err))
			}
			continue
		}
		st.SourcesSeen++
		f := &file{name: it.Name(), path: filepath.Join(c.SourceRoot, it.Name()), size: info.Size(), mtime: info.ModTime().UnixNano()}
		f.e = sn.entries[f.name]
		if f.e != nil && f.e.size == f.size && f.e.mtime == f.mtime {
			switch f.e.state {
			case stateLanded:
				if c.sessionHolds(f.e.session) {
					continue
				}
			case stateConflict, stateChanged:
				// Counted on every pass it stays, so a single pass keeps
				// failing until a person looks.
				st.Conflicts++
				continue
			case stateUnreadable:
				st.Unreadable++
				continue
			}
		}
		if err := c.read(f, sn, now, st); err != nil {
			st.Errors = append(st.Errors, err)
			continue
		}
		if f.body != nil {
			fresh = append(fresh, f)
		}
	}

	bySession := c.attribute(fresh, sn, filter, st)
	st.Errors = append(st.Errors, sn.damaged...)
	// A pass that stopped after landing and before saving an index, or whose
	// session was busy when the gap was to be closed, left an index behind
	// what is landed. No new body of that session need ever come to close
	// it, so every pass looks at each session the table names.
	checked := map[string]bool{}
	for _, e := range sn.entries {
		if e.session == "" || checked[e.session] {
			continue
		}
		checked[e.session] = true
		if _, fresh := bySession[e.session]; !fresh && c.indexBehind(e.session) {
			bySession[e.session] = nil
		}
	}
	ids := make([]string, 0, len(bySession))
	for id := range bySession {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		st.Sessions++
		if err := c.collectSession(id, bySession[id], sn, st, now); err != nil {
			st.Errors = append(st.Errors, fmt.Errorf("session %s: %w", id, err))
		}
	}
	if sn.dirty {
		if err := sn.save(c.Zone, now); err != nil {
			return st, err
		}
	}
	return st, nil
}

// indexBehind reports whether a session's index covers less than its landed
// provider files.
func (c *Collector) indexBehind(session string) bool {
	items, err := os.ReadDir(c.Zone.ProviderDir(session))
	if err != nil {
		return false
	}
	var top uint64
	for _, it := range items {
		if seq, ok := storage.LandedFileSeq(it.Name()); ok && seq > top {
			top = seq
		}
	}
	if top == 0 {
		return false
	}
	ix, err := storage.LoadIndexState(c.Zone.IndexStatePath(session), session)
	return err != nil || ix.Schema != index.Schema || ix.IndexedSeq < top
}

// sessionHolds reports whether a session's provider directory exists. A
// landed entry whose session was removed lands again.
func (c *Collector) sessionHolds(session string) bool {
	_, err := os.Stat(c.Zone.ProviderDir(session))
	return err == nil
}

// read reads a file that is new, changed, or not yet landed, and decides what
// its bytes allow.
func (c *Collector) read(f *file, sn *seen, now time.Time, st *Stats) error {
	body, err := os.ReadFile(f.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	sum := providerbody.Digest(body)[:12]
	if f.e != nil && (f.e.state == stateLanded || f.e.state == stateChanged) && c.sessionHolds(f.e.session) {
		if f.e.sha == sum {
			// The bytes that landed: a table rebuilt from landed records, a
			// file touched, or a changed file put back.
			f.e.state, f.e.size, f.e.mtime = stateLanded, int64(len(body)), f.mtime
			sn.dirty = true
			return nil
		}
		first := f.e.state == stateLanded
		f.e.state, f.e.size, f.e.mtime = stateChanged, f.size, f.mtime
		sn.dirty = true
		st.Conflicts++
		if first {
			return fmt.Errorf("claudecodeprovider: %s changed after it landed", f.name)
		}
		return nil
	}
	if f.e != nil && f.e.state == stateConflict {
		if f.e.sha == sum {
			f.e.size, f.e.mtime = f.size, f.mtime
			sn.dirty = true
			st.Conflicts++
			return nil
		}
		// Other bytes than the ones in conflict: read as new.
		delete(sn.entries, f.name)
		f.e = nil
		sn.dirty = true
	}
	if !json.Valid(body) {
		if now.Sub(time.Unix(0, f.mtime)) < Settle {
			return nil // still being written
		}
		sn.entries[f.name] = &entry{name: f.name, size: f.size, mtime: f.mtime, sha: sum, state: stateUnreadable}
		sn.dirty = true
		st.Unreadable++
		return nil
	}
	f.body = body
	f.keys = Lift(f.name, body)
	return nil
}

// attribute finds each body's session and applies the filter. A request
// names its session. A response is claimed by a later request that names its
// request id, or by the one session whose transcript holds its message id.
// Anything else waits.
func (c *Collector) attribute(fresh []*file, sn *seen, filter Filter, st *Stats) map[string][]*file {
	// Which sessions a request id is named by, from landed requests and the
	// ones read this pass.
	namedBy := map[string]map[string]bool{}
	addNamed := func(prev, session string) {
		if prev == "" || session == "" {
			return
		}
		if namedBy[prev] == nil {
			namedBy[prev] = map[string]bool{}
		}
		namedBy[prev][session] = true
	}
	type near struct {
		session string
		mtime   int64
	}
	var requests []near
	for _, e := range sn.entries {
		if e.state == stateLanded && RoleOf(e.name) == providerbody.RoleRequest {
			addNamed(e.keys.PreviousRequest, e.session)
			requests = append(requests, near{e.session, e.mtime})
		}
	}
	for _, f := range fresh {
		if RoleOf(f.name) == providerbody.RoleRequest && claudecode.IsSessionID(f.keys.Session) {
			addNamed(f.keys.PreviousRequest, f.keys.Session)
			requests = append(requests, near{f.keys.Session, f.mtime})
		}
	}
	indexes := map[string]*index.Index{}
	holdsCall := func(session, call string) bool {
		ix, ok := indexes[session]
		if !ok {
			ix, _, _ = index.Load(c.Zone.IndexDir(session), session)
			indexes[session] = ix
		}
		return ix != nil && len(ix.ProviderCall(call)) > 0
	}

	out := map[string][]*file{}
	for _, f := range fresh {
		session := ""
		switch RoleOf(f.name) {
		case providerbody.RoleRequest:
			// The session names a directory, so only a session id in the
			// runtime's own form is taken.
			if claudecode.IsSessionID(f.keys.Session) {
				session = f.keys.Session
			}
		case providerbody.RoleResponse:
			if names := namedBy[f.keys.Request]; len(names) == 1 {
				for s := range names {
					session = s
				}
			} else if len(names) == 0 && f.keys.Call != "" {
				found := map[string]bool{}
				for _, r := range requests {
					if r.session == "" || found[r.session] {
						continue
					}
					// A table rebuilt from landed records has no times; its
					// sessions are all candidates.
					if r.mtime >= 0 && absDuration(f.mtime-r.mtime) > Window {
						continue
					}
					if holdsCall(r.session, f.keys.Call) {
						found[r.session] = true
					}
				}
				if len(found) == 1 {
					for s := range found {
						session = s
					}
				}
			}
		}
		state := stateWaiting
		if session != "" {
			switch filter(session) {
			case Collect:
				out[session] = append(out[session], f)
				continue
			case Exclude:
				state = stateExcluded
			}
		}
		if state == stateWaiting {
			st.Waiting++
		}
		sn.entries[f.name] = &entry{name: f.name, size: f.size, mtime: f.mtime, sha: providerbody.Digest(f.body)[:12],
			state: state, session: session, keys: f.keys}
		sn.dirty = true
	}
	return out
}

func absDuration(ns int64) time.Duration {
	if ns < 0 {
		ns = -ns
	}
	return time.Duration(ns)
}

// collectSession lands one session's new bodies under the session's lock,
// with the session's own sequence, cut into files at MaxDelta.
func (c *Collector) collectSession(session string, files []*file, sn *seen, st *Stats, now time.Time) error {
	sessionDir := c.Zone.SessionDir(session)
	lock, err := storage.LockSession(sessionDir)
	if err != nil {
		if errors.Is(err, storage.ErrSessionBusy) {
			st.Busy++
			return nil
		}
		return err
	}
	defer func() { _ = lock.Unlock() }()

	ixState, err := storage.LoadIndexState(c.Zone.IndexStatePath(session), session)
	if err != nil {
		return err
	}
	statePath := c.Zone.SessionStatePath(session)
	state, err := storage.LoadSessionState(statePath, session)
	if err != nil {
		return err
	}
	if err := state.RecoverNextSeq(sessionDir); err != nil {
		return err
	}
	var ix *index.Index
	if ixState.Schema == index.Schema {
		if loaded, ok, lerr := index.Load(c.Zone.IndexDir(session), session); lerr == nil && ok {
			ix = loaded
		}
	}
	if ix == nil {
		ix = index.New(session)
		ixState = storage.NewIndexState(session)
	}
	// Close any gap between what has landed and what the index covers, as
	// the other adapters do.
	reindexed := false
	if landedTo := state.NextSeq - 1; ixState.IndexedSeq < landedTo {
		n, rerr := index.Rebuild(c.Zone, session, ix, ixState.IndexedSeq)
		if rerr != nil {
			return rerr
		}
		st.Reindexed += n
		ixState.IndexedSeq = landedTo
		reindexed = true
	}

	held, err := c.loadSession(session)
	if err != nil {
		return err
	}
	sort.SliceStable(files, func(i, j int) bool {
		if files[i].mtime != files[j].mtime {
			return files[i].mtime < files[j].mtime
		}
		return files[i].name < files[j].name
	})

	var batch []*sessiondata.Record
	var batchFiles []*file
	// done are the files landed in this session. The table says so only
	// once the index and the session state that describe them are saved:
	// a pass that stops before then reads the files again, finds the
	// session holds them, and closes the index gap on the way.
	var done []*file
	var size int64
	landed := false
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		seq := state.Take()
		name := storage.LandedName(prefix, storage.Stamp(now), seq)
		hdr := &sessiondata.Header{
			H: 1, Seq: seq, At: now.UTC().Format(time.RFC3339Nano),
			Kind: sessiondata.KindProviderBody, Adapter: Name + "/" + Version, Dialect: Dialect,
			Src: ".", Session: session,
		}
		err := storage.WriteAtomic(filepath.Join(c.Zone.ProviderDir(session), name), storage.PermLanded, func(w io.Writer) error {
			rw, err := sessiondata.NewWriter(w, hdr)
			if err != nil {
				return err
			}
			for row, rec := range batch {
				if err := rw.Write(rec); err != nil {
					return err
				}
				e, blocks, body := index.FromRecord(ix, hdr, rec, uint32(seq), uint32(row+1))
				ix.AppendRecord(e, blocks, body)
			}
			return rw.Close()
		})
		if err != nil {
			return err
		}
		for _, f := range batchFiles {
			st.Bytes += f.size
		}
		done = append(done, batchFiles...)
		st.Records += len(batch)
		st.SourcesLanded += len(batchFiles)
		batch, batchFiles, size = nil, nil, 0
		landed = true
		return nil
	}
	for _, f := range files {
		rec, err := held.Encode(providerbody.Body{ID: ID(f.name), Role: RoleOf(f.name), Src: f.name, Keys: f.keys, Bytes: f.body})
		switch {
		case errors.Is(err, providerbody.ErrRepeat):
			done = append(done, f)
			continue
		case err != nil:
			sn.entries[f.name] = &entry{name: f.name, size: f.size, mtime: f.mtime, sha: providerbody.Digest(f.body)[:12],
				state: stateConflict, session: session, keys: f.keys}
			sn.dirty = true
			st.Conflicts++
			st.Errors = append(st.Errors, fmt.Errorf("claudecodeprovider: %s: %w", f.name, err))
			continue
		}
		// A file ends before a body that would take it past the budget, so
		// only a body larger than the budget on its own lands in a larger file.
		n := providerbody.RecordBytes(rec)
		if len(batch) > 0 && providerbody.FileOverhead+size+n > c.MaxDelta {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = append(batch, rec)
		batchFiles = append(batchFiles, f)
		size += n
	}
	if err := flush(); err != nil {
		return err
	}

	if landed || reindexed {
		st.Changed = append(st.Changed, session)
		ixState.Schema = index.Schema
		ixState.IndexedSeq = state.NextSeq - 1
		ixState.Entries = len(ix.Entries)
		ixState.Blocks = len(ix.Blocks)
		ixState.Strings = ix.Strings.Len()
		if err := ix.Write(c.Zone.IndexDir(session)); err != nil {
			return err
		}
		if err := ixState.Save(c.Zone.IndexStatePath(session), now); err != nil {
			return err
		}
		st.Indexed += ixState.Entries
	}
	state.LastScan = now.UTC().Format(time.RFC3339Nano)
	if err := state.Save(statePath, now); err != nil {
		return err
	}
	for _, f := range done {
		c.markLanded(sn, f, session)
	}
	if sn.dirty {
		return sn.save(c.Zone, now)
	}
	return nil
}

func (c *Collector) markLanded(sn *seen, f *file, session string) {
	sn.entries[f.name] = &entry{name: f.name, size: f.size, mtime: f.mtime, sha: providerbody.Digest(f.body)[:12],
		state: stateLanded, session: session, keys: f.keys}
	sn.dirty = true
}

// loadSession reads a session's landed provider bodies, in landing order, so
// a new body is cut against everything the session already holds.
//
// A record that does not hold together is left out: a file it names was
// lost, cut short, or kept in another storage stage than this root. A new
// body is then cut only against what does, and lands whole where nothing
// earlier is left to share, so a damaged or partly restored session still
// takes new bodies. asz verify reports the damage itself.
func (c *Collector) loadSession(session string) (*providerbody.Session, error) {
	held := providerbody.NewSession()
	files, err := storage.LandedFiles(c.Zone, session)
	if err != nil {
		return nil, err
	}
	for _, lf := range files {
		if lf.Stream != "" || lf.RunID != "" {
			continue
		}
		// A file that does not read, whole and with its digest, holds nothing
		// a new body may refer to.
		var recs []*sessiondata.Record
		if err := readLanded(lf.Path, func(_ sessiondata.Header, rec *sessiondata.Record) {
			recs = append(recs, rec)
		}); err != nil {
			continue
		}
		for _, rec := range recs {
			_ = held.Add(rec)
		}
	}
	return held, nil
}
