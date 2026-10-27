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
	"fmt"
	"slices"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// Window is the part of a conversation a run judges. It selects whole main
// talks: by the time they start, from From up to but not including To, or by
// their ids, from FromTalk to ToTalk, both included. A zero window is the
// whole conversation.
//
// A window selects talks rather than calls because work outlives the talk
// that started it. Measured on 0947c9ef: 39 of 50 child agents ended after
// the talk that started them, and 14 ran past the start of a later main
// talk. Cutting by each call's time would price work whose start the run
// cannot see, or show evidence whose cost it leaves out. A selected talk
// comes whole, with every sub-task it started.
type Window struct {
	From, To         time.Time
	FromTalk, ToTalk string
}

// IsZero says the window is the whole conversation.
func (w Window) IsZero() bool {
	return w.From.IsZero() && w.To.IsZero() && w.FromTalk == "" && w.ToTalk == ""
}

// Selection is what a window selected in a document.
type Selection struct {
	// Talks are the main talks the run judges, in order, and Before the
	// main talks before them, which an evaluator reads as context.
	Talks  []string
	Before []string
	// End is the last record time of the selected work, its sub-tasks
	// included, in unix milliseconds. Nothing recorded after it is shown.
	End int64
	// Untimed counts the main talks with no recorded time, which a window
	// by time never selects.
	Untimed int
}

// Header is the selection as a stage file's header records it.
func (s *Selection) Header(w Window) *sessionjudge.Window {
	if s == nil {
		return nil
	}
	h := &sessionjudge.Window{FromTalk: w.FromTalk, ToTalk: w.ToTalk, Talks: s.Talks, Before: len(s.Before), End: s.End}
	if !w.From.IsZero() {
		h.From = w.From.UTC().Format(time.RFC3339)
	}
	if !w.To.IsZero() {
		h.To = w.To.UTC().Format(time.RFC3339)
	}
	return h
}

// Narrow returns a copy of a document that holds only what a window shows:
// the main talks before the window and those in it, with the sub-tasks they
// started, and nothing recorded after the selected work ends. Loose work and
// streams no step can be shown to have started belong to whole-conversation
// runs only. Every reader of the copy, the ledger, the navigation and the
// navigation server alike, then sees the same part, and nothing outside it
// can be read or cited. A zero window returns the document as it is.
//
// A step can start before the window ends and finish after it, as a tool
// whose result comes later. read, when given, reads the records a step stands
// on, and a step keeps only those recorded by the window's end: a tool whose
// result came after it shows no result, as it showed none at that time.
func Narrow(doc *sessionview.Conversation, w Window, read RecordsReader) (*sessionview.Conversation, *Selection, error) {
	return NarrowChecked(doc, w, unchecked(read))
}

