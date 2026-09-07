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
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	collmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"

	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp/otlptest"
	"github.com/apache/skywalking-ai-sessionizer/internal/metrics"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// The parity fixture: what Claude Code 2.1.260 sent to asz's receiver from
// one short non-interactive Opus session on 2026-09-07, over gRPC, with
// the values of every attribute naming a person, an account, an
// organisation or a session replaced. testdata/claude-code-2.1.260-metrics.json
// is the same request as the protocol's JSON.
func capture(t *testing.T) *collmetricspb.ExportMetricsServiceRequest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "claude-code-2.1.260-metrics.pb"))
	if err != nil {
		t.Fatal(err)
	}
	var req collmetricspb.ExportMetricsServiceRequest
	if err := proto.Unmarshal(data, &req); err != nil {
		t.Fatal(err)
	}
	return &req
}

func find(req *collmetricspb.ExportMetricsServiceRequest, name string) *metricspb.Metric {
	for _, rm := range req.ResourceMetrics {
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name == name {
					return m
				}
			}
		}
	}
	return nil
}

func keysOf(dp *metricspb.NumberDataPoint) []string {
	var out []string
	for _, kv := range dp.Attributes {
		out = append(out, kv.Key)
	}
	sort.Strings(out)
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// What the derivation writes matches the exporter's token metric where the
// export page says it does, and differs exactly where the page says it
// differs: the same name, description, unit, temporality, monotonicity,
// value kind and token types; the same session, model, query source and
// type labels; and, missing from a derived point, only the labels the
// page lists as not derived. Every other metric the exporter sent is on
// the page's list of metrics not derived. A newer runtime that sends
// another label or another metric fails here, and the page is updated.
func TestDerivedPointsMatchTheCapturedExporter(t *testing.T) {
	theirs := find(capture(t), metrics.TokenUsage)
	if theirs == nil {
		t.Fatal("the capture carries no token metric")
	}

	z := storage.NewZone(t.TempDir())
	land(t, z, "s1", "main", 1, []call{{id: "c1", model: "claude-opus-5", at: base, frags: 2, in: 2, out: 4, read: 0, wr: 0}})
	d := deriver(z, base.Add(time.Hour), metrics.Options{})
	if _, err := d.Pass(nil); err != nil {
		t.Fatal(err)
	}
	files, _ := storage.NewSpool(z).List()
	data, err := os.ReadFile(files[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	var mine collmetricspb.ExportMetricsServiceRequest
	if err := proto.Unmarshal(data, &mine); err != nil {
		t.Fatal(err)
	}
	ours := find(&mine, metrics.TokenUsage)
	if ours == nil {
		t.Fatal("the derivation wrote no token metric")
	}

	// The metric itself.
	if ours.Name != theirs.Name || ours.Unit != theirs.Unit || ours.Description != theirs.Description {
		t.Fatalf("metric: derived %q %q %q, exporter %q %q %q", ours.Name, ours.Unit, ours.Description, theirs.Name, theirs.Unit, theirs.Description)
	}
	if ours.GetSum().GetAggregationTemporality() != theirs.GetSum().GetAggregationTemporality() || ours.GetSum().GetIsMonotonic() != theirs.GetSum().GetIsMonotonic() {
		t.Fatalf("sum: derived %v %v, exporter %v %v", ours.GetSum().GetAggregationTemporality(), ours.GetSum().GetIsMonotonic(), theirs.GetSum().GetAggregationTemporality(), theirs.GetSum().GetIsMonotonic())
	}

	// The points: value kind, the token types, and the labels.
	theirTypes, theirSources, theirKeys := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, dp := range theirs.GetSum().GetDataPoints() {
		if _, ok := dp.GetValue().(*metricspb.NumberDataPoint_AsDouble); !ok {
			t.Fatalf("the exporter's point is not a double: %v", dp.GetValue())
		}
		a := otlptest.Attrs(dp.Attributes)
		theirTypes[a["type"]] = true
		theirSources[a["query_source"]] = true
		for _, k := range keysOf(dp) {
			theirKeys[k] = true
		}
	}
	ourTypes := map[string]bool{}
	for _, dp := range ours.GetSum().GetDataPoints() {
		if _, ok := dp.GetValue().(*metricspb.NumberDataPoint_AsDouble); !ok {
			t.Fatalf("a derived point is not a double, as the exporter's are")
		}
		a := otlptest.Attrs(dp.Attributes)
		ourTypes[a["type"]] = true
		if !theirSources[a["query_source"]] {
			t.Fatalf("a derived point names query_source %q, which the exporter never sent", a["query_source"])
		}
		for _, k := range keysOf(dp) {
			if !theirKeys[k] {
				t.Fatalf("a derived point carries %q, a label the exporter does not send", k)
			}
		}
		// Every label the exporter sends and the derivation does not is on
		// the export page's list, and nothing on that list is derived.
		for k := range theirKeys {
			if !contains(keysOf(dp), k) && !contains(metrics.NotDerivedLabels, k) {
				t.Fatalf("the exporter sends %q and the derivation does not, and the export page does not say so", k)
			}
			if contains(keysOf(dp), k) && contains(metrics.NotDerivedLabels, k) {
				t.Fatalf("%q is derived and listed as not derived", k)
			}
		}
	}
	for typ := range theirTypes {
		if !ourTypes[typ] {
			t.Fatalf("the exporter sends type %q and the derivation does not", typ)
		}
	}
	for typ := range ourTypes {
		if !theirTypes[typ] {
			t.Fatalf("the derivation writes type %q, which the exporter never sent", typ)
		}
	}
	// The exporter's auxiliary calls never reach a transcript; the capture
	// has one, so the page's claim about them stays measured.
	if !theirSources["auxiliary"] {
		t.Fatal("the capture carries no auxiliary call; the page's claim about them is no longer measured")
	}

	// Every other metric in the capture is one the page says is not derived.
	for _, rm := range capture(t).ResourceMetrics {
		for _, sm := range rm.ScopeMetrics {
			if sm.GetScope().GetName() != "com.anthropic.claude_code" {
				t.Fatalf("the exporter's scope is %q; the export page names another", sm.GetScope().GetName())
			}
			for _, m := range sm.Metrics {
				if m.Name != metrics.TokenUsage && !contains(metrics.NotDerivedMetrics, m.Name) {
					t.Fatalf("the exporter sent %s, which is neither derived nor listed as not derived", m.Name)
				}
			}
		}
		res := otlptest.Attrs(rm.GetResource().GetAttributes())
		if res["service.name"] != metrics.RuntimeService {
			t.Fatalf("the exporter's service is %q; the derivation names %q", res["service.name"], metrics.RuntimeService)
		}
	}
}
