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

package storage_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/skeleton"
)

// landOne writes a landed file of one record into a stream, and returns its
// path and bytes.
func landOne(t *testing.T, z *storage.Zone, seq uint64) (string, []byte) {
	t.Helper()
	path := filepath.Join(z.StreamDir("s", "main"), storage.LandedName("transcript", storage.Stamp(time.Unix(int64(seq), 0)), seq))
	var buf bytes.Buffer
	w, err := sessiondata.NewWriter(&buf, &sessiondata.Header{H: 1, Schema: sessiondata.Schema, Seq: seq, At: "2026-10-01T00:00:00Z",
		Kind: sessiondata.KindTranscript, Adapter: "test/0", Dialect: "test/1", Src: "s.jsonl", Session: "s", Stream: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(&sessiondata.Record{Ord: seq, Bytes: 2, Parts: []sessiondata.Part{{Kind: sessiondata.PartText, Text: "hi", State: model.ContentAvailable, Bytes: 2}}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := storage.WriteAtomic(path, storage.PermLanded, func(out io.Writer) error { _, err := out.Write(buf.Bytes()); return err }); err != nil {
		t.Fatal(err)
	}
	return path, buf.Bytes()
}

func skeletonOf(t *testing.T, path string, data []byte) string {
	t.Helper()
	sk := strings.TrimSuffix(path, sessiondata.Ext) + sessiondata.SkeletonExt
	if err := storage.WriteAtomic(sk, storage.PermLanded, func(w io.Writer) error {
		return skeleton.Write(w, data, sessiondata.Removed{Generation: 1, Receiver: sessiondata.ReceiverAccepted})
	}); err != nil {
		t.Fatal(err)
	}
	return sk
}

// A removed file keeps its place among the landed files by its skeleton,
// with its sequence, its name and its digest. While a removal is in progress
// both are there, and the landed file is the one listed.
func TestASkeletonKeepsItsFilesPlace(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	p1, d1 := landOne(t, z, 1)
	p2, d2 := landOne(t, z, 2)
	p3, _ := landOne(t, z, 3)
	sk1 := skeletonOf(t, p1, d1)
	if err := os.Remove(p1); err != nil {
		t.Fatal(err)
	}
	skeletonOf(t, p2, d2) // p2 stays: a removal between its phases

	files, err := storage.LandedFiles(z, "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("%d files listed, want 3: %+v", len(files), files)
	}
	if f := files[0]; f.Path != sk1 || !f.Skeleton || f.Seq != 1 || storage.LandedPath(f.Path) != p1 {
		t.Fatalf("the removed file is listed as %+v", f)
	}
	if f := files[1]; f.Path != p2 || f.Skeleton {
		t.Fatalf("a file with a skeleton beside it is listed as %+v, want the landed file", f)
	}
	if f := files[2]; f.Path != p3 || f.Skeleton {
		t.Fatalf("the last file is listed as %+v", f)
	}
	want, _ := storage.FileDigest(p2)
	if got, err := storage.LandedDigest(files[1]); err != nil || got != want {
		t.Fatalf("the landed file's digest is %q, %v", got, err)
	}
	if got, err := storage.LandedDigest(files[0]); err != nil || got != digestOfBytes(d1) {
		t.Fatalf("the removed file's digest is %q, %v, want the one it had", got, err)
	}
	if seq, ok := storage.LandedFileSeq(filepath.Base(sk1)); !ok || seq != 1 {
		t.Fatalf("a skeleton's name reads as sequence %d, %v", seq, ok)
	}
}

// The counter counts a skeleton: a sequence is never used twice, removed or
// not.
func TestTheCounterCountsSkeletons(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	landOne(t, z, 1)
	p2, d2 := landOne(t, z, 2)
	skeletonOf(t, p2, d2)
	if err := os.Remove(p2); err != nil {
		t.Fatal(err)
	}
	st := &storage.SessionState{NextSeq: 1}
	if err := st.RecoverNextSeq(z.SessionDir("s")); err != nil {
		t.Fatal(err)
	}
	if st.NextSeq != 3 {
		t.Fatalf("next sequence %d after a removed file 2, want 3", st.NextSeq)
	}
}

// A removal's intent reads back as written, and one that names no
// generation, no sequence or an unknown phase is refused rather than acted on.
func TestARemovalIntentReadsBack(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	path := z.RemovingPath("s")
	if r, err := storage.LoadRemoving(path); err != nil || r != nil {
		t.Fatalf("no intent read as %+v, %v", r, err)
	}
	want := &storage.Removing{Generation: 2, Through: 9, Phase: storage.RemovingDelete}
	if err := storage.SaveRemoving(path, want); err != nil {
		t.Fatal(err)
	}
	if got, err := storage.LoadRemoving(path); err != nil || *got != *want {
		t.Fatalf("read %+v, %v, want %+v", got, err, want)
	}
	for _, bad := range []string{`{"generation":0,"through":9,"phase":"delete"}`, `{"generation":1,"through":0,"phase":"delete"}`,
		`{"generation":1,"through":9,"phase":"later"}`, `not json`} {
		if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
			t.Fatal(err)
		}
		if r, err := storage.LoadRemoving(path); err == nil {
			t.Fatalf("%s read as %+v", bad, r)
		}
	}
}

func digestOfBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// A reader that listed a landed file a removal then deleted opens the
// skeleton that took its place.
func TestOpenLandedFollowsARemoval(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	path, data := landOne(t, z, 1)
	skeletonOf(t, path, data)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	f, err := storage.OpenLanded(path)
	if err != nil {
		t.Fatalf("a removed file did not open through its skeleton: %v", err)
	}
	defer f.Close()
	r, err := sessiondata.NewReader(f)
	if err != nil || r.Header().Removed == nil {
		t.Fatalf("what opened is not the skeleton: %v", err)
	}
	if _, err := storage.OpenLanded(filepath.Join(filepath.Dir(path), "transcript-x-000009.sd")); !os.IsNotExist(err) {
		t.Fatalf("a file with no skeleton opened: %v", err)
	}
}

// A collector's session lock refuses a session with a removal in progress,
// as busy, and the removal's own lock takes it.
func TestASessionIsBusyDuringARemoval(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	dir := z.SessionDir("s")
	if err := storage.SaveRemoving(z.RemovingPath("s"), &storage.Removing{Generation: 1, Through: 1, Phase: storage.RemovingSkeletons}); err != nil {
		t.Fatal(err)
	}
	if l, err := storage.LockSession(dir); !errors.Is(err, storage.ErrSessionBusy) || !errors.Is(err, storage.ErrRemovalInProgress) {
		if l != nil {
			_ = l.Unlock()
		}
		t.Fatalf("a collector's lock during a removal returned %v", err)
	}
	l, err := storage.LockSessionToRemove(dir)
	if err != nil {
		t.Fatalf("the removal's lock: %v", err)
	}
	_ = l.Unlock()
	if err := os.Remove(z.RemovingPath("s")); err != nil {
		t.Fatal(err)
	}
	l, err = storage.LockSession(dir)
	if err != nil {
		t.Fatalf("a collector's lock after the removal: %v", err)
	}
	_ = l.Unlock()
}

// A file listed before a removal deleted it keeps the digest it had, read
// from its skeleton.
func TestLandedDigestFollowsARemoval(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	path, data := landOne(t, z, 1)
	skeletonOf(t, path, data)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	got, err := storage.LandedDigest(storage.LandedFile{Path: path, Seq: 1})
	if err != nil || got != digestOfBytes(data) {
		t.Fatalf("the digest of a file removed after it was listed is %q, %v", got, err)
	}
}

// The removal generation is 0 before any removal, says a removal is in
// progress while its intent is there, and reads back what was saved.
func TestTheRemovalGenerationReadsBack(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	if g, busy, err := storage.RemovalGeneration(z, "s"); err != nil || g != 0 || busy {
		t.Fatalf("before any removal: %d, %v, %v", g, busy, err)
	}
	if err := storage.SaveRemoving(z.RemovingPath("s"), &storage.Removing{Generation: 2, Through: 1, Phase: storage.RemovingDelete}); err != nil {
		t.Fatal(err)
	}
	if _, busy, err := storage.RemovalGeneration(z, "s"); err != nil || !busy {
		t.Fatalf("during a removal: %v, %v", busy, err)
	}
	if err := storage.SaveRemovalGeneration(z, "s", 2); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(z.RemovingPath("s")); err != nil {
		t.Fatal(err)
	}
	if g, busy, err := storage.RemovalGeneration(z, "s"); err != nil || g != 2 || busy {
		t.Fatalf("after a removal: %d, %v, %v", g, busy, err)
	}
}
