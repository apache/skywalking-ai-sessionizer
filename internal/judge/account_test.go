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
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// long is a tool result the document clips: what follows the clip is read
// from its record.
var long = strings.Repeat("ok ", 1000) + "FAIL TestCache"

// accounted is deliveries with s2 failed and s3's result clipped, its whole
// text in a record.
func accounted() *Nav {
	d := deliveriesDoc()
	var walk func(ns []sessionview.Node)
	walk = func(ns []sessionview.Node) {
		for i := range ns {
			n := &ns[i]
			switch n.ID {
			case "s2":
				failed := true
				n.Failed, n.Result = &failed, "revert failed: conflict in cache.go"
			case "s3":
				n.Result, n.ResultBytes, n.Refs = long[:2000], len(long), []sessionflow.Ref{{Seq: 9, Row: 1}}
			}
			walk(n.Children)
		}
	}
	walk(d.Talks)
	read := func(seq, row uint64) (*sessiondata.Record, error) {
		if seq == 9 && row == 1 {
			return &sessiondata.Record{Parts: []sessiondata.Part{{Text: long}}}, nil
		}
		return nil, fmt.Errorf("no record %d/%d", seq, row)
	}
	return NewNav(d, Compute(d, "claude-code/1", nil, nil), read)
}

type criterionAnswer struct {
	ID           string             `json:"id"`
	State        string             `json:"state"`
	Delivered    bool               `json:"delivered"`
	Reason       string             `json:"reason"`
	Observations []observationEntry `json:"observations"`
	Cite         []string           `json:"cite"`
}

// accountOf checks an achievement answer for deliveryTarget with these
// criteria, or with none given, criteria that need no note, and this account.
func accountOf(t *testing.T, criteria []criterionAnswer, account accountEntry) (*sessionjudge.Achievement, string) {
	t.Helper()
	return accountFor(t, deliveryTarget(), criteria, account)
}

func accountFor(t *testing.T, target sessionjudge.Target, criteria []criterionAnswer, account accountEntry) (*sessionjudge.Achievement, string) {
	t.Helper()
	if criteria == nil {
		criteria = []criterionAnswer{{ID: "t1.c1", State: "met", Cite: []string{"s3"}}, {ID: "t1.c2", State: "unknown", Cite: []string{}}}
	}
	cr, _ := json.Marshal(criteria)
	ac, _ := json.Marshal(account)
	raw := `{"achievements":[{"target":"t1","basis":"observed_result","criteria":` + string(cr) + `,"claims":[],"events":[{"key":"e1","kind":"delivered","artifact":"revision","criteria":[],"text":"committed","cite":["s3"]}],"contradictions":[],"rework":[],` +
		`"account":` + string(ac) + `,"note":"","cite":["s3"]}]}`
	c, err := checkAchievement(accounted(), []sessionjudge.Target{target}, json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	return c.frames[0].(*sessionjudge.Achievement), c.note()
}

func said(text string, cites ...string) saidEntry { return saidEntry{Text: text, Cite: cites} }

// nodes names the steps some citations cite.
func nodes(cites []sessionjudge.Cite) string {
	var out []string
	for _, c := range cites {
		out = append(out, c.Node)
	}
	return strings.Join(out, " ")
}

// A question's account says what was asked, what was checked and ruled out,
// and the answer, each on what shows it.
func TestAnAccountSaysHowAQuestionWasAnswered(t *testing.T) {
	a, note := accountOf(t, nil, accountEntry{Answer: answerEntry{
		Asked:    said("why does it fail", "in/1", "r1"),
		Checked:  []triedEntry{{What: "the tests", Found: "TestCache fails", Cite: []string{"s3"}}, {What: "the cache", Found: "it is fine", Cite: []string{"r2"}}},
		RuledOut: []triedEntry{{What: "a merge conflict", Found: "the revert failed on one", Limits: "only cache.go", Cite: []string{"s2"}}},
		Answered: said("the clock", "r2"),
	}})
	ans := a.Account.Answer
	if ans == nil || nodes(ans.Asked.Cite) != "in/1" || len(ans.Checked) != 1 || ans.Checked[0].What != "the tests" ||
		len(ans.RuledOut) != 1 || ans.RuledOut[0].Limits != "only cache.go" || ans.Answered.Text != "the clock" || ans.Answered.Cite[0].Node != "r2" {
		t.Fatalf("answer %+v", ans)
	}
	if !strings.Contains(note, "1 parts of the account of target t1 that nothing they cite shows were left out") {
		t.Fatalf("note %s", note)
	}
}

// Each rule of an account on its own.
func TestEachAccountRuleStandsAlone(t *testing.T) {
	remote := func(request, response, confirmed saidEntry) accountEntry {
		return accountEntry{Remote: []remoteEntry{{System: "GitHub", Operation: "push the fix", Via: "git push", Request: request, Response: response, Confirmed: confirmed}}}
	}
	for _, tc := range []struct {
		name    string
		account accountEntry
		want    string
		note    string
	}{
		{"no account", accountEntry{}, "<nil>", ""},
		{"an answer by no reply", accountEntry{Answer: answerEntry{Answered: said("the clock", "s3")}}, "<nil>", "the answer of target t1 cites no reply of the agent, left out"},
		{"checks with no answer", accountEntry{Answer: answerEntry{Checked: []triedEntry{{What: "x", Cite: []string{"s3"}}}}}, "<nil>", "cites no reply"},
		{"a check by a step that never returned", accountEntry{Answer: answerEntry{Checked: []triedEntry{{What: "x", Cite: []string{"p1"}}}, Answered: said("y", "r2")}},
			"answer checked 0", "1 parts of the account"},
		{"an operation, its response and its confirmation", remote(said("push", "s3"), said("pushed", "s3"), said("the test passed", "sa")), "remote s3 s3 sa", ""},
		{"an operation sent by a reply", remote(said("push", "r2"), saidEntry{}, saidEntry{}), "", "1 parts of the account"},
		{"an operation that never returned", remote(said("push", "p1"), saidEntry{}, saidEntry{}), "remote p1 - -", ""},
		{"a response that never came", remote(said("push", "p1"), said("pushed", "p1"), saidEntry{}), "remote p1 - -", "1 parts of the account"},
		{"a confirmation by the person later", remote(said("push", "s3"), saidEntry{}, said("thanks", "in/3")), "remote s3 - in/3", ""},
		{"a confirmation by the input that asked", remote(said("push", "s3"), saidEntry{}, said("fix it", "in/1")), "remote s3 - -", "1 parts of the account"},
		{"a confirmation before the operation", remote(said("push", "s3"), saidEntry{}, said("committed", "s1")), "remote s3 - -", "1 parts of the account"},
		{"a confirmation by the agent's words", remote(said("push", "s3"), saidEntry{}, said("done", "r3")), "remote s3 - -", "1 parts of the account"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, note := accountOf(t, nil, tc.account)
			got := "<nil>"
			switch {
			case a.Account == nil:
			case a.Account.Answer != nil:
				got = fmt.Sprintf("answer checked %d", len(a.Account.Answer.Checked))
			case len(a.Account.Remote) == 0:
				got = ""
			default:
				r := a.Account.Remote[0]
				got = "remote " + r.Request.Cite[0].Node
				for _, s := range []*sessionjudge.Said{r.Response, r.Confirmed} {
					if s == nil {
						got += " -"
					} else {
						got += " " + s.Cite[0].Node
					}
				}
			}
			if tc.want == "" && a.Account == nil {
				got = ""
			}
			if got != tc.want {
				t.Errorf("account %s, want %s", got, tc.want)
			}
			if (tc.note == "") != (note == "") || !strings.Contains(note, tc.note) {
				t.Errorf("note %q, want %q", note, tc.note)
			}
		})
	}
}

