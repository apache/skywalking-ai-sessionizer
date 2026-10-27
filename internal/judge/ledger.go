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

package judge

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// Call is one model call as the ledger counts it.
type Call struct {
	ID     string
	Stream string
	// Owner is the one place the call is counted: the talk that contains
	// it, or LooseOwner of its stream when no talk does.
	Owner string
	At    int64
	// Order is the call's place in its owner's tree, counting every node in
	// the order the document lists them, so parts of a talk are cut by the
	// order the work happened in, never by time.
	Order int
	// Usage is the call's tokens as four separate types, or nil when they
	// are unknown: the call recorded none, or its counts do not add up.
	Usage *sessiondata.Usage
	// OutputUnknown says Usage holds the input side only: the call never
	// finished, so its output is unknown, while what it read is known.
	OutputUnknown bool
	Model         string
	Price         *sessionjudge.Price
	Money         float64

	refs []sessionflow.Ref
}

// LooseOwner names the owner of a stream's calls that no talk contains.
func LooseOwner(stream string) string { return "loose/" + stream }

// RecordsReader reads many landed records at once, by address.
type RecordsReader func(refs []sessionflow.Ref) map[[2]uint64]*sessiondata.Record

// Ledger is what a conversation's model calls consumed, computed from the
// fold with no model. The same document, the same landed records and the
// same price file always give the same ledger.
//
// Every call is counted once, under its owner, and the owners add up to the
// conversation exactly. A talk's inclusive cost adds the child streams it
// started, and is a view over the owners: it is never added up again.
type Ledger struct {
	Dialect  string
	Currency string
	Calls    []Call
	// Owners holds the direct cost of every owner, in the order the
	// document lists talks, then the loose owners by stream.
	Owners []sessionjudge.Cost
	// Main lists the main stream's talks, in time order.
	Main []string
	// Unattributed is the cost of child streams the rollup could not tie
	// to one main talk.
	Unattributed sessionjudge.Cost
	Total        sessionjudge.Cost
	Prices       []sessionjudge.Price
	// Subtasks lists every stream a step started, and every other stream
	// that is not the main one, in the order the rollup reached them: the
	// main talks in time order, each with its sub-tasks as deep as they go,
	// then the unattributed ones.
	Subtasks []Subtask

	owner     map[string]*sessionjudge.Cost
	inclusive map[string]*sessionjudge.Cost
	// callsOf lists the calls of each owner, and ownersOf the owners each
	// main talk includes, itself first.
	callsOf  map[string][]int
	ownersOf map[string][]string
	talks    map[string]*sessionview.Node
	// order is each node's place in its owner's tree, and at, per main talk,
	// the place a child owner's calls take in that talk: the place of the
	// step that started the child, or of the step that started its parent.
	order  map[string]int
	at     map[string]map[string]int
	agents map[string]int
	// producedBy names, for a step inside a model call's answer, such as a
	// tool the call asked for, that model call.
	producedBy map[string]string
	// subtask indexes Subtasks by id.
	subtask map[string]int
	// selected is the main talks a window selected; nil for the whole
	// conversation.
	selected []string
}

// Select narrows what the ledger prices to the main talks a window
// selected: its total becomes theirs, with every sub-task they started. The
// earlier talks stay in the ledger, for an evaluator to read, and are not
// priced.
func (l *Ledger) Select(sel *Selection) {
	if sel == nil {
		return
	}
	l.selected = sel.Talks
	var calls []int
	for _, t := range sel.Talks {
		calls = append(calls, l.CallsOf(t)...)
	}
	l.Total = l.sum(sessionjudge.ScopeConversation, l.Total.ID, calls)
}

// Judged lists the main talks a run judges: those a window selected, or
// every main talk.
func (l *Ledger) Judged() []string {
	if l.selected != nil {
		return l.selected
	}
	return l.Main
}

// JudgesSubtask says whether a run judges a sub-task: one a judged main talk
// started, at any depth, or, with no window, one no step can be shown to
// have started. The sub-tasks of a window's earlier talks are context, as
// those talks are, and are neither judged nor priced.
func (l *Ledger) JudgesSubtask(st *Subtask) bool {
	if st.Main == "" {
		return l.selected == nil
	}
	return slices.Contains(l.Judged(), st.Main)
}

