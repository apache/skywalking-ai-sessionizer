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

package scope_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/plugins/claude-code/internal/scope"
)

func TestUnconditionalNamesPruneAnywhere(t *testing.T) {
	r, err := scope.Compile(scope.StandardV1, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, rel := range []string{".git", "node_modules", "a/b/target", "apps/ui/node_modules", "pkg/x.egg-info", "vendor/bundle", "docs/resources/_gen"} {
		if !r.Prune(rel, filepath.Join(root, rel)) {
			t.Errorf("%s not pruned", rel)
		}
	}
	for _, rel := range []string{"src", "apps/target-practice", "packages", ".claude", "data", "vendor"} {
		if r.Prune(rel, filepath.Join(root, rel)) {
			t.Errorf("%s pruned", rel)
		}
	}
}

func TestConditionalNamesPruneOnlyBesideTheirMarker(t *testing.T) {
	r, _ := scope.Compile(scope.StandardV1, nil, nil)
	root := t.TempDir()
	mk := func(p string) {
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, p), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("java/pom.xml")
	mk("go/go.mod")
	mk("dotnet/App.csproj")
	mk("scripts/README.md")
	cases := map[string]bool{
		"java/build":    true,
		"go/bin":        true,
		"go/dist":       true,
		"go/vendor":     true,
		"dotnet/bin":    true,
		"dotnet/obj":    true,
		"scripts/build": false,
		"scripts/bin":   false,
		"java/dist":     false,
	}
	for rel, want := range cases {
		if got := r.Prune(rel, filepath.Join(root, rel)); got != want {
			t.Errorf("%s: pruned=%v want %v", rel, got, want)
		}
	}
}

func TestAdditionsAndRemovals(t *testing.T) {
	r, err := scope.Compile(scope.StandardV1, []string{"/data/", "**/generated/"}, []string{"**/target/"})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if !r.Prune("data", filepath.Join(root, "data")) || r.Prune("sub/data", filepath.Join(root, "sub/data")) {
		t.Error("an anchored addition applies at the root only")
	}
	if !r.Prune("a/generated", filepath.Join(root, "a/generated")) {
		t.Error("an unanchored addition applies anywhere")
	}
	if r.Prune("target", filepath.Join(root, "target")) {
		t.Error("a removed default still pruned")
	}
	joined := strings.Join(r.Expanded, "\n")
	if !strings.Contains(joined, "/data/") || strings.Contains(joined, "**/target/") || !strings.Contains(joined, "**/build/ beside") {
		t.Errorf("expanded rules:\n%s", joined)
	}
	if _, err := scope.Compile(scope.StandardV1, nil, []string{"**/nothing/"}); err == nil {
		t.Error("removing a rule the set does not have was accepted")
	}
	if _, err := scope.Compile("none", nil, []string{"**/target/"}); err == nil {
		t.Error("a removal with no defaults was accepted")
	}
	if _, err := scope.Compile("other-v9", nil, nil); err == nil {
		t.Error("an unknown set was accepted")
	}
	none, _ := scope.Compile("none", nil, nil)
	if none.Prune(".git", filepath.Join(root, ".git")) {
		t.Error("with no defaults nothing is pruned")
	}
}
