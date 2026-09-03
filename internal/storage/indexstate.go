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
	"os"
	"strconv"
	"strings"
	"time"
)

// IndexState records how much of a session's landed data has been indexed.
//
// It is the middle link of a three-stage watermark chain:
//
//	assembled_seq  <=  indexed_seq  <=  next_seq - 1
//
// Each stage trails the one before it, so "what needs doing" is a comparison of
// two small files rather than a scan, and each stage runs at its own cadence.
type IndexState struct {
	Schema     int
	Session    string
	IndexedSeq uint64
	Entries    int
	Blocks     int
	Strings    int
	BuiltAt    string
	UpdatedAt  string
}

// NewIndexState returns empty index state for a session.
func NewIndexState(session string) *IndexState {
	return &IndexState{Session: session}
}

// LoadIndexState reads index state, returning an empty value if absent.
func LoadIndexState(path, session string) (*IndexState, error) {
	s := NewIndexState(session)
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
		case "indexed_seq":
			s.IndexedSeq, _ = strconv.ParseUint(val, 10, 64)
		case "entries":
			s.Entries, _ = strconv.Atoi(val)
		case "blocks":
			s.Blocks, _ = strconv.Atoi(val)
		case "strings":
			s.Strings, _ = strconv.Atoi(val)
		case "built_at":
			s.BuiltAt = val
		case "updated_at":
			s.UpdatedAt = val
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("storage: read index state %s: %w", path, err)
	}
	return s, nil
}

// Save writes index state atomically.
func (s *IndexState) Save(path string, now time.Time) error {
	stamp := now.UTC().Format(time.RFC3339Nano)
	if s.BuiltAt == "" {
		s.BuiltAt = stamp
	}
	s.UpdatedAt = stamp
	return WriteAtomic(path, PermState, func(w io.Writer) error {
		bw := bufio.NewWriter(w)
		fmt.Fprintf(bw, "schema      %d\n", s.Schema)
		fmt.Fprintf(bw, "session     %s\n", s.Session)
		fmt.Fprintf(bw, "indexed_seq %d\n", s.IndexedSeq)
		fmt.Fprintf(bw, "entries     %d\n", s.Entries)
		fmt.Fprintf(bw, "blocks      %d\n", s.Blocks)
		fmt.Fprintf(bw, "strings     %d\n", s.Strings)
		fmt.Fprintf(bw, "built_at    %s\n", s.BuiltAt)
		fmt.Fprintf(bw, "updated_at  %s\n", s.UpdatedAt)
		return bw.Flush()
	})
}
