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

package claudecodeprovider

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// States of a body file.
const (
	// stateLanded: the body is landed in its session.
	stateLanded = "landed"
	// stateWaiting: no session claims the body yet, or its session is not
	// one discovery has found. Every pass tries again.
	stateWaiting = "waiting"
	// stateExcluded: the body's session is left out by include and exclude.
	stateExcluded = "excluded"
	// stateUnreadable: the file is not one JSON value, long after it was
	// last written, so it names no session.
	stateUnreadable = "unreadable"
	// stateConflict: a body of the same name with other bytes is already
	// landed in the session. Nothing lands from it.
	stateConflict = "conflict"
	// stateChanged: the file changed after it landed. The line keeps the
	// digest that landed, so putting the bytes back ends it.
	stateChanged = "changed"
)

// seenSchema is the version of the state file.
const seenSchema = "1"

// entry is what the root remembers about one body file.
type entry struct {
	name    string
	size    int64
	mtime   int64
	sha     string // the first twelve hexadecimal characters of the body's SHA-256
	state   string
	session string
	keys    providerbody.Keys
}

// seen is the root's table of body files by name.
//
// It is derived. Every landed body is a record whose manifest names its file,
// its size and its digest, so the table rebuilds from the landed records when
// it is missing, and a landed entry is trusted only while its session's
// provider directory exists.
type seen struct {
	entries map[string]*entry
	dirty   bool
	// damaged lists the landed provider files a rebuild could not read.
	// Their bodies are not in the table, and every other session's are.
	damaged []error
}

func loadSeen(z *storage.Zone) (*seen, error) {
	f, err := os.Open(z.ProviderStatePath())
	if errors.Is(err, os.ErrNotExist) {
		return rebuildSeen(z)
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s := &seen{entries: map[string]*entry{}}
	br := bufio.NewReader(f)
	first := true
	for {
		line, err := br.ReadString('\n')
		if line = strings.TrimRight(line, "\n"); line != "" {
			if first {
				if line != "schema\t"+seenSchema {
					// Another version's table is discarded, not migrated.
					return rebuildSeen(z)
				}
				first = false
			} else if e, ok := parseEntry(line); ok {
				s.entries[e.name] = e
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	return s, nil
}

// dash writes a value into one tab-separated field. A value that could break
// the line, which only a body can supply, is written as absent.
func dash(v string) string {
	if v == "" || strings.ContainsAny(v, " \t\r\n") {
		return "-"
	}
	return v
}

func undash(v string) string {
	if v == "-" {
		return ""
	}
	return v
}

func parseEntry(line string) (*entry, bool) {
	f := strings.Split(line, "\t")
	if len(f) != 10 {
		return nil, false
	}
	size, err1 := strconv.ParseInt(f[1], 10, 64)
	mtime, err2 := strconv.ParseInt(f[2], 10, 64)
	if err1 != nil || err2 != nil {
		return nil, false
	}
	return &entry{
		name: f[0], size: size, mtime: mtime, sha: undash(f[3]), state: f[4], session: undash(f[5]),
		keys: providerbody.Keys{Request: undash(f[6]), Call: undash(f[7]), PreviousRequest: undash(f[8]), Session: undash(f[9])},
	}, true
}

func (s *seen) save(z *storage.Zone, now time.Time) error {
	names := make([]string, 0, len(s.entries))
	for n := range s.entries {
		names = append(names, n)
	}
	sort.Strings(names)
	err := storage.WriteAtomic(z.ProviderStatePath(), storage.PermState, func(w io.Writer) error {
		bw := bufio.NewWriter(w)
		fmt.Fprintf(bw, "schema\t%s\n", seenSchema)
		for _, n := range names {
			e := s.entries[n]
			fmt.Fprintf(bw, "%s\t%d\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", e.name, e.size, e.mtime, dash(e.sha), e.state,
				dash(e.session), dash(e.keys.Request), dash(e.keys.Call), dash(e.keys.PreviousRequest), dash(e.keys.Session))
		}
		return bw.Flush()
	})
	if err == nil {
		s.dirty = false
	}
	_ = now
	return err
}

// rebuildSeen reads every landed provider body of every session. A rebuilt
// entry has no modification time, so the first pass reads each file once
// more and compares its digest.
func rebuildSeen(z *storage.Zone) (*seen, error) {
	s := &seen{entries: map[string]*entry{}, dirty: true}
	items, err := os.ReadDir(z.Root())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, err
	}
	for _, it := range items {
		if !it.IsDir() || strings.HasPrefix(it.Name(), "_") {
			continue
		}
		files, err := storage.LandedFiles(z, it.Name())
		if err != nil {
			s.damaged = append(s.damaged, fmt.Errorf("claudecodeprovider: session %s: %w", it.Name(), err))
			continue
		}
		for _, lf := range files {
			if lf.Stream != "" || lf.RunID != "" {
				continue
			}
			if err := readLanded(lf.Path, func(hdr sessiondata.Header, rec *sessiondata.Record) {
				m, err := providerbody.ManifestOf(rec)
				if err != nil || len(m.SHA256) < 12 {
					return
				}
				s.entries[m.Src] = &entry{
					name: m.Src, size: int64(m.Bytes), mtime: -1, sha: m.SHA256[:12], state: stateLanded, session: hdr.Session,
					keys: providerbody.Keys{Request: m.Request, Call: m.Call, PreviousRequest: m.PreviousRequest, Session: m.Session},
				}
			}); err != nil {
				// One damaged file must not keep every other session's bodies
				// from being collected. A file cut short or edited holds bodies
				// the table then does not know, and they are read again.
				s.damaged = append(s.damaged, err)
			}
		}
	}
	return s, nil
}

// readLanded calls fn for every record of a landed provider_body file.
func readLanded(path string, fn func(sessiondata.Header, *sessiondata.Record)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r, err := sessiondata.NewReader(f)
	if err != nil {
		return fmt.Errorf("claudecodeprovider: %s: %w", path, err)
	}
	if r.Header().Kind != sessiondata.KindProviderBody {
		return nil
	}
	for {
		rec, err := r.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("claudecodeprovider: %s: %w", path, err)
		}
		fn(r.Header(), rec)
	}
}

// Forget drops every line of the given sessions from the root's table. A
// removal calls it once the sessions' body files and directories are gone, as
// it drops their lines from the export and metrics state. A table that does
// not exist has nothing to drop.
func Forget(z *storage.Zone, sessions []string, now time.Time) error {
	if _, err := os.Stat(z.ProviderStatePath()); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	lock, err := storage.LockProvider(z.Root())
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	sn, err := loadSeen(z)
	if err != nil {
		return err
	}
	gone := map[string]bool{}
	for _, s := range sessions {
		gone[s] = true
	}
	for name, e := range sn.entries {
		if gone[e.session] || gone[e.keys.Session] {
			delete(sn.entries, name)
			sn.dirty = true
		}
	}
	if !sn.dirty {
		return nil
	}
	return sn.save(z, now)
}
