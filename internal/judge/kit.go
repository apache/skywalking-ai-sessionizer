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
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
)

// The kit is what drives an evaluator: one prompt every stage shares, one
// prompt and one answer schema per stage. Every evaluator of a stage gets the
// same kit, whichever model or agent CLI it is, so two runs differ by their
// evaluators and not by what they were told. Its digest goes into every stage
// file, so a run says which kit it was driven by.
//
//go:embed kit/*.md kit/*.schema.json
var kitFS embed.FS

// Kit is the loaded kit.
type Kit struct {
	files   map[string][]byte
	version string
}

// LoadKit reads the embedded kit.
func LoadKit() (*Kit, error) {
	entries, err := kitFS.ReadDir("kit")
	if err != nil {
		return nil, err
	}
	k := &Kit{files: map[string][]byte{}}
	var names []string
	for _, e := range entries {
		b, err := kitFS.ReadFile("kit/" + e.Name())
		if err != nil {
			return nil, err
		}
		k.files[e.Name()] = b
		names = append(names, e.Name())
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		fmt.Fprintf(h, "%s\x00%d\x00", n, len(k.files[n]))
		h.Write(k.files[n])
	}
	k.version = hex.EncodeToString(h.Sum(nil))
	return k, nil
}

// Version is the digest of every file of the kit.
func (k *Kit) Version() string { return k.version }

// Schema is a stage's answer schema.
func (k *Kit) Schema(stage string) (json.RawMessage, error) {
	b, ok := k.files[stage+".schema.json"]
	if !ok {
		return nil, fmt.Errorf("judge: no schema for stage %q", stage)
	}
	return b, nil
}

// Prompt is what an evaluator of a stage is told: the shared rules, the
// stage's task, then what this run gives it, which asz writes.
func (k *Kit) Prompt(stage string, task string) (string, string, error) {
	common, ok1 := k.files["common.md"]
	body, ok2 := k.files[stage+".md"]
	if !ok1 || !ok2 {
		return "", "", fmt.Errorf("judge: no prompt for stage %q", stage)
	}
	// The stages that label work share one definition of the labels, so a
	// sub-agent's work and the agent's own are judged by the same rules.
	var labels []byte
	if stage == sessionjudge.StageSubtasks || stage == sessionjudge.StageEfficiency {
		// A copy: the loaded file is shared by calls that run at once.
		labels = append(append([]byte{}, k.files["labels.md"]...), '\n')
	}
	sum := sha256.Sum256(append(append(append([]byte{}, common...), labels...), body...))
	var b strings.Builder
	b.Write(common)
	b.WriteString("\n")
	b.Write(labels)
	b.Write(body)
	b.WriteString("\n## This run\n\n")
	b.WriteString(task)
	return b.String(), hex.EncodeToString(sum[:]), nil
}

// taskText writes what a stage of this run gives its evaluator: the head it
// reads, how many inputs, talks and sub-tasks there are, and for the later
// stages the targets.
// given is what the earlier stages of a run give one call, beside the rows
// it answers for.
type given struct {
	targets      []sessionjudge.Target
	achievements []*sessionjudge.Achievement
	// misled are the sub-tasks the sub-task stage found misled the main
	// agent, whose consequences efficiency labels.
	misled       []*sessionjudge.Subtask
	subtaskStage bool
	// limit bounds each list given, in bytes.
	limit int
}

