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

// Package scan records the state of a workspace root: which files are
// there and what each holds, as a manifest of hashes with the bytes of
// every file kept beside it.
//
// A scan walks the root under the exclusion rules and hashes what it
// finds. A stat cache makes the second scan cheap: a file whose size,
// modification time and inode are as the last scan saw them keeps its
// hash, unless its modification time is within the last scan's own
// second, when it is hashed again because the clock cannot tell. The
// bytes of every new hash are copied into a content store, which is what
// a diff is later made from, since a tool may have rewritten the file in
// place by then.
//
// Every scan appends a step to the root's chain: which paths changed and
// from what hash to what. A window is the span of steps between its two
// scans, and the steps are what say which windows could have made a
// change. State lives under the root's directory in the plugin's data
// directory, and a lock file serialises the scans of one root, since two
// hooks may run at once.
package scan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Rules is what a scan asks of the exclusion policy.
type Rules interface {
	Prune(rel, dir string) bool
}

// Entry is one file as a scan saw it.
type Entry struct {
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"` // nanoseconds
	Inode uint64 `json:"inode"`
	Hash  string `json:"hash"`
}

// Manifest is the state of a root after one scan.
type Manifest struct {
	N     int              `json:"n"`
	From  string           `json:"from"` // RFC 3339, when the scan started
	To    string           `json:"to"`   // when it ended
	Files map[string]Entry `json:"files"`
}

// Step is what changed between two consecutive scans.
type Step struct {
	N       int                   `json:"n"`
	From    string                `json:"from"`
	To      string                `json:"to"`
	Changes map[string]PathChange `json:"changes"`
	// Partial says the scan stopped at its time cap; the changes above
	// are what it saw before stopping.
	Partial bool `json:"partial,omitempty"`
}

// PathChange is one path's hash before and after a step; an empty hash
// means the file was absent.
type PathChange struct {
	Before string `json:"before"`
	After  string `json:"after"`
}

// Root is one workspace root's state directory.
type Root struct {
	Path string // the workspace, absolute and clean
	ID   string // the directory name under roots/
	dir  string // the state directory
}

// ErrTimeout says a scan stopped at its time cap.
var ErrTimeout = errors.New("scan: stopped at the time cap")

