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

package client_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/pull/client"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/format"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/pulltest"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario/run"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

var (
	sourceOnce sync.Map // scenario name -> *built
)

type built struct {
	once               sync.Once
	root, conversation string
	err                error
}

// source builds a scenario's root once for the package's tests; they only
// read it.
func source(t *testing.T, name string) (root, conversation string) {
	t.Helper()
	v, _ := sourceOnce.LoadOrStore(name, &built{})
	b := v.(*built)
	b.once.Do(func() {
		out, err := os.MkdirTemp("", "pull-source-")
		if err != nil {
			b.err = err
			return
		}
		rep, err := run.Check(filepath.Join("..", "..", "..", "tests", "scenarios", name+".yaml"),
			run.Options{Out: out, Formats: []scenario.Format{scenario.FormatClaudeCode}})
		if err != nil || rep.Failed {
			b.err = errors.Join(err, errors.New(strings.Join(rep.Lines, "; ")))
			return
		}
		b.root = filepath.Join(out, string(scenario.FormatClaudeCode))
		entries, err := os.ReadDir(filepath.Join(b.root, "_conversations"))
		if err != nil || len(entries) == 0 {
			b.err = errors.Join(err, errors.New("no conversation"))
			return
		}
		b.conversation = entries[0].Name()
	})
	if b.err != nil {
		t.Fatalf("building %s: %v", name, b.err)
	}
	return b.root, b.conversation
}

// clock is a settable time.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

type env struct {
	t                 *testing.T
	src, conversation string
	local             *client.Local
	clock             *clock
	logs              []string
	mu                sync.Mutex
}

func newEnv(t *testing.T, scenarioName string) *env {
	t.Helper()
	src, conversation := source(t, scenarioName)
	e := &env{t: t, src: src, conversation: conversation, clock: &clock{now: time.Date(2026, 10, 25, 10, 32, 0, 0, time.UTC)}}
	e.local = &client.Local{Root: t.TempDir(), TTL: 720 * time.Hour, Now: e.clock.Now,
		Log: func(format string, args ...any) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.logs = append(e.logs, format)
			_ = args
		}}
	return e
}

func (e *env) manifest(round uint64) *format.Manifest {
	e.t.Helper()
	m, err := pulltest.ManifestAt(e.src, e.conversation, round)
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

func (e *env) pull(m *format.Manifest) (*client.Result, *pulltest.Source, error) {
	e.t.Helper()
	src := &pulltest.Source{Root: e.src}
	res, err := e.local.Pull(context.Background(), "prod", e.conversation, m, src)
	return res, src, err
}

func (e *env) mustPull(m *format.Manifest) (*client.Result, *pulltest.Source) {
	e.t.Helper()
	res, src, err := e.pull(m)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := res.Admission.Release(context.Background()); err != nil {
		e.t.Fatal(err)
	}
	return res, src
}

func (e *env) dir() string {
	return filepath.Join(e.local.Root, storage.RemoteDir, "prod", e.conversation)
}

func (e *env) outer() storage.OuterCopy {
	e.t.Helper()
	c, err := storage.ReadOuterCopy(e.dir(), "prod", e.conversation)
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *env) accept(tag string) *format.Accepted {
	e.t.Helper()
	marker, err := storage.ReadSnapshotCopy(filepath.Join(e.dir(), tag), "prod", e.conversation, tag)
	if err != nil {
		e.t.Fatal(err)
	}
	a, err := format.Accept(filepath.Join(e.dir(), tag), format.Published, format.Want{Conversation: e.conversation,
		ManifestDigest: format.ManifestDigest(marker.ManifestDigest), Session: marker.Session, Round: marker.Round,
		Digest: format.CommitDigest(marker.Digest), Remote: "prod", Tag: tag})
	if err != nil {
		e.t.Fatal(err)
	}
	return a
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	items, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, item := range items {
		out = append(out, item.Name())
	}
	return out
}

// A first pull fetches every file, publishes a snapshot that accepts as
// whole evidence under the clock's tag, admits it for the caller, and leaves
// no build or intent behind.
func TestAFirstPullPublishes(t *testing.T) {
	e := newEnv(t, "three-rounds")
	m := e.manifest(0)
	res, src, err := e.pull(m)
	if err != nil {
		t.Fatal(err)
	}
	if res.Unchanged || res.Reused != 0 || res.Fetched != len(m.Files) || src.Asked != len(m.Files) {
		t.Fatalf("result %+v, asked %d of %d", res, src.Asked, len(m.Files))
	}
	if res.Tag != storage.Stamp(e.clock.Now()) {
		t.Fatalf("tag %s, want the clock's %s", res.Tag, storage.Stamp(e.clock.Now()))
	}
	leases := entries(t, filepath.Join(e.dir(), res.Tag, storage.LeasesDir))
	if len(leases) != 1 || !strings.HasPrefix(leases[0], "read-") {
		t.Fatalf("the result is not held by a lease: %v", leases)
	}
	if err := res.Admission.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if leases := entries(t, filepath.Join(e.dir(), res.Tag, storage.LeasesDir)); len(leases) != 0 {
		t.Fatalf("a released lease stayed: %v", leases)
	}
	if a := e.accept(res.Tag); a.Digest != m.Digest() {
		t.Fatalf("published %s, pulled %s", a.Digest, m.Digest())
	}
	outer := e.outer()
	if outer.GreatestTag != res.Tag || outer.Publishing != nil {
		t.Fatalf("history %+v", outer)
	}
	if left := entries(t, filepath.Join(e.dir(), storage.BuildingDir)); len(left) != 0 {
		t.Fatalf("builds left: %v", left)
	}
	if _, err := os.Lstat(filepath.Join(e.dir(), res.Tag, "_work")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the published tree holds _work/: %v", err)
	}
}

// The same evidence again is the newest snapshot, unchanged: nothing is
// fetched and no tag is taken.
func TestTheSameEvidenceIsUnchanged(t *testing.T) {
	e := newEnv(t, "three-rounds")
	m := e.manifest(0)
	first, _ := e.mustPull(m)
	e.clock.set(e.clock.Now().Add(time.Hour))
	res, src, err := e.pull(e.manifest(0))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Admission.Release(context.Background()) }()
	if !res.Unchanged || res.Tag != first.Tag || src.Asked != 0 {
		t.Fatalf("result %+v, asked %d", res, src.Asked)
	}
	if outer := e.outer(); outer.GreatestTag != first.Tag {
		t.Fatalf("a tag was taken: %+v", outer)
	}
}

