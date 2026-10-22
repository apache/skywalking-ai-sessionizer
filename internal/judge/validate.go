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
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// checked is what a stage's answer became once asz checked it: the frames it
// writes, and what was taken out or filled in, which makes the stage partial.
type checked struct {
	frames   []any
	targets  []sessionjudge.Target
	coverage sessionjudge.Coverage
	problems []string
}

func (c *checked) problem(format string, args ...any) {
	c.problems = append(c.problems, fmt.Sprintf(format, args...))
}

// note says in one line what was taken out or filled in.
func (c *checked) note() string { return strings.Join(c.problems, "; ") }

// cites keeps the citations that name a node of the document, and counts the
// rest. A citation is never repaired: an id the document does not hold is
// one the evaluator did not read.
func (c *checked) cites(nav *Nav, ids []string, dropped *int) []sessionjudge.Cite {
	out := []sessionjudge.Cite{}
	seen := map[string]bool{}
	for _, id := range ids {
		n := nav.Node(id)
		if n == nil {
			*dropped++
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		cite := sessionjudge.Cite{Node: id}
		if n.Ref != nil {
			cite.Ref = &sessionjudge.Ref{Seq: n.Ref.Seq, Row: n.Ref.Row, Block: n.Ref.Block}
		}
		out = append(out, cite)
	}
	return out
}

// decodeAnswer reads an answer, which must hold its stage's top-level key: an
// answer without it says nothing, and must not read as "nothing found".
func decodeAnswer(raw json.RawMessage, key string, into any) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return fmt.Errorf("the answer is not a JSON object: %w", err)
	}
	if _, ok := top[key]; !ok {
		return fmt.Errorf("the answer has no %q", key)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("the answer does not match the schema: %w", err)
	}
	return nil
}

// The states a criterion and a claim can each be in. They are separate
// vocabularies: a criterion is met or not, a claim is supported or not.
var (
	criterionStates = []string{sessionjudge.CheckMet, sessionjudge.CheckNotMet, sessionjudge.CheckUnknown, sessionjudge.CheckNotCaptured}
	claimStates     = []string{sessionjudge.CheckSupported, sessionjudge.CheckContradicted, sessionjudge.CheckNoEvidence, sessionjudge.CheckNotCaptured}
)

