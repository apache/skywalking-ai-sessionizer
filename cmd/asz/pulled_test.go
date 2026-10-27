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

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
)

// pulledHandler is the page of p's root, with its pulled conversations.
func pulledHandler(t *testing.T, p pulled) http.Handler {
	t.Helper()
	pulled, err := newPulledPage(rootConfig(p.root), p.root)
	if err != nil {
		t.Fatal(err)
	}
	if !pulled.any() {
		t.Fatal("the root holds no pulled conversation")
	}
	return pulled.handler(view.New(storage.NewZone(p.root), nil).Handler())
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// The page lists a pulled conversation beside the root's own, under its own
// identifier and with its title opened by its remote, and serves it from
// its snapshot. Each request lets its lease go once it is answered.
func TestThePageServesPulledConversations(t *testing.T) {
	p := pulledSnapshot(t)
	h := pulledHandler(t, p)
	rec := get(t, h, "/api/conversations")
	var rows []map[string]any
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &rows) != nil || len(rows) != 1 {
		t.Fatalf("the list: %d %s", rec.Code, rec.Body)
	}
	id, _ := rows[0]["id"].(string)
	title, _ := rows[0]["title"].(string)
	if id != "remote:prod:"+p.conversation || !strings.HasPrefix(title, "[remote, prod] ") {
		t.Fatalf("the row: %v", rows[0])
	}
	rec = get(t, h, "/api/c/"+url.PathEscape(id)+"/view")
	var doc map[string]any
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &doc) != nil {
		t.Fatalf("the document: %d %.200s", rec.Code, rec.Body)
	}
	if got := p.leases(t); len(got) != 0 {
		t.Fatalf("leases outlived their requests: %v", got)
	}
	if rec := get(t, h, "/api/c/remote:prod:nope/view"); rec.Code != http.StatusNotFound {
		t.Fatalf("a conversation never pulled: %d", rec.Code)
	}
}

// A pulled conversation whose snapshot does not read fails its own request
// and is left out of the list; the page goes on.
func TestADamagedPulledConversationFailsAlone(t *testing.T) {
	p := pulledSnapshot(t)
	flipInterior(t, p)
	h := pulledHandler(t, p)
	if rec := get(t, h, "/api/c/remote:prod:"+p.conversation+"/view"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("the document: %d %.200s", rec.Code, rec.Body)
	}
	rec := get(t, h, "/api/conversations")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("the list: %d %s", rec.Code, rec.Body)
	}
}

// A page made for a snapshot is used again only while its marker says the
// same: a marker changed since is checked against the snapshot's manifest
// as a new page is, and the request fails.
func TestAChangedMarkerIsNotServedFromThePageKept(t *testing.T) {
	p := pulledSnapshot(t)
	h := pulledHandler(t, p)
	path := "/api/c/remote:prod:" + p.conversation + "/view"
	if rec := get(t, h, path); rec.Code != http.StatusOK {
		t.Fatalf("the document: %d", rec.Code)
	}
	marker := filepath.Join(p.snapshot, storage.CopyMarker)
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	digest, _ := m["digest"].(string)
	if len(digest) != 64 {
		t.Fatalf("the marker's digest: %q", digest)
	}
	other := strings.Repeat("0", 64)
	if digest == other {
		other = strings.Repeat("1", 64)
	}
	if err := os.Chmod(marker, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte(strings.Replace(string(data), digest, other, 1)), 0o444); err != nil {
		t.Fatal(err)
	}
	if rec := get(t, h, path); rec.Code == http.StatusOK {
		t.Fatalf("a changed marker was served from the page kept: %d", rec.Code)
	}
}

// leasesAtWrite records how many leases the snapshot holds when the answer
// is written.
type leasesAtWrite struct {
	*httptest.ResponseRecorder
	t       *testing.T
	p       pulled
	atWrite int
}

func (w *leasesAtWrite) Write(b []byte) (int, error) {
	w.atWrite = len(w.p.leases(w.t))
	return w.ResponseRecorder.Write(b)
}

// The list holds the lease of every pulled snapshot it read until its
// answer is written, and lets them go after.
func TestTheListHoldsItsLeasesUntilAnswered(t *testing.T) {
	p := pulledSnapshot(t)
	h := pulledHandler(t, p)
	w := &leasesAtWrite{ResponseRecorder: httptest.NewRecorder(), t: t, p: p}
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/conversations", nil))
	if w.Code != http.StatusOK || w.atWrite != 1 {
		t.Fatalf("%d, %d leases while the list was written", w.Code, w.atWrite)
	}
	if got := p.leases(t); len(got) != 0 {
		t.Fatalf("leases outlived the list: %v", got)
	}
}

