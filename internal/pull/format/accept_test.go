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

package format_test

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/pull/format"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/pulltest"
	"github.com/apache/skywalking-ai-sessionizer/internal/retention"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario/run"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/skeleton"
)

// built holds each scenario's root, built once for the package's tests and
// copied for each, since a build takes seconds and the tests change their
// copies.
var (
	builtMu   sync.Mutex
	builtDir  string
	builtRoot = map[string][2]string{}
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "pull-format-")
	if err != nil {
		panic(err)
	}
	builtDir = dir
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// source gives a test its own copy of a scenario's root, the way a
// production root holds it.
func source(t *testing.T, name string) (root, conversation string) {
	t.Helper()
	builtMu.Lock()
	b, ok := builtRoot[name]
	if !ok {
		out := filepath.Join(builtDir, name)
		rep, err := run.Check(filepath.Join("..", "..", "..", "tests", "scenarios", name+".yaml"),
			run.Options{Out: out, Formats: []scenario.Format{scenario.FormatClaudeCode}})
		if err != nil || rep.Failed {
			builtMu.Unlock()
			t.Fatalf("building %s: %v %v", name, err, rep.Lines)
		}
		built := filepath.Join(out, string(scenario.FormatClaudeCode))
		entries, err := os.ReadDir(filepath.Join(built, "_conversations"))
		if err != nil || len(entries) == 0 {
			builtMu.Unlock()
			t.Fatalf("%s: no conversation: %v", name, err)
		}
		b = [2]string{built, entries[0].Name()}
		builtRoot[name] = b
	}
	builtMu.Unlock()
	root = filepath.Join(t.TempDir(), "root")
	if err := copyRoot(b[0], root); err != nil {
		t.Fatal(err)
	}
	return root, b[1]
}

// copyRoot copies a storage root, each file with its mode.
func copyRoot(from, to string) error {
	return filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, path)
		dest := filepath.Join(to, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, data, info.Mode().Perm())
	})
}

// removed makes every landed file of root a skeleton, as metadata-only
// retention does, which also keeps the provider store.
func removed(t *testing.T, root string) {
	t.Helper()
	rem := &retention.Remover{Zone: storage.NewZone(root), MetadataOnlyAfter: time.Hour,
		Now: func() time.Time { return time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) }}
	if res := rem.Pass(retention.Input{}); res.Files == 0 || len(res.Errors) > 0 {
		t.Fatalf("retention removed %d files: %v", res.Files, res.Errors)
	}
}

// fixture is a tree holding exactly the manifest of a scenario's
// conversation.
type fixture struct {
	root, conversation, tree string
	m                        *format.Manifest
}

func newFixture(t *testing.T, name string, skeletons bool) *fixture {
	t.Helper()
	root, conversation := source(t, name)
	if skeletons {
		removed(t, root)
	}
	m, err := pulltest.Manifest(root, conversation)
	if err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(t.TempDir(), "tree")
	if err := pulltest.Tree(root, m, tree); err != nil {
		t.Fatal(err)
	}
	return &fixture{root: root, conversation: conversation, tree: tree, m: m}
}

func (f *fixture) accept(t *testing.T, layout format.Layout) (*format.Accepted, error) {
	t.Helper()
	return format.Accept(f.tree, layout, format.Want{Conversation: f.conversation})
}

func (f *fixture) first(t *testing.T, match func(format.Entry) bool) format.Entry {
	t.Helper()
	i := slices.IndexFunc(f.m.Files, match)
	if i < 0 {
		t.Fatal("the fixture has no such entry")
	}
	return f.m.Files[i]
}

func refused(t *testing.T, err error, want string) {
	t.Helper()
	if !errors.Is(err, format.ErrEvidence) && !errors.Is(err, format.ErrManifest) || !strings.Contains(err.Error(), want) {
		t.Fatalf("%v, want a refusal saying %q", err, want)
	}
}

