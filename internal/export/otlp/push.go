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

package otlp

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// Pusher sends every file in a storage root that has not been sent yet.
//
// A file is sent whole, as one log record whose body is the file's bytes.
// A receiver stores it as it was landed and checks the digest at once, and
// nothing has to be put back together. Sending a record per line was
// measured first: 39,683 records for one conversation of 306 files, with the
// attributes 16% on top of the body, and a receiver that had to track which
// lines had arrived. Landed files are cut at a budget and a round is small
// next to the files it reads, so a whole file is a reasonable record.
// Landed files and rounds are both write-once, so a file is sent once;
// push.state in the root records which ones were.
type Pusher struct {
	Zone *storage.Zone
	// Client is the receiver, over either transport; see NewClient.
	Client Client
	// Endpoint names the receiver Client sends to, as EndpointOf gives it.
	// push.state records it beside what was sent, so a reader can tell which
	// receiver took the files. Empty records no receiver, and then nothing
	// that asks where the files went trusts what this pusher sent.
	Endpoint string
	// Version is what the sender reports about itself.
	Version string
	// ServiceName is the service every record is attributed to, when one is
	// configured. Empty means the runtime that produced each session, read
	// off its landed header's adapter and named through Runtimes.
	ServiceName string
	// Runtimes names the runtime behind each adapter: claude-code-local is
	// Claude Code, mock is Mock Agent. An adapter with no entry is named by
	// its own name.
	Runtimes map[string]string
	// InstanceID identifies this sender as service.instance.id: who is
	// pushing, in words the people reading the receiver recognise, such as
	// a mailbox, a name or a machine. Empty means user@host of the machine
	// running the push, which is stable across restarts and already says
	// where the records come from.
	InstanceID string
	// Layer is the receiver's layer for the service, sent as service.layer.
	Layer string
	// NoLogs and NoMetrics leave out one of the two things a pass sends,
	// the landed files and rounds as logs, and the metrics spool, for a
	// receiver that takes only the other.
	NoLogs    bool
	NoMetrics bool
	// MetricsService is the service the metrics spool is attributed to: the
	// requests the local adapter derived and the ones the runtime's exporter
	// sent both concern the one runtime, and they leave under its name so a
	// receiver holds one service whichever produced the points. Empty means
	// ServiceName.
	MetricsService string
	// BatchBytes is how many file bytes one request carries at most. A file
	// larger than the budget is sent alone, in a request of its own.
	BatchBytes int64
	// WaitForExport is how long Pass waits for another pass over the same
	// root to finish before giving up with storage.ErrExportBusy. Zero, the
	// default, does not wait, which is what a watching pass wants: it comes
	// round again anyway.
	WaitForExport time.Duration

	// MaxBytesPerMinute caps what goes on the wire: a pass waits before a
	// request until a minute's budget, refilled continuously, holds the
	// request's size. Zero, the default, is no limit. A first push of a
	// large history is what it is for: 800 MB of landed files would
	// otherwise go out as fast as the receiver takes them.
	MaxBytesPerMinute int64
	Now               func() time.Time
	// Sleep is how the pass waits. The tests replace it with a clock.
	Sleep func(time.Duration)

	limit *limiter
}

// Stats reports what one pass did.
type Stats struct {
	Files    int
	Bytes    int64
	Wire     int64 // bytes on the wire, requests as encoded
	Requests int
	// Metrics is how many spooled metrics requests were sent.
	Metrics int
	// Deferred is how many rounds were left for a later pass because they
	// were still being written. A pass that comes round again picks them
	// up; a pass that does not has to report them.
	Deferred int

	// Rejected is how many records and data points receivers said they
	// rejected inside requests they took. The protocol says not to resend
	// them, so they are counted and reported, and the files are marked.
	Rejected int64
	Paused   time.Duration // how long the pass waited for budget
	// Throttled says the receiver asked the sender to slow down and the
	// pass stopped there; RetryAfter is the wait it named, if any.
	Throttled  bool
	RetryAfter time.Duration
	Errors     []error
}

// limiter is a token bucket in bytes: a minute's budget, refilled
// continuously and never holding more than a minute's worth. It starts
// full, so a small pass never waits.
type limiter struct {
	rate   int64
	tokens float64
	last   time.Time
	now    func() time.Time
	sleep  func(time.Duration)
}

func newLimiter(rate int64, now func() time.Time, sleep func(time.Duration)) *limiter {
	return &limiter{rate: rate, tokens: float64(rate), last: now(), now: now, sleep: sleep}
}

