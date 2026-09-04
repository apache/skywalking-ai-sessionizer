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
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// cmdParse turns landed records into conversation structure and appends one
// round to each conversation's chain.
//
// The conversation id defaults to the session id, which is the adapter's
// documented mapping: nothing on disk reliably links two Claude Code sessions.
// An application that considers several sessions one piece of work supplies its
// own id instead.
func cmdParse(cfg *config.Config, _ config.Adapter, _ bool) error {
	zoneRoot, err := cfg.ResolvedRoot()
	if err != nil {
		return err
	}
	return parseZone(zoneRoot, arg(0), cfg.Parse.MaxRoundBytes)
}

// parseZone assembles every session under zoneRoot, or the one named by want,
// writing rounds of at most maxRound bytes until each chain reaches its index.
func parseZone(zoneRoot, want string, maxRound int64) error {
	z := storage.NewZone(zoneRoot)

	sessions, err := sessionDirs(zoneRoot)
	if err != nil {
		return err
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tROUND\tSEQ\tNODES\tRELS\tUNRES\tTALKS\tRUNS\tSTEPS\tTOOLS\tCHILDREN")
	var rounds, failed int
	for _, id := range sessions {
		if want != "" && id != want {
			continue
		}
		r, err := parseToIndex(z, id, maxRound, &rounds)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s: %v\n", id, err)
			failed++
			continue
		}
		st := r.Stats
		round := "-"
		if r.Changed() {
			round = fmt.Sprintf("%d", r.Number)
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d/%d\t%d/%d\n",
			id, round, r.ThroughSeq, r.Nodes, r.Relations, r.Unresolved,
			st.Talks, st.Runs, st.Steps,
			st.ToolsResolved, st.ToolUses, st.SpawnsResolved, st.Spawns)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\n%d round(s) written\n", rounds)
	if failed > 0 {
		// A pass that could not parse some sessions must not look like a clean
		// one. A watch loop or a CI step reads the exit status, not the log.
		return fmt.Errorf("%d session(s) failed to parse", failed)
	}
	return nil
}

// parseToIndex writes rounds for one session until the chain reaches the
// index. A round is cut at the byte budget, so one pass may take several.
// It returns the last round and adds to rounds the number written.
func parseToIndex(z *storage.Zone, id string, maxRound int64, rounds *int) (*parse.Round, error) {
	for {
		r, err := parse.Session(z, parse.Options{Conversation: id, Session: id, MaxRoundBytes: maxRound})
		if err != nil {
			return nil, err
		}
		if r.Changed() {
			*rounds++
		}
		if !r.Changed() || !r.More {
			return r, nil
		}
	}
}

// printDocument writes the conversation's asz.view document to standard
// output, the same document asz view serves at /api/c/{id}/view, as JSON
// or as YAML. The page stays the page; this is the document for a pipeline
// or a diff.
func printDocument(zoneRoot, id, format string) error {
	c, err := view.New(storage.NewZone(zoneRoot), nil).Load(id)
	if err != nil {
		return err
	}
	doc, err := c.Build()
	if err != nil {
		return err
	}
	var out []byte
	switch format {
	case "json":
		out, err = json.MarshalIndent(doc, "", "  ")
	case "yaml":
		out, err = sessionview.MarshalYAML(doc)
	}
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(append(out, '\n'))
	return err
}

// termsFor returns how to render a name, from the -terms flag.
//
// A reader who knows their own agent product recognises its words; a reader
// comparing two products needs one vocabulary. Neither is the right default for
// both, so the choice is theirs and nothing is lost either way.
func termsFor(mode string) func(string) string {
	g := claudecode.Glossary()
	switch mode {
	case "native":
		return g.Native
	case "both":
		return func(u string) string {
			if n := g.Native(u); n != u {
				return u + "  (" + n + ")"
			}
			return u
		}
	}
	return func(u string) string { return u }
}

// cmdGlossary prints what a runtime calls the things the model names.
func cmdGlossary(_ *config.Config, _ config.Adapter, _ bool) error {
	g := claudecode.Glossary()
	fmt.Printf("dialect %s\n\n", g.Dialect)
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tRUNTIME\tWHERE\tNOTE")
	var derived int
	for _, t := range g.Terms() {
		native, where := t.Native, t.Where
		if t.Derived() {
			derived++
			native, where = "—", "derived here"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", t.Unified, native, where, t.Note)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	terms := g.Terms()
	fmt.Printf("\n%d terms: %d named by the runtime, %d derived by this project\n",
		len(terms), len(terms)-derived, derived)
	return nil
}

// cmdConversation folds a conversation's round chain and prints the structure.
//
// The fold is the whole definition of the conversation as of the latest round.
// There is no other state, which is why this reads the chain rather than any
// cached copy of it.
func cmdConversation(cfg *config.Config, _ config.Adapter, _ bool) error {
	zoneRoot, err := cfg.ResolvedRoot()
	if err != nil {
		return err
	}
	id := arg(0)
	if id == "" {
		return fmt.Errorf("usage: asz conversation [-json|-yaml] CONVERSATION")
	}
	if docFormat != "" {
		return printDocument(zoneRoot, id, docFormat)
	}

	chain := sessionflow.OpenChain(zoneRoot, id)
	files, err := chain.Verify()
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no rounds for conversation %s; run asz parse first", id)
	}
	v, err := chain.Fold()
	if err != nil {
		return err
	}

	fmt.Printf("conversation %s\n", id)
	fmt.Printf("  rounds     %d, head %s\n", v.Round, trunc(v.Digest, 13))
	fmt.Printf("  landed     through seq %d\n", v.ThroughSeq)
	fmt.Printf("  entities   %d nodes, %d relations, %d unresolved (%d still open)\n\n",
		len(v.Nodes), len(v.Relations), len(v.Unresolved), len(v.OpenUnresolved()))

	term := termsFor(termMode)
	byKind := map[string]int{}
	for _, n := range v.Nodes {
		byKind[n.Kind]++
	}
	type kv struct {
		k string
		n int
	}
	var ks []kv
	for k, n := range byKind {
		ks = append(ks, kv{k, n})
	}
	sort.Slice(ks, func(a, b int) bool {
		if ks[a].n != ks[b].n {
			return ks[a].n > ks[b].n
		}
		return ks[a].k < ks[b].k
	})
	fmt.Println("node kinds:")
	for _, x := range ks {
		fmt.Printf("  %-40s %6d\n", term(x.k), x.n)
	}

	byRel := map[string]int{}
	quality := map[string]int{}
	for _, r := range v.Relations {
		byRel[r.Type]++
		quality[r.Quality]++
	}
	if len(byRel) > 0 {
		fmt.Println("\nrelations:")
		for _, t := range sortedKeys(byRel) {
			fmt.Printf("  %-40s %6d\n", term(t), byRel[t])
		}
		fmt.Println("\ncorrelation quality:")
		for _, q := range sortedKeys(quality) {
			fmt.Printf("  %-40s %6d\n", term(q), quality[q])
		}
	}

	if open := v.OpenUnresolved(); len(open) > 0 {
		fmt.Printf("\nstill unresolved (%d):\n", len(open))
		for i, u := range open {
			if i >= 12 {
				fmt.Printf("  … and %d more\n", len(open)-i)
				break
			}
			fmt.Printf("  %-26s %s\n", u.Kind, u.Reason)
		}
	}

	printTree(v, term)
	return nil
}

// printTree renders the containment tree, which carries direct ownership only.
//
// Cross-stream flow does not appear here. A child agent's work stays under the
// child's own stream and the parent holds a reference to it, so nothing is
// shown twice.
func printTree(v *sessionflow.View, term func(string) string) {
	fmt.Println("\nstructure (containment only; cross-stream flow is a relation):")

	// Breadth is capped per level rather than by a running total, so a session
	// with hundreds of children still shows what is under the first of them.
	const breadth = 6
	var walk func(id string, depth int)
	walk = func(id string, depth int) {
		if depth > 6 {
			return
		}
		children := v.Children(id)
		sort.SliceStable(children, func(i, j int) bool {
			return treeRank(children[i].Kind) < treeRank(children[j].Kind)
		})
		shown := 0
		for _, c := range children {
			if shown == breadth {
				fmt.Printf("  %s… %d more\n", strings.Repeat("  ", depth), len(children)-shown)
				break
			}
			shown++
			label := term(c.Kind)
			if name := shortID(c.ID); name != "" {
				label += "  " + name
			}
			fmt.Printf("  %s%s\n", strings.Repeat("  ", depth), label)
			walk(c.ID, depth+1)
		}
	}

	var roots []string
	for id, n := range v.Nodes {
		if n.Parent == "" || v.Nodes[n.Parent] == nil {
			roots = append(roots, id)
		}
	}
	sort.Strings(roots)
	for _, id := range roots {
		fmt.Printf("  %s  %s\n", term(v.Nodes[id].Kind), shortID(id))
		walk(id, 1)
	}
}

// treeRank orders a node's children so the spine is shown before the leaves.
func treeRank(kind string) int {
	switch kind {
	case model.KindStream:
		return 0
	case model.KindEpoch:
		return 1
	case model.KindTalk:
		return 2
	case model.KindRun:
		return 3
	case model.KindLLMCall:
		return 4
	case model.KindSegment:
		return 9
	}
	return 5
}

func shortID(id string) string {
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	if len(id) > 22 {
		return id[:21] + "…"
	}
	return id
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sessionDirs lists the sessions present in the storage root.
func sessionDirs(root string) ([]string, error) {
	items, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range items {
		// A conversation's rounds live beside the sessions under a reserved name,
		// so it must not be mistaken for one.
		if d.IsDir() && !strings.HasPrefix(d.Name(), "_") {
			out = append(out, d.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}
