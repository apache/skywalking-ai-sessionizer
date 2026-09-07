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

// A call of the fixture, written as its fragments. A main transcript
// repeats the final usage on every fragment and marks each as finished; a
// child's carries streaming partials, output 1, 2, 3, and only the last
// fragment carries the final usage and the finished flag. An unfinished
// call never gets its last fragment.
type call struct {
	id, model  string
	at         time.Time
	frags      int
	in, out    int
	read, wr   int
	unfinished bool
	// only writes the first n fragments, for a call cut at a file boundary.
	only int
	// skip leaves out the first n fragments, for the file that continues it.
	skip int
}

func records(c call, main bool, ord *uint64) []*sessiondata.Record {
	var out []*sessiondata.Record
	for i := 0; i < c.frags; i++ {
		last := i == c.frags-1
		if (c.only > 0 && i >= c.only) || i < c.skip {
			continue
		}
		u := &sessiondata.Usage{Input: c.in, Output: c.out, CacheRead: c.read, CacheWrite: c.wr}
		var flags []string
		if main {
			flags = []string{"finished"}
		} else if last && !c.unfinished {
			flags = []string{"finished"}
		} else {
			u = &sessiondata.Usage{Input: c.in, Output: i + 1, CacheRead: c.read, CacheWrite: c.wr}
		}
		if c.unfinished && main {
			flags = nil
		}
		out = append(out, &sessiondata.Record{Ord: *ord, Sha: "x", Bytes: 1, Call: c.id, Model: c.model, From: sessiondata.FromAgent,
			Time:  c.at.Add(time.Duration(i) * 100 * time.Millisecond).UTC().Format(time.RFC3339Nano),
			Usage: u, Flags: flags,
			Parts: []sessiondata.Part{{Kind: sessiondata.PartText, Text: "t", State: "available", Bytes: 1}}})
		*ord++
	}
	return out
}

func land(t *testing.T, z *storage.Zone, session, stream string, seq uint64, calls []call) string {
	t.Helper()
	path := filepath.Join(z.StreamDir(session, stream), storage.LandedName("transcript", storage.Stamp(time.Unix(int64(seq), 0)), seq))
	err := storage.WriteAtomic(path, storage.PermLanded, func(w io.Writer) error {
		hdr := &sessiondata.Header{Seq: seq, At: "2026-09-07T00:00:00Z", Kind: sessiondata.KindTranscript,
			Adapter: "test/0", Dialect: "test/1", Src: session + "/" + stream, Session: session, Stream: stream}
		sw, err := sessiondata.NewWriter(w, hdr)
		if err != nil {
			return err
		}
		ord := uint64(1)
		for _, c := range calls {
			for _, rec := range records(c, stream == "main", &ord) {
				if err := sw.Write(rec); err != nil {
					return err
				}
			}
		}
		return sw.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// aged sets a landed file's time, for the grace a file cut mid-call waits.
func aged(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

type got struct {
	value      float64
	start, end time.Time
}

// points flattens every spooled request into "source/type/model#session" -> the
// points of that series in order, and the number of spool files.
func points(t *testing.T, z *storage.Zone) (map[string][]got, int) {
	t.Helper()
	files, err := storage.NewSpool(z).List()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]got{}
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
						k := a["query_source"] + "/" + a["type"] + "/" + a["model"] + "#" + a["session.id"]
						if _, ok := dp.Value.(*metricspb.NumberDataPoint_AsDouble); !ok {
							t.Fatalf("a derived point is not a double, as the exporter encodes its counters")
						}
						out[k] = append(out[k], got{value: dp.GetAsDouble(), start: time.Unix(0, int64(dp.StartTimeUnixNano)).UTC(), end: time.Unix(0, int64(dp.TimeUnixNano)).UTC()})
					}
				}
			}
		}
	}
	return out, len(files)
}

func total(ps []got) float64 {
	var n float64
	for _, p := range ps {
		n += p.value
	}
	return n
}

