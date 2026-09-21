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

package langsmith

import (
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// convertCase renders every arrival of a case, in the order they arrived.
func convertCase(t *testing.T, kase string) []sessiondata.Record {
	t.Helper()
	var out []sessiondata.Record
	seen := map[string]bool{}
	operations := operationsOf(t, kase)
	hints := Resolve(operations)
	for _, op := range operations {
		trace := decode(t, op).Trace
		records, err := Convert(op, !seen[trace], hints)
		if err != nil {
			t.Fatalf("%s: %s: %v", kase, op.RunID, err)
		}
		seen[trace] = true
		out = append(out, records...)
	}
	return out
}

// TestEveryArrivalConverts is the broad one: nothing in the corpus defeats the
// dialect, and nothing it produces is a record with no content at all.
func TestEveryArrivalConverts(t *testing.T) {
	cases, total := 0, 0
	captured(t, func(_, _ string, _ capturedMeta, _ []byte) {})
	for _, kase := range []string{"plain", "three-turns", "tool-error", "parallel-tools",
		"loop", "two-threads", "slow-tool", "large-content", "subagent",
		"subagent-own-thread", "no-thread-key", "shared-thread-key",
		"unsafe-thread-key", "abandoned-run", "traceable-only"} {
		records := convertCase(t, kase)
		if len(records) == 0 {
			t.Fatalf("%s: converted to nothing", kase)
		}
		for _, r := range records {
			if len(r.Parts) == 0 {
				t.Errorf("%s: %s has no parts", kase, r.ID)
			}
			if r.Sha == "" || r.Bytes == 0 {
				t.Errorf("%s: %s has no provenance", kase, r.ID)
			}
		}
		cases++
		total += len(records)
	}
	t.Logf("%d cases, %d records", cases, total)
}

// TestOneArrivalCarriesContent is the rule a measurement forced. Two arrivals
// of one call both carrying text produce one call with two assistant messages,
// in a round the parser calls verified — a wrong conversation that passes every
// check. Only the arrival that carries the run's end may carry its content.
func TestOneArrivalCarriesContent(t *testing.T) {
	for _, kase := range []string{"slow-tool", "abandoned-run", "three-turns", "loop"} {
		carrying := map[string]int{}
		for _, r := range convertCase(t, kase) {
			if r.Call == "" && r.Tool == "" {
				continue
			}
			key := r.Call
			if key == "" {
				key = r.ID
			}
			for _, p := range r.Parts {
				if p.Kind == sessiondata.PartText || p.Kind == sessiondata.PartCall ||
					p.Kind == sessiondata.PartResult {
					carrying[key]++
					break
				}
			}
		}
		for call, n := range carrying {
			if n > 1 {
				t.Errorf("%s: call %s carries content on %d arrivals", kase, call, n)
			}
		}
	}
}

// TestUnfinishedRunsLandAsData: work in progress is evidence, and it lands,
// but it says nothing the conversation would have to un-say later.
func TestUnfinishedRunsLandAsData(t *testing.T) {
	data, finished := 0, 0
	for _, op := range operationsOf(t, "abandoned-run") {
		records, err := Convert(op, false, Hints{})
		if err != nil {
			t.Fatal(err)
		}
		open := decode(t, op).End == ""
		for _, r := range records {
			for _, p := range r.Parts {
				if open && p.Kind != sessiondata.PartData {
					t.Errorf("%s arrived unfinished but carries a %s part", r.ID, p.Kind)
				}
			}
			if open {
				data++
			} else {
				finished++
			}
		}
	}
	if data == 0 {
		t.Fatal("no unfinished run landed")
	}
	t.Logf("%d records from unfinished runs, %d from finished", data, finished)
}

// TestToolCarriesTheCallID is the other rule a build forced: a spawn resolves
// by looking the call up by id, so the tool's name leaves a child unattached.
func TestToolCarriesTheCallID(t *testing.T) {
	for _, kase := range []string{"three-turns", "subagent", "parallel-tools"} {
		tools := 0
		for _, r := range convertCase(t, kase) {
			if r.Tool == "" {
				continue
			}
			tools++
			for _, p := range r.Parts {
				if p.Kind == sessiondata.PartResult && p.Of != r.Tool {
					t.Errorf("%s: result joins %q but the record names %q", kase, p.Of, r.Tool)
				}
			}
		}
		if tools == 0 {
			t.Errorf("%s: no tool result", kase)
		}
	}
}

// TestFailureSurvives: a tool that raised has to read as a failed result, not
// as a tool that never returned.
func TestFailureSurvives(t *testing.T) {
	failed := 0
	for _, r := range convertCase(t, "tool-error") {
		for _, p := range r.Parts {
			if p.Kind == sessiondata.PartResult && p.Failed != nil && *p.Failed {
				failed++
			}
		}
	}
	if failed == 0 {
		t.Fatal("the failing tool did not land a failed result")
	}
}

// TestFrameworkRunsSayWhatTheyDropped: the shape is kept, the repeated content
// is not, and the record states the loss rather than leaving it silent.
func TestFrameworkRunsSayWhatTheyDropped(t *testing.T) {
	kept, dropped := 0, 0
	for _, r := range convertCase(t, "loop") {
		if r.Call != "" || r.Tool != "" || len(r.Flags) > 0 && r.Flags[0] == "external_input" {
			continue
		}
		kept++
		if len(r.Dropped) > 0 {
			dropped += r.Dropped[0].Bytes
		}
	}
	if kept == 0 || dropped == 0 {
		t.Fatalf("framework runs: %d records, %d bytes reported dropped", kept, dropped)
	}
	t.Logf("%d framework records, %d bytes of repeated content reported dropped", kept, dropped)
}
