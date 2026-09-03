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
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

func TestSlugify(t *testing.T) {
	// '/', '.' and ' ' all collapse to '-', which is why the transform is not
	// reversible and why configuration only ever slugifies forward.
	cases := map[string]string{
		"/Users/w/github/skywalking-horizon-ui":  "-Users-w-github-skywalking-horizon-ui",
		"/private/tmp/agentsessionizer-v04.psix": "-private-tmp-agentsessionizer-v04-psix",
		"/Users/w/Library/Application Support/X": "-Users-w-Library-Application-Support-X",
	}
	for in, want := range cases {
		if got := claudecode.Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSessionIDCaseSensitive(t *testing.T) {
	// Real corpora contain uppercase session ids, so matching must accept them
	// while comparisons stay byte-exact.
	upper := "41090DAB-113C-432A-8594-520FA2A7DA4A"
	if !claudecode.IsSessionID(upper) {
		t.Errorf("uppercase session id rejected: %s", upper)
	}
	if claudecode.IsSessionID("memory") {
		t.Error("memory must not be treated as a session id")
	}
	if claudecode.IsSessionID("not-a-uuid") {
		t.Error("non-uuid accepted as session id")
	}
}

// buildTree writes a synthetic source tree covering every discovery case.
func buildTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const sess = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	const agent1 = "a1111111111111111"
	const agent2 = "a2222222222222222"

	// Main transcript and a direct child, under project A.
	write("-proj-a/"+sess+".jsonl", "{\"type\":\"user\",\"uuid\":\"u1\"}\n")
	write("-proj-a/"+sess+"/subagents/agent-"+agent1+".jsonl", "{\"type\":\"user\"}\n")
	write("-proj-a/"+sess+"/subagents/agent-"+agent1+".meta.json", `{"agentType":"general-purpose","spawnDepth":1}`)
	// A workflow run: child stream, journal and manifest.
	write("-proj-a/"+sess+"/subagents/workflows/wf_r1/agent-"+agent2+".jsonl", "{\"type\":\"user\"}\n")
	write("-proj-a/"+sess+"/subagents/workflows/wf_r1/journal.jsonl", "{\"type\":\"started\",\"agentId\":\""+agent2+"\"}\n")
	write("-proj-a/"+sess+"/workflows/wf_r1.json", `{"runId":"wf_r1","status":"completed"}`)

	// The SAME session also has a directory under project B, which is what
	// happens when a child agent runs in a different working directory.
	write("-proj-b/"+sess+"/subagents/agent-a3333333333333333.jsonl", "{\"type\":\"user\"}\n")

	// Noise that must be ignored.
	write("-proj-a/memory/notes.md", "not a session")
	write("-proj-a/not-a-uuid.jsonl", "{}")
	return root
}

func TestDiscoverIsSessionFirst(t *testing.T) {
	root := buildTree(t)
	sessions, err := claudecode.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1 (session-first grouping should merge across dirs)", len(sessions))
	}
	s := sessions[0]
	if len(s.Dirs) != 2 {
		t.Errorf("got %d source dirs, want 2: %v", len(s.Dirs), s.Dirs)
	}

	count := map[claudecode.SourceKind]int{}
	for _, src := range s.Sources {
		count[src.Kind]++
	}
	want := map[claudecode.SourceKind]int{
		claudecode.SrcMainTranscript:   1,
		claudecode.SrcAgentTranscript:  3, // direct + workflow + the one under project B
		claudecode.SrcAgentMeta:        1,
		claudecode.SrcJournal:          1,
		claudecode.SrcWorkflowManifest: 1,
	}
	for k, w := range want {
		if count[k] != w {
			t.Errorf("kind %d: got %d, want %d", k, count[k], w)
		}
	}
}

func TestDiscoverExcludesMemoryDir(t *testing.T) {
	root := buildTree(t)
	sessions, err := claudecode.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		if s.ID == "memory" {
			t.Fatal("memory directory was treated as a session")
		}
	}
}

