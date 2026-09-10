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

package main

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecodechanges"
	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp"
	"github.com/apache/skywalking-ai-sessionizer/internal/metrics"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
)

// refresher is the pipeline: land what is new, parse what moved, and send
// what is now on disk.
//
// It is what asz collect runs on every period, and what asz server runs
// beside the page in one process. Two local sources feed it: Claude Code's
// own files, and the change records the asz plugin writes beside them.
// Either may be absent on a machine, and the other still refreshes.
//
// The page is optional. With one, every pass records what it did so the
// list page can show when the data was last refreshed and when it will be
// next; with none, asz collect prints the same numbers and nothing else
// changes. The page itself never writes.
type refresher struct {
	// srv is the page to keep current, or nil when nothing is served.
	srv     *view.Server
	pusher  *otlp.Pusher
	zone    *storage.Zone
	deriver *metrics.Deriver
	col     *claudecode.Collector
	match   func(claudecode.Session) bool
	changes *claudecodechanges.Collector
	cmatch  func(claudecodechanges.Session) bool
	// The directories the two collectors read. They are checked on every
	// pass, not once, so a source that appears later is picked up.
	colSource     string
	changesSource string
	interval      time.Duration
	maxRound      int64
	// pushAfter is when sending may resume, set when a receiver asks to be
	// left alone for longer than one period. Zero means now.
	pushAfter time.Time

	// base carries the status fields that do not change between passes.
	base view.Status

	// full asks the next pass to parse every session rather than only the
	// ones that landed data. The first pass must: rounds can be behind the
	// landed data when an earlier collect was never followed by a parse.
	full bool

	// last says no pass follows this one, which a single pass has. Work
	// another process was holding is then not deferred but lost, because
	// nothing comes back for it, so this pass has to report it instead of
	// passing it on.
	last bool

	// retry holds sessions another builder was writing when this pass
	// reached them. They are not failures, but their landed data is not
	// parsed yet and nothing else will bring them back: a later pass looks
	// only at what moved, and a session whose source has stopped growing
	// never moves again. So they are carried until one pass gets the lock.
	retry map[string]bool
}

// newRefresher wires the local adapters into one pipeline, or returns nil
// when no local adapter is enabled at all.
//
// A source directory that does not exist yet is kept, not dropped. The
// plugin may be installed after the pipeline starts, and a source that
// appears later has to be picked up without a restart; every pass looks
// again.
func newRefresher(srv *view.Server, zone *storage.Zone, ads []config.Adapter, maxRound int64, once bool) (*refresher, error) {
	r := &refresher{srv: srv, zone: zone, maxRound: maxRound, full: true, retry: map[string]bool{}}

	// One pipeline serves every adapter, so it needs one mode and one
	// period, and both have to be settled before anything is built: the
	// metrics derivation reads the mode, and a single pass derives history
	// differently from a watching one.
	mode := config.ModeOnce
	for _, ad := range ads {
		if ad.Collector.Mode == config.ModeWatch {
			mode = config.ModeWatch
		}
		if ad.Collector.Interval > 0 && (r.interval == 0 || ad.Collector.Interval < r.interval) {
			r.interval = ad.Collector.Interval
		}
	}
	if once {
		mode = config.ModeOnce
	}

	var names, sources []string
	for _, ad := range ads {
		switch ad.Name {
		case config.AdapterClaudeCodeLocal:
			src, err := claudecode.ResolveSourceRoot(ad.SourceRoot)
			if err != nil {
				return nil, err
			}
			deriver, err := newDeriver(zone, ad, mode == config.ModeOnce)
			if err != nil {
				return nil, err
			}
			r.deriver = deriver
			r.col = claudecode.New(src, zone, ad.Collector.MaxDeltaBytes)
			r.match = claudecode.NewMatcher(ad.Include, ad.Exclude).Match
			r.colSource = src
			names, sources = append(names, ad.Name), append(sources, src)
		case config.AdapterClaudeCodeChanges:
			src, err := claudecodechanges.ResolveSourceRoot(ad.SourceRoot)
			if err != nil {
				return nil, err
			}
			r.changes = claudecodechanges.New(src, zone, ad.Collector.MaxDeltaBytes)
			r.cmatch = changesMatch(ad)
			r.changesSource = src
			names, sources = append(names, ad.Name), append(sources, src)
		}
	}
	if r.col == nil && r.changes == nil {
		// No local adapter is enabled. A root filled somewhere else, by a
		// receiver or copied from another machine, is served as it is.
		fmt.Fprintln(os.Stderr, "source   : no local adapter enabled; nothing is collected")
		return nil, nil
	}
	r.base = view.Status{Mode: mode, Adapter: strings.Join(names, "+"), Source: strings.Join(sources, ", ")}
	if mode == config.ModeWatch {
		r.base.IntervalMS = r.interval.Milliseconds()
	}
	return r, nil
}

