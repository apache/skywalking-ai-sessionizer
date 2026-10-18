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

package chain_test

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecodechanges"
	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/repack"
	"github.com/apache/skywalking-ai-sessionizer/internal/retention"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario/expect"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/verify"
	"github.com/apache/skywalking-ai-sessionizer/pkg/changes"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

func TestRepeatedRepackPreservesTheDestination(t *testing.T) {
	s := newStage(t, growing)
	s.through("two")
	dst := storage.NewZone(t.TempDir())
	if err := repack.CheckDestination(dst.Root()); err != nil {
		t.Fatal(err)
	}
	if _, err := repack.Session(s.zone, dst, s.session, 2<<20, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := parse.Session(dst, parse.Options{Conversation: s.session, Session: s.session}); err != nil {
		t.Fatal(err)
	}
	before, err := verify.LandedDigests(dst, s.session)
	if err != nil {
		t.Fatal(err)
	}
	if err := repack.CheckDestination(dst.Root()); err == nil {
		t.Fatal("populated destination accepted")
	}
	if _, err := repack.Session(s.zone, dst, s.session, 1024, time.Now()); err == nil {
		t.Fatal("repeated repack accepted")
	}
	after, err := verify.LandedDigests(dst, s.session)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("refused repack changed landed files")
	}
	report, err := verify.Chain(dst, s.session, nil)
	if err != nil || !report.OK() {
		t.Fatalf("destination chain changed: %+v, %v", report, err)
	}
}

func TestRepackRefusesAnOrphanDestinationChain(t *testing.T) {
	s := newStage(t, growing)
	s.through("one")
	dst := storage.NewZone(t.TempDir())
	chain := sessionflow.OpenChain(dst.Root(), s.session)
	if err := os.MkdirAll(chain.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(chain.Dir(), "keep")
	if err := os.WriteFile(sentinel, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repack.Session(s.zone, dst, s.session, 1024, time.Now()); err == nil {
		t.Fatal("destination chain without session data accepted")
	}
	if _, err := os.Stat(dst.SessionDir(s.session)); !os.IsNotExist(err) {
		t.Fatalf("session directory created: %v", err)
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "original" {
		t.Fatalf("chain changed: %q, %v", data, err)
	}
}

// TestRepackRefusesARetiredSession. repack.Session, called directly as the
// scenario runner calls it, refuses a source that holds a retired session,
// whichever it is asked for, and a destination where the session is retired,
// and a source whose markers cannot be read. Each refusal makes nothing.
func TestRepackRefusesARetiredSession(t *testing.T) {
	mark := func(z *storage.Zone, session string) {
		if err := os.MkdirAll(filepath.Join(z.Root(), storage.RetiredDir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(z.RetiredPath(session), nil, 0o444); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]func(src, dst *storage.Zone, session string){
		"another session retired in the source":  func(src, _ *storage.Zone, _ string) { mark(src, "another") },
		"the session retired in the destination": func(_, dst *storage.Zone, session string) { mark(dst, session) },
	}
	if runtime.GOOS != "windows" {
		cases["markers that cannot be read"] = func(src, _ *storage.Zone, _ string) {
			if err := os.WriteFile(filepath.Join(src.Root(), storage.RetiredDir), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			s := newStage(t, growing)
			s.through("one")
			dst := storage.NewZone(t.TempDir())
			set(s.zone, dst, s.session)
			if _, err := repack.Session(s.zone, dst, s.session, 1024, time.Now()); err == nil {
				t.Fatal("the repack was taken")
			}
			if _, err := os.Stat(dst.SessionDir(s.session)); !os.IsNotExist(err) {
				t.Fatalf("a refused repack made the session's directory: %v", err)
			}
		})
	}
}

// TestRepackKeepsTheIdleClock. Retention reads how long a conversation has
// been idle from the header of its newest landed file, the one with the
// highest sequence. A repack keeps that time exactly, whatever its budget:
// a file it cuts says when the newest file its records came from was
// collected, and the files are numbered in the order their records landed.
// Before, a merged file kept its first source file's time, and files were
// numbered stream by stream, so retention removed or retired the copy of a
// conversation the source kept.
func TestRepackKeepsTheIdleClock(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 3, d, 10, 0, 0, 0, time.UTC) }
	s := newStage(t, growing)
	for _, step := range []struct {
		checkpoint string
		at         time.Time
	}{{"one", day(1)}, {"two", day(2)}, {"", day(30)}} {
		b, err := scenario.Build(s.sc, scenario.FormatClaudeCode, s.out, scenario.Options{
			At: time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC), Through: step.checkpoint,
		})
		if err != nil {
			t.Fatal(err)
		}
		s.session = b.Session
		col := s.collector()
		col.Now = func() time.Time { return step.at }
		if st, err := col.CollectAll(nil); err != nil || len(st.Errors) > 0 {
			t.Fatal(err, st.Errors)
		}
	}
	s.parse()
	want := newestAt(t, s.zone, s.session)
	if !want.Equal(day(30)) {
		t.Fatalf("the source's newest file says %s; want day 30", want)
	}
	for _, budget := range []int64{64 << 20, 2 << 20, 1} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			dst := storage.NewZone(t.TempDir())
			if _, err := repack.Session(s.zone, dst, s.session, budget, day(30)); err != nil {
				t.Fatal(err)
			}
			if got := newestAt(t, dst, s.session); !got.Equal(want) {
				t.Fatalf("the destination's newest file says %s; the source's says %s", got, want)
			}
			if _, err := parse.Session(dst, parse.Options{Conversation: s.session, Session: s.session}); err != nil {
				t.Fatal(err)
			}
			for _, z := range []*storage.Zone{s.zone, dst} {
				rem := &retention.Remover{Zone: z, MetadataOnlyAfter: 10 * 24 * time.Hour, RetireAfter: 10 * 24 * time.Hour,
					Now: func() time.Time { return day(31) }}
				if res := rem.Pass(retention.Input{}); res.Files != 0 || len(res.Retired) != 0 || len(res.Errors) != 0 {
					t.Fatalf("%s: retention a day after the last collection removed %d files and retired %v: %v", z.Root(), res.Files, res.Retired, res.Errors)
				}
			}
		})
	}
}

