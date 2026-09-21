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

package tests_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// TestDocumentExampleIsCurrent regenerates the complete asz.view example
// on the format page from the fixture scenario, the way make
// asz-view-example does, and fails when the committed file differs: a
// change to the document's shape must show in the docs in the same change.
func TestDocumentExampleIsCurrent(t *testing.T) {
	sc, err := scenario.Load("scenarios/fixture.yaml")
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	b, err := scenario.Build(sc, scenario.FormatSD, out, scenario.Options{At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	z := storage.NewZone(out)
	for {
		r, err := parse.Session(z, parse.Options{Conversation: b.Session, Session: b.Session})
		if err != nil {
			t.Fatal(err)
		}
		if !r.Changed() || !r.More {
			break
		}
	}
	c, err := view.New(z, nil).Load(b.Session)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := c.Build()
	if err != nil {
		t.Fatal(err)
	}
	got, err := sessionview.MarshalYAML(doc)
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("..", "docs", "en", "formats", "asz-view-example.yaml")
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s is not what the fixture scenario produces now; run make asz-view-example and commit the result", path)
	}
}

// TestTheMenuAndThePagesAgree holds the index to the pages, both ways.
//
// docs/menu.yml is what the website builds its navigation from, so an entry
// with no page is a broken link for every reader, and a page with no entry is
// one nobody can reach from the site at all. Neither shows up in a build here:
// the pages are Markdown, nothing compiles them, and the failure only appears
// once the documentation is published.
func TestTheMenuAndThePagesAgree(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "docs", "menu.yml"))
	if err != nil {
		t.Fatal(err)
	}
	// One path per line, which is how the file is written and how the
	// website reads it. A full YAML parse would add a dependency for the one
	// field this needs.
	listed := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		_, value, ok := strings.Cut(strings.TrimSpace(line), "path:")
		if !ok {
			continue
		}
		path := strings.TrimSpace(value)
		if path == "" || path == "/readme" {
			continue
		}
		page := filepath.Join("..", "docs", filepath.FromSlash(path)+".md")
		if _, err := os.Stat(page); err != nil {
			t.Errorf("the menu lists %s, and %s is not there", path, page)
			continue
		}
		listed[page] = true
	}
	if len(listed) == 0 {
		t.Fatal("the menu lists no page")
	}
	err = filepath.WalkDir(filepath.Join("..", "docs", "en"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return err
		}
		if !listed[path] {
			t.Errorf("%s is a page nobody can reach: it is in no menu entry", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d pages, each listed once and each present", len(listed))
}
