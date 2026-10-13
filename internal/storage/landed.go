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

package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// LandedFile is one landed record file in the zone.
type LandedFile struct {
	Path   string
	Seq    uint64
	Stream string // set for a stream's records
	RunID  string // set for a workflow run's records
	// Neither is set for a file at the session level: the provider bodies,
	// which belong to no one stream or run.

	// Skeleton says Path is the skeleton that took the landed file's place
	// when retention removed it. The file keeps its identity: its name with
	// the landed extension (LandedPath) and its digest (LandedDigest).
	Skeleton bool
}

// LandedFiles lists every landed record file for a session, in sequence order.
//
// The sequence is monotonic across every stream and run in the session, so
// ordering by it reproduces the order the collector landed them. That is the
// order the index holds, and the order a rebuild must follow.
//
// This describes the zone's own layout, not any runtime's, which is why it
// lives here rather than in an adapter.
func LandedFiles(z *Zone, session string) ([]LandedFile, error) {
	var out []LandedFile
	root := z.SessionDir(session)

	add := func(dir, owner string, isRun bool) error {
		items, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		for _, it := range items {
			name := it.Name()
			if it.IsDir() {
				continue
			}
			seq, ok := LandedFileSeq(name)
			if !ok {
				continue
			}
			lf := LandedFile{Path: filepath.Join(dir, name), Seq: seq, Skeleton: sessiondata.IsSkeleton(name)}
			if isRun {
				lf.RunID = owner
			} else {
				lf.Stream = owner
			}
			out = append(out, lf)
		}
		return nil
	}

	for _, group := range []struct {
		sub   string
		isRun bool
	}{{"streams", false}, {"runs", true}} {
		base := filepath.Join(root, group.sub)
		owners, err := os.ReadDir(base)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, o := range owners {
			if !o.IsDir() {
				continue
			}
			if err := add(filepath.Join(base, o.Name()), o.Name(), group.isRun); err != nil {
				return nil, err
			}
		}
	}

	if err := add(z.ProviderDir(session), "", false); err != nil {
		return nil, err
	}

	// A removal writes a file's skeleton before it deletes the file, so both
	// can be there for a while. Each reads as the same index, and the landed
	// file is read while it is there.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Seq != out[j].Seq {
			return out[i].Seq < out[j].Seq
		}
		return !out[i].Skeleton && out[j].Skeleton
	})
	kept := out[:0]
	for i, lf := range out {
		if i > 0 && LandedPath(lf.Path) == LandedPath(out[i-1].Path) {
			continue
		}
		kept = append(kept, lf)
	}
	return kept, nil
}

// landedNameRe matches "<kind>-<stamp>-<seq>.sd", and the skeleton that takes
// its place, "<kind>-<stamp>-<seq>.sk".
var landedNameRe = regexp.MustCompile(`-(\d{6,})\.s[dk]$`)

// LandedPath is the path a file was landed under: a skeleton's path with the
// landed extension. Everything that names a landed file, such as push.state,
// metrics.state and a round's file list, names it by this path, so a file
// keeps its name after retention removes it.
func LandedPath(path string) string {
	if sessiondata.IsSkeleton(path) {
		return strings.TrimSuffix(path, sessiondata.SkeletonExt) + sessiondata.Ext
	}
	return path
}

// SkeletonPath is where the skeleton of the landed file at path is written.
func SkeletonPath(path string) string {
	return strings.TrimSuffix(path, sessiondata.Ext) + sessiondata.SkeletonExt
}

// OpenLanded opens a landed file for reading, or the skeleton that took its
// place when a removal deleted the file after its path was listed. A reader
// that holds no session lock, such as the page, lists a file a removal may
// delete before it opens it.
func OpenLanded(path string) (io.ReadCloser, error) {
	f, err := sessiondata.Open(path)
	if errors.Is(err, fs.ErrNotExist) && !sessiondata.IsSkeleton(path) {
		return sessiondata.Open(SkeletonPath(path))
	}
	return f, err
}

// LandedDigest is the SHA-256 a landed file had: its own digest, or for a
// skeleton the digest it recorded of the file it took the place of. A round
// binds a file by this digest, before a removal and after.
func LandedDigest(lf LandedFile) (string, error) {
	path := lf.Path
	if !lf.Skeleton {
		d, err := FileDigest(path)
		// A removal deleted the file after it was listed: its skeleton
		// recorded the digest.
		if !errors.Is(err, fs.ErrNotExist) {
			return d, err
		}
		path = SkeletonPath(path)
	}
	f, err := sessiondata.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	r, err := sessiondata.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("storage: skeleton %s: %w", lf.Path, err)
	}
	h := r.Header()
	if h.Removed == nil || h.Removed.Digest == "" {
		return "", fmt.Errorf("storage: skeleton %s does not say which file it took the place of", lf.Path)
	}
	return h.Removed.Digest, nil
}

// LandedFileSeq extracts the sequence from a landed filename or a skeleton's.
func LandedFileSeq(name string) (uint64, bool) {
	m := landedNameRe.FindStringSubmatch(name)
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// FileDigest returns the SHA-256 of a file as hex.
//
// A landed file is written once and never appended to, so its digest is fixed
// from the moment it appears. That is what lets a parse round bind itself to
// the evidence it read without re-reading anything it read before.
func FileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
