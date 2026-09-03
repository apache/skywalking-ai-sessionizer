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

package claudecode

import (
	"path"
	"strings"
)

// Matcher decides which sessions to collect.
//
// Patterns are evaluated per SESSION, never per directory. A session's files
// can be spread across several source directories, so excluding a directory
// would otherwise silently cut streams out of a session that is still being
// collected - losing part of a conversation while appearing to succeed.
type Matcher struct {
	include []pattern
	exclude []pattern
}

// pattern is a compiled matcher entry.
//
// subtree records that the source pattern ended in "/**". It must be captured
// before slugification, because slugifying "/private/tmp/**" would collapse the
// separator and destroy the suffix.
type pattern struct {
	base    string
	subtree bool
}

// NewMatcher compiles include and exclude patterns.
//
// An entry beginning with "/" is treated as a real working directory and is
// slugified forward to match a source directory name. Anything else is matched
// as a glob against the source directory name directly. Slugification is only
// ever applied in this direction: the transform is lossy, so the inverse is
// never attempted.
func NewMatcher(include, exclude []string) *Matcher {
	return &Matcher{include: compile(include), exclude: compile(exclude)}
}

func compile(pats []string) []pattern {
	out := make([]pattern, 0, len(pats))
	for _, p := range pats {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		var sub bool
		if rest, ok := strings.CutSuffix(p, "/**"); ok {
			p, sub = rest, true
		}
		if strings.HasPrefix(p, "/") {
			p = Slugify(p)
		}
		out = append(out, pattern{base: p, subtree: sub})
	}
	return out
}

// Match reports whether a session should be collected.
//
// Matching keys on the session's PRIMARY directory - where its main transcript
// lives - not on every directory it touches. A session's child streams are
// filed under whatever working directory the child ran in, so a single
// scratchpad or subdirectory must not exclude the whole conversation. Excluding
// "/private/tmp/**" means "do not collect sessions that ARE scratchpad
// sessions", not "drop any session that ever touched a scratchpad".
//
// When the main transcript has been pruned there is no primary directory. The
// session is then judged on all of its directories together, and is only
// excluded when every one of them matches - so an orphaned child stream
// belonging to a real project is still collected.
func (m *Matcher) Match(s Session) bool {
	if m == nil {
		return true
	}
	dirs := s.Dirs
	if s.Primary != "" {
		dirs = []string{s.Primary}
	}

	excluded := len(dirs) > 0
	for _, dir := range dirs {
		if !matchAny(m.exclude, dir) {
			excluded = false
			break
		}
	}
	if excluded {
		return false
	}

	if len(m.include) == 0 {
		return true
	}
	for _, dir := range dirs {
		if matchAny(m.include, dir) {
			return true
		}
	}
	return false
}

func matchAny(pats []pattern, dir string) bool {
	for _, p := range pats {
		if p.match(dir) {
			return true
		}
	}
	return false
}

// match applies one compiled pattern to a source directory name.
//
// For a subtree pattern the directory must either be the base itself or lie
// beneath it. Beneath is tested as base+"-" rather than a bare prefix, so
// "/private/tmp/**" does not also swallow "/private/tmpfoo": source directory
// names are flat, with path separators already collapsed to '-', so '-' is the
// only boundary available.
func (p pattern) match(dir string) bool {
	if p.subtree {
		return dir == p.base || strings.HasPrefix(dir, p.base+"-")
	}
	if ok, err := path.Match(p.base, dir); err == nil && ok {
		return true
	}
	return p.base == dir
}