// Subtask is work the agent gave to another stream: a child agent, or a
// helper stream such as a summariser. Its own calls are those of its stream;
// the sub-tasks it started in turn are its Children, as deep as they go.
type Subtask struct {
	// ID is the stream's node id, and Name the stream's name.
	ID   string
	Name string
	// Role is the stream's role, child or auxiliary, and Label what the
	// runtime called the work.
	Role  string
	Label string
	// Parent holds the step that started it: a main talk, or the stream of
	// the sub-task that started it. Main is the main talk at the top of its
	// chain. Both are empty for a stream no single step started.
	Parent    string
	Main      string
	StartedBy string
	// Depth is 1 for a sub-task a main talk started, and one more for each
	// sub-task above it.
	Depth    int
	Talks    []string
	Children []string
	// Reason says why a sub-task is unattributed: no starting step,
	// several starting steps, or a parent that is unattributed itself.
	Reason string

	own []int
}

// The reasons a sub-task is unattributed.
const (
	ReasonNoStarter      = "no starting step"
	ReasonSeveral        = "several starting steps"
	ReasonParentUnplaced = "its parent is unattributed"
)

// Compute builds the ledger of a document. dialect is the dialect the
// session's records were read in, which says what the usage counts mean.
// read, when given, reads the records of calls that never finished, whose
// input side Claude Code recorded when they started.
func Compute(doc *sessionview.Conversation, dialect string, pricing *Pricing, read RecordsReader) *Ledger {
	l, _ := ComputeChecked(doc, dialect, pricing, unchecked(read))
	return l
}

// RecordsReaderChecked is RecordsReader for a reader that must know of
// every failure, as a pulled snapshot's: a record that cannot be read is an
// error, never one left out.
type RecordsReaderChecked func(refs []sessionflow.Ref) (map[[2]uint64]*sessiondata.Record, error)

// unchecked gives a RecordsReader the checked form, with no failure to tell.
func unchecked(read RecordsReader) RecordsReaderChecked {
	if read == nil {
		return nil
	}
	return func(refs []sessionflow.Ref) (map[[2]uint64]*sessiondata.Record, error) { return read(refs), nil }
}

// ComputeChecked is Compute with a reader that tells of its failures, which
// stop the ledger.
func ComputeChecked(doc *sessionview.Conversation, dialect string, pricing *Pricing, read RecordsReaderChecked) (*Ledger, error) {
	l := &Ledger{
		Dialect: dialect, owner: map[string]*sessionjudge.Cost{}, inclusive: map[string]*sessionjudge.Cost{},
		callsOf: map[string][]int{}, ownersOf: map[string][]string{}, talks: map[string]*sessionview.Node{},
		order: map[string]int{}, at: map[string]map[string]int{}, agents: map[string]int{}, producedBy: map[string]string{},
	}
	if pricing != nil {
		l.Currency = pricing.Currency
	}
	ownerOfNode := map[string]string{}
	counter := map[string]int{}
	var walk func(n *sessionview.Node, owner, call string)
	walk = func(n *sessionview.Node, owner, call string) {
		ownerOfNode[n.ID] = owner
		l.order[n.ID] = counter[owner]
		counter[owner]++
		if call != "" {
			l.producedBy[n.ID] = call
		}
		if n.Kind == model.KindLLMCall {
			l.addCall(n, owner)
			call = n.ID
		}
		for i := range n.Children {
			walk(&n.Children[i], owner, call)
		}
	}
	var order []string
	for i := range doc.Talks {
		t := &doc.Talks[i]
		l.talks[t.ID] = t
		order = append(order, t.ID)
		if t.Stream == "main" {
			l.Main = append(l.Main, t.ID)
		}
		walk(t, t.ID, "")
	}
	loose := map[string]bool{}
	for i := range doc.Loose {
		n := &doc.Loose[i]
		o := LooseOwner(n.Stream)
		if !loose[o] {
			loose[o] = true
			order = append(order, o)
		}
		walk(n, o, "")
	}
	if err := l.readStarted(read); err != nil {
		return nil, err
	}
	for i := range l.Calls {
		l.price(&l.Calls[i], pricing)
	}
	for _, o := range order {
		c := l.sum(scopeOf(o), o, l.callsOf[o])
		if t := l.talks[o]; t != nil {
			widen(&c, t.From, t.To)
		}
		l.owner[o] = &c
		l.Owners = append(l.Owners, c)
	}
	l.Total = l.sum(sessionjudge.ScopeConversation, doc.Conversation, allCalls(len(l.Calls)))
	l.rollUp(doc, ownerOfNode)
	return l, nil
}

