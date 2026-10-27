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

package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/pull/format"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// The ways a snapshot is no longer served. Each is errGone, 410, to a
// request; a sweep tells them apart.
var (
	errGone = errors.New("server: the snapshot is gone")
	// errRemoved is a tag the conversation's history records removed.
	errRemoved = fmt.Errorf("%w: its tag was removed", errGone)
	// errExpired is a snapshot whose expiry is committed.
	errExpired = fmt.Errorf("%w: it expired", errGone)
	// errNoSnapshot is a tag with no snapshot record: never the server's,
	// or not one any more.
	errNoSnapshot = fmt.Errorf("%w: no snapshot is at its tag", errGone)
	// errUnresolved is the tag of a publication that has not finished: it
	// is blocked until its recovery discards it.
	errUnresolved = fmt.Errorf("%w: its publication did not finish", errGone)
)

// activeWork holds a snapshot while a request or a builder reads it: a lock
// file in its _requests/, which a removal probes and waits for.
func (s *Server) activeWork(dir string) (release func(), err error) {
	for {
		id, err := newID()
		if err != nil {
			return nil, err
		}
		path := filepath.Join(requestsDir(dir), id+".lock")
		if there, err := exists(path); err != nil || there {
			if err != nil {
				return nil, err
			}
			continue
		}
		l, err := storage.TryLockFile(path)
		if err != nil {
			return nil, err
		}
		return func() {
			_ = l.Unlock()
			_ = os.Remove(path)
		}, nil
	}
}

// held says whether any request or builder holds a snapshot, by probing
// every lock file in its _requests/ and _archives/ without waiting.
func held(dir string) (bool, error) {
	for _, sub := range []string{requestsDir(dir), archivesDir(dir)} {
		entries, err := os.ReadDir(sub)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".lock") {
				continue
			}
			busy, err := storage.ProbeFileLock(filepath.Join(sub, e.Name()))
			if err != nil || busy {
				return true, err
			}
		}
	}
	return false, nil
}

// lifecycle reads a snapshot's state under the global lock: its record, and
// whether it may still be used at now. An unfinished publication's tag, a
// removed tag, a committed expiry and a missing record each refuse it as
// errGone. An expiry, seen or reached, is committed by a fresh write before
// anything relies on it: one that went no further than the page cache must
// not come back after a power loss as a snapshot still in use.
func (s *Server) lifecycle(conversation, tag string, now time.Time) (*snapshotRecord, error) {
	st, err := readState(s.p)
	if err != nil {
		return nil, err
	}
	if p := st.Publishing; p != nil && p.Conversation == conversation && p.Tag == tag {
		return nil, errUnresolved
	}
	dir := s.p.snapshot(conversation, tag)
	h, err := readHistory(s.p, conversation)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNoSnapshot
	}
	if err != nil {
		return nil, err
	}
	if h.removed(tag) {
		return nil, errRemoved
	}
	rec, err := readSnapshot(dir, conversation, tag)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNoSnapshot
	}
	if err != nil {
		return nil, err
	}
	use, at, err := readLastUse(dir, tag)
	if err != nil {
		return nil, err
	}
	if use.State == useActive && !now.Before(at.Add(s.o.TTL)) {
		use.State = useExpired
	}
	if use.State == useExpired {
		if err := replaceRecord(lastUsePath(dir), use, s.o.Files); err != nil {
			return nil, err
		}
		return nil, errExpired
	}
	return rec, nil
}

// expire commits a snapshot's expiry with its last use kept, under the
// global lock: no new work starts on it, and work already holding it ends.
// expireIfUnleased expires a superseded snapshot that holds no open lease,
// and says whether it did. The supersession it relies on is committed
// again first.
func (s *Server) expireIfUnleased(dir, tag string, rec *snapshotRecord) (bool, error) {
	if rec.SupersededBy == "" {
		return false, nil
	}
	if err := replaceRecord(snapshotRecordPath(dir), rec, s.o.Files); err != nil {
		return false, err
	}
	open, err := openLeases(dir)
	if err != nil || open {
		return false, err
	}
	return true, s.expire(dir, tag)
}

func (s *Server) expire(dir, tag string) error {
	use, _, err := readLastUse(dir, tag)
	if err != nil {
		return err
	}
	use.State = useExpired
	return replaceRecord(lastUsePath(dir), use, s.o.Files)
}

