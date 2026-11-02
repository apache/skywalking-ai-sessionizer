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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// discoverOne is the one session a source root holds.
func discoverOne(t *testing.T, src string) claudecode.Session {
	t.Helper()
	sessions, err := claudecode.Discover(src)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("discovered %d sessions, want 1: %v", len(sessions), err)
	}
	return sessions[0]
}

func appendText(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// Covered holds exactly when a pass would land nothing: after a pass, and
// not while a source has a line or a version that is not landed yet. A 0-byte
// journal and an empty meta file never get a cursor, and are covered.
func TestCoveredMeansAPassWouldLandNothing(t *testing.T) {
	src, zone := t.TempDir(), t.TempDir()
	base := filepath.Join(src, "-proj-a", tSess)
	run := filepath.Join(base, "subagents", "workflows", "wf_r1")
	main := filepath.Join(src, "-proj-a", tSess+".jsonl")
	manifest := filepath.Join(base, "workflows", "wf_r1.json")
	mk(t, main, "{\"uuid\":\"m1\"}\n")
	mk(t, filepath.Join(base, "subagents", "agent-"+tAgent+".jsonl"), "{\"uuid\":\"c1\"}\n")
	mk(t, filepath.Join(base, "subagents", "agent-"+tAgent+".meta.json"), `{"agentType":"general-purpose"}`)
	mk(t, filepath.Join(run, "agent-a2222222222222222.jsonl"), "{\"uuid\":\"w1\"}\n")
	mk(t, filepath.Join(run, "agent-a2222222222222222.meta.json"), "")
	mk(t, filepath.Join(run, "journal.jsonl"), "")
	mk(t, manifest, `{"runId":"wf_r1","status":"running"}`)
	mk(t, filepath.Join(base, "workflows", "scripts", "my-flow-wf_r1.js"), "export const meta = {}\n")

	col := claudecode.New(src, storage.NewZone(zone), 0)
	covered := func() (bool, string) {
		t.Helper()
		ok, reason, err := col.Covered(discoverOne(t, src))
		if err != nil {
			t.Fatalf("Covered: %v", err)
		}
		return ok, reason
	}
	collect := func() {
		t.Helper()
		st, err := col.CollectAll(nil)
		if err != nil || len(st.Errors) != 0 {
			t.Fatalf("collect: %v %v", err, st.Errors)
		}
	}

	if ok, reason := covered(); ok || !strings.Contains(reason, "is not landed to its end yet") {
		t.Fatalf("before any pass: covered=%v reason=%q", ok, reason)
	}
	// Covered writes nothing, not even the session's directory.
	if _, err := os.Stat(filepath.Join(zone, tSess)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Covered created the session's directory: %v", err)
	}
	collect()
	if ok, reason := covered(); !ok {
		t.Fatalf("after a pass: %s", reason)
	}

	// A line appended is not covered until a pass lands it.
	appendText(t, main, "{\"uuid\":\"m2\"}\n")
	if ok, reason := covered(); ok || !strings.Contains(reason, tSess+".jsonl") {
		t.Fatalf("a line appended: covered=%v reason=%q", ok, reason)
	}
	collect()
	if ok, reason := covered(); !ok {
		t.Fatalf("after the line landed: %s", reason)
	}

	// A pass leaves a line with no newline behind, so that line is not landed.
	appendText(t, main, "{\"uuid\":\"m3\"")
	if ok, _ := covered(); ok {
		t.Fatal("a line still being written reads as landed")
	}
	collect()
	if ok, _ := covered(); ok {
		t.Fatal("after a pass, a line still being written reads as landed")
	}
	appendText(t, main, "}\n")
	collect()
	if ok, reason := covered(); !ok {
		t.Fatalf("after the finished line landed: %s", reason)
	}

	// A snapshot with new content is not covered until its version lands.
	mk(t, manifest, `{"runId":"wf_r1","status":"completed"}`)
	if ok, reason := covered(); ok || !strings.Contains(reason, "wf_r1.json") {
		t.Fatalf("a rewritten manifest: covered=%v reason=%q", ok, reason)
	}
	collect()
	if ok, reason := covered(); !ok {
		t.Fatalf("after the new version landed: %s", reason)
	}
}

// Where a pass would stop until a person looks, Covered says so with an
// error and changes nothing. That is a source cut below its cursor, a source
// that is gone, and two sources that map to one cursor.
func TestCoveredIsAnErrorWhereAPassWouldStop(t *testing.T) {
	src, zone := t.TempDir(), t.TempDir()
	main := filepath.Join(src, "-proj-a", tSess+".jsonl")
	mk(t, main, "{\"uuid\":\"a\"}\n{\"uuid\":\"b\"}\n")
	col := claudecode.New(src, storage.NewZone(zone), 0)
	if _, err := col.CollectAll(nil); err != nil {
		t.Fatal(err)
	}
	cursor := filepath.Join(zone, tSess, "streams", "main", "transcript.cursor")
	before, err := os.ReadFile(cursor)
	if err != nil {
		t.Fatal(err)
	}

	mk(t, main, "{\"uuid\":\"a\"}\n")
	var ce *claudecode.ConflictError
	if ok, _, err := col.Covered(discoverOne(t, src)); ok || !asConflict(err, &ce) {
		t.Fatalf("a source cut below its cursor: covered=%v err=%v; want a conflict", ok, err)
	}
	if after, err := os.ReadFile(cursor); err != nil || string(after) != string(before) {
		t.Fatalf("Covered changed the cursor: %v", err)
	}

	s := discoverOne(t, src)
	if err := os.Remove(main); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := col.Covered(s); ok || err == nil || !strings.Contains(err.Error(), "is gone") {
		t.Fatalf("a source that is gone: covered=%v err=%v", ok, err)
	}

	// Two child transcripts for one agent id, filed under two directories.
	src2 := t.TempDir()
	mk(t, filepath.Join(src2, "-proj-a", tSess, "subagents", "agent-"+tAgent+".jsonl"), "{\"uuid\":\"a\"}\n")
	mk(t, filepath.Join(src2, "-proj-b", tSess, "subagents", "agent-"+tAgent+".jsonl"), "{\"uuid\":\"b\"}\n")
	col2 := claudecode.New(src2, storage.NewZone(t.TempDir()), 0)
	if ok, _, err := col2.Covered(discoverOne(t, src2)); ok || err == nil || !strings.Contains(err.Error(), "both map") {
		t.Fatalf("two sources for one cursor: covered=%v err=%v", ok, err)
	}
}

// The same bytes under a new inode are a move, not new data. A pass lands
// nothing from them, so the session stays covered.
func TestCoveredTakesAMoveForNoNewData(t *testing.T) {
	src, zone := t.TempDir(), t.TempDir()
	main := filepath.Join(src, "-proj", tSess+".jsonl")
	mk(t, main, "{\"uuid\":\"a\"}\n{\"uuid\":\"b\"}\n")
	col := claudecode.New(src, storage.NewZone(zone), 0)
	if _, err := col.CollectAll(nil); err != nil {
		t.Fatal(err)
	}
	cur, err := storage.LoadCursor(filepath.Join(zone, tSess, "streams", "main", "transcript.cursor"), storage.CursorAppend, "")
	if err != nil {
		t.Fatal(err)
	}
	if cur.Ino == 0 {
		t.Skip("no inode on this platform")
	}
	body, err := os.ReadFile(main)
	if err != nil {
		t.Fatal(err)
	}
	mk(t, main+".new", string(body))
	if err := os.Rename(main+".new", main); err != nil {
		t.Fatal(err)
	}
	if ok, reason, err := col.Covered(discoverOne(t, src)); !ok || err != nil {
		t.Fatalf("the same bytes under a new inode: covered=%v reason=%q err=%v", ok, reason, err)
	}
}

// A pass says of every session it read whether all of it was read to its
// end: a line still being written, or a session another collector holds,
// leaves it incomplete, and an unchanged session read again is complete.
func TestAPassSaysWhetherASessionWasReadToItsEnd(t *testing.T) {
	src, zone := t.TempDir(), t.TempDir()
	main := filepath.Join(src, "-proj-a", tSess+".jsonl")
	mk(t, main, "{\"uuid\":\"m1\"}\n")
	z := storage.NewZone(zone)
	col := claudecode.New(src, z, 0)
	outcome := func() storage.Outcome {
		t.Helper()
		st, err := col.CollectAll(nil)
		if err != nil {
			t.Fatal(err)
		}
		o, ok := st.Outcomes.Of(tSess)
		if !ok {
			t.Fatalf("no outcome for the session: %+v", st.Outcomes)
		}
		return o
	}
	if o := outcome(); o != storage.OutcomeComplete {
		t.Fatalf("a session read to its end: %s", o)
	}
	if o := outcome(); o != storage.OutcomeComplete {
		t.Fatalf("an unchanged session read again: %s", o)
	}
	appendText(t, main, "{\"uuid\":\"m2\"")
	if o := outcome(); o != storage.OutcomeIncomplete {
		t.Fatalf("a line still being written: %s", o)
	}
	appendText(t, main, "}\n")
	if o := outcome(); o != storage.OutcomeComplete {
		t.Fatalf("once the line is finished: %s", o)
	}
	held, err := storage.LockSession(z.SessionDir(tSess))
	if err != nil {
		t.Fatal(err)
	}
	if o := outcome(); o != storage.OutcomeIncomplete {
		t.Fatalf("a session another collector holds: %s", o)
	}
	_ = held.Unlock()
}

// A source in conflict stays so on every pass without an error, and its
// session stays incomplete on every one.
func TestAConflictLeavesTheSessionIncomplete(t *testing.T) {
	src, zone := t.TempDir(), t.TempDir()
	main := filepath.Join(src, "-proj-a", tSess+".jsonl")
	mk(t, main, "{\"uuid\":\"m1\"}\n{\"uuid\":\"m2\"}\n")
	col := claudecode.New(src, storage.NewZone(zone), 0)
	if _, err := col.CollectAll(nil); err != nil {
		t.Fatal(err)
	}
	// The source is cut below what was landed: other bytes than those read.
	mk(t, main, "{\"uuid\":\"x\"}\n")
	for pass := 1; pass <= 2; pass++ {
		st, err := col.CollectAll(nil)
		if err != nil {
			t.Fatal(err)
		}
		if o, _ := st.Outcomes.Of(tSess); st.Conflicts == 0 || o != storage.OutcomeIncomplete {
			t.Fatalf("pass %d: conflicts %d, outcome %s", pass, st.Conflicts, o)
		}
	}
}
