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

package metrics_test

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"testing"
	"time"

	collmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"

	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp/otlptest"
	"github.com/apache/skywalking-ai-sessionizer/internal/metrics"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/execution"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/skeleton"
)

// observed is one record the plugin writes after a call to an MCP server.
func observed(tool, name, server string, ms int64, outcome string) *execution.Record {
	return &execution.Record{Schema: execution.Schema, ID: tool + "/" + execution.BoundaryClientHook,
		ObservedBy: execution.ObservedByASZPlugin, Boundary: execution.BoundaryClientHook,
		Session: "s1", Stream: "main", Tool: tool, ToolName: name, Protocol: execution.ProtocolMCP,
		Server: &execution.Server{Name: server, Source: "user"}, Time: base.Format(time.RFC3339Nano),
		DurationMS: &ms, Outcome: outcome}
}

// landExecutions lands one execution file, each record as the changes
// adapter lands a line: whole, as one data part.
func landExecutions(t *testing.T, z *storage.Zone, seq uint64, recs ...*execution.Record) {
	t.Helper()
	path := filepath.Join(z.StreamDir("s1", "main"), storage.LandedName("execution", storage.Stamp(time.Unix(int64(seq), 0)), seq))
	err := storage.WriteAtomic(path, storage.PermLanded, func(w io.Writer) error {
		sw, err := sessiondata.NewWriter(w, &sessiondata.Header{Seq: seq, At: "2026-09-07T00:00:00Z", Kind: sessiondata.KindExecution,
			Adapter: "test/0", Dialect: "execution/1", Src: "s1/main.execution", Session: "s1", Stream: "main"})
		if err != nil {
			return err
		}
		for i, r := range recs {
			line, err := r.Marshal()
			if err != nil {
				return err
			}
			if err := sw.Write(&sessiondata.Record{Ord: uint64(i + 1), Sha: "x", Bytes: len(line), ID: r.ID, Tool: r.Tool, Time: r.Time,
				Parts: []sessiondata.Part{{Kind: sessiondata.PartData, Data: line, State: model.ContentAvailable, Bytes: len(line)}}}); err != nil {
				return err
			}
		}
		return sw.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
	aged(t, path, base.Add(-time.Hour))
}

// mcpTotals sums the MCP family in the spool by metric, server, tool and
// outcome.
func mcpTotals(t *testing.T, z *storage.Zone) map[string]float64 {
	t.Helper()
	files, err := storage.NewSpool(z).List()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, sf := range files {
		data, err := os.ReadFile(sf.Path)
		if err != nil {
			t.Fatal(err)
		}
		var req collmetricspb.ExportMetricsServiceRequest
		if err := proto.Unmarshal(data, &req); err != nil {
			t.Fatal(err)
		}
		for _, rm := range req.ResourceMetrics {
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					for _, dp := range m.GetSum().GetDataPoints() {
						a := otlptest.Attrs(dp.Attributes)
						out[m.Name+" "+a["mcp_server.name"]+"/"+a["mcp_tool.name"]+"/"+a["outcome"]] += dp.GetAsDouble()
					}
				}
			}
		}
	}
	return out
}

// TestAnObservationIsCountedOnceAcrossFiles. The plugin appends a line for
// every event its hook is handed, and the runtime can hand one event over
// again later, so the same record can land in a later file than the first.
// It is still one call. A tool name that does not split exactly is kept
// whole, so a call is never put under a tool it did not name.
func TestAnObservationIsCountedOnceAcrossFiles(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	lookup := observed("toolu_1", "mcp__status__lookup", "status", 300, execution.OutcomeReturned)
	odd := observed("toolu_2", "mcp__odd__name__twice", "odd", 20, execution.OutcomeFailed)
	landExecutions(t, z, 1, lookup, odd)
	d := deriver(z, base.Add(time.Hour), metrics.Options{})
	if _, err := d.Pass(nil); err != nil {
		t.Fatal(err)
	}
	landExecutions(t, z, 2, lookup)
	if st, err := d.Pass(nil); err != nil || len(st.Errors) != 0 {
		t.Fatal(err, st.Errors)
	}
	want := map[string]float64{
		metrics.MCPCalls + " status/lookup/returned":              1,
		metrics.MCPDuration + " status/lookup/returned":           300,
		metrics.MCPCalls + " odd/mcp__odd__name__twice/failed":    1,
		metrics.MCPDuration + " odd/mcp__odd__name__twice/failed": 20,
	}
	got := mcpTotals(t, z)
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: %v, want %v; the spool holds %v", k, got[k], v, got)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("the spool holds %v, want %v", got, want)
	}
}

