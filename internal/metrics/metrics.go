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

// Package metrics derives the runtime's own metric family from landed
// Session Data, so a receiver sees the same metrics whether the runtime's
// exporter or asz produced them: the same names, the same attributes, the
// same delta temporality, aggregated per minute as the exporter aggregates
// over its interval. Phase one is the overlap: token usage. What the
// transcripts do not carry, cost, latency, active time and the rest, is the
// exporter's alone and is never estimated here.
package metrics

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	collmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// The family, as the runtime's exporter names it.
const (
	// TokenUsage counts tokens, by type and model, per session and per query
	// source; the exporter's claude_code.token.usage.
	TokenUsage = "claude_code.token.usage"
	tokenUnit  = "tokens"
	tokenDesc  = "Number of tokens used"

	// RuntimeService is the service name the runtime's exporter puts on its
	// resource. A derived request carries the same, and asz push normalises
	// both to asz's identity on the way out.
	RuntimeService = "claude-code"

	// ScopeName identifies what derived the points.
	ScopeName = "github.com/apache/skywalking-ai-sessionizer/metrics"

	// SourceLocal names the spool files this package writes.
	SourceLocal = "local"
)

// The token types, spelled as the exporter spells them.
const (
	TypeInput         = "input"
	TypeOutput        = "output"
	TypeCacheRead     = "cacheRead"
	TypeCacheCreation = "cacheCreation"
)

// The query sources the transcripts can tell apart. The exporter's third,
// auxiliary, is a call that never reaches a transcript.
const (
	SourceMain     = "main"
	SourceSubagent = "subagent"
)

// Options settle a derivation.
type Options struct {
	// Lookback bounds the first derivation over a root: a minute that ended
	// before now minus Lookback is not derived, so switching the flag on does
	// not send a year of history. Zero means everything. Only the first pass
	// is bounded; later passes derive every new file whole.
	Lookback time.Duration
	Version  string
	Now      func() time.Time
}

// Stats reports what one pass did.
type Stats struct {
	// Files is how many landed files were derived this pass; Requests how
	// many spool files were written, one per landed file with points.
	Files    int
	Requests int
	Points   int
	// Skipped counts files older than the look-back on the first pass.
	Skipped int
	Errors  []error
}

// Deriver derives from the landed files of a root that no earlier pass
// derived, and writes the requests to the root's spool. Which files were
// derived is kept in the spool's derived.state, by path and digest, so a
// file is derived once; which calls were counted is kept per session, so a
// call is counted once however many files its records reach. The runtime
// re-emits records before a context reset, and a landed file is cut at a
// budget, so the same call can sit in two files, and the runtime's
// exporter counted it once.
type Deriver struct {
	Zone *storage.Zone
	Options
}

// Pass derives what is new in the sessions named, or in every session when
// none is.
func (d *Deriver) Pass(sessions []string) (*Stats, error) {
	if d.Now == nil {
		d.Now = time.Now
	}
	st := &Stats{}
	spool := storage.NewSpool(d.Zone)
	state, first, err := loadState(filepath.Join(spool.Dir(), "derived.state"))
	if err != nil {
		return nil, err
	}
	var since time.Time
	if first && d.Lookback > 0 {
		since = d.Now().Add(-d.Lookback)
	}
	if sessions == nil {
		if sessions, err = sessionDirs(d.Zone.Root()); err != nil {
			return nil, err
		}
	}
	for _, session := range sessions {
		files, err := storage.LandedFiles(d.Zone, session)
		if err != nil {
			st.Errors = append(st.Errors, err)
			continue
		}
		callsPath := filepath.Join(spool.Dir(), "calls-"+session+".state")
		seen, err := loadCalls(callsPath)
		if err != nil {
			st.Errors = append(st.Errors, err)
			continue
		}
		counted := len(seen)
		for _, lf := range files {
			rel, _ := filepath.Rel(d.Zone.Root(), lf.Path)
			rel = filepath.ToSlash(rel)
			if state.has(rel) {
				continue
			}
			req, points, latest, digest, err := DeriveFile(lf.Path, since, d.Version, seen)
			if err != nil {
				st.Errors = append(st.Errors, fmt.Errorf("%s: %w", rel, err))
				continue
			}
			if !since.IsZero() && !latest.IsZero() && latest.Before(since) {
				st.Skipped++
			}
			if points > 0 {
				data, err := proto.Marshal(req)
				if err != nil {
					st.Errors = append(st.Errors, err)
					continue
				}
				if _, err := spool.Put(SourceLocal, data, d.Now()); err != nil {
					st.Errors = append(st.Errors, err)
					continue
				}
				st.Requests++
				st.Points += points
			}
			st.Files++
			state.mark(rel, digest)
		}
		if len(seen) != counted {
			if err := saveCalls(callsPath, seen, d.Now()); err != nil {
				return nil, err
			}
		}
	}
	if err := state.save(filepath.Join(spool.Dir(), "derived.state"), d.Now()); err != nil {
		return nil, err
	}
	return st, nil
}

