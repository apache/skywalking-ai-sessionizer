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
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/client"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/format"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
)

// admitted is the command root's admission when the root is a pulled
// snapshot, held from admission until the command ends. A snapshot's reader
// holds a lease the whole time it reads, so no sweep removes what it reads.
var (
	admitted   *storage.Admission
	admittedMu sync.Mutex
)

// releaseAdmission ends the command's admission, once. A command that dies
// first leaves its lease file, which the next sweep finds unheld and removes.
func releaseAdmission() {
	admittedMu.Lock()
	defer admittedMu.Unlock()
	if admitted == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := admitted.Release(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "asz: releasing the snapshot's lease: %v\n", err)
	}
	admitted = nil
}

// refuseCopyRoot refuses a root a command that writes would write pulled
// copies into: a path in pull's layout, a snapshot or anything else under
// _remote or a server's _pull, by where it is, and a directory outside them
// that holds a copy marker. A root that does not exist yet is held to its
// path's components alone, since a collector makes it.
func refuseCopyRoot(command, root string) error {
	if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
		if _, err := storage.CheckSuppliedPath(root, ""); err != nil {
			return fmt.Errorf("%s: %w", command, err)
		}
		return nil
	}
	r, err := storage.ClassifyRoot(root)
	if err != nil {
		return fmt.Errorf("%s: %w", command, err)
	}
	if r.Kind == storage.SnapshotRoot {
		return fmt.Errorf("%s: %w: %s", command, storage.ErrCopy, root)
	}
	if err := storage.RefuseCopy(r.Path); err != nil {
		return fmt.Errorf("%s: %w", command, err)
	}
	return nil
}

// admitRoot admits the command that reads root. An ordinary root is read as
// it always was. A pulled snapshot is admitted: under its conversation's
// gate its layout is checked again and its controls read, and a lease is
// held until the command ends. A path anywhere else in pull's layout, or a
// directory outside it that holds a copy marker, is refused.
func admitRoot(command string, cfg *config.Config, root string) (*storage.Root, error) {
	if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
		if _, err := storage.CheckSuppliedPath(root, ""); err != nil {
			return nil, fmt.Errorf("%s: %w", command, err)
		}
		return nil, nil
	}
	r, err := storage.ClassifyRoot(root)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", command, err)
	}
	if r.Kind != storage.SnapshotRoot {
		if copied, err := storage.IsCopy(r.Path); err != nil {
			return nil, fmt.Errorf("%s: %w", command, err)
		} else if copied {
			return nil, fmt.Errorf("%s: %s holds a copy marker outside _remote, where no snapshot is ever published; it is read as nothing", command, root)
		}
		return &r, nil
	}
	ttl, err := cfg.RemoteSnapshotTTLDuration()
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	a, err := storage.AdmitSnapshot(ctx, r, ttl, time.Now, nil)
	if errors.Is(err, storage.ErrPublishing) {
		// The tag's publication did not finish. Settling it needs the
		// conversation's _pull; a command that holds it is still
		// publishing, and its tag admits no reader until it is done.
		local := &client.Local{Root: r.StorageRoot, TTL: ttl}
		if rerr := local.RecoverIfFree(ctx, r.Remote, r.Conversation); rerr != nil {
			return nil, fmt.Errorf("%s: %w; %w", command, err, rerr)
		}
		a, err = storage.AdmitSnapshot(ctx, r, ttl, time.Now, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", command, err)
	}
	admittedMu.Lock()
	admitted = a
	admittedMu.Unlock()
	return &r, nil
}

// checkIdentity holds a conversation or session identity a person gave to
// the rule every name follows, before any path is made from it. A session
// cannot start with _, which storage keeps for its own directories.
func checkIdentity(what, id string, session bool) error {
	if id == "" {
		return nil
	}
	if err := storage.CheckNamePart(id); err != nil {
		return fmt.Errorf("%s %q: %w", what, id, err)
	}
	if session && strings.HasPrefix(id, "_") {
		return fmt.Errorf("%s %q starts with _, which storage keeps for its own directories", what, id)
	}
	return nil
}

