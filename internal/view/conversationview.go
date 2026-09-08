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
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/verify"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// apiView serves the whole conversation as one asz.view document.
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
	// Encoded whole before it is sent, so the answer carries its length and
	// the page can show the download against it. A document is tens of
	// megabytes at most, and it is built whole in memory already.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	_, _ = w.Write(buf.Bytes())
}

// Build makes the asz.view document: the fold, every talk as a tree with
// the text its records carry, and the rounds and files it was built from,
// each verified. It is made once per fold. The records every tree needs
// are read in one pass per landed file rather than one per talk.
func (c *Conversation) Build() (*sessionview.Conversation, error) {
	c.builtMu.Lock()
	defer c.builtMu.Unlock()
	if c.built != nil {
		return c.built, nil
	}
	o := c.overview()
	v := &sessionview.Conversation{
		Format: sessionview.Format, Version: sessionview.Version,
		Conversation: c.ID, Sessions: c.sessions(),
		Head:   sessionview.Head{Round: c.View.Round, Digest: c.View.Digest},
		Parser: c.View.Parser, Policy: c.View.Policy,
		Summary: sessionview.Summary{
			Title: o.title, Problems: []string{},
			Kinds: o.kinds, RelationTypes: o.rels, Quality: o.quality,
		},
		Streams: o.streams, Segments: o.segments,
		Rounds: []sessionview.Round{}, Files: []sessionview.File{}, Talks: []sessionview.Node{}, Loose: []sessionview.Node{},
		Relations: []sessionview.Relation{}, Unresolved: []sessionview.Unresolved{},
		WorkspaceChanges: []sessionview.WorkspaceChange{},
	}
	if sn := c.View.Nodes[sessionflow.NodeID("session", c.Session)]; sn != nil {
		v.Summary.From, v.Summary.To = millisOf(attrString(sn, "from_time")), millisOf(attrString(sn, "through_time"))
	}

	landed, err := storage.LandedFiles(c.zone, c.Session)
	if err != nil {
		return nil, err
	}
	files, digests, err := c.files(landed)
	if err != nil {
		return nil, err
	}
	rounds, roundFiles, problems, state := c.rounds(digests)
	v.Rounds = rounds
	v.Files = append(files, roundFiles...)
	if len(c.problems) > 0 {
		problems = append(append([]string{}, c.problems...), problems...)
		if state == sessionview.StateVerified {
			state = sessionview.StateIncomplete
		}
	}
	v.Summary.State, v.Summary.Problems = state, problems

	talks := c.Talks()
	loose := c.looseRoots()
	var refs []*sessionflow.Ref
	for _, t := range talks {
		refs = append(refs, c.refsUnder(t)...)
	}
	for _, n := range loose {
		refs = append(refs, c.refsUnder(n)...)
	}
	recs := c.records(refs)
	rows := map[string]talkRow{}
	for _, row := range o.talks {
		rows[row.ID] = row
	}
	for _, t := range talks {
		n := c.step(t, 0, recs)
		if row, ok := rows[t.ID]; ok {
			n.Label, n.Reply = row.Label, row.Reply
			n.Runs, n.Steps, n.Tools = row.Runs, row.Steps, row.Tools
			n.From, n.To, n.Child, n.Segment = row.From, row.To, row.Child, row.Segment
		}
		v.Talks = append(v.Talks, n)
	}
	// Steps are every node that is not structure, counted over the whole
	// fold, which is what the list page and the round header count too.
	steps := 0
	for _, n := range c.View.Nodes {
		if isStep(n.Kind) {
			steps++
		}
	}

	for _, n := range loose {
		v.Loose = append(v.Loose, c.step(n, 0, recs))
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
	v.Summary.Talks, v.Summary.Steps = len(v.Talks), steps
	v.Summary.Streams, v.Summary.Segments = len(v.Streams), len(v.Segments)
	v.Summary.Rounds, v.Summary.Unresolved = len(v.Rounds), len(o.open)

	// The workspace changes, joined to their steps by tool-use id, and
	// each step told which records are its own.
	v.WorkspaceChanges = c.workspaceChanges(landed, recs)
	byStep := map[string][]string{}
	for _, wc := range v.WorkspaceChanges {
		if wc.Step != "" {
			byStep[wc.Step] = append(byStep[wc.Step], wc.ID)
		}
	}
	annotateChanges(v.Talks, byStep)
	annotateChanges(v.Loose, byStep)
	v.Summary.Changes = len(v.WorkspaceChanges)
	c.built = v
	return v, nil
}

// looseRoots finds the runs and steps no talk contains, and returns the
// highest such ancestor of each, in order, so the document can hold them
// as trees. A step is contained by a talk when a talk is above it; one
// whose ancestors are only structure, the session, a stream, an epoch or a
// segment, is loose.
func (c *Conversation) looseRoots() []*sessionflow.Node {
	roots := map[string]*sessionflow.Node{}
	for _, n := range c.View.Nodes {
		if n.Kind != model.KindRun && !isStep(n.Kind) {
			continue
		}
		top := n
		covered := false
		for cur := n; cur != nil; cur = c.View.Nodes[cur.Parent] {
			if cur.Kind == model.KindTalk {
				covered = true
				break
			}
			if cur.Kind == model.KindRun || isStep(cur.Kind) {
				top = cur
			}
		}
		if !covered {
			roots[top.ID] = top
		}
	}
	var out []*sessionflow.Node
	for _, n := range roots {
		out = append(out, n)
	}
	return sessionflow.InOrder(out)
}

// sessions lists the sessions the fold holds, the conversation's own first.
func (c *Conversation) sessions() []string {
	out := []string{c.Session}
	for _, n := range c.View.NodesByKind(model.KindSession) {
		if id := strings.TrimPrefix(n.ID, "session/"); id != c.Session {
			out = append(out, id)
		}
	}
	return out
}

// rounds lists the chain from each round's header and file, and checks it
// the way asz verify does: the digest of each round, its link to the round
// before, and its input digest over the landed files it names, computed as
// the parser computed it. What fails is reported as content, never as an
// error, so a reader still gets whatever folds.
func (c *Conversation) rounds(digests map[uint64]string) (rounds []sessionview.Round, files []sessionview.File, problems []string, state string) {
	problems = []string{}
	state = sessionview.StateVerified
	rep, err := verify.Chain(c.zone, c.ID, digests)
	if err != nil {
		return nil, nil, append(problems, err.Error()), sessionview.StateIncomplete
	}
	for _, rr := range rep.Rounds {
		problems = append(problems, rr.Problems...)
		if rr.OpenError != "" {
			state = sessionview.StateMismatch
			continue
		}
		// Changed evidence is a mismatch; evidence that is gone leaves the
		// document incomplete, and a mismatch is never downgraded.
		switch {
		case rr.Damaged():
			state = sessionview.StateMismatch
		case !rr.OK() && state == sessionview.StateVerified:
			state = sessionview.StateIncomplete
		}
		h := rr.Header
		data, _ := os.ReadFile(rr.Path)
		var previous *string
		if h.Previous != "" {
			p := h.Previous
			previous = &p
		}
		rounds = append(rounds, sessionview.Round{
			Round: h.Round, Digest: rr.Digest, Previous: previous,
			FromSeq: h.FromSeq, ThroughSeq: h.ThroughSeq, InputDigest: h.InputDigest,
			FromTime: millisPtr(h.FromTime), ThroughTime: millisPtr(h.ThroughTime),
			Verified: rr.OK(),
		})
		round := h.Round
		files = append(files, sessionview.File{
			File: c.relPath(rr.Path), Format: "sf", Kind: "round", Round: &round,
			Lines: countByte(data, '\n'), Bytes: int64(len(data)),
			Digest: digestOf(data), FromTime: millisPtr(h.FromTime), ThroughTime: millisPtr(h.ThroughTime),
		})
	}
	return rounds, files, problems, state
}

// files lists the session's landed files, each with its digest, its size
// and the record time range the page already read, and returns the digests
// by sequence for the chain check.
func (c *Conversation) files(landed []storage.LandedFile) ([]sessionview.File, map[uint64]string, error) {
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
	digests := map[uint64]string{}
	for _, lf := range landed {
		data, err := os.ReadFile(lf.Path)
		if err != nil {
			return nil, nil, err
		}
		var hdr sessiondata.Header
		if i := indexByte(data, '\n'); i >= 0 {
			_ = json.Unmarshal(data[:i], &hdr)
		}
		seq := lf.Seq
		digest := digestOf(data)
		digests[seq] = digest
		f := sessionview.File{
			File: c.relPath(lf.Path), Format: "sd", Kind: string(hdr.Kind), Seq: &seq,
			Stream: strPtr(lf.Stream), Run: strPtr(lf.RunID),
			Lines: countByte(data, '\n'), Bytes: int64(len(data)), Digest: digest,
		}
		if s, ok := spans[lf.Seq]; ok {
			from, to := Millis(s.from), Millis(s.to)
			f.FromTime, f.ThroughTime = &from, &to
		}
		out = append(out, f)
	}
	return out, digests, nil
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

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
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

// millisPtr is millisOf for a key that is null when absent.
func millisPtr(s string) *int64 {
	if s == "" {
		return nil
	}
	ms := millisOf(s)
	return &ms
}
