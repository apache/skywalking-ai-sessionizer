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

package kiro

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
)

// holdLimit is how long the last records of a file wait for the record that
// names their turn. The engine writes a turn's start a few milliseconds after
// the prompt, so a prompt still waiting after this long belongs to a turn that
// never started, such as one the engine died before. It lands with no turn.
const holdLimit = 5 * time.Minute

// turnFor finds the turn of each record that belongs to a turn without naming
// it, and says how many of the lines may land now.
//
// A person's prompt carries no turn id in any measured session, nor does the
// note the engine writes just before it, nor a message sent while a turn ran.
// Each belongs to the first turn named after it in the file: the prompt's own
// turn, or the turn the message arrived in. The rule reads only line order,
// which the engine keeps; its timestamps are not in line order.
//
// A record whose turn is not in the file yet waits, because a landed record is
// never written again: landing a prompt with no turn would leave it outside the
// turn it opened for good. The lines after the chunk are read first, since the
// turn is usually one line further on. Only when the file has nothing more, and
// has been still for holdLimit, does the record land with no turn.
//
// runs holds the turn found for each line, or "" where the record names its
// own or belongs to none. keep is how many lines land now.
func turnFor(lines []claudecode.Line, path string, end uint64, mtime int64, now time.Time) (runs []string, keep int, err error) {
	runs = make([]string, len(lines))
	wants := make([]bool, len(lines))
	next := ""
	for i := len(lines) - 1; i >= 0; i-- {
		turn, want := turnRole(lines[i].Bytes)
		switch {
		case turn != "":
			next = turn
		case want:
			wants[i], runs[i] = true, next
		}
	}
	first := -1
	for i := range lines {
		if wants[i] && runs[i] == "" {
			first = i
			break
		}
	}
	if first < 0 {
		return runs, len(lines), nil
	}
	turn, err := peekTurn(path, end)
	if err != nil {
		return nil, 0, err
	}
	if turn != "" {
		for i := first; i < len(lines); i++ {
			if wants[i] {
				runs[i] = turn
			}
		}
		return runs, len(lines), nil
	}
	if now.Sub(time.Unix(mtime, 0)) >= holdLimit {
		return runs, len(lines), nil
	}
	return runs, first, nil
}

// turnRole reads the turn a record names, and whether it wants one it does not
// name.
func turnRole(raw []byte) (turn string, wants bool) {
	var env envelope
	if json.Unmarshal(raw, &env) != nil {
		return "", false
	}
	var p payload
	if json.Unmarshal(env.Payload, &p) != nil {
		return "", false
	}
	return turnOf(&p), wantsTurn(&p)
}

// peekTurn reads the complete lines after offset, up to the first that names a
// turn, and returns that turn. An unterminated last line is a write in
// progress and is not read.
func peekTurn(path string, offset uint64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	defer f.Close()
	if _, err := f.Seek(int64(offset), io.SeekStart); err != nil {
		return "", err
	}
	// A record can be several megabytes, a prompt with images, so lines are
	// read whole rather than through a scanner with a fixed limit.
	r := bufio.NewReader(f)
	for {
		ln, err := r.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "", nil
			}
			return "", err
		}
		if turn, _ := turnRole(bytes.TrimSuffix(ln, []byte("\n"))); turn != "" {
			return turn, nil
		}
	}
}
