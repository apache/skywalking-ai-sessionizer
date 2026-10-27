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
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/judge"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
)

const judgeUsage = `Usage:
  asz judge [-config FILE] [-round N] [-evaluator NAME | -plan STAGE=NAME,...] CONVERSATION
      judge a conversation at its current head, or at round N of its chain: the
      ledger, then each stage of the plan given to its evaluator, then the close
      file. The plan is judge.plan, every stage given to -evaluator, or -plan,
      such as targets=claude,achievement=codex,efficiency=claude
  asz judge [-config FILE] -reference SRC ... CONVERSATION
      judge it as above, and compare it with what each source concludes: a
      file, an http or https URL, or a source judge.references.fetch names
      a command or an MCP server for; repeat -reference for several
  asz judge [-config FILE] -reference-tools SRC
      list the tools of the MCP server judge.references.fetch names for the
      source, each with its input schema, to choose the entry's tool
  asz judge [-config FILE] -labels FILE ... CONVERSATION
      judge it as above, then regroup the run by the person's goals, a plain
      list in the file, and compare the judge's grouping with theirs
  asz judge [-config FILE] -regroup RUN -labels FILE [-evaluator NAME] CONVERSATION
      regroup a closed run by the person's goals, in a new version beside
      it: one call of the evaluator, or of the plan's targets stage, places
      each target in the goal it serves
  asz judge [-config FILE] -list CONVERSATION
      list the conversation's runs
  asz judge [-config FILE] -show RUN [-json] CONVERSATION
      print one run: its stages, then goal by goal each target's score out
      of 100, why, what was done, its rework and what it cost
  asz judge [-config FILE] [-pull] -remote-conversation REMOTE/CONVERSATION[:TAG] ...
      any of the above on a conversation asz pull copied here, in place of
      CONVERSATION: the snapshot with that tag, or the newest unexpired one
      without a tag or with :latest. -pull pulls the head first and judges
      what it pulled; it starts a new run, so it takes no tag, -list, -show
      or -regroup. -list without a tag lists the runs of every snapshot
  asz judge mcp -root DIR -conversation ID -round N -digest D [-hide FLAGS] [-ledger FILE] [-log FILE]
      the read-only navigation server an agent CLI evaluator is given

A run happens only when this command is run. What an evaluator reads goes to
the model behind it.
`