// nonzero keeps the series that carry tokens: the exporter and the
// derivation both write a point for every type, zero included.
func nonzero(m map[string][]got) map[string][]got {
	out := map[string][]got{}
	for k, ps := range m {
		if total(ps) != 0 {
			out[k] = ps
		}
	}
	return out
}

var base = time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)

func deriver(z *storage.Zone, now time.Time, opts metrics.Options) *metrics.Deriver {
	opts.Version = "test"
	opts.Now = func() time.Time { return now }
	return &metrics.Deriver{Zone: z, Options: opts}
}

// Usage is the last fragment's, in line order: a main transcript's repeated
// final usage once, a child's final usage rather than its streaming
// partials, and an unfinished call not at all.
func TestUsageIsTheLastFragmentOfAFinishedCall(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	land(t, z, "s1", "main", 1, []call{
		{id: "c1", model: "claude-opus-5", at: base.Add(10 * time.Second), frags: 3, in: 10, out: 100, read: 1000, wr: 50},
		{id: "c2", model: "claude-opus-5", at: base.Add(40 * time.Second), frags: 2, in: 5, out: 20, read: 900},
		{id: "c3", model: "claude-sonnet-5", at: base.Add(90 * time.Second), frags: 1, in: 7, out: 30},
		{id: "c4", model: "claude-opus-5", at: base.Add(100 * time.Second), frags: 2, in: 9, out: 9, unfinished: true},
	})
	land(t, z, "s1", "a1", 2, []call{
		{id: "c5", model: "claude-opus-5", at: base.Add(20 * time.Second), frags: 4, in: 3, out: 80, read: 400},
		{id: "c6", model: "claude-opus-5", at: base.Add(30 * time.Second), frags: 3, in: 2, out: 50, unfinished: true},
	})
	d := deriver(z, base.Add(time.Hour), metrics.Options{})
	st, err := d.Pass(nil)
	if err != nil || len(st.Errors) != 0 {
		t.Fatal(err, st.Errors)
	}
	if st.Files != 2 || st.Requests != 2 {
		t.Fatalf("derived %d files into %d requests, want 2 and 2", st.Files, st.Requests)
	}
	got, _ := points(t, z)
	want := map[string]float64{
		"main/input/claude-opus-5#s1": 15, "main/output/claude-opus-5#s1": 120,
		"main/cacheRead/claude-opus-5#s1": 1900, "main/cacheCreation/claude-opus-5#s1": 50,
		"main/input/claude-sonnet-5#s1": 7, "main/output/claude-sonnet-5#s1": 30,
		"subagent/input/claude-opus-5#s1": 3, "subagent/output/claude-opus-5#s1": 80, "subagent/cacheRead/claude-opus-5#s1": 400,
	}
	for k, v := range want {
		if total(got[k]) != v {
			t.Fatalf("%s = %v, want %v; all: %v", k, total(got[k]), v, got)
		}
	}
	if len(nonzero(got)) != len(want) {
		t.Fatalf("%d series with tokens, want %d: an unfinished call must not count: %v", len(nonzero(got)), len(want), got)
	}
	// A second pass derives nothing; a new file is derived whole.
	if st, err = d.Pass(nil); err != nil || st.Files != 0 || st.Requests != 0 {
		t.Fatalf("a second pass derived %d files: %v", st.Files, err)
	}
	land(t, z, "s1", "main", 3, []call{{id: "c7", model: "claude-opus-5", at: base.Add(time.Hour), frags: 2, in: 1, out: 2}})
	if st, err = d.Pass([]string{"s1"}); err != nil || st.Files != 1 || st.Requests != 1 {
		t.Fatalf("a new file derived %d files into %d requests: %v", st.Files, st.Requests, err)
	}
	got, _ = points(t, z)
	if total(got["main/input/claude-opus-5#s1"]) != 16 {
		t.Fatalf("the new file's call is missing: %v", got)
	}
}

