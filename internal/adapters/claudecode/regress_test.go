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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// landedSeqs returns every landed sequence number under a session, so a
// duplicate is detectable.
func landedSeqs(t *testing.T, sessionDir string) []string {
	t.Helper()
	var out []string
	_ = filepath.Walk(sessionDir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || !strings.HasSuffix(p, ".sd") {
			return nil
		}
		name := filepath.Base(p)
		if i := strings.LastIndex(name, "-"); i >= 0 {
			out = append(out, strings.TrimSuffix(name[i+1:], ".sd"))
		}
		return nil
	})
	return out
}

// TestSeqSurvivesMidSessionCrash is the regression for the worst defect found:
// session.state is written once per session while landed files commit per
// source, so a crash mid-session left the counter behind the filesystem and
// reissued sequence numbers. Because the assembler tracks progress with one
// monotonic watermark, a reissued sequence is never read - silent data loss.
func TestSeqSurvivesMidSessionCrash(t *testing.T) {
	src, zone := t.TempDir(), t.TempDir()
	main := filepath.Join(src, "-proj-a", tSess+".jsonl")
	agent := filepath.Join(src, "-proj-a", tSess, "subagents", "agent-"+tAgent+".jsonl")
	mk(t, main, "{\"uuid\":\"m1\"}\n")
	mk(t, agent, "{\"uuid\":\"c1\"}\n")

	z := storage.NewZone(zone)
	col := claudecode.New(src, z, 0)
	if _, err := col.CollectAll(nil); err != nil {
		t.Fatal(err)
	}

	// Simulate the crash: roll session.state back to what is on disk when the
	// process dies after landing but before the end-of-session save.
	statePath := z.SessionStatePath(tSess)
	stale, err := storage.LoadSessionState(statePath, tSess)
	if err != nil {
		t.Fatal(err)
	}
	stale.NextSeq = 1
	if err := os.WriteFile(statePath, []byte(fmt.Sprintf(
		"schema 1\nsession %s\nnext_seq 1\nliveness unknown\n", tSess)), 0o644); err != nil {
		t.Fatal(err)
	}

	// New data on both streams; the collector must not reuse seqs 1 and 2.
	mk(t, main, "{\"uuid\":\"m1\"}\n{\"uuid\":\"m2\"}\n")
	mk(t, agent, "{\"uuid\":\"c1\"}\n{\"uuid\":\"c2\"}\n")
	if _, err := col.CollectAll(nil); err != nil {
		t.Fatal(err)
	}

	seqs := landedSeqs(t, z.SessionDir(tSess))
	seen := map[string]int{}
	for _, s := range seqs {
		seen[s]++
	}
	for s, n := range seen {
		if n > 1 {
			t.Errorf("sequence %s used by %d landed files - the assembler watermark would drop all but one", s, n)
		}
	}
	if len(seqs) != 4 {
		t.Errorf("got %d landed files (%v), want 4", len(seqs), seqs)
	}
}

// TestConcurrentCollectorsDoNotCollide covers two collectors sharing a zone.
func TestConcurrentCollectorsDoNotCollide(t *testing.T) {
	src, zone := t.TempDir(), t.TempDir()
	mk(t, filepath.Join(src, "-proj-a", tSess+".jsonl"), "{\"uuid\":\"m1\"}\n")
	mk(t, filepath.Join(src, "-proj-a", tSess, "subagents", "agent-"+tAgent+".jsonl"), "{\"uuid\":\"c1\"}\n")

	z := storage.NewZone(zone)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			col := claudecode.New(src, z, 0)
			_, _ = col.CollectAll(nil)
		}()
	}
	wg.Wait()

	seen := map[string]int{}
	for _, s := range landedSeqs(t, z.SessionDir(tSess)) {
		seen[s]++
	}
	for s, n := range seen {
		if n > 1 {
			t.Errorf("concurrent collectors both issued sequence %s (%d files)", s, n)
		}
	}
}

