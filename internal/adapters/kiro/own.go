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
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Where a session worked, against the storage root.
type origin int

const (
	originOutside origin = iota
	originInside
	originUnknown
)

// originOf says whether a session worked inside the storage root, root
// already resolved. One workspace inside is enough: kiro-cli, run as a judge
// evaluator, works in a stage directory under the root and keeps its session
// in Kiro's history, which this adapter reads. Collecting it would land a
// judge's reading of a conversation as a conversation of its own. A session
// with no workspace path is outside, and so is a path that is not absolute
// on this machine, such as one a session recorded on another system: the
// evaluator works in a stage directory, which is absolute here. A path that
// cannot be resolved makes the answer unknown, never outside.
func originOf(s Session, root string) origin {
	answer := originOutside
	for _, w := range s.Workspaces {
		if !filepath.IsAbs(filepath.Clean(filepath.FromSlash(slashed(w)))) {
			continue
		}
		p, ok := resolve(w)
		if !ok {
			answer = originUnknown
			continue
		}
		if within(root, p) {
			return originInside
		}
	}
	return answer
}

// resolve gives the path a workspace names once its links are followed. A
// workspace need not exist any more: a judge's stage directory goes with its
// session when the session is retired, and Kiro keeps the header that names
// it. So the longest ancestor that exists is resolved, links followed, and
// the rest is put back as written. A link that does not resolve, or an
// ancestor that cannot be read, is no answer.
func resolve(w string) (string, bool) {
	p := filepath.Clean(filepath.FromSlash(slashed(w)))
	if !filepath.IsAbs(p) {
		return "", false
	}
	rest := ""
	for cur := p; ; {
		_, err := os.Lstat(cur)
		if err == nil {
			resolved, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return "", false
			}
			return filepath.Join(resolved, rest), true
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", false
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", false
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// within says whether p is root or lies beneath it, by whole components, so
// /work/data does not hold /work/data-project. On macOS and Windows, whose
// filesystems usually ignore case, letters are compared without it: a
// session left out wrongly is a smaller harm than an evaluator's collected.
func within(root, p string) bool {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		root, p = strings.ToLower(root), strings.ToLower(p)
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ownRoot is the storage root, resolved: it exists once a command that
// collects has made it, before any collector runs.
func ownRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}
