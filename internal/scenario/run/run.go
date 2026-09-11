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

// Package run drives a scenario end to end: build, collect, parse, and
// evaluate, at every checkpoint and in every format. It wires the collector
// side to the server side the way the command does, and it is used only by
// the command and by the tests; nothing else may import it.
package run

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecodechanges"
	"github.com/apache/skywalking-ai-sessionizer/internal/metrics"
	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/repack"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario/expect"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// Options settle a check run.
type Options struct {
	Formats  []scenario.Format
	Out      string // empty: a temporary directory, removed on success
	At       time.Time
	Scale    float64
	Interval time.Duration
}

// Report is what a check found: one line per format and checkpoint, and
// whether anything failed.
type Report struct {
	Lines  []string
	Failed bool
	Out    string
}

func (r *Report) say(format string, a ...any) { r.Lines = append(r.Lines, fmt.Sprintf(format, a...)) }

func (r *Report) fail(format string, a ...any) {
	r.Failed = true
	r.say(format, a...)
}

// ExpectPath is the expectation file beside a scenario: NAME.expect.yaml.
func ExpectPath(scenarioPath string) string {
	return strings.TrimSuffix(scenarioPath, filepath.Ext(scenarioPath)) + ".expect.yaml"
}

// Check runs a scenario's expectations in every format and compares the
// formats with each other.
func Check(scenarioPath string, opts Options) (*Report, error) {
	sc, err := scenario.Load(scenarioPath)
	if err != nil {
		return nil, err
	}
	ex, err := expect.Load(ExpectPath(scenarioPath))
	if err != nil {
		return nil, err
	}
	if opts.At.IsZero() {
		opts.At = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	formats := opts.Formats
	if len(formats) == 0 {
		formats = scenario.Formats
	}
	rep := &Report{}
	out := opts.Out
	if out == "" {
		out, err = os.MkdirTemp("", "asz-scenario-")
		if err != nil {
			return nil, err
		}
	}
	rep.Out = out
	summaries := map[scenario.Format]*expect.Summary{}
	sessions := map[scenario.Format]string{}
	for _, f := range formats {
		s, session, err := checkFormat(sc, ex, f, filepath.Join(out, string(f)), opts, rep)
		if err != nil {
			return rep, err
		}
		summaries[f], sessions[f] = s, session
	}
	if expect.On(ex.Properties.CrossFormat) && len(formats) > 1 {
		base := summaries[formats[0]]
		failed := rep.Failed
		for _, f := range formats[1:] {
			if base == nil || summaries[f] == nil {
				continue
			}
			for _, d := range expect.Compare(base, summaries[f]) {
				rep.fail("%s against %s: %s", formats[0], f, d)
			}
		}
		if rep.Failed == failed {
			rep.say("cross-format: the folds agree")
		}
	}
	if expect.On(ex.Properties.RecordsMatch) && len(formats) > 1 && sessions[formats[0]] != "" {
		failed := rep.Failed
		for _, f := range formats[1:] {
			if sessions[f] == "" {
				continue
			}
			lines, err := expect.RecordsAgree(filepath.Join(out, string(formats[0])), filepath.Join(out, string(f)), sessions[f])
			if err != nil {
				return rep, err
			}
			for _, l := range lines {
				rep.fail("%s against %s: %s", formats[0], f, l)
			}
		}
		if rep.Failed == failed {
			rep.say("records_match: the landed records agree")
		}
	}
	if !rep.Failed && opts.Out == "" {
		_ = os.RemoveAll(out)
	}
	return rep, nil
}

// checkFormat builds through every checkpoint, collects when the format
// needs it, parses, and evaluates; at the end it runs the properties. It
// returns the fold's summary and the session, for the cross-format checks.
func checkFormat(sc *scenario.Scenario, ex *expect.File, f scenario.Format, out string, opts Options, rep *Report) (*expect.Summary, string, error) {
	points := append(sc.Checkpoints(), "")
	var built *scenario.Built
	var round *parse.Round
	for _, cp := range points {
		name := cp
		if name == "" {
			name = "final"
		}
		var err error
		built, err = scenario.Build(sc, f, out, scenario.Options{At: opts.At, Scale: opts.Scale, Interval: opts.Interval, Through: cp})
		if err != nil {
			return nil, "", err
		}
		if _, err := collect(f, out); err != nil {
			return nil, "", err
		}
		// The runtime's metric family, derived as the collector derives it,
		// after each landing and before anything later happens to the root.
		if _, err := derive(out); err != nil {
			return nil, "", err
		}
		round, err = parseAll(out, built.Session, ex.Parse.MaxRoundBytes)
		if err != nil {
			rep.fail("%s %s: parse: %v", f, name, err)
			return nil, "", nil
		}
		want := ex.Checkpoints[name]
		if want == nil {
			want = &expect.Checkpoint{}
		}
		ctx := &expect.Context{Session: built.Session, Names: names(built.Plan), At: opts.At, Round: round}
		// What a person deletes from the root after the round bound to it.
		// Every check from here on runs over the damaged root.
		for _, l := range want.Lose {
			rel, err := lose(out, built.Session, l, ctx)
			if err != nil {
				return nil, "", err
			}
			rep.say("%s %s: lost %s", f, name, rel)
		}
		problems, err := expect.Evaluate(out, want, ctx)
		if err != nil {
			return nil, "", err
		}
		if want.Empty() {
			d, err := expect.Describe(out, built.Session)
			if err != nil {
				return nil, "", err
			}
			rep.say("%s %s: no expectations; the fold holds %s", f, name, d)
		} else if len(problems) == 0 {
			rep.say("%s %s: ok", f, name)
		}
		for _, p := range problems {
			rep.fail("%s %s: %s", f, name, p)
		}
	}
	session := built.Session
	zone := storage.NewZone(out)
	props := func(name string, on *bool, fn func() ([]string, error)) error {
		if !expect.On(on) {
			return nil
		}
		lines, err := fn()
		if err != nil {
			return err
		}
		for _, l := range lines {
			rep.fail("%s %s", f, l)
		}
		if len(lines) == 0 {
			rep.say("%s %s: ok", f, name)
		}
		return nil
	}
	checks := []struct {
		name string
		on   *bool
		fn   func() ([]string, error)
	}{
		{"unchanged_writes_nothing", nil, func() ([]string, error) {
			r, err := parse.Session(zone, parse.Options{Conversation: session, Session: session, MaxRoundBytes: ex.Parse.MaxRoundBytes})
			if err != nil {
				return nil, err
			}
			if r.Changed() {
				return []string{fmt.Sprintf("unchanged_writes_nothing: a parse with no new evidence wrote round %d", r.Number)}, nil
			}
			return nil, nil
		}},
		{"header_matches_fold", ex.Properties.HeaderMatchesFold, func() ([]string, error) { return expect.HeaderMatchesFold(out, session) }},
		{"fold_equals_parse", ex.Properties.FoldEqualsParse, func() ([]string, error) { return expect.FoldEqualsParse(out, session, session) }},
		{"immutable_rounds", ex.Properties.ImmutableRounds, func() ([]string, error) { return expect.ImmutableRounds(out, session) }},
		{"records_well_formed", ex.Properties.RecordsWellFormed, func() ([]string, error) { return expect.RecordsWellFormed(out, session) }},
		{"repack_keeps_structure", ex.Properties.RepackKeepsStructure, func() ([]string, error) { return repackKeepsStructure(out, session, ex.Parse.MaxRoundBytes) }},
		{"push_follows_the_wire", ex.Properties.PushFollowsTheWire, func() ([]string, error) {
			return pushFollowsTheWire(out, session, f, ex.Push, expectedTokens(built.Plan), expect.On(ex.Properties.MetricsMatchThePlan))
		}},
		{"removed_after_sent", ex.Properties.RemovedAfterSent, func() ([]string, error) {
			return removedAfterSent(sc, f, out, session, built.Plan.Project, ex.Parse.MaxRoundBytes, opts)
		}},
		{"view_covers_the_session", ex.Properties.ViewCoversTheSession, func() ([]string, error) { return expect.ViewCoversTheSession(out, session) }},
		{"changes_leave_the_fold", ex.Properties.ChangesLeaveTheFold, func() ([]string, error) {
			if !scenario.HasChanges(sc.Steps) {
				return nil, nil
			}
			return changesLeaveTheFold(sc, f, out, session, opts, ex.Parse.MaxRoundBytes)
		}},
		{"reproducible", ex.Properties.Reproducible, func() ([]string, error) {
			// Two parses of identical landed evidence must produce identical
			// rounds. The chain in out was cut at checkpoints, so it is not
			// the comparison; two fresh copies of the landed files are.
			a, b := out+"-twin-a", out+"-twin-b"
			defer os.RemoveAll(a)
			defer os.RemoveAll(b)
			for _, twin := range []string{a, b} {
				if err := copyLanded(out, twin); err != nil {
					return nil, err
				}
			}
			da, err := parseAll(a, session, ex.Parse.MaxRoundBytes)
			if err != nil {
				return nil, err
			}
			db, err := parseAll(b, session, ex.Parse.MaxRoundBytes)
			if err != nil {
				return nil, err
			}
			if da.Digest == "" || da.Digest != db.Digest {
				return []string{fmt.Sprintf("reproducible: two parses of the same landed files wrote different rounds: %s and %s", da.Digest, db.Digest)}, nil
			}
			return nil, nil
		}},
	}
	if f == scenario.FormatClaudeCode {
		checks = append(checks,
			struct {
				name string
				on   *bool
				fn   func() ([]string, error)
			}{"recollect_idempotent", ex.Properties.RecollectIdempotent, func() ([]string, error) {
				st, err := claudecode.New(filepath.Join(out, "_source"), zone, 0).CollectAll(nil)
				if err != nil {
					return nil, err
				}
				if st.Records != 0 || st.SourcesLanded != 0 {
					return []string{fmt.Sprintf("recollect_idempotent: a second collect landed %d records from %d sources", st.Records, st.SourcesLanded)}, nil
				}
				cs, err := claudecodechanges.New(filepath.Join(out, "_source", "plugins", "data"), zone, 0).CollectAll(nil)
				if err != nil {
					return nil, err
				}
				if cs.Records != 0 || cs.SourcesLanded != 0 {
					return []string{fmt.Sprintf("recollect_idempotent: a second collect of the changes landed %d records from %d sources", cs.Records, cs.SourcesLanded)}, nil
				}
				return nil, nil
			}},
			struct {
				name string
				on   *bool
				fn   func() ([]string, error)
			}{"every_line_a_record", ex.Properties.EveryLineARecord, func() ([]string, error) { return everyLineARecord(out, session) }},
			struct {
				name string
				on   *bool
				fn   func() ([]string, error)
			}{"discovery_ignores_noise", ex.Properties.DiscoveryIgnoresNoise, func() ([]string, error) { return discoveryIgnoresNoise(out, session) }},
			struct {
				name string
				on   *bool
				fn   func() ([]string, error)
			}{"pruned_sources_gone", ex.Properties.PrunedSourcesGone, func() ([]string, error) {
				return prunedSourcesGone(out, session, ex.Parse.MaxRoundBytes)
			}},
		)
	}
	for _, c := range checks {
		if err := props(c.name, c.on, c.fn); err != nil {
			return nil, "", err
		}
	}
	summary, err := expect.Summarize(out, session)
	if err != nil {
		return nil, "", err
	}
	// The bundle check removes the index and state, so it runs last.
	if err := props("bundle", ex.Properties.Bundle, func() ([]string, error) { return expect.Bundle(out, session, session) }); err != nil {
		return nil, "", err
	}
	return summary, session, nil
}

// lose deletes one landed file of the session, as a person would: the file
// is read-only, so it is made writable first. It returns the path relative
// to the root.
func lose(out, session string, l expect.Lose, ctx *expect.Context) (string, error) {
	files, err := storage.LandedFiles(storage.NewZone(out), session)
	if err != nil {
		return "", err
	}
	f, err := l.Resolve(files, ctx.Stream(l.Stream))
	if err != nil {
		return "", err
	}
	if err := os.Chmod(f.Path, 0o644); err != nil {
		return "", err
	}
	if err := os.Remove(f.Path); err != nil {
		return "", err
	}
	rel, _ := filepath.Rel(out, f.Path)
	return filepath.ToSlash(rel), nil
}

// derive writes the runtime's metric family for what is landed and not yet
// derived, with no look-back: a scenario is history by construction. It
// returns how many requests it put in the spool.
func derive(out string) (int, error) {
	d := &metrics.Deriver{Zone: storage.NewZone(out), Options: metrics.Options{Grace: -1, Version: "check", Now: checkNow}}
	st, err := d.Pass(nil)
	if err != nil {
		return 0, err
	}
	if len(st.Errors) != 0 {
		return 0, fmt.Errorf("metrics: %v", st.Errors)
	}
	return st.Requests, nil
}

// checkNow is the clock of everything a check runs after the build: the
// deriver, the pusher and the removal. It is fixed, so a check writes the
// same bytes every time.
func checkNow() time.Time { return time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC) }

