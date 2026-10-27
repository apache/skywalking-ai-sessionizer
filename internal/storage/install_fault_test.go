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
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage/faulttest"
)

// errStopped stands for a process that died: every call after it fails.
var errStopped = errors.New("the process stopped")

// faultEach runs an operation once cleanly to count its steps, then, for each
// step, three ways it can fail there: the step returns an error and the
// operation cleans up; the process dies at the step and nothing after it runs;
// or the step takes effect and the process dies before learning so. After a
// returned error the operation does nothing more than clean up. final then
// recovers from what is left, after every power loss and on the files left
// visible. setup builds the starting state on a fresh model and returns the
// operation.
func faultEach(t *testing.T, setup func(t *testing.T, model *faulttest.Model) func() error, final func(t *testing.T, recovered *faulttest.Model)) {
	t.Helper()
	clean := controlModel(t)
	run := setup(t, clean)
	start := clean.Point()
	check(t, run())
	steps := clean.Point() - start
	for _, mode := range []string{"returned", "died", "unknown"} {
		for step := 1; step <= steps; step++ {
			model := controlModel(t)
			run := setup(t, model)
			point := model.Point()
			target := point + step
			var failed faulttest.Operation
			stopped := false
			model.Fault = func(op faulttest.Operation) error {
				switch {
				case stopped:
					return errStopped
				case op.Index != target:
					return nil
				case mode == "returned":
					failed = op
					return syscall.EIO
				case mode == "died":
					failed, stopped = op, true
					return errStopped
				}
				// The step runs, and the process is gone from it on, whether
				// the step itself succeeds or fails on its own.
				failed, stopped = op, true
				return nil
			}
			model.After = func(op faulttest.Operation) error {
				if mode == "unknown" && op.Index == target {
					return errStopped
				}
				return nil
			}
			err := run()
			if mode == "returned" && !errors.Is(err, syscall.EIO) || mode != "returned" && err == nil {
				t.Fatalf("%s at step %d: %v", mode, step, err)
			}
			if mode == "returned" {
				for _, op := range model.Operations()[failed.Index:] {
					if op.Kind != "close" && op.Kind != "unlink" {
						t.Fatalf("step %d (%s): %s ran after the failure", step, failed.Kind, op.Kind)
					}
				}
			}
			model.Fault, model.After = nil, nil
			for _, cutoff := range []int{faulttest.DropAll, point, failed.Index} {
				final(t, powerLoss(t, model, cutoff))
			}
			final(t, model)
		}
	}
}

func completeOrAbsent(t *testing.T, path, want string) {
	t.Helper()
	text, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	if err == nil && string(text) != want {
		t.Fatalf("%s is partial: %q", path, text)
	}
}

func TestInstallFaultAtEveryStep(t *testing.T) {
	faultEach(t, func(_ *testing.T, model *faulttest.Model) func() error {
		return func() error {
			temp := filepath.Join(model.Root, ".tmp-x")
			if err := storage.WriteChecked(temp, bytes.NewReader([]byte("evidence")), 8, digestOf("evidence"), model); err != nil {
				return err
			}
			return storage.InstallFile(temp, filepath.Join(model.Root, "a.sd"), model)
		}
	}, func(t *testing.T, recovered *faulttest.Model) {
		dest := filepath.Join(recovered.Root, "a.sd")
		completeOrAbsent(t, dest, "evidence")
		// A later attempt, with a temporary of its own, installs the file or
		// finds the complete one already there.
		temp := filepath.Join(recovered.Root, ".tmp-again")
		check(t, storage.WriteChecked(temp, bytes.NewReader([]byte("evidence")), 8, digestOf("evidence"), recovered))
		if err := storage.InstallFile(temp, dest, recovered); err != nil && !errors.Is(err, storage.ErrExists) {
			t.Fatalf("installing again: %v", err)
		}
		requireText(t, dest, "evidence")
	})
}

