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
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/kiro"
	"github.com/apache/skywalking-ai-sessionizer/internal/config"
)

// cmdSourcesKiro lists the Kiro sessions the adapter can see, and what it
// sees and does not collect.
func cmdSourcesKiro(cfg *config.Config, ad config.Adapter, _ bool) error {
	if err := printSourceRetention(cfg, ad); err != nil {
		return err
	}
	root, err := kiro.ResolveSourceRoot(ad.SourceRoot)
	if err != nil {
		return err
	}
	all, skipped, warnings, err := kiro.Discover(root)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "warning: %v\n", w)
	}
	m := kiro.NewMatcher(ad.Include, ad.Exclude)
	var sessions []kiro.Session
	for _, s := range all {
		if m.Match(s) {
			sessions = append(sessions, s)
		}
	}
	fmt.Printf("\nkiro root  : %s\n", root)
	if n := len(all) - len(sessions); n > 0 {
		fmt.Printf("filtered   : %d session(s) excluded by config\n", n)
	}
	for _, line := range notCollected(skipped) {
		fmt.Printf("not read   : %s\n", line)
	}
	if len(sessions) == 0 {
		fmt.Println("no V3 sessions; Kiro is not installed, or has not run on its V3 engine yet")
		return nil
	}
	fmt.Println()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tSTREAMS\tWORKSPACE")
	for _, s := range sessions {
		streams := 0
		for _, src := range s.Sources {
			if src.Kind.Append() {
				streams++
			}
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\n", s.ID, streams, strings.Join(s.Workspaces, ", "))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\n%d V3 session(s)\n", len(sessions))
	ids := make([]string, 0, len(sessions))
	for _, s := range sessions {
		ids = append(ids, s.ID)
	}
	return sayRetired(cfg, ids)
}

// notCollected says, one line each, what Kiro wrote that this adapter does
// not read and why.
func notCollected(s kiro.Skipped) []string {
	var out []string
	if s.V2 > 0 {
		out = append(out, fmt.Sprintf("%d session(s) of kiro-cli's V2 engine under sessions/cli; only the V3 engine's sessions are collected: start kiro-cli with --v3, or set chat.agentEngine to v3", s.V2))
	}
	var schemas []string
	for v := range s.Schemas {
		schemas = append(schemas, v)
	}
	sort.Strings(schemas)
	for _, v := range schemas {
		out = append(out, fmt.Sprintf("%d V3 session(s) with schema %s; only schema %s is read", s.Schemas[v], v, kiro.Schema))
	}
	if s.Unreadable > 0 {
		out = append(out, fmt.Sprintf("%d V3 session(s) whose session.json is not valid JSON", s.Unreadable))
	}
	return out
}
