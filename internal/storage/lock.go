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
