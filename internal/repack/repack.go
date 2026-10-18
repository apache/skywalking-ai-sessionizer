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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
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

// slot is a run of files of one landing directory and file prefix, next to
// each other in sequence order, whose headers are the same apart from the
// sequence and the time: a stream's transcripts, a stream's sidecars, a run's
// journal, manifest or script. Records are re-cut within a slot and never
// across slots.
//
// A file's header says where its records came from. Two sources can write
// the files of one directory and prefix, such as two recorder directories,
// each behind its own cursor, and an upgrade can change an adapter's
// version. Cut together, their records would land under the first file's
// header. A header that changes and changes back starts a new slot each
// time: cut together, the two runs with the first header would put the
// records between them after both, and a provider body before a body it
// copies.
type slot struct {
	rel    string // directory relative to the session directory
	prefix string
	id     string // the header without its sequence and its time
	files  []storage.LandedFile
	hdrs   []sessiondata.Header // each file's header
	outs   []*output
}

// output is one file a repack writes, planned before any is written: how
// many records it takes, in slot order, from where the file before it ended,
// and which file of the slot its last record came from. It carries that
// file's header, and so the time that file was collected.
type output struct {
	records int
	last    int    // the index in the slot's files
	landed  uint64 // the sequence that file had in the source
	seq     uint64
}

// CheckSource refuses a root with a session retention removed files of, before
// any session is written. A skeleton keeps no records to re-cut, and a chain
// built again in the new root would have nothing to bind them to. A removal
// in progress is refused too: the next pipeline pass finishes it.
//
// A root that holds a retired session is refused whole, whichever sessions
// are named: a new root without its marker would collect it again under the
// id a receiver may still hold.
func CheckSource(src *storage.Zone, sessions []string) error {
	if err := checkRetired(src); err != nil {
		return err
	}
	for _, session := range sessions {
		files, err := storage.LandedFiles(src, session)
		if err != nil {
			return err
		}
		if err := checkRemoved(src, session, files); err != nil {
			return err
		}
	}
	return nil
}

// checkRetired refuses a root that holds a retired session.
func checkRetired(src *storage.Zone) error {
	retired, err := storage.RetiredSessions(src)
	if err != nil {
		return err
	}
	if len(retired) > 0 {
		return fmt.Errorf("repack: the root holds %d retired session(s) under %s; a new root without their markers would collect them again, so this root cannot be repacked",
			len(retired), storage.RetiredDir)
	}
	return nil
}

func checkRemoved(src *storage.Zone, session string, files []storage.LandedFile) error {
	if r, err := storage.LoadRemoving(src.RemovingPath(session)); err != nil {
		return err
	} else if r != nil {
		return fmt.Errorf("repack: a removal of session %s is in progress; run a pipeline pass to finish it first", session)
	}
	for _, lf := range files {
		if lf.Skeleton {
			return fmt.Errorf("repack: retention removed files of session %s; a removed file keeps no records to re-cut, so this root cannot be repacked", session)
		}
	}
	return nil
}

// CheckDestination refuses a populated root before any session is written.
// A prior chain binds to its original landed bytes and cannot be reused.
// The root lock's directory is not content: the repack holds it while it
// checks and writes.
func CheckDestination(root string) error {
	items, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("repack: destination: %w", err)
	}
	for _, it := range items {
		if it.Name() != storage.RootLockDir {
			return fmt.Errorf("repack: destination %s is not empty; use a new or empty directory", root)
		}
	}
	return nil
}

