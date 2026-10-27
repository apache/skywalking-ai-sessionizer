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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
)

// The modes a stage runs in.
const (
	ModeAuto   = "auto"
	ModeSingle = "single"
	ModeStream = "stream"
)

// chunk is the part of a stage one evaluator call answers for, and the rows
// it is given in its prompt so it need not page through them.
type chunk struct {
	assign Assignment
	// targets are the targets an achievement packet judges.
	targets []sessionjudge.Target
	rows    []any
}

// item is one thing a stage answers for, with the row it is shown as, and
// need, what reading its work takes: its outline, every page.
type item struct {
	id         string
	target     *sessionjudge.Target
	source     *SourceDoc
	conclusion *sessionjudge.Conclusion
	rows       []any
	bytes      int
	need       int
}

// stageItems are what a stage answers for, in order: the inputs of the
// talks judged, each target, each sub-task, or each talk a target claims.
func stageItems(stage string, nav *Nav, targets []sessionjudge.Target) []item {
	size := func(rows []any) int {
		n := 0
		for _, r := range rows {
			b, _ := json.Marshal(r)
			n += len(b) + 1
		}
		return n
	}
	var out []item
	switch stage {
	case sessionjudge.StageTargets:
		for _, r := range nav.InputRows(nav.Inputs()) {
			out = append(out, item{id: r.(inputRow).Node, rows: []any{r}, bytes: size([]any{r})})
		}
	case sessionjudge.StageAchievement:
		for i := range targets {
			rows := nav.TalkRows(targets[i].Talks)
			out = append(out, item{id: targets[i].ID, target: &targets[i], rows: rows, bytes: size(rows)})
		}
	case sessionjudge.StageSubtasks:
		for _, st := range nav.ledger.Subtasks {
			if !nav.ledger.JudgesSubtask(&st) {
				continue
			}
			rows := nav.SubtaskRows([]string{st.ID})
			out = append(out, item{id: st.ID, rows: rows, bytes: size(rows), need: rows[0].(subtaskRow).OutlineBytes})
		}
	case sessionjudge.StageReference:
		return sourceItems(nav.sourceDocs, nav.sources)
	case sessionjudge.StageDivergence:
		return conclusionItems(nav.conclusions)
	case sessionjudge.StageRegroup:
		// A target's words, never its goal: the call places targets in the
		// person's goals without seeing how the judge grouped them.
		for i := range targets {
			rows := []any{map[string]any{"id": targets[i].ID, "statement": clip(targets[i].Statement, 300), "status": targets[i].Status}}
			out = append(out, item{id: targets[i].ID, target: &targets[i], rows: rows, bytes: size(rows)})
		}
	case sessionjudge.StageEfficiency:
		claimed := map[string]bool{}
		for _, t := range targets {
			for _, tk := range t.Talks {
				claimed[tk] = true
			}
		}
		for _, tk := range nav.Judged() {
			if claimed[tk] {
				rows := nav.TalkRows([]string{tk})
				out = append(out, item{id: tk, rows: rows, bytes: size(rows), need: rows[0].(talkRow).OutlineBytes})
			}
		}
	}
	return out
}

