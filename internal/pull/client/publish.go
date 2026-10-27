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
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/pull/format"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// ErrUnownedCollision is a tree at a reserved tag's name that is not this
// conversation's publication of it. It is left for a person to repair, and
// the tag stays reserved.
var ErrUnownedCollision = errors.New("pull: another tree sits at a reserved tag's name")

// locked runs fn holding the conversation's _admit gate, when gate is set,
// then its outer _root, in that order, and releases them in reverse. The
// caller holds _pull, which it keeps throughout.
//
// The whole of it is one transition: a control replacement Windows finds
// busy releases both locks, waits, takes them again, reads the history again
// and runs fn again from the start, within RetryControl's bound. fn decides
// from what it reads each time, so nothing a busy attempt prepared crosses
// into the next.
func (c *conversationDir) locked(ctx context.Context, gate bool, fn func(outer storage.OuterCopy) error) error {
	return storage.RetryControl(ctx, func(ctx context.Context) (err error) {
		if gate {
			admit, err := storage.LockOwnedWait(ctx, filepath.Join(c.dir, storage.AdmitDir))
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, admit.Unlock()) }()
		}
		root, err := storage.LockOwnedWait(ctx, filepath.Join(c.dir, storage.RootLockDir))
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, root.Unlock()) }()
		outer, err := storage.ReadOuterCopy(c.dir, c.remote, c.conversation)
		if err != nil {
			return err
		}
		return fn(outer)
	})
}

// publish gives the accepted build its tag. The tag and the intent are
// reserved durably first, under the outer _root alone. Then, under _admit and
// _root, the history, the build and the intent are checked again, the
// snapshot's marker is written with the tag's time, the build is renamed to
// its tag without replacing anything, both parents are synced, and the intent
// is cleared with a fresh write. A failure after the reservation leaves the
// intent for recovery, which discards the build and keeps the tag reserved.
func (c *conversationDir) publish(ctx context.Context, build, tree string, accepted *format.Accepted) (string, error) {
	files := c.files()
	identity, err := os.Lstat(tree)
	if err != nil {
		return "", err
	}
	var tag string
	err = c.locked(ctx, false, func(outer storage.OuterCopy) error {
		var err error
		if tag, err = outer.Reserve(c.l.now(), build, string(accepted.Digest)); err != nil {
			return err
		}
		return storage.WriteOuterCopy(c.dir, outer, "", files)
	})
	if err != nil {
		return "", err
	}
	m := accepted.Manifest
	at, err := storage.ParseTag(tag)
	if err != nil {
		return "", err
	}
	marker := storage.SnapshotCopy{Remote: c.remote, Conversation: c.conversation, Session: m.Session,
		Round: m.Head.Round, Digest: string(m.Head.Digest), ManifestDigest: string(accepted.Digest), PublishedAt: at}
	dest := filepath.Join(c.dir, tag)
	err = c.locked(ctx, true, func(outer storage.OuterCopy) error {
		p := outer.Publishing
		if p == nil || p.Build != build || p.Tag != tag || format.ManifestDigest(p.ManifestDigest) != accepted.Digest {
			return fmt.Errorf("pull: the publication of %s changed while it was prepared", tag)
		}
		// An attempt before this one may have published the tree and found
		// the intent's clearing busy: then the tree at the tag is the build
		// itself, and only the clearing is left.
		if done, err := os.Lstat(dest); err == nil && os.SameFile(done, identity) {
			outer.Publishing = nil
			return storage.WriteOuterCopy(c.dir, outer, "", files)
		}
		now, err := os.Lstat(tree)
		if err != nil {
			return err
		}
		if !os.SameFile(now, identity) || !now.IsDir() {
			return fmt.Errorf("pull: the build of %s changed while it was prepared", tag)
		}
		if _, err := os.Lstat(filepath.Join(tree, storage.CopyMarker)); errors.Is(err, os.ErrNotExist) {
			if err := storage.WriteSnapshotCopy(tree, tag, marker, files); err != nil {
				return err
			}
		} else if had, err := storage.ReadSnapshotCopy(tree, c.remote, c.conversation, tag); err != nil || !sameMarker(had, marker) {
			return fmt.Errorf("pull: the build of %s holds another marker", tag)
		}
		if err := storage.PublishDir(tree, dest, files); err != nil {
			return err
		}
		outer.Publishing = nil
		return storage.WriteOuterCopy(c.dir, outer, "", files)
	})
	if err != nil {
		return "", err
	}
	return tag, nil
}

