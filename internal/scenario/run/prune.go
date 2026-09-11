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

package run

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecodechanges"
	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/verify"
)

// prunedSourcesGone checks what collection does when Claude Code prunes its
// files, on a copy of the finished root. Claude Code deletes a session's main
// transcript while the session's other files can stay for a while, and in
// the end deletes them all. After each, a pass must set the active cursor of
// every file that went to source_gone and leave every other cursor as it was.
// It must land nothing and introduce no conflict or error, every landed file must
// keep its digest, a parse must write no round, and asz verify must count as
// many problems as it did before. It compares the count only. Last the files
// come back with the bytes they had, as from a backup, and a pass must set
// every cursor back to what it said before, under the same checks.
func prunedSourcesGone(out, session string, maxRound int64) ([]string, error) {
	dir := out + "-pruned"
	defer os.RemoveAll(dir)
	if err := copyRoot(out, dir); err != nil {
		return nil, err
	}
	source := filepath.Join(dir, "_source")
	zone := storage.NewZone(dir)
	var lines []string
	bad := func(format string, a ...any) {
		lines = append(lines, "pruned_sources_gone: "+fmt.Sprintf(format, a...))
	}

	sessions, warnings, err := claudecode.DiscoverWithWarnings(source)
	if err != nil {
		return nil, err
	}
	if len(warnings) > 0 {
		return nil, fmt.Errorf("pruned_sources_gone: discovery: %v", warnings)
	}
	var sources []claudecode.Source
	for _, s := range sessions {
		if s.ID == session {
			sources = s.Sources
		}
	}
	if len(sources) == 0 {
		return []string{"pruned_sources_gone: the session was not discovered, so nothing could be pruned"}, nil
	}
	before, err := cursorStates(zone, session)
	if err != nil {
		return nil, err
	}
	landed, err := landedDigests(zone, session)
	if err != nil {
		return nil, err
	}
	problems, err := verifyProblems(zone, session)
	if err != nil {
		return nil, err
	}
	// The bytes of every source, to put back at the end.
	saved := map[string][]byte{}
	for _, src := range sources {
		if saved[src.Path], err = os.ReadFile(src.Path); err != nil {
			return nil, err
		}
	}

	gone := map[string]bool{} // source paths as a cursor names them
	check := func(what string) error {
		landedNow, err := collectCounting(source, zone)
		if err != nil {
			return err
		}
		if landedNow != "" {
			bad("%s: the pass %s", what, landedNow)
		}
		now, err := cursorStates(zone, session)
		if err != nil {
			return err
		}
		for _, path := range sortedKeys(before) {
			was := before[path]
			want := was.state
			// The plugin's lines are not Claude Code's files, and nothing
			// here prunes them.
			if was.state == storage.CursorActive && filepath.Base(path) != "changes.cursor" && gone[was.source] {
				want = storage.CursorSourceGone
			}
			got, ok := now[path]
			switch {
			case !ok:
				bad("%s: the cursor %s is gone", what, rel(dir, path))
			case got.state != want:
				bad("%s: the cursor %s says %s, want %s", what, rel(dir, path), got.state, want)
			}
		}
		for path := range now {
			if _, ok := before[path]; !ok {
				bad("%s: a pass made the cursor %s", what, rel(dir, path))
			}
		}
		after, err := landedDigests(zone, session)
		if err != nil {
			return err
		}
		if fmt.Sprint(after) != fmt.Sprint(landed) {
			bad("%s: the landed files changed: %d before, %d after", what, len(landed), len(after))
		}
		r, err := parse.Session(zone, parse.Options{Conversation: session, Session: session, MaxRoundBytes: maxRound})
		if err != nil {
			return err
		}
		if r.Changed() {
			bad("%s: a parse wrote round %d", what, r.Number)
		}
		p, err := verifyProblems(zone, session)
		if err != nil {
			return err
		}
		if p != problems {
			bad("%s: asz verify reports %d problem(s), and %d before", what, p, problems)
		}
		return nil
	}

	// The main transcript first, which leaves the session discovered when it
	// has any other file, and then every file left, which does not.
	var main, rest []claudecode.Source
	for _, src := range sources {
		if src.Kind == claudecode.SrcMainTranscript {
			main = append(main, src)
		} else {
			rest = append(rest, src)
		}
	}
	for _, step := range []struct {
		what string
		srcs []claudecode.Source
	}{{"the main transcript pruned", main}, {"every file pruned", rest}} {
		if len(step.srcs) == 0 {
			continue
		}
		for _, src := range step.srcs {
			if err := os.Remove(src.Path); err != nil {
				return nil, err
			}
			gone[src.Rel] = true
		}
		if err := check(step.what); err != nil {
			return nil, err
		}
	}

	// The files come back with the bytes they had.
	for path, b := range saved {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, b, 0o644); err != nil {
			return nil, err
		}
	}
	gone = map[string]bool{}
	if err := check("the files restored"); err != nil {
		return nil, err
	}
	return lines, nil
}

type cursorState struct{ source, state string }

// cursorStates reads every cursor of a session, by path.
func cursorStates(z *storage.Zone, session string) (map[string]cursorState, error) {
	out := map[string]cursorState{}
	for _, sub := range []string{"streams", "runs"} {
		owners, err := os.ReadDir(filepath.Join(z.SessionDir(session), sub))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, o := range owners {
			dir := filepath.Join(z.SessionDir(session), sub, o.Name())
			items, err := os.ReadDir(dir)
			if err != nil {
				return nil, err
			}
			for _, it := range items {
				if !strings.HasSuffix(it.Name(), ".cursor") {
					continue
				}
				path := filepath.Join(dir, it.Name())
				c, err := storage.LoadCursor(path, storage.CursorAppend, "")
				if err != nil {
					return nil, err
				}
				out[path] = cursorState{c.Source, c.State}
			}
		}
	}
	return out, nil
}

// landedDigests reads the digest of every landed file of a session, by path.
func landedDigests(z *storage.Zone, session string) (map[string]string, error) {
	files, err := storage.LandedFiles(z, session)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, f := range files {
		if out[f.Path], err = storage.FileDigest(f.Path); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// verifyProblems is what asz verify counts for a session: the streams and
// the chain together.
func verifyProblems(z *storage.Zone, session string) (int, error) {
	s, err := verify.Session(z, session)
	if err != nil {
		return 0, err
	}
	c, err := verify.Chain(z, session, nil)
	if err != nil {
		return 0, err
	}
	return s.Problems + c.Problems, nil
}

// collectCounting runs one pass of both adapters, as a process that has just
// started does. It says what the pass landed or the errors it returned, and
// returns an empty string when there was none. Existing conflicts are counted
// again when their files are discovered, so the caller checks cursor states
// instead to tell whether the pass introduced a conflict.
func collectCounting(source string, z *storage.Zone) (string, error) {
	st, err := claudecode.New(source, z, 0).CollectAll(nil)
	if err != nil {
		return "", err
	}
	cs, err := claudecodechanges.New(filepath.Join(source, "plugins", "data"), z, 0).CollectAll(nil)
	if err != nil {
		return "", err
	}
	if errs := append(st.Errors, cs.Errors...); len(errs) > 0 {
		return fmt.Sprintf("failed: %v", errs), nil
	}
	if st.SourcesLanded+cs.SourcesLanded > 0 || st.Records+cs.Records > 0 {
		return fmt.Sprintf("landed %d record(s) from %d source(s)",
			st.Records+cs.Records, st.SourcesLanded+cs.SourcesLanded), nil
	}
	return "", nil
}

func sortedKeys(m map[string]cursorState) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func rel(root, path string) string {
	r, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(r)
}
