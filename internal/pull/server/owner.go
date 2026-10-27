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
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// slotsPerKind is how many freezes, and how many archive builds, one owner
// runs at once.
const slotsPerKind = 2

// ownerRetry is how often a process that does not own the root's pulls
// tries again.
const ownerRetry = time.Second

// Options are what a pull server runs with.
type Options struct {
	// Root is the production storage root.
	Root string
	// TTL is remote_server.snapshot_ttl: how long a snapshot stays after
	// its last use.
	TTL time.Duration
	// Every is how often renewal and the sweep run.
	Every time.Duration
	// Guard is the address's guard, whose slots requests take.
	Guard *Guard
	Files storage.FileSystem
	Now   func() time.Time
	Log   func(format string, args ...any)
}

// Server serves pulls from one production root. One process at a time owns
// that root's pulls through _pull/owner/.lock, for as long as it serves;
// until it does, its pull routes answer 503 and the page goes on.
type Server struct {
	o Options
	p paths

	owned atomic.Bool
	owner *storage.SessionLock

	// ctx ends the server's work: handlers and workers watch it.
	ctx    context.Context
	cancel context.CancelFunc
	// stopping refuses new pull requests; work joins every request and
	// worker before the owner is let go.
	mu       sync.Mutex
	stopping bool
	work     sync.WaitGroup

	freezeSlots  chan struct{}
	archiveSlots chan struct{}
	// archiveCounts keeps each archive's file count for the audit.
	archiveCounts counts
}

// New makes a pull server. Nothing is read or written until Run.
func New(o Options) *Server {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
	if o.Every <= 0 {
		o.Every = time.Minute
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{o: o, p: paths{root: o.Root}, ctx: ctx, cancel: cancel,
		freezeSlots: make(chan struct{}, slotsPerKind), archiveSlots: make(chan struct{}, slotsPerKind)}
}

func (s *Server) now() time.Time { return s.o.Now() }

// files is the file system the server writes through: the options', or the
// disk.
func (s *Server) files() storage.FileSystem {
	if s.o.Files != nil {
		return s.o.Files
	}
	return storage.DiskFileSystem{}
}

func (s *Server) logf(format string, args ...any) { s.o.Log(format, args...) }

// Run owns the root's pulls until ctx ends. It waits for the lifetime lock,
// trying once a second, then initializes the server's state, recovers what
// an earlier owner left, and runs the scheduler and the sweep. When ctx
// ends it stops taking requests, cancels and joins every handler and worker,
// and only then lets the owner go.
func (s *Server) Run(ctx context.Context) error {
	defer s.cancel()
	waiting := false
	for {
		l, err := storage.LockServerOwner(s.o.Root, s.o.Files)
		if err == nil {
			s.owner = l
			break
		}
		if !errors.Is(err, storage.ErrBusy) {
			return err
		}
		if !waiting {
			s.logf("pull: another process serves this root's pulls; waiting for it to stop")
			waiting = true
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(ownerRetry):
		}
	}
	defer func() { _ = s.owner.Unlock() }()
	if err := s.init(); err != nil {
		return err
	}
	// Requests are served at once. A publication an earlier owner left is
	// recovered by the first sweep; until then its tag alone is refused,
	// and a publication that finds the intent taken recovers it first.
	s.owned.Store(true)
	s.logf("pull: serving this root's pulls")
	ticker := time.NewTicker(s.o.Every)
	defer ticker.Stop()
	for {
		s.schedule()
		s.sweep()
		select {
		case <-ctx.Done():
			s.stop()
			return nil
		case <-ticker.C:
		}
	}
}

// stop refuses new requests, cancels running ones and every worker, and
// waits for all of them to end.
func (s *Server) stop() {
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	s.owned.Store(false)
	s.cancel()
	s.work.Wait()
}

// enter counts one request or worker in, unless the server is stopping.
func (s *Server) enter() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return false
	}
	s.work.Add(1)
	return true
}

func (s *Server) leave() { s.work.Done() }

// init sets up _pull: _state and its control directories, made durable. A
// missing _state is taken only in a _pull holding nothing but its locks and
// a writer's leftovers.
func (s *Server) init() error {
	files := s.o.Files
	if err := storage.MakeAncestorsDurable(s.o.Root, s.p.pull(), 0o755, files); err != nil {
		return err
	}
	if err := storage.MakeLockFile(s.p.pull(), files); err != nil {
		return err
	}
	return s.global(func() error {
		_, err := readState(s.p)
		switch {
		case err == nil:
		case errors.Is(err, os.ErrNotExist):
			entries, rerr := os.ReadDir(s.p.pull())
			if rerr != nil {
				return rerr
			}
			for _, e := range entries {
				name := e.Name()
				if name == ".lock" || name == storage.ServerOwnerDir || strings.HasPrefix(name, ".tmp-") && e.Type().IsRegular() {
					continue
				}
				return stateError("%s holds %s and no state: it cannot be told apart from a damaged one", s.p.pull(), name)
			}
			if err := replaceRecord(s.p.state(), &state{Schema: stateSchema}, files); err != nil {
				return err
			}
		default:
			return err
		}
		for _, dir := range []string{s.p.freezes(), s.p.building(), s.p.conversations()} {
			if err := makeDir(dir, files); err != nil {
				return err
			}
		}
		return nil
	})
}

