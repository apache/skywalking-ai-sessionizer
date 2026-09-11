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

package remove_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecodechanges"
	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp"
	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp/otlptest"
	"github.com/apache/skywalking-ai-sessionizer/internal/metrics"
	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario/remove"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// everyKind is one session holding every kind of file a removal deletes: a
// main transcript, a child with a meta file, a workflow whose script is
// filed under another project, and plugin output.
const everyKind = `title: every kind of file a removal deletes
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

var at = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// root is a scenario root and the receiver its pipeline sends to.
type root struct {
	t        *testing.T
	dir      string
	source   string
	zone     *storage.Zone
	rcv      *otlptest.Receiver
	endpoint string
}

func newRoot(t *testing.T) *root {
	t.Helper()
	// Claude Code's own directory is a place of the test's, so the guard
	// on it compares against something known on every machine.
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rcv, err := otlptest.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rcv.Close)
	dir := t.TempDir()
	o := rcv.Options(otlp.ProtocolGRPC)
	return &root{t: t, dir: dir, source: filepath.Join(dir, "_source"), zone: storage.NewZone(dir), rcv: rcv,
		endpoint: otlp.EndpointOf(o.Protocol, o.Endpoint, o.TLS)}
}

func load(t *testing.T, body string) *scenario.Scenario {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scenario.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	sc, err := scenario.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

func (r *root) build(sc *scenario.Scenario, id string, opts scenario.Options) *scenario.Built {
	r.t.Helper()
	one := *sc
	if id != "" {
		one.Session = id
	}
	if opts.At.IsZero() {
		opts.At = at
	}
	b, err := scenario.Build(&one, scenario.FormatClaudeCode, r.dir, opts)
	if err != nil {
		r.t.Fatal(err)
	}
	return b
}

func (r *root) changesRoot() string { return filepath.Join(r.source, "plugins", "data") }

// collect lands with both adapters and returns how many sources landed.
func (r *root) collect() int {
	r.t.Helper()
	st, err := claudecode.New(r.source, r.zone, 0).CollectAll(nil)
	if err != nil {
		r.t.Fatal(err)
	}
	cs, err := claudecodechanges.New(r.changesRoot(), r.zone, 0).CollectAll(nil)
	if err != nil {
		r.t.Fatal(err)
	}
	if errs := append(st.Errors, cs.Errors...); len(errs) > 0 {
		r.t.Fatalf("collect: %v", errs)
	}
	return st.SourcesLanded + cs.SourcesLanded
}

// derive writes the metrics of what is landed and returns how many requests
// it put in the spool.
func (r *root) derive() int {
	r.t.Helper()
	st, err := (&metrics.Deriver{Zone: r.zone, Options: metrics.Options{Grace: -1, Version: "test"}}).Pass(nil)
	if err != nil {
		r.t.Fatal(err)
	}
	if len(st.Errors) > 0 {
		r.t.Fatalf("derive: %v", st.Errors)
	}
	return st.Requests
}

// parse writes rounds for every session directory, as a full pass does, and
// returns how many it wrote.
func (r *root) parse() int {
	r.t.Helper()
	items, err := os.ReadDir(r.dir)
	if err != nil {
		r.t.Fatal(err)
	}
	written := 0
	for _, it := range items {
		if !it.IsDir() || strings.HasPrefix(it.Name(), "_") {
			continue
		}
		for {
			res, err := parse.Session(r.zone, parse.Options{Conversation: it.Name(), Session: it.Name(), MaxRoundBytes: 4096})
			if err != nil {
				r.t.Fatalf("parse %s: %v", it.Name(), err)
			}
			if res.Changed() {
				written++
			}
			if !res.Changed() || !res.More {
				break
			}
		}
	}
	return written
}

// push sends what is not sent yet, recording endpoint as the receiver.
func (r *root) push(endpoint string) *otlp.Stats {
	r.t.Helper()
	client, err := r.rcv.Client(otlp.ProtocolGRPC)
	if err != nil {
		r.t.Fatal(err)
	}
	defer client.Close()
	p := &otlp.Pusher{Zone: r.zone, Client: client, Endpoint: endpoint, Version: "test",
		ServiceName: "Remove Test", InstanceID: "remove-test", Layer: "AI_AGENT", MetricsService: "Remove Test"}
	if err := p.Prepare(); err != nil {
		r.t.Fatal(err)
	}
	st, err := p.Pass()
	if err != nil {
		r.t.Fatal(err)
	}
	if len(st.Errors) > 0 {
		r.t.Fatalf("push: %v", st.Errors)
	}
	return st
}

// pipeline is one pass of asz collect before its removal.
func (r *root) pipeline() {
	r.t.Helper()
	r.collect()
	r.derive()
	r.parse()
	r.push(r.endpoint)
}

func (r *root) remover() *remove.Remover {
	return &remove.Remover{Zone: r.zone, Source: r.source, Local: claudecode.New(r.source, r.zone, 0),
		Changes: claudecodechanges.New(r.changesRoot(), r.zone, 0), ChangesRoot: r.changesRoot(), Derives: true}
}

func (r *root) input() remove.Input {
	return remove.Input{Endpoint: r.endpoint, SendLogs: true, SendMetrics: true}
}

func marker(t *testing.T, b *scenario.Built) *scenario.Marker {
	t.Helper()
	m, err := scenario.ReadMarker(b.Marker)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return !errors.Is(err, fs.ErrNotExist)
}

func noErrors(t *testing.T, res *remove.Result) {
	t.Helper()
	if len(res.Errors) > 0 {
		t.Fatalf("the removal reported %v", res.Errors)
	}
}

// removed checks that nothing of the session is left, and that nothing
// every session shares went with it.
func (r *root) removed(b *scenario.Built, m *scenario.Marker) {
	r.t.Helper()
	t, s := r.t, b.Session
	if exists(b.Marker) {
		t.Errorf("the marker %s is still there", b.Marker)
	}
	for _, f := range m.Files {
		if exists(filepath.Join(r.source, filepath.FromSlash(f.Path))) {
			t.Errorf("the source file %s is still there", f.Path)
		}
		if i := strings.Index(f.Path, "/"+s+"/"); i >= 0 {
			if dir := filepath.Join(r.source, filepath.FromSlash(f.Path[:i+1+len(s)])); exists(dir) {
				t.Errorf("the directory %s, named for the session, is still there", dir)
			}
		}
	}
	for _, p := range []string{filepath.Join(r.dir, s), filepath.Join(r.dir, "_conversations", s)} {
		if exists(p) {
			t.Errorf("%s is still there", p)
		}
	}
	if items, err := os.ReadDir(filepath.Join(r.dir, storage.SpoolDir)); err == nil {
		for _, it := range items {
			if owner, ok := metrics.SpoolOwner(it.Name()); ok && owner == s {
				t.Errorf("the spool file %s is still there", it.Name())
			}
		}
	}
	for _, state := range []string{filepath.Join(r.dir, otlp.StateFile), filepath.Join(r.dir, storage.SpoolDir, metrics.StateFile)} {
		if data, err := os.ReadFile(state); err == nil && bytes.Contains(data, []byte(s)) {
			t.Errorf("%s still names the session", state)
		}
	}
	if items, err := os.ReadDir(filepath.Join(r.dir, storage.RemovedDir)); err == nil && len(items) > 0 {
		t.Errorf("%s still holds %d entries", storage.RemovedDir, len(items))
	}
	for _, p := range []string{
		filepath.Join(r.source, "-Users-dev-scenario", "not-a-uuid.jsonl"),
		filepath.Join(r.source, "-Users-dev-scenario", "memory", "notes.md"),
		filepath.Join(r.source, scenario.MarkerDir),
		filepath.Join(r.dir, storage.ScenarioDir),
		filepath.Join(r.source, filepath.FromSlash(scenario.PluginOutputDir)),
	} {
		if !exists(p) {
			t.Errorf("%s, which every session shares, was removed", p)
		}
	}
}

// TestASentSessionIsRemovedWhole. Once everything the session produced is
// recorded as sent to the endpoint, one pass removes all of it, and what
// comes after finds nothing to land, derive, parse or send.
func TestASentSessionIsRemovedWhole(t *testing.T) {
	r := newRoot(t)
	b := r.build(load(t, everyKind), "", scenario.Options{})
	m := marker(t, b)
	r.pipeline()

	rm := r.remover()
	res := rm.Pass(r.input())
	noErrors(t, res)
	if len(res.Removed) != 1 || res.Removed[0] != b.Session {
		t.Fatalf("removed %v, kept %v, waiting %v; want %s removed", res.Removed, res.Kept, res.Waiting, b.Session)
	}
	r.removed(b, m)
	data, err := os.ReadFile(filepath.Join(r.dir, otlp.StateFile))
	if err != nil || !bytes.Contains(data, []byte("endpoint "+r.endpoint)) {
		t.Fatalf("push.state lost its endpoint line: %v\n%s", err, data)
	}

	// Afterwards the session is not made again by anything.
	r.rcv.Reset()
	if n := r.collect(); n != 0 {
		t.Errorf("the adapters landed %d source(s) of a removed session", n)
	}
	if n := r.derive(); n != 0 {
		t.Errorf("the deriver wrote %d request(s) after the removal", n)
	}
	if n := r.parse(); n != 0 {
		t.Errorf("the parse wrote %d round(s) after the removal", n)
	}
	r.push(r.endpoint)
	if n := len(r.rcv.Requests()) + len(r.rcv.MetricsRequests()); n != 0 {
		t.Errorf("the push sent %d request(s) after the removal", n)
	}
	r.removed(b, m)
	again := rm.Pass(r.input())
	noErrors(t, again)
	if len(again.Removed) != 0 {
		t.Errorf("a second pass removed %v", again.Removed)
	}
}

// TestNothingIsRemovedBeforeAllOfItIsSent. A session goes only once its
// landed files, its rounds and its metrics are all recorded as sent. Until
// then it waits, and nothing is said about it, since it changes on its own.
func TestNothingIsRemovedBeforeAllOfItIsSent(t *testing.T) {
	r := newRoot(t)
	b := r.build(load(t, everyKind), "", scenario.Options{})
	r.collect()
	r.parse()
	rm := r.remover()

	res := rm.Pass(r.input())
	noErrors(t, res)
	if len(res.Removed) != 0 || len(res.Kept) != 0 || !waitingFor(res, "", "not sent yet") {
		t.Fatalf("before any push: removed %v, kept %v, waiting %v", res.Removed, res.Kept, res.Waiting)
	}
	if marker(t, b).State != scenario.MarkerWritten {
		t.Fatal("a pass that removed nothing changed the marker")
	}

	// Sent, but no metrics were derived yet. The session is carried into
	// the next derivation, since it no longer lands anything.
	r.push(r.endpoint)
	res = rm.Pass(r.input())
	noErrors(t, res)
	if len(res.Removed) != 0 || !waitingFor(res, b.Session, "metrics not derived yet") {
		t.Fatalf("with no metrics derived: removed %v, waiting %v", res.Removed, res.Waiting)
	}
	if len(res.DeriveNext) != 1 || res.DeriveNext[0] != b.Session {
		t.Fatalf("DeriveNext is %v, want the session", res.DeriveNext)
	}

	r.derive()
	res = rm.Pass(r.input())
	noErrors(t, res)
	if len(res.Removed) != 0 || !waitingFor(res, b.Session, "metrics not sent yet") {
		t.Fatalf("with metrics derived and not sent: removed %v, waiting %v", res.Removed, res.Waiting)
	}

	r.push(r.endpoint)
	res = rm.Pass(r.input())
	noErrors(t, res)
	if len(res.Removed) != 1 {
		t.Fatalf("once all of it was sent: removed %v, kept %v, waiting %v", res.Removed, res.Kept, res.Waiting)
	}
}

func waitingFor(res *remove.Result, session, reason string) bool {
	for _, w := range res.Waiting {
		if w.Session == session && strings.Contains(w.Reason, reason) {
			return true
		}
	}
	return false
}

func keptFor(res *remove.Result, session, reason string) bool {
	for _, k := range res.Kept {
		if k.Session == session && strings.Contains(k.Reason, reason) {
			return true
		}
	}
	return false
}

// TestARejectionKeepsTheSession. A receiver that took a request and
// rejected records in it holds less than was sent, so the session is not
// sent whole and is kept. The record of it survives the saves later
// requests make.
func TestARejectionKeepsTheSession(t *testing.T) {
	r := newRoot(t)
	sc := load(t, everyKind)
	b := r.build(sc, "", scenario.Options{})
	r.rcv.Reject(1)
	r.collect()
	r.derive()
	r.parse()
	r.push(r.endpoint)
	rm := r.remover()
	res := rm.Pass(r.input())
	noErrors(t, res)
	if len(res.Removed) != 0 || !keptFor(res, b.Session, "rejected records") {
		t.Fatalf("after a partial success: removed %v, kept %v", res.Removed, res.Kept)
	}

	// Another session, sent whole. Its push saves push.state again.
	r.rcv.Reject(0)
	other := r.build(sc, "aaaaaaaa-bbbb-4ccc-8ddd-000000000001", scenario.Options{At: at.Add(time.Hour)})
	r.pipeline()
	res = rm.Pass(r.input())
	noErrors(t, res)
	if len(res.Removed) != 1 || res.Removed[0] != other.Session {
		t.Fatalf("removed %v, want only %s; kept %v", res.Removed, other.Session, res.Kept)
	}
	if !exists(filepath.Join(r.dir, b.Session)) || !exists(b.Marker) {
		t.Fatal("the session whose records were rejected was removed after a later save of push.state")
	}
	if keptFor(res, b.Session, "rejected") {
		t.Fatal("the same reason was reported twice by one process")
	}
}

// TestAnotherEndpointKeepsEverySession. push.state records the receivers
// files went to, not which file went to which. Sent somewhere else, nothing
// here is proved sent to this pipeline's receiver.
func TestAnotherEndpointKeepsEverySession(t *testing.T) {
	r := newRoot(t)
	b := r.build(load(t, everyKind), "", scenario.Options{})
	r.pipeline()
	in := r.input()
	in.Endpoint = "grpc://elsewhere.example:11800"
	res := r.remover().Pass(in)
	noErrors(t, res)
	if len(res.Removed) != 0 || !keptFor(res, "", "push.state records sending to "+r.endpoint) {
		t.Fatalf("removed %v, kept %v", res.Removed, res.Kept)
	}
	if !exists(b.Marker) {
		t.Fatal("the marker went")
	}
}

// TestASessionWithNoMarkerIsNeverRemoved. A real Claude Code session has no
// marker. Sent or not, nothing of it is touched.
func TestASessionWithNoMarkerIsNeverRemoved(t *testing.T) {
	r := newRoot(t)
	b := r.build(load(t, everyKind), "", scenario.Options{})
	m := marker(t, b)
	r.pipeline()
	if err := os.Remove(b.Marker); err != nil {
		t.Fatal(err)
	}
	res := r.remover().Pass(r.input())
	noErrors(t, res)
	if len(res.Removed) != 0 || len(res.Kept) != 0 {
		t.Fatalf("removed %v, kept %v", res.Removed, res.Kept)
	}
	for _, f := range m.Files {
		if !exists(filepath.Join(r.source, filepath.FromSlash(f.Path))) {
			t.Errorf("%s was deleted", f.Path)
		}
	}
	for _, p := range []string{filepath.Join(r.dir, b.Session), filepath.Join(r.dir, "_conversations", b.Session)} {
		if !exists(p) {
			t.Errorf("%s was deleted", p)
		}
	}
}

// TestRetentionCountsFromTheLastRecord. A retained session stays until its
// last record is as old as the policy says, counted from the records and
// never from when anything ran, so the same data decides the same way.
func TestRetentionCountsFromTheLastRecord(t *testing.T) {
	r := newRoot(t)
	b := r.build(load(t, everyKind), "", scenario.Options{Remove: scenario.Removal{Retain: 24 * time.Hour}})
	m := marker(t, b)
	last := lastRecord(t, r.source, m)
	r.pipeline()

	now := last.Add(24*time.Hour - time.Second)
	rm := r.remover()
	rm.Now = func() time.Time { return now }
	res := rm.Pass(r.input())
	noErrors(t, res)
	if len(res.Removed) != 0 || !waitingFor(res, b.Session, "kept until") {
		t.Fatalf("a second before its retention ends: removed %v, waiting %v", res.Removed, res.Waiting)
	}
	// The same remover, so its memory of the last record is used as well.
	now = last.Add(24 * time.Hour)
	res = rm.Pass(r.input())
	noErrors(t, res)
	if len(res.Removed) != 1 {
		t.Fatalf("once its retention ended: removed %v, waiting %v, kept %v", res.Removed, res.Waiting, res.Kept)
	}
	r.removed(b, m)
}

// lastRecord is the latest time among the lines of the source files, read
// without the landing code.
func lastRecord(t *testing.T, source string, m *scenario.Marker) time.Time {
	t.Helper()
	var last time.Time
	for _, f := range m.Files {
		data, err := os.ReadFile(filepath.Join(source, filepath.FromSlash(f.Path)))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range bytes.Split(data, []byte("\n")) {
			var rec map[string]any
			if json.Unmarshal(line, &rec) != nil {
				continue
			}
			for _, k := range []string{"timestamp", "time"} {
				if s, ok := rec[k].(string); ok {
					if ts, err := time.Parse(time.RFC3339Nano, s); err == nil && ts.After(last) {
						last = ts
					}
				}
			}
		}
	}
	if last.IsZero() {
		t.Fatal("no source line carries a time")
	}
	return last
}

// TestNeverInClaudeCodesOwnDirectory. A source directory that is Claude
// Code's own, lies inside it, or holds it, could lead a marker to a real
// transcript. Nothing is removed there, whatever the markers say.
//
// Every place Claude Code may keep its files is guarded, not only the one
// the pipeline's environment selects. The pipeline can run with
// CLAUDE_CONFIG_DIR or XDG_CONFIG_HOME set differently from the Claude Code
// a person runs, which keeps its files in ~/.claude when neither is set.
func TestNeverInClaudeCodesOwnDirectory(t *testing.T) {
	const id = "c30736f2-ac0c-4a72-89b1-01a0844af62d"
	for _, c := range []struct {
		name string
		// claude and xdg are where CLAUDE_CONFIG_DIR and XDG_CONFIG_HOME
		// point, under the test's directory. Empty leaves the variable
		// unset. The home directory is always home, under the same.
		claude, xdg string
		// source is the source directory. With link set, source is a
		// symbolic link to link.
		source, link string
		guarded      bool
	}{
		{name: "equal", claude: "cfg", source: "cfg/projects", guarded: true},
		{name: "inside", claude: "cfg", source: "cfg/projects/sub", guarded: true},
		{name: "holding it", claude: "cfg", source: "cfg", guarded: true},
		{name: "in ~/.claude with CLAUDE_CONFIG_DIR elsewhere", claude: "cfg", source: "home/.claude/projects", guarded: true},
		{name: "in ~/.claude with XDG_CONFIG_HOME elsewhere", xdg: "xdg", source: "home/.claude/projects/sub", guarded: true},
		{name: "in ~/.claude with both elsewhere", claude: "cfg", xdg: "xdg", source: "home/.claude/plugins/data", guarded: true},
		{name: "in XDG_CONFIG_HOME/claude with CLAUDE_CONFIG_DIR elsewhere", claude: "cfg", xdg: "xdg", source: "xdg/claude/projects", guarded: true},
		{name: "a link to ~/.claude/projects", claude: "cfg", xdg: "xdg", source: "scenario/_source", link: "home/.claude/projects", guarded: true},
		{name: "a place of its own", claude: "cfg", xdg: "xdg", source: "scenario/_source"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.link != "" && runtime.GOOS == "windows" {
				t.Skip("a symbolic link needs a privilege on Windows")
			}
			base := t.TempDir()
			at := func(rel string) string {
				if rel == "" {
					return ""
				}
				return filepath.Join(base, filepath.FromSlash(rel))
			}
			t.Setenv("CLAUDE_CONFIG_DIR", at(c.claude))
			t.Setenv("XDG_CONFIG_HOME", at(c.xdg))
			t.Setenv("HOME", at("home"))
			t.Setenv("USERPROFILE", at("home"))
			source := at(c.source)
			if c.link != "" {
				if err := os.MkdirAll(at(c.link), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(at(c.link), source); err != nil {
					t.Fatal(err)
				}
			}
			kept := filepath.Join(source, "-Users-dev-real", id+".jsonl")
			if err := os.MkdirAll(filepath.Dir(kept), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(kept, []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := scenario.WriteMarker(scenario.MarkerPath(source, id), &scenario.Marker{Schema: 1, Session: id,
				Policy: scenario.PolicyImmediately, State: scenario.MarkerRemoving, Files: []scenario.MarkerFile{}}); err != nil {
				t.Fatal(err)
			}
			zone := storage.NewZone(t.TempDir())
			changes := filepath.Join(source, "plugins", "data")
			rm := &remove.Remover{Zone: zone, Source: source, Local: claudecode.New(source, zone, 0),
				Changes: claudecodechanges.New(changes, zone, 0), ChangesRoot: changes}
			res := rm.Pass(remove.Input{Endpoint: "grpc://127.0.0.1:11800", SendLogs: true, SendMetrics: true})
			noErrors(t, res)
			if got := keptFor(res, "", "where Claude Code keeps its own files"); got != c.guarded {
				t.Fatalf("guarded %v, want %v: kept %v, waiting %v", got, c.guarded, res.Kept, res.Waiting)
			}
			if !c.guarded {
				// The guard let it through, so the pass went on and read what
				// was sent. Nothing was, so it waits.
				waited := false
				for _, w := range res.Waiting {
					waited = waited || w.Reason == "not sent yet"
				}
				if !waited {
					t.Fatalf("the pass stopped before it read what was sent: kept %v, waiting %v", res.Kept, res.Waiting)
				}
			}
			if len(res.Removed) != 0 {
				t.Fatalf("removed %v", res.Removed)
			}
			if !exists(kept) || !exists(scenario.MarkerPath(source, id)) {
				t.Fatal("something was deleted under the source directory")
			}
		})
	}
}

// TestALinkAtRemovedKeepsEverySession. A _removed that is a symbolic link
// leads out of the root. The pass stops before any session reaches step 0,
// so none is left half removed while a person looks at it, and nothing the
// link points at is touched.
func TestALinkAtRemovedKeepsEverySession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a symbolic link needs a privilege on Windows")
	}
	r := newRoot(t)
	b := r.build(load(t, everyKind), "", scenario.Options{})
	r.pipeline()
	outside := t.TempDir()
	precious := filepath.Join(outside, "precious.jsonl")
	if err := os.WriteFile(precious, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(r.dir, storage.RemovedDir)); err != nil {
		t.Fatal(err)
	}
	res := r.remover().Pass(r.input())
	noErrors(t, res)
	if len(res.Removed) != 0 || !keptFor(res, "", "is a symbolic link or not a directory") {
		t.Fatalf("removed %v, kept %v", res.Removed, res.Kept)
	}
	m := marker(t, b)
	if m.State != scenario.MarkerWritten {
		t.Fatalf("the marker says %q; no session may reach step 0 while %s is a link", m.State, storage.RemovedDir)
	}
	for _, f := range m.Files {
		if !exists(filepath.Join(r.source, filepath.FromSlash(f.Path))) {
			t.Fatalf("the source file %s was deleted", f.Path)
		}
	}
	if !exists(precious) || !exists(filepath.Join(r.dir, b.Session)) {
		t.Fatal("the file behind the link, or the session directory, was deleted")
	}
}

// TestALinkOutOfTheSourceStopsTheRemoval. A project directory can be a
// symbolic link to somewhere else. A removal must never delete through it,
// so it stops, and the session is left for a person.
func TestALinkOutOfTheSourceStopsTheRemoval(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a symbolic link needs a privilege on Windows")
	}
	r := newRoot(t)
	b := r.build(load(t, everyKind), "", scenario.Options{})
	r.pipeline()
	// As if a pass had decided, so the checks inside the steps are what
	// meet the link.
	m := marker(t, b)
	m.State = scenario.MarkerRemoving
	if err := scenario.WriteMarker(b.Marker, m); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(r.source, "-Users-dev-scenario")
	outside := filepath.Join(t.TempDir(), "-Users-dev-scenario")
	if err := os.Rename(project, outside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, project); err != nil {
		t.Fatal(err)
	}
	before := tree(t, outside)

	rm := r.remover()
	res := rm.Pass(r.input())
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0].Error(), "the removal stopped") {
		t.Fatalf("errors %v; want the removal to stop", res.Errors)
	}
	if after := tree(t, outside); !equal(before, after) {
		t.Fatalf("the files the link leads to changed:\nbefore %v\nafter  %v", before, after)
	}
	if again := marker(t, b); again.State != scenario.MarkerRemoving {
		t.Fatalf("the marker says %s; a stopped removal leaves it removing", again.State)
	}
	// Said once. The session waits for a person, not for the next pass.
	if res := rm.Pass(r.input()); len(res.Errors) != 0 || len(res.Removed) != 0 {
		t.Fatalf("the next pass gave errors %v and removed %v", res.Errors, res.Removed)
	}
}

// tree lists every file under dir with its content.
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		out[p] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func equal(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestAMarkerARemovalCannotTrustKeepsItsSession. A marker decides what is
// deleted, so one that names a path out of the source directory, or another
// session than its own, keeps its session. A file the build is still writing
// is not a marker at all.
func TestAMarkerARemovalCannotTrustKeepsItsSession(t *testing.T) {
	r := newRoot(t)
	const id, other = "c30736f2-ac0c-4a72-89b1-01a0844af62d", "aaaaaaaa-bbbb-4ccc-8ddd-000000000001"
	dir := filepath.Join(r.source, scenario.MarkerDir)
	write := func(name string, m *scenario.Marker) {
		if err := scenario.WriteMarker(filepath.Join(dir, name), m); err != nil {
			t.Fatal(err)
		}
	}
	file := []scenario.MarkerFile{{Path: "../outside.jsonl", SHA256: strings.Repeat("0", 64)}}
	write(id+".json", &scenario.Marker{Schema: 1, Session: id, Policy: scenario.PolicyImmediately, State: scenario.MarkerWritten, Files: file})
	write(other+".json", &scenario.Marker{Schema: 1, Session: id, Policy: scenario.PolicyImmediately, State: scenario.MarkerWritten, Files: []scenario.MarkerFile{}})
	if err := os.WriteFile(filepath.Join(dir, ".tmp-123456"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A receiver is recorded, so the pass gets as far as the markers.
	state := fmt.Sprintf("schema 1\nupdated_at 2026-01-01T00:00:00Z\nendpoint %s\n", r.endpoint)
	if err := os.WriteFile(filepath.Join(r.dir, otlp.StateFile), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	res := r.remover().Pass(r.input())
	noErrors(t, res)
	if len(res.Removed) != 0 || len(res.Kept) != 2 ||
		!keptFor(res, id, "not inside the source directory") || !keptFor(res, other, "names the session") {
		t.Fatalf("removed %v, kept %v", res.Removed, res.Kept)
	}
}

// TestALandedFileFromAnotherSourceKeepsTheSession. A real session whose id a
// scenario reused could land beside the scenario's files. Every landed file
// has to come from a file the build wrote, or none of the session goes.
func TestALandedFileFromAnotherSourceKeepsTheSession(t *testing.T) {
	r := newRoot(t)
	b := r.build(load(t, everyKind), "", scenario.Options{})
	m := marker(t, b)
	var child string
	for _, f := range m.Files {
		if strings.Contains(f.Path, "/subagents/agent-") && strings.HasSuffix(f.Path, ".jsonl") && !strings.Contains(f.Path, "/workflows/") {
			child = filepath.Join(r.source, filepath.FromSlash(f.Path))
		}
	}
	if child == "" {
		t.Fatal("the scenario wrote no child transcript")
	}
	data, err := os.ReadFile(child)
	if err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(filepath.Dir(child), "agent-a0123456789abcdef.jsonl")
	if err := os.WriteFile(foreign, data, 0o600); err != nil {
		t.Fatal(err)
	}
	r.collect()
	if err := os.Remove(foreign); err != nil {
		t.Fatal(err)
	}
	res := r.remover().Pass(r.input())
	noErrors(t, res)
	// Nothing is sent yet, so the pass stops at the first condition every
	// session shares. Record a receiver, as a push would, and look again.
	if !waitingFor(res, "", "not sent yet") {
		t.Fatalf("waiting %v", res.Waiting)
	}
	state := fmt.Sprintf("schema 1\nupdated_at 2026-01-01T00:00:00Z\nendpoint %s\n", r.endpoint)
	if err := os.WriteFile(filepath.Join(r.dir, otlp.StateFile), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	res = r.remover().Pass(r.input())
	noErrors(t, res)
	if len(res.Removed) != 0 || !keptFor(res, b.Session, "names a source the build did not write") {
		t.Fatalf("removed %v, kept %v, waiting %v", res.Removed, res.Kept, res.Waiting)
	}
}

// TestTheCapLeavesTheRestForTheNextPass. A removal holds the export lock, so
// one pass takes at most MaxPerPass sessions, and the next takes the rest.
func TestTheCapLeavesTheRestForTheNextPass(t *testing.T) {
	r := newRoot(t)
	sc, err := scenario.Load(filepath.Join("..", "..", "..", "tests", "scenarios", "assembly.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	const n = 20
	for i := range n {
		r.build(sc, fmt.Sprintf("aaaaaaaa-bbbb-4ccc-8ddd-%012d", i+1), scenario.Options{At: at.Add(time.Duration(i) * time.Hour)})
	}
	r.pipeline()
	rm := r.remover()
	first := rm.Pass(r.input())
	noErrors(t, first)
	second := rm.Pass(r.input())
	noErrors(t, second)
	if len(first.Removed) != remove.MaxPerPass || len(second.Removed) != n-remove.MaxPerPass {
		t.Fatalf("the passes removed %d and %d, want %d and %d", len(first.Removed), len(second.Removed), remove.MaxPerPass, n-remove.MaxPerPass)
	}
}
