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
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage/faulttest"
)

func controlText(text string) func(io.Writer) error {
	return func(writer io.Writer) error { _, err := io.WriteString(writer, text); return err }
}

func controlModel(t *testing.T) *faulttest.Model {
	t.Helper()
	model, err := faulttest.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func requireText(t *testing.T, path, want string) {
	t.Helper()
	text, err := os.ReadFile(path)
	if err != nil || string(text) != want {
		t.Fatalf("%s: %q, %v; want %q", path, text, err, want)
	}
}

func powerLoss(t *testing.T, model *faulttest.Model, point int) *faulttest.Model {
	t.Helper()
	recovered, err := model.PowerLoss(t.TempDir(), point)
	if err != nil {
		t.Fatal(err)
	}
	return recovered
}

func TestReplaceControlDurable(t *testing.T) {
	model := controlModel(t)
	path := filepath.Join(model.Root, "record")
	if err := storage.ReplaceControlDurable(path, 0o640, controlText("first"), model); err != nil {
		t.Fatal(err)
	}
	old, err := storage.OpenShared(path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if err := storage.ReplaceControlDurable(path, 0o640, controlText("second"), model); err != nil {
		t.Fatal(err)
	}
	text, err := io.ReadAll(old)
	if err != nil || string(text) != "first" {
		t.Fatalf("old reader: %q, %v", text, err)
	}
	requireText(t, path, "second")
	for _, point := range []int{faulttest.DropAll, 0} {
		recovered := powerLoss(t, model, point)
		requireText(t, filepath.Join(recovered.Root, "record"), "second")
		info, err := os.Stat(filepath.Join(recovered.Root, "record"))
		if err != nil {
			t.Fatal(err)
		}
		checkControlMode(t, info.Mode().Perm(), 0o640)
	}
	var kinds []string
	var temporaries []string
	for _, op := range model.Operations() {
		kinds = append(kinds, op.Kind)
		if op.Kind == "create" {
			temporaries = append(temporaries, op.Path)
			if filepath.Dir(op.Path) != model.Root || !strings.HasPrefix(filepath.Base(op.Path), ".tmp-") {
				t.Fatalf("temporary: %s", op.Path)
			}
		}
	}
	one := []string{"create", "write", "mode", "file sync", "close", "replace", "directory sync"}
	if !reflect.DeepEqual(kinds, append(append([]string(nil), one...), one...)) {
		t.Fatalf("write order: %v", kinds)
	}
	if len(temporaries) != 2 || temporaries[0] == temporaries[1] {
		t.Fatalf("reused prepared file: %v", temporaries)
	}
}

// A record is first written read-only, as a server's lease is, and a later
// replacement still takes its place on the disk: Windows refuses a rename
// over a read-only file unless the rename asks to ignore the attribute.
func TestAReadOnlyRecordIsReplaced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record")
	if err := storage.WriteNewDurable(path, 0o444, controlText("first"), nil); err != nil {
		t.Fatal(err)
	}
	if err := storage.ReplaceControlDurable(path, 0o644, controlText("second"), nil); err != nil {
		t.Fatal(err)
	}
	requireText(t, path, "second")
}

func TestReplaceControlStopsAtEveryFailure(t *testing.T) {
	for _, kind := range []string{"create", "write", "mode", "file sync", "close", "replace", "directory sync"} {
		t.Run(kind, func(t *testing.T) {
			model := controlModel(t)
			path := filepath.Join(model.Root, "record")
			if err := storage.ReplaceControlDurable(path, storage.PermState, controlText("old"), model); err != nil {
				t.Fatal(err)
			}
			point := model.Point()
			failed := false
			model.Fault = func(op faulttest.Operation) error {
				if !failed && op.Kind == kind {
					failed = true
					return syscall.EIO
				}
				return nil
			}
			err := storage.ReplaceControlDurable(path, storage.PermState, controlText("new"), model)
			if !failed || !errors.Is(err, syscall.EIO) {
				t.Fatalf("failure: %v, reached %v", err, failed)
			}
			visible := "old"
			if kind == "directory sync" {
				visible = "new"
				if !strings.Contains(err.Error(), "visible") {
					t.Fatalf("lost visible outcome: %v", err)
				}
			}
			requireText(t, path, visible)
			entries, err := os.ReadDir(model.Root)
			if err != nil || len(entries) != 1 {
				t.Fatalf("prepared replacement left open or named: %v, %v", entries, err)
			}
			operations := model.Operations()[point:]
			after := false
			for _, op := range operations {
				if after && op.Kind != "close" && op.Kind != "unlink" {
					t.Fatalf("dependent work after failure: %+v", op)
				}
				if op.Kind == kind {
					after = true
				}
			}
			model.Fault = nil
			if err := model.SyncDir(model.Root); err != nil {
				t.Fatal(err)
			}
			for _, cutoff := range []int{faulttest.DropAll, point} {
				recovered := powerLoss(t, model, cutoff)
				requireText(t, filepath.Join(recovered.Root, "record"), "old")
			}
		})
	}
}

func TestReplaceControlWriterAndCleanupErrors(t *testing.T) {
	model := controlModel(t)
	failure := errors.New("writer failed")
	cleanup := errors.New("cleanup failed")
	model.Fault = func(op faulttest.Operation) error {
		if op.Kind == "unlink" {
			return cleanup
		}
		return nil
	}
	err := storage.ReplaceControlDurable(filepath.Join(model.Root, "record"), storage.PermState, func(out io.Writer) error {
		io.WriteString(out, "partial")
		return failure
	}, model)
	if !errors.Is(err, failure) || !errors.Is(err, cleanup) {
		t.Fatalf("errors lost: %v", err)
	}
	for _, op := range model.Operations() {
		if op.Kind == "file sync" || op.Kind == "replace" {
			t.Fatalf("published failed writer: %+v", op)
		}
	}
	model = controlModel(t)
	raw := errors.New("replacement busy")
	model.Fault = func(op faulttest.Operation) error {
		if op.Kind == "replace" {
			return &storage.ControlBusyError{Err: raw}
		}
		if op.Kind == "unlink" {
			return cleanup
		}
		return nil
	}
	err = storage.ReplaceControlDurable(filepath.Join(model.Root, "record"), storage.PermState, controlText("new"), model)
	var busy *storage.ControlBusyError
	if errors.As(err, &busy) || !errors.Is(err, raw) || !errors.Is(err, cleanup) {
		t.Fatalf("cleanup failure permits retry: %v", err)
	}
}

func TestReplaceControlExclusiveTemporaryAndCheckedParent(t *testing.T) {
	model := controlModel(t)
	first := true
	model.Fault = func(op faulttest.Operation) error {
		if op.Kind == "create" && first {
			first = false
			if err := os.WriteFile(op.Path, []byte("occupied"), 0o600); err != nil {
				t.Fatal(err)
			}
			return os.ErrExist
		}
		return nil
	}
	if err := storage.ReplaceControlDurable(filepath.Join(model.Root, "record"), storage.PermState, controlText("new"), model); err != nil {
		t.Fatal(err)
	}
	requireText(t, model.Operations()[0].Path, "occupied")
	missing := filepath.Join(model.Root, "missing", "record")
	if err := storage.ReplaceControlDurable(missing, storage.PermState, controlText("new"), nil); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing checked parent: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(missing)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created unchecked parent: %v", err)
	}
	disk := storage.DiskFileSystem{}
	path := filepath.Join(model.Root, "record")
	if file, err := disk.Create(path, storage.PermState); !errors.Is(err, os.ErrExist) {
		if file != nil {
			file.Close()
		}
		t.Fatalf("exclusive creation: %v", err)
	}
	requireText(t, path, "new")
}