// --- tailing ---

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTailStopsAtLastCompleteLine(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	// The third line has no terminating newline: it is an in-flight write and
	// must not be consumed, or we land a record the writer has not finished.
	writeFile(t, p, "{\"a\":1}\n{\"a\":2}\n{\"a\":3")

	cur := storage.NewCursor(storage.CursorAppend, "s.jsonl")
	ch, err := claudecode.TailAppend(p, cur, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.Lines) != 2 {
		t.Fatalf("got %d lines, want 2 (partial tail must be left behind)", len(ch.Lines))
	}
	if ch.NewOrd != 2 {
		t.Errorf("NewOrd = %d, want 2", ch.NewOrd)
	}
	// Two 8-byte lines consumed; the 6-byte partial third line is left behind.
	if ch.NewOffset != 16 {
		t.Errorf("NewOffset = %d, want 16", ch.NewOffset)
	}
}

func TestTailResumesAndNumbersFromCursor(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	writeFile(t, p, "{\"a\":1}\n{\"a\":2}\n")

	cur := storage.NewCursor(storage.CursorAppend, "s.jsonl")
	ch, err := claudecode.TailAppend(p, cur, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	applyChunk(t, cur, p, ch)

	// Append two more lines; ord must continue from the cursor, not restart.
	writeFile(t, p, "{\"a\":1}\n{\"a\":2}\n{\"a\":3}\n{\"a\":4}\n")
	ch2, err := claudecode.TailAppend(p, cur, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(ch2.Lines) != 2 {
		t.Fatalf("got %d lines, want 2 (only the new tail)", len(ch2.Lines))
	}
	if ch2.Lines[0].Ord != 3 || ch2.Lines[1].Ord != 4 {
		t.Errorf("ords = %d,%d; want 3,4 - ord must continue from the cursor",
			ch2.Lines[0].Ord, ch2.Lines[1].Ord)
	}
}

func TestTailDetectsTruncation(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	writeFile(t, p, "{\"a\":1}\n{\"a\":2}\n")
	cur := storage.NewCursor(storage.CursorAppend, "s.jsonl")
	ch, err := claudecode.TailAppend(p, cur, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	applyChunk(t, cur, p, ch)

	writeFile(t, p, "{\"a\":1}\n") // shorter than the cursor offset
	if _, err := claudecode.TailAppend(p, cur, 1<<20); err == nil {
		t.Fatal("expected a conflict on truncation, got nil")
	}
}

func TestTailDetectsRewriteBehindCursor(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	writeFile(t, p, "{\"a\":1}\n{\"a\":2}\n")
	cur := storage.NewCursor(storage.CursorAppend, "s.jsonl")
	ch, err := claudecode.TailAppend(p, cur, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	applyChunk(t, cur, p, ch)

	// Same length, different content behind the cursor, plus new data.
	writeFile(t, p, "{\"a\":9}\n{\"a\":8}\n{\"a\":3}\n")
	_, err = claudecode.TailAppend(p, cur, 1<<20)
	if err == nil {
		t.Fatal("expected a conflict on rewrite behind the cursor, got nil")
	}
	var ce *claudecode.ConflictError
	if !asConflict(err, &ce) {
		t.Fatalf("expected ConflictError, got %T: %v", err, err)
	}
}

func TestTailSourceGone(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gone.jsonl")
	cur := storage.NewCursor(storage.CursorAppend, "gone.jsonl")
	_, err := claudecode.TailAppend(p, cur, 1<<20)
	if err == nil {
		t.Fatal("expected ErrSourceGone")
	}
	// Pruning is normal - Claude Code deletes transcripts - so this must be a
	// distinguishable state, not a generic failure.
	if !isSourceGone(err) {
		t.Fatalf("expected ErrSourceGone, got %v", err)
	}
}

func TestTailNoProgressWhenUnchanged(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	writeFile(t, p, "{\"a\":1}\n")
	cur := storage.NewCursor(storage.CursorAppend, "s.jsonl")
	ch, err := claudecode.TailAppend(p, cur, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	applyChunk(t, cur, p, ch)

	ch2, err := claudecode.TailAppend(p, cur, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(ch2.Lines) != 0 {
		t.Errorf("got %d lines from an unchanged source, want 0", len(ch2.Lines))
	}
}

func applyChunk(t *testing.T, cur *storage.Cursor, path string, ch *claudecode.Chunk) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	d, err := claudecode.TailDigestAt(path, int64(ch.NewOffset))
	if err != nil {
		t.Fatal(err)
	}
	cur.Offset, cur.Ord, cur.LastUUID, cur.TailSHA256 = ch.NewOffset, ch.NewOrd, ch.LastUUID, d
	cur.Size, cur.MTime = fi.Size(), fi.ModTime().Unix()
	_ = time.Now
}

// replaceFile writes body to a new file and renames it over path, so the path
// keeps its name and gets a new inode. This is what a bind mount, a restored
// backup or a copied source tree look like to the cursor.
func replaceFile(t *testing.T, path, body string) {
	t.Helper()
	tmp := path + ".new"
	writeFile(t, tmp, body)
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// tailThenRemember tails a fresh source and records everything the collector
// would, including the identity, which applyChunk alone does not.
func tailThenRemember(t *testing.T, p string) *storage.Cursor {
	t.Helper()
	cur := storage.NewCursor(storage.CursorAppend, filepath.Base(p))
	ch, err := claudecode.TailAppend(p, cur, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	applyChunk(t, cur, p, ch)
	cur.Dev, cur.Ino = ch.Dev, ch.Ino
	if cur.Ino == 0 {
		t.Skip("no inode on this platform")
	}
	return cur
}

func TestTailAcceptsSameBytesUnderNewInode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	writeFile(t, p, "{\"a\":1}\n{\"a\":2}\n")
	cur := tailThenRemember(t, p)

	// The same bytes, then more, under a new inode.
	replaceFile(t, p, "{\"a\":1}\n{\"a\":2}\n{\"a\":3}\n")
	ch, err := claudecode.TailAppend(p, cur, 1<<20)
	if err != nil {
		t.Fatalf("same bytes under a new inode must not conflict: %v", err)
	}
	if len(ch.Lines) != 1 || ch.Lines[0].Ord != 3 {
		t.Fatalf("got %d lines, want only the new third line", len(ch.Lines))
	}
	if !ch.Moved || ch.Ino == cur.Ino {
		t.Fatalf("chunk must report the new identity: moved=%v ino=%d old=%d", ch.Moved, ch.Ino, cur.Ino)
	}
}

func TestTailReportsNewInodeWhenNothingIsNew(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	writeFile(t, p, "{\"a\":1}\n{\"a\":2}\n")
	cur := tailThenRemember(t, p)

	replaceFile(t, p, "{\"a\":1}\n{\"a\":2}\n")
	ch, err := claudecode.TailAppend(p, cur, 1<<20)
	if err != nil {
		t.Fatalf("an identical copy must not conflict: %v", err)
	}
	if len(ch.Lines) != 0 || !ch.Moved || ch.Ino == cur.Ino {
		t.Fatalf("want no lines and the new identity, got lines=%d moved=%v", len(ch.Lines), ch.Moved)
	}
}

func TestTailRejectsDifferentBytesUnderNewInode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	writeFile(t, p, "{\"a\":1}\n{\"a\":2}\n")
	cur := tailThenRemember(t, p)

	// Same length behind the cursor, different bytes: a real replacement.
	replaceFile(t, p, "{\"a\":9}\n{\"a\":8}\n{\"a\":3}\n")
	_, err := claudecode.TailAppend(p, cur, 1<<20)
	var ce *claudecode.ConflictError
	if !asConflict(err, &ce) {
		t.Fatalf("expected ConflictError, got %T: %v", err, err)
	}
}

func TestTailConflictsOnNewInodeWithoutTailDigest(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	writeFile(t, p, "{\"a\":1}\n{\"a\":2}\n")
	cur := tailThenRemember(t, p)
	cur.TailSHA256 = "" // an old cursor that never recorded one

	replaceFile(t, p, "{\"a\":1}\n{\"a\":2}\n{\"a\":3}\n")
	_, err := claudecode.TailAppend(p, cur, 1<<20)
	var ce *claudecode.ConflictError
	if !asConflict(err, &ce) {
		t.Fatalf("without a digest the bytes cannot be checked, so this must conflict; got %T: %v", err, err)
	}
}
