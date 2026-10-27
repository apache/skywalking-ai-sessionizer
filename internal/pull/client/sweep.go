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

package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// Swept is what a sweep did and could not do.
type Swept struct {
	// Removed lists the snapshots removed, as remote/conversation:tag.
	Removed []string
	// Busy lists the conversations another command held, passed over.
	Busy []string
	// Failed holds each conversation the sweep could not settle, with why.
	Failed map[string]error
}

// Sweep settles every pulled conversation of the root, those of remotes no
// longer configured included, at the start of a command that reads or pulls
// copies. Each conversation is taken without waiting, and one another command
// holds is passed over. Under it the sweep finishes an interrupted bootstrap,
// recovers an unfinished publication, deletes private leftovers, removes dead
// leases and removes every expired snapshot no reader holds. It reads markers
// and history only, never a manifest or evidence, and it never makes a
// conversation that is not there. A root with no _remote/ is left as it is.
func (l *Local) Sweep(ctx context.Context) (*Swept, error) {
	swept := &Swept{Failed: map[string]error{}}
	remotes := filepath.Join(l.Root, storage.RemoteDir)
	info, err := os.Lstat(remotes)
	if errors.Is(err, os.ErrNotExist) {
		return swept, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a real directory", storage.ErrProtected, remotes)
	}
	remoteDirs, err := os.ReadDir(remotes)
	if err != nil {
		return nil, err
	}
	for _, r := range remoteDirs {
		conversations, err := os.ReadDir(filepath.Join(remotes, r.Name()))
		if err != nil {
			swept.Failed[r.Name()] = err
			continue
		}
		for _, cv := range conversations {
			name := r.Name() + "/" + cv.Name()
			removed, err := l.sweepOne(ctx, r.Name(), cv.Name())
			switch {
			case errors.Is(err, storage.ErrBusy):
				swept.Busy = append(swept.Busy, name)
			case err != nil:
				swept.Failed[name] = err
			}
			for _, tag := range removed {
				swept.Removed = append(swept.Removed, name+":"+tag)
			}
		}
	}
	return swept, nil
}

func (l *Local) sweepOne(ctx context.Context, remote, conversation string) ([]string, error) {
	dir, err := storage.ConversationDir(l.Root, remote, conversation)
	if err != nil {
		return nil, err
	}
	if err := storage.RealDirectory(dir); err != nil {
		return nil, err
	}
	// A conversation whose _pull lock was never made is a bootstrap that
	// stopped before it: only a pull, which may wait, finishes that one.
	pull, err := storage.LockOwned(filepath.Join(dir, storage.PullDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = pull.Unlock() }()
	if err := storage.FinishBootstrap(ctx, dir, remote, conversation, l.Files); err != nil {
		return nil, err
	}
	c := &conversationDir{l: l, dir: dir, remote: remote, conversation: conversation}
	if err := c.recover(ctx); err != nil {
		return nil, err
	}
	if err := c.cleanPrivate(ctx); err != nil {
		return nil, err
	}
	return c.removeDue(ctx)
}

// errDamagedMarker is a snapshot whose own marker does not read: a sweep
// says so and leaves it, and goes on with the others.
var errDamagedMarker = errors.New("pull: its marker does not read")

// removeDue removes each snapshot that has expired or whose tag the history
// lists as removed, when no reader holds it, and sweeps the dead leases of
// those that stay. A tree whose marker is not this conversation's own is
// left for a person. A tree being removed has its removal written to the
// history freshly, then is detached into _removing and deleted from there.
func (c *conversationDir) removeDue(ctx context.Context) ([]string, error) {
	tags, err := c.tags()
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, tag := range tags {
		gone, err := c.removeIfDue(ctx, tag)
		if errors.Is(err, errDamagedMarker) || errors.Is(err, ErrUnownedCollision) {
			// One snapshot's damage stops nothing else: it is said each
			// pass and left as it is, and the others go on.
			c.l.logf("%s/%s: %s: %v; left as it is", c.remote, c.conversation, tag, err)
			continue
		}
		if err != nil {
			return removed, fmt.Errorf("%s: %w", tag, err)
		}
		if gone != "" {
			removed = append(removed, tag)
			if err := storage.DeleteOwned(filepath.Dir(gone), filepath.Base(gone), c.files()); err != nil {
				return removed, err
			}
		}
	}
	return removed, nil
}

// removeIfDue decides, under _admit and _root, whether tag goes, and detaches
// it when it does. It returns where the tree went, or empty.
func (c *conversationDir) removeIfDue(ctx context.Context, tag string) (string, error) {
	files := c.files()
	tree := filepath.Join(c.dir, tag)
	var detached string
	err := c.locked(ctx, true, func(outer storage.OuterCopy) error {
		if p := outer.Publishing; p != nil && p.Tag == tag {
			return nil
		}
		listed, err := storage.IsRemoved(c.dir, c.remote, c.conversation, tag)
		if err != nil {
			return err
		}
		if !listed {
			marker, err := storage.ReadSnapshotCopy(tree, c.remote, c.conversation, tag)
			if err != nil {
				// Damaged controls are reported where they are read; a
				// sweep removes nothing it cannot identify.
				return fmt.Errorf("%w: %w", errDamagedMarker, err)
			}
			if c.l.now().Before(marker.PublishedAt.Add(c.l.TTL)) {
				return sweepLeases(tree, files)
			}
		} else if err := storage.RealDirectory(tree); err != nil {
			// A tag the history lists as removed is this conversation's
			// to finish removing, by its history alone: its own marker
			// may already be gone.
			return fmt.Errorf("%w: %s: %w", ErrUnownedCollision, tree, err)
		}
		state, err := storage.ProbeLeases(filepath.Join(tree, storage.LeasesDir))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if state.Held > 0 {
			return nil
		}
		if err := storage.WriteOuterCopy(c.dir, outer, tag, files); err != nil {
			return err
		}
		removing := filepath.Join(c.dir, storage.RemovingDir)
		if err := storage.MakeOwnedDir(removing, files); err != nil {
			return err
		}
		detached, err = storage.Detach(tree, removing, tag, files)
		return err
	})
	return detached, err
}

// sweepLeases removes the lease files no live reader holds. The caller holds
// the gate.
func sweepLeases(tree string, files storage.FileSystem) error {
	err := storage.SweepDeadLeases(filepath.Join(tree, storage.LeasesDir), files)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// RecoverIfFree settles an unfinished publication of remote/conversation when
// no other command holds the conversation, without waiting for it. A reader
// whose snapshot's tag is still being published calls it, then admits
// again: a held conversation is storage.ErrBusy, and its tag admits no
// reader until its owner is done.
func (l *Local) RecoverIfFree(ctx context.Context, remote, conversation string) error {
	dir, err := storage.ConversationDir(l.Root, remote, conversation)
	if err != nil {
		return err
	}
	pull, err := storage.LockOwned(filepath.Join(dir, storage.PullDir))
	if err != nil {
		return err
	}
	defer func() { _ = pull.Unlock() }()
	c := &conversationDir{l: l, dir: dir, remote: remote, conversation: conversation}
	if err := c.recover(ctx); err != nil {
		return err
	}
	// An earlier recovery can have stopped after it detached the tree;
	// what it left in _removing goes too, behind a fresh history write.
	return c.cleanPrivate(ctx)
}
