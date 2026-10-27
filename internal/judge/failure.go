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
	"sync"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// gateWait bounds how long a read waits for its chunk's gate. The gate is
// held only to make, remove or check markers, never over a read.
const gateWait = 30 * time.Second

// Strict is what every navigation over one pulled snapshot shares: the
// positions its document cites, and the first read of evidence that failed.
// A read that fails is never passed over there: whatever answer it was part
// of is not accepted, and the run fails.
type Strict struct {
	cited map[[2]uint64]bool
	held  func(seq uint64) bool
	mu    sync.Mutex
	err   error
}

// NewStrict is the strict reading of a document's evidence. Every position
// the document cites is a record the evidence must hold: one missing is
// damage, where a position an evaluator names that nothing cites is an
// ordinary wrong argument. held says which sequences the snapshot holds, so
// a sequence it does not hold, named by an evaluator, is refused unread.
func NewStrict(doc *sessionview.Conversation, held func(seq uint64) bool) *Strict {
	s := &Strict{cited: map[[2]uint64]bool{}, held: held}
	cite := func(r *sessionflow.Ref) {
		if r != nil {
			s.cited[[2]uint64{r.Seq, r.Row}] = true
		}
	}
	var walk func(x *sessionview.Node)
	walk = func(x *sessionview.Node) {
		cite(x.Ref)
		for i := range x.Refs {
			cite(&x.Refs[i])
		}
		for i := range x.Children {
			walk(&x.Children[i])
		}
	}
	for i := range doc.Talks {
		walk(&doc.Talks[i])
	}
	for i := range doc.Loose {
		walk(&doc.Loose[i])
	}
	for i := range doc.WorkspaceChanges {
		cite(&doc.WorkspaceChanges[i].Ref)
	}
	for i := range doc.ToolExecutions {
		cite(&doc.ToolExecutions[i].Ref)
	}
	return s
}

// Failed is the first read of evidence that failed, or nil.
func (s *Strict) Failed() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *Strict) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = err
	}
}

// fatal says whether a read's error is damage to the evidence rather than
// an ordinary answer to a wrong question.
func (s *Strict) fatal(seq, row uint64, err error) bool {
	return errors.Is(err, view.ErrEvidence) || errors.Is(err, view.ErrNoRecord) && s.cited[[2]uint64{seq, row}]
}

// attempt is one chunk attempt's failure marker, which every read the
// attempt's navigation makes is held to, one read at a time.
type attempt struct {
	marker string
	mu     sync.Mutex
}

// read makes one record read for a navigation over a snapshot. Under an
// attempt's marker, the read is made only once a pending marker was made, and
// the marker is removed once the read ended well; a fatal read leaves it.
func (s *Strict) read(a *attempt, seq, row uint64, read RecordReader) (*sessiondata.Record, error) {
	if !s.held(seq) && !s.cited[[2]uint64{seq, row}] {
		return nil, fmt.Errorf("%w: no landed file with sequence %d", view.ErrNoRecord, seq)
	}
	var rec *sessiondata.Record
	run := func() error {
		var err error
		rec, err = read(seq, row)
		return err
	}
	fatal := func(err error) bool { return s.fatal(seq, row, err) }
	var err error
	if a == nil {
		err = run()
		if err != nil && fatal(err) {
			s.fail(err)
		}
		return rec, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	err = ReadUnder(a.marker, run, fatal)
	if err != nil && fatal(err) {
		s.fail(err)
	}
	return rec, err
}

// ErrNotRead is a read of evidence that was not made, because its pending
// marker could not be made first. It is an ordinary tool failure: there is no
// evidence error to lose.
var ErrNotRead = errors.New("the record was not read")

// ReadUnder makes one read of a pulled snapshot's evidence under a chunk
// attempt's failure marker. A pending marker is made first, exclusively and
// without following a link, under the chunk's gate; if it cannot be, nothing
// is read. A read that ended well, or failed in an ordinary way, removes its
// marker under the gate. A fatal read marks it fatal in place and leaves it,
// and so does any failure to remove it, which the parent reads the same way.
func ReadUnder(marker string, read func() error, fatal func(error) bool) error {
	gate, err := storage.FailureGate(marker)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotRead, err)
	}
	var m *storage.Marker
	err = withGate(gate, func() error {
		var err error
		m, err = storage.CreateMarker(marker)
		return err
	})
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotRead, err)
	}
	err = read()
	if err != nil && fatal(err) {
		// What the report says is only a help: the marker fails the
		// chunk whether or not it could be written.
		_ = m.Fatal("fatal: " + err.Error() + "\n")
		return err
	}
	if withGate(gate, m.Remove) != nil {
		// The marker stays, and fails the chunk as a pending one.
		_ = m.Close()
	}
	return err
}

// withGate runs fn holding a chunk's gate.
func withGate(gate string, fn func() error) error {
	ctx, cancel := context.WithTimeout(context.Background(), gateWait)
	defer cancel()
	l, err := storage.LockFailureGate(ctx, gate)
	if err != nil {
		return err
	}
	return errors.Join(fn(), l.Unlock())
}

// CheckMarkers checks a chunk's issued markers under its gate, which is the
// one cutoff for every attempt of the chunk: any marker, pending or fatal,
// and any failure to look, is an error.
func CheckMarkers(markers []string) error {
	if len(markers) == 0 {
		return nil
	}
	gate, err := storage.FailureGate(markers[0])
	if err != nil {
		return err
	}
	return withGate(gate, func() error {
		for _, m := range markers {
			if g, err := storage.FailureGate(m); err != nil || g != gate {
				return fmt.Errorf("judge: %s is not a marker of the chunk %s gates", m, gate)
			}
			present, err := storage.MarkerPresent(m)
			if err != nil {
				return fmt.Errorf("judge: the failure marker %s: %w", m, err)
			}
			if present {
				return fmt.Errorf("judge: a read of the snapshot's evidence failed or did not finish: %s", m)
			}
		}
		return nil
	})
}

// Strictly makes a navigation read its records strictly, each under
// marker when it is set: a forked navigation of a run's chunk attempt, or an
// inherited navigation server. With no marker, the reads are the run's own,
// or an independent server's.
func (n *Nav) Strictly(s *Strict, marker string) {
	n.strict = s
	n.attempt = nil
	if marker != "" {
		n.attempt = &attempt{marker: marker}
	}
}

// Failed is the first read of evidence a strict navigation saw fail.
func (n *Nav) Failed() error {
	if n.strict == nil {
		return nil
	}
	return n.strict.Failed()
}