func scopeOf(owner string) string {
	if strings.HasPrefix(owner, "loose/") {
		return sessionjudge.ScopeLoose
	}
	return sessionjudge.ScopeTalk
}

func allCalls(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// addCall counts one call under its owner.
func (l *Ledger) addCall(n *sessionview.Node, owner string) {
	c := Call{ID: n.ID, Stream: n.Stream, Owner: owner, At: n.At, Order: l.order[n.ID], Model: n.Model,
		Usage: normalize(l.Dialect, n.Usage)}
	if c.Usage == nil && unfinished(n) {
		c.refs = n.Refs
	}
	l.callsOf[owner] = append(l.callsOf[owner], len(l.Calls))
	l.Calls = append(l.Calls, c)
}

// unfinished says a call's usage is unavailable because it never finished,
// as assembly marks it.
func unfinished(n *sessionview.Node) bool {
	var a struct {
		Usage string `json:"usage"`
	}
	return json.Unmarshal(n.Attrs, &a) == nil && a.Usage == "unavailable"
}

// readStarted counts the input side of the calls that never finished.
//
// Claude Code writes a response's usage onto each fragment of it as the
// response streams. The input side, what the call read and what it wrote to
// the cache, is known when the response starts. The output grows until the
// response ends, and only a finished call says how much. Over the local
// conversations, 56,919 finished calls had several fragments, and the input
// side was the same on every fragment of every one. A call whose final
// fragment was never written, as happens inside a child agent's transcript,
// therefore has a known input side and an unknown output, and is counted that
// way. This is Claude Code's behavior, so it applies to its dialect only.
func (l *Ledger) readStarted(read RecordsReaderChecked) error {
	if read == nil || l.Dialect != "claude-code/1" {
		return nil
	}
	var refs []sessionflow.Ref
	for _, c := range l.Calls {
		refs = append(refs, c.refs...)
	}
	if len(refs) == 0 {
		return nil
	}
	recs, err := read(refs)
	if err != nil {
		return err
	}
	for i := range l.Calls {
		c := &l.Calls[i]
		for _, r := range c.refs {
			rec := recs[[2]uint64{r.Seq, r.Row}]
			if rec == nil || rec.Usage == nil {
				continue
			}
			in := &sessiondata.Usage{Input: rec.Usage.Input, CacheRead: rec.Usage.CacheRead, CacheWrite: rec.Usage.CacheWrite}
			if in.Input < 0 || in.CacheRead < 0 || in.CacheWrite < 0 {
				break
			}
			c.Usage, c.OutputUnknown = in, true
			// The view names a call's model with its final usage, which an
			// unfinished call never wrote; the fragment names it, so the
			// call is priced at its model's rate.
			if c.Model == "" {
				c.Model = rec.Model
			}
			break
		}
		c.refs = nil
	}
	return nil
}

// price prices a call whose usage is known, by its own model.
func (l *Ledger) price(c *Call, pricing *Pricing) {
	if c.Usage == nil {
		return
	}
	if p := pricing.Lookup(c.Model, c.At); p != nil {
		c.Price = p
		c.Money = money(*c.Usage, p.PerMillion)
		l.notePrice(*p)
	}
}

func (l *Ledger) notePrice(p sessionjudge.Price) {
	for _, q := range l.Prices {
		if q.Model == p.Model && q.Key == p.Key && q.From == p.From {
			return
		}
	}
	l.Prices = append(l.Prices, p)
}

// normalize turns a call's recorded usage into four separate types.
//
// The four counts mean different things in different dialects. Anthropic's
// usage, which Claude Code records, counts input, cache reads and cache
// writes apart. LangChain's usage metadata counts every input token in its
// input, cached or not, and lists the cached part beside it, so the cached
// part is taken out once. A count below zero, recorded or left after that,
// means the counts do not add up, and the call's usage is then unknown rather
// than guessed. A call that recorded nothing is unknown too, never zero.
func normalize(dialect string, u *sessiondata.Usage) *sessiondata.Usage {
	if u == nil {
		return nil
	}
	out := *u
	if dialect == "langsmith/1" {
		out.Input = u.Input - u.CacheRead - u.CacheWrite
	}
	if out.Input < 0 || out.Output < 0 || out.CacheRead < 0 || out.CacheWrite < 0 {
		return nil
	}
	return &out
}

func money(u sessiondata.Usage, r sessionjudge.Rates) float64 {
	return (float64(u.Input)*r.In + float64(u.Output)*r.Out +
		float64(u.CacheRead)*r.CacheRead + float64(u.CacheWrite)*r.CacheWrite) / 1e6
}

// sum adds up the calls named by index.
func (l *Ledger) sum(scope, id string, calls []int) sessionjudge.Cost {
	c := sessionjudge.Cost{Scope: scope, ID: id}
	var priced float64
	anyPriced := false
	for _, i := range calls {
		call := l.Calls[i]
		c.Calls++
		widen(&c, call.At, call.At)
		if call.Usage == nil {
			c.Unknown++
			continue
		}
		if call.OutputUnknown {
			c.OutputUnknown++
		}
		add(&c.Usage, *call.Usage)
		if call.Price == nil {
			c.Unpriced++
			continue
		}
		priced += call.Money
		anyPriced = true
	}
	if anyPriced {
		c.Money, c.Currency = &priced, l.Currency
	}
	return c
}

func add(to *sessiondata.Usage, u sessiondata.Usage) {
	to.Input += u.Input
	to.Output += u.Output
	to.CacheRead += u.CacheRead
	to.CacheWrite += u.CacheWrite
}

// widen stretches a cost's time range over [from, to]; 0 is no time.
func widen(c *sessionjudge.Cost, from, to int64) {
	if from != 0 && (c.From == 0 || from < c.From) {
		c.From = from
	}
	if to != 0 && to > c.To {
		c.To = to
	}
}

// rollUp ties child streams to the main talk that started them.
//
// A child stream belongs to a talk when exactly one talk holds a step that
// starts it, followed down through children that start children of their
// own, as deep as they go. A child no call names, or a child two talks claim,
// is left unattributed: guessing would move a cost to a talk the evidence
// does not name.
func (l *Ledger) rollUp(doc *sessionview.Conversation, ownerOfNode map[string]string) {
	streamName := map[string]string{}
	for _, s := range doc.Streams {
		streamName[s.ID] = s.Name
	}
	// owners of each stream: its talks, then its loose owner.
	ownersOfStream := map[string][]string{}
	for _, c := range l.Owners {
		stream := ""
		if t := l.talks[c.ID]; t != nil {
			stream = t.Stream
		} else {
			stream = strings.TrimPrefix(c.ID, "loose/")
		}
		ownersOfStream[stream] = append(ownersOfStream[stream], c.ID)
	}
	// starters of each child stream: the owners holding a step that starts
	// it, and the earliest such step of each.
	starters := map[string]map[string]string{}
	for _, r := range doc.Relations {
		if r.Type != model.RelStarts {
			continue
		}
		name := streamName[r.To]
		if name == "" || name == "main" {
			continue
		}
		o, ok := ownerOfNode[r.From]
		if !ok {
			continue
		}
		if starters[name] == nil {
			starters[name] = map[string]string{}
		}
		if prev, ok := starters[name][o]; !ok || l.order[r.From] < l.order[prev] {
			starters[name][o] = r.From
		}
	}
	// childrenOf lists, per owner, the child streams only it starts, with
	// the step that starts each.
	type child struct{ name, step string }
	childrenOf := map[string][]child{}
	claimed := map[string]bool{}
	for name, from := range starters {
		if len(from) != 1 {
			continue
		}
		for o, step := range from {
			childrenOf[o] = append(childrenOf[o], child{name, step})
		}
	}
	for o := range childrenOf {
		sort.Slice(childrenOf[o], func(i, j int) bool { return childrenOf[o][i].name < childrenOf[o][j].name })
	}
	streamID := map[string]string{}
	role := map[string]string{}
	label := map[string]string{}
	for _, s := range doc.Streams {
		streamID[s.Name], role[s.Name], label[s.Name] = s.ID, s.Role, s.Label
	}
	streamOf := func(o string) string {
		if t := l.talks[o]; t != nil {
			return t.Stream
		}
		return strings.TrimPrefix(o, "loose/")
	}
	l.subtask = map[string]int{}
	newSubtask := func(name string) *Subtask {
		st := Subtask{ID: streamID[name], Name: name, Role: role[name], Label: label[name]}
		if st.ID == "" {
			st.ID = "stream/" + name
		}
		for _, o := range ownersOfStream[name] {
			if l.talks[o] != nil {
				st.Talks = append(st.Talks, o)
			}
			st.own = append(st.own, l.callsOf[o]...)
		}
		l.subtask[st.ID] = len(l.Subtasks)
		l.Subtasks = append(l.Subtasks, st)
		return &l.Subtasks[len(l.Subtasks)-1]
	}
	for _, t := range l.Main {
		owners := []string{t}
		visited := map[string]bool{t: true}
		at := map[string]int{}
		agents := 0
		for i := 0; i < len(owners); i++ {
			for _, ch := range childrenOf[owners[i]] {
				if claimed[ch.name] {
					continue
				}
				claimed[ch.name] = true
				agents++
				st := newSubtask(ch.name)
				st.Main, st.StartedBy, st.Parent, st.Depth = t, ch.step, t, 1
				if owners[i] != t {
					parent := &l.Subtasks[l.subtask[streamID[streamOf(owners[i])]]]
					st.Parent, st.Depth = parent.ID, parent.Depth+1
					parent.Children = append(parent.Children, st.ID)
				}
				// A child's calls take the place of the step in the main
				// talk that started it, or that started its parent.
				place := l.order[ch.step]
				if owners[i] != t {
					place = at[owners[i]]
				}
				for _, o := range ownersOfStream[ch.name] {
					if !visited[o] {
						visited[o] = true
						at[o] = place
						owners = append(owners, o)
					}
				}
			}
		}
		l.ownersOf[t], l.at[t], l.agents[t] = owners, at, agents
		var calls []int
		children := 0
		for i, o := range owners {
			calls = append(calls, l.callsOf[o]...)
			if i > 0 {
				children += len(l.callsOf[o])
			}
		}
		c := l.sum(sessionjudge.ScopeTalk, t, calls)
		c.Children = children
		for _, o := range owners {
			if tn := l.talks[o]; tn != nil {
				widen(&c, tn.From, tn.To)
			}
		}
		l.inclusive[t] = &c
	}
	// Everything outside the main talks' reach: child streams no single main
	// talk started, and their loose calls. The main stream's own loose calls
	// stay their own owner and are not counted here.
	reached := map[string]bool{LooseOwner("main"): true}
	for _, owners := range l.ownersOf {
		for _, o := range owners {
			reached[o] = true
		}
	}
	var rest []int
	for _, c := range l.Owners {
		if !reached[c.ID] {
			rest = append(rest, l.callsOf[c.ID]...)
		}
	}
	l.Unattributed = l.sum(sessionjudge.ScopeUnattributed, "", rest)
	// Every other stream is a sub-task no main talk can be shown to have
	// given, kept with the reason, so it can still be read and judged.
	var unplaced []string
	for _, s := range doc.Streams {
		if s.Name != "main" && !claimed[s.Name] {
			unplaced = append(unplaced, s.Name)
		}
	}
	sort.Strings(unplaced)
	for _, name := range unplaced {
		st := newSubtask(name)
		switch from := starters[name]; {
		case len(from) == 0:
			st.Reason = ReasonNoStarter
		case len(from) > 1:
			st.Reason = ReasonSeveral
		default:
			for o, step := range from {
				st.StartedBy, st.Reason = step, ReasonParentUnplaced
				st.Parent = streamID[streamOf(o)]
				if st.Parent == "" {
					st.Parent = "stream/" + streamOf(o)
				}
			}
		}
	}
	// A sub-task under one no step can be shown to have started is still
	// its parent's child, so the parent's cost with everything under it
	// and its nested count are whole, though neither belongs to a main talk.
	for i := range l.Subtasks {
		st := &l.Subtasks[i]
		if st.Reason != ReasonParentUnplaced {
			continue
		}
		if p, ok := l.subtask[st.Parent]; ok {
			l.Subtasks[p].Children = append(l.Subtasks[p].Children, st.ID)
		}
	}
}

// Subtask returns a sub-task by its id.
func (l *Ledger) Subtask(id string) (*Subtask, bool) {
	i, ok := l.subtask[id]
	if !ok {
		return nil, false
	}
	return &l.Subtasks[i], true
}

// SubtaskCalls returns a sub-task's own calls, or with every sub-task under
// it.
func (l *Ledger) SubtaskCalls(id string, inclusive bool) []int {
	var out []int
	seen := map[string]bool{}
	var walk func(id string)
	walk = func(id string) {
		st, ok := l.Subtask(id)
		if !ok || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, st.own...)
		if inclusive {
			for _, c := range st.Children {
				walk(c)
			}
		}
	}
	walk(id)
	return out
}

