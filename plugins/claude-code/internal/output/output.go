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

// Package output writes the plugin's records: one file per stream of a
// session, one JSON line per record, appended and never rewritten. It is
// the only thing the plugin hands anyone. The asz adapter tails these
// files the way it tails a transcript, and stops at the last complete
// newline, so a line is written whole and synced before the hook returns.
package output

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/pkg/changes"
)

// Dir is the output directory under the plugin's data directory.
const Dir = "output"

// Path is the file a session's stream is written to.
func Path(dataDir, session, stream string) string {
	return filepath.Join(dataDir, Dir, session, stream+".jsonl")
}

// Append writes one record as one line.
func Append(dataDir string, r *changes.Record) error {
	if err := r.Validate(); err != nil {
		return err
	}
	line, err := r.Marshal()
	if err != nil {
		return err
	}
	path := Path(dataDir, r.Session, r.Stream)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// Prune removes output files not written to for longer than ttl, and the
// session directories that become empty. It asks no collector: what asz
// has landed is asz's, under its own retention.
func Prune(dataDir string, ttl time.Duration, now time.Time) (removed int, err error) {
	root := filepath.Join(dataDir, Dir)
	sessions, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	for _, s := range sessions {
		if !s.IsDir() {
			continue
		}
		dir := filepath.Join(root, s.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		left := 0
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				left++
				continue
			}
			info, err := f.Info()
			if err != nil {
				left++
				continue
			}
			if now.Sub(info.ModTime()) > ttl {
				if os.Remove(filepath.Join(dir, f.Name())) == nil {
					removed++
					continue
				}
			}
			left++
		}
		if left == 0 {
			_ = os.Remove(dir)
		}
	}
	return removed, nil
}