// checkTargets checks one chunk of a targets scan and merges it into the
// catalogue the earlier chunks left, which it never changes in place.
//
// A chunk answers for its own inputs. It may update a target found earlier,
// by its id, or set a new one, by a key of its own such as n1. asz gives
// each new key the next free id, in the order of the first input that acts
// on it, so the ids of a run do not depend on how an evaluator named its
// keys. Criteria are numbered as they are added and never renumbered, and
// nothing is removed: a criterion the person dropped is marked dropped. One
// chunk with no catalogue is the whole stage in one call.
func checkTargets(nav *Nav, catalogue []sessionjudge.Target, raw json.RawMessage) (*checked, error) {
	var a struct {
		Targets []struct {
			ID           string   `json:"id"`
			Key          string   `json:"key"`
			Statement    string   `json:"statement"`
			Criteria     []string `json:"criteria"`
			DropCriteria []string `json:"drop_criteria"`
			Status       string   `json:"status"`
			ReplacedBy   string   `json:"replaced_by"`
			Talks        []string `json:"talks"`
			Cite         []string `json:"cite"`
		} `json:"targets"`
		Inputs []struct {
			Node    string                `json:"node"`
			Actions []sessionjudge.Action `json:"actions"`
		} `json:"inputs"`
	}
	if err := decodeAnswer(raw, "targets", &a); err != nil {
		return nil, err
	}
	c := &checked{}
	judged := set(nav.Judged())
	assigned := nav.Assigned().Inputs
	if len(assigned) == 0 {
		assigned = nav.Inputs()
	}
	mine := set(assigned)
	known := map[string]int{}
	merged := make([]sessionjudge.Target, len(catalogue))
	for i, t := range catalogue {
		t.Criteria, t.Talks, t.Cite = slices.Clone(t.Criteria), slices.Clone(t.Talks), slices.Clone(t.Cite)
		merged[i] = t
		known[t.ID] = i
	}
	keys := map[string]int{}
	for i := range a.Targets {
		// An id no earlier chunk found is the evaluator's name for a new
		// target: models tend to write t1 for the first target they set,
		// and taking that as a failed update would lose the target.
		if t := &a.Targets[i]; t.ID != "" && t.Key == "" {
			if _, ok := known[t.ID]; !ok {
				t.Key, t.ID = t.ID, ""
			}
		}
	}
	// clashed are the keys left out for being an existing target's id. The
	// answer means a new target by them, so a reference to one may mean
	// either target, and it is left out rather than given to the old one.
	clashed := map[string]bool{}
	for i, t := range a.Targets {
		_, taken := known[t.Key]
		switch {
		case t.ID != "" && t.Key != "":
			c.problem("an entry names both target %s and key %s, left out", t.ID, t.Key)
		case t.ID != "":
			// An update to a target found so far: an id no earlier chunk
			// found became a key above.
		case t.Key == "":
			c.problem("an entry with no id and no key was left out")
		case taken:
			// An input that names it would reach the target found earlier,
			// not the new one, and move what it directed to the wrong one.
			c.problem("key %s is the id of a target found so far; the new target set under it was left out", t.Key)
			clashed[t.Key] = true
		case keys[t.Key] > 0:
			c.problem("key %s was given twice; the first was kept", t.Key)
		default:
			keys[t.Key] = i + 1
		}
	}
	// New ids in the order the inputs first act on the keys, then the keys
	// no input acts on, in the answer's order.
	idOf := map[string]string{}
	next := len(merged) + 1
	name := func(key string) {
		if _, done := idOf[key]; !done && keys[key] > 0 {
			idOf[key] = fmt.Sprintf("t%d", next)
			next++
		}
	}
	for _, in := range a.Inputs {
		if mine[in.Node] {
			for _, act := range in.Actions {
				name(act.Target)
			}
		}
	}
	for _, t := range a.Targets {
		if t.ID == "" {
			name(t.Key)
		}
	}
	resolve := func(ref string) (string, bool) {
		if clashed[ref] {
			return "", false
		}
		if _, ok := known[ref]; ok {
			return ref, true
		}
		id, ok := idOf[ref]
		return id, ok
	}
	unresolved := func(ref string) string {
		if clashed[ref] {
			return "which names a target found so far and a new one alike"
		}
		return "which is no target"
	}
	dropped, badTalks := 0, 0
	addTalks := func(t *sessionjudge.Target, talks []string) {
		for _, tk := range talks {
			if !judged[tk] {
				badTalks++
				continue
			}
			if !slices.Contains(t.Talks, tk) {
				t.Talks = append(t.Talks, tk)
			}
		}
	}
	addCriteria := func(t *sessionjudge.Target, texts []string) {
		n := len(t.Criteria)
		for _, text := range texts {
			n++
			t.Criteria = append(t.Criteria, sessionjudge.Criterion{ID: fmt.Sprintf("%s.c%d", t.ID, n), Text: text})
		}
	}
	statuses := []string{sessionjudge.TargetOpen, sessionjudge.TargetClosed, sessionjudge.TargetCancelled, sessionjudge.TargetReplaced}
	for _, x := range a.Targets {
		var t *sessionjudge.Target
		switch {
		case x.ID != "" && x.Key == "":
			t = &merged[known[x.ID]]
			if x.Statement != "" {
				t.Statement = x.Statement
			}
		case x.ID == "" && x.Key != "":
			id, ok := idOf[x.Key]
			if !ok || slices.ContainsFunc(merged, func(m sessionjudge.Target) bool { return m.ID == id }) {
				continue
			}
			merged = append(merged, sessionjudge.Target{ID: id, Statement: x.Statement, Status: sessionjudge.TargetOpen,
				Criteria: []sessionjudge.Criterion{}, Talks: []string{}, Cite: []sessionjudge.Cite{}})
			t = &merged[len(merged)-1]
		default:
			continue
		}
		addCriteria(t, x.Criteria)
		for _, d := range x.DropCriteria {
			at := slices.IndexFunc(t.Criteria, func(cr sessionjudge.Criterion) bool { return cr.ID == d })
			if at < 0 {
				c.problem("target %s has no criterion %q to drop", t.ID, d)
				continue
			}
			t.Criteria[at].Dropped = true
		}
		switch {
		case x.Status == "":
		case slices.Contains(statuses, x.Status):
			t.Status = x.Status
		default:
			c.problem("target %s had status %q, left as it was", t.ID, x.Status)
		}
		if x.ReplacedBy != "" {
			if id, ok := resolve(x.ReplacedBy); ok {
				t.ReplacedBy = id
			} else {
				c.problem("target %s was replaced by %q, %s", t.ID, x.ReplacedBy, unresolved(x.ReplacedBy))
			}
		}
		addTalks(t, x.Talks)
		for _, ct := range c.cites(nav, x.Cite, &dropped) {
			if !slices.ContainsFunc(t.Cite, func(e sessionjudge.Cite) bool { return e.Node == ct.Node }) {
				t.Cite = append(t.Cite, ct)
			}
		}
	}
	answered := map[string]bool{}
	read := 0
	byNode := map[string][]sessionjudge.Action{}
	for _, in := range a.Inputs {
		if answered[in.Node] {
			continue
		}
		if !mine[in.Node] {
			c.problem("an answer for %q, which is not an input of this call, was left out", in.Node)
			continue
		}
		answered[in.Node] = true
		var acts []sessionjudge.Action
		for _, act := range in.Actions {
			if !slices.Contains(sessionjudge.Actions, act.Action) {
				c.problem("input %s had action %q, left out", in.Node, act.Action)
				continue
			}
			needs := !slices.Contains([]string{sessionjudge.ActionNone, sessionjudge.ActionNotPerson, sessionjudge.ActionUnknown}, act.Action)
			if needs {
				id, ok := resolve(act.Target)
				if !ok {
					c.problem("input %s acted on %q, %s", in.Node, act.Target, unresolved(act.Target))
					continue
				}
				act.Target = id
			} else {
				act.Target = ""
			}
			acts = append(acts, act)
		}
		if len(acts) == 0 {
			acts = []sessionjudge.Action{{Action: sessionjudge.ActionUnknown}}
		}
		if acts[0].Action != sessionjudge.ActionUnknown || len(acts) > 1 {
			read++
		}
		byNode[in.Node] = acts
	}
	missing := 0
	for _, node := range assigned {
		acts, ok := byNode[node]
		if !ok {
			missing++
			acts = []sessionjudge.Action{{Action: sessionjudge.ActionUnknown}}
		}
		talk := nav.TalkOf(node)
		if talk == "" {
			talk = node
		}
		c.frames = append(c.frames, &sessionjudge.Input{Node: node, Talk: talk, Actions: acts})
	}
	c.targets = merged
	if missing > 0 {
		c.problem("%d inputs were not answered and are unknown", missing)
	}
	if badTalks > 0 {
		c.problem("%d talk ids that are no main talk this run judges were left out", badTalks)
	}
	if dropped > 0 {
		c.problem("%d citations of nodes that do not exist were left out", dropped)
	}
	total, nTargets := len(assigned), len(merged)
	c.coverage = sessionjudge.Coverage{Inputs: &total, InputsRead: &read, Targets: &nTargets}
	return c, nil
}

