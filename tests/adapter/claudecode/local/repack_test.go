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

package local_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/repack"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/verify"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// zoneSessions lists the session directories of a zone.
func zoneSessions(t *testing.T, z *storage.Zone) []string {
	t.Helper()
	items, err := os.ReadDir(z.Root())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, it := range items {
		if it.IsDir() && !strings.HasPrefix(it.Name(), "_") {
			out = append(out, it.Name())
		}
	}
	return out
}

// rawLines gathers every record of a session as its line bytes, keyed by the
// slot it was landed in, in landed order. Two zones that hold the same records
// in the same order under a different cut give equal maps.
func rawLines(t *testing.T, z *storage.Zone, session string) map[string][][]byte {
	t.Helper()
	files, err := storage.LandedFiles(z, session)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][][]byte{}
	for _, lf := range files {
		key := lf.Stream + "|" + lf.RunID + "|" + strings.SplitN(filepath.Base(lf.Path), "-", 2)[0]
		f, err := os.Open(lf.Path)
		if err != nil {
			t.Fatal(err)
		}
		r, err := sessiondata.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		for {
			line, err := r.NextRaw()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			out[key] = append(out[key], line)
		}
		f.Close()
	}
	return out
}

func foldCounts(t *testing.T, z *storage.Zone, session string) (nodes, relations, unresolved int, byKind map[string]int) {
	t.Helper()
	v, err := sessionflow.OpenChain(z.Root(), session).Fold()
	if err != nil {
		t.Fatal(err)
	}
	byKind = map[string]int{}
	for _, n := range v.Nodes {
		byKind[n.Kind]++
	}
	return len(v.Nodes), len(v.Relations), len(v.Unresolved), byKind
}

// TestRepackKeepsRecordsAndStructure guards the one operation that may
// change how landed files are cut. Every record must keep its bytes and its
// order, the new files must respect the budget, the new root must verify,
// and the chain built on it must describe the same conversation.
func TestRepackKeepsRecordsAndStructure(t *testing.T) {
	c := collect(t)
	src := c.zone
	sessions := zoneSessions(t, src)
	if len(sessions) == 0 {
		t.Fatal("the fixture landed no sessions")
	}
	for _, id := range sessions {
		if _, err := parse.Session(src, parse.Options{Conversation: id, Session: id}); err != nil {
			t.Fatalf("parse %s: %v", id, err)
		}
	}

	// A budget small enough to force many cuts on the fixture.
	const budget = 2048
	dst := storage.NewZone(t.TempDir())
	var cuts bool
	for _, id := range sessions {
		st, err := repack.Session(src, dst, id, budget, time.Now())
		if err != nil {
			t.Fatalf("repack %s: %v", id, err)
		}
		if st.Records == 0 {
			t.Fatalf("repack %s landed no records", id)
		}
		if st.FilesOut > st.FilesIn {
			cuts = true
		}

		// The budget holds, except for a file that is a single record.
		files, err := storage.LandedFiles(dst, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, lf := range files {
			fi, err := os.Stat(lf.Path)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(lf.Path)
			if err != nil {
				t.Fatal(err)
			}
			records := bytes.Count(data, []byte("\n")) - 2 // header and closing line
			// The budget counts record bytes; the header and closing line are
			// on top of it, so allow for them.
			if fi.Size() > budget+1024 && records > 1 {
				t.Errorf("%s: %d bytes with %d records exceeds the budget", filepath.Base(lf.Path), fi.Size(), records)
			}
			if fi.Mode().Perm()&0o222 != 0 {
				t.Errorf("%s: a landed file must be read-only", filepath.Base(lf.Path))
			}
		}

		// Same records, same bytes, same order, slot by slot.
		before, after := rawLines(t, src, id), rawLines(t, dst, id)
		if len(before) != len(after) {
			t.Fatalf("%s: %d slots before, %d after", id, len(before), len(after))
		}
		for key, lines := range before {
			got := after[key]
			if len(got) != len(lines) {
				t.Fatalf("%s %s: %d records before, %d after", id, key, len(lines), len(got))
			}
			for i := range lines {
				if !bytes.Equal(lines[i], got[i]) {
					t.Fatalf("%s %s: record %d changed bytes", id, key, i+1)
				}
			}
		}

		// The new root verifies on its own.
		rep, err := verify.Session(dst, id)
		if err != nil {
			t.Fatal(err)
		}
		if !rep.OK() {
			t.Fatalf("%s: the repacked root does not verify: %d problem(s)", id, rep.Problems)
		}

		// Cursors came along, with the last sequence pointing into the new root.
		for key := range before {
			parts := strings.Split(key, "|")
			var dir string
			if parts[0] != "" {
				dir = dst.StreamDir(id, parts[0])
			} else {
				dir = dst.RunDir(id, parts[1])
			}
			cur := filepath.Join(dir, parts[2]+".cursor")
			if _, err := os.Stat(cur); err != nil {
				t.Errorf("%s: cursor %s was not carried over", id, cur)
			}
		}

		// The chain built on the new files describes the same conversation.
		if _, err := parse.Session(dst, parse.Options{Conversation: id, Session: id}); err != nil {
			t.Fatalf("parse repacked %s: %v", id, err)
		}
		n1, r1, u1, k1 := foldCounts(t, src, id)
		n2, r2, u2, k2 := foldCounts(t, dst, id)
		if n1 != n2 || r1 != r2 || u1 != u2 {
			t.Fatalf("%s: structure differs after repack: nodes %d/%d relations %d/%d unresolved %d/%d", id, n1, n2, r1, r2, u1, u2)
		}
		for kind, n := range k1 {
			if k2[kind] != n {
				t.Fatalf("%s: %d %s nodes before, %d after", id, n, kind, k2[kind])
			}
		}
	}
	if !cuts {
		t.Fatal("the budget forced no cuts, so the test proved nothing about re-cutting")
	}
}

// Repacking into the same root is refused: landed files are never rewritten.
func TestRepackRefusesTheSameRoot(t *testing.T) {
	c := collect(t)
	id := zoneSessions(t, c.zone)[0]
	if _, err := repack.Session(c.zone, c.zone, id, 4<<20, time.Now()); err == nil {
		t.Fatal("repack into the same root must be refused")
	}
}
