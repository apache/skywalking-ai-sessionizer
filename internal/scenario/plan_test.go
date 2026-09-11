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

package scenario

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestAWorkflowRunsItsChildrenAtTheSameTime. The runtime starts a
// workflow's children as one batch and they run at the same time: in 165 of
// 172 real runs with two or more children, the children overlapped. A
// planner that ran them one after another drew a review by four agents as
// four reviews in a row.
func TestAWorkflowRunsItsChildrenAtTheSameTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "batch.yaml")
	body := `steps:
  - input: run the workflow
  - call:
      workflow:
        name: check
        children:
          - {name: long, steps: [{call: {text: one}}, {call: {text: two}}, {call: {text: three}}]}
          - {name: short, steps: [{call: {text: done}}]}
          - {name: middle, steps: [{call: {text: one}}, {call: {text: two}}]}
  - call: {text: all done}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	sc, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := sc.Plan(Options{At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}

	var children []string
	for _, s := range p.Streams {
		if s.Batch != "" {
			children = append(children, s.ID)
		}
	}
	if len(children) != 3 {
		t.Fatalf("the plan has %d workflow children, want 3", len(children))
	}
	first, last := map[string]time.Time{}, map[string]time.Time{}
	var parentLast time.Time
	for _, e := range p.Events {
		if e.Stream == "main" {
			if e.At.After(parentLast) {
				parentLast = e.At
			}
			continue
		}
		if f, ok := first[e.Stream]; !ok || e.At.Before(f) {
			first[e.Stream] = e.At
		}
		if e.At.After(last[e.Stream]) {
			last[e.Stream] = e.At
		}
	}

	// They overlap: the last child to start does so before the first child
	// to finish has finished.
	var latestStart, earliestEnd, latestEnd time.Time
	for i, c := range children {
		if first[c].After(latestStart) {
			latestStart = first[c]
		}
		if i == 0 || last[c].Before(earliestEnd) {
			earliestEnd = last[c]
		}
		if last[c].After(latestEnd) {
			latestEnd = last[c]
		}
		// They start in the order they were written, so a reader can still
		// tell which was started first.
		if i > 0 && !first[c].After(first[children[i-1]]) {
			t.Fatalf("child %d starts at %s, not after child %d at %s", i, first[c], i-1, first[children[i-1]])
		}
	}
	if !latestStart.Before(earliestEnd) {
		t.Fatalf("the children ran one after another: the last started at %s, after the first to finish ended at %s", latestStart, earliestEnd)
	}
	// The parent's next step waits for the longest child.
	if !parentLast.After(latestEnd) {
		t.Fatalf("the parent's last record is at %s, before the last child ended at %s", parentLast, latestEnd)
	}
	// The runtime appends to the journal as things happen.
	j := p.Runs[0].Journal
	for i := 1; i < len(j); i++ {
		if j[i].At.Before(j[i-1].At) {
			t.Fatalf("journal line %d (%s at %s) is before line %d (%s at %s)", i, j[i].Type, j[i].At, i-1, j[i-1].Type, j[i-1].At)
		}
	}
}

// Invalid paths cannot be represented as a successful runtime scenario.
func TestWorkflowRunNames(t *testing.T) {
	for _, name := range []string{"<all> & report", "_check", "--", "中文", "check", "check 2"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "names.yaml")
			body := fmt.Sprintf("steps:\n  - input: check\n  - call:\n      workflow:\n        name: %q\n        children: [{name: child, steps: [{call: {text: done}}]}]\n", name)
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			sc, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := sc.Plan(Options{})
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Runs) != 1 || !regexp.MustCompile(`^wf_[A-Za-z0-9][A-Za-z0-9_-]*$`).MatchString(plan.Runs[0].ID) {
				t.Fatalf("invalid workflow run: %+v", plan.Runs)
			}
		})
	}
}

func TestWorkflowRunNameCollision(t *testing.T) {
	for _, names := range [][2]string{{"check <a>", "check [a]"}, {"same", "same"}, {"Check", "check"}} {
		path := filepath.Join(t.TempDir(), "collision.yaml")
		body := "steps:\n  - input: check\n"
		for _, name := range names {
			body += fmt.Sprintf("  - call:\n      workflow:\n        name: %q\n        children: [{name: child, steps: [{call: {text: done}}]}]\n", name)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		sc, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sc.Plan(Options{}); err == nil || !strings.Contains(err.Error(), "same run id") {
			t.Fatalf("colliding workflows %q: %v", names, err)
		}
	}
}
