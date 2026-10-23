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
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// deliveries is a conversation of three main talks, every step landed in
// order. The person asks in in/1; the agent answers in r1 and commits in s1.
// The person says it is wrong in in/2; the agent reverts in s2, answers again
// in r2 and commits again in s3, which starts a sub-agent that runs sa, and
// p1 never returned. The person thanks it in in/3, and the agent replies in
// r3; then the model API fails in x1, and the person interrupts in i1. l1
// is a step in no talk.
func deliveries() *Nav {
	d := deliveriesDoc()
	return NewNav(d, Compute(d, "claude-code/1", nil, nil), nil)
}

func deliveriesDoc() *sessionview.Conversation {
	row := uint64(0)
	node := func(id, kind, text string) sessionview.Node {
		row++
		n := sessionview.Node{ID: id, Kind: kind, Text: text, Ref: &sessionflow.Ref{Seq: 1, Row: row}}
		if kind == model.KindTool && id != "p1" {
			n.Result = "done"
		}
		return n
	}
	talk := func(id, stream string, children ...sessionview.Node) sessionview.Node {
		return sessionview.Node{ID: id, Kind: "talk", Stream: stream, Children: children}
	}
	// Each step is made in the order it landed.
	in1, r1, s1 := node("in/1", model.KindMessageExternal, "why does it fail? fix it"), node("r1", model.KindMessageAssistant, "the cause is the cache"),
		node("s1", model.KindTool, "git commit")
	in2, s2, r2, s3 := node("in/2", model.KindMessageExternal, "wrong, the cache is fine"), node("s2", model.KindTool, "git revert"),
		node("r2", model.KindMessageAssistant, "the cause is the clock"), node("s3", model.KindTool, "git commit")
	sa := node("sa", model.KindTool, "go test")
	p1 := node("p1", model.KindTool, "git push")
	in3, r3 := node("in/3", model.KindMessageExternal, "thanks"), node("r3", model.KindMessageAssistant, "glad to help")
	x1, i1 := node("x1", model.KindErrorAPI, "overloaded"), node("i1", model.KindControlInterrupt, "interrupted by the person")
	l1 := node("l1", model.KindTool, "a step in no talk")
	return &sessionview.Conversation{Conversation: "conv", Streams: []sessionview.Stream{{ID: "stream/main", Name: "main"}, {ID: "stream/a", Name: "a"}},
		Talks: []sessionview.Node{
			talk("talk/1", "main", in1, r1, s1),
			talk("talk/2", "main", in2, s2, r2, s3, p1),
			talk("talk/3", "main", in3, r3, x1, i1),
			talk("talk/a", "a", sa),
		},
		Loose:     []sessionview.Node{l1},
		Relations: []sessionview.Relation{{ID: "s3a", Type: model.RelStarts, From: "s3", To: "stream/a"}},
	}
}

// deliveryTarget is the target in/1 set, with a criterion the person dropped.
func deliveryTarget() sessionjudge.Target {
	return sessionjudge.Target{ID: "t1", Talks: []string{"talk/1", "talk/2"}, Cite: []sessionjudge.Cite{{Node: "in/1"}},
		Criteria: []sessionjudge.Criterion{{ID: "t1.c1", Text: "the cause named"}, {ID: "t1.c2", Text: "fixed"}, {ID: "t1.c3", Text: "dropped", Dropped: true}}}
}

func event(key, kind, artifact string, cites ...string) string {
	b, _ := json.Marshal(eventEntry{Key: key, Kind: kind, Artifact: artifact, Text: kind + " " + artifact, Cite: cites})
	return string(b)
}

func contradiction(ev string, conclusion bool, criteria []string, cites ...string) string {
	b, _ := json.Marshal(contradictionEntry{Event: ev, Conclusion: conclusion, Criteria: criteria, Text: "it was wrong", Cite: cites})
	return string(b)
}

