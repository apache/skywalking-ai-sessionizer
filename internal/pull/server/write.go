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

package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// writeChunk bounds one application write: a slow reader holds one write of
// at most this many bytes, never a whole answer.
const writeChunk = 32 << 10

// writeWait is how long one write, header write or flush may take. Each has
// its own deadline; progress does not extend it.
const writeWait = 60 * time.Second

// bodyWait is how long a request body may take to arrive.
const bodyWait = 30 * time.Second

// errAborted is a write that failed or ran out of time: the answer stops
// there, and the handler lets go of what it holds.
var errAborted = errors.New("server: the answer could not be written")

// boundedWriter owns the deadlines and the flushing of every answer on a
// protected address. Each header write, each write of at most 32 kibibytes
// and each flush gets a deadline of its own, and every write is flushed, so
// a stalled reader stops one write rather than holding a handler for ever.
// Once a write fails, every later one fails at once.
type boundedWriter struct {
	w      http.ResponseWriter
	rc     *http.ResponseController
	now    func() time.Time
	header bool
	failed error
}

func newBoundedWriter(w http.ResponseWriter, now func() time.Time) *boundedWriter {
	return &boundedWriter{w: w, rc: http.NewResponseController(w), now: now}
}

func (b *boundedWriter) Header() http.Header { return b.w.Header() }

// Unwrap gives the writer beneath, which only this writer sets deadlines on.
func (b *boundedWriter) Unwrap() http.ResponseWriter { return b.w }

func (b *boundedWriter) deadline() error {
	if b.failed != nil {
		return b.failed
	}
	if err := b.rc.SetWriteDeadline(b.now().Add(writeWait)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		b.failed = fmt.Errorf("%w: %w", errAborted, err)
		return b.failed
	}
	return nil
}

func (b *boundedWriter) WriteHeader(code int) {
	if b.header || b.deadline() != nil {
		return
	}
	b.header = true
	b.w.WriteHeader(code)
}

func (b *boundedWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := min(len(p), writeChunk)
		if err := b.deadline(); err != nil {
			return written, err
		}
		b.header = true
		m, err := b.w.Write(p[:n])
		written += m
		if err != nil {
			b.failed = fmt.Errorf("%w: %w", errAborted, err)
			return written, b.failed
		}
		if err := b.FlushError(); err != nil {
			return written, err
		}
		p = p[n:]
	}
	return written, nil
}

// FlushError flushes under a deadline of its own.
func (b *boundedWriter) FlushError() error {
	if err := b.deadline(); err != nil {
		return err
	}
	if err := b.rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		b.failed = fmt.Errorf("%w: %w", errAborted, err)
		return b.failed
	}
	// A flush sends the header too, as 200 when none was written.
	b.header = true
	return nil
}

// Flush is FlushError for a caller that only knows http.Flusher.
func (b *boundedWriter) Flush() { _ = b.FlushError() }

// finish flushes once more when the handler is done, an empty answer
// included, before the handler's locks are let go.
func (b *boundedWriter) finish() error {
	if !b.header {
		b.WriteHeader(http.StatusOK)
	}
	return b.FlushError()
}

// noStore keeps every answer on a protected address out of every cache: it
// sets Cache-Control: no-store over whatever the handler set, at the moment
// the header is committed, by a write, a header write or a flush alike.
type noStore struct {
	w         http.ResponseWriter
	committed bool
}

func (n *noStore) Header() http.Header { return n.w.Header() }

func (n *noStore) Unwrap() http.ResponseWriter { return n.w }

func (n *noStore) commit() {
	if !n.committed {
		n.committed = true
		n.w.Header().Set("Cache-Control", "no-store")
		n.w.Header().Del("Expires")
		n.w.Header().Del("Pragma")
	}
}

func (n *noStore) WriteHeader(code int) {
	n.commit()
	n.w.WriteHeader(code)
}

func (n *noStore) Write(p []byte) (int, error) {
	n.commit()
	return n.w.Write(p)
}

func (n *noStore) FlushError() error {
	n.commit()
	return http.NewResponseController(n.w).Flush()
}

func (n *noStore) Flush() { _ = n.FlushError() }

// Problem is an error answer: application/problem+json with a type, a
// title, a detail and the status it was sent with. Typed problems add the
// members their type names.
type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Status int    `json:"status"`
	// The members a freeze or archive failure carries.
	Conversation string `json:"conversation,omitempty"`
	Freeze       string `json:"freeze,omitempty"`
	Snapshot     string `json:"snapshot,omitempty"`
	Archive      string `json:"archive,omitempty"`
	Attempt      uint64 `json:"attempt,omitempty"`
}

// problem is a plain problem of a status.
func problem(status int, detail string) Problem {
	return Problem{Type: "about:blank", Title: http.StatusText(status), Detail: detail, Status: status}
}

// writeProblem answers with a problem. Extra headers, such as Allow or
// Retry-After, are set by the caller first.
func writeProblem(w http.ResponseWriter, p Problem) error {
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_, err = w.Write(append(body, '\n'))
	return err
}

// writeJSON answers with one JSON value.
func writeJSON(w http.ResponseWriter, status int, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, err = w.Write(append(body, '\n'))
	return err
}