// A criterion keeps the evaluator's reason and what each cited step shows:
// an excerpt exactly as recorded, read whole from the record past the
// document's clip, and the step's status as the record says it.
func TestACriterionKeepsWhatItsStepsShow(t *testing.T) {
	a, note := accountOf(t, []criterionAnswer{
		{ID: "t1.c1", State: "met", Reason: "the test names the clock", Cite: []string{"s3", "s2", "p1", "r2", "x1", "i1"}, Observations: []observationEntry{
			{Node: "s3", Excerpt: "FAIL TestCache", Interpretation: "the cache test fails, so the cache is not the cause"},
			{Node: "s2", Excerpt: "conflict in cache.go", Interpretation: "the revert did not apply"},
			{Node: "p1", Interpretation: "the push never came back"},
			{Node: "r2", Excerpt: "the clock", Interpretation: "the agent named the clock"},
			{Node: "x1", Interpretation: "the model was overloaded"},
			{Node: "i1", Interpretation: "the person stopped it"},
		}},
		{ID: "t1.c2", State: "met", Reason: "it is fixed", Cite: []string{"r2"}},
	}, accountEntry{})
	if note != "1 criteria or claims without evidence beyond the agent's words were weakened" {
		t.Fatalf("note %s", note)
	}
	c1, c2 := a.Criteria[0], a.Criteria[1]
	var got []string
	for _, o := range c1.Observations {
		got = append(got, o.Node+":"+o.Status)
	}
	if c1.Reason != "the test names the clock" || c1.GivenReason != "" || strings.Join(got, " ") != "s3:returned s2:failed p1:no_result r2: x1:error i1:interrupted" ||
		c1.Observations[0].Interpretation != "the cache test fails, so the cache is not the cause" {
		t.Fatalf("c1 %+v", c1)
	}
	// A state asz changed carries asz's reason, the evaluator's kept apart.
	if c2.State != sessionjudge.CheckUnknown || c2.Reason != "nothing it cites shows it" || c2.GivenReason != "it is fixed" {
		t.Fatalf("c2 %+v", c2)
	}
}