// NarrowChecked is Narrow with a reader that tells of its failures, which
// stop the narrowing.
func NarrowChecked(doc *sessionview.Conversation, w Window, read RecordsReaderChecked) (*sessionview.Conversation, *Selection, error) {
	if w.IsZero() {
		return doc, nil, nil
	}
	if (w.FromTalk != "" || w.ToTalk != "") && (!w.From.IsZero() || !w.To.IsZero()) {
		return nil, nil, fmt.Errorf("judge: a window is by time or by talk ids, not both")
	}
	l := Compute(doc, "", nil, nil)
	sel := &Selection{}
	first := -1
	switch {
	case w.FromTalk != "" || w.ToTalk != "":
		from, to := 0, len(l.Main)-1
		if w.FromTalk != "" {
			if from = slices.Index(l.Main, w.FromTalk); from < 0 {
				return nil, nil, fmt.Errorf("judge: %q is no main talk", w.FromTalk)
			}
		}
		if w.ToTalk != "" {
			if to = slices.Index(l.Main, w.ToTalk); to < 0 {
				return nil, nil, fmt.Errorf("judge: %q is no main talk", w.ToTalk)
			}
		}
		if to < from {
			return nil, nil, fmt.Errorf("judge: talk %s comes after %s", w.FromTalk, w.ToTalk)
		}
		sel.Talks, first = slices.Clone(l.Main[from:to+1]), from
	default:
		for i, id := range l.Main {
			at := l.Talk(id).At
			if at == 0 {
				sel.Untimed++
				continue
			}
			t := time.UnixMilli(at)
			if (w.From.IsZero() || !t.Before(w.From)) && (w.To.IsZero() || t.Before(w.To)) {
				if first < 0 {
					first = i
				}
				sel.Talks = append(sel.Talks, id)
			}
		}
	}
	if len(sel.Talks) == 0 {
		return nil, nil, fmt.Errorf("judge: the window selects no main talk")
	}
	sel.Before = slices.Clone(l.Main[:first])
	for _, id := range sel.Talks {
		if c, ok := l.Inclusive(id); ok && c.To > sel.End {
			sel.End = c.To
		}
	}
	// The streams shown: the main one, and those of the sub-tasks the shown
	// main talks started, at any depth.
	shown := set(append(slices.Clone(sel.Before), sel.Talks...))
	keep := map[string]bool{"main": true}
	for _, st := range l.Subtasks {
		if shown[st.Main] {
			keep[st.Name] = true
		}
	}
	out := *doc
	out.Talks, out.Loose, out.Streams, out.Relations, out.WorkspaceChanges, out.ToolExecutions = nil, nil, nil, nil, nil, nil
	ids := map[string]bool{}
	for _, s := range doc.Streams {
		if keep[s.Name] {
			out.Streams = append(out.Streams, s)
			ids[s.ID] = true
		}
	}
	for i := range doc.Talks {
		t := &doc.Talks[i]
		if t.Stream == "main" && !shown[t.ID] {
			continue
		}
		if !keep[t.Stream] {
			continue
		}
		if c, ok := until(t, sel.End); ok {
			out.Talks = append(out.Talks, c)
			index(&c, ids)
		}
	}
	for _, r := range doc.Relations {
		if ids[r.From] && ids[r.To] {
			out.Relations = append(out.Relations, r)
		}
	}
	for _, c := range doc.WorkspaceChanges {
		if ids[c.Step] {
			out.WorkspaceChanges = append(out.WorkspaceChanges, c)
		}
	}
	for _, e := range doc.ToolExecutions {
		if ids[e.Step] {
			out.ToolExecutions = append(out.ToolExecutions, e)
		}
	}
	if read != nil {
		if err := cutLater(&out, read, sel.End); err != nil {
			return nil, nil, err
		}
	}
	// A talk that ran past the window's end ends there: its span, and its
	// reply, which is the last of its messages the window shows.
	for i := range out.Talks {
		t := &out.Talks[i]
		if t.To <= sel.End {
			continue
		}
		t.To, t.Reply = sel.End, ""
		walkAll(t, func(n *sessionview.Node) {
			if n.Kind == model.KindMessageAssistant {
				t.Reply = n.Text
			}
		})
		countSteps(t)
	}
	// The summary says what the window shows, not the whole conversation,
	// counted as the view counts it: the steps of its talks.
	steps := 0
	for i := range out.Talks {
		steps += out.Talks[i].Steps
	}
	out.Summary.Talks, out.Summary.Steps, out.Summary.To = len(out.Talks), steps, sel.End
	return &out, sel, nil
}

