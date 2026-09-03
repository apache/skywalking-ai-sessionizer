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
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Session liveness. A Claude Code session is never "complete": resume appends
// to the same file with the same id, so a session quiet for weeks can grow
// again. The state model therefore has no terminal value.
const (
	LivenessLive    = "live"
	LivenessQuiet   = "quiet"
	LivenessUnknown = "unknown"
)

// SessionState is session-scoped collection state.
//
// NextSeq is shared by every stream in the session because the landed sequence
// must be monotonic across all of them - that is what lets the assembler track
// progress with a single watermark instead of one entry per stream. It also
// means a session is the unit of work and is processed single-threaded;
// parallelism is across sessions, never within one.
type SessionState struct {
	Schema    int
	Session   string
	NextSeq   uint64
	Liveness  string
	LastScan  string
	UpdatedAt string
}

// LoadSessionState reads session state, returning a fresh value if absent.
func LoadSessionState(path, session string) (*SessionState, error) {
	s := &SessionState{Schema: 1, Session: session, NextSeq: 1, Liveness: LivenessUnknown}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch key {
		case "schema":
			s.Schema, _ = strconv.Atoi(val)
		case "session":
			s.Session = val
		case "next_seq":
			s.NextSeq, _ = strconv.ParseUint(val, 10, 64)
		case "liveness":
			s.Liveness = val
		case "last_scan":
			s.LastScan = val
		case "updated_at":
			s.UpdatedAt = val
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("storage: read session state %s: %w", path, err)
	}
	if s.NextSeq == 0 {
		s.NextSeq = 1
	}
	return s, nil
}

// landedSeqRe extracts the sequence from a landed filename.
var landedSeqRe = regexp.MustCompile(`-(\d{6,})\.sd$`)

// RecoverNextSeq raises NextSeq above the highest sequence already present in
// sessionDir.
//
// This is the crash-safety guarantee for the counter. session.state is written
// once per session while landed files and cursors commit per source, so a kill
// mid-session leaves next_seq behind what is actually on disk. Reissuing a
// sequence is not a cosmetic collision: the assembler tracks progress with a
// single monotonic watermark, so a second file with an already-passed sequence
// is never read and its records are lost.
//
// Scanning the directory makes the filesystem the authority, which is also what
// makes a concurrent or restarted collector converge rather than diverge.
func (s *SessionState) RecoverNextSeq(sessionDir string) error {
	var highest uint64
	err := filepath.WalkDir(sessionDir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		m := landedSeqRe.FindStringSubmatch(d.Name())
		if m == nil {
			return nil
		}
		if n, convErr := strconv.ParseUint(m[1], 10, 64); convErr == nil && n > highest {
			highest = n
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("storage: recover next_seq in %s: %w", sessionDir, err)
	}
	if highest+1 > s.NextSeq {
		s.NextSeq = highest + 1
	}
	return nil
}

// Save writes session state atomically.
func (s *SessionState) Save(path string, now time.Time) error {
	s.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
	return WriteAtomic(path, PermState, func(w io.Writer) error {
		bw := bufio.NewWriter(w)
		fmt.Fprintf(bw, "schema     %d\n", s.Schema)
		fmt.Fprintf(bw, "session    %s\n", s.Session)
		fmt.Fprintf(bw, "next_seq   %d\n", s.NextSeq)
		fmt.Fprintf(bw, "liveness   %s\n", s.Liveness)
		fmt.Fprintf(bw, "last_scan  %s\n", s.LastScan)
		fmt.Fprintf(bw, "updated_at %s\n", s.UpdatedAt)
		return bw.Flush()
	})
}

// Take allocates the next landed sequence number.
func (s *SessionState) Take() uint64 {
	seq := s.NextSeq
	s.NextSeq++
	return seq
}
