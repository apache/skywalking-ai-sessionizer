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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
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
		// Without loadFiles the renderer offers no Prompt tab at all, so the
		// page beside the storage root would show less than the SkyWalking UI.
		"loadFiles", "/files?",
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
	// A hide in the page's address goes to the document, the record and the
	// files alike, and stays in the address as the reader moves.
	for _, call := range []string{`api + "/view" + hideQuery()`, `ref.row + hideQuery()`, `const q = withHidden(new URLSearchParams());
      q.set("session"`, `const q = withHidden(new URLSearchParams());
      for (const k of ["talk", "step", "stream"])`} {
		if !strings.Contains(body, call) {
			t.Fatalf("the page does not pass the address's hide on: %s", call)
		}
	}
	if !strings.Contains(body, `for (const a of document.querySelectorAll('a[href="/"]')) a.href = "/" + hideQuery();`) {
		t.Fatal("the page's links back to the list drop the address's hide")
	}
	// A key spelled another way is passed on too, so the server refuses it
	// rather than the page dropping it.
	for _, page := range []string{body, get("/").Body.String()} {
		if !strings.Contains(page, `.filter(([k]) => k.toLowerCase().replace(/\P{L}/gu, "") === "hide")`) {
			t.Fatal("a page passes on only the hide spelled exactly")
		}
	}
	// An address holding `;` is refused by both pages before anything is asked
	// for, and the conversation page's links carry it back as it is.
	for _, page := range []string{body, get("/").Body.String()} {
		if !strings.Contains(page, `location.search.includes(";")`) {
			t.Fatal("a page does not refuse an address that does not parse")
		}
	}
	if !strings.Contains(body, `if (unparsed) throw new Error(unparsed);`) || !strings.Contains(body, `if (unparsed) return location.search;`) {
		t.Fatal("the conversation page asks for a document, or links back, under an address that does not parse")
	}
	index := get("/").Body.String()
	if !strings.Contains(index, `location.href = "/c/" + encodeURIComponent(id) + hideQuery();`) {
		t.Fatal("the list's links into a conversation drop the address's hide")
	}
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

// listPageBrowser runs the list page's scripts, in order, with a stand-in for
// the browser and reports what they threw, what the page showed, and what it
// asked the server for. The stand-in keeps what a browser keeps after a script
// throws: the next script still runs, a list it hands the page has an element
// in it, every function the page hands it or sets as a handler is called once
// the scripts end, round after round with a promise's callbacks run between,
// window is the page's own global, and storage is empty, as a browser's is
// before a theme is saved, so a script that reads it goes on as it would. The body's first child is the element
// its markup starts with, so a refusal written into nothing fails as it
// would. Anything else the page touches is a function that returns itself.
// It is not a browser: a DOM method the page does not use fails inside a
// callback, which is passed over.
const listPageBrowser = `"use strict";
const vm = require("node:vm");
const fs = require("node:fs");
const fetched = [];
const handed = [];
const take = v => { if (typeof v === "function") handed.push(v); };
const any = () => new Proxy(function () {}, {
  get: (_, k) => k === Symbol.iterator ? function* () { yield any(); } : k === Symbol.toPrimitive ? () => "" : k === "then" ? undefined : any(),
  apply: (_, __, args) => { args.forEach(take); return any(); },
  set: (_, __, v) => { take(v); return true; },
});
const body = { firstChild: null, set innerHTML(html) { this.firstChild = /^\s*<[a-z]/i.test(html) ? { textContent: "" } : null; } };
const document = new Proxy({ body }, { get: (o, k) => k in o ? o[k] : any(), set: (_, __, v) => { take(v); return true; } });
const browser = {
  location: { search: process.argv[process.argv.length - 1], href: "" },
  document,
  URLSearchParams,
  console,
  fetch: url => { fetched.push(String(url)); return new Promise(() => {}); },
  localStorage: { getItem: () => null, setItem: () => {}, removeItem: () => {} },
  setTimeout: any(),
  setInterval: any(),
  requestAnimationFrame: any(),
  queueMicrotask: any(),
  addEventListener: any(),
};
const context = vm.createContext(browser);
browser.window = vm.runInContext("globalThis", context);
const thrown = [];
for (const file of process.argv.slice(2, -1)) {
  try { vm.runInContext(fs.readFileSync(file, "utf8"), context); } catch (e) { thrown.push(String(e && e.message)); }
}
const called = new Set();
(async () => {
  for (let round = 0; round < 10; round++) {
    await new Promise(r => setImmediate(r));
    for (const k of Object.keys(browser)) if (k.startsWith("on") && !called.has(browser[k])) { called.add(browser[k]); take(browser[k]); }
    if (handed.length === 0) break;
    for (const f of handed.splice(0)) { try { f(any()); } catch (e) {} }
  }
  process.stdout.write(JSON.stringify({ thrown: thrown.join("; "), shown: body.firstChild ? body.firstChild.textContent : "", fetched }));
})();
`

