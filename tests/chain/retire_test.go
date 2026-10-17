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
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp"
	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp/otlptest"
	"github.com/apache/skywalking-ai-sessionizer/internal/metrics"
	"github.com/apache/skywalking-ai-sessionizer/internal/retention"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
)

// retirementOf is a remover whose levels are both past every file a test
// lands, so it retires every session it may, with no skeleton first.
func retirementOf(r *sentRoot) *retention.Remover {
	return &retention.Remover{Zone: r.zone, MetadataOnlyAfter: time.Hour, RetireAfter: time.Hour, Metrics: true, Now: later}
}

// retiredWhole checks that nothing of a retired session is left but its
// marker and the metrics requests named, which wait to be sent: no
// directory of it, here or under _removed; no other request; no line of
// metrics.state naming it; no line of push.state naming it, its owner aside
// while a request waits; and every line of the provider's table that names
// it retired.
func retiredWhole(t *testing.T, z *storage.Zone, session string, waiting ...string) {
	t.Helper()
	root := z.Root()
	if retired, err := storage.Retired(z, session); err != nil || !retired {
		t.Fatalf("%s has no marker: %v", session, err)
	}
	for _, dir := range []string{z.SessionDir(session), filepath.Join(root, "_conversations", session),
		filepath.Join(root, storage.RemovedDir, session), filepath.Join(root, storage.RemovedDir, session+".chain")} {
		if present(dir) {
			t.Fatalf("%s is left of the retired session", dir)
		}
	}
	if left := spoolOf(t, root, session); strings.Join(left, ",") != strings.Join(waiting, ",") {
		t.Fatalf("the spool holds %v of the retired session, want %v", left, waiting)
	}
	if data, err := os.ReadFile(filepath.Join(root, storage.SpoolDir, metrics.StateFile)); err == nil && bytes.Contains(data, []byte(session)) {
		t.Fatalf("metrics.state still names the retired session:\n%s", data)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	sent, err := otlp.LoadSent(root)
	if err != nil {
		t.Fatal(err)
	}
	if owner := sent.Owner(session); owner != "" && len(waiting) == 0 {
		t.Fatalf("push.state keeps the owner %q with no request waiting", owner)
	}
	data, err := os.ReadFile(filepath.Join(root, otlp.StateFile))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		ownerKept := len(waiting) > 0 && strings.HasPrefix(line, "owner "+session+" ")
		if strings.Contains(line, session) && !ownerKept {
			t.Fatalf("push.state still names the retired session: %q", line)
		}
	}
	data, err = os.ReadFile(z.ProviderStatePath())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Split(line, "\t"); len(f) == 10 && f[5] == session && f[4] != "retired" {
			t.Fatalf("the provider's table keeps a line of the retired session that is not retired: %q", line)
		}
	}
}

// noRecordOf fails when the receiver holds a log record of the session.
func noRecordOf(t *testing.T, rcv *otlptest.Receiver, session string) {
	t.Helper()
	for _, rec := range otlptest.Records(rcv.Requests()) {
		if attrs := otlptest.Attrs(rec.Attributes); attrs["asz.session"] == session {
			t.Fatalf("%s of the retired session was sent", attrs["asz.file"])
		}
	}
}

// TestEveryScenarioRetiredWritesNothingAgain. Every scenario is collected,
// derived, parsed and sent through its checkpoints, and then retired whole:
// nothing of any of its sessions is left but the marker. Its sources are
// then offered again with their cursors gone, which a collector that did not
// ask would land again from the start, and a second pass of every writer
// writes nothing of it and sends nothing of it.
func TestEveryScenarioRetiredWritesNothingAgain(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "scenarios", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	ran := 0
	for _, path := range paths {
		if strings.HasSuffix(path, ".expect.yaml") {
			continue
		}
		name := strings.TrimSuffix(filepath.Base(path), ".yaml")
		sc, err := scenario.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Run(name, func(t *testing.T) {
			r := newSentRoot(t)
			r.maxDelta = budgetOf(t, name)
			k := 0
			for _, cp := range append(sc.Checkpoints(), "") {
				r.build(sc, scenario.Options{Through: cp})
				r.pipelineAt(landedAt(k))
				k++
			}
			sessions := sessionsOf(t, r.dir)
			if len(sessions) == 0 {
				t.Skip("the scenario lands no session")
			}
			res := retirementOf(r).Pass(retention.Input{})
			if len(res.Errors) > 0 {
				t.Fatal(res.Errors)
			}
			if strings.Join(res.Retired, ",") != strings.Join(sessions, ",") {
				t.Fatalf("retired %v, want every session %v", res.Retired, sessions)
			}
			for _, s := range sessions {
				retiredWhole(t, r.zone, s)
			}

			r.rcv.Reset()
			r.pipelineAt(landedAt(k))
			for _, rem := range []*retention.Remover{retirementOf(r), {Zone: r.zone}} {
				if res := rem.Recover(); len(res.Errors) > 0 || len(res.Retired) > 0 {
					t.Fatalf("a recovery of finished retirements did something: %+v", res)
				}
			}
			for _, s := range sessions {
				retiredWhole(t, r.zone, s)
				noRecordOf(t, r.rcv, s)
			}
			if n := len(r.rcv.MetricsRequests()); n != 0 {
				t.Fatalf("%d metrics requests were sent after the retirement", n)
			}
		})
		ran++
	}
	if ran == 0 {
		t.Fatal("no scenario ran")
	}
}

// sessionFiles lists the session's landed files and skeletons by name.
func sessionFiles(t *testing.T, z *storage.Zone, session string) (landed, skeletons int) {
	t.Helper()
	files, err := storage.LandedFiles(z, session)
	if err != nil {
		t.Fatal(err)
	}
	for _, lf := range files {
		if lf.Skeleton {
			skeletons++
		} else {
			landed++
		}
	}
	return landed, skeletons
}

