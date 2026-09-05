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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// step is one node as a reader meets it.
// step and edge are the Conversation View's node and edge: the page serves
// the same shape the view is made of, so there is one definition.
type (
	step = sessionview.Node
	edge = sessionview.Edge
)

const preview = 2000

func (s *Server) apiTalk(w http.ResponseWriter, id, talk string) {
	c, err := s.Load(id)
	if err != nil {
		fail(w, err, http.StatusNotFound)
		return
	}
	n := c.View.Nodes[talk]
	if n == nil {
		fail(w, fmt.Errorf("no such talk: %s", talk), http.StatusNotFound)
		return
	}
	// Every record this talk cites, read with one pass per landed file. Read
	// one at a time, a step at row N costs a scan to row N; a talk of several
	// hundred steps then rescans the same file hundreds of times.
	writeJSON(w, c.step(n, 0, c.records(c.refsUnder(n))))
}

// refsUnder collects every landed position the subtree reads content from.
func (c *Conversation) refsUnder(n *sessionflow.Node) []*sessionflow.Ref {
	var out []*sessionflow.Ref
	var walk func(*sessionflow.Node)
	walk = func(x *sessionflow.Node) {
		if carriesContent(x.Kind) {
			if x.Ref != nil {
				out = append(out, x.Ref)
			}
			// refs[0] is the request and refs[1] is the result; both are read.
			for i := range x.Refs {
				out = append(out, &x.Refs[i])
			}
		}
		if r := usageAt(x); r != nil {
			out = append(out, r)
		}
		for _, k := range c.View.Children(x.ID) {
			walk(k)
		}
	}
	walk(n)
	return out
}