// newestAt is the time the header of a session's highest sequence says,
// the time retention reads.
func newestAt(t *testing.T, z *storage.Zone, session string) time.Time {
	t.Helper()
	files, err := storage.LandedFiles(z, session)
	if err != nil || len(files) == 0 {
		t.Fatalf("no landed files: %v", err)
	}
	at, err := time.Parse(time.RFC3339Nano, landedHeader(t, files[len(files)-1].Path).At)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func landedHeader(t *testing.T, path string) sessiondata.Header {
	t.Helper()
	f, err := sessiondata.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r, err := sessiondata.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	return r.Header()
}

// TestRepackFitsEveryFileInItsBudget. A file a repack writes fits its
// budget with its header and closing line, as a file a collector cuts does,
// unless it holds one record larger than the budget. Before, only the record
// lines were counted.
func TestRepackFitsEveryFileInItsBudget(t *testing.T) {
	s := newStage(t, growing)
	s.through("")
	merged := false
	for _, budget := range []int64{1024, 1536, 2048, 4096} {
		dst := storage.NewZone(t.TempDir())
		if _, err := repack.Session(s.zone, dst, s.session, budget, time.Now()); err != nil {
			t.Fatal(err)
		}
		files, err := storage.LandedFiles(dst, s.session)
		if err != nil {
			t.Fatal(err)
		}
		for _, lf := range files {
			info, err := os.Stat(lf.Path)
			if err != nil {
				t.Fatal(err)
			}
			n := len(records(t, lf.Path))
			if n > 1 && info.Size() > budget {
				t.Fatalf("budget %d: %s holds %d records in %d bytes", budget, filepath.Base(lf.Path), n, info.Size())
			}
			merged = merged || n > 1
		}
	}
	if !merged {
		t.Fatal("no file held two records; the check exercised nothing")
	}
}

// TestRepackCarriesTheCursorOfEveryRecorderRoot. The change records of one
// stream can come from several sources: a recorder root outside the storage
// root, and one inside it with two plugin directories, one of them at the
// same relative path as the first root's. The first source keeps the plain
// cursor, and each other keeps one of its own, named for its origin and its
// path. A recorder inside the storage root writes its origin relative to it,
// so a repack writes that origin, and the name of its cursor, as a collector
// into the new root computes them. Each cursor names the file holding the
// last record its own source landed. Collection into the new root goes on
// from where each source stopped: it lands nothing, then only the records
// appended since, and the new root parses and verifies. Records of two files
// are cut into one only when the files' headers are the same, so every file
// names its own source.
func TestRepackCarriesTheCursorOfEveryRecorderRoot(t *testing.T) {
	const session = "11111111-2222-4333-8444-555555555555"
	line := func(id, tool string) string {
		r := &changes.Record{Schema: changes.Schema, ID: id, CapturedBy: changes.CapturedByASZPlugin, Session: session,
			Stream: "main", Tool: tool, ToolName: "Bash", Time: "2026-09-08T02:00:04Z", Basis: changes.BasisToolWindow,
			Root: &changes.Root{Path: "/w"}, ChangedFiles: changes.Int(0)}
		b, err := r.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		return string(b) + "\n"
	}
	for _, budget := range []int64{1, 64 << 20} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			src := storage.NewZone(t.TempDir())
			outside, inside := t.TempDir(), filepath.Join(src.Root(), "_recorder")
			add := func(root, plugin string, lines ...string) {
				t.Helper()
				path := filepath.Join(root, plugin, "output", session, "main.jsonl")
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
				if err != nil {
					t.Fatal(err)
				}
				for _, l := range lines {
					if _, err := f.WriteString(l); err != nil {
						t.Fatal(err)
					}
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
			}
			add(outside, "file-changes-market", line("o/c1", "toolu_1"), line("o/c2", "toolu_2"))
			add(inside, "file-changes-market", line("i/c1", "toolu_3"))
			add(inside, "asz-changes-market", line("i/c2", "toolu_4"))
			collect := func(z *storage.Zone) int {
				t.Helper()
				records := 0
				for _, root := range []string{outside, inside} {
					st, err := claudecodechanges.New(root, z, 0).CollectAll(nil)
					if err != nil || len(st.Errors) > 0 || st.Conflicts != 0 {
						t.Fatalf("%s: %+v, %v", root, st, err)
					}
					records += st.Records
				}
				return records
			}
			if n := collect(src); n != 4 {
				t.Fatalf("the source landed %d records; want four", n)
			}
			dst := storage.NewZone(t.TempDir())
			if _, err := repack.Session(src, dst, session, budget, time.Now()); err != nil {
				t.Fatal(err)
			}
			sources := func(z *storage.Zone) map[string]bool {
				out := map[string]bool{}
				files, err := storage.LandedFiles(z, session)
				if err != nil {
					t.Fatal(err)
				}
				for _, lf := range files {
					out[landedHeader(t, lf.Path).Src] = true
				}
				return out
			}
			if a, b := sources(src), sources(dst); !reflect.DeepEqual(a, b) || len(a) != 2 {
				t.Fatalf("the files name the sources %v; the source root's name %v", b, a)
			}
			before, after := cursorsIn(t, src, session), cursorsIn(t, dst, session)
			if len(after) != 3 {
				t.Fatalf("the new root has %d cursors; want one for each of three sources", len(after))
			}
			for _, a := range after {
				var b *storage.Cursor
				for _, c := range before {
					if c.Source == a.Source && c.Offset == a.Offset {
						b = c
					}
				}
				if b == nil {
					t.Fatalf("the new root's cursor for %s at %d has none in the source root", a.Source, a.Offset)
				}
				landed := recordsOf(t, src, session, b.LastSeq)
				named := recordsOf(t, dst, session, a.LastSeq)
				if len(landed) == 0 || !slices.Contains(named, landed[len(landed)-1]) {
					t.Fatalf("the cursor for %s at %d names a file holding %v; its source landed last %v", a.Source, a.Offset, named, landed)
				}
			}
			if n := collect(dst); n != 0 {
				t.Fatalf("collection into the new root landed %d records again", n)
			}
			parsedAndVerified(t, dst, session)
			add(outside, "file-changes-market", line("o/c3", "toolu_5"))
			add(inside, "file-changes-market", line("i/c3", "toolu_6"))
			add(inside, "asz-changes-market", line("i/c4", "toolu_7"))
			if n := collect(dst); n != 3 {
				t.Fatalf("collection into the new root landed %d records; want the three appended", n)
			}
			parsedAndVerified(t, dst, session)
		})
	}
}

