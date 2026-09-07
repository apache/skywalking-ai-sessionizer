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

// Package metrics derives the runtime's own token metric from landed
// Session Data, so a receiver holds one metric name whichever produced the
// points: the runtime's exporter or asz. It is a reconstructed subset of
// the exporter's family, not a copy of it. The name, the unit, the delta
// temporality and the token types are the exporter's; the labels are the
// ones a transcript can supply, the session, the model and the query source;
// the exporter's account, organisation, speed, effort and attribution labels
// are not, and neither are its auxiliary calls, which never reach a
// transcript. What the transcripts lack is never estimated.
//
// Usage follows the assembler's rule: the last fragment of a call in line
// order, never a sum, and only a call that finished. A main transcript
// repeats the final usage on every fragment; a child's carries streaming
// partials on all but the last.
package metrics

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// The metric, as the runtime's exporter names it.
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

	// ScopeName identifies what derived the points. It is asz's own, since
	// the derivation is not the runtime's instrumentation; a receiver that
	// keys on the scope sees two streams of one metric, as the export page
	// says.
	ScopeName = "github.com/apache/skywalking-ai-sessionizer/metrics"

	// SourceLocal names the spool files this package writes.
	SourceLocal = "local"

	// DefaultGrace is how long a landed file whose last record may be a
	// call continued in the next file waits for that file.
	DefaultGrace = 2 * time.Minute

	// StateFile holds what was derived and counted, under the spool.
	StateFile = "metrics.state"
)

// The token types, spelled as the exporter spells them.
const (
	TypeInput         = "input"
	TypeOutput        = "output"
	TypeCacheRead     = "cacheRead"
	TypeCacheCreation = "cacheCreation"
)

// Labels the exporter puts on its token points that a transcript cannot
// supply, so the derivation leaves them out. The parity test holds the
// difference between the derived point and a captured one to this list.
var NotDerivedLabels = []string{
	"user.id", "user.email", "user.account_uuid", "user.account_id", "organization.id",
	"terminal.type", "effort", "speed",
	"agent.name", "skill.name", "plugin.name", "marketplace.name", "mcp_server.name", "mcp_tool.name",
}

// NotDerivedMetrics are the exporter's other metrics, which a transcript
// cannot supply and which are never estimated. The parity test fails when
// a capture carries a metric that is on neither list.
var NotDerivedMetrics = []string{
	"claude_code.session.count", "claude_code.lines_of_code.count", "claude_code.pull_request.count",
	"claude_code.commit.count", "claude_code.cost.usage", "claude_code.code_edit_tool.decision",
	"claude_code.active_time.total",
}

// The query sources the transcripts can tell apart. The exporter's third,
// auxiliary, is a call that never reaches a transcript.
const (
	SourceMain     = "main"
	SourceSubagent = "subagent"
)

// Options settle a derivation.
type Options struct {
	// Lookback bounds the first derivation over a root: a minute that ended
	// before now minus Lookback is not derived, so switching the flag on
	// does not send a year of history. Zero means everything. Only the
	// first pass is bounded; later passes derive every new file whole.
	Lookback time.Duration
	// Grace is how long a file whose last record is a fragment of a call
	// waits for the next file of its stream, which may hold the call's end.
	// Zero means DefaultGrace; negative means never wait.
	Grace   time.Duration
	Version string
	Now     func() time.Time
}

// Stats reports what one pass did.
type Stats struct {
	// Files is how many landed files were derived this pass; Requests how
	// many spool files were written, one per landed file with points.
	Files    int
	Requests int
	Points   int
	// Skipped counts files older than the look-back on the first pass;
	// Deferred counts files left for a later pass because their last call
	// may continue in a file not yet landed.
	Skipped  int
	Deferred int
	Errors   []error
}

