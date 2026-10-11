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

package kiro

import (
	"path"
	"strings"
)

// Matcher decides which sessions to collect, by the paths a session worked in.
//
// The engine names a session's directory by a digest of its workspace paths,
// so the directory says nothing a pattern could match. The paths themselves
// are in the session's header, and they are what is matched.
type Matcher struct {
	include []pattern
	exclude []pattern
}

// pattern is one compiled entry. subtree records a trailing "/**".
type pattern struct {
	base    string
	subtree bool
}

// NewMatcher compiles include and exclude patterns. An entry ending in "/**"
// matches that path and everything beneath it. Any other entry is a glob over
// the whole path.
func NewMatcher(include, exclude []string) *Matcher {
	return &Matcher{include: compile(include), exclude: compile(exclude)}
}

func compile(pats []string) []pattern {
	out := make([]pattern, 0, len(pats))
	for _, p := range pats {
		p = slashed(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		var sub bool
		if rest, ok := strings.CutSuffix(p, "/**"); ok {
			p, sub = rest, true
		}
		out = append(out, pattern{base: p, subtree: sub})
	}
	return out
}

// Match reports whether a session should be collected.
//
// A session can work in several paths. It is excluded only when every one of
// them is excluded, so a session that touched a scratch directory beside a
// real project is still collected. A session with no workspace path is
// excluded by nothing.
func (m *Matcher) Match(s Session) bool {
	if m == nil {
		return true
	}
	dirs := make([]string, 0, len(s.Workspaces))
	for _, w := range s.Workspaces {
		dirs = append(dirs, slashed(w))
	}
	excluded := len(dirs) > 0
	for _, d := range dirs {
		if !matchAny(m.exclude, d) {
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
	for _, d := range dirs {
		if matchAny(m.include, d) {
			return true
		}
	}
	return false
}

// slashed spells a path with forward slashes, whatever the host. Both sides of a
// match need it: a pattern is written by hand in either spelling, and a session
// written on Windows keeps its backslashes when it is read on another host,
// where filepath.ToSlash leaves them as they are.
func slashed(p string) string {
	return strings.ReplaceAll(p, `\`, "/")
}

func matchAny(pats []pattern, dir string) bool {
	for _, p := range pats {
		if p.match(dir) {
			return true
		}
	}
	return false
}

// match applies one pattern to a path. A subtree is the path itself or one
// beneath it, tested at a separator, so "/private/tmp/**" does not also match
// "/private/tmpfoo".
func (p pattern) match(dir string) bool {
	if p.subtree {
		return dir == p.base || strings.HasPrefix(dir, p.base+"/")
	}
	if ok, err := path.Match(p.base, dir); err == nil && ok {
		return true
	}
	return p.base == dir
}
