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

// Package expect evaluates a scenario's expectation file against a storage
// root: what the fold must hold at a checkpoint, and the properties every
// chain must have. It reads Session Flow and Session Data only, so it sits
// on the server side; the runner that builds and collects is elsewhere.
package expect

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/apache/skywalking-ai-sessionizer/internal/assemble"
	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// File is an expectation file: one block per checkpoint, "final" for the
// end of the scenario, and the properties to hold or skip.
type File struct {
	Checkpoints map[string]*Checkpoint `yaml:"checkpoints"`
	Properties  Properties             `yaml:"properties"`
	Parse       ParseOptions           `yaml:"parse"`
}

// ParseOptions are parse settings a scenario runs under.
type ParseOptions struct {
	MaxRoundBytes int64 `yaml:"max_round_bytes"`
}

// Properties are the checks every scenario gets unless it opts out. A nil
// value means on.
type Properties struct {
	Reproducible        *bool `yaml:"reproducible"`
	FoldEqualsParse     *bool `yaml:"fold_equals_parse"`
	ImmutableRounds     *bool `yaml:"immutable_rounds"`
	Bundle              *bool `yaml:"bundle"`
	RecollectIdempotent *bool `yaml:"recollect_idempotent"`
	CrossFormat         *bool `yaml:"cross_format"`
	HeaderMatchesFold   *bool `yaml:"header_matches_fold"`
}

// On reports whether a property is enabled.
func On(p *bool) bool { return p == nil || *p }

// Checkpoint is what the fold must hold at one point of the scenario. Only
// what is written is checked; a count that is not listed is not compared.
type Checkpoint struct {
	// Rounds is the head round number. Written says whether this checkpoint
	// wrote a round at all.
	Rounds  *int  `yaml:"rounds"`
	Written *bool `yaml:"written"`
	// Kinds counts nodes by kind, TalksOn talks by stream, Relations by type.
	// A stream may be named by its scenario name.
	Kinds     map[string]int `yaml:"kinds"`
	TalksOn   map[string]int `yaml:"talks_on"`
	RunsIn    map[string]int `yaml:"runs_in"`
	Relations map[string]int `yaml:"relations"`
	// Unresolved counts open and resolved references; UnresolvedKinds says
	// the state of each kind: open, resolved, or none.
	Unresolved      *Unresolved       `yaml:"unresolved"`
	UnresolvedKinds map[string]string `yaml:"unresolved_kinds"`
	// Nodes checks named nodes. A scenario name in an id stands for the
	// stream's id.
	Nodes map[string]*Node `yaml:"nodes"`
	// Session is the session node's range, as deltas from the base time.
	Session *Session `yaml:"session"`
	// View checks the asz.view document.
	View *View `yaml:"view"`
	// Delta says the round this checkpoint wrote is a delta: fewer nodes
	// than the fold, and starting past the first landed file.
	Delta *bool `yaml:"delta"`
}

// Empty reports a checkpoint with nothing to check.
func (cp *Checkpoint) Empty() bool {
	return cp.Rounds == nil && cp.Written == nil && len(cp.Kinds) == 0 && len(cp.TalksOn) == 0 && len(cp.RunsIn) == 0 &&
		len(cp.Relations) == 0 && cp.Unresolved == nil && len(cp.UnresolvedKinds) == 0 && len(cp.Nodes) == 0 &&
		cp.Session == nil && cp.View == nil && cp.Delta == nil
}

// Describe says what a fold holds, for writing an expectation file.
func Describe(root, session string) (string, error) {
	s, err := Summarize(root, session)
	if err != nil {
		return "", err
	}
	v, err := parse.View(root, session)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("round %d; kinds %v; relations %v; talks_on %v; unresolved %v", v.Round, s.Kinds, s.Relations, s.TalksOn, s.Unresolved), nil
}

// Unresolved counts references by state.
type Unresolved struct {
	Open     *int `yaml:"open"`
	Resolved *int `yaml:"resolved"`
}

// Node is what one node must look like.
type Node struct {
	Kind     string         `yaml:"kind"`
	Stream   string         `yaml:"stream"`
	Refs     *int           `yaml:"refs"`
	Revision *uint64        `yaml:"revision"`
	Attrs    map[string]any `yaml:"attrs"`
	Absent   bool           `yaml:"absent"`
}

// Session is the session node's range, as deltas from the base time.
type Session struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
}

