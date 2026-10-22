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
	"fmt"
	"math"
	"slices"
	"sort"

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
)

// closeFrames prices what the evaluators decided, from the ledger alone. An
// evaluator never gives a number: it names targets and labels work, and every
// token and every unit of money here comes from the calls the ledger counted.
//
// A talk serving one target is that target's. A talk serving several is cut
// at its inputs: each call goes to the targets the last input before it
// directed work to, and is split equally only between targets one input
// directed together. Talks no target claims, and the main stream's calls
// outside any talk, are unassigned. Child streams no single talk started stay
// unattributed, as the ledger left them.
//
// Work is priced by the most specific label: a sub-task's own calls by the
// work its sub-task stage decision names, a talk's own calls by efficiency,
// and a sub-task left unjudged as not reviewed, never by its parent's label.
// When no sub-task stage ran, a child's calls follow the label of the step
// that started it, as efficiency gave it.
func closeFrames(l *Ledger, targets []sessionjudge.Target, inputs []*sessionjudge.Input, achievements []*sessionjudge.Achievement,
	labels []*sessionjudge.Label, subtasks map[string]*sessionjudge.Subtask, judging *sessionjudge.Amount) ([]any, *sessionjudge.Summary) {
	var out []any
	sum := &sessionjudge.Summary{Targets: len(targets), Usage: l.Total.Usage, Money: l.Total.Money,
		Currency: l.Currency, Unpriced: l.Total.Unpriced, Judging: judging,
		Unknown: l.Total.Unknown, OutputUnknown: l.Total.OutputUnknown}
	judged := map[string]*sessionjudge.Achievement{}
	for _, a := range achievements {
		judged[a.Target] = a
		switch a.Level {
		case sessionjudge.LevelAchieved:
			sum.Achieved++
		case sessionjudge.LevelPartly:
			sum.Partly++
		case sessionjudge.LevelNotAchieved:
			sum.NotAchieved++
		case sessionjudge.LevelCannotVerify:
			sum.CannotVerify++
		}
	}
	claims := map[string][]string{}
	for _, t := range targets {
		for _, tk := range t.Talks {
			claims[tk] = append(claims[tk], t.ID)
		}
	}
	who := ownersOfCalls(l, claims, inputs)
	var work map[string]*targetWork
	if labels != nil || subtasks != nil {
		work = labelCalls(l, targets, who, labels, subtasks)
	}
	var score scoreSum
	// The run's avoidable usage is the sum of the targets', in the same
	// whole tokens, so the two always agree.
	var avoidable, bySubtasks, causeUnknown sessiondata.Usage
	for _, t := range targets {
		var exclusive []int
		shared := map[[2]string][]int{}
		var sharedKeys [][2]string
		var spans [][2]int64
		var mine []int
		for _, tk := range t.Talks {
			spans = append(spans, l.TalkSpans(tk)...)
			for _, i := range l.CallsOf(tk) {
				ws := who[i]
				if !slices.Contains(ws, t.ID) {
					continue
				}
				mine = append(mine, i)
				if len(ws) == 1 {
					exclusive = append(exclusive, i)
					continue
				}
				k := [2]string{tk, fmt.Sprint(len(ws))}
				if _, ok := shared[k]; !ok {
					sharedKeys = append(sharedKeys, k)
				}
				shared[k] = append(shared[k], i)
			}
		}
		o := &sessionjudge.Outcome{Target: t.ID, Statement: t.Statement, Status: t.Status, Span: union(spans),
			Cost: amountFor(l, mine, who, t.ID)}
		if a := judged[t.ID]; a != nil {
			met, unknown, n := 0, 0, len(a.Criteria)
			for _, k := range a.Criteria {
				switch k.State {
				case sessionjudge.CheckMet:
					met++
				case sessionjudge.CheckUnknown, sessionjudge.CheckNotCaptured:
					unknown++
				}
			}
			contradicted := 0
			for _, k := range a.Claims {
				if k.State == sessionjudge.CheckContradicted {
					contradicted++
				}
			}
			o.Level, o.Basis, o.Met, o.Criteria, o.Unknown, o.Contradicted = a.Level, a.Basis, &met, &n, &unknown, &contradicted
		}
		if w := work[t.ID]; w != nil {
			av, nr := amountFor(l, w.avoidable, who, t.ID), amountFor(l, w.notReviewed, who, t.ID)
			bs, cu := amountFor(l, w.bySubtasks, who, t.ID), amountFor(l, w.causeUnknown, who, t.ID)
			add(&avoidable, av.Usage)
			add(&bySubtasks, bs.Usage)
			add(&causeUnknown, cu.Usage)
			span := union(w.avoidableSpans)
			o.Avoidable, o.NotReviewed, o.AvoidableSpan = &av, &nr, &span
			o.AvoidableBySubtasks, o.AvoidableCauseUnknown = &bs, &cu
		}
		out = append(out, o)
		// The scores are taken from the exact shares, before any split into
		// whole tokens: a token split three ways is a third in each.
		share := func(i int) float64 { return 1 / float64(len(who[i])) }
		ex := exactAmounts{cost: exactOf(l, mine, share)}
		if w := work[t.ID]; w != nil {
			ex.avoidable, ex.unreviewed = exactOf(l, w.avoidable, share), exactOf(l, w.notReviewed, share)
		}
		score.add(o, ex)
		c := l.Sum(sessionjudge.ScopeTarget, t.ID, exclusive)
		c.Target = t.ID
		out = append(out, &c)
		for _, k := range sharedKeys {
			calls := shared[k]
			s := 1 / float64(len(who[calls[0]]))
			sc := l.Sum(sessionjudge.ScopeShared, k[0], calls)
			sc.Target, sc.Share = t.ID, &s
			out = append(out, &sc)
		}
	}
	var unassigned []int
	for _, tk := range l.Judged() {
		for _, i := range l.CallsOf(tk) {
			if len(who[i]) == 0 {
				unassigned = append(unassigned, i)
			}
		}
	}
	unassigned = append(unassigned, l.callsOf[LooseOwner("main")]...)
	ua := l.Sum(sessionjudge.ScopeUnassigned, "", unassigned)
	out = append(out, &ua)
	if l.Unattributed.Calls > 0 {
		u := l.Unattributed
		out = append(out, &u)
	}
	// Each sub-task a judged main talk gave, with every sub-task under it: a
	// view over the calls counted above, never added to a total.
	for i := range l.Subtasks {
		if st := &l.Subtasks[i]; st.Main != "" && l.JudgesSubtask(st) {
			c := l.Sum(sessionjudge.ScopeSubtask, st.ID, l.SubtaskCalls(st.ID, true))
			if d, ok := subtasks[st.ID]; ok {
				c.Work = d.Work
			}
			out = append(out, &c)
		}
	}
	sum.Score = score.score(work != nil)
	if work == nil {
		return out, sum
	}
	var frames []any
	for _, t := range targets {
		w := work[t.ID]
		for _, g := range w.groups {
			c := l.Sum(sessionjudge.ScopeWork, g.talk, g.calls)
			c.Target, c.Work, c.Cause = t.ID, g.work, g.cause
			share := 1.0
			if g.talk != "" {
				share = 1 / float64(g.k)
				c.Share = &share
			}
			frames = append(frames, &c)
		}
	}
	out = append(out, frames...)
	sum.Avoidable, sum.AvoidableBySubtasks, sum.AvoidableCauseUnknown = &avoidable, &bySubtasks, &causeUnknown
	return out, sum
}

