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
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/judge"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
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
  asz judge [-config FILE] -goals FILE ... CONVERSATION
      judge it as above, then regroup the run by the person's goals, a plain
      list in the file, and compare the judge's grouping with theirs
  asz judge [-config FILE] -regroup RUN -goals FILE [-evaluator NAME] CONVERSATION
      regroup a closed run by the person's goals, in a new version beside
      it: one call of the evaluator, or of the plan's targets stage, places
      each target in the goal it serves
  asz judge [-config FILE] -list CONVERSATION
      list the conversation's runs
  asz judge [-config FILE] -show [-run N] [-json | -format text|markdown|html] CONVERSATION
      print the latest closed run, or run N as -list numbers it: goal by goal, each target's status, score out of 100,
      criteria, what was done and what is missing, then the overall
      assessment and result. -format markdown prints the conclusions alone;
      html prints a page of the details that opens as a file, with the steps
      of the conversation its evidence rests on
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
	show := fs.Bool("show", false, "print the latest closed run, or the one -run names")
	runNumber := fs.Int("run", 0, "with -show, the run to print, as -list numbers it")
	asJSON := fs.Bool("json", false, "with -show, print the stage files as JSON")
	format := fs.String("format", formatText, "with -show: text, markdown or html")
	from := fs.String("from", "", "judge the main talks that start at or after this time")
	to := fs.String("to", "", "judge the main talks that start before this time")
	fromID := fs.String("from-id", "", "judge the main talks from this talk id on")
	toID := fs.String("to-id", "", "judge the main talks up to this talk id")
	mode := fs.String("mode", "", "auto, single or stream: how each stage is given to its evaluator")
	var references repeated
	fs.Var(&references, "reference", "a source to compare the run with: a file, a URL, or one a configured command or MCP server fetches; repeat for several")
	referenceTools := fs.String("reference-tools", "", "list the tools of the MCP server configured for this source")
	goalsPath := fs.String("goals", "", "a file listing the person's goals in their own words: regroup the run by them")
	regroup := fs.Int("regroup", 0, "with -goals, regroup this closed run instead of making a new one")
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
	switch {
	case given["run"] && !*show:
		return usage("-run names the run -show prints")
	case given["run"] && *runNumber < 1:
		return usage("-run takes a run number, as -list prints them")
	case *format != formatText && *format != formatMarkdown && *format != formatHTML:
		return usage(fmt.Sprintf("-format %q: give text, markdown or html", *format))
	case *asJSON && given["format"]:
		return usage("-json and -format each say how to print a run: give one")
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
	case *show:
		// -json prints the stage files as they are stored, with the source's
		// own conversation identifier. The report names the snapshot inside
		// the document, in every form, so nothing comes before a page.
		opts := showOptions{json: *asJSON, format: *format}
		if selection != nil {
			opts.snapshot = display
		}
		if *format == formatHTML {
			opts.steps = pageSteps(root, cfg.View.Hide)
		}
		return judgeShow(storage.NewZone(root), session, display, *runNumber, opts)
	}
	// A goals file is read before a run, so a run is not made for goals
	// that cannot be read.
	var goalsFile *judge.GoalsFile
	if *goalsPath != "" {
		if goalsFile, err = judge.ReadGoals(*goalsPath, admittedRoot()); err != nil {
			return fail(err)
		}
	}
	if *regroup > 0 && goalsFile == nil {
		return fail(errors.New("-regroup needs -goals"))
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
		return judgeRegroup(ctx, r, session, id, *regroup, goalsFile, regrouper)
	}
	run, err := r.Run(ctx, id, *round, plan)
	if err != nil {
		return fail(err)
	}
	fmt.Printf("run %d of %s at round %d: %s\n", run.Number, display, run.Head.Round, run.Status)
	if goalsFile != nil {
		if code := judgeRegroup(ctx, r, session, id, run.Number, goalsFile, regrouper); code != 0 {
			return code
		}
	}
	return judgeShow(zone, session, display, run.Number, showOptions{format: formatText})
}

