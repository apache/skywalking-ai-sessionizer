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

// Package scope decides which directories a scan leaves out.
//
// Exclusion means no observation: a change under an excluded directory is
// never seen, so an empty result means "no changes within the observed
// scope" and never "no changes". Every record names the rule set and the
// expanded rules it ran under, so history explains itself after the set
// changes. The set is frozen under its name; a changed set gets a new one.
//
// Two kinds of rule, because the names build tools use are also names
// source uses. An unconditional name is excluded wherever it appears; it
// is never source. A conditional name is excluded only beside the
// ecosystem's own project file, which is how the build tools themselves
// decide: build/ beside pom.xml is Maven output, while build/ in a
// repository with no project file beside it is kept, because many
// repositories keep scripts there.
package scope

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// StandardV1 is the name of the default set below.
const StandardV1 = "standard-v1"

// always are the names excluded wherever they appear.
var always = []string{
	// version control
	".git", ".hg", ".svn",
	// JVM
	"target", ".gradle",
	// Node
	"node_modules", ".next", ".nuxt", ".turbo", ".parcel-cache", ".svelte-kit",
	// Python
	"__pycache__", ".venv", "venv", ".tox", ".nox", ".mypy_cache", ".pytest_cache", ".ruff_cache", ".eggs",
	// C and C++
	"CMakeFiles",
	// Swift and Xcode
	".build", "DerivedData", "Pods",
	// Scala
	".bloop", ".metals",
	// Elixir
	"_build",
	// Haskell
	"dist-newstyle", ".stack-work",
	// Dart and Flutter
	".dart_tool",
	// Zig
	"zig-cache", ".zig-cache", "zig-out",
	// Terraform
	".terraform",
	// coverage
	"coverage", ".nyc_output", "htmlcov",
	// caches and IDE state
	".cache", ".idea", ".vs",
}

// alwaysSuffix are name patterns excluded wherever they appear.
var alwaysSuffix = []string{".egg-info"}

// alwaysNested are two-level paths excluded wherever they appear.
var alwaysNested = []string{"vendor/bundle", "Carthage/Build", "resources/_gen"}

// beside are the names excluded only when a marker file sits in the same
// directory as the excluded directory.
var beside = map[string][]string{
	"build":  {"pom.xml", "build.gradle", "build.gradle.kts", "package.json", "pyproject.toml", "setup.py", "CMakeLists.txt", "pubspec.yaml"},
	"dist":   {"go.mod", "package.json", "pyproject.toml", "setup.py"},
	"bin":    {"go.mod"},
	"obj":    {},
	"vendor": {"go.mod", "composer.json"},
	"tmp":    {"Gemfile"},
	"log":    {"Gemfile"},
	"deps":   {"mix.exs"},
	"public": {"hugo.toml", "hugo.yaml", "config.toml"},
	"_site":  {"_config.yml"},
	"site":   {"mkdocs.yml"},
}

// besideGlob are markers matched by pattern, for the ecosystems whose
// project file carries the project's name.
var besideGlob = map[string][]string{
	"bin": {"*.csproj", "*.sln"},
	"obj": {"*.csproj", "*.sln"},
}

// Rules is one frozen policy: the set's name and the rules it expands to.
type Rules struct {
	Set      string
	Expanded []string

	names   map[string]bool
	nested  map[string]bool
	cond    map[string]bool
	added   []rule
	removed map[string]bool
}

// rule is one project addition: a name matched at any depth, or a path
// anchored at the root.
type rule struct {
	text     string
	anchored bool
	parts    []string
}