func cmdJudge(args []string) int {
	if len(args) > 0 && args[0] == "mcp" {
		return cmdJudgeMCP(args[1:])
	}
	fs := flag.NewFlagSet("judge", flag.ExitOnError)
	cfgPath := fs.String("config", "", "configuration file")
	evaluator := fs.String("evaluator", "", "give every stage to this evaluator")
	round := fs.Uint64("round", 0, "judge the conversation as it was at this round of its chain")
	planFlag := fs.String("plan", "", "stage=evaluator pairs, comma separated")
	list := fs.Bool("list", false, "list the conversation's runs")
	show := fs.Int("show", 0, "print this run")
	asJSON := fs.Bool("json", false, "with -show, print the stage files as JSON")
	from := fs.String("from", "", "judge the main talks that start at or after this time")
	to := fs.String("to", "", "judge the main talks that start before this time")
	fromID := fs.String("from-id", "", "judge the main talks from this talk id on")
	toID := fs.String("to-id", "", "judge the main talks up to this talk id")
	mode := fs.String("mode", "", "auto, single or stream: how each stage is given to its evaluator")
	var references repeated
	fs.Var(&references, "reference", "a source to compare the run with: a file, a URL, or one a configured command or MCP server fetches; repeat for several")
	referenceTools := fs.String("reference-tools", "", "list the tools of the MCP server configured for this source")
	labelsPath := fs.String("labels", "", "a file listing the person's goals in their own words: regroup the run by them")
	regroup := fs.Int("regroup", 0, "with -labels, regroup this closed run instead of making a new one")
	remoteConversation := fs.String("remote-conversation", "", "judge a pulled conversation: REMOTE/CONVERSATION, with :TAG or :latest after it")
	pullFirst := fs.Bool("pull", false, "with -remote-conversation, pull the conversation's head first and judge what was pulled")
	fs.Usage = func() { fmt.Fprint(os.Stderr, judgeUsage) }
	_ = fs.Parse(args)
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	usage := func(why string) int {
		fmt.Fprintln(os.Stderr, "asz judge:", why)
		return 2
	}
	if given["reference-tools"] {
		if fs.NArg() != 0 || len(given) != 1 && (len(given) != 2 || !given["config"]) {
			return usage("-reference-tools takes a source and -config, nothing else")
		}
		cfg, err := loadConfig(*cfgPath)
		if err != nil {
			return fail(err)
		}
		ctx, stop := stopping()
		defer stop()
		if err := judge.ReferenceTools(ctx, cfg.Judge.References, *referenceTools, version, os.Stdout); err != nil {
			return fail(err)
		}
		return 0
	}
	// A reference with a user or a password stops the command before
	// anything is pulled, read or written.
	if err := judge.CheckReferences(references); err != nil {
		return fail(err)
	}
	var selection *remoteSelection
	var id string
	switch {
	case *remoteConversation != "" && fs.NArg() != 0:
		return usage("give CONVERSATION or -remote-conversation, not both")
	case *remoteConversation != "":
		s, err := parseRemoteSelection(*remoteConversation)
		if err != nil {
			return usage(err.Error())
		}
		selection, id = s, s.conversation
	case fs.NArg() != 1:
		fs.Usage()
		return 2
	default:
		id = fs.Arg(0)
	}
	if *pullFirst {
		switch {
		case selection == nil:
			return usage("-pull needs -remote-conversation")
		case selection.tag != "":
			return usage("-pull takes the head: give no tag, or :latest")
		case *list || given["show"] || given["regroup"]:
			return usage("-pull starts a new run; -list, -show and -regroup select runs already made")
		}
	}
	if err := checkIdentity("conversation", id, false); err != nil {
		return fail(err)
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return fail(err)
	}
	root, err := cfg.ResolvedRoot()
	if err != nil {
		return fail(err)
	}
	// What a person reads names the conversation; a pulled one by its
	// remote and its snapshot's tag.
	display := id
	if selection != nil {
		// A pulled conversation is selected under the configured root,
		// which must be one, never a snapshot or a copy itself.
		if err := refuseCopyRoot("judge", root); err != nil {
			return fail(err)
		}
		local, err := pulledLocal(cfg, root)
		if err != nil {
			return fail(err)
		}
		// The copies here are swept first, as a pull sweeps them, and what
		// the sweep could not settle is said. One stop covers the sweep, a
		// pull and the admission: an interrupt in any of them starts
		// nothing after it.
		ctx, stop := stopping()
		if err := sweepPulled(ctx, local, selection); err != nil {
			stop()
			return fail(err)
		}
		if *list && !selection.suffixed {
			defer stop()
			return judgeListPulled(ctx, local, selection)
		}
		a, err := selection.admit(ctx, local, cfg, *pullFirst)
		stop()
		if err != nil {
			return fail(err)
		}
		admittedMu.Lock()
		admitted = a
		admittedMu.Unlock()
		root = a.Root.Path
		display = fmt.Sprintf("%s:%s", selection, a.Root.Tag)
	} else if _, err := admitRoot("judge", cfg, root); err != nil {
		return fail(err)
	}
	defer releaseAdmission()
	session := sessionOfChain(root, id)
	if admitted != nil {
		// A snapshot's marker names its session, as admission checked it.
		session = admitted.Marker.Session
	}
	if err := checkIdentity("session", session, true); err != nil {
		return fail(err)
	}
	switch {
	case *list:
		return judgeList(storage.NewZone(root), session, display, selection != nil)
	case *show > 0:
		// The stage files are printed as they are stored, with the source's
		// own conversation identifier; the snapshot is named before them.
		if selection != nil && !*asJSON {
			fmt.Println(display)
		}
		return judgeShow(storage.NewZone(root), session, display, *show, *asJSON)
	}
	// A label file is read before a run, so a run is not made for labels
	// that cannot be read.
	var labels *judge.Labels
	if *labelsPath != "" {
		if labels, err = judge.ReadLabels(*labelsPath, admittedRoot()); err != nil {
			return fail(err)
		}
	}
	if *regroup > 0 && labels == nil {
		return fail(errors.New("-regroup needs -labels"))
	}
	plan, err := judgePlan(cfg.Judge, *evaluator, *planFlag)
	if err != nil {
		return fail(err)
	}
	regrouper := regrouperOf(*evaluator, plan)
	plan = withReferences(plan, references, *evaluator)
	window, err := windowOf(*from, *to, *fromID, *toID)
	if err != nil {
		return fail(err)
	}
	switch *mode {
	case "":
	case judge.ModeAuto, judge.ModeSingle, judge.ModeStream:
		cfg.Judge.Mode = *mode
	default:
		return fail(fmt.Errorf("-mode %q: give auto, single or stream", *mode))
	}
	pricing, err := judge.LoadPricing(cfg.Judge.Pricing, admittedRoot())
	if err != nil {
		return fail(err)
	}
	self, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	zone := storage.NewZone(root)
	r := &judge.Runner{
		Zone: zone, Config: cfg.Judge, Pricing: pricing, Version: version, Window: window,
		Dialect: func(session string) string { return dialectOf(zone, session) },
		Decoder: decoderOf,
		Sources: references,
		MCP: func(a judge.MCPArgs) []string {
			cmd := []string{self, "judge", "mcp", "-root", root, "-conversation", a.Conversation,
				"-round", fmt.Sprint(a.Head.Round), "-digest", a.Head.Digest, "-log", a.Log}
			if len(cfg.Judge.Hide) > 0 {
				cmd = append(cmd, "-hide", strings.Join(cfg.Judge.Hide, ","))
			}
			if a.Ledger != "" {
				cmd = append(cmd, "-ledger", a.Ledger)
			}
			if a.ReadBytes > 0 {
				cmd = append(cmd, "-read-bytes", fmt.Sprint(a.ReadBytes))
			}
			if a.Task != "" {
				cmd = append(cmd, "-task", a.Task)
			}
			if admitted != nil {
				// The child is admitted by the run's lease, which binds it
				// to this run alone. Its log is made here, before it starts,
				// and it may only append to it; its failure report sits
				// beside the log, named for the lease.
				log, err := storage.CreateSupplied(a.Log, admittedRoot())
				if err == nil {
					err = log.Close()
				}
				if err != nil {
					fmt.Fprintf(os.Stderr, "asz: the navigation log %s: %v\n", a.Log, err)
				}
				cmd = append(cmd, "-parent-lease", admitted.Lease(), "-failure-report", a.Failure)
			}
			return append(cmd, windowArgs(a.Window)...)
		},
		Admitted: admittedRoot(),
		Log:      func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) },
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if admitted != nil {
		r.Reserved = func(number int) error { return admitted.BindRun(ctx, fmt.Sprintf("j%06d", number)) }
		r.Failures = func() string { return leaseRandom(admitted.Lease()) }
		if r.Snapshot, err = snapshotView(nil); err != nil {
			return fail(err)
		}
	}
	if *regroup > 0 {
		return judgeRegroup(ctx, r, session, id, *regroup, labels, regrouper)
	}
	run, err := r.Run(ctx, id, *round, plan)
	if err != nil {
		return fail(err)
	}
	fmt.Printf("run %d of %s at round %d: %s\n", run.Number, display, run.Head.Round, run.Status)
	if labels != nil {
		if code := judgeRegroup(ctx, r, session, id, run.Number, labels, regrouper); code != 0 {
			return code
		}
	}
	return judgeShow(zone, session, display, run.Number, false)
}

