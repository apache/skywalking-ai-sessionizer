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
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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

// TestTheReleaseGuideNamesThePackagesTheMakefileBuilds holds the release
// guide to the packages a release has. A release manager copies the vote
// mail from the guide when the script stops, and the guide once named a
// Debian package the Makefile had stopped building, so the mail would have
// asked the PMC to vote on files that do not exist.
func TestTheReleaseGuideNamesThePackagesTheMakefileBuilds(t *testing.T) {
	makefile, err := os.ReadFile(filepath.Join("..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	// tools/release/release.sh reads these two lines from a tag's Makefile
	// the same way, to know the packages of that version.
	list := func(name string) []string {
		m := regexp.MustCompile(`(?m)^` + name + `[ \t]*:?=[ \t]*(.+)$`).FindSubmatch(makefile)
		if m == nil {
			t.Fatalf("the Makefile has no %s", name)
		}
		return strings.Fields(string(m[1]))
	}
	platforms, debs := list("PLATFORMS"), list("DEB_PACKAGES")
	// The order candidate writes the checksums in its vote mail: the source
	// package, one binary package for each platform, then each Debian package
	// for each Linux platform.
	const pkg = "apache-skywalking-ai-sessionizer-$VERSION"
	want := []string{pkg + "-src.tgz"}
	for _, p := range platforms {
		system, arch, _ := strings.Cut(p, "/")
		ext := "tgz"
		if system == "windows" {
			ext = "zip"
		}
		want = append(want, fmt.Sprintf("%s-bin-%s-%s.%s", pkg, system, arch, ext))
	}
	for _, d := range debs {
		for _, p := range platforms {
			if arch, ok := strings.CutPrefix(p, "linux/"); ok {
				want = append(want, fmt.Sprintf("%s-bin-%s-%s.deb", pkg, d, arch))
			}
		}
	}
	guide, err := os.ReadFile(filepath.Join("..", "docs", "en", "guides", "how-to-release.md"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range regexp.MustCompile(`(?m)^   - <sha512>  (\S+)$`).FindAllSubmatch(guide, -1) {
		got = append(got, string(m[1]))
	}
	if !slices.Equal(got, want) {
		t.Errorf("the vote mail in the release guide lists the checksums of\n  %s\nand the Makefile builds\n  %s",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	// What each Debian package installs is said in prose, where a package
	// name can be wrong too.
	named := 0
	for _, m := range regexp.MustCompile("`([a-z0-9-]+)` installs\\s+`/usr/bin/").FindAllSubmatch(guide, -1) {
		named++
		if !slices.Contains(debs, string(m[1])) {
			t.Errorf("the release guide says the Debian package %s installs a binary, and DEB_PACKAGES in the Makefile is %v", m[1], debs)
		}
	}
	if named == 0 {
		t.Error("the release guide no longer says what each Debian package installs")
	}
}