// agentWords are the kinds of node that hold only what an agent said or
// thought: its replies, its reasoning, its model calls, and a sub-agent's
// report back. They are evidence of what the agent said, never of what
// happened.
var agentWords = []string{model.KindMessageAssistant, model.KindMessageSynthetic, model.KindThinking,
	model.KindLLMCall, model.KindAgentOutput}

// containers are the kinds of node that hold steps rather than being one. A
// talk holds everything that happened in it, so citing one points at no
// evidence in particular.
var containers = []string{model.KindConversation, model.KindSegment, model.KindSession, model.KindStream,
	model.KindEpoch, model.KindTalk, model.KindRun}

// shows says whether a citation is evidence that something happened: a tool
// step that returned a result, a failed one included; an API error or an
// interrupt; or, when people may be cited, an input from the person other
// than those in asked, the inputs that set the target, which say what was
// asked and never that it was done. The agent's words, a container, and a
// tool call whose result never came show nothing.
func shows(nav *Nav, cites []sessionjudge.Cite, people bool, asked map[string]bool) bool {
	for _, c := range cites {
		n := nav.Node(c.Node)
		if n == nil {
			continue
		}
		switch n.Kind {
		case model.KindTool:
			// A result came when a result part was read, even an empty one,
			// as a LangChain tool can return; the view then sets its state.
			if n.ResultState != "" || n.Result != "" || n.ResultBytes > 0 || (n.Failed != nil && *n.Failed) {
				return true
			}
		case model.KindErrorAPI, model.KindControlInterrupt:
			return true
		case model.KindMessageExternal:
			// The person's words show the work only when they came after
			// the inputs that asked for it: an earlier approval was of
			// something else.
			if people && nav.person[c.Node] && (asked == nil || afterAll(nav, c.Node, asked)) {
				return true
			}
		}
	}
	return false
}

// laterInput says whether a citation names the person's own reaction to the
// work: an input from the person, after every input that set the target.
func laterInput(nav *Nav, cites []sessionjudge.Cite, asked map[string]bool) bool {
	for _, c := range cites {
		if nav.person[c.Node] && afterAll(nav, c.Node, asked) {
			return true
		}
	}
	return false
}

// afterAll says whether a step came after every one of some others, in the
// order they were recorded. After none is not after: with nothing to come
// after, nothing shows the order.
func afterAll(nav *Nav, id string, others map[string]bool) bool {
	if len(others) == 0 {
		return false
	}
	for o := range others {
		if !recordedAfter(nav, id, o) {
			return false
		}
	}
	return true
}

// recordedAfter says whether step a came after step b in the order they were
// recorded: in one talk by their place in it, across talks by where their
// records landed. Times are never used: they can go back or be missing.
func recordedAfter(nav *Nav, a, b string) bool {
	if t := nav.TalkOf(a); t != "" && t == nav.TalkOf(b) {
		p, ok1 := nav.ledger.StepPlace(a)
		q, ok2 := nav.ledger.StepPlace(b)
		return ok1 && ok2 && p > q
	}
	return landedBefore(nav, b, a)
}

// actedAfter says whether a citation names what the agent that started a
// sub-task did next: a step of its stream at or after the starting step,
// other than it, and one the agent took itself: a tool, an agent call, a
// reply, or the final output a sub-agent passes back. An input, a runtime
// notice or injected context is something it was sent.
func actedAfter(nav *Nav, cites []sessionjudge.Cite, start string) bool {
	asking := nav.Node(nav.TalkOf(start))
	if asking == nil {
		return false
	}
	for _, c := range cites {
		n := nav.Node(c.Node)
		if n == nil || !slices.Contains(agentActs, n.Kind) || !recordedAfter(nav, c.Node, start) {
			continue
		}
		if t := nav.Node(nav.TalkOf(c.Node)); t != nil && t.Stream == asking.Stream {
			return true
		}
	}
	return false
}

// shownWrong says whether citations show a task wrong when it was given: the
// step that gave it, whose input is the task, and besides it a tool result,
// an error or the person's words that show it wrong, or the same agent
// giving a task again: another step of its stream that started a sub-task,
// as when it corrected the task or gave the corrected one.
func shownWrong(nav *Nav, cites []sessionjudge.Cite, start string) bool {
	if !slices.ContainsFunc(cites, func(c sessionjudge.Cite) bool { return c.Node == start }) {
		return false
	}
	rest := slices.DeleteFunc(slices.Clone(cites), func(c sessionjudge.Cite) bool { return c.Node == start })
	if shows(nav, rest, true, nil) {
		return true
	}
	stream := streamOfStep(nav, start)
	for _, c := range rest {
		if stream != "" && streamOfStep(nav, c.Node) == stream && startsSubtask(nav, c.Node) {
			return true
		}
	}
	return false
}

