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
	"crypto/sha256"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage/faulttest"
)

// noNativeNoReplace stands for a file system whose rename cannot refuse an
// existing destination, such as exFAT on macOS.
func noNativeNoReplace(op faulttest.Operation) error {
	if op.Kind == "rename-noreplace" {
		return storage.ErrNoReplaceUnsupported
	}
	return nil
}

// Every way something can already sit at a destination: a complete file, the
// empty reservation a crashed writer leaves, a link and a directory. Strict
// installation keeps each one as it was, on the native path and the fallback.
func TestInstallNoReplaceNeverReplaces(t *testing.T) {
	for _, native := range []bool{true, false} {
		for _, existing := range []string{"file", "empty", "link", "directory"} {
			model := controlModel(t)
			if !native {
				model.Fault = noNativeNoReplace
			}
			from := filepath.Join(model.Root, "from")
			to := filepath.Join(model.Root, "to")
			if err := os.WriteFile(from, []byte("new"), 0o644); err != nil {
				t.Fatal(err)
			}
			switch existing {
			case "file":
				check(t, os.WriteFile(to, []byte("old"), 0o644))
			case "empty":
				check(t, os.WriteFile(to, nil, 0o644))
			case "link":
				if runtime.GOOS == "windows" {
					continue
				}
				check(t, os.Symlink("elsewhere", to))
			case "directory":
				check(t, os.Mkdir(to, 0o755))
			}
			before, err := os.Lstat(to)
			check(t, err)
			err = storage.InstallNoReplace(from, to, model)
			if !errors.Is(err, storage.ErrExists) {
				t.Fatalf("native %v, %s: %v", native, existing, err)
			}
			after, err := os.Lstat(to)
			check(t, err)
			if after.Mode() != before.Mode() || after.Size() != before.Size() {
				t.Fatalf("native %v, %s: destination changed", native, existing)
			}
			if existing == "file" {
				requireText(t, to, "old")
			}
			requireText(t, from, "new")
		}
	}
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// The fallback is an absence check, then a rename. It runs only when the
// native call says it is unsupported, and it renames when nothing is there.
func TestInstallNoReplaceFallback(t *testing.T) {
	model := controlModel(t)
	model.Fault = noNativeNoReplace
	from := filepath.Join(model.Root, "from")
	to := filepath.Join(model.Root, "to")
	check(t, os.WriteFile(from, []byte("new"), 0o644))
	check(t, storage.InstallNoReplace(from, to, model))
	requireText(t, to, "new")
	if _, err := os.Lstat(from); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("source left behind: %v", err)
	}
	var kinds []string
	for _, op := range model.Operations() {
		kinds = append(kinds, op.Kind)
	}
	if !reflect.DeepEqual(kinds, []string{"rename-noreplace", "rename"}) {
		t.Fatalf("operations: %v", kinds)
	}
}

func TestPublishDirSyncsBothParents(t *testing.T) {
	model := controlModel(t)
	building := filepath.Join(model.Root, "_building")
	check(t, model.Mkdir(building, 0o755))
	from := filepath.Join(building, "b")
	check(t, model.Mkdir(from, 0o755))
	to := filepath.Join(model.Root, "tag")
	check(t, storage.PublishDir(from, to, model))
	ops := model.Operations()
	tail := ops[len(ops)-3:]
	want := []faulttest.Operation{
		{Kind: "rename-noreplace", Path: from, Target: to},
		{Kind: "directory sync", Path: building},
		{Kind: "directory sync", Path: model.Root},
	}
	for i := range want {
		if tail[i].Kind != want[i].Kind || tail[i].Path != want[i].Path || tail[i].Target != want[i].Target {
			t.Fatalf("operation %d: %+v, want %+v", i, tail[i], want[i])
		}
	}
	// An existing directory, even an empty one, is never replaced.
	other := filepath.Join(building, "c")
	check(t, model.Mkdir(other, 0o755))
	if err := storage.PublishDir(other, to, model); !errors.Is(err, storage.ErrExists) {
		t.Fatalf("published over a directory: %v", err)
	}
}

func digestOf(text string) [sha256.Size]byte { return sha256.Sum256([]byte(text)) }

