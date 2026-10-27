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
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/pull/client"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/pulltest"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// busyEveryOther makes every other control replacement busy, as a Windows
// reader without delete sharing does for a moment.
type busyEveryOther struct {
	storage.DiskFileSystem
	mu    sync.Mutex
	calls int
	busy  int
}

func (b *busyEveryOther) Replace(parent, from, to string) error {
	b.mu.Lock()
	b.calls++
	odd := b.calls%2 == 1
	if odd {
		b.busy++
	}
	b.mu.Unlock()
	if odd {
		return &storage.ControlBusyError{Err: errors.New("the file is being used by another process")}
	}
	return b.DiskFileSystem.Replace(parent, from, to)
}

// A busy control replacement repeats its whole transition: the locks are let
// go and taken again, the history read again, and the decision made again,
// so a pull whose every first replacement is busy still publishes once,
// whole, with no intent left.
func TestABusyReplacementRepeatsTheWholeTransition(t *testing.T) {
	e := newEnv(t, "three-rounds")
	fs := &busyEveryOther{}
	e.local.Files = fs
	res, _ := e.mustPull(e.manifest(0))
	if fs.busy < 2 {
		t.Fatalf("only %d replacements were busy", fs.busy)
	}
	if outer := e.outer(); outer.Publishing != nil || outer.GreatestTag != res.Tag {
		t.Fatalf("the history after: %+v, published %s", outer, res.Tag)
	}
	e.accept(res.Tag)
	if tags := entries(t, e.dir()); !contains(tags, res.Tag) {
		t.Fatalf("%v", tags)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// A tag the history lists as removed is removed by that alone: a tree
// restored there after an interrupted deletion, its marker already gone, is
// finished by the sweep and by publication recovery.
func TestARemovedTagIsFinishedByItsHistory(t *testing.T) {
	e := newEnv(t, "three-rounds")
	res, _ := e.mustPull(e.manifest(0))
	dir, tag := e.dir(), res.Tag
	outer := e.outer()
	if err := storage.WriteOuterCopy(dir, outer, tag, nil); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, tag, storage.CopyMarker)
	_ = os.Chmod(marker, 0o644)
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if _, err := e.local.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, tag)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the removed tag's tree is still there: %v", err)
	}

	// The same through recovery: an intent whose tag is already recorded
	// removed, its tree back without a marker.
	f := newEnv(t, "three-rounds")
	f.mustPull(f.manifest(0))
	dir = f.dir()
	ctx := context.Background()
	_, pull, err := storage.BootstrapConversation(ctx, f.local.Root, "prod", f.conversation, nil)
	if err != nil {
		t.Fatal(err)
	}
	outer, err = storage.ReadOuterCopy(dir, "prod", f.conversation)
	if err != nil {
		t.Fatal(err)
	}
	build := strings.Repeat("cd", 32)
	f.clock.set(f.clock.Now().Add(time.Second))
	next, err := outer.Reserve(f.clock.Now(), build, string(f.manifest(0).Digest()))
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.WriteOuterCopy(dir, outer, next, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, next), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := pull.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := f.local.RecoverIfFree(ctx, "prod", f.conversation); err != nil {
		t.Fatal(err)
	}
	if outer := f.outer(); outer.Publishing != nil {
		t.Fatalf("the intent stayed: %+v", outer.Publishing)
	}
	if _, err := os.Lstat(filepath.Join(dir, next)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the restored tree is still there: %v", err)
	}
	if left := entries(t, filepath.Join(dir, storage.RemovingDir)); len(left) != 0 {
		t.Fatalf("left for removal: %v", left)
	}
}

