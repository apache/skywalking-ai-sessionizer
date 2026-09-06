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

package view

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"regexp"
	"strings"
)

//go:embed page.html
var pageHTML []byte

// The conversation renderer: Horizon's @skywalking-horizon-ui/conversation-view,
// built from the Horizon commit named in conversation-view/HORIZON_COMMIT and
// committed here, with Horizon's design tokens, themes and fonts, so this page
// and the SkyWalking UI draw a conversation identically and asz builds
// without a JavaScript toolchain. tools/conversation-view.sh rebuilds it
// from the pin, and CI fails when the copy differs from that build.
//
//go:embed conversation-view
var renderer embed.FS

// AssetPrefix is where the page loads the renderer from.
const AssetPrefix = "/assets/conversation-view/"

func init() {
	// The Go mime table knows no font type; without this the fonts would be
	// served as octet streams, which a browser still uses but a strict one
	// logs about.
	_ = mime.AddExtensionType(".woff2", "font/woff2")
}

var pinLine = regexp.MustCompile(`(?m)^commit ([0-9a-f]{40})$`)

// HorizonCommit is the Horizon commit the embedded renderer was built from.
func HorizonCommit() string {
	b, err := renderer.ReadFile("conversation-view/HORIZON_COMMIT")
	if err != nil {
		return ""
	}
	if m := pinLine.FindSubmatch(b); m != nil {
		return string(m[1])
	}
	return ""
}

// assets serves the renderer's files. They change only when the pin does,
// so a browser may keep them for a day; a redeploy under a new pin serves
// new bytes under the same paths, which a day-old cache would miss, and the
// page's own HTML is never cached.
func assets() http.Handler {
	sub, err := fs.Sub(renderer, "conversation-view")
	if err != nil {
		panic(err)
	}
	files := http.StripPrefix(AssetPrefix, http.FileServer(http.FS(sub)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") || strings.HasSuffix(r.URL.Path, "HORIZON_COMMIT") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=86400")
		files.ServeHTTP(w, r)
	})
}

//go:embed index.html
var indexHTML []byte

//go:embed favicon.svg
var faviconSVG []byte

//go:embed logo.svg
var logoSVG []byte

// index lists the conversations. It is the only page that knows there is more
// than one of them.
func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

// page serves one conversation, named by the path.
//
// The conversation is in the address rather than in a query string, so a
// reader can keep a link to one and an export can be laid out the same way.
func (s *Server) page(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(pageHTML)
}

// svg serves one embedded image. The tab icon and the header of every page
// come from these same bytes, so the two cannot drift apart.
func svg(b []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(b)
	}
}
