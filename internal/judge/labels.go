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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
)

// labelBytes bounds a label file, and goalBytes one goal in it: a goal is a
// sentence or two, and the regroup call is given every one.
const (
	labelBytes = 64 << 10
	goalBytes  = 2000
)

// Labels are a person's goals for a conversation, in their own words, in the
// order the file gives them. A person knows what they wanted; which inputs
// or steps it took is for the regroup call to find.
type Labels struct {
	File   string
	SHA256 string
	goals  []string
	// text is the file as the person gave it, which a regroup keeps.
	text string
}

// ReadLabels reads a label file:
//
//	goals:
//	  - Finish the post-vote steps of the 0.4.0 release
//	  - Remove the website pull request step from the post-vote script
//
// A goal that is empty, longer than goalBytes, or given twice makes the file
// unreadable, since the comparison would rest on it.
func ReadLabels(path string) (*Labels, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// A regroup keeps the file as text, so it must be text, and small.
	switch {
	case len(data) > labelBytes:
		return nil, fmt.Errorf("%s is larger than %d KiB", path, labelBytes>>10)
	case !utf8.Valid(data):
		return nil, fmt.Errorf("%s is not UTF-8 text", path)
	}
	var f struct {
		Goals []string `yaml:"goals"`
	}
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(f.Goals) == 0 {
		return nil, fmt.Errorf("%s lists no goal under goals", path)
	}
	sum := sha256.Sum256(data)
	l := &Labels{File: filepath.Base(path), SHA256: hex.EncodeToString(sum[:]), text: string(data)}
	for i, g := range f.Goals {
		g = strings.TrimSpace(g)
		switch at := slices.Index(l.goals, g); {
		case g == "":
			return nil, fmt.Errorf("%s: goal %d is empty", path, i+1)
		case len(g) > goalBytes:
			return nil, fmt.Errorf("%s: goal %d is longer than %d bytes", path, i+1, goalBytes)
		case at >= 0:
			return nil, fmt.Errorf("%s: goal %d is goal %d again", path, i+1, at+1)
		}
		l.goals = append(l.goals, g)
	}
	return l, nil
}

// goalID is the id of the person's goal at index i: P1 for the first.
func goalID(i int) string { return fmt.Sprintf("P%d", i+1) }

// personGoalsText is what a regroup call is given of the person's goals: each
// by its id, whole.
func personGoalsText(goals []string) string {
	var b strings.Builder
	b.WriteString("\nThe person's goals for this conversation, in their own words, by id:\n\n")
	for i, g := range goals {
		fmt.Fprintf(&b, "- %s: %s\n", goalID(i), g)
	}
	return b.String()
}

