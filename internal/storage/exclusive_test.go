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
	"sync"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

func TestWriteExclusivePublishesOnlyCompleteReadOnlyFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "round.sf")
	err := storage.WriteExclusive(path, storage.PermLanded, func(w io.Writer) error {
		if _, err := io.WriteString(w, "first half\n"); err != nil {
			return err
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("an incomplete round is visible: %v", err)
		}
		_, err := io.WriteString(w, "second half\n")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "first half\nsecond half\n" {
		t.Fatalf("published content = %q", body)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != storage.PermLanded {
		t.Fatalf("published mode = %v, want %v", info.Mode().Perm(), storage.PermLanded)
	}
}

func TestWriteExclusiveRemovesFailedTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "round.sf")
	err := storage.WriteExclusive(path, storage.PermLanded, func(w io.Writer) error {
		if _, err := io.WriteString(w, "incomplete"); err != nil {
			return err
		}
		return io.ErrUnexpectedEOF
	})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("writer failure = %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed write left %d files", len(entries))
	}
}

func TestConcurrentExclusiveWritersPreserveTheWinner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "round.sf")
	const writers = 8
	ready := make(chan struct{}, writers)
	publish := make(chan struct{})
	var done sync.WaitGroup
	results := make([]error, writers)
	for i := range writers {
		done.Go(func() {
			results[i] = storage.WriteExclusive(path, storage.PermLanded, func(w io.Writer) error {
				_, err := w.Write([]byte{byte('a' + i)})
				ready <- struct{}{}
				<-publish
				return err
			})
		})
	}
	for range writers {
		<-ready
	}
	close(publish)
	done.Wait()
	winner := -1
	for i, err := range results {
		switch {
		case err == nil:
			if winner >= 0 {
				t.Fatalf("writers %d and %d both published", winner, i)
			}
			winner = i
		case errors.Is(err, storage.ErrExists):
		default:
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	if winner < 0 {
		t.Fatal("no writer published")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 1 || body[0] != byte('a'+winner) {
		t.Fatalf("winner %d was overwritten: %q", winner, body)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("publication left %d files, want one", len(entries))
	}
}

// An empty destination is how an abandoned reservation is recognized, so an
// empty file is never published.
func TestRenameExclusiveRefusesAnEmptyFile(t *testing.T) {
	dir := t.TempDir()
	from := filepath.Join(dir, ".tmp-empty")
	if err := os.WriteFile(from, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	to := filepath.Join(dir, "round.sf")
	if err := storage.RenameExclusive(from, to); err == nil {
		t.Fatal("an empty file was published")
	}
	if _, err := os.Stat(to); !os.IsNotExist(err) {
		t.Fatalf("the destination exists: %v", err)
	}
}

// On a filesystem without an exclusive rename, a crash between the
// reservation and the rename leaves an empty file under the final name. Once it is old enough it is taken for that crash's leftover
// and replaced. A fresh one may still belong to a live writer and is refused.
func TestRenameExclusiveReplacesOnlyAnAbandonedReservation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		age       time.Duration
		published bool
	}{
		{"abandoned", 2 * time.Minute, true},
		{"live", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			to := filepath.Join(dir, "000001.json")
			if err := os.WriteFile(to, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-tc.age)
			if err := os.Chtimes(to, old, old); err != nil {
				t.Fatal(err)
			}
			from := filepath.Join(dir, ".tmp-receipt")
			if err := os.WriteFile(from, []byte("receipt"), storage.PermLanded); err != nil {
				t.Fatal(err)
			}
			err := storage.RenameExclusive(from, to)
			body, _ := os.ReadFile(to)
			if tc.published && (err != nil || string(body) != "receipt") {
				t.Fatalf("abandoned reservation not replaced: %v, %q", err, body)
			}
			if !tc.published && (!errors.Is(err, storage.ErrExists) || len(body) != 0) {
				t.Fatalf("a live reservation was replaced: %v, %q", err, body)
			}
		})
	}
}