// streamOfStep names the stream a step's talk belongs to.
func streamOfStep(nav *Nav, id string) string {
	if t := nav.Node(nav.TalkOf(id)); t != nil {
		return t.Stream
	}
	return ""
}

// startsSubtask says whether a step started a sub-task.
func startsSubtask(nav *Nav, id string) bool {
	for i := range nav.ledger.Subtasks {
		if nav.ledger.Subtasks[i].StartedBy == id {
			return true
		}
	}
	return false
}

// agentActs are the steps an agent takes itself.
var agentActs = []string{model.KindTool, model.KindAgentCall, model.KindMessageAssistant, model.KindAgentOutput}

// citesStep says whether a citation names a step rather than a container,
// and, when within is set, a step of one of those talks.
func citesStep(nav *Nav, cites []sessionjudge.Cite, within map[string]bool) bool {
	for _, c := range cites {
		n := nav.Node(c.Node)
		if n == nil || slices.Contains(containers, n.Kind) {
			continue
		}
		if within == nil || within[nav.TalkOf(c.Node)] {
			return true
		}
	}
	return false
}

// citesKind says whether a citation points at a node of one of the kinds.
func citesKind(nav *Nav, cites []sessionjudge.Cite, kinds ...string) bool {
	for _, c := range cites {
		if n := nav.Node(c.Node); n != nil && slices.Contains(kinds, n.Kind) {
			return true
		}
	}
	return false
}

// checkAchievement checks an achievement answer against the run's targets,
// and gives each target its level.
//
// The evaluator gives a state for each criterion, by its id, and asz derives
// the level from those states, so a level cannot disagree with its own
// criteria, and a criterion left out is unknown rather than missing. Evidence
// that does not hold weakens the decision itself: a met or not met with no
// valid citation is unknown, and so is a met that rests only on the agent's
// own words, unless the target was an answer and the reply delivered it.
func checkAchievement(nav *Nav, targets []sessionjudge.Target, raw json.RawMessage) (*checked, error) {
	var a struct {
		Achievements []struct {
			Target   string `json:"target"`
			Basis    string `json:"basis"`
			Criteria []struct {
				ID        string   `json:"id"`
				State     string   `json:"state"`
				Delivered bool     `json:"delivered"`
				Cite      []string `json:"cite"`
			} `json:"criteria"`
			Claims []struct {
				Text  string   `json:"text"`
				State string   `json:"state"`
				Cite  []string `json:"cite"`
			} `json:"claims"`
			Note string   `json:"note"`
			Cite []string `json:"cite"`
		} `json:"achievements"`
	}
	if err := decodeAnswer(raw, "achievements", &a); err != nil {
		return nil, err
	}
	c := &checked{}
	byID := map[string]*sessionjudge.Target{}
	for i := range targets {
		byID[targets[i].ID] = &targets[i]
	}
	judged := map[string]bool{}
	dropped, missing, weakened := 0, 0, 0
	for _, x := range a.Achievements {
		t := byID[x.Target]
		if t == nil || judged[x.Target] {
			c.problem("an answer for %q, which is no target or was answered already, was left out", x.Target)
			continue
		}
		if !slices.Contains(sessionjudge.Bases, x.Basis) {
			c.problem("target %s had basis %q, left out", x.Target, x.Basis)
			continue
		}
		judged[x.Target] = true
		f := &sessionjudge.Achievement{Target: x.Target, Basis: x.Basis, Note: x.Note, Cite: c.cites(nav, x.Cite, &dropped)}

		given := map[string][]sessionjudge.Cite{}
		state := map[string]string{}
		delivered := map[string]bool{}
		for _, k := range x.Criteria {
			if _, ok := state[k.ID]; ok {
				continue
			}
			if !slices.ContainsFunc(t.Criteria, func(cr sessionjudge.Criterion) bool { return cr.ID == k.ID }) {
				c.problem("target %s answered %q, which is none of its criteria", x.Target, k.ID)
				continue
			}
			if !slices.Contains(criterionStates, k.State) {
				c.problem("criterion %s in state %q was read as unknown", k.ID, k.State)
				k.State = sessionjudge.CheckUnknown
			}
			state[k.ID], given[k.ID], delivered[k.ID] = k.State, c.cites(nav, k.Cite, &dropped), k.Delivered
		}
		all := slices.Clone(f.Cite)
		for _, cs := range given {
			all = append(all, cs...)
		}
		// The inputs that set and changed the target say what was asked,
		// never that it was done.
		// What the target was asked by: the inputs it cites, and those the
		// targets stage found set or changed it. The person's word on its
		// work counts only after them, and not at all when none is known.
		asked := map[string]bool{}
		for _, ct := range t.Cite {
			if nav.Node(ct.Node) != nil {
				asked[ct.Node] = true
			}
		}
		for _, in := range nav.askedBy[t.ID] {
			asked[in] = true
		}
		// A basis holds only with a citation of what it names.
		var holds bool
		switch f.Basis {
		case sessionjudge.BasisAccepted:
			holds = laterInput(nav, all, asked)
		case sessionjudge.BasisObserved:
			holds = shows(nav, all, false, nil)
		case sessionjudge.BasisDelivered:
			holds = citesKind(nav, all, model.KindMessageAssistant)
		default:
			holds = true
		}
		if !holds {
			c.problem("target %s rests on %s, which nothing it cites shows", x.Target, f.Basis)
			f.Basis = sessionjudge.BasisNone
			if citesKind(nav, all, agentWords...) {
				f.Basis = sessionjudge.BasisClaimOnly
			}
		}
		for _, cr := range t.Criteria {
			// A criterion the person dropped is kept on the target and
			// never judged.
			if cr.Dropped {
				continue
			}
			st, ok := state[cr.ID]
			cites := given[cr.ID]
			if !ok {
				missing++
				st = sessionjudge.CheckUnknown
			}
			// A criterion is met or not met when what it cites shows it.
			// A reply shows an answer was delivered: for a criterion marked
			// delivered, or any criterion of a target that was an answer.
			// Measured on a0fc872b: "explain findings 2 and 3 before
			// changing them" was met by the reply and nothing else could
			// show it. The delivery is all a reply shows; whether the answer
			// is true is a claim, and a contradicted claim keeps the target
			// below achieved.
			// The reply shows an answer for a criterion marked delivered, never
			// for the target as a whole: a criterion that asks for a file or
			// a change in a target that also asked for an answer is not shown
			// by the reply. It shows the answer delivered, or a requested part
			// of it missing.
			shown := shows(nav, cites, true, asked) || (delivered[cr.ID] && citesKind(nav, cites, model.KindMessageAssistant))
			if (st == sessionjudge.CheckMet || st == sessionjudge.CheckNotMet) && !shown {
				weakened++
				st = sessionjudge.CheckUnknown
			}
			f.Criteria = append(f.Criteria, sessionjudge.Check{ID: cr.ID, Text: cr.Text, State: st, Cite: cites})
		}
		for _, k := range x.Claims {
			if !slices.Contains(claimStates, k.State) {
				c.problem("a claim of target %s in state %q was left out", x.Target, k.State)
				continue
			}
			cites := c.cites(nav, k.Cite, &dropped)
			st := k.State
			// The agent's words are never their own evidence.
			if (st == sessionjudge.CheckSupported || st == sessionjudge.CheckContradicted) && !shows(nav, cites, true, asked) {
				weakened++
				st = sessionjudge.CheckNoEvidence
			}
			f.Claims = append(f.Claims, sessionjudge.Check{Text: k.Text, State: st, Cite: cites})
		}
		f.Level = levelOf(f.Criteria)
		// A target whose outcome the record contradicts somewhere is not
		// achieved, whatever its criteria say.
		if f.Level == sessionjudge.LevelAchieved && slices.ContainsFunc(f.Claims, func(k sessionjudge.Check) bool { return k.State == sessionjudge.CheckContradicted }) {
			f.Level = sessionjudge.LevelPartly
			c.problem("target %s has a contradicted claim, so it is partly rather than achieved", x.Target)
		}
		c.frames = append(c.frames, f)
	}
	if n := len(targets) - len(judged); n > 0 {
		c.problem("%d targets were not judged", n)
	}
	if missing > 0 {
		c.problem("%d criteria were not answered and are unknown", missing)
	}
	if weakened > 0 {
		c.problem("%d criteria or claims without evidence beyond the agent's words were weakened", weakened)
	}
	if dropped > 0 {
		c.problem("%d citations of nodes that do not exist were left out", dropped)
	}
	total, done := len(targets), len(judged)
	c.coverage = sessionjudge.Coverage{Targets: &total, TargetsJudged: &done}
	return c, nil
}

