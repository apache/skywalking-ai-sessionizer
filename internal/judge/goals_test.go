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
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// fiveInputs is a conversation of five main talks, each opened by one input
// of the person, in/1 to in/5.
func fiveInputs() *Nav {
	var talks []sessionview.Node
	for i := 1; i <= 5; i++ {
		talks = append(talks, sessionview.Node{ID: fmt.Sprintf("talk/%d", i), Kind: "talk", Stream: "main", From: int64(i * 100), To: int64(i*100 + 50),
			Children: []sessionview.Node{
				{ID: fmt.Sprintf("in/%d", i), Kind: model.KindMessageExternal, Text: fmt.Sprintf("request %d", i)},
				call(fmt.Sprintf("c%d", i), "main", int64(i*100+10), "m", usage(10, 1, 0, 0)),
			}})
	}
	d := &sessionview.Conversation{Conversation: "conv", Streams: []sessionview.Stream{{ID: "stream/main", Name: "main"}}, Talks: talks}
	return NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
}

// goalsAnswer is a targets answer over fiveInputs: one target per input that
// creates one, the inputs with their actions and relations, and the goals.
type goalsAnswer struct {
	inputs []string
	goals  []string
}

func (g goalsAnswer) raw(targets string) json.RawMessage {
	return json.RawMessage(`{"targets":[` + targets + `],"inputs":[` + strings.Join(g.inputs, ",") + `],"goals":[` + strings.Join(g.goals, ",") + `]}`)
}

// newTarget is a new target under key, created by the input node.
func newTarget(key, node string) string {
	return fmt.Sprintf(`{"id":"","key":%q,"statement":"do %s","criteria":["done"],"drop_criteria":[],"status":"closed","replaced_by":"","talks":[],"cite":[%q]}`, key, node, node)
}

// input is one answered input: its actions and relations as JSON lists.
func input(node, actions, relations string) string {
	return fmt.Sprintf(`{"node":%q,"actions":[%s],"relations":[%s]}`, node, actions, relations)
}

func act(target, action string) string {
	return fmt.Sprintf(`{"target":%q,"action":%q}`, target, action)
}

func rel(kind, goal, follows, statement string) string {
	return fmt.Sprintf(`{"kind":%q,"goal":%q,"follows":%q,"request":"","statement":%q}`, kind, goal, follows, statement)
}

func goal(id, key string, members []string, main, mainCite string) string {
	m, _ := json.Marshal(members)
	cite := "[]"
	if mainCite != "" {
		cite = fmt.Sprintf("[%q]", mainCite)
	}
	return fmt.Sprintf(`{"id":%q,"key":%q,"statement":"","members":%s,"main":%q,"main_reason":"","main_cite":%s,"cite":[]}`, id, key, m, main, cite)
}

// entry is a goal entry the positional helper cannot express.
func entry(e goalEntry) string {
	b, _ := json.Marshal(e)
	return string(b)
}

func unplacedOf(c *checked) int {
	_, n := goalCoverage(c.targets, c.goals)
	return n
}

// relationsOf reads the settled relations of each input, as kind:goal:follows.
func relationsOf(c *checked) map[string]string {
	out := map[string]string{}
	for _, f := range c.frames {
		in, ok := f.(*sessionjudge.Input)
		if !ok {
			continue
		}
		var parts []string
		for _, r := range in.Relations {
			parts = append(parts, r.Kind+":"+r.Goal+":"+r.Follows)
		}
		out[in.Node] = strings.Join(parts, ",")
	}
	return out
}