// View is what the asz.view document must say.
type View struct {
	State     string `yaml:"state"`
	Talks     *int   `yaml:"talks"`
	Steps     *int   `yaml:"steps"`
	Files     *int   `yaml:"files"`
	Rounds    *int   `yaml:"rounds"`
	FirstTalk *Talk  `yaml:"first_talk"`
}

// Talk is what a talk in the document must say.
type Talk struct {
	Label string `yaml:"label"`
	Reply string `yaml:"reply"`
	Runs  *int   `yaml:"runs"`
}

// Load reads an expectation file. A missing file is an empty one: every
// property on, nothing else checked.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &File{}, nil
	}
	if err != nil {
		return nil, err
	}
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("expect: %s: %w", path, err)
	}
	for name, cp := range f.Checkpoints {
		if cp == nil {
			f.Checkpoints[name] = &Checkpoint{}
		}
	}
	return &f, nil
}

// Context is what an evaluation needs beside the root: which session, what
// the scenario names its streams, the base time, and what the last parse
// reported.
type Context struct {
	Session string
	Names   map[string]string // scenario name -> stream id
	At      time.Time
	Round   *parse.Round
}

func (c *Context) resolve(s string) string {
	parts := strings.Split(s, "/")
	for i, p := range parts {
		if id, ok := c.Names[p]; ok {
			parts[i] = id
		}
	}
	return strings.Join(parts, "/")
}

// Evaluate compares the fold in root with one checkpoint and returns every
// mismatch, in words.
func Evaluate(root string, cp *Checkpoint, ctx *Context) ([]string, error) {
	v, err := parse.View(root, ctx.Session)
	if err != nil {
		return nil, err
	}
	var out []string
	bad := func(format string, a ...any) { out = append(out, fmt.Sprintf(format, a...)) }
	if cp.Rounds != nil && int(v.Round) != *cp.Rounds {
		bad("rounds: head is %d, want %d", v.Round, *cp.Rounds)
	}
	if cp.Written != nil && ctx.Round != nil && ctx.Round.Changed() != *cp.Written {
		bad("written: the parse wrote a round: %v, want %v", ctx.Round.Changed(), *cp.Written)
	}
	if cp.Delta != nil && *cp.Delta && ctx.Round != nil {
		if !ctx.Round.Changed() {
			bad("delta: no round was written")
		} else if ctx.Round.Nodes >= len(v.Nodes) || ctx.Round.FromSeq <= 1 {
			bad("delta: round %d wrote %d nodes against a fold of %d, starting at seq %d; it is not a delta", ctx.Round.Number, ctx.Round.Nodes, len(v.Nodes), ctx.Round.FromSeq)
		}
	}
	kinds, talksOn := map[string]int{}, map[string]int{}
	for _, n := range v.Nodes {
		kinds[n.Kind]++
		if n.Kind == model.KindTalk {
			talksOn[n.Stream]++
		}
	}
	for kind, want := range sorted(cp.Kinds) {
		if kinds[kind] != want {
			bad("kinds: %s is %d, want %d", kind, kinds[kind], want)
		}
	}
	for stream, want := range sorted(cp.TalksOn) {
		id := ctx.resolve(stream)
		if talksOn[id] != want {
			bad("talks_on: %s has %d talks, want %d", stream, talksOn[id], want)
		}
	}
	for talk, want := range sorted(cp.RunsIn) {
		id := ctx.resolve(talk)
		runs := 0
		for _, k := range v.Children(id) {
			if k.Kind == model.KindRun {
				runs++
			}
		}
		if runs != want {
			bad("runs_in: %s has %d runs, want %d", talk, runs, want)
		}
	}
	rels := map[string]int{}
	for _, r := range v.Relations {
		rels[r.Type]++
	}
	for typ, want := range sorted(cp.Relations) {
		if rels[typ] != want {
			bad("relations: %s is %d, want %d", typ, rels[typ], want)
		}
	}
	open, resolved := 0, 0
	byKind := map[string]string{}
	for _, u := range v.Unresolved {
		if u.State == sessionflow.UnresolvedResolved {
			resolved++
			if byKind[u.Kind] == "" {
				byKind[u.Kind] = "resolved"
			}
		} else {
			open++
			byKind[u.Kind] = "open"
		}
	}
	if cp.Unresolved != nil {
		if cp.Unresolved.Open != nil && open != *cp.Unresolved.Open {
			bad("unresolved: %d open, want %d", open, *cp.Unresolved.Open)
		}
		if cp.Unresolved.Resolved != nil && resolved != *cp.Unresolved.Resolved {
			bad("unresolved: %d resolved, want %d", resolved, *cp.Unresolved.Resolved)
		}
	}
	for kind, want := range sortedS(cp.UnresolvedKinds) {
		got := byKind[kind]
		if got == "" {
			got = "none"
		}
		if got != want {
			bad("unresolved_kinds: %s is %s, want %s", kind, got, want)
		}
	}
	for id, want := range sortedN(cp.Nodes) {
		rid := ctx.resolve(id)
		n := v.Nodes[rid]
		if want.Absent {
			if n != nil {
				bad("nodes: %s is present, want absent", id)
			}
			continue
		}
		if n == nil {
			bad("nodes: %s is missing", id)
			continue
		}
		if want.Kind != "" && n.Kind != want.Kind {
			bad("nodes: %s is a %s, want %s", id, n.Kind, want.Kind)
		}
		if want.Stream != "" && n.Stream != ctx.resolve(want.Stream) {
			bad("nodes: %s is on stream %s, want %s", id, n.Stream, want.Stream)
		}
		if want.Refs != nil && len(n.Refs) != *want.Refs {
			bad("nodes: %s references %d records, want %d", id, len(n.Refs), *want.Refs)
		}
		if want.Revision != nil && n.Revision != *want.Revision {
			bad("nodes: %s is at revision %d, want %d", id, n.Revision, *want.Revision)
		}
		if len(want.Attrs) > 0 {
			var attrs map[string]any
			_ = json.Unmarshal(n.Attrs, &attrs)
			for k, wv := range want.Attrs {
				if fmt.Sprint(attrs[k]) != fmt.Sprint(wv) {
					bad("nodes: %s attrs.%s is %v, want %v", id, k, attrs[k], wv)
				}
			}
		}
	}
	if cp.Session != nil {
		sn := v.Nodes[sessionflow.NodeID("session", ctx.Session)]
		var attrs struct {
			From    string `json:"from_time"`
			Through string `json:"through_time"`
		}
		if sn != nil {
			_ = json.Unmarshal(sn.Attrs, &attrs)
		}
		if cp.Session.From != "" {
			if want, err := delta(ctx.At, cp.Session.From); err != nil {
				bad("session.from: %v", err)
			} else if got := stamp(attrs.From); !got.Equal(want) {
				bad("session.from is %s, want %s (%s)", attrs.From, want.Format(time.RFC3339Nano), cp.Session.From)
			}
		}
		if cp.Session.To != "" {
			if want, err := delta(ctx.At, cp.Session.To); err != nil {
				bad("session.to: %v", err)
			} else if got := stamp(attrs.Through); !got.Equal(want) {
				bad("session.to is %s, want %s (%s)", attrs.Through, want.Format(time.RFC3339Nano), cp.Session.To)
			}
		}
	}
	if cp.View != nil {
		lines, err := checkView(root, ctx.Session, cp.View)
		if err != nil {
			return nil, err
		}
		out = append(out, lines...)
	}
	return out, nil
}