// TestRetirementFollowsTheNewestLandingTime. Idle is read from the header of
// the session's newest landed file, by asz's clock, whatever the record
// times, the file times and a skeleton's removal time say, and whether its
// sources are there or not.
//   - Idle for level 1 and not for level 2: only the metadata is kept.
//   - Then idle for level 2: the session, all skeletons now, is retired. Its
//     skeletons were written later than its newest landing, and that changes
//     nothing.
//   - Idle for both on its first pass, the levels apart or equal: it is
//     retired with no skeleton written first.
func TestRetirementFollowsTheNewestLandingTime(t *testing.T) {
	newest := landedAt(1)
	idle := func(d time.Duration) func() time.Time { return func() time.Time { return newest.Add(d) } }
	growthRoot := func(t *testing.T) (*sentRoot, string) {
		r := newSentRoot(t)
		b := growthAt(t, r, 2, func(k int) { r.pipelineAt(landedAt(k)) })
		// The record times are a month before the landing, and the file
		// times are years before it.
		files, err := storage.LandedFiles(r.zone, b.Session)
		if err != nil {
			t.Fatal(err)
		}
		old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
		for _, lf := range files {
			if err := os.Chtimes(lf.Path, old, old); err != nil {
				t.Fatal(err)
			}
		}
		return r, b.Session
	}

	t.Run("level 1, then level 2", func(t *testing.T) {
		r, session := growthRoot(t)
		rem := &retention.Remover{Zone: r.zone, MetadataOnlyAfter: time.Hour, RetireAfter: 2 * time.Hour, Metrics: true}
		rem.Now = idle(2*time.Hour - time.Minute)
		if res := rem.Pass(retention.Input{}); len(res.Errors) > 0 || len(res.Retired) > 0 || res.Files == 0 {
			t.Fatalf("idle for level 1 only: %+v", res)
		}
		if landed, skeletons := sessionFiles(t, r.zone, session); landed != 0 || skeletons == 0 {
			t.Fatalf("%d landed files and %d skeletons after level 1", landed, skeletons)
		}
		rem.Now = idle(2 * time.Hour)
		if res := rem.Pass(retention.Input{}); len(res.Errors) > 0 || strings.Join(res.Retired, ",") != session {
			t.Fatalf("idle for level 2: %+v", res)
		}
		retiredWhole(t, r.zone, session)
	})

	for _, c := range []struct {
		name                string
		metadataOnly, level time.Duration
	}{
		{"apart", time.Hour, 2 * time.Hour},
		{"equal", 2 * time.Hour, 2 * time.Hour},
	} {
		t.Run("both on the first pass, "+c.name, func(t *testing.T) {
			r, session := growthRoot(t)
			if err := os.RemoveAll(r.source); err != nil {
				t.Fatal(err)
			}
			rem := &retention.Remover{Zone: r.zone, MetadataOnlyAfter: c.metadataOnly, RetireAfter: c.level, Metrics: true}
			if c.metadataOnly == c.level {
				// Equal levels do nothing a second short of either.
				rem.Now = idle(c.level - time.Second)
				if res := rem.Pass(retention.Input{}); len(res.Errors) > 0 || len(res.Retired) > 0 || res.Files != 0 {
					t.Fatalf("a second short of both levels: %+v", res)
				}
			}
			skeletonsAtRetirement := -1
			rem.Stop = func(p retention.Point) error {
				if p.Phase == retention.PhaseRetireMarked {
					_, skeletonsAtRetirement = sessionFiles(t, r.zone, session)
				}
				return nil
			}
			rem.Now = idle(c.level)
			res := rem.Pass(retention.Input{})
			if len(res.Errors) > 0 || strings.Join(res.Retired, ",") != session || res.Files != 0 {
				t.Fatalf("idle for both: %+v", res)
			}
			if skeletonsAtRetirement != 0 {
				t.Fatalf("%d skeletons were written before the retirement", skeletonsAtRetirement)
			}
			retiredWhole(t, r.zone, session)
		})
	}
}