// planChunks cuts a stage into the calls that answer it. In single mode, or
// in auto mode when what the stage is given fits singleBytes and reading it
// fits the call's reading budget, one call takes all of it. Otherwise each
// chunk holds what fits chunkBytes and what can be read within the budget,
// and an achievement packet holds one target. A chunk always holds at least
// one item, however large, and every item is in exactly one chunk.
//
// Reading counts because an evaluator that cannot open what it labels labels
// it unread. Measured in round two: efficiency over 40 talks of e28bf1c1 in
// one call, with a 128 KiB budget, left 80% of the cost unreviewed.
func planChunks(stage, mode string, singleBytes, chunkBytes, readBytes int, nav *Nav, targets []sessionjudge.Target) (string, []chunk) {
	items := stageItems(stage, nav, targets)
	switch {
	// A regroup with no target still answers for the person's goals.
	case len(items) == 0 && stage == sessionjudge.StageRegroup:
		return ModeSingle, []chunk{{}}
	case len(items) == 0:
		return ModeSingle, nil
	}
	// The outlines a call reads may take three quarters of its budget; the
	// rest is for the steps and records it opens to decide.
	readable := readBytes * 3 / 4
	total, need := 0, 0
	for _, it := range items {
		total, need = total+it.bytes, need+it.need
	}
	fits := readBytes == 0 || need <= readable
	if mode == "" {
		mode = ModeAuto
	}
	// Achievement is one call per target unless one call is asked for: each
	// target then has the reading budget to itself. Measured in round two:
	// six targets of a0fc872b in one call read 117 KB of a 128 KiB budget
	// and left three of them uninvestigated, where one call per target read
	// what each needed.
	perTarget := stage == sessionjudge.StageAchievement && mode != ModeSingle
	// A reference stage is always one call: what a conclusion builds on, or
	// disagrees with, can be in another source, and its id must be unique
	// among all of them. Each item is a source's manifest, so the call
	// stays small; it reads the sources through the source tool. A regroup
	// is one call too, since two targets are compared by the goals each is
	// placed in.
	whole := stage == sessionjudge.StageReference || stage == sessionjudge.StageRegroup
	if whole || !perTarget && (mode == ModeSingle || (mode == ModeAuto && total <= singleBytes && fits)) || len(items) <= 1 {
		ch := chunk{}
		for _, it := range items {
			ch.add(stage, it)
		}
		return ModeSingle, []chunk{ch}
	}
	var out []chunk
	var cur chunk
	size, reading := 0, 0
	for _, it := range items {
		full := size > 0 && (size+it.bytes > chunkBytes || (readBytes > 0 && reading+it.need > readable))
		if stage == sessionjudge.StageAchievement || full {
			if size > 0 || len(cur.targets) > 0 {
				out = append(out, cur)
			}
			cur, size, reading = chunk{}, 0, 0
		}
		cur.add(stage, it)
		size, reading = size+it.bytes, reading+it.need
	}
	out = append(out, cur)
	return ModeStream, out
}

func (c *chunk) add(stage string, it item) {
	switch stage {
	case sessionjudge.StageTargets:
		c.assign.Inputs = append(c.assign.Inputs, it.id)
	case sessionjudge.StageAchievement:
		c.assign.Targets = append(c.assign.Targets, it.id)
		c.targets = append(c.targets, *it.target)
	case sessionjudge.StageSubtasks:
		c.assign.Subtasks = append(c.assign.Subtasks, it.id)
	case sessionjudge.StageEfficiency:
		c.assign.Talks = append(c.assign.Talks, it.id)
	case sessionjudge.StageReference:
		c.assign.Sources = append(c.assign.Sources, *it.source)
	case sessionjudge.StageDivergence:
		c.assign.Conclusions = append(c.assign.Conclusions, *it.conclusion)
	case sessionjudge.StageRegroup:
		c.assign.Targets = append(c.assign.Targets, it.id)
	}
	c.rows = append(c.rows, it.rows...)
}

