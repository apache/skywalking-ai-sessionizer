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

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
)

// reworkEntry is one round of rework in an achievement answer.
type reworkEntry struct {
	Attempt     []string `json:"attempt"`
	Trigger     string   `json:"trigger"`
	TriggerCite []string `json:"trigger_cite"`
	Cause       string   `json:"cause"`
	Redo        []string `json:"redo"`
	Finished    bool     `json:"finished"`
	Note        string   `json:"note"`
}

// checkRework checks the rounds of rework of one target's answer.
//
// A round needs an earlier attempt, a trigger and a redo, in that order. The
// trigger can be the attempt's own step, as a command whose error shows it
// must be run again, as on a run of 2026-10-23. The attempt is the agent's
// work, for this target or before the judged window; the redo is the agent's
// work for this target. A person's trigger is an input of the person; a
// check's or a tool's is a tool step that returned.
// Only a person can change a requirement, so another trigger with that cause
// has a cause the record does not show.
func checkRework(c *checked, nav *Nav, t *sessionjudge.Target, entries []reworkEntry, dropped *int) []sessionjudge.Rework {
	talks := set(t.Talks)
	ours := func(n *cited) bool { return talks[n.main] }
	earlier := func(n *cited) bool { return talks[n.main] || nav.beforeWindow(n.main) }
	var out []sessionjudge.Rework
	left, weakened := 0, 0
	for _, e := range entries {
		attempt := c.cites(nav, e.Attempt, dropped)
		trigger := c.cites(nav, e.TriggerCite, dropped)
		redo := c.cites(nav, e.Redo, dropped)
		tried := place(nav, attempt, func(n *cited) bool { return n.agent && earlier(n) }, true)
		var by string
		switch e.Trigger {
		case sessionjudge.TriggerPerson:
			by = place(nav, trigger, func(n *cited) bool { return nav.person[n.id] }, true)
		case sessionjudge.TriggerVerification, sessionjudge.TriggerTool:
			by = place(nav, trigger, func(n *cited) bool { return n.returned }, true)
		}
		again := place(nav, redo, func(n *cited) bool { return n.agent && ours(n) }, true)
		last := place(nav, redo, func(n *cited) bool { return n.agent && ours(n) }, false)
		// A step that none of the citations shows is after nothing and
		// nothing is after it, so a round missing one is out of order too.
		if (by != tried && !recordedAfter(nav, by, tried)) || !recordedAfter(nav, again, by) {
			left++
			continue
		}
		cause := e.Cause
		if !slices.Contains(sessionjudge.ReworkCauses, cause) ||
			(cause == sessionjudge.ReworkRequirementChange && e.Trigger != sessionjudge.TriggerPerson) {
			weakened++
			cause = sessionjudge.ReworkUnknown
		}
		out = append(out, sessionjudge.Rework{ID: fmt.Sprintf("%s.r%d", t.ID, len(out)+1), Attempt: attempt, Trigger: e.Trigger,
			TriggerCite: trigger, Cause: cause, Redo: redo, From: by, To: last, Finished: e.Finished, Note: e.Note})
	}
	if left > 0 {
		c.problem("%d rounds of rework of target %s without an attempt, a trigger and a redo in recorded order were left out", left, t.ID)
	}
	if weakened > 0 {
		c.problem("%d rounds of rework of target %s with a cause the record cannot show were given an unknown cause", weakened, t.ID)
	}
	return out
}

// cited is what the checks of an answer read of a cited step: its kind,
// whether the agent took it, whether it is a tool step that returned, and
// the main talk it belongs to, a sub-task's step belonging to the main talk
// at the top of its chain.
type cited struct {
	id, kind, main  string
	agent, returned bool
}

// place names the earliest or the latest of the cited steps that pass, in
// recorded order, or nothing when none does.
func place(nav *Nav, cites []sessionjudge.Cite, pass func(*cited) bool, earliest bool) string {
	at := ""
	for _, ct := range cites {
		n := nav.Node(ct.Node)
		if n == nil {
			continue
		}
		s := &cited{id: ct.Node, kind: n.Kind, main: mainTalkOf(nav, ct.Node), agent: slices.Contains(agentActs, n.Kind), returned: toolReturned(n)}
		if !pass(s) {
			continue
		}
		if at == "" || (earliest && recordedAfter(nav, at, ct.Node)) || (!earliest && recordedAfter(nav, ct.Node, at)) {
			at = ct.Node
		}
	}
	return at
}

// mainTalkOf names the main talk a step belongs to: its own talk, or for a
// step of a sub-task, the main talk at the top of the sub-task's chain.
func mainTalkOf(nav *Nav, step string) string {
	talk := nav.TalkOf(step)
	for i := range nav.ledger.Subtasks {
		if st := &nav.ledger.Subtasks[i]; slices.Contains(st.Talks, talk) {
			return st.Main
		}
	}
	return talk
}
