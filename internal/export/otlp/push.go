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
	// ServiceName is the service every record is attributed to. The command
	// supplies the runtime's name, such as Claude Code, when none is
	// configured; the pusher itself refuses to run without one.
	ServiceName string
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
	if p.ServiceName == "" {
		return errors.New("otlp: no service name")
	}
	if p.BatchBytes <= 0 {
		p.BatchBytes = 20 << 20
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
	b := &batch{p: p, st: st, state: state}
	for _, session := range sessions {
		files, err := storage.LandedFiles(p.Zone, session)
		if err != nil {
			st.Errors = append(st.Errors, err)
			continue
		}
		for _, lf := range files {
			rel, _ := filepath.Rel(p.Zone.Root(), lf.Path)
			rel = filepath.ToSlash(rel)
			if state.pushed(rel) {
				continue
			}
			if err := b.addLanded(rel, lf, session); err != nil {
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

// resource names the sender and the service its records belong to.
func (b *batch) resource() []Attr {
	attrs := []Attr{
		{Key: "service.name", Str: b.p.ServiceName},
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
func (b *batch) addLanded(rel string, lf storage.LandedFile, session string) error {
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
	if from, through, ok := timeRange(data); ok {
		attrs = append(attrs, Attr{Key: "asz.from_time", Str: from}, Attr{Key: "asz.through_time", Str: through})
	}
	now := uint64(b.p.Now().UnixNano())
	rec := Record{TimeNano: parseTime(hdr.At), ObservedNano: now, Severity: 9, SeverityText: "INFO", Body: string(data), Attrs: attrs}
	return b.add(b.resource(), rec, rel, digest)
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
	now := uint64(b.p.Now().UnixNano())
	rec := Record{TimeNano: now, ObservedNano: now, Severity: 9, SeverityText: "INFO", Body: string(data), Attrs: attrs}
	return b.add(b.resource(), rec, rel, digest)
}

// timeRange is the earliest and the latest record time in a landed file,
// written the way a round header writes them.
func timeRange(data []byte) (from, through string, ok bool) {
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
		return "", "", false
	}
	return sessiondata.FormatTime(lo), sessiondata.FormatTime(hi), true
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