func (l *limiter) refill() {
	now := l.now()
	if elapsed := now.Sub(l.last); elapsed > 0 {
		l.tokens += elapsed.Minutes() * float64(l.rate)
		if l.tokens > float64(l.rate) {
			l.tokens = float64(l.rate)
		}
	}
	l.last = now
}

// take waits until n bytes may go, and returns how long it waited. A
// request larger than a minute's budget waits for a full bucket, goes,
// and leaves it empty, so the next request waits a whole minute.
func (l *limiter) take(n int64) time.Duration {
	if l == nil || l.rate <= 0 {
		return 0
	}
	l.refill()
	need := float64(n)
	if need > float64(l.rate) {
		need = float64(l.rate)
	}
	var waited time.Duration
	if l.tokens < need {
		short := (need - l.tokens) / float64(l.rate) * float64(time.Minute)
		waited = time.Duration(math.Ceil(short/float64(time.Millisecond))) * time.Millisecond
		l.sleep(waited)
		l.refill()
	}
	l.tokens -= need
	if l.tokens < 0 {
		l.tokens = 0
	}
	return waited
}

// drain empties the bucket: after a receiver asked to slow down, the next
// request waits a whole minute's budget.
func (l *limiter) drain() {
	if l == nil {
		return
	}
	l.refill()
	l.tokens = 0
}

// ScopeName identifies the sender in every request.
const ScopeName = "github.com/apache/skywalking-ai-sessionizer"

// Prepare settles the defaults: the batch budget, the clock, and the
// instance id, which is a new UUID when none was configured. Pass calls it,
// and a caller may call it first to learn the instance id.
func (p *Pusher) Prepare() error {
	if p.Client == nil {
		return errors.New("otlp: no client")
	}
	if p.ServiceName == "" && len(p.Runtimes) == 0 {
		return errors.New("otlp: no service name and no runtime names")
	}
	if p.BatchBytes <= 0 {
		p.BatchBytes = 8 << 20
	}
	if p.Now == nil {
		p.Now = time.Now
	}
	if p.Sleep == nil {
		p.Sleep = time.Sleep
	}
	if p.limit == nil {
		p.limit = newLimiter(p.MaxBytesPerMinute, p.Now, p.Sleep)
	}
	if p.InstanceID == "" {
		id, err := defaultInstance()
		if err != nil {
			return err
		}
		p.InstanceID = id
	}
	if p.MetricsService == "" {
		p.MetricsService = p.ServiceName
	}
	return nil
}

// Pass sends what is not yet sent, session by session, the session landed
// first going first: its landed files by sequence, then the rounds of its
// conversation. A receiver rebuilds a session once it holds the session's
// files and rounds, so under a rate limit the sessions of a long first
// push become complete one after another rather than all at the end.
func (p *Pusher) Pass() (*Stats, error) {
	if err := p.Prepare(); err != nil {
		return nil, err
	}
	// One pusher at a time over one root. Reading push.state, sending, and
	// recording what went is one operation; two passes running it at once
	// would both send what neither had recorded yet, and a token counted
	// twice cannot be taken back. storage.ErrExportBusy says another pass
	// holds it, which is not a failure: it is already doing this work.
	// The root has to be there before the lock is taken: taking it creates a
	// directory under the root, which would make a mistyped path look like
	// an empty one and send nothing without a word.
	if _, err := os.Stat(p.Zone.Root()); err != nil {
		return nil, err
	}
	// A pass told to wait is one that was asked to send: it must not report
	// success while another pass holds the state, because files that landed
	// after that pass listed its work would be left unsent with nobody
	// coming back for them. A watching pass waits for nothing and skips.
	lock, err := storage.LockExportWait(p.Zone.Root(), p.WaitForExport)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Unlock() }()
	st := &Stats{}
	state, err := loadState(p.statePath())
	if err != nil {
		return nil, err
	}
	sessions, err := p.sessionsOldestFirst()
	if err != nil {
		return nil, err
	}
	b := &batch{p: p, st: st, state: state, services: map[string]string{}}
	if p.NoLogs {
		sessions = nil
	}
	sent := map[string]bool{}
	for _, s := range sessions {
		if b.stop != nil {
			break
		}
		// A session's records are attributed to the runtime that produced
		// them, which its landed headers name.
		b.services[s.id] = p.serviceOf(s.files)
		for _, lf := range s.files {
			if b.stop != nil {
				break
			}
			rel, _ := filepath.Rel(p.Zone.Root(), lf.Path)
			rel = filepath.ToSlash(rel)
			if state.pushed(rel) {
				continue
			}
			if err := b.addLanded(rel, lf, s.id, s.files); err != nil {
				st.Errors = append(st.Errors, fmt.Errorf("%s: %w", rel, err))
			}
		}
		b.addRounds(s.id)
		sent[s.id] = true
	}
	// A conversation that is not a session of this root, such as one
	// assembled from several, goes after the sessions.
	var convs []string
	if !p.NoLogs {
		if convs, err = conversationDirs(p.Zone.Root()); err != nil {
			st.Errors = append(st.Errors, err)
		}
	}
	for _, conv := range convs {
		if b.stop != nil {
			break
		}
		if !sent[conv] {
			b.addRounds(conv)
		}
	}
	if b.stop == nil {
		if err := b.flush(); err != nil {
			st.Errors = append(st.Errors, err)
		}
	}
	if b.stop == nil && !p.NoMetrics {
		p.pushSpool(b)
	}
	if b.stop != nil {
		st.Throttled, st.RetryAfter = true, b.stop.After
	}
	return st, nil
}

