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
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario/run"
)

const scenarioUsage = `asz scenario build FILE... --format {claude-code|sd} --out DIR [--at TIME] [--scale FACTOR] [--repeat N] [--every D] [--pick {cycle|random}] [--seed N] [--through CHECKPOINT] [--remove {immediately|DURATION}]
asz scenario check FILE [--format {claude-code|sd|all}] [--out DIR] [--at TIME] [--scale FACTOR]

build writes a scenario's input into DIR and stops: for claude-code, the runtime's own files under
DIR/_source for asz collect to land; for sd, Session Data landed into DIR as a storage root. It also
writes DIR/asz.yaml, so the ordinary commands finish the job:

  asz collect -once -config DIR/asz.yaml
  asz server -config DIR/asz.yaml

More than one FILE may be given, and a FILE that names a directory contributes every .yaml file in
it, except the expectation files. With a set, --pick says which one each session comes from.

With --every, build does not stop. It writes one session every D of wall clock, each stamped so its
last record lands at the moment it was written, and each with an id no other session has. That is a
mock client: run it beside asz server, whose configuration the build leaves in watch mode, and
conversations keep arriving.

  asz scenario build tests/scenarios --format claude-code --out DIR --every 30s --pick random
  asz server -config DIR/asz.yaml

A claude-code build also records, for each session, when the pipeline may remove it. asz collect
and asz server remove a session once all of it has been sent to the configured receiver:
immediately, or once its last record is older than --remove. A session no build wrote is never
removed.

check builds, collects and parses at every checkpoint, compares the fold with FILE's expectation
file (NAME.expect.yaml), runs the properties every chain must have, and compares the formats with
each other. It exits non-zero on any failure and keeps its directory when one is given.

  --at TIME        the base time, RFC 3339, or now (default now; check defaults to 2026-01-01)
  --scale FACTOR   multiplies every delta (default 1)
  --interval D     overrides the scenario's interval, the gap between steps
  --repeat N       build N sessions end to end on the clock (build only)
  --every D        keep building, one session every D of wall clock (build only)
  --pick MODE      cycle through the scenarios in order, or random (default cycle)
  --seed N         seeds --pick random; 0 varies with each run (default 0)
  --through NAME   build only the steps up to this checkpoint (build only)
  --remove POLICY  immediately (default), or a duration such as 24h or 7d counted from the
                   session's last record: when the pipeline removes a sent session (claude-code build only)
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
	every := fs.Duration("every", 0, "keep building, one session every D of wall clock")
	pick := fs.String("pick", "cycle", "cycle or random, when more than one scenario is given")
	seed := fs.Int64("seed", 0, "seeds --pick random; 0 varies with each run")
	through := fs.String("through", "", "build only through this checkpoint")
	remove := fs.String("remove", scenario.PolicyImmediately, "immediately, or a duration such as 24h or 7d")
	fs.Usage = func() { fmt.Fprint(os.Stderr, scenarioUsage) }
	// A file may come before the flags, as the usage shows, or after them,
	// and there may be several: parse, take the file the parse stopped on,
	// and parse what follows it, until nothing is left.
	var files []string
	for rest := args[1:]; ; {
		_ = fs.Parse(rest)
		if fs.NArg() == 0 {
			break
		}
		files = append(files, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if len(files) == 0 {
		fmt.Fprint(os.Stderr, scenarioUsage)
		return 2
	}
	file := files[0]
	// Which flags a person actually wrote. --every with no --repeat runs
	// until it is stopped, which is what a mock client is for.
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
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
		if *pick != pickCycle && *pick != pickRandom {
			fmt.Fprintf(os.Stderr, "asz: --pick %q; use %s or %s\n", *pick, pickCycle, pickRandom)
			return 2
		}
		// Removal is a policy of the scenario, never an option of the
		// product: each session's marker records it, and a pipeline reads it
		// from there. An sd build writes no source and no marker.
		if scenario.Format(*format) == scenario.FormatSD && given["remove"] {
			fmt.Fprintln(os.Stderr, "asz: --remove applies to a claude-code build; an sd build writes no source, and its sessions are never removed")
			return 2
		}
		removal, err := scenario.ParseRemove(*remove)
		if err != nil {
			fmt.Fprintf(os.Stderr, "asz: --remove %q; use immediately, or a duration such as 30m, 24h or 7d\n", *remove)
			return 2
		}
		marks := scenario.Format(*format) == scenario.FormatClaudeCode
		set, err := scenario.LoadSet(files)
		if err != nil {
			fmt.Fprintln(os.Stderr, "asz:", err)
			return 1
		}
		next := chooser(set, *pick, *seed)
		// A count of zero runs until the command is stopped. Only a feed may
		// reach it: a build that does not sleep between sessions would spin.
		count := *repeat
		if *every > 0 && !given["repeat"] {
			count = 0
		}
		if count < 1 && *every <= 0 {
			fmt.Fprintln(os.Stderr, "asz: --repeat must be at least 1, or give --every to keep building")
			return 2
		}
		// A feed id carries the moment it was written, in milliseconds. A
		// period shorter than that cannot give every session an id of its
		// own once the command is stopped and started again, and the second
		// run would write over the first run's sessions without a word.
		if *every > 0 && *every < time.Millisecond {
			fmt.Fprintln(os.Stderr, "asz: --every must be at least 1ms")
			return 2
		}
		// Every id this run has written. Two scenarios whose ids sit next to
		// each other would otherwise collide once a repeat counted one onto
		// the other, and two emissions inside the same millisecond would
		// share a feed id. Either one rewrites a source file that was
		// already written, and the collector, which reads forward from where
		// it stopped, would land nothing for the second.
		used := map[string]bool{}
		opts := scenario.Options{At: base, Scale: *scale, Interval: *interval, Through: *through, Watch: *every > 0, Remove: removal}
		if opts.At.IsZero() {
			opts.At = time.Now()
		}
		if *every > 0 {
			fmt.Printf("out      : %s\nevery    : %s\nscenarios: %d, %s\n", *out, *every, len(set), *pick)
			if marks {
				fmt.Printf("remove   : %s\n", removal.Describe())
			}
		}
		for k := 1; count == 0 || k <= count; k++ {
			l := next(k)
			one := *l.Scenario
			switch {
			case *every > 0:
				// A session of a feed is stamped so its last record lands
				// now. A live session that has just ended has its last
				// record now, and a receiver reads nothing stamped ahead of
				// the clock it reads it with.
				probe, err := one.Plan(scenario.Options{At: time.Unix(0, 0).UTC(), Scale: *scale, Interval: *interval, Through: *through})
				if err != nil {
					fmt.Fprintln(os.Stderr, "asz:", err)
					return 1
				}
				now := time.Now()
				opts.At = now.Add(-probe.Span())
				one.Session = unusedSession(used, feedSession(baseSession(l.Scenario), now))
			case count > 1:
				one.Session = unusedSession(used, repeatedSession(l.Scenario, k))
			}
			b, err := scenario.Build(&one, scenario.Format(*format), *out, opts)
			if err != nil {
				fmt.Fprintln(os.Stderr, "asz:", err)
				return 1
			}
			if *every > 0 {
				fmt.Printf("[%s] %s: %s (%d records, %s)\n",
					time.Now().Format("15:04:05"), filepath.Base(l.Path), b.Session, b.Events, b.Plan.Span())
				// The period is the gap between sessions, so there is none
				// to wait after the last one a count asked for.
				if count == 0 || k < count {
					time.Sleep(*every)
				}
				continue
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
				gap = one.Interval
			}
			if gap == 0 {
				gap = time.Second
			}
			opts.At = last.Add(gap)
			if k == count {
				fmt.Printf("out      : %s\nconfig   : %s\n", b.Out, b.Config)
				if marks {
					fmt.Printf("remove   : %s\n", removal.Describe())
				}
			}
		}
		return 0
	case "check":
		// A check builds and removes its own copies. A policy for a pipeline
		// has nothing to act on here.
		if given["remove"] {
			fmt.Fprintln(os.Stderr, "asz: --remove is for scenario build")
			return 2
		}
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

// How a feed chooses the next scenario when it was given more than one.
const (
	pickCycle  = "cycle"
	pickRandom = "random"
)

// chooser returns the function that picks the scenario for the k-th
// session. cycle walks the set in the order it was given and starts again
// at the top, so a fixed list is emitted in a fixed order. random picks one
// each time; a seed of zero varies with each run, and any other seed
// repeats the same order, which is what reproducing a report needs.
func chooser(set []scenario.Loaded, mode string, seed int64) func(k int) scenario.Loaded {
	if mode == pickRandom {
		if seed == 0 {
			seed = time.Now().UnixNano()
		}
		// The choice decides which scenario to write, nothing more, so a
		// plain generator is right here.
		r := rand.New(rand.NewSource(seed)) // #nosec G404
		return func(int) scenario.Loaded { return set[r.Intn(len(set))] }
	}
	return func(k int) scenario.Loaded { return set[(k-1)%len(set)] }
}

// feedSession names one session of a feed. The last twelve hex digits of a
// UUID-shaped id become the moment the session was written, in
// milliseconds, and any other name takes that number as a suffix.
//
// A feed must never write an id it has written before, including across a
// stop and a start: the same id rewrites the same source file in place, and
// the collector, which reads forward from where it stopped, would see no
// growth and land nothing. Wall-clock milliseconds give that, where a
// counter that starts again at one does not.
func feedSession(id string, at time.Time) string {
	ms := at.UnixMilli()
	if !uuidShape.MatchString(id) {
		return fmt.Sprintf("%s-%d", id, ms)
	}
	return id[:len(id)-12] + fmt.Sprintf("%012x", uint64(ms)&0xffffffffffff)
}

// baseSession is the id a scenario names, or the one it derives from its
// steps when it names none.
func baseSession(sc *scenario.Scenario) string {
	if sc.Session != "" {
		return sc.Session
	}
	p, err := sc.Plan(scenario.Options{At: time.Unix(0, 0)})
	if err != nil {
		return "scenario"
	}
	return p.Session
}

// unusedSession is id, or the next id after it that this run has not
// written yet. It is what keeps a run from writing one id twice, which
// would rewrite a source file already written and land nothing new.
func unusedSession(used map[string]bool, id string) string {
	try := id
	for n := 1; used[try]; n++ {
		try = bumpSession(id, n)
	}
	used[try] = true
	return try
}

// bumpSession counts an id on by n. A session id in the UUID shape
// discovery requires stays in that shape, its last group counted up; any
// other name gets a suffix.
func bumpSession(id string, n int) string {
	if n == 0 {
		return id
	}
	if uuidShape.MatchString(id) {
		if v, err := strconv.ParseUint(id[len(id)-12:], 16, 64); err == nil {
			return id[:len(id)-12] + fmt.Sprintf("%012x", (v+uint64(n))&0xffffffffffff)
		}
	}
	return fmt.Sprintf("%s-%d", id, n+1)
}

// repeatedSession names the k-th session of a repeated build. The first is
// the scenario's own, and an unnamed session takes the derived id and
// varies the same way.
func repeatedSession(sc *scenario.Scenario, k int) string {
	return bumpSession(baseSession(sc), k-1)
}

var uuidShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
