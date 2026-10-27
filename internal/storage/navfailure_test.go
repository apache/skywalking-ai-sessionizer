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
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func attemptDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "2-targets", "c001", "a1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A chunk's gate sits above its attempts and is named for the same lease as
// their markers. Anything that is not a marker has no gate.
func TestAMarkersGateIsItsChunks(t *testing.T) {
	gate, err := FailureGate(filepath.Join("w", "2-targets", "c001", "a2", FailurePrefix+"abc"))
	if err != nil || gate != filepath.Join("w", "2-targets", "c001", ".snapshot-check-abc.lock") {
		t.Fatalf("gate %q, %v", gate, err)
	}
	for _, name := range []string{"mcp-calls.jsonl", FailurePrefix, ".snapshot-failure"} {
		if _, err := FailureGate(filepath.Join("w", "c001", "a1", name)); err == nil {
			t.Errorf("%q has a gate", name)
		}
	}
}

// A marker is made once: an existing one, a link in its place included, is
// never taken over.
func TestAMarkerIsNeverTakenOver(t *testing.T) {
	dir := attemptDir(t)
	path := filepath.Join(dir, FailurePrefix+"r")
	m, err := CreateMarker(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CreateMarker(path); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("a second marker: %v", err)
	}
	if err := m.Fatal("fatal: first\n"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "fatal: first\n" {
		t.Fatalf("the marker says %q", b)
	}
	if runtime.GOOS == "windows" {
		return
	}
	target := filepath.Join(t.TempDir(), "elsewhere")
	link := filepath.Join(dir, FailurePrefix+"l")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateMarker(link); err == nil {
		t.Fatal("a marker was made through a link")
	}
	if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the link's target was made: %v", err)
	}
}

// A fatal report is cut to its bound.
func TestAFatalReportIsBounded(t *testing.T) {
	path := filepath.Join(attemptDir(t), FailurePrefix+"r")
	m, err := CreateMarker(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Fatal(strings.Repeat("x", 3*MaxFailureReport)); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Size() != MaxFailureReport {
		t.Fatalf("the report: %v, %v", info, err)
	}
}

// A read removes only the marker it made. A name that now holds another file
// is left, and so is that file.
func TestARemovalTakesOnlyItsOwnMarker(t *testing.T) {
	path := filepath.Join(attemptDir(t), FailurePrefix+"r")
	m, err := CreateMarker(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(); err != nil {
		t.Fatal(err)
	}
	if present, err := MarkerPresent(path); err != nil || present {
		t.Fatalf("after its removal: %v %v", present, err)
	}
	m, err = CreateMarker(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		// The marker is held open without delete sharing, so no one can take
		// its name from under the read.
		if err := os.Remove(path); err == nil {
			t.Fatal("a marker a read holds was removed")
		}
		if err := m.Remove(); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("fatal: another\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(); err == nil {
		t.Fatal("a read removed a marker it did not make")
	}
	if b, _ := os.ReadFile(path); string(b) != "fatal: another\n" {
		t.Fatalf("the other marker is now %q", b)
	}
}

// Anything at a marker's name counts as a marker, and what cannot be looked
// at is an error, never absence.
func TestAnythingAtAMarkersNameIsAMarker(t *testing.T) {
	dir := attemptDir(t)
	empty := filepath.Join(dir, FailurePrefix+"e")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(dir, FailurePrefix+"d")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	unreadable := filepath.Join(dir, FailurePrefix+"u")
	if err := os.WriteFile(unreadable, []byte("x"), 0o000); err != nil {
		t.Fatal(err)
	}
	present := []string{empty, directory, unreadable}
	if runtime.GOOS != "windows" {
		link := filepath.Join(dir, FailurePrefix+"l")
		if err := os.Symlink(filepath.Join(dir, "nothing"), link); err != nil {
			t.Fatal(err)
		}
		present = append(present, link)
	}
	for _, p := range present {
		if got, err := MarkerPresent(p); err != nil || !got {
			t.Errorf("%s: %v %v", filepath.Base(p), got, err)
		}
	}
	if got, err := MarkerPresent(filepath.Join(dir, FailurePrefix+"none")); err != nil || got {
		t.Errorf("no marker: %v %v", got, err)
	}
	if _, err := MarkerPresent(filepath.Join(dir, "gone", FailurePrefix+"x")); err == nil {
		t.Error("a missing attempt directory was taken for no marker")
	}
}

// The gate is made by whoever comes first, and waits while it is held.
func TestTheGateIsMadeOnceAndWaitedFor(t *testing.T) {
	dir := attemptDir(t)
	gate, err := FailureGate(filepath.Join(dir, FailurePrefix+"r"))
	if err != nil {
		t.Fatal(err)
	}
	held, err := LockFailureGate(context.Background(), gate)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := LockFailureGate(ctx, gate); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a held gate: %v", err)
	}
	got := make(chan error, 1)
	go func() {
		l, err := LockFailureGate(context.Background(), gate)
		if err == nil {
			err = l.Unlock()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("the gate was taken while held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := held.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := <-got; err != nil {
		t.Fatal(err)
	}
}
