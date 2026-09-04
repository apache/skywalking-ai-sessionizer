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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

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
	Zone   *storage.Zone
	Client *Client
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
	// InstanceID identifies this sender as service.instance.id. Empty means a
	// new UUID, made once per Pusher.
	InstanceID string
	// Layer is the receiver's layer for the service, sent as service.layer.
	Layer string
	// BatchBytes is how many file bytes one request carries at most. A file
	// larger than the budget is sent alone, in a request of its own.
	BatchBytes int64
	Now        func() time.Time
}

// Stats reports what one pass did.
type Stats struct {
	Files    int
	Bytes    int64
	Requests int
	Errors   []error
}

// ScopeName identifies the sender in every request.
const ScopeName = "github.com/apache/skywalking-ai-sessionizer"

// Prepare settles the defaults: the batch budget, the clock, and the
// instance id, which is a new UUID when none was configured. Pass calls it,
// and a caller may call it first to learn the instance id.
func (p *Pusher) Prepare() error {
	if p.Client == nil || p.Client.Endpoint == "" {
		return errors.New("otlp: no endpoint")
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
	if p.InstanceID == "" {
		id, err := newUUID()
		if err != nil {
			return err
		}
		p.InstanceID = id
	}
	return nil
}

// Pass sends what is not yet sent, in landed order: each session's landed
// files by sequence, then each conversation's rounds by number.
func (p *Pusher) Pass() (*Stats, error) {
	if err := p.Prepare(); err != nil {
		return nil, err
	}
	st := &Stats{}
	state, err := loadState(p.statePath())
	if err != nil {
		return nil, err
	}
	sessions, err := sessionDirs(p.Zone.Root())
	if err != nil {
		return nil, err
	}
	b := &batch{p: p, st: st, state: state, services: map[string]string{}}
	for _, session := range sessions {
		files, err := storage.LandedFiles(p.Zone, session)
		if err != nil {
			st.Errors = append(st.Errors, err)
			continue
		}
		// A session's records are attributed to the runtime that produced
		// them, which its landed headers name. The rounds of its
		// conversation follow the session.
		b.services[session] = p.serviceOf(files)
		for _, lf := range files {
			rel, _ := filepath.Rel(p.Zone.Root(), lf.Path)
			rel = filepath.ToSlash(rel)
			if state.pushed(rel) {
				continue
			}
			if err := b.addLanded(rel, lf, session, files); err != nil {
				st.Errors = append(st.Errors, fmt.Errorf("%s: %w", rel, err))
			}
		}
	}
	convs, err := conversationDirs(p.Zone.Root())
	if err != nil {
		st.Errors = append(st.Errors, err)
	}
	for _, conv := range convs {
		rounds, err := roundFiles(p.Zone.Root(), conv)
		if err != nil {
			st.Errors = append(st.Errors, err)
			continue
		}
		for _, path := range rounds {
			rel, _ := filepath.Rel(p.Zone.Root(), path)
			rel = filepath.ToSlash(rel)
			if state.pushed(rel) {
				continue
			}
			if err := b.addRound(rel, path, conv); err != nil {
				st.Errors = append(st.Errors, fmt.Errorf("%s: %w", rel, err))
			}
		}
	}
	if err := b.flush(); err != nil {
		st.Errors = append(st.Errors, err)
	}
	return st, nil
}

func (p *Pusher) statePath() string { return filepath.Join(p.Zone.Root(), "push.state") }

// batch accumulates files and sends them when the budget is reached. A file
// is recorded as pushed only after the request carrying it succeeded, so a
// failed request leaves it to the next pass.
type batch struct {
	p     *Pusher
	st    *Stats
	state *pushState

	groups  []ResourceLogs
	byKey   map[string]int
	bytes   int64
	pending []pendingFile

	// services is the service each session's records are attributed to.
	services map[string]string

	// latest is the latest record time of each session, read once per pass
	// when a file without timed records needs a timestamp.
	latest map[string]uint64
}

type pendingFile struct{ rel, digest string }

func (b *batch) group(resource []Attr) *ResourceLogs {
	key := fmt.Sprint(resource)
	if b.byKey == nil {
		b.byKey = map[string]int{}
	}
	if i, ok := b.byKey[key]; ok {
		return &b.groups[i]
	}
	b.groups = append(b.groups, ResourceLogs{Resource: resource, ScopeName: ScopeName, ScopeVersion: b.p.Version})
	b.byKey[key] = len(b.groups) - 1
	return &b.groups[len(b.groups)-1]
}

func (b *batch) add(resource []Attr, r Record, rel, digest string) error {
	// A file that does not fit next to what is already batched goes after
	// it. A file larger than the whole budget therefore travels alone: the
	// batch before it is sent first, and the file after it starts a new one.
	if b.bytes > 0 && b.bytes+int64(len(r.Body)) > b.p.BatchBytes {
		if err := b.flush(); err != nil {
			return err
		}
	}
	g := b.group(resource)
	g.Records = append(g.Records, r)
	b.bytes += int64(len(r.Body))
	b.st.Bytes += int64(len(r.Body))
	b.pending = append(b.pending, pendingFile{rel, digest})
	return nil
}

func (b *batch) flush() error {
	if len(b.groups) == 0 {
		return nil
	}
	err := b.p.Client.Export(Encode(b.groups))
	b.st.Requests++
	groups, pending := b.groups, b.pending
	b.groups, b.byKey, b.bytes, b.pending = nil, nil, 0, nil
	if err != nil {
		return err
	}
	_ = groups
	for _, f := range pending {
		b.state.mark(f.rel, f.digest)
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
func (b *batch) resource(session string) []Attr {
	service := b.services[session]
	if service == "" {
		service = b.p.ServiceName
	}
	attrs := []Attr{
		{Key: "service.name", Str: service},
		{Key: "service.instance.id", Str: b.p.InstanceID},
		{Key: "telemetry.sdk.name", Str: "asz"},
		{Key: "telemetry.sdk.version", Str: b.p.Version},
		{Key: "telemetry.sdk.language", Str: "go"},
	}
	if b.p.Layer != "" {
		attrs = append(attrs, Attr{Key: "service.layer", Str: b.p.Layer})
	}
	return attrs
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
	attrs := []Attr{
		{Key: "asz.format", Str: "sd"},
		{Key: "asz.format.version", Str: hdr.Schema},
		{Key: "asz.file", Str: rel},
		{Key: "asz.file.kind", Str: string(hdr.Kind)},
		{Key: "asz.file.digest", Str: digest},
		{Key: "asz.lines", Int: int64(bytes.Count(data, []byte("\n"))), IsInt: true},
		{Key: "asz.session", Str: session},
		{Key: "asz.seq", Int: int64(lf.Seq), IsInt: true},
	}
	if lf.Stream != "" {
		attrs = append(attrs, Attr{Key: "asz.stream", Str: lf.Stream})
	}
	if lf.RunID != "" {
		attrs = append(attrs, Attr{Key: "asz.run", Str: lf.RunID})
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
		attrs = append(attrs, Attr{Key: "asz.from_time", Str: from}, Attr{Key: "asz.through_time", Str: through})
		stamp = uint64(hi)
	}
	if stamp == 0 {
		stamp = parseTime(hdr.At)
	}
	now := uint64(b.p.Now().UnixNano())
	rec := Record{TimeNano: stamp, ObservedNano: now, Severity: 9, SeverityText: "INFO", Body: string(data), Attrs: attrs}
	return b.add(b.resource(session), rec, rel, digest)
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
	}
	if err := json.Unmarshal(headerLine, &hdr); err != nil {
		return fmt.Errorf("decode round header: %w", err)
	}
	session := hdr.Session
	if session == "" {
		session = conv
	}
	digest := digestOf(data)
	attrs := []Attr{
		{Key: "asz.format", Str: "sf"},
		{Key: "asz.format.version", Str: hdr.Schema},
		{Key: "asz.file", Str: rel},
		{Key: "asz.file.kind", Str: "round"},
		{Key: "asz.file.digest", Str: digest},
		{Key: "asz.lines", Int: int64(bytes.Count(data, []byte("\n"))), IsInt: true},
		{Key: "asz.session", Str: session},
		{Key: "asz.conversation", Str: hdr.Conversation},
		{Key: "asz.round", Int: hdr.Round, IsInt: true},
	}
	// A round's header carries the record time range of the files it
	// consumed; it travels as the same pair.
	if hdr.FromTime != "" && hdr.ThroughTime != "" {
		attrs = append(attrs, Attr{Key: "asz.from_time", Str: hdr.FromTime}, Attr{Key: "asz.through_time", Str: hdr.ThroughTime})
	}
	// The session's own range as of this round: when it began, and its last
	// activity so far. Only a round carries it. A landed file can travel
	// before any round exists and the last activity keeps moving, so on a
	// landed file the value would be missing or stale.
	if hdr.SessionFrom != "" && hdr.SessionThru != "" {
		attrs = append(attrs, Attr{Key: "asz.session.from_time", Str: hdr.SessionFrom}, Attr{Key: "asz.session.through_time", Str: hdr.SessionThru})
	}
	// What a list of conversations shows, as of this round, copied off the
	// header so a receiver lists conversations without decoding a body.
	if hdr.Talks != nil {
		if hdr.Title != "" {
			attrs = append(attrs, Attr{Key: "asz.conversation.title", Str: hdr.Title})
		}
		attrs = append(attrs,
			Attr{Key: "asz.conversation.talks", Int: *hdr.Talks, IsInt: true},
			Attr{Key: "asz.conversation.steps", Int: *hdr.Steps, IsInt: true},
			Attr{Key: "asz.conversation.streams", Int: *hdr.Streams, IsInt: true},
			Attr{Key: "asz.conversation.segments", Int: *hdr.Segments, IsInt: true},
			Attr{Key: "asz.conversation.unresolved", Int: *hdr.Unresolved, IsInt: true})
	}
	// A round is stamped with the session's last activity as of the round,
	// which only widens, so a receiver's newest row per conversation is the
	// head. A round from before that field existed is stamped with now.
	now := uint64(b.p.Now().UnixNano())
	stamp := parseTime(hdr.SessionThru)
	if stamp == 0 {
		stamp = now
	}
	rec := Record{TimeNano: stamp, ObservedNano: now, Severity: 9, SeverityText: "INFO", Body: string(data), Attrs: attrs}
	if _, known := b.services[session]; !known {
		files, err := storage.LandedFiles(b.p.Zone, session)
		if err == nil {
			b.services[session] = b.p.serviceOf(files)
		}
	}
	return b.add(b.resource(session), rec, rel, digest)
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

// pushState is the set of files already sent, with the digest each had.
type pushState struct {
	files map[string]string
}

func loadState(path string) (*pushState, error) {
	s := &pushState{files: map[string]string{}}
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
		fields := strings.Fields(string(line))
		if len(fields) == 3 && fields[0] == "pushed" {
			s.files[fields[1]] = fields[2]
		}
	}
	return s, nil
}

func (s *pushState) pushed(rel string) bool  { _, ok := s.files[rel]; return ok }
func (s *pushState) mark(rel, digest string) { s.files[rel] = digest }

func (s *pushState) save(path string, now time.Time) error {
	keys := make([]string, 0, len(s.files))
	for k := range s.files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return storage.WriteAtomic(path, storage.PermState, func(w io.Writer) error {
		bw := bufio.NewWriter(w)
		fmt.Fprintf(bw, "schema 1\nupdated_at %s\n", now.UTC().Format(time.RFC3339Nano))
		for _, k := range keys {
			fmt.Fprintf(bw, "pushed %s %s\n", k, s.files[k])
		}
		return bw.Flush()
	})
}
