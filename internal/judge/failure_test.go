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

package judge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// markerIn is an attempt's marker in a new chunk directory, and its gate.
func markerIn(t *testing.T) (marker, gate string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "2-targets", "c001", "a1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker = filepath.Join(dir, storage.FailurePrefix+"r")
	gate, err := storage.FailureGate(marker)
	if err != nil {
		t.Fatal(err)
	}
	return marker, gate
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

var errDamaged = fmt.Errorf("%w: a landed file ends early", view.ErrEvidence)

func fatalIfEvidence(err error) bool { return errors.Is(err, view.ErrEvidence) }

// A read is made only once its pending marker is there, and its marker is
// gone once it ended well or failed in an ordinary way. A fatal read leaves
// its marker, saying why.
func TestAReadIsMadeUnderAPendingMarker(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		stays  bool
		report string
	}{
		"a read that ends well": {nil, false, ""},
		"an ordinary failure":   {fmt.Errorf("%w: no row 9", view.ErrNoRecord), false, ""},
		"a fatal read":          {errDamaged, true, "fatal: " + errDamaged.Error() + "\n"},
	} {
		marker, _ := markerIn(t)
		read := false
		err := ReadUnder(marker, func() error {
			read = true
			if b, err := os.ReadFile(marker); err != nil || len(b) != 0 {
				t.Errorf("%s: no pending marker during the read: %q %v", name, b, err)
			}
			return tc.err
		}, fatalIfEvidence)
		if !read || !errors.Is(err, tc.err) && tc.err != nil || tc.err == nil && err != nil {
			t.Errorf("%s: read %v, %v", name, read, err)
		}
		b, rerr := os.ReadFile(marker)
		if exists(marker) != tc.stays || tc.stays && (rerr != nil || string(b) != tc.report) {
			t.Errorf("%s: the marker is there %v, says %q", name, exists(marker), b)
		}
	}
}