// judgeRegroup regroups a closed run by a person's goals and says how the
// judge's grouping compared.
func judgeRegroup(ctx context.Context, r *judge.Runner, session, id string, number int, goalsFile *judge.GoalsFile, evaluator string) int {
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
		path, a, err := r.Regroup(ctx, id, run, goalsFile, evaluator)
		if err != nil {
			return fail(err)
		}
		var classes []string
		for _, class := range sessionjudge.Compares {
			if n := a.Classes[class]; n > 0 && class != sessionjudge.CompareAgree {
				classes = append(classes, fmt.Sprintf("%s %d", class, n))
			}
		}
		line := fmt.Sprintf("run %d regrouped by %s: the judge agreed on %d of %s", number, goalsFile.File, a.Classes[sessionjudge.CompareAgree], plural(a.Goals, "goal"))
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

// showOptions say how -show prints a run: as the stage files in JSON, or in
// one of the forms. steps reads the conversation at the head the run judged,
// for a page to show the steps its evidence rests on; nil shows none.
type showOptions struct {
	// snapshot names a conversation pulled from another asz, as asked for.
	snapshot string
	json     bool
	format   string
	steps    func(conversation string, head sessionjudge.Head) (map[string]sessionview.Node, error)
}

// judgeShow prints run number, or the latest closed run when number is 0.
func judgeShow(zone *storage.Zone, session, id string, number int, opts showOptions) int {
	runs, err := storage.JudgmentRuns(zone, session)
	if err != nil {
		return fail(err)
	}
	if number == 0 {
		for _, run := range runs {
			if run.Closed() && run.Run > number {
				number = run.Run
			}
		}
		if number == 0 {
			return fail(fmt.Errorf("%s has no closed run", id))
		}
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
		if opts.json {
			out, _ := json.MarshalIndent(files, "", "  ")
			fmt.Println(string(out))
			return 0
		}
		doc := buildReport(run, files)
		doc.snapshot = opts.snapshot
		var assets pageAssets
		var steps map[string]sessionview.Node
		var stepsErr error
		if opts.format == formatHTML {
			assets.theme = view.HorizonTheme()
			assets.logo, assets.logoBlue, assets.favicon = view.Brand()
			if opts.steps != nil && len(files) > 0 {
				steps, stepsErr = opts.steps(files[0].Header.Conversation, files[0].Header.Head)
			}
		}
		if err := writeReport(os.Stdout, doc, opts.format, assets, steps, stepsErr); err != nil {
			return fail(err)
		}
		return 0
	}
	return fail(fmt.Errorf("%s has no run %d", id, number))
}

// readJudgment reads one judgment file.
// pageSteps reads, for a page, the steps of the conversation a run judged,
// as the page reads them. A pulled snapshot's evidence is read only through
// its checks, so a page over one shows none, whether the snapshot was
// selected or given as the storage root.
func pageSteps(root string, hide []string) func(string, sessionjudge.Head) (map[string]sessionview.Node, error) {
	if admittedRoot() != "" {
		return nil
	}
	zone := storage.NewZone(root)
	return func(conversation string, head sessionjudge.Head) (map[string]sessionview.Node, error) {
		return stepsAt(zone, conversation, head, hide)
	}
}

// stepsAt reads a conversation at the head a run judged, withholding what
// the page withholds, and indexes its steps by id. A chain parsed again may
// hold other content at that round, and then none are given.
func stepsAt(zone *storage.Zone, conversation string, head sessionjudge.Head, hide []string) (map[string]sessionview.Node, error) {
	c, err := view.New(zone, nil).LoadAt(conversation, head.Round)
	if err != nil {
		return nil, err
	}
	doc, err := c.BuildWithheld(hide)
	if err != nil {
		return nil, err
	}
	if doc.Head.Digest != head.Digest {
		return nil, fmt.Errorf("round %d of the conversation is now %s, not the %s the run judged", head.Round, doc.Head.Digest, head.Digest)
	}
	out := map[string]sessionview.Node{}
	var walk func([]sessionview.Node)
	walk = func(nodes []sessionview.Node) {
		for _, n := range nodes {
			walk(n.Children)
			n.Children = nil
			out[n.ID] = n
		}
	}
	walk(doc.Talks)
	walk(doc.Loose)
	return out, nil
}

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
		answers, failed, closed := 0, false, false
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
			if closed {
				return
			}
			closed = true
			line, _ := json.Marshal(map[string]any{"attempt": attempt, "closing": true, "answers": answers, "write_failed": failed})
			_, _ = f.Write(append(line, '\n'))
		}
	}
	// The CLI that runs the evaluator may end this server with a signal
	// instead of closing its input: Claude Code 2.1.312 sent SIGINT and never
	// closed it, and no log was closed. A signal closes the log too, once the
	// request being answered is answered and logged, and nothing is served
	// after it. A server killed outright leaves its log open, and the run
	// takes no receipts from it.
	var serving sync.Mutex
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	served := make(chan struct{})
	go func() {
		select {
		case <-stop:
			serving.Lock()
			closing()
			os.Exit(0)
		case <-served:
		}
	}()
	err = judge.ServeMCP(os.Stdin, os.Stdout, nav, onCall, &serving)
	// The log is closed before the signals are let go: a signal after that
	// would end the server as it would end any program, with no closing line.
	closing()
	signal.Stop(stop)
	close(served)
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
