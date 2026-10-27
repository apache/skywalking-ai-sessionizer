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
	"os"
	"path/filepath"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

func call(id, stream string, at int64, model string, u *sessiondata.Usage) sessionview.Node {
	return sessionview.Node{ID: id, Kind: "llm.call", Stream: stream, At: at, Model: model, Usage: u}
}

func usage(in, out, read, write int) *sessiondata.Usage {
	return &sessiondata.Usage{Input: in, Output: out, CacheRead: read, CacheWrite: write}
}

// A conversation of two main talks. The first starts child a, which starts
// child b; a child c starts from both talks, and a child d from no call.
func doc() *sessionview.Conversation {
	tool := func(id string) sessionview.Node { return sessionview.Node{ID: id, Kind: "tool", Result: "done"} }
	t1 := sessionview.Node{ID: "talk/1", Kind: "talk", Stream: "main", From: 100, To: 500, Children: []sessionview.Node{
		call("c1", "main", 100, "m", usage(10, 1, 100, 5)), tool("s1"), tool("s3a"),
	}}
	t2 := sessionview.Node{ID: "talk/2", Kind: "talk", Stream: "main", From: 600, To: 700, Children: []sessionview.Node{
		call("c2", "main", 600, "m", usage(20, 2, 200, 0)), tool("s3b"),
	}}
	ta := sessionview.Node{ID: "talk/a", Kind: "talk", Stream: "a", From: 150, To: 900, Children: []sessionview.Node{
		call("ca", "a", 150, "m", usage(1, 1, 1, 1)), tool("s2"),
	}}
	tb := sessionview.Node{ID: "talk/b", Kind: "talk", Stream: "b", From: 200, To: 300, Children: []sessionview.Node{
		call("cb", "b", 200, "other", usage(1, 0, 0, 0)), call("cb2", "b", 250, "m", nil),
	}}
	tc := sessionview.Node{ID: "talk/c", Kind: "talk", Stream: "c", Children: []sessionview.Node{call("cc", "c", 0, "m", usage(5, 0, 0, 0))}}
	td := sessionview.Node{ID: "talk/d", Kind: "talk", Stream: "d", Children: []sessionview.Node{call("cd", "d", 0, "m", usage(7, 0, 0, 0))}}
	starts := func(from, to string) sessionview.Relation {
		return sessionview.Relation{ID: from + to, Type: model.RelStarts, From: from, To: "stream/" + to}
	}
	return &sessionview.Conversation{
		Conversation: "conv",
		Streams: []sessionview.Stream{{ID: "stream/main", Name: "main"}, {ID: "stream/a", Name: "a"},
			{ID: "stream/b", Name: "b"}, {ID: "stream/c", Name: "c"}, {ID: "stream/d", Name: "d"}},
		Talks: []sessionview.Node{t1, ta, tb, t2, tc, td},
		Loose: []sessionview.Node{call("cl", "main", 800, "m", usage(3, 0, 0, 0))},
		Relations: []sessionview.Relation{
			starts("s1", "a"), starts("s2", "b"), starts("s3a", "c"), starts("s3b", "c"),
		},
	}
}

// Every call is counted once: the owners add up to the conversation, and a
// talk's inclusive cost adds only the child streams it alone started.
func TestEveryCallIsCountedOnce(t *testing.T) {
	l := Compute(doc(), "claude-code/1", nil, nil)
	if l.Total.Calls != 8 || l.Total.Unknown != 1 {
		t.Fatalf("total %+v", l.Total)
	}
	var sum sessiondata.Usage
	calls := 0
	for _, o := range l.Owners {
		add(&sum, o.Usage)
		calls += o.Calls
	}
	if sum != l.Total.Usage || calls != l.Total.Calls {
		t.Fatalf("owners add to %+v in %d calls, total %+v", sum, calls, l.Total)
	}
	one, _ := l.Inclusive("talk/1")
	if one.Calls != 4 || one.Children != 3 || one.Usage.Input != 12 {
		t.Fatalf("talk 1 with children a and b: %+v", one)
	}
	// Child a ran past the talk that started it, so the range widens.
	if one.From != 100 || one.To != 900 {
		t.Fatalf("talk 1 range %d..%d", one.From, one.To)
	}
	two, _ := l.Inclusive("talk/2")
	if two.Calls != 1 {
		t.Fatalf("talk 2 took a child two talks claim: %+v", two)
	}
	// c is claimed by two talks and d by none: both are unattributed, and the
	// main stream's loose call is its own owner, not unattributed.
	if l.Unattributed.Calls != 2 || l.Unattributed.Usage.Input != 12 {
		t.Fatalf("unattributed %+v", l.Unattributed)
	}
}

// LangChain counts cached tokens inside its input; the ledger takes them out
// once, and a count that does not add up is unknown, not negative.
func TestLangChainInputIsNormalized(t *testing.T) {
	if got := normalize("langsmith/1", usage(100, 5, 60, 10)); got.Input != 30 || got.CacheRead != 60 {
		t.Fatalf("normalized %+v", got)
	}
	if got := normalize("langsmith/1", usage(10, 5, 60, 0)); got != nil {
		t.Fatalf("counts that do not add up gave %+v", got)
	}
	if got := normalize("claude-code/1", usage(100, 5, 60, 10)); got.Input != 100 {
		t.Fatalf("Claude Code input changed: %+v", got)
	}
}