// collect lands a runtime format's source through its adapter, and returns
// how many sources it landed from. An sd build is landed already.
func collect(f scenario.Format, out string) (int, error) {
	if f != scenario.FormatClaudeCode {
		return 0, nil
	}
	st, err := claudecode.New(filepath.Join(out, "_source"), storage.NewZone(out), 0).CollectAll(nil)
	if err != nil {
		return 0, err
	}
	if len(st.Errors) != 0 {
		return 0, fmt.Errorf("collect: %v", st.Errors)
	}
	// The plugin's lines, through their own adapter, as asz view runs both.
	cs, err := claudecodechanges.New(filepath.Join(out, "_source", "plugins", "data"), storage.NewZone(out), 0).CollectAll(nil)
	if err != nil {
		return 0, err
	}
	if len(cs.Errors) != 0 {
		return 0, fmt.Errorf("collect changes: %v", cs.Errors)
	}
	return st.SourcesLanded + cs.SourcesLanded, nil
}

// changesLeaveTheFold builds the scenario again without its changes and
// compares the folds: the nodes, the relations and the talks must be the
// same, because a change record is evidence beside a step and never a
// step. The chain's bytes differ, since a round binds the files it read.
func changesLeaveTheFold(sc *scenario.Scenario, f scenario.Format, out, session string, opts Options, maxRound int64) ([]string, error) {
	plain := out + "-plain"
	defer os.RemoveAll(plain)
	built, err := scenario.Build(sc.WithoutChanges(), f, plain, scenario.Options{At: opts.At, Scale: opts.Scale, Interval: opts.Interval})
	if err != nil {
		return nil, err
	}
	if _, err := collect(f, plain); err != nil {
		return nil, err
	}
	if _, err := parseAll(plain, built.Session, maxRound); err != nil {
		return nil, err
	}
	with, err := expect.Summarize(out, session)
	if err != nil {
		return nil, err
	}
	without, err := expect.Summarize(plain, built.Session)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, d := range expect.Compare(with, without) {
		lines = append(lines, "changes_leave_the_fold: the fold differs with and without the changes: "+d)
	}
	return lines, nil
}

