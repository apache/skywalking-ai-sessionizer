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

//go:build linux || darwin

package retention

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// A pull server's snapshot can share a session's store entries through hard
// links. Reclaiming an earlier generation's tips and the pieces deletes the
// session's names only: the snapshot's links keep their bytes and their mode.
func TestReclaimKeepsSharedStoreEntries(t *testing.T) {
	root := t.TempDir()
	zone := storage.NewZone(root)
	store := zone.BodyStore("s1")
	entries := map[string]string{
		filepath.Join(store.TipsDir(1), "old.gz"): "old tip",
		filepath.Join(store.TipsDir(2), "new.gz"): "new tip",
		filepath.Join(store.PiecesDir(), "p.gz"):  "piece",
	}
	snapshot := filepath.Join(root, storage.PullDir, "conversations", "s1", "20261025T103200.000000000Z")
	if err := os.MkdirAll(snapshot, 0o755); err != nil {
		t.Fatal(err)
	}
	shared := map[string]string{}
	for path, body := range entries {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o444); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(snapshot, filepath.Base(path))
		if err := os.Link(path, link); err != nil {
			t.Fatal(err)
		}
		shared[link] = body
	}
	r := &Remover{Zone: zone}
	if err := r.reclaim("s1", 2, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.TipsDir(1)); !os.IsNotExist(err) {
		t.Fatalf("the earlier generation is still there: %v", err)
	}
	for link, body := range shared {
		info, err := os.Stat(link)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(link)
		if err != nil || string(data) != body || info.Mode().Perm() != 0o444 {
			t.Fatalf("%s: %q, %v, mode %v", link, data, err, info.Mode().Perm())
		}
	}
}