// TestRetirementWaitsForWhatItNeeds. A session idle past retire_after is not
// retired while a landed file is not parsed, its metrics are not derived, a
// removal of its files is in progress, or its parse failed this pass. Level 1
// is not given to it meanwhile either. Once none holds, it is retired.
func TestRetirementWaitsForWhatItNeeds(t *testing.T) {
	waits := []struct {
		name  string
		hold  func(r *sentRoot, session string) retention.Input
		clear func(r *sentRoot, session string)
	}{
		{"not parsed", func(r *sentRoot, _ string) retention.Input {
			r.collectAt(landedAt(2))
			r.derive()
			return retention.Input{}
		}, func(r *sentRoot, _ string) { r.parse() }},
		{"metrics not derived", func(r *sentRoot, _ string) retention.Input {
			r.collectAt(landedAt(2))
			r.parse()
			return retention.Input{}
		}, func(r *sentRoot, _ string) { r.derive() }},
		{"a removal in progress", func(r *sentRoot, session string) retention.Input {
			files, err := storage.LandedFiles(r.zone, session)
			if err != nil {
				r.t.Fatal(err)
			}
			intent := &storage.Removing{Generation: 1, Through: files[len(files)-1].Seq, Phase: storage.RemovingSkeletons}
			if err := storage.SaveRemoving(r.zone.RemovingPath(session), intent); err != nil {
				r.t.Fatal(err)
			}
			return retention.Input{}
		}, func(r *sentRoot, _ string) {
			if res := (&retention.Remover{Zone: r.zone}).Recover(); len(res.Errors) > 0 {
				r.t.Fatal(res.Errors)
			}
		}},
		{"busy", func(_ *sentRoot, session string) retention.Input {
			return retention.Input{Busy: map[string]bool{session: true}}
		}, func(*sentRoot, string) {}},
	}
	for _, w := range waits {
		t.Run(w.name, func(t *testing.T) {
			r := newSentRoot(t)
			sc, err := scenario.Load(filepath.Join("..", "scenarios", "growth.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			cps := append(sc.Checkpoints(), "")
			r.build(sc, scenario.Options{Through: cps[0]})
			r.pipelineAt(landedAt(1))
			b := r.build(sc, scenario.Options{Through: cps[1]})
			in := w.hold(r, b.Session)
			landedBefore, skeletonsBefore := sessionFiles(t, r.zone, b.Session)
			rem := retirementOf(r)
			rem.MetadataOnlyAfter = time.Minute
			if res := rem.Pass(in); len(res.Errors) > 0 || len(res.Retired) > 0 {
				t.Fatalf("it was retired, or failed: %+v", res)
			}
			if retired, _ := storage.Retired(r.zone, b.Session); retired {
				t.Fatal("it has a marker")
			}
			if landed, skeletons := sessionFiles(t, r.zone, b.Session); landed != landedBefore || skeletons != skeletonsBefore {
				t.Fatalf("level 1 ran while it waited to be retired: %d landed and %d skeletons, %d and %d before",
					landed, skeletons, landedBefore, skeletonsBefore)
			}
			w.clear(r, b.Session)
			if res := rem.Pass(retention.Input{}); len(res.Errors) > 0 || strings.Join(res.Retired, ",") != b.Session {
				t.Fatalf("once nothing holds it: %+v", res)
			}
		})
	}
}

// retiringRoot builds a scenario through its checkpoints with a pipeline pass
// after each build, and sends every pass but the last. Its session then holds
// metrics requests the receiver has, and requests it does not. It returns the
// root, the session, and the requests not sent, by name, with their bytes.
func retiringRoot(t *testing.T, name string) (*sentRoot, string, map[string][]byte) {
	t.Helper()
	r := newSentRoot(t)
	sc, err := scenario.Load(filepath.Join("..", "scenarios", name+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	r.maxDelta = budgetOf(t, name)
	cps := append(sc.Checkpoints(), "")
	var b *scenario.Built
	for k, cp := range cps {
		b = r.build(sc, scenario.Options{Through: cp})
		r.collectAt(landedAt(k))
		r.derive()
		r.parse()
		if k < len(cps)-1 {
			r.push()
		}
	}
	sent, err := otlpSent(r)
	if err != nil {
		t.Fatal(err)
	}
	waiting, answered := map[string][]byte{}, 0
	for _, name := range spoolOf(t, r.dir, b.Session) {
		if _, ok := sent.Digest(storage.SpoolDir + "/" + name); ok {
			answered++
			continue
		}
		data, err := os.ReadFile(filepath.Join(r.dir, storage.SpoolDir, name))
		if err != nil {
			t.Fatal(err)
		}
		waiting[name] = data
	}
	if answered == 0 || len(waiting) == 0 {
		t.Fatalf("%d requests sent and %d not; the test needs both", answered, len(waiting))
	}
	return r, b.Session, waiting
}

// names lists a set of requests by name, in order.
func names(requests map[string][]byte) []string {
	out := make([]string, 0, len(requests))
	for name := range requests {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// sameRequests checks the spool holds each request with the bytes given.
func sameRequests(t *testing.T, dir string, requests map[string][]byte) {
	t.Helper()
	for name, want := range requests {
		got, err := os.ReadFile(filepath.Join(dir, storage.SpoolDir, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("the request %s is not kept as it was: %v", name, err)
		}
	}
}

// TestRetirementFinishesAfterACrash. A retirement stopped at each of its
// points, as a crash would stop it, leaves a session nothing writes to: a
// pass of every collector, the deriver and the parser in between changes
// nothing of what is left. The next recovery finishes it, with both levels
// off, and a second recovery has nothing to do. The requests the receiver
// did not have stay, as they were, until they are sent; a recovery after the
// send takes them away, and the owner with them.
func TestRetirementFinishesAfterACrash(t *testing.T) {
	base, session, waiting := retiringRoot(t, "provider-bodies-long")
	const anyDone = -1
	stops := []retention.Point{
		{Phase: retention.PhaseRetireMarked, Done: anyDone},
		{Phase: retention.PhaseRetireHanded, Done: anyDone},
		{Phase: retention.PhaseRetireSession + " renamed", Done: anyDone},
		{Phase: retention.PhaseRetireSession + " deleting", Done: anyDone},
		{Phase: retention.PhaseRetireSession + " removed", Done: anyDone},
		{Phase: retention.PhaseRetireChain + " renamed", Done: anyDone},
		{Phase: retention.PhaseRetireChain + " deleting", Done: anyDone},
		{Phase: retention.PhaseRetireChain + " removed", Done: anyDone},
		{Phase: retention.PhaseRetireTidied, Done: anyDone},
		{Phase: retention.PhaseRetireState, Done: 1},
		{Phase: retention.PhaseRetireState, Done: 2},
		{Phase: retention.PhaseRetireState, Done: 3},
	}
	for _, at := range stops {
		t.Run(fmt.Sprintf("%s %d", at.Phase, at.Done), func(t *testing.T) {
			r := base.copyOf(t)
			rem := retirementOf(r)
			reached := false
			rem.Stop = func(p retention.Point) error {
				if p.Phase == at.Phase && (at.Done == anyDone || p.Done == at.Done) {
					reached = true
					return errCrash
				}
				return nil
			}
			if res := rem.Pass(retention.Input{}); !reached || len(res.Errors) == 0 {
				t.Fatalf("the retirement did not stop there: %+v", res)
			}
			before := treeOf(t, r.zone.SessionDir(session), filepath.Join(r.dir, "_conversations", session))
			r.collectAt(landedAt(10))
			r.derive()
			r.parse()
			if after := treeOf(t, r.zone.SessionDir(session), filepath.Join(r.dir, "_conversations", session)); strings.Join(after, "\n") != strings.Join(before, "\n") {
				t.Fatalf("what is left of the stopped retirement changed:\nbefore:\n%s\nafter:\n%s",
					strings.Join(before, "\n"), strings.Join(after, "\n"))
			}

			if res := (&retention.Remover{Zone: r.zone}).Recover(); len(res.Errors) > 0 {
				t.Fatal(res.Errors)
			}
			rem2 := &retention.Remover{Zone: r.zone}
			for i := 0; i < 2; i++ {
				if res := rem2.Recover(); len(res.Errors) > 0 || len(res.Retired) > 0 {
					t.Fatalf("a recovery after the finished one did something: %+v", res)
				}
			}
			retiredWhole(t, r.zone, session, names(waiting)...)
			sameRequests(t, r.dir, waiting)

			r.rcv.Reset()
			r.push()
			if got := len(r.rcv.MetricsRequests()); got != len(waiting) {
				t.Fatalf("%d metrics requests were sent, want the %d that waited", got, len(waiting))
			}
			noRecordOf(t, r.rcv, session)
			if res := (&retention.Remover{Zone: r.zone}).Recover(); len(res.Errors) > 0 {
				t.Fatal(res.Errors)
			}
			retiredWhole(t, r.zone, session)
		})
	}
}

// failingSync fails the nth sync of a directory, counting from one, and
// syncs every other one.
type failingSync struct {
	dir  string
	nth  int
	seen int
}

func (f *failingSync) sync(dir string) error {
	if dir == f.dir {
		f.seen++
		if f.seen == f.nth {
			return errors.New("the disk did not take it")
		}
	}
	return storage.SyncDir(dir)
}

// TestRetirementStopsWhenItMayNotSurviveAPowerLoss. A retirement syncs the
// marker's directory, then the root, then the spool after each request it
// writes again and the root after them, and the spool after it takes away
// what the receiver has. A sync that fails stops the retirement there, in
// the pass and in the recovery after it, until a sync succeeds. Before the
// directories go, nothing is deleted; after them, the state keeps every line
// of the session. A power loss that took the marker back loses nothing, and
// one that brought a deleted request back sends nothing again.
func TestRetirementStopsWhenItMayNotSurviveAPowerLoss(t *testing.T) {
	base, session, waiting := retiringRoot(t, "provider-bodies-long")
	// nth counts the syncs of the directory in each attempt. A recovery
	// after the directories went writes no request again, so the tidy's sync
	// is its first of the spool.
	for _, c := range []struct {
		name    string
		dir     func(root string) string
		nth     [2]int
		deleted bool
	}{
		{"the marker's directory", func(root string) string { return filepath.Join(root, storage.RetiredDir) }, [2]int{1, 1}, false},
		{"the root after the marker", func(root string) string { return root }, [2]int{1, 1}, false},
		{"the spool after a request", func(root string) string { return filepath.Join(root, storage.SpoolDir) }, [2]int{1, 1}, false},
		{"the root after the requests", func(root string) string { return root }, [2]int{2, 2}, false},
		{"the spool after the tidy", func(root string) string { return filepath.Join(root, storage.SpoolDir) }, [2]int{len(waiting) + 1, 1}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := base.copyOf(t)
			answered := map[string][]byte{}
			for _, name := range spoolOf(t, r.dir, session) {
				if _, ok := waiting[name]; !ok {
					data, err := os.ReadFile(filepath.Join(r.dir, storage.SpoolDir, name))
					if err != nil {
						t.Fatal(err)
					}
					answered[name] = data
				}
			}
			sessionDir := r.zone.SessionDir(session)
			marker := r.zone.RetiredPath(session)
			for attempt := 0; attempt < 2; attempt++ {
				rem := retirementOf(r)
				rem.SyncDir = (&failingSync{dir: c.dir(r.dir), nth: c.nth[attempt]}).sync
				var res retention.Result
				if attempt == 0 {
					res = rem.Pass(retention.Input{})
				} else {
					res = rem.Recover()
				}
				if len(res.Errors) == 0 {
					t.Fatalf("attempt %d: a failed sync did not stop the retirement", attempt+1)
				}
				data, err := os.ReadFile(marker)
				if err != nil {
					t.Fatalf("the marker is not there, so a writer would not pass the session over: %v", err)
				}
				// Every attempt that deletes anything writes the marker
				// again: a sync that succeeds after one that failed does
				// not prove the first one's data reached the disk.
				if !c.deleted {
					if attempt == 0 {
						if err := os.WriteFile(marker, []byte("data not saved"), 0o644); err != nil {
							t.Fatal(err)
						}
					} else if len(data) != 0 {
						t.Fatal("the retry did not write the marker again")
					}
				}
				if present(sessionDir) == c.deleted {
					t.Fatalf("attempt %d: the session's directory is there: %v; want deleted %v", attempt+1, present(sessionDir), c.deleted)
				}
				if c.deleted {
					// Every line stays, the owner too, until the deletions
					// are durable.
					sent, err := otlpSent(r)
					if err != nil {
						t.Fatal(err)
					}
					if sent.Owner(session) == "" {
						t.Fatal("the owner went before the tidy was durable")
					}
					for name := range answered {
						if _, ok := sent.Digest(storage.SpoolDir + "/" + name); !ok {
							t.Fatalf("the line of %s went before its deletion was durable", name)
						}
					}
				}
			}
			if c.deleted {
				// A power loss brings back a deleted request. It is not sent
				// again, and the next recovery takes it away again.
				for name, data := range answered {
					if err := os.WriteFile(filepath.Join(r.dir, storage.SpoolDir, name), data, 0o444); err != nil {
						t.Fatal(err)
					}
				}
				r.rcv.Reset()
				r.push()
				if got := len(r.rcv.MetricsRequests()); got != len(waiting) {
					t.Fatalf("%d metrics requests were sent, want only the %d that waited", got, len(waiting))
				}
			} else {
				// A power loss takes the marker back. Nothing was deleted,
				// so nothing is lost, and the session is retired again.
				if err := os.Remove(marker); err != nil {
					t.Fatal(err)
				}
				if res := retirementOf(r).Pass(retention.Input{}); len(res.Errors) > 0 || strings.Join(res.Retired, ",") != session {
					t.Fatalf("the session was not retired again: %+v", res)
				}
				if _, err := os.Stat(marker); err != nil {
					t.Fatalf("the marker was not written again: %v", err)
				}
				retiredWhole(t, r.zone, session, names(waiting)...)
				return
			}
			if res := (&retention.Remover{Zone: r.zone}).Recover(); len(res.Errors) > 0 {
				t.Fatal(res.Errors)
			}
			retiredWhole(t, r.zone, session)
		})
	}
}

// TestARetiredRequestIsWrittenAgainFromItsReceipt. A request the receiver
// does not have is written again from its receipt before the receipt goes.
// A failed writeback may have lost the request's data on disk. The test
// gives it different bytes and makes it read-only again. Retirement must
// restore the receipt's bytes and keep the request read-only.
// A retirement may also stop after deleting a request but before writing
// it again. Recovery must write it again from the receipt.
func TestARetiredRequestIsWrittenAgainFromItsReceipt(t *testing.T) {
	base, session, waiting := retiringRoot(t, "provider-bodies-long")
	t.Run("replaced", func(t *testing.T) {
		r := base.copyOf(t)
		for name := range waiting {
			path := filepath.Join(r.dir, storage.SpoolDir, name)
			if err := os.Chmod(path, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("data not saved"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, storage.PermLanded); err != nil {
				t.Fatal(err)
			}
		}
		if res := retirementOf(r).Pass(retention.Input{}); len(res.Errors) > 0 {
			t.Fatal(res.Errors)
		}
		for name := range waiting {
			after, err := os.Stat(filepath.Join(r.dir, storage.SpoolDir, name))
			if err != nil {
				t.Fatal(err)
			}
			if after.Mode().Perm() != storage.PermLanded {
				t.Fatalf("%s is %v, want read-only", name, after.Mode().Perm())
			}
		}
		sameRequests(t, r.dir, waiting)
	})
	t.Run("deleted before its write", func(t *testing.T) {
		r := base.copyOf(t)
		rem := retirementOf(r)
		rem.Stop = func(p retention.Point) error {
			if p.Phase == retention.PhaseRetireMarked {
				return errCrash
			}
			return nil
		}
		if res := rem.Pass(retention.Input{}); len(res.Errors) == 0 {
			t.Fatal("the retirement did not stop")
		}
		for name := range waiting {
			if err := storage.DeleteTree(filepath.Join(r.dir, storage.SpoolDir, name), nil); err != nil {
				t.Fatal(err)
			}
		}
		// One remover for every recovery, as a pipeline keeps one: the
		// requests the recovery wrote again are waiting, so the retirement
		// is not taken for finished.
		rec := &retention.Remover{Zone: r.zone}
		if res := rec.Recover(); len(res.Errors) > 0 {
			t.Fatal(res.Errors)
		}
		retiredWhole(t, r.zone, session, names(waiting)...)
		sameRequests(t, r.dir, waiting)
		r.push()
		if res := rec.Recover(); len(res.Errors) > 0 {
			t.Fatal(res.Errors)
		}
		retiredWhole(t, r.zone, session)
	})
}

// TestAHandOffReadsEveryReceipt. A receipt that is empty is a reservation a
// crash left, which recorded nothing, and the hand-off passes it over. A
// receipt that cannot be read stops the retirement before anything is
// deleted, since the request it makes could otherwise be lost.
func TestAHandOffReadsEveryReceipt(t *testing.T) {
	base, session, waiting := retiringRoot(t, "provider-bodies-long")
	receipt := func(r *sentRoot, name string) string {
		return filepath.Join(metrics.ReceiptsDir(r.zone, session), name)
	}
	t.Run("a reservation", func(t *testing.T) {
		r := base.copyOf(t)
		if err := os.WriteFile(receipt(r, "999999.json"), nil, 0o444); err != nil {
			t.Fatal(err)
		}
		if res := retirementOf(r).Pass(retention.Input{}); len(res.Errors) > 0 || len(res.Retired) != 1 {
			t.Fatalf("%+v", res)
		}
		retiredWhole(t, r.zone, session, names(waiting)...)
	})
	t.Run("unreadable", func(t *testing.T) {
		r := base.copyOf(t)
		if err := os.WriteFile(receipt(r, "999999.json"), []byte(`{"schema":2,"request":"AA=="}`), 0o444); err != nil {
			t.Fatal(err)
		}
		if res := retirementOf(r).Pass(retention.Input{}); len(res.Errors) == 0 || len(res.Retired) != 0 {
			t.Fatalf("a receipt that cannot be read did not stop the retirement: %+v", res)
		}
		if !present(r.zone.SessionDir(session)) {
			t.Fatal("the session's directory went with a receipt that could not be read")
		}
		if left := spoolOf(t, r.dir, session); len(left) <= len(waiting) {
			// The requests sent are still there: nothing was tidied.
			t.Fatalf("the spool holds %v; the stopped retirement deleted from it", left)
		}
	})
}

// TestARetiredSessionIsNotGivenLevelOne. A retirement that stopped after its
// marker leaves the session's files, and a later configuration may give it
// level 1 alone. Level 1 passes a retired session over, so no skeleton is
// written into it: only its retirement goes on with it.
func TestARetiredSessionIsNotGivenLevelOne(t *testing.T) {
	r := newSentRoot(t)
	b := growthAt(t, r, 2, func(k int) { r.pipelineAt(landedAt(k)) })
	rem := retirementOf(r)
	rem.Stop = func(p retention.Point) error {
		if p.Phase == retention.PhaseRetireMarked {
			return errCrash
		}
		return nil
	}
	if res := rem.Pass(retention.Input{}); len(res.Errors) == 0 {
		t.Fatal("the retirement did not stop")
	}
	before := treeOf(t, r.zone.SessionDir(b.Session))
	levelOne := &retention.Remover{Zone: r.zone, MetadataOnlyAfter: time.Hour, Metrics: true, Now: later}
	if res := levelOne.Pass(retention.Input{}); len(res.Errors) > 0 || res.Files != 0 {
		t.Fatalf("level 1 over a retired session: %+v", res)
	}
	if after := treeOf(t, r.zone.SessionDir(b.Session)); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatalf("level 1 wrote into a retired session:\nbefore:\n%s\nafter:\n%s", strings.Join(before, "\n"), strings.Join(after, "\n"))
	}
}

// pushAs sends what is not sent yet under the service given, and returns
// what the pass did.
func (r *sentRoot) pushAs(service string) *otlp.Stats {
	r.t.Helper()
	client, err := r.rcv.Client(otlp.ProtocolGRPC)
	if err != nil {
		r.t.Fatal(err)
	}
	defer client.Close()
	p := &otlp.Pusher{Zone: r.zone, Client: client, Endpoint: r.endpoint, Version: "test",
		ServiceName: service, InstanceID: "removal-test", Layer: "AI_AGENT"}
	st, err := p.Pass()
	if err != nil {
		r.t.Fatal(err)
	}
	return st
}

// TestARetiredSessionsRequestsGoToItsOwner. The metrics requests of a
// retired session stay until they are sent, and go to its owner only. The
// owner stays in push.state while any of them waits.
//   - Never sent before: no owner is kept, and the first send pins one.
//   - Sent before: a pipeline that names another owner holds them; once it
//     names the same one again, they go.
//   - A request the receiver took and rejected part of is not sent again,
//     and goes.
//   - A send whose push.state save failed leaves the requests waiting, with
//     their owner. They are sent again, at least once, and then go.
func TestARetiredSessionsRequestsGoToItsOwner(t *testing.T) {
	t.Run("never sent", func(t *testing.T) {
		r := newSentRoot(t)
		b := growthAt(t, r, 2, func(k int) {
			r.collectAt(landedAt(k))
			r.derive()
			r.parse()
		})
		requests := spoolOf(t, r.dir, b.Session)
		if len(requests) < 2 {
			t.Fatalf("%d requests; the test needs several", len(requests))
		}
		if res := retirementOf(r).Pass(retention.Input{}); len(res.Errors) > 0 || len(res.Retired) != 1 {
			t.Fatalf("%+v", res)
		}
		retiredWhole(t, r.zone, b.Session, requests...)
		if sent, err := otlpSent(r); err != nil || sent.Owner(b.Session) != "" {
			t.Fatalf("a session never sent has an owner: %v", err)
		}
		r.rcv.Reset()
		r.push()
		if got := len(r.rcv.MetricsRequests()); got != len(requests) {
			t.Fatalf("%d requests sent, want %d", got, len(requests))
		}
		if sent, err := otlpSent(r); err != nil || sent.Owner(b.Session) == "" {
			t.Fatalf("the first send pinned no owner: %v", err)
		}
		if res := (&retention.Remover{Zone: r.zone}).Recover(); len(res.Errors) > 0 {
			t.Fatal(res.Errors)
		}
		retiredWhole(t, r.zone, b.Session)
	})
	t.Run("another owner", func(t *testing.T) {
		r, session, waiting := retiringRoot(t, "provider-bodies-long")
		sent, err := otlpSent(r)
		if err != nil {
			t.Fatal(err)
		}
		owner := sent.Owner(session)
		if res := retirementOf(r).Pass(retention.Input{}); len(res.Errors) > 0 {
			t.Fatal(res.Errors)
		}
		r.rcv.Reset()
		if st := r.pushAs("Another Service"); len(st.Held) != 1 || st.Held[0].Session != session {
			t.Fatalf("another owner did not hold the session: %+v", st)
		}
		if got := len(r.rcv.MetricsRequests()); got != 0 {
			t.Fatalf("%d requests went to another owner", got)
		}
		// One remover for every recovery, as a pipeline keeps one: a
		// retirement with requests waiting is not taken for finished.
		rec := &retention.Remover{Zone: r.zone}
		if res := rec.Recover(); len(res.Errors) > 0 {
			t.Fatal(res.Errors)
		}
		retiredWhole(t, r.zone, session, names(waiting)...)
		if sent, err := otlpSent(r); err != nil || sent.Owner(session) != owner {
			t.Fatalf("the owner is %q, want %q kept: %v", sent.Owner(session), owner, err)
		}
		r.push()
		if got := len(r.rcv.MetricsRequests()); got != len(waiting) {
			t.Fatalf("%d requests sent under the owner, want %d", got, len(waiting))
		}
		if res := rec.Recover(); len(res.Errors) > 0 {
			t.Fatal(res.Errors)
		}
		retiredWhole(t, r.zone, session)
	})
	// A send can succeed while saving push.state fails. Recovery must keep
	// the requests and their owner until a later send is saved.
	t.Run("state save failed after send", func(t *testing.T) {
		r, session, waiting := retiringRoot(t, "provider-bodies-long")
		if res := retirementOf(r).Pass(retention.Input{}); len(res.Errors) > 0 {
			t.Fatal(res.Errors)
		}
		retiredWhole(t, r.zone, session, names(waiting)...)
		sent, err := otlpSent(r)
		if err != nil {
			t.Fatal(err)
		}
		owner := sent.Owner(session)
		if owner == "" {
			t.Fatal("the retirement left no owner for the waiting requests")
		}
		state := filepath.Join(r.dir, otlp.StateFile)
		saved, err := os.ReadFile(state)
		if err != nil {
			t.Fatal(err)
		}
		r.rcv.Reset()
		r.push()
		if got := len(r.rcv.MetricsRequests()); got != len(waiting) {
			t.Fatalf("%d requests reached the receiver, want %d", got, len(waiting))
		}
		if err := os.WriteFile(state, saved, storage.PermState); err != nil {
			t.Fatal(err)
		}
		rec := &retention.Remover{Zone: r.zone}
		if res := rec.Recover(); len(res.Errors) > 0 {
			t.Fatal(res.Errors)
		}
		retiredWhole(t, r.zone, session, names(waiting)...)
		if sent, err := otlpSent(r); err != nil {
			t.Fatal(err)
		} else if sent.Owner(session) != owner {
			t.Fatalf("the owner is %q, want %q kept after the failed save", sent.Owner(session), owner)
		}
		r.push()
		if got := len(r.rcv.MetricsRequests()); got != 2*len(waiting) {
			t.Fatalf("%d requests reached the receiver, want %d after sending them again", got, 2*len(waiting))
		}
		if res := rec.Recover(); len(res.Errors) > 0 {
			t.Fatal(res.Errors)
		}
		retiredWhole(t, r.zone, session)
	})
	t.Run("rejected in part", func(t *testing.T) {
		r, session, waiting := retiringRoot(t, "provider-bodies-long")
		if res := retirementOf(r).Pass(retention.Input{}); len(res.Errors) > 0 {
			t.Fatal(res.Errors)
		}
		r.rcv.Reject(1)
		st := r.pushAs("Removal Test")
		r.rcv.Reject(0)
		if st.Rejected == 0 {
			t.Fatalf("the receiver rejected nothing: %+v", st)
		}
		if res := (&retention.Remover{Zone: r.zone}).Recover(); len(res.Errors) > 0 {
			t.Fatal(res.Errors)
		}
		retiredWhole(t, r.zone, session)
		r.rcv.Reset()
		r.push()
		if got := len(r.rcv.MetricsRequests()); got != 0 {
			t.Fatalf("%d requests were sent again, of %d", got, len(waiting))
		}
	})
}

// TestThePageDuringARetirement. The page drew a conversation and keeps it.
// While its retirement is unfinished, the page lists and draws it as long as
// its chain is there, and once the chain is gone it does neither. No read of
// the page, by its kept paths or by new ones, makes anything of the session
// again, at any point of the retirement and after it.
func TestThePageDuringARetirement(t *testing.T) {
	base := newSentRoot(t)
	b := growthAt(t, base, 2, func(k int) { base.pipelineAt(landedAt(k)) })
	id := b.Session
	get := func(h http.Handler, path string) (int, string) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code, rec.Body.String()
	}
	paths := []string{"/api/c/" + id + "/view", "/api/c/" + id + "/record/1/1", "/api/c/" + id + "/files?seq=1"}
	for _, at := range []string{retention.PhaseRetireMarked, retention.PhaseRetireSession + " renamed",
		retention.PhaseRetireSession + " removed", retention.PhaseRetireChain + " renamed", ""} {
		name := at
		if name == "" {
			name = "finished"
		}
		t.Run(name, func(t *testing.T) {
			r := base.copyOf(t)
			h := view.New(r.zone, nil).Handler()
			for _, path := range paths[:2] {
				if code, body := get(h, path); code != http.StatusOK {
					t.Fatalf("%s before the retirement: %d %s", path, code, body)
				}
			}
			get(h, paths[2])
			rem := retirementOf(r)
			rem.Stop = func(p retention.Point) error {
				if p.Phase == at {
					return errCrash
				}
				return nil
			}
			rem.Pass(retention.Input{})
			sessionDir, chainDir := r.zone.SessionDir(id), filepath.Join(r.dir, "_conversations", id)
			before := treeOf(t, sessionDir, chainDir, filepath.Join(r.dir, storage.RemovedDir))
			chain := present(chainDir)
			_, list := get(h, "/api/conversations")
			if listed := strings.Contains(list, `"id":"`+id+`"`); listed != chain {
				t.Fatalf("the list names it: %v; its chain is there: %v", listed, chain)
			}
			if code, body := get(h, paths[0]); (code == http.StatusOK) != chain {
				t.Fatalf("the conversation answers %d with its chain there: %v: %s", code, chain, body)
			}
			for _, path := range paths[1:] {
				get(h, path)
			}
			fresh := view.New(r.zone, nil).Handler()
			for _, path := range paths {
				get(fresh, path)
			}
			if after := treeOf(t, sessionDir, chainDir, filepath.Join(r.dir, storage.RemovedDir)); strings.Join(after, "\n") != strings.Join(before, "\n") {
				t.Fatalf("a read of the page changed the root:\nbefore:\n%s\nafter:\n%s", strings.Join(before, "\n"), strings.Join(after, "\n"))
			}
		})
	}
}

// copyDir copies a directory tree, modes kept.
func copyDir(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, info.Mode().Perm()|0o700)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, data, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestRetirementForgetsNothingBeforeItsDirectoriesAreDurable. Each rename
// checks both directory syncs before deleting what it moved. The retirement
// checks the directories again before the state forgets anything. A failure
// at either check leaves push.state unchanged, including the owner. A retry
// whose final directory check fails must keep that state too. A power loss
// that brings back the directories, receipts and answered requests must not
// send those requests again. Recovery finishes the retirement.
func TestRetirementForgetsNothingBeforeItsDirectoriesAreDurable(t *testing.T) {
	base, session, waiting := retiringRoot(t, "provider-bodies-long")
	// The first attempt syncs the root for the marker, the requests, the
	// session rename and the final check, in that order. It syncs the
	// conversations directory for the chain rename and the final check.
	// It syncs _removed for each rename, then for the final check.
	// Each retry fails the final check of the same directory. A failed
	// rename sync leaves directories to remove, so the retry writes the
	// marker again and may still rename the chain. A failed final check
	// leaves no directories or answered requests, so only the checks remain.
	const (
		handed      = retention.PhaseRetireHanded
		sessionGone = retention.PhaseRetireSession + " removed"
		tidied      = retention.PhaseRetireTidied
	)
	for _, c := range []struct {
		name  string
		dir   func(root string) string
		nth   [2]int
		after [2]string
	}{
		{"the root after the session rename", func(root string) string { return root }, [2]int{3, 2}, [2]string{handed, tidied}},
		{"the removed directory after the session rename", func(root string) string { return filepath.Join(root, storage.RemovedDir) }, [2]int{1, 2}, [2]string{handed, tidied}},
		{"the conversations after the chain rename", func(root string) string { return filepath.Join(root, "_conversations") }, [2]int{1, 1}, [2]string{sessionGone, tidied}},
		{"the removed directory after the chain rename", func(root string) string { return filepath.Join(root, storage.RemovedDir) }, [2]int{2, 1}, [2]string{sessionGone, tidied}},
		{"the root before forgetting", func(root string) string { return root }, [2]int{4, 1}, [2]string{tidied, ""}},
		{"the conversations before forgetting", func(root string) string { return filepath.Join(root, "_conversations") }, [2]int{2, 1}, [2]string{tidied, ""}},
		{"the removed directory before forgetting", func(root string) string { return filepath.Join(root, storage.RemovedDir) }, [2]int{3, 1}, [2]string{tidied, ""}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := base.copyOf(t)
			stateBefore, err := os.ReadFile(filepath.Join(r.dir, otlp.StateFile))
			if err != nil {
				t.Fatal(err)
			}
			answered := map[string][]byte{}
			for _, name := range spoolOf(t, r.dir, session) {
				if _, ok := waiting[name]; !ok {
					data, err := os.ReadFile(filepath.Join(r.dir, storage.SpoolDir, name))
					if err != nil {
						t.Fatal(err)
					}
					answered[name] = data
				}
			}
			for attempt := 0; attempt < 2; attempt++ {
				rem := retirementOf(r)
				failure := &failingSync{dir: c.dir(r.dir), nth: c.nth[attempt]}
				rem.SyncDir = failure.sync
				last := ""
				rem.Stop = func(p retention.Point) error {
					last = p.Phase
					return nil
				}
				var res retention.Result
				if attempt == 0 {
					res = rem.Pass(retention.Input{})
				} else {
					res = rem.Recover()
				}
				if failure.seen != failure.nth || len(res.Errors) != 1 || !strings.Contains(res.Errors[0].Error(), "the disk did not take it") {
					t.Fatalf("attempt %d: sync %d of %d did not stop the retirement: %v", attempt+1, failure.seen, failure.nth, res.Errors)
				}
				if last != c.after[attempt] {
					t.Fatalf("attempt %d: the sync failed after %q, want after %q", attempt+1, last, c.after[attempt])
				}
				if present(r.zone.SessionDir(session)) {
					t.Fatalf("attempt %d: the session's directory is still there", attempt+1)
				}
				stateAfter, err := os.ReadFile(filepath.Join(r.dir, otlp.StateFile))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(stateAfter, stateBefore) {
					t.Fatalf("attempt %d: push.state changed before the directories were durable", attempt+1)
				}
			}
			restoreAfterPowerLoss(t, base, r, session, answered)
			if res := (&retention.Remover{Zone: r.zone}).Recover(); len(res.Errors) > 0 {
				t.Fatal(res.Errors)
			}
			retiredWhole(t, r.zone, session, names(waiting)...)
			r.rcv.Reset()
			r.push()
			if got := len(r.rcv.MetricsRequests()); got != len(waiting) {
				t.Fatalf("%d metrics requests were sent, want only the %d that waited", got, len(waiting))
			}
			noRecordOf(t, r.rcv, session)
			if res := (&retention.Remover{Zone: r.zone}).Recover(); len(res.Errors) > 0 {
				t.Fatal(res.Errors)
			}
			retiredWhole(t, r.zone, session)
		})
	}
}

// restoreAfterPowerLoss puts back what a power loss would bring back when
// the renames and deletions of a retirement did not reach the disk: the
// session's directory and its conversation's, as they were before, and the
// requests the receiver has. A lost rename leaves only the old name, so its
// destination under _removed must go before the old directory is restored.
// Anything the retirement did not reach is replaced by the same contents.
func restoreAfterPowerLoss(t *testing.T, base, r *sentRoot, session string, answered map[string][]byte) {
	t.Helper()
	for _, name := range []string{session, session + ".chain"} {
		if err := storage.DeleteTree(filepath.Join(r.dir, storage.RemovedDir, name), nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{session, filepath.Join("_conversations", session)} {
		if err := storage.DeleteTree(filepath.Join(r.dir, dir), nil); err != nil {
			t.Fatal(err)
		}
		copyDir(t, filepath.Join(base.dir, dir), filepath.Join(r.dir, dir))
	}
	for name, data := range answered {
		path := filepath.Join(r.dir, storage.SpoolDir, name)
		if err := storage.DeleteTree(path, nil); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o444); err != nil {
			t.Fatal(err)
		}
	}
}

// TestARetirementStoppedAfterItsDirectoriesIsCounted. A retirement whose
// directories are gone and which then stops, before the spool tidy is done,
// still names the session as retired. No later attempt finds a directory to
// say so, and the pipeline counts the session and drops it from the page's
// memory by that name.
func TestARetirementStoppedAfterItsDirectoriesIsCounted(t *testing.T) {
	r, session, waiting := retiringRoot(t, "provider-bodies-long")
	rem := retirementOf(r)
	rem.Stop = func(p retention.Point) error {
		if p.Phase == retention.PhaseRetireTidied {
			return errCrash
		}
		return nil
	}
	res := rem.Pass(retention.Input{})
	if len(res.Errors) == 0 {
		t.Fatal("the retirement did not stop")
	}
	if strings.Join(res.Retired, ",") != session {
		t.Fatalf("the stopped retirement names %v as retired, want %s", res.Retired, session)
	}
	if res := (&retention.Remover{Zone: r.zone}).Recover(); len(res.Errors) > 0 {
		t.Fatal(res.Errors)
	}
	retiredWhole(t, r.zone, session, names(waiting)...)
}
