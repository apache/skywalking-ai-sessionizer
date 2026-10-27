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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/pull/format"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/pulltest"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/skeleton"
)

// replaceSame gives path a new file holding the same bytes: what an
// inventory listed is no longer what the name holds. The new file is
// written before the old one is removed: a file system may give a removed
// file's identity to the next file made, and then nothing was replaced.
func replaceSame(t *testing.T, path string) {
	t.Helper()
	next := path + ".next"
	if err := os.WriteFile(next, read(t, path), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRemove(t, path)
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
}

// The tree acceptance checked is the tree it ends with: a listed file
// replaced, a file added or the manifest changed after the content checks
// is refused at the final inventory. A judge's work and a lease, which a
// published snapshot's readers change, may change meanwhile.
func TestAcceptSeesTheTreeChangeWhileItChecks(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(t *testing.T, f *fixture)
		want   string
	}{
		"a listed file replaced": {func(t *testing.T, f *fixture) {
			replaceSame(t, filepath.Join(f.tree, filepath.FromSlash(f.m.Files[len(f.m.Files)-1].File)))
		}, "changed while it was checked"},
		"a file added": {func(t *testing.T, f *fixture) {
			write(t, filepath.Join(f.tree, f.m.Session, "added.sd"), "x")
		}, "is not listed in the manifest"},
		"the manifest replaced by the same bytes": {func(t *testing.T, f *fixture) {
			replaceSame(t, filepath.Join(f.tree, format.ManifestFile))
		}, format.ManifestFile},
		"the manifest emptied": {func(t *testing.T, f *fixture) {
			path := filepath.Join(f.tree, format.ManifestFile)
			mustRemove(t, path)
			write(t, path, "{}")
		}, format.ManifestFile},
		"the manifest removed": {func(t *testing.T, f *fixture) {
			mustRemove(t, filepath.Join(f.tree, format.ManifestFile))
		}, format.ManifestFile},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, "three-rounds", false)
			_, err := format.AcceptBetween(f.tree, format.Building, format.Want{Conversation: f.conversation}, func() { tc.change(t, f) })
			refused(t, err, tc.want)
		})
	}
	f := newFixture(t, "three-rounds", false)
	write(t, filepath.Join(f.tree, f.m.Session, storage.JudgmentsDir, "j000001", "s1-targets-0123456789ab.sj"), "{}")
	_, err := format.AcceptBetween(f.tree, format.Building, format.Want{Conversation: f.conversation}, nil)
	refused(t, err, "no listed file is in")
	mustRemoveAll(t, filepath.Join(f.tree, f.m.Session, storage.JudgmentsDir))
	published, want := publishFixture(t, f)
	_, err = format.AcceptBetween(published, format.Published, want, func() {
		write(t, filepath.Join(published, storage.LeasesDir, "read-"+strings.Repeat("b", 64)+".lock"), "")
		write(t, filepath.Join(published, f.m.Session, storage.JudgmentsDir, "j000002", "s1-targets-0123456789ab.sj"), "{}")
		write(t, filepath.Join(published, f.m.Session, storage.JudgmentsDir, "j000002", storage.WorkDir, "x"), "x")
	})
	if err != nil {
		t.Fatalf("a reader's lease and judge work while acceptance checks: %v", err)
	}
}

// publishFixture gives f's tree the controls of a published snapshot.
func publishFixture(t *testing.T, f *fixture) (string, format.Want) {
	t.Helper()
	const tag = "20261025T103200.000000000Z"
	at, _ := storage.ParseTag(tag)
	marker := storage.SnapshotCopy{Remote: "prod", Conversation: f.conversation, Session: f.m.Session, Round: f.m.Head.Round,
		Digest: string(f.m.Head.Digest), ManifestDigest: string(f.m.Digest()), PublishedAt: at}
	if err := storage.WriteSnapshotCopy(f.tree, tag, marker, nil); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(f.tree, storage.RootLockDir, ".lock"), "")
	if err := os.MkdirAll(filepath.Join(f.tree, storage.LeasesDir), 0o755); err != nil {
		t.Fatal(err)
	}
	return f.tree, format.Want{Conversation: f.conversation, Remote: "prod", Tag: tag}
}

