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

package claudecodechanges

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/pkg/changes"
	"github.com/apache/skywalking-ai-sessionizer/pkg/execution"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// Source is one file the plugin writes: the records of one stream of one
// session, one JSON line each, appended and never rewritten.
type Source struct {
	Path    string // absolute
	Rel     string // relative to the adapter's source root; kept in the landed header
	Session string
	Stream  string // main or an agent id
	// Kind is what the file holds, and what it lands as: change records,
	// or execution records, which the plugin writes to a file of their own.
	Kind sessiondata.Kind
}

// Session is one Claude Code session with every change file the plugin
// wrote for it.
type Session struct {
	ID      string
	Sources []Source
	// Root is the workspace the session's first record names, which is what
	// the include and exclude patterns are judged against.
	Root string
}

// Discover lists the sessions with change files under a source root.
//
// The layout is the plugin's: one directory per installed plugin, named
// after the plugin and the marketplace it came from, then
// output/<session>/<stream>.jsonl. Anything that is not that shape is
// left alone, so a file the plugin did not write is never landed as
// evidence.
func Discover(root string) ([]Session, error) {
	sessions, warnings, err := DiscoverWithWarnings(root)
	if err != nil {
		return nil, err
	}
	_ = warnings
	return sessions, nil
}

// outputDir is one directory of change records, and what it sits under.
type outputDir struct {
	under string // the plugin directory, or empty when the root is the data directory
	dir   string
}

// outputDirs finds every place change records are written under a root.
//
// There are two layouts, because there are two ways the recorder is given a
// data directory. Claude Code hands each plugin its own, under
// plugins/data/<plugin>-<marketplace>, so the root holds one directory per
// plugin. Any other runtime is told a directory outright with
// ASZ_CHANGES_DATA, and there is no plugin layer at all — the records are
// directly under it.
//
// Reading only the first would mean a root configured the second way
// discovers nothing, silently: the recorder writes, the adapter finds no
// session, and the conversation never mentions the files it wrote.
func outputDirs(root string, entries []os.DirEntry) []outputDir {
	var out []outputDir
	if info, err := os.Stat(filepath.Join(root, "output")); err == nil && info.IsDir() {
		out = append(out, outputDir{dir: filepath.Join(root, "output")})
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !isPluginDir(name) {
			continue
		}
		out = append(out, outputDir{under: name, dir: filepath.Join(root, name, "output")})
	}
	return out
}

// isPluginDir reports whether a directory is one the plugin's data lives in,
// under any name the plugin has had.
func isPluginDir(name string) bool {
	for _, plugin := range PluginNames {
		if name == plugin || strings.HasPrefix(name, plugin+"-") {
			return true
		}
	}
	return false
}

// aszSessionRe matches a session directory asz itself derived.
//
// The recorder writes under whatever session id it was told, and Claude Code's
// is a UUID. Another runtime's is not: a LangChain conversation lands under a
// name derived from its project and thread, because a supplied thread key is
// never safe as a path. Both shapes are accepted, and nothing else is, so the
// guard still holds — a stray file is not landed as evidence.
var aszSessionRe = regexp.MustCompile(`^ls-[a-z0-9-]*[0-9a-f]{12}$`)

// isSessionDir reports whether a directory under output is one the recorder
// wrote for a session.
func isSessionDir(name string) bool {
	return claudecode.IsSessionID(name) || aszSessionRe.MatchString(name)
}

// DiscoverWithWarnings is Discover with the non-fatal problems it met.
func DiscoverWithWarnings(root string) (sessions []Session, warnings []error, err error) {
	plugins, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	byID := map[string]*Session{}
	for _, p := range outputDirs(root, plugins) {
		name, out := p.under, p.dir
		dirs, err := os.ReadDir(out)
		if err != nil {
			if !os.IsNotExist(err) {
				warnings = append(warnings, fmt.Errorf("claudecodechanges: %s: %w", out, err))
			}
			continue
		}
		for _, d := range dirs {
			id := d.Name()
			if !d.IsDir() || !isSessionDir(id) {
				continue
			}
			files, err := os.ReadDir(filepath.Join(out, id))
			if err != nil {
				warnings = append(warnings, fmt.Errorf("claudecodechanges: %s: %w", filepath.Join(out, id), err))
				continue
			}
			for _, f := range files {
				stream, kind, ok := streamOf(f.Name())
				if f.IsDir() || !ok || (stream != "main" && !claudecode.IsAgentID(stream)) {
					continue
				}
				s := byID[id]
				if s == nil {
					s = &Session{ID: id}
					byID[id] = s
				}
				abs := filepath.Join(out, id, f.Name())
				s.Sources = append(s.Sources, Source{
					Path: abs, Rel: path.Join(name, "output", id, f.Name()), Session: id, Stream: stream, Kind: kind,
				})
				if s.Root == "" {
					s.Root = firstRoot(abs)
				}
			}
		}
	}
	for _, s := range byID {
		sort.Slice(s.Sources, func(i, j int) bool { return s.Sources[i].Rel < s.Sources[j].Rel })
		sessions = append(sessions, *s)
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].ID < sessions[j].ID })
	return sessions, warnings, nil
}

// streamOf reads a file's stream and kind from its name: <stream>.jsonl for
// change records, <stream>.execution.jsonl for execution records.
func streamOf(name string) (string, sessiondata.Kind, bool) {
	if stream, ok := strings.CutSuffix(name, ".execution.jsonl"); ok {
		return stream, sessiondata.KindExecution, true
	}
	stream, ok := strings.CutSuffix(name, ".jsonl")
	return stream, sessiondata.KindChanges, ok
}

// firstRoot reads the workspace path the first complete record names, or
// nothing when the file has no complete record yet. A change record names
// the root it observed; an execution record, the working directory the
// runtime reported.
func firstRoot(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	line, err := bufio.NewReaderSize(f, 64<<10).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	line = bytes.TrimRight(line, "\r\n")
	if r, ok := changes.Decode(line); ok && r.Root != nil {
		return r.Root.Path
	}
	if r, ok := execution.Decode(line); ok {
		return r.Cwd
	}
	return ""
}

// Match reports whether a session should be collected under the same
// include and exclude patterns the local adapter takes, judged by the
// workspace its records name.
func Match(m *claudecode.Matcher, s Session) bool {
	if m == nil {
		return true
	}
	if s.Root == "" {
		// A session whose files hold no complete record yet names no
		// workspace. Nothing lands from it either way, so it is collected,
		// and judged again once it does.
		return true
	}
	return m.Match(claudecode.Session{ID: s.ID, Primary: claudecode.Slugify(s.Root)})
}
