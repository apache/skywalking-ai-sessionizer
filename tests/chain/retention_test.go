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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	collmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecodechanges"
	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecodeprovider"
	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp"
	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp/otlptest"
	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/internal/metrics"
	"github.com/apache/skywalking-ai-sessionizer/internal/retention"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario/expect"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/verify"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
	"github.com/apache/skywalking-ai-sessionizer/pkg/skeleton"
)

// What a scenario cannot express about retention: the same conversation
// collected, parsed and sent twice, once with every landed file removed after
// each send and once with none, compared byte for byte.

// landedAt is the clock both roots collect with at checkpoint k: a landed
// file's name and header record when it was collected, so two roots land the
// same bytes only on the same clock.
func landedAt(k int) time.Time { return time.Date(2026, 3, 1, k, 0, 0, 0, time.UTC) }

// later is a clock past the retention of every file a test lands.
func later() time.Time { return time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC) }

func retentionOf(r *sentRoot) *retention.Remover {
	return &retention.Remover{Zone: r.zone, MetadataOnlyAfter: time.Hour, Metrics: true, Now: later}
}

// pipelineAt is one pass of asz collect, collecting on the clock given.
func (r *sentRoot) pipelineAt(now time.Time) {
	r.t.Helper()
	r.collectAt(now)
	r.derive()
	r.parse()
	r.push()
}

// TestRetentionChangesNothingSent runs every scenario through its
// checkpoints twice. One root removes every file after each send, the other
// removes none. The receivers must get the same files, rounds and metrics,
// byte for byte; the chains and the indexes must be the same, the index
// rebuilt from the skeletons too; and the root with skeletons must verify.
// Provider bodies are collected too, and a body landed after a removal is cut
// exactly as with none.
func TestRetentionChangesNothingSent(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "scenarios", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	ran := 0
	for _, path := range paths {
		if strings.HasSuffix(path, ".expect.yaml") {
			continue
		}
		name := strings.TrimSuffix(filepath.Base(path), ".yaml")
		sc, err := scenario.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Run(name, func(t *testing.T) {
			plain, kept := newSentRoot(t), newSentRoot(t)
			plain.maxDelta = budgetOf(t, name)
			kept.maxDelta = plain.maxDelta
			removed := 0
			for k, cp := range append(sc.Checkpoints(), "") {
				for _, r := range []*sentRoot{plain, kept} {
					r.build(sc, scenario.Options{Through: cp})
					r.pipelineAt(landedAt(k))
				}
				res := retentionOf(kept).Pass(retention.Input{})
				if len(res.Errors) > 0 {
					t.Fatalf("checkpoint %q: %v", cp, res.Errors)
				}
				removed += res.Files
			}
			if removed == 0 {
				t.Skip("nothing was removed: the scenario lands no session that is sent")
			}
			sameSent(t, plain.rcv, kept.rcv)
			sameTree(t, plain.dir, kept.dir, "_conversations")
			for _, s := range sessionsOf(t, plain.dir) {
				sameIndex(t, plain.zone, kept.zone, s)
				verified(t, kept.zone, s)
				samePage(t, plain.zone, kept.zone, s)
			}
		})
		ran++
	}
	if ran == 0 {
		t.Fatal("no scenario ran")
	}
}

// budgetOf is the budget a scenario's expectation file collects at, so a
// test cuts its landed files as the scenario's own run does.
func budgetOf(t *testing.T, name string) int64 {
	t.Helper()
	ex, err := expect.Load(filepath.Join("..", "scenarios", name+".expect.yaml"))
	if errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return ex.Collect.MaxDeltaBytes
}

// sameSent checks two receivers got the same files and rounds, each by its
// path, and the same metrics requests, in order.
func sameSent(t *testing.T, a, b *otlptest.Receiver) {
	t.Helper()
	// A file is sent with its bytes and stamped with a time read from the
	// session's records, which a file with no timed record takes from the
	// newest record of the session, removed or not.
	files := func(rcv *otlptest.Receiver) map[string]string {
		out := map[string]string{}
		for _, rec := range otlptest.Records(rcv.Requests()) {
			out[otlptest.Attrs(rec.Attributes)["asz.file"]] = fmt.Sprintf("%d\n%s", rec.GetTimeUnixNano(), rec.GetBody().GetStringValue())
		}
		return out
	}
	fa, fb := files(a), files(b)
	if len(fa) == 0 {
		t.Fatal("nothing was sent")
	}
	for name, body := range fa {
		if got, ok := fb[name]; !ok || got != body {
			t.Fatalf("%s was sent differently with retention (sent=%v)", name, ok)
		}
	}
	if len(fb) != len(fa) {
		t.Fatalf("%d files sent with retention, %d without", len(fb), len(fa))
	}
	ma, mb := a.MetricsRequests(), b.MetricsRequests()
	if len(ma) != len(mb) {
		t.Fatalf("%d metrics requests with retention, %d without", len(mb), len(ma))
	}
	for i := range ma {
		if !proto.Equal(ma[i], mb[i]) {
			t.Fatalf("metrics request %d differs with retention", i+1)
		}
	}
}

// sameTree checks a directory of two roots holds the same files with the
// same bytes.
func sameTree(t *testing.T, a, b, sub string) {
	t.Helper()
	read := func(root string) map[string][]byte {
		out := map[string][]byte{}
		base := filepath.Join(root, sub)
		_ = filepath.Walk(base, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || strings.HasPrefix(info.Name(), ".") || strings.HasSuffix(p, ".state") {
				return nil
			}
			data, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(base, p)
			out[rel] = data
			return nil
		})
		return out
	}
	ta, tb := read(a), read(b)
	if len(ta) == 0 || len(ta) != len(tb) {
		t.Fatalf("%s holds %d files without retention and %d with it", sub, len(ta), len(tb))
	}
	for rel, data := range ta {
		if !bytes.Equal(data, tb[rel]) {
			t.Fatalf("%s/%s differs with retention", sub, rel)
		}
	}
}

// sameIndex checks a session's index is the same with retention as without,
// as the pipeline kept it and as rebuilt from the landed files and the
// skeletons alone.
func sameIndex(t *testing.T, a, b *storage.Zone, session string) {
	t.Helper()
	sameTree(t, a.SessionDir(session), b.SessionDir(session), "index")
	written := func(z *storage.Zone) map[string][]byte {
		ix := index.New(session)
		if _, err := index.Rebuild(z, session, ix, 0); err != nil {
			t.Fatal(err)
		}
		ix.Build()
		dir := t.TempDir()
		if err := ix.Write(dir); err != nil {
			t.Fatal(err)
		}
		out := map[string][]byte{}
		items, _ := os.ReadDir(dir)
		for _, it := range items {
			out[it.Name()], _ = os.ReadFile(filepath.Join(dir, it.Name()))
		}
		return out
	}
	wa, wb := written(a), written(b)
	for name, data := range wa {
		if !bytes.Equal(data, wb[name]) {
			t.Fatalf("the index rebuilt from the skeletons of %s differs in %s", session, name)
		}
	}
}