// judgeRegroup regroups a closed run by a person's goals and says how the
// judge's grouping compared.
func judgeRegroup(ctx context.Context, r *judge.Runner, session, id string, number int, labels *judge.Labels, evaluator string) int {
	runs, err := storage.JudgmentRuns(r.Zone, session)
	if err != nil {
		return fail(err)
	}
	for _, run := range runs {
		if run.Run != number {
			continue
		}
		// On a pulled snapshot the lease is bound to the selected run before
		// any evaluator works on it; a run that has its lease already keeps it.
		if admitted != nil && admitted.Run() != fmt.Sprintf("j%06d", number) {
			if err := admitted.BindRun(ctx, fmt.Sprintf("j%06d", number)); err != nil {
				return fail(err)
			}
		}
		path, a, err := r.Regroup(ctx, id, run, labels, evaluator)
		if err != nil {
			return fail(err)
		}
		var classes []string
		for _, class := range sessionjudge.Compares {
			if n := a.Classes[class]; n > 0 && class != sessionjudge.CompareAgree {
				classes = append(classes, fmt.Sprintf("%s %d", class, n))
			}
		}
		line := fmt.Sprintf("run %d regrouped by %s: the judge agreed on %d of %s", number, labels.File, a.Classes[sessionjudge.CompareAgree], plural(a.Goals, "goal"))
		if len(classes) > 0 {
			line += "; " + strings.Join(classes, ", ")
		}
		fmt.Printf("%s\n  %s\n", line, path)
		return 0
	}
	return fail(fmt.Errorf("%s has no run %d", id, number))
}

// regrouperOf is the evaluator a regroup takes: the one named, or the
// targets stage's, since a regroup places targets as that stage found them.
func regrouperOf(evaluator string, plan []config.JudgeStage) string {
	for _, p := range plan {
		if evaluator == "" && p.Stage == sessionjudge.StageTargets {
			return p.Evaluator
		}
	}
	return evaluator
}

// judgePlan is the plan a run takes: -plan, every stage to -evaluator, or the
// configured one.
func judgePlan(conf config.Judge, evaluator, plan string) ([]config.JudgeStage, error) {
	switch {
	case plan != "":
		var out []config.JudgeStage
		for _, pair := range strings.Split(plan, ",") {
			stage, name, ok := strings.Cut(strings.TrimSpace(pair), "=")
			if !ok {
				return nil, fmt.Errorf("-plan: %q is not stage=evaluator", pair)
			}
			out = append(out, config.JudgeStage{Stage: stage, Evaluator: name})
		}
		return out, nil
	case evaluator != "":
		return []config.JudgeStage{{Stage: sessionjudge.StageTargets, Evaluator: evaluator},
			{Stage: sessionjudge.StageAchievement, Evaluator: evaluator}, {Stage: sessionjudge.StageSubtasks, Evaluator: evaluator},
			{Stage: sessionjudge.StageEfficiency, Evaluator: evaluator}}, nil
	}
	return conf.Plan, nil
}

// repeated is a flag given as often as needed, in the order given.
type repeated []string

func (r *repeated) String() string     { return strings.Join(*r, ",") }
func (r *repeated) Set(v string) error { *r = append(*r, v); return nil }

// withReferences adds to a plan the stages that read the sources a run is
// compared with, when it is given any and lists neither: each given to
// -evaluator, or to the evaluator of the plan's last stage.
func withReferences(plan []config.JudgeStage, sources []string, evaluator string) []config.JudgeStage {
	if len(sources) == 0 || len(plan) == 0 || slices.ContainsFunc(plan, func(p config.JudgeStage) bool {
		return p.Stage == sessionjudge.StageReference || p.Stage == sessionjudge.StageDivergence
	}) {
		return plan
	}
	if evaluator == "" {
		evaluator = plan[len(plan)-1].Evaluator
	}
	return append(slices.Clone(plan), config.JudgeStage{Stage: sessionjudge.StageReference, Evaluator: evaluator},
		config.JudgeStage{Stage: sessionjudge.StageDivergence, Evaluator: evaluator})
}

// sessionOfChain is the session a conversation's chain was assembled from,
// whose directory keeps its judge runs: the session round 1 names, or the
// conversation's own id when it has no round.
func sessionOfChain(root, id string) string {
	files, err := sessionflow.OpenChain(root, id).List()
	if err != nil || len(files) == 0 {
		return id
	}
	r, err := sessionflow.OpenChain(root, id).Open(files[0].Path)
	if err != nil || r.Header.Session == "" {
		return id
	}
	return r.Header.Session
}

