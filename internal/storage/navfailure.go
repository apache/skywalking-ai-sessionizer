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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FailurePrefix starts the name of a chunk attempt's failure marker on a
// pulled snapshot. The rest of the name is the random part of the run lease
// that issued it, so a later regroup never takes an earlier run's marker for
// its own.
const FailurePrefix = ".snapshot-failure-"

// checkPrefix starts the name of a chunk's gate, in the chunk directory above
// its attempt directories.
const checkPrefix = ".snapshot-check-"

// MaxFailureReport bounds what a fatal marker says about its read.
const MaxFailureReport = 4 << 10

// FailureGate is the gate of the chunk a failure marker belongs to:
// .snapshot-check-<random>.lock in the directory above the marker's attempt
// directory. Every attempt of the chunk shares it.
func FailureGate(marker string) (string, error) {
	base := filepath.Base(marker)
	random, ok := strings.CutPrefix(base, FailurePrefix)
	if !ok || random == "" {
		return "", fmt.Errorf("storage: %s is not a failure marker", marker)
	}
	chunk := filepath.Dir(filepath.Dir(marker))
	return filepath.Join(chunk, checkPrefix+random+".lock"), nil
}

// LockFailureGate takes a chunk's gate, waiting for a busy gate until ctx
// ends. The gate is never held over a read of evidence: it orders the making
// and removing of markers against the parent's check of them.
func LockFailureGate(ctx context.Context, gate string) (*SessionLock, error) {
	return LockFileWait(ctx, gate)
}

// TryLockFile takes an exclusive lock on the file path without waiting,
// making the file when it is not there yet, through its directory's checked
// handle and without following a link in its name. A held lock is ErrBusy.
func TryLockFile(path string) (*SessionLock, error) {
	d, err := OpenDir(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer d.Close()
	name := filepath.Base(path)
	for {
		f, err := d.open(name, lockExisting, PermState)
		if errors.Is(err, fs.ErrNotExist) {
			f, err = d.open(name, lockNew, PermState)
			if errors.Is(err, fs.ErrExist) {
				continue
			}
		}
		if err != nil {
			return nil, err
		}
		return &SessionLock{f: f}, nil
	}
}

// ProbeFileLock says whether another holds the lock on an existing file,
// without waiting and without making anything. A missing file is not held.
func ProbeFileLock(path string) (held bool, err error) {
	l, err := lockIn(filepath.Dir(path), filepath.Base(path), lockExisting)
	switch {
	case errors.Is(err, ErrBusy):
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	}
	return false, l.Close()
}

// LockFileWait takes an exclusive lock on the file path, making it when it is
// not there yet, through its directory's checked handle and without following
// a link in its name. A busy lock is waited for until ctx ends.
func LockFileWait(ctx context.Context, path string) (*SessionLock, error) {
	d, err := OpenDir(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer d.Close()
	name := filepath.Base(path)
	for {
		f, err := d.open(name, lockExisting, PermState)
		if errors.Is(err, fs.ErrNotExist) {
			f, err = d.open(name, lockNew, PermState)
			if errors.Is(err, fs.ErrExist) {
				// Another holder made it first. Lock the one it made.
				continue
			}
		}
		if err == nil {
			return &SessionLock{f: f}, nil
		}
		if !errors.Is(err, ErrBusy) {
			return nil, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// Marker is a failure marker a read made, held open by the read until it
// ends. An empty marker says a read is pending.
type Marker struct {
	path string
	f    *os.File
	info fs.FileInfo
}

// CreateMarker makes a pending marker at path, which must not exist, without
// following a link there. An existing marker is never taken over.
func CreateMarker(path string) (*Marker, error) {
	d, err := OpenDir(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer d.Close()
	f, err := d.open(filepath.Base(path), writeNew, PermState)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return &Marker{path: path, f: f, info: info}, nil
}

// Fatal marks the read fatal in place and leaves the marker. The report is
// cut to MaxFailureReport bytes. A marker that cannot be written stays
// pending, which a parent reads the same way.
func (m *Marker) Fatal(report string) error {
	if len(report) > MaxFailureReport {
		report = report[:MaxFailureReport]
	}
	_, err := m.f.WriteString(report)
	if err == nil {
		err = syncFileHandle(m.f)
	}
	return errors.Join(err, m.f.Close())
}

// Remove removes the marker after its read ended well. It removes only the
// file this read made: a name that now holds another file is left, and so is
// the marker when anything here fails.
func (m *Marker) Remove() error {
	if err := m.f.Close(); err != nil {
		return err
	}
	d, err := OpenDir(filepath.Dir(m.path))
	if err != nil {
		return err
	}
	defer d.Close()
	name := filepath.Base(m.path)
	info, err := d.lstat(name)
	if err != nil {
		return err
	}
	if !os.SameFile(info, m.info) {
		return fmt.Errorf("storage: %s is no longer the marker this read made", m.path)
	}
	return d.remove(name)
}

// Close lets go of a marker without removing it, which leaves it pending.
func (m *Marker) Close() error { return m.f.Close() }

// MarkerPresent says whether a marker is at path, without following a link
// there. Anything at the name counts: a parent fails on what it cannot rule
// out.
func MarkerPresent(path string) (bool, error) {
	d, err := OpenDir(filepath.Dir(path))
	if err != nil {
		return false, err
	}
	defer d.Close()
	_, err = d.lstat(filepath.Base(path))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, err
}
