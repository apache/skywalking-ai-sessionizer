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
	"crypto/rand"
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
// A file is sent as one log record per line, header and closing line
// included, so a receiver can write the file back byte for byte and every
// digest still holds. Landed files and rounds are both write-once, so a file
// is sent once; push.state in the root records which ones were.
type Pusher struct {
	Zone   *storage.Zone
	Client *Client
	// Version is what the sender reports about itself.
	Version string
	// ServiceName is the service every record is attributed to. Empty means
	// the project directory the session was recorded under, one service per
	// project.
	ServiceName string
	// InstanceID identifies this sender as service.instance.id. Empty means a
	// new UUID, made once per Pusher.
	InstanceID string
	// Layer is the receiver's layer for the service, sent as service.layer.
	Layer string
	// BatchBytes is how much body text one request carries at most.
	BatchBytes int64
	Now        func() time.Time
}

// Stats reports what one pass did.
type Stats struct {
	Files    int
	Records  int
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
	if p.BatchBytes <= 0 {
		p.BatchBytes = 1 << 20
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
	projects := map[string]string{}
	for _, session := range sessions {
		files, err := storage.LandedFiles(p.Zone, session)
		if err != nil {
			st.Errors = append(st.Errors, err)
			continue
		}
		// A session belongs to the project its main transcript was recorded
		// under. A child agent can run in another directory, and its files
		// must still be attributed to the session's project, not their own.
		projects[session] = sessionProject(files)
		for _, lf := range files {
			rel, _ := filepath.Rel(p.Zone.Root(), lf.Path)
			rel = filepath.ToSlash(rel)
			if state.pushed(rel) {
				continue
			}
			if err := b.addLanded(rel, lf, session, projects); err != nil {
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
			if err := b.addRound(rel, path, conv, projects); err != nil {
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

// batch accumulates records across files and sends them when the body budget
// is reached. A file is recorded as pushed only after the request carrying
// its last line succeeded, so a failed request leaves it to the next pass.
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

func (b *batch) add(resource []Attr, r Record) error {
	if b.bytes > 0 && b.bytes+int64(len(r.Body)) > b.p.BatchBytes {
		if err := b.flush(); err != nil {
			return err
		}
	}
	g := b.group(resource)
	g.Records = append(g.Records, r)
	b.bytes += int64(len(r.Body))
	b.st.Records++
	b.st.Bytes += int64(len(r.Body))
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

// resource names the service a file's records belong to.
func (b *batch) resource(project string) []Attr {
	service := b.p.ServiceName
	if service == "" {
		service = project
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

// addLanded sends one landed file, line by line.
func (b *batch) addLanded(rel string, lf storage.LandedFile, session string, projects map[string]string) error {
	f, err := os.Open(lf.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	digest, err := storage.FileDigest(lf.Path)
	if err != nil {
		return err
	}
	br := bufio.NewReaderSize(f, 1<<20)
	headerLine, err := readLine(br)
	if err != nil {
		return err
	}
	var hdr sessiondata.Header
	if err := json.Unmarshal(headerLine, &hdr); err != nil {
		return fmt.Errorf("decode header: %w", err)
	}
	if projects[session] == "" {
		projects[session] = projectOf(hdr.Src)
	}
	res := b.resource(projects[session])
	fileAt := parseTime(hdr.At)
	now := uint64(b.p.Now().UnixNano())
	// Every line carries what a receiver needs to place it and to resolve a
	// round's reference to it: the file, the line, the session and the
	// sequence. What is constant for the file rides on the header line only,
	// and the digest on the header and the closing line, where a receiver
	// checks what it wrote back. Repeating them on every line measured 28% on
	// top of the body; this keeps it near 15%.
	common := []Attr{
		{Key: "asz.format", Str: "sd"},
		{Key: "asz.file", Str: rel},
		{Key: "asz.session", Str: session},
		{Key: "asz.seq", Int: int64(lf.Seq), IsInt: true},
	}
	once := []Attr{
		{Key: "asz.format.version", Str: hdr.Schema},
		{Key: "asz.file.kind", Str: string(hdr.Kind)},
		{Key: "asz.file.digest", Str: digest},
	}
	if lf.Stream != "" {
		once = append(once, Attr{Key: "asz.stream", Str: lf.Stream})
	}
	if lf.RunID != "" {
		once = append(once, Attr{Key: "asz.run", Str: lf.RunID})
	}
	line := int64(0)
	emit := func(kind string, text []byte, at uint64) error {
		attrs := append(append([]Attr{}, common...),
			Attr{Key: "asz.line", Int: line, IsInt: true},
			Attr{Key: "asz.line.kind", Str: kind})
		switch kind {
		case "header":
			attrs = append(attrs, once...)
		case "end":
			attrs = append(attrs, Attr{Key: "asz.file.digest", Str: digest})
		}
		line++
		return b.add(res, Record{TimeNano: at, ObservedNano: now, Severity: 9, SeverityText: "INFO", Body: string(text), Attrs: attrs})
	}
	if err := emit("header", headerLine, fileAt); err != nil {
		return err
	}
	for {
		l, err := readLine(br)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		kind, at := "record", fileAt
		if len(l) > 8 && string(l[:9]) == `{"t":"end` {
			kind = "end"
		} else if ns, ok := sessiondata.LineTime(l); ok {
			at = uint64(ns)
		}
		if err := emit(kind, l, at); err != nil {
			return err
		}
	}
	b.pending = append(b.pending, pendingFile{rel, digest})
	return nil
}

// addRound sends one round file, line by line. Rounds carry no time of their
// own, so every line is stamped with the time it was observed.
func (b *batch) addRound(rel, path, conv string, projects map[string]string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	digest, err := storage.FileDigest(path)
	if err != nil {
		return err
	}
	br := bufio.NewReaderSize(f, 1<<20)
	headerLine, err := readLine(br)
	if err != nil {
		return err
	}
	var hdr struct {
		Schema       string `json:"schema"`
		Conversation string `json:"conversation"`
		Session      string `json:"session"`
		Round        int64  `json:"round"`
	}
	if err := json.Unmarshal(headerLine, &hdr); err != nil {
		return fmt.Errorf("decode round header: %w", err)
	}
	session := hdr.Session
	if session == "" {
		session = conv
	}
	res := b.resource(projects[session])
	now := uint64(b.p.Now().UnixNano())
	common := []Attr{
		{Key: "asz.format", Str: "sf"},
		{Key: "asz.file", Str: rel},
		{Key: "asz.conversation", Str: hdr.Conversation},
		{Key: "asz.round", Int: hdr.Round, IsInt: true},
	}
	once := []Attr{
		{Key: "asz.format.version", Str: hdr.Schema},
		{Key: "asz.file.kind", Str: "round"},
		{Key: "asz.file.digest", Str: digest},
		{Key: "asz.session", Str: session},
	}
	line := int64(0)
	emit := func(text []byte) error {
		var frame struct {
			T string `json:"t"`
		}
		_ = json.Unmarshal(text, &frame)
		attrs := append(append([]Attr{}, common...),
			Attr{Key: "asz.line", Int: line, IsInt: true},
			Attr{Key: "asz.line.kind", Str: frame.T})
		switch frame.T {
		case "header":
			attrs = append(attrs, once...)
		case "commit":
			attrs = append(attrs, Attr{Key: "asz.file.digest", Str: digest})
		}
		line++
		return b.add(res, Record{TimeNano: now, ObservedNano: now, Severity: 9, SeverityText: "INFO", Body: string(text), Attrs: attrs})
	}
	if err := emit(headerLine); err != nil {
		return err
	}
	for {
		l, err := readLine(br)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if err := emit(l); err != nil {
			return err
		}
	}
	b.pending = append(b.pending, pendingFile{rel, digest})
	return nil
}

// sessionProject reads the project directory from the header of the session's
// first main-stream file, or of its first file when the main transcript is
// gone, which happens when the runtime has pruned it.
func sessionProject(files []storage.LandedFile) string {
	pick := func(want string) string {
		for _, lf := range files {
			if want != "" && lf.Stream != want {
				continue
			}
			f, err := os.Open(lf.Path)
			if err != nil {
				continue
			}
			line, err := readLine(bufio.NewReaderSize(f, 1<<20))
			f.Close()
			if err != nil {
				continue
			}
			var hdr sessiondata.Header
			if json.Unmarshal(line, &hdr) == nil && hdr.Src != "" {
				return projectOf(hdr.Src)
			}
		}
		return ""
	}
	if p := pick(storage.StreamMain); p != "" {
		return p
	}
	return pick("")
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

// projectOf takes the project directory from a landed header's source path,
// which is relative to the adapter's root and starts with that directory.
func projectOf(src string) string {
	src = strings.TrimLeft(src, "/")
	if i := strings.IndexByte(src, '/'); i > 0 {
		return src[:i]
	}
	return src
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