// TalkSpans are the spans of a main talk and of the talks of every sub-task
// it started, each as recorded, so their union counts only time some talk
// was working, never the gaps between them.
func (l *Ledger) TalkSpans(talk string) [][2]int64 {
	var out [][2]int64
	if t := l.talks[talk]; t != nil {
		out = append(out, [2]int64{t.From, t.To})
	}
	for i := range l.Subtasks {
		if st := &l.Subtasks[i]; st.Main == talk {
			for _, tk := range st.Talks {
				if t := l.talks[tk]; t != nil {
					out = append(out, [2]int64{t.From, t.To})
				}
			}
		}
	}
	return out
}

// Inclusive returns a main talk's cost with the child streams it started.
func (l *Ledger) Inclusive(talk string) (sessionjudge.Cost, bool) {
	c, ok := l.inclusive[talk]
	if !ok {
		return sessionjudge.Cost{}, false
	}
	return *c, true
}

// ChildAgents counts the child streams a main talk started, at any depth.
func (l *Ledger) ChildAgents(talk string) int { return l.agents[talk] }

// CallsOf returns the indexes of the calls a main talk includes: its own,
// then those of the child streams it started.
func (l *Ledger) CallsOf(talk string) []int {
	var out []int
	for _, o := range l.ownersOf[talk] {
		out = append(out, l.callsOf[o]...)
	}
	return out
}

// Place is where a call of a main talk's reach falls in that talk: its own
// place for the talk's own calls, and the place of the step that started a
// child for the child's calls.
func (l *Ledger) Place(talk string, call int) int {
	c := l.Calls[call]
	if c.Owner == talk {
		return c.Order
	}
	return l.at[talk][c.Owner]
}

// StepPlace is a step's place in its talk, as Place counts it.
func (l *Ledger) StepPlace(step string) (int, bool) {
	p, ok := l.order[step]
	return p, ok
}

// PartPlace is where a part of a talk that begins at a step begins: at the
// model call whose answer holds the step, such as the call that asked for a
// tool, since that call is the work of the part too.
func (l *Ledger) PartPlace(step string) (int, bool) {
	if c, ok := l.producedBy[step]; ok {
		return l.StepPlace(c)
	}
	return l.StepPlace(step)
}

// Sum adds up calls by index under a scope.
func (l *Ledger) Sum(scope, id string, calls []int) sessionjudge.Cost {
	return l.sum(scope, id, calls)
}

// Talk returns a talk node of the document.
func (l *Ledger) Talk(id string) *sessionview.Node { return l.talks[id] }
