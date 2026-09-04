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

// Command asz is the SkyWalking AI Sessionizer command-line interface.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/verify"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

const usage = `asz - SkyWalking AI Sessionizer: conversation-level observability for long-lived AI agents

Usage:
  asz sources [-config FILE]        list discovered sessions and their sources
  asz collect [-config FILE] [-once]  land new source data into the storage root
  asz index [-config FILE] [SESSION]  report what the derived index holds
  asz show [-config FILE] SESSION ID   resolve a record id or tool-use id to its payload
  asz parse [-config FILE] [SESSION]   assemble conversation structure into a round chain
  asz repack [-config FILE] DEST [SESSION]  re-cut landed files into DEST under the configured budget and build its chains
  asz conversation [-config FILE] ID   fold a conversation's rounds and show the structure
  asz view [-config FILE] [ADDR]       serve the conversations as a page (default 127.0.0.1:8787)
  asz push [-config FILE] [-once]      send landed files and rounds to an OpenTelemetry logs receiver
  asz glossary                         what the runtime calls the things the model names
  asz verify [-config FILE] [SESSION]  check landed data and round chains are intact
  asz scenario build|check FILE ...    build a session from a scenario, or check one; see asz scenario
  asz version                          print the version

Flags:
  -config FILE   configuration file (default: ./asz.yaml when present, else built-in defaults)
  -once          single pass, then exit (overrides collector.mode)
  -terms MODE    name things as "unified" (default), "native", or "both"
`

// positional holds the command's non-flag arguments, filled once flags parse.
// defaultConfig is read from the working directory when no -config is given.
const defaultConfig = "asz.yaml"

// version is set at build time from the tag or the commit. A binary built
// with plain go build says dev.
var version = "dev"

var positional []string

// termMode selects whose words to print: the model's, the runtime's, or both.
var termMode string

