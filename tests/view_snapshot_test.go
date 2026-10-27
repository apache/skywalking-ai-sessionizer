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

package tests_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/format"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/pulltest"
	"github.com/apache/skywalking-ai-sessionizer/internal/retention"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario/run"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// builtInRounds is a scenario's conversation cut into rounds of at most
// budget bytes, so its chain has several.
func builtInRounds(t *testing.T, path string, budget int64) (*storage.Zone, string) {
	t.Helper()
	sc, err := scenario.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	b, err := scenario.Build(sc, scenario.FormatSD, out, scenario.Options{At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	z := storage.NewZone(out)
	for {
		r, err := parse.Session(z, parse.Options{Conversation: b.Session, Session: b.Session, MaxRoundBytes: budget})
		if err != nil {
			t.Fatal(err)
		}
		if !r.Changed() || !r.More {
			break
		}
	}
	return z, b.Session
}

// fatals records what a snapshot's Fatal callback was told.
type fatals struct {
	mu   sync.Mutex
	errs []error
}

func (f *fatals) tell(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs = append(f.errs, err)
}

func (f *fatals) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.errs)
}

// snapshotServer is a view server bound to a snapshot of the root's head.
func snapshotServer(t *testing.T, z *storage.Zone, id string) (*view.Server, *view.Snapshot, *fatals) {
	t.Helper()
	snap := snapshotOf(t, z, id)
	told := &fatals{}
	snap.Fatal = told.tell
	srv := view.New(z, nil)
	srv.SetSnapshot(snap)
	return srv, snap, told
}

// roundFiles are the chain's round files in order.
func roundFiles(t *testing.T, z *storage.Zone, id string) []sessionflow.RoundFile {
	t.Helper()
	files, err := sessionflow.OpenChain(z.Root(), id).List()
	if err != nil || len(files) < 3 {
		t.Fatalf("%d rounds: %v", len(files), err)
	}
	return files
}

// damageRound flips a byte in the middle of a round file, keeping its name.
func damageRound(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)/2] ^= 0x01
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A snapshot folds through exactly the head its marker names: a damaged
// interior round, another digest at the same round, or a chain that goes on
// past the head fails the load.
func TestASnapshotFoldsExactlyToItsHead(t *testing.T) {
	const path = "scenarios/growth.yaml"
	z, id := builtInRounds(t, path, 4<<10)
	srv, snap, told := snapshotServer(t, z, id)
	c, err := srv.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if c.View.Round != snap.Round || c.View.Digest != snap.Digest || told.count() != 0 {
		t.Fatalf("the fold reached round %d %s, the marker names %d %s", c.View.Round, c.View.Digest, snap.Round, snap.Digest)
	}

	for name, bind := range map[string]func(*view.Snapshot){
		"another digest at the head": func(s *view.Snapshot) { s.Digest = strings.Repeat("0", len(s.Digest)) },
		"an earlier head":            func(s *view.Snapshot) { s.Round-- },
	} {
		s := *snap
		bind(&s)
		told := &fatals{}
		s.Fatal = told.tell
		other := view.New(z, nil)
		other.SetSnapshot(&s)
		if _, err := other.Load(id); !errors.Is(err, view.ErrEvidence) || told.count() != 1 {
			t.Errorf("%s: %v, told %d", name, err, told.count())
		}
	}

	files := roundFiles(t, z, id)
	srv, _, told = snapshotServer(t, z, id)
	damageRound(t, files[1].Path)
	if _, err := srv.Load(id); !errors.Is(err, view.ErrEvidence) || told.count() != 1 {
		t.Fatalf("a damaged interior round: %v, told %d", err, told.count())
	}
	// An earlier round is read only once the whole chain was checked, so a
	// damaged round after it fails it too.
	if _, err := srv.LoadAt(id, 1); !errors.Is(err, view.ErrEvidence) {
		t.Fatalf("round 1 before a damaged round 2: %v", err)
	}
}

