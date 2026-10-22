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
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// The stages that label work share one definition of the labels; the others
// are not given it.
func TestTheLabelingStagesShareTheirLabels(t *testing.T) {
	k, err := LoadKit()
	if err != nil {
		t.Fatal(err)
	}
	for stage, want := range map[string]bool{"targets": false, "achievement": false, "subtasks": true, "efficiency": true} {
		p, _, err := k.Prompt(stage, "")
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(p, "`repair_own_mistake`: fixing a defect in the agent's own work"); got != want {
			t.Errorf("%s has the labels: %v", stage, got)
		}
	}
}

// A talk row points at the tool steps that failed and the tool steps that
// ran the same input again, where avoidable work is most often found.
func TestATalkRowPointsAtFailedAndRepeatedSteps(t *testing.T) {
	failed := true
	d := doc()
	d.Talks[0].Children = append(d.Talks[0].Children,
		sessionview.Node{ID: "r1", Kind: "tool", Name: "Bash", Text: `{"command":"make test"}`, Result: "FAIL", Failed: &failed},
		sessionview.Node{ID: "r2", Kind: "tool", Name: "Bash", Text: `{"command":"make test"}`, Result: "FAIL", Failed: &failed})
	nav := NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
	ans, _ := nav.Call("talks", json.RawMessage(`{"talks":["talk/1"]}`))
	row := ans.(page).Rows[0].(talkRow)
	if strings.Join(row.FailedSteps, ",") != "r1,r2" || len(row.Repeats) != 1 || strings.Join(row.Repeats[0], ",") != "r1,r2" || row.OutlineBytes == 0 {
		t.Fatalf("row %+v", row)
	}
}

// A step's whole recorded text is searched when a step is named, past the
// 2,000 bytes the document keeps, each match saying where it is.
func TestSearchReadsAStepWhole(t *testing.T) {
	d := doc()
	d.Talks[0].Children = append(d.Talks[0].Children, sessionview.Node{ID: "long", Kind: "tool", Result: strings.Repeat("x", 2000),
		Ref: &sessionflow.Ref{Seq: 3, Row: 1}, Refs: []sessionflow.Ref{{Seq: 3, Row: 1}}})
	text := strings.Repeat("x", 5000) + " the commitId changed " + strings.Repeat("y", 100)
	nav := NewNav(d, Compute(d, "claude-code/1", nil, nil), func(_, _ uint64) (*sessiondata.Record, error) {
		return &sessiondata.Record{Parts: []sessiondata.Part{{Kind: "text", Text: "head"}, {Kind: "result", Text: text}}}, nil
	})
	ans, err := nav.Call("search", json.RawMessage(`{"query":"commitid","node":"long"}`))
	if err != nil {
		t.Fatal(err)
	}
	rows := ans.(page).Rows
	if len(rows) != 1 || *rows[0].(hit).Part != 1 || *rows[0].(hit).Offset != 5005 {
		t.Fatalf("hits %+v", rows)
	}
	if ans, _ := nav.Call("search", json.RawMessage(`{"query":"commitid"}`)); len(ans.(page).Rows) != 0 {
		t.Fatal("the search over the document found what lies past its 2,000 bytes")
	}
}

// Every answer says what is left of the call's budget, and the prompt gives
// the card and the budget, so the evaluator need not ask for either.
func TestTheBudgetAndTheCardAreGiven(t *testing.T) {
	nav := NewNav(doc(), Compute(doc(), "claude-code/1", nil, nil), nil)
	nav.ResetBudget(100000)
	ans, _ := nav.Call("talks", nil)
	if left := ans.(page).ReadingLeft; left == nil || *left >= 100000 || *left <= 0 {
		t.Fatalf("reading left %v", left)
	}
	card, _ := nav.Call("card", nil)
	if _, ok := card.(map[string]any)["reading_left"]; !ok {
		t.Fatal("the card does not say what is left")
	}
	text := taskText("targets", nav, given{})
	if !strings.Contains(text, "The card, as the `card` tool gives it") || !strings.Contains(text, "reading budget for this call is 97 KB") {
		t.Fatalf("task text %s", text)
	}
}