// levelOf derives a target's level from the states of its criteria:
// achieved when every one is met, partly when some are, not achieved when
// none is and one is not met, and cannot verify otherwise, a target with no
// criterion included.
func levelOf(criteria []sessionjudge.Check) string {
	met, notMet := 0, 0
	for _, k := range criteria {
		switch k.State {
		case sessionjudge.CheckMet:
			met++
		case sessionjudge.CheckNotMet:
			notMet++
		}
	}
	switch {
	case len(criteria) > 0 && met == len(criteria):
		return sessionjudge.LevelAchieved
	case met > 0:
		return sessionjudge.LevelPartly
	case notMet > 0:
		return sessionjudge.LevelNotAchieved
	}
	return sessionjudge.LevelCannotVerify
}

// checkEfficiency checks an efficiency answer: each label is on a talk of its
// target, and a part starts at a step of that talk. A talk counts as labeled
// only when every call of it the target was given has a label.
//
// misled holds the sub-tasks the sub-task stage found misled the main agent:
// a misled_by_subtask label names one of them. subtaskStage says the run
// has a sub-task stage, whose decisions, not these labels, then price each
// sub-task's own calls.
func checkEfficiency(nav *Nav, targets []sessionjudge.Target, misled map[string]bool, subtaskStage bool, raw json.RawMessage) (*checked, error) {
	var a struct {
		Labels []struct {
			Target  string   `json:"target"`
			Talk    string   `json:"talk"`
			From    string   `json:"from"`
			Label   string   `json:"label"`
			Subtask string   `json:"subtask"`
			Note    string   `json:"note"`
			Cite    []string `json:"cite"`
		} `json:"labels"`
	}
	if err := decodeAnswer(raw, "labels", &a); err != nil {
		return nil, err
	}
	c := &checked{}
	// A call that labels some talks answers for those only.
	assigned := nav.assigned.Talks
	talksOf := map[string]map[string]bool{}
	all := 0
	for _, t := range targets {
		talksOf[t.ID] = map[string]bool{}
		for _, tk := range t.Talks {
			if len(assigned) == 0 || nav.mine[tk] {
				talksOf[t.ID][tk] = true
				all++
			}
		}
	}
	seen := map[string]bool{}
	labeled := map[[2]string][]*sessionjudge.Label{}
	dropped := 0
	for _, x := range a.Labels {
		if !talksOf[x.Target][x.Talk] {
			c.problem("a label for talk %q of target %q, which is not one of its talks, was left out", x.Talk, x.Target)
			continue
		}
		if !slices.Contains(sessionjudge.TalkWorks, x.Label) {
			c.problem("talk %s had label %q, left out", x.Talk, x.Label)
			continue
		}
		// Work done on an untrue report names the sub-task the sub-task
		// stage found misled the agent, and begins after that sub-task was
		// started: work before it cannot have rested on its report. No other
		// label names a sub-task.
		if x.Label != sessionjudge.WorkMisled {
			x.Subtask = ""
		} else if !misled[x.Subtask] {
			c.problem("a misled_by_subtask label on talk %s names %q, which the sub-task stage did not find misled the agent, left out", x.Talk, x.Subtask)
			continue
		} else if !afterReport(nav, x.Talk, x.From, x.Subtask) {
			c.problem("a misled_by_subtask label on talk %s begins before the report of %s came back, left out", x.Talk, x.Subtask)
			continue
		}
		// A part that starts at a step outside its talk is left out, never
		// widened to the whole talk: that would put the label, perhaps an
		// avoidable one, on work it was not given for.
		if x.From != "" && nav.TalkOf(x.From) != x.Talk {
			c.problem("a %s label on talk %s starting at %q, which is not a step of it, was left out", x.Label, x.Talk, x.From)
			continue
		}
		key := x.Target + "\x00" + x.Talk + "\x00" + x.From
		if seen[key] {
			continue
		}
		cites := c.cites(nav, x.Cite, &dropped)
		// A label is given by one who read the work: it cites a step of
		// the talk it labels, never the talk as a whole. An avoidable label
		// so points at why, and work nobody opened stays not reviewed
		// rather than counted as reviewed.
		if !citesStep(nav, cites, map[string]bool{x.Talk: true}) {
			c.problem("the %s label on talk %s cites no step of it, left out", x.Label, x.Talk)
			continue
		}
		// An avoidable label, or rework charged to the person, needs what
		// shows why: a tool result, an error, or the person's own words,
		// never the agent's account of itself.
		if (sessionjudge.Avoidable(x.Label) || x.Label == sessionjudge.WorkRework) && !shows(nav, cites, true, nil) {
			c.problem("the %s label on talk %s cites nothing that shows why, left out", x.Label, x.Talk)
			continue
		}
		seen[key] = true
		k := [2]string{x.Target, x.Talk}
		lb := &sessionjudge.Label{Target: x.Target, Talk: x.Talk, From: x.From, Label: x.Label,
			Subtask: x.Subtask, Note: x.Note, Cite: cites}
		labeled[k] = append(labeled[k], lb)
		c.frames = append(c.frames, lb)
	}
	if dropped > 0 {
		c.problem("%d citations of nodes that do not exist were left out", dropped)
	}
	// A talk left unlabeled is work not reviewed, and so is a talk's work
	// before its first part when no label covers the whole talk: the close
	// file prices it so. Either way the stage covered less than it was
	// given. Only the calls the target was given count: in a talk two
	// targets share, the work before an input that asked for this one
	// served the other.
	l := nav.ledger
	claims := map[string][]string{}
	for _, t := range targets {
		for _, tk := range t.Talks {
			claims[tk] = append(claims[tk], t.ID)
		}
	}
	who := ownersOfCalls(l, claims, nav.found)
	var sub map[int]string
	if subtaskStage {
		sub = subtaskCalls(l)
	}
	whole := 0
	for _, t := range targets {
		for _, tk := range t.Talks {
			lbs := labeled[[2]string{t.ID, tk}]
			if len(lbs) == 0 {
				continue
			}
			w, ps := talkParts(l, lbs)
			missed := 0
			for _, i := range l.CallsOf(tk) {
				if _, ok := sub[i]; ok || !slices.Contains(who[i], t.ID) {
					continue
				}
				if partOf(l, tk, i, w, ps).work == "" {
					missed++
				}
			}
			if missed > 0 {
				c.problem("talk %s of target %s is labeled only from a later part, and its calls before that part are not reviewed (%d)", tk, t.ID, missed)
				continue
			}
			whole++
		}
	}
	if n := len(labeled); all > n {
		c.problem("%d of %d talks were not labeled", all-n, all)
	}
	c.coverage = sessionjudge.Coverage{Talks: &all, TalksLabeled: &whole}
	return c, nil
}