// A call cut at a file boundary is counted once, from its last fragment in
// the next file of the stream; while that file has not landed, the file
// waits, but not longer than the grace.
func TestACallCutAtAFileBoundaryIsCountedOnceFromItsEnd(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	cut := call{id: "x", model: "m", at: base, frags: 3, in: 4, out: 60}
	first := cut
	first.only = 1
	rest := cut
	rest.skip = 1
	p1 := land(t, z, "s1", "a1", 1, []call{{id: "w", model: "m", at: base.Add(-time.Minute), frags: 2, in: 1, out: 10}, first})
	aged(t, p1, base)
	d := deriver(z, base.Add(30*time.Second), metrics.Options{})
	st, err := d.Pass(nil)
	if err != nil || st.Deferred != 1 || st.Files != 0 {
		t.Fatalf("a file ending mid-call with no next file must wait: deferred=%d files=%d err=%v", st.Deferred, st.Files, err)
	}
	land(t, z, "s1", "a1", 2, []call{rest, {id: "y", model: "m", at: base.Add(time.Minute), frags: 1, in: 2, out: 20}})
	st, err = d.Pass(nil)
	if err != nil || st.Files != 2 || len(st.Errors) != 0 {
		t.Fatalf("files=%d err=%v %v", st.Files, err, st.Errors)
	}
	got, _ := points(t, z)
	if total(got["subagent/output/m#s1"]) != 90 || total(got["subagent/input/m#s1"]) != 7 {
		t.Fatalf("the cut call must count once from its final fragment: %v", got)
	}

	// Past the grace a file ending mid-call is derived with what it has,
	// and an unfinished call counts for nothing.
	z2 := storage.NewZone(t.TempDir())
	p := land(t, z2, "s1", "a1", 1, []call{{id: "w", model: "m", at: base, frags: 1, in: 1, out: 10}, first})
	aged(t, p, base)
	d2 := deriver(z2, base.Add(time.Hour), metrics.Options{})
	if st, err := d2.Pass(nil); err != nil || st.Deferred != 0 || st.Files != 1 {
		t.Fatalf("past the grace: deferred=%d files=%d err=%v", st.Deferred, st.Files, err)
	}
	if got, _ := points(t, z2); total(got["subagent/output/m#s1"]) != 10 {
		t.Fatalf("an unfinished call must not count: %v", got)
	}
}

// Two files with the same series in one minute, two children at once, give
// two points whose windows do not overlap; a file whose minute an earlier
// point has passed follows that point.
func TestWindowsOfASeriesNeverOverlap(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	land(t, z, "s1", "a1", 1, []call{{id: "a", model: "m", at: base.Add(5 * time.Second), frags: 1, in: 1, out: 10}})
	land(t, z, "s1", "a2", 2, []call{{id: "b", model: "m", at: base.Add(20 * time.Second), frags: 1, in: 2, out: 20}})
	d := deriver(z, base.Add(time.Hour), metrics.Options{})
	if st, err := d.Pass(nil); err != nil || st.Requests != 2 {
		t.Fatal(err, st)
	}
	got, _ := points(t, z)
	ps := got["subagent/output/m#s1"]
	if len(ps) != 2 || ps[0].start != base || ps[0].end != base.Add(time.Minute) || !ps[1].start.Equal(ps[0].end) || !ps[1].end.After(ps[1].start) {
		t.Fatalf("two files in one minute: %+v", ps)
	}
	// Late data: a child landed later whose call ended in that same minute.
	land(t, z, "s1", "a3", 3, []call{{id: "c", model: "m", at: base.Add(40 * time.Second), frags: 1, in: 3, out: 30}})
	if _, err := d.Pass(nil); err != nil {
		t.Fatal(err)
	}
	got, _ = points(t, z)
	ps = got["subagent/output/m#s1"]
	if len(ps) != 3 || !ps[2].start.Equal(ps[1].end) || !ps[2].end.After(ps[2].start) || total(ps) != 60 {
		t.Fatalf("late data must follow the series' last point: %+v", ps)
	}
	// A later minute starts its own window.
	land(t, z, "s1", "a4", 4, []call{{id: "d", model: "m", at: base.Add(5 * time.Minute), frags: 1, in: 4, out: 40}})
	if _, err := d.Pass(nil); err != nil {
		t.Fatal(err)
	}
	got, _ = points(t, z)
	ps = got["subagent/output/m#s1"]
	if len(ps) != 4 || ps[3].start != base.Add(5*time.Minute) {
		t.Fatalf("a later minute must start at its own minute: %+v", ps)
	}
}

