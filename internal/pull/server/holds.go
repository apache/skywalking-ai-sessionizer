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
	"context"
	"fmt"
	"net/http"
	"sync"
)

// requestHolds are what a pull request holds until its answer is sent: its
// hold on a snapshot and the renewal of its use. They are let go in the
// reverse of their taking, after the answer's last flush, so no removal or
// expiry acts on a snapshot while an answer from it is still on its way.
type requestHolds struct {
	mu  sync.Mutex
	fns []func()
}

type holdsKey struct{}

// holdUntilSent keeps fn until the request's answer is sent, then calls it.
func holdUntilSent(r *http.Request, fn func()) {
	h := r.Context().Value(holdsKey{}).(*requestHolds)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fns = append(h.fns, fn)
}

func (h *requestHolds) release() {
	h.mu.Lock()
	fns := h.fns
	h.fns = nil
	h.mu.Unlock()
	for i := len(fns) - 1; i >= 0; i-- {
		fns[i]()
	}
}

// audit is what a pull request's one log line says beyond its token, route
// and status: how many files it named, the range it asked for and the
// bytes sent of it.
type audit struct {
	// snapshot and archive are what the request was answered with, where
	// its path did not name them: a ready manifest's snapshot, a new
	// archive's identifier.
	snapshot, archive string
	files             int
	ranged            bool
	start, end        uint64
	sent              int64
}

type auditKey struct{}

func auditOf(r *http.Request) *audit { return r.Context().Value(auditKey{}).(*audit) }

func (a *audit) String() string {
	out := fmt.Sprintf("files %d", a.files)
	if a.ranged {
		out += fmt.Sprintf(" range %d-%d sent %d", a.start, a.end, a.sent)
	}
	return out
}

// withRequestState gives a pull request its holds and its audit record.
func withRequestState(r *http.Request) (*http.Request, *requestHolds, *audit) {
	h, a := &requestHolds{}, &audit{}
	ctx := context.WithValue(context.WithValue(r.Context(), holdsKey{}, h), auditKey{}, a)
	return r.WithContext(ctx), h, a
}