// pushSpool sends the metrics spool: every request not yet sent, in the
// order it was put, one request each, under the same budget and the same
// once-only rule as the files. The resource is normalised on the way out
// to asz's identity, so a receiver holds one service for the runtime
// whether asz derived the points or the runtime's exporter sent them.
func (p *Pusher) pushSpool(b *batch) {
	st, state := b.st, b.state
	files, err := storage.NewSpool(p.Zone).List()
	if err != nil {
		st.Errors = append(st.Errors, err)
		return
	}
	for _, sf := range files {
		rel, _ := filepath.Rel(p.Zone.Root(), sf.Path)
		rel = filepath.ToSlash(rel)
		if state.pushed(rel) {
			continue
		}
		if p.MetricsService == "" {
			st.Errors = append(st.Errors, fmt.Errorf("%s: no service to attribute metrics to", rel))
			return
		}
		data, err := os.ReadFile(sf.Path)
		if err != nil {
			st.Errors = append(st.Errors, err)
			continue
		}
		var req collmetricspb.ExportMetricsServiceRequest
		if err := proto.Unmarshal(data, &req); err != nil {
			st.Errors = append(st.Errors, fmt.Errorf("%s: not a metrics request: %w", rel, err))
			continue
		}
		for _, rm := range req.ResourceMetrics {
			p.normalise(rm)
		}
		size := int64(proto.Size(&req))
		st.Paused += p.limit.take(size)
		rejected, err := p.Client.ExportMetrics(&req)
		st.Requests++
		st.Wire += size
		st.Rejected += rejected
		if err != nil {
			st.Errors = append(st.Errors, fmt.Errorf("%s: %w", rel, err))
			var t *Throttled
			if errors.As(err, &t) {
				b.stop = t
				p.limit.drain()
				return
			}
			continue
		}
		state.mark(rel, digestOf(data), p.Endpoint)
		// A partial success is not sent again, as the protocol says. It is
		// recorded, so no reader takes the file as received whole.
		if rejected > 0 {
			state.reject(rel)
		}
		st.Metrics++
		if err := state.save(p.statePath(), p.Now()); err != nil {
			st.Errors = append(st.Errors, err)
		}
	}
}

// normalise puts asz's identity on a resource: the runtime as the service,
// the layer, who is pushing, and the sender. Everything else the resource
// carried, such as the runtime's own version, stays.
func (p *Pusher) normalise(rm *metricspb.ResourceMetrics) {
	if rm.Resource == nil {
		rm.Resource = &resourcepb.Resource{}
	}
	ours := map[string]string{
		"service.name": p.MetricsService, "service.instance.id": p.InstanceID,
		"telemetry.sdk.name": "asz", "telemetry.sdk.version": p.Version, "telemetry.sdk.language": "go",
	}
	if p.Layer != "" {
		ours["service.layer"] = p.Layer
	}
	kept := rm.Resource.Attributes[:0]
	for _, kv := range rm.Resource.Attributes {
		if _, replaced := ours[kv.GetKey()]; !replaced {
			kept = append(kept, kv)
		}
	}
	keys := make([]string, 0, len(ours))
	for k := range ours {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		kept = append(kept, str(k, ours[k]))
	}
	rm.Resource.Attributes = kept
}