// A pass run again from the state before it, as after a crash between the
// spool and the state, writes the same files and nothing more.
func TestAPassRunAgainWritesNothingMore(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	land(t, z, "s1", "main", 1, []call{{id: "c1", model: "m", at: base, frags: 2, in: 1, out: 10}})
	land(t, z, "s1", "a1", 2, []call{{id: "c2", model: "m", at: base.Add(10 * time.Second), frags: 2, in: 2, out: 20}})
	d := deriver(z, base.Add(time.Hour), metrics.Options{})
	if _, err := d.Pass(nil); err != nil {
		t.Fatal(err)
	}
	before, n := points(t, z)
	if err := os.Remove(filepath.Join(storage.NewSpool(z).Dir(), metrics.StateFile)); err != nil {
		t.Fatal(err)
	}
	st, err := d.Pass(nil)
	if err != nil || st.Requests != 0 || st.Files != 2 {
		t.Fatalf("the pass again wrote %d requests over %d files: %v", st.Requests, st.Files, err)
	}
	after, m := points(t, z)
	if n != m || len(after) != len(before) || total(after["subagent/output/m#s1"]) != 20 {
		t.Fatalf("the spool changed: %d -> %d files", n, m)
	}
}

// The look-back bounds the first pass only, and the runtime's own metrics
// already in the spool bound it further: the derivation starts after the
// last request the receiver landed, so a switch of source counts nothing
// twice.
func TestLookbackAndReceivedMetricsBoundTheFirstPass(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	now := base
	land(t, z, "s1", "main", 1, []call{{id: "old", model: "m", at: now.Add(-48 * time.Hour), frags: 1, in: 100, out: 100}})
	land(t, z, "s1", "main", 2, []call{
		{id: "older", model: "m", at: now.Add(-25 * time.Hour), frags: 1, in: 40, out: 40},
		{id: "recent", model: "m", at: now.Add(-2 * time.Hour), frags: 1, in: 4, out: 4},
	})
	d := deriver(z, now, metrics.Options{Lookback: 24 * time.Hour})
	st, err := d.Pass(nil)
	if err != nil || len(st.Errors) != 0 || st.Files != 2 || st.Skipped != 1 || st.Requests != 1 {
		t.Fatalf("first pass: files=%d skipped=%d requests=%d err=%v %v", st.Files, st.Skipped, st.Requests, err, st.Errors)
	}
	got, _ := points(t, z)
	if total(got["main/input/m#s1"]) != 4 {
		t.Fatalf("the first pass must derive the recent minute only: %v", got)
	}
	land(t, z, "s1", "main", 3, []call{{id: "late", model: "m", at: now.Add(-72 * time.Hour), frags: 1, in: 9, out: 9}})
	if st, err = d.Pass(nil); err != nil || st.Requests != 1 {
		t.Fatalf("a later pass must derive an old file whole: requests=%d err=%v", st.Requests, err)
	}

	z2 := storage.NewZone(t.TempDir())
	land(t, z2, "s1", "main", 1, []call{
		{id: "before", model: "m", at: now.Add(-30 * time.Minute), frags: 1, in: 5, out: 5},
		{id: "after", model: "m", at: now.Add(-5 * time.Minute), frags: 1, in: 6, out: 6},
	})
	data, _ := proto.Marshal(&collmetricspb.ExportMetricsServiceRequest{})
	if _, err := storage.NewSpool(z2).Put("otlp", data, now.Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	d2 := deriver(z2, now, metrics.Options{Lookback: 24 * time.Hour})
	if _, err := d2.Pass(nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := points(t, z2); total(got["main/input/m#s1"]) != 6 {
		t.Fatalf("what the receiver already landed must not be derived again: %v", got)
	}
}
