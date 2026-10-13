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
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/verify"
	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/skeleton"
)

// A removal that starts while verify checks a session's bodies deletes a
// body another still-landed one is built from. That body is no damage: the
// check sees the removals changed under it and runs again.
func TestABodyARemovalDeletesDuringTheCheckIsNoDamage(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	dir := z.ProviderDir("s")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The second request copies the first, so it is built from it.
	held := providerbody.NewSession()
	long := strings.Repeat("the same long instructions ", 200)
	var paths []string
	for seq, msgs := range []string{
		`[{"role":"user","content":"` + long + `"}]`,
		`[{"role":"user","content":"` + long + `"},{"role":"assistant","content":"ok"},{"role":"user","content":"and then"}]`,
	} {
		rec, err := held.Encode(providerbody.Body{ID: "req" + string(rune('1'+seq)), Role: providerbody.RoleRequest, Src: "x.json",
			Bytes: []byte(`{"messages":` + msgs + `,"model":"m"}`)})
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		w, err := sessiondata.NewWriter(&buf, &sessiondata.Header{Seq: uint64(seq + 1), At: "2026-01-01T00:00:00Z",
			Kind: sessiondata.KindProviderBody, Adapter: "a/1", Dialect: "d", Src: ".", Session: "s"})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Write(rec); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, storage.LandedName(string(sessiondata.KindProviderBody), "20260101T000000.000000000Z", uint64(seq+1)))
		if err := os.WriteFile(path, buf.Bytes(), 0o444); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	if m, ok := held.Manifest("req2"); !ok || m.Depth != 1 {
		t.Fatal("the second request does not copy the first; the test needs it to")
	}

	// The removal: its intent, its generation, both skeletons, and the first
	// file deleted, after verify read the removals and before it checks.
	ran := false
	restore := verify.BeforeScan(func() {
		if ran {
			return
		}
		ran = true
		if err := storage.SaveRemoving(z.RemovingPath("s"), &storage.Removing{Generation: 1, Through: 2, Phase: storage.RemovingDelete}); err != nil {
			t.Fatal(err)
		}
		if err := storage.SaveRemovalGeneration(z, "s", 1); err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var sk bytes.Buffer
			if err := skeleton.Write(&sk, data, sessiondata.Removed{Generation: 1}); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(storage.SkeletonPath(path), sk.Bytes(), 0o444); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Remove(paths[0]); err != nil {
			t.Fatal(err)
		}
	})
	defer restore()
	rep, err := verify.Session(z, "s")
	if err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("the removal did not run during the check")
	}
	if !rep.OK() || rep.Removing != 1 {
		t.Fatalf("problems %v, %d bodies of the removal counted; want none and 1", rep.Details(), rep.Removing)
	}
}

// A check whose view of the removals keeps changing says it could not tell,
// rather than report what it read as it was then.
func TestACheckThatKeepsRacingIsInconclusive(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	landBodies(t, z.ProviderDir("s"), 0, nil, []byte(`{"content":[{"type":"text","text":"hi"}],"id":"msg_1"}`))
	generation := 0
	restore := verify.BeforeScan(func() {
		generation++
		if err := storage.SaveRemovalGeneration(z, "s", generation); err != nil {
			t.Fatal(err)
		}
	})
	defer restore()
	rep, err := verify.Session(z, "s")
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() || len(rep.Bodies) != 1 || !strings.Contains(rep.Bodies[0].Why, "inconclusive") {
		t.Fatalf("a check that kept racing reports %v", rep.Details())
	}
	if generation < 2 {
		t.Fatalf("the check ran %d times", generation)
	}
}

// A read of the removals that finds an intent and then cannot load it saw a
// removal end between the two, and reads again: the view names the finished
// removal's generation, never an intent that names nothing. Two removals that
// each end between the reads of one view could otherwise look alike.
func TestATornReadOfTheRemovalsIsReadAgain(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	if err := storage.SaveRemoving(z.RemovingPath("s"), &storage.Removing{Generation: 1, Through: 3, Phase: storage.RemovingDelete}); err != nil {
		t.Fatal(err)
	}
	if g, busy, through, err := verify.RemovalView(z, "s"); err != nil || g != 1 || !busy || through != 3 {
		t.Fatalf("a removal in progress reads as %d, %v, %d, %v", g, busy, through, err)
	}
	ended := false
	restore := verify.BetweenReads(func() {
		if ended {
			return
		}
		ended = true
		if err := storage.SaveRemovalGeneration(z, "s", 1); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(z.RemovingPath("s")); err != nil {
			t.Fatal(err)
		}
	})
	defer restore()
	g, busy, through, err := verify.RemovalView(z, "s")
	if err != nil || !ended || g != 1 || busy || through != 0 {
		t.Fatalf("a removal that ended between the reads reads as %d, %v, %d, %v", g, busy, through, err)
	}
}
