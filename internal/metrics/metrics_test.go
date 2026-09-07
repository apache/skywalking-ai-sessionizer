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
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"

	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp/otlptest"
	"github.com/apache/skywalking-ai-sessionizer/internal/metrics"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// A call of the fixture: its id, model, minute and usage, written as the
// fragments a main transcript writes, every one repeating the usage.
type call struct {
	id, model string
	at        time.Time
	frags     int
	in, out   int
	read, wr  int
}

func land(t *testing.T, z *storage.Zone, session, stream string, seq uint64, calls []call) string {
	t.Helper()
	path := filepath.Join(z.StreamDir(session, stream), storage.LandedName("transcript", storage.Stamp(time.Unix(0, 0)), seq))
	err := storage.WriteAtomic(path, storage.PermLanded, func(w io.Writer) error {
		hdr := &sessiondata.Header{Seq: seq, At: "2026-09-07T00:00:00Z", Kind: sessiondata.KindTranscript,
			Adapter: "test/0", Dialect: "test/1", Src: session + "/" + stream, Session: session, Stream: stream}
		sw, err := sessiondata.NewWriter(w, hdr)
		if err != nil {
			return err
		}
		ord := uint64(1)
		for _, c := range calls {
			for i := 0; i < c.frags; i++ {
				rec := &sessiondata.Record{Ord: ord, Sha: "x", Bytes: 1, Call: c.id, Model: c.model, From: sessiondata.FromAgent,
					Time:  c.at.Add(time.Duration(i) * 100 * time.Millisecond).UTC().Format(time.RFC3339Nano),
					Usage: &sessiondata.Usage{Input: c.in, Output: c.out, CacheRead: c.read, CacheWrite: c.wr},
					Parts: []sessiondata.Part{{Kind: sessiondata.PartText, Text: "t", State: "available", Bytes: 1}}}
				if err := sw.Write(rec); err != nil {
					return err
				}
				ord++
			}
		}
		return sw.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// points flattens every spooled request into "source/type/model@minute" -> value.
func points(t *testing.T, z *storage.Zone) (map[string]int64, int) {
	t.Helper()
	files, err := storage.NewSpool(z).List()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
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
			if otlptest.Attrs(rm.GetResource().GetAttributes())["service.name"] != metrics.RuntimeService {
				t.Fatalf("a derived request names the service %v; it must carry the runtime's own name for push to normalise", rm.GetResource().GetAttributes())
			}
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					if m.Name != metrics.TokenUsage || m.Unit != "tokens" {
						t.Fatalf("metric %s %s", m.Name, m.Unit)
					}
					sum := m.GetSum()
					if sum == nil || sum.AggregationTemporality != metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA || !sum.IsMonotonic {
						t.Fatalf("%s is not a monotonic delta sum, as the exporter writes it", m.Name)
					}
					for _, dp := range sum.DataPoints {
						a := otlptest.Attrs(dp.Attributes)
						minute := time.Unix(0, int64(dp.StartTimeUnixNano)).UTC().Format("15:04")
						if dp.TimeUnixNano != dp.StartTimeUnixNano+uint64(time.Minute) {
							t.Fatalf("a point does not span one minute")
						}
						out[a["query_source"]+"/"+a["type"]+"/"+a["model"]+"@"+minute+"#"+a["session.id"]] += dp.GetAsInt()
					}
				}
			}
		}
	}
	return out, len(files)
}

