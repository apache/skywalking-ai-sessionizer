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
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// Receipt is what one answer gave the evaluator of the conversation's steps:
// the steps it asked for itself and was answered with something, and the
// steps a row of it named. It says nothing of whose text an answer held: a
// row can show text its step does not own.
type Receipt struct {
	Opened []string `json:"opened,omitempty"`
	Listed []string `json:"listed,omitempty"`
}

// receipts gathers the receipts of what one evaluator call was given. lost
// says why they cannot be relied on, when they cannot; a reading over them
// is then unavailable, never a smaller count.
type receipts struct {
	mu     sync.Mutex
	opened map[string]bool
	listed map[string]bool
	lost   string
}

func newReceipts() *receipts {
	return &receipts{opened: map[string]bool{}, listed: map[string]bool{}}
}

// owner is a step that stands on a record: on one block of it, or on every
// part when block is nil.
type owner struct {
	step  string
	block *int
}

// ownersOf indexes the steps by the records they stand on, so a part read by
// its address credits the step that owns it, and only that step.
func ownersOf(nodes map[string]*sessionview.Node) map[[2]uint64][]owner {
	out := map[[2]uint64][]owner{}
	for id, x := range nodes {
		if !isStep(x) {
			continue
		}
		refs := x.Refs
		if len(refs) == 0 && x.Ref != nil {
			refs = append(refs, *x.Ref)
		}
		for _, r := range refs {
			key := [2]uint64{r.Seq, r.Row}
			out[key] = append(out[key], owner{step: id, block: r.Block})
		}
	}
	return out
}

// isStep says whether a node is a step: what a person reads, never a
// container of steps.
func isStep(x *sessionview.Node) bool {
	return x != nil && !slices.Contains(containers, x.Kind)
}

// Answer answers one navigation call as Call does, with the receipt of what
// the answer gave. The receipt counts only once the answer was given: the
// caller delivers it then.
func (n *Nav) Answer(name string, args json.RawMessage) (any, Receipt, error) {
	out, a, err := n.answerCall(name, args)
	if err != nil || out == nil {
		return out, Receipt{}, err
	}
	return out, n.receiptOf(name, a, out), nil
}

// Deliver counts a receipt once its answer was given to the evaluator.
func (n *Nav) Deliver(r Receipt) {
	g := n.given
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, id := range r.Opened {
		g.opened[id] = true
	}
	for _, id := range r.Listed {
		g.listed[id] = true
	}
}

// DeliverRows counts the rows a call's prompt held as it was sent.
func (n *Nav) DeliverRows(rows []any) {
	var r Receipt
	for _, row := range rows {
		r.Listed = append(r.Listed, n.rowSteps(row)...)
	}
	n.Deliver(r)
}

// LoseReceipts says the receipts of this call cannot be relied on, and why.
func (n *Nav) LoseReceipts(reason string) {
	g := n.given
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lost == "" {
		g.lost = reason
	}
}

// receiptOf is the receipt of one answer, taken from what it returned. A
// step is opened when the evaluator asked for it and the answer was not
// empty: its step answer with a text or a result, a slice of a part it owns,
// or a match of a search in it. A step is listed when a row it was given
// names it: a row of a page, or the slice of a row read by its number. A
// record's JSON in slices is no request for one step.
func (n *Nav) receiptOf(name string, a toolArgs, out any) Receipt {
	var r Receipt
	switch name {
	case "step":
		answer, ok := out.(map[string]any)
		if !ok {
			return r
		}
		// An answer that shrank to addresses and a note has no "step".
		if x, ok := answer["step"].(*sessionview.Node); ok && (x.Text != "" || x.Result != "") {
			r.Opened = n.steps(a.ID)
		}
	case "record":
		answer, ok := out.(map[string]any)
		if !ok || a.Part == nil {
			return r
		}
		if slice, _ := answer["slice"].(string); slice != "" {
			for _, o := range n.owners[[2]uint64{a.Seq, a.Row}] {
				if o.block == nil || *o.block == *a.Part {
					r.Opened = append(r.Opened, o.step)
				}
			}
		}
	case "search", "inputs", "talks", "subtasks", "talk":
		rows, slice := n.givenRows(name, a, out)
		if name == "search" && a.Node != "" {
			if slices.ContainsFunc(rows, func(row any) bool { _, hit := row.(hit); return hit }) {
				r.Opened = n.steps(a.Node)
			}
			return r
		}
		for _, row := range rows {
			for _, id := range n.rowSteps(row) {
				if quoted, _ := json.Marshal(id); slice == "" || strings.Contains(slice, string(quoted)) {
					r.Listed = append(r.Listed, id)
				}
			}
		}
	}
	return r
}

