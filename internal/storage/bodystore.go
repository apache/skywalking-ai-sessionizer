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
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

// BodyStore keeps the provider body bytes a removal took out of a session's
// landed files that later bodies are still built from: the chain tips at
// each removal, and the removed pieces a later body cites. Each file is
// compressed with gzip and checked whenever it is read against the SHA-256
// of what it holds. A piece is named by that SHA-256. A tip is named by the
// SHA-256 of its record's id: an identical body under another id may be one
// no removal kept, because it could not be rebuilt.
//
//	<session>/provider/tips/<generation>/<sha256 of the record id>.gz
//	<session>/provider/pieces/<sha256>.gz
//
// A removal writes the tips of its own generation beside the ones before,
// and deletes the earlier ones once its files are deleted. A reader looks for
// a tip in every generation there, the newest first: while a removal runs, a
// body not yet deleted can be built from a tip of the generation before. A
// tip's record and bytes do not change, so any copy is the right one.
type BodyStore struct {
	dir string
}

// BodyStore is a session's store.
func (z *Zone) BodyStore(session string) *BodyStore {
	return &BodyStore{dir: filepath.Join(z.SessionDir(session), "provider")}
}

// TipsDir is where the tips of a generation are kept.
func (s *BodyStore) TipsDir(generation int) string {
	return filepath.Join(s.dir, "tips", strconv.Itoa(generation))
}

// PiecesDir is where the pieces are kept.
func (s *BodyStore) PiecesDir() string { return filepath.Join(s.dir, "pieces") }

// Generations lists the generations whose tips the store holds, the newest
// first.
func (s *BodyStore) Generations() ([]int, error) {
	items, err := os.ReadDir(filepath.Join(s.dir, "tips"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []int
	for _, it := range items {
		if g, err := strconv.Atoi(it.Name()); err == nil && it.IsDir() && g > 0 {
			out = append(out, g)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(out)))
	return out, nil
}

// tipName is the file a tip of the record id is kept in.
func tipName(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:]) + ".gz"
}

// Tip reads the body of record id, with this SHA-256, that a removal kept as
// a chain tip. A damaged copy is passed over for one of an earlier
// generation, and is reported only when no copy is whole.
func (s *BodyStore) Tip(id, sha string) ([]byte, error) {
	generations, err := s.Generations()
	if err != nil {
		return nil, err
	}
	var damaged error
	for _, g := range generations {
		b, err := readNamed(filepath.Join(s.TipsDir(g), tipName(id)), sha)
		switch {
		case err == nil:
			return b, nil
		case !errors.Is(err, fs.ErrNotExist) && damaged == nil:
			damaged = err
		}
	}
	if damaged != nil {
		return nil, damaged
	}
	return nil, fmt.Errorf("storage: no tip of %s: %w", id, fs.ErrNotExist)
}

// HasTip says whether the store keeps a tip of record id, in any generation,
// without reading it. A file that cannot be looked at is an error, not one
// the store does not keep.
func (s *BodyStore) HasTip(id string) (bool, error) {
	generations, err := s.Generations()
	if err != nil {
		return false, err
	}
	for _, g := range generations {
		if ok, err := exists(filepath.Join(s.TipsDir(g), tipName(id))); ok || err != nil {
			return ok, err
		}
	}
	return false, nil
}

// HasPiece says whether the store keeps the piece with this SHA-256, without
// reading it, as HasTip does.
func (s *BodyStore) HasPiece(sha string) (bool, error) {
	return exists(filepath.Join(s.PiecesDir(), sha+".gz"))
}

func exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// Piece reads the removed piece with this SHA-256.
func (s *BodyStore) Piece(sha string) ([]byte, error) {
	return readNamed(filepath.Join(s.PiecesDir(), sha+".gz"), sha)
}

// readNamed reads a stored file and checks it holds bytes with this SHA-256.
func readNamed(path, sha string) ([]byte, error) {
	b, err := readStored(path)
	if err != nil {
		return nil, err
	}
	if digest(b) != sha {
		return nil, fmt.Errorf("storage: %s does not hold the bytes it is kept for", path)
	}
	return b, nil
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// PutTip keeps the body of record id as a tip of generation, made durable
// with sync.
func (s *BodyStore) PutTip(generation int, id string, b []byte, sync func(string) error) error {
	return s.put(filepath.Join(s.TipsDir(generation), tipName(id)), b, sync)
}

// PutPiece keeps a removed piece, made durable with sync, before the body
// that cites it lands. sha must be the SHA-256 of b.
func (s *BodyStore) PutPiece(sha string, b []byte, sync func(string) error) error {
	if digest(b) != sha {
		return fmt.Errorf("storage: a piece kept as %s is not what it holds", sha)
	}
	return s.put(filepath.Join(s.PiecesDir(), sha+".gz"), b, sync)
}

// PutPieces keeps every piece of pieces, by its SHA-256, as PutPiece does.
func (s *BodyStore) PutPieces(pieces map[string][]byte, sync func(string) error) error {
	for sha, b := range pieces {
		if err := s.PutPiece(sha, b, sync); err != nil {
			return err
		}
	}
	return nil
}

// put keeps b at path, replacing a file there, makes it durable with every
// directory it is in up to the session's, and checks it reads back.
//
// A file already there is replaced rather than kept. The attempt that wrote
// it may have stopped on a failed sync, and a later sync that succeeds does
// not say the name reached the disk. Replacing it is safe: its bytes are the
// same, and nothing depends on it yet, since a removal keeps its tips before
// its intent and a collector keeps a piece only until a landed body cites it.
// The directories above are synced each time for the same reason: one an
// earlier attempt made may not be on the disk yet.
func (s *BodyStore) put(path string, b []byte, sync func(string) error) error {
	if err := WriteAtomicDurable(path, PermState, func(w io.Writer) error {
		zw := gzip.NewWriter(w)
		if _, err := zw.Write(b); err != nil {
			return err
		}
		return zw.Close()
	}, sync); err != nil {
		return err
	}
	top := filepath.Dir(s.dir)
	for dir := filepath.Dir(filepath.Dir(path)); ; dir = filepath.Dir(dir) {
		if err := syncWith(sync, dir); err != nil {
			return err
		}
		if dir == top || dir == filepath.Dir(dir) {
			break
		}
	}
	got, err := readStored(path)
	if err != nil {
		return fmt.Errorf("storage: %s does not read back: %w", path, err)
	}
	if !bytes.Equal(got, b) {
		return fmt.Errorf("storage: %s does not read back as written", path)
	}
	return nil
}

func readStored(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("storage: %s: %w", path, err)
	}
	defer zr.Close()
	b, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("storage: %s: %w", path, err)
	}
	return b, nil
}