// A skeleton is read as whole as a landed file: its header is bound to where
// it sits, and its records, count and digest must hold, though its gzip
// stays valid and its removed digest unchanged.
func TestAcceptReadsSkeletonsWhole(t *testing.T) {
	isSkeleton := func(e format.Entry) bool { return e.Kind == format.KindSkeleton }
	for name, tc := range map[string]struct {
		scenario string
		match    func(format.Entry) bool
		damage   func(t *testing.T, plain []byte) []byte
		want     string
	}{
		"another kind": {"three-rounds", isSkeleton, func(t *testing.T, plain []byte) []byte {
			return rewritten(t, plain, func(h *sessiondata.Header) { h.Kind = sessiondata.KindJournal })
		}, "kind"},
		"another batch": {"workflow", func(e format.Entry) bool { return isSkeleton(e) && strings.Contains(e.File, "/runs/") },
			func(t *testing.T, plain []byte) []byte {
				return rewritten(t, plain, func(h *sessiondata.Header) { h.Batch = "other" })
			}, `batch "other"`},
		"a record that does not decode": {"three-rounds", isSkeleton, func(_ *testing.T, plain []byte) []byte {
			lines := bytes.SplitAfter(plain, []byte{'\n'})
			lines[1] = []byte("{not json\n")
			return bytes.Join(lines, nil)
		}, "decode record"},
		"another count": {"three-rounds", isSkeleton, func(t *testing.T, plain []byte) []byte {
			// The closing line claims one more record and keeps its digest,
			// so only the count is wrong.
			lines := bytes.SplitAfter(plain, []byte{'\n'})
			closing := len(lines) - 2
			var end struct {
				Records int `json:"records"`
			}
			if err := json.Unmarshal(lines[closing], &end); err != nil {
				t.Fatal(err)
			}
			from := []byte(fmt.Sprintf(`"records":%d`, end.Records))
			changed := bytes.Replace(lines[closing], from, []byte(fmt.Sprintf(`"records":%d`, end.Records+1)), 1)
			if bytes.Equal(changed, lines[closing]) {
				t.Fatalf("no count in the closing line %q", lines[closing])
			}
			lines[closing] = changed
			return bytes.Join(lines, nil)
		}, "records, the file claims"},
		"another closing digest": {"three-rounds", isSkeleton, func(t *testing.T, plain []byte) []byte {
			lines := bytes.SplitAfter(plain, []byte{'\n'})
			closing := len(lines) - 2
			changed := bytes.Replace(lines[closing], []byte(`"digest":"`), []byte(`"digest":"f`), 1)
			if bytes.Equal(changed, lines[closing]) {
				t.Fatalf("no digest in the closing line %q", lines[closing])
			}
			lines[closing] = changed
			return bytes.Join(lines, nil)
		}, "digest mismatch"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, tc.scenario, true)
			e := f.first(t, tc.match)
			path := filepath.Join(f.tree, filepath.FromSlash(e.File))
			damaged := gzipped(t, tc.damage(t, gunzipped(t, read(t, path))))
			if err := pulltest.Restamp(f.tree, f.m, e.File, damaged); err != nil {
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

// The rounds fold as one interpretation of the requested conversation and
// the manifest's session: a round of another parser, policy, session or
// conversation is refused though every digest agrees, and so is a round
// whose name does not carry its commit.
func TestAcceptBindsTheRoundsToOneInterpretation(t *testing.T) {
	for name, edit := range map[string]func(h *sessionflow.Header){
		"parser":       func(h *sessionflow.Header) { h.Parser += "-other" },
		"policy":       func(h *sessionflow.Header) { h.Policy += "-other" },
		"session":      func(h *sessionflow.Header) { h.Session = "other" },
		"conversation": func(h *sessionflow.Header) { h.Conversation = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, "three-rounds", false)
			rechained(t, f, func(h *sessionflow.Header) {
				if h.Round == 2 {
					edit(h)
				}
			})
			if _, err := f.accept(t, format.Building); !errors.Is(err, format.ErrEvidence) {
				t.Fatalf("%v, want refused", err)
			}
		})
	}
	f := newFixture(t, "three-rounds", false)
	i := slices.IndexFunc(f.m.Files, func(e format.Entry) bool { return e.Kind == format.KindRound && e.Round == 1 })
	old := f.m.Files[i].File
	renamed := old[:strings.LastIndexByte(old, '-')+1] + "000000000000.sf"
	if err := os.Rename(filepath.Join(f.tree, filepath.FromSlash(old)), filepath.Join(f.tree, filepath.FromSlash(renamed))); err != nil {
		t.Fatal(err)
	}
	f.m.Files[i].File = renamed
	slices.SortFunc(f.m.Files, func(x, y format.Entry) int { return strings.Compare(x.File, y.File) })
	if err := pulltest.SaveManifest(f.tree, f.m); err != nil {
		t.Fatal(err)
	}
	_, err := f.accept(t, format.Building)
	refused(t, err, "is named for commit")
}

// Every store entry is decompressed and checked, cited or not. A newer tip
// whose bytes are damaged is refused though an older generation would serve
// the same body.
func TestAcceptChecksEveryStoreEntry(t *testing.T) {
	f := newFixture(t, "provider-bodies", true)
	tip := f.first(t, func(e format.Entry) bool { return e.Kind == format.KindStore && strings.Contains(e.File, "/tips/1/") })
	// An uncited tip whose content is not what the manifest says.
	other := gzipped(t, []byte("not a body"))
	uncited := f.m.Session + "/provider/tips/1/" + strings.Repeat("e", 64) + ".gz"
	write(t, filepath.Join(f.tree, filepath.FromSlash(uncited)), string(other))
	sum := sha256.Sum256(other)
	f.m.Files = append(f.m.Files, format.Entry{File: uncited, Kind: format.KindStore, Bytes: uint64(len(other)),
		Digest: format.StoredDigest(hex.EncodeToString(sum[:])), Content: format.ContentDigest(strings.Repeat("0", 64))})
	slices.SortFunc(f.m.Files, func(x, y format.Entry) int { return strings.Compare(x.File, y.File) })
	if err := pulltest.SaveManifest(f.tree, f.m); err != nil {
		t.Fatal(err)
	}
	_, err := f.accept(t, format.Building)
	refused(t, err, uncited)

	g := newFixture(t, "provider-bodies", true)
	newer := strings.Replace(tip.File, "/tips/1/", "/tips/2/", 1)
	data := read(t, filepath.Join(g.tree, filepath.FromSlash(tip.File)))
	damaged := append([]byte{}, data...)
	damaged[len(damaged)-5] ^= 0xff
	write(t, filepath.Join(g.tree, filepath.FromSlash(newer)), string(damaged))
	sum = sha256.Sum256(damaged)
	g.m.Files = append(g.m.Files, format.Entry{File: newer, Kind: format.KindStore, Bytes: uint64(len(damaged)),
		Digest: format.StoredDigest(hex.EncodeToString(sum[:])), Content: tip.Content})
	slices.SortFunc(g.m.Files, func(x, y format.Entry) int { return strings.Compare(x.File, y.File) })
	if err := pulltest.SaveManifest(g.tree, g.m); err != nil {
		t.Fatal(err)
	}
	_, err = g.accept(t, format.Building)
	refused(t, err, newer)
}

// A repeat is checked as itself through acceptance: a later occurrence of a
// body already held, under its id and digest, whose own representation does
// not rebuild, is refused at its file and row though the body it repeats is
// whole.
func TestAcceptChecksEveryRepeat(t *testing.T) {
	f := newFixture(t, "provider-bodies", false)
	var bodies []format.Entry
	for _, e := range f.m.Files {
		if e.Kind == format.KindLanded && strings.Contains(e.File, "/provider_body/") {
			bodies = append(bodies, e)
		}
	}
	slices.SortFunc(bodies, func(x, y format.Entry) int { return cmpSeq(x.Seq, y.Seq) })
	first, last := bodies[0], bodies[len(bodies)-1]
	_, held, err := sessiondata.All(bytes.NewReader(read(t, filepath.Join(f.tree, filepath.FromSlash(first.File)))))
	if err != nil {
		t.Fatal(err)
	}
	// The repeat claims the first body's id, size and digest, and holds a
	// representation that does not rebuild to it.
	repeat := *held[0]
	m, err := providerbody.ManifestOf(&repeat)
	if err != nil {
		t.Fatal(err)
	}
	m.Segments = []providerbody.Segment{{Lit: "{}"}}
	manifest, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	repeat.Parts = []sessiondata.Part{{Kind: sessiondata.PartData, Data: manifest, State: "available", Bytes: len(manifest)}}
	path := filepath.Join(f.tree, filepath.FromSlash(last.File))
	hdr, recs, err := sessiondata.All(bytes.NewReader(read(t, path)))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	w, err := sessiondata.NewWriter(&out, &hdr)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range append(recs, &repeat) {
		if err := w.Write(rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pulltest.Restamp(f.tree, f.m, last.File, out.Bytes()); err != nil {
		t.Fatal(err)
	}
	// The chain binds the changed file's new digest, so only the provider
	// check can refuse.
	rechained(t, f, func(*sessionflow.Header) {})
	_, err = f.accept(t, format.Building)
	refused(t, err, last.File)
	if !strings.Contains(err.Error(), "row "+strconv.Itoa(len(recs)+1)+":") {
		t.Fatalf("%v, want the repeat's row %d", err, len(recs)+1)
	}
}

// A landed file's record that says it was removed is refused, though it
// reads whole and the chain binds the file: only a skeleton holds records
// without their bytes, and only a skeleton's records skip their rebuild.
func TestAcceptRefusesARemovedRecordInALandedFile(t *testing.T) {
	f := newFixture(t, "provider-bodies", false)
	e := f.first(t, func(e format.Entry) bool {
		return e.Kind == format.KindLanded && strings.Contains(e.File, "/provider_body/")
	})
	path := filepath.Join(f.tree, filepath.FromSlash(e.File))
	hdr, recs, err := sessiondata.All(bytes.NewReader(read(t, path)))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	w, err := sessiondata.NewWriter(&out, &hdr)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		if err := w.Write(skeleton.Of(&hdr, rec)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pulltest.Restamp(f.tree, f.m, e.File, out.Bytes()); err != nil {
		t.Fatal(err)
	}
	rechained(t, f, func(*sessionflow.Header) {})
	_, err = f.accept(t, format.Building)
	refused(t, err, "says it was removed")
}
