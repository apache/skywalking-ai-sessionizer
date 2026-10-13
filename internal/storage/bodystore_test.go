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
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// A stored file is written again on every put, and the put syncs its own
// directory and each one above it up to the session's: an earlier attempt
// may have left the file, or made a directory, without a sync that held.
func TestABodyStorePutIsDurableEachTime(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	store := z.BodyStore("s")
	var synced []string
	sync := func(dir string) error {
		synced = append(synced, dir)
		return nil
	}
	session := z.SessionDir("s")
	provider := filepath.Join(session, "provider")
	for attempt := 0; attempt < 2; attempt++ {
		synced = nil
		if err := store.PutTip(3, "req-1", []byte("tip"), sync); err != nil {
			t.Fatal(err)
		}
		want := []string{store.TipsDir(3), filepath.Join(provider, "tips"), provider, session}
		if !slices.Equal(synced, want) {
			t.Fatalf("attempt %d synced %v, want %v", attempt, synced, want)
		}
		synced = nil
		if err := store.PutPiece(named("piece"), []byte("piece"), sync); err != nil {
			t.Fatal(err)
		}
		if want := []string{store.PiecesDir(), provider, session}; !slices.Equal(synced, want) {
			t.Fatalf("attempt %d synced %v, want %v", attempt, synced, want)
		}
	}
	// A file an earlier attempt left is replaced, not kept: it holds what
	// the put wrote, whatever it held before.
	whole := named("whole")
	if err := os.WriteFile(filepath.Join(store.PiecesDir(), whole+".gz"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.PutPiece(whole, []byte("whole"), sync); err != nil {
		t.Fatal(err)
	}
	if b, err := store.Piece(whole); err != nil || string(b) != "whole" {
		t.Fatalf("the replaced piece reads %q, %v", b, err)
	}
	// A failed sync fails the put.
	failing := errors.New("no sync")
	if err := store.PutPiece(named("x"), []byte("x"), func(string) error { return failing }); !errors.Is(err, failing) {
		t.Fatalf("a put whose sync failed: %v", err)
	}
}

// A tip is found by its record in any generation the store holds, the
// newest first, and a damaged copy is passed over for a whole one of an
// earlier generation. A missing tip, or the tip of another record with the
// same bytes, is fs.ErrNotExist; one with no whole copy, or a piece that
// does not hold what its name says, is an error that is not.
func TestABodyStoreFindsATipInAnyGeneration(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	store := z.BodyStore("s")
	old, cur := named("old"), named("new")
	if _, err := store.Tip("a", old); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a tip in an empty store: %v", err)
	}
	if err := store.PutTip(1, "a", []byte("old"), nil); err != nil {
		t.Fatal(err)
	}
	if err := store.PutTip(2, "b", []byte("new"), nil); err != nil {
		t.Fatal(err)
	}
	if b, err := store.Tip("a", old); err != nil || !bytes.Equal(b, []byte("old")) {
		t.Fatalf("a tip of the generation before: %q, %v", b, err)
	}
	if b, err := store.Tip("b", cur); err != nil || !bytes.Equal(b, []byte("new")) {
		t.Fatalf("a tip of the newest generation: %q, %v", b, err)
	}
	if g, err := store.Generations(); err != nil || !slices.Equal(g, []int{2, 1}) {
		t.Fatalf("generations %v, %v", g, err)
	}
	if _, err := store.Tip("c", old); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the tip of another record with the same bytes: %v", err)
	}
	// A damaged copy in the newest generation, and a whole one before it.
	if err := store.PutTip(2, "a", []byte("damaged"), nil); err != nil {
		t.Fatal(err)
	}
	if b, err := store.Tip("a", old); err != nil || !bytes.Equal(b, []byte("old")) {
		t.Fatalf("a tip whose newest copy is damaged: %q, %v", b, err)
	}
	if err := os.RemoveAll(store.TipsDir(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Tip("a", old); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a tip with no whole copy: %v", err)
	}
	// A piece whose bytes are not what its name says is neither kept nor
	// read.
	other := named("other")
	if err := store.PutPiece(other, []byte("something else"), nil); err == nil {
		t.Fatal("a piece was kept under a name that is not its digest")
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte("something else"))
	_ = zw.Close()
	if err := os.MkdirAll(store.PiecesDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.PiecesDir(), other+".gz"), gz.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Piece(other); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a piece that does not hold what its name says: %v", err)
	}
}

// named is the name a store gives b: its SHA-256.
func named(b string) string {
	sum := sha256.Sum256([]byte(b))
	return hex.EncodeToString(sum[:])
}
