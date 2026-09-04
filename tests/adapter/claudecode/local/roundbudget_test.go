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

package local_test

import (
	"fmt"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// TestRoundBudgetCutsTheChainByWindow parses the same evidence twice: once
// as a single round, and once under a budget so small that every round can
// cover only one landed file. The chain must reach the same index either
// way, and the two folds must hold the same entities, because a budget
// changes where the evidence is cut and never what it says.
func TestRoundBudgetCutsTheChainByWindow(t *testing.T) {
	src := t.TempDir()
	x := newTranscript(t, src)
	for i := 0; i < 4; i++ {
		x.AddTurn(fmt.Sprintf("turn %d", i+1), true)
	}
	x.Flush()

	whole := storage.NewZone(t.TempDir())
	cut := storage.NewZone(t.TempDir())
	for _, z := range []*storage.Zone{whole, cut} {
		if _, err := claudecode.New(src, z, 0).CollectAll(nil); err != nil {
			t.Fatal(err)
		}
	}
	opts := parse.Options{Conversation: growSession, Session: growSession, Reindex: index.Rebuild}

	one, err := parse.Session(whole, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !one.Changed() || one.More {
		t.Fatalf("one round under the default budget: changed=%v more=%v", one.Changed(), one.More)
	}
	if one.ThroughSeq < 2 {
		t.Fatalf("the fixture must land several files for this test to mean anything, got through %d", one.ThroughSeq)
	}

	small := opts
	small.MaxRoundBytes = 1
	var rounds []*parse.Round
	for {
		r, err := parse.Session(cut, small)
		if err != nil {
			t.Fatal(err)
		}
		rounds = append(rounds, r)
		if !r.More {
			break
		}
		if len(rounds) > int(one.ThroughSeq) {
			t.Fatalf("more rounds than landed files: %d", len(rounds))
		}
	}
	if len(rounds) != int(one.ThroughSeq) {
		t.Fatalf("a one-byte budget must write one round per landed file: %d rounds for %d files", len(rounds), one.ThroughSeq)
	}
	for i, r := range rounds {
		if !r.Changed() || r.FromSeq != r.ThroughSeq || r.Number != uint64(i+1) {
			t.Fatalf("round %d: number=%d from=%d through=%d", i, r.Number, r.FromSeq, r.ThroughSeq)
		}
	}
	if last := rounds[len(rounds)-1]; last.ThroughSeq != one.ThroughSeq {
		t.Fatalf("the cut chain stops at %d, the whole one at %d", last.ThroughSeq, one.ThroughSeq)
	}

	va, err := parse.View(whole.Root(), growSession)
	if err != nil {
		t.Fatal(err)
	}
	vb, err := parse.View(cut.Root(), growSession)
	if err != nil {
		t.Fatal(err)
	}
	if va.ThroughSeq != vb.ThroughSeq || vb.Round != uint64(len(rounds)) {
		t.Fatalf("folds: whole through %d round %d, cut through %d round %d", va.ThroughSeq, va.Round, vb.ThroughSeq, vb.Round)
	}
	for id, n := range va.Nodes {
		m := vb.Nodes[id]
		if m == nil || m.Kind != n.Kind || m.Parent != n.Parent {
			t.Fatalf("node %s differs between the folds: whole=%v cut=%v", id, n, m)
		}
	}
	for id := range vb.Nodes {
		if va.Nodes[id] == nil {
			t.Fatalf("the cut fold holds %s, the whole one does not", id)
		}
	}
	for id := range va.Relations {
		if vb.Relations[id] == nil {
			t.Fatalf("relation %s is missing from the cut fold", id)
		}
	}
	for id := range vb.Relations {
		if va.Relations[id] == nil {
			t.Fatalf("the cut fold holds relation %s, the whole one does not", id)
		}
	}
	// An unresolved reference may exist in the cut fold as resolved, where the
	// whole one never had it: its evidence arrived in a later window. Open
	// ones must agree.
	open := func(m map[string]*sessionflow.Unresolved) map[string]bool {
		out := map[string]bool{}
		for id, u := range m {
			if u.State == "open" {
				out[id] = true
			}
		}
		return out
	}
	oa, ob := open(va.Unresolved), open(vb.Unresolved)
	if len(oa) != len(ob) {
		t.Fatalf("open unresolved: whole %v, cut %v", oa, ob)
	}
	for id := range oa {
		if !ob[id] {
			t.Fatalf("open unresolved %s is missing from the cut fold", id)
		}
	}
}
