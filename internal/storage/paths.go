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
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// The directories pull and its server own. Only their owners may write
// there, and only through their own locks and gates.
const (
	PullDir     = "_pull"
	AdmitDir    = "_admit"
	LeasesDir   = "_leases"
	BuildingDir = "_building"
	RemovingDir = "_removing"
	lockFile    = ".lock"
)

// protectedNames are the path components a supplied path may not reach. Each
// names a tree or a gate with an owner of its own. They compare without letter
// case, since a file system that ignores case reaches the same directory
// through any spelling.
var protectedNames = []string{RemoteDir, PullDir, RootLockDir, AdmitDir, LeasesDir, BuildingDir, RemovingDir}

// ErrProtected says a path reaches a tree or a lock file that only its owner
// may use.
var ErrProtected = errors.New("storage: the path reaches a tree or lock file that asz owns")

func protectedName(part string) bool {
	for _, name := range protectedNames {
		if strings.EqualFold(part, name) {
			return true
		}
	}
	return false
}

// refuseProtected checks the components of rel, a path below an already
// checked directory. A lock file is refused by its name, in any letter case:
// replacing an open lock's pathname would leave its holder locking the old file
// while another process locks the new one (internal/storage/lock_unix.go).
func refuseProtected(rel, whole string) error {
	if rel == "." || rel == "" {
		return nil
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if strings.EqualFold(parts[len(parts)-1], lockFile) {
		return fmt.Errorf("%w: %s is a lock file", ErrProtected, whole)
	}
	for _, part := range parts {
		if protectedName(part) {
			return fmt.Errorf("%w: %s passes through %s; copy the file outside it first", ErrProtected, whole, part)
		}
	}
	return nil
}

// admittedRelative is path relative to the admitted snapshot, resolved, when
// it lies inside it, and path itself otherwise.
func admittedRelative(admitted, path string) string {
	if admitted == "" {
		return path
	}
	resolved, err := filepath.EvalSymlinks(admitted)
	if err != nil {
		return path
	}
	if inside, ok := within(resolved, path); ok {
		return inside
	}
	return path
}

// within reports dir's path relative to root when dir is root or below it.
func within(root, dir string) (string, bool) {
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return rel, true
}

// resolveSupplied resolves every link in path. When the path does not exist
// yet, it resolves the longest existing ancestor and appends the rest, so a
// new file is classified by where it would really be created.
func resolveSupplied(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	missing := ""
	current := absolute
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			return filepath.Join(resolved, missing), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = filepath.Join(filepath.Base(current), missing)
		current = parent
	}
}

// CheckSuppliedPath classifies a file path a person gave a command, as
// opposed to one the command found in its admitted root: a configuration,
// token, certificate, label, pricing, reference, ledger, task or log file.
// Both the path as written and the path its links resolve to are checked, so
// a link can lead neither into a protected tree nor out of one. It returns the
// resolved path.
//
// admitted is the snapshot root a held lease admits, or empty. Its own path
// stays usable, but a lock file or a protected directory inside it is still
// refused.
func CheckSuppliedPath(path, admitted string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := resolveSupplied(path)
	if err != nil {
		return "", err
	}
	written := filepath.Clean(absolute)
	writtenRel, resolvedRel := written, resolved
	if admitted != "" {
		admittedReal, err := filepath.EvalSymlinks(admitted)
		if err != nil {
			return "", err
		}
		if inside, ok := within(admittedReal, resolved); ok {
			// Inside the admitted snapshot the path is classified from the
			// snapshot down. The written path names the snapshot however
			// the caller spelled it, which may differ from its resolved
			// form, so its tail of the same length is what is classified.
			resolvedRel = inside
			writtenRel = lastParts(written, inside)
		} else if admittedAbs, err := filepath.Abs(admitted); err != nil {
			return "", err
		} else if inside, ok := within(admittedAbs, written); ok {
			writtenRel = inside
		}
	}
	for _, pair := range [][2]string{{writtenRel, written}, {resolvedRel, resolved}} {
		if err := refuseProtected(strings.TrimPrefix(pair[0], string(filepath.Separator)), pair[1]); err != nil {
			return "", err
		}
	}
	return resolved, nil
}

// lastParts is the tail of path with as many components as like has, or the
// whole path when it has fewer.
func lastParts(path, like string) string {
	if like == "." {
		return "."
	}
	n := len(strings.Split(like, string(filepath.Separator)))
	parts := strings.Split(strings.TrimPrefix(path, filepath.VolumeName(path)), string(filepath.Separator))
	if n >= len(parts) {
		return path
	}
	return filepath.Join(parts[len(parts)-n:]...)
}

// OpenSuppliedRead opens a supplied file for reading after CheckSuppliedPath.
// The open goes through the parent directory's checked handle and never
// follows a link in the file's own name, so a path changed after the check
// cannot lead the read elsewhere.
func OpenSuppliedRead(path, admitted string) (*os.File, error) {
	return openSupplied(path, admitted, readShared)
}