// givenRows are the rows an answer gave: a page's rows, or the row an item
// answer read, with the slice of it that was given. An item can be asked for
// with no page before it, and a slice from an offset can hold the rest of a
// row without its ids: only the ids the slice holds are named.
func (n *Nav) givenRows(name string, a toolArgs, out any) (rows []any, slice string) {
	switch v := out.(type) {
	case page:
		return v.Rows, ""
	case map[string]any:
		slice, _ = v["slice"].(string)
		all, _, err := n.rows(name, a)
		if slice == "" || err != nil || a.Item < 1 || a.Item > len(all) {
			return nil, ""
		}
		return all[a.Item-1 : a.Item], slice
	}
	return nil, ""
}

// rowSteps are the steps one row names: an input row its input, a talk row
// the steps that hold its input and its reply, a sub-task row the step that
// started it and the one that holds its report, an outline line its step, a
// search match its step, and a row too large for a page what its address
// keys name. A row of a talk or a sub-task never names the steps inside it.
func (n *Nav) rowSteps(row any) []string {
	switch r := row.(type) {
	case inputRow:
		return n.steps(r.Node)
	case talkRow:
		return n.steps(r.InputNode, r.ReplyNode)
	case subtaskRow:
		return n.steps(r.StartedBy, r.ReportNode)
	case outlineRow:
		return n.steps(r.ID)
	case hit:
		return n.steps(r.Node)
	case oversized:
		id, _ := r.Keys["id"].(string)
		node, _ := r.Keys["node"].(string)
		return n.steps(id, node)
	}
	return nil
}

// steps keeps the ids that name a step of the document.
func (n *Nav) steps(ids ...string) []string {
	var out []string
	for _, id := range ids {
		if id != "" && isStep(n.nodes[id]) {
			out = append(out, id)
		}
	}
	return out
}

// logLine is one line of a navigation server's log: an answer it gave, with
// its receipt, or the closing line it writes when its input ends.
type logLine struct {
	Attempt string          `json:"attempt"`
	Tool    string          `json:"tool"`
	Args    json.RawMessage `json:"args"`
	Bytes   int             `json:"bytes"`
	Error   bool            `json:"error"`
	Refused bool            `json:"refused"`
	Opened  []string        `json:"opened"`
	Listed  []string        `json:"listed"`
	Closing bool            `json:"closing"`
	// Answers is required on the closing line: one without it is no count.
	Answers     *int `json:"answers"`
	WriteFailed bool `json:"write_failed"`
}

// TakeLog delivers the receipts an agent CLI's navigation server logged for
// the attempt. They count only when every line reads and carries the
// attempt's identity, and the closing line is there with a count that
// matches: a log the server did not finish, or one of another attempt, makes
// the receipts unavailable, never fewer.
func (n *Nav) TakeLog(path, attempt string) {
	b, err := os.ReadFile(path)
	if err != nil {
		n.LoseReceipts(fmt.Sprintf("the navigation log could not be read: %v", err))
		return
	}
	var answers []logLine
	closed := false
	for _, raw := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var l logLine
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		switch {
		case dec.Decode(&l) != nil || !ended(dec):
			n.LoseReceipts("a line of the navigation log does not read")
			return
		case l.Attempt != attempt:
			n.LoseReceipts("a line of the navigation log is not this attempt's")
			return
		case closed:
			n.LoseReceipts("the navigation log goes on after its closing line")
			return
		case l.Closing:
			closed = true
			if l.WriteFailed {
				n.LoseReceipts("a write to the navigation log failed")
				return
			}
			if l.Answers == nil {
				n.LoseReceipts("the navigation log's closing line gives no count")
				return
			}
			if *l.Answers != len(answers) {
				n.LoseReceipts(fmt.Sprintf("the navigation log closes on %d answers and holds %d", *l.Answers, len(answers)))
				return
			}
		default:
			answers = append(answers, l)
		}
	}
	if !closed {
		n.LoseReceipts("the navigation log has no closing line: its server did not end as it should")
		return
	}
	for _, l := range answers {
		n.Deliver(Receipt{Opened: l.Opened, Listed: l.Listed})
	}
}

