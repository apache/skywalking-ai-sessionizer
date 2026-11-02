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
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
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

// The answers LandedOrigin gives.
const (
	OriginOutside = "outside"
	OriginInside  = "inside"
	OriginUnknown = "unknown"
)

// LandedOrigin says, for a session this adapter landed, whether it worked
// inside the storage root, from the workspace paths of its newest landed
// header: the session's source may be gone, and what landed is the record of
// it. asz server asks it before judging a conversation, so a judge evaluator's
// session collected before the guard is never judged. A session with no
// readable landed header, or one a skeleton replaced, is unknown.
func LandedOrigin(z *storage.Zone, session string) (string, error) {
	files, err := storage.LandedFiles(z, session)
	if err != nil {
		return OriginUnknown, err
	}
	main := z.StreamDir(session, storage.StreamMain)
	var newest *storage.LandedFile
	for i := range files {
		f := &files[i]
		if filepath.Dir(f.Path) == main && strings.HasPrefix(filepath.Base(f.Path), "meta-") && (newest == nil || f.Seq > newest.Seq) {
			newest = f
		}
	}
	if newest == nil || newest.Skeleton {
		return OriginUnknown, nil
	}
	in, err := os.Open(newest.Path)
	if err != nil {
		return OriginUnknown, err
	}
	defer in.Close()
	r, err := sessiondata.NewReader(in)
	if err != nil {
		return OriginUnknown, err
	}
	var h header
	found := false
	for {
		rec, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return OriginUnknown, err
		}
		for _, part := range rec.Parts {
			if part.Kind == sessiondata.PartData && json.Unmarshal(part.Data, &h) == nil {
				found = true
			}
		}
	}
	if !found {
		return OriginUnknown, nil
	}
	root, err := ownRoot(z.Root())
	if err != nil {
		return OriginUnknown, err
	}
	switch originOf(Session{ID: session, Workspaces: h.WorkspacePaths}, root) {
	case originInside:
		return OriginInside, nil
	case originOutside:
		return OriginOutside, nil
	}
	return OriginUnknown, nil
}

// SourceOrigin says, from the live source, whether a session works inside
// the storage root, and false only when the source surely holds no such
// session. asz server asks it when it admits a run, since a session's
// workspace can change after it was collected. The session's directory is
// looked for by its name, and its header read, whatever discovery would
// leave out of a pass: a directory that cannot be read, or a header that
// does not read, is no answer, never an absence.
func SourceOrigin(sourceRoot string, z *storage.Zone, session string) (string, bool, error) {
	base := filepath.Join(sourceRoot, "sessions")
	keys, err := os.ReadDir(base)
	if errors.Is(err, fs.ErrNotExist) {
		return OriginUnknown, false, nil
	}
	if err != nil {
		return OriginUnknown, true, err
	}
	for _, k := range keys {
		if !k.IsDir() {
			continue
		}
		dir := filepath.Join(base, k.Name(), session)
		if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return OriginUnknown, true, err
		}
		data, err := os.ReadFile(filepath.Join(dir, headerFile))
		if err != nil {
			return OriginUnknown, true, err
		}
		var h header
		if err := json.Unmarshal(data, &h); err != nil {
			return OriginUnknown, true, nil
		}
		root, err := ownRoot(z.Root())
		if err != nil {
			return OriginUnknown, true, err
		}
		switch originOf(Session{ID: session, Workspaces: h.WorkspacePaths}, root) {
		case originInside:
			return OriginInside, true, nil
		case originOutside:
			return OriginOutside, true, nil
		}
		return OriginUnknown, true, nil
	}
	return OriginUnknown, false, nil
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
