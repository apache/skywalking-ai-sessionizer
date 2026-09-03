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

package claudecode

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// SourceKind identifies one of the six file kinds a session is spread over.
type SourceKind int

const (
	// SrcMainTranscript is <session-id>.jsonl - the parent execution stream.
	SrcMainTranscript SourceKind = iota
	// SrcAgentTranscript is a child execution stream, direct or workflow-spawned.
	SrcAgentTranscript
	// SrcAgentMeta is a child's sidecar. Written once at spawn and never updated.
	SrcAgentMeta
	// SrcJournal is a workflow run's journal. Append-only, and it leads the
	// filesystem: a started record can precede the child transcript's existence.
	SrcJournal
	// SrcWorkflowManifest is a workflow run manifest. Rewritten as the run
	// progresses, so it is tracked by content digest rather than by position.
	SrcWorkflowManifest
	// SrcWorkflowScript is a workflow run's script source. Claude Code files it
	// under whatever working directory the agent had at the time, so it is the
	// one artifact that genuinely lives outside its session's own directory.
	SrcWorkflowScript
)

// Append reports whether the source only ever grows.
func (k SourceKind) Append() bool {
	switch k {
	case SrcMainTranscript, SrcAgentTranscript, SrcJournal:
		return true
	}
	return false
}

// RecordKind maps a source kind to the landed envelope kind.
func (k SourceKind) RecordKind() sessiondata.Kind {
	switch k {
	case SrcAgentMeta:
		return sessiondata.KindAgentMeta
	case SrcJournal:
		return sessiondata.KindJournal
	case SrcWorkflowManifest:
		return sessiondata.KindWorkflowManifest
	case SrcWorkflowScript:
		return sessiondata.KindWorkflowScript
	default:
		return sessiondata.KindTranscript
	}
}

// Source is one collectable file belonging to a session.
type Source struct {
	Kind    SourceKind
	Path    string // absolute
	Rel     string // relative to the adapter source root; retained in the landed header
	Session string
	Stream  string // "main" or an agent id; empty for journal and manifest
	RunID   string // workflow run id, for journal and manifest
}

// Session is one Claude Code session and every file belonging to it.
//
// Dirs records every source directory the session appears under. A session's
// files are NOT confined to one: when a child agent runs in a different working
// directory its stream is filed under that directory's slug instead. In a real
// corpus 7 of 61 sessions spanned more than one, one of them six.
type Session struct {
	ID   string
	Dirs []string
	// Primary is the source directory holding this session's main transcript -
	// effectively the directory Claude Code was launched in. It is empty when
	// the main transcript has been pruned, which happens routinely.
	//
	// Filtering keys on Primary rather than on Dirs: a session's child streams
	// can be filed under unrelated directories (a scratchpad, a subdirectory),
	// and letting one of those disqualify the whole session would discard a real
	// conversation because of where a subagent happened to run.
	Primary string
	Sources []Source
}

// Discover enumerates sessions under root.
//
// Discovery is session-first: it scans every source directory for entries whose
// name is a session id - both .jsonl files and directories - and groups them by
// session id ACROSS directories. A directory-first walk misses most child
// streams for roughly one session in nine.
func Discover(root string) ([]Session, error) {
	sessions, warnings, err := DiscoverWithWarnings(root)
	if err != nil {
		return nil, err
	}
	// Warnings are non-fatal by construction: a partially readable corpus still
	// yields the sessions it can. Callers that need to report them use
	// DiscoverWithWarnings.
	if len(warnings) > 0 {
		return sessions, errors.Join(warnings...)
	}
	return sessions, nil
}

// DiscoverWithWarnings is Discover, separating unreadable directories from a
// fatal failure. A partially readable corpus still yields the sessions it can.
func DiscoverWithWarnings(root string) (sessions []Session, warnings []error, err error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, nil, err
	}

	byID := map[string]*Session{}
	// A directory is recorded only once it has actually yielded a source.
	// Claude Code writes workflow *scripts* under whatever cwd the agent had at
	// the time, producing session directories that contain nothing we collect;
	// recording those as session directories let an empty stub disqualify an
	// entire real conversation through an exclude pattern.
	get := func(id string) *Session {
		s, ok := byID[id]
		if !ok {
			s = &Session{ID: id}
			byID[id] = s
		}
		return s
	}
	add := func(id, dir string, srcs ...Source) {
		if len(srcs) == 0 {
			return
		}
		s := get(id)
		if !contains(s.Dirs, dir) {
			s.Dirs = append(s.Dirs, dir)
		}
		s.Sources = append(s.Sources, srcs...)
	}

	for _, projEntry := range entries {
		if !projEntry.IsDir() {
			continue
		}
		projName := projEntry.Name()
		projPath := filepath.Join(root, projName)
		items, err := os.ReadDir(projPath)
		if err != nil {
			// A directory that vanished mid-scan is normal - the corpus is
			// live. Anything else (permissions, I/O) hides sources, so it is
			// reported rather than silently producing a short result.
			if !os.IsNotExist(err) {
				warnings = append(warnings, fmt.Errorf("claudecode: read %s: %w", projName, err))
			}
			continue
		}
		for _, it := range items {
			name := it.Name()
			switch {
			case !it.IsDir() && strings.HasSuffix(name, ".jsonl"):
				id := strings.TrimSuffix(name, ".jsonl")
				if !IsSessionID(id) {
					continue
				}
				add(id, projName, Source{
					Kind: SrcMainTranscript, Path: filepath.Join(projPath, name),
					Rel: relPath(projName, name), Session: id, Stream: storage.StreamMain,
				})
			case it.IsDir():
				// Non-session directories live here too - "memory" is a sibling of
				// the session directories and is not a session.
				if !IsSessionID(name) {
					continue
				}
				add(name, projName, discoverSessionDir(projPath, projName, name)...)
			}
		}
	}

	out := make([]Session, 0, len(byID))
	for _, s := range byID {
		for _, src := range s.Sources {
			if src.Kind == SrcMainTranscript {
				s.Primary = path.Dir(src.Rel)
				break
			}
		}
		sort.Slice(s.Sources, func(i, j int) bool { return s.Sources[i].Rel < s.Sources[j].Rel })
		sort.Strings(s.Dirs)
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, warnings, nil
}

