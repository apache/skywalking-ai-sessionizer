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

package chain_test

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
)

// What a scenario cannot express about removing a sent scenario session: a
// removal stopped at every point it reaches, as a crash stops it, the locks
// it waits for, and a build that has not finished. Each test drives the
// real adapters, deriver, parser, pusher and remover over the
// removed-after-sent scenario.

// removalBudget is the round budget removed-after-sent's expectation file
// sets. The chain then holds several rounds, so a round parsed or sent again
// would show.
const removalBudget = 3072

// removalScenario is removed-after-sent: one session with every kind of
// file a removal deletes.
func removalScenario(t *testing.T) *scenario.Scenario {
	t.Helper()
	sc, err := scenario.Load(filepath.Join("..", "scenarios", "removed-after-sent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

// sentRoot is a scenario root and the receiver its pipeline sends to.
type sentRoot struct {
	t        *testing.T
	dir      string
	source   string
	zone     *storage.Zone
	rcv      *otlptest.Receiver
	endpoint string
}

func newSentRoot(t *testing.T) *sentRoot {
	t.Helper()
	// Claude Code's own directory is a place of the test's, so the guard on
	// it compares against something known on every machine.
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rcv, err := otlptest.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rcv.Close)
	o := rcv.Options(otlp.ProtocolGRPC)
	return rootAt(t, t.TempDir(), rcv, otlp.EndpointOf(o.Protocol, o.Endpoint, o.TLS))
}

func rootAt(t *testing.T, dir string, rcv *otlptest.Receiver, endpoint string) *sentRoot {
	return &sentRoot{t: t, dir: dir, source: filepath.Join(dir, "_source"), zone: storage.NewZone(dir), rcv: rcv, endpoint: endpoint}
}

func (r *sentRoot) build(sc *scenario.Scenario, opts scenario.Options) *scenario.Built {
	r.t.Helper()
	opts.At = time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)
	b, err := scenario.Build(sc, scenario.FormatClaudeCode, r.dir, opts)
	if err != nil {
		r.t.Fatal(err)
	}
	return b
}

func (r *sentRoot) changesRoot() string { return filepath.Join(r.source, "plugins", "data") }

// collect lands with new collectors for both adapters, as a process that has
// just started does, and returns how many sources it landed from.
func (r *sentRoot) collect() int {
	r.t.Helper()
	st, err := claudecode.New(r.source, r.zone, 0).CollectAll(nil)
	if err != nil {
		r.t.Fatal(err)
	}
	cs, err := claudecodechanges.New(r.changesRoot(), r.zone, 0).CollectAll(nil)
	if err != nil {
		r.t.Fatal(err)
	}
	if len(st.Errors) > 0 || len(cs.Errors) > 0 {
		r.t.Fatalf("collect: %v %v", st.Errors, cs.Errors)
	}
	return st.SourcesLanded + cs.SourcesLanded
}

// derive runs a new deriver over what is landed, and returns how many
// requests it put in the spool.
func (r *sentRoot) derive() int {
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

// parse writes rounds for every session directory, as a pass of the
// pipeline does, and returns how many it wrote.
func (r *sentRoot) parse() int {
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
			res, err := parse.Session(r.zone, parse.Options{Conversation: it.Name(), Session: it.Name(), MaxRoundBytes: removalBudget})
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

// push sends what is not sent yet, and records the receiver in push.state,
// as the pipeline's pusher does.
func (r *sentRoot) push() {
	r.t.Helper()
	client, err := r.rcv.Client(otlp.ProtocolGRPC)
	if err != nil {
		r.t.Fatal(err)
	}
	defer client.Close()
	p := &otlp.Pusher{Zone: r.zone, Client: client, Endpoint: r.endpoint, Version: "test",
		ServiceName: "Removal Test", InstanceID: "removal-test", Layer: "AI_AGENT"}
	st, err := p.Pass()
	if err != nil {
		r.t.Fatal(err)
	}
	if len(st.Errors) > 0 {
		r.t.Fatalf("push: %v", st.Errors)
	}
}

// pipeline is one pass of asz collect before its removal.
func (r *sentRoot) pipeline() {
	r.t.Helper()
	r.collect()
	r.derive()
	r.parse()
	r.push()
}

func (r *sentRoot) remover() *remove.Remover {
	return &remove.Remover{Zone: r.zone, Source: r.source, Local: claudecode.New(r.source, r.zone, 0),
		Changes: claudecodechanges.New(r.changesRoot(), r.zone, 0), ChangesRoot: r.changesRoot(), Derives: true}
}

func (r *sentRoot) input() remove.Input {
	return remove.Input{Endpoint: r.endpoint, SendLogs: true, SendMetrics: true}
}

// requests counts what reached the receiver since it was last reset.
func (r *sentRoot) requests() int { return len(r.rcv.Requests()) + len(r.rcv.MetricsRequests()) }

// copyOf is a full copy of the root, modes kept, that sends to the same
// receiver. Nothing in a root records where the root is, so a copy is a root
// in its own right. It stands for a fresh root that was built, landed,
// derived, parsed and sent, at a small part of the time that takes.
func (r *sentRoot) copyOf(t *testing.T) *sentRoot {
	t.Helper()
	dir := t.TempDir()
	err := filepath.WalkDir(r.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(r.dir, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		to := filepath.Join(dir, rel)
		if d.IsDir() {
			return os.MkdirAll(to, info.Mode().Perm())
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(to, data, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
	return rootAt(t, dir, r.rcv, r.endpoint)
}

// in is what a build of the root this was copied from reports, with its
// paths moved into this copy.
func (r *sentRoot) in(b *scenario.Built) *scenario.Built {
	moved := *b
	moved.Out, moved.Marker = r.dir, scenario.MarkerPath(r.source, b.Session)
	return &moved
}

// gone checks that nothing of the session is left, and that nothing other
// sessions share went with it.
func (r *sentRoot) gone(b *scenario.Built, m *scenario.Marker) {
	r.t.Helper()
	t, s := r.t, b.Session
	if present(b.Marker) {
		t.Errorf("the marker %s is still there", b.Marker)
	}
	for _, f := range m.Files {
		if present(filepath.Join(r.source, filepath.FromSlash(f.Path))) {
			t.Errorf("the source file %s is still there", f.Path)
		}
		if i := strings.Index(f.Path, "/"+s+"/"); i >= 0 {
			own := filepath.Join(r.source, filepath.FromSlash(f.Path[:i+1+len(s)]))
			if present(own) {
				t.Errorf("the directory %s, named for the session, is still there", own)
			}
			if !present(filepath.Dir(own)) {
				t.Errorf("%s, which other sessions share, was removed", filepath.Dir(own))
			}
		}
	}
	for _, p := range []string{r.zone.SessionDir(s), filepath.Join(r.dir, "_conversations", s)} {
		if present(p) {
			t.Errorf("%s is still there", p)
		}
	}
	for _, name := range spoolOf(t, r.dir, s) {
		t.Errorf("the spool file %s is still there", name)
	}
	for _, state := range []string{filepath.Join(r.dir, otlp.StateFile), filepath.Join(r.dir, storage.SpoolDir, metrics.StateFile)} {
		if data, err := os.ReadFile(state); err == nil && strings.Contains(string(data), s) {
			t.Errorf("%s still names the session", state)
		}
	}
	if items, err := os.ReadDir(filepath.Join(r.dir, storage.RemovedDir)); err == nil && len(items) > 0 {
		t.Errorf("%s still holds %d entries", storage.RemovedDir, len(items))
	}
	project := filepath.Join(r.source, b.Plan.Project)
	for _, p := range []string{
		filepath.Join(project, "not-a-uuid.jsonl"),
		filepath.Join(project, "memory", "notes.md"),
		filepath.Join(r.source, scenario.MarkerDir),
		filepath.Join(r.dir, storage.ScenarioDir),
		filepath.Join(r.source, filepath.FromSlash(scenario.PluginOutputDir)),
	} {
		if !present(p) {
			t.Errorf("%s, which every session shares, was removed", p)
		}
	}
	ids, err := view.New(r.zone, nil).List()
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(ids, s) {
		t.Error("the page still lists the conversation")
	}
}

// evidence lists every landed file, round and spool file a reader of the
// root can see. What sits under _removed is being deleted and no listing
// reads it, so a file a rename moved there is not new evidence.
func evidence(t *testing.T, dir string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "_source" || d.Name() == storage.RemovedDir {
				return filepath.SkipDir
			}
			return nil
		}
		if ext := filepath.Ext(p); ext == ".sd" || ext == ".sf" || ext == ".pb" {
			rel, _ := filepath.Rel(dir, p)
			out[filepath.ToSlash(rel)] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// spoolOf lists the spool files a session owns.
func spoolOf(t *testing.T, dir, session string) []string {
	t.Helper()
	items, err := os.ReadDir(filepath.Join(dir, storage.SpoolDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, it := range items {
		if owner, ok := metrics.SpoolOwner(it.Name()); ok && owner == session {
			out = append(out, it.Name())
		}
	}
	return out
}

// sentPerFile counts, for every file the receiver holds a record of, how
// many records named it.
func sentPerFile(rcv *otlptest.Receiver) map[string]int {
	out := map[string]int{}
	for _, rec := range otlptest.Records(rcv.Requests()) {
		out[otlptest.Attrs(rec.GetAttributes())["asz.file"]]++
	}
	return out
}

// wantPoints is every point a removal of one session reaches, in order: one
// after each source file of step 1 and each spool file of step 4, and the
// places inside the rename and delete of steps 2 and 3.
func wantPoints(session string, files, spool int) []remove.Point {
	p := []remove.Point{{Step: 0, Where: remove.WhereDone, Session: session}}
	for k := 1; k <= files; k++ {
		p = append(p, remove.Point{Step: 1, Where: remove.WhereFile, Session: session, N: k})
	}
	p = append(p, remove.Point{Step: 1, Where: remove.WhereDirs, Session: session})
	for _, step := range []int{2, 3} {
		for _, where := range []string{remove.WhereUnlocked, remove.WhereRenamed, remove.WhereDeleting, remove.WhereDone} {
			p = append(p, remove.Point{Step: step, Where: where, Session: session})
		}
	}
	for k := 1; k <= spool; k++ {
		p = append(p, remove.Point{Step: 4, Where: remove.WhereFile, Session: session, N: k})
	}
	return append(p,
		remove.Point{Step: 4, Where: remove.WhereDone, Session: session},
		remove.Point{Step: 5, Where: remove.WhereDone},
		remove.Point{Step: 6, Where: remove.WhereDone},
		remove.Point{Step: 7, Where: remove.WhereDone, Session: session})
}

func readMarker(t *testing.T, path string) *scenario.Marker {
	t.Helper()
	m, err := scenario.ReadMarker(path)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func noRemovalErrors(t *testing.T, res *remove.Result) {
	t.Helper()
	if len(res.Errors) > 0 {
		t.Fatalf("the removal reported %v", res.Errors)
	}
}

func present(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// TestRemovalFinishesAfterACrash. A removal can stop at any point, as a
// crash would: between the files of steps 1 and 4, and between the rename
// and the delete of steps 2 and 3. For every point, the restarted pipeline
// lands, derives, parses and sends nothing new, and its first pass finishes
// the removal. That is what the order of the steps is for.
func TestRemovalFinishesAfterACrash(t *testing.T) {
	sent := newSentRoot(t)
	built := sent.build(removalScenario(t), scenario.Options{})
	m := readMarker(t, built.Marker)
	sent.pipeline()
	spool := spoolOf(t, sent.dir, built.Session)
	if len(spool) == 0 {
		t.Fatal("the session has no spool file, so step 4 would stop nowhere")
	}

	// Every point one removal reaches, in order.
	var points []remove.Point
	{
		r := sent.copyOf(t)
		rm := r.remover()
		rm.Hook = func(p remove.Point) error { points = append(points, p); return nil }
		res := rm.Pass(r.input())
		noRemovalErrors(t, res)
		if len(res.Removed) != 1 {
			t.Fatalf("a removal that nothing stopped removed %v, kept %v, waiting %v", res.Removed, res.Kept, res.Waiting)
		}
	}
	if want := wantPoints(built.Session, len(m.Files), len(spool)); !slices.Equal(points, want) {
		t.Fatalf("a removal reached the points\n%v\nwant\n%v", points, want)
	}

	errCrash := errors.New("the process stopped here")
	for i, pt := range points {
		name := fmt.Sprintf("%02d-step%d-%s", i, pt.Step, pt.Where)
		if pt.N > 0 {
			name += fmt.Sprintf("-%d", pt.N)
		}
		t.Run(name, func(t *testing.T) {
			r := sent.copyOf(t)
			b := r.in(built)
			before := evidence(t, r.dir)
			rm := r.remover()
			reached := 0
			rm.Hook = func(remove.Point) error {
				if reached++; reached == i+1 {
					return errCrash
				}
				return nil
			}
			if res := rm.Pass(r.input()); len(res.Errors) != 1 || !errors.Is(res.Errors[0], errCrash) {
				t.Fatalf("the stopped removal reported %v", res.Errors)
			}

			// The restart: new collectors, a new deriver, a parse of every
			// session directory, a push and a new remover. The receiver
			// forgets what it holds, and stands for a fresh one. A new
			// receiver would listen on another port, which is another
			// endpoint, and push.state ties what was sent to its endpoint.
			r.rcv.Reset()
			if n := r.collect(); n != 0 {
				t.Errorf("the restart landed %d source(s)", n)
			}
			if n := r.derive(); n != 0 {
				t.Errorf("the restart derived %d request(s)", n)
			}
			if n := r.parse(); n != 0 {
				t.Errorf("the restart wrote %d round(s)", n)
			}
			r.push()
			if n := r.requests(); n != 0 {
				t.Errorf("the restart sent %d request(s)", n)
			}
			for p := range evidence(t, r.dir) {
				if !before[p] {
					t.Errorf("the restart made %s, which was not there before", p)
				}
			}
			res := r.remover().Pass(r.input())
			noRemovalErrors(t, res)
			// Once step 7 has deleted the marker, nothing is left to finish.
			if finishes := pt.Step < 7; finishes != slices.Contains(res.Removed, b.Session) {
				t.Errorf("the restart removed %v", res.Removed)
			}
			r.gone(b, m)
		})
	}
}

// TestRemovalWaitsForTheLocks. Whoever holds the session, its chain or the
// export state is working on it right now. The removal waits and says
// nothing, since the lock is released on its own. The next pass after the
// release removes the session.
func TestRemovalWaitsForTheLocks(t *testing.T) {
	sent := newSentRoot(t)
	built := sent.build(removalScenario(t), scenario.Options{})
	m := readMarker(t, built.Marker)
	sent.pipeline()
	for _, which := range []string{"session", "chain", "export"} {
		t.Run(which, func(t *testing.T) {
			r := sent.copyOf(t)
			b := r.in(built)
			var held *storage.SessionLock
			var err error
			switch which {
			case "session":
				held, err = storage.LockSession(r.zone.SessionDir(b.Session))
			case "chain":
				held, err = storage.LockChain(filepath.Join(r.dir, "_conversations", b.Session))
			case "export":
				held, err = storage.LockExport(r.dir)
			}
			if err != nil {
				t.Fatal(err)
			}
			rm := r.remover()
			res := rm.Pass(r.input())
			noRemovalErrors(t, res)
			if len(res.Removed) != 0 || len(res.Kept) != 0 || len(res.Waiting) == 0 {
				t.Fatalf("while the %s lock was held: removed %v, kept %v, waiting %v; the removal must wait and say nothing", which, res.Removed, res.Kept, res.Waiting)
			}
			if again := readMarker(t, b.Marker); again.State != scenario.MarkerWritten {
				t.Fatalf("the marker says %s while the removal waited for the %s lock", again.State, which)
			}
			if err := held.Unlock(); err != nil {
				t.Fatal(err)
			}
			res = rm.Pass(r.input())
			noRemovalErrors(t, res)
			if len(res.Removed) != 1 {
				t.Fatalf("after the %s lock was released: removed %v, kept %v, waiting %v", which, res.Removed, res.Kept, res.Waiting)
			}
			r.gone(b, m)
		})
	}
}

// TestRemovalWaitsForTheWholeBuild. A build writes a session's files one
// after another, and its marker last. Until the marker is there, what was
// written so far can be landed and sent, and the session must stay. Once the
// build finishes, the rest is landed and sent, nothing is sent twice, and the
// session goes.
func TestRemovalWaitsForTheWholeBuild(t *testing.T) {
	r := newSentRoot(t)
	sc := removalScenario(t)
	// A place early in the session for a build to stop at: after the first
	// call, before the child and the workflow start.
	sc.Steps[1].Checkpoint = "partway"
	early := r.build(sc, scenario.Options{Through: "partway"})
	// As if the build had not reached its last act yet.
	if err := os.Remove(early.Marker); err != nil {
		t.Fatal(err)
	}
	r.pipeline()
	rm := r.remover()
	res := rm.Pass(r.input())
	noRemovalErrors(t, res)
	if len(res.Removed) != 0 || !present(r.zone.SessionDir(early.Session)) {
		t.Fatalf("a session whose build had not finished was removed: %v", res.Removed)
	}
	before := sentPerFile(r.rcv)
	if len(before) == 0 {
		t.Fatal("nothing was sent while the build was running, so the test exercised nothing")
	}

	whole := r.build(sc, scenario.Options{})
	if whole.Session != early.Session {
		t.Fatalf("the whole build wrote session %s, the early one %s", whole.Session, early.Session)
	}
	m := readMarker(t, whole.Marker)
	removed := false
	for range 3 {
		r.pipeline()
		res := rm.Pass(r.input())
		noRemovalErrors(t, res)
		if removed = slices.Contains(res.Removed, whole.Session); removed {
			break
		}
	}
	if !removed {
		t.Fatal("three passes after the build finished did not remove the session")
	}
	after := sentPerFile(r.rcv)
	for file, n := range after {
		if n != 1 {
			t.Errorf("%s was sent %d times", file, n)
		}
	}
	if len(after) <= len(before) {
		t.Fatalf("what the build wrote last was never sent: %d files before it finished, %d after", len(before), len(after))
	}
	r.gone(whole, m)
}