// checkSubtasks checks a sub-task answer: every id names a sub-task, each
// sub-task is judged once, and a decision that says the record showed
// something cites it. A decision with nothing valid behind it falls back to
// the value that claims nothing: an outcome that cannot be verified, a
// report that cannot be checked, a dispatch that is unknown, and work that
// is not reviewed.
//
// A decision is checked for each sub-task it names. Its outcome and report
// hold for a sub-task only when a citation points into that sub-task's own
// talks: what its own steps did, never the report it sent back, which the
// step that started it shows as its result. So one citation cannot vouch for
// sub-tasks nobody read. Measured on the 221ac729 window: sol judged 14
// sub-tasks alike, 12 from one workflow and 2 from another talk, and
// refusing the group as a whole lost all 14.
func checkSubtasks(nav *Nav, raw json.RawMessage) (*checked, error) {
	var a struct {
		Subtasks []struct {
			IDs      []string `json:"ids"`
			Outcome  string   `json:"outcome"`
			Report   string   `json:"report"`
			Use      string   `json:"use"`
			Dispatch string   `json:"dispatch"`
			Work     string   `json:"work"`
			Note     string   `json:"note"`
			Cite     []string `json:"cite"`
		} `json:"subtasks"`
	}
	if err := decodeAnswer(raw, "subtasks", &a); err != nil {
		return nil, err
	}
	c := &checked{}
	judged := map[string]bool{}
	dropped, weakened, unknown, twice, notMine := 0, 0, 0, 0, 0
	for _, x := range a.Subtasks {
		// misled_by_subtask labels the asking agent's work in efficiency, by
		// part; a sub-task's own work, under one label, is never given it.
		// Taken, it is work not reviewed, and the decision is kept.
		if x.Work == sessionjudge.WorkMisled {
			x.Work, weakened = sessionjudge.WorkNotReviewed, weakened+1
		}
		if !slices.Contains(sessionjudge.Levels, x.Outcome) || !slices.Contains(sessionjudge.Reports, x.Report) ||
			!slices.Contains(sessionjudge.Uses, x.Use) || !slices.Contains(sessionjudge.Dispatches, x.Dispatch) ||
			!slices.Contains(sessionjudge.SubtaskWorks, x.Work) {
			c.problem("a decision on %v with outcome %q, report %q, use %q, dispatch %q, work %q was left out",
				x.IDs, x.Outcome, x.Report, x.Use, x.Dispatch, x.Work)
			continue
		}
		var sts []*Subtask
		for _, id := range x.IDs {
			st, ok := nav.ledger.Subtask(id)
			if !ok {
				unknown++
				continue
			}
			if len(nav.assigned.Subtasks) > 0 && !nav.mine[id] {
				notMine++
				continue
			}
			sts = append(sts, st)
		}
		cites := c.cites(nav, x.Cite, &dropped)
		for _, st := range sts {
			if judged[st.ID] {
				twice++
				continue
			}
			judged[st.ID] = true
			// The evidence of this sub-task: what is cited from its own
			// talks.
			var own []sessionjudge.Cite
			talks := set(st.Talks)
			for _, ct := range cites {
				if talks[nav.TalkOf(ct.Node)] {
					own = append(own, ct)
				}
			}
			// A person does not speak inside a sub-task's stream: the input
			// its talk begins with is what it was asked.
			evidence := shows(nav, own, false, nil)
			f := sessionjudge.Subtask{ID: st.ID, Parent: st.Parent, StartedBy: st.StartedBy, Outcome: x.Outcome, Report: x.Report,
				Use: x.Use, Dispatch: x.Dispatch, Work: x.Work, Note: x.Note, Cite: cites}
			if f.Outcome != sessionjudge.LevelCannotVerify && !evidence {
				f.Outcome, weakened = sessionjudge.LevelCannotVerify, weakened+1
			}
			if f.Report != sessionjudge.ReportUnverifiable && !evidence {
				f.Report, weakened = sessionjudge.ReportUnverifiable, weakened+1
			}
			// Calling work redundant or avoidable needs what shows it: a tool
			// result, an error or the person's own words, never the
			// sub-agent's account of itself. Rework is charged to the
			// person, so the person's words are what show it.
			if f.Dispatch == sessionjudge.DispatchRedundant && !shows(nav, cites, true, nil) {
				f.Dispatch, weakened = sessionjudge.DispatchUnknown, weakened+1
			}
			// A task called wrong cites the step that gave it, whose input
			// is the task, and what shows it wrong besides.
			if f.Dispatch == sessionjudge.DispatchMisdirected && !shownWrong(nav, cites, st.StartedBy) {
				f.Dispatch, weakened = sessionjudge.DispatchUnknown, weakened+1
			}
			// Misled is the result used on a report found untrue; on any
			// other report it is only used.
			if f.Use == sessionjudge.UseMisled && f.Report != sessionjudge.ReportInaccurate {
				f.Use, weakened = sessionjudge.UseUsed, weakened+1
			}
			// Whether the result was used is shown by what the agent that
			// asked did next: a step of its stream, after the one that
			// started the sub-task. The report arriving shows nothing.
			if f.Use != sessionjudge.UseUnknown && !actedAfter(nav, cites, st.StartedBy) {
				f.Use, weakened = sessionjudge.UseUnknown, weakened+1
			}
			switch f.Dispatch {
			case sessionjudge.DispatchMisdirected:
				// A sub-task given the wrong task did what it was told:
				// whatever its work was, it is the cost of the agent that
				// gave the task.
				f.Work = sessionjudge.WorkMisdirected
			case sessionjudge.DispatchRedundant:
				// So did one given work already done or under way.
				f.Work = sessionjudge.WorkRedundant
			default:
				// Either label says the task was wrong to give, which the
				// dispatch did not say.
				if f.Work == sessionjudge.WorkMisdirected || f.Work == sessionjudge.WorkRedundant {
					f.Work, weakened = sessionjudge.WorkNotReviewed, weakened+1
				}
				// What its work was is said by one who read it: any label
				// cites a step of its own work, or the work is not
				// reviewed. Measured on 0947c9ef: a sub-task labeled
				// exploration with no citation and a note saying its work
				// could not be read.
				if f.Work != sessionjudge.WorkNotReviewed && !citesStep(nav, own, nil) {
					f.Work, weakened = sessionjudge.WorkNotReviewed, weakened+1
				}
				if (sessionjudge.Avoidable(f.Work) || f.Work == sessionjudge.WorkRework) && !shows(nav, cites, true, nil) {
					f.Work, weakened = sessionjudge.WorkNotReviewed, weakened+1
				}
			}
			c.frames = append(c.frames, &f)
		}
	}
	if unknown > 0 {
		c.problem("%d ids that are no sub-task were left out", unknown)
	}
	if notMine > 0 {
		c.problem("%d decisions on sub-tasks this call does not answer for were left out", notMine)
	}
	if twice > 0 {
		c.problem("%d sub-tasks judged twice kept their first decision", twice)
	}
	if weakened > 0 {
		c.problem("%d decisions without valid evidence were weakened", weakened)
	}
	if dropped > 0 {
		c.problem("%d citations of nodes that do not exist were left out", dropped)
	}
	total, done := len(nav.assigned.Subtasks), len(judged)
	if total == 0 {
		for i := range nav.ledger.Subtasks {
			if nav.ledger.JudgesSubtask(&nav.ledger.Subtasks[i]) {
				total++
			}
		}
	}
	if total > done {
		c.problem("%d of %d sub-tasks were not judged", total-done, total)
	}
	c.coverage = sessionjudge.Coverage{Subtasks: &total, SubtasksJudged: &done}
	return c, nil
}

