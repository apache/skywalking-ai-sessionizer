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

package client_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// published pulls a snapshot and returns its classified root.
func published(t *testing.T) (*env, storage.Root) {
	t.Helper()
	e := newEnv(t, "three-rounds")
	res, _ := e.mustPull(e.manifest(0))
	root, err := storage.ClassifyRoot(filepath.Join(e.dir(), res.Tag))
	if err != nil {
		t.Fatal(err)
	}
	return e, root
}

func (e *env) admit(root storage.Root) (*storage.Admission, error) {
	return storage.AdmitSnapshot(context.Background(), root, e.local.TTL, e.clock.Now, e.local.Files)
}

func leasesOf(t *testing.T, root storage.Root) []string {
	t.Helper()
	return entries(t, filepath.Join(root.Path, storage.LeasesDir))
}

// A reader is admitted with a lease of its own, which its release removes,
// and is refused once the snapshot is older than its time to live.
func TestAdmissionHoldsALeaseUntilExpiry(t *testing.T) {
	e, root := published(t)
	a, err := e.admit(root)
	if err != nil {
		t.Fatal(err)
	}
	if a.Marker.Round == 0 || a.Marker.Session == "" {
		t.Fatalf("marker %+v", a.Marker)
	}
	if got := leasesOf(t, root); len(got) != 1 || got[0] != a.Lease() {
		t.Fatalf("leases %v, admitted %s", got, a.Lease())
	}
	if err := a.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := leasesOf(t, root); len(got) != 0 {
		t.Fatalf("leases after release: %v", got)
	}
	e.clock.set(a.Marker.PublishedAt.Add(e.local.TTL))
	if _, err := e.admit(root); !errors.Is(err, storage.ErrExpired) {
		t.Fatalf("at its time to live: %v, want expired", err)
	}
	e.clock.set(a.Marker.PublishedAt.Add(e.local.TTL - time.Nanosecond))
	b, err := e.admit(root)
	if err != nil {
		t.Fatalf("a nanosecond before: %v", err)
	}
	_ = b.Release(context.Background())
}

// A tag the history lists as removed, or one still being published, admits
// no reader, whatever its tree holds.
func TestAdmissionReadsTheHistory(t *testing.T) {
	e, root := published(t)
	dir := e.dir()
	pull, err := storage.LockOwned(filepath.Join(dir, storage.PullDir))
	if err != nil {
		t.Fatal(err)
	}
	outer := e.outer()
	if err := storage.WriteOuterCopy(dir, outer, root.Tag, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.admit(root); !errors.Is(err, storage.ErrRemovedTag) {
		t.Fatalf("a removed tag: %v", err)
	}
	// A fresh conversation whose newest tag is still being published.
	f, other := published(t)
	fdir := f.dir()
	o := f.outer()
	o.Publishing = &storage.Publishing{Build: strings.Repeat("ab", 32), Tag: other.Tag, ManifestDigest: strings.Repeat("0", 64)}
	if err := storage.WriteOuterCopy(fdir, o, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admit(other); !errors.Is(err, storage.ErrPublishing) {
		t.Fatalf("a tag being published: %v", err)
	}
	_ = pull.Unlock()
}

// A reader's lease becomes a run lease once its run is reserved, without a
// moment unheld; a child admitted by that run lease needs no age check, and
// one whose parent ended is refused.
func TestARunLeaseIsBoundAndInherited(t *testing.T) {
	e, root := published(t)
	ctx := context.Background()
	a, err := e.admit(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.BindRun(ctx, "j000001"); err == nil {
		t.Fatal("bound a run that was never reserved")
	}
	if err := os.MkdirAll(filepath.Join(root.Path, a.Marker.Session, storage.JudgmentsDir, "j000001"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := a.BindRun(ctx, "j000001"); err != nil {
		t.Fatal(err)
	}
	if got := leasesOf(t, root); len(got) != 1 || !strings.HasPrefix(got[0], "run-j000001-") || a.Run() != "j000001" {
		t.Fatalf("leases %v, run %q", got, a.Run())
	}
	// Past the snapshot's time to live, the held run still admits its child.
	e.clock.set(a.Marker.PublishedAt.Add(2 * e.local.TTL))
	child, run, err := storage.AdmitChild(ctx, root, a.Lease(), nil, nil)
	if err != nil || run != "j000001" {
		t.Fatalf("child: %v %q", err, run)
	}
	if _, _, err := storage.AdmitChild(ctx, root, child.Lease(), nil, nil); err == nil {
		t.Fatal("a child's lease admitted a child")
	}
	parent := a.Lease()
	if err := a.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := storage.AdmitChild(ctx, root, parent, nil, nil); err == nil {
		t.Fatal("an ended run admitted a child")
	}
	// A parent that died leaves its lease file, which nobody holds.
	dead := "run-j000001-" + strings.Repeat("d", 64) + ".lock"
	if err := os.WriteFile(filepath.Join(root.Path, storage.LeasesDir, dead), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := storage.AdmitChild(ctx, root, dead, nil, nil); err == nil {
		t.Fatal("a dead run admitted a child")
	}
	if err := os.Remove(filepath.Join(root.Path, storage.LeasesDir, dead)); err != nil {
		t.Fatal(err)
	}
	if err := child.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if got := leasesOf(t, root); len(got) != 0 {
		t.Fatalf("leases left: %v", got)
	}
}

// A layout directory replaced after the root was classified is refused at
// admission, before anything inside is read.
func TestAdmissionRechecksTheLayout(t *testing.T) {
	e, root := published(t)
	conversation := e.dir()
	moved := conversation + "-moved"
	if err := os.Rename(conversation, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, conversation); err != nil {
		t.Skip(err)
	}
	if _, err := e.admit(root); err == nil {
		t.Fatal("admitted through a layout directory replaced by a link")
	}
	if got := entries(t, filepath.Join(moved, root.Tag, storage.LeasesDir)); len(got) != 0 {
		t.Fatalf("a lease was made: %v", got)
	}
}

// A snapshot replaced by a real directory, even a whole copy of itself with
// a valid marker, after the root was classified is refused at admission:
// the recheck finds another directory than the one classified.
func TestAdmissionRechecksTheSnapshotsIdentity(t *testing.T) {
	_, root := published(t)
	copied := root.Path + "-copy"
	if err := copyTree(root.Path, copied); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root.Path, root.Path+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(copied, root.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.AdmitSnapshot(context.Background(), root, time.Hour*24*365*100, time.Now, nil); !errors.Is(err, storage.ErrProtected) {
		t.Fatalf("%v, want the replaced snapshot refused", err)
	}
	if got := leasesOf(t, root); len(got) != 0 {
		t.Fatalf("a lease was made in the replacement: %v", got)
	}
}

func copyTree(from, to string) error {
	return filepath.Walk(from, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, path)
		dest := filepath.Join(to, rel)
		if info.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, data, info.Mode().Perm())
	})
}