func judgeList(zone *storage.Zone, session, id string, named bool) int {
	runs, err := storage.JudgmentRuns(zone, session)
	if err != nil {
		return fail(err)
	}
	if len(runs) == 0 {
		fmt.Printf("%s has no judge runs\n", id)
		return 0
	}
	// A pulled conversation's runs are named by their snapshot, as a
	// person reads them.
	prefix := ""
	if named {
		prefix = id + " "
	}
	for _, run := range runs {
		fmt.Printf("%srun %d  %s\n", prefix, run.Run, runLine(run))
	}
	return 0
}

// runLine is what a listing says of a run after its name.
func runLine(run storage.JudgmentRun) string {
	closeFile, ok := readClose(run)
	if !ok {
		return fmt.Sprintf("unfinished, %d stage files", len(run.Files))
	}
	h := closeFile.Header
	s := h.Summary
	line := fmt.Sprintf("round %d  %s  %s  targets %d: achieved %d, partly %d, not %d, cannot verify %d",
		h.Head.Round, h.Finished, h.Status, s.Targets, s.Achieved, s.Partly, s.NotAchieved, s.CannotVerify)
	if s.Score != nil && s.Score.Final != nil {
		line += "  score " + scoreOf(s.Score.Final)
	}
	return line
}

func readClose(run storage.JudgmentRun) (*sessionjudge.File, bool) {
	for _, f := range run.Files {
		if f.Stage != sessionjudge.StageClose {
			continue
		}
		file, err := os.Open(f.Path)
		if err != nil {
			return nil, false
		}
		defer file.Close()
		sj, err := sessionjudge.Read(file)
		return sj, err == nil
	}
	return nil, false
}

func judgeShow(zone *storage.Zone, session, id string, number int, asJSON bool) int {
	runs, err := storage.JudgmentRuns(zone, session)
	if err != nil {
		return fail(err)
	}
	for _, run := range runs {
		if run.Run != number {
			continue
		}
		var files []*sessionjudge.File
		for _, f := range run.Files {
			sj, err := readJudgment(f.Path)
			if err != nil {
				return fail(err)
			}
			files = append(files, sj)
		}
		// The latest regroup that compared anything comes after the run's
		// stages, and the report follows the person's goals in it. A later
		// one that failed comes after it, so its failure shows.
		var regroups []*sessionjudge.File
		for i := len(run.Regroups) - 1; i >= 0; i-- {
			sj, err := readJudgment(run.Regroups[i].Path)
			if err != nil {
				return fail(err)
			}
			regroups = append([]*sessionjudge.File{sj}, regroups...)
			if sj.Header.Status != sessionjudge.StatusFailed {
				break
			}
		}
		files = append(files, regroups...)
		if asJSON {
			out, _ := json.MarshalIndent(files, "", "  ")
			fmt.Println(string(out))
			return 0
		}
		printRun(run, files)
		return 0
	}
	return fail(fmt.Errorf("%s has no run %d", id, number))
}

// readJudgment reads one judgment file.
func readJudgment(path string) (*sessionjudge.File, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	sj, err := sessionjudge.Read(file)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return sj, nil
}

// stageUsage says what one stage consumed: computed by asz, no call when it
// had nothing to answer, unknown when its evaluator did not say.
func stageUsage(h sessionjudge.Header) string {
	usage := "usage unknown"
	switch {
	case h.Evaluator.Kind == sessionjudge.EvaluatorComputed:
		usage = "computed"
	case h.Chunks == 0 && h.Attempts == 0:
		usage = "no call, nothing to answer"
	case h.Usage != nil:
		usage = fmt.Sprintf("in %d, out %d, cache read %d, cache write %d", h.Usage.Input, h.Usage.Output, h.Usage.CacheRead, h.Usage.CacheWrite)
	}
	if h.Requests != nil && h.Requests.Unknown > 0 {
		usage += fmt.Sprintf("; %d requests with usage unknown, not counted", h.Requests.Unknown)
	}
	if h.ReportedCostUSD != nil {
		usage += fmt.Sprintf(", reported %.2f USD", *h.ReportedCostUSD)
	}
	return usage
}

