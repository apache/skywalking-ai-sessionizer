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

// Package faulttest models power loss for storage tests. Reads still use real files.
package faulttest

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// DropAll excludes every change that has not reached a successful sync.
const DropAll = -1

// Operation is one call the model saw, in order, for Fault and After to act on.
type Operation struct {
	Index  int
	Kind   string
	Path   string
	Target string
}

type node struct {
	mode    os.FileMode
	data    []byte
	entries map[string]uint64
}

type change struct {
	at      int
	kind    string
	owner   uint64
	name    string
	target  uint64
	offset  int64
	data    []byte
	mode    os.FileMode
	durable bool
	failed  bool
}

// Model separates file contents and attributes from each parent's entries.
// Fault runs before an operation. After runs after its visible effects, which
// lets a fixture model an unknown outcome without undoing the real write.
type Model struct {
	Root  string
	Fault func(Operation) error
	After func(Operation) error

	mutex       sync.Mutex
	operations  []Operation
	changes     []*change
	initial     map[uint64]*node
	visible     map[uint64]*node
	next        uint64
	syncs       int
	failedSyncs map[int]bool
	disk        storage.DiskFileSystem
	// identities holds the real identities of every directory the model
	// tracks, so a creation in a bound directory is recorded in the
	// directory the handle is bound to, wherever its path now leads. One
	// node can have two: a power loss can restore a renamed directory under
	// both of its parents. Each identity is read through a handle, which on
	// Windows carries the file's id, where a cached Lstat loads it later
	// from whatever then holds the path.
	identities map[uint64][]os.FileInfo
}

// New models an empty directory root. Everything a test then makes through the
// model is tracked; what it makes around the model is never durable.
func New(root string) (*Model, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("faulttest: root is not a directory")
	}
	initial := map[uint64]*node{1: {mode: info.Mode(), entries: make(map[string]uint64)}}
	identity, err := directoryIdentity(absolute)
	if err != nil {
		return nil, err
	}
	return &Model{Root: absolute, initial: initial, visible: clone(initial), next: 2, failedSyncs: make(map[int]bool),
		identities: map[uint64][]os.FileInfo{1: {identity}}}, nil
}

// directoryIdentity reads a directory's identity through a handle on it.
func directoryIdentity(path string) (os.FileInfo, error) {
	d, err := storage.OpenDir(path)
	if err != nil {
		return nil, err
	}
	info := d.Info()
	return info, d.Close()
}

// FailSync numbers sync attempts from one, including failed attempts.
func (m *Model) FailSync(numbers ...int) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	for _, number := range numbers {
		m.failedSyncs[number] = true
	}
}

// Operations lists every call so far.
func (m *Model) Operations() []Operation {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return append([]Operation(nil), m.operations...)
}

// Point is the number of calls so far, for PowerLoss's ordered variant.
func (m *Model) Point() int {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return len(m.operations)
}

func (m *Model) begin(kind, path, target string) (Operation, error) {
	operation := Operation{Index: len(m.operations) + 1, Kind: kind, Path: path, Target: target}
	m.operations = append(m.operations, operation)
	if m.Fault != nil {
		return operation, m.Fault(operation)
	}
	return operation, nil
}

func (m *Model) finish(operation Operation) error {
	if m.After != nil {
		return m.After(operation)
	}
	return nil
}

func (m *Model) record(operation Operation, value change) {
	value.at = operation.Index
	m.changes = append(m.changes, &value)
	apply(m.visible, &value)
}

func (m *Model) lookup(path string) uint64 {
	relative, err := filepath.Rel(m.Root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return 0
	}
	current := uint64(1)
	if relative == "." {
		return current
	}
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		directory := m.visible[current]
		if directory == nil {
			return 0
		}
		current = directory.entries[part]
	}
	return current
}

// bound is the tracked directory a handle is bound to, by its real identity,
// or 0 for one the model did not make.
func (m *Model) bound(dir *storage.Dir) uint64 {
	for id, infos := range m.identities {
		for _, info := range infos {
			if m.visible[id] != nil && os.SameFile(info, dir.Info()) {
				return id
			}
		}
	}
	return 0
}