func TestCommitFactFreshBarrier(t *testing.T) {
	for _, fact := range []string{"expired", "superseded_by", "removed"} {
		t.Run(fact, func(t *testing.T) {
			for _, failBarrier := range []string{"", "file sync", "directory sync"} {
				t.Run("barrier "+failBarrier, func(t *testing.T) {
					model := controlModel(t)
					path := filepath.Join(model.Root, "history")
					if err := storage.ReplaceControlDurable(path, 0o640, controlText(`{"state":"active"}`), model); err != nil {
						t.Fatal(err)
					}
					point := model.Point()
					observed := `{"fact":"` + fact + `","other":{"preserve":true}}`
					model.Fault = func(op faulttest.Operation) error {
						if op.Kind == "directory sync" {
							return syscall.EIO
						}
						return nil
					}
					if err := storage.ReplaceControlDurable(path, 0o640, controlText(observed), model); !errors.Is(err, syscall.EIO) {
						t.Fatal(err)
					}
					// Failure recording may fail too. Recovery must use the visible fact.
					model.Fault = func(op faulttest.Operation) error {
						if op.Kind == "create" {
							return syscall.EIO
						}
						return nil
					}
					if err := storage.ReplaceControlDurable(path, 0o640, controlText("failure"), model); !errors.Is(err, syscall.EIO) {
						t.Fatal(err)
					}
					requireText(t, path, observed)
					model.Fault = func(op faulttest.Operation) error {
						if op.Kind == failBarrier {
							return syscall.EIO
						}
						return nil
					}
					checked := false
					err := storage.CommitFactDurable(path, func(in io.Reader) error {
						text, err := io.ReadAll(in)
						if err != nil {
							return err
						}
						if string(text) != observed {
							t.Fatalf("checked %q", text)
						}
						checked = true
						return nil
					}, model)
					if !checked {
						t.Fatal("barrier skipped its locked check")
					}
					if failBarrier != "" {
						if !errors.Is(err, syscall.EIO) {
							t.Fatalf("barrier failure: %v", err)
						}
					} else {
						if err != nil {
							t.Fatal(err)
						}
						if err := storage.ReplaceControlDurable(filepath.Join(model.Root, "dependent"), storage.PermState, controlText("committed"), model); err != nil {
							t.Fatal(err)
						}
					}
					for _, cutoff := range []int{faulttest.DropAll, point} {
						recovered := powerLoss(t, model, cutoff)
						want := observed
						if failBarrier != "" {
							want = `{"state":"active"}`
							if _, err := os.Stat(filepath.Join(recovered.Root, "dependent")); !errors.Is(err, os.ErrNotExist) {
								t.Fatalf("dependent work survived failed barrier: %v", err)
							}
						} else {
							requireText(t, filepath.Join(recovered.Root, "dependent"), "committed")
						}
						requireText(t, filepath.Join(recovered.Root, "history"), want)
					}
				})
			}
		})
	}
}

