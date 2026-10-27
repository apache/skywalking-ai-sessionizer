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

package storage_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage/faulttest"
)

// A reader never makes a gate a pull has not made: LockOwned creates neither
// a directory nor a lock file, and it refuses links in place of either.
func TestLockOwnedCreatesNothing(t *testing.T) {
	base := t.TempDir()
	missing := filepath.Join(base, "_admit")
	if _, err := storage.LockOwned(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing gate: %v", err)
	}
	if _, err := os.Lstat(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the gate was made: %v", err)
	}
	mkdirs(t, missing)
	if _, err := storage.LockOwned(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing lock file: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(missing, ".lock")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the lock file was made: %v", err)
	}
	// LockOwnedCreate makes the lock file, never the directory.
	l, err := storage.LockOwnedCreate(missing, nil)
	check(t, err)
	if _, err := storage.LockOwned(missing); !errors.Is(err, storage.ErrBusy) {
		t.Fatalf("a held gate: %v", err)
	}
	check(t, l.Unlock())
	l, err = storage.LockOwned(missing)
	check(t, err)
	check(t, l.Unlock())
	if _, err := storage.LockOwnedCreate(filepath.Join(base, "nowhere"), nil); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a missing directory: %v", err)
	}
}

func TestLockOwnedRefusesLinksAndOtherFiles(t *testing.T) {
	base := t.TempDir()
	gate := filepath.Join(base, "gate")
	mkdirs(t, gate, filepath.Join(base, "dirlock", ".lock"))
	check(t, os.WriteFile(filepath.Join(gate, ".lock"), nil, 0o644))
	// The open itself refuses a directory, before the regular-file check.
	if l, err := storage.LockOwned(filepath.Join(base, "dirlock")); err == nil {
		_ = l.Unlock()
		t.Fatal("locked a directory named .lock")
	}
	symlinkOrSkip(t, gate, filepath.Join(base, "linked"))
	if _, err := storage.LockOwned(filepath.Join(base, "linked")); !errors.Is(err, storage.ErrProtected) {
		t.Fatalf("a link to a gate directory: %v", err)
	}
	other := filepath.Join(base, "other")
	mkdirs(t, other)
	symlinkOrSkip(t, filepath.Join(gate, ".lock"), filepath.Join(other, ".lock"))
	if _, err := storage.LockOwned(other); !errors.Is(err, storage.ErrProtected) {
		t.Fatalf("a link in place of a lock file: %v", err)
	}
}

func TestLockOwnedWait(t *testing.T) {
	dir := t.TempDir()
	held, err := storage.LockOwnedCreate(dir, nil)
	check(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := storage.LockOwnedWait(ctx, dir); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waited past its context: %v", err)
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = held.Unlock()
	}()
	l, err := storage.LockOwnedWait(context.Background(), dir)
	check(t, err)
	check(t, l.Unlock())
}

func TestLockServerOwner(t *testing.T) {
	model := controlModel(t)
	owner, err := storage.LockServerOwner(model.Root, model)
	check(t, err)
	if _, err := storage.LockServerOwner(model.Root, model); !errors.Is(err, storage.ErrBusy) {
		t.Fatalf("a second owner: %v", err)
	}
	check(t, owner.Unlock())
	// Only _pull and its owner directory are made, and both survive a power loss.
	recovered := powerLoss(t, model, faulttest.DropAll)
	entries, err := os.ReadDir(filepath.Join(recovered.Root, storage.PullDir))
	if err != nil || len(entries) != 1 || entries[0].Name() != storage.ServerOwnerDir {
		t.Fatalf("after power loss: %v, %v", entries, err)
	}
}

func TestValidRun(t *testing.T) {
	for run, want := range map[string]bool{
		"j000001": true, "j000003": true, "j999999": true, "j1000000": true,
		"j000000": false, "j00001": false, "j0000001": false, "j1": false,
		"000001": false, "J000001": false, "j00000a": false, "j-00001": false, "": false,
	} {
		if storage.ValidRun(run) != want {
			t.Fatalf("%q: %v", run, !want)
		}
	}
}

func TestLeaseNames(t *testing.T) {
	leases := t.TempDir()
	for _, tc := range []struct{ kind, run string }{{storage.ReadLease, ""}, {storage.ChildLease, ""}, {storage.RunLease, "j000003"}} {
		lease, err := storage.CreateLease(leases, tc.kind, tc.run, nil)
		check(t, err)
		kind, run, ok := storage.ParseLeaseName(lease.Name())
		if !ok || kind != tc.kind || run != tc.run {
			t.Fatalf("%s: read back %q %q %v", lease.Name(), kind, run, ok)
		}
		check(t, lease.Release())
	}
	for _, tc := range []struct{ kind, run string }{{storage.RunLease, ""}, {storage.RunLease, "j1"}, {storage.ReadLease, "j000003"}, {"write", ""}} {
		if _, err := storage.CreateLease(leases, tc.kind, tc.run, nil); err == nil {
			t.Fatalf("made a %q lease for run %q", tc.kind, tc.run)
		}
	}
	hex := strings.Repeat("a", 64)
	for _, name := range []string{
		"read-" + strings.Repeat("A", 64) + ".lock",
		"read-" + hex[:63] + ".lock",
		"read-" + hex,
		"run-" + hex + ".lock",
		"run-j1-" + hex + ".lock",
		"write-" + hex + ".lock",
		"child-j000001-" + hex + ".lock",
	} {
		if _, _, ok := storage.ParseLeaseName(name); ok {
			t.Fatalf("%s read as a lease", name)
		}
	}
}

// A probe tells a live lease from a dead one without waiting, and leaves both
// as they were. A dead-lease sweep removes only the dead.
func TestProbeAndSweepLeases(t *testing.T) {
	leases := t.TempDir()
	live, err := storage.CreateLease(leases, storage.ReadLease, "", nil)
	check(t, err)
	defer live.Release()
	dead, err := storage.CreateLease(leases, storage.RunLease, "j000002", nil)
	check(t, err)
	check(t, dead.Abandon())
	state, err := storage.ProbeLeases(leases)
	check(t, err)
	if state.Held != 1 || len(state.Dead) != 1 || filepath.Base(state.Dead[0]) != dead.Name() {
		t.Fatalf("probe: %+v", state)
	}
	// The probe released what it took: the live lease is still exclusive and
	// a second probe sees the same.
	again, err := storage.ProbeLeases(leases)
	check(t, err)
	if again.Held != 1 || len(again.Dead) != 1 {
		t.Fatalf("second probe: %+v", again)
	}
	check(t, storage.SweepDeadLeases(leases, nil))
	if _, err := os.Lstat(filepath.Join(leases, dead.Name())); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the dead lease is still there: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(leases, live.Name())); err != nil {
		t.Fatalf("the live lease was removed: %v", err)
	}
	// Nothing but leases belongs in the directory.
	check(t, os.WriteFile(filepath.Join(leases, "note"), nil, 0o644))
	if _, err := storage.ProbeLeases(leases); !errors.Is(err, storage.ErrProtected) {
		t.Fatalf("a stray file: %v", err)
	}
}

func TestLeaseReleaseRemovesItsFile(t *testing.T) {
	leases := t.TempDir()
	lease, err := storage.CreateLease(leases, storage.ChildLease, "", nil)
	check(t, err)
	path := filepath.Join(leases, lease.Name())
	check(t, lease.Release())
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("released lease file: %v", err)
	}
	check(t, lease.Release())
}
