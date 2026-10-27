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

package run

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// pullTree is a pull server's frozen snapshot of sess2. Its landed file has a
// local file's name and extension.
var pullTree = filepath.Join(storage.PullDir, "conversations", "sess2", "20261025T103200.000000000Z", "sess2", "streams", "main", "transcript-x-000001.sd")

// Remote copies and a pull server's snapshots keep another root's session ids.
// They must not enter the root used by the check, which still needs its own
// source and round chain.
func TestACopyOfARootPassesOverRemote(t *testing.T) {
	from, to := t.TempDir(), filepath.Join(t.TempDir(), "copy")
	files := map[string]string{
		filepath.Join("sess1", "streams", "main", "transcript-x-000001.sd"):                              "landed\n",
		filepath.Join("_source", "proj", "sess1.jsonl"):                                                  "source\n",
		filepath.Join("_conversations", "sess1", "rounds", "000001.sf"):                                  "round\n",
		filepath.Join(storage.RemoteDir, "server", "sess2", "streams", "main", "transcript-x-000001.sd"): "remote\n",
		pullTree: "frozen\n",
	}
	for rel, body := range files {
		path := filepath.Join(from, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	if err := copyRoot(from, to); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{storage.RemoteDir, storage.PullDir} {
		if _, err := os.Stat(filepath.Join(to, dir)); !os.IsNotExist(err) {
			t.Fatalf("the copy carried %s: %v", dir, err)
		}
	}
	for _, rel := range []string{
		filepath.Join("sess1", "streams", "main", "transcript-x-000001.sd"),
		filepath.Join("_source", "proj", "sess1.jsonl"),
		filepath.Join("_conversations", "sess1", "rounds", "000001.sf"),
	} {
		body, err := os.ReadFile(filepath.Join(to, rel))
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != files[rel] {
			t.Fatalf("the copy changed %s: got %q, want %q", rel, body, files[rel])
		}
	}
}

// A remote's landed files have the same extension as local files. The walk
// must leave their directory out before it selects files by extension. Its
// other behaviour stays: every other .sd is copied, a judge's work included.
func TestACopyOfLandedFilesPassesOverRemote(t *testing.T) {
	from, to := t.TempDir(), filepath.Join(t.TempDir(), "copy")
	local := filepath.Join("sess1", "streams", "main", "transcript-x-000001.sd")
	decoy := filepath.Join("sess1", "judgments", "j000001", "work", "decoy.sd")
	remote := filepath.Join(storage.RemoteDir, "server", "sess2", "streams", "main", "transcript-x-000001.sd")
	for _, rel := range []string{local, decoy, remote, pullTree} {
		path := filepath.Join(from, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}\n"), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	if err := copyLanded(from, to); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{storage.RemoteDir, storage.PullDir} {
		if _, err := os.Stat(filepath.Join(to, dir)); !os.IsNotExist(err) {
			t.Fatalf("the copy carried %s: %v", dir, err)
		}
	}
	for _, rel := range []string{local, decoy} {
		body, err := os.ReadFile(filepath.Join(to, rel))
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != "{}\n" {
			t.Fatalf("the copy changed %s: %q", rel, body)
		}
	}
}

// Refusal must happen at the output directory before format roots exist,
// or a check could build a collecting root inside a remote copy.
func TestMakeFormatRootsRefusesACopy(t *testing.T) {
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(out, storage.CopyMarker), nil, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := makeFormatRoots(out, scenario.Formats); !errors.Is(err, storage.ErrCopy) {
		t.Fatalf("making format roots in a copy returned %v, want ErrCopy", err)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != storage.CopyMarker {
		t.Fatalf("making format roots in a copy changed the directory: %v", entries)
	}
}

// Checking a copy must stop before a format's build gets its own root lock,
// because the marker belongs to the parent output directory.
func TestACheckRefusesACopy(t *testing.T) {
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(out, storage.CopyMarker), nil, 0o444); err != nil {
		t.Fatal(err)
	}
	_, err := Check(filepath.Join("..", "..", "..", "tests", "scenarios", "fixture.yaml"),
		Options{Formats: []scenario.Format{scenario.FormatSD}, Out: out})
	if !errors.Is(err, storage.ErrCopy) {
		t.Fatalf("checking a copy returned %v, want ErrCopy", err)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != storage.CopyMarker {
		t.Fatalf("checking a copy changed the directory: %v", entries)
	}
}

// The format directories must be present before the output lock is released,
// so a later pull cannot accept that output as an empty directory.
func TestMakeFormatRootsMakesEachFormat(t *testing.T) {
	out := filepath.Join(t.TempDir(), "check")
	if err := makeFormatRoots(out, scenario.Formats); err != nil {
		t.Fatal(err)
	}
	for _, format := range scenario.Formats {
		dir := filepath.Join(out, string(format))
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() {
			t.Fatalf("the format root %s is not a directory", dir)
		}
	}
}

// A scenario root's removal parses every session as a pipeline pass does. It
// passes over _remote and a pull server's _pull, even laid out as a session:
// no chain is built for either, and neither changes.
func TestParseEveryPassesOverPullTrees(t *testing.T) {
	root := t.TempDir()
	sc, err := scenario.Load(filepath.Join("..", "..", "..", "tests", "scenarios", "growth.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	built, err := scenario.Build(sc, scenario.FormatSD, root, scenario.Options{At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(root, built.Session)
	for _, dir := range []string{storage.RemoteDir, storage.PullDir} {
		if err := copyRoot(session, filepath.Join(root, dir)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := parseEvery(root, 1<<20); err != nil {
		t.Fatal(err)
	}
	chains, err := os.ReadDir(filepath.Join(root, "_conversations"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range chains {
		if c.Name() != built.Session {
			t.Fatalf("a chain was built for %s", c.Name())
		}
	}
}