// checkRegroup checks a regroup answer and compares the two groupings.
//
// A target placed in one of the person's goals cites an input of the person
// that shows it; one that cites none, or names a goal the person did not
// give, is not placed. A target in none of them is the evaluator's
// judgment, which no record can show. A goal the answer says was worked on
// cites the person's input that asked for it.
//
// The comparison is asz's: each of the person's goals is in one goal of the
// judge, several, or one it shares with another of the person's goals. A
// target the judge put in no goal stands alone, as the judge left it.
func checkRegroup(nav *Nav, goals []string, targets []sessionjudge.Target, judgeGoals []sessionjudge.Goal, raw json.RawMessage) (*checked, error) {
	// A decision the answer leaves out is none it made: a missing goal or
	// worked is not read as none or as false.
	var a struct {
		Targets []struct {
			Target string   `json:"target"`
			Goal   *string  `json:"goal"`
			Note   string   `json:"note"`
			Cite   []string `json:"cite"`
		} `json:"targets"`
		Goals []struct {
			Goal   string   `json:"goal"`
			Worked *bool    `json:"worked"`
			Note   string   `json:"note"`
			Cite   []string `json:"cite"`
		} `json:"goals"`
	}
	if err := decodeAnswer(raw, "targets", &a); err != nil {
		return nil, err
	}
	c := &checked{}
	index := map[string]int{}
	for i := range goals {
		index[goalID(i)] = i
	}
	// person says whether a citation names an input of the person.
	person := func(cites []sessionjudge.Cite) bool {
		return slices.ContainsFunc(cites, func(ct sessionjudge.Cite) bool { return nav.person[ct.Node] })
	}
	placed := map[string]*sessionjudge.Placement{}
	for _, t := range targets {
		placed[t.ID] = nil
	}
	// rejected are the person's goals a placement named and could not show:
	// such a goal may have a target, so nothing is said of it but unknown.
	rejected := map[string]bool{}
	dropped, left, unshown := 0, 0, 0
	for _, x := range a.Targets {
		id := strings.TrimSpace(x.Target)
		if p, known := placed[id]; !known || p != nil || x.Goal == nil {
			left++
			continue
		}
		p := &sessionjudge.Placement{Target: id, Goal: strings.TrimSpace(*x.Goal), Note: x.Note, Cite: c.cites(nav, x.Cite, &dropped)}
		if _, ok := index[p.Goal]; p.Goal != "" && (!ok || !person(p.Cite)) {
			unshown++
			rejected[p.Goal] = true
			p.Goal, p.Unknown = "", true
		}
		placed[id] = p
	}
	// claimed is what the answer said of each goal, and worked what the
	// record shows of it.
	answered, claimed, worked := map[string]bool{}, map[string]bool{}, map[string]bool{}
	notes, cites := map[string]string{}, map[string][]sessionjudge.Cite{}
	for _, x := range a.Goals {
		id := strings.TrimSpace(x.Goal)
		if _, ok := index[id]; !ok || answered[id] || x.Worked == nil {
			left++
			continue
		}
		answered[id], claimed[id] = true, *x.Worked
		cites[id], notes[id] = c.cites(nav, x.Cite, &dropped), x.Note
		worked[id] = claimed[id] && person(cites[id])
		if claimed[id] && !worked[id] {
			unshown++
		}
	}

	// The judge's goal of each target, or the target alone when the judge
	// put it in none.
	bucket, judgeOf := map[string]string{}, map[string]string{}
	for _, g := range judgeGoals {
		for _, m := range g.Members {
			bucket[m], judgeOf[m] = g.ID, g.ID
		}
	}
	status := map[string]string{}
	members := map[string][]string{}
	var extra, unknown []string
	missing := 0
	for _, t := range targets {
		status[t.ID] = t.Status
		if bucket[t.ID] == "" {
			bucket[t.ID] = "target " + t.ID
		}
		p := placed[t.ID]
		switch {
		case p == nil:
			missing++
			placed[t.ID] = &sessionjudge.Placement{Target: t.ID, Unknown: true, Cite: []sessionjudge.Cite{}}
			unknown = append(unknown, t.ID)
		case p.Unknown:
			unknown = append(unknown, t.ID)
		case p.Goal == "":
			extra = append(extra, t.ID)
		default:
			members[p.Goal] = append(members[p.Goal], t.ID)
		}
	}
	// owners are the person's goals whose targets each bucket holds.
	owners := map[string][]string{}
	for i := range goals {
		for _, m := range members[goalID(i)] {
			if !slices.Contains(owners[bucket[m]], goalID(i)) {
				owners[bucket[m]] = append(owners[bucket[m]], goalID(i))
			}
		}
	}
	agreement := &sessionjudge.Agreement{Goals: len(goals), Classes: map[string]int{}, Extra: extra, Unknown: unknown}
	unanswered := 0
	for i, statement := range goals {
		id := goalID(i)
		g := &sessionjudge.LabelGoal{ID: id, Statement: statement, Members: []string{}, Judge: []string{}, Note: notes[id], Cite: []sessionjudge.Cite{}}
		g.Members = append(g.Members, members[id]...)
		g.Cite = append(g.Cite, cites[id]...)
		g.Worked = worked[id] || len(g.Members) > 0
		var buckets []string
		for _, m := range g.Members {
			if !slices.Contains(buckets, bucket[m]) {
				buckets = append(buckets, bucket[m])
			}
			if j := judgeOf[m]; j != "" && !slices.Contains(g.Judge, j) {
				g.Judge = append(g.Judge, j)
			}
		}
		switch {
		case len(buckets) > 1:
			g.Class = sessionjudge.CompareSplit
		case len(buckets) == 1 && len(owners[buckets[0]]) > 1:
			g.Class = sessionjudge.CompareMerged
		case len(buckets) == 1:
			g.Class = sessionjudge.CompareAgree
		case !answered[id]:
			unanswered++
			g.Class = sessionjudge.CompareUnknown
		case rejected[id]:
			// A target was put in it with nothing that shows it.
			g.Class = sessionjudge.CompareUnknown
		case worked[id] && missing == 0:
			g.Class = sessionjudge.CompareMissed
		case claimed[id]:
			// Worked on, and a target left out may serve it; or worked on,
			// the answer says, with nothing that shows it.
			g.Class = sessionjudge.CompareUnknown
		default:
			g.Class = sessionjudge.CompareAbsent
		}
		g.Main = mainOf(g.Members, status, judgeGoals)
		agreement.Classes[g.Class]++
		c.frames = append(c.frames, g)
	}
	for _, t := range targets {
		c.frames = append(c.frames, placed[t.ID])
	}
	c.frames = append(c.frames, agreement)
	if left > 0 {
		c.problem("%d entries for no target or goal of this regroup, or for one answered already, were left out", left)
	}
	if missing > 0 {
		c.problem("%d targets were not placed and are unknown", missing)
	}
	if unanswered > 0 {
		c.problem("%d of the person's goals were not answered and are unknown", unanswered)
	}
	if unshown > 0 {
		c.problem("%d placements, or goals worked on, that no input of the person shows were read as unknown", unshown)
	}
	if dropped > 0 {
		c.problem("%d citations of nodes that do not exist were left out", dropped)
	}
	n, done := len(targets), len(targets)-len(unknown)
	c.coverage = sessionjudge.Coverage{Targets: &n, TargetsJudged: &done}
	return c, nil
}

