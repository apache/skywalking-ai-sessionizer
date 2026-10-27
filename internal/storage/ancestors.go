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
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// SyncAncestors requires checked real directories. Existing ancestors need the
// same syncs as new ones because another process may have stopped before syncing.
func SyncAncestors(root, directory string, files FileSystem) error {
	return ancestors(context.Background(), root, directory, 0, false, writeFileSystem(files))
}

// SyncAncestorsContext is SyncAncestors that stops between syncs once ctx
// ends: the sync it is in finishes.
func SyncAncestorsContext(ctx context.Context, root, directory string, files FileSystem) error {
	return ancestors(ctx, root, directory, 0, false, writeFileSystem(files))
}

// MakeAncestorsDurable creates one level at a time so no dependent write precedes
// its parent's sync. The caller checks and owns the ancestry before calling.
func MakeAncestorsDurable(root, directory string, mode os.FileMode, files FileSystem) error {
	return ancestors(context.Background(), root, directory, mode, true, writeFileSystem(files))
}

func ancestors(ctx context.Context, root, directory string, mode os.FileMode, create bool, files FileSystem) error {
	relative, err := filepath.Rel(root, directory)
	if err != nil {
		return err
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("storage: directory %s is outside root %s", directory, root)
	}
	current := filepath.Clean(root)
	parts := []string{"."}
	if relative != "." {
		parts = append(parts, strings.Split(relative, string(filepath.Separator))...)
	}
	for _, part := range parts {
		current = filepath.Join(current, part)
		if create {
			if err := files.Mkdir(current, mode); err != nil && !os.IsExist(err) {
				return err
			}
		}
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("storage: ancestor %s is not a real directory", current)
		}
		// Below the root, the directory found must hold this exact spelling: a
		// file system that folds letter case or Unicode normalization would
		// otherwise merge two owners into one directory.
		if part != "." {
			if err := CheckSpellings(filepath.Dir(current), []string{part}); err != nil {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := files.SyncDir(filepath.Dir(current)); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := files.SyncDir(current); err != nil {
			return err
		}
	}
	return nil
}

// SyncRenameParents checks both entries before any dependent transition.
func SyncRenameParents(from, to string, files FileSystem) error {
	files = writeFileSystem(files)
	first, second := filepath.Dir(from), filepath.Dir(to)
	if err := files.SyncDir(first); err != nil {
		return err
	}
	if second != first {
		return files.SyncDir(second)
	}
	return nil
}

// SyncTree syncs every directory of tree from the leaves up, tree last, so a
// name in each is durable before its parent's. Files are not synced here:
// each was synced when it was written or installed, and a link shares an
// already durable file.
func SyncTree(tree string, files FileSystem) error {
	return SyncTreeContext(context.Background(), tree, files)
}

// SyncTreeContext is SyncTree that stops between directories once ctx ends:
// the sync it is in finishes.
func SyncTreeContext(ctx context.Context, tree string, files FileSystem) error {
	files = writeFileSystem(files)
	entries, err := os.ReadDir(tree)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Type() == fs.ModeDir {
			if err := SyncTreeContext(ctx, filepath.Join(tree, entry.Name()), files); err != nil {
				return err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return files.SyncDir(tree)
}
