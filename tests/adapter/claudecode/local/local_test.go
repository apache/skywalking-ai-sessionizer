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

// Package local is an end-to-end test of the claude-code-local adapter against
// a fixture that reproduces every awkward shape found in a real corpus.
//
// The fixture is synthetic by necessity - real transcripts contain a person's
// actual work - but every shape in it was observed in real data and is cited at
// the assertion that depends on it.
package local_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/verify"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

const (
	fixture = "claudecode-files"

	// Case 01: a complete conversation. Case 02: an uppercase session id.
	// See claudecode-files/README.md for what each case covers.
	case1 = "00000001-0000-4000-8000-000000000001"
	case2 = "00000002-0000-4000-8000-00000000000A"

	agentChecker  = "a0000001aaaa00001" // the "check the tests" child
	agentWFStep   = "a0000001bbbb00002" // a workflow-spawned child
	runBuildCheck = "wf_buildcheck"

	projCase1 = "01-full-conversation"
)

// collected runs one full collection pass over the fixture into a temp zone.
type collected struct {
	zone  *storage.Zone
	stats *claudecode.Stats
	t     *testing.T
}

// fixtureRoot is the absolute path of the fixture corpus.
func fixtureRoot(t *testing.T) string {
	t.Helper()
	src, err := filepath.Abs(fixture)
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func collect(t *testing.T) *collected {
	t.Helper()
	src := fixtureRoot(t)
	z := storage.NewZone(t.TempDir())
	st, err := claudecode.New(src, z, 0).CollectAll(nil)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(st.Errors) != 0 {
		t.Fatalf("collect reported errors: %v", st.Errors)
	}
	if !st.Complete() {
		t.Fatalf("collect incomplete: pending=%d conflicts=%d", st.Pending, st.Conflicts)
	}
	return &collected{zone: z, stats: st, t: t}
}

// records reads every landed record of one kind from a directory, in order.
func (c *collected) records(dir, kind string) []sessiondata.Record {
	c.t.Helper()
	items, err := os.ReadDir(dir)
	if err != nil {
		c.t.Fatalf("read %s: %v", dir, err)
	}
	var names []string
	for _, it := range items {
		if strings.HasPrefix(it.Name(), kind+"-") && strings.HasSuffix(it.Name(), sessiondata.Ext) {
			names = append(names, it.Name())
		}
	}
	sort.Strings(names)
	var out []sessiondata.Record
	for _, n := range names {
		f, err := os.Open(filepath.Join(dir, n))
		if err != nil {
			c.t.Fatal(err)
		}
		r, err := sessiondata.NewReader(f)
		if err != nil {
			f.Close()
			c.t.Fatal(err)
		}
		for {
			rec, err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				f.Close()
				c.t.Fatal(err)
			}
			out = append(out, *rec)
		}
		f.Close()
	}
	return out
}

func (c *collected) mainRecords() []sessiondata.Record {
	return c.records(c.zone.StreamDir(case1, storage.StreamMain), "transcript")
}

// --- discovery -------------------------------------------------------------

func TestDiscoversSessionsAndIgnoresNoise(t *testing.T) {
	src, _ := filepath.Abs(fixture)
	sessions, err := claudecode.Discover(src)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]claudecode.Session{}
	for _, s := range sessions {
		got[s.ID] = s
	}
	if len(got) != 2 {
		t.Fatalf("discovered %d sessions, want 2: %v", len(got), keys(got))
	}
	// "memory" is a sibling directory inside a project dir, not a session.
	if _, bad := got["memory"]; bad {
		t.Error("memory/ was treated as a session")
	}
	// Session ids are not reliably lowercase in real corpora; joins are byte-exact.
	if _, ok := got[case2]; !ok {
		t.Error("uppercase session id was not discovered")
	}
	// The script lives under a different project directory; session-first
	// grouping must union it into the session it belongs to.
	s := got[case1]
	if len(s.Dirs) != 2 {
		t.Errorf("session spans %d dirs, want 2 (script filed elsewhere): %v", len(s.Dirs), s.Dirs)
	}
	if s.Primary != projCase1 {
		t.Errorf("Primary = %q, want the directory holding the main transcript", s.Primary)
	}
}

// --- the copy --------------------------------------------------------------