// A conversation keeps several goals: in/1 starts one, in/2 adjusts it,
// in/3 starts another, in/4 returns to the first and in/5 goes on with it.
// Ids follow the order the inputs first relate to the goals, and each goal
// keeps how it changed.
func TestGoalsFollowTheInputs(t *testing.T) {
	nav := fiveInputs()
	a := goalsAnswer{
		inputs: []string{
			input("in/1", act("n1", "create"), rel("new", "ga", "", "build the parser")),
			input("in/2", act("n1", "refine"), rel("adjust", "ga", "in/1", "build the parser with tests")),
			input("in/3", act("n2", "create"), rel("new", "gb", "in/2", "update the website")),
			input("in/4", act("n1", "steer"), rel("return", "ga", "in/2", "")),
			input("in/5", act("n1", "steer"), rel("continue", "ga", "in/4", "keep going")),
		},
		goals: []string{
			entry(goalEntry{Key: "gb", Statement: "update the website and its docs", Members: []string{"n2"}, Main: "n2"}),
			entry(goalEntry{Key: "ga", Members: []string{"n1"}, Main: "n1", Cite: []string{"in/2", "in/1", "in/2", "c1"}}),
		},
	}
	c, err := checkTargets(nav, catalogue{}, a.raw(newTarget("n1", "in/1")+","+newTarget("n2", "in/3")))
	if err != nil {
		t.Fatal(err)
	}
	if c.note() != "" {
		t.Fatalf("note %s", c.note())
	}
	want := map[string]string{"in/1": "new:g1:", "in/2": "adjust:g1:in/1", "in/3": "new:g2:", "in/4": "return:g1:in/2", "in/5": "continue:g1:in/4"}
	if got := relationsOf(c); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("relations %v, want %v", got, want)
	}
	g1 := c.goals[0]
	if g1.ID != "g1" || g1.Main != "t1" || fmt.Sprint(g1.Members) != "[t1]" || g1.Statement != "build the parser with tests" ||
		len(g1.Evolution) != 2 || g1.Evolution[0].Input != "in/1" || g1.Evolution[1].Input != "in/2" ||
		fmt.Sprint(g1.Cite) != "[{in/2 <nil>} {in/1 <nil>}]" {
		t.Fatalf("g1 %+v", g1)
	}
	// The evaluator's statement of a goal stands over the inputs' own.
	if g2 := c.goals[1]; g2.ID != "g2" || fmt.Sprint(g2.Members) != "[t2]" || g2.Statement != "update the website and its docs" {
		t.Fatalf("g2 %+v", g2)
	}
	// Only an input that sets or adjusts a goal states it.
	for _, f := range c.frames {
		if in, ok := f.(*sessionjudge.Input); ok && in.Node == "in/5" && in.Relations[0].Statement != "" {
			t.Fatalf("in/5 kept the statement %q", in.Relations[0].Statement)
		}
	}
	if unplacedOf(c) != 0 || len(c.goals) != 2 {
		t.Fatalf("goals %d, unplaced %d", len(c.goals), unplacedOf(c))
	}
	// A later part follows each goal from its last input.
	if c.latest["g1"] != "in/5" || c.latest["g2"] != "in/3" {
		t.Fatalf("latest %v", c.latest)
	}
}

