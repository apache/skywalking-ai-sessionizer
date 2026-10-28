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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
)

// A goals file lists the person's goals in their own words, each trimmed,
// with the file's digest and text kept. A file that lists none, or a goal
// that is empty, too long or given twice, is not read.
func TestAGoalsFileListsThePersonsGoals(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "goals.yaml")
	text := "goals:\n  - \" Finish the 0.4.0 release \"\n  - Remove the website step\n"
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := ReadGoals(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if l.File != "goals.yaml" || len(l.SHA256) != 64 || fmt.Sprint(l.goals) != "[Finish the 0.4.0 release Remove the website step]" || l.text != text {
		t.Fatalf("labels %+v", l)
	}
	for name, tc := range map[string]struct{ text, want string }{
		"no goal":              {"goals: []\n", "lists no goal"},
		"inputs, not goals":    {"labels:\n  - input: in/2\n", "lists no goal"},
		"an empty goal":        {"goals:\n  - ship it\n  - \" \"\n", "goal 2 is empty"},
		"a goal too long":      {"goals:\n  - " + strings.Repeat("x", goalBytes+1) + "\n", "goal 1 is longer than"},
		"a goal given twice":   {"goals:\n  - ship it\n  - test it\n  - \" ship it\"\n", "goal 3 is goal 1 again"},
		"not text":             {"goals:\n  - ship it\n# \xff\n", "is not UTF-8 text"},
		"too large":            {"goals:\n  - ship it\n#" + strings.Repeat("x", goalsFileBytes) + "\n", "is larger than 64 KiB"},
		"not YAML":             {"goals: [\n", "goals.yaml"},
		"goals that are lists": {"goals:\n  - [a, b]\n", "goals.yaml"},
	} {
		if err := os.WriteFile(path, []byte(tc.text), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadGoals(path, ""); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

// fourTargets are targets the judge grouped in two goals, g1 holding t1 and
// t2 with t1 as its main, g2 holding t3, and t4 in no goal; t2 was
// cancelled. The person's inputs of deliveries are in/1, in/2 and in/3.
func fourTargets() ([]sessionjudge.Target, []sessionjudge.Goal) {
	return []sessionjudge.Target{{ID: "t1", Status: sessionjudge.TargetClosed}, {ID: "t2", Status: sessionjudge.TargetCancelled},
			{ID: "t3", Status: sessionjudge.TargetClosed}, {ID: "t4", Status: sessionjudge.TargetClosed}},
		[]sessionjudge.Goal{{ID: "g1", Members: []string{"t1", "t2"}, Main: "t1"}, {ID: "g2", Members: []string{"t3"}, Main: "t3"}}
}

func placeAt(target, goal string, cites ...string) map[string]any {
	if cites == nil {
		cites = []string{}
	}
	return map[string]any{"target": target, "goal": goal, "note": "", "cite": cites}
}

func workedOn(goal string, yes bool, cites ...string) map[string]any {
	if cites == nil {
		cites = []string{}
	}
	return map[string]any{"goal": goal, "worked": yes, "note": "", "cite": cites}
}

// regrouped checks a regroup answer over four targets and four of the
// person's goals, and gives what it found.
func regrouped(t *testing.T, targets []map[string]any, goals []map[string]any) (map[string]*sessionjudge.Goal, map[string]*sessionjudge.Placement,
	*sessionjudge.Agreement, *checked) {
	t.Helper()
	ts, gs := fourTargets()
	raw, _ := json.Marshal(map[string]any{"targets": targets, "goals": goals})
	c, err := checkRegroup(deliveries(), []string{"build it", "test it", "ship it", "write the note"}, ts, gs, raw)
	if err != nil {
		t.Fatal(err)
	}
	lg, pl := map[string]*sessionjudge.Goal{}, map[string]*sessionjudge.Placement{}
	var agreement *sessionjudge.Agreement
	for _, f := range c.frames {
		switch x := f.(type) {
		case *sessionjudge.Goal:
			lg[x.ID] = x
		case *sessionjudge.Placement:
			pl[x.Target] = x
		case *sessionjudge.Agreement:
			agreement = x
		}
	}
	return lg, pl, agreement, c
}

// words prints its arguments with a space between each.
func words(args ...any) string { return strings.TrimSuffix(fmt.Sprintln(args...), "\n") }

func classes(lg map[string]*sessionjudge.Goal) string {
	return fmt.Sprintf("%s %s %s %s", lg["P1"].Class, lg["P2"].Class, lg["P3"].Class, lg["P4"].Class)
}

// Each class of the person's goals on its own: the goal's targets in one
// goal of the judge it shares with no other, in several, or in one it
// shares; none, though the conversation worked on it; none, and nothing
// worked on it. A target the judge put in no goal stands alone.
func TestEachClassOfAPersonsGoalStandsAlone(t *testing.T) {
	notWorked := []map[string]any{workedOn("P1", false), workedOn("P2", false), workedOn("P3", false), workedOn("P4", false)}
	for _, tc := range []struct {
		name    string
		targets []map[string]any
		goals   []map[string]any
		want    string
	}{
		{"each in one goal of the judge", []map[string]any{placeAt("t1", "P1", "in/1"), placeAt("t2", "P1", "in/1"), placeAt("t3", "P2", "in/3"),
			placeAt("t4", "P3", "in/3")}, notWorked, "agree agree agree absent"},
		{"one spread over two goals of the judge", []map[string]any{placeAt("t1", "P1", "in/1"), placeAt("t2", "P1", "in/1"), placeAt("t3", "P1", "in/3"),
			placeAt("t4", "P2", "in/3")}, notWorked, "split agree absent absent"},
		{"two in one goal of the judge", []map[string]any{placeAt("t1", "P1", "in/1"), placeAt("t2", "P2", "in/2"), placeAt("t3", "P3", "in/3"),
			placeAt("t4", "")}, notWorked, "merged merged agree absent"},
		{"with a target in no goal of the judge", []map[string]any{placeAt("t1", "P1", "in/1"), placeAt("t2", "P1", "in/1"), placeAt("t3", "P2", "in/3"),
			placeAt("t4", "P2", "in/3")}, notWorked, "agree split absent absent"},
		{"worked on, and no target", []map[string]any{placeAt("t1", "P1", "in/1"), placeAt("t2", "P1", "in/1"), placeAt("t3", "P2", "in/3"),
			placeAt("t4", "P3", "in/3")}, []map[string]any{workedOn("P1", true, "in/1"), workedOn("P2", true, "in/3"), workedOn("P3", true, "in/3"),
			workedOn("P4", true, "in/2")}, "agree agree agree missed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lg, _, agreement, c := regrouped(t, tc.targets, tc.goals)
			if got := classes(lg); got != tc.want || c.note() != "" {
				t.Fatalf("classes %s, want %s; %q", got, tc.want, c.note())
			}
			n := 0
			for _, k := range agreement.Classes {
				n += k
			}
			if agreement.Goals != 4 || n != 4 {
				t.Fatalf("agreement %+v", agreement)
			}
		})
	}
	// What the answer said of a goal that has targets gives way to them.
	lg, _, _, _ := regrouped(t, []map[string]any{placeAt("t1", "P1", "in/1"), placeAt("t2", "P1", "in/1"), placeAt("t3", "P2", "in/3"),
		placeAt("t4", "")}, notWorked)
	if !lg["P1"].Worked || lg["P3"].Worked || fmt.Sprint(lg["P1"].Judge) != "[g1]" || fmt.Sprint(lg["P1"].Members) != "[t1 t2]" {
		t.Fatalf("P1 %+v P3 %+v", lg["P1"], lg["P3"])
	}
}

// Targets the judge put in no goal each stand alone: two of them in two of
// the person's goals agree, rather than sharing a goal of the judge.
func TestTargetsInNoGoalOfTheJudgeStandAlone(t *testing.T) {
	ts, _ := fourTargets()
	raw, _ := json.Marshal(map[string]any{"targets": []map[string]any{placeAt("t1", "P1", "in/1"), placeAt("t2", ""), placeAt("t3", ""),
		placeAt("t4", "P2", "in/3")}, "goals": []map[string]any{workedOn("P1", true, "in/1"), workedOn("P2", true, "in/3")}})
	c, err := checkRegroup(deliveries(), []string{"build it", "ship it"}, ts, nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	p1, p2 := c.frames[0].(*sessionjudge.Goal), c.frames[1].(*sessionjudge.Goal)
	if p1.Class != sessionjudge.CompareAgree || p2.Class != sessionjudge.CompareAgree || len(p1.Judge) != 0 {
		t.Fatalf("P1 %+v P2 %+v", p1, p2)
	}
}

// A regroup reads the conversation through the window its run recorded.
func TestARegroupTakesItsRunsWindow(t *testing.T) {
	w, err := windowOf(&sessionjudge.Window{From: "2026-10-20T10:00:00Z", To: "2026-10-20T11:00:00Z", Talks: []string{"talk/2"}})
	if err != nil || w.From.UTC().Format("15:04") != "10:00" || w.To.UTC().Format("15:04") != "11:00" || w.FromTalk != "" {
		t.Fatalf("by time: %+v %v", w, err)
	}
	if w, err = windowOf(&sessionjudge.Window{FromTalk: "talk/2", ToTalk: "talk/3"}); err != nil || w.FromTalk != "talk/2" || w.ToTalk != "talk/3" || !w.From.IsZero() {
		t.Fatalf("by talk: %+v %v", w, err)
	}
	if w, err = windowOf(nil); err != nil || !w.IsZero() {
		t.Fatalf("none: %+v %v", w, err)
	}
	if _, err = windowOf(&sessionjudge.Window{From: "yesterday"}); err == nil {
		t.Fatal("a time that is not one was read")
	}
}

// Each rule of an answer on its own: a target placed in a goal cites an
// input of the person that shows it, and names a goal the person gave; a
// target or a goal is answered once; one left out is unknown; a goal worked
// on cites the person's input that asked for it.
func TestEachRegroupRuleStandsAlone(t *testing.T) {
	all := func(over ...map[string]any) []map[string]any {
		base := []map[string]any{placeAt("t1", "P1", "in/1"), placeAt("t2", "P1", "in/1"), placeAt("t3", "P2", "in/3"), placeAt("t4", "")}
		for _, o := range over {
			for i := range base {
				if base[i]["target"] == o["target"] {
					base[i] = o
				}
			}
		}
		return base
	}
	goals := []map[string]any{workedOn("P1", true, "in/1"), workedOn("P2", true, "in/3"), workedOn("P3", false), workedOn("P4", false)}
	unshown := "1 placements, or goals worked on, that no input of the person shows"
	twice := "1 entries for no target or goal of this regroup, or for one answered already"
	for _, tc := range []struct {
		name    string
		targets []map[string]any
		goals   []map[string]any
		check   func(lg map[string]*sessionjudge.Goal, pl map[string]*sessionjudge.Placement, a *sessionjudge.Agreement) string
		want    string
		note    string
	}{
		{"a goal the person did not give", all(placeAt("t3", "P9", "in/3")), goals, func(_ map[string]*sessionjudge.Goal,
			pl map[string]*sessionjudge.Placement, a *sessionjudge.Agreement) string {
			return words(pl["t3"].Unknown, pl["t3"].Goal, a.Unknown)
		}, "true  [t3]", unshown},
		{"a placement that cites a step, not the person", all(placeAt("t3", "P2", "s3")), goals, func(_ map[string]*sessionjudge.Goal,
			pl map[string]*sessionjudge.Placement, a *sessionjudge.Agreement) string {
			return words(pl["t3"].Unknown, pl["t3"].Goal, a.Unknown)
		}, "true  [t3]", unshown},
		{"a placement that cites nothing", all(placeAt("t3", "P2")), goals, func(_ map[string]*sessionjudge.Goal,
			pl map[string]*sessionjudge.Placement, a *sessionjudge.Agreement) string {
			return words(pl["t3"].Unknown, pl["t3"].Goal, a.Unknown)
		}, "true  [t3]", unshown},
		{"a target in none of the goals", all(), goals, func(_ map[string]*sessionjudge.Goal, pl map[string]*sessionjudge.Placement,
			a *sessionjudge.Agreement) string {
			return words(pl["t4"].Unknown, pl["t4"].Goal, a.Extra)
		}, "false  [t4]", ""},
		{"a target left out", all()[:3], goals, func(_ map[string]*sessionjudge.Goal, pl map[string]*sessionjudge.Placement,
			a *sessionjudge.Agreement) string {
			return words(pl["t4"].Unknown, pl["t4"].Goal, a.Unknown)
		}, "true  [t4]", "1 targets were not placed and are unknown"},
		{"a target twice", append(all(), placeAt("t1", "P2", "in/3")), goals, func(_ map[string]*sessionjudge.Goal,
			pl map[string]*sessionjudge.Placement, _ *sessionjudge.Agreement) string {
			return words(pl["t1"].Unknown, pl["t1"].Goal)
		}, "false P1", twice},
		{"a target this run does not have", append(all(), placeAt("t9", "P1", "in/1")), goals, func(_ map[string]*sessionjudge.Goal,
			pl map[string]*sessionjudge.Placement, _ *sessionjudge.Agreement) string {
			return words(pl["t9"] == nil)
		}, "true", twice},
		{"a goal left out", all(), goals[:3], func(lg map[string]*sessionjudge.Goal, _ map[string]*sessionjudge.Placement,
			_ *sessionjudge.Agreement) string {
			return words(lg["P4"].Class, lg["P4"].Worked)
		}, "unknown false", "1 of the person's goals were not answered and are unknown"},
		{"a goal twice", all(), append(goals, workedOn("P4", true, "in/2")), func(lg map[string]*sessionjudge.Goal,
			_ map[string]*sessionjudge.Placement, _ *sessionjudge.Agreement) string {
			return words(lg["P4"].Class, lg["P4"].Worked)
		}, "absent false", twice},
		{"a goal worked on, shown by no input of the person", all(), []map[string]any{goals[0], goals[1], goals[2], workedOn("P4", true, "r1")},
			func(lg map[string]*sessionjudge.Goal, _ map[string]*sessionjudge.Placement, _ *sessionjudge.Agreement) string {
				return words(lg["P4"].Class, lg["P4"].Worked)
			}, "unknown false", unshown},
		{"a goal worked on, shown", all(), []map[string]any{goals[0], goals[1], goals[2], workedOn("P4", true, "in/2")},
			func(lg map[string]*sessionjudge.Goal, _ map[string]*sessionjudge.Placement, _ *sessionjudge.Agreement) string {
				return words(lg["P4"].Class, lg["P4"].Worked)
			}, "missed true", ""},
		// A goal a target was put in, with nothing that shows it, may have
		// that target: it is not missed.
		{"a goal a placement could not show", all(placeAt("t4", "P4", "s3")), []map[string]any{goals[0], goals[1], goals[2], workedOn("P4", true, "in/2")},
			func(lg map[string]*sessionjudge.Goal, _ map[string]*sessionjudge.Placement, _ *sessionjudge.Agreement) string {
				return words(lg["P4"].Class, lg["P4"].Worked)
			}, "unknown true", unshown},
		// A target left out may serve a goal worked on: it is not missed.
		{"a goal worked on, and a target left out", all()[:3], []map[string]any{goals[0], goals[1], goals[2], workedOn("P4", true, "in/2")},
			func(lg map[string]*sessionjudge.Goal, _ map[string]*sessionjudge.Placement, _ *sessionjudge.Agreement) string {
				return words(lg["P4"].Class, lg["P4"].Worked)
			}, "unknown true", "1 targets were not placed and are unknown"},
		// A target the answer could not place may serve a goal worked on.
		{"a goal worked on, and a target not placed", all(placeAt("t3", "P9", "in/3")), []map[string]any{goals[0], goals[1], goals[2], workedOn("P4", true, "in/2")},
			func(lg map[string]*sessionjudge.Goal, _ map[string]*sessionjudge.Placement, _ *sessionjudge.Agreement) string {
				return words(lg["P4"].Class, lg["P4"].Worked)
			}, "unknown true", unshown},
		// A target left out, or one the answer could not place, may serve a
		// goal nothing else worked on: that nothing worked on it is not
		// shown, so it is not absent.
		{"a goal not worked on, and a target left out", all()[:3], goals,
			func(lg map[string]*sessionjudge.Goal, _ map[string]*sessionjudge.Placement, _ *sessionjudge.Agreement) string {
				return words(lg["P4"].Class, lg["P4"].Asserted)
			}, "unknown false", "1 targets were not placed and are unknown"},
		{"a goal not worked on, and a target not placed", all(placeAt("t3", "P9", "in/3")), goals,
			func(lg map[string]*sessionjudge.Goal, _ map[string]*sessionjudge.Placement, _ *sessionjudge.Agreement) string {
				return words(lg["P4"].Class, lg["P4"].Asserted)
			}, "unknown false", unshown},
		// Absent, with every target placed, is the evaluator's judgment.
		{"a goal absent", all(), goals,
			func(lg map[string]*sessionjudge.Goal, _ map[string]*sessionjudge.Placement, _ *sessionjudge.Agreement) string {
				return words(lg["P4"].Class, lg["P4"].Asserted)
			}, "absent true", ""},
		// A decision left out is none: a target with no goal, or a goal with
		// no worked, is not answered.
		{"a target with no goal given", all(map[string]any{"target": "t3", "note": "", "cite": []string{"in/3"}}), goals,
			func(_ map[string]*sessionjudge.Goal, pl map[string]*sessionjudge.Placement, a *sessionjudge.Agreement) string {
				return words(pl["t3"].Unknown, pl["t3"].Goal, a.Unknown)
			}, "true  [t3]", twice},
		{"a goal with no worked given", all(), []map[string]any{goals[0], goals[1], goals[2], {"goal": "P4", "note": "", "cite": []string{}}},
			func(lg map[string]*sessionjudge.Goal, _ map[string]*sessionjudge.Placement, _ *sessionjudge.Agreement) string {
				return words(lg["P4"].Class, lg["P4"].Worked)
			}, "unknown false", twice},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lg, pl, a, c := regrouped(t, tc.targets, tc.goals)
			if got := tc.check(lg, pl, a); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
			if (tc.note == "") != (c.note() == "") || !strings.Contains(c.note(), tc.note) {
				t.Errorf("note %q, want %q", c.note(), tc.note)
			}
		})
	}
}

// A person's goal's main target is its only member still wanted, or the
// main target of a goal of the judge that holds the same targets.
func TestThePersonsGoalTakesAMainOnlyItsTargetsShow(t *testing.T) {
	goals := []map[string]any{workedOn("P1", true, "in/1"), workedOn("P2", false), workedOn("P3", false), workedOn("P4", false)}
	for _, tc := range []struct {
		name    string
		targets []map[string]any
		want    string
	}{
		{"two wanted, in two goals of the judge", []map[string]any{placeAt("t1", "P1", "in/1"), placeAt("t2", "P1", "in/1"), placeAt("t3", "P1", "in/3"),
			placeAt("t4", "P2", "in/3")}, ""},
		{"the only member still wanted", []map[string]any{placeAt("t1", "P2", "in/1"), placeAt("t2", "P1", "in/1"), placeAt("t3", "P1", "in/3"),
			placeAt("t4", "P2", "in/3")}, "t3"},
		{"two wanted, one in no goal of the judge", []map[string]any{placeAt("t1", "P1", "in/1"), placeAt("t2", "P2", "in/2"),
			placeAt("t3", "P3", "in/3"), placeAt("t4", "P1", "in/3")}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lg, _, _, _ := regrouped(t, tc.targets, goals)
			if lg["P1"].Main != tc.want {
				t.Errorf("main %q, want %q", lg["P1"].Main, tc.want)
			}
		})
	}
	// Two wanted targets that are a goal of the judge take its main target.
	ts, gs := fourTargets()
	ts[1].Status = sessionjudge.TargetClosed
	raw, _ := json.Marshal(map[string]any{"targets": []map[string]any{placeAt("t1", "P1", "in/1"), placeAt("t2", "P1", "in/1"), placeAt("t3", ""),
		placeAt("t4", "")}, "goals": goals})
	c, err := checkRegroup(deliveries(), []string{"build it", "test it", "ship it", "write the note"}, ts, gs, raw)
	if err != nil {
		t.Fatal(err)
	}
	if g := c.frames[0].(*sessionjudge.Goal); g.Main != "t1" {
		t.Fatalf("main %q", g.Main)
	}
}

