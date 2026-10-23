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
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
)

// SourceDoc is a saved source a call reads through the source tool.
type SourceDoc struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Text   string `json:"text"`
}

// sourceLineBytes bounds the text of one row of the source tool.
const sourceLineBytes = 2000

// sourceRows are a source's lines from line from on, numbered from 1. A line
// longer than sourceLineBytes comes in parts, numbered from 1, so all of it
// can be read: a ticket exported as one line of JSON holds everything on
// that line.
func (n *Nav) sourceRows(id string, from int) ([]any, error) {
	for _, d := range n.assigned.Sources {
		if d.ID != id {
			continue
		}
		from = max(from, 1)
		var rows []any
		for i, l := range strings.Split(strings.TrimSuffix(d.Text, "\n"), "\n") {
			switch {
			case i+1 < from:
			case len(l) <= sourceLineBytes:
				rows = append(rows, map[string]any{"line": i + 1, "text": l})
			default:
				for part := 1; l != ""; part++ {
					piece := clipBytes(l, sourceLineBytes)
					rows = append(rows, map[string]any{"line": i + 1, "part": part, "text": piece})
					l = l[len(piece):]
				}
			}
		}
		return rows, nil
	}
	return nil, fmt.Errorf("no source %q in what this call was given", id)
}

// sourceList is the sources a call was given, a row each: its id, where it
// came from and how many lines it has, so a call whose prompt was cut short
// still finds every one.
func (n *Nav) sourceList() []any {
	var rows []any
	for _, d := range n.assigned.Sources {
		rows = append(rows, map[string]any{"id": d.ID, "source": d.Source, "lines": strings.Count(strings.TrimSuffix(d.Text, "\n"), "\n") + 1})
	}
	return rows
}

// conclusionRows are some conclusions, a row each, whole.
func conclusionRows(cs []sessionjudge.Conclusion) []any {
	var rows []any
	for _, c := range cs {
		c.T = ""
		rows = append(rows, c)
	}
	return rows
}

// sourceItems are what a reference stage answers for: each saved source,
// a row of its manifest each.
func sourceItems(docs []SourceDoc, manifests []sessionjudge.Source) []item {
	var out []item
	for i, m := range manifests {
		if m.Outcome != sessionjudge.SourceSaved {
			continue
		}
		row := map[string]any{"id": m.ID, "source": m.Source, "content_type": m.ContentType, "lines": m.Lines, "cut": m.Cut,
			"revision": m.Revision, "published": m.Published, "author": m.Author}
		b, _ := json.Marshal(row)
		out = append(out, item{id: m.ID, source: &docs[i], rows: []any{row}, bytes: len(b) + 1})
	}
	return out
}

// conclusionItems are what a divergence stage answers for: each conclusion
// a source holds.
func conclusionItems(cs []sessionjudge.Conclusion) []item {
	var out []item
	for i := range cs {
		rows := conclusionRows(cs[i : i+1])
		b, _ := json.Marshal(rows[0])
		out = append(out, item{id: cs[i].ID, conclusion: &cs[i], rows: rows, bytes: len(b) + 1})
	}
	return out
}

