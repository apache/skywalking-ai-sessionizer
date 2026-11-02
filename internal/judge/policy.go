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
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// Facts is what asz server's last pass established of one session.
type Facts struct {
	// Outcome is whether every source of the session was read to its end.
	Outcome storage.Outcome
	// Round and Digest are the chain head the pass parsed the conversation
	// to the end at, Digest as a round's name carries it, and Landed the
	// session's highest landed sequence then.
	Round  uint64
	Digest string
	Landed uint64
}

// Policy is asz server's judge policy over one production root: after each
// pass it judges, one at a time, a conversation nothing has been collected
// for since judge.server.idle_after, from the first main talk whose view
// changed since the last run that judged it.
type Policy struct {
	Zone   *storage.Zone
	Config config.Judge
	// NewRunner makes the runner of one automatic run, its evaluators and
	// its navigation wired.
	NewRunner func() *Runner
	// Facts are the server's last pass's facts of every session.
	Facts func() map[string]Facts
	// Origin says whether a session worked inside the storage root:
	// inside, outside or unknown. Only outside is judged. OriginNow, when
	// set, says the same from the live source first, and the admission asks
	// it, under the root lock.
	Origin    func(session string) (string, error)
	OriginNow func(session string) (string, error)
	Now       func() time.Time
	Log       func(format string, args ...any)

	mu sync.Mutex
	// memo keeps each conversation's window, which costs a fold to find,
	// until its head, its newest receipt or the scope moves.
	memo    map[string]memo
	stopped bool
	cancel  context.CancelFunc
	done    chan struct{}
	auto    *storage.SessionLock
	status  PolicyStatus
	since   time.Time
}

// PolicyStatus is what the page says of the policy.
type PolicyStatus struct {
	Enabled bool `json:"enabled"`
	// Since is the start of the policy's scope, unix milliseconds.
	Since   int64       `json:"since,omitempty"`
	Running *RunningRun `json:"running,omitempty"`
	// Due counts the conversations due at the last look.
	Due int `json:"due"`
	// Admitted counts the automatic runs admitted in the last 24 hours.
	Admitted int `json:"admitted_24h"`
	// Skipped are the talks left out for being larger than judge.max_steps
	// alone, as conversation/talk.
	Skipped []string `json:"skipped,omitempty"`
	// LeftOut counts the conversations not due, by the first rule each
	// failed.
	LeftOut   map[string]int `json:"left_out,omitempty"`
	LastError string         `json:"last_error,omitempty"`
}

// RunningRun is the automatic run going.
type RunningRun struct {
	Conversation string `json:"conversation"`
	Run          int    `json:"run"`
	// Stage is the stage being judged.
	Stage    string `json:"stage,omitempty"`
	Started  int64  `json:"started"`
	FromTalk string `json:"from_talk"`
	ToTalk   string `json:"to_talk"`
	Talks    int    `json:"talks"`
}

// Status is a copy of the policy's status.
func (p *Policy) Status() PolicyStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.status
	// What a request serializes outside the lock is its own copy.
	if st.Running != nil {
		running := *st.Running
		st.Running = &running
	}
	st.Skipped = slices.Clone(st.Skipped)
	st.LeftOut = maps.Clone(st.LeftOut)
	st.Enabled = true
	if !p.since.IsZero() {
		st.Since = p.since.UnixMilli()
	}
	return st
}

func (p *Policy) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Policy) logf(format string, args ...any) {
	if p.Log != nil {
		p.Log(format, args...)
	}
}

// Halt admits nothing more, at once: a run waiting for its admission is
// refused. The run going goes on until Stop.
func (p *Policy) Halt() {
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
}