// physical is the identity of the directory at path, or nil when nothing or
// something other than a directory is there. It is read before a removal or a
// replacement, which forgets it, and needs no permission on the directory
// itself: one that cannot be read is still identified. An identity that
// cannot be read at all is an error, and the operation does not run.
func physical(path string) (os.FileInfo, error) {
	// The path is cleaned as the operation cleans it: Lstat of "link/"
	// would describe what the link leads to, which the operation leaves.
	info, err := os.Lstat(filepath.Clean(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, nil
	}
	// Comparing an identity loads it: on Windows a cached Lstat reads its
	// file id only then, from whatever holds the path at that moment, so it
	// is read now.
	_ = os.SameFile(info, info)
	return info, nil
}

// forget drops one directory's identity once it was removed or replaced, so
// a new directory that reuses its inode is never taken for it. Only that
// directory goes: a node restored in two places keeps the copy that remains.
func (m *Model) forget(gone os.FileInfo) {
	if gone == nil {
		return
	}
	for id, infos := range m.identities {
		kept := infos[:0]
		for _, info := range infos {
			if !os.SameFile(info, gone) {
				kept = append(kept, info)
			}
		}
		if len(kept) == 0 {
			delete(m.identities, id)
		} else {
			m.identities[id] = kept
		}
	}
}

func (m *Model) add(operation Operation, path string, mode os.FileMode) uint64 {
	return m.addIn(operation, m.lookup(filepath.Dir(path)), filepath.Base(path), mode)
}

func (m *Model) addIn(operation Operation, owner uint64, name string, mode os.FileMode) uint64 {
	id := m.next
	m.next++
	base := &node{mode: mode.Type() | 0o600}
	if mode.IsDir() {
		base.mode = mode.Type() | 0o755
		base.entries = make(map[string]uint64)
	}
	// Unreachable objects are harmless. Only modeled parent entries expose them.
	m.initial[id] = base
	m.visible[id] = cloneNode(base)
	m.record(operation, change{kind: "mode", owner: id, mode: mode})
	m.record(operation, change{kind: "entry", owner: owner, name: name, target: id})
	return id
}

func (m *Model) Create(path string, mode os.FileMode) (storage.WriteFile, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	operation, err := m.begin("create", path, "")
	if err != nil {
		return nil, err
	}
	file, err := m.disk.Create(path, mode)
	if err != nil {
		return nil, err
	}
	id := m.add(operation, path, mode)
	if err := m.finish(operation); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return &modelFile{model: m, file: file, path: path, id: id}, nil
}

// CreateIn records a creation inside a bound directory as Create records one
// at the path, in the directory the handle is bound to: a directory renamed
// after it was bound keeps the file, whatever now holds its old path.
func (m *Model) CreateIn(dir *storage.Dir, name string, mode os.FileMode) (storage.WriteFile, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	path := filepath.Join(dir.Path(), name)
	operation, err := m.begin("create", path, "")
	if err != nil {
		return nil, err
	}
	file, err := m.disk.CreateIn(dir, name, mode)
	if err != nil {
		return nil, err
	}
	id := m.addIn(operation, m.bound(dir), name, mode)
	if err := m.finish(operation); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return &modelFile{model: m, file: file, path: path, id: id}, nil
}

// RemoveIn records a removal inside a bound directory as Remove records one.
func (m *Model) RemoveIn(dir *storage.Dir, name string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	path := filepath.Join(dir.Path(), name)
	operation, err := m.begin("unlink", path, "")
	if err != nil {
		return err
	}
	gone, err := physical(path)
	if err != nil {
		return err
	}
	if err := m.disk.RemoveIn(dir, name); err != nil {
		return err
	}
	m.forget(gone)
	m.record(operation, change{kind: "entry", owner: m.bound(dir), name: name})
	return m.finish(operation)
}

func (m *Model) Mkdir(path string, mode os.FileMode) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	operation, err := m.begin("mkdir", path, "")
	if err != nil {
		return err
	}
	if err := m.disk.Mkdir(path, mode); err != nil {
		return err
	}
	id := m.add(operation, path, mode|os.ModeDir)
	if info, err := directoryIdentity(path); err == nil {
		m.identities[id] = []os.FileInfo{info}
	}
	return m.finish(operation)
}

func (m *Model) Chmod(path string, mode os.FileMode) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	operation, err := m.begin("mode", path, "")
	if err != nil {
		return err
	}
	if err := m.disk.Chmod(path, mode); err != nil {
		return err
	}
	m.record(operation, change{kind: "mode", owner: m.lookup(path), mode: mode})
	return m.finish(operation)
}