// A later head reuses every file the base holds under the same name and
// bytes, and fetches only the new ones. Its tag follows the first even when
// the clock stands still or goes back.
func TestALaterHeadReusesTheBase(t *testing.T) {
	e := newEnv(t, "three-rounds")
	early := e.manifest(1)
	full := e.manifest(0)
	if len(early.Files) >= len(full.Files) {
		t.Fatal("the fixture needs a later head with more files")
	}
	first, _ := e.mustPull(early)
	e.clock.set(e.clock.Now().Add(-time.Hour))
	res, src := e.mustPull(full)
	if res.Unchanged || res.Reused != len(early.Files) || src.Asked != len(full.Files)-len(early.Files) {
		t.Fatalf("reused %d of %d, fetched %d of %d", res.Reused, len(early.Files), src.Asked, len(full.Files)-len(early.Files))
	}
	at, _ := storage.ParseTag(first.Tag)
	if res.Tag != storage.Stamp(at.Add(time.Nanosecond)) {
		t.Fatalf("tag %s after %s under a backward clock", res.Tag, first.Tag)
	}
	e.accept(res.Tag)
	// The older snapshot stays as it was.
	if a := e.accept(first.Tag); a.Digest != early.Digest() {
		t.Fatal("the older snapshot changed")
	}
	// A reused file is shared on Linux and macOS, never made writable.
	for _, f := range early.Files {
		older, err := os.Stat(filepath.Join(e.dir(), first.Tag, filepath.FromSlash(f.File)))
		if err != nil {
			t.Fatal(err)
		}
		newer, err := os.Stat(filepath.Join(e.dir(), res.Tag, filepath.FromSlash(f.File)))
		if err != nil {
			t.Fatal(err)
		}
		if newer.Mode().Perm()&0o222 != 0 || older.Mode().Perm()&0o222 != 0 {
			t.Fatalf("%s is writable", f.File)
		}
		if !res.Copied && !os.SameFile(older, newer) {
			t.Fatalf("%s was neither linked nor said to be copied", f.File)
		}
	}
	// Going back to the earlier head publishes again: an older match does
	// not make a pull unchanged.
	again, src2 := e.mustPull(e.manifest(1))
	if again.Unchanged || src2.Asked != 0 || again.Reused != len(early.Files) {
		t.Fatalf("back to the earlier head: %+v, asked %d", again, src2.Asked)
	}
}