// Compile builds the policy from a set name and the project's additions
// and removals. effective = (set − remove) ∪ add.
func Compile(set string, add, remove []string) (*Rules, error) {
	if set != StandardV1 && set != "none" {
		return nil, fmt.Errorf("scope: unknown exclusion set %q; the sets are %s and none", set, StandardV1)
	}
	r := &Rules{Set: set, names: map[string]bool{}, nested: map[string]bool{}, cond: map[string]bool{}, removed: map[string]bool{}}
	for _, x := range remove {
		x = normalize(x)
		if set == "none" {
			return nil, fmt.Errorf("scope: remove %q makes no sense with defaults: none", x)
		}
		if !inSet(x) {
			return nil, fmt.Errorf("scope: remove %q names no rule of %s", x, set)
		}
		r.removed[x] = true
	}
	if set == StandardV1 {
		for _, n := range always {
			if !r.removed[n] {
				r.names[n] = true
			}
		}
		for _, n := range alwaysNested {
			if !r.removed[n] {
				r.nested[n] = true
			}
		}
		for n := range beside {
			if !r.removed[n] {
				r.cond[n] = true
			}
		}
	}
	seen := map[string]bool{}
	for _, a := range add {
		text := normalize(a)
		if text == "" || seen[text] {
			continue
		}
		if r.removed[text] {
			return nil, fmt.Errorf("scope: %q is both added and removed", text)
		}
		seen[text] = true
		anchored := strings.HasPrefix(a, "/")
		r.added = append(r.added, rule{text: text, anchored: anchored, parts: strings.Split(text, "/")})
	}
	r.Expanded = r.expand()
	return r, nil
}

// normalize strips the syntax a rule may be written with: a leading
// "**/", a leading "/", a trailing "/".
func normalize(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "**/")
	s = strings.TrimPrefix(s, "/")
	s = strings.TrimSuffix(s, "/")
	return s
}

func inSet(name string) bool {
	for _, n := range always {
		if n == name {
			return true
		}
	}
	for _, n := range alwaysNested {
		if n == name {
			return true
		}
	}
	_, ok := beside[name]
	return ok
}

// expand lists the rules in force, in a stable order, for the record.
func (r *Rules) expand() []string {
	var out []string
	for n := range r.names {
		out = append(out, "**/"+n+"/")
	}
	for _, s := range alwaysSuffix {
		if r.Set == StandardV1 {
			out = append(out, "**/*"+s+"/")
		}
	}
	for n := range r.nested {
		out = append(out, "**/"+n+"/")
	}
	for n := range r.cond {
		markers := append([]string{}, beside[n]...)
		markers = append(markers, besideGlob[n]...)
		sort.Strings(markers)
		out = append(out, "**/"+n+"/ beside "+strings.Join(markers, ", "))
	}
	sort.Strings(out)
	for _, a := range r.added {
		if a.anchored {
			out = append(out, "/"+a.text+"/")
		} else {
			out = append(out, "**/"+a.text+"/")
		}
	}
	return out
}

// Prune says whether a directory is left out. rel is the directory's path
// relative to the root with forward slashes, and dir its absolute path,
// which is read only when a conditional name needs its parent looked at.
func (r *Rules) Prune(rel, dir string) bool {
	name := path.Base(rel)
	if r.names[name] {
		return true
	}
	if r.Set == StandardV1 {
		for _, s := range alwaysSuffix {
			if strings.HasSuffix(name, s) && !r.removed[name] {
				return true
			}
		}
	}
	for n := range r.nested {
		if rel == n || strings.HasSuffix(rel, "/"+n) {
			return true
		}
	}
	if r.cond[name] && r.markerBeside(name, filepath.Dir(dir)) {
		return true
	}
	for _, a := range r.added {
		if a.anchored {
			if rel == a.text {
				return true
			}
			continue
		}
		if name == a.text || rel == a.text || strings.HasSuffix(rel, "/"+a.text) {
			return true
		}
	}
	return false
}

// markerBeside reports whether the parent directory holds one of the
// ecosystem's project files.
func (r *Rules) markerBeside(name, parent string) bool {
	for _, m := range beside[name] {
		if _, err := os.Stat(filepath.Join(parent, m)); err == nil {
			return true
		}
	}
	for _, g := range besideGlob[name] {
		if matches, _ := filepath.Glob(filepath.Join(parent, g)); len(matches) > 0 {
			return true
		}
	}
	return false
}