func taskText(stage string, nav *Nav, g given) string {
	var b strings.Builder
	h := nav.Head()
	l := nav.ledger
	top, unplaced := 0, 0
	for _, st := range l.Subtasks {
		switch {
		case st.Depth == 1:
			top++
		case st.Main == "":
			unplaced++
		}
	}
	fmt.Fprintf(&b, "Conversation %s, at round %d of its chain. It has %d inputs from outside the agent, %d main talks and %d sub-tasks: %d started by the main talks, the rest nested under them or started by no step that can be shown (%d).\n",
		nav.doc.Conversation, h.Round, len(nav.inputs), len(nav.Main()), len(l.Subtasks), top, unplaced)
	// The card is given here rather than read: every stage of a run would
	// otherwise begin by asking for the same answer. Measured in round two:
	// the same outline and card were read again by every stage of a run.
	card, _ := json.Marshal(nav.card())
	fmt.Fprintf(&b, "\nThe card, as the `card` tool gives it:\n\n%s\n", card)
	if budget := nav.counts.budget; budget > 0 {
		fmt.Fprintf(&b, "\nYour reading budget for this call is %d KB of tool answers. Every answer says what is left in `reading_left`: keep enough for the decisions still open.\n", budget/1024)
	}
	if w := nav.window; w != nil {
		fmt.Fprintf(&b, "\nThis run judges a window of the conversation: %d main talks, from %s to %s, with every sub-task they started. The %d main talks before them are context: read them when you need to, and the tools mark them `before`, but do not judge them. Nothing recorded after the window's end is shown.\n",
			len(w.Talks), w.Talks[0], w.Talks[len(w.Talks)-1], len(w.Before))
	}
	if stage == sessionjudge.StageTargets {
		mine := nav.Assigned().Inputs
		if len(mine) == 0 {
			mine = nav.Inputs()
		}
		fmt.Fprintf(&b, "\nYour `inputs` answer must list each input this call answers for, %d of them, once.\n", len(mine))
		return b.String()
	}
	level := map[string]string{}
	for _, a := range g.achievements {
		level[a.Target] = a.Level
	}
	type row struct {
		ID        string                   `json:"id"`
		Statement string                   `json:"statement"`
		Criteria  []sessionjudge.Criterion `json:"criteria"`
		Status    string                   `json:"status"`
		Level     string                   `json:"level,omitempty"`
		Talks     []string                 `json:"talks"`
		Subtasks  int                      `json:"subtasks"`
	}
	rows := make([]row, 0, len(g.targets))
	for _, t := range g.targets {
		var active []sessionjudge.Criterion
		for _, c := range t.Criteria {
			if !c.Dropped {
				active = append(active, c)
			}
		}
		t.Criteria = active
		talks := set(t.Talks)
		n := 0
		for _, st := range l.Subtasks {
			if talks[st.Main] {
				n++
			}
		}
		r := row{ID: t.ID, Statement: t.Statement, Criteria: t.Criteria, Status: t.Status, Talks: t.Talks, Subtasks: n}
		if stage != sessionjudge.StageAchievement {
			r.Level = level[t.ID]
		}
		rows = append(rows, r)
	}
	// The targets come whole while they fit limit bytes. Past it, each
	// comes as one short line, and the targets tool gives any of them
	// whole: a call labeling a talk many targets claim would otherwise be
	// sent all of them.
	if out, _ := json.MarshalIndent(rows, "", "  "); g.limit <= 0 || len(out) <= g.limit {
		b.WriteString("\nThe targets, as the targets stage of this run found them:\n\n```json\n")
		b.Write(out)
		b.WriteString("\n```\n")
	} else {
		b.WriteString("\nThe targets, as the targets stage of this run found them, one short line each. The targets tool gives any of them whole:\n\n")
		var lines []string
		for _, r := range rows {
			line, _ := json.Marshal(map[string]any{"id": r.ID, "statement": clip(r.Statement, 80), "status": r.Status,
				"level": r.Level, "criteria": len(r.Criteria), "talks": len(r.Talks)})
			lines = append(lines, string(line))
		}
		b.WriteString(boundedLines(lines, g.limit, "targets; the targets tool lists them"))
	}
	// What the achievement stage cited for each target is where its work
	// was found: the stages that label work start there rather than look
	// again. Only the targets this call is given, so the prompt does not
	// grow with the conversation.
	given := map[string]bool{}
	for _, t := range g.targets {
		given[t.ID] = true
	}
	var cited []sessionjudge.Achievement
	for _, a := range g.achievements {
		if given[a.Target] {
			cited = append(cited, *a)
		}
	}
	if (stage == sessionjudge.StageEfficiency || stage == sessionjudge.StageSubtasks) && len(cited) > 0 {
		b.WriteString("\nThe steps the achievement stage cited for each target, where its work was found:\n\n")
		var lines []string
		for _, a := range cited {
			var ids []string
			seen := map[string]bool{}
			add := func(cs []sessionjudge.Cite) {
				for _, c := range cs {
					if !seen[c.Node] && len(ids) < 12 {
						seen[c.Node] = true
						ids = append(ids, c.Node)
					}
				}
			}
			add(a.Cite)
			for _, k := range a.Criteria {
				add(k.Cite)
			}
			line, _ := json.Marshal(map[string]any{"target": a.Target, "steps": ids})
			lines = append(lines, string(line))
		}
		b.WriteString(boundedLines(lines, g.limit, "targets' steps, not given; search and talk find them"))
	}
	// The sub-tasks whose untrue report the agent acted on, among those
	// started before the call's talks ended: the work that followed is
	// labeled for them, in the talk that started one or in any later talk.
	if stage == sessionjudge.StageEfficiency && len(g.misled) > 0 {
		end := int64(-1)
		for _, tk := range nav.assigned.Talks {
			if t := nav.Node(tk); t != nil && t.To > end {
				end = t.To
			}
		}
		var lines []string
		for _, st := range g.misled {
			talk := nav.TalkOf(st.StartedBy)
			if s := nav.Node(st.StartedBy); end >= 0 && (s == nil || s.At > end) {
				continue
			}
			var cites []string
			for _, c := range st.Cite {
				cites = append(cites, c.Node)
			}
			line, _ := json.Marshal(map[string]any{"subtask": st.ID, "started_by": st.StartedBy, "talk": talk,
				"note": clip(st.Note, 300), "cite": cites})
			lines = append(lines, string(line))
		}
		if len(lines) > 0 {
			b.WriteString("\nThe sub-tasks the sub-task stage found misled the agent: it acted on their reports, which were not true to their own work.\n\n")
			b.WriteString(boundedLines(lines, g.limit, "sub-tasks; the subtasks tool gives each one's decision"))
		}
	}
	if stage == sessionjudge.StageEfficiency {
		if g.subtaskStage {
			b.WriteString("\nThis run judges the sub-tasks in a stage of their own. Label only the work of each talk's own stream; the cost of its sub-tasks is priced by that stage.\n")
		} else {
			b.WriteString("\nThis run has no sub-task stage. Your labels cover each talk with the sub-tasks it started: a sub-task's work takes the label of the part of the talk its starting step is in.\n")
		}
	}
	return b.String()
}
