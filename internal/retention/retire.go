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

package retention

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecodeprovider"
	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp"
	"github.com/apache/skywalking-ai-sessionizer/internal/metrics"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// The points a retirement reaches, in order, for Stop. A directory's point is
// reached with each place RemoveDir reports, "renamed" and "deleting", and
// with "removed" once the directory is gone. The state's point counts the
// state files rewritten.
const (
	PhaseRetireMarked  = "retire marked"
	PhaseRetireHanded  = "retire handed off"
	PhaseRetireSession = "retire session"
	PhaseRetireChain   = "retire chain"
	PhaseRetireTidied  = "retire tidied"
	PhaseRetireState   = "retire state"
)

// errNotNow is an idle session that may not be retired yet. It is tried
// again on the next pass.
var errNotNow = errors.New("retention: the session may not be retired now")

// idle says whether a session has been idle for retire_after. It is read
// from the header of the newest landed file or skeleton, the clock level 1
// reads too, and never from a record's time, a file's time or a removal's.
func (r *Remover) idle(id string) (bool, error) {
	files, err := storage.LandedFiles(r.Zone, id)
	if err != nil || len(files) == 0 {
		return false, err
	}
	at, err := landedAt(files[len(files)-1])
	if err != nil {
		return false, err
	}
	return r.now().Sub(at) >= r.RetireAfter, nil
}

// retirable says whether an idle session may be retired now: no removal of
// its files is in progress, its chain covers every landed file, and its
// metrics are derived when the pipeline derives them, so each file's request
// exists for the hand-off. Sending is not waited for. It is asked under the
// session's locks.
func (r *Remover) retirable(id string) (bool, error) {
	files, err := storage.LandedFiles(r.Zone, id)
	if err != nil || len(files) == 0 {
		return false, err
	}
	if intent, err := storage.LoadRemoving(r.Zone.RemovingPath(id)); err != nil || intent != nil {
		return false, err
	}
	through, parsed, err := chainThrough(r.Zone.Root(), id)
	if err != nil || !parsed || through < files[len(files)-1].Seq {
		return false, err
	}
	if r.Metrics {
		return metrics.DerivedAll(r.Zone, id, files)
	}
	return true, nil
}

// RecoverRetired carries on every retirement that an earlier pass or command
// began and did not finish, whatever the settings are now, both levels off
// included: a session whose marker is there is retired, and what is left of
// it goes. Every command that writes to a root runs it first, under the root
// lock. A retirement this process found finished is not looked at again:
// nothing writes to a retired session, so nothing of it comes back.
func (r *Remover) RecoverRetired() Result {
	var res Result
	ids, err := storage.RetiredSessions(r.Zone)
	if err != nil {
		res.Errors = append(res.Errors, err)
		return res
	}
	var work []string
	for _, id := range ids {
		if !r.settled[id] {
			work = append(work, id)
		}
	}
	if len(work) == 0 {
		return res
	}
	r.exported(&res, func(sent *otlp.Sent) { r.retire(&res, work, sent, false) })
	return res
}

// retiredDirs are the directories of a session a retirement removes, and
// where RemoveDir moves each of them.
func (r *Remover) retiredDirs(id string) []string {
	root := r.Zone.Root()
	removed := filepath.Join(root, storage.RemovedDir)
	return []string{
		r.Zone.SessionDir(id), filepath.Join(root, conversationsDir, id),
		filepath.Join(removed, id), filepath.Join(removed, id+".chain"),
	}
}

