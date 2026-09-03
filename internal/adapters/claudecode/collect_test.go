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

package claudecode_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

func mk(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const (
	tSess  = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	tAgent = "a1234567890abcdef"
)

// TestCollideStreamsAreRefused guards against two distinct sources landing into
// one stream. Interleaving them behind a single cursor would corrupt both, and
// they would fight over the cursor position on every pass.
func TestCollideStreamsAreRefused(t *testing.T) {
	src, zone := t.TempDir(), t.TempDir()
	mk(t, filepath.Join(src, "-proj-a", tSess, "subagents", "agent-"+tAgent+".jsonl"), "{\"uuid\":\"a\"}\n")
	mk(t, filepath.Join(src, "-proj-b", tSess, "subagents", "agent-"+tAgent+".jsonl"), "{\"uuid\":\"b\"}\n")

	col := claudecode.New(src, storage.NewZone(zone), 0)
	st, err := col.CollectAll(nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Conflicts == 0 {
		t.Error("expected a conflict when two sources map to one stream")
	}
	var found bool
	for _, e := range st.Errors {
		if strings.Contains(e.Error(), "refusing to interleave") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an interleave refusal; errors = %v", st.Errors)
	}
}

// TestCollectLandsAllSixKinds exercises the whole shape end to end.
func TestCollectLandsAllSixKinds(t *testing.T) {
	src, zone := t.TempDir(), t.TempDir()
	base := filepath.Join(src, "-proj-a", tSess)
	mk(t, filepath.Join(src, "-proj-a", tSess+".jsonl"), "{\"uuid\":\"m1\"}\n")
	mk(t, filepath.Join(base, "subagents", "agent-"+tAgent+".jsonl"), "{\"uuid\":\"c1\"}\n")
	mk(t, filepath.Join(base, "subagents", "agent-"+tAgent+".meta.json"), `{"agentType":"general-purpose","spawnDepth":1}`)
	mk(t, filepath.Join(base, "subagents", "workflows", "wf_r1", "agent-a2222222222222222.jsonl"), "{\"uuid\":\"w1\"}\n")
	mk(t, filepath.Join(base, "subagents", "workflows", "wf_r1", "journal.jsonl"), "{\"type\":\"started\"}\n")
	mk(t, filepath.Join(base, "workflows", "wf_r1.json"), `{"runId":"wf_r1"}`)
	// Claude Code writes the workflow script under whatever cwd the agent had;
	// the run id is embedded in the filename.
	mk(t, filepath.Join(base, "workflows", "scripts", "my-flow-wf_r1.js"), "export const meta = {}\n")

	col := claudecode.New(src, storage.NewZone(zone), 0)
	st, err := col.CollectAll(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", st.Errors)
	}
	if st.SourcesLanded != 7 {
		t.Errorf("landed %d sources, want 7", st.SourcesLanded)
	}

	z := storage.NewZone(zone)
	for _, dir := range []string{
		z.StreamDir(tSess, storage.StreamMain),
		z.StreamDir(tSess, tAgent),
		z.StreamDir(tSess, "a2222222222222222"),
		z.RunDir(tSess, "wf_r1"),
	} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("expected landing directory %s: %v", dir, err)
		}
	}
}

// TestSnapshotLandsOnlyOnChange verifies a rewritten source lands a new version
// only when its content actually changes - otherwise a manifest that is
// rewritten with identical content would land a copy on every pass forever.
func TestSnapshotLandsOnlyOnChange(t *testing.T) {
	src, zone := t.TempDir(), t.TempDir()
	manifest := filepath.Join(src, "-proj-a", tSess, "workflows", "wf_r1.json")
	mk(t, filepath.Join(src, "-proj-a", tSess+".jsonl"), "{\"uuid\":\"m1\"}\n")
	mk(t, manifest, `{"runId":"wf_r1","status":"running"}`)

	col := claudecode.New(src, storage.NewZone(zone), 0)
	if _, err := col.CollectAll(nil); err != nil {
		t.Fatal(err)
	}
	countManifests := func() int {
		ents, _ := os.ReadDir(storage.NewZone(zone).RunDir(tSess, "wf_r1"))
		n := 0
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), "manifest-") {
				n++
			}
		}
		return n
	}
	if got := countManifests(); got != 1 {
		t.Fatalf("after first pass: %d manifests, want 1", got)
	}

	// Unchanged content must not land again.
	if _, err := col.CollectAll(nil); err != nil {
		t.Fatal(err)
	}
	if got := countManifests(); got != 1 {
		t.Errorf("unchanged manifest landed again: %d versions", got)
	}

	// A real rewrite lands a new immutable version beside the old one.
	mk(t, manifest, `{"runId":"wf_r1","status":"completed"}`)
	if _, err := col.CollectAll(nil); err != nil {
		t.Fatal(err)
	}
	if got := countManifests(); got != 2 {
		t.Errorf("changed manifest: %d versions, want 2", got)
	}
}