func checkView(root, session string, want *View) ([]string, error) {
	srv := view.New(storage.NewZone(root), nil)
	c, err := srv.Load(session)
	if err != nil {
		return nil, err
	}
	doc, err := c.Build()
	if err != nil {
		return nil, err
	}
	var out []string
	bad := func(format string, a ...any) { out = append(out, fmt.Sprintf(format, a...)) }
	if want.State != "" && doc.Summary.State != want.State {
		bad("view.state is %s (%v), want %s", doc.Summary.State, doc.Summary.Problems, want.State)
	}
	if want.Talks != nil && doc.Summary.Talks != *want.Talks {
		bad("view.talks is %d, want %d", doc.Summary.Talks, *want.Talks)
	}
	if want.Steps != nil && doc.Summary.Steps != *want.Steps {
		bad("view.steps is %d, want %d", doc.Summary.Steps, *want.Steps)
	}
	if want.Files != nil && len(doc.Files) != *want.Files {
		bad("view.files is %d, want %d", len(doc.Files), *want.Files)
	}
	if want.Rounds != nil && doc.Summary.Rounds != *want.Rounds {
		bad("view.rounds is %d, want %d", doc.Summary.Rounds, *want.Rounds)
	}
	if want.FirstTalk != nil {
		if len(doc.Talks) == 0 {
			bad("view.first_talk: the document has no talks")
		} else {
			t := doc.Talks[0]
			if want.FirstTalk.Label != "" && t.Label != want.FirstTalk.Label {
				bad("view.first_talk.label is %q, want %q", t.Label, want.FirstTalk.Label)
			}
			if want.FirstTalk.Reply != "" && t.Reply != want.FirstTalk.Reply {
				bad("view.first_talk.reply is %q, want %q", t.Reply, want.FirstTalk.Reply)
			}
			if want.FirstTalk.Runs != nil && t.Runs != *want.FirstTalk.Runs {
				bad("view.first_talk.runs is %d, want %d", t.Runs, *want.FirstTalk.Runs)
			}
		}
	}
	return out, nil
}

