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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Detach moves a tree into removing, a _removing directory its owner holds,
// under a fresh name that keeps prefix in front, and syncs both parents. A
// tag's tree keeps its tag as the prefix, so a partly deleted tree stays
// identifiable after its marker is gone. The caller has already written the
// removal record freshly and closed every handle inside the tree: Windows
// refuses to rename a directory holding an open file.
func Detach(tree, removing, prefix string, files FileSystem) (string, error) {
	files = writeFileSystem(files)
	if err := RealDirectory(tree); err != nil {
		return "", err
	}
	if err := RealDirectory(removing); err != nil {
		return "", err
	}
	for {
		random := make([]byte, 32)
		if _, err := rand.Read(random); err != nil {
			return "", err
		}
		target := filepath.Join(removing, prefix+"-"+hex.EncodeToString(random))
		err := InstallNoReplace(tree, target, files)
		if errors.Is(err, ErrExists) {
			continue
		}
		if err != nil {
			return "", err
		}
		return target, SyncRenameParents(tree, target, files)
	}
}

// DeleteOwned deletes an entry of a directory its owner holds whole, such as a
// conversation's _building or _removing, then syncs that directory. Owned
// trees are deleted by place alone: nothing inside is read or validated, so a
// tree whose internal records are already gone is still deleted. The entry must
// be a real directory directly inside the checked parent. No link is followed,
// and no file's mode is changed, so a link a snapshot shares keeps its mode.
func DeleteOwned(parent, name string, files FileSystem) error {
	return DeleteOwnedContext(context.Background(), parent, name, files)
}

// DeleteOwnedContext is DeleteOwned that stops between deletions once ctx
// ends: the deletion it is in finishes, and what is left of the tree stays
// for the owner's next cleanup.
func DeleteOwnedContext(ctx context.Context, parent, name string, files FileSystem) error {
	files = writeFileSystem(files)
	if err := RealDirectory(parent); err != nil {
		return err
	}
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return fmt.Errorf("%w: %q is not an entry of %s", ErrProtected, name, parent)
	}
	tree := filepath.Join(parent, name)
	info, err := os.Lstat(tree)
	if errors.Is(err, fs.ErrNotExist) {
		return files.SyncDir(parent)
	}
	if err != nil {
		return err
	}
	if info.Mode().Type() != fs.ModeDir {
		return fmt.Errorf("%w: %s is not a real directory", ErrProtected, tree)
	}
	if err := deleteOwnedTree(ctx, tree, files); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return files.SyncDir(parent)
}

// deleteOwnedTree removes everything under dir, children first, through files
// so a test can see each unlink. A link is removed as a link.
func deleteOwnedTree(ctx context.Context, dir string, files FileSystem) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if entry.Type() == fs.ModeDir {
			if err := deleteOwnedTree(ctx, path, files); err != nil {
				return err
			}
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := files.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := files.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
