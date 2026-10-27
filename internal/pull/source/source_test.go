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

package source

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/format"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/pulltest"
	"github.com/apache/skywalking-ai-sessionizer/internal/retention"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario/run"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

var base = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

// build lands a scenario into root and parses it, in rounds of at most
// budget bytes, and returns its conversation.
func build(t *testing.T, root, text string, at time.Time, budget int64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scenario.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	sc, err := scenario.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := scenario.Build(sc, scenario.FormatSD, root, scenario.Options{At: at})
	if err != nil {
		t.Fatal(err)
	}
	z := storage.NewZone(root)
	for {
		r, err := parse.Session(z, parse.Options{Conversation: b.Session, Session: b.Session, MaxRoundBytes: budget})
		if err != nil {
			t.Fatal(err)
		}
		if !r.Changed() || !r.More {
			break
		}
	}
	return b.Session
}

func session(n int, title, steps string) string {
	return fmt.Sprintf("session: %08x-0000-4000-8000-%012x\ntitle: %s\nsteps:\n%s", n, n, title, steps)
}

const talk = "  - input: build the parser\n  - call: {text: The parser is built.}\n"

// freezeInto freezes conversation into a new private build.
func freezeInto(t *testing.T, root, conversation string, files storage.FileSystem) (string, *format.Manifest, error) {
	t.Helper()
	build := filepath.Join(t.TempDir(), "b")
	if err := os.Mkdir(build, 0o755); err != nil {
		t.Fatal(err)
	}
	m, err := Freeze(context.Background(), root, conversation, build, files)
	return filepath.Join(build, "tree"), m, err
}

