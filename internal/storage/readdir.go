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
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
)

// ErrTooMany is a directory with more entries than its reader takes.
var ErrTooMany = errors.New("storage: the directory holds more entries than may be read")

// readDirBatch is how many entries one read of a directory takes.
const readDirBatch = 1024

// ReadDirAtMost lists a directory as os.ReadDir does, sorted by name, but
// stops with ErrTooMany once it has seen more than most entries: a
// directory of millions of entries is refused before they are all held.
// A most below zero takes every entry.
func ReadDirAtMost(dir string, most int) ([]fs.DirEntry, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []fs.DirEntry
	for {
		// Never more than one past the bound is read.
		n := readDirBatch
		if most >= 0 {
			n = min(n, most-len(out)+1)
		}
		batch, err := f.ReadDir(n)
		out = append(out, batch...)
		if most >= 0 && len(out) > most {
			return nil, fmt.Errorf("%w: %s holds more than %d", ErrTooMany, dir, most)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

// EachDirEntry gives fn the entries of a directory in batches as it reads
// them, in no particular order, and holds none of them: a directory of
// millions of entries is walked in bounded memory. What fn keeps, and what
// it counts against a bound, is its own.
func EachDirEntry(dir string, fn func(fs.DirEntry) error) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		batch, err := f.ReadDir(readDirBatch)
		for _, e := range batch {
			if err := fn(e); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