// parseAll writes rounds until the chain reaches the index, and returns the
// last round.
func parseAll(root, session string, maxRound int64) (*parse.Round, error) {
	z := storage.NewZone(root)
	for {
		r, err := parse.Session(z, parse.Options{Conversation: session, Session: session, MaxRoundBytes: maxRound})
		if err != nil {
			return nil, err
		}
		if !r.Changed() || !r.More {
			return r, nil
		}
	}
}

// copyLanded copies a session's landed files, and nothing else: no index, no
// state, no chain. What a bundle would carry.
func copyLanded(from, to string) error {
	return filepath.WalkDir(from, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if rel == "_conversations" || rel == "_source" || filepath.Base(rel) == "index" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(to, rel), 0o755)
		}
		if !strings.HasSuffix(path, ".sd") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(to, rel), body, 0o644)
	})
}

// everyLineARecord checks that every line of every source file the adapter
// read became exactly one landed record, in order.
func everyLineARecord(out, session string) ([]string, error) {
	source := filepath.Join(out, "_source")
	landed, err := storage.LandedFiles(storage.NewZone(out), session)
	if err != nil {
		return nil, err
	}
	perSource := map[string]int{}
	for _, lf := range landed {
		data, err := os.ReadFile(lf.Path)
		if err != nil {
			return nil, err
		}
		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		var hdr struct {
			Src string `json:"src"`
		}
		_ = json.Unmarshal([]byte(lines[0]), &hdr)
		perSource[hdr.Src] += len(lines) - 2
	}
	var out2 []string
	err = filepath.WalkDir(source, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return err
		}
		rel, _ := filepath.Rel(source, path)
		rel = filepath.ToSlash(rel)
		// The plugin's lines land through their own adapter, whose root is
		// plugins/data, so their landed source is relative to that.
		rel = strings.TrimPrefix(rel, "plugins/data/")
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		n := 0
		for _, l := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(l) != "" {
				n++
			}
		}
		if strings.Contains(rel, "not-a-uuid") {
			if perSource[rel] != 0 {
				out2 = append(out2, "every_line_a_record: "+rel+" is not a session's file and was landed")
			}
			return nil
		}
		if perSource[rel] != n {
			out2 = append(out2, fmt.Sprintf("every_line_a_record: %s has %d lines and %d landed records", rel, n, perSource[rel]))
		}
		return nil
	})
	return out2, err
}

