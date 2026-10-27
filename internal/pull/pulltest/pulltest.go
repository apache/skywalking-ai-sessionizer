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

// Package pulltest gives pull's tests their fixtures: the manifest of a
// settled storage root's conversation, and a tree holding exactly that
// manifest's files. It reads a root no writer is changing, so it has none of
// a live source's rules; tests only.
package pulltest

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/internal/pull/format"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// Manifest lists conversation's evidence in root at its head round.
func Manifest(root, conversation string) (*format.Manifest, error) {
	return ManifestAt(root, conversation, 0)
}

// ManifestAt lists conversation's evidence in root as it stood at round, or
// at the head for 0: the rounds through it and the landed files its range
// reached. A root no removal touched holds the same bytes for them then and
// now.
func ManifestAt(root, conversation string, round uint64) (*format.Manifest, error) {
	zone := storage.NewZone(root)
	chain := sessionflow.OpenChain(root, conversation)
	rounds, err := chain.List()
	if err != nil {
		return nil, err
	}
	if round > 0 && round < uint64(len(rounds)) {
		rounds = rounds[:round]
	}
	if len(rounds) == 0 {
		return nil, fmt.Errorf("pulltest: %s has no rounds", conversation)
	}
	head, err := chain.Open(rounds[len(rounds)-1].Path)
	if err != nil {
		return nil, err
	}
	m := &format.Manifest{
		Conversation: conversation, Session: head.Header.Session,
		Head:    format.Head{Round: head.Header.Round, Digest: format.CommitDigest(head.Commit.Digest), ThroughSeq: head.Header.ThroughSeq},
		Formats: format.Formats{SD: sessiondata.Schema, SF: sessionflow.Schema},
	}
	generation, _, err := storage.RemovalGeneration(zone, m.Session)
	if err != nil {
		return nil, err
	}
	m.Generation = int64(generation)
	add := func(path string, e format.Entry) error {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		e.File = filepath.ToSlash(rel)
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		e.Bytes = uint64(info.Size())
		stored, err := storage.FileDigest(path)
		if err != nil {
			return err
		}
		e.Digest = format.StoredDigest(stored)
		m.Files = append(m.Files, e)
		return nil
	}
	for _, rf := range rounds {
		if err := add(rf.Path, format.Entry{Kind: format.KindRound, Round: rf.Round}); err != nil {
			return nil, err
		}
	}
	landed, err := storage.LandedFiles(zone, m.Session)
	if err != nil {
		return nil, err
	}
	for _, lf := range landed {
		if lf.Seq > m.Head.ThroughSeq {
			continue
		}
		e := format.Entry{Kind: format.KindLanded, Seq: lf.Seq}
		if lf.Skeleton {
			e.Kind = format.KindSkeleton
			original, err := storage.LandedDigest(lf)
			if err != nil {
				return nil, err
			}
			e.LandedDigest = format.OriginalDigest(original)
		}
		if err := add(lf.Path, e); err != nil {
			return nil, err
		}
	}
	store := filepath.Join(zone.SessionDir(m.Session), "provider")
	err = filepath.WalkDir(store, func(path string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return filepath.SkipDir
		}
		if err != nil || d.IsDir() || strings.HasPrefix(d.Name(), ".tmp-") {
			return err
		}
		content, err := Content(path)
		if err != nil {
			return err
		}
		return add(path, format.Entry{Kind: format.KindStore, Content: format.ContentDigest(content)})
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(m.Files, func(x, y format.Entry) int { return strings.Compare(x.File, y.File) })
	return m, nil
}

// Content is the SHA-256 of what a gzip file holds.
func Content(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, zr); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Tree copies every file m lists from root into tree, read-only as landed
// files are, and saves m as its manifest.
func Tree(root string, m *format.Manifest, tree string) error {
	for _, e := range m.Files {
		dest := filepath.Join(tree, filepath.FromSlash(e.File))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(e.File)))
		if err != nil {
			return err
		}
		if err := os.WriteFile(dest, data, 0o444); err != nil {
			return err
		}
	}
	return SaveManifest(tree, m)
}

// SaveManifest writes m as tree's manifest, replacing one already there.
func SaveManifest(tree string, m *format.Manifest) error {
	path := filepath.Join(tree, format.ManifestFile)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o444)
	if err != nil {
		return err
	}
	if err := format.WriteManifest(f, m); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Restamp rewrites file in tree with data, and its entry in m and in the
// saved manifest with the new size and SHA-256, so a test damages what a
// file holds while every stored digest still agrees.
func Restamp(tree string, m *format.Manifest, file string, data []byte) error {
	path := filepath.Join(tree, filepath.FromSlash(file))
	if err := os.Remove(path); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o444); err != nil {
		return err
	}
	i := slices.IndexFunc(m.Files, func(e format.Entry) bool { return e.File == file })
	if i < 0 {
		return fmt.Errorf("pulltest: %s is not listed", file)
	}
	sum := sha256.Sum256(data)
	m.Files[i].Bytes, m.Files[i].Digest = uint64(len(data)), format.StoredDigest(hex.EncodeToString(sum[:]))
	return SaveManifest(tree, m)
}

// Source gives a pull the files of a root, by their names.
type Source struct {
	Root string
	// Asked counts the names given.
	Asked int
	// Change, when set, gives other bytes for a name.
	Change func(name string, data []byte) []byte
}

// Fetch gives every name's bytes, read from the root.
func (s *Source) Fetch(_ context.Context, _ string, names []string, put func(string, io.Reader) error) error {
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(s.Root, filepath.FromSlash(name)))
		if err != nil {
			return err
		}
		if s.Change != nil {
			data = s.Change(name, data)
		}
		s.Asked++
		if err := put(name, bytes.NewReader(data)); err != nil {
			return err
		}
	}
	return nil
}
