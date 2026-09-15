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

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecodeprovider"
	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// providerFilter builds the provider adapter's session filter for one pass.
//
// A body names its session and nothing about where the session ran, so the
// adapter's include and exclude are judged the way the local adapter judges
// them: by the directory the session's main transcript sits under. Discovery
// of Claude Code's projects finds it for a live session. A session whose
// transcripts were pruned since they landed is judged by the directories its
// landed transcripts name, the first part of each header's src. A session
// nothing names a directory for waits.
func providerFilter(ad config.Adapter, projects string, zone *storage.Zone) claudecodeprovider.Filter {
	m := claudecode.NewMatcher(ad.Include, ad.Exclude)
	var found map[string]claudecode.Session
	judged := map[string]claudecodeprovider.Verdict{}
	return func(id string) claudecodeprovider.Verdict {
		if v, ok := judged[id]; ok {
			return v
		}
		if found == nil {
			found = map[string]claudecode.Session{}
			sessions, _, err := claudecode.DiscoverWithWarnings(projects)
			if err == nil {
				for _, s := range sessions {
					found[s.ID] = s
				}
			}
		}
		s, ok := found[id]
		if !ok {
			s, ok = landedSession(zone, id)
		}
		v := claudecodeprovider.Wait
		switch {
		case !ok:
		case m.Match(s):
			v = claudecodeprovider.Collect
		default:
			v = claudecodeprovider.Exclude
		}
		judged[id] = v
		return v
	}
}

// landedSession recovers where a session ran from its landed transcripts: the
// main transcript's source directory as the primary one, and every
// transcript's as the directories it spans.
func landedSession(zone *storage.Zone, id string) (claudecode.Session, bool) {
	files, err := storage.LandedFiles(zone, id)
	if err != nil {
		return claudecode.Session{}, false
	}
	s := claudecode.Session{ID: id}
	seen := map[string]bool{}
	for _, lf := range files {
		f, err := os.Open(lf.Path)
		if err != nil {
			continue
		}
		r, err := sessiondata.NewReader(f)
		if err != nil {
			f.Close()
			continue
		}
		hdr := r.Header()
		f.Close()
		if hdr.Kind != sessiondata.KindTranscript {
			continue
		}
		dir, _, ok := strings.Cut(hdr.Src, "/")
		if !ok {
			continue
		}
		if hdr.Stream == storage.StreamMain && s.Primary == "" {
			s.Primary = dir
		}
		if !seen[dir] {
			seen[dir] = true
			s.Dirs = append(s.Dirs, dir)
		}
	}
	return s, len(s.Dirs) > 0
}

// cmdSourcesProvider says what the provider adapter can see: the directory,
// how many bodies it holds, and which sessions the requests name.
func cmdSourcesProvider(_ *config.Config, ad config.Adapter, _ bool) error {
	root, err := claudecodeprovider.ResolveSourceRoot(ad.SourceRoot)
	if err != nil {
		return err
	}
	fmt.Printf("\nprovider root: %s\n", root)
	items, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		fmt.Printf("no provider bodies; Claude Code writes them when OTEL_LOG_RAW_API_BODIES=file:%s\n", root)
		return nil
	}
	if err != nil {
		return err
	}
	var requests, responses int
	sessions := map[string]int{}
	for _, it := range items {
		name := it.Name()
		switch claudecodeprovider.RoleOf(name) {
		case providerbody.RoleResponse:
			responses++
		case providerbody.RoleRequest:
			requests++
			if s := sessionOf(filepath.Join(root, name)); s != "" {
				sessions[s]++
			}
		}
	}
	fmt.Printf("bodies       : %d request(s), %d response(s)\n", requests, responses)
	ids := make([]string, 0, len(sessions))
	for id := range sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		fmt.Printf("  %s  %d request(s)\n", id, sessions[id])
	}
	if n := len(ids); n > 0 {
		fmt.Printf("\n%d session(s) named by requests; a response is joined to its session when collected\n", n)
	}
	return nil
}

// sessionOf reads the session a request body names, or nothing.
func sessionOf(path string) string {
	b, err := os.ReadFile(path)
	if err != nil || !json.Valid(b) {
		return ""
	}
	return strings.TrimSpace(claudecodeprovider.Lift(filepath.Base(path), b).Session)
}