// mainOf is the main target of one of the person's goals: its only member
// still wanted, or the main target of a goal of the judge that holds the
// same targets, since the person's words that made it main were about those
// targets alone.
func mainOf(members []string, status map[string]string, judgeGoals []sessionjudge.Goal) string {
	var wanted []string
	for _, m := range members {
		if status[m] != sessionjudge.TargetCancelled && status[m] != sessionjudge.TargetReplaced {
			wanted = append(wanted, m)
		}
	}
	if len(wanted) == 1 {
		return wanted[0]
	}
	for _, g := range judgeGoals {
		same := len(g.Members) == len(members) && !slices.ContainsFunc(members, func(m string) bool { return !slices.Contains(g.Members, m) })
		if same && slices.Contains(wanted, g.Main) {
			return g.Main
		}
	}
	return ""
}

// windowOf is the window a run's header records, as the run was given it.
func windowOf(h *sessionjudge.Window) (Window, error) {
	var w Window
	if h == nil {
		return w, nil
	}
	w.FromTalk, w.ToTalk = h.FromTalk, h.ToTalk
	for _, t := range []struct {
		text string
		into *time.Time
	}{{h.From, &w.From}, {h.To, &w.To}} {
		if t.text == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339, t.text)
		if err != nil {
			return w, err
		}
		*t.into = at
	}
	return w, nil
}

