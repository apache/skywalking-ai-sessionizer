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

package view

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// apiView serves the whole conversation as one document.
func (s *Server) apiView(w http.ResponseWriter, id string) {
	c, err := s.Load(id)
	if err != nil {
		fail(w, err, http.StatusNotFound)
		return
	}
	v, err := c.Build()
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, v)
}

// Build makes the Conversation View: the fold, every talk as a tree with
// the text its records carry, and the rounds and files it was built from.
// It is made once per fold. The records every tree needs are read in one
// pass per landed file rather than one per talk.
func (c *Conversation) Build() (*sessionview.Conversation, error) {
	c.builtMu.Lock()
	defer c.builtMu.Unlock()
	if c.built != nil {
		return c.built, nil
	}
	o := c.overview()
	v := &sessionview.Conversation{
		Schema: sessionview.Schema, ID: c.ID, Session: c.Session, Title: o.title,
		Head: sessionview.Head{
			Round: c.View.Round, Digest: c.View.Digest, ThroughSeq: c.View.ThroughSeq,
			InputDigest: c.View.InputDigest, Parser: c.View.Parser, Policy: c.View.Policy,
		},
		Kinds: o.kinds, RelationTypes: o.rels, Quality: o.quality,
		Streams: o.streams, Segments: o.segments,
		Rounds: []sessionview.Round{}, Files: []sessionview.File{}, Talks: []sessionview.Talk{},
		Relations: []sessionview.Relation{}, Unresolved: []sessionview.Unresolved{},
	}
	if sn := c.View.Nodes[sessionflow.NodeID("session", c.Session)]; sn != nil {
		v.From, v.To = millisOf(attrString(sn, "from_time")), millisOf(attrString(sn, "through_time"))
	}
	rounds, err := c.rounds()
	if err != nil {
		return nil, err
	}
	v.Rounds = rounds
	files, err := c.files()
	if err != nil {
		return nil, err
	}
	v.Files = files

	talks := c.Talks()
	var refs []*sessionflow.Ref
	for _, t := range talks {
		refs = append(refs, c.refsUnder(t)...)
	}
	recs := c.records(refs)
	byID := map[string]*sessionflow.Node{}
	for _, t := range talks {
		byID[t.ID] = t
	}
	steps := 0
	for _, row := range o.talks {
		t := row.view()
		if n := byID[row.ID]; n != nil {
			tree := c.step(n, 0, recs)
			t.Tree = &tree
		}
		steps += row.Steps
		v.Talks = append(v.Talks, t)
	}

	for _, r := range c.View.Relations {
		v.Relations = append(v.Relations, sessionview.Relation{
			ID: r.ID, Type: r.Type, From: r.From, To: r.To, Quality: r.Quality, Via: r.Via, Evidence: r.Evidence,
		})
	}
	sort.Slice(v.Relations, func(i, j int) bool { return v.Relations[i].ID < v.Relations[j].ID })
	for _, u := range c.View.Unresolved {
		v.Unresolved = append(v.Unresolved, sessionview.Unresolved{ID: u.ID, Kind: u.Kind, Ref: u.RefID, Reason: u.Reason, State: u.State})
	}
	sort.Slice(v.Unresolved, func(i, j int) bool { return v.Unresolved[i].ID < v.Unresolved[j].ID })
	v.Counts = sessionview.Counts{
		Nodes: len(c.View.Nodes), Relations: len(c.View.Relations), Unresolved: len(o.open),
		Talks: len(v.Talks), Steps: steps, Streams: len(v.Streams), Segments: len(v.Segments),
	}
	c.built = v
	return v, nil
}

// rounds lists the chain from each round's header and file.
func (c *Conversation) rounds() ([]sessionview.Round, error) {
	chain := sessionflow.OpenChain(c.zone.Root(), c.ID)
	list, err := chain.List()
	if err != nil {
		return nil, err
	}
	out := make([]sessionview.Round, 0, len(list))
	for _, rf := range list {
		r, err := chain.Open(rf.Path)
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(rf.Path)
		if err != nil {
			return nil, err
		}
		h := r.Header
		out = append(out, sessionview.Round{
			Round: h.Round, Digest: r.Commit.Digest, Previous: h.Previous,
			FromSeq: h.FromSeq, ThroughSeq: h.ThroughSeq, InputDigest: h.InputDigest,
			From: millisOf(h.FromTime), To: millisOf(h.ThroughTime),
			SessionFrom: millisOf(h.SessionFromTime), SessionTo: millisOf(h.SessionThroughTime),
			Lines: 2 + len(r.Nodes) + len(r.Relations) + len(r.Unresolved), Bytes: int64(len(data)),
			FileDigest: digestOf(data), File: c.relPath(rf.Path),
		})
	}
	return out, nil
}

// files lists the session's landed files, each with its digest, its size
// and the record time range the page already read.
func (c *Conversation) files() ([]sessionview.File, error) {
	landed, err := storage.LandedFiles(c.zone, c.Session)
	if err != nil {
		return nil, err
	}
	type span struct{ from, to int64 }
	spans := map[uint64]span{}
	for pos, ns := range c.at {
		s := spans[pos[0]]
		if s.from == 0 || ns < s.from {
			s.from = ns
		}
		if ns > s.to {
			s.to = ns
		}
		spans[pos[0]] = s
	}
	out := make([]sessionview.File, 0, len(landed))
	for _, lf := range landed {
		data, err := os.ReadFile(lf.Path)
		if err != nil {
			return nil, err
		}
		var hdr sessiondata.Header
		if i := indexByte(data, '\n'); i >= 0 {
			_ = json.Unmarshal(data[:i], &hdr)
		}
		f := sessionview.File{
			File: c.relPath(lf.Path), Format: "sd", Version: hdr.Schema, Kind: string(hdr.Kind),
			Seq: lf.Seq, Stream: lf.Stream, Run: lf.RunID,
			Lines: countByte(data, '\n'), Bytes: int64(len(data)), Digest: digestOf(data),
		}
		if s, ok := spans[lf.Seq]; ok {
			f.From, f.To = Millis(s.from), Millis(s.to)
		}
		out = append(out, f)
	}
	return out, nil
}

func (c *Conversation) relPath(path string) string {
	rel, err := filepath.Rel(c.zone.Root(), path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func indexByte(b []byte, x byte) int {
	for i := range b {
		if b[i] == x {
			return i
		}
	}
	return -1
}

func countByte(b []byte, x byte) int {
	n := 0
	for i := range b {
		if b[i] == x {
			n++
		}
	}
	return n
}

// millisOf renders a record time a round header carries as unix
// milliseconds, or 0 when it carries none.
func millisOf(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}
