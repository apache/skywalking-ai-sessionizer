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
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage/faulttest"
)

func TestAncestorsEveryAttempt(t *testing.T) {
	model := controlModel(t)
	directory := filepath.Join(model.Root, "remote", "conversation")
	if err := storage.MakeAncestorsDurable(model.Root, directory, 0o755, model); err != nil {
		t.Fatal(err)
	}
	start := model.Point()
	if err := storage.SyncAncestors(model.Root, directory, model); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, op := range model.Operations()[start:] {
		if op.Kind != "directory sync" {
			t.Fatalf("unexpected operation: %+v", op)
		}
		paths = append(paths, op.Path)
	}
	want := []string{filepath.Dir(model.Root), model.Root, model.Root, filepath.Join(model.Root, "remote"), filepath.Join(model.Root, "remote"), directory}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("ancestor sync order: %v, want %v", paths, want)
	}
}

func TestAncestorsStopAtEachSync(t *testing.T) {
	for failure := 1; failure <= 6; failure++ {
		model := controlModel(t)
		count := 0
		model.Fault = func(op faulttest.Operation) error {
			if op.Kind == "directory sync" {
				count++
				if count == failure {
					return syscall.EIO
				}
			}
			return nil
		}
		directory := filepath.Join(model.Root, "remote", "conversation")
		if err := storage.MakeAncestorsDurable(model.Root, directory, 0o755, model); !errors.Is(err, syscall.EIO) {
			t.Fatalf("sync %d: %v", failure, err)
		}
		if count != failure {
			t.Fatalf("continued after sync %d: %d syncs", failure, count)
		}
		operations := model.Operations()
		if operations[len(operations)-1].Kind != "directory sync" {
			t.Fatal("dependent write followed failed ancestor sync")
		}
	}
}

func TestInterruptedAncestorsCanBecomeDurable(t *testing.T) {
	model := controlModel(t)
	parent := filepath.Join(model.Root, "remote")
	if err := model.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	// No sync was attempted before the process stopped.
	point := model.Point()
	directory := filepath.Join(parent, "conversation")
	if err := storage.MakeAncestorsDurable(model.Root, directory, 0o755, model); err != nil {
		t.Fatal(err)
	}
	if err := storage.ReplaceControlDurable(filepath.Join(directory, "record"), storage.PermState, controlText("complete"), model); err != nil {
		t.Fatal(err)
	}
	for _, cutoff := range []int{faulttest.DropAll, point} {
		recovered := powerLoss(t, model, cutoff)
		requireText(t, filepath.Join(recovered.Root, "remote", "conversation", "record"), "complete")
	}
}

func TestFailedAncestorCanLoseWholeTree(t *testing.T) {
	for _, parts := range [][]string{{"_remote"}, {"_remote", "remote"}, {"_remote", "remote", "conversation"}, {"_pull"}, {"_pull", "conversations"}, {"_pull", "conversations", "conversation"}} {
		t.Run(filepath.Join(parts...), func(t *testing.T) {
			model := controlModel(t)
			lost := filepath.Join(append([]string{model.Root}, parts...)...)
			parent := filepath.Dir(lost)
			if err := storage.MakeAncestorsDurable(model.Root, parent, 0o755, model); err != nil {
				t.Fatal(err)
			}
			point := model.Point()
			if err := model.Mkdir(lost, 0o755); err != nil {
				t.Fatal(err)
			}
			failed := false
			model.Fault = func(op faulttest.Operation) error {
				if !failed && op.Kind == "directory sync" && op.Path == parent {
					failed = true
					return syscall.EIO
				}
				return nil
			}
			if err := model.SyncDir(parent); !errors.Is(err, syscall.EIO) {
				t.Fatal(err)
			}
			model.Fault = nil
			snapshot := filepath.Join(lost, "snapshot", "judgments")
			if err := storage.MakeAncestorsDurable(model.Root, snapshot, 0o755, model); err != nil {
				t.Fatal(err)
			}
			tag, err := storage.NextTag(time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC), "")
			if err != nil {
				t.Fatal(err)
			}
			for name, text := range map[string]string{"history": tag, "snapshot/record": "evidence", "snapshot/judgments/run": "durable run"} {
				if err := storage.ReplaceControlDurable(filepath.Join(lost, filepath.FromSlash(name)), storage.PermState, controlText(text), model); err != nil {
					t.Fatal(err)
				}
			}
			for _, cutoff := range []int{faulttest.DropAll, point} {
				recovered := powerLoss(t, model, cutoff)
				relative, _ := filepath.Rel(model.Root, lost)
				if _, err := os.Stat(filepath.Join(recovered.Root, relative)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("failed ancestor survived: %v", err)
				}
				// Empty bootstrap may repeat a tag because no old object remains.
				next, err := storage.NextTag(time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC), "")
				if err != nil || next != tag {
					t.Fatalf("empty history: %s, %v", next, err)
				}
				next, err = storage.NextTag(time.Date(2025, 10, 25, 0, 0, 0, 0, time.UTC), tag)
				if err != nil || next <= tag {
					t.Fatalf("surviving history reused tag: %s, %v", next, err)
				}
			}
		})
	}
}

