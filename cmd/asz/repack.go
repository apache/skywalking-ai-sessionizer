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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/repack"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// cmdRepack re-cuts the landed files of the storage root into DEST under the
// configured file budget, then assembles DEST's chains.
//
// A landed file is never rewritten in place, so a change of budget applies to
// new files only. This is how an existing root is brought under a new budget:
// every record keeps its bytes and lands in DEST at a new position, the
// cursors come along so collection can continue there, and the chains are
// built again because their references name positions in the old files.
func cmdRepack(cfg *config.Config, ad config.Adapter, _ bool) error {
	dest := arg(0)
	if dest == "" {
		return fmt.Errorf("usage: asz repack [-config FILE] DEST [SESSION]")
	}
	srcRoot, err := cfg.ResolvedRoot()
	if err != nil {
		return err
	}
	dstRoot, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	if err := refuseCopyRoot("repack", dstRoot); err != nil {
		return err
	}
	// Both are compared with their symbolic links followed, since two
	// spellings can name one directory.
	realSrc, realDst := resolved(srcRoot), resolved(dstRoot)
	if realDst == realSrc {
		return fmt.Errorf("repack: DEST is the storage root itself; landed files are never rewritten in place")
	}
	// A directory inside the root would be read as one of its sessions, by
	// this repack and by every command after it.
	if rel, err := filepath.Rel(realSrc, realDst); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("repack: DEST %s is inside the storage root; use a directory outside it", dstRoot)
	}
	if err := repack.CheckDestination(dstRoot); err != nil {
		return err
	}
	// The root read from is held for the whole copy, so no pass lands a file
	// or moves a cursor while the copy reads them.
	lock, err := holdRootFor(srcRoot, "repack")
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	// A session retention removed files of cannot be repacked. It is
	// refused before the destination is made, so nothing is written.
	want := arg(1)
	sessions, err := sessionDirs(srcRoot)
	if err != nil {
		return err
	}
	checked := sessions
	if want != "" {
		checked = []string{want}
	}
	src := storage.NewZone(srcRoot)
	if err := repack.CheckSource(src, checked); err != nil {
		return err
	}
	// The root written to is held too, and checked again under its lock,
	// since another command could have started on it while this one waited.
	// It is not waited for: a command that holds a new or empty root is
	// writing into it, and the copy must not.
	if err := os.MkdirAll(dstRoot, 0o755); err != nil {
		return err
	}
	dstLock, err := storage.LockRoot(dstRoot)
	if errors.Is(err, storage.ErrRootBusy) {
		return fmt.Errorf("repack: another command is writing %s; use a new or empty directory", dstRoot)
	}
	if err != nil {
		return err
	}
	defer func() { _ = dstLock.Unlock() }()
	if err := repack.CheckDestination(dstRoot); err != nil {
		return err
	}
	dst := storage.NewZone(dstRoot)
	budget := ad.Collector.MaxDeltaBytes
	fmt.Printf("from  : %s\nto    : %s\nbudget: %s per file\n\n", srcRoot, dstRoot, humanBytes(budget))

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tFILES IN\tFILES OUT\tRECORDS\tBYTES")
	var done, failed int
	for _, id := range sessions {
		if want != "" && id != want {
			continue
		}
		st, err := repack.Session(src, dst, id, budget, time.Now())
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s: %v\n", id, err)
			failed++
			continue
		}
		done++
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%s\n", id, st.FilesIn, st.FilesOut, st.Records, humanBytes(st.Bytes))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\n%d session(s) repacked\n\n", done)
	if failed > 0 {
		return fmt.Errorf("%d session(s) failed to repack", failed)
	}
	// The chains are built on the new files: their old references named
	// positions that no longer exist.
	return parseZone(dstRoot, want, cfg.Parse.MaxRoundBytes)
}

// resolved is path with every symbolic link in the part of it that exists
// followed. The part that does not exist yet is joined on as it is written.
func resolved(path string) string {
	var rest []string
	for p := path; ; {
		if followed, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(append([]string{followed}, rest...)...)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return path
		}
		rest = append([]string{filepath.Base(p)}, rest...)
		p = parent
	}
}