// use records a request's use of a snapshot, under the global lock: its
// last use moves to now, never back.
func (s *Server) use(conversation, tag string, now time.Time) error {
	dir := s.p.snapshot(conversation, tag)
	use, at, err := readLastUse(dir, tag)
	if err != nil {
		return err
	}
	if !now.After(at) {
		return nil
	}
	use.LastUse = stamp(now)
	return replaceRecord(lastUsePath(dir), use, s.o.Files)
}

// authorize checks a request may still use its lease, under the global
// lock: its token is still in the tokens file, and the lease is open, of
// this snapshot and of that token.
func (s *Server) authorize(conversation, tag, lease string, caller Caller) error {
	if !s.o.Guard.Holds(caller) {
		return errForbidden
	}
	l, err := readLease(s.p.snapshot(conversation, tag), tag, lease)
	if errors.Is(err, os.ErrNotExist) {
		return errForbidden
	}
	if err != nil {
		return err
	}
	if l.TokenHash != caller.Hash || l.State != leaseOpen {
		return errForbidden
	}
	return nil
}

var errForbidden = errors.New("server: the lease is not this token's, or is not open")

// grantLease gives a request a lease on a ready snapshot and records its
// use, under the global lock. The request holds the snapshot already.
func (s *Server) grantLease(conversation, tag string, caller Caller) (string, error) {
	// A transition run again after a busy replacement keeps the one lease
	// it made, rather than leave it open and make another.
	var lease string
	err := s.global(func() error {
		rec, err := s.lifecycle(conversation, tag, s.now())
		if err != nil {
			return err
		}
		if !s.o.Guard.Holds(caller) {
			return errForbidden
		}
		dir := s.p.snapshot(conversation, tag)
		if lease == "" {
			// An older snapshot whose last lease was released is due, though
			// a release cut short by a busy replacement has not said so yet:
			// it expires here, and takes no new lease.
			if due, err := s.expireIfUnleased(dir, tag, rec); err != nil {
				return err
			} else if due {
				return errGone
			}
			for {
				id, err := newID()
				if err != nil {
					return err
				}
				if there, err := exists(leasePath(dir, id)); err != nil {
					return err
				} else if !there {
					lease = id
					break
				}
			}
			if err := newRecord(leasePath(dir, lease), &leaseRecord{Schema: stateSchema, Lease: lease, Snapshot: tag,
				TokenHash: caller.Hash, State: leaseOpen}, s.o.Files); err != nil {
				lease = ""
				return err
			}
		}
		return s.use(conversation, tag, s.now())
	})
	return lease, err
}

// manifestOf reads a snapshot's saved manifest.
func (s *Server) manifestOf(conversation, tag string) (*format.Manifest, error) {
	f, err := storage.OpenEvidence(filepath.Join(s.p.snapshot(conversation, tag), format.ManifestFile), nil)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return format.ReadManifest(f, conversation)
}

// release is a lease's done: on a snapshot still served, this token's lease
// is released, after its use is recorded; one already released by it is
// done again. A snapshot not served answers errGone; a lease of another
// token, or none, errForbidden.
func (s *Server) release(conversation, tag, lease string, caller Caller) error {
	err := s.global(func() error {
		rec, err := s.lifecycle(conversation, tag, s.now())
		if err != nil {
			return err
		}
		dir := s.p.snapshot(conversation, tag)
		l, err := readLease(dir, tag, lease)
		if errors.Is(err, os.ErrNotExist) {
			return errForbidden
		}
		if err != nil {
			return err
		}
		if l.TokenHash != caller.Hash {
			return errForbidden
		}
		if l.State == leaseReleased {
			return nil
		}
		if err := s.use(conversation, tag, s.now()); err != nil {
			return err
		}
		l.State = leaseReleased
		if err := replaceRecord(leasePath(dir, lease), l, s.o.Files); err != nil {
			return err
		}
		// An older snapshot's last lease makes it due in this same
		// transition: its expiry is committed now, with its last use kept,
		// so no lease is granted on it between this release and its
		// removal. A run cut short by a busy replacement leaves that to the
		// next grant, which expires it, or to its removal.
		_, err = s.expireIfUnleased(dir, tag, rec)
		return err
	})
	if err != nil {
		return err
	}
	return s.removeIfDue(conversation, tag)
}