// discoverSessionDir enumerates the sources beneath one <project>/<session>/ directory.
func discoverSessionDir(projPath, projName, session string) []Source {
	var out []Source
	base := filepath.Join(projPath, session)
	relBase := filepath.Join(projName, session)

	// subagents/: direct child streams and their sidecars.
	subDir := filepath.Join(base, "subagents")
	if items, err := os.ReadDir(subDir); err == nil {
		for _, it := range items {
			name := it.Name()
			if it.IsDir() {
				if name == "workflows" {
					out = append(out, discoverWorkflowAgents(subDir, filepath.Join(relBase, "subagents"), session)...)
				}
				continue
			}
			if agentID, ok := agentTranscript(name); ok {
				out = append(out, Source{
					Kind: SrcAgentTranscript, Path: filepath.Join(subDir, name),
					Rel: relPath(relBase, "subagents", name), Session: session, Stream: agentID,
				})
				continue
			}
			if agentID, ok := agentMeta(name); ok {
				out = append(out, Source{
					Kind: SrcAgentMeta, Path: filepath.Join(subDir, name),
					Rel: relPath(relBase, "subagents", name), Session: session, Stream: agentID,
				})
			}
		}
	}

	// workflows/: run manifests, and scripts/ beneath it.
	wfDir := filepath.Join(base, "workflows")
	if items, err := os.ReadDir(wfDir); err == nil {
		for _, it := range items {
			name := it.Name()
			if it.IsDir() {
				if name == "scripts" {
					out = append(out, discoverScripts(filepath.Join(wfDir, name),
						filepath.Join(relBase, "workflows", "scripts"), session)...)
				}
				continue
			}
			if !strings.HasPrefix(name, "wf_") || !strings.HasSuffix(name, ".json") {
				continue
			}
			out = append(out, Source{
				Kind: SrcWorkflowManifest, Path: filepath.Join(wfDir, name),
				Rel: relPath(relBase, "workflows", name), Session: session,
				RunID: strings.TrimSuffix(name, ".json"),
			})
		}
	}
	return out
}

// discoverScripts enumerates workflow scripts, keying each to its run.
func discoverScripts(dir, rel, session string) []Source {
	items, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Source
	for _, it := range items {
		if it.IsDir() {
			continue
		}
		runID, ok := ScriptRunID(it.Name())
		if !ok {
			continue
		}
		out = append(out, Source{
			Kind: SrcWorkflowScript, Path: filepath.Join(dir, it.Name()),
			Rel: relPath(rel, it.Name()), Session: session, RunID: runID,
		})
	}
	return out
}

// discoverWorkflowAgents enumerates workflow-spawned children and run journals.
func discoverWorkflowAgents(subDir, relSub, session string) []Source {
	var out []Source
	wfRoot := filepath.Join(subDir, "workflows")
	runs, err := os.ReadDir(wfRoot)
	if err != nil {
		return nil
	}
	for _, run := range runs {
		if !run.IsDir() {
			continue
		}
		runID := run.Name()
		runPath := filepath.Join(wfRoot, runID)
		relRun := filepath.Join(relSub, "workflows", runID)
		items, err := os.ReadDir(runPath)
		if err != nil {
			continue
		}
		for _, it := range items {
			name := it.Name()
			if it.IsDir() {
				continue
			}
			switch name {
			case "journal.jsonl":
				out = append(out, Source{
					Kind: SrcJournal, Path: filepath.Join(runPath, name),
					Rel: relPath(relRun, name), Session: session, RunID: runID,
				})
			default:
				if agentID, ok := agentTranscript(name); ok {
					out = append(out, Source{
						Kind: SrcAgentTranscript, Path: filepath.Join(runPath, name),
						Rel: relPath(relRun, name), Session: session, Stream: agentID,
					})
				} else if agentID, ok := agentMeta(name); ok {
					out = append(out, Source{
						Kind: SrcAgentMeta, Path: filepath.Join(runPath, name),
						Rel: relPath(relRun, name), Session: session, Stream: agentID,
					})
				}
			}
		}
	}
	return out
}

func agentTranscript(name string) (string, bool) {
	if !strings.HasPrefix(name, "agent-") || !strings.HasSuffix(name, ".jsonl") {
		return "", false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(name, "agent-"), ".jsonl")
	return id, IsAgentID(id)
}

func agentMeta(name string) (string, bool) {
	if !strings.HasPrefix(name, "agent-") || !strings.HasSuffix(name, ".meta.json") {
		return "", false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(name, "agent-"), ".meta.json")
	return id, IsAgentID(id)
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// relPath builds a source path relative to the adapter root with forward slashes
// on every platform. It is written into cursors and landed headers, and a
// storage root must read the same on the machine that collected it and on
// any other, including one with a different path separator.
func relPath(parts ...string) string { return filepath.ToSlash(filepath.Join(parts...)) }
