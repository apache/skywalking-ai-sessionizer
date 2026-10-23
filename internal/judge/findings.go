// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements.  See the NOTICE file distributed with
// this work for additional information regarding copyright ownership.
// The ASF licenses this file to You under the Apache License, Version 2.0
// (the "License"); you may not use this file except in compliance with
// the License.  You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package judge

import (
	"slices"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
)

// findingEntry is one finding of an answer. Target names the target only in
// an efficiency answer, whose findings are not inside an achievement.
type findingEntry struct {
	Target      string   `json:"target"`
	At          string   `json:"at"`
	Problem     string   `json:"problem"`
	Explanation string   `json:"explanation"`
	Improvement string   `json:"improvement"`
	Cite        []string `json:"cite"`
}

// checkFindings keeps the findings that say a problem and are anchored at a
// step of the work they concern: a step, never a container, whose main talk
// passes within. A finding proposes, so nothing else of it is checked beyond
// its citations. It returns how many were left out.
func checkFindings(c *checked, nav *Nav, entries []findingEntry, within func(main string) bool, dropped *int) ([]sessionjudge.Finding, int) {
	var out []sessionjudge.Finding
	left := 0
	for _, e := range entries {
		// A step that does not exist is in no talk, so the talk is checked
		// before the step is read.
		if !within(mainTalkOf(nav, e.At)) || slices.Contains(containers, nav.Node(e.At).Kind) || strings.TrimSpace(e.Problem) == "" {
			left++
			continue
		}
		at := c.cites(nav, []string{e.At}, dropped)[0]
		out = append(out, sessionjudge.Finding{At: e.At, Ref: at.Ref, Problem: e.Problem, Explanation: e.Explanation,
			Improvement: e.Improvement, Cite: c.cites(nav, e.Cite, dropped)})
	}
	return out, left
}
