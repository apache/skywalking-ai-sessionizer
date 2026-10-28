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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// Runner runs judge runs over one storage root.
type Runner struct {
	Zone    *storage.Zone
	Config  config.Judge
	Pricing *Pricing
	// Dialect says which dialect a session's records were read in.
	Dialect func(session string) string
	// Decoder gives the decoder of a dialect's tool requests, nil for a
	// dialect with none.
	Decoder func(dialect string) PathDecoder
	// Sources are the sources the run is compared with: files, URLs, or
	// sources a configured command fetches. They are saved before any
	// evaluator runs.
	Sources []string
	// MCP returns the command line that starts the navigation server for a
	// stage.
	MCP func(MCPArgs) []string
	// Version is asz's version, for the computed stages' header.
	Version string
	// Log reports progress, when set.
	Log func(format string, args ...any)
	// Window is the part of the conversation the run judges; zero is all
	// of it.
	Window Window
	// Executor overrides the executor a configured evaluator makes, for
	// tests.
	Executor func(name string, e config.Evaluator) (Executor, error)
	Now      func() time.Time
	// Reserved, when set, is told the run's number as soon as it is
	// reserved, before any stage is written or any evaluator runs. On a
	// pulled snapshot it binds the reader's lease to the run, which alone
	// lets an evaluator work there; an error stops the run.
	Reserved func(number int) error
	// Admitted is the pulled snapshot the run reads, under a lease, or
	// empty. A file the run is given may lie inside it, and nowhere else in
	// a tree pull owns.
	Admitted string
	// Snapshot binds every read of the conversation to a pulled snapshot
	// the run holds a lease on: folds reach its marker's head, and a read
	// that fails stops the run rather than leaving something out.
	Snapshot *view.Snapshot
	// Failures, on a pulled snapshot, is the random part of the run lease
	// the run holds, which names each chunk attempt's failure marker and
	// its chunk's gate. It is read once the lease is bound.
	Failures func() string
}

// server is the view the run reads the conversation through.
func (r *Runner) server() *view.Server {
	srv := view.New(r.Zone, nil)
	if r.Snapshot != nil {
		srv.SetSnapshot(r.Snapshot)
	}
	return srv
}

// navigation narrows a document to the window, computes its ledger and
// prepares the navigation over it, as both a run and a regroup read a
// conversation. On a pulled snapshot every record read is checked, and a
// failure stops it; on any other root what cannot be read is passed over, as
// it always was.
func (r *Runner) navigation(c *view.Conversation, doc *sessionview.Conversation, window Window, pricing *Pricing) (*sessionview.Conversation, *Selection, *Ledger, *Nav, error) {
	dialect := r.Dialect(c.Session)
	var decode PathDecoder
	if r.Decoder != nil {
		decode = r.Decoder(dialect)
	}
	read := func(seq, row uint64) (*sessiondata.Record, error) { return c.Record(seq, row, r.Config.Hide) }
	if r.Snapshot == nil {
		doc, sel, err := Narrow(doc, window, c.Records)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		ledger := Compute(doc, dialect, pricing, c.Records)
		ledger.Select(sel)
		nav := NewNav(doc, ledger, read)
		nav.Prepare(sel, c.Records, decode)
		_ = nav.MarkResultImages(unchecked(c.Records))
		return doc, sel, ledger, nav, nil
	}
	doc, sel, err := NarrowChecked(doc, window, c.RecordsChecked)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	ledger, err := ComputeChecked(doc, dialect, pricing, c.RecordsChecked)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	ledger.Select(sel)
	nav := NewNav(doc, ledger, read)
	if err := nav.PrepareChecked(sel, c.RecordsChecked, decode); err != nil {
		return nil, nil, nil, nil, err
	}
	if err := nav.MarkResultImages(c.RecordsChecked); err != nil {
		return nil, nil, nil, nil, err
	}
	nav.Strictly(NewStrict(doc, c.Holds), "")
	return doc, sel, ledger, nav, nil
}