// Deriver derives from the landed files of a root that no earlier pass
// derived, and writes the requests to the root's spool.
//
// A pass is deterministic and idempotent: what it writes depends only on
// the landed files and the state the pass before it saved, a derived
// request is named after its landed file and never rewritten, and the
// state is saved once, at the end. A pass cut short by a crash is run
// again from the saved state and writes the same files, so nothing is
// counted twice and nothing is lost.
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
	grace := d.Grace
	if grace == 0 {
		grace = DefaultGrace
	}
	st := &Stats{}
	spool := storage.NewSpool(d.Zone)
	state, first, err := loadState(filepath.Join(spool.Dir(), StateFile))
	if err != nil {
		return nil, err
	}
	var since time.Time
	if first {
		if d.Lookback > 0 {
			since = d.Now().Add(-d.Lookback)
		}
		// The runtime's exporter may have been the source until now: what
		// it sent is in the spool with the time it was received, and the
		// derivation starts after the last of it, so a switch of source
		// counts nothing twice.
		if received, err := latestReceived(spool); err == nil && received.After(since) {
			since = received
		}
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
		ss := state.session(session)
		for i, lf := range files {
			rel, _ := filepath.Rel(d.Zone.Root(), lf.Path)
			rel = filepath.ToSlash(rel)
			if state.Derived[rel] != "" {
				continue
			}
			next := nextOfStream(files, i)
			res, err := deriveFile(lf, next, since, ss)
			if err != nil {
				st.Errors = append(st.Errors, fmt.Errorf("%s: %w", rel, err))
				continue
			}
			if res.openTail && next == nil && grace > 0 {
				if fi, err := os.Stat(lf.Path); err == nil && d.Now().Sub(fi.ModTime()) < grace {
					st.Deferred++
					continue
				}
			}
			if !since.IsZero() && !res.latest.IsZero() && res.latest.Before(since) {
				st.Skipped++
			}
			if len(res.points) > 0 {
				data, err := proto.Marshal(request(res.points, d.Version))
				if err != nil {
					st.Errors = append(st.Errors, err)
					continue
				}
				name := fmt.Sprintf("metrics-%s-%06d-%s.pb", session, lf.Seq, SourceLocal)
				_, created, err := spool.PutNamed(name, data)
				if err != nil {
					st.Errors = append(st.Errors, err)
					continue
				}
				// A request that was there already, from a pass cut short
				// after writing it, is not a new one.
				if created {
					st.Requests++
					st.Points += len(res.points)
				}
			}
			// The file's effect on the state is applied only now, with its
			// request on disk, so a failed write is derived again.
			res.commit(ss)
			state.Derived[rel] = res.digest
			st.Files++
		}
	}
	if err := state.save(filepath.Join(spool.Dir(), StateFile), d.Now()); err != nil {
		return nil, err
	}
	return st, nil
}

// nextOfStream is the landed file after files[i] on the same stream, or nil.
func nextOfStream(files []storage.LandedFile, i int) *storage.LandedFile {
	for j := i + 1; j < len(files); j++ {
		if files[j].Stream == files[i].Stream && files[i].Stream != "" {
			return &files[j]
		}
	}
	return nil
}

// latestReceived is when the newest request from the runtime's exporter was
// put in the spool; zero when there is none.
func latestReceived(spool *storage.Spool) (time.Time, error) {
	files, err := spool.List()
	if err != nil {
		return time.Time{}, err
	}
	var latest time.Time
	for _, f := range files {
		if f.Source != SourceLocal && f.At.After(latest) {
			latest = f.At
		}
	}
	return latest, nil
}

// call is what one provider call's fragments said, as far as they were read:
// the last fragment's usage and model, its time, and whether the call
// finished.
type call struct {
	id       string
	model    string
	source   string
	at       time.Time
	usage    *sessiondata.Usage
	finished bool
}

// series names one time series of the metric: what a point's attributes
// name, apart from the session, which is the state's own.
type series struct{ model, source, typ string }

func (s series) key() string { return s.model + "|" + s.source + "|" + s.typ }

// point is one minute of one series, with the window it was given.
type point struct {
	series  series
	session string
	minute  time.Time
	start   time.Time
	end     time.Time
	value   int64
}

