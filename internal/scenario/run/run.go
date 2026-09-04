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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
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
	for _, f := range formats {
		s, err := checkFormat(sc, ex, f, filepath.Join(out, string(f)), opts, rep)
		if err != nil {
			return rep, err
		}
		summaries[f] = s
	}
	if expect.On(ex.Properties.CrossFormat) && len(formats) > 1 {
		base := summaries[formats[0]]
		for _, f := range formats[1:] {
			if base == nil || summaries[f] == nil {
				continue
			}
			for _, d := range expect.Compare(base, summaries[f]) {
				rep.fail("%s against %s: %s", formats[0], f, d)
			}
		}
		if !rep.Failed {
			rep.say("cross-format: the folds agree")
		}
	}
	if !rep.Failed && opts.Out == "" {
		_ = os.RemoveAll(out)
	}
	return rep, nil
}

// checkFormat builds through every checkpoint, collects when the format
// needs it, parses, and evaluates; at the end it runs the properties.
func checkFormat(sc *scenario.Scenario, ex *expect.File, f scenario.Format, out string, opts Options, rep *Report) (*expect.Summary, error) {
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
			return nil, err
		}
		if err := collect(f, out); err != nil {
			return nil, err
		}
		round, err = parseAll(out, built.Session, ex.Parse.MaxRoundBytes)
		if err != nil {
			rep.fail("%s %s: parse: %v", f, name, err)
			return nil, nil
		}
		want := ex.Checkpoints[name]
		if want == nil {
			want = &expect.Checkpoint{}
		}
		ctx := &expect.Context{Session: built.Session, Names: names(built.Plan), At: opts.At, Round: round}
		problems, err := expect.Evaluate(out, want, ctx)
		if err != nil {
			return nil, err
		}
		if want.Empty() {
			d, err := expect.Describe(out, built.Session)
			if err != nil {
				return nil, err
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
	if err := props("unchanged_writes_nothing", nil, func() ([]string, error) {
		r, err := parse.Session(storage.NewZone(out), parse.Options{Conversation: session, Session: session, MaxRoundBytes: ex.Parse.MaxRoundBytes})
		if err != nil {
			return nil, err
		}
		if r.Changed() {
			return []string{fmt.Sprintf("unchanged_writes_nothing: a parse with no new evidence wrote round %d", r.Number)}, nil
		}
		return nil, nil
	}); err != nil {
		return nil, err
	}
	if err := props("header_matches_fold", ex.Properties.HeaderMatchesFold, func() ([]string, error) { return expect.HeaderMatchesFold(out, session) }); err != nil {
		return nil, err
	}
	if err := props("fold_equals_parse", ex.Properties.FoldEqualsParse, func() ([]string, error) { return expect.FoldEqualsParse(out, session, session) }); err != nil {
		return nil, err
	}
	if err := props("immutable_rounds", ex.Properties.ImmutableRounds, func() ([]string, error) { return expect.ImmutableRounds(out, session) }); err != nil {
		return nil, err
	}
	if f == scenario.FormatClaudeCode {
		if err := props("recollect_idempotent", ex.Properties.RecollectIdempotent, func() ([]string, error) {
			st, err := claudecode.New(filepath.Join(out, "_source"), storage.NewZone(out), 0).CollectAll(nil)
			if err != nil {
				return nil, err
			}
			if st.Records != 0 || st.SourcesLanded != 0 {
				return []string{fmt.Sprintf("recollect_idempotent: a second collect landed %d records from %d sources", st.Records, st.SourcesLanded)}, nil
			}
			return nil, nil
		}); err != nil {
			return nil, err
		}
	}
	if err := props("reproducible", ex.Properties.Reproducible, func() ([]string, error) {
		// Two parses of identical landed evidence must produce identical
		// rounds. The chain in out was cut at checkpoints, so it is not the
		// comparison; two fresh copies of the landed files are.
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
	}); err != nil {
		return nil, err
	}
	summary, err := expect.Summarize(out, session)
	if err != nil {
		return nil, err
	}
	// The bundle check removes the index and state, so it runs last.
	if err := props("bundle", ex.Properties.Bundle, func() ([]string, error) { return expect.Bundle(out, session, session) }); err != nil {
		return nil, err
	}
	return summary, nil
}

// collect lands a runtime format's source through its adapter. An sd build
// is landed already.
func collect(f scenario.Format, out string) error {
	if f != scenario.FormatClaudeCode {
		return nil
	}
	st, err := claudecode.New(filepath.Join(out, "_source"), storage.NewZone(out), 0).CollectAll(nil)
	if err != nil {
		return err
	}
	if len(st.Errors) != 0 {
		return fmt.Errorf("collect: %v", st.Errors)
	}
	return nil
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

func names(p *scenario.Plan) map[string]string {
	out := map[string]string{"main": "main"}
	for _, s := range p.Streams {
		out[s.Label] = s.ID
	}
	return out
}
