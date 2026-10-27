// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements.  See the NOTICE file distributed with
// this work for additional information regarding copyright ownership.
// The ASF licenses this file to You under the Apache License, Version 2.0
// (the "License"); you may not use this file except in compliance with
// the License.  You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package judge

import (
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
)

// excerptBytes bounds an observation's excerpt. An excerpt is a quotation
// that shows a point, not a copy of the record, which the record keeps. A
// longer one is cut to its first 300 bytes, which the step holds as surely:
// on a run of 2026-10-23, an evaluator asked for at most 300 bytes gave
// excerpts of 318 bytes, and of 313 bytes in 161 characters, since it counts
// characters and a line of box drawing takes three bytes each.
const excerptBytes = 300

// observationEntry is one observation of a criterion in an achievement
// answer.
type observationEntry struct {
	Node           string `json:"node"`
	Excerpt        string `json:"excerpt"`
	Interpretation string `json:"interpretation"`
}

// saidEntry, triedEntry, remoteEntry, answerEntry and accountEntry are a
// target's account in an achievement answer. A part that is not given is
// empty: no text and no citation.
type saidEntry struct {
	Text string   `json:"text"`
	Cite []string `json:"cite"`
}

type triedEntry struct {
	What   string   `json:"what"`
	Found  string   `json:"found"`
	Limits string   `json:"limits"`
	Cite   []string `json:"cite"`
}

type remoteEntry struct {
	System    string    `json:"system"`
	Operation string    `json:"operation"`
	Via       string    `json:"via"`
	Request   saidEntry `json:"request"`
	Response  saidEntry `json:"response"`
	Confirmed saidEntry `json:"confirmed"`
}

type answerEntry struct {
	Asked    saidEntry    `json:"asked"`
	Checked  []triedEntry `json:"checked"`
	RuledOut []triedEntry `json:"ruled_out"`
	Answered saidEntry    `json:"answered"`
}

type fileGroupEntry struct {
	Area       string   `json:"area"`
	Category   string   `json:"category"`
	Summary    string   `json:"summary"`
	Operations []string `json:"operations"`
}

type accountEntry struct {
	Answer answerEntry      `json:"answer"`
	Files  []fileGroupEntry `json:"files"`
	Remote []remoteEntry    `json:"remote"`
}

func (s saidEntry) empty() bool { return s.Text == "" && len(s.Cite) == 0 }

// checkObservations checks the observations of one criterion. An
// observation is of a step the criterion cites, and its excerpt is in that
// step exactly as recorded; one that is not is left out, since its reading
// would rest on something the record does not hold. One given again, word
// for word, is left out too: on a run of 2026-10-23, an evaluator wrote one
// observation 97 times over. What the record says of the step is read by
// asz, never taken from the answer.
func checkObservations(nav *Nav, cites []sessionjudge.Cite, entries []observationEntry, left *int) []sessionjudge.Observation {
	var out []sessionjudge.Observation
	seen := map[observationEntry]bool{}
	for _, e := range entries {
		at := slices.IndexFunc(cites, func(ct sessionjudge.Cite) bool { return ct.Node == e.Node })
		e.Excerpt = clipBytes(e.Excerpt, excerptBytes)
		// No excerpt quotes nothing, which every step holds.
		if at < 0 || seen[e] || !holds(nav, e.Node, e.Excerpt) {
			*left++
			continue
		}
		seen[e] = true
		out = append(out, sessionjudge.Observation{Node: e.Node, Ref: cites[at].Ref, Excerpt: e.Excerpt,
			Status: observed(nav, e.Node), Interpretation: e.Interpretation})
	}
	return out
}

// holds says whether a step's recorded text holds an excerpt, word for word:
// its text or its result as the document gives them, or the parts of the
// records it stands on, read whole, as the record tool gives them past the
// document's clip. Whitespace is folded on both sides, and a JSON escape of
// a line break or a tab reads as one: on a run of 2026-10-23, an evaluator
// quoted a command's output aligned over several lines as one line, and
// data a record holds as JSON shows its line breaks escaped.
func holds(nav *Nav, id, excerpt string) bool {
	n := nav.Node(id)
	want := folded(excerpt)
	if strings.Contains(folded(n.Text), want) || strings.Contains(folded(n.Result), want) {
		return true
	}
	// A read that fails on a pulled snapshot is kept by the run's strict
	// reading, which fails the answer this excerpt belongs to.
	parts, _ := nav.recordedParts(n)
	return slices.ContainsFunc(parts, func(p recordedPart) bool { return strings.Contains(folded(p.text), want) })
}

