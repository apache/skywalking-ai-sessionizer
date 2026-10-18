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

package assemble

import (
	"sort"

	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// Where a call's history changed.
//
// A call's history is the message list its request records, as its adapter
// wrote it in the body's manifest, read here from the index alone. A request
// that continues a provider thread holds only what follows the message it
// names; its history is that message's call's history, then that call's
// reply, then its own messages. Each call is compared with the call before it
// in its stream and epoch, skipping only the calls the framework made for its
// own purpose. A history that is the one before it with more after it is kept;
// anything else changed, and draws a history.change card before the call. The
// cause is never inferred: a context reset is an explicit record, and a
// changed history is not one. docs/en/formats/asz-view.md gives every rule.

// maxHistoryLinks bounds the calls a thread's history is built through, and
// maxHistoryEntries the entries a history holds; past either the history is
// too large to compare. Variables only so a test can reach them without
// building tens of thousands of calls.
var (
	maxHistoryLinks   = 65536
	maxHistoryEntries = 65536
)

// maxOrderPairs bounds the pairs of equal identities the order test visits.
// Measured: 65,536 entries a side at this many pairs took about half a second.
const maxOrderPairs = 4194304

// The values a call's history attribute takes.
const (
	historyKept        = "kept"
	historyChanged     = "changed"
	historyFirst       = "first"
	historyNotCompared = "not_compared"
)

// The earlier reply's status.
const (
	replyBack        = "back"
	replyBackChanged = "back_changed"
	replyNotFound    = "not_found"
)

// callHistory is what comparing one call with its predecessor found.
type callHistory struct {
	// attrs go on the call's node.
	attrs map[string]any
	// card is the card's attributes, nil when the history is not changed,
	// and refs the two requests' records the card names.
	card map[string]any
	refs []sessionflow.Ref
}

// hcall is one call as the comparison sees it.
type hcall struct {
	*call
	pc        *providerCall
	req, resp int // rows in the index's Bodies, -1 for none
	epoch     string
	aux       bool
	at        int // place among its stream's calls
}

// historian holds what building histories needs and what it has built.
type historian struct {
	b *builder
	// bodyOf is the row of each body's record, by its interned record id.
	bodyOf map[uint32]int
	// byMessage is each stream's calls by their message id.
	byMessage map[uint32]map[uint32]*hcall
	// measured is each call's history as far as it is known without building
	// it: its length, how many continues it goes back through, or why it has
	// none. Every entry is the call's own, never a limit another call's walk
	// ran into.
	measured map[string]measure
	// systems is each Claude Code call's system prompt, once resolved.
	systems map[string]system
	// lists is each request body's own list as measured, by its row.
	lists   map[int]ownList
	classes map[string]string
}

type measure struct {
	length, links int
	why           string
}

type system struct {
	digest uint32
	known  bool
}

// compareHistories fills b.history, for emitCall to write.
func (b *builder) compareHistories() {
	b.history = map[string]*callHistory{}
	calls := b.callsInLineOrder()
	if len(calls) == 0 {
		return
	}
	pcs := map[string]*providerCall{}
	for _, c := range b.calls {
		pcs[c.NodeID] = c
	}
	h := &historian{b: b, bodyOf: map[uint32]int{}, byMessage: map[uint32]map[uint32]*hcall{},
		measured: map[string]measure{}, systems: map[string]system{}, lists: map[int]ownList{},
		classes: map[string]string{}}
	for i := range b.ix.Bodies {
		rec := b.ix.Entries[b.ix.Bodies[i].Entry].Record
		if _, ok := h.bodyOf[rec]; !ok && rec != 0 {
			h.bodyOf[rec] = i
		}
	}
	byStream := map[uint32][]*hcall{}
	for _, k := range calls {
		pc := pcs[k.node]
		c := &hcall{call: k, pc: pc, req: -1, resp: -1, epoch: b.epochOf(pc.Fragments[0])}
		if r, ok := b.requestRow[k.node]; ok {
			c.req = r
		}
		if r, ok := b.responseRow[k.node]; ok {
			c.resp = r
		}
		if s, ok := b.byStream[k.stream]; ok {
			c.aux = s.Role == model.StreamAuxiliary
		}
		c.at = len(byStream[k.stream])
		byStream[k.stream] = append(byStream[k.stream], c)
		if h.byMessage[k.stream] == nil {
			h.byMessage[k.stream] = map[uint32]*hcall{}
		}
		if k.msg != 0 {
			h.byMessage[k.stream][k.msg] = c
		}
	}
	for _, list := range byStream {
		// A call's predecessor is the nearest earlier call of its stream and
		// epoch, skipping only the calls the framework made for its own
		// purpose. A call of unknown class, or with no request or no list, is
		// never skipped: had it been a turn, skipping it would compare the
		// call with the one before it and show a change that did not happen.
		// One walk forward keeps the nearest, so a run of internal calls costs
		// nothing more.
		var last *hcall
		for _, n := range list {
			p := last
			if p != nil && p.epoch != n.epoch {
				p = nil
			}
			if n.req >= 0 && b.ix.Bodies[n.req].Form != 0 {
				b.history[n.node] = h.compare(n, p)
			}
			if h.class(n) != model.ClassInternalCall {
				last = n
			}
		}
	}
}

// class is a call's class: what its records say, and for Claude Code, whose
// every call its transcript records is a turn, the form of its request.
func (h *historian) class(c *hcall) string {
	if class, ok := h.classes[c.node]; ok {
		return class
	}
	class := ""
	turn := false
	for _, f := range c.pc.Fragments {
		if f.Flags.Has(index.FlagInternalCall) {
			class = model.ClassInternalCall
			break
		}
		turn = turn || f.Flags.Has(index.FlagTurnCall)
	}
	if class == "" && (turn || h.formOf(c.req) == providerbody.FormClaudeCode) {
		class = model.ClassTurnCall
	}
	h.classes[c.node] = class
	return class
}

func (h *historian) formOf(row int) string {
	if row < 0 {
		return ""
	}
	return h.b.str(h.b.ix.Bodies[row].Form)
}

// compare decides one call's history attributes, in the order the format
// page's table gives: the first row that applies.
func (h *historian) compare(n, p *hcall) *callHistory {
	out := &callHistory{attrs: map[string]any{}}
	set := func(history, reason string) *callHistory {
		out.attrs["history"] = history
		if reason != "" {
			out.attrs["history_reason"] = reason
		}
		return out
	}
	nm := h.length(n)
	nWhy := nm.why
	if nWhy == "" {
		out.attrs["history_messages"] = nm.length
	}
	switch class := h.class(n); {
	case n.aux:
		return set(historyNotCompared, "auxiliary_stream")
	case class == model.ClassInternalCall:
		return set(historyNotCompared, model.ClassInternalCall)
	case class == "":
		return set(model.Unavailable, "class_unknown")
	case nWhy != "":
		return set(model.Unavailable, nWhy)
	case p == nil:
		return set(historyFirst, "")
	}
	switch {
	case p.req < 0:
		return set(model.Unavailable, "earlier_no_request")
	case h.b.ix.Bodies[p.req].Form == 0:
		return set(model.Unavailable, "earlier_messages:absent")
	case h.class(p) == "":
		return set(model.Unavailable, "earlier_class_unknown")
	case h.formOf(p.req) != h.formOf(n.req):
		return set(model.Unavailable, "form_mismatch")
	}
	if why := h.length(p).why; why != "" {
		return set(model.Unavailable, "earlier_"+why)
	}
	out.attrs["history_from"] = p.node

	// A request that continues its predecessor's reply keeps its history by
	// construction, and its reply sits right after it; nothing is walked.
	if y := &h.b.ix.Bodies[n.req]; y.Thread == index.ThreadContinue && y.ThreadPrevious == p.msg && p.msg != 0 {
		set(historyKept, "")
		out.attrs["history_reply"] = replyBack
		h.system(out, p, n)
		return out
	}
	P, _ := h.list(p)
	N, _ := h.list(n)
	reply, replyWhy := h.reply(p, n)
	if isPrefix(P, N) {
		set(historyKept, "")
		h.replyAfter(out, reply, replyWhy, N[len(P):])
		h.system(out, p, n)
		return out
	}
	set(historyChanged, "")
	h.replyIn(out, reply, replyWhy, P, N)
	h.system(out, p, n)
	card := counts(P, N)
	card["from_call"], card["to_call"] = p.node, n.node
	card["cause"] = model.Unavailable
	card["reply"] = out.attrs["history_reply"]
	if why, ok := out.attrs["history_reply_reason"]; ok {
		card["reply_reason"] = why
	}
	if sc, ok := out.attrs["system_changed"]; ok {
		card["system_changed"] = sc
	}
	out.card = card
	out.refs = []sessionflow.Ref{h.refOf(p.req), h.refOf(n.req)}
	return out
}

func (h *historian) refOf(row int) sessionflow.Ref {
	e := &h.b.ix.Entries[h.b.ix.Bodies[row].Entry]
	return sessionflow.Ref{Seq: uint64(e.Seq), Row: uint64(e.Row)}
}

// length is how many entries a call's history holds, how many continues it
// goes back through, or why it has none. A thread's history is measured
// through the calls it continues without building it: one walk back to where
// the measure is known, then forward, keeping each call's own result, so a
// long session costs one walk and no call's result depends on where a walk
// began.
func (h *historian) length(c *hcall) measure {
	if m, ok := h.measured[c.node]; ok {
		return m
	}
	// Back through the thread to a call whose measure is known or that holds
	// its whole list, noting what each continues.
	var chain []*hcall
	next := map[string]*hcall{}
	onChain := map[string]bool{}
	for x := c; ; {
		if _, ok := h.measured[x.node]; ok {
			break
		}
		if onChain[x.node] {
			// A thread that comes back to itself has no start.
			for _, y := range chain {
				h.measured[y.node] = measure{why: "thread_unresolved"}
			}
			return h.measured[c.node]
		}
		onChain[x.node] = true
		chain = append(chain, x)
		if x.req < 0 {
			break
		}
		y, why := h.continued(x)
		if why != "" || y == nil {
			break
		}
		next[x.node] = y
		x = y
	}
	for i := len(chain) - 1; i >= 0; i-- {
		h.measured[chain[i].node] = h.measureOne(chain[i], next[chain[i].node])
	}
	return h.measured[c.node]
}

// measureOne is one call's measure, from the measure of the call it
// continues, which is known.
func (h *historian) measureOne(c, x *hcall) measure {
	if c.req < 0 {
		return measure{why: "no_request"}
	}
	own := h.ownOf(c.req)
	switch {
	case own.why != "":
		return measure{why: own.why}
	case own.length > maxHistoryEntries:
		return measure{why: "messages:" + providerbody.TooLarge}
	}
	if x == nil {
		if _, why := h.continued(c); why != "" {
			return measure{why: why}
		}
		return measure{length: own.length}
	}
	before := h.measured[x.node]
	switch {
	case before.why == "messages:"+providerbody.TooLarge:
		return measure{why: before.why}
	case before.why != "":
		return measure{why: "thread_unresolved"}
	case before.links+1 > maxHistoryLinks || before.length+1+own.length > maxHistoryEntries:
		return measure{why: "messages:" + providerbody.TooLarge}
	}
	return measure{length: before.length + 1 + own.length, links: before.links + 1}
}

// continued is the call a continue request goes on from, nil for a request
// that holds its whole list, or why the thread cannot be followed: the call it
// names is not in its stream, or is of another form, or gave no reply.
func (h *historian) continued(c *hcall) (*hcall, string) {
	y := &h.b.ix.Bodies[c.req]
	if y.Thread != index.ThreadContinue {
		return nil, ""
	}
	x := h.byMessage[c.stream][y.ThreadPrevious]
	if x == nil || x.req < 0 || h.formOf(x.req) != h.formOf(c.req) {
		return nil, "thread_unresolved"
	}
	if _, why := h.reply(x, c); why != "" {
		return nil, "thread_reply:" + why
	}
	return x, ""
}

// list builds a call's whole history, walking a thread back to its start.
func (h *historian) list(c *hcall) ([]index.Message, string) {
	m := h.length(c)
	if m.why != "" {
		return nil, m.why
	}
	var parts [][]index.Message
	for x := c; x != nil; {
		own, _ := h.own(x.req)
		parts = append(parts, own)
		next, _ := h.continued(x)
		if next != nil {
			r, _ := h.reply(next, x)
			parts = append(parts, []index.Message{r})
		}
		x = next
	}
	out := make([]index.Message, 0, m.length)
	for i := len(parts) - 1; i >= 0; i-- {
		out = append(out, parts[i]...)
	}
	return out, ""
}

// ownList is what a request's own body says about its list, without building
// it: how many entries it holds and how many bases deep its copies go, or why
// it has none.
type ownList struct {
	length, depth int
	why           string
}

// ownOf measures a request's own list: whole, or against the body its copy
// came from. A base must be a request landed earlier, of the same form, whose
// own list holds, at most a body's copies deep, at least the entries kept from
// it. A list over a base that does not hold is not read, however few of its
// entries a later list keeps. One walk back to a measured or whole body, then
// forward, so a long run of copies costs one walk.
func (h *historian) ownOf(row int) ownList {
	var chain []int
	for r := row; ; {
		if _, ok := h.lists[r]; ok {
			break
		}
		chain = append(chain, r)
		y := &h.b.ix.Bodies[r]
		if y.List != index.ListRelative {
			break
		}
		base, ok := h.bodyOf[y.ListBase]
		if !ok || base >= r {
			break
		}
		r = base
	}
	for i := len(chain) - 1; i >= 0; i-- {
		h.lists[chain[i]] = h.ownOne(chain[i])
	}
	return h.lists[row]
}

// ownOne is one body's measure, from its base's, which is known.
func (h *historian) ownOne(row int) ownList {
	y := &h.b.ix.Bodies[row]
	switch y.List {
	case index.ListUnavailable:
		return ownList{why: "messages:" + h.b.str(y.ListWhy)}
	case index.ListAbsent:
		return ownList{why: "messages:absent"}
	case index.ListWhole:
		return ownList{length: int(y.MsgCount)}
	}
	bad := ownList{why: "messages:" + providerbody.UnrecognisedShape}
	base, ok := h.bodyOf[y.ListBase]
	if !ok || base >= row {
		return bad
	}
	if b := &h.b.ix.Bodies[base]; b.Role != index.BodyRoleRequest || b.Form != y.Form {
		return bad
	}
	prior := h.lists[base]
	if prior.why != "" || prior.depth >= providerbody.MaxDepth || prior.length < int(y.ListKeep) {
		return bad
	}
	return ownList{length: int(y.ListKeep) + int(y.MsgCount), depth: prior.depth + 1}
}

// own is a request's whole own list, or why there is none to compare: a list
// over the entry bound is too large, though a later list may keep its front.
func (h *historian) own(row int) ([]index.Message, string) {
	l := h.ownOf(row)
	switch {
	case l.why != "":
		return nil, l.why
	case l.length > maxHistoryEntries:
		return nil, "messages:" + providerbody.TooLarge
	}
	y := &h.b.ix.Bodies[row]
	if y.List == index.ListWhole {
		return h.b.ix.Messages[y.MsgFirst : y.MsgFirst+y.MsgCount], ""
	}
	out := make([]index.Message, l.length)
	h.fill(row, out)
	return out, ""
}

// fill writes the first len(out) entries of a request's measured list into
// out: what each body adds goes in its place, and the walk goes on to its
// base only for the front it keeps, so nothing past the front is copied.
func (h *historian) fill(row int, out []index.Message) {
	for {
		y := &h.b.ix.Bodies[row]
		mine := h.b.ix.Messages[y.MsgFirst : y.MsgFirst+y.MsgCount]
		if y.List == index.ListWhole {
			copy(out, mine)
			return
		}
		if keep := int(y.ListKeep); len(out) > keep {
			copy(out[keep:], mine)
			out = out[:keep]
		}
		row = h.bodyOf[y.ListBase]
	}
}

// reply is the message p's response produced, for comparing with n, or why
// there is none to compare: no response or none it names, or one of another
// form than n's request.
func (h *historian) reply(p, n *hcall) (index.Message, string) {
	if p.resp < 0 {
		return index.Message{}, "no_reply"
	}
	y := &h.b.ix.Bodies[p.resp]
	if y.Reply != index.ReplyEntry {
		return index.Message{}, "no_reply"
	}
	if h.formOf(p.resp) != h.formOf(n.req) {
		return index.Message{}, "form_mismatch"
	}
	return index.Message{HasID: y.ReplyHasID, ID: y.ReplyID, Digest: y.ReplyDigest}, ""
}

// replyAfter finds the earlier reply in what a kept history added after the
// earlier one, where no entry is one of the earlier history's.
func (h *historian) replyAfter(out *callHistory, r index.Message, why string, after []index.Message) {
	if why != "" {
		out.attrs["history_reply"], out.attrs["history_reply_reason"] = model.Unavailable, why
		return
	}
	out.attrs["history_reply"] = find(r, after)
}

// replyIn finds the earlier reply in a changed history. The counts match
// occurrences by multiset, so a reply equal to an entry of the earlier
// history, or sharing its id, could be either, and none is claimed.
func (h *historian) replyIn(out *callHistory, r index.Message, why string, P, N []index.Message) {
	if why == "" {
		for _, e := range P {
			if e == r || (r.HasID && e.HasID && e.ID == r.ID) {
				why = "ambiguous"
				break
			}
		}
	}
	h.replyAfter(out, r, why, N)
}

func find(r index.Message, in []index.Message) string {
	result := replyNotFound
	for _, e := range in {
		switch {
		case e == r:
			return replyBack
		case r.HasID && e.HasID && e.ID == r.ID:
			result = replyBackChanged
		}
	}
	return result
}

// system says whether the system prompt changed between two Claude Code
// calls. A continue request's system is the thread's, unless it carries a
// block of its own, which may add to the thread's or replace it: that is not
// measured, so it, and every call continuing from it, is not known.
func (h *historian) system(out *callHistory, p, n *hcall) {
	if h.formOf(n.req) != providerbody.FormClaudeCode {
		return
	}
	a, aok := h.systemOf(p)
	b, bok := h.systemOf(n)
	switch {
	case !aok || !bok:
		out.attrs["system_changed"] = model.Unavailable
	case a != b:
		out.attrs["system_changed"] = "yes"
	default:
		out.attrs["system_changed"] = "no"
	}
}

// systemOf is a call's system prompt, kept once resolved: a continue
// request's comes from the call it continues, walked back iteratively to
// where it is known. The walk has no bound of its own: what it walks a
// comparison has measured within the link bound, and every answer kept is
// the call's own, never where a bound stopped some other call's walk.
func (h *historian) systemOf(c *hcall) (uint32, bool) {
	var chain []*hcall
	onChain := map[string]bool{}
	got := system{}
	for x := c; ; {
		if s, ok := h.systems[x.node]; ok {
			got = s
			break
		}
		if onChain[x.node] {
			// A thread that comes back to itself has no start.
			got = system{}
			break
		}
		onChain[x.node] = true
		chain = append(chain, x)
		if x.req < 0 {
			got = system{}
			break
		}
		y := &h.b.ix.Bodies[x.req]
		if y.SystemUnknown {
			got = system{}
			break
		}
		if y.Thread != index.ThreadContinue {
			got = system{digest: y.System, known: true}
			break
		}
		if y.System != 0 {
			got = system{}
			break
		}
		next, why := h.continued(x)
		if why != "" || next == nil {
			got = system{}
			break
		}
		x = next
	}
	// Every call walked shares the answer: a continue with no system block
	// of its own has the thread's, and one whose walk found no start has none
	// known. The call that decided it has its own answer, which is the same.
	for _, x := range chain {
		h.systems[x.node] = got
	}
	return got.digest, got.known
}

func isPrefix(p, n []index.Message) bool {
	if len(p) > len(n) {
		return false
	}
	for i := range p {
		if p[i] != n[i] {
			return false
		}
	}
	return true
}

// identity is an entry's id where it has one, else its digest; the two kinds
// are never equal.
type identity struct {
	hasID bool
	v     uint64
}

func identityOf(m index.Message) identity {
	if m.HasID {
		return identity{true, m.ID}
	}
	return identity{false, m.Digest}
}

// counts explains a changed history by multiset counts, which need no unique
// keys: repeated messages are common, such as a person typing "continue"
// twice.
func counts(P, N []index.Message) map[string]any {
	cp, cn := map[index.Message]int{}, map[index.Message]int{}
	for _, e := range P {
		cp[e]++
	}
	for _, e := range N {
		cn[e]++
	}
	kept := 0
	leftP, leftN := map[uint64]int{}, map[uint64]int{}
	for e, n := range cp {
		k := min(n, cn[e])
		kept += k
		if e.HasID {
			leftP[e.ID] += n - k
		}
	}
	for e, n := range cn {
		if e.HasID {
			leftN[e.ID] += n - min(n, cp[e])
		}
	}
	changed := 0
	for id, n := range leftP {
		changed += min(n, leftN[id])
	}
	out := map[string]any{
		"kept": kept, "changed": changed,
		"dropped": len(P) - kept - changed, "added": len(N) - kept - changed,
	}
	switch order, why := orderChanged(P, N, kept+changed); {
	case why != "":
		out["order_changed"], out["order_changed_reason"] = model.Unavailable, why
	case order:
		out["order_changed"] = "yes"
	default:
		out["order_changed"] = "no"
	}
	return out
}

// orderChanged says whether the messages kept or changed are no longer in
// their earlier order. An id that occurs twice on a side leaves which earlier
// message became which unknown. Otherwise an id names one message on each
// side and a repeated digest names identical messages, so the order changed
// exactly when the longest common subsequence of the two identity sequences
// is shorter than the messages matched.
func orderChanged(P, N []index.Message, matched int) (bool, string) {
	if repeatedID(P) || repeatedID(N) {
		return false, "repeated_id"
	}
	at := map[identity][]int{}
	for j := len(N) - 1; j >= 0; j-- {
		at[identityOf(N[j])] = append(at[identityOf(N[j])], j)
	}
	pairs := 0
	for _, e := range P {
		pairs += len(at[identityOf(e)])
	}
	if pairs > maxOrderPairs {
		return false, providerbody.TooLarge
	}
	// Hunt and Szymanski: the matching positions of N, each taken in
	// descending order, are a longest increasing subsequence away from the
	// longest common subsequence.
	var tails []int
	for _, e := range P {
		for _, j := range at[identityOf(e)] {
			k := sort.SearchInts(tails, j)
			if k == len(tails) {
				tails = append(tails, j)
			} else {
				tails[k] = j
			}
		}
	}
	return len(tails) < matched, ""
}

func repeatedID(list []index.Message) bool {
	seen := map[uint64]bool{}
	for _, e := range list {
		if !e.HasID {
			continue
		}
		if seen[e.ID] {
			return true
		}
		seen[e.ID] = true
	}
	return false
}
