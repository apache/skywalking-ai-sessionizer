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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/apache/skywalking-ai-sessionizer/internal/assemble"
	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/verify"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// File is an expectation file: one block per checkpoint, "final" for the
// end of the scenario, and the properties to hold or skip.
type File struct {
	Checkpoints map[string]*Checkpoint `yaml:"checkpoints"`
	Properties  Properties             `yaml:"properties"`
	Parse       ParseOptions           `yaml:"parse"`
	Collect     CollectOptions         `yaml:"collect"`
	// Push is what the OTLP push of the finished session must carry.
	Push *Push `yaml:"push"`
}

// Push is what a scenario's push must carry beyond what every push must:
// the file kinds that must appear on the wire.
type Push struct {
	Kinds []string `yaml:"kinds"`
}

// CollectOptions are collector settings a scenario runs under. The build
// writes them into its configuration, and the collection follows it.
type CollectOptions struct {
	MaxDeltaBytes int64 `yaml:"max_delta_bytes"`
}

// ParseOptions are parse settings a scenario runs under.
type ParseOptions struct {
	MaxRoundBytes int64 `yaml:"max_round_bytes"`
}

// Properties are the checks every scenario gets unless it opts out. A nil
// value means on.
type Properties struct {
	Reproducible          *bool `yaml:"reproducible"`
	FoldEqualsParse       *bool `yaml:"fold_equals_parse"`
	ImmutableRounds       *bool `yaml:"immutable_rounds"`
	Bundle                *bool `yaml:"bundle"`
	RecollectIdempotent   *bool `yaml:"recollect_idempotent"`
	CrossFormat           *bool `yaml:"cross_format"`
	HeaderMatchesFold     *bool `yaml:"header_matches_fold"`
	RecordsMatch          *bool `yaml:"records_match"`
	RecordsWellFormed     *bool `yaml:"records_well_formed"`
	EveryLineARecord      *bool `yaml:"every_line_a_record"`
	PartsKeepSourceBytes  *bool `yaml:"parts_keep_source_bytes"`
	DiscoveryIgnoresNoise *bool `yaml:"discovery_ignores_noise"`
	RepackKeepsStructure  *bool `yaml:"repack_keeps_structure"`
	PushFollowsTheWire    *bool `yaml:"push_follows_the_wire"`
	ViewCoversTheSession  *bool `yaml:"view_covers_the_session"`
	// ChangesLeaveTheFold says the same scenario without its changes folds
	// to the same nodes and relations: a change record is evidence beside a
	// step, never a step. Checked when a scenario has changes.
	ChangesLeaveTheFold *bool `yaml:"changes_leave_the_fold"`
	// ProviderBodiesLeaveTheFold says the same scenario without its provider
	// bodies folds to the same nodes and relations. Checked when a scenario
	// writes provider bodies.
	ProviderBodiesLeaveTheFold *bool `yaml:"provider_bodies_leave_the_fold"`
	// ProviderBodiesRebuild says every body a call lists rebuilds from the
	// session's provider files up to the one it is in, as a reader loading
	// bodies on demand reads them. Checked when a scenario writes provider
	// bodies, and again on the root repack_keeps_structure re-cuts.
	ProviderBodiesRebuild *bool `yaml:"provider_bodies_rebuild"`
	MetricsMatchThePlan   *bool `yaml:"metrics_match_the_plan"`
	// RemovedAfterSent says what a pipeline's removal does with the session,
	// on copies of the finished root. Nothing goes before all of it is sent
	// to the one receiver the pipeline sends to. All of it goes once it is,
	// by its marker's policy, and nothing is made or sent again afterwards.
	// A session with no marker stays.
	RemovedAfterSent *bool `yaml:"removed_after_sent"`
	// PrunedSourcesGone says what collection does when Claude Code prunes
	// the session's files, on a copy of the finished root: the cursor of
	// every file that went moves from active to source_gone, nothing lands,
	// the landed files and the chain stay as they were, and asz verify counts
	// as many problems as before. When the files come back, their cursors
	// return to their original states. Conflicts stay unchanged throughout.
	// Runtime formats only.
	PrunedSourcesGone *bool `yaml:"pruned_sources_gone"`
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
	// Lose names landed files to delete after this checkpoint's parse and
	// before its checks: what a person deletes from a storage root after a
	// round has bound to it. The checks that follow, here and at every
	// later checkpoint, run over the damaged root.
	Lose []Lose `yaml:"lose"`
	// Verify is what asz verify must report.
	Verify *Verify `yaml:"verify"`
}

