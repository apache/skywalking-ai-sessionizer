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

package scenario

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// MarkerDir is the directory, under a build's source directory, that holds
// one marker for each session a claude-code build wrote.
//
// Discovery reads it as a project directory. It holds only .json files,
// which are neither transcripts nor session directories, so nothing in it is
// ever collected.
const MarkerDir = ".asz-scenario"

// The policies a marker names: when a pipeline may remove a session once all
// of it has been sent.
const (
	PolicyImmediately = "immediately"
	PolicyRetain      = "retain"
)

// The states a marker is in.
const (
	// MarkerWritten is how the build leaves it.
	MarkerWritten = "written"
	// MarkerRemoving says a pipeline has started to remove the session. From
	// then on a pipeline finishes the removal without deciding again, and the
	// build refuses to write the session.
	MarkerRemoving = "removing"
)

// markerSchema is the one schema a reader takes.
const markerSchema = 1

// Marker is what a claude-code build records about one session it wrote: the
// policy for its removal, and every file it wrote with the size and digest
// the file had.
//
// The build writes it after every file of the session. So a marker is proof
// that the build finished writing the session, and the files it lists are
// the only ones a removal may delete. A session with no marker, which is every
// real Claude Code session, is never removed.
type Marker struct {
	Schema  int          `json:"schema"`
	Session string       `json:"session"`
	Policy  string       `json:"policy"`
	Retain  string       `json:"retain,omitempty"`
	State   string       `json:"state"`
	Files   []MarkerFile `json:"files"`
}

// MarkerFile is one file the build wrote for a session. Path is relative to
// the build's source directory, with forward slashes.
type MarkerFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Removal is when a pipeline may remove a session once all of it is sent: at
// once when Retain is zero, or once the session's last record is Retain old.
type Removal struct{ Retain time.Duration }

// Immediate reports whether the session goes as soon as it is sent.
func (r Removal) Immediate() bool { return r.Retain <= 0 }

// String is the policy the way a person writes it after --remove.
func (r Removal) String() string {
	if r.Immediate() {
		return PolicyImmediately
	}
	return shortDuration(r.Retain)
}

// Describe says the policy in the words the build prints.
func (r Removal) Describe() string {
	if r.Immediate() {
		return "immediately, once sent"
	}
	return shortDuration(r.Retain) + " after its last record, once sent"
}

// shortDuration drops the zero minutes and seconds Go prints, so 24h0m0s
// reads 24h.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// ParseRemove reads a removal policy: immediately, a Go duration such as 30m
// or 24h, or a whole number of days such as 7d, as metrics_lookback takes
// them. Zero, a negative value and anything else are refused, because a
// retention of nothing is immediately said another way.
func ParseRemove(s string) (Removal, error) {
	s = strings.TrimSpace(s)
	if s == PolicyImmediately {
		return Removal{}, nil
	}
	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return Removal{}, fmt.Errorf("scenario: %q is not a removal policy", s)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil {
			return Removal{}, fmt.Errorf("scenario: %q is not a removal policy", s)
		}
	}
	if d <= 0 {
		return Removal{}, fmt.Errorf("scenario: %q is not a removal policy; a retention must be longer than zero", s)
	}
	return Removal{Retain: d}, nil
}

// MarkerPath is where the marker of a session is, under a build's source
// directory.
func MarkerPath(source, session string) string {
	return filepath.Join(source, MarkerDir, session+".json")
}

// IsMarkerName reports whether a file in MarkerDir is a marker. A file that
// WriteAtomic is still writing starts with .tmp- and is not one.
func IsMarkerName(name string) bool {
	return strings.HasSuffix(name, ".json") && !strings.HasPrefix(name, ".tmp-")
}

var hexDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ReadMarker reads a marker and checks everything a removal relies on. The
// error names the marker and what is wrong with it.
func ReadMarker(p string) (*Marker, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("marker %s: %w", p, err)
	}
	var m Marker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("marker %s: not a marker: %w", p, err)
	}
	bad := func(format string, args ...any) (*Marker, error) {
		return nil, fmt.Errorf("marker %s: %s", p, fmt.Sprintf(format, args...))
	}
	if m.Schema != markerSchema {
		return bad("the schema is %d, and this asz reads %d", m.Schema, markerSchema)
	}
	name := strings.TrimSuffix(filepath.Base(p), ".json")
	if m.Session != name {
		return bad("it names the session %q, not %q", m.Session, name)
	}
	if !claudecode.IsSessionID(m.Session) {
		return bad("%q is not a session id discovery would find", m.Session)
	}
	if _, err := m.Removal(); err != nil {
		return bad("%v", err)
	}
	if m.State != MarkerWritten && m.State != MarkerRemoving {
		return bad("the state %q is not %s or %s", m.State, MarkerWritten, MarkerRemoving)
	}
	seen := map[string]bool{}
	for _, f := range m.Files {
		// A path that leaves the source directory would let a marker name a
		// file the build never wrote, such as a real transcript.
		if f.Path == "" || strings.Contains(f.Path, `\`) || path.Clean(f.Path) != f.Path || !filepath.IsLocal(filepath.FromSlash(f.Path)) {
			return bad("the path %q is not inside the source directory", f.Path)
		}
		if seen[f.Path] {
			return bad("the path %q is listed twice", f.Path)
		}
		seen[f.Path] = true
		if f.Size < 0 || !hexDigest.MatchString(f.SHA256) {
			return bad("the path %q has no size or no SHA-256", f.Path)
		}
	}
	return &m, nil
}

// WriteMarker writes a marker in one rename. It is not read-only: a pipeline
// rewrites it when it starts a removal, and on Windows a rename cannot
// replace a read-only file.
func WriteMarker(p string, m *Marker) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return storage.WriteAtomic(p, storage.PermState, func(w io.Writer) error {
		_, err := w.Write(append(data, '\n'))
		return err
	})
}

// Removal is the policy the marker names.
func (m *Marker) Removal() (Removal, error) {
	switch m.Policy {
	case PolicyImmediately:
		return Removal{}, nil
	case PolicyRetain:
		d, err := time.ParseDuration(m.Retain)
		if err != nil || d <= 0 {
			return Removal{}, fmt.Errorf("the retention %q is not a duration longer than zero", m.Retain)
		}
		return Removal{Retain: d}, nil
	}
	return Removal{}, fmt.Errorf("the policy %q is not %s or %s", m.Policy, PolicyImmediately, PolicyRetain)
}

// newMarker is the marker a build writes for a session.
func newMarker(session string, r Removal, files []MarkerFile) *Marker {
	m := &Marker{Schema: markerSchema, Session: session, Policy: PolicyImmediately, State: MarkerWritten, Files: files}
	if !r.Immediate() {
		m.Policy, m.Retain = PolicyRetain, r.Retain.String()
	}
	if m.Files == nil {
		m.Files = []MarkerFile{}
	}
	return m
}

// clearMarker gets a session ready to be written again. A marker that says
// removing means a pipeline is deleting the session's files right now, so
// the build stops rather than write under it. Any other marker is deleted
// first: a pipeline that meets no marker never removes, so an earlier marker
// cannot vouch for files this build is about to rewrite.
func clearMarker(out, source, session string) error {
	p := MarkerPath(source, session)
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var old struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(data, &old); err != nil {
		return fmt.Errorf("scenario: %s is not a marker this build can read (%w); delete it once no pipeline over %s is removing session %s, or use another --out", p, err, out, session)
	}
	if old.State == MarkerRemoving {
		return fmt.Errorf("scenario: session %s is being removed by a pipeline over %s; build it again once that has finished, or use another --out", session, out)
	}
	return os.Remove(p)
}
