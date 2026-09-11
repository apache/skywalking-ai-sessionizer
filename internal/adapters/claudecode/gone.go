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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// Claude Code prunes its own files: often the main transcript first, while a
// session's other files stay for a while, and in the end all of them. A pass
// visits only what discovery finds, so a pruned file is never met, and its
// cursor would say active forever. Measured on 2026-09-11 on one machine's
// storage root of 60 sessions, not one of 6,080 cursors said source_gone,
// while discovery no longer found 8 of the sessions and 1 file of another.
// The code here finds such cursors and sets them to source_gone. The landed
// files stay either way.

// cursorNames are the cursors this adapter keeps in a stream directory and in
// a run directory. The plugin's changes.cursor sits beside them and belongs to
// its own adapter.
var cursorNames = map[string][]string{
	"streams": {"transcript.cursor", "meta.cursor"},
	"runs":    {"journal.cursor", "manifest.cursor", "script.cursor"},
}

// markPruned sets source_gone on the cursors of one session whose file is no
// longer on disk. claimed holds the cursors of the sources this pass found,
// which are passed over without being read. The caller holds the session's
// lock.
func (c *Collector) markPruned(session string, claimed map[string]string, st *Stats, now time.Time) error {
	base := c.Zone.SessionDir(session)
	for _, sub := range []string{"streams", "runs"} {
		owners, err := os.ReadDir(filepath.Join(base, sub))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return err
		}
		for _, o := range owners {
			if !o.IsDir() {
				continue
			}
			for _, name := range cursorNames[sub] {
				p := filepath.Join(base, sub, o.Name(), name)
				if _, found := claimed[p]; found {
					continue
				}
				if err := c.markIfGone(session, p, st, now); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// markIfGone sets one cursor to source_gone when the file it names is gone.
//
// Only an active cursor moves. One that says source_gone already says so,
// and a conflict stays until a person looks at it. The cursor must name a
// file this adapter reads for this session, and that file's cursor must be
// this one. A cursor of the same name that another program wrote, such as a
// scenario sd build, names something else and is left alone. A missing
// cursor file reads as a fresh cursor with no source, and is left alone the
// same way.
func (c *Collector) markIfGone(session, cursorPath string, st *Stats, now time.Time) error {
	cur, err := storage.LoadCursor(cursorPath, storage.CursorAppend, "")
	if err != nil {
		return err
	}
	if cur.State != storage.CursorActive {
		return nil
	}
	src, ok := c.sourceOf(cur.Source, session)
	if !ok {
		return nil
	}
	if _, want, _ := c.cursorPaths(src); want != cursorPath {
		return nil
	}
	// Discovery lists no file under a directory it cannot read, and it
	// reports only an unreadable project directory. So a file it did not list
	// may still be there. The cursor moves only when the file is known not to
	// exist. Any other error, such as a denied permission, leaves it as it is.
	if _, err := os.Lstat(src.Path); !errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	st.SourcesGone++
	cur.State = storage.CursorSourceGone
	return cur.Save(cursorPath, now)
}

// sourceOf reads the source path a cursor names back into the source
// discovery makes of it, for one session. It is discovery's naming run
// backwards, and it reports false for a path discovery would not produce.
func (c *Collector) sourceOf(rel, session string) (Source, bool) {
	parts := strings.Split(rel, "/")
	if len(parts) < 2 || parts[0] == "" {
		return Source{}, false
	}
	s := Source{Path: filepath.Join(c.SourceRoot, filepath.FromSlash(rel)), Rel: rel, Session: session}
	if len(parts) == 2 {
		if parts[1] != session+".jsonl" {
			return Source{}, false
		}
		s.Kind, s.Stream = SrcMainTranscript, storage.StreamMain
		return s, true
	}
	if parts[1] != session {
		return Source{}, false
	}
	rest := parts[2:]
	switch {
	case len(rest) == 2 && rest[0] == "subagents":
		return agentSource(s, rest[1])
	case len(rest) == 4 && rest[0] == "subagents" && rest[1] == "workflows":
		if rest[3] == "journal.jsonl" {
			s.Kind, s.RunID = SrcJournal, rest[2]
			return s, true
		}
		return agentSource(s, rest[3])
	case len(rest) == 2 && rest[0] == "workflows" && strings.HasPrefix(rest[1], "wf_") && strings.HasSuffix(rest[1], ".json"):
		s.Kind, s.RunID = SrcWorkflowManifest, strings.TrimSuffix(rest[1], ".json")
		return s, true
	case len(rest) == 3 && rest[0] == "workflows" && rest[1] == "scripts":
		if id, ok := ScriptRunID(rest[2]); ok {
			s.Kind, s.RunID = SrcWorkflowScript, id
			return s, true
		}
	}
	return Source{}, false
}

// agentSource reads a file name as a child's transcript or as its metadata
// file, and reports false for any other name.
func agentSource(s Source, name string) (Source, bool) {
	if id, ok := agentTranscript(name); ok {
		s.Kind, s.Stream = SrcAgentTranscript, id
		return s, true
	}
	if id, ok := agentMeta(name); ok {
		s.Kind, s.Stream = SrcAgentMeta, id
		return s, true
	}
	return Source{}, false
}

// markLostSessions marks the cursors of the sessions in the storage root that
// discovery did not find. No other part of a pass visits such a session.
// Discovery listed none of its files. They are gone, or hidden by a directory
// of the session that discovery could not read, so markIfGone checks each
// file.
//
// A collector checks each such session until one check completes, and again
// only after discovery has found it in between. So a file hidden during that
// check and removed later keeps an active cursor while this collector runs,
// unless discovery finds the session again. Claude Code prunes by age, so a
// long-lived root holds more of these sessions every week, and reading all
// their cursors on every pass would cost more every week too. A session
// another program landed, such as a scenario sd build, is checked once, has
// no cursor of this adapter, and is left alone.
func (c *Collector) markLostSessions(found map[string]bool, filter func(Session) bool, st *Stats) error {
	items, err := os.ReadDir(c.Zone.Root())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	now := c.Now()
	for _, it := range items {
		id := it.Name()
		if !it.IsDir() || found[id] || !IsSessionID(id) || c.wasChecked(id) {
			continue
		}
		done, err := c.markLostSession(id, filter, st, now)
		if err != nil {
			st.Errors = append(st.Errors, fmt.Errorf("session %s: %w", id, err))
			continue
		}
		if done {
			c.setChecked(id, true)
		}
	}
	return nil
}

// markLostSession checks one session discovery did not find, under its lock.
// It reports false when another process holds the session, so that a later
// pass checks it.
func (c *Collector) markLostSession(id string, filter func(Session) bool, st *Stats, now time.Time) (bool, error) {
	lock, err := storage.LockSession(c.Zone.SessionDir(id))
	if err != nil {
		if errors.Is(err, storage.ErrSessionBusy) {
			return false, nil
		}
		return false, err
	}
	defer func() { _ = lock.Unlock() }()
	// A session the filter keeps out is not collected, and it is not
	// touched here either.
	if filter != nil {
		s, err := c.lostSession(id)
		if err != nil {
			return false, err
		}
		if !filter(s) {
			return true, nil
		}
	}
	return true, c.markPruned(id, nil, st, now)
}

// lostSession is what discovery would have made of a session, from the files
// its cursors name, for the filter to judge: the directory of its main
// transcript, and every directory its files were in.
func (c *Collector) lostSession(id string) (Session, error) {
	s := Session{ID: id}
	base := c.Zone.SessionDir(id)
	for _, sub := range []string{"streams", "runs"} {
		owners, err := os.ReadDir(filepath.Join(base, sub))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return s, err
		}
		for _, o := range owners {
			if !o.IsDir() {
				continue
			}
			for _, name := range cursorNames[sub] {
				cur, err := storage.LoadCursor(filepath.Join(base, sub, o.Name(), name), storage.CursorAppend, "")
				if err != nil {
					return s, err
				}
				src, ok := c.sourceOf(cur.Source, id)
				if !ok {
					continue
				}
				dir := path.Dir(src.Rel)
				if src.Kind == SrcMainTranscript {
					s.Primary = dir
				}
				if project := strings.SplitN(src.Rel, "/", 2)[0]; !contains(s.Dirs, project) {
					s.Dirs = append(s.Dirs, project)
				}
			}
		}
	}
	sort.Strings(s.Dirs)
	return s, nil
}

func (c *Collector) wasChecked(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.checked[id]
}

func (c *Collector) setChecked(id string, checked bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !checked {
		delete(c.checked, id)
		return
	}
	if c.checked == nil {
		c.checked = map[string]bool{}
	}
	c.checked[id] = true
}