// Lose names one landed file by what it holds rather than by its sequence,
// since the two formats land the same files in a different order: the
// stream or run it belongs to, its kind, and which one of that kind on that
// stream, counting from one.
type Lose struct {
	Stream string `yaml:"stream"`
	Run    string `yaml:"run"`
	Kind   string `yaml:"kind"`
	Nth    int    `yaml:"nth"`
}

// Verify is what asz verify must report over the root: how many problems,
// counting the streams and the chain together.
type Verify struct {
	Problems *int `yaml:"problems"`
}

// landedPrefix is the file name prefix a landed file of each kind carries.
var landedPrefix = map[string]string{
	"transcript": "transcript", "agent_meta": "meta", "journal": "journal",
	"workflow_manifest": "manifest", "workflow_script": "script", "changes": "changes",
	"provider_body": "provider_body",
}

// Resolve finds the landed file a Lose names among a session's files. The
// stream is the resolved stream id; the caller turns a scenario name into
// one.
func (l Lose) Resolve(files []storage.LandedFile, stream string) (*storage.LandedFile, error) {
	nth := l.Nth
	if nth == 0 {
		nth = 1
	}
	prefix, known := landedPrefix[l.Kind]
	if l.Kind != "" && !known {
		return nil, fmt.Errorf("lose: unknown kind %q; the kinds are transcript, agent_meta, journal, workflow_manifest, workflow_script, changes, provider_body", l.Kind)
	}
	seen := 0
	for i := range files {
		f := &files[i]
		if l.Run != "" && f.RunID != l.Run {
			continue
		}
		if l.Run == "" && f.Stream != stream {
			continue
		}
		if prefix != "" && !strings.HasPrefix(filepath.Base(f.Path), prefix+"-") {
			continue
		}
		seen++
		if seen == nth {
			return f, nil
		}
	}
	return nil, fmt.Errorf("lose: no landed file %d of kind %q on stream %q run %q; the session has %d files", nth, l.Kind, l.Stream, l.Run, len(files))
}