// ended says whether a decoder has read all of its input: a line of the log
// is one object and nothing after it.
func ended(dec *json.Decoder) bool {
	_, err := dec.Token()
	return err == io.EOF
}

// GivenWindow is how much of the judged work the call was given: every step
// of the main talks a window run judges, with every sub-task they started at
// any depth, or of every talk and loose work for a run over the whole
// conversation.
func (n *Nav) GivenWindow() *sessionjudge.Given {
	var talks map[string]bool
	if n.window != nil {
		talks = map[string]bool{}
		for tk := range n.judged {
			talks[tk] = true
		}
		for i := range n.ledger.Subtasks {
			if st := &n.ledger.Subtasks[i]; n.judged[st.Main] {
				for _, tk := range st.Talks {
					talks[tk] = true
				}
			}
		}
	}
	var steps []string
	for id, x := range n.nodes {
		if !isStep(x) {
			continue
		}
		if talks == nil || talks[n.talkOf[id]] {
			steps = append(steps, id)
		}
	}
	return n.givenOver(sessionjudge.GivenWindow, steps)
}

// GivenSpan is how much of a span the call was given: every step of one
// stream from from to at, both included, in stream order (talk by talk, then
// step by step), with every step of the sub-tasks a step of the span
// started, at any depth. A span asz cannot order is unavailable, with why;
// valid says it could, whether or not the receipts held, so a valid start is
// kept when the reading over it is lost.
func (n *Nav) GivenSpan(from, at string) (g *sessionjudge.Given, valid bool) {
	span := func(reason string) (*sessionjudge.Given, bool) {
		return &sessionjudge.Given{Scope: sessionjudge.GivenSpan, Reason: reason}, false
	}
	if from == "" {
		return span("no step where the question or the assumption arose was given")
	}
	ft, at0 := n.talkOf[from], n.talkOf[at]
	if !isStep(n.nodes[from]) || ft == "" {
		return span(fmt.Sprintf("%s is not a step of a talk", from))
	}
	stream := n.nodes[ft].Stream
	if at0 == "" || n.nodes[at0].Stream != stream {
		return span(fmt.Sprintf("%s and %s are not in one stream", from, at))
	}
	var order []string
	for _, tk := range n.streamTalks[stream] {
		walkAll(n.nodes[tk], func(x *sessionview.Node) {
			if isStep(x) {
				order = append(order, x.ID)
			}
		})
	}
	i, j := slices.Index(order, from), slices.Index(order, at)
	if i < 0 || j < 0 || i > j {
		return span(fmt.Sprintf("%s does not come before %s in its stream", from, at))
	}
	steps := slices.Clone(order[i : j+1])
	in := set(steps)
	seen := map[string]bool{}
	var under func(id string)
	under = func(id string) {
		st, ok := n.ledger.Subtask(id)
		if !ok || seen[id] {
			return
		}
		seen[id] = true
		for _, tk := range st.Talks {
			walkAll(n.nodes[tk], func(x *sessionview.Node) {
				if isStep(x) {
					steps = append(steps, x.ID)
				}
			})
		}
		for _, c := range st.Children {
			under(c)
		}
	}
	for i := range n.ledger.Subtasks {
		if st := &n.ledger.Subtasks[i]; in[st.StartedBy] {
			under(st.ID)
		}
	}
	return n.givenOver(sessionjudge.GivenSpan, steps), true
}

