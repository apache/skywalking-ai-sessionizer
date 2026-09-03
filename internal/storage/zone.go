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

// Package storage owns the landing zone: where landed files go, how they are
// written, and the small state files that track collection progress.
//
// Two invariants hold throughout:
//
//   - Landed record files are write-once. They are written to a temporary name,
//     fsynced, renamed into place and made read-only. They are never appended
//     to and never rewritten.
//   - State files are mutable but are replaced atomically, never edited in place.
package storage

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// StreamMain is the reserved stream name for a session's parent lineage.
// Child streams are named by their agent id.
const StreamMain = "main"

// Zone is a landing-zone root.
type Zone struct{ root string }

// NewZone returns a Zone rooted at root.
func NewZone(root string) *Zone { return &Zone{root: root} }

// Root returns the zone root.
func (z *Zone) Root() string { return z.root }

// SessionDir is the directory holding everything collected for one session.
// It is the unit of retention: purging a session is removing this directory.
func (z *Zone) SessionDir(session string) string {
	return filepath.Join(z.root, session)
}

// StreamDir holds one execution stream's deltas and cursors. Child streams are
// flat siblings of main, keyed by agent id, deliberately not mirroring the
// source tree: the storage path must not encode a relationship the pipeline is
// supposed to derive.
func (z *Zone) StreamDir(session, stream string) string {
	return filepath.Join(z.SessionDir(session), "streams", stream)
}

// RunDir holds everything belonging to one workflow run - its journal, its
// manifest versions and its script - keyed by run id.
//
// One directory rather than three parallel ones keyed by the same id, mirroring
// "everything about one stream in one folder".
func (z *Zone) RunDir(session, runID string) string {
	return filepath.Join(z.SessionDir(session), "runs", runID)
}

// IndexDir holds the session's derived lookup index.
//
// The index is disposable: deleting it loses nothing, because it rebuilds from
// the landed files it describes.
func (z *Zone) IndexDir(session string) string {
	return filepath.Join(z.SessionDir(session), "index")
}

// IndexStatePath is the index's own progress file.
func (z *Zone) IndexStatePath(session string) string {
	return filepath.Join(z.IndexDir(session), "index.state")
}

// SessionStatePath is the session-scoped state file.
func (z *Zone) SessionStatePath(session string) string {
	return filepath.Join(z.SessionDir(session), "session.state")
}

// Stamp formats a receipt time for a landed filename: compact RFC 3339 with
// nanoseconds, lexicographically sortable and free of ':' so it is safe on
// every filesystem.
func Stamp(t time.Time) string {
	return t.UTC().Format("20060102T150405.000000000Z")
}

// LandedName builds a landed record filename.
func LandedName(prefix, stamp string, seq uint64) string {
	return fmt.Sprintf("%s-%s-%06d.sd", prefix, stamp, seq)
}

// WriteAtomic writes a file via a temporary name, fsync and rename.
//
// perm is applied before the rename, so a landed file is read-only from the
// instant it becomes visible under its final name.
func WriteAtomic(path string, perm os.FileMode, fn func(io.Writer) error) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()

	if err = fn(tmp); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Chmod(tmpName, perm); err != nil {
		return err
	}
	if err = os.Rename(tmpName, path); err != nil {
		return err
	}
	// Sync the directory so the rename itself survives a crash.
	d, err := os.Open(dir)
	if err != nil {
		return nil // rename succeeded; durability of the entry is best effort
	}
	defer d.Close()
	_ = d.Sync()
	return nil
}

// ErrExists means a no-replace write found its target already present.
var ErrExists = errors.New("storage: target already exists")

// WriteExclusive creates a file that must not already exist.
//
// The exclusion is done by the kernel, with O_EXCL, and not by checking first.
// A check followed by a write has a window between the two: two writers both
// see nothing there, and both proceed. That is tolerable for a landed file,
// whose sequence makes a collision a bug to surface elsewhere. It is not
// tolerable for a published artifact in a digest chain, where replacing one
// invalidates every artifact that names it.
//
// The file is written in place rather than renamed into position, because a
// rename is what would reintroduce the replacement. It is fsynced and made
// read-only before it is visible under its final name in the directory listing
// - it is created empty, so a reader that catches it mid-write sees a short
// file, which Read rejects for having no commit frame.
func WriteExclusive(path string, perm os.FileMode, fn func(io.Writer) error) (err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%w: %s", ErrExists, path)
		}
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(path)
		}
	}()
	if err = fn(f); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Chmod(path, perm); err != nil {
		return err
	}
	d, derr := os.Open(filepath.Dir(path))
	if derr != nil {
		return nil // the file is written; durability of the entry is best effort
	}
	defer d.Close()
	_ = d.Sync()
	return nil
}

// PermLanded is the mode of a landed record file: read-only, because it is
// write-once by contract.
const PermLanded os.FileMode = 0o444

// PermState is the mode of a mutable state file.
const PermState os.FileMode = 0o644
