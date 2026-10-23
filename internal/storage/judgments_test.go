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
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// A session's judgments directory can hold more than runs: a file a person
// left there, a directory of another name, a file named like a run. Only the
// run directories are listed, each with its stage files in run order.
func TestOnlyRunDirectoriesAreListed(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	if runs, err := storage.JudgmentRuns(z, "s1"); err != nil || runs != nil {
		t.Fatalf("a session never judged lists %v, %v", runs, err)
	}
	dir := filepath.Join(z.SessionDir("s1"), storage.JudgmentsDir)
	for _, d := range []string{"j000002", "j000001", "notes"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"README", "j000003", "j000002/s2-targets-0123456789ab.sj", "j000002/s1-ledger-0123456789ab.sj", "j000002/notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := storage.JudgmentRuns(z, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].Run != 1 || runs[1].Run != 2 || len(runs[0].Files) != 0 {
		t.Fatalf("runs %+v", runs)
	}
	var stages []string
	for _, f := range runs[1].Files {
		stages = append(stages, f.Stage)
	}
	if strings.Join(stages, ",") != "ledger,targets" {
		t.Fatalf("run 2 holds %v", stages)
	}
}

// A stage file is named by the first 12 characters of its digest. A shorter
// digest cannot name one, so it is refused and nothing is written.
func TestAStageWithAShortDigestIsRefused(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	_, runDir, err := storage.ReserveRun(z, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if path, err := storage.PublishStage(z, "s1", runDir, 1, "ledger", "0123456789a", []byte("{}\n")); err == nil ||
		!strings.Contains(err.Error(), `digest "0123456789a" is too short`) || path != "" {
		t.Fatalf("published at %q: %v", path, err)
	}
	if items, err := os.ReadDir(runDir); err != nil || len(items) != 0 {
		t.Fatalf("the run holds %v, %v", items, err)
	}
	path, err := storage.PublishStage(z, "s1", runDir, 1, "ledger", "0123456789ab", []byte("{}\n"))
	if err != nil || filepath.Base(path) != "s1-ledger-0123456789ab.sj" {
		t.Fatalf("a digest of 12 characters: %q, %v", path, err)
	}
}

// A judge run writes nothing of a retired session, and nothing of one whose
// marker cannot be looked at: that error is never taken for no marker. Each
// writer is checked on its own: reserving a run, making a stage's work
// directory, and publishing a stage file.
func TestNothingOfARetiredSessionIsWritten(t *testing.T) {
	for _, tc := range []struct {
		name   string
		retire func(t *testing.T, root string)
		want   func(err error) bool
		// unix marks a case Windows cannot make: there a file where a
		// directory should be reads as not there.
		unix bool
	}{
		{"retired", func(t *testing.T, root string) {
			if err := os.MkdirAll(filepath.Join(root, storage.RetiredDir), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, storage.RetiredDir, "s1"), nil, 0o444); err != nil {
				t.Fatal(err)
			}
		}, func(err error) bool { return errors.Is(err, storage.ErrRetiredSession) }, false},
		// A file where the markers' directory should be makes every marker
		// impossible to look at.
		{"a marker that cannot be looked at", func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, storage.RetiredDir), nil, 0o444); err != nil {
				t.Fatal(err)
			}
		}, func(err error) bool { return err != nil && !errors.Is(err, storage.ErrRetiredSession) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.unix && runtime.GOOS == "windows" {
				t.Skip("a file where a directory should be reads as not there")
			}
			root := t.TempDir()
			z := storage.NewZone(root)
			tc.retire(t, root)
			if _, _, err := storage.ReserveRun(z, "s1"); !tc.want(err) {
				t.Errorf("a run was reserved: %v", err)
			}
			if _, err := os.Stat(z.SessionDir("s1")); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the session's directory was made: %v", err)
			}

			// A run reserved before the session was retired.
			root = t.TempDir()
			z = storage.NewZone(root)
			_, dir, err := storage.ReserveRun(z, "s1")
			if err != nil {
				t.Fatal(err)
			}
			tc.retire(t, root)
			if _, err := storage.MakeWorkDir(z, "s1", dir, "w"); !tc.want(err) {
				t.Errorf("a work directory was made: %v", err)
			}
			if err := storage.WriteWorkFiles(z, "s1", dir, "w", []storage.WorkFile{{Name: "a", Data: []byte("x")}}); !tc.want(err) {
				t.Errorf("work files were written: %v", err)
			}
			if _, err := storage.PublishStage(z, "s1", dir, 1, "ledger", strings.Repeat("a", 64), []byte("x\n")); !tc.want(err) {
				t.Errorf("a stage was published: %v", err)
			}
			if items, err := os.ReadDir(dir); err != nil || len(items) != 0 {
				t.Errorf("the run holds %v, %v", items, err)
			}
		})
	}
}

// A run whose directory is gone gets no work directory: making one would
// make the run again, with none of its stages.
func TestAWorkDirectoryNeedsItsRun(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	_, dir, err := storage.ReserveRun(z, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.MakeWorkDir(z, "s1", dir, "w"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a work directory for a run that is gone: %v", err)
	}
	if err := storage.WriteWorkFiles(z, "s1", dir, "w", []storage.WorkFile{{Name: "a", Data: []byte("x")}}); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("work files for a run that is gone: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the run was made again: %v", err)
	}
}

// A regroup's version is reserved with its work directory, after every
// regroup written and every one begun, so two at once never share one; a
// retired session gets none.
func TestARegroupVersionIsReservedOnce(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	_, dir, err := storage.ReserveRun(z, "s1")
	if err != nil {
		t.Fatal(err)
	}
	for want := 1; want <= 2; want++ {
		v, err := storage.ReserveRegroup(z, "s1", dir)
		if err != nil || v != want {
			t.Fatalf("reserved %d, %v; want %d", v, err, want)
		}
		if _, err := os.Stat(filepath.Join(dir, storage.WorkDir, fmt.Sprintf("g%d-regroup", want))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := storage.PublishRegroup(z, "s1", dir, 5, strings.Repeat("a", 64), []byte("x\n")); err != nil {
		t.Fatal(err)
	}
	if v, err := storage.ReserveRegroup(z, "s1", dir); err != nil || v != 6 {
		t.Fatalf("after version 5 written: %d, %v", v, err)
	}
	if err := os.MkdirAll(filepath.Join(z.Root(), storage.RetiredDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(z.Root(), storage.RetiredDir, "s1"), nil, 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.ReserveRegroup(z, "s1", dir); !errors.Is(err, storage.ErrRetiredSession) {
		t.Fatalf("a retired session: %v", err)
	}
}

// Work files are written into the run's work directory, each read-only.
func TestWorkFilesAreWrittenOnce(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	_, dir, err := storage.ReserveRun(z, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.WriteWorkFiles(z, "s1", dir, "sources", []storage.WorkFile{{Name: "R1.json", Data: []byte("{}\n")}, {Name: "R1.txt", Data: []byte("text")}}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"R1.json": "{}\n", "R1.txt": "text"} {
		path := filepath.Join(dir, storage.WorkDir, "sources", name)
		b, err := os.ReadFile(path)
		info, _ := os.Stat(path)
		if err != nil || string(b) != want || info.Mode().Perm()&0o222 != 0 {
			t.Errorf("%s: %q %v %v", name, b, err, info.Mode())
		}
	}
}