// Each relation rule on its own: a relation with nothing valid behind it is
// made unclear, the relation that claims nothing, and the stage says so.
func TestEachRelationRuleStandsAlone(t *testing.T) {
	base := func(in4 string) goalsAnswer {
		inputs := []string{
			input("in/1", act("n1", "create"), rel("new", "ga", "", "build it")),
			input("in/2", act("n2", "create"), rel("new", "gb", "", "ship it")),
			input("in/3", act("n1", "steer"), rel("return", "ga", "in/1", "")),
			in4,
			input("in/5", act("", "none"), rel("unclear", "", "", "")),
		}
		return goalsAnswer{inputs: inputs}
	}
	targets := newTarget("n1", "in/1") + "," + newTarget("n2", "in/2")
	for _, tc := range []struct {
		name, in4, want, note string
	}{
		{"continue of the goal of the input it follows", input("in/4", act("n1", "steer"), rel("continue", "ga", "in/3", "")), "continue:g1:in/3", ""},
		{"continue following an input of another goal", input("in/4", act("n1", "steer"), rel("continue", "ga", "in/2", "")), "unclear::", "made unclear"},
		{"return following an input of another goal", input("in/4", act("n1", "steer"), rel("return", "ga", "in/2", "")), "unclear::", "1 relations without"},
		{"continue following a later input", input("in/4", act("n1", "steer"), rel("continue", "ga", "in/5", "")), "unclear::", "made unclear"},
		{"continue following no input", input("in/4", act("n1", "steer"), rel("continue", "ga", "", "")), "unclear::", "made unclear"},
		{"continue of no goal", input("in/4", act("n1", "steer"), rel("continue", "gz", "in/3", "")), "unclear::", "made unclear"},
		{"new of a goal started before", input("in/4", act("n1", "steer"), rel("new", "ga", "", "again")), "unclear::", "made unclear"},
		{"new naming no goal", input("in/4", act("n1", "steer"), rel("new", "", "", "something")), "unclear::", "made unclear"},
		{"return to the goal the input before acted on", input("in/4", act("n1", "steer"), rel("return", "ga", "in/3", "")), "continue:g1:in/3", "made unclear"},
		{"an unknown kind", input("in/4", act("n1", "steer"), rel("resume", "ga", "in/3", "")), "unclear::", "made unclear"},
		{"no relation at all", input("in/4", act("n1", "steer"), ""), "unclear::", "1 inputs had no relation and are unclear"},
		{"the person's input said not from the person", input("in/4", act("n1", "steer"), rel("not_from_person", "", "", "")), "unclear::", "made unclear"},
		{"an input not from the person given a goal", input("in/4", act("", "not_from_person"), rel("continue", "ga", "in/3", "")), "not_from_person::", "made unclear"},
		{"an input not from the person said so", input("in/4", act("", "not_from_person"), rel("not_from_person", "", "", "")), "not_from_person::", ""},
		{"unclear names no goal", input("in/4", act("n1", "steer"), rel("unclear", "ga", "in/3", "")), "unclear::", ""},
		{"an input answered twice", input("in/4", act("n1", "steer"), rel("continue", "ga", "in/3", "")) + "," + input("in/4", act("n2", "steer"), rel("return", "gb", "in/2", "")),
			"continue:g1:in/3", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := checkTargets(fiveInputs(), catalogue{}, base(tc.in4).raw(targets))
			if err != nil {
				t.Fatal(err)
			}
			if got := relationsOf(c)["in/4"]; got != tc.want {
				t.Errorf("in/4 %s, want %s", got, tc.want)
			}
			if len(c.goals) != 2 {
				t.Errorf("goals %+v", c.goals)
			}
			if (tc.note == "") != (c.note() == "") || !strings.Contains(c.note(), tc.note) {
				t.Errorf("note %q, want %q", c.note(), tc.note)
			}
		})
	}
	// An input follows only an input before it, so the first follows none.
	first := goalsAnswer{inputs: []string{
		input("in/1", act("n1", "create"), rel("continue", "ga", "in/1", "")),
		input("in/2", act("n1", "steer"), rel("new", "ga", "", "build it")),
	}}
	c, err := checkTargets(fiveInputs(), catalogue{}, first.raw(newTarget("n1", "in/1")))
	if got := relationsOf(c); err != nil || got["in/1"] != "unclear::" || got["in/2"] != "new:g1:" {
		t.Fatalf("the first input: %v %v", err, got)
	}
	// A goal starts once, even when one input names it new twice.
	twice := base(input("in/4", act("n1", "steer"), rel("new", "gc", "", "start it")+","+rel("new", "gc", "", "start it again")))
	c, err = checkTargets(fiveInputs(), catalogue{}, twice.raw(targets))
	if err != nil || relationsOf(c)["in/4"] != "new:g3:,unclear::" || len(c.goals) != 3 || len(c.goals[2].Evolution) != 1 {
		t.Fatalf("new twice: %v %v %+v", err, relationsOf(c), c.goals)
	}
	// A relation to no goal does not follow an input that relates to none.
	none := goalsAnswer{inputs: []string{
		input("in/1", act("", "none"), rel("unclear", "", "", "")),
		input("in/2", act("", "none"), rel("continue", "", "in/1", "")),
	}}
	c, err = checkTargets(fiveInputs(), catalogue{}, none.raw(""))
	if err != nil || relationsOf(c)["in/2"] != "unclear::" {
		t.Fatalf("a relation to no goal: %v %v", err, relationsOf(c))
	}
	// An input the answer left out is unclear, and reported once, as not
	// answered.
	left := base(input("in/4", act("n1", "steer"), rel("continue", "ga", "in/3", "")))
	left.inputs = slices.Delete(left.inputs, 3, 4)
	c, err = checkTargets(fiveInputs(), catalogue{}, left.raw(targets))
	if err != nil || relationsOf(c)["in/4"] != "unclear::" || c.note() != "1 inputs were not answered and are unknown" {
		t.Fatalf("an input left out: %v %v %q", err, relationsOf(c), c.note())
	}
}