func printRun(run storage.JudgmentRun, files []*sessionjudge.File) {
	fmt.Printf("run %d  %s\n", run.Run, run.Dir)
	var closeFile, subtaskFile, regroup *sessionjudge.File
	statements := map[string]string{}
	for _, f := range files {
		h := f.Header
		model := ""
		if h.Evaluator.Model != "" {
			model = " (" + h.Evaluator.Model + ")"
		}
		fmt.Printf("  %-12s %-9s by %s%s, %s%s\n", h.Stage, h.Status, h.Evaluator.Name, model, stageUsage(h), noteOf(h.Note))
		for _, t := range f.Targets {
			statements[t.ID] = t.Statement
		}
		switch h.Stage {
		case sessionjudge.StageClose:
			closeFile = f
		case sessionjudge.StageSubtasks:
			subtaskFile = f
		case sessionjudge.StageRegroup:
			if h.Status != sessionjudge.StatusFailed {
				regroup = f
			}
		}
	}
	if closeFile == nil {
		fmt.Println("  not closed")
		return
	}
	// A target's lines are its outcome, which asz priced from the ledger:
	// shares of a talk two targets used are fractions until the outcome
	// rounds them once, so the targets add up to what they cost.
	type alloc struct{ calls, unknown, partial, unpriced float64 }
	cost := map[string]*alloc{}
	work := map[string]map[string]float64{}
	for _, c := range closeFile.Costs {
		share := 1.0
		if c.Share != nil {
			share = *c.Share
		}
		switch c.Scope {
		case sessionjudge.ScopeTarget, sessionjudge.ScopeShared:
			a := cost[c.Target]
			if a == nil {
				a = &alloc{}
				cost[c.Target] = a
			}
			a.calls += float64(c.Calls) * share
			a.unknown += float64(c.Unknown) * share
			a.partial += float64(c.OutputUnknown) * share
			a.unpriced += float64(c.Unpriced) * share
		case sessionjudge.ScopeWork:
			if work[c.Target] == nil {
				work[c.Target] = map[string]float64{}
			}
			work[c.Target][c.Work] += float64(c.Usage.Input+c.Usage.Output+c.Usage.CacheRead+c.Usage.CacheWrite) * share
		}
	}
	s := closeFile.Header.Summary
	if s == nil {
		s = &sessionjudge.Summary{}
	}
	costLines := func(o *sessionjudge.Outcome) {
		c := cost[o.Target]
		if c == nil {
			c = &alloc{}
		}
		fmt.Printf("        cost      %s calls, %s, observed %s\n", share(c.calls), amount(o.Cost, s.Currency), minutes(o.Span))
		// The counts are shares, as the cost's calls are: a call two targets
		// share is half of each, and a half is never shown as none.
		var notes []string
		if c.unknown > 0 {
			notes = append(notes, share(c.unknown)+" calls with usage unknown")
		}
		if c.partial > 0 {
			notes = append(notes, share(c.partial)+" calls that never finished, their output unknown")
		}
		if c.unpriced > 0 {
			notes = append(notes, share(c.unpriced)+" calls unpriced")
		}
		if len(notes) > 0 {
			fmt.Printf("                  %s\n", strings.Join(notes, "; "))
		}
		if o.Avoidable != nil {
			line := fmt.Sprintf("                  avoidable %s and %.1f min", amount(*o.Avoidable, s.Currency), float64(*o.AvoidableSpan)/60000)
			// The part sub-tasks caused by their own work going wrong; the
			// rest is the main agent's.
			if b := o.AvoidableBySubtasks; b != nil && b.Tokens() > 0 {
				line += fmt.Sprintf(", of which sub-tasks caused %s", amount(*b, s.Currency))
			}
			if u := o.AvoidableCauseUnknown; u != nil && u.Tokens() > 0 {
				line += fmt.Sprintf(", %s of a cause the record does not show", amount(*u, s.Currency))
			}
			fmt.Printf("%s; not reviewed %s\n", line, amount(*o.NotReviewed, s.Currency))
		}
		if w := work[o.Target]; len(w) > 0 {
			var parts []string
			for _, label := range sessionjudge.Works {
				if n, ok := w[label]; ok {
					parts = append(parts, fmt.Sprintf("%s %s", label, tokensOf(n)))
				}
			}
			fmt.Printf("                  work: %s\n", strings.Join(parts, "; "))
		}
	}
	printGoals(gather(files, closeFile, regroup), costLines)
	printSubtasks(subtaskFile, closeFile)
	printScore(s, closeFile.Outcomes)
}

// printSubtasks says in one line how the sub-tasks went.
func printSubtasks(f, closeFile *sessionjudge.File) {
	if f == nil || f.Header.Coverage == nil || f.Header.Coverage.Subtasks == nil {
		return
	}
	count := func(get func(sessionjudge.Subtask) string, order []string) string {
		n := map[string]int{}
		for _, st := range f.Subtasks {
			n[get(st)]++
		}
		var parts []string
		for _, k := range order {
			if n[k] > 0 {
				parts = append(parts, fmt.Sprintf("%s %d", strings.ReplaceAll(k, "_", " "), n[k]))
			}
		}
		return strings.Join(parts, ", ")
	}
	fmt.Printf("\n  sub-tasks      %d of %d judged", *f.Header.Coverage.SubtasksJudged, *f.Header.Coverage.Subtasks)
	if len(f.Subtasks) > 0 {
		fmt.Printf(": %s; reports %s; use %s; dispatch %s",
			count(func(s sessionjudge.Subtask) string { return s.Outcome }, sessionjudge.Levels),
			count(func(s sessionjudge.Subtask) string { return s.Report }, sessionjudge.Reports),
			count(func(s sessionjudge.Subtask) string { return s.Use }, sessionjudge.Uses),
			count(func(s sessionjudge.Subtask) string { return s.Dispatch }, sessionjudge.Dispatches))
	}
	fmt.Println()
	// The sub-tasks whose own work going wrong cost avoidable work, their
	// own and the asking agent's on their reports, costliest first.
	caused := map[string]float64{}
	for _, c := range closeFile.Costs {
		if c.Scope != sessionjudge.ScopeWork || c.Cause == "" || c.Cause == sessionjudge.CauseMain || c.Cause == sessionjudge.CauseUnknown {
			continue
		}
		share := 1.0
		if c.Share != nil {
			share = *c.Share
		}
		caused[c.Cause] += float64(c.Usage.Input+c.Usage.Output+c.Usage.CacheRead+c.Usage.CacheWrite) * share
	}
	ids := slices.Collect(maps.Keys(caused))
	sort.Slice(ids, func(i, j int) bool {
		return caused[ids[i]] > caused[ids[j]] || caused[ids[i]] == caused[ids[j]] && ids[i] < ids[j]
	})
	for i, id := range ids {
		if i == 5 {
			fmt.Printf("                 and %d more\n", len(ids)-5)
			break
		}
		fmt.Printf("                 %s caused %s of avoidable work\n", id, tokensOf(caused[id]))
	}
}

