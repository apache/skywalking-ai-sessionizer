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

// Package capture turns two scans of a root into a change record.
//
// A window opens at a tool's BEFORE hook with one scan and closes at its
// AFTER hook with another. The root's chain of steps says what changed in
// between and, because every window on the root is registered, which
// other windows span each step. That is what attribution rests on: a file
// changed in a step only this window spans is this window's alone; one
// changed in a step another window spans too is shared, and both records
// name each other. Nothing here claims who wrote a byte, only which tool
// windows could have.
package capture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/pkg/changes"
	"github.com/apache/skywalking-ai-sessionizer/plugins/claude-code/internal/scan"
)

// Window is one tool call's observation on a root.
type Window struct {
	Capture  string `json:"capture"`
	Session  string `json:"session"`
	Stream   string `json:"stream"`
	Tool     string `json:"tool"`
	ToolName string `json:"tool_name"`
	Before   int    `json:"before"` // the manifest the BEFORE scan produced
	After    int    `json:"after"`  // the AFTER scan's, 0 while open
	OpenedAt string `json:"opened_at"`
	ClosedAt string `json:"closed_at,omitempty"`
	// Unfinished says the window was closed without an AFTER hook.
	Unfinished bool `json:"unfinished,omitempty"`
}

// spans says whether the window covers step n.
func (w *Window) spans(n int) bool {
	if n <= w.Before {
		return false
	}
	return w.After == 0 || n <= w.After
}

// Registry is every window a root has seen recently.
type Registry struct {
	Windows []Window `json:"windows"`
}

func registryPath(r *scan.Root) string { return filepath.Join(r.Dir(), "windows.json") }

// LoadRegistry reads a root's windows.
func LoadRegistry(r *scan.Root) (*Registry, error) {
	var reg Registry
	data, err := os.ReadFile(registryPath(r))
	if err != nil {
		if os.IsNotExist(err) {
			return &reg, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("capture: windows: %w", err)
	}
	return &reg, nil
}

// Save writes the windows back.
func (reg *Registry) Save(r *scan.Root) error {
	data, err := json.Marshal(reg)
	if err != nil {
		return err
	}
	tmp := registryPath(r) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, registryPath(r))
}

func (reg *Registry) find(capture string) *Window {
	for i := range reg.Windows {
		if reg.Windows[i].Capture == capture {
			return &reg.Windows[i]
		}
	}
	return nil
}

// Get returns a window by its capture id, or nil.
func (reg *Registry) Get(capture string) *Window { return reg.find(capture) }

// Open records a window at its BEFORE scan.
func (reg *Registry) Open(w Window) { reg.Windows = append(reg.Windows, w) }

// Close records a window's AFTER scan.
func (reg *Registry) Close(capture string, after int, at string, unfinished bool) {
	if w := reg.find(capture); w != nil {
		w.After, w.ClosedAt, w.Unfinished = after, at, unfinished
	}
}

// OpenWindows lists the windows without an AFTER scan.
func (reg *Registry) OpenWindows() []Window {
	var out []Window
	for _, w := range reg.Windows {
		if w.After == 0 {
			out = append(out, w)
		}
	}
	return out
}

// Prune drops closed windows whose end is older than age, keeping the
// registry small; an open window is kept whatever its age.
func (reg *Registry) Prune(age time.Duration, now time.Time) {
	var keep []Window
	for _, w := range reg.Windows {
		if w.After == 0 {
			keep = append(keep, w)
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, w.ClosedAt); err == nil && now.Sub(t) <= age {
			keep = append(keep, w)
		}
	}
	reg.Windows = keep
}

// LastActivity is the latest time any window opened or closed.
func (reg *Registry) LastActivity() time.Time {
	var last time.Time
	for _, w := range reg.Windows {
		for _, s := range []string{w.OpenedAt, w.ClosedAt} {
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil && t.After(last) {
				last = t
			}
		}
	}
	return last
}

// Context is what a record says about where it came from.
type Context struct {
	Session  string
	Stream   string
	Tool     string
	ToolName string
	Policy   *changes.Policy
	Outcome  *changes.Outcome
}