// retire carries the retirement of each session as far as it goes, under the
// metrics and provider locks, in the order every command takes the locks:
// the marker, the spool hand-off, the directories and the spool tidy for each,
// then the root's state once for every session that got that far. decide says
// the sessions are idle and not retired yet, so whether each may be retired is
// asked under its locks. A session already retired is carried on, whatever it
// is now.
func (r *Remover) retire(res *Result, ids []string, sent *otlp.Sent, decide bool) {
	if len(ids) == 0 {
		return
	}
	root := r.Zone.Root()
	spool := storage.NewSpool(r.Zone)
	requests, err := spool.List()
	if err != nil {
		res.Errors = append(res.Errors, err)
		return
	}
	// Each lock is taken only when there is something under it, since taking
	// a lock makes its directory. The metrics lock is needed by a session
	// whose receipts make requests, which may make the spool.
	spoolThere, err := present(spool.Dir())
	if err != nil {
		res.Errors = append(res.Errors, err)
		return
	}
	needMetrics := spoolThere
	for _, id := range ids {
		if there, err := present(metrics.ReceiptsDir(r.Zone, id)); err == nil && there {
			needMetrics = true
		}
	}
	if needMetrics {
		lock, err := storage.LockMetricsWait(root, 0)
		if errors.Is(err, storage.ErrMetricsBusy) {
			return
		}
		if err != nil {
			res.Errors = append(res.Errors, err)
			return
		}
		defer func() { _ = lock.Unlock() }()
	}
	providerThere, err := present(filepath.Join(root, "_provider"))
	if err != nil {
		res.Errors = append(res.Errors, err)
		return
	}
	if providerThere {
		lock, err := storage.LockProvider(root)
		if errors.Is(err, storage.ErrProviderBusy) {
			return
		}
		if err != nil {
			res.Errors = append(res.Errors, err)
			return
		}
		defer func() { _ = lock.Unlock() }()
	}

	var done []string
	for _, id := range ids {
		removed, err := r.retireOne(id, sent, requests, decide)
		// Its directories are gone even when what came after failed, and
		// no later attempt finds them to say so.
		if removed {
			res.Retired = append(res.Retired, id)
		}
		if errors.Is(err, errBusy) || errors.Is(err, errNotNow) {
			continue
		}
		if err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("%s: retiring: %w", id, err))
			continue
		}
		done = append(done, id)
	}
	if len(done) == 0 {
		return
	}
	// Every directory a retirement changes is synced before the state forgets
	// anything, the errors checked. A power loss that brought back a session's
	// directory, its receipts with it, after push.state forgot what the
	// receiver has, would make those requests again and send them, perhaps
	// under another owner. One that brought back a deleted request would leave
	// it with no line and no owner. Every attempt syncs them, not only one
	// that changed something: an earlier attempt may have, and stopped on a
	// failed sync.
	for _, dir := range []string{root, filepath.Join(root, conversationsDir), filepath.Join(root, storage.RemovedDir), spool.Dir()} {
		there, err := present(dir)
		if err != nil {
			res.Errors = append(res.Errors, err)
			return
		}
		if !there {
			continue
		}
		if err := r.syncDir(dir); err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("%s may not survive a power loss, so the retirement stops here: %w", dir, err))
			return
		}
	}
	// A request is waiting when the receiver does not have it. The spool is
	// read again for it, since the hand-off may have made a request again.
	left, err := spool.List()
	if err != nil {
		res.Errors = append(res.Errors, err)
		return
	}
	waiting := map[string]bool{}
	for _, f := range left {
		if _, answered := sent.Digest(storage.SpoolDir + "/" + filepath.Base(f.Path)); !answered {
			waiting[f.Session] = true
		}
	}

	// Step 4, once for all of them. Each rewrites one state file, and a line
	// goes only once what it names is gone, so no file is derived twice. The
	// owner stays while a request of the session waits in the spool.
	now := r.now()
	if err := otlp.ForgetGone(root, owned(done), now); err != nil {
		res.Errors = append(res.Errors, err)
		return
	}
	if err := r.stop(PhaseRetireState, 1); err != nil {
		res.Errors = append(res.Errors, err)
		return
	}
	if needMetrics {
		if err := metrics.Forget(r.Zone, done, now); err != nil {
			res.Errors = append(res.Errors, err)
			return
		}
	}
	if err := r.stop(PhaseRetireState, 2); err != nil {
		res.Errors = append(res.Errors, err)
		return
	}
	if providerThere {
		if err := claudecodeprovider.Retire(r.Zone, done, now); err != nil {
			res.Errors = append(res.Errors, err)
			return
		}
	}
	if err := r.stop(PhaseRetireState, 3); err != nil {
		res.Errors = append(res.Errors, err)
		return
	}
	if r.settled == nil {
		r.settled = map[string]bool{}
	}
	for _, id := range done {
		if !waiting[id] {
			r.settled[id] = true
		}
	}
}

