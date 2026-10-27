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

package view

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// Snapshot binds a server to a pulled snapshot its command holds a lease on.
// The snapshot's evidence never changes, and every read of it is checked: a
// fold reaches exactly the head its marker names, every landed file a read
// uses is read to its end, and a read that fails is an error, never a record,
// a time or a text left out.
type Snapshot struct {
	// Conversation, Session, Round and Digest are what the snapshot's
	// marker names.
	Conversation string
	Session      string
	Round        uint64
	Digest       string
	// Landed maps each landed sequence to its file, relative to the root,
	// as the snapshot's frozen manifest lists it. Reads look nowhere else.
	Landed map[uint64]string
	// Store maps each provider store file the frozen manifest lists,
	// relative to the root with forward slashes, to the SHA-256 of what it
	// holds. What a skeleton's bodies keep is read from these alone.
	Store map[string]string
	// Fatal, when set, is told of every read of the evidence that failed,
	// as it fails. The page stops on it.
	Fatal func(error)
}

// ErrEvidence is a read of a snapshot's evidence that failed: the files the
// snapshot holds are not whole, or not what its marker names.
var ErrEvidence = errors.New("view: the snapshot's evidence does not read whole")

// ErrNoRecord is a record asked for that the evidence does not hold, which
// in a snapshot is a question about evidence that is whole.
var ErrNoRecord = errors.New("view: no such record")

// SetSnapshot binds the server to a pulled snapshot. It is set before the
// server serves anything.
func (s *Server) SetSnapshot(sn *Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap = sn
}

// evidenceError wraps a failed read of snapshot evidence and reports it.
func (sn *Snapshot) evidenceError(format string, args ...any) error {
	err := fmt.Errorf("%w: %s", ErrEvidence, fmt.Sprintf(format, args...))
	if sn.Fatal != nil {
		sn.Fatal(err)
	}
	return err
}

// whole is one of a snapshot's files as a read takes it: its bytes as Session
// Data, a skeleton's through its compression, its header and records, and
// the identity of the file read.
type whole struct {
	data []byte
	hdr  sessiondata.Header
	recs []*sessiondata.Record
	info os.FileInfo
}

