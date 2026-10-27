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

//go:build linux || darwin

package storage

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Only the two link errors, or a file system the probe shows has no links,
// turn a failed link into a copy. A link refused for permission on a file
// system that has links must stop: copying would hide the refusal.
func TestLinkUnavailable(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name  string
		err   error
		links bool
		want  bool
	}{
		{"another device", syscall.EXDEV, true, true},
		{"link limit", syscall.EMLINK, true, true},
		{"permission, file system has links", syscall.EPERM, true, false},
		{"permission, file system lacks links", syscall.EPERM, false, true},
		{"not supported, file system lacks links", syscall.ENOTSUP, false, true},
		{"operation not supported, file system lacks links", syscall.EOPNOTSUPP, false, true},
		{"input or output error", syscall.EIO, false, false},
		{"disk full", syscall.ENOSPC, false, false},
		{"exists", syscall.EEXIST, false, false},
	} {
		saved := linksWork
		linksWork = func(string, FileSystem) (bool, error) { return tc.links, nil }
		got, err := linkUnavailable(&os.LinkError{Op: "link", Old: "a", New: "b", Err: tc.err}, dir, nil)
		linksWork = saved
		if err != nil || got != tc.want {
			t.Fatalf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The probe uses a fresh file of its own and leaves nothing behind. A probe
// that cannot start never turns the original error into a copy.
func TestProbeLinksLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	if works, err := probeLinks(dir, nil); err != nil || !works {
		t.Fatalf("the test directory's file system has hard links: %v, %v", works, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("left behind: %v, %v", entries, err)
	}
	if works, err := probeLinks(filepath.Join(dir, "missing"), nil); !works || err == nil {
		t.Fatalf("a failed probe: %v, %v", works, err)
	}
}

// A probe that cannot remove what it made reports it.
func TestProbeLinksReportsCleanupFailure(t *testing.T) {
	dir := t.TempDir()
	failure := errors.New("unlink failed")
	files := failingRemove{err: failure}
	if _, err := probeLinks(dir, files); !errors.Is(err, failure) {
		t.Fatalf("cleanup failure: %v", err)
	}
}

type failingRemove struct {
	DiskFileSystem
	err error
}

func (f failingRemove) Remove(string) error { return f.err }

// A shared file is one file with two names: the link count says so, and the
// source keeps its mode. Content alone could not tell a link from a copy.
func TestShareFileLinks(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	if err := os.WriteFile(src, []byte("evidence"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "a.sd")
	copied, err := ShareFile(src, dest, filepath.Join(dir, ".tmp-x"), 8, sha256.Sum256([]byte("evidence")), nil)
	if err != nil || copied {
		t.Fatalf("copied %v, %v", copied, err)
	}
	info, err := os.Lstat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if links := info.Sys().(*syscall.Stat_t).Nlink; links != 2 {
		t.Fatalf("link count %d", links)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("sharing changed the mode: %v", info.Mode().Perm())
	}
}

// Where links are unavailable, the copy is checked and read-only.
func TestShareFileCopiesWithoutLinks(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	if err := os.WriteFile(src, []byte("evidence"), 0o644); err != nil {
		t.Fatal(err)
	}
	saved := linksWork
	linksWork = func(string, FileSystem) (bool, error) { return false, nil }
	defer func() { linksWork = saved }()
	files := failingLinks{err: &os.LinkError{Op: "link", Err: syscall.EPERM}}
	dest := filepath.Join(dir, "a.sd")
	copied, err := ShareFile(src, dest, filepath.Join(dir, ".tmp-x"), 8, sha256.Sum256([]byte("evidence")), files)
	if err != nil || !copied {
		t.Fatalf("copied %v, %v", copied, err)
	}
	info, err := os.Lstat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Sys().(*syscall.Stat_t).Nlink != 1 || info.Mode().Perm() != 0o444 {
		t.Fatalf("not a separate read-only copy: %v", info.Mode())
	}
	// Any other link failure stops, and nothing is copied.
	files.err = &os.LinkError{Op: "link", Err: syscall.EIO}
	other := filepath.Join(dir, "b.sd")
	if _, err := ShareFile(src, other, filepath.Join(dir, ".tmp-y"), 8, sha256.Sum256([]byte("evidence")), files); !errors.Is(err, syscall.EIO) {
		t.Fatalf("other link failure: %v", err)
	}
	if _, err := os.Lstat(other); !os.IsNotExist(err) {
		t.Fatalf("copied after a link failure that must stop: %v", err)
	}
}

type failingLinks struct {
	DiskFileSystem
	err error
}

func (f failingLinks) Link(string, string) error { return f.err }
