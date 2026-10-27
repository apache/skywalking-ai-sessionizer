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
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ConversationDir is where a storage root keeps the copies of one remote
// conversation. The names are checked before the path is made.
func ConversationDir(root, remote, conversation string) (string, error) {
	if err := CheckCopyNames(remote, conversation); err != nil {
		return "", err
	}
	return filepath.Join(root, RemoteDir, remote, conversation), nil
}

// BootstrapConversation takes a conversation's directory for a pull or a sweep
// and returns holding its _pull lock, which the caller keeps for its pass.
//
// It makes _remote, the remote's directory, the conversation's directory and
// _pull when they are missing, syncing every ancestor from the storage root
// down on every attempt: an earlier process may have stopped before its sync.
// Under the conversation's _root, it removes the temporary files a first
// marker write can leave, then admits the directory only when its marker
// names the same remote and conversation, or when it holds nothing but the
// lock directories, in which case it writes an empty history first. It then
// makes the _admit gate readers need. Nothing else in the directory changes,
// and a refused directory keeps everything but its lock directories and
// removed temporaries as it was.
func BootstrapConversation(ctx context.Context, root, remote, conversation string, files FileSystem) (_ string, _ *SessionLock, err error) {
	files = writeFileSystem(files)
	dir, err := ConversationDir(root, remote, conversation)
	if err != nil {
		return "", nil, err
	}
	pullDir := filepath.Join(dir, PullDir)
	if err := MakeAncestorsDurable(root, pullDir, 0o755, files); err != nil {
		return "", nil, err
	}
	// The lock stays in its own variable: a return that refuses sets the
	// results to nil before the release below runs.
	pull, err := lockOwnedCreateWait(ctx, pullDir, files)
	if err != nil {
		return "", nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, pull.Unlock())
		}
	}()
	if err := FinishBootstrap(ctx, dir, remote, conversation, files); err != nil {
		return "", nil, err
	}
	return dir, pull, nil
}

// FinishBootstrap is bootstrap's work once the caller holds the
// conversation's _pull: under the outer _root it removes reserved
// temporaries, checks the conversation's marker or, in a directory holding
// only lock directories, writes its empty history, and makes the _admit gate
// readers need. A sweep that took an existing _pull without waiting calls it
// directly.
//
// A control replacement Windows finds busy repeats the whole transition,
// its lock and its checks included, within RetryControl's bound.
func FinishBootstrap(ctx context.Context, dir, remote, conversation string, files FileSystem) error {
	files = writeFileSystem(files)
	rootDir := filepath.Join(dir, RootLockDir)
	if err := makeOwnedDir(rootDir, files); err != nil {
		return err
	}
	return RetryControl(ctx, func(context.Context) (err error) {
		outer, err := LockOwnedCreate(rootDir, files)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, outer.Unlock()) }()
		if err := removeReservedTemporaries(dir, files); err != nil {
			return err
		}
		if err := admitConversation(dir, remote, conversation, files); err != nil {
			return err
		}
		admit := filepath.Join(dir, AdmitDir)
		if err := makeOwnedDir(admit, files); err != nil {
			return err
		}
		return makeLockFile(admit, files)
	})
}

// waitLocal is how long a pull waits for another command's hold on the same
// conversation before it gives up on that conversation.
const waitLocal = 30 * time.Second

func lockOwnedCreateWait(ctx context.Context, dir string, files FileSystem) (*SessionLock, error) {
	ctx, cancel := context.WithTimeout(ctx, waitLocal)
	defer cancel()
	for {
		l, err := LockOwnedCreate(dir, files)
		if !errors.Is(err, ErrBusy) {
			return l, err
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("%w: %s: %w", ErrBusy, dir, ctx.Err())
		case <-timer.C:
		}
	}
}

// makeOwnedDir makes one directory of a conversation's layout, or checks the
// one there is real, and syncs it and its parent either way.
func makeOwnedDir(dir string, files FileSystem) error {
	if err := files.Mkdir(dir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	if err := RealDirectory(dir); err != nil {
		return err
	}
	if err := files.SyncDir(filepath.Dir(dir)); err != nil {
		return err
	}
	return files.SyncDir(dir)
}

// makeLockFile makes a gate's lock file without taking it, so readers can
// later open it without creating anything.
func makeLockFile(dir string, files FileSystem) error {
	path := filepath.Join(dir, lockFile)
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: %s is not a regular file", ErrProtected, path)
		}
		return files.SyncDir(dir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	d, err := OpenDir(dir)
	if err != nil {
		return err
	}
	file, err := files.CreateIn(d, lockFile, PermState)
	if closeErr := d.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		return err
	}
	return files.SyncDir(dir)
}