func TestAcceptTakesWholeEvidence(t *testing.T) {
	for _, tc := range []struct {
		scenario  string
		skeletons bool
	}{{"three-rounds", false}, {"provider-bodies", false}, {"workflow", false}, {"provider-bodies", true}, {"three-rounds", true}} {
		t.Run(tc.scenario, func(t *testing.T) {
			f := newFixture(t, tc.scenario, tc.skeletons)
			kinds := map[format.Kind]int{}
			for _, e := range f.m.Files {
				kinds[e.Kind]++
			}
			if tc.skeletons && (kinds[format.KindSkeleton] == 0 || kinds[format.KindLanded] != 0) {
				t.Fatalf("the fixture is not all skeletons: %v", kinds)
			}
			if tc.skeletons && tc.scenario == "provider-bodies" && kinds[format.KindStore] == 0 {
				t.Fatalf("the fixture keeps no store: %v", kinds)
			}
			a, err := f.accept(t, format.Building)
			if err != nil {
				t.Fatal(err)
			}
			if a.Digest != f.m.Digest() || uint64(len(a.Originals)) != f.m.Head.ThroughSeq || len(a.Stored) != len(f.m.Files) {
				t.Fatalf("accepted %s with %d originals and %d stored digests", a.Digest, len(a.Originals), len(a.Stored))
			}
			// A skeleton stands for the landed file it replaced.
			for _, e := range f.m.Files {
				if e.Kind == format.KindSkeleton && a.Originals[e.Seq] != e.LandedDigest {
					t.Fatalf("%s: original %s, want %s", e.File, a.Originals[e.Seq], e.LandedDigest)
				}
			}
		})
	}
}

func TestAcceptBindsWhatAMarkerSaved(t *testing.T) {
	f := newFixture(t, "three-rounds", false)
	for name, want := range map[string]format.Want{
		"digest":  {Conversation: f.conversation, ManifestDigest: format.ManifestDigest(strings.Repeat("0", 64))},
		"session": {Conversation: f.conversation, Session: "other"},
		"head":    {Conversation: f.conversation, Round: f.m.Head.Round, Digest: format.CommitDigest(strings.Repeat("0", 64))},
		"round":   {Conversation: f.conversation, Round: f.m.Head.Round + 1, Digest: f.m.Head.Digest},
	} {
		if _, err := format.Accept(f.tree, format.Building, want); !errors.Is(err, format.ErrEvidence) {
			t.Errorf("%s: %v, want refused", name, err)
		}
	}
	good := format.Want{Conversation: f.conversation, ManifestDigest: f.m.Digest(), Session: f.m.Session, Round: f.m.Head.Round, Digest: f.m.Head.Digest}
	if _, err := format.Accept(f.tree, format.Building, good); err != nil {
		t.Fatal(err)
	}
	if _, err := format.Accept(f.tree, format.Building, format.Want{Conversation: "other"}); !errors.Is(err, format.ErrManifest) {
		t.Fatalf("another conversation: %v", err)
	}
}

