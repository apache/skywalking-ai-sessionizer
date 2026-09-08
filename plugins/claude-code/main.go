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

// Command asz-claude-plugin is the asz Claude Code plugin: it records
// which files each tool call changed.
//
// It runs as a Claude Code hook, one short process per event, and keeps
// its state on disk under the plugin's data directory. It needs nothing
// from asz: no server, no endpoint, no acknowledgement. Its only output is
// one JSON line per observed call, appended to a file per stream of a
// session, which the asz claude-code-changes adapter tails when asz runs.
//
// A shell command is observed with a scan of the workspace before and
// after it, unless the command is classified read-only. An editing tool
// inside a subagent is observed from the hook's own response, which
// carries the runtime's patch. The main stream's editing tools are not
// observed at all: the runtime records their patches in its transcript,
// and asz reads them from there.
//
// A hook must never block a tool: every path here ends in exit status 0,
// and a failure goes to the plugin's log, never to the hook's output.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/pkg/changes"
	"github.com/apache/skywalking-ai-sessionizer/plugins/claude-code/internal/capture"
	"github.com/apache/skywalking-ai-sessionizer/plugins/claude-code/internal/edits"
	"github.com/apache/skywalking-ai-sessionizer/plugins/claude-code/internal/hook"
	"github.com/apache/skywalking-ai-sessionizer/plugins/claude-code/internal/output"
	"github.com/apache/skywalking-ai-sessionizer/plugins/claude-code/internal/readonly"
	"github.com/apache/skywalking-ai-sessionizer/plugins/claude-code/internal/scan"
	"github.com/apache/skywalking-ai-sessionizer/plugins/claude-code/internal/scope"
	"github.com/apache/skywalking-ai-sessionizer/plugins/claude-code/internal/settings"
)

// version is set at build time from the tag or the commit.
var version = "dev"

