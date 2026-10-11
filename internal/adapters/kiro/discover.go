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
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// SrcKind says what a source file is.
type SrcKind int

const (
	// SrcMessages is a session's messages.jsonl, its main stream.
	SrcMessages SrcKind = iota + 1
	// SrcChild is sub-executions/<id>.jsonl, the stream of one subagent.
	SrcChild
	// SrcHeader is session.json, the session's header. The engine replaces it
	// whole, also between turns, so it lands as a new snapshot whenever its
	// bytes change.
	SrcHeader
)

// Append reports whether a source only ever grows.
func (k SrcKind) Append() bool { return k == SrcMessages || k == SrcChild }

// Source is one file of a session.
type Source struct {
	Kind SrcKind
	// Path is absolute. Rel is relative to the source root, with forward
	// slashes on every platform.
	Path, Rel string
	Session   string
	Stream    string
}

// Session is one V3 session directory.
type Session struct {
	ID string
	// Rel is the session directory, relative to the source root.
	Rel string
	// Workspaces are the paths the session worked in, from its header. They
	// are what the include and exclude patterns are matched against.
	Workspaces []string
	Sources    []Source
}

// Skipped counts what discovery found and does not collect, so a person is
// told it is there.
type Skipped struct {
	// V2 counts sessions of kiro-cli's V2 engine, sessions/cli/<id>.jsonl.
	V2 int
	// Schemas counts V3 sessions by a schema this adapter does not read.
	Schemas map[string]int
	// Unreadable counts V3 session headers that could not be read as JSON.
	Unreadable int
}

// Total is everything discovery did not collect.
func (s Skipped) Total() int {
	n := s.V2 + s.Unreadable
	for _, c := range s.Schemas {
		n += c
	}
	return n
}

// header is the part of session.json discovery reads.
type header struct {
	SchemaVersion  *string  `json:"schemaVersion"`
	Title          string   `json:"title"`
	WorkspacePaths []string `json:"workspacePaths"`
}

const (
	headerFile   = "session.json"
	messagesFile = "messages.jsonl"
	childDir     = "sub-executions"
	// v2Dir is where kiro-cli's V2 engine keeps its sessions. It sits among
	// the V3 workspace directories and is not one.
	v2Dir = "cli"
)

// Discover lists the V3 sessions under a source root.
//
// The walk is sessions/<workspace key>/sess_<id>/. Nothing else under a
// session is read: large tool output the engine spilled to tool-outputs/,
// the file snapshots it keeps for checkpoints, its upload cursors and its
// temporary header copies are not the conversation.
//
// The warnings are directories that could not be read. A pass that met one
// has not seen every session, and says so rather than look complete.
func Discover(root string) ([]Session, Skipped, []error, error) {
	skipped := Skipped{Schemas: map[string]int{}}
	base := filepath.Join(root, "sessions")
	keys, err := os.ReadDir(base)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, skipped, nil, nil
		}
		return nil, skipped, nil, err
	}
	var sessions []Session
	var warnings []error
	for _, k := range keys {
		if !k.IsDir() || strings.HasPrefix(k.Name(), ".") {
			continue
		}
		dir := filepath.Join(base, k.Name())
		items, err := os.ReadDir(dir)
		if err != nil {
			warnings = append(warnings, fmt.Errorf("kiro: %s: %w", dir, err))
			continue
		}
		if k.Name() == v2Dir {
			for _, it := range items {
				if !it.IsDir() && strings.HasSuffix(it.Name(), ".jsonl") {
					skipped.V2++
				}
			}
			continue
		}
		for _, it := range items {
			if !it.IsDir() || !IsSessionID(it.Name()) {
				continue
			}
			s, ok, err := readSession(root, path.Join("sessions", k.Name(), it.Name()), &skipped)
			if err != nil {
				warnings = append(warnings, err)
				continue
			}
			if ok {
				sessions = append(sessions, s)
			}
		}
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].ID < sessions[j].ID })
	return sessions, skipped, warnings, nil
}

// readSession reads one session directory. It reports false for a directory
// that is not a session this adapter collects, counting why in skipped.
func readSession(root, rel string, skipped *Skipped) (Session, bool, error) {
	dir := filepath.Join(root, filepath.FromSlash(rel))
	id := path.Base(rel)
	data, err := os.ReadFile(filepath.Join(dir, headerFile))
	if err != nil {
		// The engine writes the header when it creates the session and
		// replaces it by renaming, so a missing one is a directory being
		// created or removed right now. The next pass sees which.
		if errors.Is(err, fs.ErrNotExist) {
			return Session{}, false, nil
		}
		return Session{}, false, fmt.Errorf("kiro: %s: %w", dir, err)
	}
	var h header
	if err := json.Unmarshal(data, &h); err != nil {
		skipped.Unreadable++
		return Session{}, false, nil
	}
	if h.SchemaVersion == nil || *h.SchemaVersion != Schema {
		v := "none"
		if h.SchemaVersion != nil {
			v = *h.SchemaVersion
		}
		skipped.Schemas[v]++
		return Session{}, false, nil
	}

	s := Session{ID: id, Rel: rel, Workspaces: h.WorkspacePaths}
	add := func(kind SrcKind, name, stream string) {
		r := path.Join(rel, name)
		s.Sources = append(s.Sources, Source{Kind: kind, Path: filepath.Join(root, filepath.FromSlash(r)),
			Rel: r, Session: id, Stream: stream})
	}
	add(SrcHeader, headerFile, storage.StreamMain)
	hasRecord := false
	if fi, err := os.Stat(filepath.Join(dir, messagesFile)); err == nil && fi.Mode().IsRegular() {
		add(SrcMessages, messagesFile, storage.StreamMain)
		hasRecord = fi.Size() > 0
	}
	children, err := os.ReadDir(filepath.Join(dir, childDir))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Session{}, false, fmt.Errorf("kiro: %s: %w", filepath.Join(dir, childDir), err)
	}
	for _, c := range children {
		child, ok := childOf(c.Name())
		if !c.Type().IsRegular() || !ok {
			continue
		}
		add(SrcChild, path.Join(childDir, c.Name()), child)
		if info, err := c.Info(); err == nil && info.Size() > 0 {
			hasRecord = true
		}
	}
	// A session with a header and no record is not a conversation yet. Kiro
	// Crew opens sessions ahead of use, and 30 of 86 measured held nothing
	// else. It is collected once it has a record, and an empty file holds none.
	if !hasRecord {
		return Session{}, false, nil
	}
	return s, true, nil
}

// childOf reads a subagent's stream name from its file name. The file name is
// the only identity every record of a child shares: some of them do not carry
// the child's id inside.
func childOf(name string) (string, bool) {
	id, ok := strings.CutSuffix(name, ".jsonl")
	if !ok || !childIDRe.MatchString(id) || id == storage.StreamMain {
		return "", false
	}
	return id, true
}
