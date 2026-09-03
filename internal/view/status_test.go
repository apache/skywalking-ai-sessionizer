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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

func getStatus(t *testing.T, s *Server) Status {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var st Status
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// A server nobody refreshes must say so, with no refresh times at all. The
// page hides its refresh strip on this answer.
func TestStatusDefaultsToStatic(t *testing.T) {
	s := New(storage.NewZone(t.TempDir()), nil)
	st := getStatus(t, s)
	if st.Mode != ModeStatic {
		t.Fatalf("mode %q, want %q", st.Mode, ModeStatic)
	}
	if st.LastRefresh != 0 || st.NextRefresh != 0 || st.Refreshing {
		t.Fatalf("static status carries refresh state: %+v", st)
	}
}

// What a refresher records is what the page gets back, field for field.
func TestStatusRoundTrips(t *testing.T) {
	s := New(storage.NewZone(t.TempDir()), nil)
	want := Status{
		Mode: "watch", Adapter: "claude-code-local", Source: "/src",
		IntervalMS: 5000, LastRefresh: 1000, NextRefresh: 6000,
		Landed: 2, Records: 7, Rounds: 1, TookMS: 42, LastError: "x: y",
	}
	s.SetStatus(want)
	if got := getStatus(t, s); got != want {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

// Both images are served from the embedded bytes, as valid XML from the
// first byte: a declaration after a comment is rejected by strict renderers.
func TestImagesServed(t *testing.T) {
	s := New(storage.NewZone(t.TempDir()), nil)
	for _, path := range []string{"/favicon.svg", "/logo.svg"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "image/svg+xml" {
			t.Fatalf("%s: content type %q", path, ct)
		}
		if !strings.HasPrefix(rec.Body.String(), "<?xml") {
			t.Fatalf("%s: does not start with the XML declaration", path)
		}
	}
}
