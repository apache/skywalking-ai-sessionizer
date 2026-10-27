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

package format

import (
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/boundedjson"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// ErrAnswer is an answer of a server that breaks the protocol: a client
// stops reading that remote.
var ErrAnswer = errors.New("pull: invalid answer")

func answerError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrAnswer, fmt.Sprintf(format, args...))
}

// readAnswer reads one answer object: its known members by fn, unknown ones
// skipped, every required one there, schema pull/1, and nothing after it.
func readAnswer(r io.Reader, required []string, fn func(s *boundedjson.Scanner, name string) (bool, error)) error {
	s := boundedjson.New(r)
	err := members(s, append([]string{"schema"}, required...), func(name string) (bool, error) {
		if name == "schema" {
			schema, err := stringValue(s)
			if err == nil && schema != Schema {
				err = fmt.Errorf("%q is not %s", schema, Schema)
			}
			return true, err
		}
		return fn(s, name)
	})
	if err == nil {
		err = valueEnded(s)
	}
	if err != nil {
		return answerError("%v", err)
	}
	return nil
}

// ReadListing reads a listing's answer: every row's names safe and its
// numbers whole, every skipped entry named, and next present only when
// there is a next page.
func ReadListing(r io.Reader) (*Listing, error) {
	l := &Listing{Schema: Schema}
	err := readAnswer(r, []string{"rows", "skipped"}, func(s *boundedjson.Scanner, name string) (bool, error) {
		switch name {
		case "rows":
			return true, eachObject(s, func() error {
				row, err := readRow(s)
				if err == nil {
					l.Rows = append(l.Rows, row)
				}
				return err
			})
		case "skipped":
			l.Skipped = []Skipped{}
			return true, eachObject(s, func() error {
				var k Skipped
				err := membersOpened(s, []string{"id", "reason"}, func(name string) (bool, error) {
					var err error
					switch name {
					case "id":
						k.ID, err = stringValue(s)
					case "reason":
						if k.Reason, err = stringValue(s); err == nil && k.Reason == "" {
							err = errors.New("empty")
						}
					default:
						return false, nil
					}
					return true, err
				})
				l.Skipped = append(l.Skipped, k)
				return err
			})
		case "next":
			var err error
			if l.Next, err = stringValue(s); err == nil && l.Next == "" {
				err = errors.New("empty")
			}
			return true, err
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	if l.Rows == nil {
		l.Rows = []Row{}
	}
	return l, nil
}

// eachObject reads an array of objects, calling fn for each once its
// opening brace is read.
func eachObject(s *boundedjson.Scanner, fn func() error) error {
	token, err := s.Next()
	if err != nil {
		return err
	}
	if token.Kind != boundedjson.Array {
		return errors.New("expected an array")
	}
	for n := 1; ; n++ {
		token, err := s.Next()
		if err != nil {
			return err
		}
		if token.Kind == boundedjson.ArrayEnd {
			return nil
		}
		if token.Kind != boundedjson.Object {
			return fmt.Errorf("element %d is not an object", n)
		}
		if err := fn(); err != nil {
			return fmt.Errorf("element %d: %w", n, err)
		}
	}
}

// readRow reads a row whose opening brace was read.
func readRow(s *boundedjson.Scanner) (Row, error) {
	var row Row
	counts := map[string]*uint64{"talks": &row.Talks, "steps": &row.Steps, "streams": &row.Streams,
		"segments": &row.Segments, "unresolved": &row.Unresolved}
	optional := map[string]**uint64{"changes": &row.Changes, "lines_added": &row.LinesAdded, "lines_removed": &row.LinesRemoved,
		"llm_calls": &row.LLMCalls, "subagents": &row.Subagents, "bash_runs": &row.BashRuns}
	seen := map[string]bool{}
	for {
		token, err := s.Next()
		if err != nil {
			return row, err
		}
		if token.Kind == boundedjson.ObjectEnd {
			break
		}
		name := token.Text
		switch {
		case name == "id" || name == "session":
			v, err := stringValue(s)
			if err == nil {
				err = storage.CheckNamePart(v)
			}
			if err != nil {
				return row, fmt.Errorf("%s: %w", name, err)
			}
			if name == "id" {
				row.ID = v
			} else {
				row.Session = v
			}
		case name == "runtime":
			if row.Runtime, err = stringValue(s); err != nil {
				return row, err
			}
		case name == "title":
			if row.Title, err = stringValue(s); err != nil {
				return row, err
			}
		case name == "round":
			if row.Round, err = uint64Value(s); err == nil && row.Round == 0 {
				err = errors.New("round 0")
			}
			if err != nil {
				return row, err
			}
		case name == "digest":
			if row.Digest, err = stringValue(s); err == nil && !storage.Digest64(row.Digest) {
				err = errors.New("not a digest")
			}
			if err != nil {
				return row, err
			}
		case name == "session_from_time" || name == "session_through_time":
			v, err := stringValue(s)
			if err == nil {
				_, err = time.Parse(time.RFC3339Nano, v)
			}
			if err != nil {
				return row, fmt.Errorf("%s: %w", name, err)
			}
			if name == "session_from_time" {
				row.SessionFromTime = v
			} else {
				row.SessionThroughTime = v
			}
		case counts[name] != nil:
			if *counts[name], err = uint64Value(s); err != nil {
				return row, fmt.Errorf("%s: %w", name, err)
			}
		case optional[name] != nil:
			n, err := uint64Value(s)
			if err != nil {
				return row, fmt.Errorf("%s: %w", name, err)
			}
			*optional[name] = &n
		default:
			if err := s.Skip(); err != nil {
				return row, err
			}
			continue
		}
		seen[name] = true
	}
	for _, name := range []string{"id", "session", "runtime", "title", "round", "digest", "talks", "steps", "streams", "segments", "unresolved"} {
		if !seen[name] {
			return row, fmt.Errorf("a row has no %s", name)
		}
	}
	return row, nil
}

// ReadManifestPending reads the manifest route's 202: building, for the
// conversation asked for, under a freeze identifier.
func ReadManifestPending(r io.Reader, conversation string) (ManifestPending, error) {
	p := ManifestPending{Schema: Schema}
	err := readAnswer(r, []string{"conversation", "freeze", "state"}, func(s *boundedjson.Scanner, name string) (bool, error) {
		var err error
		switch name {
		case "conversation":
			if p.Conversation, err = stringValue(s); err == nil && p.Conversation != conversation {
				err = fmt.Errorf("%q is not the conversation asked for", p.Conversation)
			}
		case "freeze":
			if p.Freeze, err = stringValue(s); err == nil && !storage.Digest64(p.Freeze) {
				err = errors.New("not 64 lower-case hexadecimal characters")
			}
		case "state":
			if p.State, err = stringValue(s); err == nil && p.State != StateBuilding {
				err = fmt.Errorf("%q is not %s", p.State, StateBuilding)
			}
		default:
			return false, nil
		}
		return true, err
	})
	return p, err
}

// ReadArchiveState reads an archive's 202 or 200, which must name the
// snapshot and the archive asked for; a ready one carries its size and its
// byte digest.
func ReadArchiveState(r io.Reader, snapshot, archive string, ready bool) (ArchiveState, error) {
	a := ArchiveState{Schema: Schema}
	required := []string{"snapshot", "archive", "state"}
	if ready {
		required = append(required, "bytes", "digest")
	}
	err := readAnswer(r, required, func(s *boundedjson.Scanner, name string) (bool, error) {
		var err error
		switch name {
		case "snapshot":
			if a.Snapshot, err = stringValue(s); err == nil && a.Snapshot != snapshot {
				err = fmt.Errorf("%q is not the snapshot asked for", a.Snapshot)
			}
		case "archive":
			if a.Archive, err = stringValue(s); err == nil && a.Archive != archive {
				err = fmt.Errorf("%q is not the archive asked for", a.Archive)
			}
		case "state":
			want := StateBuilding
			if ready {
				want = StateReady
			}
			if a.State, err = stringValue(s); err == nil && a.State != want {
				err = fmt.Errorf("%q is not %s", a.State, want)
			}
		case "bytes":
			if !ready {
				return false, nil
			}
			if a.Bytes, err = uint64Value(s); err == nil && (a.Bytes == 0 || a.Bytes > math.MaxInt64) {
				err = fmt.Errorf("%d is not a size a file can have", a.Bytes)
			}
		case "digest":
			if !ready {
				return false, nil
			}
			if a.Digest, err = stringValue(s); err == nil && !storage.Digest64(a.Digest) {
				err = errors.New("not a digest")
			}
		default:
			return false, nil
		}
		return true, err
	})
	return a, err
}

// Problem is a server's problem answer as a client reads it.
type Problem struct {
	Type, Title, Detail string
	Status              int
	Conversation        string
	Freeze              string
	Snapshot            string
	Archive             string
	Attempt             uint64
}

// ReadProblem reads a problem answer: its status must be the status it came
// with, from 400 to 599.
func ReadProblem(r io.Reader, status int) (Problem, error) {
	var p Problem
	s := boundedjson.New(r)
	err := members(s, []string{"type", "title", "detail", "status"}, func(name string) (bool, error) {
		var err error
		switch name {
		case "type":
			p.Type, err = stringValue(s)
		case "title":
			p.Title, err = stringValue(s)
		case "detail":
			p.Detail, err = stringValue(s)
		case "status":
			var n uint64
			if n, err = uint64Value(s); err == nil && (n < 400 || n > 599 || int(n) != status) {
				err = fmt.Errorf("status %d on a %d", n, status)
			}
			p.Status = int(n)
		case "conversation":
			p.Conversation, err = stringValue(s)
		case "freeze":
			p.Freeze, err = stringValue(s)
		case "snapshot":
			p.Snapshot, err = stringValue(s)
		case "archive":
			p.Archive, err = stringValue(s)
		case "attempt":
			if p.Attempt, err = uint64Value(s); err == nil && p.Attempt == 0 {
				err = errors.New("attempt 0")
			}
		default:
			return false, nil
		}
		return true, err
	})
	if err == nil {
		err = valueEnded(s)
	}
	// A type this page names comes with its one status: a typed failure
	// under another status is not that failure, and is not acted on.
	if want, ok := problemStatus[p.Type]; err == nil && ok && p.Status != want {
		err = fmt.Errorf("%s with status %d, not %d", p.Type, p.Status, want)
	}
	if err != nil {
		return Problem{}, answerError("a problem: %v", err)
	}
	return p, nil
}

// problemStatus is the status each named problem type comes with.
var problemStatus = map[string]int{
	ProblemOwnerWait:     503,
	ProblemStorageState:  500,
	ProblemSourceRead:    503,
	ProblemSourceChange:  503,
	ProblemSourceInvalid: 422,
	ProblemFreezeFailed:  500,
	ProblemArchiveFailed: 500,
}