// achieved checks an achievement answer for deliveryTarget with these events
// and contradictions.
func achieved(t *testing.T, events, contradictions []string) (*sessionjudge.Achievement, string) {
	t.Helper()
	raw := `{"achievements":[{"target":"t1","basis":"observed_result","criteria":[{"id":"t1.c1","state":"met","cite":["s3"]},{"id":"t1.c2","state":"met","cite":["s3"]}],` +
		`"claims":[],"events":[` + strings.Join(events, ",") + `],"contradictions":[` + strings.Join(contradictions, ",") + `],"note":"","cite":["s3"]}]}`
	c, err := checkAchievement(deliveries(), []sessionjudge.Target{deliveryTarget()}, json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	return c.frames[0].(*sessionjudge.Achievement), c.note()
}

func kinds(a *sessionjudge.Achievement) string {
	var out []string
	for _, e := range a.Events {
		out = append(out, e.ID+":"+e.Kind)
	}
	return strings.Join(out, " ")
}

// A result delivered, undone and delivered again keeps each event, with ids
// in recorded order, and the contradiction of the first delivery stays tied
// to it.
func TestEventsKeepTheResultsHistory(t *testing.T) {
	a, note := achieved(t,
		[]string{event("e1", "delivered", "revision", "s1"), event(" e2 ", "undone", "revision", "s2"), event(" e3 ", "delivered_again", "revision", "s3")},
		[]string{contradiction("e1", false, []string{"t1.c2", "t1.c2"}, "in/2"), contradiction(" e3", true, []string{"t1.c1"}, "in/3")})
	if note != "" {
		t.Fatalf("note %s", note)
	}
	if kinds(a) != "t1.e1:delivered t1.e2:undone t1.e3:delivered_again" || a.Events[0].Artifact != "revision" || a.Events[0].Text != "delivered revision" {
		t.Fatalf("events %+v", a.Events)
	}
	if len(a.Contradictions) != 2 || a.Contradictions[0].Event != "t1.e1" || fmt.Sprint(a.Contradictions[0].Criteria) != "[t1.c2]" ||
		a.Contradictions[1].Event != "t1.e3" || !a.Contradictions[1].Conclusion || a.Contradictions[1].Criteria != nil ||
		a.Contradictions[0].Text != "it was wrong" || a.Contradictions[0].Cite[0].Node != "in/2" {
		t.Fatalf("contradictions %+v", a.Contradictions)
	}
	// The level stays the record's: contradictions change the final score,
	// which the close computes, not the level.
	if a.Level != sessionjudge.LevelAchieved {
		t.Fatalf("level %s", a.Level)
	}
}

// A target met with no delivery kept says so, since nothing a source reports
// of it could reach its score; one whose only delivery is left out too.
func TestAResultMetWithNoDeliverySaysSo(t *testing.T) {
	want := "target t1 has criteria met but no delivery, so nothing a source reports of it can change its score"
	if _, note := achieved(t, nil, nil); note != want {
		t.Fatalf("no events: note %q", note)
	}
	if _, note := achieved(t, []string{event("e1", "delivered", "revision", "in/2")}, nil); !strings.Contains(note, want) {
		t.Fatalf("its only delivery left out: note %q", note)
	}
	if _, note := achieved(t, []string{event("e1", "delivered", "revision", "s1")}, nil); note != "" {
		t.Fatalf("a delivery kept: note %q", note)
	}
}

// Each event rule on its own: an event that nothing it cites shows, or that
// is out of order, is left out, and the rest are kept.
func TestEachEventRuleStandsAlone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []string
		want   string
	}{
		{"an answer delivered by its reply", []string{event("e1", "delivered", "answer", "in/1", "r1")}, "t1.e1:delivered"},
		{"an answer cited by a tool step only", []string{event("e1", "delivered", "answer", "s1")}, ""},
		{"a file cited by a reply only", []string{event("e1", "delivered", "file", "r1")}, ""},
		{"a file cited by a person's input only", []string{event("e1", "delivered", "file", "in/2")}, ""},
		{"a step that never returned", []string{event("e1", "delivered", "remote", "p1")}, ""},
		{"a file shown by a model API error", []string{event("e1", "delivered", "file", "x1")}, ""},
		{"an undoing shown by an interrupt", []string{event("e1", "delivered", "file", "s1"), event("e2", "undone", "file", "i1")}, "t1.e1:delivered"},
		{"an unknown kind", []string{event("e1", "shipped", "file", "s1")}, ""},
		{"an unknown artifact", []string{event("e1", "delivered", "package", "s1")}, ""},
		{"an undoing first", []string{event("e1", "undone", "file", "s2"), event("e2", "delivered", "file", "s3")}, "t1.e1:delivered"},
		{"a delivery again first", []string{event("e1", "delivered_again", "file", "s3")}, ""},
		{"a delivery in two parts", []string{event("e1", "delivered", "file", "s1"), event("e2", "delivered", "answer", "r2")}, "t1.e1:delivered t1.e2:delivered"},
		{"a delivery after an undoing", []string{event("e1", "delivered", "file", "s1"), event("e2", "undone", "file", "s2"), event("e3", "delivered", "file", "s3")},
			"t1.e1:delivered t1.e2:undone"},
		{"a part after a delivery again", []string{event("e1", "delivered", "file", "s1"), event("e2", "delivered_again", "file", "s2"), event("e3", "delivered", "answer", "r2")},
			"t1.e1:delivered t1.e2:delivered_again t1.e3:delivered"},
		{"an undoing of an undoing", []string{event("e1", "delivered", "file", "s1"), event("e2", "undone", "file", "s2"), event("e3", "undone", "file", "s3")},
			"t1.e1:delivered t1.e2:undone"},
		{"an undoing of a delivery again", []string{event("e1", "delivered", "file", "s1"), event("e2", "delivered_again", "file", "s2"), event("e3", "undone", "file", "s3")},
			"t1.e1:delivered t1.e2:delivered_again t1.e3:undone"},
		{"a delivery again after a delivery", []string{event("e1", "delivered", "answer", "r1"), event("e2", "delivered_again", "answer", "r2")},
			"t1.e1:delivered t1.e2:delivered_again"},
		{"out of recorded order", []string{event("e1", "delivered", "file", "s3"), event("e2", "undone", "file", "s2")}, "t1.e1:delivered"},
		{"two events on one step", []string{event("e1", "delivered", "file", "s1"), event("e2", "undone", "file", "s1")}, "t1.e1:delivered"},
		{"an answer placed by its earliest reply", []string{event("e1", "delivered", "answer", "r3", "r1"), event("e2", "delivered_again", "answer", "r2")},
			"t1.e1:delivered t1.e2:delivered_again"},
		{"placed by its earliest step", []string{event("e1", "delivered", "file", "s3", "s1"), event("e2", "undone", "file", "s2")}, "t1.e1:delivered t1.e2:undone"},
		{"a key named twice", []string{event("e1", "delivered", "file", "s1"), event("e1", "undone", "file", "s2")}, "t1.e1:delivered"},
		{"events with no key", []string{event("", "delivered", "file", "s1"), event("", "undone", "file", "s2")}, "t1.e1:delivered t1.e2:undone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, note := achieved(t, tc.events, nil)
			if kinds(a) != tc.want {
				t.Errorf("events %q, want %q", kinds(a), tc.want)
			}
			if left := len(tc.events) - len(a.Events); (left == 0) != (note == "") ||
				(left > 0 && !strings.Contains(note, fmt.Sprintf("%d events of target t1 were left out", left))) {
				t.Errorf("note %q", note)
			}
		})
	}
}