// verified checks a root with skeletons verifies: every stream contiguous,
// every round bound to the digests its files had, and every removed file
// reported.
func verified(t *testing.T, z *storage.Zone, session string) {
	t.Helper()
	sr, err := verify.Session(z, session)
	if err != nil {
		t.Fatal(err)
	}
	if !sr.OK() {
		t.Fatalf("%s does not verify with skeletons: %v", session, sr.Details())
	}
	cr, err := verify.Chain(z, session, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !cr.OK() {
		t.Fatalf("the chain of %s does not verify with skeletons: %v", session, cr.Details())
	}
	files, err := storage.LandedFiles(z, session)
	if err != nil {
		t.Fatal(err)
	}
	skeletons := 0
	for _, lf := range files {
		if lf.Skeleton {
			skeletons++
		}
	}
	if len(sr.Removed) != skeletons {
		t.Fatalf("verify reports %d removed files of %s, and %d skeletons are there", len(sr.Removed), session, skeletons)
	}
}

// samePage checks the page builds over skeletons, in the state it has
// without them, and lists every file as it landed: its name, size, lines and
// digest, marked removed.
func samePage(t *testing.T, a, b *storage.Zone, session string) {
	t.Helper()
	page := func(z *storage.Zone) *viewDoc {
		c, err := view.New(z, nil).Load(session)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := c.Build()
		if err != nil {
			t.Fatal(err)
		}
		return &viewDoc{state: doc.Summary.State, files: doc.Files, changes: doc.WorkspaceChanges, executions: doc.ToolExecutions}
	}
	pa, pb := page(a), page(b)
	files, err := storage.LandedFiles(b, session)
	if err != nil {
		t.Fatal(err)
	}
	removed := map[string]bool{}
	for _, lf := range files {
		if lf.Skeleton && sessiondata.IsSkeleton(lf.Path) {
			rel, _ := filepath.Rel(b.Root(), storage.LandedPath(lf.Path))
			removed[filepath.ToSlash(rel)] = true
		}
	}
	if pa.state != pb.state {
		t.Fatalf("the page of %s is %s with retention and %s without", session, pb.state, pa.state)
	}
	if len(pa.files) != len(pb.files) {
		t.Fatalf("the page of %s lists %d files with retention and %d without", session, len(pb.files), len(pa.files))
	}
	for i := range pa.files {
		fa, fb := pa.files[i], pb.files[i]
		state := fb.State
		fb.State = ""
		if fa.File != fb.File || fa.Bytes != fb.Bytes || fa.Lines != fb.Lines || fa.Digest != fb.Digest {
			t.Fatalf("the page lists %s with retention as %+v, without as %+v", fa.File, fb, fa)
		}
		if removed[fa.File] != (state == model.ContentRemoved) {
			t.Fatalf("the page marks %s %q, and its skeleton is there: %v", fa.File, state, removed[fa.File])
		}
	}
	// A change and an execution keep their step and their place in time,
	// so the page lists the same records, joined to the same steps, in the
	// same order.
	if len(pa.changes) != len(pb.changes) || len(pa.executions) != len(pb.executions) {
		t.Fatalf("the page of %s lists %d changes and %d executions with retention, %d and %d without", session,
			len(pb.changes), len(pb.executions), len(pa.changes), len(pa.executions))
	}
	for i := range pa.changes {
		a, b := pa.changes[i], pb.changes[i]
		if a.Step != b.Step || a.CapturedBy != b.CapturedBy || a.ID != b.ID || a.Time != b.Time || a.Ref.Seq != b.Ref.Seq {
			t.Fatalf("change %d of %s is %s/%s on %s with retention, %s/%s on %s without", i, session, b.CapturedBy, b.ID, b.Step, a.CapturedBy, a.ID, a.Step)
		}
		// The files the page groups and counts it by.
		files := func(w sessionview.WorkspaceChange) string {
			var out []string
			for _, c := range w.Changes {
				lines := func(n *int) string {
					if n == nil {
						return "null"
					}
					return fmt.Sprint(*n)
				}
				out = append(out, fmt.Sprintf("%s %s %s/%s", c.Path, c.Operation, lines(c.Additions), lines(c.Deletions)))
			}
			if w.Root != nil {
				out = append(out, w.Root.Path)
			}
			return strings.Join(out, ",")
		}
		if files(a) != files(b) {
			t.Fatalf("change %d of %s lists %s with retention, %s without", i, session, files(b), files(a))
		}
	}
	for i := range pa.executions {
		a, b := pa.executions[i], pb.executions[i]
		if a.Step != b.Step || a.ID != b.ID || a.Time != b.Time || a.Outcome != b.Outcome || a.Ref.Seq != b.Ref.Seq {
			t.Fatalf("execution %d of %s is %s on %s, %q, with retention, %s on %s, %q, without", i, session, b.ID, b.Step, b.Outcome, a.ID, a.Step, a.Outcome)
		}
		if b.State != model.ContentRemoved {
			t.Fatalf("execution %d of %s says %q with retention, want removed", i, session, b.State)
		}
	}
}

type viewDoc struct {
	state      string
	files      []sessionview.File
	changes    []sessionview.WorkspaceChange
	executions []sessionview.ToolExecution
}

func sessionsOf(t *testing.T, root string) []string {
	t.Helper()
	items, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, it := range items {
		if it.IsDir() && !strings.HasPrefix(it.Name(), "_") && !strings.HasPrefix(it.Name(), ".") {
			out = append(out, it.Name())
		}
	}
	sort.Strings(out)
	return out
}

// growthAt builds the growth scenario through each of the first n of its
// checkpoints, running fn after each build.
func growthAt(t *testing.T, r *sentRoot, n int, fn func(k int)) *scenario.Built {
	t.Helper()
	sc, err := scenario.Load(filepath.Join("..", "scenarios", "growth.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var b *scenario.Built
	for k, cp := range append(sc.Checkpoints(), "")[:n] {
		b = r.build(sc, scenario.Options{Through: cp})
		fn(k)
	}
	return b
}

func skeletons(t *testing.T, z *storage.Zone, session string) []sessiondata.Header {
	t.Helper()
	files, err := storage.LandedFiles(z, session)
	if err != nil {
		t.Fatal(err)
	}
	var out []sessiondata.Header
	for _, lf := range files {
		if !lf.Skeleton {
			continue
		}
		f, err := sessiondata.Open(lf.Path)
		if err != nil {
			t.Fatal(err)
		}
		rd, err := sessiondata.NewReader(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, rd.Header())
	}
	return out
}

// A session keeps its files while any reason holds: it is still active, a
// landed file is not parsed, its metrics are not derived, or its parse failed
// this pass. Once none holds, the files go. Whether it was sent is not one of
// them: see TestASessionKeepsOnlyItsMetadataSentOrNot.
func TestRetentionKeepsWhatItMust(t *testing.T) {
	type setup func(r *sentRoot, k int)
	full := func(r *sentRoot, k int) { r.pipelineAt(landedAt(k)) }
	for _, c := range []struct {
		name   string
		land   setup
		remove func(r *sentRoot, rem *retention.Remover, session string) retention.Result
	}{
		{"active", full, func(_ *sentRoot, rem *retention.Remover, _ string) retention.Result {
			rem.Now = func() time.Time { return landedAt(1).Add(30 * time.Minute) }
			return rem.Pass(retention.Input{})
		}},
		{"not parsed", func(r *sentRoot, k int) {
			if k == 0 {
				r.pipelineAt(landedAt(k))
				return
			}
			r.collectAt(landedAt(k))
			r.derive()
			r.push()
		}, nil},
		{"metrics not derived", func(r *sentRoot, k int) {
			r.collectAt(landedAt(k))
			r.parse()
			r.push()
		}, nil},
		{"busy", full, func(_ *sentRoot, rem *retention.Remover, session string) retention.Result {
			return rem.Pass(retention.Input{Busy: map[string]bool{session: true}})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newSentRoot(t)
			b := growthAt(t, r, 2, func(k int) { c.land(r, k) })
			rem := retentionOf(r)
			var res retention.Result
			if c.remove != nil {
				res = c.remove(r, rem, b.Session)
			} else {
				res = rem.Pass(retention.Input{})
			}
			if len(res.Errors) > 0 || res.Files != 0 || len(skeletons(t, r.zone, b.Session)) != 0 {
				t.Fatalf("a session kept for %s lost %d file(s): %v", c.name, res.Files, res.Errors)
			}
		})
	}
	t.Run("none holds", func(t *testing.T) {
		r := newSentRoot(t)
		b := growthAt(t, r, 2, func(k int) { r.pipelineAt(landedAt(k)) })
		// A skeleton takes its file's modification time, which the metrics
		// grace period and the export's order read.
		mtimes := map[string]time.Time{}
		landed, _ := storage.LandedFiles(r.zone, b.Session)
		for _, lf := range landed {
			fi, err := os.Stat(lf.Path)
			if err != nil {
				t.Fatal(err)
			}
			mtimes[storage.SkeletonPath(lf.Path)] = fi.ModTime()
		}
		rem := retentionOf(r)
		synced := map[string]time.Time{}
		rem.SyncFile = func(path string) error {
			fi, err := os.Stat(path)
			if err != nil {
				return err
			}
			synced[path] = fi.ModTime()
			return nil
		}
		res := rem.Pass(retention.Input{})
		if len(res.Errors) > 0 || res.Files == 0 {
			t.Fatalf("an idle session that was parsed, derived and sent kept its files: %v", res.Errors)
		}
		for sk, mtime := range mtimes {
			if got, ok := synced[sk]; !ok || !got.Equal(mtime) {
				t.Fatalf("%s was not synced with its file's time: synced %v at %v", sk, ok, got)
			}
		}
		files, _ := storage.LandedFiles(r.zone, b.Session)
		for _, lf := range files {
			if !lf.Skeleton {
				t.Fatalf("%s was not removed", lf.Path)
			}
			fi, err := os.Stat(lf.Path)
			if err != nil || !fi.ModTime().Equal(mtimes[lf.Path]) {
				t.Fatalf("%s has the time %v, its file had %v: %v", lf.Path, fi.ModTime(), mtimes[lf.Path], err)
			}
		}
		if _, err := os.Stat(r.zone.RemovingPath(b.Session)); !os.IsNotExist(err) {
			t.Fatalf("the removal left its intent: %v", err)
		}
	})
}

// A session keeps only its metadata once it is idle whether it was sent or
// not: with no endpoint set, with one set and nothing sent yet, with a send
// tried that no answer confirmed, and with part of it sent, the last two
// also with the endpoint taken away afterwards. Each skeleton says what the
// receiver confirmed of its file, and names the owner only where a send was
// tried. The root verifies, the page draws the conversation and serves its
// provider files as their skeletons, and a later send carries its rounds and
// none of its removed files.
func TestASessionKeepsOnlyItsMetadataSentOrNot(t *testing.T) {
	sc, err := scenario.Load(filepath.Join("..", "scenarios", "provider-bodies-long.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	tried := func(r *sentRoot) {
		r.rcv.Fail(true)
		defer r.rcv.Fail(false)
		client, err := r.rcv.Client(otlp.ProtocolGRPC)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		p := &otlp.Pusher{Zone: r.zone, Client: client, Endpoint: r.endpoint, Version: "test",
			ServiceName: "Removal Test", InstanceID: "removal-test", Layer: "AI_AGENT"}
		if _, err := p.Pass(); err != nil {
			t.Fatal(err)
		}
	}
	whole := func(r *sentRoot, metrics bool) *scenario.Built {
		b := r.build(sc, scenario.Options{})
		r.collectAt(landedAt(0))
		if metrics {
			r.derive()
		}
		r.parse()
		return b
	}
	for _, c := range []struct {
		name string
		// metrics is whether the pipeline that removes derives, as it does
		// only with an endpoint set, so it is false once the endpoint is
		// gone, whatever was derived before; owned is whether a send was
		// tried; sendAfter is whether the endpoint is still there after the
		// removal.
		metrics, owned, sendAfter bool
		land                      func(r *sentRoot) *scenario.Built
		receivers                 []string
	}{
		{"no endpoint", false, false, false, func(r *sentRoot) *scenario.Built { return whole(r, false) },
			[]string{sessiondata.ReceiverUnconfirmed}},
		{"an endpoint, nothing sent", true, false, true, func(r *sentRoot) *scenario.Built { return whole(r, true) },
			[]string{sessiondata.ReceiverUnconfirmed}},
		{"a send no answer confirmed", true, true, true, func(r *sentRoot) *scenario.Built {
			b := whole(r, true)
			tried(r)
			return b
		}, []string{sessiondata.ReceiverUnconfirmed}},
		{"a send no answer confirmed, the endpoint then gone", false, true, false, func(r *sentRoot) *scenario.Built {
			b := whole(r, true)
			tried(r)
			return b
		}, []string{sessiondata.ReceiverUnconfirmed}},
		{"part of it sent, the endpoint then gone", false, true, false, func(r *sentRoot) *scenario.Built {
			r.build(sc, scenario.Options{Through: sc.Checkpoints()[0]})
			r.pipelineAt(landedAt(0))
			b := r.build(sc, scenario.Options{})
			r.collectAt(landedAt(1))
			r.derive()
			r.parse()
			return b
		}, []string{sessiondata.ReceiverAccepted, sessiondata.ReceiverUnconfirmed}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newSentRoot(t)
			b := c.land(r)
			rem := retentionOf(r)
			rem.Metrics = c.metrics
			if res := rem.Pass(retention.Input{}); len(res.Errors) > 0 || res.Files == 0 {
				t.Fatalf("an idle session kept its files: %+v", res)
			}
			sent, err := otlpSent(r)
			if err != nil {
				t.Fatal(err)
			}
			owner := sent.Owner(b.Session)
			if (owner != "") != c.owned {
				t.Fatalf("owner %q: only a tried send names one", owner)
			}
			heads := skeletons(t, r.zone, b.Session)
			if len(heads) == 0 {
				t.Fatal("no skeleton took a removed file's place")
			}
			said := map[string]bool{}
			for _, h := range heads {
				if h.Removed == nil || h.Removed.Owner != owner {
					t.Fatalf("a skeleton says %+v, want the owner %q", h.Removed, owner)
				}
				said[h.Removed.Receiver] = true
			}
			if len(said) != len(c.receivers) {
				t.Fatalf("the skeletons say %v, want %v", said, c.receivers)
			}
			for _, want := range c.receivers {
				if !said[want] {
					t.Fatalf("the skeletons say %v, want %v", said, c.receivers)
				}
			}
			landed, _ := storage.LandedFiles(r.zone, b.Session)
			for _, lf := range landed {
				if !lf.Skeleton {
					t.Fatalf("%s was not removed", lf.Path)
				}
			}
			verified(t, r.zone, b.Session)

			h := view.New(r.zone, nil).Handler()
			get := func(path string) (int, []byte) {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
				return w.Code, w.Body.Bytes()
			}
			code, body := get("/api/c/" + b.Session + "/view")
			if code != http.StatusOK || !bytes.Contains(body, []byte(`"state":"removed"`)) {
				t.Fatalf("the document is %d and says nothing was removed", code)
			}
			var doc sessionview.Conversation
			if err := json.Unmarshal(body, &doc); err != nil {
				t.Fatal(err)
			}
			q := url.Values{"session": {b.Session}}
			bodies := 0
			for _, f := range doc.Files {
				if f.Kind == string(sessiondata.KindProviderBody) && f.Seq != nil {
					q.Add("seq", fmt.Sprint(*f.Seq))
					bodies++
				}
			}
			if bodies == 0 {
				t.Fatal("the scenario lands no provider body; the test needs some")
			}
			code, body = get("/api/c/" + b.Session + "/files?" + q.Encode())
			var served []struct {
				Seq   uint64 `json:"seq"`
				Bytes []byte `json:"bytes"`
			}
			if code != http.StatusOK || json.Unmarshal(body, &served) != nil || len(served) != bodies {
				t.Fatalf("the files route answered %d with %d of %d provider files", code, len(served), bodies)
			}
			for _, f := range served {
				if len(f.Bytes) == 0 {
					t.Fatalf("provider file %d was served empty", f.Seq)
				}
			}

			if !c.sendAfter {
				return
			}
			r.rcv.Reset()
			r.push()
			names := map[string]bool{}
			for _, rec := range otlptest.Records(r.rcv.Requests()) {
				names[otlptest.Attrs(rec.Attributes)["asz.file"]] = true
			}
			rounds := 0
			for name := range names {
				if strings.HasPrefix(name, b.Session+"/") {
					t.Fatalf("%s was sent, although retention removed it", name)
				}
				if strings.HasPrefix(name, "_conversations/") {
					rounds++
				}
			}
			if rounds == 0 {
				t.Fatal("the send carried no round of the conversation")
			}
		})
	}
}

// Metrics turned on after a session that was never sent kept only its
// metadata count the same tokens from its skeletons as from its landed files,
// derive no MCP call from them, and say how many of its execution
// observations they cannot derive MCP metrics for: every distinct one, since
// nothing of them was counted before the removal.
func TestLateMetricsSayWhatTheyCannotDerive(t *testing.T) {
	sc, err := scenario.Load(filepath.Join("..", "scenarios", "mcp-calls.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	plain, kept := newSentRoot(t), newSentRoot(t)
	var b *scenario.Built
	for _, r := range []*sentRoot{plain, kept} {
		b = r.build(sc, scenario.Options{})
		r.collectAt(landedAt(0))
		r.parse()
	}
	observations := map[string]bool{}
	landed, _ := storage.LandedFiles(kept.zone, b.Session)
	for _, lf := range landed {
		f, err := sessiondata.Open(lf.Path)
		if err != nil {
			t.Fatal(err)
		}
		hdr, recs, err := sessiondata.All(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Kind != sessiondata.KindExecution {
			continue
		}
		for _, rec := range recs {
			observations[rec.ID] = true
		}
	}
	if len(observations) == 0 {
		t.Fatal("the scenario lands no execution record")
	}
	rem := retentionOf(kept)
	rem.Metrics = false
	if res := rem.Pass(retention.Input{}); res.Files == 0 || len(res.Errors) > 0 {
		t.Fatalf("the removal: %+v", res)
	}
	if plain.derive() == 0 {
		t.Fatal("the landed files derive no request")
	}
	st, err := (&metrics.Deriver{Zone: kept.zone, Options: metrics.Options{Grace: -1, Version: "test"}}).Pass(nil)
	if err != nil || len(st.Errors) > 0 {
		t.Fatalf("%v %v", err, st)
	}
	if st.Unread[b.Session] != len(observations) {
		t.Errorf("unread %v, want the %d observations the session landed", st.Unread, len(observations))
	}
	whole, removed := metricTotals(t, plain.dir), metricTotals(t, kept.dir)
	if whole[metrics.TokenUsage] == 0 || whole[metrics.TokenUsage] != removed[metrics.TokenUsage] {
		t.Errorf("tokens: %v from the landed files, %v from the skeletons", whole[metrics.TokenUsage], removed[metrics.TokenUsage])
	}
	if whole[metrics.MCPCalls] == 0 || removed[metrics.MCPCalls] != 0 || removed[metrics.MCPDuration] != 0 {
		t.Errorf("MCP calls: %v from the landed files, %v and %v ms from the skeletons", whole[metrics.MCPCalls],
			removed[metrics.MCPCalls], removed[metrics.MCPDuration])
	}
}

// metricTotals adds up every point of every spooled request of a root, by
// metric.
func metricTotals(t *testing.T, dir string) map[string]float64 {
	t.Helper()
	files, err := storage.NewSpool(storage.NewZone(dir)).List()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, f := range files {
		data, err := os.ReadFile(f.Path)
		if err != nil {
			t.Fatal(err)
		}
		var req collmetricspb.ExportMetricsServiceRequest
		if err := proto.Unmarshal(data, &req); err != nil {
			t.Fatal(err)
		}
		for _, rm := range req.ResourceMetrics {
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					for _, p := range m.GetSum().GetDataPoints() {
						out[m.Name] += p.GetAsDouble()
					}
				}
			}
		}
	}
	return out
}

// Each skeleton says what the receiver confirmed of its file: accepted,
// accepted with rejected records, or nothing, for a file landed after the
// last send. Each removal is a generation of its own.
func TestRetentionRecordsWhatTheReceiverConfirmed(t *testing.T) {
	r := newSentRoot(t)
	b := growthAt(t, r, 3, func(k int) {
		switch k {
		case 0:
			r.pipelineAt(landedAt(k))
		case 1:
			r.rcv.Reject(1)
			r.pipelineAt(landedAt(k))
			r.rcv.Reject(0)
			if res := retentionOf(r).Pass(retention.Input{}); res.Files == 0 || len(res.Errors) > 0 {
				t.Fatalf("the first removal: %+v", res)
			}
		case 2:
			r.collectAt(landedAt(k))
			r.derive()
			r.parse()
		}
	})
	if res := retentionOf(r).Pass(retention.Input{}); res.Files == 0 || len(res.Errors) > 0 {
		t.Fatalf("the second removal: %+v", res)
	}
	sent, err := otlpSent(r)
	if err != nil {
		t.Fatal(err)
	}
	owner := sent.Owner(b.Session)
	seen := map[string]int{}
	gens := map[int]int{}
	for _, h := range skeletons(t, r.zone, b.Session) {
		rm := h.Removed
		if rm == nil || rm.Owner != owner || rm.Digest == "" || rm.Records == 0 || rm.At == "" {
			t.Fatalf("a skeleton header says %+v", rm)
		}
		seen[rm.Receiver]++
		gens[rm.Generation]++
	}
	for _, want := range []string{sessiondata.ReceiverAccepted, sessiondata.ReceiverRejected, sessiondata.ReceiverUnconfirmed} {
		if seen[want] == 0 {
			t.Fatalf("no skeleton says %s: %v", want, seen)
		}
	}
	if len(gens) != 2 || gens[1] == 0 || gens[2] == 0 {
		t.Fatalf("generations %v, want 1 and 2", gens)
	}
}

// A removal takes what metrics derived from its files once it was sent: the
// receipts and the sent requests. A request the receiver has not got stays
// until it is sent.
func TestRetentionKeepsUnsentMetrics(t *testing.T) {
	r := newSentRoot(t)
	b := growthAt(t, r, 2, func(k int) {
		if k == 0 {
			r.pipelineAt(landedAt(k))
			return
		}
		r.collectAt(landedAt(k))
		r.derive()
		r.parse()
	})
	spoolBefore := spoolOf(t, r.dir, b.Session)
	if len(spoolBefore) < 2 {
		t.Fatalf("the spool holds %v, want a sent and an unsent request", spoolBefore)
	}
	sent, err := otlpSent(r)
	if err != nil {
		t.Fatal(err)
	}
	if res := retentionOf(r).Pass(retention.Input{}); res.Files == 0 || len(res.Errors) > 0 {
		t.Fatalf("the removal: %+v", res)
	}
	for _, name := range spoolBefore {
		_, wasSent := sent.Digest(storage.SpoolDir + "/" + name)
		_, err := os.Stat(filepath.Join(r.dir, storage.SpoolDir, name))
		if wasSent != os.IsNotExist(err) {
			t.Fatalf("%s: sent %v, and after the removal it is there: %v", name, wasSent, err == nil)
		}
	}
	// Once the kept request is sent, the next pass takes it too.
	r.push()
	if res := retentionOf(r).Pass(retention.Input{}); len(res.Errors) > 0 {
		t.Fatal(res.Errors)
	}
	if left := spoolOf(t, r.dir, b.Session); len(left) != 0 {
		t.Fatalf("requests sent after the removal are left: %v", left)
	}
}

// A removal stopped at any point, as a crash stops it, is finished by the
// next pass's recovery, with retention off: every file removed, nothing lost,
// and the index as it was.
func TestRetentionFinishesAfterACrash(t *testing.T) {
	// growth has transcripts only; provider-bodies-long has provider bodies
	// too, whose removal keeps the chain tips first, and
	// provider-bodies-too-large bodies none of which can be rebuilt.
	for _, name := range []string{"growth", "provider-bodies-long", "provider-bodies-too-large"} {
		t.Run(name, func(t *testing.T) { removalFinishesAfterACrash(t, name) })
	}
}

// builtThrough builds a scenario through every checkpoint and its end,
// running a pipeline pass after each build.
func builtThrough(t *testing.T, r *sentRoot, name string) *scenario.Built {
	t.Helper()
	sc, err := scenario.Load(filepath.Join("..", "scenarios", name+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	r.maxDelta = budgetOf(t, name)
	var b *scenario.Built
	for k, cp := range append(sc.Checkpoints(), "") {
		b = r.build(sc, scenario.Options{Through: cp})
		r.pipelineAt(landedAt(k))
	}
	return b
}

func removalFinishesAfterACrash(t *testing.T, name string) {
	base := newSentRoot(t)
	b := builtThrough(t, base, name)
	files, _ := storage.LandedFiles(base.zone, b.Session)
	receiptsBefore, _ := filepath.Glob(filepath.Join(base.zone.SessionDir(b.Session), "metrics", "*.json"))
	stops := []retention.Point{{Phase: storage.RemovingSkeletons, Done: 0}}
	for i := 1; i <= len(files); i++ {
		stops = append(stops, retention.Point{Phase: storage.RemovingSkeletons, Done: i})
	}
	for i := 0; i <= len(files); i++ {
		stops = append(stops, retention.Point{Phase: storage.RemovingDelete, Done: i})
	}
	for _, at := range stops {
		t.Run(fmt.Sprintf("%s-%d", at.Phase, at.Done), func(t *testing.T) {
			r := base.copyOf(t)
			rem := retentionOf(r)
			rem.Stop = func(p retention.Point) error {
				if p == at {
					return errCrash
				}
				return nil
			}
			res := rem.Pass(retention.Input{})
			if len(res.Errors) == 0 {
				t.Fatal("the removal did not stop")
			}
			if intent, err := storage.LoadRemoving(r.zone.RemovingPath(b.Session)); err != nil || intent == nil {
				t.Fatalf("a stopped removal left no intent: %v", err)
			}
			// The root verifies as the removal left it: a body not yet
			// deleted rebuilds, or is one the removal deletes.
			verified(t, r.zone, b.Session)
			rec := (&retention.Remover{Zone: r.zone}).Recover()
			if len(rec.Errors) > 0 {
				t.Fatal(rec.Errors)
			}
			after, _ := storage.LandedFiles(r.zone, b.Session)
			if len(after) != len(files) {
				t.Fatalf("%d files after recovery, %d before", len(after), len(files))
			}
			for _, lf := range after {
				if !lf.Skeleton {
					t.Fatalf("%s survived the recovery", lf.Path)
				}
			}
			if _, err := os.Stat(r.zone.RemovingPath(b.Session)); !os.IsNotExist(err) {
				t.Fatalf("the recovery left the intent: %v", err)
			}
			sameIndex(t, base.zone, r.zone, b.Session)
			verified(t, r.zone, b.Session)
			// A page learns the files were replaced, whichever point the
			// removal stopped at.
			if g, busy, err := storage.RemovalGeneration(r.zone, b.Session); err != nil || busy || g != 1 {
				t.Fatalf("the removal generation after recovery is %d, in progress %v, %v; want 1", g, busy, err)
			}
			// The sent requests of the removed files go with them, a crash
			// between a file and its request included. The receipts stay.
			if left := spoolOf(t, r.dir, b.Session); len(left) != 0 {
				t.Fatalf("sent requests of removed files are left: %v", left)
			}
			if receipts, _ := filepath.Glob(filepath.Join(r.zone.SessionDir(b.Session), "metrics", "*.json")); len(receipts) != len(receiptsBefore) {
				t.Fatalf("%d receipts after the removal, %d before, want each kept", len(receipts), len(receiptsBefore))
			}
		})
	}
}

var errCrash = errors.New("stopped as a crash would stop it")

// A removal stopped after its last deletion and before the sync that makes
// the deletions durable is finished by a recovery that syncs those
// directories, though it has nothing left to delete there.
func TestRecoverySyncsAnEarlierAttemptsDeletions(t *testing.T) {
	r := newSentRoot(t)
	b := growthAt(t, r, 2, func(k int) { r.pipelineAt(landedAt(k)) })
	files, _ := storage.LandedFiles(r.zone, b.Session)
	rem := retentionOf(r)
	rem.Stop = func(p retention.Point) error {
		if p.Phase == storage.RemovingDelete && p.Done == len(files) {
			return errCrash
		}
		return nil
	}
	if res := rem.Pass(retention.Input{}); len(res.Errors) == 0 {
		t.Fatal("the removal did not stop")
	}
	synced := map[string]bool{}
	rec := (&retention.Remover{Zone: r.zone, SyncDir: func(dir string) error {
		synced[dir] = true
		return nil
	}}).Recover()
	if len(rec.Errors) > 0 {
		t.Fatal(rec.Errors)
	}
	for _, lf := range files {
		if !synced[filepath.Dir(lf.Path)] {
			t.Fatalf("the recovery did not sync %s, where the stopped attempt deleted %s", filepath.Dir(lf.Path), filepath.Base(lf.Path))
		}
	}
}

// A receipt of a removed file stays. A deriver whose metrics.state a power
// loss took back reads it to derive the file as before; without it, a call
// that went on into a later file could be counted at once and never again.
func TestAReceiptStaysAfterARemoval(t *testing.T) {
	r := newSentRoot(t)
	b := growthAt(t, r, 2, func(k int) { r.pipelineAt(landedAt(k)) })
	before, _ := filepath.Glob(filepath.Join(r.zone.SessionDir(b.Session), "metrics", "*.json"))
	if len(before) == 0 {
		t.Fatal("the derivation wrote no receipt")
	}
	if res := retentionOf(r).Pass(retention.Input{}); res.Files == 0 || len(res.Errors) > 0 {
		t.Fatalf("the removal: %+v", res)
	}
	if res := retentionOf(r).Pass(retention.Input{}); len(res.Errors) > 0 {
		t.Fatal(res.Errors)
	}
	after, _ := filepath.Glob(filepath.Join(r.zone.SessionDir(b.Session), "metrics", "*.json"))
	if strings.Join(after, ",") != strings.Join(before, ",") {
		t.Fatalf("receipts %v after the removal, %v before", after, before)
	}
}

// A skeleton whose first sync fails stops the removal before any landed file
// is deleted. A later sync that succeeds does not say the data reached the
// disk, so the next pass writes that skeleton again, and only then goes on.
// A failed sync after the deletions keeps the intent, so the next pass
// finishes the removal.
func TestRetentionStopsWhenASkeletonMayBeLost(t *testing.T) {
	r := newSentRoot(t)
	b := growthAt(t, r, 2, func(k int) { r.pipelineAt(landedAt(k)) })
	rem := retentionOf(r)
	// A clock that moves on each reading, so a skeleton written again says
	// so in its header. Its inode cannot: Linux gives a new file the number
	// the deleted one had.
	ticks := 0
	rem.Now = func() time.Time {
		ticks++
		return later().Add(time.Duration(ticks) * time.Second)
	}
	failed := 0
	rem.SyncDir = func(string) error {
		if failed == 0 {
			failed++
			return errors.New("input/output error")
		}
		return nil
	}
	if res := rem.Pass(retention.Input{}); len(res.Errors) == 0 {
		t.Fatal("a failed sync did not stop the removal")
	}
	files, _ := storage.LandedFiles(r.zone, b.Session)
	for _, lf := range files {
		if lf.Skeleton {
			t.Fatalf("%s was deleted while its skeleton may be lost", storage.LandedPath(lf.Path))
		}
	}
	first := storage.SkeletonPath(files[0].Path)
	removedAt := func() string {
		t.Helper()
		f, err := sessiondata.Open(first)
		if err != nil {
			t.Fatalf("the skeleton is not there: %v", err)
		}
		defer f.Close()
		rd, err := sessiondata.NewReader(f)
		if err != nil || rd.Header().Removed == nil {
			t.Fatalf("the skeleton does not read: %v", err)
		}
		return rd.Header().Removed.At
	}
	before := removedAt()
	if rec := rem.Recover(); len(rec.Errors) > 0 || rec.Files == 0 {
		t.Fatalf("the next pass did not finish the removal: %+v", rec)
	}
	if after := removedAt(); after == before {
		t.Fatalf("the skeleton whose sync failed was kept, not written again: removed at %s both times", after)
	}

	// A sync that fails after the deletions keeps the intent.
	r2 := newSentRoot(t)
	b2 := growthAt(t, r2, 2, func(k int) { r2.pipelineAt(landedAt(k)) })
	rem2 := retentionOf(r2)
	deleting := false
	rem2.Stop = func(p retention.Point) error {
		deleting = deleting || p.Phase == storage.RemovingDelete
		return nil
	}
	rem2.SyncDir = func(string) error {
		if deleting {
			return errors.New("input/output error")
		}
		return nil
	}
	if res := rem2.Pass(retention.Input{}); len(res.Errors) == 0 {
		t.Fatal("a failed sync after the deletions did not stop the removal")
	}
	if intent, err := storage.LoadRemoving(r2.zone.RemovingPath(b2.Session)); err != nil || intent == nil || intent.Phase != storage.RemovingDelete {
		t.Fatalf("the intent after a failed sync of the deletions is %+v, %v", intent, err)
	}
	rem2.SyncDir, rem2.Stop = nil, nil
	if rec := rem2.Recover(); len(rec.Errors) > 0 {
		t.Fatal(rec.Errors)
	}
	if _, err := os.Stat(r2.zone.RemovingPath(b2.Session)); !os.IsNotExist(err) {
		t.Fatalf("the intent is left after the removal finished: %v", err)
	}
}

// Nothing lands into a session while a removal of it is in progress: a
// collector finds it busy, and lands into it once the removal is finished.
func TestNothingLandsDuringARemoval(t *testing.T) {
	r := newSentRoot(t)
	sc, err := scenario.Load(filepath.Join("..", "scenarios", "growth.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cps := append(sc.Checkpoints(), "")
	b := r.build(sc, scenario.Options{Through: cps[0]})
	r.pipelineAt(landedAt(0))
	files, _ := storage.LandedFiles(r.zone, b.Session)
	intent := &storage.Removing{Generation: 1, Through: files[len(files)-1].Seq, Phase: storage.RemovingSkeletons}
	if err := storage.SaveRemoving(r.zone.RemovingPath(b.Session), intent); err != nil {
		t.Fatal(err)
	}
	r.build(sc, scenario.Options{Through: cps[1]})
	col := claudecode.New(r.source, r.zone, 0)
	col.Now = func() time.Time { return landedAt(1) }
	st, err := col.CollectAll(nil)
	if err != nil {
		t.Fatal(err)
	}
	if after, _ := storage.LandedFiles(r.zone, b.Session); len(after) != len(files) || st.Busy == 0 {
		t.Fatalf("%d files after a collection during a removal, %d before; busy %d", len(after), len(files), st.Busy)
	}
	if rec := retentionOf(r).Recover(); len(rec.Errors) > 0 {
		t.Fatal(rec.Errors)
	}
	r.collectAt(landedAt(1))
	if after, _ := storage.LandedFiles(r.zone, b.Session); len(after) <= len(files) {
		t.Fatalf("nothing landed once the removal was finished: %d files", len(after))
	}
}

// A removal finishes over what a crash left of a skeleton: an empty
// reservation is written again. A skeleton that is not its file's stops the
// deletion, and the file is kept.
func TestRetentionChecksEachSkeleton(t *testing.T) {
	r := newSentRoot(t)
	b := growthAt(t, r, 2, func(k int) { r.pipelineAt(landedAt(k)) })
	files, _ := storage.LandedFiles(r.zone, b.Session)
	through := files[len(files)-1].Seq
	if err := os.WriteFile(storage.SkeletonPath(files[0].Path), nil, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := storage.SaveRemoving(r.zone.RemovingPath(b.Session), &storage.Removing{Generation: 1, Through: through, Phase: storage.RemovingSkeletons}); err != nil {
		t.Fatal(err)
	}
	if rec := retentionOf(r).Recover(); len(rec.Errors) > 0 || rec.Files != len(files) {
		t.Fatalf("a removal over an empty reservation: %+v", rec)
	}
	verified(t, r.zone, b.Session)

	r2 := newSentRoot(t)
	b2 := growthAt(t, r2, 2, func(k int) { r2.pipelineAt(landedAt(k)) })
	files2, _ := storage.LandedFiles(r2.zone, b2.Session)
	if err := os.WriteFile(storage.SkeletonPath(files2[0].Path), []byte("not a skeleton"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := storage.SaveRemoving(r2.zone.RemovingPath(b2.Session), &storage.Removing{Generation: 1, Through: files2[len(files2)-1].Seq, Phase: storage.RemovingDelete}); err != nil {
		t.Fatal(err)
	}
	if rec := retentionOf(r2).Recover(); len(rec.Errors) == 0 {
		t.Fatal("a removal deleted past a skeleton that is not its file's")
	}
	if _, err := os.Stat(files2[0].Path); err != nil {
		t.Fatalf("the file whose skeleton is not its own was deleted: %v", err)
	}
}

func otlpSent(r *sentRoot) (*otlp.Sent, error) { return otlp.LoadSent(r.dir) }

// collectAt lands with both adapters on the clock given.
func (r *sentRoot) collectAt(now time.Time) {
	r.t.Helper()
	clock := func() time.Time { return now }
	col := claudecode.New(r.source, r.zone, r.maxDelta)
	col.Now = clock
	st, err := col.CollectAll(nil)
	if err != nil {
		r.t.Fatal(err)
	}
	cc := claudecodechanges.New(r.changesRoot(), r.zone, r.maxDelta)
	cc.Now = clock
	cs, err := cc.CollectAll(nil)
	if err != nil {
		r.t.Fatal(err)
	}
	// The provider bodies last, as the pipeline collects them.
	pc := claudecodeprovider.New(filepath.Join(r.source, scenario.ProviderBodyDir), r.zone, r.maxDelta)
	pc.Now = clock
	ps, err := pc.CollectAll(func(string) claudecodeprovider.Verdict { return claudecodeprovider.Collect })
	if err != nil {
		r.t.Fatal(err)
	}
	if len(st.Errors) > 0 || len(cs.Errors) > 0 || len(ps.Errors) > 0 {
		r.t.Fatalf("collect: %v %v %v", st.Errors, cs.Errors, ps.Errors)
	}
}

// A skeleton keeps every call's usage and model, so metrics turned on only
// after a removal count the same tokens as over the landed files. It keeps
// no execution record's data, so no MCP call is counted from it; growth has
// none.
func TestMetricsDeriveFromSkeletons(t *testing.T) {
	plain, kept := newSentRoot(t), newSentRoot(t)
	for _, r := range []*sentRoot{plain, kept} {
		growthAt(t, r, 5, func(k int) {
			r.collectAt(landedAt(k))
			r.parse()
			r.push()
		})
	}
	rem := retentionOf(kept)
	rem.Metrics = false
	if res := rem.Pass(retention.Input{}); res.Files == 0 || len(res.Errors) > 0 {
		t.Fatalf("the removal: %+v", res)
	}
	if plain.derive() == 0 {
		t.Fatal("the landed files derive no request")
	}
	kept.derive()
	for _, s := range sessionsOf(t, plain.dir) {
		a, b := spoolOf(t, plain.dir, s), spoolOf(t, kept.dir, s)
		if len(a) == 0 || strings.Join(a, ",") != strings.Join(b, ",") {
			t.Fatalf("requests %v from the landed files, %v from the skeletons", a, b)
		}
		for _, name := range a {
			da, _ := os.ReadFile(filepath.Join(plain.dir, storage.SpoolDir, name))
			db, _ := os.ReadFile(filepath.Join(kept.dir, storage.SpoolDir, name))
			if !bytes.Equal(da, db) {
				t.Fatalf("%s differs when derived from the skeletons", name)
			}
		}
	}
}

// The page serves a conversation whose files were removed. One that found
// a landed file before the removal finds its skeleton after it, and a page
// built after the removal reads the skeletons, whose steps say their content
// was removed.
func TestThePageFollowsARemoval(t *testing.T) {
	// growth has steps on a part of a record and on a whole record of one
	// part; attachment-input and too-large have prompts with an attachment.
	// A step on a whole record of several parts is covered in internal/view.
	for _, name := range []string{"growth", "attachment-input", "too-large"} {
		t.Run(name, func(t *testing.T) { pageFollowsARemoval(t, name) })
	}
}

func pageFollowsARemoval(t *testing.T, name string) {
	r := newSentRoot(t)
	sc, err := scenario.Load(filepath.Join("..", "scenarios", name+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var b *scenario.Built
	for k, cp := range append(sc.Checkpoints(), "") {
		b = r.build(sc, scenario.Options{Through: cp})
		r.pipelineAt(landedAt(k))
	}
	h := view.New(r.zone, nil).Handler()
	get := func(path string) (int, string) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w.Code, w.Body.String()
	}
	record := "/api/c/" + b.Session + "/record/1/1"
	document := "/api/c/" + b.Session + "/view"
	if code, body := get(record); code != http.StatusOK || strings.Contains(body, `"removed"`) {
		t.Fatalf("before the removal the record is %d: %s", code, body)
	}
	if code, body := get(document); code != http.StatusOK || strings.Contains(body, `"state":"removed"`) {
		t.Fatalf("before the removal the document is %d", code)
	}
	if res := retentionOf(r).Pass(retention.Input{}); res.Files == 0 || len(res.Errors) > 0 {
		t.Fatalf("the removal: %+v", res)
	}
	if code, body := get(record); code != http.StatusOK || !strings.Contains(body, `"removed"`) {
		t.Fatalf("after the removal the record is %d: %s", code, body)
	}
	// The server kept the document it built before the removal, which ran
	// as another process would, telling it nothing. It builds it again.
	if code, body := get(document); code != http.StatusOK || !strings.Contains(body, `"state":"removed"`) {
		t.Fatalf("after the removal the document is %d and still says nothing was removed", code)
	}

	c, err := view.New(r.zone, nil).Load(b.Session)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := c.Build()
	if err != nil {
		t.Fatal(err)
	}
	// A step stands on a part of a record, or on a whole record, such as a
	// person's prompt; both say their content was removed.
	parts, wholes := 0, 0
	var walk func([]sessionview.Node)
	walk = func(nodes []sessionview.Node) {
		for _, n := range nodes {
			// A talk, a run and a model call are structure, and carry no
			// content state of their own.
			content := n.Ref != nil && n.Kind != model.KindTalk && n.Kind != model.KindRun && n.Kind != model.KindLLMCall
			if content && n.State != model.ContentRemoved {
				t.Fatalf("the step %s on %d/%d says %q, not removed", n.ID, n.Ref.Seq, n.Ref.Row, n.State)
			}
			switch {
			case content && n.Ref.Block == nil:
				wholes++
			case content:
				parts++
			}
			walk(n.Children)
		}
	}
	walk(doc.Talks)
	if parts == 0 || wholes == 0 {
		t.Fatalf("%d steps on a part and %d on a whole record; the page should have both", parts, wholes)
	}
}

// A file with no timed record is stamped with the session's newest record
// time. When it lands alone after a removal, that time is in a skeleton, and
// it is stamped as it would be with nothing removed.
func TestAnUntimedFileAfterARemovalTakesTheSessionsTime(t *testing.T) {
	plain, kept := newSentRoot(t), newSentRoot(t)
	var session string
	for _, r := range []*sentRoot{plain, kept} {
		b := growthAt(t, r, 5, func(k int) { r.pipelineAt(landedAt(k)) })
		session = b.Session
	}
	if res := retentionOf(kept).Pass(retention.Input{}); res.Files == 0 || len(res.Errors) > 0 {
		t.Fatalf("the removal: %+v", res)
	}
	stamps := map[*sentRoot]uint64{}
	for _, r := range []*sentRoot{plain, kept} {
		st, err := storage.LoadSessionState(r.zone.SessionStatePath(session), session)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.RecoverNextSeq(r.zone.SessionDir(session)); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(r.zone.StreamDir(session, "main"), storage.LandedName("meta", storage.Stamp(landedAt(9)), st.NextSeq))
		var buf bytes.Buffer
		w, err := sessiondata.NewWriter(&buf, &sessiondata.Header{H: 1, Schema: sessiondata.Schema, Seq: st.NextSeq,
			At: landedAt(9).Format(time.RFC3339Nano), Kind: sessiondata.KindAgentMeta, Adapter: "test/0", Dialect: "test/1",
			Src: "meta.json", Session: session, Stream: "main"})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Write(&sessiondata.Record{Ord: 1, Bytes: 2, Parts: []sessiondata.Part{{Kind: sessiondata.PartText, Text: "{}", State: model.ContentAvailable, Bytes: 2}}}); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, buf.Bytes(), 0o444); err != nil {
			t.Fatal(err)
		}
		r.rcv.Reset()
		r.push()
		name, _ := filepath.Rel(r.dir, path)
		for _, rec := range otlptest.Records(r.rcv.Requests()) {
			if otlptest.Attrs(rec.Attributes)["asz.file"] == filepath.ToSlash(name) {
				stamps[r] = rec.GetTimeUnixNano()
			}
		}
	}
	if stamps[plain] == 0 || stamps[kept] != stamps[plain] {
		t.Fatalf("an untimed file is stamped %d after a removal and %d without one", stamps[kept], stamps[plain])
	}
}

// The page keeps nothing it read of a session while a removal of it is in
// progress, since its files may be replaced from one read to the next, and
// keeps it again once the removal is finished.
func TestThePageKeepsNothingDuringARemoval(t *testing.T) {
	r := newSentRoot(t)
	b := growthAt(t, r, 2, func(k int) { r.pipelineAt(landedAt(k)) })
	srv := view.New(r.zone, nil)
	load := func() *view.Conversation {
		c, err := srv.Load(b.Session)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if first, again := load(), load(); first != again {
		t.Fatal("the page did not keep a conversation with no removal in progress")
	}
	files, _ := storage.LandedFiles(r.zone, b.Session)
	if err := storage.SaveRemoving(r.zone.RemovingPath(b.Session), &storage.Removing{Generation: 1, Through: files[0].Seq, Phase: storage.RemovingSkeletons}); err != nil {
		t.Fatal(err)
	}
	if first, again := load(), load(); first == again {
		t.Fatal("the page kept a conversation while a removal of it was in progress")
	}
	if rec := retentionOf(r).Recover(); len(rec.Errors) > 0 {
		t.Fatal(rec.Errors)
	}
	if first, again := load(), load(); first != again {
		t.Fatal("the page did not keep the conversation once the removal was finished")
	}
}

// Provider bodies landed after a removal are cut against the removed ones as
// with no removal: they copy the front of a removed tip, kept in the store,
// and cite removed pieces, kept from the first body that cited them. Every
// body that is not removed rebuilds, and the root verifies. provider-bodies-long
// is the scenario whose requests share a front long enough to be copied; that
// they are cut exactly as with no removal is TestRetentionChangesNothingSent.
func TestProviderBodiesAreCutAcrossARemoval(t *testing.T) {
	for _, name := range []string{"provider-bodies-long"} {
		t.Run(name, func(t *testing.T) {
			r := newSentRoot(t)
			r.maxDelta = budgetOf(t, name)
			sc, err := scenario.Load(filepath.Join("..", "scenarios", name+".yaml"))
			if err != nil {
				t.Fatal(err)
			}
			var b *scenario.Built
			for k, cp := range append(sc.Checkpoints(), "") {
				b = r.build(sc, scenario.Options{Through: cp})
				r.pipelineAt(landedAt(k))
				if cp != "" {
					if res := retentionOf(r).Pass(retention.Input{}); res.Files == 0 || len(res.Errors) > 0 {
						t.Fatalf("the removal at %s: %+v", cp, res)
					}
				}
			}
			files, err := storage.LandedFiles(r.zone, b.Session)
			if err != nil {
				t.Fatal(err)
			}
			removed := map[string]bool{}
			removedPieces := map[string]bool{}
			copies, cites, after := 0, 0, 0
			for _, lf := range files {
				if lf.Stream != "" || lf.RunID != "" {
					continue
				}
				f, err := sessiondata.Open(lf.Path)
				if err != nil {
					t.Fatal(err)
				}
				_, recs, err := sessiondata.All(f)
				f.Close()
				if err != nil {
					t.Fatal(err)
				}
				for _, rec := range recs {
					if lf.Skeleton {
						removed[rec.ID] = true
						for _, p := range rec.Parts {
							if p.State == model.ContentRemoved && p.SHA256 != "" {
								removedPieces[p.SHA256] = true
							}
						}
						continue
					}
					after++
					m, err := providerbody.ManifestOf(rec)
					if err != nil {
						continue
					}
					for _, seg := range m.Segments {
						if seg.Copy != nil && removed[seg.Copy.From] {
							copies++
						}
						if removedPieces[seg.Piece] {
							cites++
						}
					}
				}
			}
			if len(removed) == 0 || after == 0 {
				t.Fatalf("%d bodies removed and %d landed after; the test needs both", len(removed), after)
			}
			if copies == 0 {
				t.Fatal("no body landed after the removal copies a removed tip; the tip store is not reached")
			}
			if cites == 0 {
				t.Fatal("no body landed after the removal cites a removed piece; the piece store is not reached")
			}
			t.Logf("%d removed, %d after, %d copies of a removed tip, %d cited removed pieces", len(removed), after, copies, cites)
			verified(t, r.zone, b.Session)
			// Each removal keeps the tips of its own generation, and takes
			// those of the one before.
			generation, _, err := storage.RemovalGeneration(r.zone, b.Session)
			if err != nil {
				t.Fatal(err)
			}
			kept, err := r.zone.BodyStore(b.Session).Generations()
			if err != nil || len(kept) != 1 || kept[0] != generation {
				t.Fatalf("the store keeps tips of %v after removal %d, want only its own: %v", kept, generation, err)
			}
		})
	}
}

// A chain tip that cites a piece too large to land cannot be rebuilt, so no
// removal keeps it. A later removal that finds it still the tip, because only
// the transcript moved, passes over it as the first did rather than stop and
// hold the session's collectors off.
func TestATipTooLargeToRebuildDoesNotStopARemoval(t *testing.T) {
	r := newSentRoot(t)
	r.maxDelta = budgetOf(t, "provider-bodies-too-large")
	sc, err := scenario.Load(filepath.Join("..", "scenarios", "provider-bodies-too-large.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var b *scenario.Built
	for k, cp := range append(sc.Checkpoints(), "") {
		b = r.build(sc, scenario.Options{Through: cp})
		r.pipelineAt(landedAt(k))
		if cp == "" {
			continue
		}
		if k > 0 {
			// Nothing but the transcript landed since the first removal.
			files, _ := storage.LandedFiles(r.zone, b.Session)
			for _, lf := range files {
				if lf.Stream == "" && lf.RunID == "" && !lf.Skeleton {
					t.Fatalf("%s landed after the first removal; the test needs the tip unchanged", lf.Path)
				}
			}
		}
		if res := retentionOf(r).Pass(retention.Input{}); res.Files == 0 || len(res.Errors) > 0 {
			t.Fatalf("the removal at %s: %+v", cp, res)
		}
	}
	if g, err := r.zone.BodyStore(b.Session).Generations(); err != nil || len(g) != 0 {
		t.Fatalf("the store keeps tips of generations %v, and no tip can be rebuilt: %v", g, err)
	}
	verified(t, r.zone, b.Session)
}

// A removal finished again, after a power loss brought its intent back once
// a body had landed after it, keeps the pieces that body cites. The removal
// syncs the session directory once its intent is gone, so this needs that
// sync to have failed; reclaim does not count on it.
func TestARemovalFinishedAgainKeepsWhatALaterBodyCites(t *testing.T) {
	r := newSentRoot(t)
	r.maxDelta = budgetOf(t, "provider-bodies-long")
	sc, err := scenario.Load(filepath.Join("..", "scenarios", "provider-bodies-long.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cps := sc.Checkpoints()
	b := r.build(sc, scenario.Options{Through: cps[0]})
	r.pipelineAt(landedAt(0))
	rem := retentionOf(r)
	var intent []byte
	rem.Stop = func(p retention.Point) error {
		if p.Phase == storage.RemovingDelete && p.Done == 0 {
			intent, _ = os.ReadFile(r.zone.RemovingPath(b.Session))
		}
		return nil
	}
	if res := rem.Pass(retention.Input{}); res.Files == 0 || len(res.Errors) > 0 {
		t.Fatalf("the removal: %+v", res)
	}
	if len(intent) == 0 {
		t.Fatal("the removal's intent was not read")
	}
	r.build(sc, scenario.Options{Through: cps[1]})
	r.pipelineAt(landedAt(1))
	store := r.zone.BodyStore(b.Session)
	if kept, _ := os.ReadDir(store.PiecesDir()); len(kept) == 0 {
		t.Fatal("no body after the removal cites a removed piece; the test needs one")
	}
	if err := os.WriteFile(r.zone.RemovingPath(b.Session), intent, 0o644); err != nil {
		t.Fatal(err)
	}
	if res := retentionOf(r).Recover(); len(res.Errors) > 0 {
		t.Fatal(res.Errors)
	}
	if kept, _ := os.ReadDir(store.PiecesDir()); len(kept) == 0 {
		t.Fatal("finishing the removal again took the pieces a later body cites")
	}
	verified(t, r.zone, b.Session)
}

// The end of a removal is made durable: once the intent is deleted, the
// session directory is synced, and a failed sync is an error. Otherwise a
// power loss could bring the intent back after a body landed.
func TestTheEndOfARemovalIsSynced(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint("fail=", fail), func(t *testing.T) {
			r := newSentRoot(t)
			b := growthAt(t, r, 2, func(k int) { r.pipelineAt(landedAt(k)) })
			rem := retentionOf(r)
			seen, ended := false, false
			rem.SyncDir = func(dir string) error {
				_, err := os.Stat(r.zone.RemovingPath(b.Session))
				seen = seen || err == nil
				if dir == r.zone.SessionDir(b.Session) && seen && os.IsNotExist(err) {
					ended = true
					if fail {
						return errCrash
					}
				}
				return storage.SyncDir(dir)
			}
			res := rem.Pass(retention.Input{})
			if !ended {
				t.Fatal("the session directory was not synced once the intent was deleted")
			}
			if fail != (len(res.Errors) > 0) {
				t.Fatalf("a failed sync at the end of the removal: errors %v", res.Errors)
			}
		})
	}
}

// A tip that cannot be kept stops a removal before its intent is written, so
// the session keeps its files and its collectors go on landing into it.
// Here the store lost the tip a later body copies, as damage would.
func TestATipThatCannotBeKeptLeavesNoIntent(t *testing.T) {
	r := newSentRoot(t)
	r.maxDelta = budgetOf(t, "provider-bodies-long")
	sc, err := scenario.Load(filepath.Join("..", "scenarios", "provider-bodies-long.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cps := sc.Checkpoints()
	b := r.build(sc, scenario.Options{Through: cps[0]})
	r.pipelineAt(landedAt(0))
	if res := retentionOf(r).Pass(retention.Input{}); res.Files == 0 || len(res.Errors) > 0 {
		t.Fatalf("the first removal: %+v", res)
	}
	r.build(sc, scenario.Options{Through: cps[1]})
	r.pipelineAt(landedAt(1))
	if err := os.RemoveAll(filepath.Join(r.zone.SessionDir(b.Session), "provider", "tips")); err != nil {
		t.Fatal(err)
	}
	before, _ := storage.LandedFiles(r.zone, b.Session)
	if res := retentionOf(r).Pass(retention.Input{}); len(res.Errors) == 0 {
		t.Fatal("a removal went on without the tip a body is built from")
	}
	if _, err := os.Stat(r.zone.RemovingPath(b.Session)); !os.IsNotExist(err) {
		t.Fatalf("the stopped removal left an intent: %v", err)
	}
	r.build(sc, scenario.Options{})
	r.collectAt(landedAt(2))
	if after, _ := storage.LandedFiles(r.zone, b.Session); len(after) <= len(before) {
		t.Fatalf("nothing landed after the stopped removal: %d files, %d before", len(after), len(before))
	}
}

// A scenario written as Session Data directly makes no scenario marker, so
// retention can remove from it, and a later checkpoint is then written into a
// root with skeletons. The writer holds them and reads the store as the
// adapter does, so every body after a removal lands as the same record as
// with nothing removed, and the root verifies.
func TestAnSDBuildContinuesAfterARemoval(t *testing.T) {
	const name = "provider-bodies-long"
	sc, err := scenario.Load(filepath.Join("..", "scenarios", name+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	plain, kept := newSentRoot(t), newSentRoot(t)
	opts := scenario.Options{At: time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC), MaxDelta: budgetOf(t, name)}
	var b *scenario.Built
	for _, cp := range append(sc.Checkpoints(), "") {
		opts.Through = cp
		for _, r := range []*sentRoot{plain, kept} {
			if b, err = scenario.Build(sc, scenario.FormatSD, r.dir, opts); err != nil {
				t.Fatal(err)
			}
			r.derive()
			r.parse()
			r.push()
		}
		if cp != "" {
			if res := retentionOf(kept).Pass(retention.Input{}); res.Files == 0 || len(res.Errors) > 0 {
				t.Fatalf("the removal at %s: %+v", cp, res)
			}
		}
	}
	files, err := storage.LandedFiles(kept.zone, b.Session)
	if err != nil {
		t.Fatal(err)
	}
	after := 0
	for _, lf := range files {
		if lf.Stream != "" || lf.RunID != "" || lf.Skeleton {
			continue
		}
		rel, _ := filepath.Rel(kept.dir, lf.Path)
		want, err := os.ReadFile(filepath.Join(plain.dir, rel))
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(lf.Path); !bytes.Equal(got, want) {
			t.Fatalf("%s lands differently after a removal", rel)
		}
		after++
	}
	if after == 0 {
		t.Fatal("no provider body landed after a removal")
	}
	verified(t, kept.zone, b.Session)
}

// servedStore is the store a page holds from the files endpoint's answers:
// the bodies kept of removed records, by id, and the removed pieces, by
// digest.
type servedStore struct{ bodies, pieces map[string][]byte }

func (s servedStore) Tip(id, _ string) ([]byte, error) { return served(s.bodies, id) }
func (s servedStore) Piece(sha string) ([]byte, error) { return served(s.pieces, sha) }

func served(m map[string][]byte, key string) ([]byte, error) {
	if b, ok := m[key]; ok {
		return b, nil
	}
	return nil, fs.ErrNotExist
}

// The page serves a removed provider file as its skeleton, with the bytes the
// store kept of it, so a reader that rebuilds bodies from the files endpoint
// alone, as the renderer does, rebuilds every body that landed after the
// removal, the ones that copy a removed tip or cite a removed piece included.
//
// With ASZ_PROMPT_FIXTURE set to a directory, the endpoint's answer and the
// document are written there: the fixture of Horizon's prompt store tests.
func TestThePageServesWhatABodyAfterARemovalNeeds(t *testing.T) {
	r, b := removedThenContinued(t)
	h := view.New(r.zone, nil).Handler()
	get := func(path string) []byte {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		return w.Body.Bytes()
	}
	document := get("/api/c/" + b.Session + "/view")
	var doc sessionview.Conversation
	if err := json.Unmarshal(document, &doc); err != nil {
		t.Fatal(err)
	}
	q := url.Values{"session": {b.Session}}
	removedFiles := 0
	for _, f := range doc.Files {
		if f.Kind == string(sessiondata.KindProviderBody) && f.Seq != nil {
			q.Add("seq", fmt.Sprint(*f.Seq))
			if f.State == model.ContentRemoved {
				removedFiles++
			}
		}
	}
	if removedFiles == 0 {
		t.Fatal("no provider file was removed; the test needs some")
	}
	answer := get("/api/c/" + b.Session + "/files?" + q.Encode())
	var files []struct {
		Seq   uint64                       `json:"seq"`
		File  string                       `json:"file"`
		Bytes []byte                       `json:"bytes"`
		Kept  map[string]map[string][]byte `json:"kept"`
	}
	if err := json.Unmarshal(answer, &files); err != nil {
		t.Fatal(err)
	}
	if out := os.Getenv("ASZ_PROMPT_FIXTURE"); out != "" {
		if err := os.WriteFile(filepath.Join(out, "provider-bodies-removed.files.json"), answer, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, "provider-bodies-removed.json"), document, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	store := servedStore{bodies: map[string][]byte{}, pieces: map[string][]byte{}}
	held := providerbody.NewSession()
	held.Use(store)
	type at struct {
		rec     *sessiondata.Record
		removed bool
	}
	var all []at
	for _, f := range files {
		for id, body := range f.Kept["bodies"] {
			store.bodies[id] = body
		}
		for sha, piece := range f.Kept["pieces"] {
			store.pieces[sha] = piece
		}
		_, recs, err := sessiondata.All(bytes.NewReader(f.Bytes))
		if err != nil {
			t.Fatalf("%s does not read as Session Data: %v", f.File, err)
		}
		for _, rec := range recs {
			if err := held.Add(rec); err != nil && !errors.Is(err, providerbody.ErrRepeat) {
				t.Fatalf("%s: %v", rec.ID, err)
			}
			all = append(all, at{rec, sessiondata.IsSkeleton(f.File)})
		}
	}
	if len(store.bodies) == 0 || len(store.pieces) == 0 {
		t.Fatalf("the answer kept %d bodies and %d pieces; the test needs both", len(store.bodies), len(store.pieces))
	}
	after, copies := 0, 0
	for _, a := range all {
		if a.removed {
			continue
		}
		after++
		body, err := held.Body(a.rec.ID)
		if err != nil {
			t.Fatalf("%s, landed after the removal, does not rebuild from what the page served: %v", a.rec.ID, err)
		}
		m, _ := providerbody.ManifestOf(a.rec)
		if providerbody.Digest(body) != m.SHA256 {
			t.Fatalf("%s rebuilds to other bytes", a.rec.ID)
		}
		for _, seg := range m.Segments {
			if seg.Copy != nil {
				if _, kept := store.bodies[seg.Copy.From]; kept {
					copies++
				}
			}
		}
	}
	if after == 0 || copies == 0 {
		t.Fatalf("%d bodies after the removal, %d copying a kept tip; the test needs both", after, copies)
	}
}

// removedThenContinued is provider-bodies-long removed at its first
// checkpoint and collected to its end: provider files removed, and bodies
// landed after the removal that copy a removed tip and cite removed pieces.
func removedThenContinued(t *testing.T) (*sentRoot, *scenario.Built) {
	t.Helper()
	const name = "provider-bodies-long"
	r := newSentRoot(t)
	r.maxDelta = budgetOf(t, name)
	sc, err := scenario.Load(filepath.Join("..", "scenarios", name+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cps := sc.Checkpoints()
	b := r.build(sc, scenario.Options{Through: cps[0]})
	r.pipelineAt(landedAt(0))
	if res := retentionOf(r).Pass(retention.Input{}); res.Files == 0 || len(res.Errors) > 0 {
		t.Fatalf("the removal: %+v", res)
	}
	r.build(sc, scenario.Options{})
	r.pipelineAt(landedAt(1))
	return r, b
}

// The files route serves a file that does not fit its budget on its own when
// it is first in an answer, so every file can be had, and leaves the rest of
// that answer for a later ask. A landed file a removal deletes after the
// route found it is served as its skeleton.
func TestTheFilesRouteServesEveryFile(t *testing.T) {
	r, b := removedThenContinued(t)
	files, err := storage.LandedFiles(r.zone, b.Session)
	if err != nil {
		t.Fatal(err)
	}
	var seqs []string
	var landed storage.LandedFile
	for _, lf := range files {
		if lf.Stream == "" && lf.RunID == "" {
			seqs = append(seqs, fmt.Sprint(lf.Seq))
			if !lf.Skeleton {
				landed = lf
			}
		}
	}
	if len(seqs) < 2 || landed.Path == "" {
		t.Fatalf("%d provider files, a landed one %q; the test needs both kinds", len(seqs), landed.Path)
	}
	ask := func(h http.Handler, seqs ...string) []map[string]any {
		t.Helper()
		w := httptest.NewRecorder()
		q := url.Values{"session": {b.Session}, "seq": seqs}
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/c/"+b.Session+"/files?"+q.Encode(), nil))
		if w.Code != http.StatusOK {
			t.Fatalf("files: %d %s", w.Code, w.Body.String())
		}
		var out []map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	srv := view.New(r.zone, nil)
	srv.MaxFileBytes = 1
	h := srv.Handler()
	if got := ask(h, seqs...); len(got) != 1 || fmt.Sprint(got[0]["seq"]) != seqs[0] {
		t.Fatalf("an answer over a budget no file fits holds %d files, want the first alone", len(got))
	}
	for _, seq := range seqs {
		if got := ask(h, seq); len(got) != 1 {
			t.Fatalf("seq %s asked alone is not served", seq)
		}
	}

	// The page found the landed file before a removal deleted it.
	h = view.New(r.zone, nil).Handler()
	if got := ask(h, fmt.Sprint(landed.Seq)); len(got) != 1 || strings.HasSuffix(fmt.Sprint(got[0]["file"]), sessiondata.SkeletonExt) {
		t.Fatalf("the landed file before the removal: %v", got)
	}
	data, err := os.ReadFile(landed.Path)
	if err != nil {
		t.Fatal(err)
	}
	var sk bytes.Buffer
	if err := skeleton.Write(&sk, data, sessiondata.Removed{Generation: 2}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(storage.SkeletonPath(landed.Path), sk.Bytes(), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(landed.Path); err != nil {
		t.Fatal(err)
	}
	if got := ask(h, fmt.Sprint(landed.Seq)); len(got) != 1 || !strings.HasSuffix(fmt.Sprint(got[0]["file"]), sessiondata.SkeletonExt) {
		t.Fatalf("a file deleted after the page found it is served as %v, want its skeleton", got)
	}
}