func TestCommitFactRefusalWritesNothing(t *testing.T) {
	model := controlModel(t)
	path := filepath.Join(model.Root, "record")
	if err := storage.ReplaceControlDurable(path, storage.PermState, controlText("old"), model); err != nil {
		t.Fatal(err)
	}
	point := model.Point()
	refused := errors.New("fact or identity changed")
	if err := storage.CommitFactDurable(path, func(io.Reader) error { return refused }, model); !errors.Is(err, refused) {
		t.Fatal(err)
	}
	if model.Point() != point {
		t.Fatal("refused fact was rewritten")
	}
	checkedDirectory := false
	if err := storage.CommitFactDurable(model.Root, func(io.Reader) error { checkedDirectory = true; return nil }, model); err == nil || checkedDirectory {
		t.Fatal("accepted a directory for the fact check")
	}
	requireText(t, path, "old")
}

func TestControlUnknownOutcome(t *testing.T) {
	model := controlModel(t)
	path := filepath.Join(model.Root, "record")
	if err := storage.ReplaceControlDurable(path, storage.PermState, controlText("old"), model); err != nil {
		t.Fatal(err)
	}
	point := model.Point()
	stopped := errors.New("owner stopped after rename")
	model.After = func(op faulttest.Operation) error {
		if op.Kind == "replace" {
			return stopped
		}
		return nil
	}
	if err := storage.ReplaceControlDurable(path, storage.PermState, controlText("new"), model); !errors.Is(err, stopped) {
		t.Fatal(err)
	}
	requireText(t, path, "new")
	for _, cutoff := range []int{faulttest.DropAll, point} {
		recovered := powerLoss(t, model, cutoff)
		requireText(t, filepath.Join(recovered.Root, "record"), "old")
	}
}

func TestReplaceControlBusyAbandonsPreparedFile(t *testing.T) {
	model := controlModel(t)
	raw := errors.New("replacement busy")
	replacements := 0
	model.Fault = func(op faulttest.Operation) error {
		if op.Kind == "replace" {
			replacements++
			return &storage.ControlBusyError{Err: raw}
		}
		return nil
	}
	err := storage.ReplaceControlDurable(filepath.Join(model.Root, "record"), storage.PermState, controlText("new"), model)
	var busy *storage.ControlBusyError
	if !errors.As(err, &busy) || !errors.Is(err, raw) || busy.Error() != raw.Error() || replacements != 1 {
		t.Fatalf("busy result: %v, replacements %d", err, replacements)
	}
	entries, err := os.ReadDir(model.Root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("prepared file remains: %v, %v", entries, err)
	}
	operations := model.Operations()
	for _, op := range operations {
		if op.Kind == "directory sync" {
			t.Fatal("dependent sync followed busy replacement")
		}
	}
}