// Empty reports a checkpoint with nothing to check.
func (cp *Checkpoint) Empty() bool {
	return cp.Rounds == nil && cp.Written == nil && len(cp.Kinds) == 0 && len(cp.TalksOn) == 0 && len(cp.RunsIn) == 0 &&
		len(cp.Relations) == 0 && cp.Unresolved == nil && len(cp.UnresolvedKinds) == 0 && len(cp.Nodes) == 0 && cp.Verify == nil &&
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
	State string `yaml:"state"`
	// Problems is how many problems the summary lists.
	Problems  *int  `yaml:"problems"`
	Talks     *int  `yaml:"talks"`
	Steps     *int  `yaml:"steps"`
	Files     *int  `yaml:"files"`
	Rounds    *int  `yaml:"rounds"`
	FirstTalk *Talk `yaml:"first_talk"`
	// Changes counts the workspace change records, ChangedFiles the files
	// across them, CapturedBy the records by who captured them, and
	// ChangesJoined the records that found their step.
	Changes       *int           `yaml:"changes"`
	ChangedFiles  *int           `yaml:"changed_files"`
	CapturedBy    map[string]int `yaml:"captured_by"`
	ChangesJoined *int           `yaml:"changes_joined"`
	// ProviderBodies counts the landed provider bodies, and CapturedPrompts
	// the calls that list a request.
	// ProviderFiles counts the session's provider_body files.
	ProviderFiles   *int `yaml:"provider_files"`
	ProviderBodies  *int `yaml:"provider_bodies"`
	CapturedPrompts *int `yaml:"captured_prompts"`
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

// Stream turns a scenario's stream name into the stream's id; a name that
// is not a scenario name is returned as it is.
func (c *Context) Stream(name string) string { return c.resolve(name) }

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
	if cp.Verify != nil {
		z := storage.NewZone(root)
		streams, err := verify.Session(z, ctx.Session)
		if err != nil {
			return nil, err
		}
		chain, err := verify.Chain(z, ctx.Session, nil)
		if err != nil {
			return nil, err
		}
		got := streams.Problems + chain.Problems
		if cp.Verify.Problems != nil && got != *cp.Verify.Problems {
			bad("verify: %d problem(s) (%v %v), want %d", got, streams.Details(), chain.Details(), *cp.Verify.Problems)
		}
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
	if want.Problems != nil && len(doc.Summary.Problems) != *want.Problems {
		bad("view.problems is %d (%v), want %d", len(doc.Summary.Problems), doc.Summary.Problems, *want.Problems)
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
	if want.Changes != nil && len(doc.WorkspaceChanges) != *want.Changes {
		bad("view.changes is %d, want %d", len(doc.WorkspaceChanges), *want.Changes)
	}
	if doc.Summary.Changes != len(doc.WorkspaceChanges) {
		bad("view.summary.changes is %d, the document lists %d", doc.Summary.Changes, len(doc.WorkspaceChanges))
	}
	if want.ProviderBodies != nil && doc.Summary.ProviderBodies != *want.ProviderBodies {
		bad("view.provider_bodies is %d, want %d", doc.Summary.ProviderBodies, *want.ProviderBodies)
	}
	if want.ProviderFiles != nil {
		n := 0
		for _, f := range doc.Files {
			if f.Kind == string(sessiondata.KindProviderBody) {
				n++
			}
		}
		if n != *want.ProviderFiles {
			bad("view.provider_files is %d, want %d", n, *want.ProviderFiles)
		}
	}
	if want.CapturedPrompts != nil && doc.Summary.CapturedPrompts != *want.CapturedPrompts {
		bad("view.captured_prompts is %d, want %d", doc.Summary.CapturedPrompts, *want.CapturedPrompts)
	}
	if want.ChangedFiles != nil || want.CapturedBy != nil || want.ChangesJoined != nil {
		files, joined := 0, 0
		bySource := map[string]int{}
		for _, wc := range doc.WorkspaceChanges {
			files += len(wc.Changes)
			bySource[wc.CapturedBy]++
			if wc.Step != "" {
				joined++
			}
		}
		if want.ChangedFiles != nil && files != *want.ChangedFiles {
			bad("view.changed_files is %d, want %d", files, *want.ChangedFiles)
		}
		if want.ChangesJoined != nil && joined != *want.ChangesJoined {
			bad("view.changes_joined is %d, want %d", joined, *want.ChangesJoined)
		}
		for who, n := range want.CapturedBy {
			if bySource[who] != n {
				bad("view.captured_by[%s] is %d, want %d", who, bySource[who], n)
			}
		}
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

// Summary is what one format's fold looks like, for comparing folds of the
// same evidence. Unresolved counts only the open references: a reference
// that was open in one round and resolved in a later one stays in the fold
// as resolved, while a parse that saw the evidence at once never had it, and
// both are the same conversation.
type Summary struct {
	Kinds      map[string]int
	Relations  map[string]int
	TalksOn    map[string]int
	Unresolved map[string]int // kind -> open references
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
	for _, u := range v.OpenUnresolved() {
		s.Unresolved[u.Kind]++
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

// Properties over the landed records themselves.

// recordView is the part of a landed record two formats must agree on: the
// role-named identifiers, who produced it, when, what started it, its flags,
// its usage, and the shape of its parts. Provenance differs by construction,
// and a data part's bytes are the runtime's own, so neither is compared.
type recordView struct {
	Kind, Stream, Batch                                                       string
	ID, Parent, Call, Run, Continues, Tool, Child, RecBatch, Label, StartedBy string
	From, Time, Trigger                                                       string
	Flags                                                                     string
	Usage, Model                                                              string
	Parts                                                                     string
	Dropped                                                                   int
}

func viewOf(hdr *sessiondata.Header, r *sessiondata.Record) recordView {
	flags := append([]string(nil), r.Flags...)
	sort.Strings(flags)
	usage := ""
	if r.Usage != nil {
		usage = fmt.Sprintf("%d/%d/%d/%d", r.Usage.Input, r.Usage.Output, r.Usage.CacheRead, r.Usage.CacheWrite)
	}
	var parts []string
	for _, p := range r.Parts {
		text := p.Text
		if p.Kind == sessiondata.PartData || p.Kind == sessiondata.PartUnknown {
			text = ""
		}
		failed := ""
		if p.Failed != nil {
			failed = fmt.Sprint(*p.Failed)
		}
		parts = append(parts, fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s", p.Kind, p.ID, p.Of, p.Name, p.State, failed, text))
	}
	return recordView{
		Kind: string(hdr.Kind), Stream: hdr.Stream, Batch: hdr.Batch,
		ID: r.ID, Parent: r.Parent, Call: r.Call, Run: r.Run, Continues: r.Continues, Tool: r.Tool, Child: r.Child,
		RecBatch: r.Batch, Label: r.Label, StartedBy: r.StartedBy,
		From: string(r.From), Time: r.Time, Trigger: r.Trigger,
		Flags: strings.Join(flags, ","), Usage: usage, Model: r.Model, Parts: strings.Join(parts, ";"), Dropped: len(r.Dropped),
	}
}

// landedRecords reads every record of a session, grouped by the file it
// came from, in landed order.
func landedRecords(root, session string) (map[string][]recordView, error) {
	files, err := storage.LandedFiles(storage.NewZone(root), session)
	if err != nil {
		return nil, err
	}
	out := map[string][]recordView{}
	for _, lf := range files {
		f, err := os.Open(lf.Path)
		if err != nil {
			return nil, err
		}
		r, err := sessiondata.NewReader(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		hdr := r.Header()
		key := string(hdr.Kind) + ":" + hdr.Stream + ":" + hdr.Batch
		for {
			rec, err := r.Next()
			if err != nil {
				break
			}
			out[key] = append(out[key], viewOf(&hdr, rec))
		}
		f.Close()
	}
	return out, nil
}

// RecordsAgree compares the landed records of two roots: every file kind
// and stream holds the same records, with the same identifiers, producer,
// time, flags, usage and parts, in the same order. A runtime's adapter and
// the sd writer must land the same evidence from the same scenario; where
// they do not, one of them reads the model differently.
func RecordsAgree(rootA, rootB, session string) ([]string, error) {
	a, err := landedRecords(rootA, session)
	if err != nil {
		return nil, err
	}
	b, err := landedRecords(rootB, session)
	if err != nil {
		return nil, err
	}
	var out []string
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	var names []string
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		ra, rb := a[k], b[k]
		if len(ra) != len(rb) {
			out = append(out, fmt.Sprintf("records_match: %s holds %d records against %d", k, len(ra), len(rb)))
			continue
		}
		for i := range ra {
			if ra[i] != rb[i] {
				out = append(out, fmt.Sprintf("records_match: %s record %d (%s) differs:\n    %+v\n    %+v", k, i+1, ra[i].ID, ra[i], rb[i]))
				if len(out) > 8 {
					return out, nil
				}
			}
		}
	}
	return out, nil
}

// RecordsWellFormed checks every landed file's header names what it is and
// every record carries only the fields the format states a purpose for, and
// the ones provenance requires.
func RecordsWellFormed(root, session string) ([]string, error) {
	files, err := storage.LandedFiles(storage.NewZone(root), session)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{
		"ord": true, "off": true, "sha": true, "bytes": true,
		"from": true, "time": true, "trigger": true, "flags": true,
		"id": true, "parent": true, "call": true, "run": true, "continues": true, "tool": true, "child": true,
		"batch": true, "label": true, "started_by": true,
		"parts": true, "usage": true, "model": true, "dropped": true,
	}
	var out []string
	for _, lf := range files {
		data, err := os.ReadFile(lf.Path)
		if err != nil {
			return nil, err
		}
		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		var hdr sessiondata.Header
		if err := json.Unmarshal([]byte(lines[0]), &hdr); err != nil {
			return nil, err
		}
		for name, got := range map[string]string{"kind": string(hdr.Kind), "session": hdr.Session, "src": hdr.Src, "adapter": hdr.Adapter, "dialect": hdr.Dialect, "at": hdr.At} {
			if got == "" {
				out = append(out, fmt.Sprintf("records_well_formed: %s: header field %q is empty", filepath.Base(lf.Path), name))
			}
		}
		// A provider body belongs to the session and no stream or run in it.
		if hdr.Session != session || (hdr.Stream == "" && hdr.Batch == "" && hdr.Kind != sessiondata.KindProviderBody) {
			out = append(out, fmt.Sprintf("records_well_formed: %s: header identity %s stream=%q batch=%q", filepath.Base(lf.Path), hdr.Session, hdr.Stream, hdr.Batch))
		}
		for i, line := range lines[1 : len(lines)-1] {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(line), &fields); err != nil {
				return nil, err
			}
			for k := range fields {
				if !allowed[k] {
					out = append(out, fmt.Sprintf("records_well_formed: %s record %d carries field %q with no stated purpose", filepath.Base(lf.Path), i+1, k))
				}
			}
			for _, must := range []string{"ord", "off", "sha", "bytes", "parts"} {
				if _, ok := fields[must]; !ok {
					out = append(out, fmt.Sprintf("records_well_formed: %s record %d lacks %q", filepath.Base(lf.Path), i+1, must))
				}
			}
		}
	}
	return out, nil
}

// ViewCoversTheSession checks the asz.view document holds the whole
// session: every round of the chain, verified; every landed file, with its
// digest as on disk; every talk, run and step of the fold in a tree, under
// a talk or under loose; the session's own range; and a verified state.
func ViewCoversTheSession(root, session string) ([]string, error) {
	srv := view.New(storage.NewZone(root), nil)
	c, err := srv.Load(session)
	if err != nil {
		return nil, err
	}
	doc, err := c.Build()
	if err != nil {
		return nil, err
	}
	v := c.View
	var out []string
	bad := func(format string, a ...any) {
		out = append(out, "view_covers_the_session: "+fmt.Sprintf(format, a...))
	}
	// The document's state is what asz verify says of the root: verified,
	// incomplete when a round names a file that is gone, mismatch when the
	// evidence changed. A root a scenario damaged must be reported as such.
	chainRep, err := verify.Chain(storage.NewZone(root), session, nil)
	if err != nil {
		return nil, err
	}
	wantState := sessionview.StateVerified
	verifiedRound := map[uint64]bool{}
	for _, rr := range chainRep.Rounds {
		verifiedRound[rr.Round] = rr.OK()
		switch {
		case rr.Damaged():
			wantState = sessionview.StateMismatch
		case !rr.OK() && wantState == sessionview.StateVerified:
			wantState = sessionview.StateIncomplete
		}
	}
	if doc.Summary.State != wantState {
		bad("state is %s (%v), asz verify says %s (%v)", doc.Summary.State, doc.Summary.Problems, wantState, chainRep.Details())
	}
	if len(doc.Summary.Problems) != chainRep.Problems {
		bad("the document lists %d problem(s) %v, asz verify %d %v", len(doc.Summary.Problems), doc.Summary.Problems, chainRep.Problems, chainRep.Details())
	}
	// Every round, verified, and the head.
	chain := sessionflow.OpenChain(root, session)
	files, err := chain.List()
	if err != nil {
		return nil, err
	}
	if len(doc.Rounds) != len(files) || doc.Head.Round != v.Round || doc.Head.Digest != v.Digest {
		bad("%d rounds in the document, %d on disk; head %d/%s against %d/%s", len(doc.Rounds), len(files), doc.Head.Round, firstN(doc.Head.Digest, 12), v.Round, firstN(v.Digest, 12))
	}
	for _, r := range doc.Rounds {
		if r.Verified != verifiedRound[r.Round] {
			bad("round %d is verified: %v, asz verify says %v", r.Round, r.Verified, verifiedRound[r.Round])
		}
	}
	// Every landed file, as on disk.
	landed, err := storage.LandedFiles(storage.NewZone(root), session)
	if err != nil {
		return nil, err
	}
	byFile := map[string]sessionview.File{}
	for _, f := range doc.Files {
		byFile[f.File] = f
	}
	for _, lf := range landed {
		rel, _ := filepath.Rel(root, lf.Path)
		rel = filepath.ToSlash(rel)
		f, ok := byFile[rel]
		if !ok {
			bad("landed file %s is not in the document", rel)
			continue
		}
		d, err := storage.FileDigest(lf.Path)
		if err != nil {
			return nil, err
		}
		if f.Digest != d || f.Seq == nil || *f.Seq != lf.Seq {
			bad("landed file %s: digest or sequence differ from disk", rel)
		}
	}
	if len(doc.Files) != len(landed)+len(files) {
		bad("%d files in the document, %d landed and %d rounds on disk", len(doc.Files), len(landed), len(files))
	}
	// Every talk, run and step, in a tree.
	inTrees := map[string]bool{}
	var walk func(n *sessionview.Node)
	walk = func(n *sessionview.Node) {
		inTrees[n.ID] = true
		for i := range n.Children {
			walk(&n.Children[i])
		}
	}
	for i := range doc.Talks {
		walk(&doc.Talks[i])
	}
	for i := range doc.Loose {
		walk(&doc.Loose[i])
	}
	for id, n := range v.Nodes {
		switch n.Kind {
		case model.KindConversation, model.KindSession, model.KindStream, model.KindEpoch, model.KindSegment:
			continue
		}
		if !inTrees[id] {
			bad("%s (%s) is in the fold but in no tree of the document", id, n.Kind)
		}
	}
	// Every change record joined to a step is listed on that step, and a
	// step lists nothing the document does not hold.
	onStep := map[string][]string{}
	var collect func(n *sessionview.Node)
	collect = func(n *sessionview.Node) {
		if len(n.Changes) > 0 {
			onStep[n.ID] = n.Changes
		}
		for i := range n.Children {
			collect(&n.Children[i])
		}
	}
	for i := range doc.Talks {
		collect(&doc.Talks[i])
	}
	for i := range doc.Loose {
		collect(&doc.Loose[i])
	}
	listed := map[string][]string{}
	for _, wc := range doc.WorkspaceChanges {
		if wc.Step != "" {
			listed[wc.Step] = append(listed[wc.Step], wc.ID)
		}
	}
	for step, ids := range listed {
		if strings.Join(onStep[step], ",") != strings.Join(ids, ",") {
			bad("step %s carries changes %v, the document joins %v to it", step, onStep[step], ids)
		}
	}
	for step := range onStep {
		if _, ok := listed[step]; !ok {
			bad("step %s carries changes the document does not join to it", step)
		}
	}
	if doc.Summary.Changes != len(doc.WorkspaceChanges) {
		bad("summary.changes is %d, the document lists %d", doc.Summary.Changes, len(doc.WorkspaceChanges))
	}
	// Only a call lists provider bodies, its request before its response,
	// each naming a landed provider_body record of that role, and the
	// summary counts what the calls list.
	bodies := providerRecords(root, session)
	prompts := 0
	var checkBodies func(n *sessionview.Node)
	checkBodies = func(n *sessionview.Node) {
		if len(n.ProviderBodies) > 0 && n.Kind != model.KindLLMCall {
			bad("%s (%s) lists provider bodies and is no call", n.ID, n.Kind)
		}
		// A synthetic call was never sent to a provider: it has no body.
		for _, c := range n.Children {
			if c.Kind == model.KindMessageSynthetic && len(n.ProviderBodies) > 0 {
				bad("%s is a synthetic call and lists provider bodies", n.ID)
			}
		}
		for i, pb := range n.ProviderBodies {
			if pb.Role == "request" {
				prompts++
			}
			role, ok := bodies[[2]uint64{pb.Ref.Seq, pb.Ref.Row}]
			switch {
			case !ok:
				bad("call %s lists a provider body at %d/%d that is no landed provider body", n.ID, pb.Ref.Seq, pb.Ref.Row)
			case role != pb.Role:
				bad("call %s lists a %s at %d/%d, and the record there is a %s", n.ID, pb.Role, pb.Ref.Seq, pb.Ref.Row, role)
			case i > 0 && pb.Role == "request":
				bad("call %s lists its request after another body", n.ID)
			}
		}
		for i := range n.Children {
			checkBodies(&n.Children[i])
		}
	}
	for i := range doc.Talks {
		checkBodies(&doc.Talks[i])
	}
	for i := range doc.Loose {
		checkBodies(&doc.Loose[i])
	}
	if doc.Summary.CapturedPrompts != prompts || doc.Summary.ProviderBodies != len(bodies) {
		bad("summary.provider_bodies is %d and captured_prompts %d, the root holds %d bodies and %d calls list a request",
			doc.Summary.ProviderBodies, doc.Summary.CapturedPrompts, len(bodies), prompts)
	}
	if len(doc.Talks) != len(v.NodesByKind(model.KindTalk)) || doc.Summary.Talks != len(doc.Talks) {
		bad("%d talks in the document, %d in the fold, summary says %d", len(doc.Talks), len(v.NodesByKind(model.KindTalk)), doc.Summary.Talks)
	}
	// The same fold renders the same document, byte for byte.
	again, err := view.New(storage.NewZone(root), nil).Load(session)
	if err != nil {
		return nil, err
	}
	doc2, err := again.Build()
	if err != nil {
		return nil, err
	}
	a, _ := json.Marshal(doc)
	b, _ := json.Marshal(doc2)
	if string(a) != string(b) {
		bad("two builds of the same fold differ")
	}
	// The session's own range.
	if sn := v.Nodes[sessionflow.NodeID("session", session)]; sn != nil {
		var attrs struct {
			From    string `json:"from_time"`
			Through string `json:"through_time"`
		}
		_ = json.Unmarshal(sn.Attrs, &attrs)
		if doc.Summary.From != stamp(attrs.From).UnixMilli() || doc.Summary.To != stamp(attrs.Through).UnixMilli() {
			bad("summary range %d..%d, the session node says %s..%s", doc.Summary.From, doc.Summary.To, attrs.From, attrs.Through)
		}
	}
	return out, nil
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// providerRecords is the role of every landed provider body of a session, by
// file sequence and row, a body landed twice counted once.
func providerRecords(root, session string) map[[2]uint64]string {
	out := map[[2]uint64]string{}
	files, err := storage.LandedFiles(storage.NewZone(root), session)
	if err != nil {
		return out
	}
	seen := map[string]bool{}
	for _, lf := range files {
		if !strings.HasPrefix(filepath.Base(lf.Path), string(sessiondata.KindProviderBody)+"-") {
			continue
		}
		f, err := os.Open(lf.Path)
		if err != nil {
			continue
		}
		_, recs, err := sessiondata.All(f)
		f.Close()
		if err != nil {
			continue
		}
		for row, rec := range recs {
			m, err := providerbody.ManifestOf(rec)
			if err != nil || seen[rec.ID] {
				continue
			}
			seen[rec.ID] = true
			out[[2]uint64{lf.Seq, uint64(row + 1)}] = m.Role
		}
	}
	return out
}

// ProviderBodiesRebuild reads every provider body a call lists the way a
// reader that loads bodies on demand reads it: the session's provider_body
// files with a sequence up to the one the body's ref names, in order, and
// nothing else. Each body must rebuild to the digest and size its manifest
// states. It also says how many provider files the session has, so a check
// can tell that bodies referred across files.
func ProviderBodiesRebuild(root, session string) ([]string, int, error) {
	c, err := view.New(storage.NewZone(root), nil).Load(session)
	if err != nil {
		return nil, 0, err
	}
	doc, err := c.Build()
	if err != nil {
		return nil, 0, err
	}
	var out []string
	bad := func(format string, a ...any) {
		out = append(out, "provider_bodies_rebuild: "+fmt.Sprintf(format, a...))
	}
	var refs []sessionview.ProviderBody
	var walk func(n *sessionview.Node)
	walk = func(n *sessionview.Node) {
		refs = append(refs, n.ProviderBodies...)
		for i := range n.Children {
			walk(&n.Children[i])
		}
	}
	for i := range doc.Talks {
		walk(&doc.Talks[i])
	}
	for i := range doc.Loose {
		walk(&doc.Loose[i])
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Ref.Seq != refs[j].Ref.Seq {
			return refs[i].Ref.Seq < refs[j].Ref.Seq
		}
		return refs[i].Ref.Row < refs[j].Ref.Row
	})

	files, err := storage.LandedFiles(storage.NewZone(root), session)
	if err != nil {
		return nil, 0, err
	}
	type landed struct {
		seq  uint64
		recs []*sessiondata.Record
	}
	var provider []landed
	for _, lf := range files {
		if !strings.HasPrefix(filepath.Base(lf.Path), string(sessiondata.KindProviderBody)+"-") {
			continue
		}
		f, err := os.Open(lf.Path)
		if err != nil {
			return nil, 0, err
		}
		_, recs, err := sessiondata.All(f)
		f.Close()
		if err != nil {
			return nil, 0, err
		}
		provider = append(provider, landed{lf.Seq, recs})
	}

	// One session, fed file by file as the refs reach further, never past
	// the file a ref names.
	held := providerbody.NewSession()
	next := 0
	for _, r := range refs {
		for next < len(provider) && provider[next].seq <= r.Ref.Seq {
			for _, rec := range provider[next].recs {
				if err := held.Add(rec); err != nil && !errors.Is(err, providerbody.ErrRepeat) {
					bad("%s in file %d does not hold together: %v", rec.ID, provider[next].seq, err)
				}
			}
			next++
		}
		var rec *sessiondata.Record
		for _, p := range provider {
			if p.seq == r.Ref.Seq && r.Ref.Row >= 1 && int(r.Ref.Row) <= len(p.recs) {
				rec = p.recs[r.Ref.Row-1]
			}
		}
		if rec == nil {
			bad("a call lists %d/%d, which is no landed provider body", r.Ref.Seq, r.Ref.Row)
			continue
		}
		m, err := providerbody.ManifestOf(rec)
		if err != nil {
			bad("%v", err)
			continue
		}
		body, err := held.Body(rec.ID)
		switch {
		case err != nil:
			bad("%s does not rebuild from the files up to %d: %v", rec.ID, r.Ref.Seq, err)
		case len(body) != m.Bytes || providerbody.Digest(body) != m.SHA256:
			bad("%s rebuilds to other bytes than its manifest states", rec.ID)
		case m.Role != r.Role:
			bad("%s is a %s, listed as a %s", rec.ID, m.Role, r.Role)
		}
	}
	return out, len(provider), nil
}

// SameIdentity compares two folds of one scenario node by node, parent by
// parent and relation by relation, not only by count. A node id built from a
// landed position names the file's sequence, which files of another kind
// landing between them shift, so a node with a reference is named by the
// record it stands on: its kind, its stream, its file's source and the
// record's line in that source, and the part.
func SameIdentity(rootA, sessionA, rootB, sessionB string) ([]string, error) {
	a, err := foldIdentity(rootA, sessionA)
	if err != nil {
		return nil, err
	}
	b, err := foldIdentity(rootB, sessionB)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, side := range []struct {
		label    string
		has, not map[string]bool
	}{{"only in the first", a, b}, {"only in the second", b, a}} {
		var missing []string
		for k := range side.has {
			if !side.not[k] {
				missing = append(missing, k)
			}
		}
		sort.Strings(missing)
		for i, k := range missing {
			if i == 5 {
				out = append(out, fmt.Sprintf("%s: … and %d more", side.label, len(missing)-5))
				break
			}
			out = append(out, side.label+": "+k)
		}
	}
	return out, nil
}

// positionID matches an id sessionflow.RefID builds: a kind, a landed
// sequence and row, and a part.
var positionID = regexp.MustCompile(`^[^/]+/[0-9]+/[0-9]+(:[0-9]+)?$`)

func foldIdentity(root, session string) (map[string]bool, error) {
	v, err := parse.View(root, session)
	if err != nil {
		return nil, err
	}
	files, err := storage.LandedFiles(storage.NewZone(root), session)
	if err != nil {
		return nil, err
	}
	type at struct{ seq, row uint64 }
	source := map[at]string{}
	for _, lf := range files {
		f, err := os.Open(lf.Path)
		if err != nil {
			return nil, err
		}
		hdr, recs, err := sessiondata.All(f)
		f.Close()
		if err != nil {
			return nil, err
		}
		for row, rec := range recs {
			source[at{lf.Seq, uint64(row + 1)}] = fmt.Sprintf("%s#%d", hdr.Src, rec.Ord)
		}
	}
	name := map[string]string{}
	nameOf := func(id string) string {
		if s, ok := name[id]; ok {
			return s
		}
		n := v.Nodes[id]
		// Only an id built from a landed position is renamed; any other id
		// is compared as it is, since nothing but the evidence moves it.
		if n == nil || n.Ref == nil || !positionID.MatchString(id) {
			name[id] = id
			return id
		}
		s := fmt.Sprintf("%s|%s|%s", n.Kind, n.Stream, source[at{n.Ref.Seq, n.Ref.Row}])
		if n.Ref.Block != nil {
			s += fmt.Sprintf(":%d", *n.Ref.Block)
		}
		name[id] = s
		return s
	}
	out := map[string]bool{}
	for id, n := range v.Nodes {
		out["node "+nameOf(id)+" in "+nameOf(n.Parent)] = true
	}
	for _, r := range v.Relations {
		out["relation "+r.Type+" "+nameOf(r.From)+" -> "+nameOf(r.To)] = true
	}
	return out, nil
}
