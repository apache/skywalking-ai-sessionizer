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
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// LandedFile is one landed record file in the zone.
type LandedFile struct {
	Path   string
	Seq    uint64
	Stream string // set for a stream's records
	RunID  string // set for a workflow run's records
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
			if it.IsDir() || !strings.HasSuffix(name, ".sd") {
				continue
			}
			seq, ok := LandedFileSeq(name)
			if !ok {
				continue
			}
			lf := LandedFile{Path: filepath.Join(dir, name), Seq: seq}
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

	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

// landedNameRe matches "<kind>-<stamp>-<seq>.jsonl".
var landedNameRe = regexp.MustCompile(`-(\d{6,})\.sd$`)

// LandedFileSeq extracts the sequence from a landed filename.
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