// result is what deriving one file found, held until its request is on
// disk and then committed to the session's state.
type result struct {
	points   []point
	counted  []string
	lastEnd  map[string]time.Time
	digest   string
	latest   time.Time
	openTail bool
}

func (r *result) commit(ss *sessionState) {
	for _, id := range r.counted {
		ss.Calls[id] = true
	}
	for k, t := range r.lastEnd {
		ss.Series[k] = t.UnixNano()
	}
}

// deriveFile reads one landed file's calls and turns the ones that finished
// into points. A call whose last fragment is the file's last record may go
// on in the next file of the stream; when that file exists its leading
// fragments of the call are read too, and the call is counted here, once.
// Points take the minute the call's last fragment falls in, and a window
// that never overlaps the series' last: a minute already passed by an
// earlier point follows that point instead.
func deriveFile(lf storage.LandedFile, next *storage.LandedFile, since time.Time, ss *sessionState) (*result, error) {
	f, err := os.Open(lf.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	rd, err := sessiondata.NewReader(io.TeeReader(f, h))
	if err != nil {
		return nil, err
	}
	hdr := rd.Header()
	source := SourceSubagent
	if hdr.Stream == "main" {
		source = SourceMain
	}
	res := &result{lastEnd: map[string]time.Time{}}
	calls := map[string]*call{}
	var order []string
	var last *call
	var tail bool
	for {
		rec, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		t, ok := recordTime(rec)
		if ok && t.After(res.latest) {
			res.latest = t
		}
		tail = false
		if hdr.Kind != sessiondata.KindTranscript || rec.Call == "" {
			continue
		}
		c := calls[rec.Call]
		if c == nil {
			c = &call{id: rec.Call, source: source}
			calls[rec.Call] = c
			order = append(order, rec.Call)
		}
		c.take(rec, t, ok)
		last, tail = c, true
	}
	// Drain what the reader did not consume, so the digest is the file's.
	if _, err := io.Copy(io.Discard, f); err != nil {
		return nil, err
	}
	res.digest = hex.EncodeToString(h.Sum(nil))
	if tail && last != nil && !ss.Calls[last.id] {
		if next != nil {
			if err := continueIn(next.Path, last); err != nil {
				return nil, err
			}
		} else {
			res.openTail = true
		}
	}
	sums := map[point]int64{}
	for _, id := range order {
		c := calls[id]
		if ss.Calls[id] {
			continue
		}
		res.counted = append(res.counted, id)
		if !c.finished || c.usage == nil || c.at.IsZero() {
			continue
		}
		minute := c.at.Truncate(time.Minute)
		if !since.IsZero() && minute.Add(time.Minute).Before(since) {
			continue
		}
		// A point for every type, zero included: the exporter writes all
		// four for each call, and a receiver reading one family should not
		// see a type appear and vanish by source.
		for typ, n := range map[string]int{
			TypeInput: c.usage.Input, TypeOutput: c.usage.Output,
			TypeCacheRead: c.usage.CacheRead, TypeCacheCreation: c.usage.CacheWrite,
		} {
			k := point{series: series{model: c.model, source: c.source, typ: typ}, session: hdr.Session, minute: minute}
			sums[k] += int64(n)
		}
	}
	points := make([]point, 0, len(sums))
	for k, v := range sums {
		k.value = v
		points = append(points, k)
	}
	// The same file derives the same bytes: points in one order, and the
	// windows follow from it.
	sort.Slice(points, func(i, j int) bool {
		a, b := points[i], points[j]
		if !a.minute.Equal(b.minute) {
			return a.minute.Before(b.minute)
		}
		return a.series.key() < b.series.key()
	})
	for i := range points {
		p := &points[i]
		prev := res.lastEnd[p.series.key()]
		if prev.IsZero() {
			if ns, ok := ss.Series[p.series.key()]; ok {
				prev = time.Unix(0, ns).UTC()
			}
		}
		p.start, p.end = p.minute, p.minute.Add(time.Minute)
		if !prev.IsZero() && !prev.Before(p.start) {
			// The series has a point at or past this minute already: this
			// one follows it, so no two windows of the series overlap and
			// none is thrown away for it.
			p.start = prev
			if !p.end.After(p.start) {
				p.end = p.start.Add(time.Millisecond)
			}
		}
		res.lastEnd[p.series.key()] = p.end
	}
	res.points = points
	return res, nil
}

// take folds one fragment into the call: the last fragment's usage, model
// and time win, and the call has finished once any fragment says so.
func (c *call) take(rec *sessiondata.Record, t time.Time, ok bool) {
	if rec.Usage != nil {
		c.usage = rec.Usage
	}
	if rec.Model != "" {
		c.model = rec.Model
	}
	if ok {
		c.at = t
	}
	for _, f := range rec.Flags {
		if f == "finished" {
			c.finished = true
		}
	}
}

// continueIn reads the leading fragments of a call from the next file of
// its stream: the file was cut at a budget, and the call's end is there.
func continueIn(path string, c *call) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	rd, err := sessiondata.NewReader(f)
	if err != nil {
		return err
	}
	for {
		rec, err := rd.Next()
		if errors.Is(err, io.EOF) || (err == nil && rec.Call != c.id) {
			return nil
		}
		if err != nil {
			return err
		}
		t, ok := recordTime(rec)
		c.take(rec, t, ok)
	}
}

