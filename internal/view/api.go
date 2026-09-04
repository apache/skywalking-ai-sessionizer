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
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// Handler serves the page and the four questions a reader asks: which
// conversation, what happened in it, what happened in this turn, and what
// exactly did this step say.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.index)
	mux.HandleFunc("/c/", s.page)
	mux.Handle("/favicon.svg", svg(faviconSVG))
	mux.Handle("/logo.svg", svg(logoSVG))
	mux.HandleFunc("/api/status", s.apiStatus)
	mux.HandleFunc("/api/conversations", s.apiList)
	mux.HandleFunc("/api/glossary", s.apiGlossary)
	mux.HandleFunc("/api/c/", s.apiConversation)
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func fail(w http.ResponseWriter, err error, code int) {
	writeJSONStatus(w, code, map[string]string{"error": err.Error()})
}

func writeJSONStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// summary is one row in the conversation list.
type summary struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Talks    int    `json:"talks"`
	Steps    int    `json:"steps"`
	Stream   int    `json:"streams"`
	Segments int    `json:"segments"`
	Rounds   uint64 `json:"rounds"`
	From     int64  `json:"from"`
	To       int64  `json:"to"`
	Open     int    `json:"unresolved"`
}

func (s *Server) apiList(w http.ResponseWriter, _ *http.Request) {
	ids, err := s.List()
	if err != nil {
		fail(w, err, http.StatusInternalServerError)
		return
	}
	out := make([]summary, 0, len(ids))
	for _, id := range ids {
		c, err := s.Load(id)
		if err != nil {
			continue
		}
		row := summary{ID: id, Rounds: c.View.Round, Open: len(c.View.OpenUnresolved())}
		for _, n := range c.View.Nodes {
			switch n.Kind {
			case model.KindTalk:
				row.Talks++
			case model.KindStream:
				row.Stream++
			case model.KindSegment:
				row.Segments++
			case model.KindSession:
				row.Title = attrString(n, "title")
			}
			if isStep(n.Kind) {
				row.Steps++
			}
		}
		for _, t := range c.Talks() {
			lo, hi := c.Span(t)
			if lo != 0 && (row.From == 0 || lo < row.From) {
				row.From = lo
			}
			if hi > row.To {
				row.To = hi
			}
		}
		row.From, row.To = Millis(row.From), Millis(row.To)
		// The session node carries when the session began and its last
		// activity, the same pair the document and the wire carry; the
		// talks' span is the fallback for a chain from before that field.
		if sn := c.View.Nodes[sessionflow.NodeID("session", c.Session)]; sn != nil {
			if from, to := millisOf(attrString(sn, "from_time")), millisOf(attrString(sn, "through_time")); from != 0 && to != 0 {
				row.From, row.To = from, to
			}
		}
		if row.Title == "" {
			row.Title = "(untitled)"
		}
		out = append(out, row)
	}
	writeJSON(w, out)
}

// apiGlossary says what each name means, and what the runtime calls it.
//
// Two sources, kept apart on purpose. A field that names something the model
// names is described by the adapter's glossary, which also carries the
// runtime's word for it. The rest are the landed envelope's own fields, and
// the format describes those itself.
func (s *Server) apiGlossary(w http.ResponseWriter, _ *http.Request) {
	g := s.glossary
	if g == nil {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "no glossary was supplied for this storage root"})
		return
	}
	terms := map[string]map[string]string{}
	for _, t := range g.Terms() {
		terms[t.Unified] = map[string]string{
			"native": t.Native, "where": t.Where, "note": t.Note,
		}
	}
	writeJSON(w, map[string]any{
		"dialect": g.Dialect,
		"terms":   terms,
		"fields":  sessiondata.Fields(),
	})
}

var flowPath = regexp.MustCompile(`^/api/c/([^/]+)/flow$`)
var viewPath = regexp.MustCompile(`^/api/c/([^/]+)/view$`)
var recordPath = regexp.MustCompile(`^/api/c/([^/]+)/record/(\d+)/(\d+)$`)
var talkPath = regexp.MustCompile(`^/api/c/([^/]+)/talk/(.+)$`)