// OpenSuppliedAppend opens an existing supplied file to append to it, as a
// child appends to the log its parent made.
func OpenSuppliedAppend(path, admitted string) (*os.File, error) {
	return openSupplied(path, admitted, appendExisting)
}

// CreateSupplied creates a supplied file that must not exist yet. A log a
// command makes for itself is created this way and never appended to: an
// existing file of that name, such as evidence reached through a hard link,
// is refused rather than written.
func CreateSupplied(path, admitted string) (*os.File, error) {
	return openSupplied(path, admitted, writeNew)
}

func openSupplied(path, admitted string, how fileOpen) (*os.File, error) {
	resolved, err := CheckSuppliedPath(path, admitted)
	if err != nil {
		return nil, err
	}
	return openResolved(resolved, admitted, how)
}

// openResolved opens a supplied path CheckSuppliedPath resolved and
// classified, binding its parent first.
func openResolved(resolved, admitted string, how fileOpen) (*os.File, error) {
	// The parent is bound through real directories from the volume's root,
	// and the path it was reached by is classified again: an ancestor replaced
	// by a link after the first check leads there, and is refused.
	dir, err := OpenDir(filepath.Dir(resolved))
	if err != nil {
		return nil, err
	}
	bound := filepath.Join(dir.Real(), filepath.Base(resolved))
	if err := refuseProtected(strings.TrimPrefix(admittedRelative(admitted, bound), string(filepath.Separator)), bound); err != nil {
		return nil, errors.Join(err, dir.Close())
	}
	f, err := dir.open(filepath.Base(resolved), how, PermState)
	return f, errors.Join(err, dir.Close())
}

// RootKind says what a command root is, by where it sits in the layouts pull
// owns rather than by any marker inside it.
type RootKind int

const (
	// OrdinaryRoot is a storage root outside both layouts.
	OrdinaryRoot RootKind = iota
	// SnapshotRoot is a published local snapshot,
	// <root>/_remote/<remote>/<conversation>/<tag>.
	SnapshotRoot
)

// Root is a classified command root.
type Root struct {
	Kind RootKind
	// Path is the root with its links resolved. Admission works on it.
	Path string
	// Written is the root as the caller named it, absolute and clean.
	Written string
	// For a snapshot: the storage root holding _remote, and its names.
	StorageRoot, Remote, Conversation, Tag string
	// For a snapshot: the identities of its conversation directory and of
	// the snapshot itself.
	ConversationInfo, SnapshotInfo fs.FileInfo
	// layout holds the identities of _remote, the remote, the conversation
	// and the snapshot along the written path, which Recheck compares.
	layout []fs.FileInfo
}

// layoutDirs lists _remote, the remote, the conversation and the snapshot
// along a snapshot's path.
func layoutDirs(snapshot string) []string {
	conversation := filepath.Dir(snapshot)
	return []string{filepath.Dir(filepath.Dir(conversation)), filepath.Dir(conversation), conversation, snapshot}
}

// Recheck confirms that a snapshot root is still what ClassifyRoot found:
// the written path still resolves to the same place, every layout directory
// along it is still the same real directory, and the resolved path is still
// reached through real directories alone. Admission calls it under its gate,
// so a layout directory replaced since the classification, even by a link to
// itself, is refused before anything inside is read.
func (r Root) Recheck() error {
	if r.Kind != SnapshotRoot {
		return nil
	}
	resolved, err := filepath.EvalSymlinks(r.Written)
	if err != nil {
		return err
	}
	if resolved != r.Path {
		return fmt.Errorf("%w: %s now leads to %s, not %s", ErrProtected, r.Written, resolved, r.Path)
	}
	for i, dir := range layoutDirs(r.Written) {
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if info.Mode().Type() != fs.ModeDir || !os.SameFile(info, r.layout[i]) {
			return fmt.Errorf("%w: %s changed since it was classified", ErrProtected, dir)
		}
	}
	d, err := OpenDir(r.Path)
	if err != nil {
		return err
	}
	same := os.SameFile(d.info, r.SnapshotInfo)
	if err := d.Close(); err != nil {
		return err
	}
	if !same {
		return fmt.Errorf("%w: %s changed since it was classified", ErrProtected, r.Path)
	}
	return nil
}

