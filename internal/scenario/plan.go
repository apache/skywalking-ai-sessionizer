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

package scenario

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Options settle a build's clock and how far it goes.
type Options struct {
	// At is the base time. Zero means now.
	At time.Time
	// Scale multiplies every delta. Zero means one.
	Scale float64
	// Interval overrides the scenario's. Zero means the scenario's, and one
	// second when the scenario gives none.
	Interval time.Duration
	// Through stops the plan after the step carrying this checkpoint. Empty
	// means every step.
	Through string
}

// EventKind is one shape of record.
type EventKind int

// The shapes of record a scenario produces. Every one maps to one record in
// every writer, so the two formats hold the same evidence.
const (
	EvInput     EventKind = iota + 1 // a person's message, or a child's prompt
	EvQueued                         // input that exists only as a queued command
	EvInject                         // material the harness put into context
	EvFragment                       // one fragment of a provider call
	EvResult                         // a tool result, a launch acknowledgement, a fork or a workflow launch
	EvNotice                         // the runtime reporting a child finished
	EvSynthetic                      // an assistant-role message the client made
	EvBoundary                       // a context reset
	EvSummary                        // the summary that replaced the context
	EvSystem                         // a system record of any subtype
	EvTitle                          // the title the runtime gave the session
)

// FragKind is what one fragment of a call carries.
type FragKind int

// The three fragment shapes, written in this order within a call.
const (
	FragThinking FragKind = iota + 1
	FragText
	FragToolUse
)

// ToolUse is the request a tool_use fragment carries.
type ToolUse struct {
	ID    string
	Name  string
	Input map[string]any
}

// Ack is what a launch acknowledgement names.
type Ack struct {
	Child  string
	Prompt string
	Label  string
}

// Fork is what a skill fork's result names: the child it started.
type Fork struct {
	Child string
}

// Launch is what a workflow launch's result names.
type Launch struct {
	Run  string
	Name string
}

// Event is one record to write, in either format.
type Event struct {
	Kind   EventKind
	Stream string // "main" or an agent id
	// Batch is the workflow run a child stream belongs to, so a writer can
	// file it with the run.
	Batch string
	At    time.Time

	ID     string
	Parent string
	Run    string
	Call   string
	Req    string
	Text   string
	// Replayed marks a copy the runtime re-emitted, with its run rewritten.
	Replayed bool

	// A fragment.
	Frag  FragKind
	Last  bool
	Stop  string
	Usage Usage
	Tool  *ToolUse

	// A result.
	Of               string
	Failed           *bool
	StringEnrichment bool
	Ack              *Ack
	Fork             *Fork
	Launch           *Launch

	// A notice.
	NoticeTool  string
	NoticeChild string

	// A queued command, an injection, a system record.
	Mode   string
	Type   string
	Fields map[string]any

	// A boundary.
	Continues string

	// A title.
	Label string
}

// Stream is one child stream the plan created, with what the runtime
// recorded about it.
type Stream struct {
	ID     string
	Label  string
	Prompt string
	Tool   string
	Batch  string
}

// Run is one workflow run: its children and the files filed with it.
type Run struct {
	ID       string
	Name     string
	Children []string
	// ScriptProject is the project directory the script is filed under,
	// when not the session's own.
	ScriptProject string
	// Journal is the run's journal, in order: a started line per child, and
	// a result line when a child finished.
	Journal []JournalLine
	Script  string
}

// JournalLine is one line of a workflow's journal.
type JournalLine struct {
	Type  string // "started" or "result"
	Child string
	At    time.Time
}

// Plan is a scenario resolved into timed records with stable ids.
type Plan struct {
	Session  string
	Project  string
	Title    string
	Events   []Event
	Streams  []Stream
	Runs     []Run
	interval time.Duration
	scale    float64
}

