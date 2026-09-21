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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
)

// TestTheGlossaryIsNotOneRuntimes: the page asks for one vocabulary as it
// loads, and it used to get Claude Code's whichever runtime the root held. A
// LangChain conversation described in Claude Code's words is wrong in every
// term the two do not share.
func TestTheGlossaryIsNotOneRuntimes(t *testing.T) {
	one := model.NewGlossary("one/1", model.Term{Unified: model.KindTalk, Native: "first"})
	two := model.NewGlossary("two/1", model.Term{Unified: model.KindTalk, Native: "second"})
	zone := storage.NewZone(t.TempDir())
	server := view.NewWithGlossaries(zone, map[string]*model.Glossary{
		"one/1": one, "two/1": two})

	ask := func(query string) (string, int) {
		r := httptest.NewRequest(http.MethodGet, "/api/glossary"+query, nil)
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, r)
		var got struct {
			Dialect string `json:"dialect"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		return got.Dialect, w.Code
	}
	if d, code := ask("?dialect=two/1"); d != "two/1" {
		t.Errorf("asked for two/1, got %q (status %d)", d, code)
	}
	if d, code := ask("?dialect=one/1"); d != "one/1" {
		t.Errorf("asked for one/1, got %q (status %d)", d, code)
	}
	// A dialect the root does not know is not answered with another one's
	// words, which would be worse than saying nothing.
	if _, code := ask("?dialect=three/1"); code != http.StatusNotFound {
		t.Errorf("an unknown dialect answered %d, want 404", code)
	}
}

// TestOneRegisteredGlossaryIsStillAnAnswer keeps an empty root working: there
// is nothing landed to read a dialect from, and one vocabulary is unambiguous.
func TestOneRegisteredGlossaryIsStillAnAnswer(t *testing.T) {
	only := model.NewGlossary("only/1", model.Term{Unified: model.KindTalk})
	zone := storage.NewZone(t.TempDir())
	server := view.NewWithGlossaries(zone, map[string]*model.Glossary{"only/1": only})
	r := httptest.NewRequest(http.MethodGet, "/api/glossary", nil)
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, r)
	var got struct {
		Dialect string `json:"dialect"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Dialect != "only/1" {
		t.Fatalf("got %q, want only/1 (status %d)", got.Dialect, w.Code)
	}
}
