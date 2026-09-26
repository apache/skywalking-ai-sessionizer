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
	"io"
	"os"
	"path/filepath"
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