// Regroup compares a closed run's grouping with a person's goals, in a
// versioned file beside the run's, whose files are never changed. One call
// of evaluator places each of the run's targets in the goal it serves,
// reading the conversation as the run did: at its head, through its window.
// It is not shown the judge's goals, so the comparison measures them. The
// file keeps the label file's text first, so the comparison can be made
// again from it alone; a regroup whose call failed is kept too, failed, with
// what the call consumed.
func (r *Runner) Regroup(ctx context.Context, conversation string, run storage.JudgmentRun, labels *Labels, evaluator string) (string, *sessionjudge.Agreement, error) {
	if !run.Closed() {
		return "", nil, fmt.Errorf("run %d is not closed", run.Run)
	}
	// The targets stage and the ledger, whose rates price the calls as the
	// run priced them.
	var targets, ledgerFile *sessionjudge.File
	var ledgerPath string
	for _, f := range run.Files {
		if f.Stage != sessionjudge.StageLedger && f.Stage != sessionjudge.StageTargets {
			continue
		}
		file, err := os.Open(f.Path)
		if err != nil {
			return "", nil, err
		}
		sj, err := sessionjudge.Read(file)
		file.Close()
		if err != nil {
			return "", nil, fmt.Errorf("%s: %w", f.Path, err)
		}
		if f.Stage == sessionjudge.StageLedger {
			ledgerFile, ledgerPath = sj, f.Path
		} else {
			targets = sj
		}
	}
	// A run whose targets stage found none can still be regrouped: a goal
	// the conversation worked on is then missed.
	switch {
	case targets == nil || ledgerFile == nil:
		return "", nil, fmt.Errorf("run %d has no targets stage or no ledger to regroup", run.Run)
	case targets.Header.Status == sessionjudge.StatusFailed:
		return "", nil, fmt.Errorf("run %d found no targets: its targets stage failed", run.Run)
	}
	pricing, err := PricingOf(ledgerFile)
	if err != nil {
		return "", nil, err
	}
	conf, ok := r.Config.Evaluators[evaluator]
	if !ok {
		return "", nil, fmt.Errorf("judge: the regroup names evaluator %q, which judge.evaluators does not list", evaluator)
	}
	newExec := NewExecutor
	if r.Executor != nil {
		newExec = r.Executor
	}
	ex, err := newExec(evaluator, conf)
	if err != nil {
		return "", nil, err
	}
	timeout, err := r.Config.TimeoutDuration()
	if err != nil {
		return "", nil, err
	}
	kit, err := LoadKit()
	if err != nil {
		return "", nil, err
	}
	_, promptDigest, err := kit.Prompt(sessionjudge.StageRegroup, "")
	if err != nil {
		return "", nil, err
	}
	schema, err := kit.Schema(sessionjudge.StageRegroup)
	if err != nil {
		return "", nil, err
	}

	// The conversation as the run read it.
	t := targets.Header
	window, err := windowOf(t.Window)
	if err != nil {
		return "", nil, err
	}
	c, err := view.New(r.Zone, nil).LoadAt(conversation, t.Head.Round)
	if err != nil {
		return "", nil, err
	}
	doc, err := c.BuildWithheld(r.Config.Hide)
	if err != nil {
		return "", nil, err
	}
	if doc.Head.Digest != t.Head.Digest {
		return "", nil, fmt.Errorf("judge: round %d of %s is not the head run %d read", t.Head.Round, conversation, run.Run)
	}
	doc, sel, err := Narrow(doc, window, c.Records)
	if err != nil {
		return "", nil, err
	}
	dialect := r.Dialect(c.Session)
	ledger := Compute(doc, dialect, pricing, c.Records)
	ledger.Select(sel)
	nav := NewNav(doc, ledger, func(seq, row uint64) (*sessiondata.Record, error) { return c.Record(seq, row, r.Config.Hide) })
	var decode PathDecoder
	if r.Decoder != nil {
		decode = r.Decoder(dialect)
	}
	nav.Prepare(sel, c.Records, decode)
	var inputs []*sessionjudge.Input
	for i := range targets.Inputs {
		inputs = append(inputs, &targets.Inputs[i])
	}
	// An input the targets stage found was not the person's shows nothing
	// of what the person wanted.
	nav.TargetsFound(inputs)

	version, err := storage.ReserveRegroup(r.Zone, t.Session, run.Dir)
	if err != nil {
		return "", nil, err
	}
	mode, chunks := planChunks(sessionjudge.StageRegroup, r.Config.Mode, r.Config.SingleBytes, r.Config.ChunkBytes, r.Config.ReadBytes, nav, targets.Targets)
	r.logf("  regroup %d of run %d by %s, %d calls ...", version, run.Run, evaluator, len(chunks))
	started := r.now()
	sr := &stageRun{runner: r, kit: kit, ex: ex, nav: nav, stage: sessionjudge.StageRegroup, work: fmt.Sprintf("g%d-regroup", version),
		session: t.Session, dir: run.Dir, schema: schema, targets: targets.Targets, goals: targets.Goals, inputs: inputs, personGoals: labels.goals,
		timeout: timeout, mcp: MCPArgs{Conversation: conversation, Head: t.Head, Ledger: ledgerPath, Window: window, ReadBytes: r.Config.ReadBytes}}
	m, _, used, err := sr.run(ctx, chunks)
	if err != nil {
		return "", nil, err
	}
	h := sessionjudge.Header{Conversation: t.Conversation, Session: t.Session, Run: t.Run, Stage: sessionjudge.StageRegroup, Head: t.Head,
		Parser: t.Parser, Policy: t.Policy, Window: t.Window, BuildsOn: []sessionjudge.Dependency{{Stage: sessionjudge.StageTargets, Digest: targets.Commit.Digest}},
		Evaluator: ex.Describe(), Kit: &sessionjudge.Kit{Version: kit.Version(), Prompt: promptDigest},
		Labels: &sessionjudge.LabelFile{File: labels.File, SHA256: labels.SHA256, Version: version}, Mode: mode, Chunks: len(chunks),
		Started: stamp(started), Finished: stamp(r.now())}
	h.Usage, h.ReportedCostUSD, h.Reading, h.Attempts = used.usage, used.costUSD, used.reading, used.attempts
	if used.attempts > 0 {
		rq := used.requests
		h.Requests = &rq
	}
	if used.model != "" {
		h.Evaluator.Model = used.model
	}
	cov := m.coverage
	h.Coverage = &cov
	var frames []any
	for _, piece := range textRuns(labels.text) {
		frames = append(frames, &sessionjudge.LabelText{From: piece.from, Text: piece.text})
	}
	var agreement *sessionjudge.Agreement
	if m.failed == len(chunks) {
		h.Status, h.Note = sessionjudge.StatusFailed, clip(strings.Join(m.problems, "; "), 500)
	} else {
		h.Status = sessionjudge.StatusComplete
		if len(m.problems) > 0 {
			h.Status, h.Note = sessionjudge.StatusPartial, clip(strings.Join(m.problems, "; "), 1000)
		}
		frames = append(frames, m.frames...)
		agreement, _ = frames[len(frames)-1].(*sessionjudge.Agreement)
	}
	w, err := sessionjudge.NewWriter(h)
	if err != nil {
		return "", nil, err
	}
	for _, f := range frames {
		if err := w.Write(f); err != nil {
			return "", nil, err
		}
	}
	data, digest, err := w.Close()
	if err != nil {
		return "", nil, err
	}
	path, err := storage.PublishRegroup(r.Zone, t.Session, run.Dir, version, digest, data)
	if err != nil {
		return "", nil, err
	}
	if agreement == nil {
		return path, nil, fmt.Errorf("regroup %d of run %d failed: %s", version, run.Run, h.Note)
	}
	return path, agreement, nil
}