// TestSourceGoneIsNotAnError covers pruning, which Claude Code does routinely.
func TestSourceGoneIsNotAnError(t *testing.T) {
	src, zone := t.TempDir(), t.TempDir()
	main := filepath.Join(src, "-proj-a", tSess+".jsonl")
	mk(t, main, "{\"uuid\":\"m1\"}\n")

	col := claudecode.New(src, storage.NewZone(zone), 0)
	if _, err := col.CollectAll(nil); err != nil {
		t.Fatal(err)
	}
	// The landed delta must survive its source being deleted.
	if err := os.Remove(main); err != nil {
		t.Fatal(err)
	}
	st, err := col.CollectAll(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Errors) != 0 {
		t.Errorf("pruning must not be an error: %v", st.Errors)
	}
	ents, _ := os.ReadDir(storage.NewZone(zone).StreamDir(tSess, storage.StreamMain))
	var kept bool
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "transcript-") {
			kept = true
		}
	}
	if !kept {
		t.Error("landed data must outlive its pruned source")
	}
}

// TestSourceUnderNewInodeResumes guards the path a storage root takes when its
// source is reached through another filesystem: a bind mount into a container,
// a restored backup, a copied tree. The bytes are the same, so collection must
// continue, and the cursor must record the new identity even though nothing
// landed, or every later pass re-verifies the tail of every unchanged source.
func TestSourceUnderNewInodeResumes(t *testing.T) {
	src, zone := t.TempDir(), t.TempDir()
	main := filepath.Join(src, "-proj", tSess+".jsonl")
	mk(t, main, "{\"uuid\":\"a\",\"type\":\"user\"}\n{\"uuid\":\"b\",\"type\":\"assistant\"}\n")

	col := claudecode.New(src, storage.NewZone(zone), 0)
	if st, err := col.CollectAll(nil); err != nil || st.SourcesLanded != 1 {
		t.Fatalf("first pass: landed=%d err=%v", st.SourcesLanded, err)
	}
	cursorPath := filepath.Join(zone, tSess, "streams", "main", "transcript.cursor")
	before, err := storage.LoadCursor(cursorPath, storage.CursorAppend, "")
	if err != nil {
		t.Fatal(err)
	}
	if before.Ino == 0 {
		t.Skip("no inode on this platform")
	}

	// The same bytes under a new inode.
	body, err := os.ReadFile(main)
	if err != nil {
		t.Fatal(err)
	}
	mk(t, main+".new", string(body))
	if err := os.Rename(main+".new", main); err != nil {
		t.Fatal(err)
	}

	st, err := col.CollectAll(nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Conflicts != 0 || st.SourcesLanded != 0 {
		t.Fatalf("second pass: conflicts=%d landed=%d, want 0 and 0", st.Conflicts, st.SourcesLanded)
	}
	after, err := storage.LoadCursor(cursorPath, storage.CursorAppend, "")
	if err != nil {
		t.Fatal(err)
	}
	if after.State != storage.CursorActive || after.Ino == before.Ino {
		t.Fatalf("cursor must stay active and record the new inode: state=%s ino %d -> %d",
			after.State, before.Ino, after.Ino)
	}

	// New data after the move lands as usual.
	f, err := os.OpenFile(main, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"uuid\":\"c\",\"type\":\"user\"}\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if st, err := col.CollectAll(nil); err != nil || st.SourcesLanded != 1 || st.Records != 1 {
		t.Fatalf("third pass: landed=%d records=%d err=%v", st.SourcesLanded, st.Records, err)
	}
}