// arg returns the nth positional argument, or "" when there is none.
func arg(n int) string {
	if n < len(positional) {
		return positional[n]
	}
	return ""
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd := os.Args[1]
	if cmd == "scenario" {
		os.Exit(cmdScenario(os.Args[2:]))
	}
	if cmd == "version" {
		fmt.Printf("asz %s (%s %s/%s)\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	cfgPath := fs.String("config", "", "configuration file")
	once := fs.Bool("once", false, "single pass, then exit")
	terms := fs.String("terms", "unified", "name things in the `mode`: unified, native, or both")
	_ = fs.Parse(os.Args[2:])
	// Take the positional arguments from the flag set, not by scanning argv for
	// anything without a leading dash. A flag's VALUE has no leading dash either,
	// so scanning reads "-config asz.yaml" as the positional "asz.yaml".
	positional = fs.Args()
	termMode = *terms

	// A checkout carries asz.yaml with every default written out, so a
	// command run from its root reads that file rather than a copy of the
	// same values compiled in. An explicit -config always wins.
	if *cfgPath == "" {
		if _, err := os.Stat(defaultConfig); err == nil {
			*cfgPath = defaultConfig
		}
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal(err)
	}

	var run func(*config.Config, config.Adapter, bool) error
	switch cmd {
	case "sources":
		run = cmdSources
	case "collect":
		run = cmdCollect
	case "index":
		run = cmdIndex
	case "show":
		run = cmdShow
	case "parse":
		run = cmdParse
	case "repack":
		run = cmdRepack
	case "conversation":
		run = cmdConversation
	case "glossary":
		run = cmdGlossary
	case "view":
		run = cmdView
	case "push":
		run = cmdPush
	case "verify":
		run = cmdVerify
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	for _, ad := range cfg.Adapters {
		if !ad.Enabled {
			continue
		}
		if ad.Name != config.AdapterClaudeCodeLocal {
			fmt.Fprintf(os.Stderr, "skipping unknown adapter %q\n", ad.Name)
			continue
		}
		if err := run(cfg, ad, *once); err != nil {
			fatal(err)
		}
	}
}

func cmdSources(_ *config.Config, ad config.Adapter, _ bool) error {
	root, err := claudecode.ResolveSourceRoot(ad.SourceRoot)
	if err != nil {
		return err
	}
	all, err := claudecode.Discover(root)
	if err != nil {
		return err
	}
	m := claudecode.NewMatcher(ad.Include, ad.Exclude)
	var sessions []claudecode.Session
	for _, s := range all {
		if m.Match(s) {
			sessions = append(sessions, s)
		}
	}
	fmt.Printf("source root: %s\n", root)
	if n := len(all) - len(sessions); n > 0 {
		fmt.Printf("filtered   : %d session(s) excluded by config\n", n)
	}
	fmt.Println()

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tDIRS\tSTREAMS\tMETA\tJOURNAL\tMANIFEST")
	var totalStreams int
	for _, s := range sessions {
		var streams, metas, journals, manifests int
		for _, src := range s.Sources {
			switch src.Kind {
			case claudecode.SrcMainTranscript, claudecode.SrcAgentTranscript:
				streams++
			case claudecode.SrcAgentMeta:
				metas++
			case claudecode.SrcJournal:
				journals++
			case claudecode.SrcWorkflowManifest:
				manifests++
			}
		}
		totalStreams += streams
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\n", s.ID, len(s.Dirs), streams, metas, journals, manifests)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\n%d sessions, %d streams\n", len(sessions), totalStreams)

	// A session spread over several source directories is normal - a child
	// agent running in a different working directory is filed under that
	// directory instead. Surfacing it makes the session-first grouping visible.
	var multi []string
	for _, s := range sessions {
		if len(s.Dirs) > 1 {
			multi = append(multi, fmt.Sprintf("  %s  (%d dirs)", s.ID, len(s.Dirs)))
		}
	}
	if len(multi) > 0 {
		sort.Strings(multi)
		fmt.Printf("\n%d session(s) span more than one source directory:\n", len(multi))
		for _, m := range multi {
			fmt.Println(m)
		}
	}
	return nil
}

// cmdIndex reports what the derived index holds, which is the surface the
// assembler resolves structure against.
func cmdIndex(cfg *config.Config, _ config.Adapter, _ bool) error {
	zoneRoot, err := cfg.ResolvedRoot()
	if err != nil {
		return err
	}
	z := storage.NewZone(zoneRoot)

	want := arg(0)

	sessions, err := os.ReadDir(zoneRoot)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tENTRIES\tBLOCKS\tSTREAMS\tSTRINGS\tINDEXED_SEQ")
	var totE, totB int
	var detail *index.Index
	var detailID string
	for _, d := range sessions {
		if !d.IsDir() {
			continue
		}
		id := d.Name()
		if want != "" && id != want {
			continue
		}
		ix, ok, err := index.Load(z.IndexDir(id), id)
		if err != nil || !ok {
			continue
		}
		stt, _ := storage.LoadIndexState(z.IndexStatePath(id), id)
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\n", id, len(ix.Entries), len(ix.Blocks),
			len(ix.Streams()), ix.Strings.Len(), stt.IndexedSeq)
		totE += len(ix.Entries)
		totB += len(ix.Blocks)
		if want != "" {
			detail, detailID = ix, id
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\ntotal: %d entries, %d joinable blocks\n", totE, totB)

	if detail != nil {
		printIndexDetail(detail, detailID)
	}
	return nil
}

func printIndexDetail(ix *index.Index, id string) {
	ix.Build()
	names := map[index.Kind]string{
		index.KindUser: "user", index.KindAssistant: "assistant",
		index.KindAttachment: "attachment", index.KindSystem: "system",
		index.KindMeta: "agent_meta", index.KindJournal: "journal",
		index.KindManifest: "manifest", index.KindScript: "script",
		index.KindOther: "other", index.KindUnknown: "unknown",
	}
	byKind := map[index.Kind]int{}
	msgs := map[uint32]int{}
	var withCall, withRecord, withRun int
	for i := range ix.Entries {
		e := &ix.Entries[i]
		byKind[e.Kind]++
		if e.Record != 0 {
			withRecord++
		}
		if e.Call != 0 {
			withCall++
			msgs[e.Call]++
		}
		if e.Run != 0 {
			withRun++
		}
	}
	fmt.Printf("\n%s\n\nrecord kinds:\n", id)
	type kv struct {
		k index.Kind
		n int
	}
	var ks []kv
	for k, n := range byKind {
		ks = append(ks, kv{k, n})
	}
	sort.Slice(ks, func(a, b int) bool { return ks[a].n > ks[b].n })
	for _, x := range ks {
		fmt.Printf("  %-12s %8d\n", names[x.k], x.n)
	}

	var tu, tr int
	toolNames := map[uint32]int{}
	for i := range ix.Blocks {
		if b := &ix.Blocks[i]; b.Kind == index.BlockToolUse {
			tu++
			toolNames[b.Name]++
		} else if b.Kind == index.BlockToolResult {
			tr++
		}
	}
	fmt.Printf("\njoinable blocks:\n  tool_use     %8d\n  tool_result  %8d\n", tu, tr)
	if len(msgs) > 0 {
		fmt.Printf("\nprovider calls: %d distinct message ids over %d fragment records (mean %.2f)\n",
			len(msgs), withCall, float64(withCall)/float64(len(msgs)))
	}
	fmt.Printf("\nidentity coverage: uuid=%d  run=%d  of %d entries\n",
		withRecord, withRun, len(ix.Entries))

	type tn struct {
		n    int
		name string
	}
	var tns []tn
	for tid, n := range toolNames {
		tns = append(tns, tn{n, ix.Strings.String(tid)})
	}
	sort.Slice(tns, func(a, b int) bool { return tns[a].n > tns[b].n })
	fmt.Println("\ntop tools:")
	for i, t := range tns {
		if i >= 8 {
			break
		}
		fmt.Printf("  %-14s %7d\n", t.name, t.n)
	}

	// A sample of entries, showing that one index spans every stream and every
	// landed sequence in the session.
	fmt.Println("\nsample entries (one index, all streams, all rounds):")
	et := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(et, "  SEQ\tROW\tKIND\tSTREAM\tRECORD\tCALL")
	shown, perStream := 0, map[uint32]int{}
	for i := range ix.Entries {
		e := &ix.Entries[i]
		if perStream[e.Stream] >= 2 || shown >= 12 {
			continue
		}
		perStream[e.Stream]++
		shown++
		fmt.Fprintf(et, "  %d\t%d\t%s\t%s\t%s\t%s\n",
			e.Seq, e.Row, names[e.Kind],
			trunc(ix.Strings.String(e.Stream), 18),
			trunc(ix.Strings.String(e.Record), 13),
			trunc(ix.Strings.String(e.Call), 16))
	}
	_ = et.Flush()
}

func trunc(s string, n int) string {
	if s == "" {
		return "-"
	}
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// cmdShow resolves an identifier through the index to the landed record it
// names, and prints the payload.
//
// This is the same path materialisation takes, on one record: the index answers WHERE a
// record is - which landed file, which line - and only then are the bytes read.
// cmdVerify checks that landed data is contiguous and intact, using only the
// landed files - the source is usually gone by the time anyone asks.
func cmdVerify(cfg *config.Config, _ config.Adapter, _ bool) error {
	zoneRoot, err := cfg.ResolvedRoot()
	if err != nil {
		return err
	}
	z := storage.NewZone(zoneRoot)

	want := arg(0)
	entries, err := os.ReadDir(zoneRoot)
	if err != nil {
		return err
	}

	var sessions, streams, records, problems, relanded int
	for _, d := range entries {
		if !d.IsDir() || (want != "" && d.Name() != want) {
			continue
		}
		rep, err := verify.Session(z, d.Name())
		if err != nil {
			return err
		}
		if len(rep.Streams) == 0 {
			continue
		}
		sessions++
		streams += len(rep.Streams)
		records += rep.Records
		problems += rep.Problems
		relanded += rep.Relanded
		if !rep.OK() {
			fmt.Printf("%s: %d problem(s)\n", rep.Session, rep.Problems)
			for _, line := range rep.Details() {
				fmt.Printf("   %s\n", line)
			}
		}
	}

	fmt.Printf("checked %d session(s), %d stream(s), %d records\n", sessions, streams, records)
	if relanded > 0 {
		// Expected after an interrupted pass, not a fault: data is landed before
		// the cursor is committed, so a crash repeats rather than loses.
		fmt.Printf("%d record(s) landed more than once by an interrupted pass; assembly removes them\n", relanded)
	}

	// A conversation's rounds are checked too. They are a digest chain, so a
	// missing or edited round is detectable in a way landed files are not, and
	// the check costs one pass over a directory that is far smaller than the data.
	chains, rounds, err := verifyChains(zoneRoot, want)
	if err != nil {
		return err
	}
	if chains > 0 {
		fmt.Printf("checked %d conversation chain(s), %d round(s)\n", chains, rounds)
	}

	if problems > 0 {
		return fmt.Errorf("%d contiguity or integrity problem(s)", problems)
	}
	fmt.Println("all landed data is contiguous and matches its digests")
	return nil
}

// verifyChains walks every conversation chain in the zone.
func verifyChains(root, want string) (chains, rounds int, err error) {
	base := filepath.Join(root, "_conversations")
	items, rerr := os.ReadDir(base)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return 0, 0, nil
		}
		return 0, 0, rerr
	}
	for _, d := range items {
		if !d.IsDir() || (want != "" && d.Name() != want) {
			continue
		}
		files, verr := sessionflow.OpenChain(root, d.Name()).Verify()
		if verr != nil {
			return chains, rounds, verr
		}
		chains++
		rounds += len(files)
	}
	return chains, rounds, nil
}

func cmdShow(cfg *config.Config, _ config.Adapter, _ bool) error {
	zoneRoot, err := cfg.ResolvedRoot()
	if err != nil {
		return err
	}
	z := storage.NewZone(zoneRoot)

	session, want := arg(0), arg(1)
	if session == "" || want == "" {
		return fmt.Errorf("usage: asz show SESSION ID   (a record uuid or a tool_use id)")
	}

	ix, ok, err := index.Load(z.IndexDir(session), session)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no index for session %s", session)
	}

	// A tool-use id resolves to every block carrying it - normally the call and
	// its result, which is the join in its most direct form.
	if blocks := ix.ToolBlocks(want); len(blocks) > 0 {
		fmt.Printf("tool-use id %s resolves to %d block(s)\n\n", want, len(blocks))
		for _, b := range blocks {
			e := &ix.Entries[b.Entry]
			kind := "tool_use"
			if b.Kind == index.BlockToolResult {
				kind = "tool_result"
			}
			fmt.Printf("── %s  block %d of record  stream=%s\n", kind, b.Ord, ix.Strings.String(e.Stream))
			if err := printEntry(z, ix, session, e); err != nil {
				return err
			}
		}
		return nil
	}

	e, found := ix.EntryByRecord(want)
	if !found {
		return fmt.Errorf("id %q not found in the index for %s", want, session)
	}
	return printEntry(z, ix, session, e)
}

// printEntry locates one indexed record on disk and prints it.
func printEntry(z *storage.Zone, ix *index.Index, session string, e *index.Entry) error {
	stream := ix.Strings.String(e.Stream)
	run := ix.Strings.String(e.Run)
	dir := z.StreamDir(session, stream)
	if run != "" {
		dir = z.RunDir(session, run)
	}

	// seq selects the landed file; row selects the line within it.
	path, err := landedPathForSeq(dir, e.Seq)
	if err != nil {
		return err
	}
	fmt.Printf("   index : seq=%d row=%d\n", e.Seq, e.Row)
	fmt.Printf("   file  : %s\n", strings.TrimPrefix(path, z.Root()+"/"))

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r, err := sessiondata.NewReader(f)
	if err != nil {
		return err
	}
	for row := uint32(1); ; row++ {
		rec, rerr := r.Next()
		if rerr != nil {
			return fmt.Errorf("row %d not found in %s", e.Row, path)
		}
		if row != e.Row {
			continue
		}
		fmt.Printf("   source: ord=%d off=%d sha=%s bytes=%d   (where it came from)\n",
			rec.Ord, rec.Off, rec.Sha, rec.Bytes)
		fmt.Printf("   from  : %s", rec.From)
		if rec.Trigger != "" {
			fmt.Printf("   trigger: %s", rec.Trigger)
		}
		if len(rec.Flags) > 0 {
			fmt.Printf("   flags: %s", strings.Join(rec.Flags, ","))
		}
		fmt.Println()
		for i, p := range rec.Parts {
			fmt.Printf("   part %d  %-9s", i, p.Kind)
			switch {
			case p.Name != "":
				fmt.Printf("%s", p.Name)
			case p.Of != "":
				fmt.Printf("of %s", trunc(p.Of, 24))
			}
			fmt.Printf("   %s, %d bytes\n", p.State, p.Bytes)
			if p.Text != "" {
				fmt.Printf("      %s\n", trunc(strings.ReplaceAll(p.Text, "\n", " "), 300))
			}
			if len(p.Data) > 0 {
				fmt.Printf("      %s\n", trunc(string(p.Data), 300))
			}
		}
		for _, d := range rec.Dropped {
			fmt.Printf("   dropped %s, %d bytes  (%s)\n", d.What, d.Bytes, d.Why)
		}
		fmt.Println()
		return nil
	}
}

// landedPathForSeq finds the landed file carrying a sequence.
func landedPathForSeq(dir string, seq uint32) (string, error) {
	items, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	suffix := fmt.Sprintf("-%06d.jsonl", seq)
	for _, it := range items {
		if strings.HasSuffix(it.Name(), suffix) {
			return filepath.Join(dir, it.Name()), nil
		}
	}
	return "", fmt.Errorf("no landed file with seq %d in %s", seq, dir)
}

func cmdCollect(cfg *config.Config, ad config.Adapter, once bool) error {
	root, err := claudecode.ResolveSourceRoot(ad.SourceRoot)
	if err != nil {
		return err
	}
	zoneRoot, err := cfg.ResolvedRoot()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(zoneRoot, 0o755); err != nil {
		return err
	}
	col := claudecode.New(root, storage.NewZone(zoneRoot), ad.Collector.MaxDeltaBytes)
	match := claudecode.NewMatcher(ad.Include, ad.Exclude).Match

	fmt.Printf("source root : %s\nstorage root: %s\n", root, zoneRoot)

	pass := func() error {
		start := time.Now()
		st, err := col.CollectAll(match)
		if err != nil {
			return err
		}
		fmt.Printf("[%s] sessions=%d sources=%d landed=%d records=%d bytes=%s indexed=%d gone=%d conflicts=%d busy=%d pending=%d errors=%d (%s)\n",
			time.Now().Format("15:04:05"), st.Sessions, st.SourcesSeen, st.SourcesLanded,
			st.Records, humanBytes(st.Bytes), st.Indexed, st.SourcesGone, st.Conflicts,
			st.Busy, st.Pending, len(st.Errors), time.Since(start).Round(time.Millisecond))
		for _, e := range st.Errors {
			fmt.Fprintf(os.Stderr, "  error: %v\n", e)
		}
		if !st.Complete() {
			// A pass that under-collected must not look like a clean one.
			return fmt.Errorf("pass incomplete: %d source(s) still pending, %d error(s)",
				st.Pending, len(st.Errors))
		}
		return nil
	}

	if once || ad.Collector.Mode == config.ModeOnce {
		return pass()
	}
	for {
		if err := pass(); err != nil {
			return err
		}
		time.Sleep(ad.Collector.Interval)
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "%s: %v\n", filepath.Base(os.Args[0]), err)
	os.Exit(1)
}