// A regroup writes the person's goals first, in order, then where each
// target went, then the agreement, and covers every target it placed.
func TestARegroupWritesGoalsThenPlacements(t *testing.T) {
	_, _, _, c := regrouped(t, []map[string]any{placeAt("t1", "P1", "in/1"), placeAt("t2", "P1", "in/1"), placeAt("t3", "P2", "in/3")},
		[]map[string]any{workedOn("P1", true, "in/1"), workedOn("P2", true, "in/3"), workedOn("P3", false), workedOn("P4", false)})
	var kinds []string
	for _, f := range c.frames {
		switch x := f.(type) {
		case *sessionjudge.Goal:
			kinds = append(kinds, x.ID)
		case *sessionjudge.Placement:
			kinds = append(kinds, x.Target)
		case *sessionjudge.Agreement:
			kinds = append(kinds, "agreement")
		}
	}
	if strings.Join(kinds, " ") != "P1 P2 P3 P4 t1 t2 t3 t4 agreement" || *c.coverage.Targets != 4 || *c.coverage.TargetsJudged != 3 {
		t.Fatalf("frames %v, coverage %d of %d", kinds, *c.coverage.TargetsJudged, *c.coverage.Targets)
	}
	if s := personGoalsText([]string{"build it", "test it"}); !strings.Contains(s, "- P1: build it\n- P2: test it\n") {
		t.Fatalf("goals text %q", s)
	}
}
