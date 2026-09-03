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
	_ "embed"
	"net/http"
)

//go:embed page.html
var pageHTML []byte

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
