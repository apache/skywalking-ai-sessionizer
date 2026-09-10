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
	"text/tabwriter"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecodechanges"
	"github.com/apache/skywalking-ai-sessionizer/internal/config"
)

// changesMatch builds the session filter of the changes adapter, which
// takes the same patterns as the local adapter and judges them against the
// workspace each session's records name.
func changesMatch(ad config.Adapter) func(claudecodechanges.Session) bool {
	m := claudecode.NewMatcher(ad.Include, ad.Exclude)
	return func(s claudecodechanges.Session) bool { return claudecodechanges.Match(m, s) }
}

// cmdSourcesChanges lists the sessions the changes adapter can see.
func cmdSourcesChanges(_ *config.Config, ad config.Adapter, _ bool) error {
	root, err := claudecodechanges.ResolveSourceRoot(ad.SourceRoot)
	if err != nil {
		return err
	}
	all, err := claudecodechanges.Discover(root)
	if err != nil {
		return err
	}
	match := changesMatch(ad)
	var sessions []claudecodechanges.Session
	for _, s := range all {
		if match(s) {
			sessions = append(sessions, s)
		}
	}
	fmt.Printf("\nchanges root: %s\n", root)
	if n := len(all) - len(sessions); n > 0 {
		fmt.Printf("filtered    : %d session(s) excluded by config\n", n)
	}
	if len(sessions) == 0 {
		fmt.Println("no change records; the plugin is not installed, or has not observed a tool call yet")
		return nil
	}
	fmt.Println()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tSTREAMS\tWORKSPACE")
	for _, s := range sessions {
		fmt.Fprintf(tw, "%s\t%d\t%s\n", s.ID, len(s.Sources), s.Root)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\n%d session(s) with change records\n", len(sessions))
	return nil
}