// Stop admits nothing more, ends the work of the run going and waits for its
// cleanup, which is bounded. A pass still running is not waited for here.
func (p *Policy) Stop() {
	p.mu.Lock()
	p.stopped = true
	cancel, done := p.cancel, p.done
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// Wait waits for the run going, if one is, to end.
func (p *Policy) Wait() {
	p.mu.Lock()
	done := p.done
	p.mu.Unlock()
	if done != nil {
		<-done
	}
}

// candidate is a conversation due, and the window its next run takes.
type candidate struct {
	conversation, session string
	facts                 Facts
	collected             time.Time
	newest                int
	parser, policy        string
	talks                 []storage.ReceiptTalk
	steps                 int
	target                int64
	// skip is a first due talk larger than judge.max_steps alone.
	skip bool
}

// Reasons a conversation is not due, as the status counts them.
const (
	leftOutcome   = "collection not complete"
	leftParse     = "parse not complete"
	leftOrigin    = "worked inside the storage root, or not known"
	leftQuiet     = "not quiet yet"
	leftSkeleton  = "files removed by retention"
	leftVerified  = "chain not verified"
	leftRetry     = "the last run did not succeed, and nothing new landed"
	leftNothing   = "nothing new to judge"
	leftTrimmed   = "the window would trim selected evidence"
	leftActive    = "a run or regroup is active"
	leftReadError = "its records do not read"
)

// Tick looks for a conversation due and starts its run, unless one is
// going, the day's allowance is spent or the policy was stopped. It returns
// once the run is started; the run goes on under ctx.
func (p *Policy) Tick(ctx context.Context) {
	p.mu.Lock()
	busy := p.stopped || p.done != nil
	p.mu.Unlock()
	if busy {
		return
	}
	now := p.now()
	fail := func(err error) {
		p.mu.Lock()
		p.status.LastError = err.Error()
		p.mu.Unlock()
		p.logf("judge    : %v", err)
	}
	since, err := p.scopeStart(ctx, now)
	if err != nil {
		fail(err)
		return
	}
	admitted, err := storage.RunReceiptsSince(p.Zone, now.Add(-24*time.Hour))
	if err != nil {
		fail(err)
		return
	}
	cands, leftOut, err := p.candidates(p.Facts(), since, now)
	skipped, serr := skippedTalks(p.Zone)
	p.mu.Lock()
	p.since, p.status.Due, p.status.LeftOut, p.status.Admitted = since, len(cands), leftOut, admitted
	p.status.Skipped, p.status.LastError = skipped, ""
	p.mu.Unlock()
	if err == nil {
		err = serr
	}
	if err != nil {
		fail(err)
		return
	}
	for _, c := range cands {
		if c.skip {
			if err := p.writeSkip(ctx, c, now); err != nil {
				fail(err)
				return
			}
			continue
		}
		if admitted >= p.Config.Server.RunsPerDay {
			return
		}
		p.start(c, now)
		return
	}
}

// skippedTalks are the talks every skip receipt of the root names, as
// conversation/talk: they stay skipped across a restart, until their views
// change.
func skippedTalks(z *storage.Zone) ([]string, error) {
	all, err := storage.AllReceipts(z)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range all {
		if r.Kind == storage.ReceiptSkip {
			for _, t := range r.Talks {
				out = append(out, r.Conversation+"/"+t.ID)
			}
		}
	}
	return out, nil
}

// scopeStart is judge.server.since, or the moment the policy was first
// turned on for the root, written once.
func (p *Policy) scopeStart(ctx context.Context, now time.Time) (time.Time, error) {
	if t, err := p.Config.Server.SinceTime(); err != nil || !t.IsZero() {
		return t, err
	}
	if t, ok, err := storage.LoadSince(p.Zone); err != nil || ok {
		return t, err
	}
	l, err := storage.LockRootContext(ctx, p.Zone.Root())
	if err != nil {
		return time.Time{}, err
	}
	defer func() { _ = l.Unlock() }()
	if t, ok, err := storage.LoadSince(p.Zone); err != nil || ok {
		return t, err
	}
	return now, storage.SaveSince(p.Zone, now)
}

// candidates lists the conversations due, the newest collected first.
func (p *Policy) candidates(facts map[string]Facts, since, now time.Time) ([]candidate, map[string]int, error) {
	idle, err := p.Config.Server.IdleAfterDuration()
	if err != nil {
		return nil, nil, err
	}
	leftOut := map[string]int{}
	var out []candidate
	ids := make([]string, 0, len(facts))
	for id := range facts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		c, why, err := p.consider(id, facts[id], since, now, idle)
		if err != nil {
			return nil, nil, err
		}
		if why != "" {
			leftOut[why]++
			continue
		}
		out = append(out, *c)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].collected.After(out[j].collected) })
	return out, leftOut, nil
}