// ClassifyRoot places a command root before anything inside it is read or
// locked. A path inside _remote/<remote>/<conversation>/ belongs to that
// layout: only its canonical tag directories are readable roots, and the
// outer directories, private builds, removal trees, gates and anything below a
// snapshot are refused. Anything inside a server's _pull is refused too:
// server snapshots are read only by admitted pull routes. Both the path as
// written and the resolved path are classified, so a link cannot turn a path
// in an owned layout into an ordinary root, or lead an ordinary-looking path
// into one. A missing or damaged marker never makes a layout path ordinary.
func ClassifyRoot(path string) (Root, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Root{}, err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return Root{}, err
	}
	written, err := classifyLayout(filepath.Clean(absolute))
	if err != nil {
		return Root{}, err
	}
	actual, err := classifyLayout(resolved)
	if err != nil {
		return Root{}, err
	}
	if written.Kind != actual.Kind || written.Remote != actual.Remote || written.Conversation != actual.Conversation || written.Tag != actual.Tag {
		return Root{}, fmt.Errorf("%w: %s and the path its links resolve to sit in different places", ErrProtected, path)
	}
	actual.Path = resolved
	actual.Written = filepath.Clean(absolute)
	if actual.Kind == SnapshotRoot {
		// A link may lead to the storage root, but not within the layout
		// below it: _remote, the remote, the conversation and the tag are
		// each a real directory where the path names them.
		for _, dir := range layoutDirs(actual.Written) {
			info, err := os.Lstat(dir)
			if err != nil {
				return Root{}, err
			}
			if info.Mode().Type() != fs.ModeDir {
				return Root{}, fmt.Errorf("%w: %s is not a real directory", ErrProtected, dir)
			}
			actual.layout = append(actual.layout, info)
		}
		actual.ConversationInfo, actual.SnapshotInfo = actual.layout[2], actual.layout[3]
		// The resolved path must be reached through real directories too,
		// and lead to the same snapshot.
		if err := actual.Recheck(); err != nil {
			return Root{}, err
		}
	}
	return actual, nil
}

func classifyLayout(path string) (Root, error) {
	parts := strings.Split(path, string(filepath.Separator))
	for i, part := range parts {
		if !protectedName(part) {
			continue
		}
		// Only a canonical tag directory exactly three levels below _remote,
		// with nothing protected above it, is a readable root.
		if part != RemoteDir || len(parts) != i+4 {
			return Root{}, fmt.Errorf("%w: %s is inside %s and is not a command root", ErrProtected, path, part)
		}
		remote, conversation, tag := parts[i+1], parts[i+2], parts[i+3]
		if err := CheckCopyNames(remote, conversation); err != nil {
			return Root{}, fmt.Errorf("%w: %s: %w", ErrProtected, path, err)
		}
		if _, err := ParseTag(tag); err != nil {
			return Root{}, fmt.Errorf("%w: %s is not a snapshot: %w", ErrProtected, path, err)
		}
		return Root{
			Kind:         SnapshotRoot,
			StorageRoot:  filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(path)))),
			Remote:       remote,
			Conversation: conversation,
			Tag:          tag,
		}, nil
	}
	return Root{Kind: OrdinaryRoot}, nil
}

// ReplaceSuppliedDurable replaces a supplied file, such as a tokens file,
// whose path CheckSuppliedPath resolved to resolved. The parent is bound
// once, and its real path must still be the one the check resolved: an
// ancestor replaced by a link since then leads the bind elsewhere, and is
// refused. The temporary file, the replacement and the directory's sync then
// all go through that one handle, so nothing reaches a directory the check
// never saw. A busy replacement on Windows is a ControlBusyError, for the
// caller's RetryControl.
func ReplaceSuppliedDurable(resolved string, mode os.FileMode, write func(io.Writer) error, files FileSystem) (err error) {
	files = writeFileSystem(files)
	parent := filepath.Dir(resolved)
	dir, err := OpenDir(parent)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	if dir.Real() != parent {
		return fmt.Errorf("%w: %s now leads to %s", ErrProtected, parent, dir.Real())
	}
	bound := filepath.Join(dir.Real(), filepath.Base(resolved))
	if err := refuseProtected(strings.TrimPrefix(admittedRelative("", bound), string(filepath.Separator)), bound); err != nil {
		return err
	}
	var name string
	var file WriteFile
	for {
		name = reservedPrefix + rand.Text()
		file, err = files.CreateIn(dir, name, 0o600)
		if !errors.Is(err, os.ErrExist) {
			break
		}
	}
	if err != nil {
		return err
	}
	closed, renamed := false, false
	defer func() {
		if !closed {
			err = errors.Join(err, file.Close())
		}
		if !renamed {
			cleanup := files.RemoveIn(dir, name)
			var busy *ControlBusyError
			if cleanup != nil && errors.As(err, &busy) {
				err = busy.Err
			}
			err = errors.Join(err, cleanup)
		}
	}()
	if err = write(file); err != nil {
		return err
	}
	if err = file.Chmod(mode); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return fmt.Errorf("storage: supplied file sync failed: %w", err)
	}
	closed = true
	if err = file.Close(); err != nil {
		return err
	}
	if err = files.ReplaceIn(dir, name, filepath.Base(resolved)); err != nil {
		return err
	}
	renamed = true
	if err = files.SyncDirIn(dir); err != nil {
		return fmt.Errorf("storage: supplied file replacement is visible but its directory sync failed: %w", err)
	}
	return nil
}