// read reads one of the snapshot's files, the one its inventory lists at
// seq, through its closing line and to its end. It is opened without
// following a link, and as the file want describes when that is set: a name
// replaced since the conversation was loaded is damage. Its header must name
// the snapshot's session, that sequence, and the kind its name says.
// Nothing read short of all that is used.
func (sn *Snapshot) read(path string, seq uint64, want os.FileInfo) (*whole, error) {
	f, err := storage.OpenEvidence(path, want)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var r io.Reader = f
	if sessiondata.IsSkeleton(path) {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		r = zr
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	rd, err := sessiondata.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	w := &whole{data: data, info: info}
	for {
		rec, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		w.recs = append(w.recs, rec)
	}
	if err := rd.Complete(); err != nil {
		return nil, err
	}
	w.hdr = rd.Header()
	kind, _ := storage.LandedKind(filepath.Base(path))
	switch {
	case sn.Session != "" && w.hdr.Session != sn.Session:
		return nil, fmt.Errorf("its header names session %q", w.hdr.Session)
	case w.hdr.Seq != seq:
		return nil, fmt.Errorf("its header names sequence %d, and it is listed at %d", w.hdr.Seq, seq)
	case kind != "" && w.hdr.Kind != kind:
		return nil, fmt.Errorf("its header names kind %q, and its name %q", w.hdr.Kind, kind)
	}
	return w, nil
}

// readSnapshot reads one of a conversation's snapshot files as the file it
// read when it was loaded.
func (c *Conversation) readSnapshot(seq uint64, path string) (*whole, error) {
	w, err := c.snap.read(path, seq, c.ids[seq])
	if err != nil {
		return nil, c.snap.evidenceError("%s: %v", path, err)
	}
	return w, nil
}

// snapshotPaths is a snapshot's landed inventory as absolute paths.
func (s *Server) snapshotPaths() map[uint64]string {
	out := make(map[uint64]string, len(s.snap.Landed))
	for seq, rel := range s.snap.Landed {
		out[seq] = filepath.Join(s.zone.Root(), filepath.FromSlash(rel))
	}
	return out
}

// inventory is the snapshot's landed files as storage lists a session's, in
// sequence order, from its frozen inventory alone.
func (sn *Snapshot) inventory(root string) []storage.LandedFile {
	out := make([]storage.LandedFile, 0, len(sn.Landed))
	for seq, rel := range sn.Landed {
		lf := storage.LandedFile{Path: filepath.Join(root, filepath.FromSlash(rel)), Seq: seq, Skeleton: sessiondata.IsSkeleton(rel)}
		if parts := strings.Split(rel, "/"); len(parts) == 4 {
			switch parts[1] {
			case "streams":
				lf.Stream = parts[2]
			case "runs":
				lf.RunID = parts[2]
			}
		}
		out = append(out, lf)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// landedFiles lists the conversation's landed files. A snapshot's are its
// frozen inventory and nothing else in its session.
func (c *Conversation) landedFiles() ([]storage.LandedFile, error) {
	if c.snap == nil {
		return storage.LandedFiles(c.zone, c.Session)
	}
	return c.snap.inventory(c.zone.Root()), nil
}

// eachRecord gives fn every record of a landed file, by row. A snapshot's
// file is read whole and checked, and a failure is an error the page is told
// of; on any other root what does not read ends the file, as it always did.
func (c *Conversation) eachRecord(lf storage.LandedFile, fn func(row uint64, rec *sessiondata.Record)) error {
	if c.snap != nil {
		w, err := c.readSnapshot(lf.Seq, lf.Path)
		if err != nil {
			return err
		}
		for i, rec := range w.recs {
			fn(uint64(i+1), rec)
		}
		return nil
	}
	f, err := storage.OpenLanded(lf.Path)
	if err != nil {
		return nil
	}
	defer f.Close()
	rd, err := sessiondata.NewReader(f)
	if err != nil {
		return nil
	}
	for row := uint64(1); ; row++ {
		rec, err := rd.Next()
		if err != nil {
			return nil
		}
		fn(row, rec)
	}
}

// loadStatus is the status a failed load is answered with: damage to a
// snapshot's evidence is the server's failure, never a conversation that is
// not there.
func loadStatus(err error) int {
	if errors.Is(err, ErrEvidence) {
		return http.StatusInternalServerError
	}
	return http.StatusNotFound
}

// snapshotStore is a snapshot's provider store, as its frozen manifest
// lists it. What the manifest lists is there, or the snapshot is damaged:
// a listed file that does not read is an error, never one the store does
// not keep.
type snapshotStore struct {
	root    string
	session string
	listed  map[string]string
	// tips maps each listed tip's file name to its file of the newest
	// generation listed.
	tips map[string]string
}

// tipIndex maps each tip file name a session's listed store holds to its
// file of the newest generation listed, as the store's own reader looks
// first. A load builds it once, so a lookup never walks the listing.
func tipIndex(listed map[string]string, session string) map[string]string {
	out, best := map[string]string{}, map[string]int{}
	for rel := range listed {
		parts := strings.Split(rel, "/")
		if len(parts) != 5 || parts[0] != session || parts[1] != "provider" || parts[2] != "tips" {
			continue
		}
		if g, err := strconv.Atoi(parts[3]); err == nil && g > best[parts[4]] {
			best[parts[4]], out[parts[4]] = g, rel
		}
	}
	return out
}

// tip is the listed file that keeps a tip of record id.
func (s snapshotStore) tip(id string) string { return s.tips[storage.TipName(id)] }

func (s snapshotStore) piece(sha string) string {
	rel := s.session + "/provider/pieces/" + sha + ".gz"
	if _, ok := s.listed[rel]; ok {
		return rel
	}
	return ""
}

func (s snapshotStore) HasTip(id string) (bool, error) { return s.tip(id) != "", nil }

func (s snapshotStore) HasPiece(sha string) (bool, error) { return s.piece(sha) != "", nil }

func (s snapshotStore) Tip(id, sha string) ([]byte, error) {
	rel := s.tip(id)
	if rel == "" {
		return nil, fmt.Errorf("the manifest lists no tip of %s", id)
	}
	return s.read(rel, sha)
}

func (s snapshotStore) Piece(sha string) ([]byte, error) {
	rel := s.piece(sha)
	if rel == "" {
		return nil, fmt.Errorf("the manifest lists no piece %s", sha)
	}
	return s.read(rel, sha)
}

// read reads a listed file, never through a link, and checks it holds what
// the manifest says and what the skeleton asks for.
func (s snapshotStore) read(rel, sha string) ([]byte, error) {
	if s.listed[rel] != sha {
		return nil, fmt.Errorf("%s holds %s, and the skeleton asks for %s", rel, s.listed[rel], sha)
	}
	f, err := storage.OpenEvidence(filepath.Join(s.root, filepath.FromSlash(rel)), nil)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", rel, err)
	}
	defer zr.Close()
	b, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", rel, err)
	}
	if digestOf(b) != sha {
		return nil, fmt.Errorf("%s does not hold what the manifest lists", rel)
	}
	return b, nil
}