// pushText writes the rows a call answers for into its prompt, one JSON
// object per line, and for a targets scan the catalogue the earlier chunks
// left: at most stateBytes of it, as catalogueLines shortens it, since the
// targets tool gives any target whole.
//
// The rows take at most rowsBytes. A chunk holds at least one item, and one
// item, such as a target with many talks, can be larger than a chunk: its
// rows past the bound are named by count, and the tools list them.
func pushText(stage string, part, of int, ch chunk, cat catalogue, stateBytes, rowsBytes int) string {
	var b strings.Builder
	if of > 1 {
		fmt.Fprintf(&b, "\nThis call answers for part %d of %d of this stage. Everything outside it is context.\n", part, of)
	}
	if stage == sessionjudge.StageTargets {
		b.WriteString("\nThe targets found so far by earlier parts of this run")
		if len(cat.targets) == 0 {
			b.WriteString(": none yet.\n")
		} else {
			b.WriteString(", one per line. Update one by its id; set a new one under a key such as n1. The targets tool gives any of them whole:\n\n")
			b.WriteString(catalogueLines(cat.targets, stateBytes))
		}
		b.WriteString("\nThe goals found so far by earlier parts of this run")
		if len(cat.goals) == 0 {
			b.WriteString(": none yet.\n")
		} else {
			b.WriteString(", one per line. Update one by its id; start a new one under a key such as ga. A relation follows an earlier input of its goal, such as its last_input:\n\n")
			b.WriteString(goalLines(cat.goals, cat.latest, stateBytes))
		}
	}
	what := map[string]string{
		sessionjudge.StageTargets:     "The inputs this call answers for, in order",
		sessionjudge.StageAchievement: "The talks of the targets this call judges",
		sessionjudge.StageSubtasks:    "The sub-tasks this call judges",
		sessionjudge.StageEfficiency:  "The talks this call labels",
		sessionjudge.StageReference:   "The sources this call reads, each as asz saved it",
		sessionjudge.StageDivergence:  "The conclusions this call compares with the conversation",
		sessionjudge.StageRegroup:     "The targets this call places in the person's goals",
	}[stage]
	fmt.Fprintf(&b, "\n%s, one per line, as the tools show them:\n\n", what)
	var lines []string
	for _, r := range ch.rows {
		line, _ := json.Marshal(r)
		lines = append(lines, string(line))
	}
	tool := map[string]string{
		sessionjudge.StageTargets:     "inputs",
		sessionjudge.StageAchievement: "talks",
		sessionjudge.StageSubtasks:    "subtasks",
		sessionjudge.StageEfficiency:  "talks",
		sessionjudge.StageReference:   "source",
		sessionjudge.StageDivergence:  "conclusions",
		sessionjudge.StageRegroup:     "targets",
	}[stage]
	b.WriteString(boundedLines(lines, rowsBytes, "rows, which the "+tool+" tool lists"))
	return b.String()
}

// merged gathers what the chunks of one stage answered into one stage's
// frames, coverage and notes.
type merged struct {
	frames   []any
	inputs   []any
	problems []string
	coverage sessionjudge.Coverage
	failed   int
}

func (m *merged) add(part, of int, c *checked) {
	for _, p := range c.problems {
		if of > 1 {
			p = fmt.Sprintf("part %d: %s", part, p)
		}
		m.problems = append(m.problems, p)
	}
	for _, f := range c.frames {
		if in, ok := f.(*sessionjudge.Input); ok {
			m.inputs = append(m.inputs, in)
			continue
		}
		m.frames = append(m.frames, f)
	}
	m.coverage = addCoverage(m.coverage, c.coverage)
}

// addCoverage adds two stages' coverage, count by count.
func addCoverage(a, b sessionjudge.Coverage) sessionjudge.Coverage {
	sum := func(x, y *int) *int {
		if x == nil && y == nil {
			return nil
		}
		n := 0
		if x != nil {
			n += *x
		}
		if y != nil {
			n += *y
		}
		return &n
	}
	return sessionjudge.Coverage{
		Inputs: sum(a.Inputs, b.Inputs), InputsRead: sum(a.InputsRead, b.InputsRead),
		Targets: sum(a.Targets, b.Targets), TargetsJudged: sum(a.TargetsJudged, b.TargetsJudged),
		Talks: sum(a.Talks, b.Talks), TalksLabeled: sum(a.TalksLabeled, b.TalksLabeled),
		Subtasks: sum(a.Subtasks, b.Subtasks), SubtasksJudged: sum(a.SubtasksJudged, b.SubtasksJudged),
		Calls: sum(a.Calls, b.Calls), CallsWithoutUsage: sum(a.CallsWithoutUsage, b.CallsWithoutUsage),
	}
}

