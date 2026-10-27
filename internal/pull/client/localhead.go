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
	"slices"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// ErrNoSnapshot is a remote conversation with no snapshot here a reader can
// be admitted to: none pulled, or every one expired.
var ErrNoSnapshot = errors.New("pull: no snapshot of the conversation is here")

// Copy is one remote conversation pulled into the root.
type Copy struct{ Remote, Conversation string }

// Copies lists the remote conversations the root holds a directory for, by
// remote then conversation. A name no pull could have made is passed over.
func (l *Local) Copies() ([]Copy, error) {
	remotes, err := os.ReadDir(filepath.Join(l.Root, storage.RemoteDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Copy
	for _, r := range remotes {
		if !r.IsDir() || storage.CheckRemoteName(r.Name()) != nil {
			continue
		}
		conversations, err := os.ReadDir(filepath.Join(l.Root, storage.RemoteDir, r.Name()))
		if err != nil {
			return nil, err
		}
		for _, c := range conversations {
			if c.IsDir() && storage.CheckCopyNames(r.Name(), c.Name()) == nil {
				out = append(out, Copy{Remote: r.Name(), Conversation: c.Name()})
			}
		}
	}
	return out, nil
}

// Tags lists the tags of remote/conversation's snapshots here, oldest first:
// every entry named as a tag is, whatever it is, so a damaged snapshot is
// met where its tag places it, never passed over unseen. A conversation
// with no directory here has none. Its directories on the way from the root
// must be real ones.
func (l *Local) Tags(remote, conversation string) ([]string, error) {
	dir, err := storage.ConversationDir(l.Root, remote, conversation)
	if err != nil {
		return nil, err
	}
	if err := layoutReal(l.Root, dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var tags []string
	for _, e := range entries {
		if t, err := storage.ParseTag(e.Name()); err == nil && storage.Stamp(t) == e.Name() {
			tags = append(tags, e.Name())
		}
	}
	slices.Sort(tags)
	return tags, nil
}

// AdmitNewest admits a reader to the newest unexpired snapshot of
// remote/conversation: the greatest tag whose time to live, counted from
// the tag's own time, has not passed, a tag whose removal is recorded or
// whose publication is unfinished passed over. Damage to that snapshot, or
// to the conversation's own directories and controls, is an error, never a
// reason to read an older snapshot. It returns ErrNoSnapshot when no
// snapshot is unexpired. The caller releases the admission.
func (l *Local) AdmitNewest(ctx context.Context, remote, conversation string) (*storage.Admission, error) {
	tags, err := l.Tags(remote, conversation)
	if err != nil {
		return nil, err
	}
	dir, err := storage.ConversationDir(l.Root, remote, conversation)
	if err != nil {
		return nil, err
	}
	for i := len(tags) - 1; i >= 0; i-- {
		if t, _ := storage.ParseTag(tags[i]); !l.now().Before(t.Add(l.TTL)) {
			return nil, ErrNoSnapshot
		}
		a, err := l.admit(ctx, dir, tags[i])
		switch {
		case errors.Is(err, storage.ErrRemovedTag), errors.Is(err, storage.ErrPublishing):
			continue
		case errors.Is(err, storage.ErrExpired):
			return nil, ErrNoSnapshot
		case err != nil:
			return nil, fmt.Errorf("%s: %w", tags[i], err)
		}
		return a, nil
	}
	return nil, ErrNoSnapshot
}

// AdmitTag admits a reader to the snapshot of remote/conversation at tag.
// The caller releases the admission.
func (l *Local) AdmitTag(ctx context.Context, remote, conversation, tag string) (*storage.Admission, error) {
	if t, err := storage.ParseTag(tag); err != nil || storage.Stamp(t) != tag {
		return nil, fmt.Errorf("pull: %q is not a snapshot's tag", tag)
	}
	dir, err := storage.ConversationDir(l.Root, remote, conversation)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(filepath.Join(dir, tag)); errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoSnapshot
	}
	return l.admit(ctx, dir, tag)
}

func (l *Local) admit(ctx context.Context, dir, tag string) (*storage.Admission, error) {
	root, err := storage.ClassifyRoot(filepath.Join(dir, tag))
	if err != nil {
		return nil, err
	}
	return storage.AdmitSnapshot(ctx, root, l.TTL, l.now, l.Files)
}

// LocalHeads finds, for a listing's local column, the head round of the
// newest unexpired snapshot of a conversation that admits a reader. The
// first admitted one's marker gives the head: no manifest and no evidence
// is read. Its lease is kept until the listing calls release.
type LocalHeads struct {
	l      *Local
	leases []*storage.Admission
}

// NewLocalHeads starts a listing's local column over l's root.
func (l *Local) NewLocalHeads() *LocalHeads { return &LocalHeads{l: l} }

// Head is a conversation's local head round, or false when no snapshot
// here admits a reader. Damage to one snapshot is said, and the next older
// snapshot is tried. Damage to the conversation itself, its directories on
// the way from the root or its own controls, is an error: the listing that
// asked fails.
func (h *LocalHeads) Head(ctx context.Context, remote, conversation string) (uint64, bool, error) {
	tags, err := h.l.Tags(remote, conversation)
	if err != nil {
		return 0, false, err
	}
	dir, err := storage.ConversationDir(h.l.Root, remote, conversation)
	if err != nil {
		return 0, false, err
	}
	for i := len(tags) - 1; i >= 0; i-- {
		a, err := h.l.admit(ctx, dir, tags[i])
		switch {
		case err == nil:
			// Kept for release, though the command stopped meanwhile.
			h.leases = append(h.leases, a)
			return a.Marker.Round, true, nil
		case errors.Is(err, storage.ErrExpired), errors.Is(err, storage.ErrRemovedTag), errors.Is(err, storage.ErrPublishing):
			continue
		case errors.Is(err, storage.ErrConversationControl), errors.Is(err, storage.ErrLocalWrite), ctx.Err() != nil:
			return 0, false, err
		}
		h.l.logf("%s/%s:%s is damaged and is passed over: %v", remote, conversation, tags[i], err)
	}
	return 0, false, nil
}

// layoutReal checks that each directory from the root's _remote down to dir
// is a real one, not a link: a pulled conversation is read only where its
// path says it is.
func layoutReal(root, dir string) error {
	rel, err := filepath.Rel(filepath.Join(root, storage.RemoteDir), dir)
	if err != nil {
		return err
	}
	at := filepath.Join(root, storage.RemoteDir)
	if err := storage.RealDirectory(at); err != nil {
		return err
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		at = filepath.Join(at, part)
		if err := storage.RealDirectory(at); err != nil {
			return err
		}
	}
	return nil
}

// Release lets go of every lease the column holds.
func (h *LocalHeads) Release(ctx context.Context) error {
	var err error
	for _, a := range h.leases {
		err = errors.Join(err, a.Release(ctx))
	}
	h.leases = nil
	return err
}