// Derivation counts a call once whatever its fragments repeat, sums per
// minute and per attribute set, tells main from a child stream, and keeps
// state so a file is derived once and a new file is derived later.
func TestDerivationCountsEachCallOncePerMinute(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	base := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	land(t, z, "s1", "main", 1, []call{
		{id: "c1", model: "claude-opus-5", at: base.Add(10 * time.Second), frags: 3, in: 10, out: 100, read: 1000, wr: 50},
		{id: "c2", model: "claude-opus-5", at: base.Add(40 * time.Second), frags: 2, in: 5, out: 20, read: 900},
		{id: "c3", model: "claude-sonnet-5", at: base.Add(90 * time.Second), frags: 1, in: 7, out: 30},
	})
	land(t, z, "s1", "a1", 2, []call{
		{id: "c4", model: "claude-opus-5", at: base.Add(20 * time.Second), frags: 2, in: 3, out: 8, read: 400},
	})
	now := base.Add(time.Hour)
	d := &metrics.Deriver{Zone: z, Options: metrics.Options{Version: "test", Now: func() time.Time { return now }}}
	st, err := d.Pass(nil)
	if err != nil || len(st.Errors) != 0 {
		t.Fatal(err, st.Errors)
	}
	if st.Files != 2 || st.Requests != 2 {
		t.Fatalf("derived %d files into %d requests, want 2 and 2", st.Files, st.Requests)
	}
	got, n := points(t, z)
	if n != 2 {
		t.Fatalf("%d spool files, want one per landed file with points", n)
	}
	want := map[string]int64{
		"main/input/claude-opus-5@10:00#s1": 15, "main/output/claude-opus-5@10:00#s1": 120,
		"main/cacheRead/claude-opus-5@10:00#s1": 1900, "main/cacheCreation/claude-opus-5@10:00#s1": 50,
		"main/input/claude-sonnet-5@10:01#s1": 7, "main/output/claude-sonnet-5@10:01#s1": 30,
		"subagent/input/claude-opus-5@10:00#s1": 3, "subagent/output/claude-opus-5@10:00#s1": 8, "subagent/cacheRead/claude-opus-5@10:00#s1": 400,
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %d, want %d; all: %v", k, got[k], v, got)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("%d points, want %d: %v", len(got), len(want), got)
	}

	// A second pass derives nothing; a new file is derived whole.
	st, err = d.Pass(nil)
	if err != nil || st.Files != 0 || st.Requests != 0 {
		t.Fatalf("a second pass derived %d files: %v", st.Files, err)
	}
	land(t, z, "s1", "main", 3, []call{{id: "c5", model: "claude-opus-5", at: now, frags: 2, in: 1, out: 2}})
	st, err = d.Pass([]string{"s1"})
	if err != nil || st.Files != 1 || st.Requests != 1 {
		t.Fatalf("a new file derived %d files into %d requests: %v", st.Files, st.Requests, err)
	}
	got, _ = points(t, z)
	if got["main/input/claude-opus-5@11:00#s1"] != 1 {
		t.Fatalf("the new file's call is missing: %v", got)
	}

	// A call counted in an earlier file is not counted again from a later
	// one: the runtime re-emits records before a reset, and a file cut at
	// a budget can hold the last fragment of a call the file before began.
	land(t, z, "s1", "main", 4, []call{
		{id: "c5", model: "claude-opus-5", at: now, frags: 1, in: 1, out: 2},
		{id: "c1", model: "claude-opus-5", at: base.Add(10 * time.Second), frags: 1, in: 10, out: 100, read: 1000, wr: 50},
		{id: "c6", model: "claude-opus-5", at: now.Add(time.Minute), frags: 1, in: 5, out: 6},
	})
	st, err = d.Pass([]string{"s1"})
	if err != nil || st.Files != 1 || st.Requests != 1 {
		t.Fatalf("the later file derived %d files into %d requests: %v", st.Files, st.Requests, err)
	}
	got, _ = points(t, z)
	if got["main/input/claude-opus-5@11:00#s1"] != 1 || got["main/input/claude-opus-5@10:00#s1"] != 15 || got["main/input/claude-opus-5@11:01#s1"] != 5 {
		t.Fatalf("a call reached from a later file was counted again: %v", got)
	}
}

// The look-back bounds the first pass only: history older than it is
// marked derived without points, a minute inside it is derived, and every
// later pass derives whole files.
func TestLookbackBoundsTheFirstPassOnly(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	land(t, z, "s1", "main", 1, []call{
		{id: "old", model: "m", at: now.Add(-48 * time.Hour), frags: 1, in: 100, out: 100},
	})
	land(t, z, "s1", "main", 2, []call{
		{id: "older", model: "m", at: now.Add(-25 * time.Hour), frags: 1, in: 40, out: 40},
		{id: "recent", model: "m", at: now.Add(-2 * time.Hour), frags: 1, in: 4, out: 4},
	})
	d := &metrics.Deriver{Zone: z, Options: metrics.Options{Lookback: 24 * time.Hour, Version: "test", Now: func() time.Time { return now }}}
	st, err := d.Pass(nil)
	if err != nil || len(st.Errors) != 0 {
		t.Fatal(err, st.Errors)
	}
	if st.Files != 2 || st.Skipped != 1 || st.Requests != 1 {
		t.Fatalf("first pass: files=%d skipped=%d requests=%d, want 2 files, 1 skipped as older than the look-back, 1 request", st.Files, st.Skipped, st.Requests)
	}
	got, _ := points(t, z)
	if got["main/input/m@10:00#s1"] != 4 || len(got) != 2 {
		t.Fatalf("the first pass must derive the recent minute only: %v", got)
	}
	// After the first pass a file two days old is derived whole.
	land(t, z, "s1", "main", 3, []call{{id: "late", model: "m", at: now.Add(-72 * time.Hour), frags: 1, in: 9, out: 9}})
	st, err = d.Pass(nil)
	if err != nil || st.Requests != 1 {
		t.Fatalf("a later pass must derive an old file whole: requests=%d err=%v", st.Requests, err)
	}
}