// addRounds queues every round of a conversation not yet sent.
func (b *batch) addRounds(conv string) {
	rounds, err := roundFiles(b.p.Zone.Root(), conv)
	if err != nil {
		b.st.Errors = append(b.st.Errors, err)
		return
	}
	for _, path := range rounds {
		if b.stop != nil {
			return
		}
		rel, _ := filepath.Rel(b.p.Zone.Root(), path)
		rel = filepath.ToSlash(rel)
		if b.state.pushed(rel) {
			continue
		}
		if err := b.addRound(rel, path, conv); err != nil {
			// A round still being written is left for the next pass, not
			// reported as a failure: another builder is finishing it right
			// now. It is counted, so a pass with no next one can say so.
			if errors.Is(err, errRoundUnfinished) {
				b.st.Deferred++
				continue
			}
			b.st.Errors = append(b.st.Errors, fmt.Errorf("%s: %w", rel, err))
		}
	}
}

// session is one session of the root with its landed files, and when the
// first of them was landed.
type session struct {
	id     string
	files  []storage.LandedFile
	landed time.Time
}

// sessionsOldestFirst lists the root's sessions in the order they were
// first landed, so a long first push sends history in order.
func (p *Pusher) sessionsOldestFirst() ([]session, error) {
	names, err := sessionDirs(p.Zone.Root())
	if err != nil {
		return nil, err
	}
	out := make([]session, 0, len(names))
	for _, id := range names {
		files, err := storage.LandedFiles(p.Zone, id)
		if err != nil {
			return nil, err
		}
		s := session{id: id, files: files}
		if len(files) > 0 {
			if fi, err := os.Stat(files[0].Path); err == nil {
				s.landed = fi.ModTime()
			}
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].landed.Equal(out[j].landed) {
			return out[i].id < out[j].id
		}
		return out[i].landed.Before(out[j].landed)
	})
	return out, nil
}

func (p *Pusher) statePath() string { return filepath.Join(p.Zone.Root(), StateFile) }

// batch accumulates files and sends them when the budget is reached. A file
// is recorded as pushed only after the request carrying it succeeded, so a
// failed request leaves it to the next pass.
type batch struct {
	p     *Pusher
	st    *Stats
	state *pushState

	groups  []*logspb.ResourceLogs
	byKey   map[string]int
	bytes   int64
	pending []pendingFile

	// stop is set when the receiver asked to slow down: nothing more is
	// sent this pass.
	stop *Throttled

	// services is the service each session's records are attributed to.
	services map[string]string

	// latest is the latest record time of each session, read once per pass
	// when a file without timed records needs a timestamp.
	latest map[string]uint64
}

type pendingFile struct{ rel, digest string }

// group is the resource a session's records go under: one per service,
// since everything else on the resource is the same for the whole pass.
func (b *batch) group(session string) *logspb.ResourceLogs {
	service := b.services[session]
	if service == "" {
		service = b.p.ServiceName
	}
	if b.byKey == nil {
		b.byKey = map[string]int{}
	}
	if i, ok := b.byKey[service]; ok {
		return b.groups[i]
	}
	g := &logspb.ResourceLogs{
		Resource: &resourcepb.Resource{Attributes: b.resource(service)},
		ScopeLogs: []*logspb.ScopeLogs{{
			Scope: &commonpb.InstrumentationScope{Name: ScopeName, Version: b.p.Version},
		}},
	}
	b.groups = append(b.groups, g)
	b.byKey[service] = len(b.groups) - 1
	return g
}

func (b *batch) add(session string, r *logspb.LogRecord, rel, digest string) error {
	size := int64(len(r.GetBody().GetStringValue()))
	// A file that does not fit next to what is already batched goes after
	// it. A file larger than the whole budget therefore travels alone: the
	// batch before it is sent first, and the file after it starts a new one.
	if b.bytes > 0 && b.bytes+size > b.p.BatchBytes {
		if err := b.flush(); err != nil {
			return err
		}
	}
	g := b.group(session)
	g.ScopeLogs[0].LogRecords = append(g.ScopeLogs[0].LogRecords, r)
	b.bytes += size
	b.st.Bytes += size
	b.pending = append(b.pending, pendingFile{rel, digest})
	return nil
}

