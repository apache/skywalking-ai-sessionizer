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

package metrics_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/metrics"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// readMetricsState reads metrics.state as a test sees it.
func readMetricsState(t *testing.T, z *storage.Zone) (derived map[string]string, sessions map[string]json.RawMessage) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(storage.NewSpool(z).Dir(), metrics.StateFile))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Derived  map[string]string          `json:"derived"`
		Sessions map[string]json.RawMessage `json:"sessions"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	return s.Derived, s.Sessions
}

// relUnder is a landed file's path under the root, as metrics.state keys it.
func relUnder(t *testing.T, z *storage.Zone, path string) string {
	t.Helper()
	rel, err := filepath.Rel(z.Root(), path)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.ToSlash(rel)
}

// SpoolOwner reads a name from the right, so no session owns another's
// files. The ids abc and abc-1 are the case a pattern gets wrong.
func TestSpoolOwnerReadsTheNameFromTheRight(t *testing.T) {
	for _, c := range []struct {
		name, owner string
		ok          bool
	}{
		{metrics.SpoolName("abc", 1), "abc", true},
		{metrics.SpoolName("abc-1", 2), "abc-1", true},
		{"metrics-abc-1-000002-local.pb", "abc-1", true},
		{metrics.SpoolName("c30736f2-ac0c-4a72-89b1-01a0844af62d", 1234567), "c30736f2-ac0c-4a72-89b1-01a0844af62d", true},
		// A request the receiver adapter received names no session.
		{"metrics-20260911T100000.000000000Z-000003-otlp.pb", "", false},
		// The deriver writes at least six digits.
		{"metrics-abc-00001-local.pb", "", false},
		{"metrics-abc-00000x-local.pb", "", false},
		{"metrics--000001-local.pb", "", false},
		{"metrics-000001-local.pb", "", false},
		{"spool.state", "", false},
		{metrics.StateFile, "", false},
	} {
		owner, ok := metrics.SpoolOwner(c.name)
		if owner != c.owner || ok != c.ok {
			t.Errorf("SpoolOwner(%q) = %q, %v; want %q, %v", c.name, owner, ok, c.owner, c.ok)
		}
	}
}

// The deriver names its files the way SpoolOwner reads them back.
func TestTheDeriverNamesItsFilesForTheirOwner(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	land(t, z, "abc", "main", 1, []call{{id: "c1", model: "m", at: base, frags: 1, in: 1, out: 1}})
	land(t, z, "abc-1", "main", 1, []call{{id: "c2", model: "m", at: base, frags: 1, in: 2, out: 2}})
	if _, err := deriver(z, base.Add(time.Hour), metrics.Options{}).Pass(nil); err != nil {
		t.Fatal(err)
	}
	files, err := storage.NewSpool(z).List()
	if err != nil {
		t.Fatal(err)
	}
	owners := map[string]int{}
	for _, f := range files {
		owner, ok := metrics.SpoolOwner(filepath.Base(f.Path))
		if !ok {
			t.Fatalf("%s has no owner", f.Path)
		}
		owners[owner]++
	}
	if len(files) != 2 || owners["abc"] != 1 || owners["abc-1"] != 1 {
		t.Fatalf("owners %v of %d files, want one file each for abc and abc-1", owners, len(files))
	}
}

// A session is derived only once the deriver has read every landed file of
// it as the file is now. A file landed since is not derived yet.
func TestDerivedAllNeedsEveryLandedFile(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	land(t, z, "s1", "main", 1, []call{{id: "c1", model: "m", at: base, frags: 1, in: 1, out: 1}})
	land(t, z, "s1", "a1", 2, []call{{id: "c2", model: "m", at: base, frags: 1, in: 1, out: 1}})
	if ok, err := metrics.DerivedAll(z, "s1", nil); err != nil || ok {
		t.Fatalf("before any pass: derived=%v err=%v", ok, err)
	}
	d := deriver(z, base.Add(time.Hour), metrics.Options{})
	if _, err := d.Pass(nil); err != nil {
		t.Fatal(err)
	}
	if ok, err := metrics.DerivedAll(z, "s1", nil); err != nil || !ok {
		t.Fatalf("after a pass: derived=%v err=%v", ok, err)
	}
	land(t, z, "s1", "main", 3, []call{{id: "c3", model: "m", at: base.Add(time.Minute), frags: 1, in: 1, out: 1}})
	if ok, err := metrics.DerivedAll(z, "s1", nil); err != nil || ok {
		t.Fatalf("a file landed since the pass: derived=%v err=%v", ok, err)
	}
	if _, err := d.Pass(nil); err != nil {
		t.Fatal(err)
	}
	if ok, err := metrics.DerivedAll(z, "s1", nil); err != nil || !ok {
		t.Fatalf("after the next pass: derived=%v err=%v", ok, err)
	}

	// A different digest is not the file that was derived.
	loaded, err := metrics.LoadDerived(z)
	if err != nil {
		t.Fatal(err)
	}
	files, err := storage.LandedFiles(z, "s1")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := storage.FileDigest(files[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	rel := relUnder(t, z, files[0].Path)
	if !loaded.Has(rel, digest) || loaded.Has(rel, strings.Repeat("0", 64)) {
		t.Fatalf("Has(%s) must hold for its own digest only", rel)
	}
}

// Forget drops only the entries whose landed file is gone, and a session's
// counted calls only once no file of it is left.
func TestForgetDropsOnlyWhatIsGone(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	p1 := land(t, z, "s1", "main", 1, []call{{id: "c1", model: "m", at: base, frags: 1, in: 1, out: 1}})
	p2 := land(t, z, "s1", "a1", 2, []call{{id: "c2", model: "m", at: base, frags: 1, in: 1, out: 1}})
	land(t, z, "s2", "main", 1, []call{{id: "c3", model: "m", at: base, frags: 1, in: 1, out: 1}})
	d := deriver(z, base.Add(time.Hour), metrics.Options{})
	if _, err := d.Pass(nil); err != nil {
		t.Fatal(err)
	}
	now := base.Add(2 * time.Hour)

	if err := storage.DeleteTree(p2, nil); err != nil {
		t.Fatal(err)
	}
	if err := metrics.Forget(z, []string{"s1"}, now); err != nil {
		t.Fatal(err)
	}
	derived, sessions := readMetricsState(t, z)
	if _, ok := derived[relUnder(t, z, p1)]; !ok {
		t.Fatal("the entry of a file that is still there was dropped")
	}
	if _, ok := derived[relUnder(t, z, p2)]; ok {
		t.Fatal("the entry of a file that is gone stayed")
	}
	if _, ok := sessions["s1"]; !ok {
		t.Fatal("s1 still has a landed file, and lost its counted calls")
	}

	// The session gone whole, as a scenario removal leaves it.
	if err := storage.DeleteTree(z.SessionDir("s1"), nil); err != nil {
		t.Fatal(err)
	}
	if err := metrics.Forget(z, []string{"s1"}, now); err != nil {
		t.Fatal(err)
	}
	derived, sessions = readMetricsState(t, z)
	for rel := range derived {
		if strings.HasPrefix(rel, "s1/") {
			t.Fatalf("%s stayed after s1 was removed whole", rel)
		}
	}
	if _, ok := sessions["s1"]; ok {
		t.Fatal("the counted calls of s1 stayed after it was removed whole")
	}
	if _, ok := sessions["s2"]; !ok || len(derived) != 1 {
		t.Fatalf("s2 was touched: derived %v, sessions %v", derived, sessions)
	}

	// A pass after it never makes the state of s1 again.
	if _, err := d.Pass(nil); err != nil {
		t.Fatal(err)
	}
	if _, sessions = readMetricsState(t, z); sessions["s1"] != nil {
		t.Fatal("a pass made the state of a removed session again")
	}

	z2 := storage.NewZone(t.TempDir())
	if err := metrics.Forget(z2, []string{"s1"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(storage.NewSpool(z2).Dir(), metrics.StateFile)); !os.IsNotExist(err) {
		t.Fatalf("Forget made a metrics.state in a root that had none: %v", err)
	}
}
