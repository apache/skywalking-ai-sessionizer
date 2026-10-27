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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/client"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
)

// pulledPrefix opens the identifier the page gives a pulled conversation,
// remote:NAME:CONVERSATION. A conversation's own identifier never holds a
// colon, so the two never meet.
const pulledPrefix = "remote:"

// pulledPage serves, beside a root's own conversations, the ones asz pull
// copied into it: each listed once, from its newest unexpired snapshot,
// its title opened by [remote, NAME], and read from that snapshot. Every
// request holds a lease on the snapshot it reads, and lets it go when it is
// answered, so a page left open never keeps a snapshot from expiring. Each
// read is checked as a snapshot's page checks it; one that fails is that
// request's failure, and the page goes on.
type pulledPage struct {
	local *client.Local
	cfg   *config.Config

	mu    sync.Mutex
	views map[client.Copy]*pulledView
}

// pulledView is the view of one pulled conversation's snapshot, kept while
// that snapshot is still its newest and its marker still says the same.
type pulledView struct {
	path   string
	marker storage.SnapshotCopy
	srv    *view.Server
}

func newPulledPage(cfg *config.Config, root string) (*pulledPage, error) {
	ttl, err := cfg.RemoteSnapshotTTLDuration()
	if err != nil {
		return nil, err
	}
	return &pulledPage{cfg: cfg, views: map[client.Copy]*pulledView{},
		local: &client.Local{Root: root, TTL: ttl, Log: func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "asz: "+format+"\n", args...)
		}}}, nil
}

// fingerprint is what decides the pulled rows of the list, read without
// admitting anything: every pulled conversation, each of its tags and
// whether its time to live has passed, and the publication in progress. It
// changes when a pull adds a conversation or a snapshot, a publication
// becomes readable, a snapshot expires, or a sweep removes one. A page with
// no pulled conversations has none.
func (p *pulledPage) fingerprint() string {
	if p == nil {
		return ""
	}
	copies, err := p.local.Copies()
	if err != nil {
		return "! " + err.Error()
	}
	now := time.Now()
	var b strings.Builder
	for _, c := range copies {
		fmt.Fprintf(&b, "%s/%s", c.Remote, c.Conversation)
		tags, err := p.local.Tags(c.Remote, c.Conversation)
		if err != nil {
			fmt.Fprintf(&b, " ! %v;", err)
			continue
		}
		for _, tag := range tags {
			expired := "-"
			if t, err := storage.ParseTag(tag); err == nil && !now.Before(t.Add(p.local.TTL)) {
				expired = "x"
			}
			fmt.Fprintf(&b, " %s%s", tag, expired)
		}
		if dir, err := storage.ConversationDir(p.local.Root, c.Remote, c.Conversation); err == nil {
			outer, err := storage.ReadOuterCopy(dir, c.Remote, c.Conversation)
			switch {
			case err != nil:
				fmt.Fprintf(&b, " ! %v", err)
			case outer.Publishing != nil:
				fmt.Fprintf(&b, " publishing %s", outer.Publishing.Tag)
			}
		}
		b.WriteString(";")
	}
	return b.String()
}

// any says whether the root holds a pulled conversation to serve.
func (p *pulledPage) any() bool {
	copies, err := p.local.Copies()
	return err == nil && len(copies) > 0
}

// handler serves page, and the pulled conversations beside its own.
func (p *pulledPage) handler(page http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/conversations":
			p.list(w, r, page)
		case strings.HasPrefix(r.URL.Path, "/api/c/"+pulledPrefix):
			p.conversation(w, r)
		default:
			page.ServeHTTP(w, r)
		}
	})
}

// captured is an answer kept to be read before it is sent on.
type captured struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func newCaptured() *captured { return &captured{header: http.Header{}, code: http.StatusOK} }

func (c *captured) Header() http.Header         { return c.header }
func (c *captured) WriteHeader(code int)        { c.code = code }
func (c *captured) Write(b []byte) (int, error) { return c.body.Write(b) }