// verifySnapshot checks a pulled snapshot as a pull accepted it: exactly the
// evidence its manifest lists, whole, bound to its marker. A snapshot holds
// no cursors and no index, so the stream checks of an ordinary root do not
// apply to it.
func verifySnapshot(a *storage.Admission) error {
	m := a.Marker
	accepted, err := format.Accept(a.Root.Path, format.Published, format.Want{
		Conversation: a.Root.Conversation, ManifestDigest: format.ManifestDigest(m.ManifestDigest),
		Session: m.Session, Round: m.Round, Digest: format.CommitDigest(m.Digest), Remote: a.Root.Remote, Tag: a.Root.Tag,
	})
	if err != nil {
		return err
	}
	fmt.Printf("snapshot %s/%s:%s holds %d file(s) through round %d, and they are whole evidence\n",
		a.Root.Remote, a.Root.Conversation, a.Root.Tag, len(accepted.Manifest.Files), accepted.Manifest.Head.Round)
	return nil
}

// admitNavigation admits a judge's navigation server to its root. A server a
// run started on a pulled snapshot inherits the run: its parent's held run
// lease admits it, with no age check and no configuration, and its log and
// failure report must be in that run's work directory, the log one its
// parent made. One started on its own is admitted as any reader is, with the
// configured time to live. It returns the log's identity as it was checked,
// for an inherited server, so the open that appends is held to it.
func admitNavigation(root, cfgPath, parentLease, logPath, failureReport string) (os.FileInfo, int) {
	if parentLease != "" {
		r, err := storage.ClassifyRoot(root)
		if err != nil {
			return nil, fail(err)
		}
		if r.Kind != storage.SnapshotRoot {
			return nil, fail(fmt.Errorf("-parent-lease is for a pulled snapshot, and %s is not one", root))
		}
		var log os.FileInfo
		check := func(marker storage.SnapshotCopy, run string) error {
			var err error
			log, err = inheritedLog(r.Path, marker.Session, run, parentLease, logPath, failureReport)
			return err
		}
		a, _, err := storage.AdmitChild(context.Background(), r, parentLease, nil, check)
		if err != nil {
			return nil, fail(err)
		}
		admittedMu.Lock()
		admitted = a
		admittedMu.Unlock()
		return log, 0
	}
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return nil, fail(err)
	}
	if _, err := admitRoot("judge mcp", cfg, root); err != nil {
		return nil, fail(err)
	}
	return nil, 0
}

// inheritedLog checks what an inherited navigation server was given against
// its run: the log must be an existing regular file its parent made, inside
// that run's real work directory, and the failure report must be the one
// beside it that the parent's lease names. A path that leads elsewhere, a
// link included, is refused before anything is opened.
func inheritedLog(snapshot, session, run, parentLease, logPath, failureReport string) (os.FileInfo, error) {
	if logPath == "" {
		return nil, errors.New("-parent-lease needs -log, the log its run made for it")
	}
	// The run's work directory and the log's directory are each reached
	// through real directories only: a link under the snapshot, one that
	// leads run A's work directory to run B's included, is refused, never
	// followed.
	work, err := storage.OpenDir(filepath.Join(snapshot, session, storage.JudgmentsDir, run, storage.WorkDir))
	if err != nil {
		return nil, err
	}
	workReal := work.Real()
	_ = work.Close()
	log, err := filepath.Abs(logPath)
	if err != nil {
		return nil, err
	}
	parent, err := storage.OpenDir(filepath.Dir(log))
	if err != nil {
		return nil, fmt.Errorf("-log %s: %w", logPath, err)
	}
	logDir := parent.Real()
	_ = parent.Close()
	if rel, err := filepath.Rel(workReal, logDir); err != nil || strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("-log %s is not in the work directory of run %s", logPath, run)
	}
	info, err := os.Lstat(filepath.Join(logDir, filepath.Base(log)))
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("-log %s is not a regular file", logPath)
	}
	report, err := filepath.Abs(failureReport)
	if err != nil {
		return nil, err
	}
	if filepath.Dir(report) != filepath.Dir(log) || filepath.Base(report) != storage.FailurePrefix+leaseRandom(parentLease) {
		return nil, fmt.Errorf("-failure-report %s is not the report beside -log %s that the run's lease names", failureReport, logPath)
	}
	return info, nil
}