func (m *Model) Link(from, to string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	operation, err := m.begin("link", from, to)
	if err != nil {
		return err
	}
	if err := m.disk.Link(from, to); err != nil {
		return err
	}
	m.record(operation, change{kind: "entry", owner: m.lookup(filepath.Dir(to)), name: filepath.Base(to), target: m.lookup(from)})
	return m.finish(operation)
}

func (m *Model) Rename(from, to string) error {
	return m.rename("rename", from, to, func() error { return m.disk.Rename(from, to) })
}

// RenameNoReplace records a rename like Rename. A Fault returning
// storage.ErrNoReplaceUnsupported for the "rename-noreplace" kind models a
// file system without the native call, before anything changes.
func (m *Model) RenameNoReplace(from, to string) error {
	return m.rename("rename-noreplace", from, to, func() error { return m.disk.RenameNoReplace(from, to) })
}

func (m *Model) Replace(parent, from, to string) error {
	return m.rename("replace", filepath.Join(parent, from), filepath.Join(parent, to), func() error {
		return m.disk.Replace(parent, from, to)
	})
}

// ReplaceIn records a replacement inside a bound directory as Replace
// records one at its path.
func (m *Model) ReplaceIn(dir *storage.Dir, from, to string) error {
	return m.rename("replace", filepath.Join(dir.Path(), from), filepath.Join(dir.Path(), to), func() error {
		return m.disk.ReplaceIn(dir, from, to)
	})
}

func (m *Model) rename(kind, from, to string, perform func() error) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	operation, err := m.begin(kind, from, to)
	if err != nil {
		return err
	}
	// A directory the rename replaces is gone, though the model may hold
	// it and the source as one node, as after a power loss restored both.
	source, err := physical(from)
	if err != nil {
		return err
	}
	destination, err := physical(to)
	if err != nil {
		return err
	}
	var replaced os.FileInfo
	if destination != nil && (source == nil || !os.SameFile(source, destination)) {
		replaced = destination
	}
	if err := perform(); err != nil {
		return err
	}
	m.forget(replaced)
	id := m.lookup(from)
	m.record(operation, change{kind: "entry", owner: m.lookup(filepath.Dir(from)), name: filepath.Base(from)})
	m.record(operation, change{kind: "entry", owner: m.lookup(filepath.Dir(to)), name: filepath.Base(to), target: id})
	return m.finish(operation)
}

func (m *Model) Remove(path string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	operation, err := m.begin("unlink", path, "")
	if err != nil {
		return err
	}
	gone, err := physical(path)
	if err != nil {
		return err
	}
	if err := m.disk.Remove(path); err != nil {
		return err
	}
	m.forget(gone)
	m.record(operation, change{kind: "entry", owner: m.lookup(filepath.Dir(path)), name: filepath.Base(path)})
	return m.finish(operation)
}

func (m *Model) SyncFile(path string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return m.sync("file sync", path, m.lookup(path), func() error { return m.disk.SyncFile(path) })
}

func (m *Model) SyncDir(path string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return m.sync("directory sync", path, m.lookup(path), func() error { return m.disk.SyncDir(path) })
}

func (m *Model) SyncDirIn(dir *storage.Dir) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	// The sync is of the directory the handle is bound to, wherever its
	// path leads now.
	return m.sync("directory sync", dir.Path(), m.bound(dir), func() error { return m.disk.SyncDirIn(dir) })
}

func (m *Model) sync(kind, path string, id uint64, apply func() error) error {
	operation, err := m.begin(kind, path, "")
	m.syncs++
	if m.failedSyncs[m.syncs] {
		err = syscall.EIO
	}
	if err == nil {
		err = apply()
	}
	if err == nil {
		err = m.finish(operation)
	}
	for _, value := range m.changes {
		if id == 0 || value.owner != id || value.durable || value.failed {
			continue
		}
		if (kind == "file sync" && value.kind == "entry") || (kind == "directory sync" && value.kind == "write") {
			continue
		}
		if err != nil {
			value.failed = true
		} else {
			value.durable = true
		}
	}
	return err
}

type modelFile struct {
	model  *Model
	file   storage.WriteFile
	path   string
	id     uint64
	offset int64
}

func (f *modelFile) Write(data []byte) (int, error) {
	m := f.model
	m.mutex.Lock()
	defer m.mutex.Unlock()
	operation, err := m.begin("write", f.path, "")
	if err != nil {
		return 0, err
	}
	count, err := f.file.Write(data)
	if count > 0 {
		m.record(operation, change{kind: "write", owner: f.id, offset: f.offset, data: append([]byte(nil), data[:count]...)})
		f.offset += int64(count)
	}
	return count, errors.Join(err, m.finish(operation))
}