// reuse finds the newest snapshot of a conversation that a freeze's
// evidence matches: not superseded, not removed, not expired, of the same
// file-list digest, and accepted as whole evidence while it is held. It
// returns its tag, or empty when a new one must be published. A
// supersession it sees is committed by a fresh write before it is relied
// on.
func (s *Server) reuse(conversation string, accepted *format.Accepted) (string, error) {
	tags, err := s.tagsOf(conversation)
	if err != nil || len(tags) == 0 {
		return "", err
	}
	tag := tags[len(tags)-1]
	dir := s.p.snapshot(conversation, tag)
	eligible := func() (*snapshotRecord, error) {
		rec, err := s.lifecycle(conversation, tag, s.now())
		if errors.Is(err, errGone) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if rec.SupersededBy != "" {
			return nil, replaceRecord(snapshotRecordPath(dir), rec, s.o.Files)
		}
		if format.ManifestDigest(rec.ManifestDigest) != accepted.Digest {
			return nil, nil
		}
		return rec, nil
	}
	// The hold is taken with the check, under the global lock.
	var rec *snapshotRecord
	var release func()
	err = s.global(func() error {
		if release != nil {
			release()
			release = nil
		}
		var err error
		if rec, err = eligible(); err != nil || rec == nil {
			return err
		}
		release, err = s.activeWork(dir)
		return err
	})
	if release != nil {
		defer release()
	}
	if err != nil || rec == nil {
		return "", err
	}
	if _, err := format.Accept(dir, format.ServerSnapshot, format.Want{Conversation: conversation,
		ManifestDigest: accepted.Digest, Session: rec.Session}); err != nil {
		s.logf("pull: %s/%s does not accept as whole evidence, and is not reused: %v", conversation, tag, err)
		return "", nil
	}
	var again *snapshotRecord
	if err := s.global(func() (err error) { again, err = eligible(); return }); err != nil || again == nil {
		return "", err
	}
	return tag, nil
}