// MCPArgs is what the navigation server of one stage is started over.
type MCPArgs struct {
	Conversation string
	Head         sessionjudge.Head
	// Ledger is the run's ledger stage file, whose rates the server prices
	// calls by, as the ledger did.
	Ledger string
	// Log is where the server logs every call it answers.
	Log string
	// Window is the run's window, which the server narrows the
	// conversation to as the run did.
	Window Window
	// ReadBytes is the stage's reading budget.
	ReadBytes int
	// Task is the file that says what the call answers for.
	Task string
	// Failure, on a pulled snapshot, is the chunk attempt's failure marker,
	// which the server makes before each read of evidence.
	Failure string
}

// Run is one finished run.
type Run struct {
	Number int
	Dir    string
	Head   sessionjudge.Head
	Status string
	Files  []string
}

func (r *Runner) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(format, args...)
	}
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

// stageOrder is where each stage's file sorts in its run.
var stageOrder = map[string]int{
	sessionjudge.StageLedger: 1, sessionjudge.StageTargets: 2, sessionjudge.StageAchievement: 3,
	sessionjudge.StageSubtasks: 4, sessionjudge.StageEfficiency: 5, sessionjudge.StageReference: 6, sessionjudge.StageDivergence: 7,
	sessionjudge.StageClose: 9,
}