// directs says whether an input action sends work to its target. Accepting,
// cancelling, thanks and text from no person do not: the work after them goes
// on for whatever came before.
func directs(action string) bool {
	switch action {
	case sessionjudge.ActionCreate, sessionjudge.ActionRefine, sessionjudge.ActionSteer, sessionjudge.ActionCorrect:
		return true
	}
	return false
}

// ownersOfCalls gives each call of a claimed main talk the targets it served.
//
// One talk can hold several inputs, as when the person types a new request
// while the agent is still working. Measured on 772204d1: one talk served a
// question and a change, with 4 calls after the question and 14 after the
// change, and an equal split charged the question 2.8 times what it used. So
// a call goes to the targets the last input before it, in the talk's order,
// directed work to, among the targets that claim the talk. A call before any
// such input is split between every target of the talk. A child agent's
// calls take the place of the step that started it.
func ownersOfCalls(l *Ledger, claims map[string][]string, inputs []*sessionjudge.Input) map[int][]string {
	type cut struct {
		place   int
		targets []string
	}
	cuts := map[string][]cut{}
	for _, in := range inputs {
		claimed := claims[in.Talk]
		place, ok := l.StepPlace(in.Node)
		if !ok {
			continue
		}
		var ts []string
		for _, t := range claimed {
			for _, a := range in.Actions {
				if a.Target == t && directs(a.Action) && !slices.Contains(ts, t) {
					ts = append(ts, t)
				}
			}
		}
		if len(ts) > 0 {
			cuts[in.Talk] = append(cuts[in.Talk], cut{place, ts})
		}
	}
	who := map[int][]string{}
	for tk, claimed := range claims {
		cs := cuts[tk]
		sort.SliceStable(cs, func(i, j int) bool { return cs[i].place < cs[j].place })
		for _, i := range l.CallsOf(tk) {
			ts, place := claimed, l.Place(tk, i)
			for _, c := range cs {
				if c.place <= place {
					ts = c.targets
				}
			}
			who[i] = ts
		}
	}
	return who
}

