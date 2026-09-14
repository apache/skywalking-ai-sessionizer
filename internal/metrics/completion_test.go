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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/metrics"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

func TestFinishedCallAcrossThreeLandedFiles(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	cut := call{id: "three-files", model: "m", at: base, frags: 3, in: 4, out: 60}
	a, b, c := cut, cut, cut
	a.only = 1
	b.skip, b.only = 1, 2
	c.skip = 2
	land(t, z, "s1", "a1", 1, []call{a})
	land(t, z, "s1", "a1", 2, []call{b})
	land(t, z, "s1", "a1", 3, []call{c})
	d := deriver(z, base.Add(time.Hour), metrics.Options{})
	st, err := d.Pass(nil)
	if err != nil || len(st.Errors) > 0 {
		t.Fatal(err, st)
	}
	got, _ := points(t, z)
	if n := total(got["subagent/output/m#s1"]); n != 60 {
		t.Fatalf("finished three-fragment call produced %v output tokens, want 60; stats=%+v", n, st)
	}
}

func TestCallFinishingAfterGraceIsCounted(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	cut := call{id: "late-finish", model: "m", at: base, frags: 2, in: 4, out: 60}
	a, b := cut, cut
	a.only = 1
	b.skip = 1
	land(t, z, "s1", "a1", 1, []call{a})
	d := deriver(z, base.Add(time.Hour), metrics.Options{})
	if st, err := d.Pass(nil); err != nil || len(st.Errors) > 0 {
		t.Fatal(err, st)
	}
	land(t, z, "s1", "a1", 2, []call{b})
	st, err := d.Pass(nil)
	if err != nil || len(st.Errors) > 0 {
		t.Fatal(err, st)
	}
	got, _ := points(t, z)
	if n := total(got["subagent/output/m#s1"]); n != 60 {
		t.Fatalf("call finishing after grace produced %v output tokens, want 60; stats=%+v", n, st)
	}
	// A resumed transcript can repeat the final fragment in another file.
	land(t, z, "s1", "a1", 3, []call{b})
	st, err = d.Pass(nil)
	if err != nil || len(st.Errors) > 0 || st.Requests != 0 {
		t.Fatalf("replayed completion was counted again: %v %+v", err, st)
	}
	got, _ = points(t, z)
	if n := total(got["subagent/output/m#s1"]); n != 60 {
		t.Fatalf("replayed completion produced %v output tokens, want 60", n)
	}
}

// Every main-stream fragment carries finished, so stopping at the first
// finished fragment would put the call in the wrong minute.
func TestMainCallUsesTheFinalFragmentAcrossFiles(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	cut := call{id: "main-split", model: "m", at: base.Add(time.Minute - 200*time.Millisecond), frags: 3, in: 4, out: 60}
	for i := 0; i < 3; i++ {
		part := cut
		part.skip, part.only = i, i+1
		land(t, z, "s1", "main", uint64(i+1), []call{part})
	}
	d := deriver(z, base.Add(time.Hour), metrics.Options{})
	st, err := d.Pass(nil)
	if err != nil || len(st.Errors) > 0 {
		t.Fatal(err, st)
	}
	got, _ := points(t, z)
	ps := got["main/output/m#s1"]
	if len(ps) != 1 || total(ps) != 60 || !ps[0].start.Equal(base.Add(time.Minute)) {
		t.Fatalf("main call must count once in its final fragment's minute: %+v", ps)
	}
}

// A later fragment restarts the grace. The first file can already be old
// while the last file is still being followed by more fragments.
func TestContinuationWaitsForTheLatestFilesGrace(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	cut := call{id: "continuing", model: "m", at: base, frags: 3, in: 4, out: 60}
	first, second := cut, cut
	first.only = 1
	second.skip, second.only = 1, 2
	land(t, z, "s1", "main", 1, []call{first})
	latest := land(t, z, "s1", "main", 2, []call{second})
	aged(t, latest, base)
	d := deriver(z, base.Add(time.Minute), metrics.Options{})
	st, err := d.Pass(nil)
	if err != nil || len(st.Errors) > 0 || st.Deferred != 2 || st.Requests != 0 {
		t.Fatalf("the latest fragment has not passed its grace: %v %+v", err, st)
	}
	if len(st.PendingSessions) != 1 || st.PendingSessions[0] != "s1" {
		t.Fatalf("deferred files must request one session retry: %+v", st.PendingSessions)
	}
}

func TestDeferredHistoryKeepsItsLookbackAcrossRestart(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	p := land(t, z, "s1", "main", 1, []call{
		{id: "old", model: "m", at: base.Add(-48 * time.Hour), frags: 1, in: 100, out: 100},
		{id: "recent", model: "m", at: base, frags: 1, in: 4, out: 60},
	})
	aged(t, p, base)
	d := deriver(z, base.Add(time.Minute), metrics.Options{Lookback: 24 * time.Hour})
	st, err := d.Pass(nil)
	if err != nil || len(st.Errors) > 0 || st.Deferred != 1 {
		t.Fatalf("first pass must defer the file: %v %+v", err, st)
	}
	d = deriver(z, base.Add(time.Hour), metrics.Options{Lookback: 24 * time.Hour})
	st, err = d.Pass(nil)
	if err != nil || len(st.Errors) > 0 || st.Files != 1 {
		t.Fatalf("restarted pass must derive the file: %v %+v", err, st)
	}
	got, _ := points(t, z)
	if n := total(got["main/output/m#s1"]); n != 60 {
		t.Fatalf("retry bypassed the first pass's look-back: got %v output tokens, want 60", n)
	}
	land(t, z, "s1", "main", 2, []call{{id: "old", model: "m", at: base.Add(-48 * time.Hour), frags: 1, in: 100, out: 100}})
	st, err = d.Pass(nil)
	if err != nil || len(st.Errors) > 0 || st.Requests != 0 {
		t.Fatalf("replaying skipped history must not bypass its look-back: %v %+v", err, st)
	}
}