// request builds the exporter's request for the points: one resource, one
// scope, one metric, delta sums that are monotonic, as the exporter's SDK
// writes them.
func request(points []point, version string) *collmetricspb.ExportMetricsServiceRequest {
	dps := make([]*metricspb.NumberDataPoint, 0, len(points))
	for _, p := range points {
		attrs := []*commonpb.KeyValue{
			str("session.id", p.session),
			str("type", p.series.typ),
			str("query_source", p.series.source),
		}
		if p.series.model != "" {
			attrs = append(attrs, str("model", p.series.model))
		}
		// A double, as the exporter's SDK encodes its counters, so a
		// receiver reads one value kind from both sources.
		dps = append(dps, &metricspb.NumberDataPoint{
			Attributes:        attrs,
			StartTimeUnixNano: uint64(p.start.UnixNano()),
			TimeUnixNano:      uint64(p.end.UnixNano()),
			Value:             &metricspb.NumberDataPoint_AsDouble{AsDouble: float64(p.value)},
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
	return t.UTC(), true
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

// state is everything a pass depends on besides the landed files: which
// files were derived, which calls were counted, and where each series'
// last window ended. One file, written once per pass.
type state struct {
	Schema    int                      `json:"schema"`
	UpdatedAt string                   `json:"updated_at"`
	Derived   map[string]string        `json:"derived"`
	Sessions  map[string]*sessionState `json:"sessions"`
}

type sessionState struct {
	Calls  map[string]bool  `json:"calls"`
	Series map[string]int64 `json:"series"`
}

func (s *state) session(id string) *sessionState {
	ss := s.Sessions[id]
	if ss == nil {
		ss = &sessionState{Calls: map[string]bool{}, Series: map[string]int64{}}
		s.Sessions[id] = ss
	}
	return ss
}

func loadState(path string) (*state, bool, error) {
	s := &state{Schema: 1, Derived: map[string]string{}, Sessions: map[string]*sessionState{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, true, nil
		}
		return nil, false, err
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, false, fmt.Errorf("%s: %w", path, err)
	}
	if s.Derived == nil {
		s.Derived = map[string]string{}
	}
	if s.Sessions == nil {
		s.Sessions = map[string]*sessionState{}
	}
	for _, ss := range s.Sessions {
		if ss.Calls == nil {
			ss.Calls = map[string]bool{}
		}
		if ss.Series == nil {
			ss.Series = map[string]int64{}
		}
	}
	return s, false, nil
}

func (s *state) save(path string, now time.Time) error {
	s.Schema, s.UpdatedAt = 1, now.UTC().Format(time.RFC3339Nano)
	data, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return err
	}
	return storage.WriteAtomic(path, storage.PermState, func(w io.Writer) error {
		_, err := w.Write(append(data, '\n'))
		return err
	})
}