// Plan resolves a scenario: every step becomes records on a clock, with ids
// derived from the step's position, so the same scenario always plans the
// same session and the same ids come out of every writer.
func (sc *Scenario) Plan(opts Options) (*Plan, error) {
	at := opts.At
	if at.IsZero() {
		at = time.Now()
	}
	at = at.UTC().Truncate(time.Millisecond)
	interval := opts.Interval
	if interval == 0 {
		interval = sc.Interval
	}
	if interval == 0 {
		interval = time.Second
	}
	scale := opts.Scale
	if scale == 0 {
		scale = 1
	}
	p := &Plan{Session: sc.Session, Project: sc.Project, Title: sc.Title, interval: interval, scale: scale}
	if p.Project == "" {
		p.Project = "-Users-dev-scenario"
	}
	if p.Session == "" {
		p.Session = derivedSession(sc)
	}
	main := &lane{stream: "main", t: at, first: true}
	b := &planner{p: p, main: main}
	if sc.Title != "" {
		// The title is written first, so every checkpoint has it. It carries
		// no time, as the runtime writes it.
		p.Events = append(p.Events, Event{Kind: EvTitle, Stream: "main", Label: sc.Title})
	}
	for i := range sc.Steps {
		s := &sc.Steps[i]
		b.n++
		if err := b.step(main, s, fmt.Sprintf("s%d", b.n)); err != nil {
			return nil, err
		}
		if s.Checkpoint != "" && s.Checkpoint == opts.Through {
			return p, nil
		}
	}
	if opts.Through != "" {
		return nil, fmt.Errorf("scenario: no checkpoint named %q", opts.Through)
	}
	return p, nil
}

// lane is one stream's clock and parent chain.
type lane struct {
	stream string
	batch  string
	t      time.Time
	last   string
	first  bool
	n      int
	prefix string
}

type planner struct {
	p    *Plan
	main *lane
	n    int
}

// tick advances a lane by a step's delta, or the interval.
func (b *planner) tick(l *lane, after time.Duration) time.Time {
	if l.first {
		l.first = false
		l.t = l.t.Add(b.scaled(after))
		return l.t
	}
	if after == 0 {
		after = b.p.interval
	}
	l.t = l.t.Add(b.scaled(after))
	return l.t
}

func (b *planner) scaled(d time.Duration) time.Duration {
	return time.Duration(float64(d) * b.p.scale).Truncate(time.Millisecond)
}

func (b *planner) emit(e Event) {
	b.p.Events = append(b.p.Events, e)
}

func (b *planner) step(l *lane, s *Step, id string) error {
	switch {
	case s.Input != "":
		at := b.tick(l, s.After)
		run := id + "-cycle"
		e := Event{Kind: EvInput, Stream: l.stream, Batch: l.batch, At: at, ID: id + "-input", Run: run, Text: s.Input}
		if l.stream != "main" {
			e.Parent = l.last
		}
		b.emit(e)
		l.last = e.ID
	case s.Queued != nil:
		at := b.tick(l, s.After)
		e := Event{Kind: EvQueued, Stream: l.stream, Batch: l.batch, At: at, ID: id + "-queued", Parent: l.last, Text: s.Queued.Text, Mode: s.Queued.Mode}
		b.emit(e)
		l.last = e.ID
	case s.Inject != nil:
		at := b.tick(l, s.After)
		e := Event{Kind: EvInject, Stream: l.stream, Batch: l.batch, At: at, ID: id + "-inject", Parent: l.last, Type: s.Inject.Type, Text: s.Inject.Text}
		b.emit(e)
		l.last = e.ID
	case s.Call != nil:
		return b.call(l, s, id)
	case s.Result != nil:
		at := b.tick(l, s.After)
		r := s.Result
		e := Event{Kind: EvResult, Stream: l.stream, Batch: l.batch, At: at, ID: id + "-result", Parent: l.last,
			Run: b.runOf(l), Of: r.Of, Text: r.Text, Failed: r.Failed, StringEnrichment: r.String}
		b.emit(e)
		l.last = e.ID
	case s.Error != "":
		at := b.tick(l, s.After)
		e := Event{Kind: EvSynthetic, Stream: l.stream, Batch: l.batch, At: at, ID: id + "-synthetic", Parent: l.last,
			Call: id + "-synthetic-call", Req: id + "-req", Text: s.Error}
		b.emit(e)
		l.last = e.ID
	case s.Reset != nil:
		at := b.tick(l, s.After)
		boundary := Event{Kind: EvBoundary, Stream: l.stream, At: at, ID: id + "-boundary", Continues: l.last}
		b.emit(boundary)
		// The summary is timestamped before the boundary that produced it,
		// as the runtime writes it, so anything ordering an epoch by time
		// gets it backwards. That is the property a scenario must reproduce.
		summary := Event{Kind: EvSummary, Stream: l.stream, At: at.Add(-400 * time.Millisecond), ID: id + "-summary",
			Parent: boundary.ID, Run: id + "-cycle-compact", Text: s.Reset.Summary}
		b.emit(summary)
		l.last = summary.ID
	case s.Replay > 0:
		b.replay(s.Replay)
	case s.System != nil:
		at := b.tick(l, s.After)
		e := Event{Kind: EvSystem, Stream: l.stream, Batch: l.batch, At: at, ID: id + "-sys-" + s.System.Subtype, Parent: l.last,
			Type: s.System.Subtype, Fields: s.System.Fields}
		b.emit(e)
		l.last = e.ID
	}
	return nil
}

