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

package storage_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage/faulttest"
)

// treeOf lists every entry below dir with its kind, size and mode, so a test
// can tell that nothing in it changed.
func treeOf(t *testing.T, dir string) string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		size := info.Size()
		if info.IsDir() {
			size = 0
		}
		out = append(out, fmt.Sprintf("%s %s %d", rel, info.Mode(), size))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(out, "\n")
}

// twoRoots makes the same conversation layout in two storage roots, a and b,
// with a published tag and its leases directory in each.
func twoRoots(t *testing.T) (a, b string) {
	t.Helper()
	base := t.TempDir()
	a, b = filepath.Join(base, "a"), filepath.Join(base, "b")
	for _, root := range []string{a, b} {
		for _, dir := range []string{"_pull", "20261025T103200.000000000Z/" + storage.LeasesDir, "20261025T103200.000000000Z/" + storage.RootLockDir} {
			if err := os.MkdirAll(filepath.Join(root, storage.RemoteDir, "prod", "c1", dir), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(root, storage.RemoteDir, "prod", "c1", "kept"), []byte("kept"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return a, b
}

// replaceWithLink renames dir away and puts a link to target at its name.
func replaceWithLink(t *testing.T, dir, target string) {
	t.Helper()
	if err := os.Rename(dir, dir+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, dir); err != nil {
		t.Fatal(err)
	}
}

// A layout directory replaced by a link before anything is bound leads no
// operation into the tree the link names: every lock, lease, marker and
// FileSystem operation below it is refused, and the other tree is unchanged.
func TestALayoutLinkMadeBeforeBindingIsRefused(t *testing.T) {
	a, b := twoRoots(t)
	before := treeOf(t, b)
	replaceWithLink(t, filepath.Join(a, storage.RemoteDir), filepath.Join(b, storage.RemoteDir))
	conversation := filepath.Join(a, storage.RemoteDir, "prod", "c1")
	tag := filepath.Join(conversation, "20261025T103200.000000000Z")
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	disk := storage.DiskFileSystem{}
	for name, op := range map[string]func() error{
		"lock": func() error {
			l, err := storage.LockOwnedCreate(filepath.Join(conversation, storage.PullDir), nil)
			if err == nil {
				_ = l.Unlock()
			}
			return err
		},
		"lease": func() error {
			l, err := storage.CreateLease(filepath.Join(tag, storage.LeasesDir), storage.ReadLease, "", nil)
			if err == nil {
				_ = l.Release()
			}
			return err
		},
		"open": func() error {
			d, err := storage.OpenDir(conversation)
			if err == nil {
				_ = d.Close()
			}
			return err
		},
		"marker": func() error { _, err := storage.ReadOuterCopy(conversation, "prod", "c1"); return err },
		"mkdir":  func() error { return disk.Mkdir(filepath.Join(conversation, storage.BuildingDir), 0o755) },
		"create": func() error {
			f, err := disk.Create(filepath.Join(conversation, "new"), 0o600)
			if err == nil {
				_ = f.Close()
			}
			return err
		},
		"remove": func() error { return disk.Remove(filepath.Join(conversation, "kept")) },
		"chmod":  func() error { return disk.Chmod(filepath.Join(conversation, "kept"), 0o600) },
		"rename": func() error {
			return disk.Rename(filepath.Join(conversation, "kept"), filepath.Join(conversation, "moved"))
		},
		"install": func() error {
			return disk.RenameNoReplace(filepath.Join(conversation, "kept"), filepath.Join(tag, "moved"))
		},
		"replace":  func() error { return disk.Replace(conversation, "kept", "moved") },
		"link":     func() error { return disk.Link(outside, filepath.Join(conversation, "linked")) },
		"sync":     func() error { return disk.SyncDir(conversation) },
		"syncfile": func() error { return disk.SyncFile(filepath.Join(conversation, "kept")) },
	} {
		if err := op(); !errors.Is(err, storage.ErrProtected) {
			t.Errorf("%s: %v, want the layout link refused", name, err)
		}
	}
	if after := treeOf(t, b); after != before {
		t.Fatalf("the tree the link leads to changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// Bootstrap checks the layout as it makes it, then locks and writes. A
// layout directory replaced by a link before any one of its operations stops
// it there: nothing reaches the tree the link names.
func TestBootstrapRefusesALayoutLinkMadeDuringIt(t *testing.T) {
	count := func() int {
		root := t.TempDir()
		model, err := faulttest.New(root)
		if err != nil {
			t.Fatal(err)
		}
		_, lock, err := storage.BootstrapConversation(context.Background(), root, "prod", "c1", model)
		if err != nil {
			t.Fatal(err)
		}
		if err := lock.Unlock(); err != nil {
			t.Fatal(err)
		}
		return len(model.Operations())
	}()
	// The link is made once the layout below _remote exists, so a refusal
	// is the layout check's and not a missing directory's.
	refused := 0
	for at := 1; at <= count; at++ {
		base := t.TempDir()
		a, b := filepath.Join(base, "a"), filepath.Join(base, "b")
		for _, dir := range []string{a, filepath.Join(b, storage.RemoteDir, "prod", "c1", storage.PullDir)} {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		before := treeOf(t, b)
		model, err := faulttest.New(a)
		if err != nil {
			t.Fatal(err)
		}
		swapped := false
		model.Fault = func(op faulttest.Operation) error {
			if op.Index != at || swapped {
				return nil
			}
			if _, err := os.Lstat(filepath.Join(a, storage.RemoteDir, "prod", "c1")); err != nil {
				return nil
			}
			swapped = true
			replaceWithLink(t, filepath.Join(a, storage.RemoteDir), filepath.Join(b, storage.RemoteDir))
			return nil
		}
		_, lock, err := storage.BootstrapConversation(context.Background(), a, "prod", "c1", model)
		// Where the link leads to a tree without the directory the next
		// operation names, it fails as not found before the layout check:
		// a refusal all the same.
		if err == nil {
			_ = lock.Unlock()
		} else if swapped && errors.Is(err, storage.ErrProtected) {
			refused++
		}
		if after := treeOf(t, b); after != before {
			t.Fatalf("operation %d: the tree the link leads to changed:\nbefore:\n%s\nafter:\n%s", at, before, after)
		}
	}
	if refused == 0 {
		t.Fatal("no swap reached an operation that binds the layout")
	}
}
