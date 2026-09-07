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
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SpoolDir is where OTLP metrics requests wait under a storage root: the
// ones the local adapter derived from landed files, and the ones the
// runtime's own exporter sent to the receiver adapter. Each is one file,
// bytes as produced or as received, write-once like every landed file, and
// asz push sends them in order and records which were sent.
const SpoolDir = "_metrics"

// Spool is the metrics spool of one storage root.
type Spool struct{ dir string }

// NewSpool opens the spool of a root; nothing is created until Put.
func NewSpool(z *Zone) *Spool { return &Spool{dir: filepath.Join(z.Root(), SpoolDir)} }

// Dir is the spool directory.
func (s *Spool) Dir() string { return s.dir }

// SpoolFile is one request waiting in the spool.
type SpoolFile struct {
	Path   string
	Seq    uint64
	Source string
	// At is when a received request was put, read off its name; zero for
	// a derived request, which is named after its landed file instead.
	At time.Time
}

// spoolName builds a spool filename: sortable by the time it was put and
// then by a sequence, which keeps two requests of one nanosecond apart.
func spoolName(stamp string, seq uint64, source string) string {
	return fmt.Sprintf("metrics-%s-%06d-%s.pb", stamp, seq, source)
}

// Put writes one request to the spool and returns its path. The sequence
// comes from the spool's own state file under the spool's lock, so two
// writers in one root, the local derivation and the receiver, never take
// the same number.
func (s *Spool) Put(source string, data []byte, now time.Time) (string, error) {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return "", err
	}
	lock, err := lockDir(s.dir)
	if err != nil {
		return "", err
	}
	defer func() { _ = lock.Unlock() }()
	seq, err := s.nextSeq()
	if err != nil {
		return "", err
	}
	path := filepath.Join(s.dir, spoolName(Stamp(now), seq, source))
	if err := WriteAtomic(path, PermLanded, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	}); err != nil {
		return "", err
	}
	if err := s.saveSeq(seq+1, now); err != nil {
		return "", err
	}
	return path, nil
}

// PutNamed writes one request under a name the caller chose, and reports
// whether it wrote it: a request that exists already is kept as it is.
// The local derivation names its requests after the landed file they came
// from, so deriving a file again after a crash writes the same file once.
func (s *Spool) PutNamed(name string, data []byte) (string, bool, error) {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return "", false, err
	}
	path := filepath.Join(s.dir, name)
	if _, err := os.Stat(path); err == nil {
		return path, false, nil
	}
	if err := WriteAtomic(path, PermLanded, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	}); err != nil {
		return "", false, err
	}
	return path, true, nil
}

func (s *Spool) statePath() string { return filepath.Join(s.dir, "spool.state") }

// nextSeq reads the counter, or recovers it from the files when the state
// is gone: the files are the authority, as they are for a session.
func (s *Spool) nextSeq() (uint64, error) {
	f, err := os.Open(s.statePath())
	if err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), "next_seq "); ok {
				return strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			}
		}
		return 0, errors.New("storage: spool.state carries no next_seq")
	}
	if !os.IsNotExist(err) {
		return 0, err
	}
	files, err := s.List()
	if err != nil {
		return 0, err
	}
	var next uint64 = 1
	for _, sf := range files {
		if sf.Seq >= next {
			next = sf.Seq + 1
		}
	}
	return next, nil
}

func (s *Spool) saveSeq(next uint64, now time.Time) error {
	return WriteAtomic(s.statePath(), PermState, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "schema 1\nupdated_at %s\nnext_seq %d\n", now.UTC().Format(time.RFC3339Nano), next)
		return err
	})
}

// List returns every request in the spool, in the order they were put.
func (s *Spool) List() ([]SpoolFile, error) {
	items, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []SpoolFile
	for _, it := range items {
		name := it.Name()
		if it.IsDir() || !strings.HasPrefix(name, "metrics-") || !strings.HasSuffix(name, ".pb") {
			continue
		}
		// metrics-<stamp>-<seq>-<source>.pb
		parts := strings.Split(strings.TrimSuffix(name, ".pb"), "-")
		if len(parts) < 4 {
			continue
		}
		seq, err := strconv.ParseUint(parts[len(parts)-2], 10, 64)
		if err != nil {
			continue
		}
		sf := SpoolFile{Path: filepath.Join(s.dir, name), Seq: seq, Source: parts[len(parts)-1]}
		if at, err := time.Parse("20060102T150405.000000000Z", parts[1]); err == nil {
			sf.At = at
		}
		out = append(out, sf)
	}
	// Received requests in the order they were put, derived ones by name;
	// the order only has to be the same every time.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Seq != out[j].Seq {
			return out[i].Seq < out[j].Seq
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}