// recover settles a publication that did not finish. The tag it reserved is
// recorded removed, even when nothing was published under it, after its
// name is checked to hold nothing or this conversation's own tree, which is
// detached. The detached tree and the private build are deleted, and the
// intent is cleared with a fresh write. A finished publication is never
// completed by recovery; its tag stays reserved for good.
func (c *conversationDir) recover(ctx context.Context) error {
	files := c.files()
	var intent *storage.Publishing
	var detached string
	err := c.locked(ctx, true, func(outer storage.OuterCopy) error {
		intent = outer.Publishing
		if intent == nil {
			return nil
		}
		owner, err := c.tagOwner(intent)
		if err != nil {
			return err
		}
		if owner == storage.TagUnowned {
			return fmt.Errorf("%w: %s", ErrUnownedCollision, filepath.Join(c.dir, intent.Tag))
		}
		if err := storage.WriteOuterCopy(c.dir, outer, intent.Tag, files); err != nil {
			return err
		}
		if owner == storage.TagOwned {
			removing := filepath.Join(c.dir, storage.RemovingDir)
			if err := storage.MakeOwnedDir(removing, files); err != nil {
				return err
			}
			if detached, err = storage.Detach(filepath.Join(c.dir, intent.Tag), removing, intent.Tag, files); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil || intent == nil {
		return err
	}
	if detached != "" {
		if err := storage.DeleteOwned(filepath.Dir(detached), filepath.Base(detached), files); err != nil {
			return err
		}
	}
	// The abandoned build goes before the intent that names it: whoever
	// recovers, a pull, a sweep or a reader, leaves nothing of it.
	if err := c.deleteBuild(intent.Build); err != nil {
		return err
	}
	return c.locked(ctx, true, func(outer storage.OuterCopy) error {
		if p := outer.Publishing; p == nil || *p != *intent {
			return fmt.Errorf("pull: the publication of %s changed while it was recovered", intent.Tag)
		}
		outer.Publishing = nil
		return storage.WriteOuterCopy(c.dir, outer, "", files)
	})
}

// sameMarker says whether two snapshot markers say the same thing.
func sameMarker(a, b storage.SnapshotCopy) bool {
	at, bt := a.PublishedAt, b.PublishedAt
	a.PublishedAt, b.PublishedAt = time.Time{}, time.Time{}
	return a == b && at.Equal(bt)
}

// tagOwner says whose the tree at an intent's tag is. A tag the history
// already lists as removed is this conversation's by that alone, its marker
// gone or not, since an earlier recovery recorded it before it detached;
// otherwise the tree's own marker must name this publication.
func (c *conversationDir) tagOwner(intent *storage.Publishing) (storage.TagOwnership, error) {
	listed, err := storage.IsRemoved(c.dir, c.remote, c.conversation, intent.Tag)
	if err != nil {
		return storage.TagUnowned, err
	}
	if !listed {
		return storage.CheckCollision(c.dir, c.remote, c.conversation, intent.Tag, intent.ManifestDigest)
	}
	tree := filepath.Join(c.dir, intent.Tag)
	switch _, err := os.Lstat(tree); {
	case errors.Is(err, os.ErrNotExist):
		return storage.TagAbsent, nil
	case err != nil:
		return storage.TagUnowned, err
	}
	if err := storage.RealDirectory(tree); err != nil {
		return storage.TagUnowned, nil
	}
	return storage.TagOwned, nil
}

// deleteBuild deletes one private build, if it is there.
func (c *conversationDir) deleteBuild(name string) error {
	building := filepath.Join(c.dir, storage.BuildingDir)
	if _, err := os.Lstat(building); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return storage.DeleteOwned(building, name, c.files())
}

// cleanPrivate deletes what earlier commands left in the conversation's
// private directories, reading nothing inside them: every build, and every
// tree detached for removal, the latter only after the history is written
// again freshly, so its removal record is durable before anything goes. An
// unfinished publication is settled first.
func (c *conversationDir) cleanPrivate(ctx context.Context) error {
	files := c.files()
	building := filepath.Join(c.dir, storage.BuildingDir)
	if entries, err := os.ReadDir(building); err == nil {
		for _, e := range entries {
			if err := storage.DeleteOwned(building, e.Name(), files); err != nil {
				return err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	removing := filepath.Join(c.dir, storage.RemovingDir)
	entries, err := os.ReadDir(removing)
	if errors.Is(err, os.ErrNotExist) || err == nil && len(entries) == 0 {
		return nil
	}
	if err != nil {
		return err
	}
	if err := c.locked(ctx, true, func(outer storage.OuterCopy) error {
		return storage.WriteOuterCopy(c.dir, outer, "", files)
	}); err != nil {
		return err
	}
	for _, e := range entries {
		if err := storage.DeleteOwned(removing, e.Name(), files); err != nil {
			return err
		}
	}
	return nil
}