// cursorsIn reads every cursor of a session's main stream.
func cursorsIn(t *testing.T, z *storage.Zone, session string) []*storage.Cursor {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(z.StreamDir(session, "main"), "*.cursor"))
	if err != nil {
		t.Fatal(err)
	}
	var out []*storage.Cursor
	for _, p := range paths {
		c, err := storage.LoadCursor(p, storage.CursorAppend, "")
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

// recordsOf is the line, the byte offset and the id of every record of a
// session's file with sequence seq, in order.
func recordsOf(t *testing.T, z *storage.Zone, session string, seq uint64) []string {
	t.Helper()
	files, err := storage.LandedFiles(z, session)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, lf := range files {
		if lf.Seq == seq {
			for _, rec := range records(t, lf.Path) {
				out = append(out, fmt.Sprintf("%d@%d %s", rec.Ord, rec.Off, rec.ID))
			}
		}
	}
	return out
}

// parsedAndVerified parses a session until its chain reaches its files,
// and checks the files and the chain verify. A second parse writes nothing.
func parsedAndVerified(t *testing.T, z *storage.Zone, session string) {
	t.Helper()
	for {
		r, err := parse.Session(z, parse.Options{Conversation: session, Session: session})
		if err != nil {
			t.Fatal(err)
		}
		if !r.More {
			break
		}
	}
	if r, err := parse.Session(z, parse.Options{Conversation: session, Session: session}); err != nil || r.Number != 0 {
		t.Fatalf("a second parse wrote round %d: %v", r.Number, err)
	}
	if sr, err := verify.Session(z, session); err != nil || !sr.OK() {
		t.Fatalf("the session does not verify: %v %v", err, sr.Details())
	}
	if cr, err := verify.Chain(z, session, nil); err != nil || !cr.OK() {
		t.Fatalf("the chain does not verify: %v %v", err, cr.Details())
	}
}

// TestRepackKeepsTheOrderWhenAHeaderComesBack. A header can change and
// change back within one stream, as an adapter upgraded and then rolled back
// does. A repack cuts records of two files together only when they are next
// to each other, so the records keep their order: the transcript's lines and
// byte offsets run on with no gap, and every provider body still comes after
// the earlier bodies it copies and cites, so each one rebuilds. Before, the
// two files with the first header were cut into one, which came after the
// file between them.
func TestRepackKeepsTheOrderWhenAHeaderComesBack(t *testing.T) {
	sc, err := scenario.Load(filepath.Join("..", "scenarios", "provider-bodies-long.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	var session string
	for _, checkpoint := range []string{"first", "second", ""} {
		b, err := scenario.Build(sc, scenario.FormatSD, out, scenario.Options{
			At: time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC), Through: checkpoint,
		})
		if err != nil {
			t.Fatal(err)
		}
		session = b.Session
	}
	src := storage.NewZone(out)
	files, err := storage.LandedFiles(src, session)
	if err != nil {
		t.Fatal(err)
	}
	byDir := map[string][]storage.LandedFile{}
	for _, lf := range files {
		byDir[filepath.Dir(lf.Path)] = append(byDir[filepath.Dir(lf.Path)], lf)
	}
	main, provider := byDir[src.StreamDir(session, "main")], byDir[src.ProviderDir(session)]
	if len(main) < 3 || len(provider) < 3 {
		t.Fatalf("the build landed %d transcript and %d provider files; the test needs three of each", len(main), len(provider))
	}
	for _, lf := range []storage.LandedFile{main[1], provider[1]} {
		reheader(t, lf.Path, func(h *sessiondata.Header) { h.Adapter += "-other" })
	}
	if sr, err := verify.Session(src, session); err != nil || !sr.OK() {
		t.Fatalf("the source does not verify: %v %v", err, sr.Details())
	}

	dst := storage.NewZone(t.TempDir())
	if _, err := repack.Session(src, dst, session, 64<<20, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []func(*storage.Zone) string{
		func(z *storage.Zone) string { return z.StreamDir(session, "main") },
		func(z *storage.Zone) string { return z.ProviderDir(session) },
	} {
		if a, b := recordOrder(t, src, session, dir(src)), recordOrder(t, dst, session, dir(dst)); !reflect.DeepEqual(a, b) {
			t.Fatalf("%s holds its records in another order:\n%v\n%v", filepath.Base(dir(dst)), a, b)
		}
	}
	sr, err := verify.Session(dst, session)
	if err != nil || !sr.OK() {
		t.Fatalf("the destination does not verify: %v %v", err, sr.Details())
	}
	for {
		r, err := parse.Session(dst, parse.Options{Conversation: session, Session: session})
		if err != nil {
			t.Fatal(err)
		}
		if !r.More {
			break
		}
	}
	if cr, err := verify.Chain(dst, session, nil); err != nil || !cr.OK() {
		t.Fatalf("the destination's chain does not verify: %v %v", err, cr.Details())
	}
	lines, _, err := expect.ProviderBodiesRebuild(dst.Root(), session)
	if err != nil || len(lines) > 0 {
		t.Fatalf("the destination's provider bodies do not rebuild: %v %v", err, lines)
	}
}

// reheader writes a landed file again with its header changed, keeping its
// record lines byte for byte.
func reheader(t *testing.T, path string, change func(*sessiondata.Header)) {
	t.Helper()
	f, err := sessiondata.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := sessiondata.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	hdr := r.Header()
	var lines [][]byte
	for {
		line, err := r.NextRaw()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, append([]byte(nil), line...))
	}
	f.Close()
	change(&hdr)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	err = storage.WriteAtomic(path, storage.PermLanded, func(w io.Writer) error {
		sw, err := sessiondata.NewWriter(w, &hdr)
		if err != nil {
			return err
		}
		for _, line := range lines {
			if err := sw.WriteRaw(line); err != nil {
				return err
			}
		}
		return sw.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
}

// recordOrder is the line, the byte offset and the id of every record in
// one directory of a session, in the order the session's files hold them.
func recordOrder(t *testing.T, z *storage.Zone, session, dir string) []string {
	t.Helper()
	files, err := storage.LandedFiles(z, session)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, lf := range files {
		if filepath.Dir(lf.Path) != dir {
			continue
		}
		for _, rec := range records(t, lf.Path) {
			out = append(out, fmt.Sprintf("%d@%d %s", rec.Ord, rec.Off, rec.ID))
		}
	}
	return out
}