// The spool can survive a failed state save, then a continuation can land
// before retry. The existing request and its counted calls must stay paired.
func TestStateWriteFailurePreservesCallAccountingAcrossContinuation(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	cut := call{id: "continued", model: "m", at: base, frags: 2, in: 4, out: 60}
	first, rest := cut, cut
	first.only = 1
	rest.skip = 1
	land(t, z, "s1", "a1", 1, []call{
		{id: "finished", model: "m", at: base.Add(-time.Minute), frags: 1, in: 1, out: 20}, first,
	})
	d := deriver(z, base.Add(time.Hour), metrics.Options{Grace: -1})
	statePath := filepath.Join(storage.NewSpool(z).Dir(), metrics.StateFile)
	// With no look-back or grace, the clock is read only for the final
	// state save. Make its destination a directory after the spool write.
	d.Now = func() time.Time {
		if err := os.Mkdir(statePath, 0o755); err != nil {
			t.Fatal(err)
		}
		return base.Add(time.Hour)
	}
	if _, err := d.Pass(nil); err == nil {
		t.Fatal("state save unexpectedly succeeded")
	}
	got, _ := points(t, z)
	if n := total(got["subagent/output/m#s1"]); n != 20 {
		t.Fatalf("the first request must survive the failed save: got %v tokens", n)
	}
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	land(t, z, "s1", "a1", 2, []call{rest})
	d = deriver(z, base.Add(time.Hour), metrics.Options{Grace: -1})
	st, err := d.Pass(nil)
	if err != nil || len(st.Errors) > 0 {
		t.Fatal(err, st)
	}
	got, _ = points(t, z)
	if n := total(got["subagent/output/m#s1"]); n != 80 {
		t.Fatalf("recovery lost or repeated a call: output=%v, want 20 + 60", n)
	}
	st, err = d.Pass(nil)
	if err != nil || len(st.Errors) > 0 || st.Requests != 0 {
		t.Fatalf("a completed retry wrote another request: %v %+v", err, st)
	}
}

func TestDerivationErrorStopsTheSessionBeforeLaterReceipts(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	cut := call{id: "continued", model: "m", at: base, frags: 2, in: 4, out: 60}
	first, rest := cut, cut
	first.only, rest.skip = 1, 1
	land(t, z, "s1", "a1", 1, []call{first})
	land(t, z, "s1", "a1", 2, []call{rest})
	receipts := filepath.Join(z.SessionDir("s1"), "metrics")
	if err := os.MkdirAll(receipts, 0o755); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(receipts, "000001.json")
	if err := os.WriteFile(broken, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := deriver(z, base.Add(time.Hour), metrics.Options{Grace: -1})
	st, err := d.Pass(nil)
	if err != nil || len(st.Errors) != 1 || st.Requests != 0 || len(st.PendingSessions) != 1 {
		t.Fatalf("an unreadable receipt must stop its session and request a retry: %v %+v", err, st)
	}
	if _, err := os.Stat(filepath.Join(receipts, "000002.json")); !os.IsNotExist(err) {
		t.Fatalf("a later file was decided without its predecessor: %v", err)
	}
	if err := os.Remove(broken); err != nil {
		t.Fatal(err)
	}
	st, err = d.Pass(nil)
	if err != nil || len(st.Errors) > 0 {
		t.Fatal(err, st)
	}
	got, _ := points(t, z)
	if n := total(got["subagent/output/m#s1"]); n != 60 {
		t.Fatalf("receipt recovery counted one completed call as %v tokens, want 60", n)
	}
}

// A receipt may be durable while the spool cannot accept its request. No
// later receipt may count that call again before the first one is retried.
func TestFailedSpoolWriteDoesNotRecordLaterReceipts(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	cut := call{id: "continued", model: "m", at: base, frags: 2, in: 4, out: 60}
	first, rest := cut, cut
	first.only, rest.skip = 1, 1
	land(t, z, "s1", "a1", 1, []call{first})
	land(t, z, "s1", "a1", 2, []call{rest})
	spool := storage.NewSpool(z).Dir()
	if err := os.Mkdir(spool, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(spool, 0o755) })
	if probe, err := os.CreateTemp(spool, "probe"); err == nil {
		_ = probe.Close()
		_ = os.Remove(probe.Name())
		t.Skip("this platform or user can write through read-only directory permissions")
	}
	d := deriver(z, base.Add(time.Hour), metrics.Options{Grace: -1})
	if _, err := d.Pass(nil); err == nil {
		t.Fatal("the read-only spool unexpectedly accepted the pass")
	}
	if _, err := os.Stat(filepath.Join(z.SessionDir("s1"), "metrics", "000002.json")); !os.IsNotExist(err) {
		t.Fatalf("a later receipt was published after the spool failed: %v", err)
	}
	if err := os.Chmod(spool, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := d.Pass(nil)
	if err != nil || len(st.Errors) > 0 {
		t.Fatal(err, st)
	}
	got, _ := points(t, z)
	if n := total(got["subagent/output/m#s1"]); n != 60 {
		t.Fatalf("spool recovery counted one completed call as %v tokens, want 60", n)
	}
}