// Open names a root's state directory under dataDir, creating it.
func Open(dataDir, workspace string) (*Root, error) {
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	abs = filepath.Clean(abs)
	sum := sha256.Sum256([]byte(filepath.ToSlash(abs)))
	id := hex.EncodeToString(sum[:8])
	dir := filepath.Join(dataDir, "roots", id)
	for _, sub := range []string{"steps", "content", "pending"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	return &Root{Path: abs, ID: id, dir: dir}, nil
}

// Dir is the root's state directory.
func (r *Root) Dir() string { return r.dir }

// Lock takes the root's lock, which serialises scans and window updates.
func (r *Root) Lock() (*Lock, error) { return lockFile(filepath.Join(r.dir, "lock")) }

// Head reads the current manifest, or an empty one at N 0.
func (r *Root) Head() (*Manifest, error) {
	var m Manifest
	data, err := os.ReadFile(filepath.Join(r.dir, "manifest.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return &Manifest{Files: map[string]Entry{}}, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("scan: manifest: %w", err)
	}
	if m.Files == nil {
		m.Files = map[string]Entry{}
	}
	return &m, nil
}

// Options bound a scan.
type Options struct {
	Rules   Rules
	Timeout time.Duration
	// SizeCap is the largest file whose bytes are kept. A larger file is
	// still hashed, so its change is seen, but no diff can be made.
	SizeCap int64
	Now     func() time.Time
}

// Scan walks the root, commits the new manifest and its step, and returns
// both. The caller holds the lock. On a timeout the step is committed as
// partial, with what was seen, and ErrTimeout is returned beside it.
func (r *Root) Scan(opt Options) (*Manifest, *Step, error) {
	now := opt.Now
	if now == nil {
		now = time.Now
	}
	prev, err := r.Head()
	if err != nil {
		return nil, nil, err
	}
	start := now()
	var deadline time.Time
	if opt.Timeout > 0 {
		deadline = start.Add(opt.Timeout)
	}
	// A file changed within the last scan's own second may have been hashed
	// before the change landed; the clock cannot tell, so it is read again.
	var racy int64
	if prev.To != "" {
		if t, err := time.Parse(time.RFC3339Nano, prev.To); err == nil {
			racy = t.Add(-time.Second).UnixNano()
		}
	}

	next := &Manifest{N: prev.N + 1, From: start.UTC().Format(time.RFC3339Nano), Files: map[string]Entry{}}
	timedOut := false
	walkErr := filepath.WalkDir(r.Path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !deadline.IsZero() && now().After(deadline) {
			timedOut = true
			return fs.SkipAll
		}
		rel, rerr := filepath.Rel(r.Path, p)
		if rerr != nil || rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if opt.Rules != nil && opt.Rules.Prune(rel, p) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // links and specials are neither followed nor recorded
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		e := Entry{Size: info.Size(), MTime: info.ModTime().UnixNano(), Inode: inodeOf(info)}
		if old, ok := prev.Files[rel]; ok && old.Size == e.Size && old.MTime == e.MTime && old.Inode == e.Inode && e.MTime < racy {
			e.Hash = old.Hash
		} else {
			h, herr := r.hashAndKeep(p, e.Size, opt.SizeCap)
			if herr != nil {
				return nil // unreadable now; absent from this manifest, seen again next time
			}
			e.Hash = h
		}
		next.Files[rel] = e
		return nil
	})
	if walkErr != nil {
		return nil, nil, walkErr
	}
	next.To = now().UTC().Format(time.RFC3339Nano)

	step := &Step{N: next.N, From: next.From, To: next.To, Changes: map[string]PathChange{}, Partial: timedOut}
	for p, e := range next.Files {
		if old, ok := prev.Files[p]; !ok || old.Hash != e.Hash {
			before := ""
			if ok {
				before = old.Hash
			}
			step.Changes[p] = PathChange{Before: before, After: e.Hash}
		}
	}
	if !timedOut {
		// A path the walk did not reach is gone. On a timeout nothing can be
		// said about what was not reached, so no deletion is recorded.
		for p, old := range prev.Files {
			if _, ok := next.Files[p]; !ok {
				step.Changes[p] = PathChange{Before: old.Hash, After: ""}
			}
		}
	} else {
		// Keep what was not reached as it was, so the head stays whole.
		for p, old := range prev.Files {
			if _, ok := next.Files[p]; !ok {
				next.Files[p] = old
			}
		}
	}
	if err := writeJSON(filepath.Join(r.dir, "steps", fmt.Sprintf("%06d.json", step.N)), step); err != nil {
		return nil, nil, err
	}
	if err := writeJSON(filepath.Join(r.dir, "manifest.json"), next); err != nil {
		return nil, nil, err
	}
	if timedOut {
		return next, step, ErrTimeout
	}
	return next, step, nil
}

// Touch records one file's state right after an editing tool wrote it,
// as a step of its own, so the next scan does not find that edit as a
// change nobody made. The runtime records the edit itself; what the plugin
// needs is only that its manifest agrees with the file. It returns nil
// when there is nothing to record: no manifest yet, a path outside the
// observed scope, or a file whose bytes are as the manifest has them.
func (r *Root) Touch(rel string, opt Options) (*Step, error) {
	head, err := r.Head()
	if err != nil {
		return nil, err
	}
	if head.N == 0 || excluded(rel, r.Path, opt.Rules) {
		return nil, nil
	}
	now := opt.Now
	if now == nil {
		now = time.Now
	}
	abs := filepath.Join(r.Path, filepath.FromSlash(rel))
	old, had := head.Files[rel]
	var cur *Entry
	info, serr := os.Lstat(abs)
	switch {
	case serr == nil && info.Mode().IsRegular():
		h, herr := r.hashAndKeep(abs, info.Size(), opt.SizeCap)
		if herr != nil {
			return nil, herr
		}
		cur = &Entry{Size: info.Size(), MTime: info.ModTime().UnixNano(), Inode: inodeOf(info), Hash: h}
	case serr != nil && !os.IsNotExist(serr):
		return nil, serr
	}
	if (cur == nil && !had) || (cur != nil && had && cur.Hash == old.Hash) {
		return nil, nil
	}
	at := now().UTC().Format(time.RFC3339Nano)
	change := PathChange{}
	if had {
		change.Before = old.Hash
	}
	if cur != nil {
		change.After = cur.Hash
		head.Files[rel] = *cur
	} else {
		delete(head.Files, rel)
	}
	step := &Step{N: head.N + 1, From: at, To: at, Changes: map[string]PathChange{rel: change}}
	head.N, head.From, head.To = step.N, at, at
	if err := writeJSON(filepath.Join(r.dir, "steps", fmt.Sprintf("%06d.json", step.N)), step); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(r.dir, "manifest.json"), head); err != nil {
		return nil, err
	}
	return step, nil
}

