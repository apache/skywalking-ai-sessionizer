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

package view

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// TestAFoldIsKeptByWhatItReachedNotByWhatWasListed.
//
// A round file is visible in the directory before all of its bytes are
// there, so the head a listing reports can be one ahead of what a fold can
// actually read. Keeping the listed head would cache that short fold as if
// it were current, and every later read would return it until yet another
// round arrived. Keeping the round the fold reached makes the next read try
// again, which is what a page beside a separate asz collect needs.
func TestAFoldIsKeptByWhatItReachedNotByWhatWasListed(t *testing.T) {
	root := t.TempDir()
	id := "0438c73b-2367-4ed5-9de3-13ef9a17ed01"
	rounds := filepath.Join(root, "_conversations", id, "rounds")
	if err := os.MkdirAll(rounds, 0o755); err != nil {
		t.Fatal(err)
	}
	// A round that is listed but cannot be folded: the write did not finish.
	if err := os.WriteFile(filepath.Join(rounds, "000001.sf"), []byte("{\"round\":1"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New(storage.NewZone(root), nil)
	// Nothing can be folded, so nothing is cached and nothing is served.
	if _, err := s.Load(id); err == nil {
		t.Fatal("a chain whose only round is half written was served as a conversation")
	}
	if _, cached := s.loaded[id]; cached {
		t.Fatal("a fold that failed was put in the cache; the next read would never try again")
	}
}

// TestListForgetsAConversationRemovedByAnotherProcess. A separate collector
// can retire a conversation while no page is open. Listing the root must
// release its stored fold without another Load or an explicit Forget.
func TestListForgetsAConversationRemovedByAnotherProcess(t *testing.T) {
	root := t.TempDir()
	s := New(storage.NewZone(root), nil)
	for _, id := range []string{"removed", "kept"} {
		w, err := sessionflow.NewWriter(sessionflow.Header{
			Conversation: id, Session: id, Round: 1, Parser: "v1", Policy: "v1",
			FromSeq: 1, InputDigest: "d0",
		})
		if err != nil {
			t.Fatal(err)
		}
		data, digest, err := w.Close()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sessionflow.OpenChain(root, id).Publish(1, digest, data); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Load(id); err != nil {
			t.Fatal(err)
		}
		if !s.Kept(id) {
			t.Fatalf("%s was not kept after loading", id)
		}
	}
	if err := storage.RemoveDir(root, sessionflow.OpenChain(root, "removed").Dir(), "removed.chain", nil); err != nil {
		t.Fatal(err)
	}
	ids, err := s.List()
	if err != nil || len(ids) != 1 || ids[0] != "kept" {
		t.Fatalf("listed %v, %v; want only the remaining conversation", ids, err)
	}
	if s.Kept("removed") {
		t.Fatal("the removed conversation is still kept after listing")
	}
	if !s.Kept("kept") {
		t.Fatal("the remaining conversation was forgotten")
	}
	// A missing parent directory is an empty list too.
	if err := storage.DeleteTree(filepath.Join(root, "_conversations"), nil); err != nil {
		t.Fatal(err)
	}
	if ids, err := s.List(); err != nil || len(ids) != 0 {
		t.Fatalf("listed %v, %v after the parent directory went", ids, err)
	}
	if s.Kept("kept") {
		t.Fatal("the last conversation is still kept after its parent directory went")
	}
}