// leaseRandom is the random part of a lease's name, which names the failure
// markers of the run that holds it.
func leaseRandom(lease string) string {
	return strings.TrimSuffix(lease[strings.LastIndexByte(lease, '-')+1:], ".lock")
}

// admittedRoot is the pulled snapshot the command reads under a lease, or
// empty: a file the command is given may lie inside it.
func admittedRoot() string {
	if admitted == nil {
		return ""
	}
	return admitted.Root.Path
}

// openNavigationLog opens a navigation server's log. An inherited server
// appends to the log its parent made, the very file its admission checked,
// and makes nothing. A server started on its own makes a new log, outside
// every snapshot, and never appends to a file already there: a path that
// leads to evidence, through a hard link too, would otherwise be written.
func openNavigationLog(path string, checked os.FileInfo) (*os.File, error) {
	if checked == nil {
		return storage.CreateSupplied(path, "")
	}
	f, err := storage.OpenSuppliedAppend(path, admittedRoot())
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && !os.SameFile(info, checked) {
		err = fmt.Errorf("%s changed after it was checked", path)
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

// snapshotView binds a reader of the admitted snapshot to it: its marker's
// head, and the landed inventory of its frozen manifest, read with the
// bounded parser and held to the marker. It is nil on an ordinary root. The
// manifest is read once a command was admitted, outside the gate, and its
// whole digest is not taken again: acceptance did that when it was pulled.
func snapshotView(fatal func(error)) (*view.Snapshot, error) {
	if admitted == nil {
		return nil, nil
	}
	return snapshotViewOf(admitted, fatal)
}

// snapshotViewOf binds a reader of the snapshot a holds to it, as
// snapshotView does the command's own.
func snapshotViewOf(admitted *storage.Admission, fatal func(error)) (*view.Snapshot, error) {
	m := admitted.Marker
	// The manifest is evidence: one that does not read, or does not match
	// its marker, is damage to the snapshot.
	f, err := storage.OpenEvidence(filepath.Join(admitted.Root.Path, format.ManifestFile), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", view.ErrEvidence, err)
	}
	defer f.Close()
	manifest, err := format.ReadManifest(f, admitted.Root.Conversation)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", view.ErrEvidence, err)
	}
	if manifest.Session != m.Session || manifest.Head.Round != m.Round || string(manifest.Head.Digest) != m.Digest {
		return nil, fmt.Errorf("%w: %s: its manifest names session %s at round %d, and its marker session %s at round %d",
			view.ErrEvidence, admitted.Root.Path, manifest.Session, manifest.Head.Round, m.Session, m.Round)
	}
	landed, store := map[uint64]string{}, map[string]string{}
	for _, e := range manifest.Files {
		switch e.Kind {
		case format.KindLanded, format.KindSkeleton:
			landed[e.Seq] = e.File
		case format.KindStore:
			store[e.File] = string(e.Content)
		}
	}
	return &view.Snapshot{Conversation: admitted.Root.Conversation, Session: m.Session, Round: m.Round, Digest: m.Digest,
		Landed: landed, Store: store, Fatal: fatal}, nil
}

// viewServer is a view server over a command's root, bound to the snapshot
// the command was admitted to when it was. The snapshot is nil on an
// ordinary root, and reads there keep their ordinary forms.
func viewServer(zone *storage.Zone, fatal func(error)) (*view.Server, *view.Snapshot, error) {
	srv := view.New(zone, nil)
	snap, err := snapshotView(fatal)
	if err != nil {
		return nil, nil, err
	}
	if snap != nil {
		srv.SetSnapshot(snap)
	}
	return srv, snap, nil
}
