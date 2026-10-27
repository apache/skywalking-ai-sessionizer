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

package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrBusy says another process holds a lock that pull, its server or a reader
// needs. A probe reports it without waiting.
var ErrBusy = errors.New("storage: the lock is held by another process")

// RealDirectory checks that dir is a directory and not a link to one, without
// following anything. Pull's layout is checked this way before any lock is
// taken, since the older helpers make directories and follow paths
// (internal/storage/lock_unix.go).
func RealDirectory(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode().Type() != fs.ModeDir {
		return fmt.Errorf("%w: %s is not a real directory", ErrProtected, dir)
	}
	return nil
}

// LockOwned takes the lock in an existing real directory, without waiting,
// without creating anything and without following a link. The lock file is
// opened through the directory's checked handle, so a directory swapped after
// the check cannot lead the lock elsewhere. Readers take the _admit gate this
// way: a reader never makes a gate a pull has not made.
func LockOwned(dir string) (*SessionLock, error) {
	f, err := lockIn(dir, lockFile, lockExisting)
	if err != nil {
		return nil, err
	}
	return &SessionLock{f: f}, nil
}

// LockOwnedCreate is LockOwned for the owner that may make the lock file: the
// pull for a conversation's _pull and _root, and the serving process for its
// owner lock. The directory itself must exist already, made and synced by
// MakeAncestorsDurable. A missing lock file is made through files, in the
// same bound directory the lock is then taken in, and its directory synced:
// a sweep reads a missing _pull lock as a bootstrap that never got that far,
// so one a power loss could take while the snapshots beside it stay would
// leave them unswept.
func LockOwnedCreate(dir string, files FileSystem) (*SessionLock, error) {
	files = writeFileSystem(files)
	d, err := OpenDir(dir)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	file, err := files.CreateIn(d, lockFile, PermState)
	switch {
	case err == nil:
		if err := file.Close(); err != nil {
			return nil, err
		}
	case !errors.Is(err, fs.ErrExist):
		return nil, err
	}
	// Synced on every attempt: an earlier process may have made the file
	// and stopped before its sync.
	if err := files.SyncDir(dir); err != nil {
		return nil, err
	}
	f, err := d.open(lockFile, lockExisting, PermState)
	if err != nil {
		return nil, err
	}
	return &SessionLock{f: f}, nil
}

// LockOwnedWait is LockOwned, waiting for a busy lock until ctx ends.
func LockOwnedWait(ctx context.Context, dir string) (*SessionLock, error) {
	for {
		l, err := LockOwned(dir)
		if !errors.Is(err, ErrBusy) {
			return l, err
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// ServerOwnerDir is where the process serving pulls holds its lifetime lock.
const ServerOwnerDir = "owner"

// LockServerOwner takes the lifetime lock of the one process that serves pulls
// from a production root, without waiting. It makes only _pull, its owner
// directory and the lock, syncing every ancestor from the root down, since a
// contender may wait here before any server state exists.
func LockServerOwner(root string, files FileSystem) (*SessionLock, error) {
	dir := filepath.Join(root, PullDir, ServerOwnerDir)
	if err := MakeAncestorsDurable(root, dir, 0o755, files); err != nil {
		return nil, err
	}
	return LockOwnedCreate(dir, files)
}

// The kinds of reader lease. A run lease carries its judgment run, so a child
// that inherits it can be bound to that run's work directory alone.
const (
	ReadLease  = "read"
	RunLease   = "run"
	ChildLease = "child"
)

// Lease is one reader's hold on a snapshot: an exclusive lock on a file of its
// own in the snapshot's _leases directory. A removal probes every lease
// without waiting and skips a snapshot any of them holds. Death releases it.
type Lease struct {
	f     *os.File
	path  string
	files FileSystem
}

// dirAndName are the lease directory and the lease file's name.
func (l *Lease) dirAndName() (string, string) { return filepath.Dir(l.path), filepath.Base(l.path) }

// Name is the lease file's name, which a parent passes to the children that
// inherit its run.
func (l *Lease) Name() string { return filepath.Base(l.path) }

// CreateLease makes and locks a new lease file. The caller holds the
// snapshot's _admit gate, so no dead-lease sweep can take the file between
// its creation through files and its lock. kind is ReadLease, ChildLease or
// RunLease; run is the judgment run identifier for a run lease and empty
// otherwise.
func CreateLease(leases, kind, run string, files FileSystem) (*Lease, error) {
	files = writeFileSystem(files)
	d, err := OpenDir(leases)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	for {
		name, err := leaseName(kind, run)
		if err != nil {
			return nil, err
		}
		file, err := files.CreateIn(d, name, PermState)
		if errors.Is(err, fs.ErrExist) {
			// 32 random bytes do not collide, but a collision must not join
			// another reader's lease.
			continue
		}
		// The directory was found and bound: what fails from here is a
		// write of this machine's, never damage to what holds it.
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrLocalWrite, err)
		}
		if err := file.Close(); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrLocalWrite, err)
		}
		f, err := d.open(name, lockExisting, PermState)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrLocalWrite, errors.Join(err, files.RemoveIn(d, name)))
		}
		return &Lease{f: f, path: filepath.Join(leases, name), files: files}, nil
	}
}