// stageRun runs the chunks of one stage.
type stageRun struct {
	runner       *Runner
	kit          *Kit
	ex           Executor
	nav          *Nav
	stage        string
	order        int
	session, dir string
	schema       json.RawMessage
	targets      []sessionjudge.Target
	goals        []sessionjudge.Goal
	inputs       []*sessionjudge.Input
	achievements []*sessionjudge.Achievement
	misled       []*sessionjudge.Subtask
	subtaskStage bool
	timeout      time.Duration
	mcp          MCPArgs
	// work names the stage's work directory, the stage's order and name
	// when empty; a regroup, which a run can have many of, its version.
	work string
	// personGoals are the person's goals a regroup places targets in.
	personGoals []string

	mu   sync.Mutex
	used used
	// live is what the calls that are running have consumed so far, and
	// running their navigation and log, for the progress lines.
	live    sessiondata.Usage
	liveReq int
	done    int
	running map[string]*Nav
}

// used is what the calls of one stage consumed, failed attempts included.
// A total is unknown as soon as one call did not say.
type used struct {
	usage    *sessiondata.Usage
	byModel  map[string]sessiondata.Usage
	costUSD  *float64
	reading  *sessionjudge.Reading
	attempts int
	model    string
	unknown  bool
	noCost   bool
	requests sessionjudge.Requests
	// promptBytes is the largest prompt a call of the stage was sent.
	promptBytes int
}

func (s *stageRun) note(res Result, reading *sessionjudge.Reading) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := &s.used
	u.attempts++
	if u.model == "" {
		u.model = res.Model
	}
	for _, r := range res.Requests {
		if r.Whole {
			u.requests.WholeOnly = true
			continue
		}
		u.requests.Count++
		if in := r.Usage.Input + r.Usage.CacheRead + r.Usage.CacheWrite; in > u.requests.Largest {
			u.requests.Largest = in
		}
	}
	if res.ContextWindow > u.requests.ContextWindow {
		u.requests.ContextWindow = res.ContextWindow
	}
	u.requests.Unknown += res.UnknownRequests
	if res.Usage == nil {
		u.unknown = true
	} else {
		if u.usage == nil {
			u.usage = &sessiondata.Usage{}
		}
		add(u.usage, *res.Usage)
		if u.byModel == nil {
			u.byModel = map[string]sessiondata.Usage{}
		}
		by := res.ByModel
		if len(by) == 0 {
			by = map[string]sessiondata.Usage{res.Model: *res.Usage}
		}
		for m, mu := range by {
			v := u.byModel[m]
			add(&v, mu)
			u.byModel[m] = v
		}
	}
	if res.CostUSD == nil {
		u.noCost = true
	} else {
		if u.costUSD == nil {
			u.costUSD = new(float64)
		}
		*u.costUSD += *res.CostUSD
	}
	if u.reading == nil {
		u.reading = &sessionjudge.Reading{Budget: reading.Budget}
	}
	u.reading.Calls += reading.Calls
	u.reading.Bytes += reading.Bytes
	u.reading.Refused += reading.Refused
}

// progress is told of each request as it ends, while its call runs.
func (s *stageRun) progress(r Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	add(&s.live, r.Usage)
	if !r.Whole {
		s.liveReq++
	}
}

// line says what the stage has consumed so far: the calls ended and
// running, the requests and tokens counted, and what the running calls read.
func (s *stageRun) line(chunks int, since time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	reads, bytes := 0, 0
	for log, nav := range s.running {
		r := readingOf(nav, log, 0)
		reads, bytes = reads+r.Calls, bytes+r.Bytes
	}
	if s.used.reading != nil {
		reads, bytes = reads+s.used.reading.Calls, bytes+s.used.reading.Bytes
	}
	elapsed := time.Since(since)
	return fmt.Sprintf("    %s, %s: %d of %d calls done, %d running; %d requests, %s; %s; read %s in %d calls",
		s.stage, elapsed.Round(time.Second), s.done, chunks, len(s.running), s.liveReq, usageLine(s.live), rateLine(s.live, elapsed), kb(bytes), reads)
}