// consider decides whether one conversation is due, and its window.
func (p *Policy) consider(id string, f Facts, since, now time.Time, idle time.Duration) (*candidate, string, error) {
	if f.Outcome != storage.OutcomeComplete {
		return nil, leftOutcome, nil
	}
	if f.Round == 0 {
		return nil, leftParse, nil
	}
	if origin, err := p.Origin(id); err != nil || origin != "outside" {
		return nil, leftOrigin, nil
	}
	collected, landed, err := storage.Collected(p.Zone, id)
	if err != nil {
		return nil, leftParse, nil
	}
	if collected.After(now) || now.Sub(collected) < idle {
		return nil, leftQuiet, nil
	}
	if landed != f.Landed {
		return nil, leftParse, nil
	}
	if same, err := p.headIs(id, f); err != nil || !same {
		return nil, leftParse, nil
	}
	files, err := storage.LandedFiles(p.Zone, id)
	if err != nil {
		return nil, leftParse, nil
	}
	for _, lf := range files {
		if lf.Skeleton {
			return nil, leftSkeleton, nil
		}
	}
	if active, err := storage.JudgeActive(p.Zone, id); err != nil || active {
		return nil, leftActive, nil
	}
	receipts, err := storage.Receipts(p.Zone, id)
	if err != nil {
		return nil, "", err
	}
	newest := 0
	if n := len(receipts); n > 0 {
		last := receipts[n-1]
		newest = last.Number
		ok, err := p.succeeded(last)
		if err != nil {
			return nil, "", err
		}
		if !ok && f.Landed <= last.Landed {
			return nil, leftRetry, nil
		}
	}
	key := fmt.Sprintf("%d %s %d %d %d %d", f.Round, f.Digest, f.Landed, newest, since.UnixMilli(), p.Config.MaxSteps)
	p.mu.Lock()
	m, ok := p.memo[id]
	p.mu.Unlock()
	if !ok || m.key != key {
		m = memo{key: key}
		m.c, m.why, err = p.find(id, f, receipts, newest, since)
		if err != nil {
			return nil, "", err
		}
		// A read that failed may pass on the next look, so it is not kept.
		if m.why != leftReadError {
			p.mu.Lock()
			if p.memo == nil {
				p.memo = map[string]memo{}
			}
			p.memo[id] = m
			p.mu.Unlock()
		}
	}
	if m.why != "" {
		return nil, m.why, nil
	}
	c := *m.c
	c.collected = collected
	return &c, "", nil
}

// find folds the conversation at the head the pass recorded and finds the
// window its next run takes.
func (p *Policy) find(id string, f Facts, receipts []storage.Receipt, newest int, since time.Time) (*candidate, string, error) {
	conv, err := view.New(p.Zone, nil).LoadAt(id, f.Round)
	if err != nil {
		return nil, leftReadError, nil
	}
	doc, err := conv.Build()
	if err != nil {
		return nil, leftReadError, nil
	}
	if doc.Summary.State != sessionview.StateVerified {
		return nil, leftVerified, nil
	}
	c := &candidate{conversation: id, session: conv.Session, facts: f, newest: newest, parser: doc.Parser, policy: doc.Policy}
	why, err := p.window(c, conv, doc, receipts, since)
	if err != nil || why != "" {
		return nil, why, err
	}
	return c, "", nil
}

// headIs says whether the conversation's chain head is still the one the
// pass recorded.
func (p *Policy) headIs(id string, f Facts) (bool, error) {
	files, err := sessionflow.OpenChain(p.Zone.Root(), id).List()
	if err != nil || len(files) == 0 {
		return false, err
	}
	last := files[len(files)-1]
	return last.Round == f.Round && last.Digest == f.Digest, nil
}