// Nothing is read when the pending marker cannot be made first: an existing
// marker, which is left as it was, or a gate that cannot be taken. The
// failure is an ordinary one, since no evidence was read.
func TestNoMarkerNoRead(t *testing.T) {
	marker, _ := markerIn(t)
	if err := os.WriteFile(marker, []byte("fatal: earlier\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := ReadUnder(marker, func() error { t.Error("read with a marker already there"); return nil }, fatalIfEvidence)
	if !errors.Is(err, ErrNotRead) {
		t.Fatalf("over an existing marker: %v", err)
	}
	if b, _ := os.ReadFile(marker); string(b) != "fatal: earlier\n" {
		t.Fatalf("a later call changed the marker: %q", b)
	}

	marker, gate := markerIn(t)
	if err := os.Mkdir(gate, 0o755); err != nil {
		t.Fatal(err)
	}
	err = ReadUnder(marker, func() error { t.Error("read without its gate"); return nil }, fatalIfEvidence)
	if !errors.Is(err, ErrNotRead) || exists(marker) {
		t.Fatalf("without its gate: %v, marker %v", err, exists(marker))
	}
	if err := ReadUnder(filepath.Join(filepath.Dir(marker), "mcp-calls.jsonl"), func() error {
		t.Error("read under a name that is no marker")
		return nil
	}, fatalIfEvidence); !errors.Is(err, ErrNotRead) {
		t.Fatalf("a name that is no marker: %v", err)
	}
}

// A marker that cannot be removed stays, and fails the chunk as a pending
// one.
func TestAMarkerThatCannotBeRemovedStays(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a marker a read holds open cannot be deleted on Windows")
	}
	marker, _ := markerIn(t)
	err := ReadUnder(marker, func() error {
		// The name now holds another file, which the read did not make.
		if err := os.Remove(marker); err != nil {
			return err
		}
		return os.WriteFile(marker, nil, 0o600)
	}, fatalIfEvidence)
	if err != nil {
		t.Fatal(err)
	}
	if !exists(marker) {
		t.Fatal("the marker was removed though it was not the read's own")
	}
	if err := CheckMarkers([]string{marker}); err == nil {
		t.Fatal("a marker left by a failed removal passed the check")
	}
}

// The gate is the cutoff: while a parent checks, a read waits before its
// marker and reads nothing; a read that starts after the check is the
// child's own affair.
func TestTheGateOrdersReadsAgainstTheCheck(t *testing.T) {
	marker, gate := markerIn(t)
	held, err := storage.LockFailureGate(context.Background(), gate)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- ReadUnder(marker, func() error { close(started); return nil }, fatalIfEvidence)
	}()
	select {
	case <-started:
		t.Fatal("a read began while the gate was held")
	case <-time.After(100 * time.Millisecond):
	}
	if exists(marker) {
		t.Fatal("a marker was made while the gate was held")
	}
	if err := held.Unlock(); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// The check looks at the chunk's issued markers only, under its gate, and
// fails on anything it finds or cannot rule out.
func TestTheCheckFailsOnAnyMarker(t *testing.T) {
	marker, _ := markerIn(t)
	retry := filepath.Join(filepath.Dir(filepath.Dir(marker)), "a2", filepath.Base(marker))
	if err := os.Mkdir(filepath.Dir(retry), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CheckMarkers([]string{marker, retry}); err != nil {
		t.Fatalf("no markers: %v", err)
	}
	if err := os.WriteFile(retry, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckMarkers([]string{marker, retry}); err == nil {
		t.Fatal("a retry's pending marker passed")
	}
	if err := CheckMarkers([]string{marker}); err != nil {
		t.Fatalf("a marker the check was not given failed it: %v", err)
	}
	other, _ := markerIn(t)
	if err := CheckMarkers([]string{marker, other}); err == nil {
		t.Fatal("a marker of another chunk was checked under this chunk's gate")
	}
	if err := CheckMarkers([]string{filepath.Join(filepath.Dir(marker), "gone", "a3", filepath.Base(marker))}); err == nil {
		t.Fatal("a marker that could not be looked at passed")
	}
}

// strictOver is a strict reading of a document that cites {1, 1}, over a
// snapshot that holds sequences 1 and 2.
func strictOver() *Strict {
	doc := &sessionview.Conversation{Talks: []sessionview.Node{{ID: "t1", Ref: &sessionflow.Ref{Seq: 1, Row: 1}}}}
	return NewStrict(doc, func(seq uint64) bool { return seq == 1 || seq == 2 })
}

// A fatal read fails the run whether or not its marker could be written: the
// strict reading keeps it, and the parent checks both.
func TestAFatalReadFailsWithoutItsReport(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a marker a read holds open cannot be deleted on Windows")
	}
	marker, _ := markerIn(t)
	s := strictOver()
	_, err := s.read(&attempt{marker: marker}, 1, 1, func(uint64, uint64) (*sessiondata.Record, error) {
		// The marker is gone before its report: as good as a report that
		// could not be written.
		_ = os.Remove(marker)
		return nil, errDamaged
	})
	if !errors.Is(err, view.ErrEvidence) {
		t.Fatal(err)
	}
	if exists(marker) {
		t.Fatal("a marker was made again")
	}
	if CheckMarkers([]string{marker}) != nil || !errors.Is(s.Failed(), view.ErrEvidence) {
		t.Fatalf("the failure is lost: %v", s.Failed())
	}
}

// A record the document cites that is not there is damage. A position an
// evaluator names that nothing cites is a wrong argument, and a sequence
// the snapshot does not hold is refused before any read or marker.
func TestAMissingCitedRecordIsDamage(t *testing.T) {
	missing := func(seq, row uint64) (*sessiondata.Record, error) {
		return nil, fmt.Errorf("%w: no row %d in sequence %d", view.ErrNoRecord, row, seq)
	}
	marker, gate := markerIn(t)
	s := strictOver()
	if _, err := s.read(&attempt{marker: marker}, 3, 1, func(uint64, uint64) (*sessiondata.Record, error) {
		t.Error("a sequence the snapshot does not hold was read")
		return nil, nil
	}); !errors.Is(err, view.ErrNoRecord) || exists(marker) || exists(gate) || s.Failed() != nil {
		t.Fatalf("an unheld sequence: %v, marker %v, gate %v, failed %v", err, exists(marker), exists(gate), s.Failed())
	}
	if _, err := s.read(&attempt{marker: marker}, 2, 9, missing); !errors.Is(err, view.ErrNoRecord) || exists(marker) || s.Failed() != nil {
		t.Fatalf("an uncited row: %v, marker %v, failed %v", err, exists(marker), s.Failed())
	}
	if _, err := s.read(&attempt{marker: marker}, 1, 1, missing); !errors.Is(err, view.ErrNoRecord) || !exists(marker) || s.Failed() == nil {
		t.Fatalf("a cited row: %v, marker %v, failed %v", err, exists(marker), s.Failed())
	}
	if b, _ := os.ReadFile(marker); !strings.HasPrefix(string(b), "fatal: ") {
		t.Fatalf("the marker says %q", b)
	}
}

// Wrong tools, wrong arguments and a spent budget are answered before any
// read: no gate and no marker is made for them.
func TestOrdinaryFailuresMakeNoMarker(t *testing.T) {
	marker, gate := markerIn(t)
	doc := &sessionview.Conversation{Talks: []sessionview.Node{{ID: "t1", Ref: &sessionflow.Ref{Seq: 1, Row: 1}}}}
	n := NewNav(doc, &Ledger{}, func(uint64, uint64) (*sessiondata.Record, error) {
		t.Error("an ordinary failure read a record")
		return nil, nil
	})
	n.Strictly(strictOver(), marker)
	for name, call := range map[string]struct {
		tool string
		args string
	}{
		"no such tool":       {"nope", `{}`},
		"broken arguments":   {"record", `{"seq":`},
		"an unheld sequence": {"record", `{"seq": 7, "row": 1}`},
	} {
		if _, err := n.Call(call.tool, []byte(call.args)); err == nil {
			t.Errorf("%s answered", name)
		}
	}
	n.ResetBudget(1)
	n.counts.spent = 1
	if _, err := n.Call("record", []byte(`{"seq": 1, "row": 1}`)); !errors.Is(err, ErrBudgetSpent) {
		t.Errorf("a spent budget: %v", err)
	}
	if exists(marker) || exists(gate) {
		t.Fatalf("an ordinary failure made a marker %v or a gate %v", exists(marker), exists(gate))
	}
}

// A step's whole text is never searched past a record that could not be
// read: on a snapshot the search fails, where elsewhere the record is
// passed over.
func TestASearchFailsOnARecordItCannotRead(t *testing.T) {
	doc := &sessionview.Conversation{Talks: []sessionview.Node{{ID: "t1", Ref: &sessionflow.Ref{Seq: 1, Row: 1}}}}
	broken := func(uint64, uint64) (*sessiondata.Record, error) { return nil, errDamaged }
	n := NewNav(doc, &Ledger{}, broken)
	if _, err := n.Call("search", []byte(`{"query": "x", "node": "t1"}`)); err != nil {
		t.Fatalf("an ordinary root: %v", err)
	}
	n.Strictly(strictOver(), "")
	if _, err := n.Call("search", []byte(`{"query": "x", "node": "t1"}`)); !errors.Is(err, view.ErrEvidence) {
		t.Fatalf("a snapshot: %v", err)
	}
}

// A fork answers another call, and reads under no attempt's marker until
// it is given its own.
func TestAForkReadsUnderNoMarkerOfItsParent(t *testing.T) {
	marker, gate := markerIn(t)
	doc := &sessionview.Conversation{Talks: []sessionview.Node{{ID: "t1", Ref: &sessionflow.Ref{Seq: 1, Row: 1}}}}
	n := NewNav(doc, &Ledger{}, func(uint64, uint64) (*sessiondata.Record, error) { return &sessiondata.Record{}, nil })
	n.Strictly(strictOver(), marker)
	if _, err := n.Fork().Call("record", []byte(`{"seq": 1, "row": 1}`)); err != nil {
		t.Fatal(err)
	}
	if exists(gate) {
		t.Fatal("a fork read under its parent's marker")
	}
}

// A navigation server on its own stops at a read of damaged evidence: it
// answers that call with the error, and fails rather than serve on.
func TestAnIndependentServerStopsAtDamage(t *testing.T) {
	doc := &sessionview.Conversation{Talks: []sessionview.Node{{ID: "t1", Ref: &sessionflow.Ref{Seq: 1, Row: 1}}}}
	n := NewNav(doc, &Ledger{}, func(uint64, uint64) (*sessiondata.Record, error) { return nil, errDamaged })
	n.Strictly(strictOver(), "")
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"record","arguments":{"seq":1,"row":1}}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"card","arguments":{}}}` + "\n")
	var out strings.Builder
	err := ServeMCP(in, &out, n, nil)
	if !errors.Is(err, view.ErrEvidence) {
		t.Fatalf("the server ended with %v", err)
	}
	if lines := strings.Count(out.String(), "\n"); lines != 1 || !strings.Contains(out.String(), `"isError":true`) {
		t.Fatalf("the server answered %d calls: %s", lines, out.String())
	}
}

// A record a document cites only through a tool execution is cited all the
// same: missing, it is damage, never an ordinary wrong argument.
func TestAnExecutionsRecordIsCited(t *testing.T) {
	marker, _ := markerIn(t)
	doc := &sessionview.Conversation{ToolExecutions: []sessionview.ToolExecution{{Ref: sessionflow.Ref{Seq: 2, Row: 1}}}}
	s := NewStrict(doc, func(uint64) bool { return true })
	_, err := s.read(&attempt{marker: marker}, 2, 1, func(uint64, uint64) (*sessiondata.Record, error) {
		return nil, fmt.Errorf("%w: no row 1 in sequence 2", view.ErrNoRecord)
	})
	if !errors.Is(err, view.ErrNoRecord) || !exists(marker) || s.Failed() == nil {
		t.Fatalf("%v, marker %v, failed %v", err, exists(marker), s.Failed())
	}
}