// rateLine says how fast tokens were consumed: the input side, cache reads
// and writes included, and the output, per minute.
func rateLine(u sessiondata.Usage, d time.Duration) string {
	if d <= 0 {
		return "no time for a rate yet"
	}
	m := d.Minutes()
	in := float64(u.Input + u.CacheRead + u.CacheWrite)
	return fmt.Sprintf("%s input and %s output tokens per minute", tokenCount(int(in/m)), tokenCount(int(float64(u.Output)/m)))
}

// usageLine says a usage in a few words.
func usageLine(u sessiondata.Usage) string {
	return fmt.Sprintf("in %s, cache read %s, cache write %s, out %s tokens", tokenCount(u.Input), tokenCount(u.CacheRead), tokenCount(u.CacheWrite), tokenCount(u.Output))
}

func tokenCount(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

func kb(n int) string { return fmt.Sprintf("%.0f KB", float64(n)/1024) }

func (s *stageRun) total() used {
	u := s.used
	if u.unknown {
		u.usage = nil
	}
	if u.noCost {
		u.costUSD = nil
	}
	return u
}

// run runs every chunk and merges what they answered. A targets scan runs in
// order, each chunk given the catalogue the one before left, and stops at a
// chunk that fails. Other stages run their chunks side by side, up to the
// configured concurrency, and merge them in order, so the stage file does
// not depend on which call finished first.
func (s *stageRun) run(ctx context.Context, chunks []chunk) (merged, catalogue, used, error) {
	var m merged
	s.running = map[string]*Nav{}
	// What the stage has consumed so far, said every so often while it
	// runs, since a stage of a large conversation takes minutes.
	if every, err := time.ParseDuration(s.runner.Config.Progress); err == nil && every > 0 && len(chunks) > 0 {
		since := time.Now()
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			t := time.NewTicker(every)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					s.runner.logf("%s", s.line(len(chunks), since))
				}
			}
		}()
	}
	if s.stage == sessionjudge.StageTargets {
		var cat catalogue
		for i, ch := range chunks {
			c, err := s.chunk(ctx, i, len(chunks), ch, cat)
			if err != nil {
				if storageError(err) {
					return m, catalogue{}, used{}, err
				}
				m.failed++
				m.problems = append(m.problems, fmt.Sprintf("part %d failed: %v", i+1, err))
				return m, catalogue{}, s.total(), nil
			}
			cat = catalogue{targets: c.targets, goals: c.goals, relations: c.relations, latest: c.latest}
			m.add(i+1, len(chunks), c)
		}
		return m, cat, s.total(), nil
	}
	results := make([]*checked, len(chunks))
	errs := make([]error, len(chunks))
	limit := s.runner.Config.Concurrency
	if limit < 1 {
		limit = 1
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := range chunks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i], errs[i] = s.chunk(ctx, i, len(chunks), chunks[i], catalogue{})
		}(i)
	}
	// Every chunk ends, or is cancelled, before the stage is merged and
	// published.
	wg.Wait()
	for i := range chunks {
		if errs[i] != nil {
			if storageError(errs[i]) {
				return m, catalogue{}, used{}, errs[i]
			}
			m.failed++
			m.problems = append(m.problems, fmt.Sprintf("part %d failed: %v", i+1, errs[i]))
			m.coverage = addCoverage(m.coverage, unjudged(s.stage, chunks[i], s.targets))
			continue
		}
		m.add(i+1, len(chunks), results[i])
	}
	return m, catalogue{}, s.total(), nil
}

// errStorage marks an error of the storage root, which ends the run rather
// than one chunk.
type errStorage struct{ err error }

func (e errStorage) Error() string { return e.err.Error() }
func (e errStorage) Unwrap() error { return e.err }

func storageError(err error) bool {
	var e errStorage
	return errors.As(err, &e)
}

