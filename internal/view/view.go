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

// Package view serves a conversation to a reader.
//
// It folds the chain on demand and caches nothing on disk. Measured on the
// largest real conversation, folding takes 302 ms, building the talk list takes
// 1 ms, and walking one talk's subtree takes 13 microseconds - so a derived read
// artefact would solve a problem that does not exist at this size.
//
// One thing is held in memory per conversation: the time of each landed
// position. A round carries no wall-clock time, deliberately, so that its bytes
// stay reproducible; a timeline needs one per step. Both hold, because the
// index already has it and the lookup is built once on load.
package view

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// Server reads conversations out of a storage zone.
type Server struct {
	zone *storage.Zone

	mu     sync.Mutex
	loaded map[string]*Conversation

	statusMu sync.Mutex
	status   Status

	// glossaries hold each runtime's words, by dialect. A root can carry
	// conversations from more than one, so which to serve is decided from
	// what the root holds rather than from what the command was built with.
	glossaries map[string]*model.Glossary
	// glossary says what the runtime calls each name the model uses. It is
	// supplied by whoever wires the server, never imported from an adapter,
	// so the side that reads never depends on the side that collects.
	glossary *model.Glossary
}

// New returns a server over a zone. Nothing refreshes it until a caller says
// so through SetStatus. A nil glossary means the runtime's words are not
// available to the page.
func New(z *storage.Zone, glossary *model.Glossary) *Server {
	return &Server{zone: z, loaded: map[string]*Conversation{}, status: Status{Mode: ModeStatic}, glossary: glossary}
}

// NewWithGlossaries is New for a root that can hold more than one runtime.
//
// The page asks for one glossary as it loads, so one is chosen: the dialect
// the root's own landed files were read in. A root holding two runtimes still
// gets one answer, which is a limit of that contract rather than of this —
// serving both would need the page to ask per conversation.
func NewWithGlossaries(z *storage.Zone, glossaries map[string]*model.Glossary) *Server {
	s := New(z, nil)
	s.glossaries = glossaries
	return s
}

// Conversation is one folded chain, with the times its rounds do not carry.
type Conversation struct {
	ID      string
	View    *sessionflow.View
	Session string

	// at maps a landed position to when the runtime says it happened.
	at map[[2]uint64]int64
	// lanes maps a landed sequence to the lane its file belongs to: a stream,
	// a workflow run, or the session itself. A position orders records only
	// inside one lane.
	lanes map[uint64]string
	// from and to index relations by the node they touch, so an inspector does
	// not scan every edge for every step.
	from map[string][]*sessionflow.Relation
	to   map[string][]*sessionflow.Relation

	zone *storage.Zone

	// paths maps a landed sequence to the file carrying it. It is built once,
	// here, because it is read from many request goroutines at the same time
	// and a lazily filled map is a data race the race detector catches on two
	// concurrent record reads.
	//
	// A file that lands after this was built is not in it, so a miss rescans
	// under the lock rather than failing.
	pathsMu   sync.Mutex
	paths     map[uint64]string
	pathsScan time.Time

	// built is the Conversation View, made once per fold. Forget drops the
	// whole Conversation, and the view with it, when a round arrives.
	builtMu sync.Mutex
	built   *sessionview.Conversation

	// problems is what stopped the fold short of the chain's last file, in
	// words, for the document to carry.
	problems []string
	// head is the round this fold actually reached, which is not always the
	// last round on disk. A round file becomes visible before its bytes are
	// all there, so a fold can stop one short of what the directory lists.
	// Keeping what was folded, rather than what was listed, makes the next
	// read try again instead of serving the short fold for good.
	head uint64
}

// Load folds a conversation and builds its lookups, once per round.
//
// A fold is kept until the chain grows past it. The check is the head round
// on disk, which is a directory listing, not a fold. It has to be made on
// every read: the rounds may be written by another process entirely, such
// as an asz collect running beside a read-only asz view, and then nothing
// in this process knows the conversation moved.
//
// What is kept is the round the fold reached, not the round the directory
// listed. A round being written is visible before it is complete, so the
// two can differ; comparing against what was folded makes the next read
// pick the rest up.
func (s *Server) Load(id string) (*Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.loaded[id]; ok {
		head, err := sessionflow.OpenChain(s.zone.Root(), id).Head()
		if err == nil && head == c.head {
			return c, nil
		}
		delete(s.loaded, id)
	}
	// The fold goes as far as the chain holds. A round that is missing or
	// broken is reported in the document, not returned as an error, so a
	// reader sees what could be folded and what could not; only a chain with
	// no usable round at all is an error, because there is nothing to show.
	v, problems, ferr := sessionflow.OpenChain(s.zone.Root(), id).FoldPartial()
	if ferr != nil {
		return nil, ferr
	}
	if v.Round == 0 {
		if len(problems) > 0 {
			return nil, fmt.Errorf("view: %s: %s", id, problems[0])
		}
		return nil, fmt.Errorf("view: no rounds for %s; run asz parse", id)
	}
	c := &Conversation{
		ID: id, View: v, Session: v.Session, zone: s.zone, problems: problems, head: v.Round,
		at:    map[[2]uint64]int64{},
		lanes: map[uint64]string{},
		from:  map[string][]*sessionflow.Relation{},
		to:    map[string][]*sessionflow.Relation{},
	}
	// The time of every landed position, from the records themselves. A node
	// carries {seq, row}; this is what turns that into a moment. The page reads
	// Session Data and Session Flow and nothing else: the index is assembly's
	// accelerator, and a root that arrives without one still shows its times.
	if err := timesOf(s.zone, v.Session, c.at, c.lanes); err != nil {
		return nil, err
	}
	for _, r := range v.Relations {
		c.from[r.From] = append(c.from[r.From], r)
		c.to[r.To] = append(c.to[r.To], r)
	}
	c.paths = c.scanPaths()
	c.pathsScan = time.Now()
	s.loaded[id] = c
	return c, nil
}