// The goals of an answer are checked against its targets and relations: a
// target belongs to the goal of the input that created it, to one goal only;
// a main target is a member still wanted, with the person's words behind it
// when the goal has more than one; goal keys and ids are told apart as
// target keys and ids are.
func TestEachGoalRuleStandsAlone(t *testing.T) {
	inputs := []string{
		input("in/1", act("n1", "create"), rel("new", "ga", "", "build it")),
		input("in/2", act("n2", "create"), rel("adjust", "ga", "in/1", "build and test it")),
		input("in/3", act("n3", "create"), rel("new", "gb", "", "ship it")),
		input("in/4", act("n3", "cancel")+","+act("n4", "create"), rel("adjust", "gb", "in/3", "")),
		input("in/5", act("", "none"), rel("unclear", "", "", "")),
	}
	gone := func(key, node, status string) string {
		return fmt.Sprintf(`{"id":"","key":%q,"statement":"ship it","criteria":["shipped"],"drop_criteria":[],"status":%q,"replaced_by":"","talks":[],"cite":[%q]}`, key, status, node)
	}
	targets := newTarget("n1", "in/1") + "," + newTarget("n2", "in/2") + "," + gone("n3", "in/3", "cancelled") + "," + gone("n4", "in/4", "replaced")
	b := goal("", "gb", []string{"n3", "n4"}, "", "")
	mainOf := func(i int) func(c *checked) string {
		return func(c *checked) string {
			if c.goals[i].Main != "" {
				return "main " + c.goals[i].Main
			}
			return ""
		}
	}
	for _, tc := range []struct {
		name  string
		goals []string
		check func(c *checked) string
		note  string
	}{
		{"members and a main target the person named", []string{goal("", "ga", []string{"n1", "n2"}, "n2", "in/2"), b},
			func(c *checked) string {
				if len(c.goals) != 2 || c.goals[0].Main != "t2" || fmt.Sprint(c.goals[0].Members) != "[t1 t2]" || unplacedOf(c) != 0 {
					return fmt.Sprintf("goals %+v", c.goals)
				}
				return ""
			}, ""},
		{"a main target among several with no words of the person", []string{goal("", "ga", []string{"n1", "n2"}, "n2", ""), b},
			mainOf(0), "main targets that could not be shown"},
		{"a main target among several with only the agent's words", []string{goal("", "ga", []string{"n1", "n2"}, "n2", "c2"), b},
			mainOf(0), "main targets that could not be shown"},
		{"a cancelled main target", []string{goal("", "ga", []string{"n1", "n2"}, "", ""), goal("", "gb", []string{"n3", "n4"}, "n3", "in/3")},
			mainOf(1), "main targets that could not be shown"},
		{"a replaced main target", []string{goal("", "ga", []string{"n1", "n2"}, "", ""), goal("", "gb", []string{"n3", "n4"}, "n4", "in/4")},
			mainOf(1), "main targets that could not be shown"},
		{"a main target that is no member", []string{goal("", "ga", []string{"n1"}, "n2", "in/2"), b},
			mainOf(0), "main targets that could not be shown"},
		{"undetermined", []string{goal("", "ga", []string{"n1", "n2"}, "undetermined", ""), b},
			mainOf(0), ""},
		{"a target created by an input of another goal", []string{goal("", "ga", []string{"n1", "n2", "n3"}, "", ""), goal("", "gb", []string{}, "", "")},
			func(c *checked) string {
				if fmt.Sprint(c.goals[0].Members) != "[t1 t2]" || unplacedOf(c) != 2 {
					return fmt.Sprintf("goals %+v unplaced %d", c.goals, unplacedOf(c))
				}
				return ""
			}, "first input does not relate to the goal"},
		{"a target in two goals", []string{goal("", "ga", []string{"n1"}, "", ""), goal("", "gb", []string{"n1", "n3"}, "", "")},
			func(c *checked) string {
				if fmt.Sprint(c.goals[1].Members) != "[t3]" {
					return fmt.Sprintf("goals %+v", c.goals)
				}
				return ""
			}, "already in another goal"},
		{"a member that is no target", []string{goal("", "ga", []string{"n1", "n9"}, "", ""), b},
			func(*checked) string { return "" }, `goal g1 names "n9", which is no target`},
		{"a goal named by an id no part found", []string{goal("ga", "", []string{"n1", "n2"}, "", ""), b},
			func(c *checked) string {
				if fmt.Sprint(c.goals[0].Members) != "[t1 t2]" {
					return fmt.Sprintf("goals %+v", c.goals)
				}
				return ""
			}, ""},
		{"a goal named by id and key", []string{goal("g1", "gz", []string{"n1"}, "", ""), b},
			func(*checked) string { return "" }, "names both goal g1 and key gz"},
		{"a goal key given twice", []string{goal("", "ga", []string{"n1"}, "", ""), goal("", "ga", []string{"n2"}, "", ""), b},
			func(c *checked) string {
				if fmt.Sprint(c.goals[0].Members) != "[t1]" {
					return fmt.Sprintf("goals %+v", c.goals)
				}
				return ""
			}, "goal key ga was given twice"},
		{"a goal key no relation started", []string{goal("", "gc", []string{"n1"}, "", ""), b},
			func(c *checked) string {
				if len(c.goals) != 2 || len(c.goals[0].Members) != 0 {
					return fmt.Sprintf("goals %+v", c.goals)
				}
				return ""
			}, "goal key gc, which no relation started, was left out"},
		{"a goal citing nodes that do not exist", []string{entry(goalEntry{Key: "ga", Members: []string{"n1"}, Cite: []string{"in/1", "nope"}}), b},
			func(c *checked) string {
				if fmt.Sprint(c.goals[0].Cite) != "[{in/1 <nil>}]" {
					return fmt.Sprintf("goals %+v", c.goals)
				}
				return ""
			}, "1 goal citations of nodes that do not exist"},
		{"a goal started by a relation alone", []string{},
			func(c *checked) string {
				if len(c.goals) != 2 || c.goals[0].Statement != "build and test it" || c.goals[1].Statement != "ship it" || unplacedOf(c) != 4 {
					return fmt.Sprintf("goals %+v", c.goals)
				}
				return ""
			}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := checkTargets(fiveInputs(), catalogue{}, goalsAnswer{inputs: inputs, goals: tc.goals}.raw(targets))
			if err != nil {
				t.Fatal(err)
			}
			if why := tc.check(c); why != "" {
				t.Error(why)
			}
			if (tc.note == "") != (c.note() == "") || !strings.Contains(c.note(), tc.note) {
				t.Errorf("note %q, want %q", c.note(), tc.note)
			}
		})
	}
}