// chunkTargets are the targets a call is given: an achievement packet's own,
// those claiming a talk the call labels, those whose talks started a
// sub-task it judges. A call is not given every target of the run: the
// prompt would grow with the conversation and the call would pay for what it
// does not answer.
func (s *stageRun) chunkTargets(ch chunk) []sessionjudge.Target {
	var keep map[string]bool
	switch s.stage {
	case sessionjudge.StageAchievement:
		return ch.targets
	case sessionjudge.StageEfficiency:
		keep = set(ch.assign.Talks)
	case sessionjudge.StageSubtasks:
		keep = map[string]bool{}
		for _, id := range ch.assign.Subtasks {
			if st, ok := s.nav.ledger.Subtask(id); ok {
				keep[st.Main] = true
			}
		}
	default:
		return s.targets
	}
	var out []sessionjudge.Target
	for _, t := range s.targets {
		if slices.ContainsFunc(t.Talks, func(tk string) bool { return keep[tk] }) {
			out = append(out, t)
		}
	}
	return out
}

// unjudged is what a call was to answer for, counted as not judged: a call
// that failed stays in the stage's totals, and a stage with no call to make
// still says it covered none.
func unjudged(stage string, ch chunk, targets []sessionjudge.Target) sessionjudge.Coverage {
	zero := 0
	n := 0
	switch stage {
	case sessionjudge.StageAchievement:
		n = len(ch.targets)
		return sessionjudge.Coverage{Targets: &n, TargetsJudged: &zero}
	case sessionjudge.StageRegroup:
		n = len(ch.assign.Targets)
		return sessionjudge.Coverage{Targets: &n, TargetsJudged: &zero}
	case sessionjudge.StageSubtasks:
		n = len(ch.assign.Subtasks)
		return sessionjudge.Coverage{Subtasks: &n, SubtasksJudged: &zero}
	case sessionjudge.StageEfficiency:
		mine := set(ch.assign.Talks)
		for _, t := range targets {
			for _, tk := range t.Talks {
				if mine[tk] {
					n++
				}
			}
		}
		return sessionjudge.Coverage{Talks: &n, TalksLabeled: &zero}
	}
	return sessionjudge.Coverage{}
}