// verdict says in one line what a target came to and what it rests on.
func verdict(o sessionjudge.Outcome) string {
	if o.Level == "" {
		return "not judged, " + o.Status
	}
	v := strings.ReplaceAll(o.Level, "_", " ")
	if o.Criteria != nil {
		v += fmt.Sprintf(": %d of %d criteria met", *o.Met, *o.Criteria)
		if o.Unknown != nil && *o.Unknown > 0 {
			v += fmt.Sprintf(", %d unknown", *o.Unknown)
		}
	}
	if o.Basis != "" {
		v += ", on " + strings.ReplaceAll(o.Basis, "_", " ")
	}
	return v + "; " + o.Status
}

// amount prints tokens, and money where any call in the amount was priced.
func amount(a sessionjudge.Amount, currency string) string {
	out := tokensOf(float64(a.Tokens())) + " tokens"
	if a.Money != nil {
		out += fmt.Sprintf(", %.2f %s", *a.Money, currency)
	}
	return out
}

// printScore prints the run's two scores, never combined, and what the
// conversation and the judging cost.
func printScore(s *sessionjudge.Summary, outcomes []sessionjudge.Outcome) {
	sc := s.Score
	if sc == nil {
		return
	}
	pct := func(f *float64) string {
		if f == nil {
			return "unavailable"
		}
		return fmt.Sprintf("%.0f%%", *f*100)
	}
	// The levels counted are those of the targets the score is over: a
	// cancelled or replaced target is not scored, and is named apart.
	levels, unscored := map[string]int{}, 0
	for _, o := range outcomes {
		if o.Status == sessionjudge.TargetCancelled || o.Status == sessionjudge.TargetReplaced {
			unscored++
			continue
		}
		levels[o.Level]++
	}
	fmt.Println("\n  score")
	if sc.Achievement != nil {
		line := fmt.Sprintf("    achievement  %s over %d targets judged: achieved %d, partly %d, not achieved %d; %d cannot be verified",
			scoreOf(sc.Achievement), sc.Judged, levels[sessionjudge.LevelAchieved], levels[sessionjudge.LevelPartly],
			levels[sessionjudge.LevelNotAchieved], levels[sessionjudge.LevelCannotVerify])
		if unscored > 0 {
			line += fmt.Sprintf("; %d cancelled or replaced, not scored", unscored)
		}
		fmt.Println(line)
	} else {
		fmt.Printf("    achievement  unavailable: no target could be verified (%d cannot be verified)\n", levels[sessionjudge.LevelCannotVerify])
	}
	// The final score applies what contradicted or undid each delivered
	// result; the record's mean is beside it when the two differ.
	if sc.Final != nil {
		line := fmt.Sprintf("    final        %s over the targets scored", scoreOf(sc.Final))
		if sc.Achievement != nil && scoreOf(sc.Achievement) != scoreOf(sc.Final) {
			line += fmt.Sprintf(", %s by the record", strings.TrimSuffix(scoreOf(sc.Achievement), "/100"))
		}
		fmt.Println(line)
	}
	if sc.Efficiency != nil {
		fmt.Printf("    efficiency   %s: avoidable %s of the reviewed cost, by %s; %s of the targets' cost reviewed\n",
			scoreOf(sc.Efficiency), pct(ptr(1-*sc.Efficiency)), sc.Unit, pct(sc.Reviewed))
	} else {
		fmt.Println("    efficiency   unavailable: no work was labeled")
	}
	fmt.Printf("    delivered    %s of the targets' cost went to targets achieved, by %s\n", pct(sc.Delivered), sc.Unit)
	conv := sessionjudge.Amount{Usage: s.Usage, Money: s.Money, Unpriced: s.Unpriced}
	line := amount(conv, s.Currency)
	// A total that is not whole says so, beside the number.
	if s.Unknown > 0 {
		line += fmt.Sprintf("; %d calls with usage unknown, not counted", s.Unknown)
	}
	if s.OutputUnknown > 0 {
		line += fmt.Sprintf("; %d calls that never finished, their output not counted", s.OutputUnknown)
	}
	if s.Money != nil && s.Unpriced > 0 {
		line += fmt.Sprintf("; %d calls unpriced", s.Unpriced)
	}
	fmt.Printf("\n  conversation   %s\n", line)
	if s.Judging == nil {
		fmt.Println("  judging        unknown: an evaluator did not report what it used")
		return
	}
	j := amount(*s.Judging, s.Currency)
	if s.Judging.Unpriced > 0 {
		j += fmt.Sprintf(" (%d stages unpriced)", s.Judging.Unpriced)
	}
	if s.Judging.Unknown > 0 {
		j += fmt.Sprintf("; %d model requests with usage unknown, not counted", s.Judging.Unknown)
	}
	if share, ok := judgingShare(s); ok {
		j += fmt.Sprintf(", %.0f%% of what the conversation cost", share*100)
	}
	if s.JudgingReportedUSD != nil {
		j += fmt.Sprintf("; the evaluators reported %.2f USD", *s.JudgingReportedUSD)
	}
	if s.JudgingMs > 0 {
		m := float64(s.JudgingMs) / 60000
		u := s.Judging.Usage
		j += fmt.Sprintf("\n                 over %.1f min: %s input and %s output tokens per minute", m,
			tokensOf(float64(u.Input+u.CacheRead+u.CacheWrite)/m), tokensOf(float64(u.Output)/m))
	}
	fmt.Printf("  judging        %s\n", j)
}