// subtaskCalls names the sub-task each call of a sub-task's own stream is.
func subtaskCalls(l *Ledger) map[int]string {
	out := map[int]string{}
	for i := range l.Subtasks {
		for _, c := range l.Subtasks[i].own {
			out[c] = l.Subtasks[i].ID
		}
	}
	return out
}

// part is one labeled piece of a talk. It begins at its first step for the
// child agents' calls, which take the place of the step that started them,
// and at the model call that asked for that step for the talk's own calls:
// the call is the work of the part too.
type part struct {
	place, callPlace int
	work, subtask    string
}

// talkParts orders one target's labels of a talk: the label of the whole
// talk, empty when there is none, and the parts in the order their steps
// were recorded. A call that asked for the first steps of several parts
// belongs to the first of them.
func talkParts(l *Ledger, labels []*sessionjudge.Label) (part, []part) {
	whole := part{}
	var ps []part
	for _, p := range labels {
		if p.From == "" {
			whole = part{work: p.Label, subtask: p.Subtask}
			continue
		}
		place, ok := l.StepPlace(p.From)
		if !ok {
			continue
		}
		call, _ := l.PartPlace(p.From)
		ps = append(ps, part{place, call, p.Label, p.Subtask})
	}
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].place < ps[j].place })
	for i := 1; i < len(ps); i++ {
		if ps[i].callPlace <= ps[i-1].callPlace {
			ps[i].callPlace = ps[i].place
		}
	}
	return whole, ps
}

// partOf is the part that labels call i of a talk: the last part begun by
// the call's place, or the label of the whole talk before the first. Its
// work is empty when nothing labels the call.
func partOf(l *Ledger, talk string, i int, whole part, ps []part) part {
	lb, place := whole, l.Place(talk, i)
	own := l.Calls[i].Owner == talk
	for _, p := range ps {
		if place >= p.place || own && place >= p.callPlace {
			lb = p
		}
	}
	return lb
}

