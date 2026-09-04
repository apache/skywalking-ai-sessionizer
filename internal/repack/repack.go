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

// Package repack re-cuts the landed files of a storage root into another root
// under a different file budget.
//
// A landed file is cut once and never rewritten: every round addresses a
// record by file and line and binds itself to file digests, so re-cutting in
// place would break every reference. Changing the cut therefore means a new
// root, where each record keeps its bytes but lands in a new file at a new
// position, and where the chains are built again on the new files.
package repack

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// Stats reports what a repack of one session did.
type Stats struct {
	FilesIn  int
	FilesOut int
	Records  int
	Bytes    int64
}

// slot is one landing directory and file prefix: a stream's transcripts, a
// stream's sidecars, a run's journal, manifest or script. Records are re-cut
// within a slot and never across slots.
type slot struct {
	rel    string // directory relative to the session directory
	prefix string
	files  []storage.LandedFile
}

// Session re-cuts every landed file of session from src into dst so that no
// file exceeds budget bytes, except one holding a single record larger than
// that. Records keep their bytes and their order; the session's cursors are
// carried over so a collector can continue into dst; the index and the chain
// are not copied, because both are derived and are rebuilt on the new files.
func Session(src, dst *storage.Zone, session string, budget int64, now time.Time) (*Stats, error) {
	if budget <= 0 {
		return nil, errors.New("repack: the budget must be positive")
	}
	if src.Root() == dst.Root() {
		return nil, errors.New("repack: the destination must be a different root; landed files are never rewritten in place")
	}
	files, err := storage.LandedFiles(src, session)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("repack: session %s has no landed files", session)
	}
	srcDir, dstDir := src.SessionDir(session), dst.SessionDir(session)
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return nil, err
	}

	// Group by slot, keeping slots in the order their first file landed and
	// files within a slot in sequence order, which is source order.
	var slots []*slot
	byKey := map[string]*slot{}
	for _, lf := range files {
		rel, err := filepath.Rel(srcDir, filepath.Dir(lf.Path))
		if err != nil {
			return nil, err
		}
		prefix := strings.SplitN(filepath.Base(lf.Path), "-", 2)[0]
		key := rel + "/" + prefix
		s, ok := byKey[key]
		if !ok {
			s = &slot{rel: rel, prefix: prefix}
			byKey[key] = s
			slots = append(slots, s)
		}
		s.files = append(s.files, lf)
	}

	state, err := storage.LoadSessionState(src.SessionStatePath(session), session)
	if err != nil {
		return nil, err
	}
	state.NextSeq = 1
	st := &Stats{FilesIn: len(files)}
	for _, s := range slots {
		last, err := repackSlot(s, filepath.Join(dstDir, s.rel), budget, state, st)
		if err != nil {
			return nil, err
		}
		if err := carryCursor(filepath.Join(srcDir, s.rel, s.prefix+".cursor"),
			filepath.Join(dstDir, s.rel, s.prefix+".cursor"), last, now); err != nil {
			return nil, err
		}
	}
	if err := state.Save(dst.SessionStatePath(session), now); err != nil {
		return nil, err
	}
	return st, nil
}

// out is one landed file being written.
type out struct {
	tmp   *os.File
	w     *sessiondata.Writer
	final string
	size  int64
	n     int
}

func repackSlot(s *slot, outDir string, budget int64, state *storage.SessionState, st *Stats) (uint64, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return 0, err
	}
	var cur *out
	var last uint64
	finish := func() error {
		if cur == nil {
			return nil
		}
		err := closeOut(cur)
		cur = nil
		if err == nil {
			st.FilesOut++
		}
		return err
	}
	for _, lf := range s.files {
		f, err := os.Open(lf.Path)
		if err != nil {
			return 0, err
		}
		r, err := sessiondata.NewReader(f)
		if err != nil {
			f.Close()
			return 0, fmt.Errorf("repack: %s: %w", lf.Path, err)
		}
		hdr := r.Header()
		for {
			line, err := r.NextRaw()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				f.Close()
				return 0, fmt.Errorf("repack: %s: %w", lf.Path, err)
			}
			need := int64(len(line) + 1)
			// Cut before a record that would push the file past the budget,
			// unless the file is empty: a single record larger than the budget
			// is landed whole, as the collector does.
			if cur != nil && cur.n > 0 && cur.size+need > budget {
				if err := finish(); err != nil {
					f.Close()
					return 0, err
				}
			}
			if cur == nil {
				seq := state.Take()
				last = seq
				o, err := openOut(outDir, s.prefix, hdr, seq)
				if err != nil {
					f.Close()
					return 0, err
				}
				cur = o
			}
			if err := cur.w.WriteRaw(line); err != nil {
				f.Close()
				return 0, err
			}
			cur.size += need
			cur.n++
			st.Records++
			st.Bytes += int64(len(line))
		}
		f.Close()
	}
	return last, finish()
}

// openOut starts a landed file under a new sequence, with the header of the
// file its first record came from. Everything in that header is constant for
// the slot except the sequence.
func openOut(dir, prefix string, hdr sessiondata.Header, seq uint64) (*out, error) {
	hdr.Seq = seq
	at, err := time.Parse(time.RFC3339Nano, hdr.At)
	if err != nil {
		at = time.Now()
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return nil, err
	}
	w, err := sessiondata.NewWriter(tmp, &hdr)
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, err
	}
	return &out{tmp: tmp, w: w, final: filepath.Join(dir, storage.LandedName(prefix, storage.Stamp(at), seq))}, nil
}

// closeOut finishes a landed file the way the collector does: closing line,
// fsync, read-only, then the rename that makes it visible.
func closeOut(o *out) (err error) {
	name := o.tmp.Name()
	defer func() {
		if err != nil {
			o.tmp.Close()
			os.Remove(name)
		}
	}()
	if err = o.w.Close(); err != nil {
		return err
	}
	if err = o.tmp.Sync(); err != nil {
		return err
	}
	if err = o.tmp.Close(); err != nil {
		return err
	}
	if err = os.Chmod(name, storage.PermLanded); err != nil {
		return err
	}
	if err = os.Rename(name, o.final); err != nil {
		return err
	}
	if d, derr := os.Open(filepath.Dir(o.final)); derr == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// carryCursor copies a slot's cursor into the new root with its last landed
// sequence updated, so a collector pointed at the new root continues from
// where the old one stopped instead of landing everything again. The rest of
// the cursor describes the source file, which the repack did not touch.
func carryCursor(srcPath, dstPath string, last uint64, now time.Time) error {
	data, err := os.ReadFile(srcPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "last_seq "):
			lines[i] = fmt.Sprintf("last_seq      %d", last)
		case strings.HasPrefix(l, "updated_at "):
			lines[i] = "updated_at    " + now.UTC().Format(time.RFC3339Nano)
		}
	}
	return storage.WriteAtomic(dstPath, storage.PermState, func(w io.Writer) error {
		_, err := io.WriteString(w, strings.Join(lines, "\n")+"\n")
		return err
	})
}