// records reads many landed positions with one pass per file.
func (c *Conversation) records(refs []*sessionflow.Ref) map[[2]uint64]*sessiondata.Record {
	want := map[uint64]map[uint64]bool{}
	for _, r := range refs {
		if r == nil {
			continue
		}
		if want[r.Seq] == nil {
			want[r.Seq] = map[uint64]bool{}
		}
		want[r.Seq][r.Row] = true
	}
	out := map[[2]uint64]*sessiondata.Record{}
	for seq, rows := range want {
		path, err := c.landedPath(seq)
		if err != nil {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		rd, err := sessiondata.NewReader(f)
		if err != nil {
			f.Close()
			continue
		}
		left := len(rows)
		for i := uint64(1); left > 0; i++ {
			rec, rerr := rd.Next()
			if rerr != nil {
				break
			}
			if rows[i] {
				out[[2]uint64{seq, i}] = rec
				left--
			}
		}
		f.Close()
	}
	return out
}

// step renders one node and everything under it.
func (c *Conversation) step(n *sessionflow.Node, depth int, recs map[[2]uint64]*sessiondata.Record) step {
	out := step{
		ID: n.ID, Kind: n.Kind, Parent: n.Parent, Stream: n.Stream,
		At: Millis(c.Time(n)), Ref: n.Ref, Refs: n.Refs, Attrs: n.Attrs,
	}
	// Both directions together, by relation id and then direction, so the
	// same fold renders the same edges in the same order every time; a
	// relation from a node to itself appears once in each direction.
	for _, r := range c.from[n.ID] {
		out.Edges = append(out.Edges, edge{Type: r.Type, Other: r.To, Dir: "out", Quality: r.Quality, Via: r.Via, ID: r.ID})
	}
	for _, r := range c.to[n.ID] {
		out.Edges = append(out.Edges, edge{Type: r.Type, Other: r.From, Dir: "in", Quality: r.Quality, Via: r.Via, ID: r.ID})
	}
	sort.SliceStable(out.Edges, func(i, j int) bool {
		if out.Edges[i].ID != out.Edges[j].ID {
			return out.Edges[i].ID < out.Edges[j].ID
		}
		return out.Edges[i].Dir < out.Edges[j].Dir
	})
	// The content a reader sees, taken from the part this node points at.
	//
	// Only a leaf carries content. A provider call is a container - its text,
	// its reasoning and its tools are its children - so filling it from the
	// record it starts at would show a tool's command as the call's own words.
	if n.Ref != nil && carriesContent(n.Kind) {
		if rec := recs[[2]uint64{n.Ref.Seq, n.Ref.Row}]; rec != nil {
			fill(&out, rec, n.Ref.Block)
			fillDuration(&out, n, rec)
			// What else the record says, copied once so a viewer never
			// opens a landed file: its flags and what the conversion left out.
			out.Flags, out.Dropped = rec.Flags, rec.Dropped
		}
		// A tool use is one step carrying request and result. The request is
		// refs[0]; anything after it is what came back.
		for i := 1; i < len(n.Refs); i++ {
			r := n.Refs[i]
			if rec := recs[[2]uint64{r.Seq, r.Row}]; rec != nil && out.Result == "" {
				fillResult(&out, rec, r.Block)
			}
		}
		c.fillRequestToResult(&out, n)
	}
	// A call's token counts come from the one record usage_at names. Never
	// from summing fragments: a main transcript stamps usage on every one.
	if r := usageAt(n); r != nil {
		if rec := recs[[2]uint64{r.Seq, r.Row}]; rec != nil && rec.Usage != nil {
			out.Usage = rec.Usage
		}
	}
	if depth < 12 {
		for _, k := range c.View.Children(n.ID) {
			out.Children = append(out.Children, c.step(k, depth+1, recs))
		}
	}
	return out
}

// usageAt is the record an llm.call's usage is read from, named by the
// assembler in the node's attrs, or nil for any other node.
func usageAt(n *sessionflow.Node) *sessionflow.Ref {
	if n.Kind != model.KindLLMCall || len(n.Attrs) == 0 {
		return nil
	}
	var a struct {
		UsageAt *sessionflow.Ref `json:"usage_at"`
	}
	if json.Unmarshal(n.Attrs, &a) != nil || a.UsageAt == nil {
		return nil
	}
	return a.UsageAt
}

// fill takes a step's readable content from the record it points at.
func fill(s *step, rec *sessiondata.Record, block *int) {
	var p *sessiondata.Part
	if block != nil && *block < len(rec.Parts) {
		p = &rec.Parts[*block]
	} else if len(rec.Parts) == 1 {
		p = &rec.Parts[0]
	}
	if p == nil {
		s.Text = clip(readable(rec))
		return
	}
	s.Name, s.Failed, s.State, s.Bytes = p.Name, p.Failed, p.State, p.Bytes
	switch {
	case p.Text != "":
		s.Text = clip(p.Text)
	case len(p.Data) > 0:
		// A wrapped human message reads as the message, not as its wrapper.
		if t := readable(rec); t != "" && t != strings.TrimSpace(string(p.Data)) {
			s.Text = clip(t)
		} else {
			s.Text = clip(string(p.Data))
		}
	}
}

// fillRequestToResult reports the interval between the request record and the
// result record, where an exact identifier ties the two together.
//
// The assembler says timing is unavailable, and that stays true: the runtime
// does not report how long the tool ran. This is a different and smaller
// claim - the two records are joined on the tool-use identifier, so the time
// between them is attributable to this call rather than guessed from whatever
// was written next. Where the join is anything less than exact, nothing is
// reported at all.
func (c *Conversation) fillRequestToResult(out *step, n *sessionflow.Node) {
	if len(n.Refs) < 2 {
		return
	}
	var a struct {
		Join string `json:"result_join"`
	}
	if len(n.Attrs) > 0 {
		_ = json.Unmarshal(n.Attrs, &a)
	}
	if a.Join != model.ExactUnique {
		return
	}
	from := c.at[[2]uint64{n.Refs[0].Seq, n.Refs[0].Row}]
	to := c.at[[2]uint64{n.Refs[1].Seq, n.Refs[1].Row}]
	if from == 0 || to == 0 || to < from {
		return
	}
	out.RequestToResultMS = durationMillis(to - from)
	out.RequestToResultBy = a.Join
}

// fillResult takes what a tool sent back from the record it points at.
func fillResult(s *step, rec *sessiondata.Record, block *int) {
	var p *sessiondata.Part
	if block != nil && *block < len(rec.Parts) {
		p = &rec.Parts[*block]
	} else {
		// The result part of the record, whichever block holds it.
		for i := range rec.Parts {
			if rec.Parts[i].Kind == sessiondata.PartResult {
				p = &rec.Parts[i]
				break
			}
		}
	}
	if p == nil {
		return
	}
	s.ResultState, s.ResultBytes = p.State, p.Bytes
	if p.Failed != nil && s.Failed == nil {
		s.Failed = p.Failed
	}
	switch {
	case p.Text != "":
		s.Result = clip(p.Text)
	case len(p.Data) > 0:
		s.Result = clip(string(p.Data))
	}
}

// fillDuration reports a duration only where the runtime measured one.
//
// Nothing here is inferred from the gap between two records. A step whose
// source does not report a duration keeps none, and the reader is told so
// rather than shown a number that was calculated.
func fillDuration(s *step, n *sessionflow.Node, rec *sessiondata.Record) {
	if n.Kind != model.KindTurnDuration {
		return
	}
	// The runtime reports this as a whole record rather than as readable
	// text, so it arrives as a data part. Text() sees no text part and
	// returns nothing; the bytes are what has to be read.
	var probe struct {
		DurationMS int64 `json:"durationMs"`
	}
	ok := false
	for _, raw := range candidates(rec) {
		if json.Unmarshal(raw, &probe) == nil && probe.DurationMS != 0 {
			ok = true
			break
		}
	}
	if !ok {
		return
	}
	s.DurationMS = probe.DurationMS
	var a struct {
		MeasuredBy string `json:"measured_by"`
	}
	if len(n.Attrs) > 0 {
		_ = json.Unmarshal(n.Attrs, &a)
	}
	s.DurationHow = a.MeasuredBy
	// The raw record is not what a reader wants to see for a duration.
	s.Text = ""
}

// readable returns what a person would read in a record.
//
// A message someone typed while the agent was working does not arrive as
// text. The runtime wraps it in a queued_command envelope and lands it as a
// data part, so Text() finds nothing and the card shows the envelope instead
// of the sentence. The words are in there; this is where they are taken out.
func readable(rec *sessiondata.Record) string {
	if t := strings.TrimSpace(rec.Text()); t != "" {
		return t
	}
	for _, raw := range candidates(rec) {
		var env struct {
			Type   string `json:"type"`
			Prompt []struct {
				Text string `json:"text"`
			} `json:"prompt"`
		}
		if json.Unmarshal(raw, &env) != nil || env.Type != "queued_command" {
			continue
		}
		var out []string
		for _, p := range env.Prompt {
			if p.Text != "" {
				out = append(out, p.Text)
			}
		}
		if len(out) > 0 {
			return strings.TrimSpace(strings.Join(out, "\n"))
		}
	}
	return ""
}

// candidates returns every place a record's own JSON may be found.
func candidates(rec *sessiondata.Record) [][]byte {
	var out [][]byte
	for i := range rec.Parts {
		if len(rec.Parts[i].Data) > 0 {
			out = append(out, rec.Parts[i].Data)
		}
		if t := rec.Parts[i].Text; t != "" {
			out = append(out, []byte(t))
		}
	}
	if t := rec.Text(); t != "" {
		out = append(out, []byte(t))
	}
	return out
}

// carriesContent reports whether a step is something a reader reads, rather
// than something that contains what they read.
func carriesContent(kind string) bool {
	switch kind {
	case model.KindLLMCall, model.KindSession, model.KindSegment,
		model.KindStream, model.KindEpoch, model.KindTalk, model.KindRun:
		return false
	}
	return true
}

// clip keeps the longest prefix of whole characters within the preview
// budget, so a preview never ends in a broken character.
func clip(t string) string {
	if len(t) <= preview {
		return t
	}
	cut := preview
	for cut > 0 && !utf8.RuneStart(t[cut]) {
		cut--
	}
	return t[:cut]
}

func (s *Server) apiRecord(w http.ResponseWriter, id string, seq, row uint64) {
	c, err := s.Load(id)
	if err != nil {
		fail(w, err, http.StatusNotFound)
		return
	}
	rec, err := c.record(seq, row)
	if err != nil {
		fail(w, err, http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{
		"seq": seq, "row": row, "record": rec, "at": Millis(c.at[[2]uint64{seq, row}]),
	})
}

// record reads one landed record whole.
//
// Nothing is cached: a record is read when a reader asks to see it, which is
// the only time its bytes are wanted. The landed file is found by its sequence,
// which is unique across every stream in a session.
func (c *Conversation) record(seq, row uint64) (*sessiondata.Record, error) {
	path, err := c.landedPath(seq)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r, err := sessiondata.NewReader(f)
	if err != nil {
		return nil, err
	}
	for i := uint64(1); ; i++ {
		rec, rerr := r.Next()
		if errors.Is(rerr, io.EOF) {
			return nil, fmt.Errorf("view: no row %d in sequence %d", row, seq)
		}
		if rerr != nil {
			return nil, rerr
		}
		if i == row {
			return rec, nil
		}
	}
}

var seqSuffix = regexp.MustCompile(`-(\d{6,})\.sd$`)

// scanPaths walks the session once and maps every landed sequence to its file.
//
// The sequence is monotonic across every stream and run in a session, so it
// identifies a file on its own - which stream it belongs to does not have to be
// known, and must not be assumed.
func (c *Conversation) scanPaths() map[uint64]string {
	out := map[uint64]string{}
	_ = filepath.WalkDir(c.zone.SessionDir(c.Session), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		m := seqSuffix.FindStringSubmatch(d.Name())
		if m == nil {
			return nil
		}
		if n, cerr := strconv.ParseUint(m[1], 10, 64); cerr == nil {
			out[n] = p
		}
		return nil
	})
	return out
}

// landedPath finds the landed file carrying a sequence.
func (c *Conversation) landedPath(seq uint64) (string, error) {
	c.pathsMu.Lock()
	defer c.pathsMu.Unlock()
	if p, ok := c.paths[seq]; ok {
		return p, nil
	}
	// Not in the map. Either the sequence does not exist, or it landed after
	// this conversation was loaded, so look again before giving up.
	//
	// A sequence that does not exist would otherwise walk the whole session
	// directory on every request asking for it, so a rescan is allowed at most
	// once a second. Nothing is lost by waiting: a file that just landed is
	// found on the next attempt.
	if time.Since(c.pathsScan) > time.Second {
		c.pathsScan = time.Now()
		c.paths = c.scanPaths()
		if p, ok := c.paths[seq]; ok {
			return p, nil
		}
	}
	return "", fmt.Errorf("view: no landed file with sequence %d", seq)
}