// loadCalls reads the calls counted so far for one session, one id per
// line after the header.
func loadCalls(path string) (map[string]bool, error) {
	seen := map[string]bool{}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return seen, nil
		}
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if id, ok := strings.CutPrefix(sc.Text(), "call "); ok {
			seen[id] = true
		}
	}
	return seen, sc.Err()
}

func saveCalls(path string, seen map[string]bool, now time.Time) error {
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return storage.WriteAtomic(path, storage.PermState, func(w io.Writer) error {
		bw := bufio.NewWriter(w)
		fmt.Fprintf(bw, "schema 1\nupdated_at %s\n", now.UTC().Format(time.RFC3339Nano))
		for _, id := range ids {
			fmt.Fprintf(bw, "call %s\n", id)
		}
		return bw.Flush()
	})
}

// point is one minute of one attribute set.
type point struct {
	minute  time.Time
	session string
	model   string
	source  string
	typ     string
	value   int64
}

// DeriveFile derives the token usage of one landed file: one count per
// call, never per fragment, since a main transcript repeats the usage on
// every fragment of a call, and never for a call in seen, which the caller
// keeps across the files of a session; summed per minute and attribute
// set. Minutes that ended before since are left out. It returns the
// request, how many points it holds, the latest record time in the file,
// and the file's digest, and adds the calls it counted to seen.
func DeriveFile(path string, since time.Time, version string, seen map[string]bool) (*collmetricspb.ExportMetricsServiceRequest, int, time.Time, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, time.Time{}, "", err
	}
	defer f.Close()
	h := sha256.New()
	rd, err := sessiondata.NewReader(io.TeeReader(f, h))
	if err != nil {
		return nil, 0, time.Time{}, "", err
	}
	hdr := rd.Header()
	source := SourceSubagent
	if hdr.Stream == "main" {
		source = SourceMain
	}
	sums := map[point]int64{}
	if seen == nil {
		seen = map[string]bool{}
	}
	var latest time.Time
	for {
		rec, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, 0, time.Time{}, "", err
		}
		t, ok := recordTime(rec)
		if ok && t.After(latest) {
			latest = t
		}
		if hdr.Kind != sessiondata.KindTranscript || rec.Call == "" || rec.Usage == nil || seen[rec.Call] || !ok {
			continue
		}
		seen[rec.Call] = true
		minute := t.Truncate(time.Minute)
		if !since.IsZero() && minute.Add(time.Minute).Before(since) {
			continue
		}
		for typ, n := range map[string]int{
			TypeInput: rec.Usage.Input, TypeOutput: rec.Usage.Output,
			TypeCacheRead: rec.Usage.CacheRead, TypeCacheCreation: rec.Usage.CacheWrite,
		} {
			if n == 0 {
				continue
			}
			k := point{minute: minute, session: hdr.Session, model: rec.Model, source: source, typ: typ}
			sums[k] += int64(n)
		}
	}
	// Drain what the reader did not consume, so the digest is the file's.
	if _, err := io.Copy(io.Discard, f); err != nil {
		return nil, 0, time.Time{}, "", err
	}
	digest := hex.EncodeToString(h.Sum(nil))
	points := make([]point, 0, len(sums))
	for k, v := range sums {
		k.value = v
		points = append(points, k)
	}
	// The same file derives the same bytes: points in one order.
	sort.Slice(points, func(i, j int) bool {
		a, b := points[i], points[j]
		if !a.minute.Equal(b.minute) {
			return a.minute.Before(b.minute)
		}
		if a.session != b.session {
			return a.session < b.session
		}
		if a.model != b.model {
			return a.model < b.model
		}
		if a.source != b.source {
			return a.source < b.source
		}
		return a.typ < b.typ
	})
	return request(points, version), len(points), latest, digest, nil
}

