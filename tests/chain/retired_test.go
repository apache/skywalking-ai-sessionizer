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
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	collmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"

	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp/otlptest"
	"github.com/apache/skywalking-ai-sessionizer/internal/metrics"
	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// treeOf is every file under the directories given, with its mode, size and
// time, so a write of any kind shows. A directory that is not there says so.
// A directory is listed by its name and mode only: Windows updates a
// directory's time some moments after a file in it is written, and an entry
// added or removed shows by its own name.
func treeOf(t *testing.T, dirs ...string) []string {
	t.Helper()
	var out []string
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(dir, path)
			if d.IsDir() {
				out = append(out, fmt.Sprintf("%s/%s %s", filepath.Base(dir), filepath.ToSlash(rel), info.Mode()))
				return nil
			}
			out = append(out, fmt.Sprintf("%s/%s %s %d %s", filepath.Base(dir), filepath.ToSlash(rel),
				info.Mode(), info.Size(), info.ModTime()))
			return nil
		})
		if errors.Is(err, fs.ErrNotExist) {
			out = append(out, filepath.Base(dir)+" absent")
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(out)
	return out
}

// retire marks a session retired, as retirement will: an empty file named by
// its id.
func retire(t *testing.T, z *storage.Zone, session string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(z.Root(), storage.RetiredDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(z.RetiredPath(session), nil, 0o444); err != nil {
		t.Fatal(err)
	}
}

// grow appends a line to every transcript of the session's sources, as a
// runtime that resumed it would.
func grow(t *testing.T, r *sentRoot, session string) {
	t.Helper()
	grown := 0
	err := filepath.WalkDir(r.source, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") || !strings.Contains(path, session) {
			return err
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.WriteString(`{"type":"user","uuid":"retired-resume","sessionId":"` + session +
			`","timestamp":"2026-03-02T00:00:00Z","message":{"role":"user","content":"resumed after it was retired"}}` + "\n")
		grown++
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if grown == 0 {
		t.Fatal("no transcript of the session was found to grow")
	}
}

// pointsOf counts the metric points of the requests that name the session,
// and those that name any other.
func pointsOf(reqs []*collmetricspb.ExportMetricsServiceRequest, session string) (points, others int) {
	for _, req := range reqs {
		for _, rm := range req.GetResourceMetrics() {
			for _, sm := range rm.GetScopeMetrics() {
				for _, m := range sm.GetMetrics() {
					for _, p := range m.GetSum().GetDataPoints() {
						if otlptest.Attrs(p.GetAttributes())["session.id"] == session {
							points++
						} else {
							others++
						}
					}
				}
			}
		}
	}
	return points, others
}

// TestARetiredSessionIsNeverWrittenAgain. A session collected, derived and
// parsed, and never sent, is retired with what is left of it still there, as
// when its retirement has not finished; with its directory alone or its chain
// alone, as when it stopped between its two directories; and with nothing
// left, as when it has finished. What is left in the first has no index,
// which a writer that did not ask would build again, and the provider's table
// is lost in every case, so it is built again from what is landed. Its
// runtime resumes it, so its sources grow. Every writer passes it over: no
// file of it lands, no index or chain of it is made or changed, nothing of it
// is derived, and none of its files or rounds is sent. Its metrics requests
// are, as any spooled request is. Every kind of source is there: a
// transcript, a child agent, a workflow, the recorder's execution records and
// provider bodies.
func TestARetiredSessionIsNeverWrittenAgain(t *testing.T) {
	// What is left: everything, its index aside; its directory alone, or its
	// chain alone, as when a retirement stopped between its two directories;
	// and nothing.
	states := []string{"unfinished", "chain gone", "half finished", "finished"}
	for _, name := range []string{"all-kinds", "provider-bodies-long"} {
		for _, state := range states {
			t.Run(name+", "+state, func(t *testing.T) {
				sc, err := scenario.Load(filepath.Join("..", "scenarios", name+".yaml"))
				if err != nil {
					t.Fatal(err)
				}
				r := newSentRoot(t)
				b := r.build(sc, scenario.Options{})
				r.collectAt(landedAt(0))
				r.derive()
				r.parse()
				sessionDir := r.zone.SessionDir(b.Session)
				chainDir := filepath.Join(r.dir, "_conversations", b.Session)
				retire(t, r.zone, b.Session)
				gone := map[string][]string{
					"unfinished":    {r.zone.IndexDir(b.Session)},
					"chain gone":    {chainDir},
					"half finished": {sessionDir},
					"finished":      {sessionDir, chainDir},
				}[state]
				requests := spoolOf(t, r.dir, b.Session)
				if len(requests) == 0 {
					t.Fatal("the session spooled no metrics request; the test needs some")
				}
				// The provider's table is built again from what is landed,
				// which a retired session no longer has, or has only in part.
				if err := os.Remove(r.zone.ProviderStatePath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
					t.Fatal(err)
				}
				for _, dir := range gone {
					if err := storage.DeleteTree(dir, nil); err != nil {
						t.Fatal(err)
					}
				}
				before := treeOf(t, sessionDir, chainDir)
				grow(t, r, b.Session)
				r.rcv.Reset()

				r.collectAt(landedAt(1))
				st, err := (&metrics.Deriver{Zone: r.zone, Options: metrics.Options{Grace: -1, Version: "test"}}).Pass([]string{b.Session})
				if err != nil || len(st.Errors) > 0 {
					t.Fatalf("derive: %v %v", err, st)
				}
				res, err := parse.Session(r.zone, parse.Options{Conversation: b.Session, Session: b.Session, MaxRoundBytes: removalBudget})
				if err != nil || res.Changed() {
					t.Fatalf("parse: %v, changed %v", err, res != nil && res.Changed())
				}
				r.parse()
				r.push()

				if after := treeOf(t, sessionDir, chainDir); strings.Join(after, "\n") != strings.Join(before, "\n") {
					t.Fatalf("what is left of the retired session changed:\nbefore:\n%s\nafter:\n%s",
						strings.Join(before, "\n"), strings.Join(after, "\n"))
				}
				for _, rec := range otlptest.Records(r.rcv.Requests()) {
					if attrs := otlptest.Attrs(rec.Attributes); attrs["asz.session"] == b.Session {
						t.Fatalf("%s of the retired session was sent", attrs["asz.file"])
					}
				}
				// Its metrics requests still go, as any spooled request does:
				// the receiver holds each of them, and the push state records
				// each as sent.
				received := r.rcv.MetricsRequests()
				if len(received) != len(requests) {
					t.Errorf("the receiver holds %d metrics requests, want the session's %d", len(received), len(requests))
				}
				if points, others := pointsOf(received, b.Session); points == 0 || others != 0 {
					t.Errorf("the receiver holds %d points of the retired session and %d of others", points, others)
				}
				sent, err := otlpSent(r)
				if err != nil {
					t.Fatal(err)
				}
				for _, name := range requests {
					if _, ok := sent.Digest(storage.SpoolDir + "/" + name); !ok {
						t.Errorf("the metrics request %s of the retired session was not sent", name)
					}
				}
			})
		}
	}
}

// TestARoundOfALiveSessionUnderARetiredDirectoryIsSent. A conversation's
// directory need not be its session's: here the rounds of live session B are
// under the directory of A, which is retired while its own directory is
// still there. None of A's files is sent, and B's rounds are, each asked by
// the session its header names.
func TestARoundOfALiveSessionUnderARetiredDirectoryIsSent(t *testing.T) {
	r := newSentRoot(t)
	sa, err := scenario.Load(filepath.Join("..", "scenarios", "all-kinds.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	sb, err := scenario.Load(filepath.Join("..", "scenarios", "growth.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	a, b := r.build(sa, scenario.Options{}), r.build(sb, scenario.Options{})
	r.collectAt(landedAt(0))
	for {
		res, err := parse.Session(r.zone, parse.Options{Conversation: a.Session, Session: b.Session, MaxRoundBytes: removalBudget})
		if err != nil {
			t.Fatal(err)
		}
		if !res.Changed() || !res.More {
			break
		}
	}
	retire(t, r.zone, a.Session)
	r.push()
	rounds := 0
	for _, rec := range otlptest.Records(r.rcv.Requests()) {
		attrs := otlptest.Attrs(rec.Attributes)
		if attrs["asz.session"] == a.Session {
			t.Fatalf("%s of the retired session was sent", attrs["asz.file"])
		}
		if strings.HasPrefix(attrs["asz.file"], "_conversations/"+a.Session+"/") && attrs["asz.session"] == b.Session {
			rounds++
		}
	}
	if rounds == 0 {
		t.Fatal("no round of the live session under the retired directory was sent")
	}
}