func (b *batch) flush() error {
	if len(b.groups) == 0 {
		return nil
	}
	req := &collogspb.ExportLogsServiceRequest{ResourceLogs: b.groups}
	size := int64(proto.Size(req))
	b.st.Paused += b.p.limit.take(size)
	rejected, err := b.p.Client.Export(req)
	b.st.Requests++
	b.st.Wire += size
	b.st.Rejected += rejected
	pending := b.pending
	b.groups, b.byKey, b.bytes, b.pending = nil, nil, 0, nil
	if err != nil {
		var t *Throttled
		if errors.As(err, &t) {
			b.stop = t
			b.p.limit.drain()
		}
		return err
	}
	for _, f := range pending {
		b.state.mark(f.rel, f.digest, b.p.Endpoint)
		// The request was taken, so it is not sent again. The answer does not
		// say which records were rejected, so every file in it is recorded.
		if rejected > 0 {
			b.state.reject(f.rel)
		}
	}
	b.st.Files += len(pending)
	return b.state.save(b.p.statePath(), b.p.Now())
}

// serviceOf is the service a session's records are attributed to: the
// configured name, or the runtime named by the adapter on the session's
// first landed header.
func (p *Pusher) serviceOf(files []storage.LandedFile) string {
	if p.ServiceName != "" {
		return p.ServiceName
	}
	for _, lf := range files {
		f, err := os.Open(lf.Path)
		if err != nil {
			continue
		}
		line, err := readLine(bufio.NewReaderSize(f, 1<<20))
		f.Close()
		if err != nil {
			continue
		}
		var hdr struct {
			Adapter string `json:"adapter"`
		}
		if json.Unmarshal(line, &hdr) != nil || hdr.Adapter == "" {
			continue
		}
		name, _, _ := strings.Cut(hdr.Adapter, "/")
		if runtime, ok := p.Runtimes[name]; ok {
			return runtime
		}
		return name
	}
	return "unknown"
}

// resource names the sender and the service a session's records belong to.
func (b *batch) resource(service string) []*commonpb.KeyValue {
	attrs := []*commonpb.KeyValue{
		str("service.name", service),
		str("service.instance.id", b.p.InstanceID),
		str("telemetry.sdk.name", "asz"),
		str("telemetry.sdk.version", b.p.Version),
		str("telemetry.sdk.language", "go"),
	}
	if b.p.Layer != "" {
		attrs = append(attrs, str("service.layer", b.p.Layer))
	}
	return attrs
}

// The attribute kinds the exporter sends: strings and integers.

func str(key, val string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: val}}}
}

func integer(key string, val int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: val}}}
}

// record is one file as one log record: the file's bytes as the body, at
// severity INFO, stamped as the caller decided.
func record(stamp, observed uint64, body []byte, attrs []*commonpb.KeyValue) *logspb.LogRecord {
	return &logspb.LogRecord{
		TimeUnixNano:         stamp,
		ObservedTimeUnixNano: observed,
		SeverityNumber:       logspb.SeverityNumber_SEVERITY_NUMBER_INFO,
		SeverityText:         "INFO",
		Body:                 &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: string(body)}},
		Attributes:           attrs,
	}
}

// addLanded sends one landed file as one record.
func (b *batch) addLanded(rel string, lf storage.LandedFile, session string, files []storage.LandedFile) error {
	data, err := os.ReadFile(lf.Path)
	if err != nil {
		return err
	}
	headerLine, _, _ := bytes.Cut(data, []byte("\n"))
	var hdr sessiondata.Header
	if err := json.Unmarshal(headerLine, &hdr); err != nil {
		return fmt.Errorf("decode header: %w", err)
	}
	digest := digestOf(data)
	// The attributes say what the file is without decoding it: a receiver
	// routes, indexes and verifies on them, and reads the body only to serve
	// it. Session and sequence are what a round's {seq, row} reference names,
	// and a row is a line of this body.
	attrs := []*commonpb.KeyValue{
		str("asz.format", "sd"),
		str("asz.format.version", hdr.Schema),
		str("asz.file", rel),
		str("asz.file.kind", string(hdr.Kind)),
		str("asz.file.digest", digest),
		integer("asz.lines", int64(bytes.Count(data, []byte("\n")))),
		str("asz.session", session),
		integer("asz.seq", int64(lf.Seq)),
	}
	if lf.Stream != "" {
		attrs = append(attrs, str("asz.stream", lf.Stream))
	}
	if lf.RunID != "" {
		attrs = append(attrs, str("asz.run", lf.RunID))
	}
	// The record time range of the file lets a receiver place it in time
	// without decoding the body. A file whose records carry no time, such
	// as a child's meta file, carries neither attribute.
	//
	// The record's own time becomes the row's timestamp in a receiver, and
	// a receiver reads a session's files by a time range it takes from the
	// head round, so the stamp must fall inside the session's range: the
	// file's own last record time, or, for a file without one, the latest
	// record time of the session as known now. That point is always inside
	// the range and, unlike a range, cannot go stale as the session grows.
	stamp := b.sessionLatest(session, files)
	if from, through, hi, ok := timeRange(data); ok {
		attrs = append(attrs, str("asz.from_time", from), str("asz.through_time", through))
		stamp = uint64(hi)
	}
	if stamp == 0 {
		stamp = parseTime(hdr.At)
	}
	now := uint64(b.p.Now().UnixNano())
	return b.add(session, record(stamp, now, data, attrs), rel, digest)
}

