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
	"os"
	"path/filepath"
	"time"
)

// ErrSessionBusy means another collector holds this session.
var ErrSessionBusy = errors.New("storage: session is locked by another collector")

// SessionLock is an exclusive lock over one directory, held through an open
// file. The platform files supply lockDir and unlock; this file is what every
// platform shares.
//
// A session is the unit of work precisely because its landed sequence must be
// monotonic across every stream in it. Two collectors sharing a storage root
// would otherwise allocate the same sequence numbers to different content, and
// the assembler's single watermark would skip whichever it reached second.
type SessionLock struct{ f *os.File }

// ErrRemovalInProgress means a removal of the session's files has not
// finished. It is ErrSessionBusy to a collector: nothing lands into the
// session until the pass that finishes the removal has run.
var ErrRemovalInProgress = fmt.Errorf("%w: a removal of its files is in progress", ErrSessionBusy)

// LockSession takes the lock, returning ErrSessionBusy if it is already held,
// and ErrRemovalInProgress when a removal of the session's files has not
// finished. A file landed beside an unfinished removal could take a sequence
// the removal still counts on.
func LockSession(sessionDir string) (*SessionLock, error) {
	l, err := lockDir(sessionDir)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(filepath.Join(sessionDir, RemovingFile)); err == nil {
		_ = l.Unlock()
		return nil, ErrRemovalInProgress
	}
	return l, nil
}

// LockSessionToRemove takes a session's lock for the removal of its files,
// whether or not one is in progress.
func LockSessionToRemove(sessionDir string) (*SessionLock, error) { return lockDir(sessionDir) }

// ErrChainBusy means another builder holds this conversation's chain.
var ErrChainBusy = errors.New("storage: conversation chain is locked by another builder")

// LockChain takes an exclusive lock over one conversation's round chain.
//
// Publishing a round is a read-then-write: decide the next round number from
// what is on disk, then create that file. Two builders doing this at once can
// both read round N and both try to write round N+1, and because a round's
// number is part of its filename together with its digest, they would not even
// collide - they would produce two differently named files claiming the same
// position and fork the chain.
func LockChain(chainDir string) (*SessionLock, error) {
	l, err := lockDir(chainDir)
	if errors.Is(err, ErrSessionBusy) {
		return nil, ErrChainBusy
	}
	return l, err
}

// ErrExportBusy means another pusher holds this root's export state.
var ErrExportBusy = errors.New("storage: the export state is held by another pusher")

// LockExport takes an exclusive lock over one storage root's export state.
//
// A push is a read-then-write over push.state: read what has already gone,
// send what has not, then record it. Two pushers over one root would both
// read the same state and send the same records, and a token counted twice
// is worse than one counted late. The commands put two pipelines on one
// root on purpose - asz server locally beside an asz collect elsewhere - so
// this is an ordinary case, not a rare one.
//
// The directory name starts with an underscore, so a storage root's session
// listing passes over it as it passes over _conversations.
func LockExport(root string) (*SessionLock, error) {
	l, err := lockDir(filepath.Join(root, "_export"))
	if errors.Is(err, ErrSessionBusy) {
		return nil, ErrExportBusy
	}
	return l, err
}

// ErrProviderBusy means another collector holds this root's provider state.
var ErrProviderBusy = errors.New("storage: the provider body state is held by another collector")

// LockProvider takes an exclusive lock over one storage root's provider body
// state.
//
// The runtime writes every session's bodies into one flat directory, so which
// of them have landed is known for the root, not for a session. Two collectors
// deciding that at once would both land a new body into its session.
func LockProvider(root string) (*SessionLock, error) {
	l, err := lockDir(filepath.Join(root, "_provider"))
	if errors.Is(err, ErrSessionBusy) {
		return nil, ErrProviderBusy
	}
	return l, err
}

