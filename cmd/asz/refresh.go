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
	"github.com/apache/skywalking-ai-sessionizer/internal/metrics"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
)

// refresher keeps a served storage root current from its local sources.
//
// It runs the steps the collect and parse commands run, inside the
// process that serves the page, so one command is a complete local setup.
// The page itself still only reads. Two local sources feed it: Claude
// Code's own files, and the change records the asz plugin writes beside
// them. Either may be absent on a machine, and the other still refreshes.
type refresher struct {
	srv      *view.Server
	zone     *storage.Zone
	deriver  *metrics.Deriver
	col      *claudecode.Collector
	match    func(claudecode.Session) bool
	changes  *claudecodechanges.Collector
	cmatch   func(claudecodechanges.Session) bool
	interval time.Duration
	maxRound int64

	// base carries the status fields that do not change between passes.
	base view.Status

	// full asks the next pass to parse every session rather than only the
	// ones that landed data. The first pass must: rounds can be behind the
	// landed data when an earlier collect was never followed by a parse.
	full bool
}

// newRefresher wires the local adapters to a server, or returns nil when
// none of their sources is on this machine.
func newRefresher(srv *view.Server, zone *storage.Zone, ads []config.Adapter, maxRound int64, once bool) (*refresher, error) {
	r := &refresher{srv: srv, zone: zone, maxRound: maxRound, full: true}
	var names, sources []string
	mode := ""
	for _, ad := range ads {
		if mode == "" {
			mode = ad.Collector.Mode
			r.interval = ad.Collector.Interval
		}
		switch ad.Name {
		case config.AdapterClaudeCodeLocal:
			src, err := claudecode.ResolveSourceRoot(ad.SourceRoot)
			if err != nil {
				return nil, err
			}
			if _, err := os.Stat(src); err != nil {
				fmt.Fprintf(os.Stderr, "source   : %s not found; not refreshed\n", src)
				continue
			}
			deriver, err := newDeriver(zone, ad, once || mode == config.ModeOnce)
			if err != nil {
				return nil, err
			}
			r.deriver = deriver
			r.col = claudecode.New(src, zone, ad.Collector.MaxDeltaBytes)
			r.match = claudecode.NewMatcher(ad.Include, ad.Exclude).Match
			names, sources = append(names, ad.Name), append(sources, src)
		case config.AdapterClaudeCodeChanges:
			src, err := claudecodechanges.ResolveSourceRoot(ad.SourceRoot)
			if err != nil {
				return nil, err
			}
			if _, err := os.Stat(src); err != nil {
				// The plugin is not installed, or has never run. Nothing to
				// say: the transcripts refresh on their own.
				continue
			}
			r.changes = claudecodechanges.New(src, zone, ad.Collector.MaxDeltaBytes)
			r.cmatch = changesMatch(ad)
			names, sources = append(names, ad.Name), append(sources, src)
		}
	}
	if r.col == nil && r.changes == nil {
		// A zone copied from another machine has no source beside it. Serving
		// what is already landed is the right thing; pretending to refresh is
		// not.
		fmt.Fprintln(os.Stderr, "source   : none found; serving what is already landed")
		return nil, nil
	}
	if once {
		mode = config.ModeOnce
	}
	r.base = view.Status{Mode: mode, Adapter: strings.Join(names, "+"), Source: strings.Join(sources, ", ")}
	if mode == config.ModeWatch {
		r.base.IntervalMS = r.interval.Milliseconds()
	}
	return r, nil
}

// pass lands what is new from every source, writes a round wherever
// something moved, and records the result for the page.
func (r *refresher) pass() {
	start := time.Now()
	first := r.full

	st := r.base
	st.LastRefresh = r.srv.Status().LastRefresh
	st.Refreshing = true
	r.srv.SetStatus(st)

	var errs []error
	var landed, records, sessionsSeen int
	changed := map[string]bool{}

	var cs *claudecode.Stats
	if r.col != nil {
		var err error
		cs, err = r.col.CollectAll(r.match)
		if err != nil {
			errs = append(errs, err)
			cs = &claudecode.Stats{}
		}
		errs = append(errs, cs.Errors...)
		landed, records, sessionsSeen = cs.SourcesLanded, cs.Records, cs.Sessions
		for _, id := range cs.Changed {
			changed[id] = true
		}
	}
	if r.changes != nil {
		ch, err := r.changes.CollectAll(r.cmatch)
		if err != nil {
			errs = append(errs, err)
			ch = &claudecodechanges.Stats{}
		}
		errs = append(errs, ch.Errors...)
		landed += ch.SourcesLanded
		records += ch.Records
		for _, id := range ch.Changed {
			changed[id] = true
		}
	}

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
			errs = append(errs, fmt.Errorf("%s: %w", id, perr))
			continue
		}
		if res.Changed() || written > 0 {
			rounds += written
			// The server caches a folded conversation. A new round has to
			// reach the next reader.
			r.srv.Forget(id)
		}
	}
	r.full = false

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
	r.srv.SetStatus(st)

	// A quiet pass every few seconds is not worth a line. The first one and
	// any that changed or failed are.
	if first || landed > 0 || rounds > 0 || len(errs) > 0 {
		fmt.Printf("[%s] refreshed: sessions=%d landed=%d records=%d rounds=%d (%s)\n",
			now.Format("15:04:05"), sessionsSeen, landed, records, rounds,
			now.Sub(start).Round(time.Millisecond))
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "  error: %v\n", e)
		}
	}
}

// loop runs passes one interval apart, forever.
func (r *refresher) loop() {
	for {
		time.Sleep(r.interval)
		r.pass()
	}
}