// addRound sends one round file as one record. A round carries no time of
// its own, so the record is stamped with the time it was sent.
func (b *batch) addRound(rel, path, conv string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	headerLine, _, _ := bytes.Cut(data, []byte("\n"))
	var hdr struct {
		Schema       string `json:"schema"`
		Conversation string `json:"conversation"`
		Session      string `json:"session"`
		Round        int64  `json:"round"`
		FromTime     string `json:"from_time"`
		ThroughTime  string `json:"through_time"`
		SessionFrom  string `json:"session_from_time"`
		SessionThru  string `json:"session_through_time"`
		Title        string `json:"title"`
		Talks        *int64 `json:"talks"`
		Steps        *int64 `json:"steps"`
		Streams      *int64 `json:"streams"`
		Segments     *int64 `json:"segments"`
		Unresolved   *int64 `json:"unresolved"`
		Changes      *int64 `json:"changes"`
		LinesAdded   *int64 `json:"lines_added"`
		LinesRemoved *int64 `json:"lines_removed"`
		LLMCalls     *int64 `json:"llm_calls"`
		Subagents    *int64 `json:"subagents"`
		BashRuns     *int64 `json:"bash_runs"`
	}
	if err := json.Unmarshal(headerLine, &hdr); err != nil {
		return fmt.Errorf("decode round header: %w", err)
	}
	// A round is written straight to its final name, not to a temporary one,
	// so the file is there to be read before all of its bytes are. The last
	// frame a finished round carries is its commit, and the round's digest
	// is in it; a file without one is still being written. Send the short
	// read and it is recorded as sent, and the finished bytes never go.
	if !roundIsFinished(data, path) {
		return errRoundUnfinished
	}
	digest := digestOf(data)
	session := hdr.Session
	if session == "" {
		session = conv
	}
	attrs := []*commonpb.KeyValue{
		str("asz.format", "sf"),
		str("asz.format.version", hdr.Schema),
		str("asz.file", rel),
		str("asz.file.kind", "round"),
		str("asz.file.digest", digest),
		integer("asz.lines", int64(bytes.Count(data, []byte("\n")))),
		str("asz.session", session),
		str("asz.conversation", hdr.Conversation),
		integer("asz.round", hdr.Round),
	}
	// A round's header carries the record time range of the files it
	// consumed; it travels as the same pair.
	if hdr.FromTime != "" && hdr.ThroughTime != "" {
		attrs = append(attrs, str("asz.from_time", hdr.FromTime), str("asz.through_time", hdr.ThroughTime))
	}
	// The session's own range as of this round: when it began, and its last
	// activity so far. Only a round carries it. A landed file can travel
	// before any round exists and the last activity keeps moving, so on a
	// landed file the value would be missing or stale.
	if hdr.SessionFrom != "" && hdr.SessionThru != "" {
		attrs = append(attrs, str("asz.session.from_time", hdr.SessionFrom), str("asz.session.through_time", hdr.SessionThru))
	}
	// What a list of conversations shows, as of this round, copied off the
	// header so a receiver lists conversations without decoding a body.
	if hdr.Talks != nil {
		if hdr.Title != "" {
			attrs = append(attrs, str("asz.conversation.title", hdr.Title))
		}
		attrs = append(attrs,
			integer("asz.conversation.talks", *hdr.Talks),
			integer("asz.conversation.steps", *hdr.Steps),
			integer("asz.conversation.streams", *hdr.Streams),
			integer("asz.conversation.segments", *hdr.Segments),
			integer("asz.conversation.unresolved", *hdr.Unresolved))
		// These counts came later than the ones above, so a round may carry
		// those and not these; each travels on its own, so one that is
		// missing never holds the others back.
		for _, c := range []struct {
			key string
			v   *int64
		}{
			{"asz.conversation.changes", hdr.Changes},
			{"asz.conversation.lines_added", hdr.LinesAdded},
			{"asz.conversation.lines_removed", hdr.LinesRemoved},
			{"asz.conversation.llm_calls", hdr.LLMCalls},
			{"asz.conversation.subagents", hdr.Subagents},
			{"asz.conversation.bash_runs", hdr.BashRuns},
		} {
			if c.v != nil {
				attrs = append(attrs, integer(c.key, *c.v))
			}
		}
	}
	// A round is stamped with the session's last activity as of the round,
	// which only widens, so a receiver's newest row per conversation is the
	// head. A round from before that field existed is stamped with now.
	now := uint64(b.p.Now().UnixNano())
	stamp := parseTime(hdr.SessionThru)
	if stamp == 0 {
		stamp = now
	}
	if _, known := b.services[session]; !known {
		files, err := storage.LandedFiles(b.p.Zone, session)
		if err == nil {
			b.services[session] = b.p.serviceOf(files)
		}
	}
	return b.add(session, record(stamp, now, data, attrs), rel, digest)
}

