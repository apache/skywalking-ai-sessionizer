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
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// everyKindOfFile is one session holding every kind of file a removal
// deletes: a main transcript, a child with a meta file, a workflow whose
// script is filed under another project, and plugin output.
const everyKindOfFile = `title: every kind of file a removal deletes
steps:
  - input: fix the timeout and check everything
  - call:
      text: Fixing it.
      tool:
        name: Bash
        input: {command: "sed -i 's/= 10/= 30/' server.go"}
        result: {text: "", after: 500ms}
        changes:
          - {path: server.go, before: "package main\n\nvar timeout = 10\n", after: "package main\n\nvar timeout = 30\n"}
      usage: {in: 2, out: 50, cache_read: 900, cache_write: 100}
  - call:
      agent: {name: helper, prompt: look at the tests, steps: [{call: {text: tests look fine, usage: {out: 20}}}], notify: true}
  - call:
      workflow:
        name: verify
        script_project: -Users-dev-script-filed-elsewhere
        children:
          - {name: lint, prompt: run the linter, steps: [{call: {text: lint done}}]}
  - call: {text: Everything checks out.}
`

var markerAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func loadEveryKind(t *testing.T) *Scenario {
	t.Helper()
	path := filepath.Join(t.TempDir(), "every-kind.yaml")
	if err := os.WriteFile(path, []byte(everyKindOfFile), 0o600); err != nil {
		t.Fatal(err)
	}
	sc, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

// TestTheMarkerListsEveryFileOfTheSessionAndNothingElse. A removal deletes
// exactly what the marker lists, so a file left out would outlive its
// session, and a file listed that is not the session's would be deleted with
// it. The two noise files are shared by every session of the project.
func TestTheMarkerListsEveryFileOfTheSessionAndNothingElse(t *testing.T) {
	sc := loadEveryKind(t)
	out := t.TempDir()
	b, err := Build(sc, FormatClaudeCode, out, Options{At: markerAt, Remove: Removal{Retain: 24 * time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(b.Out, "_scenario")); err != nil || !fi.IsDir() {
		t.Fatalf("a claude-code build left no _scenario directory in its root: %v", err)
	}
	source := filepath.Join(b.Out, "_source")
	if b.Marker != MarkerPath(source, b.Session) {
		t.Fatalf("the marker is at %s, want %s", b.Marker, MarkerPath(source, b.Session))
	}
	m, err := ReadMarker(b.Marker)
	if err != nil {
		t.Fatal(err)
	}
	if m.Policy != PolicyRetain || m.Retain != "24h0m0s" || m.State != MarkerWritten || m.Session != b.Session {
		t.Fatalf("the marker says policy %q, retain %q, state %q, session %q", m.Policy, m.Retain, m.State, m.Session)
	}

	onDisk := map[string]bool{}
	err = filepath.WalkDir(source, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == MarkerDir {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(source, p)
		rel = filepath.ToSlash(rel)
		if strings.HasSuffix(rel, "/not-a-uuid.jsonl") || strings.Contains(rel, "/memory/") {
			return nil
		}
		onDisk[rel] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, f := range m.Files {
		listed[f.Path] = true
		data, err := os.ReadFile(filepath.Join(source, filepath.FromSlash(f.Path)))
		if err != nil {
			t.Fatalf("the marker lists %s: %v", f.Path, err)
		}
		sum := sha256.Sum256(data)
		if int64(len(data)) != f.Size || hex.EncodeToString(sum[:]) != f.SHA256 {
			t.Errorf("%s: the marker records %d bytes and %s, the file has %d bytes", f.Path, f.Size, f.SHA256, len(data))
		}
	}
	for p := range onDisk {
		if !listed[p] {
			t.Errorf("the build wrote %s and the marker does not list it", p)
		}
	}
	for p := range listed {
		if !onDisk[p] {
			t.Errorf("the marker lists %s, which is not a file of the session", p)
		}
	}
	// Every kind is there, so the comparison above covers each of them.
	for _, part := range []string{b.Session + ".jsonl", ".meta.json", "journal.jsonl", "-Users-dev-script-filed-elsewhere/", PluginOutputDir + "/"} {
		found := false
		for p := range listed {
			found = found || strings.Contains(p, part)
		}
		if !found {
			t.Errorf("the marker lists no file with %q in its path", part)
		}
	}
	// What the command prints does not change: the noise is still written
	// and listed, and the marker is not one of the files.
	noise := 0
	for _, f := range b.Files {
		if strings.HasSuffix(f, "not-a-uuid.jsonl") || strings.HasSuffix(f, "notes.md") {
			noise++
		}
		if strings.Contains(f, MarkerDir) {
			t.Errorf("the build lists its marker %s among the files it wrote", f)
		}
	}
	if noise != 2 {
		t.Errorf("the build lists %d noise files, want 2", noise)
	}
}

// TestABuildLeavesASessionBeingRemovedAlone. A pipeline that started a
// removal deletes the files its marker lists. A build that wrote them again
// meanwhile would have its new files deleted, or stop the removal half way.
func TestABuildLeavesASessionBeingRemovedAlone(t *testing.T) {
	sc, err := Load(filepath.Join("..", "..", "tests", "scenarios", "assembly.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	b, err := Build(sc, FormatClaudeCode, out, Options{At: markerAt})
	if err != nil {
		t.Fatal(err)
	}
	m, err := ReadMarker(b.Marker)
	if err != nil {
		t.Fatal(err)
	}
	m.State = MarkerRemoving
	if err := WriteMarker(b.Marker, m); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(b.Out, "_source", filepath.FromSlash(m.Files[0].Path))
	before, err := os.ReadFile(main)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Build(sc, FormatClaudeCode, out, Options{At: markerAt.Add(time.Hour)})
	if err == nil || !strings.Contains(err.Error(), "is being removed") {
		t.Fatalf("a build over a session being removed gave %v", err)
	}
	after, err := os.ReadFile(main)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the refused build still rewrote the session's files")
	}
}

// TestABuildReplacesAnEarlierMarker. A marker vouches for the bytes it
// records. One left from an earlier build must not survive a build that
// writes the files again, or it would describe files that are no longer
// there, and keep a policy nobody asked for this time.
func TestABuildReplacesAnEarlierMarker(t *testing.T) {
	sc, err := Load(filepath.Join("..", "..", "tests", "scenarios", "assembly.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	b, err := Build(sc, FormatClaudeCode, out, Options{At: markerAt})
	if err != nil {
		t.Fatal(err)
	}
	m, err := ReadMarker(b.Marker)
	if err != nil {
		t.Fatal(err)
	}
	m.Files[0].SHA256 = strings.Repeat("0", 64)
	m.Policy, m.Retain = PolicyRetain, "1h0m0s"
	if err := WriteMarker(b.Marker, m); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(sc, FormatClaudeCode, out, Options{At: markerAt}); err != nil {
		t.Fatal(err)
	}
	again, err := ReadMarker(b.Marker)
	if err != nil {
		t.Fatal(err)
	}
	if again.Policy != PolicyImmediately || again.Retain != "" {
		t.Fatalf("the policy of the earlier marker survived: %q %q", again.Policy, again.Retain)
	}
	data, err := os.ReadFile(filepath.Join(b.Out, "_source", filepath.FromSlash(again.Files[0].Path)))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if again.Files[0].SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("the marker still records a digest the file does not have")
	}
}

// TestAnSDBuildWritesNoMarker. An sd build writes no source, so there is
// nothing a removal could delete, and its root is not a scenario root.
func TestAnSDBuildWritesNoMarker(t *testing.T) {
	sc, err := Load(filepath.Join("..", "..", "tests", "scenarios", "assembly.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	b, err := Build(sc, FormatSD, out, Options{At: markerAt})
	if err != nil {
		t.Fatal(err)
	}
	if b.Marker != "" {
		t.Fatalf("an sd build reports the marker %s", b.Marker)
	}
	for _, p := range []string{filepath.Join(b.Out, "_scenario"), filepath.Join(b.Out, "_source", MarkerDir)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("an sd build made %s", p)
		}
	}
}

// TestParseRemove holds the syntax of --remove. A retention of zero or less
// is refused rather than read as immediately, since a person who wrote a
// duration meant one.
func TestParseRemove(t *testing.T) {
	for s, want := range map[string]time.Duration{"immediately": 0, "30m": 30 * time.Minute, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour} {
		got, err := ParseRemove(s)
		if err != nil || got.Retain != want {
			t.Errorf("ParseRemove(%q) = %v, %v; want %s", s, got.Retain, err, want)
		}
	}
	for _, s := range []string{"0", "0d", "-1h", "soon", "", "1.5d"} {
		if _, err := ParseRemove(s); err == nil {
			t.Errorf("ParseRemove(%q) was accepted", s)
		}
	}
	if got := (Removal{Retain: 24 * time.Hour}).Describe(); got != "24h after its last record, once sent" {
		t.Errorf("Describe gave %q", got)
	}
	if got := (Removal{}).Describe(); got != "immediately, once sent" {
		t.Errorf("Describe gave %q", got)
	}
}

// TestReadMarkerRefusesWhatARemovalCannotTrust. Each field decides what a
// removal deletes or when, so a marker that is wrong in any of them keeps its
// session instead.
func TestReadMarkerRefusesWhatARemovalCannotTrust(t *testing.T) {
	const id = "c30736f2-ac0c-4a72-89b1-01a0844af62d"
	empty := sha256.Sum256(nil)
	good := func() *Marker {
		return &Marker{Schema: 1, Session: id, Policy: PolicyImmediately, State: MarkerWritten,
			Files: []MarkerFile{{Path: "-Users-dev-scenario/" + id + ".jsonl", SHA256: hex.EncodeToString(empty[:])}}}
	}
	dir := t.TempDir()
	if _, err := readMarkerAt(t, dir, id+".json", good()); err != nil {
		t.Fatalf("a good marker was refused: %v", err)
	}
	for i, c := range []struct {
		name, file string
		change     func(*Marker)
		want       string
	}{
		{"a path out of the source", id + ".json", func(m *Marker) { m.Files[0].Path = "../elsewhere/x.jsonl" }, "not inside the source directory"},
		{"an absolute path", id + ".json", func(m *Marker) { m.Files[0].Path = "/etc/passwd" }, "not inside the source directory"},
		{"another session than its name", id + ".json", func(m *Marker) { m.Session = "aaaaaaaa-bbbb-4ccc-8ddd-000000000001" }, "names the session"},
		{"a name that is no session id", "notes.json", func(m *Marker) { m.Session = "notes" }, "not a session id"},
		{"an unknown policy", id + ".json", func(m *Marker) { m.Policy = "later" }, "policy"},
		{"a retention of nothing", id + ".json", func(m *Marker) { m.Policy, m.Retain = PolicyRetain, "0s" }, "retention"},
		{"an unknown state", id + ".json", func(m *Marker) { m.State = "done" }, "state"},
		{"a path listed twice", id + ".json", func(m *Marker) { m.Files = append(m.Files, m.Files[0]) }, "listed twice"},
		{"another schema", id + ".json", func(m *Marker) { m.Schema = 2 }, "schema"},
	} {
		m := good()
		c.change(m)
		_, err := readMarkerAt(t, filepath.Join(dir, strings.Repeat("x", i+1)), c.file, m)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: ReadMarker gave %v, want an error saying %q", c.name, err, c.want)
		}
	}
	for name, want := range map[string]bool{id + ".json": true, ".tmp-123456": false, id + ".json.tmp": false, "notes.md": false} {
		if IsMarkerName(name) != want {
			t.Errorf("IsMarkerName(%q) = %v", name, !want)
		}
	}
}

// readMarkerAt writes a marker under dir with the file name given and reads
// it back.
func readMarkerAt(t *testing.T, dir, name string, m *Marker) (*Marker, error) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := WriteMarker(p, m); err != nil {
		t.Fatal(err)
	}
	return ReadMarker(p)
}