// request builds the exporter's request for the points: one resource, one
// scope, one metric, delta sums that are monotonic, as the exporter's SDK
// writes them.
func request(points []point, version string) *collmetricspb.ExportMetricsServiceRequest {
	dps := make([]*metricspb.NumberDataPoint, 0, len(points))
	for _, p := range points {
		attrs := []*commonpb.KeyValue{
			str("session.id", p.session),
			str("type", p.typ),
			str("query_source", p.source),
		}
		if p.model != "" {
			attrs = append(attrs, str("model", p.model))
		}
		dps = append(dps, &metricspb.NumberDataPoint{
			Attributes:        attrs,
			StartTimeUnixNano: uint64(p.minute.UnixNano()),
			TimeUnixNano:      uint64(p.minute.Add(time.Minute).UnixNano()),
			Value:             &metricspb.NumberDataPoint_AsInt{AsInt: p.value},
		})
	}
	return &collmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{str("service.name", RuntimeService)}},
		ScopeMetrics: []*metricspb.ScopeMetrics{{
			Scope: &commonpb.InstrumentationScope{Name: ScopeName, Version: version},
			Metrics: []*metricspb.Metric{{
				Name: TokenUsage, Description: tokenDesc, Unit: tokenUnit,
				Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
					DataPoints:             dps,
					AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
					IsMonotonic:            true,
				}},
			}},
		}},
	}}}
}

func str(key, val string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: val}}}
}

func recordTime(rec *sessiondata.Record) (time.Time, bool) {
	if rec.Time == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, rec.Time)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func sessionDirs(root string) ([]string, error) {
	items, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range items {
		if d.IsDir() && !strings.HasPrefix(d.Name(), "_") {
			out = append(out, d.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// derivedState is the set of landed files derived so far, with the digest
// each had.
type derivedState struct{ files map[string]string }

func loadState(path string) (*derivedState, bool, error) {
	s := &derivedState{files: map[string]string{}}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, true, nil
		}
		return nil, false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 3 && fields[0] == "derived" {
			s.files[fields[1]] = fields[2]
		}
	}
	return s, false, sc.Err()
}

func (s *derivedState) has(rel string) bool     { _, ok := s.files[rel]; return ok }
func (s *derivedState) mark(rel, digest string) { s.files[rel] = digest }

func (s *derivedState) save(path string, now time.Time) error {
	keys := make([]string, 0, len(s.files))
	for k := range s.files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return storage.WriteAtomic(path, storage.PermState, func(w io.Writer) error {
		bw := bufio.NewWriter(w)
		fmt.Fprintf(bw, "schema 1\nupdated_at %s\n", now.UTC().Format(time.RFC3339Nano))
		for _, k := range keys {
			fmt.Fprintf(bw, "derived %s %s\n", k, s.files[k])
		}
		return bw.Flush()
	})
}