// Each rule of an observation and of a reason on its own.
func TestEachObservationRuleStandsAlone(t *testing.T) {
	met := func(o ...observationEntry) []criterionAnswer {
		return []criterionAnswer{{ID: "t1.c1", State: "met", Reason: "r", Cite: []string{"s3", "r2", "s2"}, Observations: o}, {ID: "t1.c2", State: "unknown", Cite: []string{}}}
	}
	for _, tc := range []struct {
		name string
		obs  observationEntry
		kept bool
	}{
		{"an excerpt of the text", observationEntry{Node: "r2", Excerpt: "the clock"}, true},
		{"an excerpt within the clip", observationEntry{Node: "s3", Excerpt: "ok ok"}, true},
		{"an excerpt past the clip", observationEntry{Node: "s3", Excerpt: "FAIL TestCache"}, true},
		{"an excerpt the step does not hold", observationEntry{Node: "s3", Excerpt: "PASS TestCache"}, false},
		{"an excerpt that differs in case", observationEntry{Node: "r2", Excerpt: "The Clock"}, false},
		{"a step not cited", observationEntry{Node: "s1"}, false},
		{"an excerpt too long, cut to its start", observationEntry{Node: "s3", Excerpt: strings.Repeat("ok ", 101)}, true},
		{"an excerpt of another layout", observationEntry{Node: "r2", Excerpt: "the\n   clock"}, true},
		{"an excerpt of escaped line breaks", observationEntry{Node: "s2", Excerpt: `revert failed:\nconflict in cache.go`}, true},
		{"an excerpt that differs in a word", observationEntry{Node: "s2", Excerpt: "revert failed: conflict in clock.go"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, note := accountOf(t, met(tc.obs), accountEntry{})
			if kept := len(a.Criteria[0].Observations) == 1; kept != tc.kept {
				t.Errorf("kept %v", kept)
			}
			if tc.kept == strings.Contains(note, "1 observations of target t1 that are not of a step it cites, whose excerpt the step does not hold, or given twice, were left out") {
				t.Errorf("note %q", note)
			}
		})
	}
	// One given again, word for word, is kept once; the same step read
	// another way is another observation.
	same := observationEntry{Node: "r2", Excerpt: "the clock", Interpretation: "the cause"}
	other := same
	other.Interpretation = "a guess"
	a, note := accountOf(t, met(same, same, other, same), accountEntry{})
	if len(a.Criteria[0].Observations) != 2 || !strings.Contains(note, "2 observations of target t1") {
		t.Fatalf("repeated: %d kept; %q", len(a.Criteria[0].Observations), note)
	}
	// An input that set the target confirms nothing, even after the
	// operation.
	asking := deliveryTarget()
	asking.Cite = append(asking.Cite, sessionjudge.Cite{Node: "in/3"})
	a, note = accountFor(t, asking, nil, accountEntry{Remote: []remoteEntry{{System: "GitHub", Request: said("push", "s3"), Confirmed: said("thanks", "in/3")}}})
	if a.Account == nil || a.Account.Remote[0].Confirmed != nil || !strings.Contains(note, "1 parts of the account") {
		t.Fatalf("confirmed by an input that asked: %+v %q", a.Account, note)
	}
	// A long excerpt is cut on a whole character.
	a, _ = accountOf(t, met(observationEntry{Node: "s3", Excerpt: strings.Repeat("ok ", 101)}), accountEntry{})
	if got := a.Criteria[0].Observations[0].Excerpt; len(got) != 300 || got != strings.Repeat("ok ", 100) {
		t.Fatalf("cut to %d bytes: %q", len(got), got)
	}
	if got := clipBytes(strings.Repeat("═", 101), 300); got != strings.Repeat("═", 100) {
		t.Fatalf("cut inside a character: %q", got)
	}
	if clipBytes("ab═", 3) != "ab" || clipBytes("abc", 300) != "abc" {
		t.Fatal("a short text was cut")
	}
	// A clipped step whose record cannot be read holds nothing past the clip.
	nav := accounted()
	nav.read = nil
	if holds(nav, "s3", "FAIL TestCache") || !holds(nav, "s3", "ok ok") {
		t.Fatal("an unread record was taken to hold an excerpt")
	}
	// A criterion left unanswered, or in a state asz does not know, says so.
	a, _ = accountOf(t, []criterionAnswer{{ID: "t1.c1", State: "done", Reason: "it is done", Cite: []string{"s3"}}}, accountEntry{})
	if c := a.Criteria[0]; c.Reason != "the state given is not one asz knows" || c.GivenReason != "it is done" {
		t.Fatalf("a state asz does not know: %+v", c)
	}
	if c := a.Criteria[1]; c.Reason != "the evaluator did not answer it" || c.GivenReason != "" {
		t.Fatalf("a criterion not answered: %+v", c)
	}
}