// sessionLatest is the latest record time among a session's landed files,
// read once per pass and only when a file without timed records needs a
// timestamp.
func (b *batch) sessionLatest(session string, files []storage.LandedFile) uint64 {
	if b.latest == nil {
		b.latest = map[string]uint64{}
	}
	if v, ok := b.latest[session]; ok {
		return v
	}
	var hi int64
	for _, lf := range files {
		data, err := os.ReadFile(lf.Path)
		if err != nil {
			continue
		}
		if _, _, h, ok := timeRange(data); ok && h > hi {
			hi = h
		}
	}
	b.latest[session] = uint64(hi)
	return uint64(hi)
}

// timeRange is the earliest and the latest record time in a landed file,
// written the way a round header writes them, and the latest as nanoseconds.
func timeRange(data []byte) (from, through string, hiNS int64, ok bool) {
	var lo, hi int64
	for len(data) > 0 {
		line := data
		if k := bytes.IndexByte(data, '\n'); k >= 0 {
			line, data = data[:k], data[k+1:]
		} else {
			data = nil
		}
		ns, has := sessiondata.LineTime(line)
		if !has {
			continue
		}
		if !ok || ns < lo {
			lo = ns
		}
		if !ok || ns > hi {
			hi = ns
		}
		ok = true
	}
	if !ok {
		return "", "", 0, false
	}
	return sessiondata.FormatTime(lo), sessiondata.FormatTime(hi), hi, true
}

// digestOf is the file digest a receiver checks: SHA-256 over the bytes as
// landed, the same value storage.FileDigest reads from disk.
func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// defaultInstance names the sender by the person and the machine, user@host,
// so the same machine is the same instance on every push. A machine that
// can name neither gets a random UUID, which at least stays unique.
func defaultInstance() (string, error) {
	host, herr := os.Hostname()
	u, uerr := user.Current()
	if herr != nil || uerr != nil || host == "" || u.Username == "" {
		return newUUID()
	}
	name := u.Username
	if i := strings.LastIndexAny(name, `\/`); i >= 0 {
		name = name[i+1:] // a Windows account carries its domain in front
	}
	return name + "@" + host, nil
}

// newUUID returns a random version 4 UUID.
func newUUID() (string, error) {
	var u [16]byte
	if _, err := rand.Read(u[:]); err != nil {
		return "", err
	}
	u[6] = (u[6] & 0x0f) | 0x40
	u[8] = (u[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16]), nil
}

func parseTime(s string) uint64 {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0
	}
	return uint64(t.UnixNano())
}

// readLine reads one line without its newline. Lines reach a megabyte and
// more, past bufio.Scanner's default, so this reads until the newline.
func readLine(br *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		part, err := br.ReadSlice('\n')
		out = append(out, part...)
		if err == nil {
			return out[:len(out)-1], nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if len(out) == 0 {
				return nil, io.EOF
			}
			return out, nil
		}
		return nil, err
	}
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