// judgingShare is what judging cost against what the conversation cost. It
// is given only when both are whole: never against a priced part of either,
// nor a total missing calls with no usage or calls that never finished.
func judgingShare(s *sessionjudge.Summary) (float64, bool) {
	if s.Judging == nil || s.Judging.Money == nil || s.Money == nil || *s.Money <= 0 || s.Judging.Unpriced > 0 ||
		s.Judging.Unknown > 0 || s.Unpriced > 0 || s.Unknown > 0 || s.OutputUnknown > 0 {
		return 0, false
	}
	return *s.Judging.Money / *s.Money, true
}

func ptr(f float64) *float64 { return &f }

func noteOf(note string) string {
	if note == "" {
		return ""
	}
	return " - " + trunc(note, 160)
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// share prints a count a share may have made fractional: whole when it is
// whole, and to two significant digits otherwise, so a call split among
// many targets is never shown as none.
func share(f float64) string {
	if math.Abs(f-math.Round(f)) < 1e-9 {
		return fmt.Sprintf("%.0f", f)
	}
	return strconv.FormatFloat(f, 'f', 1, 64)
}

func tokensOf(f float64) string {
	n := int(math.Round(f))
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

func minutes(ms int64) string {
	if ms == 0 {
		return "no time"
	}
	return fmt.Sprintf("%.1f min", float64(ms)/60000)
}

// cmdJudgeMCP serves the navigation over one conversation at one round, on
// standard input and output. An agent CLI starts it as its only MCP server.
func cmdJudgeMCP(args []string) int {
	fs := flag.NewFlagSet("judge mcp", flag.ExitOnError)
	root := fs.String("root", "", "the storage root")
	conversation := fs.String("conversation", "", "the conversation")
	round := fs.Uint64("round", 0, "the round of the chain to read")
	digest := fs.String("digest", "", "the digest that round must have")
	hide := fs.String("hide", "", "flags whose records are withheld, comma separated")
	ledgerPath := fs.String("ledger", "", "the run's ledger stage file, whose rates price the calls")
	logPath := fs.String("log", "", "where to log the calls answered")
	from := fs.String("from", "", "the window's start, RFC 3339")
	to := fs.String("to", "", "the window's end, RFC 3339")
	fromID := fs.String("from-id", "", "the window's first main talk")
	toID := fs.String("to-id", "", "the window's last main talk")
	readBytes := fs.Int("read-bytes", 0, "the reading budget of the call, in bytes of answers; 0 is no bound")
	taskPath := fs.String("task", "", "the file that says what the call answers for")
	cfgPath := fs.String("config", "", "configuration file, for a server admitted on its own to a pulled snapshot")
	parentLease := fs.String("parent-lease", "", "the run lease of the judge run that started this server, on a pulled snapshot")
	failureReport := fs.String("failure-report", "", "with -parent-lease, where to report a read of damaged evidence")
	_ = fs.Parse(args)
	if *root == "" || *conversation == "" || *round == 0 || *digest == "" {
		fmt.Fprint(os.Stderr, judgeUsage)
		return 2
	}
	if err := checkIdentity("conversation", *conversation, false); err != nil {
		return fail(err)
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	switch {
	case *parentLease != "" && set["config"]:
		return fail(errors.New("-parent-lease takes its root and lease from the run that started it, and no -config"))
	case *parentLease != "" && *failureReport == "":
		return fail(errors.New("-parent-lease needs -failure-report, where the run reads a damaged read from"))
	case *parentLease == "" && set["failure-report"]:
		return fail(errors.New("-failure-report is for a server a judge run started, with -parent-lease"))
	}
	checkedLog, code := admitNavigation(*root, *cfgPath, *parentLease, *logPath, *failureReport)
	if code != 0 {
		return code
	}
	defer releaseAdmission()
	var hidden []string
	if *hide != "" {
		hidden = strings.Split(*hide, ",")
	}
	var pricing *judge.Pricing
	if *ledgerPath != "" {
		f, err := storage.OpenSuppliedRead(*ledgerPath, admittedRoot())
		if err != nil {
			return fail(err)
		}
		ledgerFile, err := sessionjudge.Read(f)
		f.Close()
		if err != nil {
			return fail(err)
		}
		if pricing, err = judge.PricingOf(ledgerFile); err != nil {
			return fail(err)
		}
	}
	window, err := windowOf(*from, *to, *fromID, *toID)
	if err != nil {
		return fail(err)
	}
	zone := storage.NewZone(*root)
	var nav *judge.Nav
	// Loading reads the snapshot's manifest first: under the attempt's
	// marker too, for an inherited server.
	load := func() error {
		srv, snap, err := viewServer(zone, nil)
		if err != nil {
			return err
		}
		nav, err = navigationOf(srv, snap != nil, *failureReport, zone, *conversation, *round, *digest, hidden, window, pricing)
		return err
	}
	switch {
	case *failureReport != "":
		// An inherited server loads under its chunk attempt's marker too:
		// loading reads the evidence the run gave it to navigate.
		err = judge.ReadUnder(*failureReport, load, func(err error) bool {
			return errors.Is(err, view.ErrEvidence) || errors.Is(err, view.ErrNoRecord)
		})
	default:
		err = load()
	}
	if err != nil {
		return fail(err)
	}
	nav.ResetBudget(*readBytes)
	if *taskPath != "" {
		f, err := storage.OpenSuppliedRead(*taskPath, admittedRoot())
		if err != nil {
			return fail(err)
		}
		b, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			return fail(err)
		}
		var a judge.Assignment
		if err := json.Unmarshal(b, &a); err != nil {
			return fail(fmt.Errorf("%s: %w", *taskPath, err))
		}
		nav.Assign(a)
	}
	var onCall func(string, json.RawMessage, int, error, judge.Receipt)
	// closing ends the log with the number of answers it holds, so the run
	// can tell a log its server finished from one cut short. A write that
	// failed stops the log, and the closing line says so when it can be
	// written: the run then takes no receipts from it.
	closing := func() {}
	if *logPath != "" {
		f, err := openNavigationLog(*logPath, checkedLog)
		if err != nil {
			return fail(err)
		}
		defer f.Close()
		var mu sync.Mutex
		attempt := nav.Assigned().Attempt
		answers, failed := 0, false
		onCall = func(tool string, args json.RawMessage, size int, err error, given judge.Receipt) {
			mu.Lock()
			defer mu.Unlock()
			if failed {
				return
			}
			line, _ := json.Marshal(map[string]any{"attempt": attempt, "tool": tool, "args": args, "bytes": size, "error": err != nil,
				"refused": errors.Is(err, judge.ErrBudgetSpent), "opened": given.Opened, "listed": given.Listed})
			if _, werr := f.Write(append(line, '\n')); werr != nil {
				failed = true
				return
			}
			answers++
		}
		closing = func() {
			mu.Lock()
			defer mu.Unlock()
			line, _ := json.Marshal(map[string]any{"attempt": attempt, "closing": true, "answers": answers, "write_failed": failed})
			_, _ = f.Write(append(line, '\n'))
		}
	}
	err = judge.ServeMCP(os.Stdin, os.Stdout, nav, onCall)
	closing()
	if err != nil {
		return fail(err)
	}
	return 0
}

// navigationOf loads a conversation at the round a navigation server serves
// and prepares its navigation, as the run that started it prepared its own.
// On a pulled snapshot every record is read checked, and a head that is not
// the one the run judges is damage to the snapshot. Each read the navigation
// makes there is held to marker, when its run gave it one.
func navigationOf(srv *view.Server, strict bool, marker string, zone *storage.Zone, conversation string, round uint64, digest string, hidden []string,
	window judge.Window, pricing *judge.Pricing) (*judge.Nav, error) {
	c, err := srv.LoadAt(conversation, round)
	if err != nil {
		return nil, err
	}
	doc, err := c.BuildWithheld(hidden)
	if err != nil {
		return nil, err
	}
	// A chain parsed again can reach the same round number with other
	// rounds; the digest says it is the head the run judges.
	if doc.Head.Digest != digest {
		err := fmt.Errorf("round %d of %s is now %s, not the %s the run judges", round, conversation, doc.Head.Digest, digest)
		if strict {
			err = fmt.Errorf("%w: %w", view.ErrEvidence, err)
		}
		return nil, err
	}
	dialect := dialectOf(zone, c.Session)
	read := func(seq, row uint64) (*sessiondata.Record, error) { return c.Record(seq, row, hidden) }
	if !strict {
		doc, sel, err := judge.Narrow(doc, window, c.Records)
		if err != nil {
			return nil, err
		}
		ledger := judge.Compute(doc, dialect, pricing, c.Records)
		ledger.Select(sel)
		nav := judge.NewNav(doc, ledger, read)
		nav.Prepare(sel, c.Records, decoderOf(dialect))
		return nav, nil
	}
	doc, sel, err := judge.NarrowChecked(doc, window, c.RecordsChecked)
	if err != nil {
		return nil, err
	}
	ledger, err := judge.ComputeChecked(doc, dialect, pricing, c.RecordsChecked)
	if err != nil {
		return nil, err
	}
	ledger.Select(sel)
	nav := judge.NewNav(doc, ledger, read)
	if err := nav.PrepareChecked(sel, c.RecordsChecked, decoderOf(dialect)); err != nil {
		return nil, err
	}
	nav.Strictly(judge.NewStrict(doc, c.Holds), marker)
	return nav, nil
}

// windowOf reads a window from the command line. A time is RFC 3339, or a
// date and a time of day, or a date alone, in the local time zone.
func windowOf(from, to, fromID, toID string) (judge.Window, error) {
	w := judge.Window{FromTalk: fromID, ToTalk: toID}
	parse := func(s string) (time.Time, error) {
		if s == "" {
			return time.Time{}, nil
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t, nil
		}
		for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"} {
			if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
				return t, nil
			}
		}
		return time.Time{}, fmt.Errorf("%q is not a time: give RFC 3339, a date and a time of day, or a date", s)
	}
	var err error
	if w.From, err = parse(from); err != nil {
		return w, err
	}
	if w.To, err = parse(to); err != nil {
		return w, err
	}
	if !w.From.IsZero() && !w.To.IsZero() && !w.From.Before(w.To) {
		return w, fmt.Errorf("the window ends at %s, before it starts at %s", to, from)
	}
	return w, nil
}

// windowArgs passes a run's window on to its navigation server.
func windowArgs(w judge.Window) []string {
	var out []string
	if !w.From.IsZero() {
		out = append(out, "-from", w.From.Format(time.RFC3339Nano))
	}
	if !w.To.IsZero() {
		out = append(out, "-to", w.To.Format(time.RFC3339Nano))
	}
	if w.FromTalk != "" {
		out = append(out, "-from-id", w.FromTalk)
	}
	if w.ToTalk != "" {
		out = append(out, "-to-id", w.ToTalk)
	}
	return out
}