// Forget drops a cached conversation, so a later read sees new rounds.
func (s *Server) Forget(id string) {
	s.mu.Lock()
	delete(s.loaded, id)
	s.mu.Unlock()
}

// Time returns when a node happened, or 0 when nothing observed it.
//
// A node with no reference has no observed time, and a zero must render as
// "none" rather than as 1970 - which is the most likely bug in anything that
// reads this.
func (c *Conversation) Time(n *sessionflow.Node) int64 {
	if n == nil || n.Ref == nil {
		return 0
	}
	return c.at[[2]uint64{n.Ref.Seq, n.Ref.Row}]
}

// Span returns the first and last observed time under a node.
func (c *Conversation) Span(n *sessionflow.Node) (int64, int64) {
	var lo, hi int64
	var walk func(*sessionflow.Node)
	walk = func(x *sessionflow.Node) {
		if t := c.Time(x); t != 0 {
			if lo == 0 || t < lo {
				lo = t
			}
			if t > hi {
				hi = t
			}
		}
		for _, k := range c.View.Children(x.ID) {
			walk(k)
		}
	}
	walk(n)
	return lo, hi
}

// Talks returns the conversation's talks in the order they happened.
//
// Across streams that order is their time, not their landed position. A
// position orders records within one stream, which is the only place it
// means anything: a child's file can land before its parent's, because one
// request carries both and the files are written per stream. Ordering by
// position put a sub-agent's turn before the turn that delegated to it,
// and the document says its talks are in time order.
//
// Position still decides between talks that share a time, or where a talk
// has no observed time at all, so the order stays the same on every read.
func (c *Conversation) Talks() []*sessionflow.Node {
	var out []*sessionflow.Node
	for _, n := range c.View.Nodes {
		if n.Kind == model.KindTalk {
			out = append(out, n)
		}
	}
	began := make(map[string]int64, len(out))
	for _, n := range out {
		from, _ := c.Span(n)
		began[n.ID] = from
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := began[out[i].ID], began[out[j].ID]
		// A total order, so the sort is defined: every timed talk before
		// every untimed one, timed talks by time, position only among
		// equals. Deciding some pairs by time and others by position was
		// not transitive, and a sort over that could leave two timed talks
		// reversed and give a different order on the next read.
		switch {
		case (a != 0) != (b != 0):
			return a != 0
		case a != b:
			return a < b
		}
		return sessionflow.Before(out[i], out[j])
	})
	return out
}

// Streams returns the conversation's execution streams, parent lineage first.
func (c *Conversation) Streams() []*sessionflow.Node {
	var out []*sessionflow.Node
	for _, n := range c.View.Nodes {
		if n.Kind == model.KindStream {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		mi := strings.Contains(string(out[i].Attrs), `"role":"main"`)
		mj := strings.Contains(string(out[j].Attrs), `"role":"main"`)
		if mi != mj {
			return mi
		}
		return sessionflow.Before(out[i], out[j])
	})
	return out
}

// List returns every conversation in the zone that has rounds.
// Root is the storage root the page reads.
func (s *Server) Root() string { return s.zone.Root() }

func (s *Server) List() ([]string, error) {
	base := filepath.Join(s.zone.Root(), "_conversations")
	items, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, d := range items {
		if !d.IsDir() {
			continue
		}
		// A writer killed during its first round leaves only a temporary
		// file, so an entry in the directory is not yet a round.
		if rounds, rerr := sessionflow.OpenChain(s.zone.Root(), d.Name()).List(); rerr == nil && len(rounds) > 0 {
			out = append(out, d.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// Millis renders an observed time as unix milliseconds, or 0 when
// unobserved. It floors, as time.Time does, so a time before 1970 rounds
// the same way everywhere; a duration is not a time and takes
// durationMillis.
func Millis(ns int64) int64 {
	if ns == 0 {
		return 0
	}
	return time.Unix(0, ns).UnixMilli()
}

// durationMillis renders an elapsed time as milliseconds, toward zero.
func durationMillis(ns int64) int64 {
	return ns / int64(time.Millisecond)
}

// timesOf fills at with the time of every record in the session's landed
// files, keyed by landed position, and lanes with the lane of every file.
//
// Only the time is taken from each line, without decoding the record: a
// conversation has tens of thousands of records and the page needs one field
// of each. A file that fails to read contributes no times rather than failing
// the page; its records still render, without a moment.
func timesOf(z *storage.Zone, session string, at map[[2]uint64]int64, lanes map[uint64]string) error {
	files, err := storage.LandedFiles(z, session)
	if err != nil {
		return err
	}
	for _, lf := range files {
		switch {
		case lf.Stream != "":
			lanes[lf.Seq] = "stream/" + lf.Stream
		case lf.RunID != "":
			lanes[lf.Seq] = "run/" + lf.RunID
		}
		f, err := os.Open(lf.Path)
		if err != nil {
			continue
		}
		r, err := sessiondata.NewReader(f)
		if err != nil {
			f.Close()
			continue
		}
		for row := uint64(1); ; row++ {
			line, err := r.NextRaw()
			if err != nil {
				break
			}
			if ns, ok := sessiondata.LineTime(line); ok {
				at[[2]uint64{lf.Seq, row}] = ns
			}
		}
		f.Close()
	}
	return nil
}