// afterReport says whether a label's piece begins after a sub-task's report
// came back to the agent that asked. The report comes back with the earliest
// runtime notice that the sub-task ended, or, for one that ran in the
// foreground, with the result of the call that started it, which the record
// marks as the sub-task's own; a background launch's acknowledgement is not
// a report, and with neither, nothing shows the report came back. In the
// talk it came back to, the piece begins at a later step; in another talk,
// after it in the order the records landed. Order is never taken from
// times, which can go back or be missing.
func afterReport(nav *Nav, talk, from, subtask string) bool {
	st, ok := nav.ledger.Subtask(subtask)
	if !ok || st.StartedBy == "" {
		return false
	}
	// Where the report came back, as the record that carried it: the
	// earliest runtime notice that the sub-task ended, or, for one run in
	// the foreground, the record of the starting call's own result, which
	// the record marks as the sub-task's. The call's first record is its
	// request; what came back is in the ones after it.
	var arrival *sessionflow.Ref
	reached := ""
	for _, r := range nav.doc.Relations {
		if r.Type == model.RelReports && r.To == st.ID {
			if ref, ok := refOf(nav.Node(r.From)); ok && (arrival == nil || refBefore(ref, *arrival)) {
				arrival, reached = &ref, nav.TalkOf(r.From)
			}
		}
	}
	start := nav.Node(st.StartedBy)
	for _, r := range nav.doc.Relations {
		if arrival == nil && r.Type == model.RelResultOf && r.To == st.StartedBy && len(start.Refs) > 1 {
			ref := start.Refs[len(start.Refs)-1]
			arrival, reached = &ref, nav.TalkOf(st.StartedBy)
		}
	}
	if arrival == nil {
		return false
	}
	// In the talk the report reached, a whole talk began before it.
	if talk == reached && (from == "" || nav.TalkOf(from) != talk) {
		return false
	}
	first := from
	if first == "" {
		first = talk
	}
	ref, ok := refOf(nav.Node(first))
	return ok && refBefore(*arrival, ref)
}