// A call is priced only by the exact name it recorded, at the entry in force
// at its time; one with no name, no entry or no usage stays unpriced.
func TestCallsArePricedByTheirOwnModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pricing.yaml")
	body := `currency: USD
models:
  - model: m
    aliases: [provider.m]
    per_million: {in: 1, out: 10, cache_read: 0.1, cache_write: 2}
  - model: m
    from: 2026-01-01T00:00:00.400Z
    per_million: {in: 2, out: 20, cache_read: 0.2, cache_write: 4}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPricing(path, "")
	if err != nil {
		t.Fatal(err)
	}
	early := p.Lookup("m", 100)
	late := p.Lookup("m", 1767225600500)
	if early == nil || early.From != "" || late == nil || late.PerMillion.In != 2 {
		t.Fatalf("early %+v late %+v", early, late)
	}
	if p.Lookup("provider.m", 100).Key != "m" || p.Lookup("m", 0).From != "" {
		t.Fatal("an alias or an untimed call took the wrong entry")
	}
	if p.Lookup("other", 100) != nil || p.Lookup("", 100) != nil {
		t.Fatal("a name with no entry was priced")
	}
	l := Compute(doc(), "claude-code/1", p, nil)
	// cb ran "other" and cb2 has no usage: neither is priced, and only cb
	// counts as unpriced.
	if l.Total.Unpriced != 1 || l.Total.Money == nil {
		t.Fatalf("total %+v", l.Total)
	}
	// c1 is 10 in, 1 out, 100 read, 5 write at the undated rates.
	if got := l.Calls[0].Money; got < 0.0000399 || got > 0.0000401 {
		t.Fatalf("c1 cost %v", got)
	}
	// The summary names the price file's currency even when none of the
	// conversation's calls is priced, since judging is priced by it too.
	d := doc()
	for i := range d.Talks {
		for j := range d.Talks[i].Children {
			d.Talks[i].Children[j].Model = "unlisted"
		}
	}
	d.Loose = nil
	judging := 0.12
	_, sum := closeFrames(Compute(d, "claude-code/1", p, nil), nil, nil, nil, nil, nil, &sessionjudge.Amount{Money: &judging})
	if sum.Money != nil || sum.Currency != "USD" {
		t.Fatalf("summary money %v in %q", sum.Money, sum.Currency)
	}
}

// A price file that could price one call two ways is refused.
func TestAnAmbiguousPriceFileIsRefused(t *testing.T) {
	for name, body := range map[string]string{
		"two entries at one time": "currency: USD\nmodels:\n  - model: m\n  - model: m\n",
		"one alias, two models":   "currency: USD\nmodels:\n  - model: m\n    aliases: [x]\n  - model: n\n    aliases: [x]\n",
		"no currency":             "models:\n  - model: m\n",
		"bad time":                "currency: USD\nmodels:\n  - model: m\n    from: yesterday\n",
	} {
		path := filepath.Join(t.TempDir(), "p.yaml")
		_ = os.WriteFile(path, []byte(body), 0o600)
		if _, err := LoadPricing(path, ""); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A call that never finished counts what it read, from any of its
// fragments, and its output stays unknown; only Claude Code's dialect, whose
// fragments were measured, is read this way.
func TestAnUnfinishedCallCountsWhatItRead(t *testing.T) {
	d := doc()
	// cb2 in child b recorded no usage and never finished, so the view
	// names no model for it.
	d.Talks[2].Children[1].Attrs = json.RawMessage(`{"usage":"unavailable"}`)
	d.Talks[2].Children[1].Refs = []sessionflow.Ref{{Seq: 7, Row: 1}, {Seq: 7, Row: 2}}
	d.Talks[2].Children[1].Model = ""
	read := func(refs []sessionflow.Ref) map[[2]uint64]*sessiondata.Record {
		out := map[[2]uint64]*sessiondata.Record{}
		for _, r := range refs {
			out[[2]uint64{r.Seq, r.Row}] = &sessiondata.Record{Usage: usage(3, 1, 500, 40), Model: "m"}
		}
		return out
	}
	path := filepath.Join(t.TempDir(), "pricing.yaml")
	if err := os.WriteFile(path, []byte("currency: USD\nmodels:\n  - model: m\n    per_million: {in: 1, out: 10, cache_read: 0.1, cache_write: 2}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prices, err := LoadPricing(path, "")
	if err != nil {
		t.Fatal(err)
	}
	l := Compute(d, "claude-code/1", prices, read)
	var cb2 Call
	for _, c := range l.Calls {
		if c.ID == "cb2" {
			cb2 = c
		}
	}
	if cb2.ID != "cb2" || !cb2.OutputUnknown || *cb2.Usage != (sessiondata.Usage{Input: 3, CacheRead: 500, CacheWrite: 40}) {
		t.Fatalf("cb2 %+v %+v", cb2, cb2.Usage)
	}
	// It is priced at the rate of the model its fragment names.
	if cb2.Model != "m" || cb2.Price == nil || cb2.Money == 0 {
		t.Fatalf("cb2 model %q, price %+v, money %v", cb2.Model, cb2.Price, cb2.Money)
	}
	if l.Total.Unknown != 0 || l.Total.OutputUnknown != 1 {
		t.Fatalf("total %+v", l.Total)
	}
	other := Compute(d, "langsmith/1", nil, read)
	for _, c := range other.Calls {
		if c.ID == "cb2" && (c.Usage != nil || c.OutputUnknown) {
			t.Fatalf("another dialect read the fragments: %+v", c)
		}
	}
	if other.Total.OutputUnknown != 0 {
		t.Fatalf("another dialect counted a call by what it read: %+v", other.Total)
	}
}

// workOf closes a run over the doc and returns the calls of each label, and
// the avoidable tokens.
func workOf(t *testing.T, l *Ledger, targets []sessionjudge.Target, inputs []*sessionjudge.Input, labels []*sessionjudge.Label) (map[string]int, sessiondata.Usage) {
	t.Helper()
	frames, sum := closeFrames(l, targets, inputs, nil, labels, nil, nil)
	got := map[string]int{}
	for _, f := range frames {
		if c, ok := f.(*sessionjudge.Cost); ok && c.Scope == sessionjudge.ScopeWork {
			got[c.Work] += c.Calls
		}
	}
	return got, *sum.Avoidable
}

// A part of a talk is cut by the place its work has in the talk: a child
// agent's calls, and its own child's, go with the step that started it.
func TestPartsAreCutByPlace(t *testing.T) {
	l := Compute(doc(), "claude-code/1", nil, nil)
	targets := []sessionjudge.Target{{ID: "t1", Talks: []string{"talk/1"}}}
	labels := []*sessionjudge.Label{
		{Target: "t1", Talk: "talk/1", Label: sessionjudge.WorkUseful},
		{Target: "t1", Talk: "talk/1", From: "s1", Label: sessionjudge.WorkRepeated},
	}
	got, avoidable := workOf(t, l, targets, nil, labels)
	// c1 comes before s1; child a (ca) started at s1, and its child b (cb,
	// cb2) with it.
	if got[sessionjudge.WorkUseful] != 1 || got[sessionjudge.WorkRepeated] != 3 {
		t.Fatalf("work %v", got)
	}
	if avoidable != (sessiondata.Usage{Input: 2, Output: 1, CacheRead: 1, CacheWrite: 1}) {
		t.Fatalf("avoidable %+v", avoidable)
	}
}

// A talk shared three ways by one input keeps its fractions until the end, so
// its tokens are counted whole.
func TestSharesAreKeptAsFractions(t *testing.T) {
	l := Compute(doc(), "claude-code/1", nil, nil)
	var targets []sessionjudge.Target
	var labels []*sessionjudge.Label
	for _, id := range []string{"t1", "t2", "t3"} {
		targets = append(targets, sessionjudge.Target{ID: id, Talks: []string{"talk/2"}})
		labels = append(labels, &sessionjudge.Label{Target: id, Talk: "talk/2", Label: sessionjudge.WorkRepeated})
	}
	_, avoidable := workOf(t, l, targets, nil, labels)
	whole, _ := l.Inclusive("talk/2")
	if avoidable != whole.Usage {
		t.Fatalf("avoidable %+v, the talk %+v", avoidable, whole.Usage)
	}
}

// One talk holding two requests: a question, then a change typed while the
// agent worked. Each call goes to the request before it, and a child agent
// goes with the step that started it.
func twoRequests() *sessionview.Conversation {
	in := func(id string) sessionview.Node { return sessionview.Node{ID: id, Kind: model.KindMessageExternal} }
	t1 := sessionview.Node{ID: "talk/1", Kind: "talk", Stream: "main", From: 100, To: 1000, Children: []sessionview.Node{
		in("input/1"), call("q1", "main", 100, "m", usage(10, 0, 0, 0)), call("q2", "main", 200, "m", usage(10, 0, 0, 0)),
		in("input/2"), call("w1", "main", 300, "m", usage(100, 0, 0, 0)),
		{ID: "s1", Kind: "tool"}, call("w2", "main", 800, "m", usage(100, 0, 0, 0)),
	}}
	ta := sessionview.Node{ID: "talk/a", Kind: "talk", Stream: "a", Children: []sessionview.Node{call("ca", "a", 400, "m", usage(50, 0, 0, 0))}}
	return &sessionview.Conversation{
		Conversation: "conv",
		Streams:      []sessionview.Stream{{ID: "stream/main", Name: "main"}, {ID: "stream/a", Name: "a"}},
		Talks:        []sessionview.Node{t1, ta},
		Relations:    []sessionview.Relation{{ID: "r", Type: model.RelStarts, From: "s1", To: "stream/a"}},
	}
}

// A talk serving two targets is cut at its inputs, never split equally when
// the inputs say which work served which. This is the shape of 772204d1,
// where an equal split charged a question 2.8 times what it used.
func TestASharedTalkIsCutAtItsInputs(t *testing.T) {
	l := Compute(twoRequests(), "claude-code/1", nil, nil)
	targets := []sessionjudge.Target{
		{ID: "t1", Status: sessionjudge.TargetClosed, Talks: []string{"talk/1"}},
		{ID: "t2", Status: sessionjudge.TargetClosed, Talks: []string{"talk/1"}},
	}
	inputs := []*sessionjudge.Input{
		{Node: "input/1", Talk: "talk/1", Actions: []sessionjudge.Action{{Target: "t1", Action: sessionjudge.ActionCreate}}},
		{Node: "input/2", Talk: "talk/1", Actions: []sessionjudge.Action{{Target: "t2", Action: sessionjudge.ActionCreate}}},
	}
	frames, _ := closeFrames(l, targets, inputs, nil, nil, nil, nil)
	cost := map[string]int{}
	for _, f := range frames {
		if o, ok := f.(*sessionjudge.Outcome); ok {
			cost[o.Target] = o.Cost.Usage.Input
		}
		if c, ok := f.(*sessionjudge.Cost); ok && c.Scope == sessionjudge.ScopeShared {
			t.Fatalf("a call was split although an input named its target: %+v", c)
		}
	}
	if cost["t1"] != 20 || cost["t2"] != 250 {
		t.Fatalf("costs %v, want t1 20 and t2 250", cost)
	}
	// The targets stage can list the inputs in any order: the cuts follow
	// where the inputs are in the talk.
	frames, _ = closeFrames(l, targets, []*sessionjudge.Input{inputs[1], inputs[0]}, nil, nil, nil, nil)
	for _, f := range frames {
		if o, ok := f.(*sessionjudge.Outcome); ok && o.Cost.Usage.Input != cost[o.Target] {
			t.Fatalf("inputs listed out of order: %s costs %d", o.Target, o.Cost.Usage.Input)
		}
	}
	// One input that asks for both splits what follows it between them.
	inputs[1].Actions = append(inputs[1].Actions, sessionjudge.Action{Target: "t1", Action: sessionjudge.ActionRefine})
	frames, _ = closeFrames(l, targets, inputs, nil, nil, nil, nil)
	for _, f := range frames {
		if o, ok := f.(*sessionjudge.Outcome); ok {
			cost[o.Target] = o.Cost.Usage.Input
		}
	}
	if cost["t1"] != 145 || cost["t2"] != 125 {
		t.Fatalf("costs %v, want t1 145 and t2 125", cost)
	}
}

// The scores: achievement over the targets judged, efficiency over the cost
// reviewed, and the avoidable time from the main stream's avoidable calls.
func TestTheScoresComeFromTheOutcomes(t *testing.T) {
	l := Compute(twoRequests(), "claude-code/1", nil, nil)
	targets := []sessionjudge.Target{
		{ID: "t1", Status: sessionjudge.TargetClosed, Talks: []string{"talk/1"}},
		{ID: "t2", Status: sessionjudge.TargetClosed, Talks: []string{"talk/1"}},
	}
	inputs := []*sessionjudge.Input{
		{Node: "input/1", Talk: "talk/1", Actions: []sessionjudge.Action{{Target: "t1", Action: sessionjudge.ActionCreate}}},
		{Node: "input/2", Talk: "talk/1", Actions: []sessionjudge.Action{{Target: "t2", Action: sessionjudge.ActionCreate}}},
	}
	achievements := []*sessionjudge.Achievement{
		{Target: "t1", Level: sessionjudge.LevelAchieved},
		{Target: "t2", Level: sessionjudge.LevelPartly},
	}
	labels := []*sessionjudge.Label{
		{Target: "t1", Talk: "talk/1", Label: sessionjudge.WorkUseful},
		{Target: "t2", Talk: "talk/1", Label: sessionjudge.WorkUseful},
		{Target: "t2", Talk: "talk/1", From: "s1", Label: sessionjudge.WorkRepeated},
	}
	frames, sum := closeFrames(l, targets, inputs, achievements, labels, nil, nil)
	sc := sum.Score
	// t1 achieved and t2 partly: 0.75. Of the 270 tokens reviewed, ca (50)
	// and w2 (100) are avoidable. t1's 20 went to an achieved target.
	if sc.Judged != 2 || *sc.Achievement != 0.75 || sc.Unit != sessionjudge.UnitTokens ||
		*sc.Reviewed != 1 || *sc.Efficiency != 0.4444 || *sc.Delivered != 0.0741 {
		t.Fatalf("score %+v achievement %v efficiency %v delivered %v", sc, *sc.Achievement, *sc.Efficiency, *sc.Delivered)
	}
	for _, f := range frames {
		if o, ok := f.(*sessionjudge.Outcome); ok && o.Target == "t2" {
			// w2 is the main stream's only avoidable call, from 800 to the
			// talk's end at 1000; ca is a child's, beside the main stream.
			if o.Avoidable.Usage.Input != 150 || *o.AvoidableSpan != 200 {
				t.Fatalf("t2 %+v span %d", o.Avoidable, *o.AvoidableSpan)
			}
		}
	}
	// An avoidable run in the middle of a talk ends where the talk's next
	// call starts, whoever's it is: t1's q1 and q2 run from 100 to w1 at 300.
	labels[0].Label = sessionjudge.WorkRepeated
	labels[0].Cite = []sessionjudge.Cite{{Node: "q1"}}
	frames, _ = closeFrames(l, targets, inputs, achievements, labels, nil, nil)
	for _, f := range frames {
		if o, ok := f.(*sessionjudge.Outcome); ok && o.Target == "t1" && *o.AvoidableSpan != 200 {
			t.Fatalf("t1 avoidable span %d, want 200", *o.AvoidableSpan)
		}
	}
}

// Every stream a step started is a sub-task of that step, nested as deep as
// the agents went; a stream no single step started is kept with its reason.
func TestSubtasksFollowTheAgents(t *testing.T) {
	l := Compute(doc(), "claude-code/1", nil, nil)
	got := map[string]Subtask{}
	for _, st := range l.Subtasks {
		got[st.Name] = st
	}
	a, b, c, d := got["a"], got["b"], got["c"], got["d"]
	if a.Main != "talk/1" || a.Parent != "talk/1" || a.StartedBy != "s1" || a.Depth != 1 || len(a.Children) != 1 || a.Children[0] != "stream/b" {
		t.Fatalf("a %+v", a)
	}
	if b.Main != "talk/1" || b.Parent != "stream/a" || b.StartedBy != "s2" || b.Depth != 2 {
		t.Fatalf("b %+v", b)
	}
	if c.Main != "" || c.Reason != ReasonSeveral || d.Main != "" || d.Reason != ReasonNoStarter {
		t.Fatalf("c %+v d %+v", c, d)
	}
	if n := len(l.SubtaskCalls("stream/a", false)); n != 1 {
		t.Fatalf("a's own calls %d", n)
	}
	if n := len(l.SubtaskCalls("stream/a", true)); n != 3 {
		t.Fatalf("a with b %d", n)
	}
}

// A sub-task's own calls take the label the sub-task stage gave it, and a
// nested sub-task left unjudged is not reviewed, never priced by its parent's
// label. Without that stage, a child's calls follow the talk's label.
func TestSubtaskWorkIsTheMostSpecificLabel(t *testing.T) {
	l := Compute(doc(), "claude-code/1", nil, nil)
	targets := []sessionjudge.Target{{ID: "t1", Talks: []string{"talk/1"}}}
	labels := []*sessionjudge.Label{{Target: "t1", Talk: "talk/1", Label: sessionjudge.WorkUseful}}
	work := func(sub map[string]string) map[string]int {
		var decided map[string]*sessionjudge.Subtask
		if sub != nil {
			decided = map[string]*sessionjudge.Subtask{}
			for id, w := range sub {
				decided[id] = &sessionjudge.Subtask{ID: id, Work: w}
			}
		}
		frames, _ := closeFrames(l, targets, nil, nil, labels, decided, nil)
		got := map[string]int{}
		for _, f := range frames {
			if c, ok := f.(*sessionjudge.Cost); ok && c.Scope == sessionjudge.ScopeWork {
				got[c.Work] += c.Calls
			}
		}
		return got
	}
	// c1 is the talk's own; ca is a's; cb and cb2 are b's, under a.
	if got := work(map[string]string{"stream/a": sessionjudge.WorkRepeated}); got[sessionjudge.WorkUseful] != 1 ||
		got[sessionjudge.WorkRepeated] != 1 || got[sessionjudge.WorkNotReviewed] != 2 {
		t.Fatalf("with the sub-task stage %v", got)
	}
	if got := work(nil); got[sessionjudge.WorkUseful] != 4 {
		t.Fatalf("without it %v", got)
	}
}

// A target partly achieved counts the share of its criteria met, so one
// criterion nobody could verify does not halve it.
func TestAPartlyTargetCountsItsCriteria(t *testing.T) {
	var s scoreSum
	six, seven := 6, 7
	s.add(&sessionjudge.Outcome{Status: sessionjudge.TargetClosed, Level: sessionjudge.LevelPartly, Met: &six, Criteria: &seven}, exactAmounts{})
	s.add(&sessionjudge.Outcome{Status: sessionjudge.TargetClosed, Level: sessionjudge.LevelAchieved}, exactAmounts{})
	if sc := s.score(false); sc.Judged != 2 || *sc.Achievement != 0.9286 {
		t.Fatalf("score %+v %v", sc, *sc.Achievement)
	}
}

// A token split three ways counts whole in the scores, and the targets'
// amounts, in whole tokens, add up to the ledger.
func TestScoresUseExactShares(t *testing.T) {
	d := &sessionview.Conversation{Conversation: "c", Streams: []sessionview.Stream{{ID: "stream/main", Name: "main"}},
		Talks: []sessionview.Node{{ID: "talk/1", Kind: "talk", Stream: "main", Children: []sessionview.Node{
			{ID: "in", Kind: "message.external"}, call("c1", "main", 1, "m", usage(1, 0, 0, 0))}}}}
	l := Compute(d, "claude-code/1", nil, nil)
	var targets []sessionjudge.Target
	var acts []sessionjudge.Action
	var achieved []*sessionjudge.Achievement
	for _, id := range []string{"t1", "t2", "t3"} {
		targets = append(targets, sessionjudge.Target{ID: id, Status: sessionjudge.TargetClosed, Talks: []string{"talk/1"}})
		acts = append(acts, sessionjudge.Action{Target: id, Action: sessionjudge.ActionCreate})
		achieved = append(achieved, &sessionjudge.Achievement{Target: id, Level: sessionjudge.LevelAchieved})
	}
	inputs := []*sessionjudge.Input{{Node: "in", Talk: "talk/1", Actions: acts}}
	frames, sum := closeFrames(l, targets, inputs, achieved, nil, nil, nil)
	// In whole tokens the one token goes to one target and none is lost or
	// made: the targets add up to the ledger.
	total := 0
	for _, f := range frames {
		if o, ok := f.(*sessionjudge.Outcome); ok {
			total += o.Cost.Tokens()
		}
	}
	if total != 1 {
		t.Fatalf("the targets add up to %d tokens of the call's 1", total)
	}
	if sum.Score.Delivered == nil || *sum.Score.Delivered != 1 {
		t.Fatalf("delivered %v", sum.Score.Delivered)
	}
	// Only the first target's share is avoidable: the run's avoidable total
	// is what its targets' add up to.
	labels := []*sessionjudge.Label{{Target: "t1", Talk: "talk/1", Label: sessionjudge.WorkRepeated},
		{Target: "t2", Talk: "talk/1", Label: sessionjudge.WorkUseful}, {Target: "t3", Talk: "talk/1", Label: sessionjudge.WorkUseful}}
	frames, sum = closeFrames(l, targets, inputs, achieved, labels, nil, nil)
	avoidable := 0
	for _, f := range frames {
		if o, ok := f.(*sessionjudge.Outcome); ok && o.Avoidable != nil {
			avoidable += o.Avoidable.Tokens()
		}
	}
	if sum.Avoidable == nil || sum.Avoidable.Input != avoidable || avoidable != 1 {
		t.Fatalf("the targets' avoidable %d, the run's %+v", avoidable, sum.Avoidable)
	}
}

// A sub-task under one no main talk can be shown to have started stays its
// child, so the parent's cost with everything under it is whole.
func TestAnUnattributedTreeKeepsItsChildren(t *testing.T) {
	d := doc()
	// Child d, which no step started, starts child e.
	d.Streams = append(d.Streams, sessionview.Stream{ID: "stream/e", Name: "e"})
	d.Talks = append(d.Talks, sessionview.Node{ID: "talk/e", Kind: "talk", Stream: "e", Children: []sessionview.Node{call("ce", "e", 0, "m", usage(4, 0, 0, 0))}})
	d.Talks[5].Children = append(d.Talks[5].Children, sessionview.Node{ID: "sd", Kind: "tool", Result: "ok"})
	d.Relations = append(d.Relations, sessionview.Relation{ID: "sde", Type: model.RelStarts, From: "sd", To: "stream/e"})
	l := Compute(d, "claude-code/1", nil, nil)
	st, _ := l.Subtask("stream/d")
	if len(st.Children) != 1 || st.Children[0] != "stream/e" || len(l.SubtaskCalls("stream/d", true)) != 2 {
		t.Fatalf("d %+v, with everything under it %d calls", st, len(l.SubtaskCalls("stream/d", true)))
	}
}

// A run's summary, and the cost of each target, say how many calls their
// totals leave out: calls with no usage, and calls that never finished,
// whose output is not counted.
func TestTheSummarySaysWhatTheTotalLeavesOut(t *testing.T) {
	d := doc()
	d.Talks[0].Children = append([]sessionview.Node{{ID: "in", Kind: model.KindMessageExternal, Text: "do it"}}, d.Talks[0].Children...)
	targets := []sessionjudge.Target{{ID: "t1", Status: sessionjudge.TargetClosed, Talks: []string{"talk/1"}}}
	inputs := []*sessionjudge.Input{{Node: "in", Talk: "talk/1", Actions: []sessionjudge.Action{{Target: "t1", Action: sessionjudge.ActionCreate}}}}
	cost := func(l *Ledger) (sessionjudge.Amount, *sessionjudge.Summary) {
		frames, sum := closeFrames(l, targets, inputs, nil, nil, nil, nil)
		for _, f := range frames {
			if o, ok := f.(*sessionjudge.Outcome); ok {
				return o.Cost, sum
			}
		}
		t.Fatal("no outcome")
		return sessionjudge.Amount{}, nil
	}
	// cb2, under talk 1 through child a, recorded no usage.
	if c, sum := cost(Compute(d, "claude-code/1", nil, nil)); sum.Unknown != 1 || c.Unknown != 1 || c.OutputUnknown != 0 {
		t.Fatalf("summary %+v, target %+v", sum, c)
	}
	// Read from its fragments, it read but never finished.
	d.Talks[2].Children[1].Attrs = json.RawMessage(`{"usage":"unavailable"}`)
	d.Talks[2].Children[1].Refs = []sessionflow.Ref{{Seq: 7, Row: 1}}
	read := func(refs []sessionflow.Ref) map[[2]uint64]*sessiondata.Record {
		out := map[[2]uint64]*sessiondata.Record{}
		for _, r := range refs {
			out[[2]uint64{r.Seq, r.Row}] = &sessiondata.Record{Usage: usage(3, 1, 500, 40)}
		}
		return out
	}
	if c, sum := cost(Compute(d, "claude-code/1", nil, read)); sum.Unknown != 0 || sum.OutputUnknown != 1 || c.Unknown != 0 || c.OutputUnknown != 1 {
		t.Fatalf("summary %+v, target %+v", sum, c)
	}
}

// A target kept from achieved by a contradicted claim is not scored as if it
// were achieved: the claim counts as one more requirement not met.
func TestAContradictedClaimLowersTheScore(t *testing.T) {
	var s scoreSum
	two, none, one := 2, 0, 1
	s.add(&sessionjudge.Outcome{Status: sessionjudge.TargetClosed, Level: sessionjudge.LevelPartly, Met: &two, Criteria: &two,
		Unknown: &none, Contradicted: &one}, exactAmounts{})
	if sc := s.score(false); *sc.Achievement != 0.6667 {
		t.Fatalf("achievement %v", *sc.Achievement)
	}
	// The close stage counts the contradicted claims from the achievement.
	targets := []sessionjudge.Target{{ID: "t1", Status: sessionjudge.TargetClosed, Talks: []string{"talk/1"}}}
	achieved := []*sessionjudge.Achievement{{Target: "t1", Level: sessionjudge.LevelPartly,
		Criteria: []sessionjudge.Check{{ID: "t1.c1", State: sessionjudge.CheckMet}, {ID: "t1.c2", State: sessionjudge.CheckMet}},
		Claims:   []sessionjudge.Check{{State: sessionjudge.CheckContradicted}, {State: sessionjudge.CheckSupported}}}}
	frames, sum := closeFrames(Compute(doc(), "claude-code/1", nil, nil), targets, nil, achieved, nil, nil, nil)
	for _, f := range frames {
		if o, ok := f.(*sessionjudge.Outcome); ok && (o.Contradicted == nil || *o.Contradicted != 1) {
			t.Fatalf("outcome %+v", o)
		}
	}
	if *sum.Score.Achievement != 0.6667 {
		t.Fatalf("the run's achievement %v", *sum.Score.Achievement)
	}
}

// Avoidable work is the sub-task's when its own work went wrong: its own
// calls, and the asking agent's work done on its untrue report. When the
// task it was given was wrong, both are the main agent's.
func TestAvoidableWorkSaysWhoseDoingItWas(t *testing.T) {
	l := Compute(doc(), "claude-code/1", nil, nil)
	targets := []sessionjudge.Target{{ID: "t1", Status: sessionjudge.TargetClosed, Talks: []string{"talk/1"}}}
	labels := []*sessionjudge.Label{{Target: "t1", Talk: "talk/1", Label: sessionjudge.WorkMisled, Subtask: "stream/a"}}
	cause := func(dispatch, work string) (map[string]string, *sessionjudge.Outcome) {
		t.Helper()
		subtasks := map[string]*sessionjudge.Subtask{
			"stream/a": {ID: "stream/a", Dispatch: dispatch, Use: sessionjudge.UseMisled, Work: work},
			"stream/b": {ID: "stream/b", Dispatch: sessionjudge.DispatchReasonable, Work: sessionjudge.WorkUseful}}
		frames, _ := closeFrames(l, targets, nil, nil, labels, subtasks, nil)
		got := map[string]string{}
		var o *sessionjudge.Outcome
		for _, f := range frames {
			switch x := f.(type) {
			case *sessionjudge.Cost:
				if x.Scope == sessionjudge.ScopeWork {
					got[x.Work] = x.Cause
				}
			case *sessionjudge.Outcome:
				o = x
			}
		}
		return got, o
	}
	// a's own work repeated itself: its doing, and so is the talk's work on
	// its untrue report. b's useful work has no cause.
	got, o := cause(sessionjudge.DispatchReasonable, sessionjudge.WorkRepeated)
	if got[sessionjudge.WorkMisled] != "stream/a" || got[sessionjudge.WorkRepeated] != "stream/a" || got[sessionjudge.WorkUseful] != "" {
		t.Fatalf("a's own work went wrong: %v", got)
	}
	if o.AvoidableBySubtasks == nil || o.AvoidableBySubtasks.Tokens() != o.Avoidable.Tokens() || o.Avoidable.Tokens() == 0 {
		t.Fatalf("outcome avoidable %+v, by sub-tasks %+v", o.Avoidable, o.AvoidableBySubtasks)
	}
	// a was given the wrong task: all of it is the main agent's.
	got, o = cause(sessionjudge.DispatchMisdirected, sessionjudge.WorkMisdirected)
	if got[sessionjudge.WorkMisled] != sessionjudge.CauseMain || got[sessionjudge.WorkMisdirected] != sessionjudge.CauseMain {
		t.Fatalf("a's task was wrong: %v", got)
	}
	if o.AvoidableBySubtasks.Tokens() != 0 || o.Avoidable.Tokens() == 0 {
		t.Fatalf("outcome avoidable %+v, by sub-tasks %+v", o.Avoidable, o.AvoidableBySubtasks)
	}
}

// Efficiency labels the main talks, so only a sub-task a main talk started
// can have misled the work it labels; a nested one misled its sub-agent.
func TestOnlyASubtaskOfAMainTalkMisledTheMainAgent(t *testing.T) {
	// e, which talk 2 started, was used and misled nothing.
	d := doc()
	d.Streams = append(d.Streams, sessionview.Stream{ID: "stream/e", Name: "e"})
	d.Talks = append(d.Talks, sessionview.Node{ID: "talk/e", Kind: "talk", Stream: "e", Children: []sessionview.Node{call("ce", "e", 0, "m", usage(1, 0, 0, 0))}})
	d.Talks[3].Children = append(d.Talks[3].Children, sessionview.Node{ID: "s4", Kind: "tool", Result: "ok"})
	d.Relations = append(d.Relations, sessionview.Relation{ID: "s4e", Type: model.RelStarts, From: "s4", To: "stream/e"})
	l := Compute(d, "claude-code/1", nil, nil)
	if st, _ := l.Subtask("stream/e"); st.Depth != 1 {
		t.Fatalf("e %+v", st)
	}
	got := misledOf(map[string]*sessionjudge.Subtask{
		"stream/a": {ID: "stream/a", Use: sessionjudge.UseMisled},
		"stream/b": {ID: "stream/b", Use: sessionjudge.UseMisled},
		"stream/e": {ID: "stream/e", Use: sessionjudge.UseUsed}}, l)
	if len(got) != 1 || got[0].ID != "stream/a" {
		t.Fatalf("misled %+v", got)
	}
}

// Whose doing work was follows who gave the task: a nested sub-task's wrong
// task is the sub-agent's that gave it, work already done given again is
// the giver's, and when the record does not show whether a misleading
// sub-task's task was right, the cause is unknown. The run's totals are the
// targets'.
func TestTheGiverOwnsATaskItGaveWrong(t *testing.T) {
	l := Compute(doc(), "claude-code/1", nil, nil)
	targets := []sessionjudge.Target{{ID: "t1", Status: sessionjudge.TargetClosed, Talks: []string{"talk/1"}}}
	labels := []*sessionjudge.Label{{Target: "t1", Talk: "talk/1", Label: sessionjudge.WorkMisled, Subtask: "stream/a"}}
	run := func(a, b sessionjudge.Subtask) (map[string]string, *sessionjudge.Outcome, *sessionjudge.Summary) {
		t.Helper()
		a.ID, b.ID = "stream/a", "stream/b"
		frames, sum := closeFrames(l, targets, nil, nil, labels, map[string]*sessionjudge.Subtask{a.ID: &a, b.ID: &b}, nil)
		got := map[string]string{}
		var o *sessionjudge.Outcome
		for _, f := range frames {
			switch x := f.(type) {
			case *sessionjudge.Cost:
				if x.Scope == sessionjudge.ScopeWork {
					got[x.Work] = x.Cause
				}
			case *sessionjudge.Outcome:
				o = x
			}
		}
		if sum.AvoidableBySubtasks.Input != o.AvoidableBySubtasks.Usage.Input || sum.AvoidableCauseUnknown.Input != o.AvoidableCauseUnknown.Usage.Input {
			t.Fatalf("the summary %+v %+v, the target %+v %+v", sum.AvoidableBySubtasks, sum.AvoidableCauseUnknown, o.AvoidableBySubtasks, o.AvoidableCauseUnknown)
		}
		return got, o, sum
	}
	// a gave b, nested under it, the wrong task: b's work is a's doing.
	got, _, _ := run(sessionjudge.Subtask{Dispatch: sessionjudge.DispatchReasonable, Use: sessionjudge.UseMisled, Work: sessionjudge.WorkUseful},
		sessionjudge.Subtask{Dispatch: sessionjudge.DispatchMisdirected, Work: sessionjudge.WorkMisdirected})
	if got[sessionjudge.WorkMisdirected] != "stream/a" || got[sessionjudge.WorkMisled] != "stream/a" {
		t.Fatalf("a nested wrong task: %v", got)
	}
	// The main agent gave a work already done: a's own cost is the main
	// agent's, while what its untrue report led to is a's.
	got, _, _ = run(sessionjudge.Subtask{Dispatch: sessionjudge.DispatchRedundant, Use: sessionjudge.UseMisled, Work: sessionjudge.WorkRedundant},
		sessionjudge.Subtask{Dispatch: sessionjudge.DispatchReasonable, Work: sessionjudge.WorkUseful})
	if got[sessionjudge.WorkRedundant] != sessionjudge.CauseMain || got[sessionjudge.WorkMisled] != "stream/a" {
		t.Fatalf("a redundant dispatch: %v", got)
	}
	// Whether a's task was right is unknown: so is whose doing the work on
	// its report was, counted apart from both.
	got, o, sum := run(sessionjudge.Subtask{Dispatch: sessionjudge.DispatchUnknown, Use: sessionjudge.UseMisled, Work: sessionjudge.WorkUseful},
		sessionjudge.Subtask{Dispatch: sessionjudge.DispatchReasonable, Work: sessionjudge.WorkUseful})
	if got[sessionjudge.WorkMisled] != sessionjudge.CauseUnknown || o.AvoidableCauseUnknown.Tokens() == 0 || o.AvoidableBySubtasks.Tokens() != 0 ||
		sum.AvoidableCauseUnknown.Input == 0 {
		t.Fatalf("an unknown dispatch: %v, outcome %+v", got, o.AvoidableCauseUnknown)
	}
}

// A part that begins at a tool begins at the model call that asked for it:
// the call is the work of the part too. In a runtime's view a tool is a
// child of that call.
func TestAPartBeginsAtTheCallThatAskedForItsFirstStep(t *testing.T) {
	c1 := call("c1", "main", 100, "m", usage(10, 0, 0, 0))
	c1.Children = []sessionview.Node{{ID: "ta", Kind: "tool", Result: "ok"}}
	c2 := call("c2", "main", 200, "m", usage(5, 0, 0, 0))
	c2.Children = []sessionview.Node{{ID: "tb", Kind: "tool", Result: "FAIL"}}
	d := &sessionview.Conversation{Conversation: "c", Streams: []sessionview.Stream{{ID: "stream/main", Name: "main"}},
		Talks: []sessionview.Node{{ID: "talk/1", Kind: "talk", Stream: "main", Children: []sessionview.Node{c1, c2}}}}
	l := Compute(d, "claude-code/1", nil, nil)
	targets := []sessionjudge.Target{{ID: "t1", Status: sessionjudge.TargetClosed, Talks: []string{"talk/1"}}}
	labels := []*sessionjudge.Label{{Target: "t1", Talk: "talk/1", Label: sessionjudge.WorkUseful},
		{Target: "t1", Talk: "talk/1", From: "tb", Label: sessionjudge.WorkRepeated}}
	_, sum := closeFrames(l, targets, nil, nil, labels, nil, nil)
	// c2, which asked for tb, is the repeated work: its 5 tokens, and c1's
	// 10 stay useful.
	if sum.Avoidable == nil || sum.Avoidable.Input != 5 {
		t.Fatalf("avoidable %+v", sum.Avoidable)
	}
}

// Two parts can begin inside one model call's answer: the call belongs to
// the first, in recorded order, whatever order the labels came in, and a
// child agent started inside the call keeps the label of the part its own
// starting step is in.
func TestPartsInsideOneCallKeepTheirOrder(t *testing.T) {
	// c1 asks for ta, which starts child x, and tb; c2 comes after.
	c1 := call("c1", "main", 100, "m", usage(10, 0, 0, 0))
	c1.Children = []sessionview.Node{{ID: "ta", Kind: "agent.call", Result: "ok"}, {ID: "tb", Kind: "tool", Result: "FAIL"}}
	c2 := call("c2", "main", 200, "m", usage(5, 0, 0, 0))
	d := &sessionview.Conversation{Conversation: "c", Streams: []sessionview.Stream{{ID: "stream/main", Name: "main"}, {ID: "stream/x", Name: "x"}},
		Talks: []sessionview.Node{{ID: "talk/1", Kind: "talk", Stream: "main", Children: []sessionview.Node{c1, c2}},
			{ID: "talk/x", Kind: "talk", Stream: "x", Children: []sessionview.Node{call("cx", "x", 150, "m", usage(100, 0, 0, 0))}}},
		Relations: []sessionview.Relation{{ID: "tax", Type: model.RelStarts, From: "ta", To: "stream/x"}}}
	l := Compute(d, "claude-code/1", nil, nil)
	targets := []sessionjudge.Target{{ID: "t1", Status: sessionjudge.TargetClosed, Talks: []string{"talk/1"}}}
	avoidable := func(labels ...*sessionjudge.Label) int {
		t.Helper()
		_, sum := closeFrames(l, targets, nil, nil, labels, nil, nil)
		return sum.Avoidable.Input
	}
	whole := &sessionjudge.Label{Target: "t1", Talk: "talk/1", Label: sessionjudge.WorkUseful}
	fromA := &sessionjudge.Label{Target: "t1", Talk: "talk/1", From: "ta", Label: sessionjudge.WorkRepeated}
	fromB := &sessionjudge.Label{Target: "t1", Talk: "talk/1", From: "tb", Label: sessionjudge.WorkUseful}
	// From ta repeated, from tb useful: c1 and x are ta's part, c2 tb's.
	for _, labels := range [][]*sessionjudge.Label{{whole, fromA, fromB}, {whole, fromB, fromA}} {
		if got := avoidable(labels...); got != 110 {
			t.Fatalf("labels in order %s, %s: %d avoidable", labels[1].From, labels[2].From, got)
		}
	}
	// From tb repeated alone: c1, which asked for it, and c2 are repeated;
	// x, started by ta before tb, keeps the whole talk's useful.
	fromB.Label = sessionjudge.WorkRepeated
	if got := avoidable(whole, fromB); got != 15 {
		t.Fatalf("from tb: %d avoidable", got)
	}
}

// A cancelled or replaced target is left out of the achievement score,
// whatever level it was given.
func TestACancelledTargetIsNotScored(t *testing.T) {
	var s scoreSum
	one := 1
	for _, status := range []string{sessionjudge.TargetCancelled, sessionjudge.TargetReplaced} {
		s.add(&sessionjudge.Outcome{Status: status, Level: sessionjudge.LevelNotAchieved, Met: new(int), Criteria: &one}, exactAmounts{})
	}
	s.add(&sessionjudge.Outcome{Status: sessionjudge.TargetClosed, Level: sessionjudge.LevelAchieved, Met: &one, Criteria: &one}, exactAmounts{})
	if sc := s.score(false); sc.Judged != 1 || *sc.Achievement != 1 {
		t.Fatalf("score %+v", sc)
	}
}