// discoveryIgnoresNoise checks the adapter discovers the scenario's session
// and nothing else, whatever else the project directory holds.
func discoveryIgnoresNoise(out, session string) ([]string, error) {
	sessions, err := claudecode.Discover(filepath.Join(out, "_source"))
	if err != nil {
		return nil, err
	}
	var lines []string
	found := false
	for _, s := range sessions {
		if s.ID == session {
			found = true
		} else {
			lines = append(lines, "discovery_ignores_noise: discovered "+s.ID+", which is not the scenario's session")
		}
		for _, src := range s.Sources {
			if strings.Contains(src.Rel, "not-a-uuid") || strings.Contains(src.Rel, "memory/") {
				lines = append(lines, "discovery_ignores_noise: collected "+src.Rel+", which is not part of any session")
			}
		}
	}
	if !found {
		lines = append(lines, "discovery_ignores_noise: the session "+session+" was not discovered")
	}
	return lines, nil
}

// repackKeepsStructure re-cuts the root under the smallest budget into a twin,
// parses the twin, and checks the records and the fold are the same.
func repackKeepsStructure(out, session string, maxRound int64) ([]string, error) {
	twin := out + "-repack"
	defer os.RemoveAll(twin)
	src, dst := storage.NewZone(out), storage.NewZone(twin)
	// A one-byte budget cuts before every record after the first, so every
	// stream of two or more records is re-cut and every reference moves.
	st, err := repack.Session(src, dst, session, 1, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		return nil, err
	}
	// A session whose every file holds one record cannot be cut further;
	// any other must come out in more files than it went in.
	if st.Records > st.FilesIn && st.FilesOut <= st.FilesIn {
		return []string{fmt.Sprintf("repack_keeps_structure: a one-byte budget re-cut %d files into %d; the check exercised nothing", st.FilesIn, st.FilesOut)}, nil
	}
	if _, err := parseAll(twin, session, maxRound); err != nil {
		return nil, err
	}
	lines, err := expect.RecordsAgree(out, twin, session)
	if err != nil {
		return nil, err
	}
	for i := range lines {
		lines[i] = "repack_keeps_structure: " + strings.TrimPrefix(lines[i], "records_match: ")
	}
	a, err := expect.Summarize(out, session)
	if err != nil {
		return nil, err
	}
	b, err := expect.Summarize(twin, session)
	if err != nil {
		return nil, err
	}
	for _, d := range expect.Compare(a, b) {
		lines = append(lines, "repack_keeps_structure: "+d)
	}
	return lines, nil
}