// A freeze lists what a pull needs of the head, saves the manifest first,
// and holds the same bytes the live root does, linked where it can be: the
// tree is accepted as whole evidence.
func TestAFreezeHoldsTheHeadWhole(t *testing.T) {
	root := t.TempDir()
	growth, err := os.ReadFile(filepath.Join("..", "..", "..", "tests", "scenarios", "growth.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	id := build(t, root, string(growth), base, 4<<10)
	tree, m, err := freezeInto(t, root, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	want, err := pulltest.Manifest(root, id)
	if err != nil {
		t.Fatal(err)
	}
	if m.Digest() != want.Digest() || m.Head != want.Head || m.Session != want.Session {
		t.Fatalf("the freeze listed %s at %+v, the root holds %s at %+v", m.Digest(), m.Head, want.Digest(), want.Head)
	}
	if m.Head.Round < 2 {
		t.Fatalf("a chain of %d rounds", m.Head.Round)
	}
	if _, err := format.Accept(tree, format.Building, format.Want{Conversation: id}); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		for _, e := range m.Files {
			frozen, _ := os.Stat(filepath.Join(tree, filepath.FromSlash(e.File)))
			live, _ := os.Stat(filepath.Join(root, filepath.FromSlash(e.File)))
			if !os.SameFile(frozen, live) {
				t.Fatalf("%s was copied, not linked", e.File)
			}
		}
	}
}

// A freeze of the provider store lists every published entry and passes
// over a writer's regular temporaries unread; anything else there that is
// not an entry is invalid.
func TestAFreezeListsTheStoreAsItIs(t *testing.T) {
	out := t.TempDir()
	rep, err := run.Check(filepath.Join("..", "..", "..", "tests", "scenarios", "provider-bodies.yaml"),
		run.Options{Out: out, Formats: []scenario.Format{scenario.FormatClaudeCode}})
	if err != nil || rep.Failed {
		t.Fatalf("building: %v %v", err, rep.Lines)
	}
	root := filepath.Join(out, string(scenario.FormatClaudeCode))
	conversations, err := os.ReadDir(filepath.Join(root, "_conversations"))
	if err != nil || len(conversations) != 1 {
		t.Fatalf("conversations: %v", err)
	}
	id := conversations[0].Name()
	// Removing the landed files puts their bodies in the store, and
	// skeletons in their place.
	rem := &retention.Remover{Zone: storage.NewZone(root), MetadataOnlyAfter: time.Hour,
		Now: func() time.Time { return time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) }}
	if res := rem.Pass(retention.Input{}); res.Files == 0 || len(res.Errors) > 0 {
		t.Fatalf("retention removed %d files: %v", res.Files, res.Errors)
	}
	want, err := pulltest.Manifest(root, id)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[format.Kind]int{}
	for _, e := range want.Files {
		kinds[e.Kind]++
	}
	if kinds[format.KindStore] == 0 || kinds[format.KindSkeleton] == 0 {
		t.Fatalf("the scenario holds %v", kinds)
	}
	// The directory of a store entry the root holds.
	var pieces string
	for _, e := range want.Files {
		if e.Kind == format.KindStore {
			pieces = filepath.Dir(filepath.Join(root, filepath.FromSlash(e.File)))
			break
		}
	}
	if err := os.WriteFile(filepath.Join(pieces, ".tmp-123"), []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	tree, m, err := freezeInto(t, root, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Digest() != want.Digest() {
		t.Fatal("a temporary changed the listing")
	}
	// Only entries count against the bound: a store of n entries is
	// listed under a bound of n however many temporaries it holds, and a
	// freeze lists it again the same.
	stored := 0
	for _, e := range m.Files {
		if e.Kind == format.KindStore {
			stored++
		}
	}
	for i := range 1100 {
		if err := os.WriteFile(filepath.Join(pieces, fmt.Sprintf(".tmp-many-%d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store := filepath.Join(storage.NewZone(root).SessionDir(m.Session), "provider")
	if entries, err := storeEntries(store, stored); err != nil || len(entries) != stored {
		t.Fatalf("%d entries under a bound of %d: %v", len(entries), stored, err)
	}
	if _, err := storeEntries(store, stored-1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("%d entries under a bound of %d: %v", stored, stored-1, err)
	}
	if _, again, err := freezeInto(t, root, id, nil); err != nil || again.Digest() != want.Digest() {
		t.Fatalf("a store with many temporaries: %v", err)
	}
	for i := range 1100 {
		_ = os.Remove(filepath.Join(pieces, fmt.Sprintf(".tmp-many-%d", i)))
	}
	if _, err := format.Accept(tree, format.Building, format.Want{Conversation: id}); err != nil {
		t.Fatal(err)
	}
	for name, make := range map[string]func(string) error{
		"a temporary directory": func(p string) error { return os.Mkdir(filepath.Join(p, ".tmp-dir"), 0o755) },
		"a misnamed entry":      func(p string) error { return os.WriteFile(filepath.Join(p, "x.gz"), []byte("x"), 0o600) },
		"a link": func(p string) error {
			return os.Symlink(filepath.Join(p, ".tmp-123"), filepath.Join(p, strings.Repeat("a", 64)+".gz"))
		},
	} {
		if name == "a link" && runtime.GOOS == "windows" {
			continue
		}
		if err := make(pieces); err != nil {
			t.Fatal(err)
		}
		if _, _, err := freezeInto(t, root, id, nil); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
		for _, p := range []string{".tmp-dir", "x.gz", strings.Repeat("a", 64) + ".gz"} {
			_ = os.Remove(filepath.Join(pieces, p))
		}
	}
}

// A freeze tells apart a conversation there is none of, a retired session
// and one whose files a removal is taking.
func TestAFreezeSaysWhyItCannot(t *testing.T) {
	root := t.TempDir()
	id := build(t, root, session(1, "t", talk), base, 0)
	if _, _, err := freezeInto(t, root, "00000009-0000-4000-8000-000000000009", nil); !errors.Is(err, ErrNoChain) {
		t.Fatalf("no chain: %v", err)
	}
	if _, _, err := freezeInto(t, root, "../x", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an unsafe name: %v", err)
	}
	z := storage.NewZone(root)
	if err := os.WriteFile(z.RemovingPath(id), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := freezeInto(t, root, id, nil); !errors.Is(err, ErrRemoving) {
		t.Fatalf("removing: %v", err)
	}
	if err := os.Remove(z.RemovingPath(id)); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(z.RetiredPath(id)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(z.RetiredPath(id), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := freezeInto(t, root, id, nil); !errors.Is(err, ErrRetired) {
		t.Fatalf("retired: %v", err)
	}
}

// changesOnce changes the source the first time the freeze writes into its
// tree, as a collector does beside a pull.
type changesOnce struct {
	storage.DiskFileSystem
	change func()
	done   bool
}

func (c *changesOnce) Link(from, to string) error {
	if !c.done {
		c.done = true
		c.change()
	}
	return c.DiskFileSystem.Link(from, to)
}

func (c *changesOnce) CreateIn(d *storage.Dir, name string, mode os.FileMode) (storage.WriteFile, error) {
	if !c.done && name != format.ManifestFile {
		c.done = true
		c.change()
	}
	return c.DiskFileSystem.CreateIn(d, name, mode)
}

// Create is where a freeze that cannot link, as on Windows, starts the copy
// of its first file.
func (c *changesOnce) Create(path string, mode os.FileMode) (storage.WriteFile, error) {
	if !c.done && filepath.Base(path) != format.ManifestFile {
		c.done = true
		c.change()
	}
	return c.DiskFileSystem.Create(path, mode)
}

// A source that changes while it is frozen is found when the freeze reads it
// again: a new round, a new store entry, a removal begun or finished.
func TestAFreezeFindsASourceThatChanged(t *testing.T) {
	steps := talk + "  - input: add tests\n  - call: {text: Tests added.}\n"
	for name, change := range map[string]func(root, session, conversation string) error{
		"a new round": func(root, _, conversation string) error {
			// The last round is taken away and parsed again from what is
			// landed: the head's commit is what changes.
			files, err := sessionflow.OpenChain(root, conversation).List()
			if err != nil {
				return err
			}
			last := files[len(files)-1].Path
			_ = os.Chmod(last, 0o644)
			b, _ := os.ReadFile(last)
			return os.WriteFile(last, bytes.Replace(b, []byte(`"title":"t"`), []byte(`"title":"u"`), 1), 0o444)
		},
		"a new store entry": func(root, session, _ string) error {
			dir := filepath.Join(root, session, "provider", "pieces")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, strings.Repeat("b", 64)+".gz"), []byte("x"), 0o444)
		},
		"a removal begun": func(root, session, _ string) error {
			return os.WriteFile(storage.NewZone(root).RemovingPath(session), []byte("{}"), 0o600)
		},
		// A removal that began and finished while the files were frozen
		// leaves no marker, and touches no file a freeze lists: only its
		// generation says it ran.
		"a removal finished": func(root, session, _ string) error {
			return os.WriteFile(storage.NewZone(root).RemovalStatePath(session), []byte(`{"generation":5}`), 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			id := build(t, root, session(1, "t", steps), base, 2<<10)
			fs := &changesOnce{change: func() {
				if err := change(root, id, id); err != nil {
					t.Error(err)
				}
			}}
			_, _, err := freezeInto(t, root, id, fs)
			if !fs.done || !errors.Is(err, ErrChanging) && !errors.Is(err, ErrRemoving) {
				t.Fatalf("changed %v: %v", fs.done, err)
			}
		})
	}
}

// listed builds three conversations of different times, titles and sizes
// in one root.
func listed(t *testing.T) (string, []string) {
	t.Helper()
	root := t.TempDir()
	ids := []string{
		build(t, root, session(1, "Build the Parser", talk), base, 0),
		build(t, root, session(2, "ship ǅ release", talk+talk+talk), base.Add(48*time.Hour), 0),
		build(t, root, session(3, "notes", talk+talk), base.Add(24*time.Hour), 0),
	}
	return root, ids
}

func page(t *testing.T, root, query string, now time.Time) *format.Listing {
	t.Helper()
	values, err := url.ParseQuery(query)
	if err != nil {
		t.Fatal(err)
	}
	q, err := ParseQuery(values, now)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	l, err := List(root, q)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func idsOf(l *format.Listing) []string {
	var out []string
	for _, r := range l.Rows {
		out = append(out, r.ID)
	}
	return out
}

// A listing gives each conversation's head as its header says it, newest
// activity first, and filters and pages it.
func TestAListingFiltersAndPages(t *testing.T) {
	root, ids := listed(t)
	now := base.Add(72 * time.Hour)
	all := page(t, root, "", now)
	if got := idsOf(all); fmt.Sprint(got) != fmt.Sprint([]string{ids[1], ids[2], ids[0]}) || all.Next != "" || len(all.Skipped) != 0 {
		t.Fatalf("all: %v next %q skipped %v", got, all.Next, all.Skipped)
	}
	r := all.Rows[0]
	if r.Runtime == "" || r.Title != "ship ǅ release" || r.Round == 0 || len(r.Digest) != 64 || r.Talks != 3 || r.SessionThroughTime == "" {
		t.Fatalf("a row: %+v", r)
	}
	for query, want := range map[string][]string{
		"match=PARSER":         {ids[0]},
		"match=ǆ":              {ids[1]},
		"min_talks=2":          {ids[1], ids[2]},
		"runtime=" + r.Runtime: {ids[1], ids[2], ids[0]},
		"runtime=nope":         nil,
		"since=36h":            {ids[1]},
		"until=36h":            {ids[2], ids[0]},
		"since=60h&until=36h":  {ids[2]},
		"since=" + base.Add(24*time.Hour).Format(time.RFC3339Nano) + "&until=" + base.Add(24*time.Hour).Format(time.RFC3339Nano): nil,
	} {
		if got := idsOf(page(t, root, query, now)); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: %v, want %v", query, got, want)
		}
	}
	first := page(t, root, "limit=2&min_talks=1", now)
	if fmt.Sprint(idsOf(first)) != fmt.Sprint([]string{ids[1], ids[2]}) || first.Next == "" {
		t.Fatalf("the first page: %v %q", idsOf(first), first.Next)
	}
	// The bounds and filters ride in the cursor: a later clock changes
	// nothing on the next page.
	second := page(t, root, "limit=2&min_talks=1&after="+first.Next, now.Add(1000*time.Hour))
	if fmt.Sprint(idsOf(second)) != fmt.Sprint([]string{ids[0]}) || second.Next != "" {
		t.Fatalf("the second page: %v %q", idsOf(second), second.Next)
	}
}

// What the listing is asked is checked whole before it scans: unknown,
// repeated and out-of-range parameters, bad durations and times, reversed
// bounds, and cursors that are malformed or carry other filters.
func TestAListingRefusesWhatItCannotRead(t *testing.T) {
	root, _ := listed(t)
	now := base.Add(72 * time.Hour)
	first := page(t, root, "limit=1", now)
	if first.Next == "" {
		t.Fatal("no cursor")
	}
	long := strings.Repeat("x", 4<<10)
	for _, query := range []string{
		"nope=1", "limit=1&limit=2", "limit=0", "limit=501", "limit=01", "limit=-1", "min_talks=x", "min_talks=-1",
		"since=-1h", "since=0s", "since=yesterday", "since=1h&until=2h", "match=" + long,
		"after=" + first.Next + "=", "after=" + first.Next + "&limit=2", "after=" + first.Next[:len(first.Next)-2],
		"after=e30", "limit=1&after=" + first.Next + "&runtime=x",
	} {
		values, _ := url.ParseQuery(query)
		if _, err := ParseQuery(values, now); !errors.Is(err, format.ErrRequest) {
			t.Errorf("%.80s: %v", query, err)
		}
	}
}

// A conversation the listing cannot read is skipped and named, a retired
// one left out, and one whose rounds are damaged only inside still listed:
// the listing reads only the head's header and commit.
func TestAListingSkipsWhatItCannotRead(t *testing.T) {
	root, ids := listed(t)
	now := base.Add(72 * time.Hour)
	head := func(id string) string {
		files, err := sessionflow.OpenChain(root, id).List()
		if err != nil {
			t.Fatal(err)
		}
		return files[len(files)-1].Path
	}
	rewrite := func(path string, fn func([]byte) []byte) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(path, 0o644)
		if err := os.WriteFile(path, fn(b), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	// Inside only: a frame between the header and the commit.
	rewrite(head(ids[0]), func(b []byte) []byte {
		lines := bytes.SplitAfter(b, []byte("\n"))
		lines[1] = []byte("{broken\n")
		return bytes.Join(lines, nil)
	})
	// The header.
	rewrite(head(ids[2]), func(b []byte) []byte { return append([]byte("{broken\n"), b...) })
	z := storage.NewZone(root)
	if err := os.MkdirAll(filepath.Dir(z.RetiredPath(ids[1])), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(z.RetiredPath(ids[1]), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Linux keeps a name that is not UTF-8; macOS refuses to make one, so
	// there a name the portability rule refuses stands in. Windows refuses
	// that one too, and keeps a name of 258 bytes in UTF-8, past the rule's
	// 255, since it counts 86 characters.
	bad, shown := "bad\xffname", `bad\xFFname`
	switch runtime.GOOS {
	case "linux":
	case "windows":
		bad = strings.Repeat("名", 86)
		shown = bad
	default:
		bad, shown = "bad:name", "bad:name"
	}
	if err := os.MkdirAll(filepath.Join(root, "_conversations", bad, "rounds"), 0o755); err != nil {
		t.Fatal(err)
	}
	l := page(t, root, "", now)
	if fmt.Sprint(idsOf(l)) != fmt.Sprint([]string{ids[0]}) {
		t.Fatalf("rows %v", idsOf(l))
	}
	if len(l.Skipped) != 2 || l.Skipped[0].ID != ids[2] || l.Skipped[1].ID != shown {
		t.Fatalf("skipped %+v", l.Skipped)
	}
}

// failsSyncAfterLink fails the first directory sync after a file is linked
// into a freeze's tree, as a disk that fails a write does.
type failsSyncAfterLink struct {
	storage.DiskFileSystem
	linked, failed bool
}

func (f *failsSyncAfterLink) Link(from, to string) error {
	err := f.DiskFileSystem.Link(from, to)
	f.linked = f.linked || err == nil
	return err
}

func (f *failsSyncAfterLink) SyncDir(path string) error {
	if f.linked && !f.failed {
		f.failed = true
		return syscall.EIO
	}
	return f.DiskFileSystem.SyncDir(path)
}

// A freeze whose own write fails stops with that failure: it is the
// server's storage failing, not a source that changed and is read again.
func TestAFreezesOwnWriteFailureIsNotAChangedSource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a freeze copies on Windows, and links nothing")
	}
	root := t.TempDir()
	id := build(t, root, session(1, "t", talk), base, 0)
	files := &failsSyncAfterLink{}
	_, _, err := freezeInto(t, root, id, files)
	if !files.failed || err == nil || errors.Is(err, ErrChanging) || !errors.Is(err, syscall.EIO) {
		t.Fatalf("failed %v: %v", files.failed, err)
	}
}

// Evidence is a regular file at its own name: a link where a landed file
// should be is invalid evidence, refused before anything reads through it.
func TestALinkedLandedFileIsInvalid(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("links need privileges on Windows")
	}
	root := t.TempDir()
	id := build(t, root, session(1, "t", talk), base, 0)
	v, err := sessionflow.OpenChain(root, id).Fold()
	if err != nil {
		t.Fatal(err)
	}
	files, err := storage.LandedFiles(storage.NewZone(root), v.Session)
	if err != nil || len(files) == 0 {
		t.Fatalf("%d landed files: %v", len(files), err)
	}
	elsewhere := filepath.Join(t.TempDir(), "landed.sd")
	b, err := os.ReadFile(files[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(elsewhere, b, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(files[0].Path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, files[0].Path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := freezeInto(t, root, id, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a linked landed file: %v", err)
	}
}

// A conversation whose landed files retention replaced by skeletons is
// listed as before: the runtime is read from a skeleton's header through
// its compression.
func TestAListingReadsASkeletonsHeader(t *testing.T) {
	root, _ := listed(t)
	now := base.Add(72 * time.Hour)
	before := page(t, root, "", now)
	rem := &retention.Remover{Zone: storage.NewZone(root), MetadataOnlyAfter: time.Hour,
		Now: func() time.Time { return time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) }}
	if res := rem.Pass(retention.Input{}); res.Files == 0 || len(res.Errors) > 0 {
		t.Fatalf("retention removed %d files: %v", res.Files, res.Errors)
	}
	after := page(t, root, "", now)
	a, _ := json.Marshal(after.Rows)
	b, _ := json.Marshal(before.Rows)
	if len(after.Skipped) != 0 || !bytes.Equal(a, b) {
		t.Fatalf("after the removal %s skipped %+v, before %s", a, after.Skipped, b)
	}
	for _, r := range after.Rows {
		if r.Runtime == "" {
			t.Fatalf("%s lists no runtime", r.ID)
		}
	}
}

// A copy this machine cannot make is its own storage failing: it is never
// taken for a source to read again.
func TestALocalCopyFailureIsNotASourceRead(t *testing.T) {
	live := filepath.Join(t.TempDir(), "live")
	if err := os.WriteFile(live, []byte("evidence"), 0o444); err != nil {
		t.Fatal(err)
	}
	_, err := copyOut(live, filepath.Join(t.TempDir(), "missing"))
	var local *localCopyError
	if !errors.As(err, &local) {
		t.Fatalf("a copy into a directory that is not there: %v", err)
	}
	if got := readError(live, err); errors.Is(got, ErrRead) || errors.Is(got, ErrChanging) || errors.Is(got, ErrInvalid) {
		t.Fatalf("a local copy failure classified as %v", got)
	}
}

// A copy this machine cannot make while the freeze reads the source's
// state again stays its own failure, never a source that changed and is
// tried again; anything else read again is a changed source.
func TestALocalCopyFailureOnTheRereadIsNotAChange(t *testing.T) {
	local := &localCopyError{errors.New("no space left on device")}
	if got := rereadError(fmt.Errorf("its head round: %w", local)); errors.Is(got, ErrChanging) || !errors.As(got, &local) {
		t.Fatalf("a local copy failure: %v", got)
	}
	if got := rereadError(errors.New("a round went away")); !errors.Is(got, ErrChanging) {
		t.Fatalf("a changed source: %v", got)
	}
}