// LockExportWait is LockExport, waiting up to timeout for the lock rather
// than giving up at once.
//
// A command asked to send, such as asz push or a single collect pass, has
// to send: reporting success while another pass holds the state would hide
// the files that landed after that pass had already listed them. A watching
// pass has no such problem and skips instead, because it comes round again.
func LockExportWait(root string, timeout time.Duration) (*SessionLock, error) {
	deadline := time.Now().Add(timeout)
	for {
		l, err := LockExport(root)
		if !errors.Is(err, ErrExportBusy) || time.Now().After(deadline) {
			return l, err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ErrRootBusy means another command holds this storage root for a pass.
var ErrRootBusy = errors.New("storage: the storage root is held by another command")

// RootLockDir is where the root lock lives. The name starts with an
// underscore, so a storage root's session listing passes over it.
const RootLockDir = "_root"

// LockRoot takes the lock every command that writes to a storage root holds
// for one pass: asz collect and asz server for each pass, asz parse, asz push
// and asz repack for their run, asz scenario build for each session it
// builds, and asz scenario check for each root it builds. asz view and asz
// index read and take none. The LangSmith receiver adds requests to its inbox
// at any time, and a pass takes them under the inbox's own lock.
//
// Two commands over one root each load shared state, such as push.state, the
// metrics state and a session's index, and save it back, so the commands take
// turns. Retention, still to come, replaces landed files inside a pass and
// needs the same: no command may read a file while another replaces it. The
// lock is taken before any shared state is loaded. Locks are taken in one order: the
// scenario lock, this one, the export lock, the metrics lock, the provider
// lock, a session's and a chain's.
func LockRoot(root string) (*SessionLock, error) {
	l, err := lockDir(filepath.Join(root, RootLockDir))
	if errors.Is(err, ErrSessionBusy) {
		return nil, ErrRootBusy
	}
	return l, err
}

// LockRootWait is LockRoot, waiting up to timeout. A command asked to do its
// work waits; a watching pipeline tries on every pass.
func LockRootWait(root string, timeout time.Duration) (*SessionLock, error) {
	deadline := time.Now().Add(timeout)
	for {
		l, err := LockRoot(root)
		if !errors.Is(err, ErrRootBusy) || time.Now().After(deadline) {
			return l, err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ErrMetricsBusy means another derivation holds this root's metrics.
var ErrMetricsBusy = errors.New("storage: the metrics are being derived by another pass")

// LockMetricsWait takes the lock one metrics derivation holds over a root,
// waiting up to timeout for it. Two derivations at once, of different files
// of one series, would each decide a window from the state as it was before
// either saved, and publish windows that overlap. asz push derives under the
// export lock and then this one; a pipeline derives under this one alone, so
// the two are always taken in that order.
func LockMetricsWait(root string, timeout time.Duration) (*SessionLock, error) {
	deadline := time.Now().Add(timeout)
	for {
		l, err := lockDir(filepath.Join(root, "_metrics"))
		if errors.Is(err, ErrSessionBusy) {
			err = ErrMetricsBusy
		}
		if !errors.Is(err, ErrMetricsBusy) || time.Now().After(deadline) {
			return l, err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ScenarioDir is the directory a claude-code scenario build creates in its
// storage root. A pipeline over a root that has it takes the scenario lock
// there, because a pipeline over such a root removes sessions.
//
// The name starts with an underscore, so a storage root's session listing
// passes over it as it passes over _conversations.
const ScenarioDir = "_scenario"

// ErrScenarioBusy means another pipeline holds this scenario root.
var ErrScenarioBusy = errors.New("storage: the scenario root is held by another pipeline")

// LockScenario takes the lock one pipeline holds over a scenario root for as
// long as it runs.
//
// A pipeline over a scenario root removes a session once all of it is sent.
// A second pipeline over the same root could land a removed session again
// through a source it listed a moment earlier, or parse evidence that is
// gone. So only one pipeline works on such a root at a time. A rolling
// update that starts a new process before the old one stops makes two of
// them an ordinary case.
func LockScenario(root string) (*SessionLock, error) {
	l, err := lockDir(filepath.Join(root, ScenarioDir))
	if errors.Is(err, ErrSessionBusy) {
		return nil, ErrScenarioBusy
	}
	return l, err
}

// LockScenarioWait is LockScenario, waiting up to timeout for the lock
// rather than giving up at once. A single pass waits, because it has to do
// its work or say it could not. A watching pipeline tries on every pass.
func LockScenarioWait(root string, timeout time.Duration) (*SessionLock, error) {
	deadline := time.Now().Add(timeout)
	for {
		l, err := LockScenario(root)
		if !errors.Is(err, ErrScenarioBusy) || time.Now().After(deadline) {
			return l, err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Unlock releases the lock.
func (l *SessionLock) Unlock() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := unlock(l.f)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}