// runOf is the run a record on a lane belongs to: the last input's cycle.
func (b *planner) runOf(l *lane) string {
	for i := len(b.p.Events) - 1; i >= 0; i-- {
		e := &b.p.Events[i]
		if e.Stream == l.stream && (e.Kind == EvInput || e.Kind == EvNotice || e.Kind == EvSummary) && !e.Replayed {
			return e.Run
		}
	}
	return ""
}

func (b *planner) call(l *lane, s *Step, id string) error {
	c := s.Call
	at := b.tick(l, s.After)
	usage := Usage{In: 2, Out: 40, CacheRead: 900, CacheWrite: 100}
	if c.Usage != nil {
		if c.Usage.In != 0 {
			usage.In = c.Usage.In
		}
		if c.Usage.Out != 0 {
			usage.Out = c.Usage.Out
		}
		if c.Usage.CacheRead != 0 {
			usage.CacheRead = c.Usage.CacheRead
		}
		if c.Usage.CacheWrite != 0 {
			usage.CacheWrite = c.Usage.CacheWrite
		}
	}
	call, req := id+"-call", id+"-req"
	var frags []Event
	if c.Thinking != "" {
		text := c.Thinking
		if text == "unavailable" {
			text = ""
		}
		frags = append(frags, Event{Kind: EvFragment, Frag: FragThinking, Text: text})
	}
	if c.Text != "" {
		frags = append(frags, Event{Kind: EvFragment, Frag: FragText, Text: c.Text})
	}
	var tool *ToolUse
	switch {
	case c.Tool != nil:
		tid := c.Tool.ID
		if tid == "" {
			tid = id + "-tool"
		}
		tool = &ToolUse{ID: tid, Name: c.Tool.Name, Input: c.Tool.Input}
	case c.Agent != nil:
		tool = &ToolUse{ID: id + "-tool", Name: "Agent", Input: map[string]any{"description": c.Agent.Name, "prompt": c.Agent.Prompt}}
	case c.Skill != nil:
		tool = &ToolUse{ID: id + "-tool", Name: "Skill", Input: map[string]any{"skill": c.Skill.Name}}
	case c.Workflow != nil:
		tool = &ToolUse{ID: id + "-tool", Name: "Workflow", Input: map[string]any{"script": "export const meta = {}"}}
	}
	if tool != nil {
		frags = append(frags, Event{Kind: EvFragment, Frag: FragToolUse, Tool: tool})
	}
	if len(frags) == 0 {
		return fmt.Errorf("scenario: step %s: a call has thinking, text, or a request", id)
	}
	for i := range frags {
		f := &frags[i]
		f.Stream, f.Batch = l.stream, l.batch
		f.At = at.Add(b.scaled(time.Duration(i) * 100 * time.Millisecond))
		f.ID = fmt.Sprintf("%s-call-f%d", id, i+1)
		f.Parent = l.last
		f.Call, f.Req, f.Usage = call, req, usage
		f.Last = i == len(frags)-1
		f.Stop = "end_turn"
		if tool != nil {
			f.Stop = "tool_use"
		}
		b.emit(*f)
		l.last = f.ID
		l.t = f.At
	}
	run := b.runOf(l)
	switch {
	case c.Tool != nil && c.Tool.Result != nil:
		r := c.Tool.Result
		rat := b.tick(l, r.After)
		e := Event{Kind: EvResult, Stream: l.stream, Batch: l.batch, At: rat, ID: id + "-result", Parent: l.last, Run: run,
			Of: tool.ID, Text: r.Text, Failed: r.Failed, StringEnrichment: r.String}
		b.emit(e)
		l.last = e.ID
	case c.Agent != nil:
		a := c.Agent
		child := agentID(a.Name)
		ack := Event{Kind: EvResult, Stream: l.stream, Batch: l.batch, At: b.tick(l, 100*time.Millisecond), ID: id + "-ack",
			Parent: l.last, Run: run, Of: tool.ID, Text: "launched",
			Ack: &Ack{Child: child, Prompt: a.Prompt, Label: a.Name}}
		b.emit(ack)
		l.last = ack.ID
		b.p.Streams = append(b.p.Streams, Stream{ID: child, Label: a.Name, Prompt: a.Prompt, Tool: tool.ID})
		cl := &lane{stream: child, t: ack.At, first: true, prefix: a.Name}
		if a.After == 0 {
			cl.t = cl.t.Add(b.scaled(b.p.interval))
		} else {
			cl.t = cl.t.Add(b.scaled(a.After))
		}
		if a.Prompt != "" {
			prompt := Event{Kind: EvInput, Stream: child, At: cl.t, ID: child + "-prompt", Run: child + "-cycle", Text: a.Prompt}
			cl.first = false
			b.emit(prompt)
			cl.last = prompt.ID
		}
		if err := b.steps(cl, a.Steps); err != nil {
			return err
		}
		if a.Notify {
			if cl.t.After(l.t) {
				l.t = cl.t
			}
			nat := b.tick(l, 0)
			notice := Event{Kind: EvNotice, Stream: l.stream, Batch: l.batch, At: nat, ID: id + "-notice", Parent: l.last,
				Run: id + "-cycle-notification", NoticeTool: tool.ID, NoticeChild: child,
				Text: "<task-notification>\n<task-id>" + child + "</task-id>\n<tool-use-id>" + tool.ID + "</tool-use-id>\n<status>completed</status>\n</task-notification>"}
			b.emit(notice)
			l.last = notice.ID
		}
	case c.Skill != nil:
		sk := c.Skill
		child := agentID(sk.Agent)
		res := Event{Kind: EvResult, Stream: l.stream, Batch: l.batch, At: b.tick(l, 0), ID: id + "-result", Parent: l.last, Run: run,
			Of: tool.ID, Text: "forked", Fork: &Fork{Child: child}}
		b.emit(res)
		l.last = res.ID
		b.p.Streams = append(b.p.Streams, Stream{ID: child, Label: sk.Agent, Tool: tool.ID})
		cl := &lane{stream: child, t: res.At.Add(b.scaled(b.p.interval)), prefix: sk.Agent}
		prompt := Event{Kind: EvInput, Stream: child, At: cl.t, ID: child + "-prompt", Run: child + "-cycle", Text: "run the skill " + sk.Name}
		b.emit(prompt)
		cl.last = prompt.ID
		if err := b.steps(cl, sk.Steps); err != nil {
			return err
		}
	case c.Workflow != nil:
		w := c.Workflow
		runID := "wf_" + strings.ReplaceAll(w.Name, " ", "-")
		res := Event{Kind: EvResult, Stream: l.stream, Batch: l.batch, At: b.tick(l, 0), ID: id + "-result", Parent: l.last, Run: run,
			Of: tool.ID, Text: "workflow launched", Launch: &Launch{Run: runID, Name: w.Name}}
		b.emit(res)
		l.last = res.ID
		r := Run{ID: runID, Name: w.Name, Script: "export const meta = {\n  name: '" + w.Name + "',\n}", ScriptProject: w.ScriptProject}
		t := res.At
		for _, ch := range w.Children {
			child := agentID(w.Name + "/" + ch.Name)
			r.Children = append(r.Children, child)
			b.p.Streams = append(b.p.Streams, Stream{ID: child, Label: ch.Name, Prompt: ch.Prompt, Batch: runID})
			t = t.Add(b.scaled(b.p.interval))
			r.Journal = append(r.Journal, JournalLine{Type: "started", Child: child, At: t})
			cl := &lane{stream: child, batch: runID, t: t, prefix: ch.Name}
			prompt := Event{Kind: EvInput, Stream: child, Batch: runID, At: t, ID: child + "-prompt", Run: child + "-cycle", Text: ch.Prompt}
			if prompt.Text == "" {
				prompt.Text = "do " + ch.Name
			}
			b.emit(prompt)
			cl.last = prompt.ID
			if err := b.steps(cl, ch.Steps); err != nil {
				return err
			}
			r.Journal = append(r.Journal, JournalLine{Type: "result", Child: child, At: cl.t.Add(b.scaled(100 * time.Millisecond))})
			if cl.t.After(t) {
				t = cl.t
			}
		}
		b.p.Runs = append(b.p.Runs, r)
		if t.After(l.t) {
			l.t = t
		}
	}
	return nil
}

