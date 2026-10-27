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
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"
)

// The reasons a snapshot admits no new reader.
var (
	// ErrExpired: the snapshot is older than its time to live.
	ErrExpired = errors.New("storage: the snapshot has expired")
	// ErrRemovedTag: the snapshot's tag is in its conversation's removal list.
	ErrRemovedTag = errors.New("storage: the snapshot's tag was removed")
	// ErrPublishing: the snapshot's tag is still being published, or its
	// publication was interrupted and waits for recovery.
	ErrPublishing = errors.New("storage: the snapshot's publication has not finished")
	// ErrConversationControl: a control of the conversation itself, its
	// _admit gate, outer _copy or removal records, does not read. It fails
	// the conversation, where damage to one snapshot only passes that
	// snapshot over.
	ErrConversationControl = errors.New("storage: a control of the conversation does not read")
	// ErrLocalWrite: a write of this machine's own failed, such as a lease
	// file it could not make in a directory it found. It fails the work, as
	// a conversation's own control does, and is never damage to pass over.
	ErrLocalWrite = errors.New("storage: a write here failed")
)

// Admission is a reader admitted to one snapshot. It holds a lease in the
// snapshot's _leases until Release, which keeps the snapshot from being
// removed while the reader uses it.
type Admission struct {
	Root   Root
	Marker SnapshotCopy
	lease  *Lease
	files  FileSystem
}

// Lease is the lease file's name, which a child given it can inherit.
func (a *Admission) Lease() string { return a.lease.Name() }

// gate is a conversation's _admit gate.
func (a *Admission) gate() string { return filepath.Join(filepath.Dir(a.Root.Path), AdmitDir) }

// AdmitSnapshot admits a reader to the snapshot r classified. Under the
// conversation's _admit gate, which it never makes, it checks the snapshot is
// still where it was classified, reads the conversation's history, the
// publication in progress and the removal list, reads the snapshot's own
// marker, checks its age against ttl at the time now gives once the gate is
// held, and creates and locks a read lease. The clock is read under the
// gate, never before waiting for it: a reader that waited past expiry is
// refused. It reads no manifest and no evidence: whole checks belong to the
// reader that needs them, outside the gate.
//
// ErrPublishing means the tag's publication has not finished. The caller
// releases nothing more, recovers the publication under the conversation's
// _pull if that is free, and admits again from the start.
func AdmitSnapshot(ctx context.Context, r Root, ttl time.Duration, now func() time.Time, files FileSystem) (*Admission, error) {
	if r.Kind != SnapshotRoot {
		return nil, fmt.Errorf("storage: %s is not a snapshot", r.Path)
	}
	files = writeFileSystem(files)
	conversation := filepath.Dir(r.Path)
	gate, err := LockOwnedWait(ctx, filepath.Join(conversation, AdmitDir))
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %w", ErrConversationControl, err)
	}
	defer func() { _ = gate.Unlock() }()
	marker, err := admissible(r, conversation, now(), ttl)
	if err != nil {
		return nil, err
	}
	lease, err := CreateLease(filepath.Join(r.Path, LeasesDir), ReadLease, "", files)
	if err != nil {
		return nil, err
	}
	return &Admission{Root: r, Marker: marker, lease: lease, files: files}, nil
}

// admissible checks, under the gate, what admission reads. A zero ttl
// checks no age: a held lease admits across expiry.
func admissible(r Root, conversation string, now time.Time, ttl time.Duration) (SnapshotCopy, error) {
	if err := r.Recheck(); err != nil {
		return SnapshotCopy{}, err
	}
	outer, err := ReadOuterCopy(conversation, r.Remote, r.Conversation)
	if err != nil {
		return SnapshotCopy{}, fmt.Errorf("%w: %w", ErrConversationControl, err)
	}
	if p := outer.Publishing; p != nil && p.Tag == r.Tag {
		return SnapshotCopy{}, fmt.Errorf("%w: %s", ErrPublishing, r.Tag)
	}
	if r.Tag > outer.GreatestTag {
		return SnapshotCopy{}, fmt.Errorf("%w: tag %s is past the history's greatest %s", ErrMarker, r.Tag, outer.GreatestTag)
	}
	removed, err := IsRemoved(conversation, r.Remote, r.Conversation, r.Tag)
	if err != nil {
		return SnapshotCopy{}, fmt.Errorf("%w: %w", ErrConversationControl, err)
	}
	if removed {
		return SnapshotCopy{}, fmt.Errorf("%w: %s", ErrRemovedTag, r.Tag)
	}
	marker, err := ReadSnapshotCopy(r.Path, r.Remote, r.Conversation, r.Tag)
	if err != nil {
		return SnapshotCopy{}, err
	}
	if ttl > 0 && !now.Before(marker.PublishedAt.Add(ttl)) {
		return SnapshotCopy{}, fmt.Errorf("%w: %s was published at %s, and copies live %s", ErrExpired, r.Tag,
			marker.PublishedAt.Format(time.RFC3339), ttl)
	}
	return marker, nil
}

