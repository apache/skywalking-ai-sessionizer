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
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// The shapes below are all present in the fixture. Presence is not coverage, so
// each has an assertion here naming the wrong output it prevents.

// TestSyntheticRecordIsDistinguishable guards a record that carries the
// assistant role but that no model produced. Counted as agent output it
// corrupts token statistics and attributes text to the agent it never wrote.
func TestSyntheticRecordIsDistinguishable(t *testing.T) {
	c := collect(t)
	var synthetic, genuine int
	for _, rec := range c.mainRecords() {
		if rec.From != sessiondata.FromAgent {
			continue
		}
		// The conversion decided this, from the model name rather than from a
		// missing request id: real corpora hold synthetics that carry a request
		// id and genuine calls that carry none.
		if hasFlag(rec, "synthetic") {
			synthetic++
		} else {
			genuine++
		}
	}
	if synthetic != 1 {
		t.Errorf("found %d synthetic records, want 1", synthetic)
	}
	if genuine == 0 {
		t.Error("no genuine assistant records — the discriminator matched everything")
	}
}

// TestUsageFragmentsArePreservedInOrder pins the shape the "take the last
// fragment" rule depends on. A child stream stamps streaming partials on every
// fragment but the last; summing them inflates output tokens several-fold.
func TestUsageFragmentsArePreservedInOrder(t *testing.T) {
	c := collect(t)
	var outputs []int
	for _, rec := range c.records(c.zone.StreamDir(case1, agentChecker), "transcript") {
		if rec.Call == "checker-call1" && rec.Usage != nil {
			outputs = append(outputs, rec.Usage.Output)
		}
	}
	if len(outputs) != 2 {
		t.Fatalf("call has %d fragments, want 2", len(outputs))
	}
	// Monotone non-decreasing, last is the authoritative total.
	if outputs[0] >= outputs[1] {
		t.Errorf("output_tokens = %v, want a partial followed by a larger total", outputs)
	}
	if sum := outputs[0] + outputs[1]; sum == outputs[1] {
		t.Error("fixture does not exercise the summing hazard")
	}
}

// TestCompactionSummaryFollowsItsBoundary covers a pair whose order cannot be
// taken from timestamps: the summary is written with an EARLIER timestamp than
// the boundary that owns it.
func TestCompactionSummaryFollowsItsBoundary(t *testing.T) {
	c := collect(t)
	recs := c.mainRecords()
	boundaryAt := -1
	for i, rec := range recs {
		if hasFlag(rec, "context_reset") {
			boundaryAt = i
		}
	}
	if boundaryAt < 0 {
		t.Fatal("no context reset in the fixture")
	}
	if boundaryAt+1 >= len(recs) {
		t.Fatal("the reset is the last record; no summary follows")
	}
	boundary, summary := recs[boundaryAt], recs[boundaryAt+1]

	if !hasFlag(summary, "reset_summary") {
		t.Errorf("the record after a reset is not its summary: flags %v", summary.Flags)
	}
	// They are paired by containment, which is exact. Pairing them by time would
	// fail, because the summary is stamped EARLIER than the reset that produced
	// it - which is also why an epoch is ordered by landed position.
	if summary.Parent != boundary.ID {
		t.Errorf("summary parent = %q, want the reset %q", summary.Parent, boundary.ID)
	}
	if summary.Time >= boundary.Time {
		t.Errorf("the fixture does not reproduce the inverted timestamps that make "+
			"ordering an epoch by time wrong: summary %s, reset %s", summary.Time, boundary.Time)
	}
	// The reset carries the only link back across itself.
	if boundary.Continues == "" {
		t.Error("the reset names no record to continue from, so nothing links across it")
	}
}

// TestInjectedContextIsCollected covers material the harness pushed into model
// context. It shapes a response as much as the human turn does, and dropping it
// loses part of what the model actually saw.
func TestInjectedContextIsCollected(t *testing.T) {
	c := collect(t)
	var attachments int
	for _, rec := range c.mainRecords() {
		if !hasFlag(rec, "injected") {
			continue
		}
		attachments++
		// Injected material is only useful if what it said survives. An
		// attachment shape the dialect cannot read keeps its bytes rather than
		// landing empty.
		if len(rec.Parts) == 0 {
			t.Errorf("injected context landed with no content at all: ord %d", rec.Ord)
		}
	}
	if attachments == 0 {
		t.Error("injected context was not collected")
	}
}

// TestOutOfBandRecordsAreCollectedButCarryNoIdentity covers host artefacts that
// must survive collection yet become no conversation node: they carry no record
// id, so nothing can attach to them.
func TestOutOfBandRecordsAreCollectedButCarryNoIdentity(t *testing.T) {
	c := collect(t)
	// They convert to records with no identity: nothing can attach to them, and
	// their bytes survive as unknown parts rather than being dropped.
	var outOfBand int
	for _, rec := range c.mainRecords() {
		if rec.ID != "" {
			continue
		}
		outOfBand++
		if len(rec.Parts) == 0 {
			t.Errorf("an out-of-band record landed with nothing in it: ord %d", rec.Ord)
		}
	}
	if outOfBand == 0 {
		t.Error("out-of-band records were not collected")
	}

	// They are indexed for completeness, but with no identity to join on.
	ix, ok, _ := index.Load(c.zone.IndexDir(case1), case1)
	if !ok {
		t.Fatal("no index")
	}
	ix.Build()
	var noIdentity int
	for i := range ix.Entries {
		if ix.Entries[i].Record == 0 {
			noIdentity++
		}
	}
	if noIdentity == 0 {
		t.Error("expected indexed entries with no record id")
	}
}

// TestNonSessionFilesAreIgnored covers a file inside a project directory whose
// name is not a session id. Treating it as one would produce an empty
// conversation with a nonsense identity.
func TestNonSessionFilesAreIgnored(t *testing.T) {
	src := fixtureRoot(t)
	sessions, err := claudecode.Discover(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		if !claudecode.IsSessionID(s.ID) {
			t.Errorf("discovered %q, which is not a session id", s.ID)
		}
		for _, srcFile := range s.Sources {
			if strings.Contains(srcFile.Rel, "not-a-uuid") {
				t.Errorf("collected %s, which is not part of any session", srcFile.Rel)
			}
		}
	}
	if len(sessions) != 2 {
		t.Errorf("discovered %d sessions, want 2", len(sessions))
	}
}

var _ = storage.StreamMain