func conversationDirs(root string) ([]string, error) {
	items, err := os.ReadDir(filepath.Join(root, "_conversations"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, d := range items {
		if d.IsDir() {
			out = append(out, d.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// errRoundUnfinished says a round file was read before it was completely
// written. It is not a failure: the next pass reads it whole.
var errRoundUnfinished = errors.New("round is still being written")

// roundIsFinished reports whether a round file holds every byte it will.
//
// The test is its last frame: a finished round ends with a commit carrying
// the digest of everything before it, and that digest's first twelve hex
// digits are the ones in the file's name. A file being written has no
// commit line yet, or only part of one.
func roundIsFinished(data []byte, path string) bool {
	m := roundNameRe.FindStringSubmatch(filepath.Base(path))
	if m == nil {
		// Not a name this can check. Leave it to the reader.
		return true
	}
	trimmed := bytes.TrimRight(data, "\n")
	i := bytes.LastIndexByte(trimmed, '\n')
	var commit struct {
		T      string `json:"t"`
		Digest string `json:"digest"`
	}
	if err := json.Unmarshal(trimmed[i+1:], &commit); err != nil {
		return false
	}
	// "commit" is Session Flow's frame name. It is written out rather than
	// imported: the push is on the collector side, and the two sides meet
	// only at the storage root, which tests/boundary enforces.
	return commit.T == "commit" && strings.HasPrefix(commit.Digest, m[2])
}

var roundNameRe = regexp.MustCompile(`^r(\d{6,})-([0-9a-f]{12})\.sf$`)

func roundFiles(root, conv string) ([]string, error) {
	dir := filepath.Join(root, "_conversations", conv, "rounds")
	items, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, it := range items {
		if !it.IsDir() && strings.HasSuffix(it.Name(), ".sf") {
			out = append(out, filepath.Join(dir, it.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

// StateFile is the file in a storage root that records what was sent.
const StateFile = "push.state"

// pushState is what push.state records: the files already sent, with the
// digest each had, the receivers they went to, and the files of a request a
// receiver took while it rejected some of the request's records.
//
// Every save writes all three. An older asz reads only the pushed lines, and
// rewrites the file without the other two. So a file with pushed lines and
// no endpoint line names the receiver unknown, and nothing that asks where
// the files went trusts it.
type pushState struct {
	files     map[string]string
	endpoints map[string]bool
	rejected  map[string]bool
}

// unknownEndpoint is the receiver of files that push.state records with no
// endpoint: a file written before endpoints were recorded, or rewritten by an
// older asz.
const unknownEndpoint = "unknown"

func newPushState() *pushState {
	return &pushState{files: map[string]string{}, endpoints: map[string]bool{}, rejected: map[string]bool{}}
}

func loadState(path string) (*pushState, error) {
	s := newPushState()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	defer f.Close()
	br := bufio.NewReader(f)
	for {
		line, err := readLine(br)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		// The two newer line kinds have two fields. An older asz keeps only
		// three-field pushed lines, so it passes over them.
		fields := strings.Fields(string(line))
		switch {
		case len(fields) == 3 && fields[0] == "pushed":
			s.files[fields[1]] = fields[2]
		case len(fields) == 2 && fields[0] == "endpoint":
			s.endpoints[fields[1]] = true
		case len(fields) == 2 && fields[0] == "rejected":
			s.rejected[fields[1]] = true
		}
	}
	if len(s.files) > 0 && len(s.endpoints) == 0 {
		s.endpoints[unknownEndpoint] = true
	}
	return s, nil
}

func (s *pushState) pushed(rel string) bool { _, ok := s.files[rel]; return ok }

// mark records rel as sent with its digest, to endpoint when one is named.
func (s *pushState) mark(rel, digest, endpoint string) {
	s.files[rel] = digest
	if endpoint != "" {
		if s.endpoints == nil {
			s.endpoints = map[string]bool{}
		}
		s.endpoints[endpoint] = true
	}
}

// reject records that a receiver rejected records of rel.
func (s *pushState) reject(rel string) {
	if s.rejected == nil {
		s.rejected = map[string]bool{}
	}
	s.rejected[rel] = true
}

// save writes every line kind, every time. flush and pushSpool save after
// each request, so a kind left out here would be gone after the next request,
// and a file a receiver rejected records of would then read as taken whole.
func (s *pushState) save(path string, now time.Time) error {
	return storage.WriteAtomic(path, storage.PermState, func(w io.Writer) error {
		bw := bufio.NewWriter(w)
		fmt.Fprintf(bw, "schema 1\nupdated_at %s\n", now.UTC().Format(time.RFC3339Nano))
		for _, e := range sortedKeys(s.endpoints) {
			fmt.Fprintf(bw, "endpoint %s\n", e)
		}
		for _, k := range sortedKeys(s.files) {
			fmt.Fprintf(bw, "pushed %s %s\n", k, s.files[k])
		}
		for _, k := range sortedKeys(s.rejected) {
			fmt.Fprintf(bw, "rejected %s\n", k)
		}
		return bw.Flush()
	})
}
