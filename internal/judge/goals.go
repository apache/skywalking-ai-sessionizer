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
	"fmt"
	"slices"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
)

// catalogue is what the parts of a targets scan found before the current
// one: the targets, the goals, the relations of the inputs already answered,
// and the last input related to each goal. A later part reads them to update
// a target or a goal by its id, and its relations follow earlier inputs of a
// goal.
type catalogue struct {
	targets   []sessionjudge.Target
	goals     []sessionjudge.Goal
	relations map[string][]sessionjudge.Relation
	latest    map[string]string
}

// goalEntry is one goal of a targets answer: a new goal under a key of the
// evaluator's own, or a change to a goal found so far, by its id.
type goalEntry struct {
	ID         string   `json:"id"`
	Key        string   `json:"key"`
	Statement  string   `json:"statement"`
	Members    []string `json:"members"`
	Main       string   `json:"main"`
	MainReason string   `json:"main_reason"`
	MainCite   []string `json:"main_cite"`
	Cite       []string `json:"cite"`
}

// undetermined is the main target an answer gives when no member can be
// shown to be the one the goal is for.
const undetermined = "undetermined"

// checkGoals checks the goals and relations of a targets answer against the
// targets it settled. Goals group targets that serve one result the person
// asked for; relations say how each input, or each request in it, relates to
// the inputs before it. A relation with nothing valid behind it falls back to
// unclear, the relation that claims nothing, and a target whose goal cannot
// be shown stays in no goal.
//
// assigned are the inputs this call answers for, actions their accepted
// actions, and resolve names a target by its id or key.
func checkGoals(c *checked, nav *Nav, cat catalogue, entries []goalEntry, rels map[string][]sessionjudge.Relation,
	assigned []string, actions map[string][]sessionjudge.Action, targets []sessionjudge.Target, resolve func(string) (string, bool)) {
	order := map[string]int{}
	inputs := nav.Inputs()
	for i, in := range inputs {
		order[in] = i
	}
	goals := make([]sessionjudge.Goal, len(cat.goals))
	known := map[string]int{}
	for i, g := range cat.goals {
		g.Members, g.Evolution, g.Cite, g.MainCite = slices.Clone(g.Members), slices.Clone(g.Evolution), slices.Clone(g.Cite), slices.Clone(g.MainCite)
		goals[i] = g
		known[g.ID] = i
	}
	given := map[string]bool{}
	for i := range entries {
		e := &entries[i]
		// An id no earlier part found is the evaluator's name for a new
		// goal, as with targets.
		if _, ok := known[e.ID]; e.ID != "" && e.Key == "" && !ok {
			e.Key, e.ID = e.ID, ""
		}
		switch {
		case e.ID != "" && e.Key != "":
			c.problem("a goal entry names both goal %s and key %s, left out", e.ID, e.Key)
			e.ID, e.Key = "", ""
		case e.Key != "":
			if _, taken := known[e.Key]; taken {
				c.problem("goal key %s is the id of a goal found so far; the new goal set under it was left out", e.Key)
				e.Key = ""
			} else if given[e.Key] {
				c.problem("goal key %s was given twice; the first was kept", e.Key)
				e.Key = ""
			} else {
				given[e.Key] = true
			}
		}
	}
	// Any other name a relation gives is a key for a goal it may start.
	// Until the relations are settled a key stands for its id: a key is
	// never the id of a goal found so far, so the two cannot be mistaken
	// for each other.
	keys := map[string]bool{}
	for _, node := range assigned {
		for _, r := range rels[node] {
			key := strings.TrimSpace(r.Goal)
			if _, taken := known[key]; key != "" && !taken {
				keys[key] = true
			}
		}
	}
	answered := slices.Clone(assigned)
	slices.SortFunc(answered, func(a, b string) int { return order[a] - order[b] })

	// Relations, in the order the inputs were recorded, so a relation can
	// follow an earlier input of this answer as well as one found before.
	// The parts of a scan run in that order too, so only an earlier input
	// can have acted on a goal when a relation is checked.
	settled := map[string][]sessionjudge.Relation{}
	for node, rs := range cat.relations {
		settled[node] = rs
	}
	actsOn := func(node, goal string) bool {
		return slices.ContainsFunc(settled[node], func(r sessionjudge.Relation) bool { return r.Goal == goal })
	}
	// unclear is the relation that claims nothing, kept for the request it
	// concerns.
	unclear := func(r sessionjudge.Relation) sessionjudge.Relation {
		return sessionjudge.Relation{Kind: sessionjudge.RelationUnclear, Request: r.Request}
	}
	started := map[string]bool{}
	weakened, empty := 0, 0
	for _, node := range answered {
		if slices.ContainsFunc(actions[node], func(a sessionjudge.Action) bool { return a.Action == sessionjudge.ActionNotPerson }) {
			// Who wrote an input is settled by its actions, which the rest
			// of the judge reads; a relation that disagrees gives way.
			if len(rels[node]) != 1 || rels[node][0].Kind != sessionjudge.RelationNotPerson {
				weakened++
			}
			settled[node] = []sessionjudge.Relation{{Kind: sessionjudge.RelationNotPerson}}
			continue
		}
		var out []sessionjudge.Relation
		for _, r := range rels[node] {
			r.Goal = strings.TrimSpace(r.Goal)
			valid := true
			switch r.Kind {
			case sessionjudge.RelationUnclear:
				r = unclear(r)
			case sessionjudge.RelationNew:
				// A new goal is a key no earlier input started. A goal found
				// so far has an id, never a key.
				if valid = keys[r.Goal] && !started[r.Goal]; valid {
					started[r.Goal], r.Follows = true, ""
				}
			case sessionjudge.RelationContinue, sessionjudge.RelationAdjust, sessionjudge.RelationReturn:
				// An unclear input relates to no goal, so the relation must
				// name one that the input it follows relates to.
				valid = r.Goal != "" && actsOn(r.Follows, r.Goal)
				// A return is to a goal the input just before did not act
				// on; when it did, the input goes on with that goal. The
				// relation follows an earlier input, so there is one before.
				if valid && r.Kind == sessionjudge.RelationReturn && actsOn(inputs[order[node]-1], r.Goal) {
					weakened++
					r.Kind = sessionjudge.RelationContinue
				}
			default:
				// Not from the person, which the actions deny, or no kind
				// asz knows.
				valid = false
			}
			if !valid {
				weakened++
				r = unclear(r)
			}
			// A statement is the goal as an input sets or adjusts it.
			if r.Kind != sessionjudge.RelationNew && r.Kind != sessionjudge.RelationAdjust {
				r.Statement = ""
			}
			out = append(out, r)
		}
		if len(out) == 0 {
			// An input the answer left out is reported as not answered.
			if _, answered := rels[node]; answered {
				empty++
			}
			out = []sessionjudge.Relation{unclear(sessionjudge.Relation{})}
		}
		settled[node] = out
	}
	if weakened > 0 {
		c.problem("%d relations without a valid goal or earlier input were made unclear", weakened)
	}
	if empty > 0 {
		c.problem("%d inputs had no relation and are unclear", empty)
	}
	// A new goal gets the next free id in the order the inputs started it,
	// so ids do not depend on the evaluator's keys. Only a new relation can
	// be the first valid one on a key, since the others follow an earlier
	// input of the goal; a key no relation started names no goal.
	idOf := map[string]string{}
	for _, node := range answered {
		for i, r := range settled[node] {
			if !keys[r.Goal] {
				continue
			}
			if _, named := idOf[r.Goal]; !named {
				idOf[r.Goal] = fmt.Sprintf("g%d", len(goals)+1)
				goals = append(goals, sessionjudge.Goal{ID: idOf[r.Goal], Members: []string{}, Evolution: []sessionjudge.Evolution{}, Cite: []sessionjudge.Cite{}})
				known[idOf[r.Goal]] = len(goals) - 1
			}
			settled[node][i].Goal = idOf[r.Goal]
		}
	}
	// A goal's history: its result after each input that set or adjusted
	// it, the only relations that keep a statement.
	for _, node := range answered {
		for _, r := range settled[node] {
			if r.Statement != "" {
				g := &goals[known[r.Goal]]
				g.Evolution = append(g.Evolution, sessionjudge.Evolution{Input: node, Statement: r.Statement})
				g.Statement = r.Statement
			}
		}
	}

	// Members, the main target and the grouping's evidence.
	home := map[string]string{}
	for _, g := range goals {
		for _, m := range g.Members {
			home[m] = g.ID
		}
	}
	// The input that created a target: its create action in this call, or
	// for a target found before, the earliest input it cites.
	status := map[string]string{}
	creator := map[string]string{}
	for _, node := range answered {
		for _, a := range actions[node] {
			if _, seen := creator[a.Target]; a.Action == sessionjudge.ActionCreate && !seen {
				creator[a.Target] = node
			}
		}
	}
	for _, t := range targets {
		status[t.ID] = t.Status
		if _, seen := creator[t.ID]; seen {
			continue
		}
		for _, ct := range t.Cite {
			if at, ok := order[ct.Node]; ok && (creator[t.ID] == "" || at < order[creator[t.ID]]) {
				creator[t.ID] = ct.Node
			}
		}
	}
	dropped, moved, unrelated, mains := 0, 0, 0, 0
	for _, e := range entries {
		id := e.ID
		if e.Key != "" {
			id = idOf[e.Key]
		}
		i, ok := known[id]
		if !ok {
			if e.Key != "" {
				c.problem("goal key %s, which no relation started, was left out", e.Key)
			}
			continue
		}
		g := &goals[i]
		if e.Statement != "" {
			g.Statement = e.Statement
		}
		for _, ref := range e.Members {
			t, ok := resolve(ref)
			switch {
			case !ok:
				c.problem("goal %s names %q, which is no target", g.ID, ref)
			case home[t] != "" && home[t] != g.ID:
				moved++
			case home[t] == g.ID:
			// A target belongs to the goal of the request that created it:
			// its first input must relate to the goal.
			case !actsOn(creator[t], g.ID):
				unrelated++
			default:
				home[t] = g.ID
				g.Members = append(g.Members, t)
			}
		}
		for _, ct := range c.cites(nav, e.Cite, &dropped) {
			if nav.person[ct.Node] && !slices.ContainsFunc(g.Cite, func(x sessionjudge.Cite) bool { return x.Node == ct.Node }) {
				g.Cite = append(g.Cite, ct)
			}
		}
		switch e.Main {
		case "":
		case undetermined:
			g.Main, g.MainReason, g.MainCite = "", e.MainReason, nil
		default:
			// A name that is no target is in no goal.
			t, _ := resolve(e.Main)
			var cites []sessionjudge.Cite
			for _, ct := range c.cites(nav, e.MainCite, &dropped) {
				if nav.person[ct.Node] {
					cites = append(cites, ct)
				}
			}
			// The main target is a member still wanted. With more than one
			// member, the person's words must show which result the goal
			// is for; the only member needs none.
			switch {
			case home[t] != g.ID, status[t] == sessionjudge.TargetCancelled, status[t] == sessionjudge.TargetReplaced,
				len(g.Members) > 1 && len(cites) == 0:
				mains++
				g.Main, g.MainReason, g.MainCite = "", "the main target given could not be shown", nil
			default:
				g.Main, g.MainReason, g.MainCite = t, e.MainReason, cites
			}
		}
		// A goal's only member is its main with no words of the person; one
		// that gained members since needs them, or its main is unknown.
		if g.Main != "" && len(g.Members) > 1 && len(g.MainCite) == 0 {
			mains++
			g.Main, g.MainReason = "", "the goal gained members, and nothing the person said shows which is main"
		}
	}
	if moved > 0 {
		c.problem("%d targets already in another goal kept their first goal", moved)
	}
	if unrelated > 0 {
		c.problem("%d targets whose first input does not relate to the goal were left in no goal", unrelated)
	}
	if mains > 0 {
		c.problem("%d main targets that could not be shown were left undetermined", mains)
	}
	if dropped > 0 {
		c.problem("%d goal citations of nodes that do not exist were left out", dropped)
	}
	c.goals = goals
	c.relations = settled
	// The last input of each goal, which a later part's relation can follow
	// and which tells a return from a continue. Only a goal's id is ever
	// looked up.
	c.latest = map[string]string{}
	for _, node := range inputs {
		for _, r := range settled[node] {
			c.latest[r.Goal] = node
		}
	}
}

// goalCoverage counts the goals a targets scan found and the targets no goal
// holds. Both are totals of the whole scan, so the run counts them once, over
// what its last part settled, rather than adding up its parts.
func goalCoverage(targets []sessionjudge.Target, goals []sessionjudge.Goal) (found, unplaced int) {
	home := map[string]bool{}
	for _, g := range goals {
		for _, m := range g.Members {
			home[m] = true
		}
	}
	for _, t := range targets {
		if !home[t.ID] {
			unplaced++
		}
	}
	return len(goals), unplaced
}