// cutLater takes out of every step the records written after end. A tool or
// an agent call that loses any of its records loses its result, which came
// after its request; its request stays. A change or execution record written
// after end is taken out too.
func cutLater(doc *sessionview.Conversation, read RecordsReaderChecked, end int64) error {
	talks := doc.Talks
	var refs []sessionflow.Ref
	for i := range talks {
		walkAll(&talks[i], func(n *sessionview.Node) { refs = append(refs, n.Refs...) })
	}
	for _, c := range doc.WorkspaceChanges {
		refs = append(refs, c.Ref)
	}
	for _, e := range doc.ToolExecutions {
		refs = append(refs, e.Ref)
	}
	recs, err := read(refs)
	if err != nil {
		return err
	}
	late := func(r sessionflow.Ref) bool {
		rec := recs[[2]uint64{r.Seq, r.Row}]
		if rec == nil || rec.Time == "" {
			return false
		}
		t, err := time.Parse(time.RFC3339Nano, rec.Time)
		return err == nil && t.UnixMilli() > end
	}
	for i := range talks {
		walkAll(&talks[i], func(n *sessionview.Node) {
			kept := n.Refs[:0:0]
			for _, r := range n.Refs {
				if !late(r) {
					kept = append(kept, r)
				}
			}
			if len(kept) == len(n.Refs) {
				return
			}
			n.Refs = kept
			switch n.Kind {
			case model.KindTool, model.KindAgentCall:
				n.Result, n.ResultBytes, n.ResultState, n.Failed, n.RequestToResultMS = "", 0, "", nil, 0
			case model.KindLLMCall:
				// A model call still running at the end has not finished:
				// what it read is counted, as for any call that never
				// finished, and its output is unknown. What came with its
				// end, such as why it stopped, is not known yet; its model
				// is read again from the records it keeps.
				n.Usage, n.Attrs, n.Model = nil, json.RawMessage(`{"usage":"unavailable"}`), ""
				for _, r := range kept {
					if rec := recs[[2]uint64{r.Seq, r.Row}]; rec != nil && rec.Model != "" {
						n.Model = rec.Model
					}
				}
			}
		})
	}
	var changes []sessionview.WorkspaceChange
	for _, c := range doc.WorkspaceChanges {
		if !late(c.Ref) {
			changes = append(changes, c)
		}
	}
	var executions []sessionview.ToolExecution
	for _, e := range doc.ToolExecutions {
		if !late(e.Ref) {
			executions = append(executions, e)
		}
	}
	doc.WorkspaceChanges, doc.ToolExecutions = changes, executions
	// A step names only the change and execution records the window keeps.
	keep := map[string]bool{}
	for _, c := range changes {
		keep[c.ID] = true
	}
	for _, e := range executions {
		keep[e.ID] = true
	}
	for i := range talks {
		walkAll(&talks[i], func(n *sessionview.Node) {
			n.Changes = slices.DeleteFunc(slices.Clone(n.Changes), func(id string) bool { return !keep[id] })
			n.Executions = slices.DeleteFunc(slices.Clone(n.Executions), func(id string) bool { return !keep[id] })
		})
	}
	return nil
}

// countSteps counts a talk's runs, steps and tools as the view does.
func countSteps(t *sessionview.Node) {
	t.Runs, t.Steps, t.Tools = 0, 0, 0
	for i := range t.Children {
		walkAll(&t.Children[i], func(n *sessionview.Node) {
			switch n.Kind {
			case model.KindRun:
				t.Runs++
			case model.KindSession, model.KindSegment, model.KindStream, model.KindEpoch, model.KindTalk:
			default:
				t.Steps++
			}
			if n.Kind == model.KindTool || n.Kind == model.KindAgentCall {
				t.Tools++
			}
		})
	}
}

// until copies a node without what was recorded after end. A node with no
// time stays with its parent. A talk that began after end is left out
// whole.
func until(n *sessionview.Node, end int64) (sessionview.Node, bool) {
	if n.At != 0 && n.At > end {
		return sessionview.Node{}, false
	}
	c := *n
	c.Children = nil
	for i := range n.Children {
		if k, ok := until(&n.Children[i], end); ok {
			c.Children = append(c.Children, k)
		}
	}
	return c, true
}

func index(n *sessionview.Node, ids map[string]bool) {
	ids[n.ID] = true
	for i := range n.Children {
		index(&n.Children[i], ids)
	}
}
