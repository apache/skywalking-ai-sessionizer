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