// The tree holds the listed files and its layout's controls, and nothing
// else: an unlisted file or directory, a missing or resized file, or a link
// anywhere acceptance looks is refused.
func TestAcceptRefusesAnyOtherInventory(t *testing.T) {
	session := func(f *fixture) string { return filepath.Join(f.tree, f.m.Session) }
	landed := func(f *fixture) format.Entry {
		return f.m.Files[slices.IndexFunc(f.m.Files, func(e format.Entry) bool { return e.Kind == format.KindLanded })]
	}
	for name, tc := range map[string]struct {
		change func(t *testing.T, f *fixture)
		want   string
	}{
		"an unlisted file": {func(t *testing.T, f *fixture) {
			write(t, filepath.Join(session(f), "index", "seen.state"), "x")
		}, "no listed file is in"},
		"an unlisted file beside listed ones": {func(t *testing.T, f *fixture) {
			write(t, filepath.Join(f.tree, filepath.FromSlash(pathDir(landed(f).File)), "extra.sd"), "x")
		}, "is not listed in the manifest"},
		"a missing file": {func(t *testing.T, f *fixture) {
			mustRemove(t, filepath.Join(f.tree, filepath.FromSlash(landed(f).File)))
		}, "is missing"},
		"a resized file": {func(t *testing.T, f *fixture) {
			path := filepath.Join(f.tree, filepath.FromSlash(landed(f).File))
			data := read(t, path)
			mustRemove(t, path)
			write(t, path, string(data)+"\n")
		}, "holds"},
		"a link in a file's place": {func(t *testing.T, f *fixture) {
			path := filepath.Join(f.tree, filepath.FromSlash(landed(f).File))
			copied := filepath.Join(t.TempDir(), "copy")
			write(t, copied, string(read(t, path)))
			mustRemove(t, path)
			link(t, copied, path)
		}, "is a link"},
		"a link in a directory's place": {func(t *testing.T, f *fixture) {
			dir := filepath.Join(f.tree, filepath.FromSlash(pathDir(landed(f).File)))
			moved := filepath.Join(t.TempDir(), "moved")
			if err := os.Rename(dir, moved); err != nil {
				t.Fatal(err)
			}
			link(t, moved, dir)
		}, "is a link"},
		"judge runs in a build": {func(t *testing.T, f *fixture) {
			write(t, filepath.Join(session(f), storage.JudgmentsDir, "j000001", "work", "x"), "x")
		}, "no listed file is in"},
		"a temporary directory": {func(t *testing.T, f *fixture) {
			if err := os.Mkdir(filepath.Join(f.tree, ".tmp-x"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "is not a regular file"},
		"a lease directory with an unknown file": {func(t *testing.T, f *fixture) {
			write(t, filepath.Join(f.tree, storage.LeasesDir, "x"), "")
		}, "is not one of its empty lock files"},
		"a lock with content": {func(t *testing.T, f *fixture) {
			write(t, filepath.Join(f.tree, storage.RootLockDir, ".lock"), "x")
		}, "is not one of its empty lock files"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, "three-rounds", false)
			tc.change(t, f)
			_, err := f.accept(t, format.Building)
			refused(t, err, tc.want)
		})
	}
}

// A build's _work/ is checked and never entered, and a published snapshot
// carries its controls and may hold judge runs, whose work is never read.
func TestAcceptAllowsEachLayoutsOwnFiles(t *testing.T) {
	f := newFixture(t, "three-rounds", false)
	write(t, filepath.Join(f.tree, "_work", "archive.part"), "anything")
	if err := os.Symlink("/", filepath.Join(f.tree, "_work", "link")); err != nil {
		t.Skip(err)
	}
	write(t, filepath.Join(f.tree, ".tmp-123"), "a control replacement left over")
	if _, err := f.accept(t, format.Building); err != nil {
		t.Fatalf("a build's own work: %v", err)
	}
	if _, err := f.accept(t, format.Published); err == nil {
		t.Fatal("a published snapshot holding _work/ was accepted")
	}
	mustRemoveAll(t, filepath.Join(f.tree, "_work"))
	_, err := f.accept(t, format.Published)
	refused(t, err, "a published snapshot needs its _copy")
	const tag = "20261025T103200.000000000Z"
	published := format.Want{Conversation: f.conversation, Remote: "prod", Tag: tag}
	at, _ := storage.ParseTag(tag)
	marker := storage.SnapshotCopy{Remote: "prod", Conversation: f.conversation, Session: f.m.Session, Round: f.m.Head.Round,
		Digest: string(f.m.Head.Digest), ManifestDigest: string(f.m.Digest()), PublishedAt: at}
	if err := storage.WriteSnapshotCopy(f.tree, tag, marker, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.tree, storage.RootLockDir), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(f.tree, storage.LeasesDir, "read-"+strings.Repeat("a", 64)+".lock"), "")
	_, err = format.Accept(f.tree, format.Published, published)
	refused(t, err, "_root/.lock")
	write(t, filepath.Join(f.tree, storage.RootLockDir, ".lock"), "")
	run := filepath.Join(f.tree, f.m.Session, storage.JudgmentsDir, "j000001")
	write(t, filepath.Join(run, "s1-targets-0123456789ab.sj"), "{}")
	write(t, filepath.Join(run, "g1-regroup-0123456789ab.sj"), "{}")
	write(t, filepath.Join(run, ".tmp-4294967295"), "")
	write(t, filepath.Join(run, storage.WorkDir, "decoy-000001.sd"), "not evidence")
	if _, err := format.Accept(f.tree, format.Published, published); err != nil {
		t.Fatalf("a published snapshot with a judge run: %v", err)
	}
	// The marker is read and bound to the manifest: one that is not there
	// for a name, names another tag, or another manifest, session or head,
	// is refused.
	if _, err := f.accept(t, format.Published); err == nil || !strings.Contains(err.Error(), "nothing names the snapshot") {
		t.Fatalf("a marker with no snapshot named: %v", err)
	}
	if _, err := format.Accept(f.tree, format.Published, format.Want{Conversation: f.conversation, Remote: "prod", Tag: "20261025T103201.000000000Z"}); err == nil {
		t.Fatal("a marker of another tag was accepted")
	}
	for name, change := range map[string]func(*storage.SnapshotCopy){
		"manifest": func(c *storage.SnapshotCopy) { c.ManifestDigest = strings.Repeat("0", 64) },
		"session":  func(c *storage.SnapshotCopy) { c.Session = "other" },
		"round":    func(c *storage.SnapshotCopy) { c.Round++ },
		"digest":   func(c *storage.SnapshotCopy) { c.Digest = strings.Repeat("0", 64) },
	} {
		other := marker
		change(&other)
		if err := storage.WriteSnapshotCopy(f.tree, tag, other, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := format.Accept(f.tree, format.Published, published); !errors.Is(err, format.ErrEvidence) {
			t.Errorf("a marker naming another %s: %v", name, err)
		}
	}
	mustRemove(t, filepath.Join(f.tree, storage.CopyMarker))
	write(t, filepath.Join(f.tree, storage.CopyMarker), "{}")
	if _, err := format.Accept(f.tree, format.Published, published); !errors.Is(err, format.ErrEvidence) {
		t.Fatalf("an empty marker: %v", err)
	}
	if err := os.Remove(filepath.Join(f.tree, storage.CopyMarker)); err != nil {
		t.Fatal(err)
	}
	if err := storage.WriteSnapshotCopy(f.tree, tag, marker, nil); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"a stage temporary past 32 bits": filepath.Join(run, ".tmp-4294967296"),
		"a stage temporary with a zero":  filepath.Join(run, ".tmp-01"),
		"an unknown run file":            filepath.Join(run, "notes.txt"),
		"a run directory not canonical":  filepath.Join(f.tree, f.m.Session, storage.JudgmentsDir, "j1", "x"),
	} {
		write(t, path, "")
		if _, err := format.Accept(f.tree, format.Published, published); !errors.Is(err, format.ErrEvidence) || !strings.Contains(err.Error(), "judgments/") {
			t.Errorf("%s: %v, want refused", name, err)
		}
		mustRemove(t, path)
		_ = os.Remove(filepath.Dir(path))
	}
}