// Summary is what one format's fold looks like, for comparing formats.
type Summary struct {
	Kinds      map[string]int
	Relations  map[string]int
	TalksOn    map[string]int
	Unresolved map[string]int // kind:state -> count
	Streams    []string
}

// Summarize reads a fold into a Summary.
func Summarize(root, session string) (*Summary, error) {
	v, err := parse.View(root, session)
	if err != nil {
		return nil, err
	}
	s := &Summary{Kinds: map[string]int{}, Relations: map[string]int{}, TalksOn: map[string]int{}, Unresolved: map[string]int{}}
	for _, n := range v.Nodes {
		s.Kinds[n.Kind]++
		if n.Kind == model.KindTalk {
			s.TalksOn[n.Stream]++
		}
		if n.Kind == model.KindStream {
			s.Streams = append(s.Streams, n.Stream)
		}
	}
	for _, r := range v.Relations {
		s.Relations[r.Type]++
	}
	for _, u := range v.Unresolved {
		s.Unresolved[u.Kind+":"+u.State]++
	}
	sort.Strings(s.Streams)
	return s, nil
}

// Compare says how two summaries differ.
func Compare(a, b *Summary) []string {
	var out []string
	diff := func(what string, x, y map[string]int) {
		keys := map[string]bool{}
		for k := range x {
			keys[k] = true
		}
		for k := range y {
			keys[k] = true
		}
		var names []string
		for k := range keys {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			if x[k] != y[k] {
				out = append(out, fmt.Sprintf("%s %s: %d against %d", what, k, x[k], y[k]))
			}
		}
	}
	diff("kinds", a.Kinds, b.Kinds)
	diff("relations", a.Relations, b.Relations)
	diff("talks on", a.TalksOn, b.TalksOn)
	diff("unresolved", a.Unresolved, b.Unresolved)
	if strings.Join(a.Streams, ",") != strings.Join(b.Streams, ",") {
		out = append(out, fmt.Sprintf("streams %v against %v", a.Streams, b.Streams))
	}
	return out
}

// Properties that read the root alone.

// FoldEqualsParse checks that folding every round equals one full parse of
// the same evidence.
func FoldEqualsParse(root, session, conversation string) ([]string, error) {
	z := storage.NewZone(root)
	v, err := parse.View(root, conversation)
	if err != nil {
		return nil, err
	}
	ix, ok, err := index.Load(z.IndexDir(session), session)
	if err != nil {
		return nil, err
	}
	if !ok {
		return []string{"fold_equals_parse: no index to parse from"}, nil
	}
	full, err := assemble.Session(ix, assemble.Options{Conversation: conversation, Session: session, ThroughSeq: v.ThroughSeq})
	if err != nil {
		return nil, err
	}
	var out []string
	if len(full.Nodes) != len(v.Nodes) {
		out = append(out, fmt.Sprintf("fold_equals_parse: a full parse has %d nodes, the fold of %d rounds has %d", len(full.Nodes), v.Round, len(v.Nodes)))
	}
	for _, n := range full.Nodes {
		f, present := v.Nodes[n.ID]
		if !present {
			out = append(out, "fold_equals_parse: "+n.ID+" is in a full parse but not in the fold")
			continue
		}
		if f.Kind != n.Kind || f.Parent != n.Parent || string(f.Attrs) != string(n.Attrs) {
			out = append(out, fmt.Sprintf("fold_equals_parse: %s differs: parse %s parent=%s attrs=%s; fold %s parent=%s attrs=%s",
				n.ID, n.Kind, n.Parent, n.Attrs, f.Kind, f.Parent, f.Attrs))
		}
	}
	if len(full.Relations) != len(v.Relations) {
		out = append(out, fmt.Sprintf("fold_equals_parse: relations: a full parse has %d, the fold has %d", len(full.Relations), len(v.Relations)))
	}
	return out, nil
}