// folded is text with every run of whitespace, a JSON escape of a line
// break or a tab included, made one space.
func folded(s string) string {
	s = strings.NewReplacer(`\r\n`, " ", `\n`, " ", `\r`, " ", `\t`, " ").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// clipBytes cuts text to its longest prefix of whole characters within n
// bytes.
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// observed is what the record says of a step: whether a tool step failed,
// returned or never returned, or whether the step is an error of the model
// API or an interrupt. Other steps say nothing of the kind.
func observed(nav *Nav, id string) string {
	n := nav.Node(id)
	switch {
	case n.Kind == model.KindErrorAPI:
		return sessionjudge.ObservedError
	case n.Kind == model.KindControlInterrupt:
		return sessionjudge.ObservedInterrupted
	case n.Kind != model.KindTool:
		return ""
	case n.Failed != nil && *n.Failed:
		return sessionjudge.ObservedFailed
	case shows(nav, []sessionjudge.Cite{{Node: id}}, false, nil):
		return sessionjudge.ObservedReturned
	}
	return sessionjudge.ObservedNoResult
}

// checkAccount checks a target's account. The answer of a question is a
// reply of the agent, what was asked is the person's input, and each check
// rests on a step that shows what it found. A group of files names the
// target's own operations, those of steps in its talks or their sub-tasks,
// each in one group only. An operation on another system
// rests on the tool step that sent it; its response, on a step that
// returned; and what confirmed it, on a step after it that shows something
// happened or the person's later input. asked are the inputs that set the
// target.
func checkAccount(c *checked, nav *Nav, t *sessionjudge.Target, e accountEntry, asked map[string]bool, dropped *int) *sessionjudge.Account {
	var a sessionjudge.Account
	left := 0
	tried := func(entries []triedEntry) []sessionjudge.Tried {
		var out []sessionjudge.Tried
		for _, x := range entries {
			cites := c.cites(nav, x.Cite, dropped)
			if !shows(nav, cites, false, nil) {
				left++
				continue
			}
			out = append(out, sessionjudge.Tried{What: x.What, Found: x.Found, Limits: x.Limits, Cite: cites})
		}
		return out
	}
	if ans := e.Answer; !ans.Answered.empty() || len(ans.Checked) > 0 || len(ans.RuledOut) > 0 {
		answered := c.cites(nav, ans.Answered.Cite, dropped)
		if citesKind(nav, answered, model.KindMessageAssistant) {
			var by []sessionjudge.Cite
			for _, ct := range c.cites(nav, ans.Asked.Cite, dropped) {
				if nav.person[ct.Node] {
					by = append(by, ct)
				}
			}
			a.Answer = &sessionjudge.Answer{Asked: sessionjudge.Said{Text: ans.Asked.Text, Cite: by}, Checked: tried(ans.Checked),
				RuledOut: tried(ans.RuledOut), Answered: sessionjudge.Said{Text: ans.Answered.Text, Cite: answered}}
		} else {
			c.problem("the answer of target %s cites no reply of the agent, left out", t.ID)
		}
	}
	grouped := map[string]bool{}
	strays := 0
	for _, g := range e.Files {
		var ids []string
		for _, id := range g.Operations {
			// An id that is no operation is no target's.
			if grouped[id] || !nav.owned[t.ID][id] {
				strays++
				continue
			}
			grouped[id] = true
			ids = append(ids, id)
		}
		if len(ids) == 0 {
			left++
			continue
		}
		a.Files = append(a.Files, sessionjudge.FileGroup{Area: g.Area, Category: g.Category, Summary: g.Summary, Operations: ids})
	}
	if strays > 0 {
		c.problem("%d operations in the file groups of target %s that are not its own, or named twice, were left out", strays, t.ID)
	}
	for _, x := range e.Remote {
		request := c.cites(nav, x.Request.Cite, dropped)
		sent := place(nav, request, func(n *cited) bool { return n.kind == model.KindTool }, true)
		if sent == "" {
			left++
			continue
		}
		r := sessionjudge.Remote{System: x.System, Operation: x.Operation, Via: x.Via, Request: sessionjudge.Said{Text: x.Request.Text, Cite: request}}
		if !x.Response.empty() {
			if cites := c.cites(nav, x.Response.Cite, dropped); shows(nav, cites, false, nil) {
				r.Response = &sessionjudge.Said{Text: x.Response.Text, Cite: cites}
			} else {
				left++
			}
		}
		if !x.Confirmed.empty() {
			cites := c.cites(nav, x.Confirmed.Cite, dropped)
			after := slices.DeleteFunc(slices.Clone(cites), func(ct sessionjudge.Cite) bool { return !recordedAfter(nav, ct.Node, sent) })
			if shows(nav, after, true, asked) {
				r.Confirmed = &sessionjudge.Said{Text: x.Confirmed.Text, Cite: cites}
			} else {
				left++
			}
		}
		a.Remote = append(a.Remote, r)
	}
	if left > 0 {
		c.problem("%d parts of the account of target %s that nothing they cite shows were left out", left, t.ID)
	}
	if a.Answer == nil && len(a.Files) == 0 && len(a.Remote) == 0 {
		return nil
	}
	return &a
}