// landedBefore says whether one step's record landed before another's, by
// file, line and block. A talk is placed by its first step. A step with no
// record is placed nowhere, and nothing is shown before or after it.
func landedBefore(nav *Nav, a, b string) bool {
	ra, ok1 := refOf(nav.Node(a))
	rb, ok2 := refOf(nav.Node(b))
	return ok1 && ok2 && refBefore(ra, rb)
}

// refBefore says whether one record landed before another, by file, line
// and block.
func refBefore(ra, rb sessionflow.Ref) bool {
	block := func(r sessionflow.Ref) int {
		if r.Block == nil {
			return -1
		}
		return *r.Block
	}
	switch {
	case ra.Seq != rb.Seq:
		return ra.Seq < rb.Seq
	case ra.Row != rb.Row:
		return ra.Row < rb.Row
	}
	return block(ra) < block(rb)
}

// refOf is where a node was recorded: its own record, or for a container the
// first of its steps that has one.
func refOf(n *sessionview.Node) (sessionflow.Ref, bool) {
	if n == nil {
		return sessionflow.Ref{}, false
	}
	if n.Ref != nil {
		return *n.Ref, true
	}
	if len(n.Refs) > 0 {
		return n.Refs[0], true
	}
	for i := range n.Children {
		if r, ok := refOf(&n.Children[i]); ok {
			return r, true
		}
	}
	return sessionflow.Ref{}, false
}
