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

package claudecode_test

import (
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
)

func TestMatcherIsSessionScoped(t *testing.T) {
	// A real conversation whose main transcript lives in a project directory,
	// with a child stream filed under a scratchpad. Excluding /private/tmp must
	// NOT discard it: two real sessions on a live corpus have exactly this shape.
	realSession := claudecode.Session{
		ID:      "s1",
		Primary: "-Users-w-github-proj",
		Dirs:    []string{"-Users-w-github-proj", "-private-tmp-scratch"},
	}
	m := claudecode.NewMatcher(nil, []string{"/private/tmp/**"})
	if !m.Match(realSession) {
		t.Error("a scratchpad child stream must not exclude the whole session")
	}

	// A session that IS a scratchpad session - its main transcript lives there.
	scratch := claudecode.Session{
		ID:      "s2",
		Primary: "-private-tmp-scratch",
		Dirs:    []string{"-private-tmp-scratch"},
	}
	if m.Match(scratch) {
		t.Error("a session whose main transcript is under an excluded path must be excluded")
	}

	// Pruned main transcript: judged on all directories, excluded only if every
	// one matches, so an orphaned child stream of a real project survives.
	orphanReal := claudecode.Session{
		ID:   "s3",
		Dirs: []string{"-Users-w-github-proj", "-private-tmp-scratch"},
	}
	if !m.Match(orphanReal) {
		t.Error("a pruned session with a real directory must still be collected")
	}
	orphanScratch := claudecode.Session{ID: "s4", Dirs: []string{"-private-tmp-scratch"}}
	if m.Match(orphanScratch) {
		t.Error("a pruned session with only excluded directories must be excluded")
	}
}

func TestMatcherExcludeGlob(t *testing.T) {
	m := claudecode.NewMatcher(nil, []string{"/private/tmp/**"})
	cases := map[string]bool{ // dir -> want match (i.e. collected)
		"-Users-w-github-proj": true,
		"-private-tmp-scratch": false, // beneath /private/tmp
		"-private-tmp":         false, // the directory itself
		// /private/tmpfoo is a DIFFERENT directory, so "/private/tmp/**" must
		// not swallow it. Once separators are collapsed, '-' is the only
		// boundary available to tell the two apart.
		"-private-tmpfoo":      true,
		"-Users-w-private-tmp": true, // not rooted at /private/tmp
	}
	for dir, want := range cases {
		got := m.Match(claudecode.Session{ID: "s", Primary: dir, Dirs: []string{dir}})
		if got != want {
			t.Errorf("dir %q: Match = %v, want %v", dir, got, want)
		}
	}
}

func TestMatcherIncludeAcceptsRealPaths(t *testing.T) {
	// A real path is slugified forward to match a source directory name.
	// The inverse is never attempted, because slugification is lossy.
	m := claudecode.NewMatcher([]string{"/Users/w/github/skywalking-horizon-ui"}, nil)
	if !m.Match(claudecode.Session{ID: "s", Primary: "-Users-w-github-skywalking-horizon-ui", Dirs: []string{"-Users-w-github-skywalking-horizon-ui"}}) {
		t.Error("a real path should match its slugified directory name")
	}
	if m.Match(claudecode.Session{ID: "s", Primary: "-Users-w-github-other", Dirs: []string{"-Users-w-github-other"}}) {
		t.Error("non-matching directory was included")
	}
}

func TestMatcherIncludeGlob(t *testing.T) {
	m := claudecode.NewMatcher([]string{"-Users-w-github-skywalking-*"}, nil)
	if !m.Match(claudecode.Session{ID: "s", Primary: "-Users-w-github-skywalking-python", Dirs: []string{"-Users-w-github-skywalking-python"}}) {
		t.Error("glob should match")
	}
	if m.Match(claudecode.Session{ID: "s", Primary: "-Users-w-github-other", Dirs: []string{"-Users-w-github-other"}}) {
		t.Error("glob matched something it should not")
	}
}

func TestMatcherEmptyIncludesEverything(t *testing.T) {
	m := claudecode.NewMatcher(nil, nil)
	if !m.Match(claudecode.Session{ID: "s", Primary: "anything", Dirs: []string{"anything"}}) {
		t.Error("an empty matcher must collect everything")
	}
	var nilM *claudecode.Matcher
	if !nilM.Match(claudecode.Session{ID: "s"}) {
		t.Error("a nil matcher must collect everything")
	}
}