// The list page stops before it draws a link or asks for anything when its
// address holds `;`, and goes on when it does not: with the refusal gone it
// would offer every conversation without the hide. The scripts are run, since
// a check of their text passes with the refusal commented out. Without node
// the test is skipped, as the LangChain shim's tests are without python3.
func TestListPageRefusesAnAddressThatDoesNotParse(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("the list page's script runs under node, which is not installed")
	}
	h := view.New(storage.NewZone(t.TempDir()), nil).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	dir := t.TempDir()
	browserPath := filepath.Join(dir, "browser.js")
	if err := os.WriteFile(browserPath, []byte(listPageBrowser), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{browserPath}
	guarded := false
	for i, m := range regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllStringSubmatch(rec.Body.String(), -1) {
		guarded = guarded || strings.Contains(m[1], "const unparsed")
		path := filepath.Join(dir, fmt.Sprintf("script-%d.js", i))
		if err := os.WriteFile(path, []byte(m[1]), 0o644); err != nil {
			t.Fatal(err)
		}
		args = append(args, path)
	}
	if !guarded {
		t.Fatal("the list page has no script that reads its address")
	}
	type run struct {
		Thrown  string   `json:"thrown"`
		Shown   string   `json:"shown"`
		Fetched []string `json:"fetched"`
	}
	open := func(search string) run {
		out, err := exec.Command(node, append(args, search)...).Output()
		if err != nil {
			t.Fatalf("node: %v", err)
		}
		var r run
		if err := json.Unmarshal(out, &r); err != nil {
			t.Fatalf("node wrote %q: %v", out, err)
		}
		return r
	}
	const refusal = "';' is not a separator"
	if r := open("?x=1;hide=system_prompt"); !strings.Contains(r.Thrown, refusal) || !strings.Contains(r.Shown, refusal) || len(r.Fetched) != 0 {
		t.Fatalf("an address holding ';' is not refused before anything is asked for: %+v", r)
	}
	if r := open("?hide=system_prompt"); r.Thrown != "" || r.Shown != "" || !slices.Contains(r.Fetched, "/api/status") {
		t.Fatalf("an address that parses does not reach the list: %+v", r)
	}
}

// Only the document, a record and the landed files are served under a
// conversation: the routes the old page read are gone with it.
func TestOnlyTheDocumentARecordAndFilesAreServed(t *testing.T) {
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

// A parser killed while writing its first round leaves a temporary file in
// the rounds directory. The list names only conversations with a round.
func TestListSkipsAConversationWithOnlyATemporaryRound(t *testing.T) {
	root := t.TempDir()
	rounds := filepath.Join(root, "_conversations", "interrupted", "rounds")
	if err := os.MkdirAll(rounds, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rounds, ".tmp-123"), []byte("incomplete"), 0o600); err != nil {
		t.Fatal(err)
	}
	ids, err := view.New(storage.NewZone(root), nil).List()
	if err != nil || len(ids) != 0 {
		t.Fatalf("listed %v, %v; want no conversation", ids, err)
	}
}