// An open list is told when a pulled snapshot comes or goes: the page's
// watcher and asz server's watcher both mark it changed, though nothing of
// the root's own changed.
func TestAPulledSnapshotRefreshesTheOpenList(t *testing.T) {
	p := pulledSnapshot(t)
	pulled, err := newPulledPage(rootConfig(p.root), p.root)
	if err != nil {
		t.Fatal(err)
	}
	for name, watch := range map[string]func(srv *view.Server){
		"view":   func(srv *view.Server) { go watchRoot(srv, 10*time.Millisecond, pulled) },
		"server": func(srv *view.Server) { go watchPulled(srv, 10*time.Millisecond, pulled) },
	} {
		t.Run(name, func(t *testing.T) {
			srv := view.New(storage.NewZone(p.root), nil)
			watch(srv)
			// The first look is the page as it was loaded.
			time.Sleep(100 * time.Millisecond)
			before := srv.Status().LastRefresh
			newer := filepath.Join(filepath.Dir(p.snapshot), storage.Stamp(time.Now().UTC().Add(time.Hour)))
			if err := os.Mkdir(newer, 0o755); err != nil {
				t.Fatal(err)
			}
			defer os.Remove(newer)
			for deadline := time.Now().Add(5 * time.Second); srv.Status().LastRefresh == before; time.Sleep(10 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("the list was not marked changed")
				}
			}
		})
	}
}

// pulledWatched is a pulled page whose snapshots live ttl, with asz
// server's watcher over it, and the list's last refresh once the watcher
// has had its first look.
func pulledWatched(t *testing.T, p pulled, ttl time.Duration) (*view.Server, int64) {
	t.Helper()
	cfg := rootConfig(p.root)
	cfg.RemoteSnapshotTTL = fmt.Sprintf("%ds", int(ttl.Seconds())+1)
	pulled, err := newPulledPage(cfg, p.root)
	if err != nil {
		t.Fatal(err)
	}
	srv := view.New(storage.NewZone(p.root), nil)
	go watchPulled(srv, 10*time.Millisecond, pulled)
	time.Sleep(100 * time.Millisecond)
	return srv, srv.Status().LastRefresh
}

func refreshed(t *testing.T, srv *view.Server, before int64, within time.Duration, what string) {
	t.Helper()
	for deadline := time.Now().Add(within); srv.Status().LastRefresh == before; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the list was not marked changed when %s", what)
		}
	}
}

// A pulled snapshot that expires, with nothing else changed, marks the
// open list changed: a fresh list leaves it out.
func TestAnExpiringSnapshotRefreshesTheOpenList(t *testing.T) {
	p := pulledSnapshot(t)
	tag, err := storage.ParseTag(filepath.Base(p.snapshot))
	if err != nil {
		t.Fatal(err)
	}
	// It expires a few seconds from now.
	srv, before := pulledWatched(t, p, time.Since(tag)+2*time.Second)
	refreshed(t, srv, before, 10*time.Second, "the snapshot expired")
}

// A publication that becomes readable marks the open list changed, though
// it changes no tag: its intent is cleared once its tree is in place.
func TestAFinishedPublicationRefreshesTheOpenList(t *testing.T) {
	p := pulledSnapshot(t)
	dir := filepath.Dir(p.snapshot)
	outer, err := storage.ReadOuterCopy(dir, "prod", p.conversation)
	if err != nil {
		t.Fatal(err)
	}
	newer := storage.Stamp(time.Now().UTC().Add(time.Second))
	outer.GreatestTag = newer
	outer.Publishing = &storage.Publishing{Build: strings.Repeat("a", 64), Tag: newer, ManifestDigest: strings.Repeat("b", 64)}
	if err := storage.WriteOuterCopy(dir, outer, "", nil); err != nil {
		t.Fatal(err)
	}
	srv, before := pulledWatched(t, p, time.Hour)
	outer.Publishing = nil
	if err := storage.WriteOuterCopy(dir, outer, "", nil); err != nil {
		t.Fatal(err)
	}
	refreshed(t, srv, before, 5*time.Second, "the publication finished")
}