func (b *planner) steps(l *lane, steps []Step) error {
	for i := range steps {
		l.n++
		if err := b.step(l, &steps[i], fmt.Sprintf("%s-s%d", l.prefix, l.n)); err != nil {
			return err
		}
	}
	return nil
}

// replay copies the last n main-stream records with the run rewritten, as
// the runtime does before a reset. The copies keep their ids and times: the
// later copy is the worse one, and the assembler must keep the first.
func (b *planner) replay(n int) {
	var idx []int
	for i := len(b.p.Events) - 1; i >= 0 && len(idx) < n; i-- {
		if b.p.Events[i].Stream == "main" && b.p.Events[i].Kind != EvTitle {
			idx = append(idx, i)
		}
	}
	for i := len(idx) - 1; i >= 0; i-- {
		e := b.p.Events[idx[i]]
		e.Replayed = true
		if e.Run != "" {
			e.Run = "replayed-cycle"
		}
		b.emit(e)
	}
}

// agentID derives a child's id from its name, in the shape the Claude Code
// adapter requires of an agent id: "a" and sixteen hex digits.
func agentID(name string) string {
	sum := sha256.Sum256([]byte("agent:" + name))
	return "a" + hex.EncodeToString(sum[:8])
}

// derivedSession names a session from its steps, in the UUID shape session
// discovery requires, so an unnamed scenario still reproduces.
func derivedSession(sc *Scenario) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%d", sc.Title, sc.Project, len(sc.Steps))
	for _, s := range sc.Steps {
		fmt.Fprintf(h, "|%s%v%v%v%s%v%d%v", s.Input, s.Queued, s.Inject, s.Call != nil, s.Error, s.Reset != nil, s.Replay, s.System)
	}
	x := hex.EncodeToString(h.Sum(nil))
	return fmt.Sprintf("%s-%s-4%s-8%s-%s", x[0:8], x[8:12], x[13:16], x[17:20], x[20:32])
}
