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
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

func TestWriteAtomicIsReadOnlyAndComplete(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "landed.jsonl")
	if err := storage.WriteAtomic(p, storage.PermLanded, func(w io.Writer) error {
		_, err := io.WriteString(w, "hello\n")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	// A landed file is write-once by contract, so it becomes read-only at the
	// instant it appears under its final name.
	if fi.Mode().Perm() != storage.PermLanded {
		t.Errorf("mode = %v, want %v", fi.Mode().Perm(), storage.PermLanded)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "hello\n" {
		t.Errorf("content = %q", b)
	}
	// No temporary files may survive.
	ents, _ := os.ReadDir(filepath.Dir(p))
	if len(ents) != 1 {
		t.Errorf("expected only the landed file, got %d entries", len(ents))
	}
}

func TestWriteAtomicLeavesNothingOnFailure(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "landed.jsonl")
	err := storage.WriteAtomic(p, storage.PermLanded, func(io.Writer) error {
		return io.ErrUnexpectedEOF
	})
	if err == nil {
		t.Fatal("expected the writer error to propagate")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("a failed write must not leave the target in place")
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 0 {
		t.Errorf("a failed write left %d stray file(s)", len(ents))
	}
}

func TestCursorRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cursor")
	in := storage.NewCursor(storage.CursorAppend, "proj/sess.jsonl")
	in.Dev, in.Ino = 16777232, 8461299
	in.Offset, in.Ord = 8823104, 1042
	in.LastUUID = "ccb3597f-81b5-4013-9d7e-fc16237912f6"
	in.TailSHA256 = "1a2b3c"
	in.Size, in.MTime, in.LastSeq = 8823104, 1788046628, 175

	if err := in.Save(p, time.Now()); err != nil {
		t.Fatal(err)
	}
	out, err := storage.LoadCursor(p, storage.CursorAppend, "proj/sess.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if out.Offset != in.Offset || out.Ord != in.Ord || out.Ino != in.Ino ||
		out.LastUUID != in.LastUUID || out.TailSHA256 != in.TailSHA256 || out.LastSeq != in.LastSeq {
		t.Errorf("cursor did not round trip:\n got %+v\nwant %+v", out, in)
	}
}

func TestLoadCursorMissingIsFresh(t *testing.T) {
	c, err := storage.LoadCursor(filepath.Join(t.TempDir(), "nope"), storage.CursorAppend, "x.jsonl")
	if err != nil {
		t.Fatalf("a missing cursor is how a new source begins, not an error: %v", err)
	}
	if c.Offset != 0 || c.Ord != 0 || c.State != storage.CursorActive {
		t.Errorf("fresh cursor = %+v", c)
	}
}

func TestSessionStateSequenceIsMonotonicAcrossStreams(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "session.state")
	s, err := storage.LoadSessionState(p, "sess")
	if err != nil {
		t.Fatal(err)
	}
	// The sequence is shared by every stream in the session; that is what lets
	// the assembler track progress with a single watermark.
	var got []uint64
	for i := 0; i < 3; i++ {
		got = append(got, s.Take())
	}
	if got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Errorf("sequence = %v, want 1,2,3", got)
	}
	if err := s.Save(p, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Restart must resume, not restart, or landed files would collide.
	s2, err := storage.LoadSessionState(p, "sess")
	if err != nil {
		t.Fatal(err)
	}
	if n := s2.Take(); n != 4 {
		t.Errorf("after reload Take() = %d, want 4", n)
	}
}

func TestZoneLayout(t *testing.T) {
	z := storage.NewZone("/root")
	cases := map[string]string{
		z.SessionDir("S"):        "/root/S",
		z.StreamDir("S", "main"): "/root/S/streams/main",
		z.StreamDir("S", "a1"):   "/root/S/streams/a1",
		z.RunDir("S", "wf1"):     "/root/S/runs/wf1",
	}
	// The zone builds real filesystem paths, so on Windows they carry
	// backslashes. The expectation is written once and converted.
	for got, want := range cases {
		if got != filepath.FromSlash(want) {
			t.Errorf("got %q, want %q", got, filepath.FromSlash(want))
		}
	}
}

func TestStampIsSortableAndPathSafe(t *testing.T) {
	a := storage.Stamp(time.Date(2026, 8, 31, 9, 12, 4, 481523000, time.UTC))
	b := storage.Stamp(time.Date(2026, 8, 31, 9, 12, 5, 0, time.UTC))
	if a >= b {
		t.Errorf("stamps must sort lexicographically: %q >= %q", a, b)
	}
	for _, r := range a {
		if r == ':' || r == '/' {
			t.Errorf("stamp %q contains a path-unsafe character", a)
		}
	}
}

// The exclusion is the kernel's, not a check followed by a write. A check has a
// window: two writers both see nothing there and both proceed. That is
// tolerable for a landed file and not for a digest chain, where replacing one
// artifact invalidates every artifact that names it.
func TestWriteExclusiveRefusesAndPreserves(t *testing.T) {
	path := filepath.Join(t.TempDir(), "round.jsonl")
	write := func(body string) error {
		return storage.WriteExclusive(path, storage.PermLanded, func(w io.Writer) error {
			_, err := io.WriteString(w, body)
			return err
		})
	}
	if err := write("original\n"); err != nil {
		t.Fatalf("first write: %v", err)
	}
	err := write("replacement\n")
	if !errors.Is(err, storage.ErrExists) {
		t.Fatalf("second write returned %v, want ErrExists", err)
	}
	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != "original\n" {
		t.Fatalf("content is %q; the refused write still modified the file", got)
	}
	// The refused write must not leave a temporary file behind either.
	ents, derr := os.ReadDir(filepath.Dir(path))
	if derr != nil {
		t.Fatal(derr)
	}
	if len(ents) != 1 {
		t.Fatalf("directory holds %d entries, want just the original", len(ents))
	}
}
