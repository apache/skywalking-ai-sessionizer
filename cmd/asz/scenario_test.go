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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
)

// TestFeedSessionNeverRepeats is the property the whole feed rests on. Two
// sessions with the same id write the same source file twice, and the
// collector, which reads forward from where it stopped, sees no growth and
// lands nothing. The ids must differ for emissions a millisecond apart, and
// across a stop and a start, which is why they carry the clock and not a
// counter.
func TestFeedSessionNeverRepeats(t *testing.T) {
	const base = "c30736f2-ac0c-4a72-89b1-e5e984c03078"
	start := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	seen := map[string]bool{}
	for i := range 5000 {
		id := feedSession(base, start.Add(time.Duration(i)*time.Millisecond))
		if seen[id] {
			t.Fatalf("id %s repeats after %d emissions", id, i)
		}
		seen[id] = true
		if !uuidShape.MatchString(id) {
			t.Fatalf("id %s is not in the UUID shape session discovery requires", id)
		}
		if got, want := id[:len(id)-12], base[:len(base)-12]; got != want {
			t.Fatalf("id %s changed the part before the last group; want the prefix %s", id, want)
		}
	}
	// A counter that starts again at one would collide here; the clock does
	// not, which is what makes a restarted feed safe.
	later := feedSession(base, start.Add(time.Hour))
	if seen[later] {
		t.Fatalf("an emission an hour later reused an id a restarted feed would have written")
	}
}

// TestFeedSessionKeepsAnUnusualName is the other branch: an id a scenario
// names itself, which need not be a UUID.
func TestFeedSessionKeepsAnUnusualName(t *testing.T) {
	at := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	id := feedSession("my-session", at)
	if id != "my-session-1788955200000" {
		t.Fatalf("feedSession gave %q", id)
	}
	if feedSession("my-session", at.Add(time.Millisecond)) == id {
		t.Fatal("two emissions a millisecond apart share an id")
	}
}

// TestUnusedSessionSeparatesNeighbouringIds. Two scenarios whose ids sit
// next to each other collide once a repeat counts one onto the other: the
// first keeps ...0002 and the second counts ...0001 up to ...0002. The
// second build would then rewrite the first one's source file and land
// nothing, so a run must never write an id twice.
func TestUnusedSessionSeparatesNeighbouringIds(t *testing.T) {
	used := map[string]bool{}
	first := unusedSession(used, "aaaaaaaa-bbbb-4ccc-8ddd-000000000002")
	second := unusedSession(used, "aaaaaaaa-bbbb-4ccc-8ddd-000000000002")
	if first == second {
		t.Fatalf("both sessions took the id %s", first)
	}
	if !uuidShape.MatchString(second) {
		t.Fatalf("the second id %s left the UUID shape", second)
	}
}

// TestUnusedSessionSeparatesOneMillisecond. A period shorter than a
// millisecond puts two emissions on the same feed id; they must still
// differ.
func TestUnusedSessionSeparatesOneMillisecond(t *testing.T) {
	at := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	used := map[string]bool{}
	seen := map[string]bool{}
	for range 100 {
		id := unusedSession(used, feedSession("aaaaaaaa-bbbb-4ccc-8ddd-000000000001", at))
		if seen[id] {
			t.Fatalf("id %s written twice inside one millisecond", id)
		}
		seen[id] = true
	}
}

// TestChooserCycleIsFixed: a fixed list is emitted in the order it was
// given, and starts again at the top.
func TestChooserCycleIsFixed(t *testing.T) {
	set := []scenario.Loaded{{Path: "a"}, {Path: "b"}, {Path: "c"}}
	next := chooser(set, pickCycle, 0)
	var got []string
	for k := 1; k <= 7; k++ {
		got = append(got, next(k).Path)
	}
	want := []string{"a", "b", "c", "a", "b", "c", "a"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cycle gave %v, want %v", got, want)
		}
	}
}

// TestChooserRandomRepeatsWithASeed: a seed reproduces the order, so a
// report made from a feed can be made again.
func TestChooserRandomRepeatsWithASeed(t *testing.T) {
	set := []scenario.Loaded{{Path: "a"}, {Path: "b"}, {Path: "c"}, {Path: "d"}}
	one, two := chooser(set, pickRandom, 42), chooser(set, pickRandom, 42)
	varied := false
	for k := 1; k <= 20; k++ {
		a, b := one(k).Path, two(k).Path
		if a != b {
			t.Fatalf("the same seed gave %q and %q at %d", a, b, k)
		}
		if a != set[0].Path {
			varied = true
		}
	}
	if !varied {
		t.Fatal("random never left the first scenario")
	}
}

// TestTheRemovePolicyIsRefusedWhereItMeansNothing. --remove is a policy for
// the pipeline over a claude-code build's root. An sd build writes no source
// to remove, and a check keeps nothing, so both refuse it, as they refuse a
// value that is no policy. Each is refused before anything is written.
func TestTheRemovePolicyIsRefusedWhereItMeansNothing(t *testing.T) {
	out := t.TempDir()
	for name, args := range map[string][]string{
		"an sd build":              {"build", "x.yaml", "--format", "sd", "--out", out, "--remove", "1h"},
		"a check":                  {"check", "x.yaml", "--remove", "1h"},
		"a retention of nothing":   {"build", "x.yaml", "--format", "claude-code", "--out", out, "--remove", "0"},
		"a word that is no policy": {"build", "x.yaml", "--format", "claude-code", "--out", out, "--remove", "soon"},
	} {
		if code := cmdScenario(args); code != 2 {
			t.Errorf("%s: exit status %d, want 2", name, code)
		}
	}
	if items, err := os.ReadDir(out); err != nil || len(items) != 0 {
		t.Fatalf("a refused build wrote %d entries: %v", len(items), err)
	}
}

// TestABuildRecordsTheRemovePolicy. The policy a person gives reaches the
// session's marker, which is the only place a pipeline reads it from.
func TestABuildRecordsTheRemovePolicy(t *testing.T) {
	out := t.TempDir()
	file := filepath.Join("..", "..", "tests", "scenarios", "assembly.yaml")
	if code := cmdScenario([]string{"build", file, "--format", "claude-code", "--out", out, "--at", "2026-01-01T00:00:00Z", "--remove", "7d"}); code != 0 {
		t.Fatalf("exit status %d", code)
	}
	markers, err := filepath.Glob(filepath.Join(out, "_source", scenario.MarkerDir, "*.json"))
	if err != nil || len(markers) != 1 {
		t.Fatalf("%d markers: %v", len(markers), err)
	}
	m, err := scenario.ReadMarker(markers[0])
	if err != nil {
		t.Fatal(err)
	}
	if m.Policy != scenario.PolicyRetain || m.Retain != "168h0m0s" {
		t.Fatalf("the marker says %q %q, want retain 168h0m0s", m.Policy, m.Retain)
	}
	// Without the flag, an sd build is not refused, and writes no marker.
	sd := t.TempDir()
	if code := cmdScenario([]string{"build", file, "--format", "sd", "--out", sd, "--at", "2026-01-01T00:00:00Z"}); code != 0 {
		t.Fatalf("an sd build with no --remove exited %d", code)
	}
	if _, err := os.Stat(filepath.Join(sd, "_scenario")); !os.IsNotExist(err) {
		t.Fatal("an sd build made a scenario root")
	}
}