// present reports whether a source directory is there to be read.
//
// optional says what a missing directory means. For the plugin's records it
// is ordinary: the plugin may not be installed, or may not have run yet, and
// the next pass looks again. For the runtime's own transcripts it is a
// misconfiguration worth stopping on, which is what the collector has always
// said about a source root it cannot read.
//
// Anything other than "it is not there", a permission that was taken away or
// a disk that stopped answering, is always returned. A pass that collected
// nothing for that reason must not look like a pass that had nothing to
// collect.
func present(dir string, optional bool) (bool, error) {
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) && optional {
			return false, nil
		}
		return false, fmt.Errorf("source %s: %w", dir, err)
	}
	return true, nil
}

// pass lands what is new from every source, writes a round wherever
// something moved, sends what the receiver is owed, and records the result
// for the page.
//
// It returns what went wrong, so a single pass can exit non-zero rather
// than leave a backfill half done without saying so. A watching pass logs
// the same and goes round again.
func (r *refresher) pass() error {
	start := time.Now()
	first := r.full

	st := r.base
	if r.srv != nil {
		st.LastRefresh = r.srv.Status().LastRefresh
	}
	st.Refreshing = true
	r.setStatus(st)

	var errs []error
	var landed, records, sessionsSeen, pending, conflicts, busy int
	changed := map[string]bool{}

	var cs *claudecode.Stats
	colHere := false
	if r.col != nil {
		var cerr error
		if colHere, cerr = present(r.colSource, false); cerr != nil {
			errs = append(errs, cerr)
		}
	}
	if colHere {
		var err error
		cs, err = r.col.CollectAll(r.match)
		if err != nil {
			errs = append(errs, err)
			cs = &claudecode.Stats{}
		}
		errs = append(errs, cs.Errors...)
		landed, records, sessionsSeen = cs.SourcesLanded, cs.Records, cs.Sessions
		pending += cs.Pending
		conflicts += cs.Conflicts
		busy += cs.Busy
		for _, id := range cs.Changed {
			changed[id] = true
		}
	}
	changesHere := false
	if r.changes != nil {
		var cherr error
		if changesHere, cherr = present(r.changesSource, true); cherr != nil {
			errs = append(errs, cherr)
		}
	}
	if changesHere {
		ch, err := r.changes.CollectAll(r.cmatch)
		if err != nil {
			errs = append(errs, err)
			ch = &claudecodechanges.Stats{}
		}
		errs = append(errs, ch.Errors...)
		landed += ch.SourcesLanded
		records += ch.Records
		sessionsSeen += ch.Sessions
		pending += ch.Pending
		conflicts += ch.Conflicts
		busy += ch.Busy
		for _, id := range ch.Changed {
			changed[id] = true
		}
	}

	// What this pass landed, plus whatever an earlier pass could not take
	// the lock for.
	for id := range r.retry {
		changed[id] = true
	}
	r.retry = map[string]bool{}
	var sessions []string
	for id := range changed {
		sessions = append(sessions, id)
	}
	sort.Strings(sessions)
	if r.full {
		if all, lerr := sessionDirs(r.zone.Root()); lerr != nil {
			errs = append(errs, lerr)
		} else {
			sessions = all
		}
	}
	if r.deriver != nil && cs != nil {
		// The first pass derives history once; later passes only what moved.
		var scope []string
		if !r.full {
			scope = cs.Changed
		}
		if ms, derr := r.deriver.Pass(scope); derr != nil {
			errs = append(errs, derr)
		} else {
			errs = append(errs, ms.Errors...)
		}
	}
	rounds := 0
	for _, id := range sessions {
		written := 0
		res, perr := parseToIndex(r.zone, id, r.maxRound, &written)
		if perr != nil {
			// Another process holds the chain. Nothing is wrong and nothing
			// is lost: whoever holds it is writing the same round, and the
			// next pass reads it. The landing step counts the same thing as
			// busy rather than as an error, and a pass must not exit
			// non-zero because two of them ran at once.
			if errors.Is(perr, storage.ErrChainBusy) || errors.Is(perr, storage.ErrSessionBusy) {
				busy++
				r.retry[id] = true
				continue
			}
			errs = append(errs, fmt.Errorf("%s: %w", id, perr))
			continue
		}
		if res.Changed() || written > 0 {
			rounds += written
			// The server caches a folded conversation. A new round has to
			// reach the next reader.
			if r.srv != nil {
				r.srv.Forget(id)
			}
		}
	}
	r.full = false

	// Send what is now on disk. The push comes last, so a pass ships the
	// rounds it has just written rather than leaving them for the next one.
	pushed := ""
	switch {
	case r.pusher == nil:
	case time.Now().Before(r.pushAfter):
		// The receiver asked to be left alone for longer than one period.
		// Landing and parsing carry on; only the sending waits.
		pushed = fmt.Sprintf(" push-held=%s", time.Until(r.pushAfter).Round(time.Second))
	default:
		ps, perr := r.pusher.Pass()
		if errors.Is(perr, storage.ErrExportBusy) {
			// Another pipeline over this root is sending. A pass that comes
			// round again leaves it to that one; a single pass cannot, since
			// the other pass may have listed its work before this one landed.
			busy++
			if r.last {
				errs = append(errs, fmt.Errorf("another pass held the export state; nothing was sent"))
			}
			break
		}
		if perr != nil {
			errs = append(errs, perr)
			break
		}
		errs = append(errs, ps.Errors...)
		if ps.Files > 0 || ps.Metrics > 0 {
			pushed = fmt.Sprintf(" pushed=%d metrics=%d", ps.Files, ps.Metrics)
		}
		if r.last && ps.Deferred > 0 {
			errs = append(errs, fmt.Errorf("%d round(s) were still being written and were not sent", ps.Deferred))
		}
		if ps.Rejected > 0 {
			// The receiver took the request and dropped records inside it.
			// They are marked sent and never go again, so the only place
			// this can be seen is here.
			pushed += fmt.Sprintf(" rejected=%d", ps.Rejected)
			errs = append(errs, fmt.Errorf("the receiver rejected %d record(s); they are not sent again", ps.Rejected))
		}
		if ps.Throttled && ps.RetryAfter > 0 {
			r.pushAfter = time.Now().Add(ps.RetryAfter)
		}
	}
	if r.last && len(r.retry) > 0 {
		// No pass follows, so these are not deferred, they are unparsed.
		errs = append(errs, fmt.Errorf("%d session(s) were held by another builder and are not parsed", len(r.retry)))
	}
	// The same test the collectors use for a complete pass: anything left
	// behind here is reported, and a single pass exits non-zero for it.
	if pending > 0 {
		errs = append(errs, fmt.Errorf("%d source(s) still had data when the per-pass limit was reached", pending))
	}
	if conflicts > 0 {
		errs = append(errs, fmt.Errorf("%d source(s) were rotated, truncated or rewritten behind the cursor; collection stopped for them", conflicts))
	}

	now := time.Now()
	st.Refreshing = false
	st.LastRefresh = now.UnixMilli()
	if st.Mode == config.ModeWatch {
		st.NextRefresh = now.Add(r.interval).UnixMilli()
	}
	st.Landed, st.Records, st.Rounds = landed, records, rounds
	st.TookMS = now.Sub(start).Milliseconds()
	if len(errs) > 0 {
		st.LastError = errors.Join(errs...).Error()
	}
	r.setStatus(st)

	// A quiet pass every few seconds is not worth a line. The first one and
	// any that changed or failed are.
	if first || landed > 0 || rounds > 0 || busy > 0 || pushed != "" || len(errs) > 0 {
		contended := ""
		if busy > 0 {
			// Two writers on one root is a supported setup, so this is a
			// number to read, not an error. It says how much of this pass
			// the other one was already doing.
			contended = fmt.Sprintf(" busy=%d", busy)
		}
		fmt.Printf("[%s] refreshed: sessions=%d landed=%d records=%d rounds=%d%s%s (%s)\n",
			now.Format("15:04:05"), sessionsSeen, landed, records, rounds, contended, pushed,
			now.Sub(start).Round(time.Millisecond))
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "  error: %v\n", e)
		}
	}
	return errors.Join(errs...)
}

// setStatus records the pass for the page, when there is one.
func (r *refresher) setStatus(st view.Status) {
	if r.srv != nil {
		r.srv.SetStatus(st)
	}
}

// loop runs passes one interval apart, forever. A pass that fails is
// reported by the pass itself and the next one tries again.
func (r *refresher) loop() {
	for {
		time.Sleep(r.interval)
		_ = r.pass()
	}
}
