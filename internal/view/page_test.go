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

package view_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
)

// The page draws a conversation with the renderer embedded from a pinned
// Horizon commit: every file the page loads is served, with its type, and
// the page names each of them and the two API routes the renderer needs.
func TestPageServesTheEmbeddedRenderer(t *testing.T) {
	h := view.New(storage.NewZone(t.TempDir()), nil).Handler()
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	assets := map[string]string{
		view.AssetPrefix + "conversation-view.js":         "text/javascript",
		view.AssetPrefix + "conversation-view.css":        "text/css",
		view.AssetPrefix + "host-shell/horizon-theme.css": "text/css",
		view.AssetPrefix + "host-shell/themes.json":       "application/json",
	}
	// The two fonts are under the SIL Open Font License, which the ASF keeps
	// out of source releases, so a build from the source package has neither
	// and the page falls back to system fonts. A checkout has both and serves
	// them.
	for _, font := range []string{"inter-latin-wght-normal.woff2", "jetbrains-mono-latin-wght-normal.woff2"} {
		path := view.AssetPrefix + "host-shell/fonts/" + font
		if _, err := os.Stat(filepath.Join("conversation-view", "host-shell", "fonts", font)); err == nil {
			assets[path] = "font/woff2"
		} else if rec := get(path); rec.Code != http.StatusNotFound {
			t.Fatalf("%s: %d without the font file, want 404", path, rec.Code)
		}
	}
	for path, ctype := range assets {
		rec := get(path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, ctype) {
			t.Fatalf("%s: served as %q, want %s", path, got, ctype)
		}
		if rec.Header().Get("Cache-Control") == "" {
			t.Fatalf("%s: a renderer file is served without a cache header", path)
		}
	}
	for _, path := range []string{view.AssetPrefix, view.AssetPrefix + "HORIZON_COMMIT", view.AssetPrefix + "missing.js"} {
		if rec := get(path); rec.Code != http.StatusNotFound {
			t.Fatalf("%s: %d, want 404", path, rec.Code)
		}
	}

	page := get("/c/some-conversation")
	if page.Code != http.StatusOK || !strings.HasPrefix(page.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("page: %d %s", page.Code, page.Header().Get("Content-Type"))
	}
	body := page.Body.String()
	for _, want := range []string{
		view.AssetPrefix + "conversation-view.js",
		view.AssetPrefix + "conversation-view.css",
		view.AssetPrefix + "host-shell/horizon-theme.css",
		view.AssetPrefix + "host-shell/themes.json",
		"mountConversationView", "isSupportedDocument", "/view", "/record/", "/api/glossary",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("the page does not name %q", want)
		}
	}
	if strings.Contains(body, "fonts.googleapis.com") {
		t.Fatal("the page loads a font from the network; the host shell carries the fonts")
	}

	// The pin names one Horizon commit, and the list page follows the same theme.
	if c := view.HorizonCommit(); !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(c) {
		t.Fatalf("HORIZON_COMMIT names %q, not a commit", c)
	}
	index := get("/").Body.String()
	if !strings.Contains(index, view.AssetPrefix+"host-shell/horizon-theme.css") || strings.Contains(index, "fonts.googleapis.com") {
		t.Fatal("the list page does not use the host shell's theme")
	}
	for _, page := range []string{index, body} {
		if !strings.Contains(page, `id="theme"`) || !strings.Contains(page, "host-shell/themes.json") || !strings.Contains(page, `"asz.theme"`) {
			t.Fatal("a page has no theme picker, or one that does not share the saved choice")
		}
		// The logo follows the ground: white on a dark theme, brand blue on
		// a light one, as Horizon's top bar shows it.
		if !strings.Contains(page, `class="logo-dark" src="/logo.svg"`) && !strings.Contains(page, `logo-dark" src="/logo.svg"`) ||
			!strings.Contains(page, `src="/logo-blue.svg"`) || !strings.Contains(page, `html[data-appearance="light"] .logo-dark { display: none; }`) {
			t.Fatal("a page does not switch the logo with the theme's appearance")
		}
	}
	blue := get("/logo-blue.svg")
	if blue.Code != http.StatusOK || !strings.Contains(blue.Body.String(), `fill="#1368B3"`) || strings.Contains(blue.Body.String(), `fill="#fff"`) {
		t.Fatalf("the blue logo is not the white one recoloured: %d", blue.Code)
	}
}

// Only the document and a record are served under a conversation: the
// routes the old page read are gone with it.
func TestOnlyTheDocumentAndARecordAreServed(t *testing.T) {
	h := view.New(storage.NewZone(t.TempDir()), nil).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/c/nothing/view", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("an unknown conversation answered %d", rec.Code)
	}
	for _, path := range []string{"/api/c/nothing/flow", "/api/c/nothing/talk/x", "/api/c/nothing"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s answered %d; only the document and a record are served", path, rec.Code)
		}
	}
}