// A call is given only its own targets, and a call that failed stays in the
// stage's totals as not judged.
func TestACallIsGivenItsOwnTargets(t *testing.T) {
	nav := navWithInputs()
	targets := []sessionjudge.Target{{ID: "t1", Talks: []string{"talk/1"}}, {ID: "t2", Talks: []string{"talk/2"}}}
	s := &stageRun{stage: "efficiency", nav: nav, targets: targets}
	if got := s.chunkTargets(chunk{assign: Assignment{Talks: []string{"talk/2"}}}); len(got) != 1 || got[0].ID != "t2" {
		t.Fatalf("efficiency targets %+v", got)
	}
	s.stage = "subtasks"
	if got := s.chunkTargets(chunk{assign: Assignment{Subtasks: []string{"stream/a"}}}); len(got) != 1 || got[0].ID != "t1" {
		t.Fatalf("sub-task targets %+v", got)
	}
	c := unjudged("efficiency", chunk{assign: Assignment{Talks: []string{"talk/1", "talk/2"}}}, targets)
	if *c.Talks != 2 || *c.TalksLabeled != 0 {
		t.Fatalf("coverage %+v", c)
	}
}

// Each schema allows exactly the words the checker accepts for the same
// field. A word only the checker knows can never be answered; a word only
// the schema allows is answered and then refused.
func TestTheSchemasAllowWhatTheCheckerAccepts(t *testing.T) {
	k, err := LoadKit()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		stage string
		path  []string
		want  []string
	}{
		{"targets", []string{"inputs", "actions", "action"}, sessionjudge.Actions},
		{"achievement", []string{"achievements", "basis"}, sessionjudge.Bases},
		{"subtasks", []string{"subtasks", "outcome"}, sessionjudge.Levels},
		{"subtasks", []string{"subtasks", "report"}, sessionjudge.Reports},
		{"subtasks", []string{"subtasks", "use"}, sessionjudge.Uses},
		{"subtasks", []string{"subtasks", "dispatch"}, sessionjudge.Dispatches},
		{"subtasks", []string{"subtasks", "work"}, sessionjudge.SubtaskWorks},
		// A talk the evaluator did not label stays not reviewed; it never
		// says so in a label.
		{"efficiency", []string{"labels", "label"}, sessionjudge.TalkWorks},
	} {
		raw, err := k.Schema(tc.stage)
		if err != nil {
			t.Fatal(err)
		}
		var node map[string]any
		if err := json.Unmarshal(raw, &node); err != nil {
			t.Fatal(err)
		}
		for _, p := range tc.path {
			node = node["properties"].(map[string]any)[p].(map[string]any)
			if items, ok := node["items"].(map[string]any); ok {
				node = items
			}
		}
		var got []string
		for _, w := range node["enum"].([]any) {
			got = append(got, w.(string))
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s %v allows %v, the checker accepts %v", tc.stage, tc.path, got, tc.want)
		}
	}
}

// Building a prompt never writes into the loaded kit: calls that run at once
// share it. A loaded file can have room past its end, where an append would
// write.
func TestAPromptLeavesTheKitAsLoaded(t *testing.T) {
	labels := make([]byte, 6, 16)
	copy(labels, "labels")
	labels[:7][6] = 'X'
	k := &Kit{files: map[string][]byte{"common.md": []byte("common"), "subtasks.md": []byte("body"), "labels.md": labels}}
	p, _, err := k.Prompt("subtasks", "task")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p, "labels\nbody") || labels[:7][6] != 'X' {
		t.Fatalf("prompt %q; the loaded labels became %q", p, labels[:7])
	}
}