// Session re-cuts every landed file of session from src into dst so that no
// file exceeds budget bytes, header and closing line included, except one
// holding a single record larger than that. Records keep their bytes and their
// order; the session's cursors are carried over so a collector can continue
// into dst; the index and the chain are not copied, because both are derived
// and are rebuilt on the new files.
//
// The new files keep the session's idle clock, which retention reads from the
// header of the file with the highest sequence. Each file says when the newest
// file its records came from was collected, and the files are numbered in the
// order their records landed.
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
	if err := checkRemoved(src, session, files); err != nil {
		return nil, err
	}
	// Also when Session is called without the command's whole-root check,
	// as the scenario runner calls it. A destination that retired the
	// session never writes it again either.
	if err := checkRetired(src); err != nil {
		return nil, err
	}
	if retired, err := storage.Retired(dst, session); err != nil {
		return nil, err
	} else if retired {
		return nil, fmt.Errorf("repack: session %s is retired in the destination, and is never written again", session)
	}
	srcDir, dstDir := src.SessionDir(session), dst.SessionDir(session)
	// Reserve a new session directory even when Session is called without
	// the command's whole-root check. A retry must never rewrite its files.
	chainDir := filepath.Join(dst.Root(), "_conversations", session)
	if _, err := os.Lstat(chainDir); err == nil {
		return nil, fmt.Errorf("repack: destination already has a chain for %s", session)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(dst.Root(), 0o755); err != nil {
		return nil, err
	}
	if err := os.Mkdir(dstDir, 0o755); err != nil {
		return nil, fmt.Errorf("repack: destination session must be new: %w", err)
	}

	slots, err := group(srcDir, files)
	if err != nil {
		return nil, err
	}
	// Every file is planned before any is written, because a file's
	// sequence is its place among the files of every slot. landedIn maps each
	// source file to the new file its last record goes into, which is where
	// a cursor that named the source file points now.
	landedIn := map[uint64]*output{}
	for _, s := range slots {
		if err := plan(s, budget, landedIn); err != nil {
			return nil, err
		}
	}
	state, err := storage.LoadSessionState(src.SessionStatePath(session), session)
	if err != nil {
		return nil, err
	}
	state.NextSeq = 1
	number(slots, state)
	st := &Stats{FilesIn: len(files)}
	for _, s := range slots {
		if err := write(s, filepath.Join(dstDir, s.rel), st); err != nil {
			return nil, err
		}
	}
	if err := carryCursors(src, dst, session, landedIn, now); err != nil {
		return nil, err
	}
	if err := state.Save(dst.SessionStatePath(session), now); err != nil {
		return nil, err
	}
	return st, nil
}

// group puts every landed file in its slot, keeping slots in the order their
// first file landed and files within a slot in sequence order, which is
// source order. A file joins the slot of the file before it in its directory
// and prefix only when their headers are the same.
func group(srcDir string, files []storage.LandedFile) ([]*slot, error) {
	var slots []*slot
	current := map[string]*slot{}
	for _, lf := range files {
		rel, err := filepath.Rel(srcDir, filepath.Dir(lf.Path))
		if err != nil {
			return nil, err
		}
		hdr, err := readHeader(lf.Path)
		if err != nil {
			return nil, err
		}
		same := hdr
		same.Seq, same.At = 0, ""
		id, err := json.Marshal(same)
		if err != nil {
			return nil, err
		}
		prefix := strings.SplitN(filepath.Base(lf.Path), "-", 2)[0]
		key := rel + "\n" + prefix
		s := current[key]
		if s == nil || s.id != string(id) {
			s = &slot{rel: rel, prefix: prefix, id: string(id)}
			current[key] = s
			slots = append(slots, s)
		}
		s.files = append(s.files, lf)
		s.hdrs = append(s.hdrs, hdr)
	}
	return slots, nil
}

