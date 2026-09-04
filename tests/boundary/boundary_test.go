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

// Package boundary holds the import rules between the two sides of the
// project.
//
// The collector side lands Session Data. The server side assembles it into
// conversations and serves them. They meet only at the storage root, so that
// a later split into a collector binary and a server binary, talking over a
// network, is packaging and not a refactor. This test fails the moment either
// side imports the other.
package boundary_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

const module = "github.com/apache/skywalking-ai-sessionizer/"

var (
	// The exporter is the collector side's way out: it reads the storage root
	// and sends, and must not know how anything is assembled or shown. A
	// scenario's writers land input, so they are collector side too.
	collectorSide = []string{"internal/adapters/", "internal/export/", "internal/scenario/"}
	serverSide    = []string{"internal/assemble/", "internal/parse/", "internal/view/", "internal/verify/", "pkg/sessionflow/", "pkg/sessionview/", "internal/scenario/expect/"}
	// The scenario runner wires both sides, as the command does. Only the
	// command and the tests may import it.
	wiring = []string{"internal/scenario/run/"}
)

// sideOf classifies a package directory by its longest matching prefix.
func sideOf(pkg string) string {
	best, side := 0, ""
	for name, list := range map[string][]string{"collector": collectorSide, "server": serverSide, "wiring": wiring} {
		for _, prefix := range list {
			if strings.HasPrefix(pkg, prefix) && len(prefix) > best {
				best, side = len(prefix), name
			}
		}
	}
	return side
}

func TestSidesMeetOnlyAtTheStorageRoot(t *testing.T) {
	root := filepath.Join("..", "..")
	for pkg, imports := range importsUnder(t, root) {
		from := sideOf(pkg)
		for _, imp := range imports {
			if !strings.HasPrefix(imp, module) {
				continue
			}
			to := sideOf(strings.TrimPrefix(imp, module) + "/")
			switch {
			case from == "collector" && to == "server", from == "server" && to == "collector":
				t.Errorf("%s imports %s; the two sides may meet only at the storage root", pkg, imp)
			case to == "wiring" && from != "wiring" && !strings.HasPrefix(pkg, "cmd/") && !strings.HasPrefix(pkg, "tests/"):
				t.Errorf("%s imports %s; the scenario runner is wiring, for the command and the tests only", pkg, imp)
			}
		}
	}
}

// The page reads Session Data and Session Flow and nothing else. The index is
// assembly's accelerator, derived and disposable; a root that arrives with
// only its landed files and its rounds must render in full.
func TestThePageReadsOnlyTheTwoFormats(t *testing.T) {
	root := filepath.Join("..", "..")
	for pkg, imports := range importsUnder(t, filepath.Join(root, "internal/view/")) {
		for _, imp := range imports {
			if strings.HasPrefix(imp, module+"internal/index") {
				t.Errorf("%s imports %s; the page must read .sd and .sf only", pkg, imp)
			}
		}
	}
}

// importsUnder lists the imports of every non-test package under dir, keyed
// by package directory. Test files are left out: a test may drive both sides.
func importsUnder(t *testing.T, dir string) map[string][]string {
	t.Helper()
	root := filepath.Join("..", "..")
	out := map[string][]string{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		key, _ := filepath.Rel(root, filepath.Dir(p))
		key = filepath.ToSlash(key) + "/"
		for _, imp := range f.Imports {
			out[key] = append(out[key], strings.Trim(imp.Path.Value, `"`))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatalf("no packages found under %s", dir)
	}
	return out
}