// Every Session Data file is read to its end and bound to where it sits.
// Each damage below keeps every stored digest in agreement, so only the read
// can find it.
func TestAcceptReadsSessionDataWhole(t *testing.T) {
	isLanded := func(e format.Entry) bool { return e.Kind == format.KindLanded }
	isSkeleton := func(e format.Entry) bool { return e.Kind == format.KindSkeleton }
	for name, tc := range map[string]struct {
		skeletons bool
		match     func(format.Entry) bool
		damage    func(t *testing.T, data []byte) []byte
		want      string
	}{
		"a record after the closing line": {false, isLanded, func(_ *testing.T, data []byte) []byte {
			lines := bytes.SplitAfter(data, []byte{'\n'})
			return append(data, lines[1]...)
		}, "after its closing line"},
		"a changed record": {false, isLanded, func(_ *testing.T, data []byte) []byte {
			return bytes.Replace(data, []byte(`"ord":`), []byte(`"ord" :`), 1)
		}, "digest mismatch"},
		"no closing line": {false, isLanded, func(_ *testing.T, data []byte) []byte {
			lines := bytes.SplitAfter(data, []byte{'\n'})
			return bytes.Join(lines[:len(lines)-2], nil)
		}, "no closing line"},
		"another session": {false, isLanded, func(t *testing.T, data []byte) []byte {
			return rewritten(t, data, func(h *sessiondata.Header) { h.Session = "other" })
		}, "session other"},
		"another sequence": {false, isLanded, func(t *testing.T, data []byte) []byte {
			return rewritten(t, data, func(h *sessiondata.Header) { h.Seq += 100 })
		}, "sequence"},
		"another time": {false, isLanded, func(t *testing.T, data []byte) []byte {
			return rewritten(t, data, func(h *sessiondata.Header) { h.At = "2020-01-01T00:00:00Z" })
		}, "stamped"},
		"another stream": {false, func(e format.Entry) bool { return isLanded(e) && strings.Contains(e.File, "/streams/") },
			func(t *testing.T, data []byte) []byte {
				return rewritten(t, data, func(h *sessiondata.Header) { h.Stream = "other" })
			}, `stream "other"`},
		"a landed file that says it was removed": {false, isLanded, func(t *testing.T, data []byte) []byte {
			return rewritten(t, data, func(h *sessiondata.Header) { h.Removed = &sessiondata.Removed{Digest: strings.Repeat("0", 64)} })
		}, "says it was removed"},
		"a skeleton's damaged trailer": {true, isSkeleton, func(_ *testing.T, data []byte) []byte {
			data[len(data)-5] ^= 0xff
			return data
		}, "checksum"},
		"a second gzip member": {true, isSkeleton, func(t *testing.T, data []byte) []byte {
			var extra bytes.Buffer
			zw := gzip.NewWriter(&extra)
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
			return append(data, extra.Bytes()...)
		}, "bytes follow its gzip member"},
		"a skeleton's record after its closing line": {true, isSkeleton, func(t *testing.T, data []byte) []byte {
			plain := gunzipped(t, data)
			lines := bytes.SplitAfter(plain, []byte{'\n'})
			return gzipped(t, append(plain, lines[0]...))
		}, "after its closing line"},
		"a skeleton keeping another digest": {true, isSkeleton, func(t *testing.T, data []byte) []byte {
			return gzipped(t, rewritten(t, gunzipped(t, data), func(h *sessiondata.Header) { h.Removed.Digest = strings.Repeat("0", 64) }))
		}, "must keep the landed digest"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, "three-rounds", tc.skeletons)
			e := f.first(t, tc.match)
			path := filepath.Join(f.tree, filepath.FromSlash(e.File))
			if err := pulltest.Restamp(f.tree, f.m, e.File, tc.damage(t, read(t, path))); err != nil {
				t.Fatal(err)
			}
			_, err := f.accept(t, format.Building)
			refused(t, err, e.File)
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%v, want %q", err, tc.want)
			}
		})
	}
}