// chunk runs one chunk: an attempt, and one more if the first gave nothing
// that could be read. Each attempt has its own directory, so a retry never
// writes over what the first left, and what a failed attempt consumed still
// counts.
func (s *stageRun) chunk(ctx context.Context, i, of int, ch chunk, cat catalogue) (*checked, error) {
	targets := s.chunkTargets(ch)
	var last error
	// issued are the failure markers given to the chunk's attempts on a
	// pulled snapshot, which are checked before any answer is accepted.
	var issued []string
	for attempt := 1; attempt <= 2; attempt++ {
		work := s.work
		if work == "" {
			work = fmt.Sprintf("%d-%s", s.order, s.stage)
		}
		dir, err := storage.MakeWorkDir(s.runner.Zone, s.session, s.dir, fmt.Sprintf("%s/c%03d/a%d", work, i+1, attempt))
		if err != nil {
			return nil, errStorage{err}
		}
		nav := s.nav.Fork()
		var marker string
		if s.nav.strict != nil {
			if s.runner.Failures == nil {
				return nil, errStorage{errors.New("judge: a run over a pulled snapshot needs its lease to name failure markers")}
			}
			marker = filepath.Join(dir, storage.FailurePrefix+s.runner.Failures())
			issued = append(issued, marker)
			nav.Strictly(s.nav.strict, marker)
		}
		// The call's task names the catalogue too, so the targets tool can
		// give whole what the prompt gives compactly.
		ch.assign.Catalogue, ch.assign.Goals = cat.targets, cat.goals
		if s.stage != sessionjudge.StageTargets {
			ch.assign.Catalogue, ch.assign.Goals = s.targets, s.goals
		}
		// A regroup places targets without seeing the judge's goals, so the
		// comparison measures them.
		if s.stage == sessionjudge.StageRegroup {
			ch.assign.Goals = nil
		}
		ch.assign.Decided = s.nav.decisions()
		// A divergence call reads the sources its conclusions came from, and
		// names the deliveries its conclusions show wrong.
		if s.stage == sessionjudge.StageDivergence {
			ch.assign.Sources = s.nav.sourceDocs
			ch.assign.Events = map[string][]sessionjudge.Event{}
			for _, a := range s.achievements {
				ch.assign.Events[a.Target] = a.Events
			}
		}
		nav.Assign(ch.assign)
		nav.ResetBudget(s.runner.Config.ReadBytes)
		task, _ := json.Marshal(ch.assign)
		taskPath := filepath.Join(dir, "task.json")
		if err := os.WriteFile(taskPath, task, 0o600); err != nil {
			return nil, errStorage{err}
		}
		text := taskText(s.stage, nav, given{targets: targets, achievements: s.achievements, misled: s.misled, subtaskStage: s.subtaskStage,
			limit: s.runner.Config.StateBytes})
		if s.stage == sessionjudge.StageRegroup {
			text += personGoalsText(s.personGoals)
		}
		prompt, _, err := s.kit.Prompt(s.stage, text+pushText(s.stage, i+1, of, ch, cat, s.runner.Config.StateBytes, s.runner.Config.ChunkBytes))
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		if len(prompt) > s.used.promptBytes {
			s.used.promptBytes = len(prompt)
		}
		s.mu.Unlock()
		args := s.mcp
		args.Log, args.Task, args.Failure = filepath.Join(dir, "mcp-calls.jsonl"), taskPath, marker
		s.mu.Lock()
		s.running[args.Log] = nav
		s.mu.Unlock()
		cctx, cancel := context.WithTimeout(ctx, s.timeout)
		res, err := s.ex.Run(cctx, Task{Stage: s.stage, Prompt: prompt, Schema: s.schema, Dir: dir, Nav: nav, MCP: s.runner.MCP(args),
			Progress: s.progress})
		cancel()
		s.mu.Lock()
		delete(s.running, args.Log)
		s.mu.Unlock()
		writeRequests(filepath.Join(dir, "requests.jsonl"), res.Requests)
		s.note(res, readingOf(nav, args.Log, s.runner.Config.ReadBytes))
		if s.nav.strict != nil {
			// On a pulled snapshot, a read of evidence that failed or did
			// not finish, here or in a navigation server, fails the run
			// rather than the attempt: another attempt reads the same
			// evidence. The answer is checked only after this, by the
			// run's own reads.
			if ferr := s.nav.strict.Failed(); ferr != nil {
				return nil, errStorage{ferr}
			}
			if ferr := CheckMarkers(issued); ferr != nil {
				return nil, errStorage{ferr}
			}
			nav.Strictly(s.nav.strict, "")
		}
		if err == nil {
			_ = os.WriteFile(filepath.Join(dir, "output.json"), res.Output, 0o600)
			var c *checked
			switch s.stage {
			case sessionjudge.StageTargets:
				c, err = checkTargets(nav, cat, res.Output)
			case sessionjudge.StageAchievement:
				c, err = checkAchievement(nav, targets, res.Output)
			case sessionjudge.StageSubtasks:
				c, err = checkSubtasks(nav, res.Output)
			case sessionjudge.StageEfficiency:
				c, err = checkEfficiency(nav, targets, misledSet(s.misled), s.subtaskStage, res.Output)
			case sessionjudge.StageReference:
				c, err = checkReference(nav, res.Output)
			case sessionjudge.StageDivergence:
				c, err = checkDivergence(nav, targets, s.inputs, s.achievements, s.misled, res.Output)
			case sessionjudge.StageRegroup:
				c, err = checkRegroup(nav, s.personGoals, targets, s.goals, res.Output)
			}
			if err == nil && s.nav.strict != nil {
				// The answer's checks read evidence too.
				if ferr := s.nav.strict.Failed(); ferr != nil {
					return nil, errStorage{ferr}
				}
			}
			if err == nil {
				s.mu.Lock()
				s.done++
				s.mu.Unlock()
				return c, nil
			}
		}
		last = err
		if ctx.Err() != nil {
			break
		}
		s.runner.logf("    part %d attempt %d: %v", i+1, attempt, err)
	}
	return nil, last
}