func leaseName(kind, run string) (string, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	text := hex.EncodeToString(random)
	switch {
	case kind == RunLease && ValidRun(run):
		return kind + "-" + run + "-" + text + lockFile, nil
	case (kind == ReadLease || kind == ChildLease) && run == "":
		return kind + "-" + text + lockFile, nil
	}
	return "", fmt.Errorf("storage: no %q lease for run %q", kind, run)
}

// ParseLeaseName reads a lease file's name back into its kind and, for a run
// lease, its run. Anything else is not a lease.
func ParseLeaseName(name string) (kind, run string, ok bool) {
	stem, found := strings.CutSuffix(name, lockFile)
	if !found {
		return "", "", false
	}
	parts := strings.Split(stem, "-")
	switch {
	case len(parts) == 2 && (parts[0] == ReadLease || parts[0] == ChildLease) && lowerHex64(parts[1]):
		return parts[0], "", true
	case len(parts) == 3 && parts[0] == RunLease && ValidRun(parts[1]) && lowerHex64(parts[2]):
		return parts[0], parts[1], true
	}
	return "", "", false
}

func lowerHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// ValidRun reports whether run is a judgment run identifier as ReserveRun
// writes it: j, then a positive number in at least six decimal digits, which
// reads back to the same spelling (internal/storage/judgments.go).
func ValidRun(run string) bool {
	digits, ok := strings.CutPrefix(run, "j")
	if !ok || len(digits) < 6 {
		return false
	}
	n, err := strconv.ParseUint(digits, 10, 63)
	return err == nil && n > 0 && fmt.Sprintf("j%06d", n) == run
}

// Release ends the lease: unlock, close, then remove its file. The caller
// holds the snapshot's _admit gate, so a removal probing leases never sees a
// file that is going away as a reader that is still there.
func (l *Lease) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := unlock(l.f)
	err = errors.Join(err, l.f.Close())
	l.f = nil
	dir, name := l.dirAndName()
	d, openErr := OpenDir(dir)
	if openErr != nil {
		return errors.Join(err, openErr)
	}
	if rmErr := writeFileSystem(l.files).RemoveIn(d, name); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
		err = errors.Join(err, rmErr)
	}
	return errors.Join(err, d.Close())
}

// Abandon closes the lease without removing its file, for a command that could
// not take the gate before it had to stop. A later dead-lease sweep removes the
// file under the gate; nothing unlinks a lease file outside it.
func (l *Lease) Abandon() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := errors.Join(unlock(l.f), l.f.Close())
	l.f = nil
	return err
}

// ProbeLease reports whether a live process holds the lease at path, without
// waiting. A lease it could take is released before it returns.
func ProbeLease(path string) (held bool, err error) {
	f, err := lockIn(filepath.Dir(path), filepath.Base(path), lockExisting)
	if errors.Is(err, ErrBusy) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, errors.Join(unlock(f), f.Close())
}

// LeaseState is what a probe found in a snapshot's _leases directory.
type LeaseState struct {
	// Held counts leases a live process holds.
	Held int
	// Dead lists lease files nobody holds, left by a process that died.
	Dead []string
}

// ProbeLeases probes every lease without waiting. The caller holds _admit. A
// file there that is not a lease is an error: nothing else belongs there.
func ProbeLeases(leases string) (LeaseState, error) {
	var state LeaseState
	if err := RealDirectory(leases); err != nil {
		return state, err
	}
	entries, err := os.ReadDir(leases)
	if err != nil {
		return state, err
	}
	for _, entry := range entries {
		if _, _, ok := ParseLeaseName(entry.Name()); !ok || entry.Type() != 0 {
			return state, fmt.Errorf("%w: %s is not a lease", ErrProtected, filepath.Join(leases, entry.Name()))
		}
		path := filepath.Join(leases, entry.Name())
		held, err := ProbeLease(path)
		if err != nil {
			return state, err
		}
		if held {
			state.Held++
		} else {
			state.Dead = append(state.Dead, path)
		}
	}
	return state, nil
}

// SweepDeadLeases removes the lease files no live process holds. The caller
// holds _admit, so no reader is admitted or released meanwhile.
func SweepDeadLeases(leases string, files FileSystem) error {
	state, err := ProbeLeases(leases)
	if err != nil {
		return err
	}
	d, err := OpenDir(leases)
	if err != nil {
		return err
	}
	defer d.Close()
	for _, path := range state.Dead {
		if err := writeFileSystem(files).RemoveIn(d, filepath.Base(path)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