// checkReference checks a reference answer: the conclusions each source the
// call was given holds. A conclusion keeps an id of its own, unique, as a
// person's structured file gives them; its kind is final or intermediate;
// what it expects is required or explanatory, explanatory when unclear,
// since that changes no score; its support is saved only when it cites
// lines of the source, and asserted when unclear. Its scope names inputs of
// the conversation, those it does not hold kept apart as unbound. Cited
// lines are lines of the source; builds_on names earlier conclusions, and
// one that would close a cycle is left out.
func checkReference(nav *Nav, raw json.RawMessage) (*checked, error) {
	var a struct {
		Conclusions []struct {
			ID          string   `json:"id"`
			Source      string   `json:"source"`
			Kind        string   `json:"kind"`
			Statement   string   `json:"statement"`
			Expectation string   `json:"expectation"`
			Scope       []string `json:"scope"`
			Context     []struct {
				Source string `json:"source"`
				Lines  []int  `json:"lines"`
				Note   string `json:"note"`
			} `json:"context"`
			BuildsOn     []string `json:"builds_on"`
			Support      string   `json:"support"`
			Author       string   `json:"author"`
			Revision     string   `json:"revision"`
			Environment  string   `json:"environment"`
			Conflicts    []string `json:"conflicts"`
			SupersededBy string   `json:"superseded_by"`
			Ambiguity    string   `json:"ambiguity"`
			NotCovered   string   `json:"not_covered"`
		} `json:"conclusions"`
	}
	if err := decodeAnswer(raw, "conclusions", &a); err != nil {
		return nil, err
	}
	c := &checked{}
	lines := map[string]int{}
	for _, d := range nav.assigned.Sources {
		lines[d.ID] = strings.Count(strings.TrimSuffix(d.Text, "\n"), "\n") + 1
	}
	inputs := set(nav.Inputs())
	var out []*sessionjudge.Conclusion
	ids := map[string]bool{}
	left, badLines, weakened := 0, 0, 0
	for _, x := range a.Conclusions {
		id := strings.TrimSpace(x.ID)
		if id == "" || ids[id] || lines[x.Source] == 0 || strings.TrimSpace(x.Statement) == "" ||
			(x.Kind != sessionjudge.ConclusionFinal && x.Kind != sessionjudge.ConclusionIntermediate) {
			left++
			continue
		}
		ids[id] = true
		k := &sessionjudge.Conclusion{ID: id, Source: x.Source, Kind: x.Kind, Statement: x.Statement, Expectation: x.Expectation,
			Scope: []string{}, Support: x.Support, Author: x.Author, Revision: x.Revision, Environment: x.Environment,
			Ambiguity: x.Ambiguity, NotCovered: x.NotCovered}
		if k.Expectation != sessionjudge.ExpectRequired && k.Expectation != sessionjudge.ExpectExplanatory {
			weakened++
			k.Expectation = sessionjudge.ExpectExplanatory
		}
		for _, node := range x.Scope {
			switch {
			case inputs[node] && !slices.Contains(k.Scope, node):
				k.Scope = append(k.Scope, node)
			case !inputs[node]:
				k.Unbound = append(k.Unbound, node)
			}
		}
		for _, ct := range x.Context {
			n := lines[ct.Source]
			if len(ct.Lines) != 2 || ct.Lines[0] < 1 || ct.Lines[0] > ct.Lines[1] || ct.Lines[1] > n {
				badLines++
				continue
			}
			k.Context = append(k.Context, sessionjudge.SourceCite{Source: ct.Source, Lines: [2]int{ct.Lines[0], ct.Lines[1]}, Note: ct.Note})
		}
		switch {
		case k.Support == sessionjudge.SupportSaved && len(k.Context) == 0,
			k.Support != sessionjudge.SupportSaved && k.Support != sessionjudge.SupportLinked && k.Support != sessionjudge.SupportAsserted:
			weakened++
			k.Support = sessionjudge.SupportAsserted
		}
		k.BuildsOn, k.Conflicts, k.SupersededBy = x.BuildsOn, x.Conflicts, strings.TrimSpace(x.SupersededBy)
		out = append(out, k)
	}
	// What a conclusion builds on and conflicts with are conclusions of the
	// answer; a builds_on that would close a cycle is left out.
	unknown := 0
	known := func(ref string, self string) bool {
		ok := ids[ref] && ref != self
		if !ok {
			unknown++
		}
		return ok
	}
	// A builds_on is checked against those accepted before it, so the one
	// that would close a cycle, in the answer's order, is left out.
	accepted := map[string][]string{}
	reaches := func(from, to string) bool {
		seen := map[string]bool{}
		var walk func(id string) bool
		walk = func(id string) bool {
			if id == to {
				return true
			}
			if seen[id] {
				return false
			}
			seen[id] = true
			return slices.ContainsFunc(accepted[id], walk)
		}
		return walk(from)
	}
	for _, k := range out {
		var on []string
		for _, ref := range k.BuildsOn {
			if !known(ref, k.ID) || slices.Contains(on, ref) {
				continue
			}
			if reaches(ref, k.ID) {
				unknown++
				continue
			}
			on = append(on, ref)
			accepted[k.ID] = append(accepted[k.ID], ref)
		}
		k.BuildsOn = on
		var with []string
		for _, ref := range k.Conflicts {
			if known(ref, k.ID) && !slices.Contains(with, ref) {
				with = append(with, ref)
			}
		}
		k.Conflicts = with
		c.frames = append(c.frames, k)
	}
	// A conclusion is superseded by another of the same source; one that
	// would close a chain of corrections, in the answer's order, is not.
	source := map[string]string{}
	for _, k := range out {
		source[k.ID] = k.Source
	}
	corrected := map[string]string{}
	for _, k := range out {
		by := k.SupersededBy
		if by == "" {
			continue
		}
		at := by
		for at != "" && at != k.ID {
			at = corrected[at]
		}
		// An id of no conclusion has no source, and naming itself closes
		// a chain.
		if source[by] != k.Source || at == k.ID {
			unknown++
			k.SupersededBy = ""
			continue
		}
		corrected[k.ID] = by
	}
	if left > 0 {
		c.problem("%d conclusions with no unique id, no statement, a kind asz does not know, or no source of this call were left out", left)
	}
	if badLines > 0 {
		c.problem("%d citations of lines the source does not have were left out", badLines)
	}
	if weakened > 0 {
		c.problem("%d conclusions with an expectation or a support asz cannot take were read as explanatory or asserted", weakened)
	}
	if unknown > 0 {
		c.problem("%d references to no other conclusion, to a correction from another source, or ones that would close a cycle, were left out", unknown)
	}
	return c, nil
}