// Every source line becomes exactly one record, in order, pinned to where it
// came from.
//
// This replaced a stronger claim. Landed records used to keep the source bytes
// verbatim, so the test could compare them; now the source is converted on the
// way in and the bytes are not kept. What survives is the part that a reader
// depends on: one record per line, its line number, its byte offset, its size,
// and the digest of the bytes it came from.
//
// The digest is what keeps this honest. It cannot rebuild the source, but two
// collectors reading the same line produce the same digest, so a record that
// claims a line it did not come from is still detectable.
func TestEverySourceLineBecomesOneRecord(t *testing.T) {
	c := collect(t)
	srcRoot, _ := filepath.Abs(fixture)

	cases := []struct{ dir, kind, src string }{
		{c.zone.StreamDir(case1, storage.StreamMain), "transcript", projCase1 + "/" + case1 + ".jsonl"},
		{c.zone.StreamDir(case1, agentChecker), "transcript", projCase1 + "/" + case1 + "/subagents/agent-" + agentChecker + ".jsonl"},
		{c.zone.RunDir(case1, runBuildCheck), "journal", projCase1 + "/" + case1 + "/subagents/workflows/" + runBuildCheck + "/journal.jsonl"},
	}
	for _, tc := range cases {
		raw, err := os.ReadFile(filepath.Join(srcRoot, tc.src))
		if err != nil {
			t.Fatal(err)
		}
		want := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
		recs := c.records(tc.dir, tc.kind)
		if len(recs) != len(want) {
			t.Errorf("%s: landed %d records, source has %d lines", tc.src, len(recs), len(want))
			continue
		}
		var off uint64
		for i, rec := range recs {
			line := want[i]
			if rec.Ord != uint64(i+1) {
				t.Errorf("%s: record %d has ord %d, want %d", tc.src, i, rec.Ord, i+1)
			}
			if rec.Off != off {
				t.Errorf("%s line %d: off = %d, want %d", tc.src, i+1, rec.Off, off)
			}
			if rec.Bytes != len(line) {
				t.Errorf("%s line %d: bytes = %d, want %d", tc.src, i+1, rec.Bytes, len(line))
			}
			sum := sha256.Sum256([]byte(line))
			if got := hex.EncodeToString(sum[:])[:12]; got != rec.Sha {
				t.Errorf("%s line %d: sha = %s, want %s", tc.src, i+1, rec.Sha, got)
			}
			if len(rec.Parts) == 0 {
				t.Errorf("%s line %d converted to nothing at all", tc.src, i+1)
			}
			off += uint64(len(line)) + 1 // the newline the source carried
		}
	}
}

// TestEveryEnvelopeFieldEarnsItsPlace asserts the rule that a landed record
// carries only what rebuilds the conversation or is the conversation: position,
// integrity, and the source bytes. Nothing decorative.
func TestEveryEnvelopeFieldEarnsItsPlace(t *testing.T) {
	c := collect(t)
	dir := c.zone.StreamDir(case1, storage.StreamMain)
	items, _ := os.ReadDir(dir)
	var landed string
	for _, it := range items {
		if strings.HasPrefix(it.Name(), "transcript-") {
			landed = filepath.Join(dir, it.Name())
		}
	}
	f, err := os.Open(landed)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r, err := sessiondata.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	h := r.Header()

	// Header: identity and provenance for the whole file.
	for name, got := range map[string]string{
		"kind":    string(h.Kind),
		"session": h.Session,
		"stream":  h.Stream,
		"src":     h.Src,
		"adapter": h.Adapter,
		"at":      h.At,
	} {
		if got == "" {
			t.Errorf("header field %q is empty", name)
		}
	}
	if h.Session != case1 || h.Stream != storage.StreamMain {
		t.Errorf("header identity = %s/%s", h.Session, h.Stream)
	}
	// src is the only place the source path survives - the landing zone keys by
	// session and drops the project slug.
	if !strings.Contains(h.Src, projCase1) {
		t.Errorf("src = %q, expected to retain the source directory", h.Src)
	}

	// Per record: where it came from, what it is, what it joins to, what was
	// in it. Nothing decorative.
	raw, _ := os.ReadFile(landed)
	line := strings.Split(string(raw), "\n")[1]
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &fields); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]string{
		// where it came from
		"ord":   "source line number - proves no line was skipped",
		"off":   "source byte offset - proves no byte range is unaccounted for",
		"sha":   "digest of the source bytes - provenance, though they are not kept",
		"bytes": "size of the source record - what makes byte contiguity checkable",
		// what it is
		"from":    "who produced it: the agent, something outside it, or the harness",
		"time":    "when the runtime says it happened",
		"trigger": "what started the run it belongs to",
		"flags":   "facts about its shape that no identifier expresses",
		// what it joins to, in role names
		"id":        "its own identity",
		"parent":    "its containment parent",
		"call":      "the provider call it is a fragment of",
		"run":       "the agent loop it belongs to",
		"continues": "the record a new model context resumes from",
		"tool":      "the tool use it is about",
		"child":     "the child agent stream it names",
		// what was in it
		"parts":   "the content, in one shape regardless of runtime",
		"usage":   "what the provider reported it spent",
		"dropped": "what the conversion deliberately left behind, and how much",
	}
	for k := range fields {
		if _, ok := allowed[k]; !ok {
			t.Errorf("record carries field %q with no stated purpose", k)
		}
	}
	for _, must := range []string{"ord", "off", "sha", "bytes", "parts"} {
		if _, ok := fields[must]; !ok {
			t.Errorf("record missing required field %q", must)
		}
	}
}

func keys(m map[string]claudecode.Session) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var _ = verify.Session
var _ = index.New

func hasFlag(rec sessiondata.Record, name string) bool {
	for _, f := range rec.Flags {
		if f == name {
			return true
		}
	}
	return false
}

// content is every readable piece of a record, joined, for a substring check.
func content(rec sessiondata.Record) string {
	var b strings.Builder
	for _, p := range rec.Parts {
		b.WriteString(p.Text)
		b.Write(p.Data)
	}
	return b.String()
}