// A snapshot found damaged is passed over, reported and left as it is; the
// pull builds from the next valid one or from nothing.
func TestADamagedSnapshotIsNotABase(t *testing.T) {
	e := newEnv(t, "three-rounds")
	first, _ := e.mustPull(e.manifest(0))
	victim := e.manifest(0).Files[0].File
	path := filepath.Join(e.dir(), first.Tag, filepath.FromSlash(victim))
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("damaged"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.clock.set(e.clock.Now().Add(time.Minute))
	res, src := e.mustPull(e.manifest(0))
	if res.Unchanged || res.Reused != 0 || src.Asked != len(e.manifest(0).Files) {
		t.Fatalf("result %+v, asked %d", res, src.Asked)
	}
	if !slices.ContainsFunc(e.logs, func(s string) bool { return strings.Contains(s, "damaged") }) {
		t.Fatalf("the damage was not reported: %v", e.logs)
	}
	if data, _ := os.ReadFile(path); string(data) != "damaged" {
		t.Fatal("the damaged snapshot was touched")
	}
}

// Bytes the source gives that are not what the manifest lists stop the
// build; nothing is published and the private build is gone.
func TestWrongBytesPublishNothing(t *testing.T) {
	e := newEnv(t, "three-rounds")
	m := e.manifest(0)
	src := &pulltest.Source{Root: e.src, Change: func(name string, data []byte) []byte {
		if name == m.Files[len(m.Files)-1].File {
			return append([]byte{}, append(data, '\n')...)
		}
		return data
	}}
	if _, err := e.local.Pull(context.Background(), "prod", e.conversation, m, src); !errors.Is(err, storage.ErrChecked) {
		t.Fatalf("%v, want the fetched bytes refused as they were written", err)
	}
	if tags := entries(t, e.dir()); slices.ContainsFunc(tags, func(n string) bool { _, err := storage.ParseTag(n); return err == nil }) {
		t.Fatalf("published: %v", tags)
	}
	if left := entries(t, filepath.Join(e.dir(), storage.BuildingDir)); len(left) != 0 {
		t.Fatalf("builds left: %v", left)
	}
	if outer := e.outer(); outer.GreatestTag != "" || outer.Publishing != nil {
		t.Fatalf("a tag was taken: %+v", outer)
	}
}

// Evidence that disagrees with the base about a landed file it holds stops
// the pull before anything is built.
func TestConflictingEvidenceStopsBeforeBuilding(t *testing.T) {
	e := newEnv(t, "three-rounds")
	e.mustPull(e.manifest(0))
	m := e.manifest(0)
	for i := range m.Files {
		if m.Files[i].Kind == format.KindLanded {
			m.Files[i].Digest = format.StoredDigest(strings.Repeat("0", 64))
			break
		}
	}
	_, src, err := e.pull(m)
	if !errors.Is(err, client.ErrConflict) || src.Asked != 0 {
		t.Fatalf("%v after asking %d", err, src.Asked)
	}
}

// An expired snapshot admits no reader and is no base. The command-start
// sweep removes it when no reader holds it, records its tag removed, and
// leaves one a reader holds.
func TestExpiredSnapshotsAreSweptWhenUnheld(t *testing.T) {
	e := newEnv(t, "three-rounds")
	e.local.TTL = 2 * time.Second
	first, _ := e.mustPull(e.manifest(1))
	e.clock.set(e.clock.Now().Add(time.Second))
	held, _, err := e.pull(e.manifest(0))
	if err != nil {
		t.Fatal(err)
	}
	e.clock.set(e.clock.Now().Add(3 * time.Second))
	swept, err := e.local.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(swept.Removed) != 1 || !strings.HasSuffix(swept.Removed[0], first.Tag) || len(swept.Failed) != 0 {
		t.Fatalf("swept %+v", swept)
	}
	if _, err := os.Lstat(filepath.Join(e.dir(), held.Tag)); err != nil {
		t.Fatalf("a held snapshot was removed: %v", err)
	}
	removed, err := storage.IsRemoved(e.dir(), "prod", e.conversation, first.Tag)
	if err != nil || !removed {
		t.Fatalf("the removed tag is not recorded: %v %v", removed, err)
	}
	if err := held.Admission.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if swept, err := e.local.Sweep(context.Background()); err != nil || len(swept.Removed) != 1 {
		t.Fatalf("after release: %+v %v", swept, err)
	}
	// An expired snapshot is no base: the next pull fetches everything.
	_, src, err := e.pull(e.manifest(0))
	if err != nil || src.Asked != len(e.manifest(0).Files) {
		t.Fatalf("%v, asked %d", err, src.Asked)
	}
	// A root with no copies is left as it is.
	empty := &client.Local{Root: t.TempDir(), TTL: time.Hour}
	if _, err := empty.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if left := entries(t, empty.Root); len(left) != 0 {
		t.Fatalf("a sweep made %v", left)
	}
}

// A publication interrupted after its tag was reserved is recovered by the
// next command: the tag is recorded removed and never issued again, the
// private build is deleted, and the intent is cleared.
func TestAnInterruptedPublicationIsRecovered(t *testing.T) {
	e := newEnv(t, "three-rounds")
	m := e.manifest(0)
	dir := e.dir()
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	// Reserve a tag and leave a build behind, as a process that stopped
	// after the reservation does.
	ctx := context.Background()
	_, pull, err := storage.BootstrapConversation(ctx, e.local.Root, "prod", e.conversation, nil)
	if err != nil {
		t.Fatal(err)
	}
	outer, err := storage.ReadOuterCopy(dir, "prod", e.conversation)
	if err != nil {
		t.Fatal(err)
	}
	build := strings.Repeat("ab", 32)
	tag, err := outer.Reserve(e.clock.Now(), build, string(m.Digest()))
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.WriteOuterCopy(dir, outer, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, storage.BuildingDir, build, "half"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := pull.Unlock(); err != nil {
		t.Fatal(err)
	}
	res, _ := e.mustPull(m)
	if res.Tag <= tag {
		t.Fatalf("tag %s after the abandoned %s", res.Tag, tag)
	}
	removed, err := storage.IsRemoved(dir, "prod", e.conversation, tag)
	if err != nil || !removed {
		t.Fatalf("the abandoned tag is not recorded removed: %v %v", removed, err)
	}
	if left := entries(t, filepath.Join(dir, storage.BuildingDir)); len(left) != 0 {
		t.Fatalf("builds left: %v", left)
	}
	if outer := e.outer(); outer.Publishing != nil {
		t.Fatalf("the intent stayed: %+v", outer.Publishing)
	}
}

// A tree at a reserved tag's name that is not this conversation's own is
// left for a person: recovery records nothing, deletes nothing and keeps the
// intent.
func TestAnUnownedCollisionIsLeftAlone(t *testing.T) {
	e := newEnv(t, "three-rounds")
	m := e.manifest(0)
	dir := e.dir()
	ctx := context.Background()
	_, pull, err := storage.BootstrapConversation(ctx, e.local.Root, "prod", e.conversation, nil)
	if err != nil {
		t.Fatal(err)
	}
	outer, _ := storage.ReadOuterCopy(dir, "prod", e.conversation)
	tag, err := outer.Reserve(e.clock.Now(), strings.Repeat("cd", 32), string(m.Digest()))
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.WriteOuterCopy(dir, outer, "", nil); err != nil {
		t.Fatal(err)
	}
	stranger := filepath.Join(dir, tag, "someone-elses")
	if err := os.MkdirAll(filepath.Dir(stranger), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stranger, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := pull.Unlock(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.pull(m); !errors.Is(err, client.ErrUnownedCollision) {
		t.Fatalf("%v, want the collision left for a person", err)
	}
	if data, err := os.ReadFile(stranger); err != nil || string(data) != "x" {
		t.Fatalf("the stranger's tree changed: %v", err)
	}
	if after := e.outer(); after.Publishing == nil {
		t.Fatal("the intent was cleared")
	}
	if removed, _ := storage.IsRemoved(dir, "prod", e.conversation, tag); removed {
		t.Fatal("the stranger's tag was recorded removed")
	}
}

// Only the newest snapshot can make a pull unchanged. With the newest one
// damaged, an older one holding the same evidence is the base, and the pull
// publishes again, reusing it whole.
func TestAnOlderMatchDoesNotMakeAPullUnchanged(t *testing.T) {
	e := newEnv(t, "three-rounds")
	early, _ := e.mustPull(e.manifest(1))
	e.clock.set(e.clock.Now().Add(time.Minute))
	newest, _ := e.mustPull(e.manifest(0))
	victim := filepath.Join(e.dir(), newest.Tag, filepath.FromSlash(e.manifest(0).Files[0].File))
	if err := os.Remove(victim); err != nil {
		t.Fatal(err)
	}
	e.clock.set(e.clock.Now().Add(time.Minute))
	res, src := e.mustPull(e.manifest(1))
	if res.Unchanged || res.Tag == early.Tag || src.Asked != 0 || res.Reused != len(e.manifest(1).Files) {
		t.Fatalf("result %+v, asked %d", res, src.Asked)
	}
}

// A sweep removes nothing that has not expired, whoever holds it.
func TestASweepLeavesUnexpiredSnapshots(t *testing.T) {
	e := newEnv(t, "three-rounds")
	e.local.TTL = time.Hour
	res, _ := e.mustPull(e.manifest(0))
	e.clock.set(e.clock.Now().Add(59 * time.Minute))
	swept, err := e.local.Sweep(context.Background())
	if err != nil || len(swept.Removed) != 0 || len(swept.Failed) != 0 {
		t.Fatalf("swept %+v %v", swept, err)
	}
	e.accept(res.Tag)
}

// Bytes that match what the manifest lists but are not whole evidence pass
// the checks made while they are written, and acceptance refuses the build:
// nothing is published.
func TestEvidenceThatDoesNotHoldPublishesNothing(t *testing.T) {
	e := newEnv(t, "three-rounds")
	m := e.manifest(0)
	i := slices.IndexFunc(m.Files, func(f format.Entry) bool { return f.Kind == format.KindLanded })
	data, err := os.ReadFile(filepath.Join(e.src, filepath.FromSlash(m.Files[i].File)))
	if err != nil {
		t.Fatal(err)
	}
	damaged := append(append([]byte{}, data...), data[:strings.IndexByte(string(data), '\n')+1]...)
	sum := sha256.Sum256(damaged)
	m.Files[i].Bytes, m.Files[i].Digest = uint64(len(damaged)), format.StoredDigest(hex.EncodeToString(sum[:]))
	src := &pulltest.Source{Root: e.src, Change: func(name string, b []byte) []byte {
		if name == m.Files[i].File {
			return damaged
		}
		return b
	}}
	if _, err := e.local.Pull(context.Background(), "prod", e.conversation, m, src); !errors.Is(err, format.ErrEvidence) {
		t.Fatalf("%v, want the build refused as evidence", err)
	}
	if tags := entries(t, e.dir()); slices.ContainsFunc(tags, func(n string) bool { _, err := storage.ParseTag(n); return err == nil }) {
		t.Fatalf("published: %v", tags)
	}
	if left := entries(t, filepath.Join(e.dir(), storage.BuildingDir)); len(left) != 0 {
		t.Fatalf("builds left: %v", left)
	}
}

// A reader that finds its snapshot's tag still being published recovers the
// publication when no other command holds the conversation: the tag is
// recorded removed, the build is deleted before the intent goes, and nothing
// else of the conversation changes. A held conversation is left to its owner.
func TestARecoveringReaderLeavesNothingOfTheBuild(t *testing.T) {
	e := newEnv(t, "three-rounds")
	m := e.manifest(0)
	dir := e.dir()
	ctx := context.Background()
	_, pull, err := storage.BootstrapConversation(ctx, e.local.Root, "prod", e.conversation, nil)
	if err != nil {
		t.Fatal(err)
	}
	outer, err := storage.ReadOuterCopy(dir, "prod", e.conversation)
	if err != nil {
		t.Fatal(err)
	}
	build := strings.Repeat("ef", 32)
	tag, err := outer.Reserve(e.clock.Now(), build, string(m.Digest()))
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.WriteOuterCopy(dir, outer, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, storage.BuildingDir, build, "half"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := e.local.RecoverIfFree(ctx, "prod", e.conversation); !errors.Is(err, storage.ErrBusy) {
		t.Fatalf("a held conversation: %v, want busy", err)
	}
	if err := pull.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := e.local.RecoverIfFree(ctx, "prod", e.conversation); err != nil {
		t.Fatal(err)
	}
	if left := entries(t, filepath.Join(dir, storage.BuildingDir)); len(left) != 0 {
		t.Fatalf("the abandoned build stayed: %v", left)
	}
	if removed, err := storage.IsRemoved(dir, "prod", e.conversation, tag); err != nil || !removed {
		t.Fatalf("the abandoned tag: %v %v", removed, err)
	}
	if after := e.outer(); after.Publishing != nil || after.GreatestTag != tag {
		t.Fatalf("history %+v", after)
	}
}

// stoppingSource cancels its pull once the first file it gives is
// installed, and counts the files installed after that.
type stoppingSource struct {
	inner   *pulltest.Source
	cancel  context.CancelFunc
	stopped bool
	after   int
}

func (s *stoppingSource) Fetch(ctx context.Context, work string, names []string, put func(string, io.Reader) error) error {
	return s.inner.Fetch(ctx, work, names, func(name string, r io.Reader) error {
		err := put(name, r)
		switch {
		case s.stopped && err == nil:
			s.after++
		case !s.stopped && err == nil:
			s.stopped = true
			s.cancel()
		}
		return err
	})
}

// stoppingLinks cancels a pull at its first link of a reused file, and
// counts the reused files linked after that.
type stoppingLinks struct {
	storage.DiskFileSystem
	base    string
	cancel  context.CancelFunc
	stopped bool
	after   int
}

func (s *stoppingLinks) Link(from, to string) error {
	if strings.HasPrefix(from, s.base+string(filepath.Separator)) {
		if s.stopped {
			s.after++
		}
		s.stopped = true
		defer s.cancel()
	}
	return s.DiskFileSystem.Link(from, to)
}

// A stopped pull installs no file after it stopped, fetched or reused, and
// publishes nothing: only the operation it was in finishes.
func TestAStoppedBuildInstallsNoMoreFiles(t *testing.T) {
	e := newEnv(t, "three-rounds")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := &stoppingSource{inner: &pulltest.Source{Root: e.src}, cancel: cancel}
	if _, err := e.local.Pull(ctx, "prod", e.conversation, e.manifest(1), src); !errors.Is(err, context.Canceled) || src.after != 0 {
		t.Fatalf("fetched: %v, %d installed after the stop", err, src.after)
	}
	if runtime.GOOS == "windows" {
		// A reused file is copied there, through no link to stop at.
		return
	}
	first, _ := e.mustPull(e.manifest(1))
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	// The base is read at its real path.
	base, err := filepath.EvalSymlinks(filepath.Join(e.dir(), first.Tag))
	if err != nil {
		t.Fatal(err)
	}
	links := &stoppingLinks{base: base, cancel: cancel}
	e.local.Files = links
	if _, err := e.local.Pull(ctx, "prod", e.conversation, e.manifest(0), &pulltest.Source{Root: e.src}); !errors.Is(err, context.Canceled) || !links.stopped || links.after != 0 {
		t.Fatalf("reused: %v, %d linked after the stop", err, links.after)
	}
	e.local.Files = nil
	for _, name := range entries(t, e.dir()) {
		if _, err := storage.ParseTag(name); err == nil && name != first.Tag {
			t.Fatalf("a snapshot was published: %s", name)
		}
	}
}

// The same files under another head are not unchanged: the copy is built
// from them and checked against that head, and nothing is published.
func TestTheSameFilesUnderAnotherHeadAreNotUnchanged(t *testing.T) {
	e := newEnv(t, "three-rounds")
	first, _ := e.mustPull(e.manifest(0))
	m := e.manifest(0)
	d := []byte(m.Head.Digest)
	if d[20] == '0' {
		d[20] = '1'
	} else {
		d[20] = '0'
	}
	m.Head.Digest = format.CommitDigest(d)
	res, _, err := e.pull(m)
	if err == nil || res != nil && res.Unchanged {
		t.Fatalf("%+v %v", res, err)
	}
	for _, name := range entries(t, e.dir()) {
		if _, err := storage.ParseTag(name); err == nil && name != first.Tag {
			t.Fatalf("a snapshot was published: %s", name)
		}
	}
}

// The steps of making a build durable a pull is stopped at.
const (
	// atScratch: once the build's scratch is removed.
	atScratch = iota
	// atTree: once the finished tree's own directory is synced the second
	// time, the last sync of the tree after the one its scratch's removal
	// makes.
	atTree
	// atAncestors: at the first sync of the ancestors, from the root down.
	atAncestors
)

// stoppingSyncs cancels a pull at one step of making its build durable, and
// counts the directory syncs inside _building after that.
type stoppingSyncs struct {
	storage.DiskFileSystem
	at        int
	cancel    context.CancelFunc
	treeSyncs int
	stopped   bool
	after     int
}

func (s *stoppingSyncs) Remove(path string) error {
	err := s.DiskFileSystem.Remove(path)
	if s.at == atScratch && filepath.Base(path) == "_work" && err == nil {
		s.stop()
	}
	return err
}

func (s *stoppingSyncs) SyncDir(path string) error {
	if s.stopped && strings.Contains(path, string(filepath.Separator)+storage.BuildingDir+string(filepath.Separator)) {
		s.after++
	}
	if s.at == atAncestors && s.treeSyncs == 2 {
		s.stop()
	}
	err := s.DiskFileSystem.SyncDir(path)
	if s.at != atScratch && filepath.Base(filepath.Dir(path)) == storage.BuildingDir && finished(path) {
		if s.treeSyncs++; s.treeSyncs == 2 && s.at == atTree {
			s.stop()
		}
	}
	return err
}

// finished says whether a build's tree is complete: its leases directory is
// made and its scratch is gone.
func finished(tree string) bool {
	_, leases := os.Lstat(filepath.Join(tree, storage.LeasesDir))
	_, work := os.Lstat(filepath.Join(tree, "_work"))
	return leases == nil && errors.Is(work, os.ErrNotExist)
}

func (s *stoppingSyncs) stop() {
	if !s.stopped {
		s.stopped = true
		s.cancel()
	}
}

// stoppingRemoves cancels a pull at its first fetched file, then counts the
// removals inside _building that follow.
type stoppingRemoves struct {
	storage.DiskFileSystem
	stopped bool
	after   int
}

func (s *stoppingRemoves) Remove(path string) error {
	if s.stopped && strings.Contains(path, string(filepath.Separator)+storage.BuildingDir+string(filepath.Separator)) {
		s.after++
	}
	return s.DiskFileSystem.Remove(path)
}

// A stopped pull stops making its tree durable, and stops deleting what it
// built, between operations: the rest is left for the next cleanup, which
// the next pull makes before it publishes.
func TestAStoppedPullStopsItsSyncsAndCleanup(t *testing.T) {
	e := newEnv(t, "three-rounds")
	for _, at := range []int{atScratch, atTree, atAncestors} {
		ctx, cancel := context.WithCancel(context.Background())
		syncs := &stoppingSyncs{at: at, cancel: cancel}
		e.local.Files = syncs
		_, err := e.local.Pull(ctx, "prod", e.conversation, e.manifest(0), &pulltest.Source{Root: e.src})
		cancel()
		if !errors.Is(err, context.Canceled) || !syncs.stopped || syncs.after != 0 {
			t.Fatalf("syncs, stopped at step %d: %v, %d after the stop", at, err, syncs.after)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	removes := &stoppingRemoves{}
	e.local.Files = removes
	src := &stoppingSource{inner: &pulltest.Source{Root: e.src}, cancel: func() {
		removes.stopped = true
		cancel()
	}}
	if _, err := e.local.Pull(ctx, "prod", e.conversation, e.manifest(0), src); !errors.Is(err, context.Canceled) || removes.after != 0 {
		t.Fatalf("cleanup: %v, %d removals after the stop", err, removes.after)
	}
	e.local.Files = nil
	if res, _ := e.mustPull(e.manifest(0)); res.Tag == "" {
		t.Fatal("the next pull published nothing")
	}
	if left := entries(t, filepath.Join(e.dir(), storage.BuildingDir)); len(left) != 0 {
		t.Fatalf("left in _building: %v", left)
	}
}

// The local column passes over a damaged snapshot, said, to the next older
// one; damage to the conversation's own controls fails it.
func TestTheLocalColumnPassesOverADamagedSnapshot(t *testing.T) {
	e := newEnv(t, "three-rounds")
	older, _ := e.mustPull(e.manifest(1))
	e.clock.set(e.clock.Now().Add(time.Hour))
	newer, _ := e.mustPull(e.manifest(0))
	ctx := context.Background()
	breakFile := func(path string) {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{broken"), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	breakFile(filepath.Join(e.dir(), newer.Tag, storage.CopyMarker))
	heads := e.local.NewLocalHeads()
	round, ok, err := heads.Head(ctx, "prod", e.conversation)
	if err != nil || !ok || round != e.manifest(1).Head.Round {
		t.Fatalf("round %d %v: %v", round, ok, err)
	}
	if err := heads.Release(ctx); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	said := strings.Join(e.logs, "\n")
	e.mu.Unlock()
	if !strings.Contains(said, "damaged") {
		t.Fatalf("the damage was not said: %s", said)
	}
	// The gate that is not a directory fails the conversation.
	gate := filepath.Join(e.dir(), storage.AdmitDir)
	if err := os.Rename(gate, gate+".kept"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.local.NewLocalHeads().Head(ctx, "prod", e.conversation); !errors.Is(err, storage.ErrConversationControl) {
		t.Fatalf("a damaged _admit: %v", err)
	}
	if err := os.Remove(gate); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(gate+".kept", gate); err != nil {
		t.Fatal(err)
	}
	breakFile(filepath.Join(e.dir(), storage.CopyMarker))
	if _, _, err := e.local.NewLocalHeads().Head(ctx, "prod", e.conversation); !errors.Is(err, storage.ErrConversationControl) {
		t.Fatalf("a damaged outer _copy: %v", err)
	}
	_ = older
}

// failsLeases fails the creation of every lease file, as a full disk does.
type failsLeases struct{ storage.DiskFileSystem }

func (failsLeases) CreateIn(dir *storage.Dir, name string, mode os.FileMode) (storage.WriteFile, error) {
	if filepath.Base(dir.Path()) == storage.LeasesDir {
		return nil, syscall.ENOSPC
	}
	return storage.DiskFileSystem{}.CreateIn(dir, name, mode)
}

// A lease this machine cannot write fails the local column: it is never
// taken for damage to the snapshot and passed over.
func TestALeaseThatCannotBeWrittenFailsTheLocalColumn(t *testing.T) {
	e := newEnv(t, "three-rounds")
	e.mustPull(e.manifest(0))
	e.local.Files = failsLeases{}
	_, ok, err := e.local.NewLocalHeads().Head(context.Background(), "prod", e.conversation)
	if !errors.Is(err, storage.ErrLocalWrite) || ok {
		t.Fatalf("a lease that cannot be written: %v %v", ok, err)
	}
}

// An admission that succeeds as the command stops is kept, and let go with
// the others: no lease is left behind untracked.
func TestAnAdmissionAsTheCommandStopsIsKept(t *testing.T) {
	e := newEnv(t, "three-rounds")
	res, _ := e.mustPull(e.manifest(0))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	local := &client.Local{Root: e.local.Root, TTL: e.local.TTL, Now: func() time.Time {
		cancel()
		return e.clock.Now()
	}}
	heads := local.NewLocalHeads()
	round, ok, err := heads.Head(ctx, "prod", e.conversation)
	if err != nil || !ok || round != e.manifest(0).Head.Round {
		t.Fatalf("round %d %v: %v", round, ok, err)
	}
	if err := heads.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if left := entries(t, filepath.Join(e.dir(), res.Tag, storage.LeasesDir)); len(left) != 0 {
		t.Fatalf("leases left: %v", left)
	}
}

// The newest snapshot here is admitted, and damage to it is an error: an
// older snapshot is never read in its place.
func TestTheNewestSnapshotIsAdmittedOrItsDamageSaid(t *testing.T) {
	e := newEnv(t, "three-rounds")
	e.mustPull(e.manifest(1))
	e.clock.set(e.clock.Now().Add(time.Hour))
	newer, _ := e.mustPull(e.manifest(0))
	ctx := context.Background()
	a, err := e.local.AdmitNewest(ctx, "prod", e.conversation)
	if err != nil || a.Root.Tag != newer.Tag {
		t.Fatalf("admitted %v: %v", a, err)
	}
	if err := a.Release(ctx); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(e.dir(), newer.Tag, storage.CopyMarker)
	if err := os.Chmod(marker, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("{broken"), 0o444); err != nil {
		t.Fatal(err)
	}
	a, err = e.local.AdmitNewest(ctx, "prod", e.conversation)
	if err == nil || errors.Is(err, client.ErrNoSnapshot) {
		if a != nil {
			_ = a.Release(ctx)
			t.Fatalf("%s was admitted in place of the damaged %s", a.Root.Tag, newer.Tag)
		}
		t.Fatalf("a damaged newest snapshot: %v", err)
	}
}
