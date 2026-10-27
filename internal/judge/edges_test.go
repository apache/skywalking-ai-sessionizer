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
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// subtasksOf returns a ledger's sub-tasks by stream name.
func subtasksOf(l *Ledger) map[string]Subtask {
	out := map[string]Subtask{}
	for _, st := range l.Subtasks {
		out[st.Name] = st
	}
	return out
}

// An unfinished call reads its input side from the first of its fragments
// that has a usage. A fragment the reader does not return, or one with no
// usage, is passed over.
func TestAnUnfinishedCallReadsTheFirstFragmentWithUsage(t *testing.T) {
	d := doc()
	cb2 := &d.Talks[2].Children[1]
	cb2.Attrs = json.RawMessage(`{"usage":"unavailable"}`)
	cb2.Refs = []sessionflow.Ref{{Seq: 7, Row: 1}, {Seq: 7, Row: 2}, {Seq: 7, Row: 3}}
	read := func([]sessionflow.Ref) map[[2]uint64]*sessiondata.Record {
		return map[[2]uint64]*sessiondata.Record{{7, 2}: {}, {7, 3}: {Usage: usage(3, 1, 500, 40)}}
	}
	l := Compute(d, "claude-code/1", nil, read)
	for _, c := range l.Calls {
		if c.ID == "cb2" && (c.Usage == nil || *c.Usage != (sessiondata.Usage{Input: 3, CacheRead: 500, CacheWrite: 40}) || !c.OutputUnknown) {
			t.Fatalf("cb2 %+v %+v", c, c.Usage)
		}
	}
}

// Only a step that starts a stream makes it a sub-task of its talk. A
// notification in talk/2 that child a finished does not make talk/2 a
// second starter of a.
func TestANotificationDoesNotStartAStream(t *testing.T) {
	d := doc()
	d.Talks[3].Children = append(d.Talks[3].Children, sessionview.Node{ID: "n2", Kind: model.KindRuntimeNotification})
	d.Relations = append(d.Relations, sessionview.Relation{ID: "n2a", Type: model.RelReports, From: "n2", To: "stream/a"})
	l := Compute(d, "claude-code/1", nil, nil)
	if a := subtasksOf(l)["a"]; a.Main != "talk/1" || a.StartedBy != "s1" || a.Reason != "" {
		t.Fatalf("a %+v", a)
	}
}

// A start of the main stream, or of a stream the document does not hold,
// makes no sub-task. The main stream is no one's sub-task, and its talks
// are never counted in another talk's cost.
func TestAStartOfNoChildStreamMakesNoSubtask(t *testing.T) {
	for _, to := range []string{"stream/main", "stream/none"} {
		d := doc()
		d.Relations = append(d.Relations, sessionview.Relation{ID: "s3b-" + to, Type: model.RelStarts, From: "s3b", To: to})
		l := Compute(d, "claude-code/1", nil, nil)
		var names []string
		for _, st := range l.Subtasks {
			names = append(names, st.Name)
		}
		two, _ := l.Inclusive("talk/2")
		if strings.Join(names, ",") != "a,b,c,d" || two.Calls != 1 {
			t.Errorf("a start of %s: sub-tasks %v, talk/2 counts %d calls", to, names, two.Calls)
		}
	}
}

// A start from a step the document does not hold names no starter. Child a
// stays the sub-task of talk/1, which holds its one known starter, and child
// d, which no step in the document started, has no starting step.
func TestAStartFromAStepNotInTheDocumentIsLeftOut(t *testing.T) {
	d := doc()
	d.Relations = append(d.Relations, sessionview.Relation{ID: "gone-a", Type: model.RelStarts, From: "gone", To: "stream/a"},
		sessionview.Relation{ID: "gone-d", Type: model.RelStarts, From: "gone", To: "stream/d"})
	got := subtasksOf(Compute(d, "claude-code/1", nil, nil))
	if a := got["a"]; a.Main != "talk/1" || a.StartedBy != "s1" {
		t.Fatalf("a %+v", a)
	}
	if st := got["d"]; st.Reason != ReasonNoStarter || st.StartedBy != "" || st.Parent != "" {
		t.Fatalf("d %+v", st)
	}
}

// A price entry must name its model: the ledger records it as the key of
// every price it applies. An entry without one is refused, named by its
// place in the file.
func TestAPriceEntryNamesItsModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pricing.yaml")
	body := "currency: USD\nmodels:\n  - model: m\n  - aliases: [x]\n    per_million: {in: 1}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPricing(path, ""); err == nil || !strings.Contains(err.Error(), "entry 2 names no model") {
		t.Fatalf("an entry with no model: %v", err)
	}
}

