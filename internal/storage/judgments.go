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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// JudgmentsDir holds a session's judge runs, inside the session's directory.
// They are not inside a chain's directory, which is removed to parse a
// conversation again, since a judgment cannot be rebuilt from landed files.
// Inside the session's directory they go wherever the session goes: a
// retirement removes them with it, and a pass's state forgets them with it.
const JudgmentsDir = "judgments"

// WorkDir is where a run keeps what its evaluators were given and said, for a
// person reading how a run went. It is never sent.
const WorkDir = "work"

var (
	runDirRe    = regexp.MustCompile(`^j(\d{6,})$`)
	stageFileRe = regexp.MustCompile(`^s(\d)-([a-z]+)-([0-9a-f]{12})\.sj$`)
)

// JudgmentRun is one run of a conversation as the directory holds it.
type JudgmentRun struct {
	Run   int
	Dir   string
	Files []JudgmentFile
}

// JudgmentFile is one stage file of a run.
type JudgmentFile struct {
	Path  string
	Order int
	Stage string
}

// Closed says whether the run has its close file. A run is sent only once it
// is closed.
func (r JudgmentRun) Closed() bool {
	for _, f := range r.Files {
		if f.Stage == "close" {
			return true
		}
	}
	return false
}

// JudgmentRuns lists a session's runs in order, each with its stage files in
// run order.
func JudgmentRuns(z *Zone, session string) ([]JudgmentRun, error) {
	dir := filepath.Join(z.SessionDir(session), JudgmentsDir)
	items, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var runs []JudgmentRun
	for _, it := range items {
		m := runDirRe.FindStringSubmatch(it.Name())
		if m == nil || !it.IsDir() {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		run := JudgmentRun{Run: n, Dir: filepath.Join(dir, it.Name())}
		files, err := os.ReadDir(run.Dir)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			fm := stageFileRe.FindStringSubmatch(f.Name())
			if fm == nil {
				continue
			}
			order, _ := strconv.Atoi(fm[1])
			run.Files = append(run.Files, JudgmentFile{Path: filepath.Join(run.Dir, f.Name()), Order: order, Stage: fm[2]})
		}
		sort.Slice(run.Files, func(i, j int) bool { return run.Files[i].Order < run.Files[j].Order })
		runs = append(runs, run)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].Run < runs[j].Run })
	return runs, nil
}

// ErrRetiredSession means a session was retired, so nothing of it is written.
var ErrRetiredSession = errors.New("storage: the session is retired")

// ReserveRun makes the next run's directory of a session and returns its
// number. It holds the root lock only while it does, since a run's
// evaluators can take minutes and collection must go on meanwhile. A retired
// session gets no run.
func ReserveRun(z *Zone, session string) (int, string, error) {
	l, err := LockRootWait(z.Root(), 30*time.Second)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = l.Unlock() }()
	if retired, err := Retired(z, session); err != nil || retired {
		if err == nil {
			err = ErrRetiredSession
		}
		return 0, "", err
	}
	runs, err := JudgmentRuns(z, session)
	if err != nil {
		return 0, "", err
	}
	next := 1
	if len(runs) > 0 {
		next = runs[len(runs)-1].Run + 1
	}
	parent := filepath.Join(z.SessionDir(session), JudgmentsDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return 0, "", err
	}
	dir := filepath.Join(parent, fmt.Sprintf("j%06d", next))
	if err := os.Mkdir(dir, 0o755); err != nil {
		return 0, "", err
	}
	SyncAfterRename(parent)
	return next, dir, nil
}

// PublishStage writes one stage file of a run, once and read-only, under the
// root lock and only while the session is not retired.
func PublishStage(z *Zone, session, runDir string, order int, stage, digest string, data []byte) (string, error) {
	l, err := LockRootWait(z.Root(), 30*time.Second)
	if err != nil {
		return "", err
	}
	defer func() { _ = l.Unlock() }()
	if retired, err := Retired(z, session); err != nil || retired {
		if err == nil {
			err = ErrRetiredSession
		}
		return "", err
	}
	if len(digest) < 12 {
		return "", fmt.Errorf("storage: digest %q is too short", digest)
	}
	// A run holds one file per stage, whatever its digest: a second one
	// would be a second answer to the same question in one run.
	items, err := os.ReadDir(runDir)
	if err != nil {
		return "", err
	}
	for _, it := range items {
		if m := stageFileRe.FindStringSubmatch(it.Name()); m != nil && m[2] == stage {
			return "", fmt.Errorf("storage: the run already has its %s stage, %s; a stage is written once", stage, it.Name())
		}
	}
	path := filepath.Join(runDir, fmt.Sprintf("s%d-%s-%s.sj", order, stage, digest[:12]))
	// A failed sync of the directory after the rename does not fail the
	// write: the file is in place, and a reader may already have it.
	err = WriteAtomic(path, 0o444, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
	return path, err
}

// MakeWorkDir makes the directory a stage's evaluator works in, under the root
// lock and only while the session is not retired. It is inside the session's
// directory, so making it for a retired session would make that session again.
func MakeWorkDir(z *Zone, session, runDir, name string) (string, error) {
	l, err := LockRootWait(z.Root(), 30*time.Second)
	if err != nil {
		return "", err
	}
	defer func() { _ = l.Unlock() }()
	if retired, err := Retired(z, session); err != nil || retired {
		if err == nil {
			err = ErrRetiredSession
		}
		return "", err
	}
	if _, err := os.Stat(runDir); err != nil {
		return "", err
	}
	dir := filepath.Join(runDir, WorkDir, name)
	return dir, os.MkdirAll(dir, 0o700)
}