// targetWork is how one target's calls were labeled: the calls of each label,
// grouped as the close file writes them, and what was avoidable or not
// reviewed.
type targetWork struct {
	groups      []workGroup
	avoidable   []int
	notReviewed []int
	// bySubtasks are the avoidable calls a sub-agent caused, and
	// causeUnknown those whose cause the record does not show.
	bySubtasks, causeUnknown []int
	avoidableSpans           [][2]int64
}

// workGroup is the calls of one label of one target. A shared group names
// its talk and how many targets each of its calls is split between, and an
// avoidable group whose doing it was.
type workGroup struct {
	work, talk, cause string
	k                 int
	calls             []int
}

// labelCalls labels each call of each target's talks: by the label of the
// whole talk, or of the part of it from its first step on. A part runs to the
// next part; the talk's calls before its first part go to a label of the
// whole talk when there is one. Parts are cut by the place each call has in
// the talk, the order the work happened in, never by time: a child agent's
// calls take the place of the step that started it. Work no label covers is
// not reviewed.
//
// The avoidable time is taken from the main stream only, since a child's
// work can run beside it: a run of avoidable calls lasts from its first call
// to the talk's next call, or to the talk's end.
//
// Avoidable work is charged to the agent whose doing it was. A sub-task's own
// avoidable work is its own, but when it was given the wrong task, or work
// already done, it did what it was told: that is the agent that gave it the
// task, the main agent or the sub-task above it. Work the asking agent did on
// a sub-task's untrue report is the sub-task's when its task was right, the
// giver's when the task was wrong, and of unknown cause when the record does
// not show which.
func labelCalls(l *Ledger, targets []sessionjudge.Target, who map[int][]string, labels []*sessionjudge.Label, subtasks map[string]*sessionjudge.Subtask) map[string]*targetWork {
	// causeOf says whose doing a sub-task's avoidable work, or work done on
	// its report, was.
	// giver names the agent that gave a sub-task its task: the main agent
	// for one a main talk started, else the sub-task above it, which the
	// ledger always names for a nested one.
	giver := func(sid string) string {
		if st, ok := l.Subtask(sid); ok && st.Depth > 1 {
			return st.Parent
		}
		return sessionjudge.CauseMain
	}
	// misledBy says whose doing work on a sub-task's untrue report was.
	misledBy := func(sid string) string {
		d := subtasks[sid]
		switch {
		case d == nil:
			return sessionjudge.CauseUnknown
		case d.Dispatch == sessionjudge.DispatchMisdirected:
			return giver(sid)
		case d.Dispatch == sessionjudge.DispatchReasonable, d.Dispatch == sessionjudge.DispatchRedundant:
			return sid
		}
		return sessionjudge.CauseUnknown
	}
	var subOf map[int]string
	if subtasks != nil {
		subOf = subtaskCalls(l)
	}
	byPair := map[[2]string][]*sessionjudge.Label{}
	for _, lb := range labels {
		k := [2]string{lb.Target, lb.Talk}
		byPair[k] = append(byPair[k], lb)
	}
	out := map[string]*targetWork{}
	for _, t := range targets {
		w := &targetWork{}
		out[t.ID] = w
		index := map[[4]string]int{}
		for _, tk := range t.Talks {
			whole, ps := talkParts(l, byPair[[2]string{t.ID, tk}])
			// labelOf gives a call's label and, when it is avoidable, whose
			// doing it was.
			labelOf := func(i int) (string, string) {
				if sid, ok := subOf[i]; ok {
					d, judged := subtasks[sid]
					switch {
					case !judged:
						return sessionjudge.WorkNotReviewed, ""
					case d.Work == sessionjudge.WorkMisdirected, d.Work == sessionjudge.WorkRedundant:
						return d.Work, giver(sid)
					case sessionjudge.Avoidable(d.Work):
						return d.Work, sid
					}
					return d.Work, ""
				}
				lb := partOf(l, tk, i, whole, ps)
				switch {
				case lb.work == "":
					return sessionjudge.WorkNotReviewed, ""
				case lb.work == sessionjudge.WorkMisled:
					return lb.work, misledBy(lb.subtask)
				case sessionjudge.Avoidable(lb.work):
					return lb.work, sessionjudge.CauseMain
				}
				return lb.work, ""
			}
			for _, i := range l.CallsOf(tk) {
				if !slices.Contains(who[i], t.ID) {
					continue
				}
				lb, cause := labelOf(i)
				talk, k := "", len(who[i])
				if k > 1 {
					talk = tk
				}
				key := [4]string{lb, talk, fmt.Sprint(k), cause}
				g, ok := index[key]
				if !ok {
					g = len(w.groups)
					index[key] = g
					w.groups = append(w.groups, workGroup{work: lb, talk: talk, cause: cause, k: k})
				}
				w.groups[g].calls = append(w.groups[g].calls, i)
				switch {
				case sessionjudge.Avoidable(lb):
					w.avoidable = append(w.avoidable, i)
					switch cause {
					case sessionjudge.CauseMain:
					case sessionjudge.CauseUnknown:
						w.causeUnknown = append(w.causeUnknown, i)
					default:
						w.bySubtasks = append(w.bySubtasks, i)
					}
				case lb == sessionjudge.WorkNotReviewed:
					w.notReviewed = append(w.notReviewed, i)
				}
			}
			own := l.callsOf[tk]
			end := int64(0)
			if n := l.Talk(tk); n != nil {
				end = n.To
			}
			for x := 0; x < len(own); x++ {
				i := own[x]
				if lb, _ := labelOf(i); !slices.Contains(who[i], t.ID) || !sessionjudge.Avoidable(lb) {
					continue
				}
				from := l.Calls[i].At
				for x+1 < len(own) && slices.Contains(who[own[x+1]], t.ID) {
					if lb, _ := labelOf(own[x+1]); !sessionjudge.Avoidable(lb) {
						break
					}
					x++
				}
				to := end
				if x+1 < len(own) {
					to = l.Calls[own[x+1]].At
				}
				w.avoidableSpans = append(w.avoidableSpans, [2]int64{from, to})
			}
		}
	}
	return out
}