func TestShareFaultAtEveryStep(t *testing.T) {
	faultEach(t, func(t *testing.T, model *faulttest.Model) func() error {
		src := filepath.Join(model.Root, "source")
		file, err := model.Create(src, 0o444)
		check(t, err)
		_, err = file.Write([]byte("evidence"))
		check(t, err)
		check(t, file.Sync())
		check(t, file.Close())
		check(t, model.SyncDir(model.Root))
		return func() error {
			_, err := storage.ShareFile(src, filepath.Join(model.Root, "a.sd"), filepath.Join(model.Root, ".tmp-x"), 8, digestOf("evidence"), model)
			return err
		}
	}, func(t *testing.T, recovered *faulttest.Model) {
		dest, src := filepath.Join(recovered.Root, "a.sd"), filepath.Join(recovered.Root, "source")
		completeOrAbsent(t, dest, "evidence")
		requireText(t, src, "evidence")
		if _, err := storage.ShareFile(src, dest, filepath.Join(recovered.Root, ".tmp-again"), 8, digestOf("evidence"), recovered); err != nil && !errors.Is(err, storage.ErrExists) {
			t.Fatalf("sharing again: %v", err)
		}
		requireText(t, dest, "evidence")
	})
}

func TestDetachFaultAtEveryStep(t *testing.T) {
	faultEach(t, func(t *testing.T, model *faulttest.Model) func() error {
		dir := filepath.Join(model.Root, "c1")
		removing := filepath.Join(dir, storage.RemovingDir)
		tree := filepath.Join(dir, testTag)
		check(t, storage.MakeAncestorsDurable(model.Root, removing, 0o755, model))
		check(t, storage.MakeAncestorsDurable(model.Root, tree, 0o755, model))
		return func() error {
			_, err := storage.Detach(tree, removing, testTag, model)
			return err
		}
	}, func(t *testing.T, recovered *faulttest.Model) {
		// Whatever the power loss kept, the tree is in at most one place: at
		// its name or in _removing, never both. The model keeps each
		// directory's entries apart, so a rename whose source directory alone
		// reached the disk loses the tree, which removal allows: its removal
		// record was durable before the rename.
		dir := filepath.Join(recovered.Root, "c1")
		removing := filepath.Join(dir, storage.RemovingDir)
		_, atName := os.Lstat(filepath.Join(dir, testTag))
		entries, err := os.ReadDir(removing)
		check(t, err)
		if (atName == nil && len(entries) != 0) || len(entries) > 1 {
			t.Fatalf("the tree is at its name: %v, and in _removing %d times", atName == nil, len(entries))
		}
		// Recovery detaches what is still at its name and deletes what is in
		// _removing, until the tree is nowhere.
		if atName == nil {
			if _, err := storage.Detach(filepath.Join(dir, testTag), removing, testTag, recovered); err != nil {
				t.Fatalf("detaching again: %v", err)
			}
		}
		entries, err = os.ReadDir(removing)
		check(t, err)
		for _, entry := range entries {
			check(t, storage.DeleteOwned(removing, entry.Name(), recovered))
		}
		if entries, _ := os.ReadDir(removing); len(entries) != 0 {
			t.Fatalf("left in _removing: %v", entries)
		}
	})
}

func TestDeleteOwnedFaultAtEveryStep(t *testing.T) {
	faultEach(t, func(t *testing.T, model *faulttest.Model) func() error {
		building := filepath.Join(model.Root, storage.BuildingDir)
		files := filepath.Join(building, "b", "_work", "files")
		check(t, storage.MakeAncestorsDurable(model.Root, files, 0o755, model))
		for _, name := range []string{"x.tmp", "y.tmp"} {
			file, err := model.Create(filepath.Join(files, name), 0o444)
			check(t, err)
			check(t, file.Sync())
			check(t, file.Close())
		}
		check(t, model.SyncDir(files))
		return func() error { return storage.DeleteOwned(building, "b", model) }
	}, func(t *testing.T, recovered *faulttest.Model) {
		// An owned tree is never read, so what a stop leaves is deleted by
		// running again, through the recovered model.
		building := filepath.Join(recovered.Root, storage.BuildingDir)
		check(t, storage.DeleteOwned(building, "b", recovered))
		if _, err := os.Lstat(filepath.Join(building, "b")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("a second run left the tree: %v", err)
		}
	})
}