// writeRequests keeps the model requests of one attempt beside what it was
// given and answered, one per line, as the provider counted them.
func writeRequests(path string, requests []Request) {
	var b strings.Builder
	for _, r := range requests {
		line, _ := json.Marshal(r)
		b.Write(line)
		b.WriteString("\n")
	}
	_ = os.WriteFile(path, []byte(b.String()), 0o600)
}

// catalogueLines writes the targets found so far, one per line, within
// limit bytes when it is set. It takes the first of these that fits: every
// target whole; the open targets whole and the others short; every target
// short; every target as its id and status.
func catalogueLines(catalogue []sessionjudge.Target, limit int) string {
	type crit struct {
		ID      string `json:"id"`
		Text    string `json:"text"`
		Dropped bool   `json:"dropped,omitempty"`
	}
	whole := func(t sessionjudge.Target) any {
		cs := []crit{}
		for _, c := range t.Criteria {
			cs = append(cs, crit{c.ID, clip(c.Text, 200), c.Dropped})
		}
		return map[string]any{"id": t.ID, "statement": clip(t.Statement, 300), "status": t.Status, "criteria": cs, "talks": len(t.Talks)}
	}
	short := func(t sessionjudge.Target) any {
		return map[string]any{"id": t.ID, "statement": clip(t.Statement, 80), "status": t.Status}
	}
	bare := func(t sessionjudge.Target) any { return map[string]any{"id": t.ID, "status": t.Status} }
	openWhole := func(t sessionjudge.Target) any {
		if t.Status == sessionjudge.TargetOpen {
			return whole(t)
		}
		return short(t)
	}
	write := func(form func(sessionjudge.Target) any) string {
		var b strings.Builder
		for _, t := range catalogue {
			line, _ := json.Marshal(form(t))
			b.Write(line)
			b.WriteString("\n")
		}
		return b.String()
	}
	for _, form := range []func(sessionjudge.Target) any{whole, openWhole, short} {
		if text := write(form); limit <= 0 || len(text) <= limit {
			return text
		}
	}
	var lines []string
	for _, t := range catalogue {
		line, _ := json.Marshal(bare(t))
		lines = append(lines, string(line))
	}
	return boundedLines(lines, limit, "targets; the targets tool lists them")
}

// goalLines writes the goals found so far, one per line, within limit bytes:
// each goal's id, statement, members and main target, and the last input
// related to it, which a later relation can follow.
func goalLines(goals []sessionjudge.Goal, latest map[string]string, limit int) string {
	var lines []string
	for _, g := range goals {
		row := map[string]any{"id": g.ID, "statement": clip(g.Statement, 200), "members": g.Members}
		if g.Main != "" {
			row["main"] = g.Main
		}
		if in := latest[g.ID]; in != "" {
			row["last_input"] = in
		}
		line, _ := json.Marshal(row)
		lines = append(lines, string(line))
	}
	return boundedLines(lines, limit, "goals; the targets tool lists them")
}

// boundedLines writes lines, one each, while they fit limit bytes, and then
// says how many more there are and where to find them.
func boundedLines(lines []string, limit int, rest string) string {
	var b strings.Builder
	for i, l := range lines {
		// Room is kept for the line that says what is left.
		if limit > 0 && b.Len()+len(l)+1 > limit-len(rest)-16 {
			fmt.Fprintf(&b, "%d more %s.\n", len(lines)-i, rest)
			break
		}
		b.WriteString(l)
		b.WriteString("\n")
	}
	return b.String()
}

// misledSet names the sub-tasks found to have misled the main agent.
func misledSet(misled []*sessionjudge.Subtask) map[string]bool {
	out := map[string]bool{}
	for _, st := range misled {
		out[st.ID] = true
	}
	return out
}
