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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// RemovingFile is the name of a session's removal intent, written in the
// session's directory before a removal touches any of its files and deleted
// when the removal is done.
const RemovingFile = "removing.json"

// The phases of a removal, in order.
const (
	// RemovingSkeletons writes the skeleton of every landed file it removes.
	// Nothing is deleted yet.
	RemovingSkeletons = "skeletons"
	// RemovingDelete deletes each landed file whose skeleton is in place.
	RemovingDelete = "delete"
)

// Removing is a removal in progress. A pass that finds one finishes it,
// from its phase, before anything lands into the session.
type Removing struct {
	// Generation is which removal of the session this is, counting from 1.
	Generation int `json:"generation"`
	// Through is the highest sequence the removal removes.
	Through uint64 `json:"through"`
	Phase   string `json:"phase"`
}

// RemovingPath is where a session's removal intent is kept.
func (z *Zone) RemovingPath(session string) string {
	return filepath.Join(z.SessionDir(session), RemovingFile)
}

// LoadRemoving reads a session's removal intent, or nil when no removal is in
// progress.
func LoadRemoving(path string) (*Removing, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r Removing
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("storage: removal intent %s: %w", path, err)
	}
	switch {
	case r.Generation < 1, r.Through < 1:
		return nil, fmt.Errorf("storage: removal intent %s names generation %d through sequence %d", path, r.Generation, r.Through)
	case r.Phase != RemovingSkeletons && r.Phase != RemovingDelete:
		return nil, fmt.Errorf("storage: removal intent %s has the unknown phase %q", path, r.Phase)
	}
	return &r, nil
}

// SaveRemoving writes a session's removal intent and makes it survive a power
// loss before the removal goes on: a removal deletes files, and the intent is
// what lets the next pass finish it.
func SaveRemoving(path string, r *Removing) error {
	return WriteAtomicDurable(path, PermState, func(w io.Writer) error {
		return json.NewEncoder(w).Encode(r)
	}, nil)
}

// SyncDir makes the names in a directory survive a power loss, and says when
// it could not. A filesystem that cannot sync a directory at all is not an
// error: that is as far as it goes. Unlike a write, which only reports a
// failed sync, a removal stops on one: it must not delete a file while the
// file that takes its place may still be lost.
func SyncDir(dir string) error { return syncDir(dir) }

// RemovalStateFile is where a session records the generation of its last
// removal whose files are being, or were, deleted.
const RemovalStateFile = "removal.state"

// RemovalStatePath is where a session's removal generation is kept.
func (z *Zone) RemovalStatePath(session string) string {
	return filepath.Join(z.SessionDir(session), RemovalStateFile)
}

// SaveRemovalGeneration records that the files of a session's removal of
// this generation are about to be deleted. A reader that keeps what it read
// of the session, such as the page, knows from it that its landed files were
// replaced by skeletons, though the chain did not move.
func SaveRemovalGeneration(z *Zone, session string, generation int) error {
	return WriteAtomicDurable(z.RemovalStatePath(session), PermState, func(w io.Writer) error {
		return json.NewEncoder(w).Encode(struct {
			Generation int `json:"generation"`
		}{generation})
	}, nil)
}

// RemovalGeneration says which removal of a session last replaced its files:
// 0 for none. inProgress says a removal of it has not finished, and what is
// read of the session may change from one read to the next.
//
// The generation is read before the intent is looked for. A removal writes
// its intent before its generation and deletes the intent last, so a reader
// that finds no intent after it read the generation read the generation of a
// removal that is finished, or of none; read the other way round, it could
// take the new generation while the files it reads next are still the old.
func RemovalGeneration(z *Zone, session string) (generation int, inProgress bool, err error) {
	data, err := os.ReadFile(z.RemovalStatePath(session))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return 0, false, err
	default:
		var s struct {
			Generation int `json:"generation"`
		}
		if err := json.Unmarshal(data, &s); err != nil {
			return 0, false, fmt.Errorf("storage: removal state %s: %w", z.RemovalStatePath(session), err)
		}
		generation = s.Generation
	}
	if _, err := os.Lstat(z.RemovingPath(session)); err == nil {
		return 0, true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, false, err
	}
	return generation, false, nil
}