// removeReservedTemporaries removes the regular files whose names start with
// the reserved prefix, which an interrupted atomic write leaves, and syncs the
// directory. A link or a directory with that prefix is refused: nothing
// pull writes makes one.
func removeReservedTemporaries(dir string, files FileSystem) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	removed := false
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), reservedPrefix) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if entry.Type() != 0 {
			return fmt.Errorf("%w: %s is not a regular file", ErrProtected, path)
		}
		if err := files.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		removed = true
	}
	if !removed {
		return nil
	}
	return files.SyncDir(dir)
}

// lockDirs are the only entries a conversation directory holds before its
// first marker: taking a lock makes its directory (internal/storage/lock.go),
// so they are not content, as repack.CheckDestination rules too.
var lockDirs = map[string]bool{PullDir: true, RootLockDir: true, AdmitDir: true}

func admitConversation(dir, remote, conversation string, files FileSystem) error {
	if _, err := os.Lstat(filepath.Join(dir, CopyMarker)); err == nil {
		_, err := ReadOuterCopy(dir, remote, conversation)
		return err
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !lockDirs[entry.Name()] {
			return fmt.Errorf("%w: %s holds %s but no copy marker, so it is not a conversation's copies",
				ErrMarker, dir, entry.Name())
		}
	}
	return WriteOuterCopy(dir, OuterCopy{Remote: remote, Conversation: conversation}, "", files)
}

// TagOwnership is what a collision check found at a tag's pathname.
type TagOwnership int

const (
	// TagAbsent: nothing is at the pathname. Recording the tag removed needs no marker.
	TagAbsent TagOwnership = iota
	// TagOwned: the tree is this conversation's publication of this tag.
	TagOwned
	// TagUnowned: something else is there. It is left for a person to repair:
	// its tag is not recorded, nothing is deleted and no intent is cleared.
	TagUnowned
)

// CheckCollision reports who owns what sits at a reserved tag's pathname in
// the conversation directory dir, without following a link. A tree is this
// conversation's own when its marker names this remote, conversation and tag
// time, and, when manifestDigest is not empty, the digest of the intent that
// reserved the tag. Every recovery path checks this before it records a tag
// as removed, so an unrelated tree never becomes removable.
func CheckCollision(dir, remote, conversation, tag, manifestDigest string) (TagOwnership, error) {
	path := filepath.Join(dir, tag)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return TagAbsent, nil
	}
	if err != nil {
		return TagUnowned, err
	}
	if info.Mode().Type() != fs.ModeDir {
		return TagUnowned, nil
	}
	marker, err := ReadSnapshotCopy(path, remote, conversation, tag)
	if err != nil || (manifestDigest != "" && marker.ManifestDigest != manifestDigest) {
		return TagUnowned, nil
	}
	return TagOwned, nil
}

// MakeOwnedDir makes one directory of pull's layout, or checks the one there
// is a real directory, and syncs it and its parent either way, as bootstrap
// does for a conversation's own.
func MakeOwnedDir(dir string, files FileSystem) error {
	return makeOwnedDir(dir, writeFileSystem(files))
}

// MakeLockFile makes a gate's lock file in dir without taking it, so later
// readers open it without creating anything, and syncs dir.
func MakeLockFile(dir string, files FileSystem) error {
	return makeLockFile(dir, writeFileSystem(files))
}

// WriteNewDurable makes the file path, which must not exist, writes it,
// syncs it and closes it, then syncs its directory. A build saves its
// manifest this way before any evidence.
func WriteNewDurable(path string, mode os.FileMode, write func(io.Writer) error, files FileSystem) (err error) {
	files = writeFileSystem(files)
	file, err := files.Create(path, mode)
	if err != nil {
		return err
	}
	if err := write(file); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Sync(); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Close(); err != nil {
		return err
	}
	return files.SyncDir(filepath.Dir(path))
}