// A recovery that stopped after it detached the tree leaves it in
// _removing; the next recovery deletes it, after a fresh history write.
func TestRecoveryFinishesWhatAnEarlierOneDetached(t *testing.T) {
	e := newEnv(t, "three-rounds")
	e.mustPull(e.manifest(0))
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
	e.clock.set(e.clock.Now().Add(time.Second))
	tag, err := outer.Reserve(e.clock.Now(), strings.Repeat("ef", 32), string(e.manifest(0).Digest()))
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.WriteOuterCopy(dir, outer, tag, nil); err != nil {
		t.Fatal(err)
	}
	detached := filepath.Join(dir, storage.RemovingDir, tag+"-"+strings.Repeat("0", 16))
	if err := os.MkdirAll(filepath.Join(detached, "half"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := pull.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := e.local.RecoverIfFree(ctx, "prod", e.conversation); err != nil {
		t.Fatal(err)
	}
	if left := entries(t, filepath.Join(dir, storage.RemovingDir)); len(left) != 0 {
		t.Fatalf("left for removal: %v", left)
	}
	if outer := e.outer(); outer.Publishing != nil {
		t.Fatalf("the intent stayed: %+v", outer.Publishing)
	}
}

// One snapshot whose marker is damaged stops nothing else: the sweep says
// so, leaves it, and removes the healthy expired one beside it.
func TestOneDamagedMarkerStopsNoOtherRemoval(t *testing.T) {
	e := newEnv(t, "three-rounds")
	first, _ := e.mustPull(e.manifest(1))
	e.clock.set(e.clock.Now().Add(time.Second))
	second, _ := e.mustPull(e.manifest(0))
	if first.Tag == second.Tag {
		t.Fatal("one tag for two heads")
	}
	marker := filepath.Join(e.dir(), second.Tag, storage.CopyMarker)
	_ = os.Chmod(marker, 0o644)
	if err := os.WriteFile(marker, []byte("{broken"), 0o444); err != nil {
		t.Fatal(err)
	}
	e.clock.set(e.clock.Now().Add(2 * e.local.TTL))
	for range 2 {
		if _, err := e.local.Sweep(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	tags := entries(t, e.dir())
	if contains(tags, first.Tag) || !contains(tags, second.Tag) {
		t.Fatalf("after the sweeps: %v", tags)
	}
	said := 0
	for _, line := range e.logs {
		if strings.Contains(line, "left as it is") {
			said++
		}
	}
	if said != 2 {
		t.Fatalf("the damage was said %d times: %v", said, e.logs)
	}
}

// A reader's age check reads the clock once it holds the gate: a reader
// that waited for the gate past the snapshot's time to live is refused.
func TestAReaderThatWaitedPastExpiryIsRefused(t *testing.T) {
	e, root := published(t)
	marker, err := storage.ReadSnapshotCopy(root.Path, "prod", e.conversation, filepath.Base(root.Path))
	if err != nil {
		t.Fatal(err)
	}
	e.clock.set(marker.PublishedAt.Add(time.Second))
	gate, err := storage.LockOwned(filepath.Join(filepath.Dir(root.Path), storage.AdmitDir))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		a, err := e.admit(root)
		if err == nil {
			err = a.Release(context.Background())
			if err == nil {
				err = errors.New("admitted")
			}
		}
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	e.clock.set(marker.PublishedAt.Add(e.local.TTL + time.Second))
	if err := gate.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, storage.ErrExpired) {
		t.Fatalf("a reader that waited past expiry: %v", err)
	}
}

// slowSource moves the clock while it is read, as a slow download does.
type slowSource struct {
	inner client.Source
	clock *clock
	by    time.Duration
}

func (s slowSource) Fetch(ctx context.Context, work string, names []string, put func(string, io.Reader) error) error {
	s.clock.set(s.clock.Now().Add(s.by))
	return s.inner.Fetch(ctx, work, names, put)
}

// A snapshot's tag and publication time are taken once its build is done:
// a build longer than the time to live still publishes a snapshot readers
// are admitted to, marked with its tag's time.
func TestAPublicationIsTimedWhenItsBuildIsDone(t *testing.T) {
	e := newEnv(t, "three-rounds")
	e.local.TTL = 2 * time.Second
	start := e.clock.Now()
	res, err := e.local.Pull(context.Background(), "prod", e.conversation, e.manifest(0),
		slowSource{inner: &pulltest.Source{Root: e.src}, clock: e.clock, by: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Admission.Release(context.Background())
	at, err := storage.ParseTag(res.Tag)
	if err != nil || at.Before(start.Add(3*time.Second)) {
		t.Fatalf("tag %s, the build ended at %s", res.Tag, start.Add(3*time.Second))
	}
	marker, err := storage.ReadSnapshotCopy(filepath.Join(e.dir(), res.Tag), "prod", e.conversation, res.Tag)
	if err != nil || !marker.PublishedAt.Equal(at) {
		t.Fatalf("the marker's time %v, the tag's %v: %v", marker.PublishedAt, at, err)
	}
}

// expiresAtReservation moves the clock past the time to live once the
// publication's tag is reserved, before it is committed.
type expiresAtReservation struct {
	storage.DiskFileSystem
	clock *clock
	by    time.Duration
	mu    sync.Mutex
	calls int
}

func (x *expiresAtReservation) Replace(parent, from, to string) error {
	err := x.DiskFileSystem.Replace(parent, from, to)
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.calls++; x.calls == 2 {
		x.clock.set(x.clock.Now().Add(x.by))
	}
	return err
}

// A snapshot that expires while it is committed is published, but no reader
// is admitted to it, the pull's own hand-off included, and the next sweep
// removes it.
func TestASnapshotThatExpiresWhileCommittedIsPublishedAndRemoved(t *testing.T) {
	e := newEnv(t, "three-rounds")
	e.local.TTL = 2 * time.Second
	e.local.Files = &expiresAtReservation{clock: e.clock, by: 3 * time.Second}
	res, _, err := e.pull(e.manifest(0))
	if err == nil || !errors.Is(err, storage.ErrExpired) || res == nil || res.Tag == "" {
		t.Fatalf("the pull: %v %+v", err, res)
	}
	if outer := e.outer(); outer.Publishing != nil || outer.GreatestTag != res.Tag {
		t.Fatalf("the history: %+v", outer)
	}
	e.accept(res.Tag)
	if _, err := e.local.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if contains(entries(t, e.dir()), res.Tag) {
		t.Fatal("the expired snapshot was not swept")
	}
}