const usage = `asz-claude-plugin - the asz Claude Code plugin: which files each tool call changed

Usage:
  asz-claude-plugin hook      run as a Claude Code hook; reads the event from standard input
  asz-claude-plugin status    print the settings in force and the exclusion rules they expand to
  asz-claude-plugin prune     drop idle snapshot bytes and expired output now
  asz-claude-plugin version   print the version

The plugin's data directory is CLAUDE_PLUGIN_DATA, which Claude Code sets for
every hook. status and prune take it from the environment too.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "hook":
		os.Exit(runHook(os.Stdin, os.Getenv("CLAUDE_PLUGIN_DATA"), os.Getenv("CLAUDE_PROJECT_DIR"), time.Now()))
	case "version":
		fmt.Printf("asz-claude-plugin %s (%s %s/%s)\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	case "status":
		if err := status(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "prune":
		if err := pruneNow(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

// plugin is one hook invocation's state.
type plugin struct {
	dataDir    string
	projectDir string
	st         *settings.Settings
	rules      *scope.Rules
	in         *hook.Input
	// now is when the hook started, and clock is that moment plus what has
	// elapsed since, so a scan's times and the retention rules read one
	// clock: the real one under Claude Code, a fixed one under a test.
	now   time.Time
	clock func() time.Time
	log   *os.File
}

func (p *plugin) logf(format string, a ...any) {
	if p.log == nil {
		return
	}
	fmt.Fprintf(p.log, "%s %s", p.now.UTC().Format(time.RFC3339), fmt.Sprintf(format, a...))
	fmt.Fprintln(p.log)
}

// runHook handles one event, read from stdin, with the data directory and
// the project directory Claude Code sets in the environment. It returns 0
// whatever happened: a hook that fails must not stop the tool.
func runHook(stdin io.Reader, dataDir, projectDir string, now time.Time) (code int) {
	start := time.Now()
	p := &plugin{now: now, dataDir: dataDir, projectDir: projectDir}
	p.clock = func() time.Time { return now.Add(time.Since(start)) }
	defer func() {
		if r := recover(); r != nil {
			p.logf("panic: %v", r)
		}
		if p.log != nil {
			_ = p.log.Close()
		}
		code = 0
	}()
	if p.dataDir == "" {
		return 0 // not run by Claude Code as a plugin; nothing to do
	}
	_ = os.MkdirAll(filepath.Join(p.dataDir, "log"), 0o755)
	p.log, _ = os.OpenFile(filepath.Join(p.dataDir, "log", "plugin.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)

	in, err := hook.Read(stdin)
	if err != nil {
		p.logf("%v", err)
		return 0
	}
	p.in = in
	if p.st, err = settings.Load(p.dataDir); err != nil {
		p.logf("%v; using the defaults", err)
		p.st = settings.Default()
	}
	if p.rules, err = scope.Compile(p.st.Exclude.Defaults, p.st.Exclude.Add, p.st.Exclude.Remove); err != nil {
		p.logf("%v; using %s", err, scope.StandardV1)
		p.rules, _ = scope.Compile(scope.StandardV1, nil, nil)
	}

	switch in.Event {
	case hook.SessionStart, hook.SessionEnd:
		p.maintain()
	case hook.PreToolUse:
		if p.st.IsScopeTool(in.ToolName) {
			p.maintain()
			for _, root := range p.roots() {
				if err := p.before(root); err != nil {
					p.logf("%s %s before: %v", in.ToolName, in.ToolUseID, err)
				}
			}
		}
	case hook.PostToolUse, hook.PostToolUseFailure:
		switch {
		case p.st.IsScopeTool(in.ToolName):
			for _, root := range p.roots() {
				if err := p.after(root); err != nil {
					p.logf("%s %s after: %v", in.ToolName, in.ToolUseID, err)
				}
			}
		case edits.IsEditingTool(in.ToolName) && in.Event == hook.PostToolUse:
			// Inside a subagent the runtime records no patch, so the plugin
			// does, from the hook's response. On the main stream it records
			// its own. Either way the plugin's manifest must learn what the
			// tool wrote, or the next scan would find the edit as a change
			// nobody made.
			if in.AgentID != "" {
				if err := p.edit(); err != nil {
					p.logf("%s %s: %v", in.ToolName, in.ToolUseID, err)
				}
			}
			if file := p.editedFile(); file != "" {
				for _, root := range p.roots() {
					if err := p.touch(root, file); err != nil {
						p.logf("%s %s touch: %v", in.ToolName, in.ToolUseID, err)
					}
				}
			}
		}
	}
	return 0
}

// roots are the workspaces observed: the settings' roots, or the session's
// project directory, which Claude Code hands every hook.
func (p *plugin) roots() []string {
	if len(p.st.Roots) > 0 {
		return p.st.Roots
	}
	if p.projectDir != "" {
		return []string{p.projectDir}
	}
	if p.in.Cwd != "" {
		return []string{p.in.Cwd}
	}
	return nil
}

// captureID names the record of one call on one root.
func (p *plugin) captureID(r *scan.Root) string {
	id := "plugin/" + p.in.ToolUseID
	if len(p.roots()) > 1 {
		id += "/" + r.ID
	}
	return id
}

func (p *plugin) policy() *changes.Policy {
	pol := &changes.Policy{Exclusions: p.rules.Set, Expanded: p.rules.Expanded}
	if p.st.ReadOnly.On() {
		pol.ReadOnly = readonly.ReadonlyV1
	}
	return pol
}

func (p *plugin) scanOptions() scan.Options {
	return scan.Options{Rules: p.rules, Timeout: p.st.ScanTimeout, SizeCap: p.st.SizeCap, Now: p.clock}
}

// before opens a window: a scan of the root, unless the command is
// read-only, and a note for the AFTER hook.
func (p *plugin) before(root string) error {
	r, err := scan.Open(p.dataDir, root)
	if err != nil {
		return err
	}
	lock, err := r.Lock()
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	reg, err := capture.LoadRegistry(r)
	if err != nil {
		return err
	}
	p.closeStale(r, reg)

	in := p.in
	pending := &scan.Pending{
		Capture: p.captureID(r), Session: in.SessionID, Stream: in.Stream(),
		Tool: in.ToolUseID, ToolName: in.ToolName, At: p.now.UTC().Format(time.RFC3339Nano),
	}
	if p.st.ReadOnly.On() && readonly.IsReadOnly(in.Command()) {
		pending.Skipped = true
		return r.PutPending(pending)
	}
	head, err := r.Head()
	if err != nil {
		return err
	}
	m, step, err := r.Scan(p.scanOptions())
	switch {
	case errors.Is(err, scan.ErrTimeout):
		pending.Partial = true
		p.logf("%s: the before scan stopped at its time cap", root)
	case err != nil:
		pending.Before, pending.Partial = head.N, true
		p.logf("%s: before scan: %v", root, err)
		return r.PutPending(pending)
	}
	// What changed since the last scan with no window open is nobody's:
	// a person, an editor, an unhooked tool. It is recorded as such rather
	// than folded into the next window.
	if len(step.Changes) > 0 && len(reg.OpenWindows()) == 0 && head.N > 0 {
		if err := output.Append(p.dataDir, capture.BuildGap(r, step, in.SessionID, in.Stream(), p.policy(), p.st.SizeCap)); err != nil {
			p.logf("%s: gap record: %v", root, err)
		}
	}
	pending.Before = m.N
	reg.Open(capture.Window{
		Capture: pending.Capture, Session: pending.Session, Stream: pending.Stream,
		Tool: pending.Tool, ToolName: pending.ToolName, Before: m.N, OpenedAt: m.To,
	})
	reg.Prune(p.st.Retention.Idle, p.now)
	if err := reg.Save(r); err != nil {
		return err
	}
	return r.PutPending(pending)
}

// after closes the window with a second scan and writes the record.
func (p *plugin) after(root string) error {
	r, err := scan.Open(p.dataDir, root)
	if err != nil {
		return err
	}
	lock, err := r.Lock()
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	pending, err := r.TakePending(p.in.ToolUseID)
	if err != nil {
		if os.IsNotExist(err) {
			return errors.New("no before note; the call was not observed")
		}
		return err
	}
	in := p.in
	outcome := &changes.Outcome{State: changes.OutcomeReturned}
	if in.Event == hook.PostToolUseFailure {
		outcome.State = changes.OutcomeFailed
		if code, ok := in.ExitCode(); ok {
			outcome.ExitCode = changes.Int(code)
		}
	}
	if pending.Skipped {
		rec := &changes.Record{
			Schema: changes.Schema, ID: pending.Capture, Session: pending.Session, Stream: pending.Stream,
			Tool: pending.Tool, ToolName: pending.ToolName, Time: p.now.UTC().Format(time.RFC3339Nano),
			Basis: changes.BasisSkippedReadOnly, Root: &changes.Root{Path: r.Path, ID: r.ID},
			Policy: p.policy(), Outcome: outcome, Changes: []changes.FileChange{},
		}
		return output.Append(p.dataDir, rec)
	}
	reg, err := capture.LoadRegistry(r)
	if err != nil {
		return err
	}
	m, _, err := r.Scan(p.scanOptions())
	partial := pending.Partial
	switch {
	case errors.Is(err, scan.ErrTimeout):
		partial = true
		p.logf("%s: the after scan stopped at its time cap", root)
	case err != nil:
		return err
	}
	reg.Close(pending.Capture, m.N, m.To, false)
	w := reg.Get(pending.Capture)
	if w == nil {
		// The window was closed as stale by another hook, or the registry
		// was pruned; the comparison is still made from the note.
		w = &capture.Window{Capture: pending.Capture, Session: pending.Session, Stream: pending.Stream,
			Tool: pending.Tool, ToolName: pending.ToolName, Before: pending.Before, After: m.N, OpenedAt: pending.At, ClosedAt: m.To}
	}
	rec, err := capture.Build(r, reg, w, capture.Context{
		Session: pending.Session, Stream: pending.Stream, Tool: pending.Tool, ToolName: pending.ToolName,
		Policy: p.policy(), Outcome: outcome,
	}, p.st.SizeCap)
	if err != nil {
		return err
	}
	if partial && rec.Coverage != changes.CoveragePartial {
		rec.Coverage = changes.CoveragePartial
		rec.Gaps = append(rec.Gaps, "a scan stopped at its time cap; paths it did not reach are not observed")
	}
	reg.Prune(p.st.Retention.Idle, p.now)
	if err := reg.Save(r); err != nil {
		return err
	}
	return output.Append(p.dataDir, rec)
}

// editedFile is the file an editing tool wrote, from its response or,
// failing that, its input.
func (p *plugin) editedFile() string {
	var resp struct {
		FilePath     string `json:"filePath"`
		NotebookPath string `json:"notebook_path"`
	}
	if len(p.in.ToolResponse) > 0 && json.Unmarshal(p.in.ToolResponse, &resp) == nil {
		if resp.FilePath != "" {
			return resp.FilePath
		}
		if resp.NotebookPath != "" {
			return resp.NotebookPath
		}
	}
	var input struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
	}
	if len(p.in.ToolInput) > 0 && json.Unmarshal(p.in.ToolInput, &input) == nil {
		if input.FilePath != "" {
			return input.FilePath
		}
		return input.NotebookPath
	}
	return ""
}

// touch brings the root's manifest up to date with one file an editing
// tool wrote, and registers the edit as a closed window of its own, so a
// window open at the same time sees the change as shared with the edit
// rather than as its own.
func (p *plugin) touch(root, file string) error {
	r, err := scan.Open(p.dataDir, root)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(r.Path, file)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil // outside the observed root
	}
	lock, err := r.Lock()
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	step, err := r.Touch(filepath.ToSlash(rel), p.scanOptions())
	if err != nil || step == nil {
		return err
	}
	reg, err := capture.LoadRegistry(r)
	if err != nil {
		return err
	}
	in := p.in
	id := "plugin/" + in.ToolUseID
	reg.Open(capture.Window{
		Capture: id, Session: in.SessionID, Stream: in.Stream(), Tool: in.ToolUseID, ToolName: in.ToolName,
		Before: step.N - 1, OpenedAt: step.From,
	})
	reg.Close(id, step.N, step.To, false)
	reg.Prune(p.st.Retention.Idle, p.now)
	return reg.Save(r)
}

// closeStale closes windows whose AFTER hook never came, after the idle
// time: a denied or cancelled call, or a hook that was cut off.
func (p *plugin) closeStale(r *scan.Root, reg *capture.Registry) {
	stale, err := r.StalePending(p.st.Retention.Idle, p.now)
	if err != nil {
		return
	}
	head, err := r.Head()
	if err != nil {
		return
	}
	for _, s := range stale {
		if _, err := r.TakePending(s.Tool); err == nil {
			reg.Close(s.Capture, head.N, p.now.UTC().Format(time.RFC3339Nano), true)
		}
	}
}

// edit records an editing tool's change inside a subagent from the hook's
// own response.
func (p *plugin) edit() error {
	in := p.in
	root := ""
	if roots := p.roots(); len(roots) > 0 {
		root = roots[0]
	}
	rec, ok := edits.Record(edits.Context{
		ID: "plugin/" + in.ToolUseID, Session: in.SessionID, Stream: in.Stream(), Tool: in.ToolUseID,
		ToolName: in.ToolName, Time: p.now.UTC().Format(time.RFC3339Nano), Root: root,
	}, in.ToolResponse)
	if !ok {
		return nil // nothing the response could say; a failed edit
	}
	return output.Append(p.dataDir, rec)
}

// maintain applies the two retention rules: output files past their TTL
// are removed, and a root idle past the limit with nothing open drops its
// kept bytes and its steps, keeping its manifest.
func (p *plugin) maintain() {
	if _, err := output.Prune(p.dataDir, p.st.Retention.TTL, p.now); err != nil {
		p.logf("prune output: %v", err)
	}
	dirs, err := scan.Roots(p.dataDir)
	if err != nil {
		p.logf("list roots: %v", err)
		return
	}
	for _, dir := range dirs {
		r := scan.OpenDir(dir)
		lock, err := r.Lock()
		if err != nil {
			continue
		}
		reg, err := capture.LoadRegistry(r)
		if err == nil {
			p.closeStale(r, reg)
			last := reg.LastActivity()
			if len(reg.OpenWindows()) == 0 && !last.IsZero() && p.now.Sub(last) > p.st.Retention.Idle {
				if err := r.DropBytes(); err != nil {
					p.logf("%s: drop bytes: %v", dir, err)
				}
				reg.Windows = nil
			}
			reg.Prune(p.st.Retention.Idle, p.now)
			_ = reg.Save(r)
		}
		_ = lock.Unlock()
	}
}

// status prints what the plugin would run under.
func status() error {
	dataDir := os.Getenv("CLAUDE_PLUGIN_DATA")
	if dataDir == "" {
		return errors.New("CLAUDE_PLUGIN_DATA is not set; run under Claude Code, or set it to the plugin's data directory")
	}
	st, err := settings.Load(dataDir)
	if err != nil {
		return err
	}
	rules, err := scope.Compile(st.Exclude.Defaults, st.Exclude.Add, st.Exclude.Remove)
	if err != nil {
		return err
	}
	fmt.Printf("data directory : %s\n", dataDir)
	fmt.Printf("roots          : %s\n", orDefault(strings.Join(st.Roots, ", "), "the session's project directory"))
	fmt.Printf("scope tools    : %s\n", strings.Join(st.Tools.Scope, ", "))
	fmt.Printf("read-only skip : %v (%s)\n", st.ReadOnly.On(), readonly.ReadonlyV1)
	fmt.Printf("retention      : snapshot bytes %s idle, output %s\n", st.Retention.Idle, st.Retention.TTL)
	fmt.Printf("scan cap       : %s; file size cap %d bytes\n", st.ScanTimeout, st.SizeCap)
	fmt.Printf("exclusions     : %s, %d rules\n", rules.Set, len(rules.Expanded))
	for _, r := range rules.Expanded {
		fmt.Printf("  %s\n", r)
	}
	return nil
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// pruneNow runs the retention rules from the command line.
func pruneNow() error {
	dataDir := os.Getenv("CLAUDE_PLUGIN_DATA")
	if dataDir == "" {
		return errors.New("CLAUDE_PLUGIN_DATA is not set")
	}
	st, err := settings.Load(dataDir)
	if err != nil {
		return err
	}
	p := &plugin{dataDir: dataDir, st: st, now: time.Now(), clock: time.Now}
	p.maintain()
	return nil
}