func names(p *scenario.Plan) map[string]string {
	out := map[string]string{"main": "main"}
	for _, s := range p.Streams {
		out[s.Label] = s.ID
	}
	return out
}

// expectedTokens is what the plan says the runtime's exporter would have
// counted: every call's usage once, whatever its fragments repeat, under
// the stream that made it, main or a child. A lost call left no record, a
// lost stream left no file, and a replayed copy is the same call again.
func expectedTokens(p *scenario.Plan) map[string]int64 {
	lost := map[string]bool{}
	for _, s := range p.Streams {
		if s.Lost {
			lost[s.ID] = true
		}
	}
	seen := map[string]bool{}
	out := map[string]int64{}
	for i := range p.Events {
		e := &p.Events[i]
		if e.Kind != scenario.EvFragment || e.Lost || lost[e.Stream] || seen[e.Call] {
			continue
		}
		seen[e.Call] = true
		source := metrics.SourceSubagent
		if e.Stream == "main" {
			source = metrics.SourceMain
		}
		for typ, n := range map[string]int{
			metrics.TypeInput: e.Usage.In, metrics.TypeOutput: e.Usage.Out,
			metrics.TypeCacheRead: e.Usage.CacheRead, metrics.TypeCacheCreation: e.Usage.CacheWrite,
		} {
			if n > 0 {
				out[source+"/"+typ] += int64(n)
			}
		}
	}
	return out
}