// A landed file that reads whole but is not the one its round bound, by
// its digest, is refused by the chain's verification.
func TestAcceptBindsLandedFilesToTheChain(t *testing.T) {
	f := newFixture(t, "three-rounds", false)
	e := f.first(t, func(e format.Entry) bool { return e.Kind == format.KindLanded })
	path := filepath.Join(f.tree, filepath.FromSlash(e.File))
	hdr, recs, err := sessiondata.All(bytes.NewReader(read(t, path)))
	if err != nil {
		t.Fatal(err)
	}
	recs[0].Ord++
	var out bytes.Buffer
	w, err := sessiondata.NewWriter(&out, &hdr)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		if err := w.Write(rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pulltest.Restamp(f.tree, f.m, e.File, out.Bytes()); err != nil {
		t.Fatal(err)
	}
	_, err = f.accept(t, format.Building)
	refused(t, err, "input digest does not match")
}

// rewritten writes a Session Data file again with its records unchanged and
// its header edited, so the file reads whole and its header is what differs.
func rewritten(t *testing.T, data []byte, edit func(*sessiondata.Header)) []byte {
	t.Helper()
	hdr, recs, err := sessiondata.All(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	edit(&hdr)
	var out bytes.Buffer
	w, err := sessiondata.NewWriter(&out, &hdr)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		if err := w.Write(rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// rechained writes every round of the fixture again, edit applied to each
// header in order, each naming the round before by its new digest and named
// for its own, and lists them in the manifest. The chain stays a chain: only
// what edit changed differs.
func rechained(t *testing.T, f *fixture, edit func(h *sessionflow.Header)) {
	t.Helper()
	var rounds []format.Entry
	for _, e := range f.m.Files {
		if e.Kind == format.KindRound {
			rounds = append(rounds, e)
		}
	}
	f.m.Files = slices.DeleteFunc(f.m.Files, func(e format.Entry) bool { return e.Kind == format.KindRound })
	// Each round's input digest is computed again from the manifest's
	// originals, so a test that changed a landed file keeps a chain that
	// binds it.
	originals := map[uint64]string{}
	for _, e := range f.m.Files {
		if o := e.Original(); o != "" {
			originals[e.Seq] = string(o)
		}
	}
	previous, input := "", ""
	for _, e := range rounds {
		path := filepath.Join(f.tree, filepath.FromSlash(e.File))
		r, err := sessionflow.Read(bytes.NewReader(read(t, path)))
		if err != nil {
			t.Fatal(err)
		}
		mustRemove(t, path)
		h := r.Header
		if previous != "" {
			h.Previous = previous
		}
		var added []string
		for seq := h.FromSeq; seq <= h.ThroughSeq && seq != 0; seq++ {
			added = append(added, originals[seq])
		}
		h.InputDigest = sessionflow.ChainInputDigest(input, added)
		input = h.InputDigest
		edit(&h)
		w, err := sessionflow.NewWriter(h)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range r.Nodes {
			if err := w.Node(n); err != nil {
				t.Fatal(err)
			}
		}
		for _, rel := range r.Relations {
			if err := w.Relation(rel); err != nil {
				t.Fatal(err)
			}
		}
		for _, u := range r.Unresolved {
			if err := w.Unresolved(u); err != nil {
				t.Fatal(err)
			}
		}
		data, digest, err := w.Close()
		if err != nil {
			t.Fatal(err)
		}
		name := fmt.Sprintf("_conversations/%s/rounds/r%06d-%s.sf", f.conversation, h.Round, digest[:12])
		write(t, filepath.Join(f.tree, filepath.FromSlash(name)), string(data))
		sum := sha256.Sum256(data)
		f.m.Files = append(f.m.Files, format.Entry{File: name, Kind: format.KindRound, Round: h.Round,
			Bytes: uint64(len(data)), Digest: format.StoredDigest(hex.EncodeToString(sum[:]))})
		previous = digest
		f.m.Head.Digest = format.CommitDigest(digest)
	}
	slices.SortFunc(f.m.Files, func(x, y format.Entry) int { return strings.Compare(x.File, y.File) })
	if err := pulltest.SaveManifest(f.tree, f.m); err != nil {
		t.Fatal(err)
	}
}

// A round's range is checked against the inventory before anything walks
// it. Here round 1 claims every sequence there is while the rounds after it
// keep their own, so the chain links, folds and ends at the head's sequence,
// and a walk of round 1's range would not end. Acceptance refuses it at once.
func TestAcceptBoundsRoundsBeforeTheChain(t *testing.T) {
	f := newFixture(t, "three-rounds", false)
	if f.m.Head.Round < 2 {
		t.Fatal("the fixture needs two rounds")
	}
	rechained(t, f, func(h *sessionflow.Header) {
		if h.Round == 1 {
			h.ThroughSeq = ^uint64(0)
		}
	})
	done := make(chan error, 1)
	go func() {
		_, err := f.accept(t, format.Building)
		done <- err
	}()
	select {
	case err := <-done:
		refused(t, err, "outside 0 to")
	case <-time.After(10 * time.Second):
		t.Fatal("acceptance walked an unbounded range")
	}
	// Rounds written again without a change still accept: the helper keeps
	// the chain a chain.
	g := newFixture(t, "three-rounds", false)
	rechained(t, g, func(*sessionflow.Header) {})
	if _, err := g.accept(t, format.Building); err != nil {
		t.Fatal(err)
	}
	// A round starts just after the one before ended. A backward range is
	// refused by the round's own header check, so none can be written.
	for name, edit := range map[string]func(h *sessionflow.Header){
		"a gap":      func(h *sessionflow.Header) { h.FromSeq++ },
		"an overlap": func(h *sessionflow.Header) { h.FromSeq-- },
	} {
		h := newFixture(t, "three-rounds", false)
		rechained(t, h, func(hdr *sessionflow.Header) {
			if hdr.Round == 2 {
				edit(hdr)
			}
		})
		if _, err := h.accept(t, format.Building); !errors.Is(err, format.ErrEvidence) {
			t.Errorf("%s: %v, want refused", name, err)
		}
	}
}

// withoutBodies replaces the provider file at index gone of the fixture's
// provider files by its skeleton, as a removal does. When kept, the store
// keeps every body of that file, as a removal keeps the ones later bodies
// are built from.
func withoutBodies(t *testing.T, gone int, kept bool) *fixture {
	t.Helper()
	f := newFixture(t, "provider-bodies", false)
	var bodies []format.Entry
	for _, e := range f.m.Files {
		if e.Kind == format.KindLanded && strings.Contains(e.File, "/provider_body/") {
			bodies = append(bodies, e)
		}
	}
	slices.SortFunc(bodies, func(x, y format.Entry) int { return cmpSeq(x.Seq, y.Seq) })
	if gone >= len(bodies) {
		return nil
	}
	removed := bodies[gone]
	path := filepath.Join(f.tree, filepath.FromSlash(removed.File))
	data := read(t, path)
	if kept {
		s := providerbody.NewSession()
		for _, e := range bodies[:gone+1] {
			_, recs, err := sessiondata.All(bytes.NewReader(read(t, filepath.Join(f.tree, filepath.FromSlash(e.File)))))
			if err != nil {
				t.Fatal(err)
			}
			for _, rec := range recs {
				if err := s.Add(rec); err != nil && !errors.Is(err, providerbody.ErrRepeat) {
					t.Fatal(err)
				}
				if e != removed {
					continue
				}
				store := storage.NewZone(f.tree).BodyStore(f.m.Session)
				for _, part := range rec.Parts[:len(rec.Parts)-1] {
					if part.Kind == sessiondata.PartData && len(part.Data) > 0 {
						if err := store.PutPiece(providerbody.Digest(part.Data), part.Data, nil); err != nil {
							t.Fatal(err)
						}
					}
				}
				b, err := s.Body(rec.ID)
				if errors.Is(err, providerbody.ErrTooLarge) {
					// A removal cannot keep a body it could never rebuild.
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := store.PutTip(1, rec.ID, b, nil); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	var sk bytes.Buffer
	if err := skeleton.Write(&sk, data, sessiondata.Removed{At: "2030-01-01T00:00:00Z", Generation: 1, Receiver: sessiondata.ReceiverUnconfirmed}); err != nil {
		t.Fatal(err)
	}
	mustRemove(t, path)
	skPath := strings.TrimSuffix(removed.File, sessiondata.Ext) + sessiondata.SkeletonExt
	write(t, filepath.Join(f.tree, filepath.FromSlash(skPath)), sk.String())
	i := slices.IndexFunc(f.m.Files, func(e format.Entry) bool { return e.File == removed.File })
	sum := sha256.Sum256(sk.Bytes())
	f.m.Files[i] = format.Entry{File: skPath, Kind: format.KindSkeleton, Seq: removed.Seq, Bytes: uint64(sk.Len()),
		Digest: format.StoredDigest(hex.EncodeToString(sum[:])), LandedDigest: removed.Original()}
	store := filepath.Join(f.tree, f.m.Session, "provider")
	err := filepath.WalkDir(store, func(p string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return filepath.SkipDir
		}
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(f.tree, p)
		content, err := pulltest.Content(p)
		if err != nil {
			return err
		}
		info, err := os.Stat(p)
		if err != nil {
			return err
		}
		stored, err := storage.FileDigest(p)
		if err != nil {
			return err
		}
		f.m.Files = append(f.m.Files, format.Entry{File: filepath.ToSlash(rel), Kind: format.KindStore,
			Bytes: uint64(info.Size()), Digest: format.StoredDigest(stored), Content: format.ContentDigest(content)})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(f.m.Files, func(x, y format.Entry) int { return strings.Compare(x.File, y.File) })
	if err := pulltest.SaveManifest(f.tree, f.m); err != nil {
		t.Fatal(err)
	}
	return f
}

func cmpSeq(x, y uint64) int {
	switch {
	case x < y:
		return -1
	case x > y:
		return 1
	}
	return 0
}

// A provider body is rebuilt from its own representation. A provider file
// whose bodies later ones are built on is replaced by its skeleton: without
// the store keeping those bodies the later ones cannot be rebuilt, and
// acceptance refuses, though every file is listed and whole. With the store
// keeping them, it accepts.
func TestAcceptRebuildsProviderBodies(t *testing.T) {
	for gone := 0; ; gone++ {
		f := withoutBodies(t, gone, false)
		if f == nil {
			t.Fatal("no provider file of the fixture has bodies built on it")
		}
		_, err := f.accept(t, format.Building)
		if err == nil {
			continue
		}
		if !errors.Is(err, format.ErrEvidence) || !errors.Is(err, providerbody.ErrRemoved) || !strings.Contains(err.Error(), "row ") {
			t.Fatalf("without the store: %v, want the body that cannot be rebuilt, by file and row", err)
		}
		if _, err := withoutBodies(t, gone, true).accept(t, format.Building); err != nil {
			t.Fatalf("with the store: %v", err)
		}
		return
	}
}

// A tip is bound to the body of the record it is named for: one holding
// other bytes, whole and listed, is refused.
func TestAcceptBindsATipToItsRecord(t *testing.T) {
	g := newFixture(t, "provider-bodies", true)
	tip := slices.IndexFunc(g.m.Files, func(e format.Entry) bool { return e.Kind == format.KindStore && strings.Contains(e.File, "/tips/") })
	if tip < 0 {
		t.Fatal("the fixture keeps no tip")
	}
	other := gzipped(t, []byte(`{"other":"body"}`))
	sum := sha256.Sum256([]byte(`{"other":"body"}`))
	g.m.Files[tip].Content = format.ContentDigest(hex.EncodeToString(sum[:]))
	if err := pulltest.Restamp(g.tree, g.m, g.m.Files[tip].File, other); err != nil {
		t.Fatal(err)
	}
	_, err := g.accept(t, format.Building)
	refused(t, err, "the body of its record")
}

func pathDir(p string) string { return p[:strings.LastIndexByte(p, '/')] }

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mustRemove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

func mustRemoveAll(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
}

func link(t *testing.T, target, path string) {
	t.Helper()
	if err := os.Symlink(target, path); err != nil {
		t.Skip(err)
	}
}

func gzipped(t *testing.T, data []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func gunzipped(t *testing.T, data []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