// removeAll replaces every landed file of s1 with its skeleton, as retention
// does, so only what a skeleton keeps is left to derive from.
func removeAll(t *testing.T, z *storage.Zone) {
	t.Helper()
	files, err := storage.LandedFiles(z, "s1")
	if err != nil {
		t.Fatal(err)
	}
	for _, lf := range files {
		if lf.Skeleton {
			continue
		}
		data, err := os.ReadFile(lf.Path)
		if err != nil {
			t.Fatal(err)
		}
		removed := sessiondata.Removed{At: "2026-09-08T00:00:00Z", Generation: 1, Receiver: sessiondata.ReceiverUnconfirmed}
		if err := storage.WriteAtomic(storage.SkeletonPath(lf.Path), storage.PermLanded, func(w io.Writer) error {
			return skeleton.Write(w, data, removed)
		}); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(lf.Path, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(lf.Path); err != nil {
			t.Fatal(err)
		}
	}
}

// TestAnObservationASkeletonKeepsIsSaidOnce. Retention removed the files of a
// session before its executions were all derived. A skeleton keeps an
// observation's id and outcome, not whether it was an MCP call, so its MCP
// metrics cannot be derived, and the deriver says how many observations
// that is. An observation counted before the removal is not among them. One
// that was not an MCP call is, since nothing recorded why it was not
// counted. Two observations of one call are two. A process says it once;
// a restart says it again.
func TestAnObservationASkeletonKeepsIsSaidOnce(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	other := observed("toolu_other", "WebFetch", "", 50, execution.OutcomeReturned)
	other.Protocol, other.Server = "http", nil
	landExecutions(t, z, 1, observed("toolu_a", "mcp__status__lookup", "status", 100, execution.OutcomeReturned), other)
	derived := &metrics.Deriver{Zone: z, Options: metrics.Options{Grace: -1, Version: "test"}}
	if st, err := derived.Pass(nil); err != nil || len(st.Errors) > 0 || len(st.Unread) != 0 {
		t.Fatalf("%v, %+v: a session with no skeleton has nothing unread", err, st)
	}
	hook := observed("toolu_b", "mcp__status__lookup", "status", 100, execution.OutcomeReturned)
	// A second observer of the same call, which writes its own record.
	second := observed("toolu_b", "mcp__status__lookup", "status", 100, execution.OutcomeReturned)
	second.ID = "toolu_b/second"
	landExecutions(t, z, 2, hook, second)
	removeAll(t, z)

	d := &metrics.Deriver{Zone: z, Options: metrics.Options{Grace: -1, Version: "test"}}
	st, err := d.Pass(nil)
	if err != nil || len(st.Errors) > 0 {
		t.Fatalf("%v %v", err, st)
	}
	if st.Unread["s1"] != 3 {
		t.Fatalf("unread %v, want 3: the observation that was not an MCP call and both of the second call", st.Unread)
	}
	if again, err := d.Pass(nil); err != nil || len(again.Unread) != 0 {
		t.Fatalf("%v, unread %v: a process says it once", err, again.Unread)
	}
	restarted := &metrics.Deriver{Zone: z, Options: metrics.Options{Grace: -1, Version: "test"}}
	if st, err := restarted.Pass(nil); err != nil || st.Unread["s1"] != 3 {
		t.Fatalf("%v, unread %v: a restart says it again", err, st.Unread)
	}
}

// unreadDeriver is a deriver as a process starts it.
func unreadDeriver(z *storage.Zone) *metrics.Deriver {
	return &metrics.Deriver{Zone: z, Options: metrics.Options{Grace: -1, Version: "test"}}
}

// landedPathOf is where landExecutions puts file seq of s1.
func landedPathOf(z *storage.Zone, prefix string, seq uint64) string {
	return filepath.Join(z.StreamDir("s1", "main"), storage.LandedName(prefix, storage.Stamp(time.Unix(int64(seq), 0)), seq))
}

// handSkeleton writes a skeleton by hand, for a shape no landed file has:
// a file named as one kind and saying another, or parts the skeleton writer
// would not keep.
func handSkeleton(t *testing.T, z *storage.Zone, prefix string, kind sessiondata.Kind, seq uint64, parts ...sessiondata.Part) {
	t.Helper()
	path := storage.SkeletonPath(landedPathOf(z, prefix, seq))
	err := storage.WriteAtomic(path, storage.PermLanded, func(w io.Writer) error {
		zw := gzip.NewWriter(w)
		sw, err := sessiondata.NewWriter(zw, &sessiondata.Header{Seq: seq, At: "2026-09-07T00:00:00Z", Kind: kind,
			Adapter: "test/0", Dialect: "execution/1", Src: "s1/main.execution", Session: "s1", Stream: "main",
			Removed: &sessiondata.Removed{Digest: "0", At: "2026-09-08T00:00:00Z", Generation: 1, Receiver: sessiondata.ReceiverUnconfirmed}})
		if err != nil {
			return err
		}
		for i, p := range parts {
			if err := sw.Write(&sessiondata.Record{Ord: uint64(i + 1), Sha: "x", Bytes: 1, ID: "r" + strconv.Itoa(i),
				Parts: []sessiondata.Part{p}}); err != nil {
				return err
			}
		}
		if err := sw.Close(); err != nil {
			return err
		}
		return zw.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
}

func executionPart(t *testing.T, r *execution.Record, kind sessiondata.PartKind) sessiondata.Part {
	t.Helper()
	line, err := r.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return sessiondata.Part{Kind: kind, Data: line, State: model.ContentRemoved, Bytes: len(line)}
}

// TestAnUnsavedPassSaysItsCountAgain. A pass that counted the observations a
// skeleton keeps and then could not save its state returns nothing, so its
// count was never said. The next pass of the same process says it, and the
// one after it does not.
func TestAnUnsavedPassSaysItsCountAgain(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("a read-only directory stops a write only for a user that is not root, and not on Windows")
	}
	z := storage.NewZone(t.TempDir())
	landExecutions(t, z, 1, observed("toolu_a", "mcp__status__lookup", "status", 100, execution.OutcomeReturned))
	removeAll(t, z)
	if _, err := unreadDeriver(z).Pass(nil); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(z.Root(), storage.SpoolDir)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	d := unreadDeriver(z)
	_, err := d.Pass(nil)
	if chmodErr := os.Chmod(dir, 0o755); chmodErr != nil {
		t.Fatal(chmodErr)
	}
	if err == nil {
		t.Fatal("a pass that could not save its state succeeded")
	}
	if st, err := d.Pass(nil); err != nil || st.Unread["s1"] != 1 {
		t.Fatalf("%v, unread %v: the pass after an unsaved one must say the count", err, st.Unread)
	}
	if st, err := d.Pass(nil); err != nil || len(st.Unread) != 0 {
		t.Fatalf("%v, unread %v: then it is said", err, st.Unread)
	}
}

// TestASkeletonThatCannotBeReadIsTriedAgain. A session's files were all
// derived, then removed, and one of its execution skeletons cannot be read.
// The session stays pending, so a later pass reads it again once it can, and
// says the count then.
func TestASkeletonThatCannotBeReadIsTriedAgain(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	other := observed("toolu_other", "WebFetch", "", 50, execution.OutcomeReturned)
	other.Protocol, other.Server = "http", nil
	landExecutions(t, z, 1, other)
	if _, err := unreadDeriver(z).Pass(nil); err != nil {
		t.Fatal(err)
	}
	removeAll(t, z)
	sk := storage.SkeletonPath(landedPathOf(z, "execution", 1))
	good, err := os.ReadFile(sk)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sk, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sk, []byte("not a skeleton"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := unreadDeriver(z)
	st, err := d.Pass(nil)
	if err != nil || len(st.Errors) == 0 || !slices.Contains(st.PendingSessions, "s1") {
		t.Fatalf("%v, %+v: a skeleton that cannot be read must keep its session pending", err, st)
	}
	if err := os.WriteFile(sk, good, 0o444); err != nil {
		t.Fatal(err)
	}
	if st, err := d.Pass(st.PendingSessions); err != nil || len(st.Errors) > 0 || st.Unread["s1"] != 1 {
		t.Fatalf("%v, %+v: once it reads, the count is said", err, st)
	}
}

// TestOnlyExecutionSkeletonsAreRead. A transcript's skeleton holds no
// execution record, and a root's skeletons are mostly transcripts, so the
// count does not open them: a transcript skeleton that cannot be read is no
// error, once its file was derived.
func TestOnlyExecutionSkeletonsAreRead(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	path := landedPathOf(z, "transcript", 1)
	err := storage.WriteAtomic(path, storage.PermLanded, func(w io.Writer) error {
		sw, err := sessiondata.NewWriter(w, &sessiondata.Header{Seq: 1, At: "2026-09-07T00:00:00Z", Kind: sessiondata.KindTranscript,
			Adapter: "test/0", Dialect: "test/1", Src: "s1/main.jsonl", Session: "s1", Stream: "main"})
		if err != nil {
			return err
		}
		if err := sw.Write(&sessiondata.Record{Ord: 1, Sha: "x", Bytes: 2, ID: "u1",
			Parts: []sessiondata.Part{{Kind: sessiondata.PartText, Text: "hi", State: model.ContentAvailable, Bytes: 2}}}); err != nil {
			return err
		}
		return sw.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unreadDeriver(z).Pass(nil); err != nil {
		t.Fatal(err)
	}
	removeAll(t, z)
	sk := storage.SkeletonPath(path)
	if err := os.Chmod(sk, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sk, []byte("not a skeleton"), 0o644); err != nil {
		t.Fatal(err)
	}
	if st, err := unreadDeriver(z).Pass(nil); err != nil || len(st.Errors) > 0 {
		t.Fatalf("%v, %+v: a transcript skeleton was opened", err, st)
	}
}

// TestTheCountReadsWhatItShould. An observation landed twice, in two
// skeletons, is one. An observation counted before the removal is not among
// them, also when the state that said so was lost and its file's receipt says
// it again. A part that is not data, a record with no id, and a file named
// for executions that says it holds something else add nothing.
func TestTheCountReadsWhatItShould(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	counted := observed("toolu_a", "mcp__status__lookup", "status", 100, execution.OutcomeReturned)
	landExecutions(t, z, 1, counted)
	if _, err := unreadDeriver(z).Pass(nil); err != nil {
		t.Fatal(err)
	}
	twice := observed("toolu_b", "mcp__status__lookup", "status", 100, execution.OutcomeReturned)
	landExecutions(t, z, 2, twice)
	landExecutions(t, z, 3, twice)
	removeAll(t, z)
	if err := os.Remove(filepath.Join(z.Root(), storage.SpoolDir, metrics.StateFile)); err != nil {
		t.Fatal(err)
	}
	unnamed := observed("toolu_c", "mcp__status__lookup", "status", 100, execution.OutcomeReturned)
	unnamed.ID = ""
	inText := observed("toolu_d", "mcp__status__lookup", "status", 100, execution.OutcomeReturned)
	handSkeleton(t, z, "execution", sessiondata.KindExecution, 4,
		executionPart(t, unnamed, sessiondata.PartData), executionPart(t, inText, sessiondata.PartText))
	misnamed := observed("toolu_e", "mcp__status__lookup", "status", 100, execution.OutcomeReturned)
	handSkeleton(t, z, "execution", sessiondata.KindTranscript, 5, executionPart(t, misnamed, sessiondata.PartData))
	st, err := unreadDeriver(z).Pass(nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Unread["s1"] != 1 {
		t.Fatalf("unread %v, want 1: the observation landed twice, and nothing else", st.Unread)
	}
}