// givenOver counts what the call was given of some steps. A step counts
// once: opened before listed, listed before none.
func (n *Nav) givenOver(scope string, steps []string) *sessionjudge.Given {
	g := n.given
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lost != "" {
		return &sessionjudge.Given{Scope: scope, Reason: g.lost}
	}
	c := &sessionjudge.GivenCounts{}
	done := map[string]bool{}
	for _, id := range steps {
		if done[id] {
			continue
		}
		done[id] = true
		c.Steps++
		switch {
		case g.opened[id]:
			c.Opened++
		case g.listed[id]:
			c.Listed++
		default:
			c.None++
		}
		if n.missing(id) {
			c.Unavailable++
		}
	}
	return &sessionjudge.Given{Scope: scope, Counts: c}
}

// missing says whether some of a step's evidence cannot be given by anyone:
// its text or result left out, cut, withheld, too large to land or removed,
// a part the conversion dropped, or an image, which no evaluator here reads.
func (n *Nav) missing(id string) bool {
	x := n.nodes[id]
	gone := func(state string) bool { return state != "" && state != model.ContentAvailable }
	if gone(x.State) || gone(x.ResultState) || len(x.Dropped) > 0 {
		return true
	}
	return n.imaged[id] || slices.ContainsFunc(n.inputs, func(r inputRow) bool { return r.Node == id && r.Images > 0 })
}

// MarkResultImages marks the steps whose landed record carried an image: a
// tool's result, as a screenshot is, or any other record the model did not
// write. The view keeps their words only. Only a run counts what its
// evaluators were given, so only a run reads these. What the model wrote
// carries no image and is not read: its messages, its thinking, its calls,
// a history change card, which stands at its call's record, and a tool's or
// agent's request. Records are read a landed file at a time, so no more than
// one file's records are held at once.
func (n *Nav) MarkResultImages(read RecordsReaderChecked) error {
	if read == nil {
		return nil
	}
	held := map[uint64]map[uint64][]owner{}
	for id, x := range n.nodes {
		if !isStep(x) || slices.Contains(modelWritten, x.Kind) {
			continue
		}
		refs := x.Refs
		if len(refs) == 0 && x.Ref != nil {
			refs = []sessionflow.Ref{*x.Ref}
		}
		if x.Kind == model.KindTool || x.Kind == model.KindAgentCall {
			// The first is the request; a call with one has no result yet.
			refs = refs[min(1, len(refs)):]
		}
		for _, r := range refs {
			if held[r.Seq] == nil {
				held[r.Seq] = map[uint64][]owner{}
			}
			held[r.Seq][r.Row] = append(held[r.Seq][r.Row], owner{step: id, block: r.Block})
		}
	}
	n.imaged = map[string]bool{}
	for _, seq := range slices.Sorted(maps.Keys(held)) {
		var refs []sessionflow.Ref
		for row := range held[seq] {
			refs = append(refs, sessionflow.Ref{Seq: seq, Row: row})
		}
		recs, err := read(refs)
		if err != nil {
			return err
		}
		for row, owners := range held[seq] {
			rec := recs[[2]uint64{seq, row}]
			if rec == nil {
				continue
			}
			for _, o := range owners {
				if imagesIn(rec, o.block) > 0 {
					n.imaged[o.step] = true
				}
			}
		}
	}
	return nil
}

// modelWritten are the kinds of step whose records the model wrote.
var modelWritten = []string{model.KindMessageAssistant, model.KindThinking, model.KindLLMCall, model.KindHistoryChange}

// imagesIn counts the images in the part of a record a block names, or in
// every part when it names none, by the rule imagesOf counts an input's.
func imagesIn(rec *sessiondata.Record, block *int) int {
	if block == nil {
		count, _ := imagesOf(rec)
		return count
	}
	if *block < 0 || *block >= len(rec.Parts) {
		return 0
	}
	count, _ := imagesOf(&sessiondata.Record{Parts: rec.Parts[*block : *block+1]})
	return count
}