// A navigation server prices from the run's ledger in one currency. A
// ledger that names two is refused, since adding its amounts would add
// two currencies together.
func TestALedgerInTwoCurrenciesIsRefused(t *testing.T) {
	ledger := &sessionjudge.File{Prices: []sessionjudge.Price{
		{Model: "m", Key: "m", Currency: "USD", PerMillion: sessionjudge.Rates{In: 1}},
		{Model: "n", Key: "n", Currency: "EUR", PerMillion: sessionjudge.Rates{In: 1}},
	}}
	if _, err := PricingOf(ledger); err == nil || !strings.Contains(err.Error(), "the ledger prices in USD and EUR") {
		t.Fatalf("a ledger in two currencies: %v", err)
	}
}

// The chunks of a stage add up their coverage count by count. A count no
// chunk kept stays unset, so the stage file does not claim a zero it never
// counted.
func TestCoverageNoChunkCountedStaysUnset(t *testing.T) {
	one, two, three := 1, 2, 3
	sum := addCoverage(sessionjudge.Coverage{Inputs: &two, InputsRead: &one}, sessionjudge.Coverage{Inputs: &three})
	got, err := json.Marshal(sum)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"inputs":5,"inputs_read":1}` {
		t.Fatalf("coverage %s", got)
	}
}

// A request an evaluator reported only as the whole call's total is no
// single request. It marks the stage as reported whole, and is neither
// counted nor taken for the largest request.
func TestAWholeCallIsNotCountedAsOneRequest(t *testing.T) {
	s := &stageRun{}
	whole := sessiondata.Usage{Input: 9000, CacheRead: 90000, Output: 100}
	s.note(Result{Model: "m", Usage: &whole, Requests: []Request{{Usage: whole, Whole: true}}}, &sessionjudge.Reading{})
	one := sessiondata.Usage{Input: 10, CacheRead: 200, Output: 5}
	s.note(Result{Model: "m", Usage: &one, Requests: []Request{{Usage: one}}}, &sessionjudge.Reading{})
	if got := s.used.requests; got != (sessionjudge.Requests{Count: 1, Largest: 210, WholeOnly: true}) {
		t.Fatalf("requests %+v", got)
	}
}

// A plan names only the stages an evaluator answers. asz computes the
// ledger and close stages itself. A plan naming one of them, or a name that
// is no stage at all, is refused for that reason. A close stage would
// otherwise pass the order check, and the run would fail only after it was
// reserved.
func TestAPlanNamesOnlyStagesAnEvaluatorTakes(t *testing.T) {
	r := &Runner{
		Zone:     storage.NewZone(t.TempDir()),
		Config:   config.Judge{Timeout: "1m", Evaluators: map[string]config.Evaluator{"e": {Kind: "cli", Program: "e"}}},
		Executor: func(string, config.Evaluator) (Executor, error) { return nil, nil },
	}
	for _, stage := range []string{sessionjudge.StageLedger, sessionjudge.StageClose, "verdict"} {
		plan := []config.JudgeStage{{Stage: sessionjudge.StageTargets, Evaluator: "e"}, {Stage: stage, Evaluator: "e"}}
		_, err := r.Run(context.Background(), "conv", 0, plan)
		if want := fmt.Sprintf("%q is not a stage an evaluator takes", stage); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("a plan with %s: %v", stage, err)
		}
	}
}

// What an agent CLI read is counted from its navigation server's log, one
// answer per line. The log ends with a newline, and a server stopped while
// it wrote leaves part of a line: neither is an answer.
func TestReadingCountsEachLoggedAnswer(t *testing.T) {
	answers := `{"bytes":100}` + "\n" + `{"bytes":50,"refused":true}` + "\n"
	for name, log := range map[string]string{"whole": answers, "stopped": answers + `{"bytes":7`} {
		path := filepath.Join(t.TempDir(), "nav.log")
		if err := os.WriteFile(path, []byte(log), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := readingOf(nil, path, 4096); *got != (sessionjudge.Reading{Calls: 2, Bytes: 150, Budget: 4096, Refused: 1}) {
			t.Errorf("%s log: reading %+v", name, got)
		}
	}
}

// The kit has a schema and a prompt only for the stages an evaluator
// answers. asz computes the others, and a lookup of one is refused rather
// than read as an empty prompt.
func TestTheKitHoldsOnlyTheEvaluatorsStages(t *testing.T) {
	k, err := LoadKit()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Schema(sessionjudge.StageClose); err == nil || !strings.Contains(err.Error(), `no schema for stage "close"`) {
		t.Errorf("a schema for close: %v", err)
	}
	if _, _, err := k.Prompt(sessionjudge.StageClose, ""); err == nil || !strings.Contains(err.Error(), `no prompt for stage "close"`) {
		t.Errorf("a prompt for close: %v", err)
	}
}