func TestAncestorRefusals(t *testing.T) {
	model := controlModel(t)
	outside := filepath.Join(model.Root, "..", "outside")
	if err := storage.MakeAncestorsDurable(model.Root, outside, 0o755, model); err == nil {
		t.Fatal("accepted outside ancestor")
	}
	if model.Point() != 0 {
		t.Fatal("wrote before ancestry check")
	}
	file := filepath.Join(model.Root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := storage.MakeAncestorsDurable(model.Root, file, 0o755, model); err == nil {
		t.Fatal("accepted file as ancestor")
	}
	missing := filepath.Join(model.Root, "missing")
	if err := storage.SyncAncestors(model.Root, missing, model); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing directory: %v", err)
	}
	link := filepath.Join(model.Root, "link")
	if err := os.Symlink(model.Root, link); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	if err := storage.MakeAncestorsDurable(model.Root, link, 0o755, model); err == nil {
		t.Fatal("accepted linked ancestor")
	}
}

func TestRenameParents(t *testing.T) {
	for _, fail := range []string{"", "first", "second"} {
		model := controlModel(t)
		from, to := filepath.Join(model.Root, "first", "file"), filepath.Join(model.Root, "second", "file")
		for _, dir := range []string{filepath.Dir(from), filepath.Dir(to)} {
			if err := storage.MakeAncestorsDurable(model.Root, dir, 0o755, model); err != nil {
				t.Fatal(err)
			}
		}
		start := model.Point()
		model.Fault = func(op faulttest.Operation) error {
			if filepath.Base(op.Path) == fail {
				return syscall.EIO
			}
			return nil
		}
		err := storage.SyncRenameParents(from, to, model)
		if (err != nil) != (fail != "") {
			t.Fatalf("%s: %v", fail, err)
		}
		operations := model.Operations()[start:]
		want := 2
		if fail == "first" {
			want = 1
		}
		if len(operations) != want {
			t.Fatalf("%s: parents synced %v", fail, operations)
		}
	}
	model := controlModel(t)
	if err := storage.SyncRenameParents(filepath.Join(model.Root, "a"), filepath.Join(model.Root, "b"), model); err != nil {
		t.Fatal(err)
	}
	if len(model.Operations()) != 1 {
		t.Fatal("same parent synced twice")
	}
}

func TestAncestorsCreationFailure(t *testing.T) {
	model := controlModel(t)
	model.Fault = func(op faulttest.Operation) error {
		if op.Kind == "mkdir" {
			return syscall.EIO
		}
		return nil
	}
	err := storage.MakeAncestorsDurable(model.Root, filepath.Join(model.Root, "child"), 0o755, model)
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("directory creation: %v", err)
	}
	operations := model.Operations()
	if len(operations) != 1 || operations[0].Kind != "mkdir" {
		t.Fatalf("continued after failed creation: %v", operations)
	}
}
