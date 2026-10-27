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
	"net/http"
	"strings"
	"sync"
	"time"
)

// Guard protects every route on one address with tokens. Each request is
// answered through a bounded writer, under no-store, and authenticated
// before anything reads its body. Its method is checked next, then it takes
// one of its token's request slots, or is answered 429.
type Guard struct {
	keeper *Keeper
	limit  int
	now    func() time.Time
	// Log, when set, is told of every pull request refused before a pull
	// handler sees it, for the audit.
	Log func(format string, args ...any)

	mu    sync.Mutex
	slots map[string]int
}

// Holds says whether a caller's token is still in the tokens file.
func (g *Guard) Holds(c Caller) bool { return g.keeper.Holds(c.Hash) }

type finishKey struct{}

// finishAnswer sends a request's answer whole: its header committed under
// no-store, and its last flush, an empty answer's included. A handler calls
// it before it lets go of what it holds, so nothing is let go while the
// answer is still on its way. The guard calls it again, which does nothing.
func finishAnswer(r *http.Request) {
	if finish, ok := r.Context().Value(finishKey{}).(func() error); ok {
		_ = finish()
	}
}

// NewGuard protects an address with the tokens keeper holds, each running at
// most limit requests at once.
func NewGuard(keeper *Keeper, limit int, now func() time.Time) *Guard {
	if now == nil {
		now = time.Now
	}
	return &Guard{keeper: keeper, limit: limit, now: now, slots: map[string]int{}}
}

// Caller is the token a request was authenticated with.
type Caller struct {
	// Name is the token's name, which the audit log gives.
	Name string
	// Hash is the token's SHA-256, which a lease is bound to.
	Hash string
}

type callerKey struct{}

// CallerOf is the token a request came with, when a guard admitted it.
func CallerOf(r *http.Request) (Caller, bool) {
	c, ok := r.Context().Value(callerKey{}).(Caller)
	return c, ok
}

// Protect wraps the handler of a whole address. A request reaches page only
// as GET; pull, when set, gets every request under /pull/ and checks its own
// methods, then calls Admit. Without pull, those routes answer 404.
func (g *Guard) Protect(page, pull http.Handler) http.Handler {
	return http.HandlerFunc(func(raw http.ResponseWriter, r *http.Request) {
		bounded := newBoundedWriter(raw, g.now)
		w := &noStore{w: bounded}
		// The last flush goes through no-store too, so an answer the
		// handler left empty is kept out of caches as well.
		finish := sync.OnceValue(func() error {
			w.commit()
			return bounded.finish()
		})
		defer func() { _ = finish() }()
		r = r.WithContext(context.WithValue(r.Context(), finishKey{}, finish))
		pullRoute := r.URL.Path == "/pull" || strings.HasPrefix(r.URL.Path, "/pull/")
		caller, ok := g.authenticate(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="asz"`)
			_ = writeProblem(w, problem(http.StatusUnauthorized, "a token is required: send Authorization: Bearer <token>"))
			if pullRoute && g.Log != nil {
				g.Log("pull: - %s %s status %d", r.Method, r.URL.EscapedPath(), http.StatusUnauthorized)
			}
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), callerKey{}, caller))
		switch {
		case r.URL.Path == "*" || r.RequestURI == "*":
			w.Header().Set("Allow", "GET, POST")
			_ = writeProblem(w, problem(http.StatusMethodNotAllowed, "OPTIONS * is not served"))
		case pullRoute:
			if pull == nil {
				_ = writeProblem(w, problem(http.StatusNotFound, "this address serves no pulls"))
				return
			}
			pull.ServeHTTP(w, r)
		default:
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", http.MethodGet)
				_ = writeProblem(w, problem(http.StatusMethodNotAllowed, r.Method+" is not served here"))
				return
			}
			release, ok := g.Admit(caller)
			if !ok {
				_ = writeProblem(w, tooMany())
				return
			}
			// The slot is let go once the answer is sent, its last flush
			// included.
			defer func() {
				_ = finish()
				release()
			}()
			page.ServeHTTP(w, r)
		}
	})
}

// authenticate takes the one Authorization header a request carries, as
// Bearer and a token, and finds the token. Nothing of the body is read.
func (g *Guard) authenticate(r *http.Request) (Caller, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return Caller{}, false
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return Caller{}, false
	}
	name, hash, err := g.keeper.Lookup(token)
	if err != nil {
		return Caller{}, false
	}
	return Caller{Name: name, Hash: hash}, true
}

// Admit takes one of the caller's request slots without waiting. It
// returns false when the token already runs as many requests as it may.
func (g *Guard) Admit(c Caller) (release func(), ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.slots[c.Hash] >= g.limit {
		return nil, false
	}
	g.slots[c.Hash]++
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.slots[c.Hash]--; g.slots[c.Hash] == 0 {
				delete(g.slots, c.Hash)
			}
		})
	}, true
}

func tooMany() Problem {
	return problem(http.StatusTooManyRequests, "this token already runs as many requests as it may; try again")
}