// publish gives a freeze's tree a new tag: the tag and the intent are
// reserved under the global lock, the tree finished and made durable
// outside it, then under it again the job and the intent are checked, the
// tree renamed to its tag without replacing anything, older snapshots
// marked superseded, its first use written, and the intent cleared. Each
// step under the lock is safe to run again: a busy replacement on Windows
// repeats the whole transition, and what it already did is recognized.
func (s *Server) publish(job *freezeJob, tree string, m *format.Manifest, accepted *format.Accepted) (string, error) {
	reserved := ""
	for {
		wait := false
		err := s.global(func() error {
			wait = false
			st, err := readState(s.p)
			if err != nil {
				return err
			}
			own := reserved != "" && st.Publishing != nil && st.Publishing.Build == job.Freeze && st.Publishing.Tag == reserved
			if st.Publishing != nil && !own {
				wait = true
				return nil
			}
			root, err := s.probeConversation(job.Conversation)
			if err != nil {
				return err
			}
			defer func() { _ = root.Unlock() }()
			h, err := readHistory(s.p, job.Conversation)
			if err != nil {
				return err
			}
			if !own {
				tag, err := storage.NextTag(s.now(), max(st.GreatestTag, h.GreatestTag))
				if err != nil {
					return err
				}
				st.GreatestTag = tag
				st.Publishing = &publishing{Conversation: job.Conversation, Build: job.Freeze, Tag: tag, ManifestDigest: string(accepted.Digest)}
				if err := replaceRecord(s.p.state(), st, s.o.Files); err != nil {
					return err
				}
				reserved = tag
			}
			h.GreatestTag = max(h.GreatestTag, reserved)
			return replaceRecord(s.p.history(job.Conversation), h, s.o.Files)
		})
		if errors.Is(err, storage.ErrBusy) {
			wait = true
		} else if err != nil {
			return "", err
		}
		if !wait {
			break
		}
		// Another publication holds the one intent slot: recover it when
		// its owner is gone, else wait for it.
		if err := s.settleIntent(false); err != nil {
			return "", err
		}
		select {
		case <-s.ctx.Done():
			return "", s.ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	tag := reserved
	if err := s.finishTree(tree, job.Conversation, tag, m, accepted); err != nil {
		return "", err
	}
	dest := s.p.snapshot(job.Conversation, tag)
	err := s.global(func() error {
		root, err := s.probeConversation(job.Conversation)
		if err != nil {
			return err
		}
		defer func() { _ = root.Unlock() }()
		st, err := readState(s.p)
		if err != nil {
			return err
		}
		if p := st.Publishing; p == nil || p.Build != job.Freeze || p.Tag != tag || p.ManifestDigest != string(accepted.Digest) {
			return stateError("the publication of %s changed while it was prepared", tag)
		}
		// A transition run again finds its tree already published.
		if there, err := exists(dest); err != nil {
			return err
		} else if there {
			rec, err := readSnapshot(dest, job.Conversation, tag)
			if err != nil || rec.ManifestDigest != string(accepted.Digest) {
				return stateError("%s is not this publication's tree", dest)
			}
		} else if err := storage.PublishDir(tree, dest, s.o.Files); err != nil {
			return err
		}
		if err := s.supersede(job.Conversation, tag); err != nil {
			return err
		}
		if err := replaceRecord(lastUsePath(dest), &lastUse{Schema: stateSchema, Snapshot: tag, LastUse: stamp(s.now()),
			State: useActive}, s.o.Files); err != nil {
			return err
		}
		st.Publishing = nil
		return replaceRecord(s.p.state(), st, s.o.Files)
	})
	if err != nil {
		return "", err
	}
	return tag, nil
}

// finishTree writes a freeze tree's snapshot record and its control
// directories, then makes the tree durable from its leaves up and its
// ancestors from the root down.
func (s *Server) finishTree(tree, conversation, tag string, m *format.Manifest, accepted *format.Accepted) error {
	if err := newRecord(snapshotRecordPath(tree), &snapshotRecord{Schema: stateSchema, Conversation: conversation,
		Session: m.Session, Tag: tag, ManifestDigest: string(accepted.Digest)}, s.o.Files); err != nil {
		return err
	}
	for _, dir := range []string{leasesDir(tree), requestsDir(tree), archivesDir(tree)} {
		if err := storage.MakeOwnedDir(dir, s.o.Files); err != nil {
			return err
		}
	}
	if err := storage.SyncTree(tree, s.o.Files); err != nil {
		return err
	}
	return storage.SyncAncestors(s.o.Root, s.p.conversation(conversation), s.o.Files)
}

// supersede marks every older snapshot of a conversation superseded by tag.
// Each record is written whole again, one already superseded too: a
// supersession seen is committed before it is relied on. The caller holds
// the global lock.
func (s *Server) supersede(conversation, tag string) error {
	tags, err := s.tagsOf(conversation)
	if err != nil {
		return err
	}
	for _, older := range tags {
		if older >= tag {
			continue
		}
		dir := s.p.snapshot(conversation, older)
		rec, err := readSnapshot(dir, conversation, older)
		if err != nil {
			s.logf("pull: %s/%s: %v", conversation, older, err)
			continue
		}
		if rec.SupersededBy == "" {
			rec.SupersededBy = tag
		}
		if err := replaceRecord(snapshotRecordPath(dir), rec, s.o.Files); err != nil {
			return err
		}
	}
	return nil
}

// settleIntent recovers a publication whose owner is gone, holding that
// owner's lock through the recovery; one whose owner still works is left to
// it. A worker settles within its own slot; anything else takes a freeze
// slot first, and leaves the recovery to a later pass when none is free.
func (s *Server) settleIntent(standalone bool) error {
	if standalone {
		if !takeSlot(s.freezeSlots) {
			return nil
		}
		defer giveSlot(s.freezeSlots)
	}
	var intent *publishing
	var owner *storage.SessionLock
	err := s.global(func() error {
		intent, owner = nil, nil
		st, err := readState(s.p)
		if err != nil || st.Publishing == nil {
			return err
		}
		l, err := storage.TryLockFile(s.p.freezeLock(st.Publishing.Build))
		if errors.Is(err, storage.ErrBusy) {
			return nil
		}
		if err != nil {
			return err
		}
		intent, owner = st.Publishing, l
		return nil
	})
	if err != nil || intent == nil {
		return err
	}
	defer func() { _ = owner.Unlock() }()
	return s.discardPublication(intent)
}

// discardPublication settles a publication that did not finish. The caller
// holds its owner. Its tag is recorded removed in the conversation's
// history, by a fresh write even when it is there already, after the tree
// at its name is checked to be this publication's own; that tree is
// detached, then deleted under the conversation's _root without the global
// lock, and the intent cleared. An abandoned publication is never finished.
func (s *Server) discardPublication(intent *publishing) error {
	var detached string
	var root *storage.SessionLock
	err := s.global(func() error {
		detached = ""
		r, err := s.probeConversation(intent.Conversation)
		if err != nil {
			return err
		}
		keep := false
		defer func() {
			if !keep {
				_ = r.Unlock()
			}
		}()
		st, err := readState(s.p)
		if err != nil {
			return err
		}
		if st.Publishing == nil || *st.Publishing != *intent {
			return nil
		}
		h, err := readHistory(s.p, intent.Conversation)
		if err != nil {
			return err
		}
		dir := s.p.snapshot(intent.Conversation, intent.Tag)
		there, err := exists(dir)
		if err != nil {
			return err
		}
		if there && !h.removed(intent.Tag) {
			rec, err := readSnapshot(dir, intent.Conversation, intent.Tag)
			if err != nil || rec.ManifestDigest != intent.ManifestDigest {
				return stateError("%s is not this publication's tree; it is left for a person to repair", dir)
			}
		}
		h.remove(intent.Tag)
		if err := replaceRecord(s.p.history(intent.Conversation), h, s.o.Files); err != nil {
			return err
		}
		if there {
			if detached, err = storage.Detach(dir, s.p.removing(intent.Conversation), intent.Tag, s.o.Files); err != nil {
				return err
			}
		}
		keep, root = true, r
		return nil
	})
	if err != nil {
		return err
	}
	if root != nil {
		// The detached tree is deleted under _root alone, which is let go
		// before the global lock is taken again.
		if detached != "" {
			err = storage.DeleteOwned(filepath.Dir(detached), filepath.Base(detached), s.o.Files)
		}
		_ = root.Unlock()
		if err != nil {
			return err
		}
	}
	return s.global(func() error {
		st, err := readState(s.p)
		if err != nil {
			return err
		}
		if st.Publishing == nil || *st.Publishing != *intent {
			return nil
		}
		// What the publication marked superseded before it stopped stays
		// superseded, committed again before the intent goes.
		if err := s.commitSupersededBy(intent.Conversation, intent.Tag); err != nil {
			return err
		}
		st.Publishing = nil
		return replaceRecord(s.p.state(), st, s.o.Files)
	})
}

// commitSupersededBy writes again, whole, every snapshot record of a
// conversation superseded by tag. The caller holds the global lock.
func (s *Server) commitSupersededBy(conversation, tag string) error {
	tags, err := s.tagsOf(conversation)
	if err != nil {
		return err
	}
	for _, older := range tags {
		dir := s.p.snapshot(conversation, older)
		rec, err := readSnapshot(dir, conversation, older)
		if err != nil || rec.SupersededBy != tag {
			continue
		}
		if err := replaceRecord(snapshotRecordPath(dir), rec, s.o.Files); err != nil {
			return err
		}
	}
	return nil
}

// retain removes a conversation's snapshots that are due, and finishes
// deleting what an earlier pass detached and did not delete.
func (s *Server) retain(conversation string) {
	tags, err := s.tagsOf(conversation)
	if err != nil {
		s.sweepLog(conversation, err)
		return
	}
	for _, tag := range tags {
		if err := s.removeIfDue(conversation, tag); err != nil {
			s.sweepLog(conversation+"/"+tag, err)
		}
	}
	s.replayRemoving(conversation)
}

// removeIfDue removes one snapshot when it is due and nothing works in it:
// an expired one, a removed tag's tree left behind, or a superseded one no
// open lease holds. Its tag is recorded removed in the history, written
// whole again, then the tree is detached into _removing and deleted under
// the conversation's _root. A superseded snapshot whose last lease went
// while work still holds it is expired instead, so no new work starts on
// it, and goes once that work ends. An unfinished publication is left to
// its recovery, and a tag with no snapshot record to a person.
func (s *Server) removeIfDue(conversation, tag string) error {
	dir := s.p.snapshot(conversation, tag)
	var detached string
	var root *storage.SessionLock
	err := s.global(func() error {
		detached = ""
		rec, err := s.lifecycle(conversation, tag, s.now())
		switch {
		case errors.Is(err, errRemoved), errors.Is(err, errExpired):
		case errors.Is(err, errUnresolved), errors.Is(err, errNoSnapshot):
			return nil
		case err != nil:
			return err
		case rec.SupersededBy == "":
			return nil
		default:
			// The supersession is committed before it is relied on.
			if err := replaceRecord(snapshotRecordPath(dir), rec, s.o.Files); err != nil {
				return err
			}
			open, err := openLeases(dir)
			if err != nil || open {
				return err
			}
		}
		if busy, err := held(dir); err != nil {
			return err
		} else if busy {
			if rec != nil {
				return s.expire(dir, tag)
			}
			return nil
		}
		r, err := s.probeConversation(conversation)
		if err != nil {
			return err
		}
		keep := false
		defer func() {
			if !keep {
				_ = r.Unlock()
			}
		}()
		h, err := readHistory(s.p, conversation)
		if err != nil {
			return err
		}
		h.remove(tag)
		if err := replaceRecord(s.p.history(conversation), h, s.o.Files); err != nil {
			return err
		}
		if there, err := exists(dir); err != nil {
			return err
		} else if there {
			if detached, err = storage.Detach(dir, s.p.removing(conversation), tag, s.o.Files); err != nil {
				return err
			}
		}
		keep, root = true, r
		return nil
	})
	if root != nil {
		defer func() { _ = root.Unlock() }()
	}
	if errors.Is(err, storage.ErrBusy) {
		return nil
	}
	if err != nil || detached == "" {
		return err
	}
	return storage.DeleteOwned(filepath.Dir(detached), filepath.Base(detached), s.o.Files)
}

// replayRemoving finishes deleting what a conversation's _removing/ still
// holds: trees a removal detached and a stop or a failed deletion left,
// whatever their names now. The server owns that directory whole. Before
// each entry goes, the history is written again, whole, with the tag a
// tree's name carries recorded, under the global lock and the
// conversation's _root, which is kept through the deletion.
func (s *Server) replayRemoving(conversation string) {
	removing := s.p.removing(conversation)
	entries, err := os.ReadDir(removing)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		s.sweepLog(removing, err)
		return
	}
	for _, e := range entries {
		name := e.Name()
		tag := ""
		if cut := strings.LastIndexByte(name, '-'); cut > 0 {
			if t, err := storage.ParseTag(name[:cut]); err == nil && storage.Stamp(t) == name[:cut] {
				tag = name[:cut]
			}
		}
		var root *storage.SessionLock
		err := s.global(func() error {
			r, err := s.probeConversation(conversation)
			if err != nil {
				return err
			}
			h, err := readHistory(s.p, conversation)
			if err == nil {
				if tag != "" {
					h.remove(tag)
				}
				err = replaceRecord(s.p.history(conversation), h, s.o.Files)
			}
			if err != nil {
				_ = r.Unlock()
				return err
			}
			root = r
			return nil
		})
		if errors.Is(err, storage.ErrBusy) {
			continue
		}
		if err != nil {
			s.sweepLog(removing+"/"+name, err)
			continue
		}
		// A directory goes whole, never through a link; anything else is
		// unlinked, a link itself and never what it leads to.
		if e.IsDir() {
			err = storage.DeleteOwned(removing, name, s.o.Files)
		} else if err = s.files().Remove(filepath.Join(removing, name)); err == nil {
			err = s.files().SyncDir(removing)
		}
		_ = root.Unlock()
		s.sweepLog(removing+"/"+name, err)
	}
}

// openLeases says whether any lease of a snapshot is still open.
func openLeases(dir string) (bool, error) {
	entries, err := os.ReadDir(leasesDir(dir))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	tag := filepath.Base(dir)
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !storage.Digest64(id) {
			continue
		}
		l, err := readLease(dir, tag, id)
		if err != nil {
			return false, err
		}
		if l.State == leaseOpen {
			return true, nil
		}
	}
	return false, nil
}
