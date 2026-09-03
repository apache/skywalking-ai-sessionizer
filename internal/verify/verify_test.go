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

package verify_test

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/verify"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// landStream writes a landed transcript delta covering source lines from..to.
func landStream(t *testing.T, dir string, seq uint64, from, to int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, err := sessiondata.NewWriter(&buf, &sessiondata.Header{
		H: 1, Seq: seq, At: "2026-08-31T12:00:00Z", Kind: sessiondata.KindTranscript,
		Adapter: "test/0", Src: "proj/s.sd", Session: "s", Stream: "main",
		Dialect: "test/1",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Offsets must be exactly what a real source would produce: each line's
	// payload plus the newline the source carried.
	var off uint64
	for i := 1; i < from; i++ {
		off += uint64(len(payloadFor(i))) + 1
	}
	for i := from; i <= to; i++ {
		p := payloadFor(i)
		if err := w.Write(&sessiondata.Record{
			Ord: uint64(i), Off: off, Bytes: len(p), Sha: "0123456789ab",
			Parts: []sessiondata.Part{{Kind: sessiondata.PartText, Text: string(p)}},
		}); err != nil {
			t.Fatal(err)
		}
		off += uint64(len(p)) + 1
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, "transcript-20260831T120000.000000000Z-"+pad(seq)+".sd")
	if err := os.WriteFile(name, buf.Bytes(), 0o444); err != nil {
		t.Fatal(err)
	}
}

func payloadFor(i int) []byte {
	return []byte(`{"uuid":"u` + strconv.Itoa(i) + `","n":` + strconv.Itoa(i) + `}`)
}

func pad(s uint64) string { return fmt.Sprintf("%06d", s) }

// TestCleanStreamPasses is the control: without it, a verifier that always
// reported a problem would look just as "working" as one that never does.
func TestCleanStreamPasses(t *testing.T) {
	dir := t.TempDir()
	landStream(t, dir, 1, 1, 10)
	landStream(t, dir, 2, 11, 20) // a second delta continuing the same source
	rep, err := verify.Stream(dir, "transcript")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Fatalf("clean stream reported problems: ord=%v byte=%v sha=%v",
			rep.OrdGaps, rep.ByteGaps, rep.ShaBad)
	}
	if rep.Records != 20 || rep.FirstOrd != 1 || rep.LastOrd != 20 {
		t.Errorf("records=%d ord=%d..%d, want 20 records ord 1..20",
			rep.Records, rep.FirstOrd, rep.LastOrd)
	}
}

// TestDetectsMissingDelta covers the failure the check exists for: a whole
// delta lost, which is otherwise invisible - no error, no conflict, just a
// shorter conversation.
func TestDetectsMissingDelta(t *testing.T) {
	dir := t.TempDir()
	landStream(t, dir, 1, 1, 10)
	landStream(t, dir, 3, 21, 30) // lines 11..20 never landed
	rep, err := verify.Stream(dir, "transcript")
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() {
		t.Fatal("a missing delta was not detected")
	}
	if len(rep.OrdGaps) == 0 {
		t.Error("expected an ord gap")
	}
	if len(rep.ByteGaps) == 0 {
		t.Error("expected a byte gap")
	}
}

// A landed file carries a digest over everything in it, so any edit is caught
// when the file is read - a dropped line, a changed value, a truncation.
//
// This is stronger than what it replaced. A per-record digest over source bytes
// could only catch a change to those bytes; this covers the identifiers, the
// positions and the closing counts as well. The trade is that it fails on READ
// rather than as a count, because a file that fails it cannot be interpreted at
// all.
func TestDetectsAnyEditToALandedFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(t *testing.T, path string)
		want string
	}{
		{"a dropped record", func(t *testing.T, path string) { dropLine(t, path, 6) }, "digest mismatch"},
		{"a changed value", func(t *testing.T, path string) {
			rewrite(t, path, func(b []byte) []byte {
				return bytes.Replace(b, []byte("u3"), []byte("u9"), 1)
			})
		}, "digest mismatch"},
		{"a truncated file", func(t *testing.T, path string) {
			rewrite(t, path, func(b []byte) []byte {
				return b[:bytes.LastIndexByte(b[:len(b)-1], '\n')+1]
			})
		}, "no closing line"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			landStream(t, dir, 1, 1, 10)
			path := filepath.Join(dir, "transcript-20260831T120000.000000000Z-000001.sd")
			tc.edit(t, path)

			_, err := verify.Stream(dir, "transcript")
			if err == nil {
				t.Fatalf("%s was not detected", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("detected, but reported %q; expected %q", err, tc.want)
			}
		})
	}
}

// rewrite edits a landed file in place, which chmod 0444 discourages and cannot
// prevent.
func rewrite(t *testing.T, path string, edit func([]byte) []byte) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := edit(b)
	if bytes.Equal(out, b) {
		t.Fatal("test setup: nothing was edited")
	}
	_ = os.Chmod(path, 0o644)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

// dropLine removes the nth line (1-based) from a landed file.
func dropLine(t *testing.T, path string, n int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for i := 1; sc.Scan(); i++ {
		if i == n {
			continue
		}
		out.Write(sc.Bytes())
		out.WriteByte('\n')
	}
	f.Close()
	_ = os.Chmod(path, 0o644)
	if err := os.WriteFile(path, out.Bytes(), 0o444); err != nil {
		t.Fatal(err)
	}
}