// A target belongs to the goal of the input that first asked for it: its
// first create action, or for a target an earlier part found, the earliest
// input it cites. Each joins a goal once.
func TestATargetJoinsTheGoalOfItsFirstInput(t *testing.T) {
	twice := goalsAnswer{
		inputs: []string{
			input("in/1", act("n1", "create"), rel("new", "ga", "", "build it")),
			input("in/2", act("n1", "create"), rel("new", "gb", "", "ship it")),
		},
		goals: []string{goal("", "gb", []string{"n1"}, "", "")},
	}
	c, err := checkTargets(fiveInputs(), catalogue{}, twice.raw(newTarget("n1", "in/1")))
	if err != nil || len(c.goals[1].Members) != 0 || !strings.Contains(c.note(), "first input does not relate to the goal") {
		t.Fatalf("created twice: %v %+v %q", err, c.goals, c.note())
	}

	// A target created by a later input stays with its goal, whatever
	// earlier input it cites.
	later := goalsAnswer{
		inputs: []string{
			input("in/1", act("", "none"), rel("new", "ga", "", "build it")),
			input("in/2", act("n1", "create"), rel("new", "gb", "", "ship it")),
		},
		goals: []string{goal("", "gb", []string{"n1"}, "", "")},
	}
	early := `{"id":"","key":"n1","statement":"ship it","criteria":["done"],"drop_criteria":[],"status":"open","replaced_by":"","talks":[],"cite":["in/1","in/2"]}`
	c, err = checkTargets(fiveInputs(), catalogue{}, later.raw(early))
	if err != nil || fmt.Sprint(c.goals[1].Members) != "[t1]" {
		t.Fatalf("created later: %v %+v %q", err, c.goals, c.note())
	}

	nav := fiveInputs()
	nav.Assign(Assignment{Inputs: []string{"in/1", "in/2"}})
	first := goalsAnswer{inputs: []string{
		input("in/1", act("", "none"), rel("new", "ga", "", "build it")),
		input("in/2", act("n1", "create"), rel("unclear", "", "", "")),
	}}
	cited := `{"id":"","key":"n1","statement":"build it","criteria":["done"],"drop_criteria":[],"status":"open","replaced_by":"","talks":[],"cite":["c2","in/2","in/1","in/4"]}`
	c1, err := checkTargets(nav, catalogue{}, first.raw(cited))
	if err != nil || len(c1.goals) != 1 || len(c1.goals[0].Members) != 0 {
		t.Fatalf("first part: %v %+v", err, c1.goals)
	}
	nav.Assign(Assignment{Inputs: []string{"in/3"}})
	second := goalsAnswer{
		inputs: []string{input("in/3", act("t1", "steer"), rel("continue", "g1", "in/1", ""))},
		goals:  []string{goal("g1", "", []string{"t1", "t1"}, "", "")},
	}
	c2, err := checkTargets(nav, catalogue{targets: c1.targets, goals: c1.goals, relations: c1.relations}, second.raw(""))
	if err != nil || fmt.Sprint(c2.goals[0].Members) != "[t1]" || c2.note() != "" {
		t.Fatalf("found before: %v %+v %q", err, c2.goals, c2.note())
	}
}