// memo is a conversation's window as last found, and what it was found at.
type memo struct {
	key string
	c   *candidate
	why string
}

// cover is what the newest successful receipt of a talk recorded.
type cover struct {
	view, parser, policy string
	landed               uint64
}

// window finds the conversation's due talks and the window its next run
// takes, into c, or says why nothing is due.
func (p *Policy) window(c *candidate, conv *view.Conversation, doc *sessionview.Conversation, receipts []storage.Receipt, since time.Time) (string, error) {
	covered := map[string]cover{}
	for _, r := range receipts {
		ok, err := p.succeeded(r)
		if err != nil {
			return "", err
		}
		if !ok {
			continue
		}
		for _, t := range r.Talks {
			covered[t.ID] = cover{view: t.View, parser: r.Parser, policy: r.Policy, landed: r.Landed}
		}
	}
	views := TalkViews(doc)
	evidence := ownEvidence(doc)
	l := Compute(doc, "", nil, nil)
	steps := map[string]int{}
	for _, w := range TalkSteps(doc) {
		steps[w.Talk] = w.Steps
	}
	ends := workEnds(doc, l, conv.Records)
	due := func(t string) bool {
		if ends[t] < since.UnixMilli() {
			return false
		}
		cv, ok := covered[t]
		switch {
		case !ok:
			return true
		case cv.parser == doc.Parser && cv.policy == doc.Policy:
			return cv.view != views[t]
		}
		return evidence[t] > cv.landed
	}
	first, last := -1, -1
	for i, t := range l.Main {
		if due(t) {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		return leftNothing, nil
	}
	limit := p.Config.MaxSteps
	if steps[l.Main[first]] > limit {
		c.skip = true
		c.talks = []storage.ReceiptTalk{{ID: l.Main[first], View: views[l.Main[first]]}}
		c.steps = steps[l.Main[first]]
		return "", nil
	}
	end, total := first, 0
	for i := first; i <= last; i++ {
		if total+steps[l.Main[i]] > limit {
			break
		}
		total += steps[l.Main[i]]
		end = i
	}
	// Nothing selected may be trimmed: each selected talk's view in the
	// narrowed document, built before withholding as the head's, is the
	// one the receipt records.
	narrowed, sel, err := Narrow(doc, Window{FromTalk: l.Main[first], ToTalk: l.Main[end]}, conv.Records)
	if err != nil {
		return leftReadError, nil
	}
	shown := TalkViews(narrowed)
	for _, t := range l.Main[first : end+1] {
		if shown[t] != views[t] {
			return leftTrimmed, nil
		}
		c.talks = append(c.talks, storage.ReceiptTalk{ID: t, View: views[t]})
	}
	c.steps, c.target = total, sel.End
	return "", nil
}

// workEnds is the last record time of each main talk's work: its span, with
// its sub-tasks, and the change and execution records joined to its steps,
// which a span leaves out.
func workEnds(doc *sessionview.Conversation, l *Ledger, read RecordsReader) map[string]int64 {
	out := map[string]int64{}
	for _, id := range l.Main {
		if inc, ok := l.Inclusive(id); ok {
			out[id] = inc.To
		}
	}
	owner := map[string]string{}
	for _, st := range l.Subtasks {
		owner[st.Name] = st.Main
	}
	stepOwner := map[string]string{}
	for i := range doc.Talks {
		t := &doc.Talks[i]
		main := t.ID
		if t.Stream != "main" {
			main = owner[t.Stream]
		}
		if main != "" {
			walkAll(t, func(n *sessionview.Node) { stepOwner[n.ID] = main })
		}
	}
	refOwner := map[[2]uint64]string{}
	var refs []sessionflow.Ref
	for _, c := range doc.WorkspaceChanges {
		if main := stepOwner[c.Step]; main != "" {
			refs, refOwner[[2]uint64{c.Ref.Seq, c.Ref.Row}] = append(refs, c.Ref), main
		}
	}
	for _, e := range doc.ToolExecutions {
		if main := stepOwner[e.Step]; main != "" {
			refs, refOwner[[2]uint64{e.Ref.Seq, e.Ref.Row}] = append(refs, e.Ref), main
		}
	}
	if read == nil || len(refs) == 0 {
		return out
	}
	for key, rec := range read(refs) {
		if rec == nil || rec.Time == "" {
			continue
		}
		if at, err := time.Parse(time.RFC3339Nano, rec.Time); err == nil && at.UnixMilli() > out[refOwner[key]] {
			out[refOwner[key]] = at.UnixMilli()
		}
	}
	return out
}

// ownEvidence is the highest landed sequence among the records each main
// talk's view stands on, provider bodies left out: across an upgrade of the
// chain's interpretation, a talk is due again only when a record of its own
// lands, never for a body alone.
func ownEvidence(doc *sessionview.Conversation) map[string]uint64 {
	bodies := map[uint64]bool{}
	for _, f := range doc.Files {
		if f.Seq != nil && f.Kind == string(sessiondata.KindProviderBody) {
			bodies[*f.Seq] = true
		}
	}
	l := Compute(doc, "", nil, nil)
	owner, ofStream := map[string]string{}, map[string]string{}
	for _, st := range l.Subtasks {
		if st.Main != "" {
			owner[st.Name], ofStream[st.ID] = st.Main, st.Main
		}
	}
	out := map[string]uint64{}
	add := func(main string, r sessionflow.Ref) {
		if !bodies[r.Seq] && r.Seq > out[main] {
			out[main] = r.Seq
		}
	}
	stepOwner := map[string]string{}
	for i := range doc.Talks {
		t := &doc.Talks[i]
		main := t.ID
		if t.Stream != "main" {
			main = owner[t.Stream]
		}
		if main == "" {
			continue
		}
		walkAll(t, func(n *sessionview.Node) {
			stepOwner[n.ID] = main
			if n.Ref != nil {
				add(main, *n.Ref)
			}
			for _, r := range n.Refs {
				add(main, r)
			}
		})
	}
	for _, c := range doc.WorkspaceChanges {
		if main := stepOwner[c.Step]; main != "" {
			add(main, c.Ref)
		}
	}
	for _, e := range doc.ToolExecutions {
		if main := stepOwner[e.Step]; main != "" {
			add(main, e.Ref)
		}
	}
	for _, r := range doc.Relations {
		if main := ofStream[r.To]; main != "" && r.Type == model.RelStarts {
			for _, ev := range r.Evidence {
				add(main, ev)
			}
		}
	}
	return out
}

// succeeded says whether a receipt's attempt covers its talks: a skip does;
// a run does only through the run directory bound to it, closed complete.
func (p *Policy) succeeded(r storage.Receipt) (bool, error) {
	if r.Kind == storage.ReceiptSkip {
		return true, nil
	}
	dir := filepath.Join(p.Zone.SessionDir(r.Session), storage.JudgmentsDir, fmt.Sprintf("j%06d", r.Run))
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if ok, err := storage.ReceiptRunMatches(dir, r); err != nil || !ok {
		return false, err
	}
	run := storage.JudgmentRun{Run: r.Run, Dir: dir}
	state, _, err := storage.StateOf(&run)
	if err != nil || state != storage.RunClosed {
		return false, err
	}
	for _, f := range run.Files {
		if f.Stage == sessionjudge.StageClose {
			in, err := os.Open(f.Path)
			if err != nil {
				return false, err
			}
			sj, err := sessionjudge.Read(in)
			in.Close()
			if err != nil {
				return false, nil
			}
			return sj.Header.Status == sessionjudge.StatusComplete, nil
		}
	}
	return false, nil
}

// errNotDue refuses an admission whose conversation changed since the list.
var errNotDue = errors.New("judge: the conversation is no longer due as listed")

// recheck asks again, under the root lock, what the list decided from: the
// head and the newest landed file, the newest receipt, and that the root is
// a production one.
func (p *Policy) recheck(c candidate) error {
	if _, err := os.Stat(filepath.Join(p.Zone.Root(), storage.ScenarioDir)); err == nil {
		return errors.New("judge: a scenario root is not judged automatically")
	}
	// What the pass established may have changed since the list: a line
	// found unfinished, a session found inside the root.
	if f, ok := p.Facts()[c.conversation]; !ok || f != c.facts {
		return errNotDue
	}
	originNow := p.OriginNow
	if originNow == nil {
		originNow = p.Origin
	}
	if origin, err := originNow(c.session); err != nil || origin != "outside" {
		return errors.Join(errNotDue, err)
	}
	if same, err := p.headIs(c.conversation, c.facts); err != nil || !same {
		return errors.Join(errNotDue, err)
	}
	if _, landed, err := storage.Collected(p.Zone, c.session); err != nil || landed != c.facts.Landed {
		return errors.Join(errNotDue, err)
	}
	files, err := storage.LandedFiles(p.Zone, c.session)
	if err != nil {
		return err
	}
	for _, lf := range files {
		if lf.Skeleton {
			return errNotDue
		}
	}
	receipts, err := storage.Receipts(p.Zone, c.conversation)
	if err != nil {
		return err
	}
	newest := 0
	if n := len(receipts); n > 0 {
		newest = receipts[n-1].Number
	}
	if newest != c.newest {
		return errNotDue
	}
	return nil
}

// receipt is the receipt of an attempt on a candidate.
func (c candidate) receipt(kind string, now time.Time) *storage.Receipt {
	return &storage.Receipt{Kind: kind, Conversation: c.conversation, Session: c.session, Time: now.UTC().Format(time.RFC3339Nano),
		Round: c.facts.Round, Digest: c.facts.Digest, Landed: c.facts.Landed, Parser: c.parser, Policy: c.policy,
		Talks: c.talks, Target: c.target, Steps: c.steps}
}

// writeSkip writes a skip receipt for a first due talk larger than
// judge.max_steps alone: it covers the talk at its view, calls no model, and
// counts against nothing.
func (p *Policy) writeSkip(ctx context.Context, c candidate, now time.Time) error {
	l, err := storage.LockRootContext(ctx, p.Zone.Root())
	if err != nil {
		return err
	}
	defer func() { _ = l.Unlock() }()
	if err := p.recheck(c); err != nil {
		if errors.Is(err, errNotDue) {
			return nil
		}
		return err
	}
	if err := storage.WriteReceipt(p.Zone, c.receipt(storage.ReceiptSkip, now)); err != nil {
		return err
	}
	p.mu.Lock()
	p.status.Skipped = append(p.status.Skipped, c.conversation+"/"+c.talks[0].ID)
	p.mu.Unlock()
	p.logf("judge    : %s: talk %s has %d steps, more than judge.max_steps %d; skipped", c.conversation, c.talks[0].ID, c.steps, p.Config.MaxSteps)
	return nil
}

// start starts the run of a candidate. Its admission, under the root lock,
// checks again what the list decided, takes the automatic lock and writes the
// receipt before the run's directory is reserved.
//
// The run's context is its own, not the passes': a stop of the server lets
// the running pass finish first, then ends the run's work with Stop.
func (p *Policy) start(c candidate, now time.Time) {
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	p.mu.Lock()
	p.cancel, p.done = cancel, done
	p.status.Running = &RunningRun{Conversation: c.conversation, Started: now.UnixMilli(), FromTalk: c.talks[0].ID,
		ToTalk: c.talks[len(c.talks)-1].ID, Talks: len(c.talks)}
	p.mu.Unlock()
	r := p.NewRunner()
	r.Stage = func(stage string) {
		p.mu.Lock()
		if p.status.Running != nil {
			p.status.Running.Stage = stage
		}
		p.mu.Unlock()
	}
	r.Trigger = storage.TriggerServer
	r.Window = Window{FromTalk: c.talks[0].ID, ToTalk: c.talks[len(c.talks)-1].ID}
	r.Select = func(next int) (storage.RunRecord, error) {
		p.mu.Lock()
		halted := p.stopped
		p.mu.Unlock()
		if halted {
			return storage.RunRecord{}, errNotDue
		}
		if err := p.recheck(c); err != nil {
			return storage.RunRecord{}, err
		}
		// The clock is read at the admission, which can come long after the
		// list: the allowance and the receipt are both counted from it.
		at := p.now()
		if n, err := storage.RunReceiptsSince(p.Zone, at.Add(-24*time.Hour)); err != nil {
			return storage.RunRecord{}, err
		} else if n >= p.Config.Server.RunsPerDay {
			return storage.RunRecord{}, fmt.Errorf("judge: %d automatic runs in the last 24 hours, judge.server.runs_per_day", n)
		}
		lock, err := storage.TryLockAutomatic(p.Zone)
		if errors.Is(err, storage.ErrBusy) {
			return storage.RunRecord{}, errors.New("judge: another server's automatic run is going on this root")
		}
		if err != nil {
			return storage.RunRecord{}, err
		}
		rec := c.receipt(storage.ReceiptRun, at)
		rec.Run = next
		if err := storage.WriteReceipt(p.Zone, rec); err != nil {
			_ = lock.Unlock()
			return storage.RunRecord{}, err
		}
		p.mu.Lock()
		p.auto = lock
		if p.status.Running != nil {
			p.status.Running.Run = next
		}
		p.mu.Unlock()
		return storage.RunRecord{Receipt: rec.Number, Landed: c.facts.Landed, Target: c.target}, nil
	}
	p.logf("judge    : %s: talks %s to %s (%d), %d steps", c.conversation, c.talks[0].ID, c.talks[len(c.talks)-1].ID, len(c.talks), c.steps)
	go func() {
		defer close(done)
		run, err := r.Run(runCtx, c.conversation, c.facts.Round, p.Config.Plan)
		cancel()
		p.mu.Lock()
		if p.auto != nil {
			_ = p.auto.Unlock()
			p.auto = nil
		}
		p.cancel, p.done, p.status.Running = nil, nil, nil
		switch {
		case err != nil && !errors.Is(err, errNotDue):
			p.status.LastError = fmt.Sprintf("%s: %v", c.conversation, err)
		case err == nil:
			p.status.Admitted++
		}
		p.mu.Unlock()
		switch {
		case err != nil:
			p.logf("judge    : %s: %v", c.conversation, err)
		default:
			p.logf("judge    : %s: run %d closed %s", c.conversation, run.Number, run.Status)
		}
	}()
}

// CheckPolicy refuses a judge configuration asz server cannot judge with on
// its own, before the server starts: nobody watches a run it starts.
func CheckPolicy(j config.Judge) error {
	idle, err := j.Server.IdleAfterDuration()
	if err != nil {
		return err
	}
	if idle < config.MinIdleAfter {
		return fmt.Errorf("judge.server.idle_after %s is under %s", idle, config.MinIdleAfter)
	}
	if j.MaxSteps <= 0 {
		return errors.New("judge.server needs judge.max_steps set: nobody watches a run it starts")
	}
	if d, err := j.RunTimeoutDuration(); err != nil || d <= 0 {
		return errors.Join(errors.New("judge.server needs judge.run_timeout set: nobody watches a run it starts"), err)
	}
	if len(j.Plan) == 0 || j.Plan[0].Stage != sessionjudge.StageTargets {
		return errors.New("judge.plan starts with the targets stage")
	}
	for _, s := range j.Plan {
		if s.Stage == sessionjudge.StageReference || s.Stage == sessionjudge.StageDivergence {
			return fmt.Errorf("judge.plan has a %s stage, which needs a source asz server has none of", s.Stage)
		}
		e, ok := j.Evaluators[s.Evaluator]
		if !ok {
			return fmt.Errorf("judge.plan names evaluator %q, which judge.evaluators does not list", s.Evaluator)
		}
		if e.Program == "kiro-cli" {
			return fmt.Errorf("judge.plan gives %s to kiro-cli, which keeps every session it runs; asz server does not judge with it", s.Stage)
		}
	}
	return nil
}