func TestWriteCheckedRefusesOtherBytes(t *testing.T) {
	for _, tc := range []struct {
		name, source string
	}{
		{"shorter", "evidenc"},
		{"longer", "evidence!"},
		{"different", "Evidence"},
	} {
		model := controlModel(t)
		temp := filepath.Join(model.Root, ".tmp-x")
		err := storage.WriteChecked(temp, bytes.NewReader([]byte(tc.source)), int64(len("evidence")), digestOf("evidence"), model)
		if !errors.Is(err, storage.ErrChecked) {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if _, err := os.Lstat(temp); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s: temporary left behind: %v", tc.name, err)
		}
	}
}

// The order is the guarantee: content synced before the mode, the mode synced
// before the name appears, and the name and both parents synced before a later
// step may depend on the file.
func TestInstallFileOrderAndPowerLoss(t *testing.T) {
	model := controlModel(t)
	temp := filepath.Join(model.Root, ".tmp-x")
	dest := filepath.Join(model.Root, "a.sd")
	check(t, storage.WriteChecked(temp, bytes.NewReader([]byte("evidence")), 8, digestOf("evidence"), model))
	check(t, storage.InstallFile(temp, dest, model))
	var kinds []string
	for _, op := range model.Operations() {
		kinds = append(kinds, op.Kind)
	}
	want := []string{"create", "write", "file sync", "close", "mode", "file sync",
		"rename-noreplace", "file sync", "directory sync"}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("operations: %v", kinds)
	}
	recovered := powerLoss(t, model, faulttest.DropAll)
	path := filepath.Join(recovered.Root, "a.sd")
	requireText(t, path, "evidence")
	info, err := os.Stat(path)
	check(t, err)
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o444 {
		t.Fatalf("mode after power loss: %v", info.Mode().Perm())
	}
}

// Lost, never partial: whichever sync fails, a power loss afterwards leaves
// either no file or the complete one under the final name.
func TestInstallFileFailedSyncIsLostNotPartial(t *testing.T) {
	for failed := 1; failed <= 4; failed++ {
		model := controlModel(t)
		model.FailSync(failed)
		temp := filepath.Join(model.Root, ".tmp-x")
		dest := filepath.Join(model.Root, "a.sd")
		err := storage.WriteChecked(temp, bytes.NewReader([]byte("evidence")), 8, digestOf("evidence"), model)
		if err == nil {
			err = storage.InstallFile(temp, dest, model)
		}
		if err == nil {
			t.Fatalf("sync %d failed but installation succeeded", failed)
		}
		for _, point := range []int{faulttest.DropAll, model.Point()} {
			recovered := powerLoss(t, model, point)
			text, err := os.ReadFile(filepath.Join(recovered.Root, "a.sd"))
			if err == nil && string(text) != "evidence" {
				t.Fatalf("sync %d, point %d: partial file %q", failed, point, text)
			}
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				t.Fatal(err)
			}
		}
	}
}

func TestCopyChecked(t *testing.T) {
	model := controlModel(t)
	src := filepath.Join(t.TempDir(), "source")
	check(t, os.WriteFile(src, []byte("evidence"), 0o644))
	dest := filepath.Join(model.Root, "a.sd")
	check(t, storage.CopyChecked(src, filepath.Join(model.Root, ".tmp-x"), dest, 8, digestOf("evidence"), model))
	requireText(t, dest, "evidence")
	// A source changed since its digest was taken is refused, and nothing lands.
	check(t, os.WriteFile(src, []byte("Evidence"), 0o644))
	other := filepath.Join(model.Root, "b.sd")
	err := storage.CopyChecked(src, filepath.Join(model.Root, ".tmp-y"), other, 8, digestOf("evidence"), model)
	if !errors.Is(err, storage.ErrChecked) {
		t.Fatalf("changed source: %v", err)
	}
	if _, err := os.Lstat(other); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("destination after a refused copy: %v", err)
	}
}

func TestShareFileRefusesAnExistingDestination(t *testing.T) {
	model := controlModel(t)
	src := filepath.Join(model.Root, "source")
	check(t, os.WriteFile(src, []byte("evidence"), 0o444))
	dest := filepath.Join(model.Root, "a.sd")
	check(t, os.WriteFile(dest, []byte("old"), 0o644))
	_, err := storage.ShareFile(src, dest, filepath.Join(model.Root, ".tmp-x"), 8, digestOf("evidence"), model)
	if !errors.Is(err, storage.ErrExists) {
		t.Fatalf("shared over an existing file: %v", err)
	}
	requireText(t, dest, "old")
}