// Run judges a conversation at a round of its chain, or at its current head
// for round 0: the ledger, then each stage of the plan with its evaluator,
// then the close file. A stage that fails is written as failed, and the run
// still closes, so what was done is kept and says what was not.
func (r *Runner) Run(ctx context.Context, conversation string, round uint64, plan []config.JudgeStage) (*Run, error) {
	// Everything a run can be refused for is checked before it is reserved,
	// so a run that starts can always close.
	if len(plan) == 0 || plan[0].Stage != sessionjudge.StageTargets {
		return nil, fmt.Errorf("judge: a plan starts with the targets stage, which every later stage reads")
	}
	executors := map[string]Executor{}
	newExec := NewExecutor
	if r.Executor != nil {
		newExec = r.Executor
	}
	last := 0
	for _, p := range plan {
		order, ok := stageOrder[p.Stage]
		if !ok || p.Stage == sessionjudge.StageLedger || p.Stage == sessionjudge.StageClose {
			return nil, fmt.Errorf("judge: %q is not a stage an evaluator takes", p.Stage)
		}
		// One answer per question: a run that asked twice would count a
		// target twice in its close.
		if order <= last {
			return nil, fmt.Errorf("judge: stage %s comes twice or out of order; a plan is targets, achievement, subtasks, efficiency, each at most once", p.Stage)
		}
		last = order
		conf, ok := r.Config.Evaluators[p.Evaluator]
		if !ok {
			return nil, fmt.Errorf("judge: stage %s names evaluator %q, which judge.evaluators does not list", p.Stage, p.Evaluator)
		}
		ex, err := newExec(p.Evaluator, conf)
		if err != nil {
			return nil, err
		}
		executors[p.Stage] = ex
	}
	// The sources and the stages that read them go together: a source no
	// stage reads would be saved for nothing, and a stage with no source
	// would compare with nothing.
	_, reads := executors[sessionjudge.StageReference]
	_, compares := executors[sessionjudge.StageDivergence]
	switch {
	case len(r.Sources) > 0 && (!reads || !compares):
		return nil, fmt.Errorf("judge: a run given sources needs the reference and divergence stages in its plan")
	case len(r.Sources) == 0 && (reads || compares):
		return nil, fmt.Errorf("judge: the reference and divergence stages need a source to compare with")
	}
	if err := CheckReferences(r.Sources); err != nil {
		return nil, err
	}
	timeout, err := r.Config.TimeoutDuration()
	if err != nil {
		return nil, err
	}
	kit, err := LoadKit()
	if err != nil {
		return nil, err
	}
	srv := r.server()
	var c *view.Conversation
	if round == 0 {
		c, err = srv.Load(conversation)
	} else {
		c, err = srv.LoadAt(conversation, round)
	}
	if err != nil {
		return nil, err
	}
	doc, err := c.BuildWithheld(r.Config.Hide)
	if err != nil {
		return nil, err
	}
	head := sessionjudge.Head{Round: doc.Head.Round, Digest: doc.Head.Digest}
	doc, sel, ledger, nav, err := r.navigation(c, doc, r.Window, r.Pricing)
	if err != nil {
		return nil, err
	}
	// The sources are fetched before any evaluator runs, and kept in the
	// run's work directory whether or not the stages that read them run.
	saved := fetchSources(ctx, r.Sources, r.Config.References, r.now, r.Admitted, r.Version)
	for _, s := range saved {
		nav.sources = append(nav.sources, s.manifest)
		nav.sourceDocs = append(nav.sourceDocs, SourceDoc{ID: s.manifest.ID, Source: s.manifest.Source, Text: s.text, Left: LeftOut(s.manifest)})
		if s.manifest.Outcome == sessionjudge.SourceFailed {
			r.logf("source %s could not be saved: %s", s.manifest.Source, s.manifest.Error)
		}
	}

	number, dir, err := storage.ReserveRun(r.Zone, c.Session)
	if err != nil {
		return nil, err
	}
	if r.Reserved != nil {
		if err := r.Reserved(number); err != nil {
			return nil, err
		}
	}
	run := &Run{Number: number, Dir: dir, Head: head, Status: sessionjudge.StatusComplete}
	r.logf("run %d of %s at round %d", number, conversation, head.Round)
	if len(saved) > 0 {
		files, err := sourceFiles(saved)
		if err == nil {
			err = storage.WriteWorkFiles(r.Zone, c.Session, dir, "sources", files)
		}
		if err != nil {
			return nil, err
		}
	}
	base := sessionjudge.Header{Conversation: conversation, Session: c.Session, Run: number, Head: head,
		Parser: doc.Parser, Policy: doc.Policy, Window: sel.Header(r.Window)}
	computed := sessionjudge.Evaluator{Name: "asz", Kind: sessionjudge.EvaluatorComputed, Program: "asz", Version: r.Version}

	// publish writes one stage file and returns the stage frame the close
	// file lists it by.
	publish := func(h sessionjudge.Header, frames []any) (*sessionjudge.Stage, error) {
		// Nothing is published once a read of a pulled snapshot's
		// evidence failed, whoever made it.
		if err := nav.Failed(); err != nil {
			return nil, err
		}
		w, err := sessionjudge.NewWriter(h)
		if err != nil {
			return nil, err
		}
		for _, f := range frames {
			if err := w.Write(f); err != nil {
				return nil, err
			}
		}
		data, digest, err := w.Close()
		if err != nil {
			return nil, err
		}
		path, err := storage.PublishStage(r.Zone, c.Session, dir, stageOrder[h.Stage], h.Stage, digest, data)
		if err != nil {
			return nil, err
		}
		run.Files = append(run.Files, path)
		return &sessionjudge.Stage{Stage: h.Stage, File: filepath.Base(path), Digest: digest, Status: h.Status, Evaluator: h.Evaluator.Name}, nil
	}

	started := r.now()
	lh := base
	lh.Stage, lh.Evaluator, lh.Status = sessionjudge.StageLedger, computed, sessionjudge.StatusComplete
	lh.Started, lh.Finished = stamp(started), stamp(r.now())
	calls, unknown := ledger.Total.Calls, ledger.Total.Unknown
	lh.Coverage = &sessionjudge.Coverage{Calls: &calls, CallsWithoutUsage: &unknown}
	ledgerStage, err := publish(lh, ledgerFrames(ledger))
	if err != nil {
		return nil, err
	}
	ledgerDigest := ledgerStage.Digest
	ledgerPath := filepath.Join(dir, ledgerStage.File)
	stages := []any{ledgerStage}

	var targets []sessionjudge.Target
	var goals []sessionjudge.Goal
	var inputs []*sessionjudge.Input
	var targetsDigest string
	// judging adds up what the run's evaluators consumed, priced by the
	// price file when it lists their models. It is unknown as soon as one
	// stage that ran did not report it.
	judging := &sessionjudge.Amount{}
	var judgingMoney, reported float64
	judgingPriced, reportedAll := false, true
	// judgingFrom and judgingTo bound the time the evaluator stages took.
	var judgingFrom, judgingTo time.Time
	var achievements []*sessionjudge.Achievement
	// findings are the efficiency and divergence stages', each naming its
	// target; the achievement stage's are inside its achievements.
	var findings []*sessionjudge.Finding
	// divergences and rollbacks are how the conversation compares with the
	// conclusions of its sources.
	var divergences []*sessionjudge.Divergence
	var rollbacks []*sessionjudge.Rollback
	// labels stays nil until an efficiency stage is accepted, so the close
	// file prices work as not reviewed only when that stage ran; subtasks
	// likewise for the sub-task stage.
	var labels []*sessionjudge.Label
	var subtasks map[string]*sessionjudge.Subtask
	ran := map[string]bool{}
	for _, p := range plan {
		ran[p.Stage] = true
	}
	// A planned sub-task stage that fails, or answers nothing, leaves its
	// sub-tasks not reviewed: efficiency was told to label only each talk's
	// own stream, so its labels must not reach the sub-tasks. Only a plan
	// without the stage lets them.
	if ran[sessionjudge.StageSubtasks] {
		subtasks = map[string]*sessionjudge.Subtask{}
	}
	for _, p := range plan {
		if p.Stage != sessionjudge.StageTargets && targetsDigest == "" {
			r.logf("  %s skipped: the run has no targets", p.Stage)
			run.Status = sessionjudge.StatusPartial
			continue
		}
		ex := executors[p.Stage]
		_, promptDigest, err := kit.Prompt(p.Stage, "")
		if err != nil {
			return nil, err
		}
		schema, err := kit.Schema(p.Stage)
		if err != nil {
			return nil, err
		}
		h := base
		h.Stage, h.Evaluator = p.Stage, ex.Describe()
		h.Kit = &sessionjudge.Kit{Version: kit.Version(), Prompt: promptDigest}
		h.BuildsOn = []sessionjudge.Dependency{{Stage: sessionjudge.StageLedger, Digest: ledgerDigest}}
		if p.Stage != sessionjudge.StageTargets {
			h.BuildsOn = append(h.BuildsOn, sessionjudge.Dependency{Stage: sessionjudge.StageTargets, Digest: targetsDigest})
		}
		mode, chunks := planChunks(p.Stage, r.Config.Mode, r.Config.SingleBytes, r.Config.ChunkBytes, r.Config.ReadBytes, nav, targets)
		h.Mode, h.Chunks = mode, len(chunks)
		r.logf("  %s by %s, %s, %d calls ...", p.Stage, p.Evaluator, mode, len(chunks))
		stageStart := r.now()
		if p.Stage == sessionjudge.StageAchievement {
			nav.Own(targets, inputs)
		}
		sr := &stageRun{runner: r, kit: kit, ex: ex, nav: nav, stage: p.Stage, order: stageOrder[p.Stage], session: c.Session,
			dir: dir, schema: schema, targets: targets, goals: goals, inputs: inputs, achievements: achievements, misled: misledOf(subtasks, ledger),
			subtaskStage: ran[sessionjudge.StageSubtasks],
			timeout:      timeout, mcp: MCPArgs{Conversation: conversation, Head: head, Ledger: ledgerPath, Window: r.Window, ReadBytes: r.Config.ReadBytes}}
		m, cat, used, err := sr.run(ctx, chunks)
		if err != nil {
			return nil, err
		}
		h.Started, h.Finished = stamp(stageStart), stamp(r.now())
		if used.attempts > 0 {
			if judgingFrom.IsZero() {
				judgingFrom = stageStart
			}
			judgingTo = r.now()
		}
		h.Usage, h.ReportedCostUSD, h.Reading, h.Attempts = used.usage, used.costUSD, used.reading, used.attempts
		if used.attempts > 0 {
			rq := used.requests
			h.Requests = &rq
		}
		if used.model != "" {
			h.Evaluator.Model = used.model
		}
		// A stage with nothing to answer made no call and consumed nothing;
		// it leaves what the run's evaluators consumed as it was.
		switch {
		case used.attempts == 0:
		case used.costUSD == nil:
			reportedAll = false
		default:
			reported += *used.costUSD
		}
		if judging != nil && used.attempts > 0 {
			if used.usage == nil {
				judging = nil
			} else {
				add(&judging.Usage, *used.usage)
				// A request whose usage was never said is counted apart,
				// as a conversation call with no usage is.
				judging.Unknown += used.requests.Unknown
				// Each model at its own rate; a stage with one model the
				// price file does not list is unpriced.
				priced := true
				for mdl, u := range used.byModel {
					if pr := r.Pricing.Lookup(mdl, stageStart.UnixMilli()); pr != nil {
						judgingMoney += money(u, pr.PerMillion)
						judgingPriced = true
					} else {
						priced = false
					}
				}
				if !priced {
					judging.Unpriced++
				}
			}
		}
		// A stage with nothing to answer made no call. What it consumed is
		// known, nothing, and so is what it covered.
		if len(chunks) == 0 {
			h.Usage = &sessiondata.Usage{}
			m.coverage = unjudged(p.Stage, chunk{}, nil)
		}
		// A targets scan is only as good as every chunk of it: a later chunk
		// builds on what an earlier one found, and achievement judged
		// against a catalogue that missed a change could certify an old
		// requirement. A failed chunk fails the stage.
		var frames []any
		switch {
		case len(chunks) > 0 && m.failed == len(chunks), p.Stage == sessionjudge.StageTargets && m.failed > 0:
			// A failed stage still says what it was to cover, all of it
			// unjudged where no call answered.
			h.Status, h.Note = sessionjudge.StatusFailed, clip(strings.Join(m.problems, "; "), 500)
			cov := m.coverage
			h.Coverage = &cov
		case len(m.problems) > 0:
			h.Status, h.Note = sessionjudge.StatusPartial, clip(strings.Join(m.problems, "; "), 1000)
			frames = append(m.inputs, m.frames...)
			cov := m.coverage
			h.Coverage = &cov
		default:
			h.Status = sessionjudge.StatusComplete
			frames = append(m.inputs, m.frames...)
			cov := m.coverage
			h.Coverage = &cov
		}
		// A targets scan answers for every input of the talks judged; a scan
		// that stopped counts those it never reached.
		if p.Stage == sessionjudge.StageTargets && h.Coverage != nil {
			all := len(nav.Inputs())
			h.Coverage.Inputs = &all
			if h.Coverage.InputsRead == nil {
				none := 0
				h.Coverage.InputsRead = &none
			}
		}
		h.PromptBytes = used.promptBytes
		// A reference stage keeps every source it was given, saved or not,
		// whatever its evaluator answered.
		if p.Stage == sessionjudge.StageReference {
			var kept []any
			for _, s := range saved {
				kept = append(kept, sourceFrames(s)...)
			}
			frames = append(kept, frames...)
		}
		var chk *checked
		if h.Status != sessionjudge.StatusFailed {
			chk = &checked{frames: m.frames, targets: cat.targets, goals: cat.goals}
			if p.Stage == sessionjudge.StageTargets {
				for i := range cat.targets {
					frames = append(frames, &cat.targets[i])
				}
				for i := range cat.goals {
					frames = append(frames, &cat.goals[i])
				}
				n := len(cat.targets)
				found, unplaced := goalCoverage(cat.targets, cat.goals)
				h.Coverage.Targets, h.Coverage.Goals, h.Coverage.Unplaced = &n, &found, &unplaced
				chk.frames = m.inputs
			}
		}
		stage, perr := publish(h, frames)
		if perr != nil {
			return nil, perr
		}
		digest := stage.Digest
		r.logf("  %s %s%s", p.Stage, h.Status, noteSuffix(h.Note))
		if used.attempts > 0 {
			r.logf("    %s", stageLine(h, len(chunks)))
		}
		if h.Status != sessionjudge.StatusComplete {
			run.Status = sessionjudge.StatusPartial
		}
		stages = append(stages, stage)
		if chk == nil {
			continue
		}
		switch p.Stage {
		case sessionjudge.StageTargets:
			targets, goals, targetsDigest = chk.targets, chk.goals, digest
			for _, f := range chk.frames {
				inputs = append(inputs, f.(*sessionjudge.Input))
			}
			nav.TargetsFound(inputs)
		case sessionjudge.StageAchievement:
			for _, f := range chk.frames {
				achievements = append(achievements, f.(*sessionjudge.Achievement))
			}
		case sessionjudge.StageSubtasks:
			subtasks = map[string]*sessionjudge.Subtask{}
			for _, f := range chk.frames {
				st := f.(*sessionjudge.Subtask)
				subtasks[st.ID] = st
			}
			nav.Decided(subtasks)
		case sessionjudge.StageReference:
			for _, f := range chk.frames {
				if k, ok := f.(*sessionjudge.Conclusion); ok {
					nav.conclusions = append(nav.conclusions, *k)
				}
			}
		case sessionjudge.StageDivergence:
			for _, f := range chk.frames {
				switch f := f.(type) {
				case *sessionjudge.Divergence:
					divergences = append(divergences, f)
				case *sessionjudge.Rollback:
					rollbacks = append(rollbacks, f)
				case *sessionjudge.Finding:
					findings = append(findings, f)
				}
			}
		case sessionjudge.StageEfficiency:
			labels = []*sessionjudge.Label{}
			// The stage's findings are frames of their own beside its labels.
			for _, f := range chk.frames {
				switch f := f.(type) {
				case *sessionjudge.Label:
					labels = append(labels, f)
				case *sessionjudge.Finding:
					findings = append(findings, f)
				}
			}
		}
	}

	ch := base
	ch.Stage, ch.Evaluator, ch.Status = sessionjudge.StageClose, computed, run.Status
	ch.Started = stamp(r.now())
	if judging != nil && judgingPriced {
		judging.Money = &judgingMoney
	}
	var reportedCost *float64
	if reportedAll && len(stages) > 1 {
		reportedCost = &reported
	}
	more, summary := closeFrames(ledger, targets, inputs, achievements, labels, subtasks, judging)
	more = append(more, report(nav, ledger, targets, goals, inputs, achievements, findings, labels, subtasks, references{conclusions: nav.conclusions,
		divergences: divergences, rollbacks: rollbacks}, more, summary)...)
	summary.JudgingReportedUSD = reportedCost
	if !judgingFrom.IsZero() {
		summary.JudgingMs = judgingTo.Sub(judgingFrom).Milliseconds()
		if judging != nil {
			r.logf("run %d judging: %s over %s; %s", number, usageLine(judging.Usage),
				time.Duration(summary.JudgingMs)*time.Millisecond, rateLine(judging.Usage, judgingTo.Sub(judgingFrom)))
		}
	}
	frames := append(stages, more...)
	ch.Summary = summary
	ch.Finished = stamp(r.now())
	if _, err := publish(ch, frames); err != nil {
		return nil, err
	}
	r.logf("run %d closed %s: %s", number, run.Status, dir)
	return run, nil
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func noteSuffix(note string) string {
	if note == "" {
		return ""
	}
	return ": " + note
}

// ledgerFrames are the ledger stage's frames: the prices applied, the
// conversation's total, each main talk with the child streams it started, the
// main stream's calls no talk contains, and the child streams no single talk
// started. The talks, the main loose calls and the unattributed calls add up
// to the total exactly.
func ledgerFrames(l *Ledger) []any {
	var out []any
	for i := range l.Prices {
		out = append(out, &l.Prices[i])
	}
	total := l.Total
	out = append(out, &total)
	for _, t := range l.Judged() {
		if c, ok := l.Inclusive(t); ok {
			out = append(out, &c)
		}
	}
	for i := range l.Owners {
		if l.Owners[i].ID == LooseOwner("main") {
			c := l.Owners[i]
			out = append(out, &c)
		}
	}
	if l.Unattributed.Calls > 0 {
		u := l.Unattributed
		out = append(out, &u)
	}
	return out
}

// readingOf is what an evaluator read in one stage. An agent CLI reads
// through its own navigation server, which logs every answer; an API
// evaluator reads through the run's navigation, which counts it.
func readingOf(nav *Nav, log string, budget int) *sessionjudge.Reading {
	r := &sessionjudge.Reading{Budget: budget}
	b, err := os.ReadFile(log)
	if err != nil {
		r.Bytes, r.Refused = nav.Spent()
		r.Calls = nav.Calls()
		return r
	}
	for _, line := range bytes.Split(b, []byte("\n")) {
		var c struct {
			Bytes   int  `json:"bytes"`
			Refused bool `json:"refused"`
			// Closing marks the line a server ends its log with, which is
			// no call.
			Closing bool `json:"closing"`
		}
		if json.Unmarshal(line, &c) != nil || c.Closing {
			continue
		}
		r.Calls++
		r.Bytes += c.Bytes
		if c.Refused {
			r.Refused++
		}
	}
	return r
}

// stageLine says in one line what a stage's evaluator calls consumed.
func stageLine(h sessionjudge.Header, calls int) string {
	var parts []string
	parts = append(parts, fmt.Sprintf("%d calls, %d attempts", calls, h.Attempts))
	if rq := h.Requests; rq != nil {
		switch {
		case rq.Count > 0 && rq.ContextWindow > 0:
			parts = append(parts, fmt.Sprintf("%d requests, the largest %s of a %s window", rq.Count, tokenCount(rq.Largest), tokenCount(rq.ContextWindow)))
		case rq.Count > 0:
			parts = append(parts, fmt.Sprintf("%d requests, the largest %s", rq.Count, tokenCount(rq.Largest)))
		case rq.WholeOnly:
			parts = append(parts, "requests not reported one by one")
		}
	}
	if h.Usage != nil {
		parts = append(parts, usageLine(*h.Usage))
	} else {
		parts = append(parts, "tokens unknown")
	}
	if h.ReportedCostUSD != nil {
		parts = append(parts, fmt.Sprintf("reported %.2f USD", *h.ReportedCostUSD))
	}
	if r := h.Reading; r != nil {
		parts = append(parts, fmt.Sprintf("read %s in %d calls", kb(r.Bytes), r.Calls))
	}
	if t0, err := time.Parse(time.RFC3339, h.Started); err == nil {
		if t1, err := time.Parse(time.RFC3339, h.Finished); err == nil {
			parts = append(parts, t1.Sub(t0).String())
			if h.Usage != nil {
				parts = append(parts, rateLine(*h.Usage, t1.Sub(t0)))
			}
		}
	}
	return strings.Join(parts, "; ")
}

// misledOf are the sub-tasks a main talk started whose untrue report the main
// agent acted on, in a stable order. Efficiency labels the main talks only,
// so a nested sub-task that misled the sub-agent above it is not among them.
func misledOf(subtasks map[string]*sessionjudge.Subtask, l *Ledger) []*sessionjudge.Subtask {
	var out []*sessionjudge.Subtask
	for id, d := range subtasks {
		if st, ok := l.Subtask(id); ok && st.Depth == 1 && d.Use == sessionjudge.UseMisled {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