// makeDir makes a control directory, or takes the real one there, and syncs
// it and its parent: the directory alone, never what is below it, which can
// be a conversation's snapshots.
func makeDir(dir string, files storage.FileSystem) error {
	if files == nil {
		files = storage.DiskFileSystem{}
	}
	info, err := os.Lstat(dir)
	switch {
	case err == nil && info.IsDir():
		if err := files.SyncDir(dir); err != nil {
			return err
		}
		return files.SyncDir(filepath.Dir(dir))
	case err == nil:
		return stateError("%s is not a real directory", dir)
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	return storage.MakeOwnedDir(dir, files)
}

// global runs fn holding _pull/.lock, the lock of every state transition.
// A replacement Windows finds busy runs the whole transition again: fn
// lets go of what it took, the lock is let go and taken again, and fn reads
// the state again and decides again, for at most two seconds.
func (s *Server) global(fn func() error) error {
	return storage.RetryControl(s.ctx, func(ctx context.Context) error {
		l, err := storage.LockOwnedWait(ctx, s.p.pull())
		if err != nil {
			return err
		}
		defer func() { _ = l.Unlock() }()
		return fn()
	})
}

// probeConversation takes a conversation's _root without waiting, as a
// transition under the global lock does. Busy is storage.ErrBusy.
func (s *Server) probeConversation(conversation string) (*storage.SessionLock, error) {
	return storage.LockOwned(s.p.convRoot(conversation))
}

// conversationSetup makes a conversation's directory the first time: its
// _root lock, an empty history and _removing/, under the global lock.
func (s *Server) conversationSetup(conversation string) error {
	dir := s.p.conversation(conversation)
	if err := makeDir(dir, s.o.Files); err != nil {
		return err
	}
	if err := makeDir(s.p.convRoot(conversation), s.o.Files); err != nil {
		return err
	}
	if err := storage.MakeLockFile(s.p.convRoot(conversation), s.o.Files); err != nil {
		return err
	}
	l, err := s.probeConversation(conversation)
	if err != nil {
		return err
	}
	defer func() { _ = l.Unlock() }()
	if _, err := readHistory(s.p, conversation); errors.Is(err, os.ErrNotExist) {
		// History never comes from tags: one missing beside evidence is
		// damage, not a first setup.
		entries, rerr := os.ReadDir(dir)
		if rerr != nil {
			return rerr
		}
		for _, e := range entries {
			if n := e.Name(); n != storage.RootLockDir && n != storage.RemovingDir && !strings.HasPrefix(n, ".tmp-") {
				return stateError("%s holds %s and no history", dir, n)
			}
		}
		if err := replaceRecord(s.p.history(conversation), &history{Schema: stateSchema, Conversation: conversation, Removed: []string{}}, s.o.Files); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	return makeDir(s.p.removing(conversation), s.o.Files)
}

// takeSlot takes one of a kind's in-process slots without waiting.
func takeSlot(slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func giveSlot(slots chan struct{}) { <-slots }

// errNoCapacity is work that found every slot of its kind busy.
var errNoCapacity = errors.New("server: every slot is busy")

// sweepLog is the sweep's way to report a failure once per pass.
func (s *Server) sweepLog(what string, err error) {
	if err != nil && !errors.Is(err, context.Canceled) {
		s.logf("pull: %s: %v", what, err)
	}
}

// conversationsOn lists the conversations the server has state for.
func (s *Server) conversationsOn() ([]string, error) {
	entries, err := os.ReadDir(s.p.conversations())
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && storage.CheckNamePart(e.Name()) == nil {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// tagsOf lists a conversation's published snapshot tags, oldest first.
func (s *Server) tagsOf(conversation string) ([]string, error) {
	entries, err := os.ReadDir(s.p.conversation(conversation))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if _, err := storage.ParseTag(e.Name()); err == nil && e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// String names the server in logs.
func (s *Server) String() string { return fmt.Sprintf("pull server over %s", filepath.Clean(s.o.Root)) }