// The targets found so far are given within the state budget: whole while
// they fit, then the open ones whole and the rest short, then all short,
// then only their ids and status.
func TestTheCatalogueStaysWithinItsBudget(t *testing.T) {
	long := strings.Repeat("a requirement that takes room ", 8)
	var cat []sessionjudge.Target
	for i, st := range []string{sessionjudge.TargetOpen, sessionjudge.TargetClosed, sessionjudge.TargetClosed} {
		cat = append(cat, sessionjudge.Target{ID: fmt.Sprintf("t%d", i+1), Statement: long, Status: st,
			Criteria: []sessionjudge.Criterion{{ID: fmt.Sprintf("t%d.c1", i+1), Text: long}}})
	}
	whole := catalogueLines(cat, 0)
	openWhole := catalogueLines(cat, len(whole)-1)
	short := catalogueLines(cat, len(openWhole)-1)
	bare := catalogueLines(cat, len(short)-1)
	lines := func(s string) []string { return strings.Split(strings.TrimSpace(s), "\n") }
	if strings.Count(whole, `"criteria"`) != 3 {
		t.Fatalf("whole %s", whole)
	}
	if l := lines(openWhole); !strings.Contains(l[0], `"criteria"`) || strings.Contains(l[1], `"criteria"`) || len(openWhole) >= len(whole) {
		t.Fatalf("open whole %s", openWhole)
	}
	if strings.Contains(short, `"criteria"`) || !strings.Contains(short, `"statement"`) || len(short) >= len(openWhole) {
		t.Fatalf("short %s", short)
	}
	if strings.Contains(bare, `"statement"`) || len(lines(bare)) != 3 {
		t.Fatalf("bare %s", bare)
	}
	if got := pushText(sessionjudge.StageTargets, 2, 3, chunk{}, cat, len(short)-1, 0); !strings.Contains(got, bare) {
		t.Fatalf("the prompt's catalogue %s", got)
	}
	// Even ids and status alone are bounded: past the budget, a line says
	// how many more there are.
	if tiny := catalogueLines(cat, 100); len(tiny) > 100 || !strings.Contains(tiny, "2 more targets; the targets tool lists them") {
		t.Fatalf("tiny %s", tiny)
	}
	// So are the targets a labeling call is given.
	var many []sessionjudge.Target
	for i := range 50 {
		many = append(many, sessionjudge.Target{ID: fmt.Sprintf("t%d", i+1), Statement: long, Talks: []string{"talk/1"}})
	}
	nav := NewNav(doc(), Compute(doc(), "claude-code/1", nil, nil), nil)
	// The rows a call is pushed are bounded too, however large one item is.
	var rows []any
	for i := range 200 {
		rows = append(rows, map[string]any{"talk": fmt.Sprintf("talk/%d", i), "label": long})
	}
	pushed := pushText(sessionjudge.StageAchievement, 1, 1, chunk{rows: rows}, nil, 0, 8192)
	if len(pushed) > 8192+200 || !strings.Contains(pushed, "more rows, which the talks tool lists") {
		t.Fatalf("pushed %d bytes:\n%s", len(pushed), pushed)
	}
	// And so are the steps the achievement stage cited.
	var achievements []*sessionjudge.Achievement
	for _, tg := range many {
		achievements = append(achievements, &sessionjudge.Achievement{Target: tg.ID, Cite: []sessionjudge.Cite{{Node: "s1"}, {Node: "s3a"}}})
	}
	cited := taskText("efficiency", nav, given{targets: many, achievements: achievements, subtaskStage: true, limit: 1024})
	if _, block, _ := strings.Cut(cited, "where its work was found:\n\n"); len(block) > 1024+400 || !strings.Contains(block, "targets' steps, not given") {
		t.Fatalf("cited %d bytes:\n%s", len(block), block)
	}
	bounded := taskText("efficiency", nav, given{targets: many, subtaskStage: true, limit: 4096})
	_, block, _ := strings.Cut(bounded, "one short line each. The targets tool gives any of them whole:\n\n")
	block, _, _ = strings.Cut(block, "\n\n")
	if len(block) == 0 || len(block) > 4096 || !strings.Contains(block, "more targets; the targets tool lists them") {
		t.Fatalf("a block of %d bytes:\n%s", len(block), bounded)
	}
}