// A later part of a targets scan goes on with the goals an earlier part
// found: its relations follow inputs that part answered, and its members join
// goals by id. A goal key that is an earlier goal's id starts nothing.
func TestALaterPartGoesOnWithTheGoalsFound(t *testing.T) {
	nav := fiveInputs()
	nav.Assign(Assignment{Inputs: []string{"in/1", "in/2"}})
	first := goalsAnswer{
		inputs: []string{input("in/1", act("n1", "create"), rel("new", "ga", "", "build it")), input("in/2", act("n1", "steer"), rel("continue", "ga", "in/1", ""))},
		goals:  []string{entry(goalEntry{Key: "ga", Members: []string{"n1"}, Main: "n1", Cite: []string{"in/1"}})},
	}
	c1, err := checkTargets(nav, catalogue{}, first.raw(newTarget("n1", "in/1")))
	if err != nil || c1.note() != "" {
		t.Fatalf("first part: %v %s", err, c1.note())
	}
	nav.Assign(Assignment{Inputs: []string{"in/3", "in/4"}})
	second := goalsAnswer{
		inputs: []string{
			input("in/3", act("n1", "create"), rel("adjust", "g1", "in/2", "build and test it")),
			input("in/4", act("n2", "create"), rel("new", "g1", "", "a new goal under an old id")),
		},
		goals: []string{
			entry(goalEntry{ID: "g1", Members: []string{"t1", "n1"}, Cite: []string{"in/1", "in/3"}}),
			goal("", "g1", []string{"n2"}, "", ""),
		},
	}
	// A part that is asked again starts from the same catalogue, which may
	// have room to grow in place.
	cat := catalogue{targets: c1.targets, goals: c1.goals, relations: c1.relations}
	for i := range cat.goals {
		g := &cat.goals[i]
		g.Members, g.Evolution, g.Cite = slices.Grow(g.Members, 4), slices.Grow(g.Evolution, 4), slices.Grow(g.Cite, 4)
	}
	c2, err := checkTargets(nav, cat, second.raw(newTarget("n1", "in/3")+","+newTarget("n2", "in/4")))
	if err != nil {
		t.Fatal(err)
	}
	again := goalsAnswer{inputs: slices.Clone(second.inputs), goals: []string{entry(goalEntry{ID: "g1", Cite: []string{"in/4"}})}}
	again.inputs[0] = input("in/3", act("n1", "create"), rel("adjust", "g1", "in/2", "build, test and ship it"))
	if _, err := checkTargets(nav, cat, again.raw(newTarget("n1", "in/3")+","+newTarget("n2", "in/4"))); err != nil {
		t.Fatal(err)
	}
	if got := relationsOf(c2); got["in/3"] != "adjust:g1:in/2" || got["in/4"] != "unclear::" {
		t.Fatalf("relations %v", got)
	}
	if g := c2.goals; len(g) != 1 || fmt.Sprint(g[0].Members) != "[t1 t2]" || len(g[0].Evolution) != 2 ||
		g[0].Statement != "build and test it" || g[0].Evolution[1].Statement != "build and test it" || fmt.Sprint(g[0].Cite) != "[{in/1 <nil>} {in/3 <nil>}]" {
		t.Fatalf("goals %+v", c2.goals)
	}
	// The earlier part's goals are its own: the later part changed a copy.
	if g := c1.goals[0]; fmt.Sprint(g.Members) != "[t1]" || len(g.Evolution) != 1 || len(g.Cite) != 1 {
		t.Fatalf("the first part's goal changed: %+v", g)
	}
	if !strings.Contains(c2.note(), "made unclear") || !strings.Contains(c2.note(), "goal key g1 is the id of a goal found so far") {
		t.Fatalf("note %s", c2.note())
	}
	// t1 was main as g1's only member; g1 gained t2 with no words of the
	// person on which is main, so its main is unknown.
	if c1.goals[0].Main != "t1" || c2.goals[0].Main != "" || !strings.Contains(c2.goals[0].MainReason, "gained members") ||
		!strings.Contains(c2.note(), "1 main targets that could not be shown were left undetermined") {
		t.Fatalf("main %q then %q: %q", c1.goals[0].Main, c2.goals[0].Main, c2.goals[0].MainReason)
	}
	// A key may be spelled like an id no goal has yet. It stays the
	// evaluator's name: here ga takes g2, and the key g2 the next id, g3.
	nav.Assign(Assignment{Inputs: []string{"in/3", "in/4", "in/5"}})
	spelled := goalsAnswer{
		inputs: []string{
			input("in/3", act("n1", "create"), rel("new", "ga", "", "update the website")),
			input("in/4", act("n2", "create"), rel("new", "g2", "", "write the release notes")),
			input("in/5", act("n2", "steer"), rel("continue", "g2", "in/4", "")),
		},
		goals: []string{goal("", "g2", []string{"n2"}, "", ""), goal("", "ga", []string{"n1"}, "", "")},
	}
	c3, err := checkTargets(nav, catalogue{targets: c1.targets, goals: c1.goals, relations: c1.relations}, spelled.raw(newTarget("n1", "in/3")+","+newTarget("n2", "in/4")))
	if got := relationsOf(c3); err != nil || got["in/3"] != "new:g2:" || got["in/4"] != "new:g3:" || got["in/5"] != "continue:g3:in/4" {
		t.Fatalf("relations %v %v", got, err)
	}
	if g := c3.goals; len(g) != 3 || fmt.Sprint(g[1].Members) != "[t2]" || g[1].Statement != "update the website" ||
		fmt.Sprint(g[2].Members) != "[t3]" || g[2].Statement != "write the release notes" || c3.note() != "" {
		t.Fatalf("goals %+v %q", g, c3.note())
	}
}