// Each contradiction rule on its own: a contradiction must name a delivery
// of the target, say what it disproves, and rest on evidence beyond the
// agent's words.
func TestEachContradictionRuleStandsAlone(t *testing.T) {
	events := []string{event("e1", "delivered", "file", "s1"), event("e2", "undone", "file", "s2"), event("e3", "delivered_again", "file", "s3")}
	for _, tc := range []struct {
		name, contradiction, want, note string
	}{
		{"named criteria", contradiction("e1", false, []string{"t1.c1"}, "in/2"), "t1.e1:[t1.c1]", ""},
		{"the conclusion", contradiction("e1", true, nil, "in/2"), "t1.e1:conclusion", ""},
		{"a tool result shows it", contradiction("e1", false, []string{"t1.c2"}, "s2"), "t1.e1:[t1.c2]", ""},
		{"an undoing named", contradiction("e2", true, nil, "in/2"), "", "1 contradictions of target t1 that name no delivery of it"},
		{"no event named", contradiction("e9", true, nil, "in/2"), "", "1 contradictions of target t1 that name no delivery of it"},
		{"nothing disproved", contradiction("e1", false, nil, "in/2"), "", "1 contradictions of target t1 that say nothing they disprove"},
		{"a criterion of no target", contradiction("e1", false, []string{"t1.c9", "t1.c1"}, "in/2"), "t1.e1:[t1.c1]", `names "t1.c9", which is none of its criteria`},
		{"a dropped criterion", contradiction("e1", false, []string{"t1.c3"}, "in/2"), "", `names "t1.c3", which is none of its criteria`},
		{"only the agent's words", contradiction("e1", true, nil, "r2"), "", "1 contradictions of target t1 without evidence beyond the agent's words"},
		{"only the input that asked", contradiction("e1", true, nil, "in/1"), "", "1 contradictions of target t1 without evidence beyond the agent's words"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, note := achieved(t, events, []string{tc.contradiction})
			var got []string
			for _, x := range a.Contradictions {
				if x.Conclusion {
					got = append(got, x.Event+":conclusion")
				} else {
					got = append(got, x.Event+":"+fmt.Sprint(x.Criteria))
				}
			}
			if strings.Join(got, " ") != tc.want {
				t.Errorf("contradictions %v, want %q", got, tc.want)
			}
			if (tc.note == "") != (note == "") || !strings.Contains(note, tc.note) {
				t.Errorf("note %q, want %q", note, tc.note)
			}
		})
	}
}