// list answers the root's own list with a row for each pulled conversation
// after it. A pulled conversation whose snapshot does not read is said, and
// left out of this list.
func (p *pulledPage) list(w http.ResponseWriter, r *http.Request, page http.Handler) {
	own := newCaptured()
	page.ServeHTTP(own, r)
	var rows []json.RawMessage
	if own.code != http.StatusOK || json.Unmarshal(own.body.Bytes(), &rows) != nil {
		for k, v := range own.header {
			w.Header()[k] = v
		}
		w.WriteHeader(own.code)
		_, _ = w.Write(own.body.Bytes())
		return
	}
	copies, err := p.local.Copies()
	if err != nil {
		fmt.Fprintf(os.Stderr, "asz: the pulled conversations: %v\n", err)
	}
	// Every snapshot a row was read from stays held until the list is
	// answered.
	var held []func()
	defer func() {
		for _, release := range held {
			release()
		}
	}()
	for _, c := range copies {
		row, release, err := p.row(r, c)
		if release != nil {
			held = append(held, release)
		}
		switch {
		case errors.Is(err, client.ErrNoSnapshot):
		case err != nil:
			fmt.Fprintf(os.Stderr, "asz: %s/%s: %v\n", c.Remote, c.Conversation, err)
		default:
			rows = append(rows, row)
		}
	}
	if rows == nil {
		rows = []json.RawMessage{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rows)
}

// row is a pulled conversation's row, as its snapshot's own page lists it,
// under the page's identifier for it and its title opened by its remote. It
// hands back the release of the snapshot it read, for the caller to call
// once its answer is sent.
func (p *pulledPage) row(r *http.Request, c client.Copy) (json.RawMessage, func(), error) {
	var out json.RawMessage
	release, err := p.admitted(r, c, func(srv *view.Server) error {
		answer := newCaptured()
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "/api/conversations", nil)
		if err != nil {
			return err
		}
		srv.Handler().ServeHTTP(answer, req)
		var rows []map[string]any
		if answer.code != http.StatusOK || json.Unmarshal(answer.body.Bytes(), &rows) != nil || len(rows) != 1 {
			return fmt.Errorf("its snapshot's page answered %d", answer.code)
		}
		row := rows[0]
		title, _ := row["title"].(string)
		row["id"] = pulledPrefix + c.Remote + ":" + c.Conversation
		row["title"] = "[remote, " + c.Remote + "] " + title
		out, err = json.Marshal(row)
		return err
	})
	return out, release, err
}

// conversation answers a pulled conversation's request from its snapshot's
// page, as a request for that conversation there.
func (p *pulledPage) conversation(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/c/"+pulledPrefix)
	id, tail, _ := strings.Cut(rest, "/")
	remote, conversation, ok := strings.Cut(id, ":")
	if !ok || storage.CheckCopyNames(remote, conversation) != nil {
		http.Error(w, "no such pulled conversation", http.StatusNotFound)
		return
	}
	c := client.Copy{Remote: remote, Conversation: conversation}
	release, err := p.admitted(r, c, func(srv *view.Server) error {
		req := r.Clone(r.Context())
		req.URL.Path = "/api/c/" + conversation + "/" + tail
		req.URL.RawPath = ""
		srv.Handler().ServeHTTP(w, req)
		return nil
	})
	if release != nil {
		release()
	}
	switch {
	case errors.Is(err, client.ErrNoSnapshot):
		http.Error(w, "no unexpired snapshot of "+remote+"/"+conversation+" is here", http.StatusNotFound)
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// admitted admits the request to the newest unexpired snapshot of c and
// runs fn with that snapshot's page. It returns the snapshot's release,
// which the caller calls once what fn read is answered.
func (p *pulledPage) admitted(r *http.Request, c client.Copy, fn func(*view.Server) error) (func(), error) {
	a, err := p.local.AdmitNewest(r.Context(), c.Remote, c.Conversation)
	if err != nil {
		return nil, err
	}
	release := func() {
		if err := a.Release(r.Context()); err != nil {
			fmt.Fprintf(os.Stderr, "asz: %s/%s:%s: %v\n", c.Remote, c.Conversation, a.Root.Tag, err)
		}
	}
	srv, err := p.viewOf(c, a)
	if err != nil {
		return release, err
	}
	return release, fn(srv)
}

// viewOf is the page of the snapshot a admits, made once for it: what it
// loaded is kept while that snapshot stays the conversation's newest. A read
// of it that fails forgets it, so the next request loads it again.
func (p *pulledPage) viewOf(c client.Copy, a *storage.Admission) (*view.Server, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// A marker that changed since the page was made is read again, and
	// checked against the snapshot's manifest as a new page is.
	if v, ok := p.views[c]; ok && v.path == a.Root.Path && sameMarker(v.marker, a.Marker) {
		return v.srv, nil
	}
	srv := view.NewWithGlossaries(storage.NewZone(a.Root.Path), glossaries())
	if err := srv.SetHide(p.cfg.View.Hide, p.cfg.View.HideOverride); err != nil {
		return nil, err
	}
	path := a.Root.Path
	snap, err := snapshotViewOf(a, func(err error) {
		fmt.Fprintf(os.Stderr, "asz: %s/%s:%s: %v\n", c.Remote, c.Conversation, a.Root.Tag, err)
		p.mu.Lock()
		defer p.mu.Unlock()
		if v, ok := p.views[c]; ok && v.path == path {
			delete(p.views, c)
		}
	})
	if err != nil {
		return nil, err
	}
	srv.SetSnapshot(snap)
	p.views[c] = &pulledView{path: path, marker: a.Marker, srv: srv}
	return srv, nil
}

// sameMarker says whether two readings of a snapshot's marker name the same
// evidence.
func sameMarker(a, b storage.SnapshotCopy) bool {
	return a.Session == b.Session && a.Round == b.Round && a.Digest == b.Digest && a.ManifestDigest == b.ManifestDigest &&
		a.PublishedAt.Equal(b.PublishedAt)
}