// excluded says whether a path lies under a pruned directory.
func excluded(rel, root string, rules Rules) bool {
	if rules == nil {
		return false
	}
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		dir := strings.Join(parts[:i], "/")
		if rules.Prune(dir, filepath.Join(root, filepath.FromSlash(dir))) {
			return true
		}
	}
	return false
}

// hashAndKeep hashes a file and keeps its bytes under the content store
// when they are new and within the size cap.
func (r *Root) hashAndKeep(path string, size, sizeCap int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if size > sizeCap && sizeCap > 0 {
		if _, err := io.Copy(h, f); err != nil {
			return "", err
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	h.Write(data)
	sum := hex.EncodeToString(h.Sum(nil))
	dest := filepath.Join(r.dir, "content", sum)
	if _, err := os.Stat(dest); err == nil {
		return sum, nil
	}
	tmp := dest + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return "", err
	}
	return sum, nil
}

// Content returns the kept bytes of a hash, or ok false when they were
// never kept: an empty hash, a file over the cap, or bytes since pruned.
func (r *Root) Content(hash string) ([]byte, bool) {
	if hash == "" {
		return nil, false
	}
	data, err := os.ReadFile(filepath.Join(r.dir, "content", hash))
	if err != nil {
		return nil, false
	}
	return data, true
}

// Steps reads the steps after from and up to through, in order.
func (r *Root) Steps(from, through int) ([]*Step, error) {
	var out []*Step
	for n := from + 1; n <= through; n++ {
		var s Step
		data, err := os.ReadFile(filepath.Join(r.dir, "steps", fmt.Sprintf("%06d.json", n)))
		if err != nil {
			if os.IsNotExist(err) {
				continue // pruned while idle; nothing can be said about it
			}
			return nil, err
		}
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, err
		}
		out = append(out, &s)
	}
	return out, nil
}

// DropBytes removes the kept bytes and the steps, keeping the manifest:
// the next scan still knows what was there, as paths and hashes, and
// starts a fresh chain of steps.
func (r *Root) DropBytes() error {
	for _, sub := range []string{"content", "steps"} {
		dir := filepath.Join(r.dir, sub)
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// Pending is the plugin's note to itself between a tool's BEFORE hook and
// its AFTER hook.
type Pending struct {
	Capture  string `json:"capture"`
	Session  string `json:"session"`
	Stream   string `json:"stream"`
	Tool     string `json:"tool"`
	ToolName string `json:"tool_name"`
	At       string `json:"at"`
	// Skipped says the command was classified read-only and no scan ran.
	Skipped bool `json:"skipped,omitempty"`
	// Before is the manifest the BEFORE scan produced; Partial says it
	// stopped at the time cap.
	Before  int  `json:"before"`
	Partial bool `json:"partial,omitempty"`
}

// PutPending writes the note under the tool-use id.
func (r *Root) PutPending(p *Pending) error {
	return writeJSON(filepath.Join(r.dir, "pending", safeName(p.Tool)+".json"), p)
}

// TakePending reads and removes the note of a tool-use id.
func (r *Root) TakePending(tool string) (*Pending, error) {
	path := filepath.Join(r.dir, "pending", safeName(tool)+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Pending
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	_ = os.Remove(path)
	return &p, nil
}

// StalePending lists the notes older than age, which an AFTER hook never
// claimed: a denied or cancelled call, or a hook that timed out.
func (r *Root) StalePending(age time.Duration, now time.Time) ([]*Pending, error) {
	items, err := os.ReadDir(filepath.Join(r.dir, "pending"))
	if err != nil {
		return nil, err
	}
	var out []*Pending
	for _, it := range items {
		data, err := os.ReadFile(filepath.Join(r.dir, "pending", it.Name()))
		if err != nil {
			continue
		}
		var p Pending
		if json.Unmarshal(data, &p) != nil {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, p.At); err == nil && now.Sub(t) > age {
			out = append(out, &p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At < out[j].At })
	return out, nil
}

// Roots lists every root state directory under dataDir.
func Roots(dataDir string) ([]string, error) {
	items, err := os.ReadDir(filepath.Join(dataDir, "roots"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, it := range items {
		if it.IsDir() {
			out = append(out, filepath.Join(dataDir, "roots", it.Name()))
		}
	}
	return out, nil
}

// OpenDir opens a root's state directory without knowing its workspace,
// for maintenance.
func OpenDir(dir string) *Root { return &Root{ID: filepath.Base(dir), dir: dir} }

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '_'
	}, s)
}

// writeJSON writes a file whole: to a temporary name, then renamed into
// place, so a reader never sees half of it.
func writeJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