// BindRun turns the admission's read lease into a run lease for run, a judge
// run already reserved in the snapshot. Under the gate it checks the
// snapshot and the run's directory, creates and locks the run lease, then
// releases the read lease, so the snapshot is held throughout and no age is
// checked again. Only a run lease lets an evaluator work and a child inherit.
func (a *Admission) BindRun(ctx context.Context, run string) error {
	if !ValidRun(run) {
		return fmt.Errorf("storage: %q is not a judge run", run)
	}
	gate, err := LockOwnedWait(ctx, a.gate())
	if err != nil {
		return err
	}
	defer func() { _ = gate.Unlock() }()
	if err := a.Root.Recheck(); err != nil {
		return err
	}
	if err := RealDirectory(filepath.Join(a.Root.Path, a.Marker.Session, JudgmentsDir, run)); err != nil {
		return err
	}
	lease, err := CreateLease(filepath.Join(a.Root.Path, LeasesDir), RunLease, run, a.files)
	if err != nil {
		return err
	}
	old := a.lease
	a.lease = lease
	return old.Release()
}

// Run is the judge run a run lease binds, or empty.
func (a *Admission) Run() string {
	_, run, _ := ParseLeaseName(a.lease.Name())
	return run
}

// AdmitChild admits a child of a judge run to the snapshot r classified, by
// the parent's run lease. Under the gate it checks the snapshot, parses the
// parent's lease name and probes it without waiting: a lease no live process
// holds admits nothing. check, when set, then binds what the child was given
// to the run, under the same gate: its log and its failure report must be in
// that run's work directory. The child gets a lease of its own, with no age
// check: the parent's held lease is its authority. It returns the parent's
// run.
func AdmitChild(ctx context.Context, r Root, parent string, files FileSystem, check func(marker SnapshotCopy, run string) error) (*Admission, string, error) {
	if r.Kind != SnapshotRoot {
		return nil, "", fmt.Errorf("storage: %s is not a snapshot", r.Path)
	}
	kind, run, ok := ParseLeaseName(parent)
	if !ok || kind != RunLease || filepath.Base(parent) != parent {
		return nil, "", fmt.Errorf("storage: %q is not a judge run's lease", parent)
	}
	files = writeFileSystem(files)
	conversation := filepath.Dir(r.Path)
	gate, err := LockOwnedWait(ctx, filepath.Join(conversation, AdmitDir))
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = gate.Unlock() }()
	marker, err := admissible(r, conversation, time.Time{}, 0)
	if err != nil {
		return nil, "", err
	}
	held, err := ProbeLease(filepath.Join(r.Path, LeasesDir, parent))
	if err != nil {
		return nil, "", err
	}
	if !held {
		return nil, "", fmt.Errorf("storage: the parent lease %s is not held; its run has ended", parent)
	}
	if check != nil {
		if err := check(marker, run); err != nil {
			return nil, "", err
		}
	}
	lease, err := CreateLease(filepath.Join(r.Path, LeasesDir), ChildLease, "", files)
	if err != nil {
		return nil, "", err
	}
	return &Admission{Root: r, Marker: marker, lease: lease, files: files}, run, nil
}

// Release ends the admission: its lease is unlocked and removed under the
// gate. The caller has joined every writer and closed every handle in the
// snapshot first. When the gate cannot be taken before ctx ends, the lease
// is closed and its file left for a dead-lease sweep, which removes it under
// the gate.
func (a *Admission) Release(ctx context.Context) error {
	if a == nil || a.lease == nil {
		return nil
	}
	gate, err := LockOwnedWait(ctx, a.gate())
	if err != nil {
		return errors.Join(err, a.lease.Abandon())
	}
	defer func() { _ = gate.Unlock() }()
	return a.lease.Release()
}
