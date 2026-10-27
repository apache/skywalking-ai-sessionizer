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

// Package client is the local end of a pull: it builds a conversation's
// snapshot from a manifest, reusing the files of the newest valid snapshot
// it already has and fetching the rest, accepts the build as whole evidence,
// and publishes it under a new tag; it recovers an interrupted publication,
// removes expired snapshots, and sweeps every pulled conversation when a
// command starts.
//
// Each operation owns one conversation, under its _pull, and reads nothing of
// the server but what its Source gives it.
package client

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/pull/format"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// Source gives a pull the bytes of the files a build needs.
type Source interface {
	// Fetch gives put the bytes of each name, once each, in any order. A
	// name it cannot give is an error. work is the build's private scratch
	// directory, for what the source keeps while the bytes arrive.
	Fetch(ctx context.Context, work string, names []string, put func(name string, r io.Reader) error) error
}

// Local is a storage root that holds pulled snapshots.
type Local struct {
	Root string
	// TTL is remote_snapshot_ttl: how long a snapshot lives from its
	// publication.
	TTL   time.Duration
	Files storage.FileSystem
	// Now is the clock; nil is the system's.
	Now func() time.Time
	// Log reports what a person should know and that does not stop the work:
	// damage passed over, copies made instead of links.
	Log func(format string, args ...any)
}

func (l *Local) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *Local) logf(format string, args ...any) {
	if l.Log != nil {
		l.Log(format, args...)
	}
}

// Result is what a pull of one conversation ended with.
type Result struct {
	// Tag is the snapshot that holds the manifest's evidence: a new one, or
	// the newest one when it held the same evidence already.
	Tag       string
	Unchanged bool
	// Admission holds the result: the caller judges this exact snapshot, or
	// releases it.
	Admission *storage.Admission
	// Reused and Fetched count the files taken from the base and from the
	// source. Copied says a reused file was copied, not linked.
	Reused, Fetched int
	Copied          bool
}

// ErrConflict is evidence that disagrees with what a base holds for the same
// landed file, or a session other than the base's.
var ErrConflict = errors.New("pull: the remote's evidence conflicts with the copy here")

// Pull makes the snapshot of remote/conversation that m lists, taking from
// src what no base holds. The conversation is held under its _pull for the
// whole operation, and the result is handed over admitted. A source that
// finds the conversation's files changed while they were fetched ends the
// build with a ChangedList: the build is discarded, and made again from its
// manifest, base selection and conflicts included, still under the same
// _pull, so no other pull or sweep takes the conversation between the two.
func (l *Local) Pull(ctx context.Context, remote, conversation string, m *format.Manifest, src Source) (_ *Result, err error) {
	if err := m.Check(conversation); err != nil {
		return nil, err
	}
	dir, pull, err := storage.BootstrapConversation(ctx, l.Root, remote, conversation, l.Files)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, pull.Unlock()) }()
	c := &conversationDir{l: l, dir: dir, remote: remote, conversation: conversation}
	if err := c.recover(ctx); err != nil {
		return nil, err
	}
	if err := c.cleanPrivate(ctx); err != nil {
		return nil, err
	}
	for {
		res, err := c.pull(ctx, m, src)
		changed := changedAlone(err)
		if changed == nil {
			return res, err
		}
		if err := changed.Manifest.Check(conversation); err != nil {
			return nil, err
		}
		m = changed.Manifest
	}
}

// changedAlone is the ChangedList err is when it is that and nothing else.
// A ChangedList joined with a local failure, such as a build's cleanup or a
// base's release that failed, is that failure's: no build starts again over
// it.
func changedAlone(err error) *ChangedList {
	for err != nil {
		if c, ok := err.(*ChangedList); ok { //nolint:errorlint // a ChangedList wrapped in another error is that error's
			return c
		}
		joined, ok := err.(interface{ Unwrap() []error })
		if !ok || len(joined.Unwrap()) != 1 {
			return nil
		}
		err = joined.Unwrap()[0]
	}
	return nil
}