// An earlier round of a sound snapshot is bound to that round's own digest.
func TestAnEarlierRoundOfASnapshotIsBoundToItsDigest(t *testing.T) {
	z, id := builtInRounds(t, "scenarios/growth.yaml", 4<<10)
	srv, snap, _ := snapshotServer(t, z, id)
	files := roundFiles(t, z, id)
	c, err := srv.LoadAt(id, 2)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := c.Build()
	if err != nil {
		t.Fatal(err)
	}
	if doc.Head.Round != 2 || !strings.HasPrefix(doc.Head.Digest, files[1].Digest) {
		t.Fatalf("round 2 read as %d %s, its file is %s", doc.Head.Round, doc.Head.Digest, files[1].Digest)
	}
	if _, err := srv.LoadAt(id, snap.Round+1); err == nil || errors.Is(err, view.ErrEvidence) {
		t.Fatalf("a round past the head: %v", err)
	}
}

// A read of a snapshot's evidence that fails is an error everywhere it is
// made, and the page is told of it: the times a load reads, the records a
// document is built from, a record asked for and the records a judge
// reads. A record that is not there is a question about whole evidence, and
// tells nothing.
func TestASnapshotReadFailsRatherThanLeavesOut(t *testing.T) {
	z, id := builtFixture(t)
	srv, snap, told := snapshotServer(t, z, id)
	c, err := srv.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Record(1, 100000, nil); !errors.Is(err, view.ErrNoRecord) || told.count() != 0 {
		t.Fatalf("a row past the end: %v, told %d", err, told.count())
	}
	if _, err := c.Record(100000, 1, nil); !errors.Is(err, view.ErrNoRecord) || told.count() != 0 {
		t.Fatalf("a sequence the snapshot does not hold: %v, told %d", err, told.count())
	}
	if err := damage(z, snap.Landed[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Build(); !errors.Is(err, view.ErrEvidence) {
		t.Fatalf("building the document: %v", err)
	}
	if _, err := c.Record(1, 1, nil); !errors.Is(err, view.ErrEvidence) {
		t.Fatalf("a record: %v", err)
	}
	if _, err := c.RecordsChecked([]sessionflow.Ref{{Seq: 1, Row: 1}}); !errors.Is(err, view.ErrEvidence) {
		t.Fatalf("the records a judge reads: %v", err)
	}
	if told.count() < 3 {
		t.Fatalf("the page was told of %d failed reads", told.count())
	}
	again := view.New(z, nil)
	told = &fatals{}
	bound := *snap
	bound.Fatal = told.tell
	again.SetSnapshot(&bound)
	if _, err := again.Load(id); !errors.Is(err, view.ErrEvidence) || told.count() != 1 {
		t.Fatalf("the times a load reads: %v, told %d", err, told.count())
	}
}

// A snapshot's files are looked for in its frozen inventory only: a file
// with a landed name anywhere else in the session, or a link in a landed
// file's place, is never read.
func TestASnapshotReadsOnlyItsInventory(t *testing.T) {
	z, id := builtFixture(t)
	srv, snap, _ := snapshotServer(t, z, id)
	session := filepath.Dir(filepath.Join(z.Root(), filepath.FromSlash(snap.Landed[1])))
	for session != z.Root() && filepath.Dir(session) != z.Root() {
		session = filepath.Dir(session)
	}
	work := filepath.Join(session, storage.JudgmentsDir, "j000001", storage.WorkDir)
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(z.Root(), filepath.FromSlash(snap.Landed[1])))
	if err != nil {
		t.Fatal(err)
	}
	var top uint64
	for seq := range snap.Landed {
		top = max(top, seq)
	}
	base := filepath.Base(snap.Landed[1])
	decoy := fmt.Sprintf("%s%06d.sd", base[:strings.LastIndexByte(base, '-')+1], top+1)
	if err := os.WriteFile(filepath.Join(work, decoy), b, 0o444); err != nil {
		t.Fatal(err)
	}
	c, err := srv.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Record(top+1, 1, nil); !errors.Is(err, view.ErrNoRecord) {
		t.Fatalf("a file in a work directory was read: %v", err)
	}
	ordinary, err := view.New(z, nil).Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ordinary.Record(top+1, 1, nil); err == nil {
		t.Fatal("an ordinary root read a file in a work directory")
	}
}

// decoyBeside copies a landed file to an unused sequence beside it, under a
// name live discovery takes for landed, and returns its path.
func decoyBeside(t *testing.T, z *storage.Zone, snap *view.Snapshot) string {
	t.Helper()
	var top uint64
	for seq := range snap.Landed {
		top = max(top, seq)
	}
	src := filepath.Join(z.Root(), filepath.FromSlash(snap.Landed[1]))
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(src)
	decoy := filepath.Join(filepath.Dir(src), fmt.Sprintf("%s%06d.sd", base[:strings.LastIndexByte(base, '-')+1], top+1))
	if err := os.WriteFile(decoy, b, 0o444); err != nil {
		t.Fatal(err)
	}
	return decoy
}

// A snapshot's document describes its frozen inventory and nothing else in
// its session: a file landed beside it later is not in the document, where
// an ordinary root's document takes it.
func TestASnapshotDocumentHoldsOnlyItsInventory(t *testing.T) {
	z, id := builtFixture(t)
	srv, snap, _ := snapshotServer(t, z, id)
	decoy := decoyBeside(t, z, snap)
	rel, _ := filepath.Rel(z.Root(), decoy)
	listed := func(srv *view.Server) bool {
		c, err := srv.Load(id)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := c.Build()
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range doc.Files {
			if f.File == filepath.ToSlash(rel) {
				return true
			}
		}
		return false
	}
	if listed(srv) {
		t.Fatal("a snapshot's document took a file its inventory does not list")
	}
	if !listed(view.New(z, nil)) {
		t.Fatal("an ordinary root's document did not take the file: the check above proves nothing")
	}
}

// rewriteRecord changes one record's text in a landed file and writes the
// file again as valid Session Data, its closing count and digest made
// anew: only the chain, which bound the old bytes, can tell.
func rewriteRecord(t *testing.T, path string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	h, recs, err := sessiondata.All(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	changed := false
	for _, r := range recs {
		for i := range r.Parts {
			if r.Parts[i].Text != "" && !changed {
				r.Parts[i].Text += " (changed)"
				changed = true
			}
		}
	}
	if !changed {
		t.Fatal("no text to change")
	}
	var out bytes.Buffer
	w, err := sessiondata.NewWriter(&out, &h)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if err := w.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(path, 0o644)
	if err := os.WriteFile(path, out.Bytes(), 0o444); err != nil {
		t.Fatal(err)
	}
}

// A landed file whose records changed, rewritten as valid Session Data,
// fails a snapshot's load: its chain is checked against its files there,
// where an ordinary root reports the mismatch inside a document.
func TestAChangedFileFailsASnapshotsLoad(t *testing.T) {
	z, id := builtFixture(t)
	snap := snapshotOf(t, z, id)
	rewriteRecord(t, filepath.Join(z.Root(), filepath.FromSlash(snap.Landed[1])))
	told := &fatals{}
	snap.Fatal = told.tell
	srv := view.New(z, nil)
	srv.SetSnapshot(snap)
	if _, err := srv.Load(id); !errors.Is(err, view.ErrEvidence) || told.count() == 0 {
		t.Fatalf("a changed file: %v, told %d", err, told.count())
	}
	c, err := view.New(z, nil).Load(id)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := c.Build()
	if err != nil {
		t.Fatal(err)
	}
	if doc.Summary.State == "verified" {
		t.Fatal("the change went unseen everywhere: the check above proves nothing")
	}
}

// A snapshot's file is read as the one its load read: a link in its place,
// another copy of the same bytes, or a file of another sequence is damage.
func TestASnapshotFileReplacedAfterItsLoadIsDamage(t *testing.T) {
	z, id := builtFixture(t)
	path := func(snap *view.Snapshot, seq uint64) string {
		return filepath.Join(z.Root(), filepath.FromSlash(snap.Landed[seq]))
	}
	replace := map[string]func(*view.Snapshot){
		"another copy": func(snap *view.Snapshot) {
			p := path(snap, 1)
			b, _ := os.ReadFile(p)
			_ = os.Remove(p)
			_ = os.WriteFile(p, b, 0o444)
		},
		"another sequence": func(snap *view.Snapshot) {
			p := path(snap, 1)
			b, _ := os.ReadFile(path(snap, 2))
			_ = os.Remove(p)
			_ = os.WriteFile(p, b, 0o444)
		},
	}
	if runtime.GOOS != "windows" {
		replace["a link"] = func(snap *view.Snapshot) {
			p := path(snap, 1)
			b, _ := os.ReadFile(p)
			elsewhere := filepath.Join(t.TempDir(), filepath.Base(p))
			_ = os.WriteFile(elsewhere, b, 0o444)
			_ = os.Remove(p)
			_ = os.Symlink(elsewhere, p)
		}
	}
	for name, fn := range replace {
		t.Run(name, func(t *testing.T) {
			z, id = builtFixture(t)
			srv, snap, told := snapshotServer(t, z, id)
			if len(snap.Landed) < 2 {
				t.Fatal("the fixture has one landed file")
			}
			c, err := srv.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			fn(snap)
			if _, err := c.Record(1, 1, nil); !errors.Is(err, view.ErrEvidence) || told.count() == 0 {
				t.Fatalf("%v, told %d", err, told.count())
			}
		})
	}
}

// A file whose header names another sequence than the one its inventory
// lists fails the load that first reads it.
func TestASnapshotFileOfAnotherSequenceFailsTheLoad(t *testing.T) {
	z, id := builtFixture(t)
	srv, snap, told := snapshotServer(t, z, id)
	p := filepath.Join(z.Root(), filepath.FromSlash(snap.Landed[1]))
	b, err := os.ReadFile(filepath.Join(z.Root(), filepath.FromSlash(snap.Landed[2])))
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(p, 0o644)
	if err := os.WriteFile(p, b, 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Load(id); !errors.Is(err, view.ErrEvidence) || !strings.Contains(err.Error(), "sequence") || told.count() == 0 {
		t.Fatalf("%v, told %d", err, told.count())
	}
}

// builtClaude is a scenario built as Claude Code writes it and collected,
// provider bodies and all.
func builtClaude(t *testing.T, name string) (*storage.Zone, string) {
	t.Helper()
	out := t.TempDir()
	rep, err := run.Check(filepath.Join("scenarios", name+".yaml"), run.Options{Out: out, Formats: []scenario.Format{scenario.FormatClaudeCode}})
	if err != nil || rep.Failed {
		t.Fatalf("building %s: %v %v", name, err, rep.Lines)
	}
	root := filepath.Join(out, string(scenario.FormatClaudeCode))
	entries, err := os.ReadDir(filepath.Join(root, "_conversations"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("conversations: %v", err)
	}
	return storage.NewZone(root), entries[0].Name()
}

// The page over a snapshot serves a provider body whole and checked, and a
// body file that went away is damage the page is told of, never a file left
// out of a successful answer; so is a conversation the list cannot load.
func TestASnapshotPageNeverLeavesAFileOut(t *testing.T) {
	z, id := builtClaude(t, "provider-bodies")
	srv, snap, told := snapshotServer(t, z, id)
	page := httptest.NewServer(srv.Handler())
	defer page.Close()
	var body uint64
	for seq, rel := range snap.Landed {
		if strings.Contains(rel, "/provider_body/") {
			body = seq
			break
		}
	}
	if body == 0 {
		t.Fatal("the scenario landed no provider body")
	}
	get := func(path string) int {
		resp, err := http.Get(page.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	files := fmt.Sprintf("/api/c/%s/files?seq=%d", id, body)
	if code := get(files); code != http.StatusOK || told.count() != 0 {
		t.Fatalf("a whole body: %d, told %d", code, told.count())
	}
	p := filepath.Join(z.Root(), filepath.FromSlash(snap.Landed[body]))
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if code := get(files); code != http.StatusInternalServerError || told.count() == 0 {
		t.Fatalf("a body that went away: %d, told %d", code, told.count())
	}
	// A fresh page over the damaged snapshot: its list fails rather than
	// leave the conversation out.
	again, _, told := snapshotServerOf(t, z, snap)
	list := httptest.NewServer(again.Handler())
	defer list.Close()
	resp, err := http.Get(list.URL + "/api/conversations")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError || told.count() == 0 {
		t.Fatalf("the list: %d, told %d", resp.StatusCode, told.count())
	}
}

// A snapshot's list holds the conversation its marker names even when its
// rounds went: the load of it fails as damage, where a list read from the
// directory would answer an empty list.
func TestASnapshotListKeepsAConversationWhoseRoundsWent(t *testing.T) {
	z, id := builtFixture(t)
	snap := snapshotOf(t, z, id)
	rounds, err := sessionflow.OpenChain(z.Root(), id).List()
	if err != nil || len(rounds) == 0 {
		t.Fatalf("%d rounds: %v", len(rounds), err)
	}
	for _, rf := range rounds {
		if err := os.Remove(rf.Path); err != nil {
			t.Fatal(err)
		}
	}
	srv, _, told := snapshotServerOf(t, z, snap)
	page := httptest.NewServer(srv.Handler())
	defer page.Close()
	resp, err := http.Get(page.URL + "/api/conversations")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError || told.count() == 0 {
		t.Fatalf("the list: %d, told %d", resp.StatusCode, told.count())
	}
}

// snapshotServerOf is a view server bound to a snapshot already taken.
func snapshotServerOf(t *testing.T, z *storage.Zone, snap *view.Snapshot) (*view.Server, *view.Snapshot, *fatals) {
	t.Helper()
	bound := *snap
	told := &fatals{}
	bound.Fatal = told.tell
	srv := view.New(z, nil)
	srv.SetSnapshot(&bound)
	return srv, &bound, told
}

// rewriteHeader writes a landed file again as valid Session Data with its
// header changed by fn.
func rewriteHeader(t *testing.T, path string, fn func(*sessiondata.Header)) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	h, recs, err := sessiondata.All(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	fn(&h)
	var out bytes.Buffer
	w, err := sessiondata.NewWriter(&out, &h)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if err := w.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(path, 0o644)
	if err := os.WriteFile(path, out.Bytes(), 0o444); err != nil {
		t.Fatal(err)
	}
}

// A load reads each file as what its inventory lists: not through a link,
// and with a header naming the snapshot's session and the kind its name
// says.
func TestASnapshotLoadReadsEachFileAsListed(t *testing.T) {
	// Each is caught where the file is first read, before the chain is
	// checked against the files, and says so.
	says := map[string]string{"another session": "names session", "another kind": "names kind", "a link": "is a link"}
	for name, hurt := range map[string]func(*testing.T, string){
		"another session": func(t *testing.T, p string) {
			rewriteHeader(t, p, func(h *sessiondata.Header) { h.Session = "00000009-0000-4000-8000-000000000009" })
		},
		"another kind": func(t *testing.T, p string) {
			rewriteHeader(t, p, func(h *sessiondata.Header) { h.Kind = sessiondata.KindJournal })
		},
		"a link": func(t *testing.T, p string) {
			if runtime.GOOS == "windows" {
				t.Skip("links need privileges on Windows")
			}
			b, _ := os.ReadFile(p)
			elsewhere := filepath.Join(t.TempDir(), filepath.Base(p))
			_ = os.WriteFile(elsewhere, b, 0o444)
			_ = os.Remove(p)
			if err := os.Symlink(elsewhere, p); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			z, id := builtFixture(t)
			srv, snap, told := snapshotServer(t, z, id)
			hurt(t, filepath.Join(z.Root(), filepath.FromSlash(snap.Landed[1])))
			if _, err := srv.Load(id); !errors.Is(err, view.ErrEvidence) || !strings.Contains(err.Error(), says[name]) || told.count() == 0 {
				t.Fatalf("%v, told %d", err, told.count())
			}
		})
	}
}

// The workspace changes a document reads after its load are held to the
// files the load read: a changes file replaced since fails the document.
func TestASnapshotsChangesAreReadAsLoaded(t *testing.T) {
	z, id := builtFixture(t)
	srv, snap, told := snapshotServer(t, z, id)
	c, err := srv.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	var changes string
	for _, rel := range snap.Landed {
		if strings.HasPrefix(filepath.Base(rel), string(sessiondata.KindChanges)+"-") {
			changes = filepath.Join(z.Root(), filepath.FromSlash(rel))
		}
	}
	if changes == "" {
		t.Fatal("the fixture has no changes file")
	}
	b, err := os.ReadFile(changes)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(changes)
	if err := os.WriteFile(changes, b, 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Build(); !errors.Is(err, view.ErrEvidence) || told.count() == 0 {
		t.Fatalf("%v, told %d", err, told.count())
	}
}

// removedBodies is the provider-bodies scenario after retention removed its
// landed files: skeletons in their place, and the bodies later ones are
// built from kept in the session's store.
func removedBodies(t *testing.T) (*storage.Zone, string) {
	t.Helper()
	z, id := builtClaude(t, "provider-bodies")
	rem := &retention.Remover{Zone: z, MetadataOnlyAfter: time.Hour,
		Now: func() time.Time { return time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) }}
	if res := rem.Pass(retention.Input{}); res.Files == 0 || len(res.Errors) > 0 {
		t.Fatalf("retention removed %d files: %v", res.Files, res.Errors)
	}
	return z, id
}

// manifestSnapshot binds a root to its conversation's head as a pulled
// snapshot's frozen manifest lists it, store entries and all.
func manifestSnapshot(t *testing.T, z *storage.Zone, id string) *view.Snapshot {
	t.Helper()
	m, err := pulltest.Manifest(z.Root(), id)
	if err != nil {
		t.Fatal(err)
	}
	landed, store := map[uint64]string{}, map[string]string{}
	for _, e := range m.Files {
		switch e.Kind {
		case format.KindLanded, format.KindSkeleton:
			landed[e.Seq] = e.File
		case format.KindStore:
			store[e.File] = string(e.Content)
		}
	}
	if len(store) == 0 {
		t.Fatal("the snapshot lists no store entry")
	}
	return &view.Snapshot{Conversation: id, Session: m.Session, Round: m.Head.Round, Digest: string(m.Head.Digest), Landed: landed, Store: store}
}

// keptFile is one file of a files answer.
type keptFile struct {
	Seq   uint64                       `json:"seq"`
	Bytes []byte                       `json:"bytes"`
	Kept  map[string]map[string]string `json:"kept"`
}

// filesAnswer asks a page for files by sequence.
func filesAnswer(t *testing.T, url, id string, seqs ...uint64) (int, []keptFile) {
	t.Helper()
	q := make([]string, len(seqs))
	for i, seq := range seqs {
		q[i] = fmt.Sprintf("seq=%d", seq)
	}
	resp, err := http.Get(fmt.Sprintf("%s/api/c/%s/files?%s", url, id, strings.Join(q, "&")))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []keptFile
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, out
}

// skeletonsKeeping are a snapshot's provider body skeletons, in sequence
// order, split by whether the store keeps something of them.
func skeletonsKeeping(t *testing.T, url, id string, snap *view.Snapshot) (keeping, plain []keptFile) {
	t.Helper()
	var seqs []uint64
	for seq, rel := range snap.Landed {
		if strings.Contains(rel, "/provider_body/") && sessiondata.IsSkeleton(rel) {
			seqs = append(seqs, seq)
		}
	}
	slices.Sort(seqs)
	for _, seq := range seqs {
		code, files := filesAnswer(t, url, id, seq)
		if code != http.StatusOK || len(files) != 1 {
			t.Fatalf("sequence %d: %d, %d files", seq, code, len(files))
		}
		if len(files[0].Kept) > 0 {
			keeping = append(keeping, files[0])
		} else {
			plain = append(plain, files[0])
		}
	}
	if len(keeping) == 0 {
		t.Fatal("the store keeps nothing of any skeleton")
	}
	return keeping, plain
}

// A snapshot's store keeps what its manifest lists: a listed entry that went
// is damage the page is told of, never a body left out of a successful
// answer.
func TestASnapshotsListedStoreEntryThatWentIsDamage(t *testing.T) {
	z, id := removedBodies(t)
	snap := manifestSnapshot(t, z, id)
	srv, _, told := snapshotServerOf(t, z, snap)
	page := httptest.NewServer(srv.Handler())
	defer page.Close()
	keeping, _ := skeletonsKeeping(t, page.URL, id, snap)
	if told.count() != 0 {
		t.Fatalf("a whole store: told %d", told.count())
	}
	for rel := range snap.Store {
		if err := os.Remove(filepath.Join(z.Root(), filepath.FromSlash(rel))); err != nil {
			t.Fatal(err)
		}
	}
	if code, _ := filesAnswer(t, page.URL, id, keeping[0].Seq); code != http.StatusInternalServerError || told.count() == 0 {
		t.Fatalf("a store entry that went: %d, told %d", code, told.count())
	}
}

// What the store keeps of a snapshot's skeleton counts toward the files
// budget before it is read, as on any other root.
func TestASnapshotsKeptBytesCountTowardTheBudget(t *testing.T) {
	z, id := removedBodies(t)
	snap := manifestSnapshot(t, z, id)
	srv, _, _ := snapshotServerOf(t, z, snap)
	page := httptest.NewServer(srv.Handler())
	defer page.Close()
	keeping, plain := skeletonsKeeping(t, page.URL, id, snap)
	second := append(keeping[1:], plain...)
	if len(second) == 0 {
		t.Fatal("the snapshot has one provider body skeleton")
	}
	a, b := keeping[0], second[0]
	tight, _, _ := snapshotServerOf(t, z, snap)
	// Both files fit when what is kept is not counted.
	tight.MaxFileBytes = int64(len(a.Bytes) + len(b.Bytes))
	small := httptest.NewServer(tight.Handler())
	defer small.Close()
	code, files := filesAnswer(t, small.URL, id, a.Seq, b.Seq)
	if code != http.StatusOK || len(files) != 1 || files[0].Seq != a.Seq {
		t.Fatalf("%d, %d files", code, len(files))
	}
}

// The glossary reads a snapshot's dialect from its files as their load read
// them: a file replaced since, by a valid copy, fails the request as damage
// rather than answering without the dialect.
func TestASnapshotsDialectIsReadAsLoaded(t *testing.T) {
	z, id := builtFixture(t)
	srv, snap, told := snapshotServer(t, z, id)
	page := httptest.NewServer(srv.Handler())
	defer page.Close()
	get := func() int {
		resp, err := http.Get(page.URL + "/api/glossary")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get(); code == http.StatusInternalServerError || told.count() != 0 {
		t.Fatalf("a whole snapshot: %d, told %d", code, told.count())
	}
	p := filepath.Join(z.Root(), filepath.FromSlash(snap.Landed[1]))
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o444); err != nil {
		t.Fatal(err)
	}
	if code := get(); code != http.StatusInternalServerError || told.count() == 0 {
		t.Fatalf("a replaced file: %d, told %d", code, told.count())
	}
}
