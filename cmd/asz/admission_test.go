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

package main

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/judge"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/client"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/pulltest"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario/run"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// pulled is a storage root holding one pulled snapshot of a scenario's
// conversation.
type pulled struct {
	root, snapshot, conversation, session string
}

func pulledSnapshot(t *testing.T) pulled {
	t.Helper()
	out := t.TempDir()
	rep, err := run.Check(filepath.Join("..", "..", "tests", "scenarios", "three-rounds.yaml"),
		run.Options{Out: out, Formats: []scenario.Format{scenario.FormatClaudeCode}})
	if err != nil || rep.Failed {
		t.Fatalf("building the source: %v %v", err, rep.Lines)
	}
	src := filepath.Join(out, string(scenario.FormatClaudeCode))
	entries, err := os.ReadDir(filepath.Join(src, "_conversations"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("no conversation: %v", err)
	}
	conversation := entries[0].Name()
	m, err := pulltest.Manifest(src, conversation)
	if err != nil {
		t.Fatal(err)
	}
	local := &client.Local{Root: t.TempDir(), TTL: 720 * time.Hour}
	res, err := local.Pull(context.Background(), "prod", conversation, m, &pulltest.Source{Root: src})
	if err != nil {
		t.Fatal(err)
	}
	if err := res.Admission.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	return pulled{root: local.Root, snapshot: filepath.Join(local.Root, storage.RemoteDir, "prod", conversation, res.Tag),
		conversation: conversation, session: m.Session}
}

func (p pulled) leases(t *testing.T) []string {
	t.Helper()
	items, err := os.ReadDir(filepath.Join(p.snapshot, storage.LeasesDir))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, item := range items {
		out = append(out, item.Name())
	}
	return out
}

func rootConfig(root string) *config.Config {
	cfg := config.Default()
	cfg.Storage.Root = root
	return cfg
}

// Every command that writes refuses a pulled snapshot and every other path
// in pull's layout before it writes anything, and leaves the tree as it was.
func TestWritersRefusePulledCopies(t *testing.T) {
	p := pulledSnapshot(t)
	conversation := filepath.Dir(p.snapshot)
	building := filepath.Join(conversation, storage.BuildingDir, "x")
	server := filepath.Join(t.TempDir(), storage.PullDir, "conversations", "c1")
	for _, dir := range []string{building, server} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	before := treeState(t, p.root, storage.RemoteDir)
	for _, root := range []string{p.snapshot, conversation, building, filepath.Join(p.root, storage.RemoteDir), server} {
		for _, cmd := range []string{"sources", "collect", "server", "parse", "push", "repack"} {
			if err := admitCommand(cmd, rootConfig(root)); err == nil {
				t.Errorf("%s over %s was not refused", cmd, root)
			}
		}
		positional = []string{root}
		if err := cmdRepack(rootConfig(t.TempDir()), config.Adapter{}, false); err == nil {
			t.Errorf("repack into %s was not refused", root)
		}
		for _, sub := range []string{"build", "check"} {
			code := 0
			stderrOf(t, func() {
				code = cmdScenario([]string{sub, filepath.Join("..", "..", "tests", "scenarios", "fixture.yaml"), "--format", "sd", "--out", root})
			})
			if code == 0 {
				t.Errorf("scenario %s into %s was not refused", sub, root)
			}
		}
	}
	// A snapshot that lost its marker is damaged, and still refused by where
	// it is: no marker makes a path in the layout an ordinary root.
	if err := os.MkdirAll(filepath.Join(filepath.Dir(p.snapshot), "20261025T103200.000000000Z"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"sources", "collect", "server", "parse", "push", "repack"} {
		if err := admitCommand(cmd, rootConfig(filepath.Join(filepath.Dir(p.snapshot), "20261025T103200.000000000Z"))); err == nil {
			t.Errorf("%s over a snapshot without its marker was not refused", cmd)
		}
	}
	// A root that does not exist yet is held to its path alone.
	if err := admitCommand("collect", rootConfig(filepath.Join(p.root, storage.RemoteDir, "prod", "new"))); err == nil {
		t.Error("a new root inside _remote was not refused")
	}
	if after := treeState(t, p.root, storage.RemoteDir); !maps.Equal(before, after) {
		t.Fatal("a refused command changed the pulled copies")
	}
}

// Every command that reads a pulled snapshot holds a lease while it reads
// and releases it after; past the snapshot's time to live it is refused and
// takes no lease.
func TestReadersHoldALeaseOnASnapshot(t *testing.T) {
	p := pulledSnapshot(t)
	cfg := rootConfig(p.snapshot)
	positional = nil
	if err := admitCommand("verify", cfg); err != nil {
		t.Fatal(err)
	}
	if got := p.leases(t); len(got) != 1 || !strings.HasPrefix(got[0], "read-") {
		t.Fatalf("leases while reading: %v", got)
	}
	out := stdoutOf(t, func() {
		if err := cmdVerify(cfg, config.Adapter{}, false); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "whole evidence") {
		t.Fatalf("verify said: %s", out)
	}
	releaseAdmission()
	if got := p.leases(t); len(got) != 0 {
		t.Fatalf("leases after the command: %v", got)
	}
	for _, cmd := range []string{"index", "conversation", "view", "show"} {
		positional = []string{p.conversation}
		if cmd == "show" || cmd == "index" {
			positional = []string{p.session, "x"}
		}
		if err := admitCommand(cmd, cfg); err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		if got := p.leases(t); len(got) != 1 {
			t.Fatalf("%s: leases while reading: %v", cmd, got)
		}
		releaseAdmission()
	}
	positional = []string{p.conversation}
	if err := admitCommand("conversation", cfg); err != nil {
		t.Fatal(err)
	}
	if out := stdoutOf(t, func() {
		if err := cmdConversation(cfg, config.Adapter{}, false); err != nil {
			t.Fatal(err)
		}
	}); !strings.Contains(out, "conversation "+p.conversation) {
		t.Fatalf("conversation said: %s", out)
	}
	releaseAdmission()
	// measure and judge admit and release by themselves.
	cfgFile := filepath.Join(t.TempDir(), "asz.yaml")
	if err := os.WriteFile(cfgFile, []byte("storage:\n  root: "+p.snapshot+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"measure":     {"-config", cfgFile, p.conversation},
		"judge -list": {"-config", cfgFile, "-list", p.conversation},
	} {
		code := 0
		stdoutOf(t, func() {
			if strings.HasPrefix(name, "measure") {
				code = cmdMeasure(args)
			} else {
				code = cmdJudge(args)
			}
		})
		if code != 0 {
			t.Fatalf("%s: exit %d", name, code)
		}
		if got := p.leases(t); len(got) != 0 {
			t.Fatalf("%s left leases: %v", name, got)
		}
	}
	// Past its time to live a snapshot admits no reader.
	if err := os.WriteFile(cfgFile, []byte("storage:\n  root: "+p.snapshot+"\nremote_snapshot_ttl: 2s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	for name, run := range map[string]func() int{
		"measure":     func() int { return cmdMeasure([]string{"-config", cfgFile, p.conversation}) },
		"judge -list": func() int { return cmdJudge([]string{"-config", cfgFile, "-list", p.conversation}) },
	} {
		code := 0
		errText := stderrOf(t, func() { code = run() })
		if code == 0 || !strings.Contains(errText, "expired") {
			t.Fatalf("%s over an expired snapshot: exit %d, %s", name, code, errText)
		}
	}
	if got := p.leases(t); len(got) != 0 {
		t.Fatalf("a refused reader took a lease: %v", got)
	}
}

// A reader pointed anywhere else in pull's layout is refused: the outer
// conversation directory, a private build, _remote itself and a server's
// tree are never readable roots.
func TestReadersRefuseTheRestOfTheLayout(t *testing.T) {
	p := pulledSnapshot(t)
	conversation := filepath.Dir(p.snapshot)
	building := filepath.Join(conversation, storage.BuildingDir, "x")
	if err := os.MkdirAll(building, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{conversation, building, filepath.Join(p.root, storage.RemoteDir),
		filepath.Join(p.root, storage.RemoteDir, "prod"), filepath.Join(p.snapshot, storage.LeasesDir)} {
		positional = nil
		for _, cmd := range []string{"index", "verify", "conversation", "view", "show"} {
			if err := admitCommand(cmd, rootConfig(root)); !errors.Is(err, storage.ErrProtected) {
				t.Errorf("%s over %s: %v", cmd, root, err)
			}
		}
	}
	// A copy marker outside _remote is no snapshot, and is read as nothing.
	stray := t.TempDir()
	if err := os.WriteFile(filepath.Join(stray, storage.CopyMarker), []byte("{}"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := admitCommand("index", rootConfig(stray)); err == nil {
		t.Error("a root holding a stray copy marker was read")
	}
}

// Identities a person gives are held to the naming rule before any path is
// made from them, whether or not a chain of that name exists.
func TestIdentitiesAreCheckedBeforeAnyPath(t *testing.T) {
	root := t.TempDir()
	cfgFile := filepath.Join(t.TempDir(), "asz.yaml")
	if err := os.WriteFile(cfgFile, []byte("storage:\n  root: "+root+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../escape", "a/b", "CON", "x:y", "trailing.", ".tmp-x", "a\\b"} {
		for name, run := range map[string]func() int{
			"judge -list": func() int { return cmdJudge([]string{"-config", cfgFile, "-list", bad}) },
			"judge -show": func() int { return cmdJudge([]string{"-config", cfgFile, "-show", "1", bad}) },
			"judge -regroup": func() int {
				return cmdJudge([]string{"-config", cfgFile, "-regroup", "1", "-labels", cfgFile, bad})
			},
			"measure": func() int { return cmdMeasure([]string{"-config", cfgFile, bad}) },
		} {
			code := 0
			text := stderrOf(t, func() { code = run() })
			if code == 0 || !strings.Contains(text, strconv.Quote(bad)) {
				t.Errorf("%s %q: exit %d, %s", name, bad, code, text)
			}
		}
		positional = []string{bad, "x"}
		for _, cmd := range []string{"index", "verify", "show", "conversation"} {
			if err := admitCommand(cmd, rootConfig(root)); err == nil {
				t.Errorf("%s %q was not refused", cmd, bad)
			}
		}
	}
	positional = []string{"_session", "x"}
	if err := admitCommand("show", rootConfig(root)); err == nil {
		t.Error("a session starting with _ was not refused")
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("a refused command made %v", entries)
	}
}

// runLease admits a reader to p's snapshot and binds it to run, whose work
// directory it makes, as a judge run does.
func runLease(t *testing.T, p pulled, run string) (*storage.Admission, string) {
	t.Helper()
	r, err := storage.ClassifyRoot(p.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a, err := storage.AdmitSnapshot(ctx, r, time.Hour*24*365, time.Now, nil)
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(p.snapshot, p.session, storage.JudgmentsDir, run, storage.WorkDir)
	if err := os.MkdirAll(filepath.Join(work, "2-targets", "c001", "a1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := a.BindRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	return a, work
}

// reportFor is the failure report beside log that lease names.
func reportFor(log, lease string) string {
	return filepath.Join(filepath.Dir(log), storage.FailurePrefix+leaseRandom(lease))
}

// A navigation server a run started inherits the run's lease, with no
// configuration, and its log must be the one its run made in that run's
// work directory, its failure report beside it. Anything else, an ended run
// or a crossed one, a missing log, a link or a report elsewhere, is refused
// before the log is opened.
func TestNavigationInheritsTheRunsLease(t *testing.T) {
	p := pulledSnapshot(t)
	ctx := context.Background()
	parent, work := runLease(t, p, "j000001")
	other, otherWork := runLease(t, p, "j000002")
	log := filepath.Join(work, "2-targets", "c001", "a1", "mcp-calls.jsonl")
	otherLog := filepath.Join(otherWork, "2-targets", "c001", "a1", "mcp-calls.jsonl")
	for _, f := range []string{log, otherLog} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	checked, code := admitNavigation(p.snapshot, "", parent.Lease(), log, reportFor(log, parent.Lease()))
	if code != 0 || checked == nil {
		t.Fatalf("inherited admission: exit %d", code)
	}
	if got := p.leases(t); len(got) != 3 {
		t.Fatalf("leases with a child: %v", got)
	}
	f, err := openNavigationLog(log, checked)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	releaseAdmission()
	link := filepath.Join(work, "2-targets", "c001", "a1", "link.jsonl")
	if err := os.Symlink(otherLog, link); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "mcp-calls.jsonl")
	if err := os.WriteFile(outside, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sameRun := filepath.Join(work, "2-targets", "c001", "a1", "same.jsonl")
	if err := os.Symlink(log, sameRun); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(work, "2-targets", "c001", "a1", "dir.jsonl")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	missing := "no such file"
	if runtime.GOOS == "windows" {
		missing = "cannot find the file"
	}
	for name, tc := range map[string]struct{ log, report, want string }{
		"a log in another run":       {otherLog, reportFor(otherLog, parent.Lease()), "not in the work directory"},
		"a log outside the snapshot": {outside, reportFor(outside, parent.Lease()), "not in the work directory"},
		"a missing log":              {filepath.Join(filepath.Dir(log), "none.jsonl"), reportFor(log, parent.Lease()), missing},
		"no log":                     {"", reportFor(log, parent.Lease()), "needs -log"},
		"a link to another run":      {link, reportFor(link, parent.Lease()), "not a regular file"},
		"a link in the same run":     {sameRun, reportFor(sameRun, parent.Lease()), "not a regular file"},
		"a directory":                {directory, reportFor(directory, parent.Lease()), "not a regular file"},
		"a report elsewhere":         {log, filepath.Join(work, storage.FailurePrefix+"x"), "-failure-report"},
		"another lease's report":     {log, reportFor(log, other.Lease()), "-failure-report"},
	} {
		var code int
		text := stderrOf(t, func() { _, code = admitNavigation(p.snapshot, "", parent.Lease(), tc.log, tc.report) })
		if code == 0 {
			releaseAdmission()
		}
		if code == 0 || !strings.Contains(text, tc.want) {
			t.Errorf("%s: exit %d, %s", name, code, text)
		}
	}
	if got := p.leases(t); len(got) != 2 {
		t.Fatalf("a refused child left a lease: %v", got)
	}
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"-parent-lease with -config": {[]string{"-root", p.snapshot, "-conversation", p.conversation, "-round", "1", "-digest", "x",
			"-parent-lease", parent.Lease(), "-failure-report", "f", "-config", "asz.yaml"}, "no -config"},
		"-parent-lease without -failure-report": {[]string{"-root", p.snapshot, "-conversation", p.conversation, "-round", "1", "-digest", "x",
			"-parent-lease", parent.Lease()}, "needs -failure-report"},
		"-failure-report without -parent-lease": {[]string{"-root", p.snapshot, "-conversation", p.conversation, "-round", "1", "-digest", "x",
			"-failure-report", "f"}, "with -parent-lease"},
	} {
		code := 0
		text := stderrOf(t, func() { code = cmdJudgeMCP(tc.args) })
		if code == 0 || !strings.Contains(text, tc.want) {
			t.Errorf("%s: exit %d, %s", name, code, text)
		}
	}
	// A run that died leaves its lease file, which nobody holds.
	dead := "run-j000001-" + strings.Repeat("d", 64) + ".lock"
	if err := os.WriteFile(filepath.Join(p.snapshot, storage.LeasesDir, dead), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var code2 int
	stderrOf(t, func() { _, code2 = admitNavigation(p.snapshot, "", dead, log, reportFor(log, dead)) })
	if code2 == 0 {
		t.Fatal("a dead run's lease admitted a child")
	}
	if err := os.Remove(filepath.Join(p.snapshot, storage.LeasesDir, dead)); err != nil {
		t.Fatal(err)
	}
	lease := parent.Lease()
	if err := parent.Release(ctx); err != nil {
		t.Fatal(err)
	}
	stderrOf(t, func() { _, code2 = admitNavigation(p.snapshot, "", lease, log, reportFor(log, lease)) })
	if code2 == 0 {
		t.Fatal("an ended run's lease admitted a child")
	}
	_ = other.Release(ctx)
}

// A navigation server started on its own makes a new log outside every
// snapshot and never appends: a file already there is refused unchanged,
// a store entry reached through its production path included, and so is any
// path in a snapshot.
func TestAnIndependentLogIsNewAndOutsideSnapshots(t *testing.T) {
	p := pulledSnapshot(t)
	fresh := filepath.Join(t.TempDir(), "calls.jsonl")
	f, err := openNavigationLog(fresh, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	existing := filepath.Join(t.TempDir(), "evidence.gz")
	if err := os.WriteFile(existing, []byte("evidence"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openNavigationLog(existing, nil); err == nil {
		t.Fatal("an existing file was opened as an independent log")
	}
	if data, _ := os.ReadFile(existing); string(data) != "evidence" {
		t.Fatal("the existing file changed")
	}
	inside := filepath.Join(p.snapshot, p.session, storage.JudgmentsDir, "j000009", storage.WorkDir, "calls.jsonl")
	if err := os.MkdirAll(filepath.Dir(inside), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := openNavigationLog(inside, nil); !errors.Is(err, storage.ErrProtected) {
		t.Fatalf("a log inside a snapshot: %v", err)
	}
	if _, err := os.Lstat(inside); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused log was made")
	}
}

// Labels and prices may lie in the snapshot a lease admits, in its judge
// work, and nowhere else in a tree pull owns: not in another snapshot's
// work, and never at a lock file or a lease.
func TestSuppliedFilesFollowTheAdmittedSnapshot(t *testing.T) {
	p := pulledSnapshot(t)
	work := filepath.Join(p.snapshot, p.session, storage.JudgmentsDir, "j000001", storage.WorkDir)
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	labels := filepath.Join(work, "labels.yaml")
	if err := os.WriteFile(labels, []byte("goals:\n  - finish\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := judge.ReadLabels(labels, p.snapshot); err != nil {
		t.Fatalf("labels in the admitted snapshot: %v", err)
	}
	if _, err := judge.ReadLabels(labels, ""); !errors.Is(err, storage.ErrProtected) {
		t.Fatalf("labels in a snapshot no lease admits: %v", err)
	}
	if _, err := judge.LoadPricing(labels, t.TempDir()); !errors.Is(err, storage.ErrProtected) {
		t.Fatalf("prices in another tree: %v", err)
	}
	lease := filepath.Join(p.snapshot, storage.LeasesDir, "read-"+strings.Repeat("a", 64)+".lock")
	if err := os.WriteFile(lease, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := judge.ReadLabels(lease, p.snapshot); !errors.Is(err, storage.ErrProtected) {
		t.Fatalf("a lease read as labels: %v", err)
	}
}