// pull makes one build of m, with the conversation held.
func (c *conversationDir) pull(ctx context.Context, m *format.Manifest, src Source) (*Result, error) {
	digest := m.Digest()
	base, newest, err := c.selectBase(ctx)
	if err != nil {
		return nil, err
	}
	// A release that waits for the gate stops when ctx does, and leaves
	// the lease for a later sweep.
	release := func() error {
		if base == nil {
			return nil
		}
		return base.admission.Release(ctx)
	}
	// The same files are unchanged only under the same head and session: a
	// manifest that names another head for them is built, and checked.
	if base != nil && newest && base.accepted.Digest == digest && base.accepted.Manifest.Head == m.Head &&
		base.accepted.Manifest.Session == m.Session {
		return &Result{Tag: base.admission.Root.Tag, Unchanged: true, Admission: base.admission}, nil
	}
	if err := conflicts(base, m.Session, func(yield func(uint64, format.OriginalDigest) bool) {
		for _, e := range m.Files {
			if o := e.Original(); o != "" && !yield(e.Seq, o) {
				return
			}
		}
	}); err != nil {
		return nil, errors.Join(err, release())
	}
	res, err := c.build(ctx, m, base, src)
	return res, errors.Join(err, release())
}

// conversationDir is one conversation's copies, held under its _pull.
type conversationDir struct {
	l                    *Local
	dir                  string
	remote, conversation string
	// made holds the build directories already made and synced.
	made map[string]bool
}

func (c *conversationDir) files() storage.FileSystem { return c.l.Files }

// base is the snapshot a build reuses files from, accepted and admitted.
type base struct {
	admission *storage.Admission
	accepted  *format.Accepted
}

// tags lists the conversation's published tag directories, newest first. A
// name that is not a canonical tag is not a publication.
func (c *conversationDir) tags() ([]string, error) {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if _, err := storage.ParseTag(e.Name()); err == nil {
			out = append(out, e.Name())
		}
	}
	slices.Sort(out)
	slices.Reverse(out)
	return out, nil
}

// selectBase admits the unexpired snapshots newest first and accepts each,
// stopping at the first that holds whole evidence. A snapshot that does not
// is reported and passed over, never removed: only a person removes evidence
// found damaged. newest says the base is the newest snapshot that admitted a
// reader, which alone can make a pull unchanged.
func (c *conversationDir) selectBase(ctx context.Context) (*base, bool, error) {
	tags, err := c.tags()
	if err != nil {
		return nil, false, err
	}
	first := true
	for _, tag := range tags {
		root, err := storage.ClassifyRoot(filepath.Join(c.dir, tag))
		if err != nil {
			c.l.logf("%s/%s:%s: %v", c.remote, c.conversation, tag, err)
			continue
		}
		admission, err := storage.AdmitSnapshot(ctx, root, c.l.TTL, c.l.now, c.files())
		switch {
		case errors.Is(err, storage.ErrExpired), errors.Is(err, storage.ErrRemovedTag), errors.Is(err, storage.ErrPublishing):
			continue
		case err != nil:
			c.l.logf("%s/%s:%s: %v", c.remote, c.conversation, tag, err)
			first = false
			continue
		}
		m := admission.Marker
		accepted, err := format.Accept(root.Path, format.Published, format.Want{
			Conversation: c.conversation, ManifestDigest: format.ManifestDigest(m.ManifestDigest), Session: m.Session,
			Round: m.Round, Digest: format.CommitDigest(m.Digest), Remote: c.remote, Tag: tag,
		})
		if err != nil {
			c.l.logf("%s/%s:%s is damaged and is passed over: %v", c.remote, c.conversation, tag, err)
			first = false
			if rerr := admission.Release(ctx); rerr != nil {
				return nil, false, rerr
			}
			continue
		}
		return &base{admission: admission, accepted: accepted}, first, nil
	}
	return nil, false, nil
}

// conflicts compares a manifest's session and original landed digests with
// the base's checked ones, sequence by sequence where both have it. A
// skeleton may stand for a landed file and the reverse, by its original.
func conflicts(b *base, session string, originals func(yield func(uint64, format.OriginalDigest) bool)) error {
	if b == nil {
		return nil
	}
	if b.accepted.Manifest.Session != session {
		return fmt.Errorf("%w: session %s, and the copy here is of session %s", ErrConflict, session, b.accepted.Manifest.Session)
	}
	var err error
	originals(func(seq uint64, digest format.OriginalDigest) bool {
		if have, ok := b.accepted.Originals[seq]; ok && have != digest {
			err = fmt.Errorf("%w: landed file %d is %s, and the copy here has %s", ErrConflict, seq, digest, have)
			return false
		}
		return true
	})
	return err
}

// randomName is 32 random bytes as 64 lower-case hexadecimal characters.
func randomName() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