// Build makes the record for a window from the root's steps: every path
// whose hash changed between the window's two scans, with the hunks from
// the kept bytes, its attribution, and the other windows that overlapped.
func Build(r *scan.Root, reg *Registry, w *Window, ctx Context, sizeCap int64) (*changes.Record, error) {
	// The BEFORE scan's own step is read too, for when that scan ran; the
	// changes counted are those of the steps after it.
	steps, err := r.Steps(w.Before-1, w.After)
	if err != nil {
		return nil, err
	}
	// The net change of every path over the window, and the steps in
	// which it changed.
	type net struct {
		before, after string
		steps         []int
	}
	nets := map[string]*net{}
	var order []string
	partial := false
	for _, s := range steps {
		if s.N <= w.Before {
			continue
		}
		if s.Partial {
			partial = true
		}
		for p, c := range s.Changes {
			n, ok := nets[p]
			if !ok {
				n = &net{before: c.Before}
				nets[p] = n
				order = append(order, p)
			}
			n.after = c.After
			n.steps = append(n.steps, s.N)
		}
	}
	sort.Strings(order)

	rec := &changes.Record{
		Schema: changes.Schema, ID: w.Capture, CapturedBy: changes.CapturedByASZPlugin, Session: ctx.Session, Stream: ctx.Stream,
		Tool: ctx.Tool, ToolName: ctx.ToolName, Time: w.ClosedAt, Basis: changes.BasisToolWindow,
		Root:    &changes.Root{Path: r.Path, ID: r.ID},
		Policy:  ctx.Policy,
		Outcome: ctx.Outcome,
		Window: &changes.Window{
			Before: interval(steps, w.Before), After: interval(steps, w.After),
		},
		Coverage: changes.CoverageComplete,
	}
	if partial {
		rec.Coverage = changes.CoveragePartial
		rec.Gaps = append(rec.Gaps, "a scan stopped at its time cap; paths it did not reach are not observed")
	}
	for _, other := range reg.Windows {
		if other.Capture == w.Capture || !overlaps(&other, w) {
			continue
		}
		state := "closed"
		if other.After == 0 {
			state = "open"
		}
		rec.Overlaps = append(rec.Overlaps, changes.Overlap{
			Capture: other.Capture, Session: other.Session, Stream: other.Stream,
			Tool: other.Tool, ToolName: other.ToolName, State: state,
		})
	}
	for _, p := range order {
		n := nets[p]
		if n.before == n.after {
			continue // changed and changed back within the window
		}
		fc := fileChange(r, p, n.before, n.after, sizeCap)
		fc.Windows = []string{w.Capture}
		fc.Attribution = changes.AttributionOnlyThisWindow
		for _, other := range reg.Windows {
			if other.Capture == w.Capture {
				continue
			}
			for _, s := range n.steps {
				if other.spans(s) {
					fc.Windows = append(fc.Windows, other.Capture)
					fc.Attribution = changes.AttributionShared
					break
				}
			}
		}
		rec.Changes = append(rec.Changes, fc)
	}
	rec.ChangedFiles = changes.Int(len(rec.Changes))
	if rec.Changes == nil {
		rec.Changes = []changes.FileChange{}
	}
	return rec, nil
}

// BuildGap makes the unattributed record for a step no window spanned:
// what changed between the previous scan and this one, by something no
// tool window covered.
func BuildGap(r *scan.Root, step *scan.Step, session, stream string, policy *changes.Policy, sizeCap int64) *changes.Record {
	rec := &changes.Record{
		Schema: changes.Schema, ID: fmt.Sprintf("gap/%s/%d", r.ID, step.N), CapturedBy: changes.CapturedByASZPlugin, Session: session, Stream: stream,
		Time: step.To, Basis: changes.BasisUnattributed,
		Root: &changes.Root{Path: r.Path, ID: r.ID}, Policy: policy,
		Window:   &changes.Window{Before: changes.Interval{From: step.From, To: step.From}, After: changes.Interval{From: step.From, To: step.To}},
		Coverage: changes.CoverageComplete,
	}
	if step.Partial {
		rec.Coverage = changes.CoveragePartial
	}
	var paths []string
	for p := range step.Changes {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		c := step.Changes[p]
		fc := fileChange(r, p, c.Before, c.After, sizeCap)
		fc.Attribution = changes.AttributionOutsideAnyWindow
		rec.Changes = append(rec.Changes, fc)
	}
	rec.ChangedFiles = changes.Int(len(rec.Changes))
	if rec.Changes == nil {
		rec.Changes = []changes.FileChange{}
	}
	return rec
}

// overlaps says whether two windows were open on the root at once.
func overlaps(a, b *Window) bool {
	aEnd, bEnd := a.After, b.After
	if aEnd == 0 {
		aEnd = int(^uint(0) >> 1)
	}
	if bEnd == 0 {
		bEnd = int(^uint(0) >> 1)
	}
	return a.Before < bEnd && b.Before < aEnd
}

// interval is when the scan that produced manifest n ran, from its step.
func interval(steps []*scan.Step, n int) changes.Interval {
	for _, s := range steps {
		if s.N == n {
			return changes.Interval{From: s.From, To: s.To}
		}
	}
	return changes.Interval{}
}

// fileChange describes one path's change from the kept bytes.
func fileChange(r *scan.Root, path, before, after string, sizeCap int64) changes.FileChange {
	bb, bok := r.Content(before)
	ab, aok := r.Content(after)
	fc := changes.FileChange{Path: path, Diff: changes.DiffAvailable}
	switch {
	case before == "":
		fc.Operation = changes.OpCreate
	case after == "":
		fc.Operation = changes.OpDelete
	default:
		fc.Operation = changes.OpModify
	}
	fc.Before = endpoint(bb, bok, before)
	fc.After = endpoint(ab, aok, after)
	switch {
	case (before != "" && !bok) || (after != "" && !aok):
		// Bytes over the cap are never kept; bytes pruned while idle are
		// gone. Either way the hash is known and the text is not.
		if (before != "" && fc.Before.Bytes != nil && *fc.Before.Bytes > sizeCap) || (after != "" && fc.After.Bytes != nil && *fc.After.Bytes > sizeCap) {
			fc.Diff = changes.DiffTooLarge
		} else {
			fc.Diff = changes.DiffUnavailable
		}
	case !changes.IsText(bb) || !changes.IsText(ab):
		fc.Diff = changes.DiffBinary
	default:
		hunks, add, del, ok := changes.Diff(changes.SplitLines(bb), changes.SplitLines(ab))
		if !ok {
			fc.Diff = changes.DiffTooLarge
			break
		}
		fc.Hunks, fc.Additions, fc.Deletions = hunks, changes.Int(add), changes.Int(del)
	}
	return fc
}

// endpoint describes a side from its kept bytes, or from its hash alone
// when the bytes were not kept.
func endpoint(b []byte, kept bool, hash string) changes.Endpoint {
	if hash == "" {
		return changes.Endpoint{Present: false}
	}
	if kept {
		return changes.EndpointOf(b, true)
	}
	return changes.Endpoint{Present: true, SHA256: hash}
}
