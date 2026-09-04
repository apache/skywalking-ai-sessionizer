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
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario/run"
)

const scenarioUsage = `asz scenario build FILE --format {claude-code|sd} --out DIR [--at TIME] [--scale FACTOR] [--repeat N] [--through CHECKPOINT]
asz scenario check FILE [--format {claude-code|sd|all}] [--out DIR] [--at TIME] [--scale FACTOR]

build writes a scenario's input into DIR and stops: for claude-code, the runtime's own files under
DIR/_source for asz collect to land; for sd, Session Data landed into DIR as a storage root. It also
writes DIR/asz.yaml, so the ordinary commands finish the job:

  asz collect -once -config DIR/asz.yaml
  asz parse -config DIR/asz.yaml
  asz view -config DIR/asz.yaml

check builds, collects and parses at every checkpoint, compares the fold with FILE's expectation
file (NAME.expect.yaml), runs the properties every chain must have, and compares the formats with
each other. It exits non-zero on any failure and keeps its directory when one is given.

  --at TIME        the base time, RFC 3339, or now (default now; check defaults to 2026-01-01)
  --scale FACTOR   multiplies every delta (default 1)
  --interval D     overrides the scenario's interval, the gap between steps
  --repeat N       build N sessions end to end on the clock (build only)
  --through NAME   build only the steps up to this checkpoint (build only)
`

// cmdScenario handles both subcommands. It is dispatched before the common
// flags are parsed, because its flags are its own.
func cmdScenario(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, scenarioUsage)
		return 2
	}
	sub := args[0]
	fs := flag.NewFlagSet("scenario "+sub, flag.ExitOnError)
	format := fs.String("format", "", "writer: claude-code or sd; check: all")
	out := fs.String("out", "", "the output directory")
	at := fs.String("at", "now", "the base time, RFC 3339, or now")
	scale := fs.Float64("scale", 1, "multiplies every delta")
	interval := fs.Duration("interval", 0, "overrides the scenario's interval")
	repeat := fs.Int("repeat", 1, "build N sessions end to end")
	through := fs.String("through", "", "build only through this checkpoint")
	fs.Usage = func() { fmt.Fprint(os.Stderr, scenarioUsage) }
	// The file may come before the flags, as the usage shows, or after
	// them: parse, take the file, and parse what follows it.
	_ = fs.Parse(args[1:])
	file := fs.Arg(0)
	if file != "" && fs.NArg() > 1 {
		_ = fs.Parse(fs.Args()[1:])
	}
	if file == "" {
		fmt.Fprint(os.Stderr, scenarioUsage)
		return 2
	}
	base := time.Time{}
	if *at != "now" && *at != "" {
		t, err := time.Parse(time.RFC3339Nano, *at)
		if err != nil {
			fmt.Fprintf(os.Stderr, "asz: --at %q is not RFC 3339\n", *at)
			return 2
		}
		base = t
	}
	switch sub {
	case "build":
		if *format == "" || *out == "" {
			fmt.Fprintln(os.Stderr, "asz: scenario build needs --format and --out")
			return 2
		}
		sc, err := scenario.Load(file)
		if err != nil {
			fmt.Fprintln(os.Stderr, "asz:", err)
			return 1
		}
		opts := scenario.Options{At: base, Scale: *scale, Interval: *interval, Through: *through}
		if opts.At.IsZero() {
			opts.At = time.Now()
		}
		for k := 1; k <= *repeat; k++ {
			one := *sc
			if *repeat > 1 {
				one.Session = repeatedSession(sc, k)
			}
			b, err := scenario.Build(&one, scenario.Format(*format), *out, opts)
			if err != nil {
				fmt.Fprintln(os.Stderr, "asz:", err)
				return 1
			}
			for _, f := range b.Files {
				fmt.Println(f)
			}
			fmt.Printf("session  : %s (%d records)\n", b.Session, b.Events)
			// The next session begins one interval after the last record of
			// this one.
			last := opts.At
			for _, e := range b.Plan.Events {
				if e.At.After(last) {
					last = e.At
				}
			}
			gap := *interval
			if gap == 0 {
				gap = sc.Interval
			}
			if gap == 0 {
				gap = time.Second
			}
			opts.At = last.Add(gap)
			if k == *repeat {
				fmt.Printf("out      : %s\nconfig   : %s\n", b.Out, b.Config)
			}
		}
		return 0
	case "check":
		var formats []scenario.Format
		if *format != "" && *format != "all" {
			formats = []scenario.Format{scenario.Format(*format)}
		}
		rep, err := run.Check(file, run.Options{Formats: formats, Out: *out, At: base, Scale: *scale, Interval: *interval})
		if rep != nil {
			for _, l := range rep.Lines {
				fmt.Println(l)
			}
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "asz:", err)
			return 1
		}
		if rep.Failed {
			fmt.Printf("FAILED; files kept in %s\n", rep.Out)
			return 1
		}
		fmt.Println("ok")
		return 0
	default:
		fmt.Fprint(os.Stderr, scenarioUsage)
		return 2
	}
}

// repeatedSession names the k-th session of a repeated build. A named
// session gets a suffix; an unnamed one stays in the UUID shape discovery
// requires, varied in its last group.
func repeatedSession(sc *scenario.Scenario, k int) string {
	if sc.Session != "" {
		return fmt.Sprintf("%s-%d", sc.Session, k)
	}
	p, err := sc.Plan(scenario.Options{At: time.Unix(0, 0)})
	if err != nil {
		return fmt.Sprintf("scenario-%d", k)
	}
	id := p.Session
	tail := fmt.Sprintf("%012x", k)
	return id[:len(id)-12] + strings.ToLower(tail[len(tail)-12:])
}
