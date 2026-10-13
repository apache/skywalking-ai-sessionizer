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
	"sync"
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
//
// asz never removes it for a real session, so what was landed outlives the
// source. The one exception is a session a scenario build marked. Once all
// of it is sent, a pipeline renames this directory into RemovedDir and
// deletes it there, so no reader ever sees half of it.
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

// ProviderDir holds the session's provider bodies: what the runtime sent its
// model provider and what came back. It sits at the session level, not under
// a stream, because a body names no stream. Which stream and which call it
// belongs to is joined later, from evidence, and a path must not say it.
func (z *Zone) ProviderDir(session string) string {
	return filepath.Join(z.SessionDir(session), "provider_body")
}

// ProviderStatePath is where the root keeps which provider body files have
// landed, and which wait for a session. It is derived from the landed records
// and rebuilt when it is missing.
func (z *Zone) ProviderStatePath() string {
	return filepath.Join(z.root, "_provider", "seen.state")
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
func WriteAtomic(path string, perm os.FileMode, fn func(io.Writer) error) error {
	if err := writeAtomic(path, perm, fn); err != nil {
		return err
	}
	SyncAfterRename(filepath.Dir(path))
	return nil
}

// WriteAtomicDurable is WriteAtomic for a file something is about to depend
// on surviving a power loss: the directory is synced with sync, and a
// failure is returned rather than reported. The first sync after a failed
// writeback is the one that knows: a second one can succeed though the data
// was lost.
func WriteAtomicDurable(path string, perm os.FileMode, fn func(io.Writer) error, sync func(dir string) error) error {
	if err := writeAtomic(path, perm, fn); err != nil {
		return err
	}
	return syncWith(sync, filepath.Dir(path))
}

func writeAtomic(path string, perm os.FileMode, fn func(io.Writer) error) (err error) {
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
	return os.Rename(tmpName, path)
}

// ErrExists means a no-replace write found its target already present.
var ErrExists = errors.New("storage: target already exists")

// WriteExclusive publishes a complete file without replacing an existing one.
//
// A round's name advances the chain head, so even an interrupted writer must
// never leave a partial file at that name. Write and sync a temporary file
// first, then let the kernel install it only if the destination is absent.
// A crash before that operation leaves only a temporary file, which readers
// ignore. A crash after it leaves the complete, read-only round.
func WriteExclusive(path string, perm os.FileMode, fn func(io.Writer) error) error {
	if err := writeExclusive(path, perm, fn); err != nil {
		return err
	}
	SyncAfterRename(filepath.Dir(path))
	return nil
}

// WriteExclusiveDurable is WriteExclusive for a file something is about to
// depend on surviving a power loss, as WriteAtomicDurable is.
func WriteExclusiveDurable(path string, perm os.FileMode, fn func(io.Writer) error, sync func(dir string) error) error {
	if err := writeExclusive(path, perm, fn); err != nil {
		return err
	}
	return syncWith(sync, filepath.Dir(path))
}

// syncWith syncs dir with sync, or with SyncDir when sync is nil.
func syncWith(sync func(string) error, dir string) error {
	if sync == nil {
		sync = SyncDir
	}
	if err := sync(dir); err != nil {
		return fmt.Errorf("storage: %s may not survive a power loss: %w", dir, err)
	}
	return nil
}

func writeExclusive(path string, perm os.FileMode, fn func(io.Writer) error) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := f.Name()
	defer func() {
		_ = f.Close()
		_ = os.Remove(tmpName)
	}()
	if err = fn(f); err != nil {
		return err
	}
	if err = f.Chmod(perm); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return RenameExclusive(tmpName, path)
}

// SyncAfterRename syncs a directory after a rename into it, an attempt to make
// the new name survive a power loss. A write does not fail when it cannot: the
// file is in place, which is all a process crash needs, and another process
// may already have read it. Failing the write made a collector land the same records
// again on every pass of a disk that kept failing. A failure other than a
// filesystem that cannot sync a directory at all is reported, once per
// directory, because a power loss may then lose the newest names there.
func SyncAfterRename(dir string) {
	if err := syncDir(dir); err != nil {
		if _, said := syncWarned.LoadOrStore(dir, true); !said {
			syncWarning(dir, err)
		}
	}
}

var (
	syncWarned  sync.Map
	syncWarning = func(dir string, err error) {
		fmt.Fprintf(os.Stderr, "warning  : could not sync the directory %s: %v; a power loss may lose its newest files\n", dir, err)
	}
)

// PermLanded is the mode of a landed record file: read-only, because it is
// write-once by contract.
const PermLanded os.FileMode = 0o444

// PermState is the mode of a mutable state file.
const PermState os.FileMode = 0o644
