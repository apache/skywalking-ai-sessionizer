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

package tests_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp"
	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp/otlptest"
	"github.com/apache/skywalking-ai-sessionizer/internal/judge"
	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/retention"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
)

// standIn answers every stage from what the navigation shows it: one target
// served by every main talk, achieved, its first talk labeled repeated work
// and the rest useful. It stands where a model would, so the test covers what
// asz does with an answer, not what a model would answer.
type standIn struct{}

func (standIn) Describe() sessionjudge.Evaluator {
	return sessionjudge.Evaluator{Name: "stand-in", Kind: sessionjudge.EvaluatorCLI, Program: "stand-in"}
}

func (standIn) Run(_ context.Context, t judge.Task) (judge.Result, error) {
	main := t.Nav.Main()
	inputs := t.Nav.Inputs()
	var out any
	switch t.Stage {
	case sessionjudge.StageTargets:
		// The first call sets t1 under a key; a later chunk, which is given
		// the catalogue, carries it on by its id.
		mine := t.Nav.Assigned().Inputs
		if len(mine) == 0 {
			mine = inputs
		}
		first := !strings.Contains(t.Prompt, `"id":"t1"`)
		ref, action := "t1", "steer"
		if first {
			ref, action = "n1", "create"
		}
		var acts []map[string]any
		var talks []string
		for i, in := range mine {
			a := "steer"
			if i == 0 {
				a = action
			}
			acts = append(acts, map[string]any{"node": in, "actions": []any{map[string]string{"target": ref, "action": a}}})
			talks = append(talks, t.Nav.TalkOf(in))
		}
		entry := map[string]any{"id": "", "key": "n1", "statement": "build it", "criteria": []string{"it builds"}, "drop_criteria": []string{},
			"status": "closed", "replaced_by": "", "talks": talks, "cite": []string{mine[0]}}
		if !first {
			entry["id"], entry["key"], entry["statement"], entry["criteria"] = "t1", "", "", []string{}
		}
		out = map[string]any{"targets": []any{entry}, "inputs": acts}
	case sessionjudge.StageAchievement:
		// A tool step that returned shows the work; with none, nothing
		// shows it, and an honest answer says so.
		var shown []string
		for _, tk := range main {
			outline, err := t.Nav.Call("talk", json.RawMessage(`{"id":"`+tk+`"}`))
			if err != nil {
				continue
			}
			b, _ := json.Marshal(outline)
			var rows struct {
				Rows []struct {
					ID     string `json:"id"`
					Kind   string `json:"kind"`
					Result string `json:"result"`
				} `json:"rows"`
			}
			_ = json.Unmarshal(b, &rows)
			for _, r := range rows.Rows {
				if r.Kind == "tool" && r.Result != "" {
					shown = append(shown, r.ID)
				}
			}
		}
		state, basis := "met", "observed_result"
		if len(shown) == 0 {
			state, basis = "unknown", "no_evidence"
		}
		out = map[string]any{"achievements": []any{map[string]any{"target": "t1", "basis": basis,
			"criteria": []any{map[string]any{"id": "t1.c1", "state": state, "delivered": false, "cite": shown}},
			"claims":   []any{}, "note": "", "cite": shown}}}
	case sessionjudge.StageSubtasks:
		// Every sub-task the navigation lists at the top, judged useful on
		// the step that started it.
		ans, err := t.Nav.Call("subtasks", nil)
		if err != nil {
			return judge.Result{}, err
		}
		b, _ := json.Marshal(ans)
		var pg struct {
			Rows []struct {
				ID        string   `json:"id"`
				StartedBy string   `json:"started_by"`
				Talks     []string `json:"talks"`
			} `json:"rows"`
		}
		_ = json.Unmarshal(b, &pg)
		decisions := []any{}
		mine := map[string]bool{}
		for _, id := range t.Nav.Assigned().Subtasks {
			mine[id] = true
		}
		for _, r := range pg.Rows {
			if len(mine) > 0 && !mine[r.ID] {
				continue
			}
			// A step of the sub-task's own talk is the evidence of what it did.
			cite := []string{}
			shownBy := 0
			if len(r.Talks) > 0 {
				if outline, err := t.Nav.Call("talk", json.RawMessage(`{"id":"`+r.Talks[0]+`"}`)); err == nil {
					b, _ := json.Marshal(outline)
					var steps struct {
						Rows []struct {
							ID   string `json:"id"`
							Kind string `json:"kind"`
						} `json:"rows"`
					}
					_ = json.Unmarshal(b, &steps)
					tools := 0
					for i, st := range steps.Rows {
						if st.Kind == "tool" || i == 0 {
							cite = append(cite, st.ID)
						}
						if st.Kind == "tool" {
							tools++
						}
					}
					shownBy = tools
				}
			}
			// A sub-agent that only wrote shows nothing beyond its words;
			// its work was still read, and is labelled.
			outcome, report := "achieved", "accurate"
			if shownBy == 0 {
				outcome, report = "cannot_verify", "unverifiable"
			}
			decisions = append(decisions, map[string]any{"ids": []string{r.ID}, "outcome": outcome, "report": report,
				"use": "unknown", "dispatch": "reasonable", "work": "useful", "note": "", "cite": cite})
		}
		out = map[string]any{"subtasks": decisions}
	case sessionjudge.StageEfficiency:
		var labels []any
		labelled := main
		if a := t.Nav.Assigned().Talks; len(a) > 0 {
			labelled = a
		}
		for i, tk := range labelled {
			label := "useful"
			if i == 0 {
				label = "repeated"
			}
			// A label cites a step of the talk it labels.
			step := ""
			if outline, err := t.Nav.Call("talk", json.RawMessage(`{"id":"`+tk+`"}`)); err == nil {
				b, _ := json.Marshal(outline)
				var rows struct {
					Rows []struct {
						ID string `json:"id"`
					} `json:"rows"`
				}
				_ = json.Unmarshal(b, &rows)
				if len(rows.Rows) > 0 {
					step = rows.Rows[0].ID
				}
			}
			labels = append(labels, map[string]any{"target": "t1", "talk": tk, "from": "", "label": label, "note": "", "cite": []string{step}})
		}
		out = map[string]any{"labels": labels}
	}
	b, _ := json.Marshal(out)
	return judge.Result{Output: b, Usage: &sessiondata.Usage{Input: 1, Output: 2}}, nil
}

func builtFixture(t *testing.T) (*storage.Zone, string) {
	t.Helper()
	return built(t, "scenarios/fixture.yaml")
}

// threeRequests is a conversation of three requests, the second handed in
// part to a sub-agent, so a stage can be cut into several chunks.
const threeRequests = `steps:
  - input: build the parser
  - call: {text: The parser is built.}
  - input: add tests for it
  - call:
      agent: {name: tester, prompt: write the parser tests, steps: [{call: {text: tests written}}], notify: true}
  - call: {text: The tests are added.}
  - input: now ship it
  - call: {text: Shipped.}
`

func builtThree(t *testing.T) (*storage.Zone, string) {
	t.Helper()
	return built(t, writeScenario(t, threeRequests))
}