// amountFor adds up a target's share of each call. A call split between k
// targets gives each a k-th of each token type in whole tokens, the
// remainder going to the first targets in the order the call lists them, so
// the targets' amounts add up to the ledger exactly. Money, a fraction
// already, is split as it is.
func amountFor(l *Ledger, calls []int, who map[int][]string, target string) sessionjudge.Amount {
	var a sessionjudge.Amount
	var money float64
	priced := false
	for _, i := range calls {
		c := l.Calls[i]
		if c.Usage == nil {
			a.Unknown++
			continue
		}
		if c.OutputUnknown {
			a.OutputUnknown++
		}
		k, j := 1, 0
		if ws := who[i]; len(ws) > 1 {
			k, j = len(ws), slices.Index(ws, target)
		}
		part := func(v int) int {
			n := v / k
			if j < v%k {
				n++
			}
			return n
		}
		a.Usage.Input += part(c.Usage.Input)
		a.Usage.Output += part(c.Usage.Output)
		a.Usage.CacheRead += part(c.Usage.CacheRead)
		a.Usage.CacheWrite += part(c.Usage.CacheWrite)
		if c.Price == nil {
			a.Unpriced++
			continue
		}
		money += c.Money / float64(k)
		priced = true
	}
	if priced {
		a.Money = &money
	}
	return a
}

// scoreSum gathers the targets' outcomes into the run's scores.
type scoreSum struct {
	value, judged                          float64
	cost, delivered, avoidable, unreviewed [2]float64
	unpriced                               bool
}

// exactAmounts are a target's cost, avoidable cost and cost not reviewed,
// each as money and as tokens, kept as fractions.
type exactAmounts struct{ cost, avoidable, unreviewed [2]float64 }