// The stages that label work are given the steps the achievement stage
// cited, for the targets the call is given and no other.
func TestTheLabelingStagesStartFromTheCitedSteps(t *testing.T) {
	nav := NewNav(doc(), Compute(doc(), "claude-code/1", nil, nil), nil)
	targets := []sessionjudge.Target{{ID: "t1", Talks: []string{"talk/1"}}}
	achievements := []*sessionjudge.Achievement{
		{Target: "t1", Cite: []sessionjudge.Cite{{Node: "s1"}}, Criteria: []sessionjudge.Check{{ID: "t1.c1", Cite: []sessionjudge.Cite{{Node: "s3a"}, {Node: "s1"}}}}},
		{Target: "t2", Cite: []sessionjudge.Cite{{Node: "s3b"}}}}
	for stage, want := range map[string]bool{"achievement": false, "subtasks": true, "efficiency": true} {
		text := taskText(stage, nav, given{targets: targets, achievements: achievements, subtaskStage: true})
		if got := strings.Contains(text, `{"steps":["s1","s3a"],"target":"t1"}`); got != want {
			t.Errorf("%s has the cited steps: %v\n%s", stage, got, text)
		}
		if strings.Contains(text, `"target":"t2"`) {
			t.Errorf("%s was given the steps of a target it does not judge", stage)
		}
	}
}

// A labeling call is given the misleading sub-tasks started before its talks
// ended, in its own talks or earlier, and the subtasks tool gives every
// sub-task's decision once the sub-task stage ran.
func TestALaterTalkIsToldWhatMisledIt(t *testing.T) {
	d := doc()
	for i := range d.Talks[0].Children {
		if d.Talks[0].Children[i].ID == "s1" {
			d.Talks[0].Children[i].At = 150
		}
	}
	nav := NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
	misled := []*sessionjudge.Subtask{{ID: "stream/a", StartedBy: "s1", Use: sessionjudge.UseMisled}}
	listed := func(talk string) bool {
		nav.Assign(Assignment{Talks: []string{talk}})
		return strings.Contains(taskText("efficiency", nav, given{misled: misled, subtaskStage: true}), `"subtask":"stream/a"`)
	}
	// a was started in talk 1, at 150; talk 2 runs 600 to 700.
	if !listed("talk/1") || !listed("talk/2") {
		t.Fatal("a talk at or after the sub-task's start was not told of it")
	}
	misled[0].StartedBy = "s3b"
	d.Talks[3].Children[1].At = 900
	nav = NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
	if listed("talk/1") {
		t.Fatal("a talk that ended before the sub-task started was told of it")
	}
	nav.Decided(map[string]*sessionjudge.Subtask{"stream/a": {ID: "stream/a", Use: sessionjudge.UseMisled, Cite: []sessionjudge.Cite{{Node: "s2"}}}})
	ans, _ := nav.Call("subtasks", json.RawMessage(`{"talks":["talk/1"]}`))
	if row := ans.(page).Rows[0].(subtaskRow); row.Decision == nil || row.Decision.Use != sessionjudge.UseMisled || row.Decision.Cite[0] != "s2" {
		t.Fatalf("row %+v", row)
	}
	// An agent CLI's navigation, in a process of its own, reads them from
	// the call's task.
	task, _ := json.Marshal(Assignment{Talks: []string{"talk/1"}, Decided: nav.decisions()})
	var a Assignment
	if err := json.Unmarshal(task, &a); err != nil {
		t.Fatal(err)
	}
	other := NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
	other.Assign(a)
	ans, _ = other.Call("subtasks", json.RawMessage(`{"talks":["talk/1"]}`))
	if row := ans.(page).Rows[0].(subtaskRow); row.Decision == nil || row.Decision.Use != sessionjudge.UseMisled {
		t.Fatalf("from the task %+v", row)
	}
}