func (s *Server) apiConversation(w http.ResponseWriter, r *http.Request) {
	if m := recordPath.FindStringSubmatch(r.URL.Path); m != nil {
		seq, _ := strconv.ParseUint(m[2], 10, 64)
		row, _ := strconv.ParseUint(m[3], 10, 64)
		s.apiRecord(w, m[1], seq, row)
		return
	}
	if m := flowPath.FindStringSubmatch(r.URL.Path); m != nil {
		s.apiFlow(w, m[1])
		return
	}
	if m := viewPath.FindStringSubmatch(r.URL.Path); m != nil {
		s.apiView(w, m[1])
		return
	}
	if m := talkPath.FindStringSubmatch(r.URL.Path); m != nil {
		s.apiTalk(w, m[1], m[2])
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/c/")
	if id == "" || strings.Contains(id, "/") {
		fail(w, fmt.Errorf("not found"), http.StatusNotFound)
		return
	}
	s.apiOverview(w, id)
}

// talkRow is one talk in the list down the side.
type talkRow struct {
	ID      string `json:"id"`
	Stream  string `json:"stream"`
	Label   string `json:"label"`
	Runs    int    `json:"runs"`
	Steps   int    `json:"steps"`
	Tools   int    `json:"tools"`
	From    int64  `json:"from"`
	To      int64  `json:"to"`
	Child   bool   `json:"child"`
	Segment string `json:"segment,omitempty"`

	Reply string `json:"reply,omitempty"`

	// labelAt and replyAt are where those are read from. They never leave
	// the server. labelAt holds candidates in preference order, because
	// which one can name the talk is only known once its text is read.
	labelAt []*sessionflow.Ref
	replyAt *sessionflow.Ref
}

// overview is what the page's conversation summary and the Conversation
// View share: computed once per fold, from the fold alone plus the text of
// the records that name and answer each talk.
type overview struct {
	title                string
	kinds, rels, quality map[string]int
	talks                []talkRow
	streams              []sessionview.Stream
	segments             []sessionview.Segment
	open                 []map[string]string
}

func (s *Server) apiOverview(w http.ResponseWriter, id string) {
	c, err := s.Load(id)
	if err != nil {
		fail(w, err, http.StatusNotFound)
		return
	}
	o := c.overview()
	writeJSON(w, map[string]any{
		"id": id, "title": o.title, "session": c.Session,
		"rounds": c.View.Round, "digest": c.View.Digest,
		"parser": c.View.Parser, "policy": c.View.Policy,
		"through_seq": c.View.ThroughSeq,
		"nodes":       len(c.View.Nodes), "relations": len(c.View.Relations),
		"kinds": o.kinds, "relation_types": o.rels, "quality": o.quality,
		"talks": o.talks, "unresolved": o.open,
		"streams":  o.streams,
		"segments": o.segments,
	})
}

func (c *Conversation) overview() *overview {
	var title string
	kinds := map[string]int{}
	for _, n := range c.View.Nodes {
		kinds[n.Kind]++
		if n.Kind == model.KindSession {
			title = attrString(n, "title")
		}
	}
	quality := map[string]int{}
	rels := map[string]int{}
	for _, r := range c.View.Relations {
		rels[r.Type]++
		quality[r.Quality]++
	}

	childStream := map[string]bool{}
	for _, st := range c.Streams() {
		if !strings.Contains(string(st.Attrs), `"role":"main"`) {
			childStream[st.Stream] = true
		}
	}

	// Which segment a talk sits in. The assembler relates the talk to the
	// segment, so this is read, never recomputed from time.
	inSegment := map[string]string{}
	for _, r := range c.View.Relations {
		if r.Type == model.RelInSegment {
			inSegment[r.From] = r.To
		}
	}

	talks := make([]talkRow, 0, 64)
	var labelRefs []*sessionflow.Ref
	for _, t := range c.Talks() {
		lo, hi := c.Span(t)
		row := talkRow{ID: t.ID, Stream: t.Stream, Child: childStream[t.Stream],
			From: Millis(lo), To: Millis(hi), Segment: inSegment[t.ID]}
		var walk func(string)
		walk = func(nid string) {
			for _, k := range c.View.Children(nid) {
				if k.Kind == model.KindRun {
					row.Runs++
				}
				if isStep(k.Kind) {
					row.Steps++
				}
				if k.Kind == model.KindTool || k.Kind == model.KindAgentCall {
					row.Tools++
				}
				walk(k.ID)
			}
		}
		walk(t.ID)
		if refs := c.labelRefs(t); len(refs) > 0 {
			row.labelAt = refs
			labelRefs = append(labelRefs, refs...)
		}
		if ref := c.replyRef(t); ref != nil {
			row.replyAt = ref
			labelRefs = append(labelRefs, ref)
		}
		talks = append(talks, row)
	}
	// One pass per landed file, not one per talk.
	texts := c.texts(labelRefs)
	for i := range talks {
		for _, r := range talks[i].labelAt {
			text := texts[[2]uint64{r.Seq, r.Row}]
			// A tools-list delta says which tools appeared, not what the
			// work is, so it cannot name a talk.
			if strings.HasPrefix(strings.TrimSpace(text), `{"type":"deferred_tools_delta"`) {
				continue
			}
			if text != "" {
				talks[i].Label = clip(text)
				break
			}
		}
		if r := talks[i].replyAt; r != nil {
			talks[i].Reply = clip(texts[[2]uint64{r.Seq, r.Row}])
		}
	}

	open := []map[string]string{}
	for _, u := range c.View.OpenUnresolved() {
		open = append(open, map[string]string{"kind": u.Kind, "ref": u.RefID, "reason": u.Reason})
	}

	return &overview{
		title: title, kinds: kinds, rels: rels, quality: quality,
		talks: talks, streams: streamRows(c, talks), segments: segmentRows(c, talks), open: open,
	}
}

// segmentRows lists the conversation's segments, each with the span of the
// talks the assembler placed in it.
//
// A segment's own time is not recomputed here. Its bounds are the bounds of its
// talks, which is what "in_segment" already decided.
func segmentRows(c *Conversation, talks []talkRow) []sessionview.Segment {
	span := map[string][2]int64{}
	count := map[string]int{}
	for _, t := range talks {
		if t.Segment == "" {
			continue
		}
		count[t.Segment]++
		s := span[t.Segment]
		if t.From != 0 && (s[0] == 0 || t.From < s[0]) {
			s[0] = t.From
		}
		if t.To > s[1] {
			s[1] = t.To
		}
		span[t.Segment] = s
	}
	var segs []*sessionflow.Node
	for _, n := range c.View.Nodes {
		if n.Kind == model.KindSegment {
			segs = append(segs, n)
		}
	}
	out := []sessionview.Segment{}
	for _, n := range sessionflow.InOrder(segs) {
		s := span[n.ID]
		out = append(out, sessionview.Segment{
			ID: n.ID, State: attrString(n, "state"),
			Talks: count[n.ID], From: s[0], To: s[1],
			Committable: attrBool(n, "committable"),
		})
	}
	return out
}

// streamRows lists the execution streams, each with the parent that started it
// and the talk that opens it.
//
// The parent is read from the "starts" relation, never from the stream name. A
// call that could have started several streams reports each of them, because
// the assembler does not choose one and neither may this.
func streamRows(c *Conversation, talks []talkRow) []sessionview.Stream {
	firstTalk := map[string]string{}
	steps := map[string]int{}
	for i := range talks {
		if _, seen := firstTalk[talks[i].Stream]; !seen {
			firstTalk[talks[i].Stream] = talks[i].ID
		}
		steps[talks[i].Stream] += talks[i].Steps
	}
	// Which step started a stream, and how well that is known. A call the
	// assembler could not tie to one stream leaves several candidates, and
	// every one is reported rather than one being chosen here.
	type origin struct {
		Step, Stream, Quality, Talk string
	}
	parent := map[string]string{}
	from := map[string][]origin{}
	for _, r := range c.View.Relations {
		if r.Type != model.RelStarts {
			continue
		}
		if n := c.View.Nodes[r.From]; n != nil && n.Stream != "" {
			parent[r.To] = n.Stream
			from[r.To] = append(from[r.To], origin{r.From, n.Stream, r.Quality, c.talkOf(r.From)})
		}
	}
	names := c.journalNames()
	out := []sessionview.Stream{}
	for _, st := range c.Streams() {
		label, namedBy := attrString(st, "label"), ""
		if label == "" {
			if n, ok := names[st.Stream]; ok {
				label, namedBy = n, "journal"
			}
		}
		opened := []sessionview.Origin{}
		for _, o := range from[st.ID] {
			opened = append(opened, sessionview.Origin{Step: o.Step, Stream: o.Stream, Quality: o.Quality, Talk: o.Talk})
		}
		out = append(out, sessionview.Stream{
			ID: st.ID, Name: st.Stream,
			Role:     attrString(st, "role"),
			Label:    label,
			NamedBy:  namedBy,
			Records:  int(attrNumber(st, "records")),
			Parent:   parent[st.ID],
			Talk:     firstTalk[st.Stream],
			Steps:    steps[st.Stream],
			OpenedBy: opened,
		})
	}
	return out
}

func attrBool(n *sessionflow.Node, key string) bool {
	if len(n.Attrs) == 0 {
		return false
	}
	var m map[string]any
	if json.Unmarshal(n.Attrs, &m) != nil {
		return false
	}
	b, _ := m[key].(bool)
	return b
}

// labelRefs finds where a talk's name should be read from: the first
// external input under it. A talk with no external input at all - work
// delegated to a pool agent arrives outside its own records - is named by
// what was first put into its context instead, so the first few injections
// are offered as candidates and the caller, which reads the texts, picks
// the first one that can serve as a name.
//
// This returns positions rather than texts because resolving one position
// means opening a landed file and reading forward to a row. Done per talk,
// that is one file scan per talk - measured at six seconds for a
// conversation with 922 of them. The positions are collected first and
// read together instead.
func (c *Conversation) labelRefs(t *sessionflow.Node) []*sessionflow.Ref {
	var ext *sessionflow.Ref
	var inj []*sessionflow.Ref
	var walk func(string) bool
	walk = func(nid string) bool {
		for _, k := range c.View.Children(nid) {
			if k.Kind == model.KindMessageExternal && k.Ref != nil {
				ext = k.Ref
				return true
			}
			if k.Kind == model.KindContextInjection && k.Ref != nil && len(inj) < 3 {
				inj = append(inj, k.Ref)
			}
			if walk(k.ID) {
				return true
			}
		}
		return false
	}
	walk(t.ID)
	if ext != nil {
		return []*sessionflow.Ref{ext}
	}
	return inj
}

// replyRef finds where a talk's answer should be read from: the last thing the
// agent said in it.
//
// Not the first, and not the only. The agent speaks between tool calls, so a
// talk usually holds several assistant messages; the last one is the answer
// and the earlier ones are narration on the way to it.
func (c *Conversation) replyRef(t *sessionflow.Node) *sessionflow.Ref {
	var found *sessionflow.Ref
	var walk func(string)
	walk = func(nid string) {
		for _, k := range c.View.Children(nid) {
			if (k.Kind == model.KindMessageAssistant || k.Kind == model.KindAgentOutput) && k.Ref != nil {
				found = k.Ref
			}
			walk(k.ID)
		}
	}
	walk(t.ID)
	return found
}

// journalNames names child streams from the run journals that started
// them, keyed by stream name.
//
// A pool agent's own records never say what it was for: its opening context
// is the same tool roster and skill catalogue every agent gets, and the
// prompt reaches it outside its transcript. Measured over 178 children of
// one conversation, 177 open with byte-identical injections. The workflow's
// journal is the one place that tells them apart: each result row carries
// the agent id and, in what the agent returned, the surface it covered. That
// is read here, at render time, because a journal row is payload and the
// assembler reads only the index.
//
// The name is what the result says - a surface, else a summary, else the
// verdict and the first claim it was about. An agent with no result row has
// no name from here and keeps whatever it had.
func (c *Conversation) journalNames() map[string]string {
	names := map[string]string{}
	files, err := storage.LandedFiles(c.zone, c.Session)
	if err != nil {
		return names
	}
	for _, lf := range files {
		if lf.RunID == "" {
			continue
		}
		f, err := os.Open(lf.Path)
		if err != nil {
			continue
		}
		rd, err := sessiondata.NewReader(f)
		if err != nil {
			f.Close()
			continue
		}
		for {
			rec, rerr := rd.Next()
			if rerr != nil {
				break
			}
			if rec.Child == "" {
				continue
			}
			if _, seen := names[rec.Child]; seen {
				continue
			}
			for _, raw := range candidates(rec) {
				var row struct {
					Type   string `json:"type"`
					Result struct {
						Surface string `json:"surface"`
						Summary string `json:"summary"`
						Verdict string `json:"verdict"`
						Refuted []struct {
							Claim string `json:"claim"`
						} `json:"refuted_claims"`
					} `json:"result"`
				}
				if json.Unmarshal(raw, &row) != nil || row.Type != "result" {
					continue
				}
				r := row.Result
				name := r.Surface
				if name == "" {
					name = r.Summary
				}
				if name == "" && r.Verdict != "" {
					name = r.Verdict
					if len(r.Refuted) > 0 && r.Refuted[0].Claim != "" {
						name += " · " + r.Refuted[0].Claim
					}
				}
				if name = shortName(name); name != "" {
					names[rec.Child] = name
				}
				break
			}
		}
		f.Close()
	}
	return names
}

// shortName cuts a name to what a label can hold, on a rune boundary, and
// collapses the line breaks a result text carries.
func shortName(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const limit = 160
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// texts reads many landed positions with one pass per file.
//
// A landed file is a sequence of records with no row offsets, so a row is
// reached by reading forward. Reading every wanted row of one file in a single
// pass turns a scan per position into a scan per file.
func (c *Conversation) texts(refs []*sessionflow.Ref) map[[2]uint64]string {
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
	out := map[[2]uint64]string{}
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
				out[[2]uint64{seq, i}] = readable(rec)
				left--
			}
		}
		f.Close()
	}
	return out
}