// exactOf adds up a share of each call, as money and as tokens, unrounded.
func exactOf(l *Ledger, calls []int, share func(int) float64) [2]float64 {
	var out [2]float64
	for _, i := range calls {
		c := l.Calls[i]
		if c.Usage == nil {
			continue
		}
		s := share(i)
		out[0] += c.Money * s
		out[1] += float64(c.Usage.Input+c.Usage.Output+c.Usage.CacheRead+c.Usage.CacheWrite) * s
	}
	return out
}

func (s *scoreSum) add(o *sessionjudge.Outcome, ex exactAmounts) {
	if o.Status != sessionjudge.TargetCancelled && o.Status != sessionjudge.TargetReplaced {
		switch o.Level {
		case sessionjudge.LevelAchieved:
			s.value, s.judged = s.value+1, s.judged+1
		case sessionjudge.LevelPartly:
			// The share of its criteria met, so one criterion nobody could
			// verify does not halve a target whose other criteria were.
			// A contradicted claim counts as one more requirement not met,
			// so a target kept from achieved by one is not scored as if it
			// were.
			share := 0.5
			if o.Met != nil && o.Criteria != nil && *o.Criteria > 0 {
				against := 0
				if o.Contradicted != nil {
					against = *o.Contradicted
				}
				share = float64(*o.Met) / float64(*o.Criteria+against)
			}
			s.value, s.judged = s.value+share, s.judged+1
		case sessionjudge.LevelNotAchieved:
			s.judged++
		}
	}
	if o.Cost.Unpriced > 0 || o.Cost.Unknown > 0 || (o.Cost.Money == nil && o.Cost.Tokens() > 0) {
		s.unpriced = true
	}
	for x := range 2 {
		s.cost[x] += ex.cost[x]
		if o.Level == sessionjudge.LevelAchieved {
			s.delivered[x] += ex.cost[x]
		}
		s.avoidable[x] += ex.avoidable[x]
		s.unreviewed[x] += ex.unreviewed[x]
	}
}

// score is the run's scores. Money is the unit when every call of the
// targets was priced, since a token read from the cache costs a tenth of one
// read fresh, and counting them alike would let cache reads decide.
func (s *scoreSum) score(labeled bool) *sessionjudge.Score {
	out := &sessionjudge.Score{Judged: int(s.judged), Unit: sessionjudge.UnitMoney}
	x := 0
	if s.unpriced {
		x, out.Unit = 1, sessionjudge.UnitTokens
	}
	if s.judged > 0 {
		out.Achievement = ratio(s.value, s.judged)
	}
	if s.cost[x] > 0 {
		out.Delivered = ratio(s.delivered[x], s.cost[x])
		if labeled {
			reviewed := s.cost[x] - s.unreviewed[x]
			out.Reviewed = ratio(reviewed, s.cost[x])
			if reviewed > 0 {
				e := 1 - s.avoidable[x]/reviewed
				out.Efficiency = round4(&e)
			}
		}
	}
	return out
}

func ratio(a, b float64) *float64 {
	r := a / b
	return round4(&r)
}

// round4 keeps four decimals, so a score reads the same in every file.
func round4(v *float64) *float64 {
	r := math.Round(*v*1e4) / 1e4
	return &r
}

// union is the length of the union of time ranges, so overlapping talks count
// once. A range missing either end is left out.
func union(spans [][2]int64) int64 {
	var r [][2]int64
	for _, s := range spans {
		if s[0] != 0 && s[1] >= s[0] {
			r = append(r, s)
		}
	}
	sort.Slice(r, func(i, j int) bool { return r[i][0] < r[j][0] })
	var total, start, end int64
	for i, s := range r {
		if i == 0 || s[0] > end {
			total += end - start
			start, end = s[0], s[1]
			continue
		}
		if s[1] > end {
			end = s[1]
		}
	}
	if len(r) > 0 {
		total += end - start
	}
	return total
}