func built(t *testing.T, path string) (*storage.Zone, string) {
	t.Helper()
	sc, err := scenario.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	b, err := scenario.Build(sc, scenario.FormatSD, out, scenario.Options{At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	z := storage.NewZone(out)
	for {
		r, err := parse.Session(z, parse.Options{Conversation: b.Session, Session: b.Session})
		if err != nil {
			t.Fatal(err)
		}
		if !r.Changed() || !r.More {
			break
		}
	}
	return z, b.Session
}

func runner(z *storage.Zone) *judge.Runner {
	return &judge.Runner{
		Zone:    z,
		Config:  config.Judge{Timeout: "1m", Evaluators: map[string]config.Evaluator{"stand-in": {Kind: "cli", Program: "stand-in"}}},
		Dialect: func(string) string { return "claude-code/1" },
		MCP:     func(judge.MCPArgs) []string { return []string{"unused"} },
		Executor: func(string, config.Evaluator) (judge.Executor, error) {
			return standIn{}, nil
		},
	}
}

var plan = []config.JudgeStage{{Stage: "targets", Evaluator: "stand-in"}, {Stage: "achievement", Evaluator: "stand-in"},
	{Stage: "subtasks", Evaluator: "stand-in"}, {Stage: "efficiency", Evaluator: "stand-in"}}

func readStage(t *testing.T, path string) *sessionjudge.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sj, err := sessionjudge.Read(f)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return sj
}

func add(to *sessiondata.Usage, u sessiondata.Usage, share float64) {
	to.Input += int(float64(u.Input) * share)
	to.Output += int(float64(u.Output) * share)
	to.CacheRead += int(float64(u.CacheRead) * share)
	to.CacheWrite += int(float64(u.CacheWrite) * share)
}

// A run writes one verified file per stage, prices every label from the
// ledger, and leaves the chain as it was.
func TestAJudgeRunIsWrittenOnceAndAddsUp(t *testing.T) {
	z, id := builtFixture(t)
	rounds := func() string {
		var names []string
		_ = filepath.WalkDir(filepath.Join(z.Root(), "_conversations"), func(p string, d os.DirEntry, _ error) error {
			if d != nil && !d.IsDir() {
				info, _ := d.Info()
				names = append(names, fmt.Sprintf("%s %d", p, info.Size()))
			}
			return nil
		})
		return strings.Join(names, "\n")
	}
	before := rounds()
	run, err := runner(z).Run(context.Background(), id, 0, plan)
	if err != nil {
		t.Fatal(err)
	}
	if run.Number != 1 || run.Status != sessionjudge.StatusComplete || len(run.Files) != 6 {
		t.Fatalf("run %d %s: %s", run.Number, run.Status, notes(t, run))
	}
	if rounds() != before {
		t.Fatal("a judge run changed the chain")
	}
	byStage := map[string]*sessionjudge.File{}
	for _, p := range run.Files {
		info, err := os.Stat(p)
		if err != nil || info.Mode().Perm() != 0o444 {
			t.Fatalf("%s is not written once and read-only: %v %v", p, info.Mode(), err)
		}
		f := readStage(t, p)
		byStage[f.Header.Stage] = f
	}
	ledger, closeFile := byStage["ledger"], byStage["close"]

	// The ledger's main talks, the main stream's loose calls and the
	// unattributed calls add up to the total exactly.
	var total sessionjudge.Cost
	var sum sessiondata.Usage
	calls := 0
	for _, c := range ledger.Costs {
		switch c.Scope {
		case sessionjudge.ScopeConversation:
			total = c
		case sessionjudge.ScopeTalk, sessionjudge.ScopeLoose, sessionjudge.ScopeUnattributed:
			add(&sum, c.Usage, 1)
			calls += c.Calls
		}
	}
	if total.Calls == 0 || sum != total.Usage || calls != total.Calls {
		t.Fatalf("ledger parts %+v in %d calls, total %+v", sum, calls, total)
	}

	// Every main talk serves t1, so t1 and the unassigned and unattributed
	// calls are the whole conversation, and t1's work adds up to t1.
	var target, rest, work, avoidable sessiondata.Usage
	for _, c := range closeFile.Costs {
		share := 1.0
		if c.Share != nil {
			share = *c.Share
		}
		switch c.Scope {
		case sessionjudge.ScopeTarget, sessionjudge.ScopeShared:
			add(&target, c.Usage, share)
		case sessionjudge.ScopeUnassigned, sessionjudge.ScopeUnattributed:
			add(&rest, c.Usage, 1)
		case sessionjudge.ScopeWork:
			add(&work, c.Usage, share)
			if sessionjudge.Avoidable(c.Work) {
				add(&avoidable, c.Usage, share)
			}
		}
	}
	all := target
	add(&all, rest, 1)
	if all != total.Usage || work != target {
		t.Fatalf("target %+v rest %+v work %+v, total %+v", target, rest, work, total.Usage)
	}
	s := closeFile.Header.Summary
	if s == nil || s.Targets != 1 || s.Achieved != 1 || s.Avoidable == nil || *s.Avoidable != avoidable || avoidable == (sessiondata.Usage{}) {
		t.Fatalf("summary %+v, avoidable %+v", s, avoidable)
	}
	if len(closeFile.Stages) != 5 || closeFile.Stages[0].Stage != "ledger" || closeFile.Outcomes[0].Level != "achieved" {
		t.Fatalf("close stages %+v outcomes %+v", closeFile.Stages, closeFile.Outcomes)
	}
	// The stand-in read the sub-tasks through the navigation, and the stage
	// says how much.
	if r := byStage["subtasks"].Header.Reading; r == nil || r.Calls != 2 || r.Bytes == 0 {
		t.Fatalf("reading %+v", r)
	}
	// The fixture's child agent is a sub-task, judged once, and the close
	// file shows its cost with the label it was given.
	subs := byStage["subtasks"]
	if len(subs.Subtasks) == 0 || subs.Subtasks[0].Work != "useful" || *subs.Header.Coverage.SubtasksJudged != len(subs.Subtasks) {
		t.Fatalf("sub-tasks %+v coverage %+v", subs.Subtasks, subs.Header.Coverage)
	}
	viewed := false
	for _, c := range closeFile.Costs {
		viewed = viewed || (c.Scope == sessionjudge.ScopeSubtask && c.ID == subs.Subtasks[0].ID && c.Work == "useful" && c.Calls > 0)
	}
	if !viewed {
		t.Fatalf("no cost of sub-task %s in the close file", subs.Subtasks[0].ID)
	}
	// Each stage names the stages it read, by digest.
	if eff := byStage["efficiency"]; len(eff.Header.BuildsOn) != 2 || eff.Header.BuildsOn[1].Digest != byStage["targets"].Commit.Digest {
		t.Fatalf("efficiency builds on %+v", eff.Header.BuildsOn)
	}

	// A second run at the same head is a second run.
	again, err := runner(z).Run(context.Background(), id, 0, plan)
	if err != nil || again.Number != 2 || again.Head != run.Head {
		t.Fatalf("second run %+v %v", again, err)
	}
	runs, err := storage.JudgmentRuns(z, id)
	if err != nil || len(runs) != 2 || !runs[0].Closed() || !runs[1].Closed() {
		t.Fatalf("runs %+v %v", runs, err)
	}
}

// A retired session gets no run.
func TestARetiredSessionIsNotJudged(t *testing.T) {
	z, id := builtFixture(t)
	if err := os.MkdirAll(filepath.Join(z.Root(), "_retired"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(z.Root(), "_retired", id), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := storage.ReserveRun(z, id); !errors.Is(err, storage.ErrRetiredSession) {
		t.Fatalf("a retired session got a run: %v", err)
	}
}

// A closed run goes to the receiver as one record per stage file, the close
// file last, each as it was written; a run that never closed stays here.
func TestAClosedRunIsSentAndAnOpenOneWaits(t *testing.T) {
	z, id := builtFixture(t)
	if _, err := runner(z).Run(context.Background(), id, 0, plan); err != nil {
		t.Fatal(err)
	}
	// A second run that stopped before its close file.
	_, dir, err := storage.ReserveRun(z, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "s1-ledger-000000000000.sj"), []byte("x\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	rcv, err := otlptest.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rcv.Close)
	client, err := rcv.Client(otlp.ProtocolGRPC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	p := &otlp.Pusher{Zone: z, Client: client, Version: "test", ServiceName: "Claude Code", InstanceID: "me", NoMetrics: true}
	st, err := p.Pass()
	if err != nil || len(st.Errors) > 0 {
		t.Fatalf("pass %+v %v", st, err)
	}
	var stages []string
	for _, r := range otlptest.Records(rcv.Requests()) {
		a := otlptest.Attrs(r.GetAttributes())
		if a["asz.format"] != "sj" {
			continue
		}
		if a["asz.judgment.run"] != "int:1" || a["asz.file.kind"] != "judgment" || a["asz.conversation"] != id {
			t.Fatalf("attributes %v", a)
		}
		f, err := sessionjudge.Read(strings.NewReader(r.GetBody().GetStringValue()))
		if err != nil {
			t.Fatalf("the body is not the file as written: %v", err)
		}
		if f.Header.Stage == "close" && a["asz.judgment.achieved"] != "int:1" {
			t.Fatalf("close attributes %v", a)
		}
		stages = append(stages, a["asz.judgment.stage"])
	}
	if strings.Join(stages, ",") != "ledger,targets,achievement,subtasks,efficiency,close" {
		t.Fatalf("sent %v", stages)
	}
	// Nothing goes twice.
	rcv.Reset()
	if _, err := p.Pass(); err != nil {
		t.Fatal(err)
	}
	for _, r := range otlptest.Records(rcv.Requests()) {
		if otlptest.Attrs(r.GetAttributes())["asz.format"] == "sj" {
			t.Fatal("a stage file was sent twice")
		}
	}
}

// A retirement removes a session's judge runs with the session: they live in
// its directory, so nothing of a retired session is left to send.
func TestRetirementTakesTheRunsWithIt(t *testing.T) {
	z, id := builtFixture(t)
	if _, err := runner(z).Run(context.Background(), id, 0, plan); err != nil {
		t.Fatal(err)
	}
	// A scenario root is never retired, so the root is made an ordinary one.
	_ = os.RemoveAll(filepath.Join(z.Root(), storage.ScenarioDir))
	runs := filepath.Join(z.SessionDir(id), storage.JudgmentsDir)
	if _, err := os.Stat(runs); err != nil {
		t.Fatal(err)
	}
	rem := &retention.Remover{Zone: z, MetadataOnlyAfter: time.Hour, RetireAfter: time.Hour,
		Now: func() time.Time { return time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC) }}
	if res := rem.Pass(retention.Input{}); len(res.Errors) > 0 || len(res.Retired) != 1 {
		t.Fatalf("not retired: %+v", res)
	}
	if _, err := os.Stat(runs); !os.IsNotExist(err) {
		t.Fatalf("the runs outlived their session: %v", err)
	}
}

// A plan that asks a question twice, or names an evaluator that cannot be
// made, is refused before any run is reserved, so no run is left open.
func TestABadPlanReservesNothing(t *testing.T) {
	z, id := builtFixture(t)
	r := runner(z)
	for name, p := range map[string][]config.JudgeStage{
		"twice":    append(append([]config.JudgeStage{}, plan...), config.JudgeStage{Stage: "achievement", Evaluator: "stand-in"}),
		"no such":  {{Stage: "targets", Evaluator: "nobody"}},
		"no start": {{Stage: "achievement", Evaluator: "stand-in"}},
	} {
		if _, err := r.Run(context.Background(), id, 0, p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	r.Executor = func(string, config.Evaluator) (judge.Executor, error) { return nil, errors.New("cannot be made") }
	if _, err := r.Run(context.Background(), id, 0, plan); err == nil {
		t.Error("an evaluator that cannot be made was accepted")
	}
	if _, err := os.Stat(filepath.Join(z.SessionDir(id), storage.JudgmentsDir)); !os.IsNotExist(err) {
		t.Fatalf("a refused plan reserved a run: %v", err)
	}
}

// A run holds one file per stage: a second answer to the same stage is
// refused, whatever its digest.
func TestAStageIsWrittenOnce(t *testing.T) {
	z, id := builtFixture(t)
	_, dir, err := storage.ReserveRun(z, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.PublishStage(z, id, dir, 3, "achievement", strings.Repeat("a", 64), []byte("x\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.PublishStage(z, id, dir, 3, "achievement", strings.Repeat("b", 64), []byte("y\n")); err == nil {
		t.Fatal("a second achievement stage was written")
	}
}

// A stage's work directory is never made for a retired session, so a run
// that outlives a retirement does not make the session again.
func TestAWorkDirectoryIsNotMadeForARetiredSession(t *testing.T) {
	z, id := builtFixture(t)
	_, dir, err := storage.ReserveRun(z, id)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(z.Root(), "_retired"), 0o755)
	if err := os.WriteFile(filepath.Join(z.Root(), "_retired", id), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(z.SessionDir(id)); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.MakeWorkDir(z, id, dir, "2-targets"); !errors.Is(err, storage.ErrRetiredSession) {
		t.Fatalf("made for a retired session: %v", err)
	}
	if _, err := os.Stat(z.SessionDir(id)); !os.IsNotExist(err) {
		t.Fatalf("the retired session's directory was made again: %v", err)
	}
}

// failLedgerOnce refuses the first request that carries a run's ledger, as a
// receiver that is briefly away would.
type failLedgerOnce struct {
	otlp.Client
	failed bool
}

func (f *failLedgerOnce) Export(req *collogspb.ExportLogsServiceRequest) (int64, error) {
	for _, r := range otlptest.Records([]*collogspb.ExportLogsServiceRequest{req}) {
		if otlptest.Attrs(r.GetAttributes())["asz.judgment.stage"] == "ledger" && !f.failed {
			f.failed = true
			return 0, errors.New("away")
		}
	}
	return f.Client.Export(req)
}

// A run's close file never reaches the receiver before the stages it
// closes: while the ledger has not gone, the close waits for a later pass.
func TestACloseWaitsForItsStages(t *testing.T) {
	z, id := builtFixture(t)
	if _, err := runner(z).Run(context.Background(), id, 0, plan); err != nil {
		t.Fatal(err)
	}
	rcv, err := otlptest.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rcv.Close)
	client, err := rcv.Client(otlp.ProtocolGRPC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	p := &otlp.Pusher{Zone: z, Client: &failLedgerOnce{Client: client}, Version: "test", ServiceName: "Claude Code",
		InstanceID: "me", NoMetrics: true, BatchBytes: 1}
	stages := func() []string {
		var out []string
		for _, r := range otlptest.Records(rcv.Requests()) {
			if a := otlptest.Attrs(r.GetAttributes()); a["asz.format"] == "sj" {
				out = append(out, a["asz.judgment.stage"])
			}
		}
		return out
	}
	if _, err := p.Pass(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(stages(), ","); got != "targets,achievement,subtasks,efficiency" {
		t.Fatalf("first pass sent %s", got)
	}
	if _, err := p.Pass(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(stages(), ","); got != "targets,achievement,subtasks,efficiency,ledger,close" {
		t.Fatalf("second pass left %s", got)
	}
}

func judgedAndPushed(t *testing.T) (*storage.Zone, string, *otlptest.Receiver, *otlp.Pusher) {
	t.Helper()
	z, id := builtFixture(t)
	if _, err := runner(z).Run(context.Background(), id, 0, plan); err != nil {
		t.Fatal(err)
	}
	rcv, err := otlptest.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rcv.Close)
	client, err := rcv.Client(otlp.ProtocolGRPC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return z, id, rcv, &otlp.Pusher{Zone: z, Client: client, Version: "test", ServiceName: "Claude Code", InstanceID: "me", NoMetrics: true}
}

func sentStages(rcv *otlptest.Receiver) []string {
	var out []string
	for _, r := range otlptest.Records(rcv.Requests()) {
		if a := otlptest.Attrs(r.GetAttributes()); a["asz.format"] == "sj" {
			out = append(out, a["asz.judgment.stage"])
		}
	}
	return out
}

// A stage still unsent goes even when its run's close went already, as an
// older asz could send the close first; the close is not sent again.
func TestAStageLeftBehindStillGoes(t *testing.T) {
	z, _, rcv, p := judgedAndPushed(t)
	if _, err := p.Pass(); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(z.Root(), otlp.StateFile)
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, line := range strings.Split(string(state), "\n") {
		if !strings.Contains(line, "/s1-ledger-") {
			kept = append(kept, line)
		}
	}
	if err := os.WriteFile(statePath, []byte(strings.Join(kept, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	rcv.Reset()
	if _, err := p.Pass(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sentStages(rcv), ","); got != "ledger" {
		t.Fatalf("second pass sent %q", got)
	}
}

// A run whose close lists a stage it no longer holds is not sent at all.
func TestARunMissingAStageIsNotSent(t *testing.T) {
	z, id, rcv, p := judgedAndPushed(t)
	runs, err := storage.JudgmentRuns(z, id)
	if err != nil || len(runs) != 1 {
		t.Fatal(runs, err)
	}
	for _, f := range runs[0].Files {
		if f.Stage == "efficiency" {
			if err := os.Remove(f.Path); err != nil {
				t.Fatal(err)
			}
		}
	}
	st, err := p.Pass()
	if err != nil {
		t.Fatal(err)
	}
	if got := sentStages(rcv); len(got) != 0 || len(st.Errors) == 0 {
		t.Fatalf("sent %v, errors %v", got, st.Errors)
	}
}

// A closed run is sent only as its close file lists it: a stage file that
// does not read, or a valid stage the close does not list, holds the whole
// run back and names that file.
func TestARunNotAsItsCloseListsIsNotSent(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, run storage.JudgmentRun) string{
		"a stage that does not read": func(t *testing.T, run storage.JudgmentRun) string {
			for _, f := range run.Files {
				if f.Stage == "targets" {
					if err := os.Chmod(f.Path, 0o644); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(f.Path, []byte("not a stage\n"), 0o644); err != nil {
						t.Fatal(err)
					}
					return filepath.Base(f.Path)
				}
			}
			t.Fatal("no targets stage")
			return ""
		},
		"a stage the close does not list": func(t *testing.T, run storage.JudgmentRun) string {
			for _, f := range run.Files {
				if f.Stage == "targets" {
					h := readStage(t, f.Path).Header
					h.Finished = "2026-10-22T00:00:00Z"
					w, err := sessionjudge.NewWriter(h)
					if err != nil {
						t.Fatal(err)
					}
					data, digest, err := w.Close()
					if err != nil {
						t.Fatal(err)
					}
					extra := fmt.Sprintf("s8-targets-%s.sj", digest[:12])
					if err := os.WriteFile(filepath.Join(run.Dir, extra), data, 0o444); err != nil {
						t.Fatal(err)
					}
					return extra
				}
			}
			t.Fatal("no targets stage")
			return ""
		},
	} {
		t.Run(name, func(t *testing.T) {
			z, id, rcv, p := judgedAndPushed(t)
			runs, err := storage.JudgmentRuns(z, id)
			if err != nil || len(runs) != 1 {
				t.Fatal(runs, err)
			}
			file := change(t, runs[0])
			st, err := p.Pass()
			if err != nil {
				t.Fatal(err)
			}
			if got := sentStages(rcv); len(got) != 0 || !strings.Contains(fmt.Sprint(st.Errors), file) {
				t.Fatalf("sent %v, errors %v", got, st.Errors)
			}
		})
	}
}

// A stage cut into chunks reaches the same targets as one call: the scan
// carries the catalogue from chunk to chunk, each chunk answers for its own
// inputs, and each stage says how it ran and how many calls it made.
func TestAStreamedRunScansItsChunks(t *testing.T) {
	z, id := builtThree(t)
	r := runner(z)
	r.Config.Mode, r.Config.SingleBytes, r.Config.ChunkBytes, r.Config.Concurrency = "stream", 1, 1, 2
	run, err := r.Run(context.Background(), id, 0, plan)
	if err != nil {
		t.Fatal(err)
	}
	byStage := map[string]*sessionjudge.File{}
	for _, p := range run.Files {
		f := readStage(t, p)
		byStage[f.Header.Stage] = f
	}
	tg := byStage["targets"]
	if run.Status != sessionjudge.StatusComplete || tg.Header.Mode != "stream" || tg.Header.Chunks < 2 || tg.Header.Attempts != tg.Header.Chunks {
		t.Fatalf("run %s: %s; targets %+v", run.Status, notes(t, run), tg.Header)
	}
	if len(tg.Targets) != 1 || tg.Targets[0].ID != "t1" || len(tg.Inputs) != tg.Header.Chunks {
		t.Fatalf("targets %+v inputs %d", tg.Targets, len(tg.Inputs))
	}
	if tg.Inputs[0].Actions[0].Action != "create" || tg.Inputs[1].Actions[0] != (sessionjudge.Action{Target: "t1", Action: "steer"}) {
		t.Fatalf("inputs %+v", tg.Inputs)
	}
	if eff := byStage["efficiency"]; eff.Header.Chunks != len(tg.Targets[0].Talks) || len(eff.Labels) != eff.Header.Chunks {
		t.Fatalf("efficiency %+v labels %d", eff.Header, len(eff.Labels))
	}
	if u := tg.Header.Usage; u == nil || u.Input != tg.Header.Attempts {
		t.Fatalf("the targets stage's usage %+v adds up its %d calls", u, tg.Header.Attempts)
	}
}

// flaky fails the first attempt of every call, which a retry recovers.
type flaky struct{ standIn }

func (f flaky) Run(ctx context.Context, t judge.Task) (judge.Result, error) {
	if filepath.Base(t.Dir) == "a1" {
		return judge.Result{Usage: &sessiondata.Usage{Input: 1}}, fmt.Errorf("the model went away")
	}
	return f.standIn.Run(ctx, t)
}

// A call that fails is tried once more in a directory of its own, and what
// the failed attempt consumed still counts.
func TestAFailedCallIsTriedAgain(t *testing.T) {
	z, id := builtFixture(t)
	r := runner(z)
	r.Executor = func(string, config.Evaluator) (judge.Executor, error) { return flaky{}, nil }
	run, err := r.Run(context.Background(), id, 0, plan)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != sessionjudge.StatusComplete {
		t.Fatalf("run %s", run.Status)
	}
	for _, p := range run.Files {
		h := readStage(t, p).Header
		if h.Evaluator.Kind == sessionjudge.EvaluatorComputed || h.Chunks == 0 {
			continue
		}
		if h.Attempts != 2*h.Chunks || h.Usage == nil || h.Usage.Input != 2*h.Chunks {
			t.Fatalf("%s: %d chunks, %d attempts, usage %+v", h.Stage, h.Chunks, h.Attempts, h.Usage)
		}
	}
	if _, err := os.Stat(filepath.Join(run.Dir, "work", "2-targets", "c001", "a1")); err != nil {
		t.Fatalf("the failed attempt left nothing: %v", err)
	}
}

// broken fails every call of one chunk of the targets stage.
type broken struct {
	standIn
	chunk string
}

func (b broken) Run(ctx context.Context, t judge.Task) (judge.Result, error) {
	if t.Stage == sessionjudge.StageTargets && strings.Contains(t.Dir, b.chunk) {
		return judge.Result{}, fmt.Errorf("no answer")
	}
	return b.standIn.Run(ctx, t)
}

// A targets scan is only as good as every chunk of it: one that fails fails
// the stage, nothing is judged against what the scan left, and the stage
// counts the inputs it never read.
func TestAFailedChunkFailsTheTargetsScan(t *testing.T) {
	z, id := builtThree(t)
	r := runner(z)
	r.Config.Mode, r.Config.SingleBytes, r.Config.ChunkBytes = "stream", 1, 1
	// The scan says it was to read every input, and how few it did, none
	// when its first call failed.
	for chunk, read := range map[string]int{"c002": 1, "c001": 0} {
		r.Executor = func(string, config.Evaluator) (judge.Executor, error) { return broken{chunk: chunk}, nil }
		run, err := r.Run(context.Background(), id, 0, plan)
		if err != nil {
			t.Fatal(err)
		}
		stages := map[string]string{}
		var cov *sessionjudge.Coverage
		for _, p := range run.Files {
			h := readStage(t, p).Header
			stages[h.Stage] = h.Status
			if h.Stage == "targets" {
				cov = h.Coverage
			}
		}
		if run.Status != sessionjudge.StatusPartial || stages["targets"] != sessionjudge.StatusFailed || stages["achievement"] != "" {
			t.Fatalf("run %s, stages %v", run.Status, stages)
		}
		if cov == nil || cov.Inputs == nil || *cov.Inputs != 3 || cov.InputsRead == nil || *cov.InputsRead != read {
			t.Fatalf("%s failing: targets coverage %+v", chunk, cov)
		}
	}
}

// A stage with nothing to answer, such as sub-tasks in a conversation with
// none, makes no call, and leaves what judging consumed known.
func TestAStageWithNothingToAnswerMakesNoCall(t *testing.T) {
	z, id := built(t, writeScenario(t, "steps:\n  - input: say hello\n  - call: {text: Hello.}\n"))
	run, err := runner(z).Run(context.Background(), id, 0, plan)
	if err != nil {
		t.Fatal(err)
	}
	var closeFile *sessionjudge.File
	for _, p := range run.Files {
		f := readStage(t, p)
		switch f.Header.Stage {
		case "subtasks":
			// It consumed nothing, which is known, and covered no sub-task.
			h := f.Header
			if h.Chunks != 0 || h.Attempts != 0 || h.Status != sessionjudge.StatusComplete || h.Usage == nil || *h.Usage != (sessiondata.Usage{}) ||
				h.Coverage == nil || h.Coverage.Subtasks == nil || *h.Coverage.Subtasks != 0 || *h.Coverage.SubtasksJudged != 0 {
				t.Fatalf("sub-tasks %+v, coverage %+v", h, h.Coverage)
			}
		case "close":
			closeFile = f
		}
	}
	if closeFile.Header.Summary.Judging == nil {
		t.Fatal("judging is unknown after a stage that made no call")
	}
}

func writeScenario(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scenario.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// talker reports two requests as it goes, slowly, as a real evaluator does.
type talker struct{ standIn }

func (s talker) Run(ctx context.Context, t judge.Task) (judge.Result, error) {
	var reqs []judge.Request
	for _, in := range []int{100, 300} {
		r := judge.Request{Model: "m", Usage: sessiondata.Usage{Input: 1, CacheRead: in, Output: 2}}
		reqs = append(reqs, r)
		t.Progress(r)
		time.Sleep(30 * time.Millisecond)
	}
	res, err := s.standIn.Run(ctx, t)
	res.Requests, res.ContextWindow = reqs, 1000
	return res, err
}

// A running stage says every so often what it has consumed, and each stage
// records its requests: how many, and the largest against the model's
// window.
func TestAStageSaysWhatItConsumesAsItRuns(t *testing.T) {
	z, id := builtThree(t)
	r := runner(z)
	r.Config.Progress = "20ms"
	r.Executor = func(string, config.Evaluator) (judge.Executor, error) { return talker{}, nil }
	var mu sync.Mutex
	var lines []string
	r.Log = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	run, err := r.Run(context.Background(), id, 0, plan)
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(lines, "\n")
	if !strings.Contains(all, "calls done") || !strings.Contains(all, "requests, the largest 301 of a 1.0k window") ||
		!strings.Contains(all, "output tokens per minute") || !strings.Contains(all, "run 1 judging: ") {
		t.Fatalf("progress:\n%s", all)
	}
	for _, l := range lines {
		if strings.Contains(l, "calls done") && !strings.Contains(l, "output tokens per minute") {
			t.Fatalf("a progress line without its rate: %s", l)
		}
	}
	for _, p := range run.Files {
		h := readStage(t, p).Header
		if h.Stage == "close" && h.Summary.JudgingMs <= 0 {
			t.Fatalf("the run's judging time %+v", h.Summary)
		}
		if h.Stage == "targets" && (h.Requests == nil || h.Requests.Count != 2*h.Chunks || h.Requests.Largest != 301 || h.Requests.ContextWindow != 1000) {
			t.Fatalf("targets requests %+v", h.Requests)
		}
	}
}

// notes says how each stage of a run ended, for a test that expected it to
// end otherwise.
func notes(t *testing.T, run *judge.Run) string {
	t.Helper()
	var out []string
	for _, p := range run.Files {
		h := readStage(t, p).Header
		out = append(out, fmt.Sprintf("%s %s %s", h.Stage, h.Status, h.Note))
	}
	return strings.Join(out, "; ")
}

// subtasksFail answers every stage but the sub-task stage, which fails.
type subtasksFail struct{ standIn }

func (s subtasksFail) Run(ctx context.Context, t judge.Task) (judge.Result, error) {
	if t.Stage == sessionjudge.StageSubtasks {
		return judge.Result{}, fmt.Errorf("no answer")
	}
	return s.standIn.Run(ctx, t)
}

// A planned sub-task stage that fails leaves the sub-tasks not reviewed.
// Efficiency labeled only each talk's own stream, so its labels never reach
// the sub-tasks' calls.
func TestAFailedSubtaskStageLeavesThemNotReviewed(t *testing.T) {
	z, id := builtFixture(t)
	r := runner(z)
	r.Executor = func(string, config.Evaluator) (judge.Executor, error) { return subtasksFail{}, nil }
	run, err := r.Run(context.Background(), id, 0, plan)
	if err != nil {
		t.Fatal(err)
	}
	var closeFile, subs *sessionjudge.File
	for _, p := range run.Files {
		f := readStage(t, p)
		switch f.Header.Stage {
		case "close":
			closeFile = f
		case "subtasks":
			subs = f
		}
	}
	if subs.Header.Status != sessionjudge.StatusFailed {
		t.Fatalf("sub-tasks %s", subs.Header.Status)
	}
	child := map[string]bool{}
	for _, c := range closeFile.Costs {
		if c.Scope == sessionjudge.ScopeSubtask && c.Calls > 0 {
			child[c.ID] = true
		}
	}
	notReviewed := 0
	for _, c := range closeFile.Costs {
		if c.Scope == sessionjudge.ScopeWork && c.Work == sessionjudge.WorkNotReviewed {
			notReviewed += c.Calls
		}
	}
	if len(child) == 0 || notReviewed == 0 {
		t.Fatalf("sub-tasks %v; calls not reviewed %d", child, notReviewed)
	}
}

// answersNothing gives valid answers that judge no sub-task and label no talk.
type answersNothing struct{ standIn }

func (s answersNothing) Run(ctx context.Context, t judge.Task) (judge.Result, error) {
	switch t.Stage {
	case sessionjudge.StageSubtasks:
		return judge.Result{Output: []byte(`{"subtasks":[]}`), Usage: &sessiondata.Usage{Input: 1}}, nil
	case sessionjudge.StageEfficiency:
		return judge.Result{Output: []byte(`{"labels":[]}`), Usage: &sessiondata.Usage{Input: 1}}, nil
	}
	return s.standIn.Run(ctx, t)
}

// A valid answer that leaves out work it was given covers less than all, so
// its stage, and the run, are partial and never complete.
func TestAnAnswerThatLeavesWorkOutIsPartial(t *testing.T) {
	z, id := builtFixture(t)
	r := runner(z)
	r.Executor = func(string, config.Evaluator) (judge.Executor, error) { return answersNothing{}, nil }
	run, err := r.Run(context.Background(), id, 0, plan)
	if err != nil {
		t.Fatal(err)
	}
	stages := map[string]*sessionjudge.File{}
	for _, p := range run.Files {
		f := readStage(t, p)
		stages[f.Header.Stage] = f
	}
	subs, eff := stages["subtasks"].Header, stages["efficiency"].Header
	if subs.Status != sessionjudge.StatusPartial || !strings.Contains(subs.Note, "sub-tasks were not judged") ||
		*subs.Coverage.SubtasksJudged != 0 || *subs.Coverage.Subtasks == 0 {
		t.Fatalf("sub-tasks %s %q %+v", subs.Status, subs.Note, subs.Coverage)
	}
	if eff.Status != sessionjudge.StatusPartial || !strings.Contains(eff.Note, "talks were not labeled") ||
		*eff.Coverage.TalksLabeled != 0 || *eff.Coverage.Talks == 0 {
		t.Fatalf("efficiency %s %q %+v", eff.Status, eff.Note, eff.Coverage)
	}
	if run.Status != sessionjudge.StatusPartial {
		t.Fatalf("run %s", run.Status)
	}
}

// labelsTheEnd labels each talk only from its last model call.
type labelsTheEnd struct{ standIn }

func (s labelsTheEnd) Run(ctx context.Context, t judge.Task) (judge.Result, error) {
	if t.Stage != sessionjudge.StageEfficiency {
		return s.standIn.Run(ctx, t)
	}
	labels := []any{}
	for _, tk := range t.Nav.Assigned().Talks {
		outline, err := t.Nav.Call("talk", json.RawMessage(`{"id":"`+tk+`"}`))
		if err != nil {
			return judge.Result{}, err
		}
		b, _ := json.Marshal(outline)
		var rows struct {
			Rows []struct {
				ID   string `json:"id"`
				Kind string `json:"kind"`
			} `json:"rows"`
		}
		_ = json.Unmarshal(b, &rows)
		last := ""
		for _, r := range rows.Rows {
			if r.Kind == model.KindLLMCall {
				last = r.ID
			}
		}
		labels = append(labels, map[string]any{"target": "t1", "talk": tk, "from": last, "label": "useful", "note": "", "cite": []string{last}})
	}
	b, _ := json.Marshal(map[string]any{"labels": labels})
	return judge.Result{Output: b, Usage: &sessiondata.Usage{Input: 1}}, nil
}

// A talk labeled only from a later part leaves the work before it not
// reviewed, so the efficiency stage is partial. Of the three talks, only the
// second has a model call before its last one.
func TestATalkLabeledOnlyLaterIsPartial(t *testing.T) {
	z, id := builtThree(t)
	r := runner(z)
	r.Executor = func(string, config.Evaluator) (judge.Executor, error) { return labelsTheEnd{}, nil }
	run, err := r.Run(context.Background(), id, 0, plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range run.Files {
		if f := readStage(t, p); f.Header.Stage == "efficiency" {
			h := f.Header
			// With a sub-task stage, the tester's call is that stage's to
			// price: only the talk's own first call is left.
			if h.Status != sessionjudge.StatusPartial ||
				!strings.Contains(h.Note, "talk talk/main/s3-cycle of target t1 is labeled only from a later part, and its calls before that part are not reviewed (1)") ||
				*h.Coverage.Talks != 3 || *h.Coverage.TalksLabeled != 2 {
				t.Fatalf("efficiency %s %q talks %d labeled %d", h.Status, h.Note, *h.Coverage.Talks, *h.Coverage.TalksLabeled)
			}
			return
		}
	}
	t.Fatal("no efficiency stage")
}

// perInput sets one target for each input, answers each later stage for the
// target its prompt names, and keeps every prompt it was given. Efficiency
// fails every call that labels a main talk whose place, counted from 1, is
// in fail.
type perInput struct {
	standIn
	mu      *sync.Mutex
	prompts map[string]string
	fail    map[int]bool
}

var targetRow = regexp.MustCompile(`"id": "(t\d+)"`)

func (p perInput) Run(ctx context.Context, t judge.Task) (judge.Result, error) {
	p.mu.Lock()
	p.prompts[t.Dir] = t.Prompt
	p.mu.Unlock()
	if t.Stage == sessionjudge.StageTargets {
		var acts, targets []any
		for i, in := range t.Nav.Assigned().Inputs {
			key := fmt.Sprintf("n%d", i+1)
			acts = append(acts, map[string]any{"node": in, "actions": []any{map[string]string{"target": key, "action": "create"}}})
			targets = append(targets, map[string]any{"id": "", "key": key, "statement": "do " + in, "criteria": []string{"it is done"},
				"drop_criteria": []string{}, "status": "closed", "replaced_by": "", "talks": []string{t.Nav.TalkOf(in)}, "cite": []string{in}})
		}
		b, _ := json.Marshal(map[string]any{"targets": targets, "inputs": acts})
		return judge.Result{Output: b, Usage: &sessiondata.Usage{Input: 1, Output: 2}}, nil
	}
	if t.Stage == sessionjudge.StageEfficiency {
		for _, tk := range t.Nav.Assigned().Talks {
			if p.fail[slices.Index(t.Nav.Main(), tk)+1] {
				return judge.Result{}, fmt.Errorf("no answer")
			}
		}
	}
	res, err := p.standIn.Run(ctx, t)
	if m := targetRow.FindStringSubmatch(t.Prompt); m != nil && err == nil {
		res.Output = []byte(strings.ReplaceAll(string(res.Output), `"t1`, `"`+m[1]))
	}
	return res, err
}

// Each call is given only the targets it answers for, however many the run
// found, and a call that fails stays in its stage's coverage as not judged.
func TestACallIsGivenOnlyItsOwnTargets(t *testing.T) {
	z, id := builtThree(t)
	r := runner(z)
	r.Config.Mode, r.Config.SingleBytes, r.Config.ChunkBytes = "stream", 1, 1
	ev := perInput{mu: &sync.Mutex{}, prompts: map[string]string{}, fail: map[int]bool{}}
	r.Executor = func(string, config.Evaluator) (judge.Executor, error) { return ev, nil }
	run, err := r.Run(context.Background(), id, 0, plan)
	if err != nil {
		t.Fatal(err)
	}
	given := map[string][]string{}
	for dir, prompt := range ev.prompts {
		if !strings.Contains(dir, "-efficiency") && !strings.Contains(dir, "-achievement") {
			continue
		}
		var ids []string
		for _, m := range targetRow.FindAllStringSubmatch(prompt, -1) {
			ids = append(ids, m[1])
		}
		if len(ids) != 1 {
			t.Fatalf("%s was given targets %v", dir, ids)
		}
		stage := filepath.Base(filepath.Dir(filepath.Dir(dir)))
		given[stage] = append(given[stage], ids[0])
	}
	for stage, ids := range given {
		sort.Strings(ids)
		if strings.Join(ids, ",") != "t1,t2,t3" {
			t.Fatalf("%s calls were given %v", stage, ids)
		}
	}
	if len(given) != 2 {
		t.Fatalf("calls given targets %v", given)
	}
	// The targets tool of a labeling call serves every target the run
	// found, as its task names them.
	task, err := os.ReadFile(filepath.Join(run.Dir, "work", "5-efficiency", "c001", "a1", "task.json"))
	if err != nil {
		t.Fatal(err)
	}
	var assigned judge.Assignment
	if err := json.Unmarshal(task, &assigned); err != nil || len(assigned.Catalogue) != 3 {
		t.Fatalf("the task's catalogue %+v %v", assigned.Catalogue, err)
	}
	if run.Status != sessionjudge.StatusComplete {
		t.Fatalf("run %s: %s", run.Status, notes(t, run))
	}
}

// A labeling call that fails counts its talks as not labeled, and a stage
// whose every call failed still says what it was to cover.
func TestAFailedCallStaysInTheCoverage(t *testing.T) {
	for _, tc := range []struct {
		fail          map[int]bool
		status        string
		talks, labels int
	}{
		{fail: map[int]bool{2: true}, status: sessionjudge.StatusPartial, talks: 3, labels: 2},
		{fail: map[int]bool{1: true, 2: true, 3: true}, status: sessionjudge.StatusFailed, talks: 3, labels: 0},
	} {
		z, id := builtThree(t)
		r := runner(z)
		r.Config.Mode, r.Config.SingleBytes, r.Config.ChunkBytes = "stream", 1, 1
		ev := perInput{mu: &sync.Mutex{}, prompts: map[string]string{}, fail: tc.fail}
		r.Executor = func(string, config.Evaluator) (judge.Executor, error) { return ev, nil }
		run, err := r.Run(context.Background(), id, 0, plan)
		if err != nil {
			t.Fatal(err)
		}
		var eff, tg *sessionjudge.File
		for _, p := range run.Files {
			f := readStage(t, p)
			switch f.Header.Stage {
			case "efficiency":
				eff = f
			case "targets":
				tg = f
			}
		}
		c := eff.Header.Coverage
		if eff.Header.Status != tc.status || c == nil || c.Talks == nil || *c.Talks != tc.talks || *c.TalksLabeled != tc.labels {
			t.Fatalf("efficiency %s, coverage %+v", eff.Header.Status, c)
		}
		if c := tg.Header.Coverage; c == nil || *c.Inputs != 3 || *c.InputsRead != 3 {
			t.Fatalf("targets coverage %+v", c)
		}
	}
}

// lastNotPerson sets one target from the first input, marks the last input
// as from no person, and says the person accepted the target by that last
// input.
type lastNotPerson struct{ standIn }

func (s lastNotPerson) Run(ctx context.Context, t judge.Task) (judge.Result, error) {
	in := t.Nav.Inputs()
	last := in[len(in)-1]
	var out any
	switch t.Stage {
	case sessionjudge.StageTargets:
		var acts []any
		for i, n := range in {
			a := map[string]string{"target": "n1", "action": "steer"}
			switch {
			case i == 0:
				a["action"] = "create"
			case n == last:
				a = map[string]string{"target": "", "action": "not_from_person"}
			}
			acts = append(acts, map[string]any{"node": n, "actions": []any{a}})
		}
		out = map[string]any{"inputs": acts, "targets": []any{map[string]any{"id": "", "key": "n1", "statement": "build it",
			"criteria": []string{"it builds"}, "drop_criteria": []string{}, "status": "closed", "replaced_by": "",
			"talks": []string{t.Nav.TalkOf(in[0])}, "cite": []string{in[0]}}}}
	case sessionjudge.StageAchievement:
		out = map[string]any{"achievements": []any{map[string]any{"target": "t1", "basis": "person_accepted",
			"criteria": []any{map[string]any{"id": "t1.c1", "state": "unknown", "delivered": false, "cite": []string{}}},
			"claims":   []any{}, "note": "", "cite": []string{in[0], last}}}}
	default:
		return s.standIn.Run(ctx, t)
	}
	b, _ := json.Marshal(out)
	return judge.Result{Output: b, Usage: &sessiondata.Usage{Input: 1, Output: 2}}, nil
}

// An input the targets stage found came from no person is never taken for
// the person accepting the work.
func TestAnInputFromNoPersonAcceptsNothing(t *testing.T) {
	z, id := builtThree(t)
	r := runner(z)
	r.Executor = func(string, config.Evaluator) (judge.Executor, error) { return lastNotPerson{}, nil }
	run, err := r.Run(context.Background(), id, 0, plan[:2])
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range run.Files {
		f := readStage(t, p)
		for _, a := range f.Achievements {
			if a.Basis == sessionjudge.BasisAccepted {
				t.Fatalf("accepted by an input from no person: %+v", a)
			}
		}
		if f.Header.Stage == "achievement" && len(f.Achievements) != 1 {
			t.Fatalf("achievement %s %s", f.Header.Status, f.Header.Note)
		}
	}
}

// A targets scan gives each call the targets found so far within
// judge.state_bytes, and its task names them all for the targets tool; a
// later stage's call is given its targets within the same bound.
func TestTheTargetsFoundSoFarStayWithinTheirBudget(t *testing.T) {
	z, id := builtThree(t)
	r := runner(z)
	r.Config.Mode, r.Config.SingleBytes, r.Config.ChunkBytes, r.Config.StateBytes = "stream", 1, 1, 1
	ev := perInput{mu: &sync.Mutex{}, prompts: map[string]string{}, fail: map[int]bool{}}
	r.Executor = func(string, config.Evaluator) (judge.Executor, error) { return ev, nil }
	run, err := r.Run(context.Background(), id, 0, plan[:2])
	if err != nil {
		t.Fatal(err)
	}
	// A later stage's call is given its targets within the same bound.
	if p := ev.prompts[filepath.Join(run.Dir, "work", "3-achievement", "c001", "a1")]; !strings.Contains(p, "one short line each") {
		t.Fatalf("an achievement call's prompt:\n%s", p)
	}
	dir := filepath.Join(run.Dir, "work", "2-targets", "c003", "a1")
	// And its rows keep to judge.chunk_bytes, here one byte.
	if p := ev.prompts[dir]; !strings.Contains(p, "2 more targets; the targets tool lists them") || strings.Contains(p, `"statement"`) ||
		!strings.Contains(p, "1 more rows, which the inputs tool lists") {
		t.Fatalf("the third call's prompt:\n%s", p)
	}
	task, _ := os.ReadFile(filepath.Join(dir, "task.json"))
	var assigned judge.Assignment
	if err := json.Unmarshal(task, &assigned); err != nil || len(assigned.Catalogue) != 2 {
		t.Fatalf("the task's catalogue %+v %v", assigned.Catalogue, err)
	}
}

// misleadingCheck is a conversation whose sub-agent's tests fail while it
// reports them passing, and whose main agent pushes on that report.
const misleadingCheck = `steps:
  - input: check the tests, then push
  - call:
      agent:
        name: checker
        prompt: run the tests
        steps:
          - call: {tool: {name: Bash, input: {command: make test}, result: "FAIL: 2 tests"}}
          - call: {text: All tests pass.}
        notify: true
  - call: {tool: {name: Bash, input: {command: git push}, result: pushed}}
  - call: {text: Pushed.}
`

// misleading answers the sub-task stage as misled, its task right or, when
// wrongTask is set, wrong, and labels the main talk's work as done on the
// report of the sub-task the prompt names as misleading.
type misleading struct {
	standIn
	wrongTask bool
}

var misledRow = regexp.MustCompile(`"subtask":"([^"]+)"`)

func (m misleading) Run(ctx context.Context, t judge.Task) (judge.Result, error) {
	var out any
	switch t.Stage {
	case sessionjudge.StageSubtasks:
		ans, _ := t.Nav.Call("subtasks", nil)
		b, _ := json.Marshal(ans)
		var pg struct {
			Rows []struct {
				ID        string   `json:"id"`
				StartedBy string   `json:"started_by"`
				Talks     []string `json:"talks"`
			} `json:"rows"`
		}
		_ = json.Unmarshal(b, &pg)
		var decisions []any
		for _, r := range pg.Rows {
			// Its own failing test, and the push the main agent made after.
			cite := append(toolsOf(t, r.Talks[0]), r.StartedBy)
			for _, tk := range t.Nav.Main() {
				cite = append(cite, toolsOf(t, tk)...)
			}
			dispatch := "reasonable"
			if m.wrongTask {
				dispatch = "misdirected"
			}
			decisions = append(decisions, map[string]any{"ids": []string{r.ID}, "outcome": "not_achieved", "report": "inaccurate",
				"use": "misled", "dispatch": dispatch, "work": "useful", "note": "", "cite": cite})
		}
		out = map[string]any{"subtasks": decisions}
	case sessionjudge.StageEfficiency:
		// The prompt names the misleading sub-task, and the subtasks tool
		// gives its decision: an evaluator missing either names none.
		sub := ""
		if m := misledRow.FindStringSubmatch(t.Prompt); m != nil {
			sub = m[1]
		}
		ans, _ := t.Nav.Call("subtasks", nil)
		b, _ := json.Marshal(ans)
		if !strings.Contains(string(b), `"use":"misled"`) {
			sub = ""
		}
		// The work from the push on rests on the report: the talk's part
		// before it, the dispatch, does not.
		var labels []any
		for _, tk := range t.Nav.Main() {
			tools := toolsOf(t, tk)
			labels = append(labels, map[string]any{"target": "t1", "talk": tk, "from": tools[0], "label": "misled_by_subtask",
				"subtask": sub, "note": "", "cite": tools})
		}
		out = map[string]any{"labels": labels}
	default:
		return m.standIn.Run(ctx, t)
	}
	b, _ := json.Marshal(out)
	return judge.Result{Output: b, Usage: &sessiondata.Usage{Input: 1, Output: 2}}, nil
}

// toolsOf lists the tool steps of a talk.
func toolsOf(t judge.Task, talk string) []string {
	ans, err := t.Nav.Call("talk", json.RawMessage(`{"id":"`+talk+`","only":"tools"}`))
	if err != nil {
		return nil
	}
	b, _ := json.Marshal(ans)
	var pg struct {
		Rows []struct {
			ID string `json:"id"`
		} `json:"rows"`
	}
	_ = json.Unmarshal(b, &pg)
	var out []string
	for _, r := range pg.Rows {
		out = append(out, r.ID)
	}
	return out
}

// Work done on a sub-agent's untrue report is avoidable, and the sub-agent's
// doing when its task was right; when its task was wrong, it and the work
// it led to are the main agent's.
func TestWorkOnAnUntrueReportIsCharged(t *testing.T) {
	for _, wrongTask := range []bool{false, true} {
		z, id := built(t, writeScenario(t, misleadingCheck))
		r := runner(z)
		r.Executor = func(string, config.Evaluator) (judge.Executor, error) { return misleading{wrongTask: wrongTask}, nil }
		run, err := r.Run(context.Background(), id, 0, plan)
		if err != nil {
			t.Fatal(err)
		}
		var closeFile *sessionjudge.File
		for _, p := range run.Files {
			if f := readStage(t, p); f.Header.Stage == "close" {
				closeFile = f
			}
		}
		causes := map[string]string{}
		for _, c := range closeFile.Costs {
			if c.Scope == sessionjudge.ScopeWork {
				causes[c.Work] = c.Cause
			}
		}
		// The call's task carries the decisions, for an agent CLI's
		// navigation in a process of its own.
		task, err := os.ReadFile(filepath.Join(run.Dir, "work", "5-efficiency", "c001", "a1", "task.json"))
		if err != nil || !strings.Contains(string(task), `"use":"misled"`) {
			t.Fatalf("the efficiency task %s %v", task, err)
		}
		o := closeFile.Outcomes[0]
		if wrongTask {
			if causes[sessionjudge.WorkMisled] != sessionjudge.CauseMain || causes[sessionjudge.WorkMisdirected] != sessionjudge.CauseMain ||
				o.AvoidableBySubtasks == nil || o.AvoidableBySubtasks.Tokens() != 0 {
				t.Fatalf("a wrong task: causes %v, %s", causes, notes(t, run))
			}
			continue
		}
		if c := causes[sessionjudge.WorkMisled]; c == "" || c == sessionjudge.CauseMain || o.AvoidableBySubtasks == nil || o.AvoidableBySubtasks.Tokens() == 0 {
			t.Fatalf("a right task done wrong: causes %v, %s", causes, notes(t, run))
		}
	}
}

// lossy answers as the stand-in does, and loses one request's usage on
// every call, as a response lost on the way would.
type lossy struct{ standIn }

func (l lossy) Run(ctx context.Context, t judge.Task) (judge.Result, error) {
	res, err := l.standIn.Run(ctx, t)
	res.UnknownRequests = 1
	return res, err
}

// A request whose usage was never said is counted apart in its stage and
// in the run's judging, and the usage the others said is kept.
func TestARequestWithNoUsageIsCountedApart(t *testing.T) {
	z, id := builtFixture(t)
	r := runner(z)
	r.Executor = func(string, config.Evaluator) (judge.Executor, error) { return lossy{}, nil }
	run, err := r.Run(context.Background(), id, 0, plan)
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	var summary *sessionjudge.Summary
	for _, p := range run.Files {
		h := readStage(t, p).Header
		if h.Stage == "close" {
			summary = h.Summary
		}
		if h.Attempts == 0 {
			continue
		}
		attempts += h.Attempts
		if h.Usage == nil || h.Requests == nil || h.Requests.Unknown != h.Attempts {
			t.Fatalf("%s: usage %+v, requests %+v", h.Stage, h.Usage, h.Requests)
		}
	}
	if summary.Judging == nil || summary.Judging.Unknown != attempts || summary.Judging.Usage.Input == 0 {
		t.Fatalf("judging %+v over %d attempts", summary.Judging, attempts)
	}
}

// stoppedFirst stops every call's first attempt before its stream showed a
// request, as an interrupted CLI does, and answers as the stand-in on the
// retry.
type stoppedFirst struct{ standIn }

func (s stoppedFirst) Run(ctx context.Context, t judge.Task) (judge.Result, error) {
	if filepath.Base(t.Dir) == "a1" {
		return judge.Result{Usage: &sessiondata.Usage{}, UnknownRequests: 1}, fmt.Errorf("stopped")
	}
	return s.standIn.Run(ctx, t)
}

// A call stopped before its stream showed a request keeps that request
// unknown, and the retry that answered keeps its usage known beside it.
func TestAStoppedCallAndItsRetryAreBothCounted(t *testing.T) {
	z, id := builtFixture(t)
	r := runner(z)
	r.Executor = func(string, config.Evaluator) (judge.Executor, error) { return stoppedFirst{}, nil }
	run, err := r.Run(context.Background(), id, 0, plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range run.Files {
		h := readStage(t, p).Header
		if h.Chunks == 0 {
			continue
		}
		// The stand-in says 1 input and 2 output tokens per answer.
		if h.Usage == nil || h.Usage.Input != h.Chunks || h.Requests == nil || h.Requests.Unknown != h.Chunks {
			t.Fatalf("%s: usage %+v, requests %+v", h.Stage, h.Usage, h.Requests)
		}
	}
}
