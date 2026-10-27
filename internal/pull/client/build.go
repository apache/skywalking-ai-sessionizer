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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"slices"

	"github.com/apache/skywalking-ai-sessionizer/internal/pull/format"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// workDir is a build's private scratch, removed before publication.
const workDir = "_work"

// build makes the snapshot in a private directory of _building, accepts it,
// and publishes it. The private directory is disposed of on any failure.
func (c *conversationDir) build(ctx context.Context, m *format.Manifest, b *base, src Source) (_ *Result, err error) {
	files := c.files()
	building := filepath.Join(c.dir, storage.BuildingDir)
	if err := storage.MakeOwnedDir(building, files); err != nil {
		return nil, err
	}
	name, err := randomName()
	if err != nil {
		return nil, err
	}
	tree := filepath.Join(building, name)
	if err := storage.MakeOwnedDir(tree, files); err != nil {
		return nil, err
	}
	published := false
	defer func() {
		if !published {
			// A stopped command stops between deletions, and leaves the
			// rest for the next cleanup of _building.
			err = errors.Join(err, storage.DeleteOwnedContext(ctx, building, name, files))
		}
	}()
	// The manifest is saved before any evidence, so a build is never
	// reconstructed from the files it happens to hold.
	if err := storage.WriteNewDurable(filepath.Join(tree, format.ManifestFile), storage.PermLanded, func(w io.Writer) error {
		return format.WriteManifest(w, m)
	}, files); err != nil {
		return nil, err
	}
	temps := filepath.Join(tree, workDir, "files")
	if err := storage.MakeAncestorsDurable(tree, temps, 0o755, files); err != nil {
		return nil, err
	}
	res := &Result{}
	var missing []string
	want := map[string]format.Entry{}
	for _, e := range m.Files {
		if b == nil || b.accepted.Stored[e.File] != e.Digest {
			missing = append(missing, e.File)
			want[e.File] = e
			continue
		}
		// The same name with the same stored bytes: a round, a landed file
		// or skeleton, or a store entry, never one standing in for another.
		// A stopped command stops between files: only the operation it is
		// in can hold it past its grace.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		dest, err := c.destination(tree, e.File)
		if err != nil {
			return nil, err
		}
		temp, err := tempName(temps)
		if err != nil {
			return nil, err
		}
		sum, err := sumOf(string(e.Digest))
		if err != nil {
			return nil, err
		}
		copied, err := storage.ShareFile(filepath.Join(b.admission.Root.Path, filepath.FromSlash(e.File)), dest, temp, int64(e.Bytes), sum, files)
		if err != nil {
			return nil, err
		}
		res.Reused++
		res.Copied = res.Copied || copied
	}
	if res.Copied {
		c.l.logf("%s/%s: reused files are copied, not linked, so this snapshot takes its whole size", c.remote, c.conversation)
	}
	if len(missing) > 0 {
		put := func(name string, r io.Reader) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			e, ok := want[name]
			if !ok {
				return fmt.Errorf("pull: %s was not asked for, or was given twice", name)
			}
			delete(want, name)
			dest, err := c.destination(tree, name)
			if err != nil {
				return err
			}
			temp, err := tempName(temps)
			if err != nil {
				return err
			}
			sum, err := sumOf(string(e.Digest))
			if err != nil {
				return err
			}
			if err := storage.WriteChecked(temp, r, int64(e.Bytes), sum, files); err != nil {
				return fmt.Errorf("pull: %s: %w", name, err)
			}
			res.Fetched++
			return storage.InstallFile(temp, dest, files)
		}
		if err := src.Fetch(ctx, filepath.Join(tree, workDir), missing, put); err != nil {
			return nil, err
		}
		if len(want) > 0 {
			return nil, fmt.Errorf("pull: the source did not give %d of the files asked for", len(want))
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := checkSpellings(tree, m); err != nil {
		return nil, err
	}
	accepted, err := format.Accept(tree, format.Building, format.Want{Conversation: c.conversation, ManifestDigest: m.Digest()})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The originals were compared with the base before the build, as the
	// manifest advertised them; the checked ones are compared again.
	if err := conflicts(b, accepted.Manifest.Session, func(yield func(uint64, format.OriginalDigest) bool) {
		for seq, digest := range accepted.Originals {
			if !yield(seq, digest) {
				return
			}
		}
	}); err != nil {
		return nil, err
	}
	if err := c.finishBuild(ctx, tree); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tag, err := c.publish(ctx, name, tree, accepted)
	if err != nil {
		return nil, err
	}
	published = true
	root, err := storage.ClassifyRoot(filepath.Join(c.dir, tag))
	if err != nil {
		return nil, err
	}
	res.Tag = tag
	res.Admission, err = storage.AdmitSnapshot(ctx, root, c.l.TTL, c.l.now, files)
	if err != nil {
		return res, fmt.Errorf("pull: %s was published and cannot be admitted: %w", tag, err)
	}
	return res, nil
}

// destination is where a listed file goes in a build, with its directories
// made and synced, each once. Names were checked by the manifest; a
// directory on the way must be a real one.
func (c *conversationDir) destination(tree, name string) (string, error) {
	dest := filepath.Join(tree, filepath.FromSlash(name))
	dir := filepath.Dir(dest)
	if c.made[dir] {
		return dest, nil
	}
	if err := storage.MakeAncestorsDurable(tree, dir, 0o755, c.files()); err != nil {
		return "", err
	}
	if c.made == nil {
		c.made = map[string]bool{}
	}
	c.made[dir] = true
	return dest, nil
}

func tempName(dir string) (string, error) {
	name, err := randomName()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".tmp"), nil
}

func sumOf(digest string) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	b, err := hex.DecodeString(digest)
	if err != nil || len(b) != sha256.Size {
		return sum, fmt.Errorf("pull: malformed digest %q", digest)
	}
	copy(sum[:], b)
	return sum, nil
}

// checkSpellings confirms the build holds every listed name under exactly
// its own spelling: a file system that folds case or normalizes a name
// would merge two owners or answer to another spelling.
func checkSpellings(tree string, m *format.Manifest) error {
	byDir := map[string][]string{}
	for _, e := range m.Files {
		dir, file := path.Split(e.File)
		byDir[dir] = append(byDir[dir], file)
		for dir != "" {
			parent, child := path.Split(path.Clean(dir))
			if !slices.Contains(byDir[parent], child) {
				byDir[parent] = append(byDir[parent], child)
			}
			dir = parent
		}
	}
	for dir, names := range byDir {
		if err := storage.CheckSpellings(filepath.Join(tree, filepath.FromSlash(dir)), names); err != nil {
			return err
		}
	}
	return nil
}

// finishBuild makes the controls a published snapshot carries, without
// taking them, removes the private scratch, and makes the whole tree durable:
// every directory from the leaves up, then every ancestor from the storage
// root down. A stopped command stops between those operations.
func (c *conversationDir) finishBuild(ctx context.Context, tree string) error {
	files := c.files()
	lockDir := filepath.Join(tree, storage.RootLockDir)
	if err := storage.MakeOwnedDir(lockDir, files); err != nil {
		return err
	}
	if err := storage.MakeLockFile(lockDir, files); err != nil {
		return err
	}
	if err := storage.MakeOwnedDir(filepath.Join(tree, storage.LeasesDir), files); err != nil {
		return err
	}
	if err := storage.DeleteOwnedContext(ctx, tree, workDir, files); err != nil {
		return err
	}
	if err := storage.SyncTreeContext(ctx, tree, files); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return storage.SyncAncestorsContext(ctx, c.l.Root, tree, files)
}
