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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

func TestACopyIsKnownByItsMarker(t *testing.T) {
	root := t.TempDir()
	if copied, err := storage.IsCopy(root); err != nil || copied {
		t.Fatalf("a root with no marker says %v, %v", copied, err)
	}
	if copied, err := storage.IsCopy(filepath.Join(root, "missing")); err != nil || copied {
		t.Fatalf("a directory that does not exist says %v, %v", copied, err)
	}
	if err := os.WriteFile(filepath.Join(root, storage.CopyMarker), nil, 0o444); err != nil {
		t.Fatal(err)
	}
	if copied, err := storage.IsCopy(root); err != nil || !copied {
		t.Fatalf("a root with its marker says %v, %v", copied, err)
	}
}

func TestRefuseCopyNamesTheDirectory(t *testing.T) {
	root := t.TempDir()
	if err := storage.RefuseCopy(root); err != nil {
		t.Fatalf("a root with no marker was refused: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, storage.CopyMarker), nil, 0o444); err != nil {
		t.Fatal(err)
	}
	err := storage.RefuseCopy(root)
	if !errors.Is(err, storage.ErrCopy) {
		t.Fatalf("a root with its marker gave %v, want ErrCopy", err)
	}
	if !strings.Contains(err.Error(), root) {
		t.Fatalf("the refusal does not name %s: %v", root, err)
	}
}

// Refusing a copy must leave it untouched, including the lock's directory.
// Each entry starts with a new root so one lock cannot hide another's writes.
func TestRootLocksRefuseACopyWithoutWriting(t *testing.T) {
	for _, tc := range []struct {
		name string
		lock func(string) (*storage.SessionLock, error)
	}{
		{"LockRoot", storage.LockRoot},
		{"LockRootWait", func(root string) (*storage.SessionLock, error) {
			return storage.LockRootWait(root, 0)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := markedCopyRoot(t)
			lock, err := tc.lock(root)
			if lock != nil {
				if unlockErr := lock.Unlock(); unlockErr != nil {
					t.Errorf("unlock: %v", unlockErr)
				}
				t.Error("a copy was locked")
			}
			if !errors.Is(err, storage.ErrCopy) {
				t.Errorf("a root with its marker gave %v, want ErrCopy", err)
			}
			if _, err := os.Lstat(filepath.Join(root, storage.RootLockDir)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the root lock's directory was made: %v", err)
			}
			if items, err := os.ReadDir(root); err != nil || len(items) != 1 || items[0].Name() != storage.CopyMarker {
				t.Errorf("the refused root holds %v, %v; want only %s", items, err, storage.CopyMarker)
			}
		})
	}
}

// The judge may write a copy, but it must still exclude another writer.
func TestLockRootInCopyTakesACopy(t *testing.T) {
	root := markedCopyRoot(t)
	held, err := storage.LockRootInCopy(root)
	if err != nil {
		t.Fatal(err)
	}
	if held == nil {
		t.Fatal("no lock was returned")
	}
	defer func() { _ = held.Unlock() }()
	if _, err := os.Stat(filepath.Join(root, storage.RootLockDir, ".lock")); err != nil {
		t.Fatalf("the lock is not under %s: %v", storage.RootLockDir, err)
	}
	again, err := storage.LockRootInCopy(root)
	if again != nil {
		if unlockErr := again.Unlock(); unlockErr != nil {
			t.Errorf("unlock: %v", unlockErr)
		}
		t.Error("a second lock was returned")
	}
	if !errors.Is(err, storage.ErrRootBusy) {
		t.Fatalf("a second LockRootInCopy gave %v, want ErrRootBusy", err)
	}
	if err := held.Unlock(); err != nil {
		t.Fatal(err)
	}
	again, err = storage.LockRootInCopy(root)
	if err != nil {
		t.Fatalf("the lock was not free once released: %v", err)
	}
	if again == nil {
		t.Fatal("no lock was returned after release")
	}
	if err := again.Unlock(); err != nil {
		t.Fatal(err)
	}
}

// The judge adds its runs to a copied snapshot, whose marker must stay in place.
func TestReserveRunInACopy(t *testing.T) {
	root := markedCopyRoot(t)
	z := storage.NewZone(root)
	run, dir, err := storage.ReserveRun(z, "s1")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(z.SessionDir("s1"), storage.JudgmentsDir, "j000001")
	if run != 1 || dir != want {
		t.Fatalf("reserved run %d at %q; want run 1 at %q", run, dir, want)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("the run's directory was not made: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, storage.CopyMarker)); err != nil {
		t.Fatalf("the copy's marker is gone: %v", err)
	}
}

func markedCopyRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, storage.CopyMarker), nil, 0o444); err != nil {
		t.Fatal(err)
	}
	return root
}
