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

// LockSession takes the lock, returning ErrSessionBusy if it is already held.
func LockSession(sessionDir string) (*SessionLock, error) { return lockDir(sessionDir) }

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