// plan cuts a slot's records into files by what each file takes once
// written, its header and closing line included, as a collector cuts its
// files. The header is measured at its longest, since a file's sequence is
// not known until every slot is planned and its time is that of the newest
// file it takes records from. landedIn gets, for each source file, the new
// file its last record goes into.
func plan(s *slot, budget int64, landedIn map[uint64]*output) error {
	longest := s.hdrs[0]
	longest.Seq = math.MaxUint64
	for _, h := range s.hdrs {
		if len(h.At) > len(longest.At) {
			longest.At = h.At
		}
	}
	var cut *sessiondata.Cut
	var cur *output
	for i, lf := range s.files {
		err := eachLine(lf.Path, func(line []byte) error {
			if cur == nil || !cut.TakeRaw(line) {
				c, err := sessiondata.NewCut(&longest, budget)
				if err != nil {
					return err
				}
				cut, cur = c, &output{}
				cut.TakeRaw(line)
				s.outs = append(s.outs, cur)
			}
			cur.records++
			cur.last, cur.landed = i, lf.Seq
			landedIn[lf.Seq] = cur
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// number gives every planned file its sequence in the order the collector
// landed its records: by the source sequence of the file its last record
// came from. The file holding the newest records then has the highest
// sequence and their time, which is where retention reads how long the
// session has been idle. Within a slot the files keep their records' order.
func number(slots []*slot, state *storage.SessionState) {
	var all []*output
	for _, s := range slots {
		all = append(all, s.outs...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].landed < all[j].landed })
	for _, o := range all {
		o.seq = state.Take()
	}
}

// out is one landed file being written.
type out struct {
	tmp   *os.File
	w     *sessiondata.Writer
	final string
}

// write writes a slot's planned files.
func write(s *slot, outDir string, st *Stats) (err error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	var cur *out
	k, n := 0, 0
	defer func() {
		if err != nil && cur != nil {
			cur.tmp.Close()
			os.Remove(cur.tmp.Name())
		}
	}()
	for _, lf := range s.files {
		err := eachLine(lf.Path, func(line []byte) error {
			if cur == nil {
				hdr := s.hdrs[s.outs[k].last]
				hdr.Seq = s.outs[k].seq
				o, err := openOut(outDir, s.prefix, hdr)
				if err != nil {
					return err
				}
				cur = o
			}
			if err := cur.w.WriteRaw(line); err != nil {
				return err
			}
			st.Records++
			st.Bytes += int64(len(line))
			if n++; n == s.outs[k].records {
				o := cur
				cur = nil
				if err := closeOut(o); err != nil {
					return err
				}
				st.FilesOut++
				k, n = k+1, 0
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// readHeader reads a landed file's header.
func readHeader(path string) (sessiondata.Header, error) {
	f, err := sessiondata.Open(path)
	if err != nil {
		return sessiondata.Header{}, err
	}
	defer f.Close()
	r, err := sessiondata.NewReader(f)
	if err != nil {
		return sessiondata.Header{}, fmt.Errorf("repack: %s: %w", path, err)
	}
	return r.Header(), nil
}

// eachLine hands every record line of a landed file to fn, as it landed.
func eachLine(path string, fn func(line []byte) error) error {
	f, err := sessiondata.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r, err := sessiondata.NewReader(f)
	if err != nil {
		return fmt.Errorf("repack: %s: %w", path, err)
	}
	for {
		line, err := r.NextRaw()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("repack: %s: %w", path, err)
		}
		if err := fn(line); err != nil {
			return err
		}
	}
}

// openOut starts a landed file under the header given, the header of the
// file its last record came from with the new sequence. Its name carries
// that file's time too.
func openOut(dir, prefix string, hdr sessiondata.Header) (*out, error) {
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
	return &out{tmp: tmp, w: w, final: filepath.Join(dir, storage.LandedName(prefix, storage.Stamp(at), hdr.Seq))}, nil
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
	if err = storage.RenameExclusive(name, o.final); err != nil {
		return err
	}
	storage.SyncAfterRename(filepath.Dir(o.final))
	return nil
}

// carryCursors copies every cursor of the session into the new root, so a
// collector pointed at the new root continues each source from where the old
// one stopped instead of landing it again. A directory holds one cursor per
// source: <prefix>.cursor for the first, and <prefix>-<hash>.cursor for each
// other source of the same files, such as a second recorder directory. Each
// names the new file holding the last record of the file its source landed
// last.
//
// An origin relative to the storage root names a source root inside it. A
// collector into the new root writes that root as it writes any root
// outside, and names the source's own cursor by it, so the origin and that
// name are written again as it computes them. Left as they were, the
// collector found no cursor of its own and read the source from its start.
func carryCursors(src, dst *storage.Zone, session string, landedIn map[uint64]*output, now time.Time) error {
	srcDir, dstDir := src.SessionDir(session), dst.SessionDir(session)
	return filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".cursor") {
			return err
		}
		rel, err := filepath.Rel(srcDir, filepath.Dir(path))
		if err != nil {
			return err
		}
		cur, err := storage.LoadCursor(path, storage.CursorAppend, "")
		if err != nil {
			return err
		}
		var last uint64
		if o := landedIn[cur.LastSeq]; o != nil {
			last = o.seq
		}
		name, origin := d.Name(), cur.Origin
		if origin != "" && !filepath.IsAbs(filepath.FromSlash(origin)) {
			origin = storage.CursorOrigin(dst.Root(), filepath.Join(src.Root(), filepath.FromSlash(cur.Origin)))
			prefix := strings.SplitN(strings.TrimSuffix(name, ".cursor"), "-", 2)[0]
			if name == storage.SourceCursorName(prefix, cur.Origin, cur.Source) {
				name = storage.SourceCursorName(prefix, origin, cur.Source)
			}
		}
		return carryCursor(path, filepath.Join(dstDir, rel, name), last, origin, now)
	})
}

// carryCursor copies one cursor with its last landed sequence, its origin
// and its time updated. The rest of the cursor describes the source file,
// which the repack did not touch.
func carryCursor(srcPath, dstPath string, last uint64, origin string, now time.Time) error {
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "last_seq "):
			lines[i] = fmt.Sprintf("last_seq      %d", last)
		case strings.HasPrefix(l, "origin "):
			lines[i] = "origin        " + origin
		case strings.HasPrefix(l, "updated_at "):
			lines[i] = "updated_at    " + now.UTC().Format(time.RFC3339Nano)
		}
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return err
	}
	return storage.WriteAtomic(dstPath, storage.PermState, func(w io.Writer) error {
		_, err := io.WriteString(w, strings.Join(lines, "\n")+"\n")
		return err
	})
}
