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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// swapForLink renames dir away and puts a link to target at its name, as a
// process racing a check would. It returns where dir went.
func swapForLink(t *testing.T, dir, target string) string {
	t.Helper()
	moved := dir + "-moved"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, dir); err != nil {
		t.Fatal(err)
	}
	return moved
}

// A file is created in the directory that was checked, not where a link put
// at its name after the check leads.
func TestCheckedDirCreatesInTheDirectoryItChecked(t *testing.T) {
	base := t.TempDir()
	logs := filepath.Join(base, "logs")
	pull := filepath.Join(base, "root", PullDir)
	for _, dir := range []string{logs, pull} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	d, err := OpenDir(logs)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	moved := swapForLink(t, logs, pull)
	f, err := d.open("navigation.log", writeNew, PermState)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(moved, "navigation.log")); err != nil {
		t.Fatalf("not created in the checked directory: %v", err)
	}
	if entries, _ := os.ReadDir(pull); len(entries) != 0 {
		t.Fatalf("created through the link: %v", entries)
	}
}

// A lock is taken on the checked directory's file, so a second owner that
// follows the swapped path never shares it.
func TestCheckedDirLocksInTheDirectoryItChecked(t *testing.T) {
	base := t.TempDir()
	gate := filepath.Join(base, "c1", AdmitDir)
	other := filepath.Join(base, "other")
	for _, dir := range []string{gate, other} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, lockFile), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	d, err := OpenDir(gate)
	if err != nil {
		t.Fatal(err)
	}
	moved := swapForLink(t, gate, other)
	held, err := d.open(lockFile, lockExisting, PermState)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := lockIn(moved, lockFile, lockExisting); !errors.Is(err, ErrBusy) {
		t.Fatalf("the checked directory's lock is not the one held: %v", err)
	}
	// The swapped name is now a link, which a new lock refuses outright.
	if _, err := lockIn(gate, lockFile, lockExisting); !errors.Is(err, ErrProtected) {
		t.Fatalf("a lock through the link: %v", err)
	}
}

// No open follows a link in the name it opens, for reading or appending.
func TestCheckedDirRefusesALinkedName(t *testing.T) {
	dir := t.TempDir()
	evidence := filepath.Join(dir, "a.sd")
	if err := os.WriteFile(evidence, []byte("evidence"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(evidence, filepath.Join(dir, "log")); err != nil {
		t.Fatal(err)
	}
	d, err := OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, how := range []fileOpen{readShared, appendExisting, lockExisting, writeNew, lockNew} {
		if f, err := d.open("log", how, PermState); err == nil {
			f.Close()
			t.Fatalf("open %d followed a link", how)
		}
	}
	if text, err := os.ReadFile(evidence); err != nil || string(text) != "evidence" {
		t.Fatalf("evidence changed: %q, %v", text, err)
	}
}

// A path classified by its components is the place the work happens: an
// ancestor replaced by a link after the classification, here leading into a
// pull server's tree that holds a directory of the same name, is refused
// rather than followed.
func TestOpenDirRefusesAnAncestorReplacedAfterTheCheck(t *testing.T) {
	base := t.TempDir()
	safe := filepath.Join(base, "safe")
	leaf := filepath.Join(safe, "a", "leaf")
	pullLeaf := filepath.Join(base, "root", PullDir, "leaf")
	for _, dir := range []string{leaf, pullLeaf} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	resolved, err := CheckSuppliedPath(filepath.Join(leaf, "navigation.log"), "")
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(safe, "a-moved")
	if err := os.Rename(filepath.Join(safe, "a"), moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "root", PullDir), filepath.Join(safe, "a")); err != nil {
		t.Fatal(err)
	}
	// Binding follows the link above the directory to where it leads now, and
	// says so; the supplied path is then refused by where it really is.
	d, err := OpenDir(filepath.Dir(resolved))
	if err != nil {
		t.Fatal(err)
	}
	bound := d.Real()
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bound, PullDir) {
		t.Fatalf("bound %s, not where the link leads", bound)
	}
	if _, err := CreateSupplied(filepath.Join(leaf, "navigation.log"), ""); !errors.Is(err, ErrProtected) {
		t.Fatalf("created through an ancestor replaced by a link: %v", err)
	}
	if entries, _ := os.ReadDir(pullLeaf); len(entries) != 0 {
		t.Fatalf("created inside the pull tree: %v", entries)
	}
}

// A lock file and a lease are made in the directory that was bound, even when
// the directory is replaced by a link before they are made.
func TestCreateInUsesTheBoundDirectory(t *testing.T) {
	base := t.TempDir()
	leases := filepath.Join(base, "snapshot", LeasesDir)
	other := filepath.Join(base, "other")
	for _, dir := range []string{leases, other} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	d, err := OpenDir(leases)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	moved := swapForLink(t, leases, other)
	file, err := DiskFileSystem{}.CreateIn(d, "read-x.lock", PermState)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(moved, "read-x.lock")); err != nil {
		t.Fatalf("not made in the bound directory: %v", err)
	}
	if entries, _ := os.ReadDir(other); len(entries) != 0 {
		t.Fatalf("made through the link: %v", entries)
	}
	// Removal through the bound directory refuses once the path no longer
	// leads to it, and the file of that name where the link leads stays.
	if err := os.WriteFile(filepath.Join(other, "read-x.lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (DiskFileSystem{}).RemoveIn(d, "read-x.lock"); err == nil {
		t.Fatal("removed through a path that no longer leads to the bound directory")
	}
	if _, err := os.Lstat(filepath.Join(other, "read-x.lock")); err != nil {
		t.Fatalf("removed the file the link leads to: %v", err)
	}
}

// A supplied path is classified again where its parent was bound: an ancestor
// replaced by a link into a pull server's tree between the first check and
// the open is refused, and nothing is made there.
func TestOpenResolvedClassifiesWhereItBinds(t *testing.T) {
	base := t.TempDir()
	safe := filepath.Join(base, "safe")
	leaf := filepath.Join(safe, "a", "leaf")
	pullLeaf := filepath.Join(base, "root", PullDir, "leaf")
	for _, dir := range []string{leaf, pullLeaf} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	resolved, err := CheckSuppliedPath(filepath.Join(leaf, "navigation.log"), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(safe, "a"), filepath.Join(safe, "a-moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "root", PullDir), filepath.Join(safe, "a")); err != nil {
		t.Fatal(err)
	}
	if f, err := openResolved(resolved, "", writeNew); !errors.Is(err, ErrProtected) {
		if f != nil {
			f.Close()
		}
		t.Fatalf("opened through an ancestor replaced after the check: %v", err)
	}
	if entries, _ := os.ReadDir(pullLeaf); len(entries) != 0 {
		t.Fatalf("made inside the pull tree: %v", entries)
	}
}

// A handle is bound only to the directory the descent reached: one replaced
// by another real directory at the same path in between is refused.
func TestBindRefusesADirectoryReplacedAfterTheDescent(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "c1", AdmitDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	named, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	reached, err := descendTo(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, dir+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if d, err := bind(dir, resolved, named, reached); !errors.Is(err, ErrProtected) {
		if d != nil {
			d.Close()
		}
		t.Fatalf("bound a directory replaced after the descent: %v", err)
	}
}
