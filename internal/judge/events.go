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

	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
)

// eventEntry is one event of an achievement answer, under a key of the
// evaluator's own that its contradictions name.
type eventEntry struct {
	Key      string   `json:"key"`
	Kind     string   `json:"kind"`
	Artifact string   `json:"artifact"`
	Criteria []string `json:"criteria"`
	Replaces []string `json:"replaces"`
	Undoes   []string `json:"undoes"`
	Text     string   `json:"text"`
	Cite     []string `json:"cite"`
}

// contradictionEntry is one contradiction of an achievement answer, naming
// the event it concerns by the evaluator's key.
type contradictionEntry struct {
	Event      string   `json:"event"`
	Conclusion bool     `json:"conclusion"`
	Criteria   []string `json:"criteria"`
	Text       string   `json:"text"`
	Cite       []string `json:"cite"`
}

// checkEvents checks the events and contradictions of one target's answer.
//
// An event holds only when what it cites shows it: an answer is given or
// withdrawn by a reply, and anything else by a tool step that returned. A
// request to undo something is not its undoing. Events come in the order they
// were recorded: a delivery first, in one part or several, an undoing after
// a delivery, and after an undoing a delivery again, which may also follow
// any delivery it corrects. On a run of 2026-10-23, an evaluator gave one
// result as three deliveries: the script, the guide that describes it, and
// the reply that reported both. A contradiction holds only when it names a
// delivery of the target, what it shows wrong, and evidence beyond the
// agent's words; asked are the inputs that set the target, which show what
// was asked and never that a result was wrong.
func checkEvents(c *checked, nav *Nav, t *sessionjudge.Target, entries []eventEntry, against []contradictionEntry,
	asked map[string]bool, dropped *int) ([]sessionjudge.Event, []sessionjudge.Contradiction) {
	var events []sessionjudge.Event
	idOf := map[string]string{}
	kindOf := map[string]string{}
	last, lastAt := "", ""
	left := 0
	for _, e := range entries {
		key := strings.TrimSpace(e.Key)
		cites := c.cites(nav, e.Cite, dropped)
		at := eventPlace(nav, e.Artifact, cites)
		_, taken := idOf[key]
		valid := slices.Contains(sessionjudge.Artifacts, e.Artifact) && at != "" && !taken &&
			(lastAt == "" || recordedAfter(nav, at, lastAt))
		switch e.Kind {
		case sessionjudge.EventDelivered:
			valid = valid && last != sessionjudge.EventUndone
		case sessionjudge.EventUndone:
			valid = valid && (last == sessionjudge.EventDelivered || last == sessionjudge.EventDeliveredAgain)
		case sessionjudge.EventDeliveredAgain:
			valid = valid && last != ""
		default:
			valid = false
		}
		if !valid {
			left++
			continue
		}
		// earlier names the earlier deliveries some keys name, each once,
		// and says whether one that named some kept any.
		earlier := func(keys []string, what string) ([]string, bool) {
			var out []string
			for _, k := range keys {
				id := idOf[strings.TrimSpace(k)]
				switch {
				case kindOf[id] != sessionjudge.EventDelivered && kindOf[id] != sessionjudge.EventDeliveredAgain:
					c.problem("%s of target %s names %q, which is no earlier delivery of it", what, t.ID, k)
				case !slices.Contains(out, id):
					out = append(out, id)
				}
			}
			return out, len(keys) == 0 || len(out) > 0
		}
		// An undoing names the criteria it took back, or none for the whole
		// result, and the deliveries it took back, or none for every one
		// standing; a delivery names none. A delivery again names the
		// earlier deliveries it corrects, or none for every one. One that
		// named some, none of which holds, is left out rather than read as
		// the whole.
		var criteria, replaces, undoes []string
		switch e.Kind {
		case sessionjudge.EventUndone:
			criteria = criteriaOf(c, t, "an undoing", e.Criteria)
			var ok bool
			undoes, ok = earlier(e.Undoes, "an undoing")
			valid = (len(e.Criteria) == 0 || len(criteria) > 0) && ok
		case sessionjudge.EventDeliveredAgain:
			replaces, valid = earlier(e.Replaces, "a delivery again")
		}
		if !valid {
			left++
			continue
		}
		id := fmt.Sprintf("%s.e%d", t.ID, len(events)+1)
		if key != "" {
			idOf[key] = id
		}
		kindOf[id] = e.Kind
		events = append(events, sessionjudge.Event{ID: id, Kind: e.Kind, Artifact: e.Artifact, Criteria: criteria, Replaces: replaces, Undoes: undoes,
			Text: e.Text, Cite: cites})
		last, lastAt = e.Kind, at
	}
	if left > 0 {
		c.problem("%d events of target %s were left out: unknown, named twice, not shown by what they cite, or out of recorded order", left, t.ID)
	}

	var out []sessionjudge.Contradiction
	unbound, empty, weak := 0, 0, 0
	for _, x := range against {
		id := idOf[strings.TrimSpace(x.Event)]
		if k := kindOf[id]; k != sessionjudge.EventDelivered && k != sessionjudge.EventDeliveredAgain {
			unbound++
			continue
		}
		// A contradiction of the conclusion itself takes the whole result;
		// otherwise it names the criteria it shows not met.
		var criteria []string
		if !x.Conclusion {
			criteria = criteriaOf(c, t, "a contradiction", x.Criteria)
			if len(criteria) == 0 {
				empty++
				continue
			}
		}
		cites := c.cites(nav, x.Cite, dropped)
		if !shows(nav, cites, true, asked) {
			weak++
			continue
		}
		out = append(out, sessionjudge.Contradiction{Event: id, Conclusion: x.Conclusion, Criteria: criteria, Text: x.Text, Cite: cites})
	}
	if unbound > 0 {
		c.problem("%d contradictions of target %s that name no delivery of it were left out", unbound, t.ID)
	}
	if empty > 0 {
		c.problem("%d contradictions of target %s that say nothing they disprove were left out", empty, t.ID)
	}
	if weak > 0 {
		c.problem("%d contradictions of target %s without evidence beyond the agent's words were left out", weak, t.ID)
	}
	return events, out
}

// criteriaOf keeps the criteria of a target that a contradiction or an
// undoing names, each once: a criterion the person dropped is none of them.
func criteriaOf(c *checked, t *sessionjudge.Target, what string, refs []string) []string {
	var out []string
	for _, ref := range refs {
		if slices.Contains(out, ref) {
			continue
		}
		if !slices.ContainsFunc(t.Criteria, func(cr sessionjudge.Criterion) bool { return cr.ID == ref && !cr.Dropped }) {
			c.problem("%s of target %s names %q, which is none of its criteria", what, t.ID, ref)
			continue
		}
		out = append(out, ref)
	}
	return out
}

// eventPlace names the step that shows an event happened, the earliest in
// recorded order when several do, or nothing when none does. A reply shows
// an answer given or withdrawn; a tool step that returned shows anything
// else, such as a commit, a file written or a remote change.
func eventPlace(nav *Nav, artifact string, cites []sessionjudge.Cite) string {
	if artifact == sessionjudge.ArtifactAnswer {
		return place(nav, cites, func(n *cited) bool { return n.kind == model.KindMessageAssistant }, true)
	}
	return place(nav, cites, func(n *cited) bool { return n.returned }, true)
}