// ImmutableRounds checks every round verifies, links to the one before, and
// is not writable.
func ImmutableRounds(root, conversation string) ([]string, error) {
	chain := sessionflow.OpenChain(root, conversation)
	files, err := chain.Verify()
	if err != nil {
		return []string{"immutable_rounds: " + err.Error()}, nil
	}
	var out []string
	for _, f := range files {
		fi, err := os.Stat(f.Path)
		if err != nil {
			return nil, err
		}
		if fi.Mode().Perm()&0o222 != 0 {
			out = append(out, fmt.Sprintf("immutable_rounds: round %d is writable: %v", f.Round, fi.Mode()))
		}
	}
	return out, nil
}

// Bundle checks that landed files and rounds are self-sufficient: with the
// index and every state file gone, the fold reads the same and a parse
// writes nothing new.
func Bundle(root, session, conversation string) ([]string, error) {
	z := storage.NewZone(root)
	before, err := parse.View(root, conversation)
	if err != nil {
		return nil, err
	}
	for _, gone := range []string{z.IndexDir(session), z.IndexStatePath(session), z.SessionStatePath(session),
		filepath.Join(root, "_conversations", conversation, "conversation.state")} {
		if err := os.RemoveAll(gone); err != nil {
			return nil, err
		}
	}
	after, err := parse.View(root, conversation)
	if err != nil {
		return []string{"bundle: the root could not be read without its index and state: " + err.Error()}, nil
	}
	var out []string
	if after.Round != before.Round || after.Digest != before.Digest || len(after.Nodes) != len(before.Nodes) {
		out = append(out, fmt.Sprintf("bundle: the fold changed without index and state: round %d/%s became %d/%s", before.Round, before.Digest[:8], after.Round, after.Digest[:8]))
	}
	again, err := parse.Session(z, parse.Options{Conversation: conversation, Session: session, Reindex: index.Rebuild})
	if err != nil {
		return nil, err
	}
	if again.Changed() {
		out = append(out, fmt.Sprintf("bundle: re-deriving from the landed files wrote round %d with %d nodes", again.Number, again.Nodes))
	}
	return out, nil
}

// HeaderMatchesFold checks the head round's header says what the fold
// holds: the session's title and range, and the counts a list shows.
func HeaderMatchesFold(root, conversation string) ([]string, error) {
	v, err := parse.View(root, conversation)
	if err != nil {
		return nil, err
	}
	chain := sessionflow.OpenChain(root, conversation)
	files, err := chain.List()
	if err != nil || len(files) == 0 {
		return nil, err
	}
	head, err := chain.Open(files[len(files)-1].Path)
	if err != nil {
		return nil, err
	}
	h := head.Header
	var out []string
	sn := v.Nodes[sessionflow.NodeID("session", v.Session)]
	var attrs struct {
		From    string `json:"from_time"`
		Through string `json:"through_time"`
		Title   string `json:"title"`
	}
	if sn != nil {
		_ = json.Unmarshal(sn.Attrs, &attrs)
	}
	if h.SessionFromTime != attrs.From || h.SessionThroughTime != attrs.Through || h.Title != attrs.Title {
		out = append(out, fmt.Sprintf("header_matches_fold: header says %q %s..%s, the session node says %q %s..%s",
			h.Title, h.SessionFromTime, h.SessionThroughTime, attrs.Title, attrs.From, attrs.Through))
	}
	steps := 0
	for _, n := range v.Nodes {
		switch n.Kind {
		case model.KindConversation, model.KindSegment, model.KindSession, model.KindStream, model.KindEpoch, model.KindTalk, model.KindRun:
		default:
			steps++
		}
	}
	if h.Talks != len(v.NodesByKind(model.KindTalk)) || h.Streams != len(v.NodesByKind(model.KindStream)) ||
		h.Segments != len(v.NodesByKind(model.KindSegment)) || h.Unresolved != len(v.OpenUnresolved()) || h.Steps != steps {
		out = append(out, fmt.Sprintf("header_matches_fold: header counts talks=%d steps=%d streams=%d segments=%d unresolved=%d; fold has %d %d %d %d %d",
			h.Talks, h.Steps, h.Streams, h.Segments, h.Unresolved,
			len(v.NodesByKind(model.KindTalk)), steps, len(v.NodesByKind(model.KindStream)), len(v.NodesByKind(model.KindSegment)), len(v.OpenUnresolved())))
	}
	return out, nil
}

func delta(base time.Time, s string) (time.Time, error) {
	d, err := time.ParseDuration(strings.TrimPrefix(s, "+"))
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not a delta such as +1.5s", s)
	}
	return base.Add(d), nil
}

func stamp(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func sorted(m map[string]int) map[string]int        { return m }
func sortedS(m map[string]string) map[string]string { return m }
func sortedN(m map[string]*Node) map[string]*Node   { return m }