// retireOne is steps 1 to 3 and the spool tidy for one session, and reports
// whether any directory of it was there to remove, also when something after
// the directories failed.
//
// The marker goes first and is made durable before anything is deleted:
// after it, a crash anywhere leaves a session that nothing collects, and the
// next pass finishes it. It is written again by every attempt that deletes
// anything, since an earlier attempt may have renamed it and stopped before
// its sync, and a later sync that succeeds does not prove the first one's
// data reached the disk.
func (r *Remover) retireOne(id string, sent *otlp.Sent, requests []storage.SpoolFile, decide bool) (bool, error) {
	root := r.Zone.Root()
	dirs := r.retiredDirs(id)
	sessionLock, err := lockIfThere(dirs[0], storage.LockSessionToRemove, storage.ErrSessionBusy)
	if err != nil {
		return false, err
	}
	defer func() { unlock(&sessionLock) }()
	chainLock, err := lockIfThere(dirs[1], storage.LockChain, storage.ErrChainBusy)
	if err != nil {
		return false, err
	}
	defer func() { unlock(&chainLock) }()
	// The conversation directory named by the session's id is its chain
	// only when its rounds name the session, as the push reads them. One
	// whose rounds name another session is that session's chain, and stays.
	ownChain := true
	if hdr, rounds, err := headRound(root, id); err != nil {
		return false, err
	} else if rounds && hdr.Session != id {
		ownChain = false
		unlock(&chainLock)
	}
	if decide {
		if now, err := r.retirable(id); err != nil || !now {
			if err == nil {
				err = errNotNow
			}
			return false, err
		}
	}

	left := false
	for _, dir := range dirs {
		if !ownChain && (dir == dirs[1] || dir == dirs[3]) {
			continue
		}
		there, err := present(dir)
		if err != nil {
			return false, err
		}
		left = left || there
	}
	var answered []string
	for _, f := range requests {
		if _, ok := sent.Digest(storage.SpoolDir + "/" + filepath.Base(f.Path)); ok && f.Session == id {
			answered = append(answered, f.Path)
		}
	}
	if !left && len(answered) == 0 && !decide {
		return false, nil
	}

	// Step 1.
	if err := r.mark(id); err != nil {
		return false, err
	}
	if err := r.stop(PhaseRetireMarked, 0); err != nil {
		return false, err
	}

	// Step 2, while the session's directory holds its receipts. A directory
	// renamed away was renamed after its hand-off was durable.
	answeredBy := func(rel string) bool {
		_, ok := sent.Digest(rel)
		return ok
	}
	written, err := metrics.HandOff(r.Zone, id, answeredBy, r.syncDir)
	if err != nil {
		return false, err
	}
	// The spool may be new, and its own name is in the root.
	if written > 0 {
		if err := r.syncDir(root); err != nil {
			return false, fmt.Errorf("the spool may not survive a power loss, so the retirement stops here: %w", err)
		}
	}
	if err := r.stop(PhaseRetireHanded, written); err != nil {
		return false, err
	}

	// Step 3. Each directory goes in one rename, its lock released first:
	// Windows cannot rename a directory that holds an open lock. The two
	// directories each rename changed are synced, the errors checked, before
	// anything moved is deleted.
	for i, held := range []**storage.SessionLock{&sessionLock, &chainLock} {
		if i == 1 && !ownChain {
			continue
		}
		dir, target := dirs[i], dirs[i+2]
		phase := []string{PhaseRetireSession, PhaseRetireChain}[i]
		unlock(held)
		if err := storage.RemoveDirDurable(root, dir, filepath.Base(target), func(where string) error {
			return r.stop(phase+" "+where, 0)
		}, r.syncDir); err != nil {
			return false, err
		}
		if err := r.stop(phase+" removed", 0); err != nil {
			return false, err
		}
	}

	// The spool tidy: every request of the session the receiver has, a
	// partly rejected one included, since a pusher does not send it again.
	// The spool is synced with the directories, before the state.
	if len(answered) > 0 {
		for _, path := range answered {
			if err := storage.DeleteTree(path, nil); err != nil {
				return left, err
			}
		}
		if err := r.stop(PhaseRetireTidied, len(answered)); err != nil {
			return left, err
		}
	}
	return left, nil
}

// mark writes a session's retired marker and makes it durable: the marker's
// directory and the root are synced, every error checked. A marker already
// there is written again over itself, never deleted first, so it is never
// absent. It is writable, since on Windows a rename cannot replace a
// read-only file.
func (r *Remover) mark(id string) error {
	path := r.Zone.RetiredPath(id)
	if err := storage.WriteAtomicDurable(path, storage.PermState, func(io.Writer) error { return nil }, r.syncDir); err != nil {
		return err
	}
	if err := r.syncDir(r.Zone.Root()); err != nil {
		return fmt.Errorf("%s may not survive a power loss, so the retirement stops here: %w", storage.RetiredDir, err)
	}
	return nil
}

// owned says which lines of push.state belong to the sessions: their landed
// files, their conversation's rounds, their metrics requests, and their owner
// lines, which are named by the session's id alone.
func owned(ids []string) func(rel string) bool {
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	return func(rel string) bool {
		first, rest, ok := strings.Cut(rel, "/")
		switch {
		case !ok:
			return set[rel]
		case first == conversationsDir:
			conv, _, _ := strings.Cut(rest, "/")
			return set[conv]
		case first == storage.SpoolDir:
			session, ok := metrics.SpoolOwner(rest)
			return ok && set[session]
		}
		return set[first]
	}
}

// lockIfThere takes a directory's lock only when the directory is there,
// since taking a lock makes its directory.
func lockIfThere(dir string, lock func(string) (*storage.SessionLock, error), busy error) (*storage.SessionLock, error) {
	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, err
	case !fi.IsDir():
		return nil, fmt.Errorf("%s is a symbolic link or not a directory", dir)
	}
	l, err := lock(dir)
	if errors.Is(err, busy) {
		return nil, errBusy
	}
	return l, err
}

func unlock(held **storage.SessionLock) {
	if *held != nil {
		_ = (*held).Unlock()
		*held = nil
	}
}

// present reports whether a path is there. Anything other than its not being
// there is an error.
func present(path string) (bool, error) {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, err
}