// An undoing names the criteria it took back, each once and each the
// target's; a delivery names none.
func TestAnUndoingNamesWhatItTookBack(t *testing.T) {
	ev := func(key, kind string, criteria []string, cites ...string) string {
		b, _ := json.Marshal(eventEntry{Key: key, Kind: kind, Artifact: "file", Criteria: criteria, Cite: cites})
		return string(b)
	}
	a, note := achieved(t, []string{ev("e1", "delivered", []string{"t1.c1"}, "s1"), ev("e2", "undone", []string{"t1.c2", "t1.c2", "t1.c9", "t1.c3"}, "s2")}, nil)
	if a.Events[0].Criteria != nil || fmt.Sprint(a.Events[1].Criteria) != "[t1.c2]" {
		t.Fatalf("events %+v", a.Events)
	}
	if !strings.Contains(note, `an undoing of target t1 names "t1.c9", which is none of its criteria`) ||
		!strings.Contains(note, `an undoing of target t1 names "t1.c3", which is none of its criteria`) {
		t.Fatalf("note %q", note)
	}
	// One that named criteria, none of them the target's, is not an undoing
	// of the whole result.
	a, note = achieved(t, []string{ev("e1", "delivered", nil, "s1"), ev("e2", "undone", []string{"t1.c9"}, "s2")}, nil)
	if kinds(a) != "t1.e1:delivered" || !strings.Contains(note, "1 events of target t1 were left out") {
		t.Fatalf("events %q; note %q", kinds(a), note)
	}
	// It names the deliveries it took back, each an earlier delivery; one
	// that names only others is left out, rather than read as taking back
	// every one.
	undo := func(undoes ...string) string {
		b, _ := json.Marshal(eventEntry{Key: "e3", Kind: "undone", Artifact: "file", Undoes: undoes, Cite: []string{"s2"}})
		return string(b)
	}
	parts := []string{event("e1", "delivered", "answer", "r1"), event("e2", "delivered", "file", "s1")}
	if a, note = achieved(t, append(slices.Clone(parts), undo("e2", "e2")), nil); len(a.Events) != 3 || fmt.Sprint(a.Events[2].Undoes) != "[t1.e2]" || note != "" {
		t.Fatalf("one part: %+v; %q", a.Events, note)
	}
	if a, note = achieved(t, append(slices.Clone(parts), undo("e9")), nil); len(a.Events) != 2 ||
		!strings.Contains(note, `an undoing of target t1 names "e9", which is no earlier delivery of it`) {
		t.Fatalf("no delivery: %+v; %q", a.Events, note)
	}
}

// A delivery again names the earlier deliveries it corrects, each an earlier
// delivery of the target; one that names only others is left out, rather than
// read as replacing every one.
func TestADeliveryAgainNamesWhatItReplaces(t *testing.T) {
	again := func(replaces ...string) string {
		b, _ := json.Marshal(eventEntry{Key: "e4", Kind: "delivered_again", Artifact: "file", Replaces: replaces, Cite: []string{"s3"}})
		return string(b)
	}
	before := []string{event("e1", "delivered", "answer", "r1"), event("e2", "delivered", "file", "s1"), event("e3", "undone", "file", "s2")}
	for _, tc := range []struct {
		name     string
		replaces []string
		want     string
		note     string
	}{
		{"none, so every one", nil, "[]", ""},
		{"one part", []string{"e1", " e1"}, "[t1.e1]", ""},
		{"an undoing and a part", []string{"e3", "e2"}, "[t1.e2]", `a delivery again of target t1 names "e3", which is no earlier delivery of it`},
		{"only what is no delivery", []string{"e9"}, "left out", "1 events of target t1 were left out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, note := achieved(t, append(slices.Clone(before), again(tc.replaces...)), nil)
			got := "left out"
			if len(a.Events) == 4 {
				got = fmt.Sprint(a.Events[3].Replaces)
			}
			if got != tc.want || (tc.note == "") != (note == "") || !strings.Contains(note, tc.note) {
				t.Errorf("replaces %s, want %s; note %q", got, tc.want, note)
			}
		})
	}
}