func (f *modelFile) Chmod(mode os.FileMode) error {
	m := f.model
	m.mutex.Lock()
	defer m.mutex.Unlock()
	operation, err := m.begin("mode", f.path, "")
	if err != nil {
		return err
	}
	if err := f.file.Chmod(mode); err != nil {
		return err
	}
	m.record(operation, change{kind: "mode", owner: f.id, mode: mode})
	return m.finish(operation)
}

func (f *modelFile) Sync() error {
	m := f.model
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return m.sync("file sync", f.path, f.id, f.file.Sync)
}

func (f *modelFile) Close() error {
	m := f.model
	m.mutex.Lock()
	defer m.mutex.Unlock()
	operation, err := m.begin("close", f.path, "")
	// A reported close error must not leak a real handle in a fixture.
	return errors.Join(err, f.file.Close(), m.finish(operation))
}

// PowerLoss drops non-durable changes after keepThrough, in their original order.
// DropAll drops all of them. The destination must be a fresh, empty directory.
// Failed syncs never become durable through later syncs. Earlier non-durable
// changes may survive only in the ordered variant. Unmodeled writes never survive.
func (m *Model) PowerLoss(destination string, keepThrough int) (*Model, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	entries, err := os.ReadDir(destination)
	if err != nil {
		return nil, err
	}
	if len(entries) != 0 {
		return nil, errors.New("faulttest: power-loss destination is not empty")
	}
	state := clone(m.initial)
	for _, value := range m.changes {
		if value.durable || value.at <= keepThrough {
			apply(state, value)
		}
	}
	dirs := make(map[uint64][]string)
	if err := materialize(destination, 1, state, make(map[uint64]string), dirs); err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(destination)
	if err != nil {
		return nil, err
	}
	// The materialized directories are new ones, with identities of their
	// own, and a node restored in two places has both.
	identities := map[uint64][]os.FileInfo{}
	for id, paths := range dirs {
		for _, path := range paths {
			info, err := directoryIdentity(path)
			if err != nil {
				return nil, err
			}
			identities[id] = append(identities[id], info)
		}
	}
	return &Model{Root: absolute, initial: state, visible: clone(state), next: m.next, failedSyncs: make(map[int]bool),
		identities: identities}, nil
}

func apply(state map[uint64]*node, value *change) {
	owner := state[value.owner]
	if owner == nil {
		return
	}
	switch value.kind {
	case "entry":
		if value.target == 0 {
			delete(owner.entries, value.name)
		} else {
			owner.entries[value.name] = value.target
		}
	case "mode":
		owner.mode = owner.mode.Type() | value.mode.Perm()
	case "write":
		end := int(value.offset) + len(value.data)
		if end > len(owner.data) {
			owner.data = append(owner.data, make([]byte, end-len(owner.data))...)
		}
		copy(owner.data[int(value.offset):], value.data)
	}
}

func cloneNode(source *node) *node {
	result := &node{mode: source.mode, data: append([]byte(nil), source.data...)}
	if source.entries != nil {
		result.entries = make(map[string]uint64, len(source.entries))
		for name, id := range source.entries {
			result.entries[name] = id
		}
	}
	return result
}

func clone(source map[uint64]*node) map[uint64]*node {
	result := make(map[uint64]*node, len(source))
	for id, value := range source {
		result[id] = cloneNode(value)
	}
	return result
}

func materialize(path string, id uint64, state map[uint64]*node, links map[uint64]string, dirs map[uint64][]string) error {
	value := state[id]
	if value.mode.IsDir() {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return err
		}
		dirs[id] = append(dirs[id], path)
		names := make([]string, 0, len(value.entries))
		for name := range value.entries {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if err := materialize(filepath.Join(path, name), value.entries[name], state, links, dirs); err != nil {
				return err
			}
		}
	} else if previous, exists := links[id]; exists {
		return os.Link(previous, path)
	} else {
		if err := os.WriteFile(path, value.data, 0o600); err != nil {
			return err
		}
		links[id] = path
	}
	return os.Chmod(path, value.mode.Perm())
}

var _ storage.FileSystem = (*Model)(nil)
var _ io.WriteCloser = (*modelFile)(nil)