// flowStep is one step as the timeline needs it: where it sits and when, and
// nothing it does not draw.
//
// The timeline shows the whole conversation, so it needs every step at once.
// Sending each talk's full tree would be tens of megabytes of text the axis
// never renders; this is the same steps without their content.
type flowStep struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Stream string `json:"stream"`
	Talk   string `json:"talk"`
	Parent string `json:"parent,omitempty"`
	At     int64  `json:"at"`
	Name   string `json:"name,omitempty"`
	Depth  int    `json:"depth"`
}

func (s *Server) apiFlow(w http.ResponseWriter, id string) {
	c, err := s.Load(id)
	if err != nil {
		fail(w, err, http.StatusNotFound)
		return
	}
	out := make([]flowStep, 0, 4096)
	for _, t := range c.Talks() {
		var walk func(*sessionflow.Node, int)
		walk = func(n *sessionflow.Node, depth int) {
			for _, k := range c.View.Children(n.ID) {
				if isStep(k.Kind) {
					out = append(out, flowStep{
						ID: k.ID, Kind: k.Kind, Stream: k.Stream, Talk: t.ID,
						Parent: k.Parent, At: Millis(c.Time(k)),
						Name: attrString(k, "name"), Depth: depth,
					})
				}
				walk(k, depth+1)
			}
		}
		walk(t, 1)
	}
	writeJSON(w, out)
}

// talkOf walks up from a node to the talk that contains it.
//
// A reader sent to a step needs the talk holding it, because a talk is what
// the page loads. Without it, following a child stream back to the step that
// opened it lands on a stream whose talk was never fetched, and the page shows
// nothing at all.
func (c *Conversation) talkOf(id string) string {
	for i := 0; i < 24 && id != ""; i++ {
		n := c.View.Nodes[id]
		if n == nil {
			return ""
		}
		if n.Kind == model.KindTalk {
			return n.ID
		}
		id = n.Parent
	}
	return ""
}

func isStep(kind string) bool {
	switch kind {
	case model.KindSession, model.KindSegment, model.KindStream,
		model.KindEpoch, model.KindTalk, model.KindRun:
		return false
	}
	return true
}

func attrString(n *sessionflow.Node, key string) string {
	if len(n.Attrs) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(n.Attrs, &m) != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func attrNumber(n *sessionflow.Node, key string) float64 {
	if len(n.Attrs) == 0 {
		return 0
	}
	var m map[string]any
	if json.Unmarshal(n.Attrs, &m) != nil {
		return 0
	}
	f, _ := m[key].(float64)
	return f
}