// TestOncePassDrainsLargeSource is the regression for Chunk.More being computed
// and never read: a single pass landed one budget-sized window and reported a
// clean, complete-looking result.
func TestOncePassDrainsLargeSource(t *testing.T) {
	src, zone := t.TempDir(), t.TempDir()
	var b strings.Builder
	const lines = 400
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&b, "{\"uuid\":\"u%d\",\"pad\":\"%s\"}\n", i, strings.Repeat("x", 200))
	}
	mk(t, filepath.Join(src, "-proj-a", tSess+".jsonl"), b.String())

	// A budget far smaller than the source, so a single window is nowhere near enough.
	col := claudecode.New(src, storage.NewZone(zone), 4096)
	st, err := col.CollectAll(nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Records != lines {
		t.Errorf("landed %d of %d records in one pass - the pass under-collected", st.Records, lines)
	}
	if !st.Complete() {
		t.Errorf("pass reported incomplete: pending=%d errors=%v", st.Pending, st.Errors)
	}
}

// TestOversizedLineDoesNotStall covers a single source line larger than the
// byte budget, which previously returned zero lines with no error, forever.
func TestOversizedLineDoesNotStall(t *testing.T) {
	src, zone := t.TempDir(), t.TempDir()
	big := fmt.Sprintf("{\"uuid\":\"big\",\"pad\":\"%s\"}\n", strings.Repeat("y", 50_000))
	mk(t, filepath.Join(src, "-proj-a", tSess+".jsonl"), big+"{\"uuid\":\"after\"}\n")

	col := claudecode.New(src, storage.NewZone(zone), 1024) // budget far below one line
	st, err := col.CollectAll(nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Records != 2 {
		t.Errorf("landed %d records, want 2 - an oversized line stalled the source", st.Records)
	}
}

// TestStubDirectoryDoesNotVetoSession is the regression for the discovery half
// of the exclusion bug: a session directory containing nothing collectable was
// still recorded as a source directory, letting it disqualify the whole session.
func TestStubDirectoryDoesNotVetoSession(t *testing.T) {
	src := t.TempDir()
	mk(t, filepath.Join(src, "-Users-me-proj", tSess+".jsonl"), "{\"uuid\":\"m1\"}\n")
	// Claude Code files workflow scripts under whatever cwd the agent had; we
	// never collect scripts, so this directory yields nothing.
	mk(t, filepath.Join(src, "-private-tmp-scratch", tSess, "workflows", "scripts", "x.js"), "//\n")

	sessions, err := claudecode.Discover(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	if got := sessions[0].Dirs; len(got) != 1 || got[0] != "-Users-me-proj" {
		t.Errorf("Dirs = %v; a directory yielding no sources must not be recorded", got)
	}
	m := claudecode.NewMatcher(nil, []string{"/private/tmp/**"})
	if !m.Match(sessions[0]) {
		t.Error("an empty stub directory disqualified a real session")
	}
}

// TestScriptRunIDParse pins the filename rule Claude Code uses for workflow
// scripts. It is anchored to the end so a label containing "-wf_" cannot
// shadow the real run id.
func TestScriptRunIDParse(t *testing.T) {
	cases := map[string]string{
		"terminal-card-mappers-wf_afa31e47-f6c.js": "wf_afa31e47-f6c",
		"oauth2-provider-facts-wf_7e2f3f7a-e04.js": "wf_7e2f3f7a-e04",
		"a-wf_decoy-then-wf_real123.js":            "wf_real123",
		"plain.js":                                 "",
		"no-extension-wf_abc":                      "",
	}
	for name, want := range cases {
		got, ok := claudecode.ScriptRunID(name)
		if want == "" {
			if ok {
				t.Errorf("%s: expected no run id, got %q", name, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("%s: got %q (ok=%v), want %q", name, got, ok, want)
		}
	}
}

// TestScriptCollectedIntoItsRunDirectory covers the case that motivated
// collecting scripts at all: Claude Code files them under whatever working
// directory the agent had, so they are the one artifact that genuinely lives
// outside its session's own directory. Collecting them reunifies the session.
func TestScriptCollectedIntoItsRunDirectory(t *testing.T) {
	src, zone := t.TempDir(), t.TempDir()
	// Main transcript in the real project directory.
	mk(t, filepath.Join(src, "-proj-a", tSess+".jsonl"), "{\"uuid\":\"m1\"}\n")
	mk(t, filepath.Join(src, "-proj-a", tSess, "workflows", "wf_r1.json"), `{"runId":"wf_r1"}`)
	// Script filed under an unrelated directory, for the same session and run.
	mk(t, filepath.Join(src, "-proj-elsewhere", tSess, "workflows", "scripts", "flow-wf_r1.js"),
		"export const meta = { name: 'flow' }\n")

	col := claudecode.New(src, storage.NewZone(zone), 0)
	st, err := col.CollectAll(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", st.Errors)
	}

	runDir := storage.NewZone(zone).RunDir(tSess, "wf_r1")
	ents, err := os.ReadDir(runDir)
	if err != nil {
		t.Fatalf("run directory not created: %v", err)
	}
	var haveScript, haveManifest bool
	for _, e := range ents {
		switch {
		case strings.HasPrefix(e.Name(), "script-") && strings.HasSuffix(e.Name(), ".sd"):
			haveScript = true
		case strings.HasPrefix(e.Name(), "manifest-") && strings.HasSuffix(e.Name(), ".sd"):
			haveManifest = true
		}
	}
	if !haveScript {
		t.Errorf("script from another directory was not collected into %s", runDir)
	}
	if !haveManifest {
		t.Errorf("manifest not collected into %s", runDir)
	}
}

// TestScriptPayloadIsKeptWhole checks that a non-JSON source - a
// JavaScript file - still round trips. The envelope wraps it as a string so the
// landed file stays parseable, with the bytes recoverable exactly.
func TestScriptPayloadIsKeptWhole(t *testing.T) {
	src, zone := t.TempDir(), t.TempDir()
	const body = "export const meta = {\n  name: 'flow',\n}\n"
	mk(t, filepath.Join(src, "-proj-a", tSess+".jsonl"), "{\"uuid\":\"m1\"}\n")
	mk(t, filepath.Join(src, "-proj-a", tSess, "workflows", "scripts", "flow-wf_r1.js"), body)

	col := claudecode.New(src, storage.NewZone(zone), 0)
	if _, err := col.CollectAll(nil); err != nil {
		t.Fatal(err)
	}
	runDir := storage.NewZone(zone).RunDir(tSess, "wf_r1")
	ents, _ := os.ReadDir(runDir)
	var landed string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "script-") && strings.HasSuffix(e.Name(), ".sd") {
			landed = filepath.Join(runDir, e.Name())
		}
	}
	if landed == "" {
		t.Fatal("no landed script file")
	}
	f, err := os.Open(landed)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r, err := sessiondata.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	// A workflow script is JavaScript. The dialect cannot break it into parts,
	// so it keeps the bytes whole in an unknown part rather than describing it
	// or dropping it - which is the rule that makes converting on read safe for
	// anything a dialect does not understand.
	if len(rec.Parts) != 1 || rec.Parts[0].Kind != sessiondata.PartUnknown {
		t.Fatalf("script converted to %d part(s): %+v", len(rec.Parts), rec.Parts)
	}
	var got string
	if err := json.Unmarshal(rec.Parts[0].Data, &got); err != nil {
		t.Fatal(err)
	}
	if got != strings.TrimRight(body, "\n") {
		t.Errorf("script bytes not preserved:\n got %q\nwant %q", got, strings.TrimRight(body, "\n"))
	}
}

// TestIndexGapIsClosedAfterInterruptedPass is the regression for a silent,
// permanent data gap.
//
// Cursors commit per source while the index is written once per session. A
// crash between them advances the cursors without recording their entries, and
// because nothing re-lands, the index would stay short forever while
// indexed_seq claimed to cover everything.
func TestIndexGapIsClosedAfterInterruptedPass(t *testing.T) {
	src, zone := t.TempDir(), t.TempDir()
	main := filepath.Join(src, "-proj-a", tSess+".jsonl")
	mk(t, main, "{\"uuid\":\"m1\"}\n{\"uuid\":\"m2\"}\n")

	z := storage.NewZone(zone)
	col := claudecode.New(src, z, 0)
	if _, err := col.CollectAll(nil); err != nil {
		t.Fatal(err)
	}

	// Simulate the crash: the landed files and cursors survive, the index does
	// not. This is exactly the on-disk state after a kill between the two.
	if err := os.RemoveAll(z.IndexDir(tSess)); err != nil {
		t.Fatal(err)
	}

	// Land more data. Without gap recovery the index would hold only m3,
	// silently missing m1 and m2 which no longer re-land.
	mk(t, main, "{\"uuid\":\"m1\"}\n{\"uuid\":\"m2\"}\n{\"uuid\":\"m3\"}\n")
	st, err := col.CollectAll(nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Reindexed == 0 {
		t.Error("expected the interrupted pass to be detected and re-indexed")
	}

	ix, ok, err := index.Load(z.IndexDir(tSess), tSess)
	if err != nil || !ok {
		t.Fatalf("index missing after recovery: ok=%v err=%v", ok, err)
	}
	for _, want := range []string{"m1", "m2", "m3"} {
		if _, found := ix.EntryByRecord(want); !found {
			t.Errorf("record %q missing from the index after recovery", want)
		}
	}
}

// TestIndexRebuildsFromLandedFiles covers a wholesale loss of the index - the
// case that makes it safe to treat the index as derived and disposable.
func TestIndexRebuildsFromLandedFiles(t *testing.T) {
	src, zone := t.TempDir(), t.TempDir()
	base := filepath.Join(src, "-proj-a", tSess)
	mk(t, filepath.Join(src, "-proj-a", tSess+".jsonl"), "{\"uuid\":\"m1\"}\n")
	mk(t, filepath.Join(base, "subagents", "agent-"+tAgent+".jsonl"), "{\"uuid\":\"c1\"}\n")
	mk(t, filepath.Join(base, "subagents", "workflows", "wf_r1", "journal.jsonl"),
		"{\"type\":\"started\",\"agentId\":\""+tAgent+"\"}\n")

	z := storage.NewZone(zone)
	col := claudecode.New(src, z, 0)
	if _, err := col.CollectAll(nil); err != nil {
		t.Fatal(err)
	}
	before, ok, err := index.Load(z.IndexDir(tSess), tSess)
	if err != nil || !ok {
		t.Fatal("no index after first pass")
	}
	wantEntries := len(before.Entries)

	// Throw the index away entirely and rebuild it from the landed files alone.
	if err := os.RemoveAll(z.IndexDir(tSess)); err != nil {
		t.Fatal(err)
	}
	rebuilt := index.New(tSess)
	n, err := index.Rebuild(z, tSess, rebuilt, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != wantEntries {
		t.Errorf("rebuilt %d entries, want %d", n, wantEntries)
	}
	for _, want := range []string{"m1", "c1"} {
		if _, found := rebuilt.EntryByRecord(want); !found {
			t.Errorf("record %q missing from the rebuilt index", want)
		}
	}
	// Stream attribution must survive a rebuild that has only the landed files.
	if got := rebuilt.Stream(tAgent); len(got) == 0 {
		t.Error("child stream lost its attribution on rebuild")
	}
}

// TestRecoveredIndexPersistsWithNothingNewLanded covers the steady state, which
// is the common one: a watch loop finds a session whose index is behind and
// whose sources have not changed.
//
// Persisting the index only when a source lands makes that case rebuild the
// index on every pass and throw it away every time - the recovery never sticks,
// the work repeats forever, and indexed_seq on disk keeps claiming coverage the
// stored index does not have.
func TestRecoveredIndexPersistsWithNothingNewLanded(t *testing.T) {
	src, zone := t.TempDir(), t.TempDir()
	main := filepath.Join(src, "-proj-a", tSess+".jsonl")
	mk(t, main, "{\"uuid\":\"m1\"}\n{\"uuid\":\"m2\"}\n")

	z := storage.NewZone(zone)
	col := claudecode.New(src, z, 0)
	if _, err := col.CollectAll(nil); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(z.IndexDir(tSess)); err != nil {
		t.Fatal(err)
	}

	// The source is untouched, so this pass lands nothing.
	st, err := col.CollectAll(nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Records != 0 {
		t.Fatalf("the pass landed %d records; the test needs a pass that lands nothing", st.Records)
	}
	if st.Reindexed == 0 {
		t.Fatal("the missing index was not detected")
	}

	ix, ok, err := index.Load(z.IndexDir(tSess), tSess)
	if err != nil || !ok {
		t.Fatalf("the rebuilt index was not written: ok=%v err=%v", ok, err)
	}
	if len(ix.Entries) != 2 {
		t.Fatalf("index holds %d entries, want 2", len(ix.Entries))
	}

	// And it must stick: a third pass has nothing left to recover.
	third, err := col.CollectAll(nil)
	if err != nil {
		t.Fatal(err)
	}
	if third.Reindexed != 0 {
		t.Errorf("re-indexed %d entries again; the recovery was discarded", third.Reindexed)
	}
}
