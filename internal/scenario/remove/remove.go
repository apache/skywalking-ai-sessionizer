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

// Package remove removes a scenario session once all of it has been sent.
//
// Only a session a claude-code scenario build marked is ever removed, and
// only by the policy its marker names. A real Claude Code session has no
// marker, so nothing here ever touches it. The product configuration has no
// removal setting at all.
//
// It is collector side. It reads the source directory, the storage root and
// the export state, and it imports nothing that assembles or serves.
package remove

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecodechanges"
	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp"
	"github.com/apache/skywalking-ai-sessionizer/internal/metrics"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// MaxPerPass bounds how many sessions one pass takes through step 0, resumed
// ones included. A removal holds the export lock, and asz push waits two
// minutes for it. The time one removal takes was not measured, so the cap
// bounds the hold whatever it turns out to be. It is a starting value.
const MaxPerPass = 16

// conversationsDir is where the chains are, under the storage root. It is
// written out because the chain's package is server side.
const conversationsDir = "_conversations"

// changesPrefix is where, under the local adapter's source directory, the
// changes adapter's source directory is. The build writes the plugin output
// there, and a removal checks that the changes adapter reads it there.
const changesPrefix = "plugins/data/"

// Remover removes the sessions a scenario build marked, once all of each is
// sent. The pipeline calls Pass at the end of every pass, after the push,
// while it holds the scenario lock.
type Remover struct {
	Zone *storage.Zone
	// Source is claude-code-local's source directory, where the build wrote
	// the session and its marker.
	Source     string
	Local      *claudecode.Collector
	LocalMatch func(claudecode.Session) bool
	// Changes is nil when the changes adapter is not enabled.
	Changes      *claudecodechanges.Collector
	ChangesRoot  string
	ChangesMatch func(claudecodechanges.Session) bool
	// Derives says claude-code-local derives metrics, so every landed file
	// must be derived before its session can go.
	Derives bool
	Now     func() time.Time
	// Hook is called at every point a removal reaches, in order. An error
	// stops the removal there, as a crash would, and the locks are released
	// as a dying process's are. Tests use it to run the first steps only.
	Hook func(Point) error

	// said holds the blocked reasons already reported, by session, so each
	// is printed once per process. The empty session is every session.
	said map[string]map[string]bool
	// stopped holds sessions whose removal stopped on a check, left for a
	// person. They are not tried again by this process.
	stopped map[string]bool
	// last is the latest record time found in each session's landed files.
	// It only grows while a session is kept, since landed files are never
	// changed, so a session it shows as too young is still too young.
	last map[string]time.Time
}

// Input is what the pipeline knows that the root does not say.
type Input struct {
	// Endpoint names the receiver the pipeline sends to, as otlp.EndpointOf
	// gives it. Empty when there is no receiver.
	Endpoint    string
	SendLogs    bool
	SendMetrics bool
	// Busy holds sessions another process held this pass, the pipeline's
	// sessions to parse again.
	Busy map[string]bool
}

// Result is what one pass did.
type Result struct {
	Removed []string
	// Kept holds the reasons a session needs a person that this process has
	// not reported yet. The pipeline prints each once.
	Kept []Kept
	// Waiting holds why a session is not ready yet. Nothing prints these.
	// They change on their own, and tests read them.
	Waiting []Kept
	// DeriveNext holds sessions whose only missing condition is that metrics
	// were not derived from all of their landed files. The pipeline adds them
	// to the next derivation, because a session that no longer lands anything
	// is otherwise not derived again.
	DeriveNext []string
	Errors     []error
}

// Kept is a session kept this pass, and why. An empty Session means every
// marked session.
type Kept struct{ Session, Reason string }

// Point is a place a removal reaches. The steps are 0 to 7:
//
//	{0, done}                       the marker says removing
//	{1, file, k} ... {1, dirs}      after each source file, then its directories
//	{2, unlocked} {2, renamed} {2, deleting} {2, done}   <root>/S
//	{3, unlocked} {3, renamed} {3, deleting} {3, done}   <root>/_conversations/S
//	{4, file, k} ... {4, done}      after each spool file
//	{5, done} {6, done}             push.state and metrics.state, once per pass
//	{7, done}                       the marker is deleted
//
// Steps 5 and 6 run once for every session that finished step 4 in the
// pass, so their Session is empty. deleting comes after the first entry of
// the renamed tree is deleted, and only when it has one.
type Point struct {
	Step    int
	Where   string
	Session string
	// N counts the files inside step 1 or step 4, from 1.
	N int
}

// The places inside a step.
const (
	WhereDone     = "done"
	WhereFile     = "file"
	WhereDirs     = "dirs"
	WhereUnlocked = "unlocked"
	WhereRenamed  = "renamed"
	WhereDeleting = "deleting"
)

// crash is what a Hook returned. Nothing after it runs.
type crash struct{ err error }

func (c crash) Error() string { return "stopped by the hook: " + c.err.Error() }
func (c crash) Unwrap() error { return c.err }

// stop is a check inside a removal that refused. The session is left for a
// person, and its marker keeps saying removing.
type stop struct{ path, why string }

func (s stop) Error() string { return s.path + " " + s.why }

// verdict is the answer to one condition. An empty reason means it holds.
type verdict struct {
	reason string
	// blocked means a person has to act. Otherwise the session waits.
	blocked bool
	// derive means the reason is only that metrics were not derived yet.
	derive bool
	// every means the reason applies to every marked session, and the pass
	// stops deciding.
	every bool
}

func waiting(format string, a ...any) verdict { return verdict{reason: fmt.Sprintf(format, a...)} }
func blocked(format string, a ...any) verdict {
	return verdict{reason: fmt.Sprintf(format, a...), blocked: true}
}

// Pass removes every marked session that is sent and that its policy lets
// go, up to MaxPerPass, and finishes any removal an earlier pass left.
func (r *Remover) Pass(in Input) *Result {
	res := &Result{}
	if r.said == nil {
		r.said, r.stopped, r.last = map[string]map[string]bool{}, map[string]bool{}, map[string]time.Time{}
	}
	root := r.Zone.Root()
	// A directory an earlier pass renamed away and did not finish deleting.
	// Nothing lists it, so it can wait, but nothing else deletes it either.
	if err := storage.SweepRemoved(root); errors.Is(err, storage.ErrUnsafeRemoved) {
		// A link at _removed leads out of the root. The storage functions
		// refuse it. The pass stops here too, before any session reaches
		// step 0. Otherwise step 1 would delete a session's source, step 2
		// would be refused, and the session would stay half removed until a
		// person replaced _removed.
		r.keep(res, "", fmt.Sprintf("%s is a symbolic link or not a directory; nothing is deleted or moved through it",
			filepath.Join(root, storage.RemovedDir)))
		return res
	} else if err != nil {
		res.Errors = append(res.Errors, fmt.Errorf("remove: %w", err))
	}
	names, err := r.markerNames()
	if err != nil {
		res.Errors = append(res.Errors, err)
		return res
	}
	if len(names) == 0 {
		return res
	}
	if reason := r.Blocked(in); reason != "" {
		r.keep(res, "", reason)
		return res
	}
	lock, err := storage.LockExport(root)
	if errors.Is(err, storage.ErrExportBusy) {
		res.Waiting = append(res.Waiting, Kept{Reason: "another pusher holds the export state"})
		return res
	}
	if err != nil {
		res.Errors = append(res.Errors, err)
		return res
	}
	defer func() { _ = lock.Unlock() }()

	p := &pass{r: r, in: in, root: root, now: r.now()}
	if p.sent, err = otlp.LoadSent(root); err != nil {
		res.Errors = append(res.Errors, err)
		return res
	}
	switch eps := p.sent.Endpoints(); {
	case len(eps) == 0:
		res.Waiting = append(res.Waiting, Kept{Reason: "not sent yet"})
		return res
	case len(eps) != 1 || eps[0] != in.Endpoint:
		r.keep(res, "", fmt.Sprintf("push.state records sending to %s; this pipeline sends to %s; which file reached which receiver cannot be told",
			strings.Join(eps, ", "), in.Endpoint))
		return res
	}
	if p.derived, err = metrics.LoadDerived(r.Zone); err != nil {
		res.Errors = append(res.Errors, err)
		return res
	}
	if p.spool, err = spoolOwners(root); err != nil {
		res.Errors = append(res.Errors, err)
		return res
	}

	var finished []*session
	reached := 0
	for _, c := range r.candidates(names) {
		if reached >= MaxPerPass {
			break
		}
		if r.stopped[c.id] {
			continue
		}
		if c.err != nil {
			r.keep(res, c.id, c.err.Error())
			continue
		}
		s := &session{id: c.id, markerPath: c.path, m: c.m}
		var v verdict
		if c.m.State == scenario.MarkerRemoving {
			// Decided by an earlier pass, which proved everything then. Only
			// the locks are needed to go on.
			v = p.lock(s)
		} else {
			v = p.decide(s)
		}
		if v.reason != "" {
			s.unlock()
			switch {
			case v.every:
				r.keep(res, "", v.reason)
			case v.blocked:
				r.keep(res, s.id, v.reason)
			default:
				res.Waiting = append(res.Waiting, Kept{Session: s.id, Reason: v.reason})
			}
			if v.derive {
				res.DeriveNext = append(res.DeriveNext, s.id)
			}
			if v.every {
				break
			}
			continue
		}
		reached++
		err := r.run(s)
		s.unlock()
		if err != nil {
			var c crash
			var st stop
			switch {
			case errors.As(err, &c):
				res.Errors = append(res.Errors, fmt.Errorf("%s: %w", s.id, err))
				return res
			case errors.As(err, &st):
				r.stopped[s.id] = true
				res.Errors = append(res.Errors, fmt.Errorf("%s: the removal stopped: %w; the session is left for a person", s.id, st))
			default:
				res.Errors = append(res.Errors, fmt.Errorf("%s: the removal did not finish, and the next pass goes on with it: %w", s.id, err))
			}
			continue
		}
		finished = append(finished, s)
	}
	if len(finished) == 0 {
		return res
	}

	// Steps 5 and 6, once for all of them. Each rewrites a whole state file,
	// so one write serves every session this pass removed. A line is dropped
	// only once its file is gone, so no file is ever sent or derived twice.
	ids := make([]string, 0, len(finished))
	owned := map[string]bool{}
	for _, s := range finished {
		ids = append(ids, s.id)
		owned[s.id] = true
	}
	if err := otlp.ForgetGone(root, ownedBy(owned), p.now); err != nil {
		res.Errors = append(res.Errors, err)
		return res
	}
	if err := r.hook(Point{Step: 5, Where: WhereDone}); err != nil {
		res.Errors = append(res.Errors, err)
		return res
	}
	if err := metrics.Forget(r.Zone, ids, p.now); err != nil {
		res.Errors = append(res.Errors, err)
		return res
	}
	if err := r.hook(Point{Step: 6, Where: WhereDone}); err != nil {
		res.Errors = append(res.Errors, err)
		return res
	}
	// Step 7. The marker goes last, so a session whose removal began is
	// finished by the next pass, whatever stopped this one.
	for _, s := range finished {
		if err := os.Remove(s.markerPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			res.Errors = append(res.Errors, fmt.Errorf("%s: %w", s.id, err))
			continue
		}
		delete(r.last, s.id)
		delete(r.said, s.id)
		res.Removed = append(res.Removed, s.id)
		if err := r.hook(Point{Step: 7, Where: WhereDone, Session: s.id}); err != nil {
			res.Errors = append(res.Errors, err)
			return res
		}
	}
	return res
}

func (r *Remover) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Remover) hook(p Point) error {
	if r.Hook == nil {
		return nil
	}
	if err := r.Hook(p); err != nil {
		return crash{err}
	}
	return nil
}

// keep reports a blocked reason once per process.
func (r *Remover) keep(res *Result, session, reason string) {
	if r.said[session] == nil {
		r.said[session] = map[string]bool{}
	}
	if r.said[session][reason] {
		return
	}
	r.said[session][reason] = true
	res.Kept = append(res.Kept, Kept{Session: session, Reason: reason})
}

// markerNames lists the marker files, in name order.
func (r *Remover) markerNames() ([]string, error) {
	items, err := os.ReadDir(filepath.Join(r.Source, scenario.MarkerDir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, it := range items {
		if it.Type().IsRegular() && scenario.IsMarkerName(it.Name()) {
			out = append(out, it.Name())
		}
	}
	return out, nil
}

type candidate struct {
	id   string
	path string
	m    *scenario.Marker
	err  error
}

// candidates reads every marker. A removal that has begun goes first, so a
// half-removed session is never left behind others that are only waiting.
func (r *Remover) candidates(names []string) []candidate {
	var removing, rest []candidate
	for _, name := range names {
		c := candidate{id: strings.TrimSuffix(name, ".json"), path: filepath.Join(r.Source, scenario.MarkerDir, name)}
		c.m, c.err = scenario.ReadMarker(c.path)
		if c.err == nil && c.m.State == scenario.MarkerRemoving {
			removing = append(removing, c)
			continue
		}
		rest = append(rest, c)
	}
	return append(removing, rest...)
}

// Blocked reports why no marked session can be removed with this input, or
// an empty string when nothing at the level of the pass prevents it. It
// checks what holds for every session or for none: that the pipeline sends
// everything a session produces, that the changes adapter lands the plugin
// output the build wrote, and that neither source directory is Claude
// Code's own. It reads no marker and nothing in the storage root. So the
// pipeline asks it before the first pass, and prints the answer, which then
// agrees with what every pass does.
func (r *Remover) Blocked(in Input) string {
	if in.Endpoint == "" {
		return "no export endpoint; nothing is sent, so nothing is removed"
	}
	if !in.SendLogs {
		return "export.otlp.logs is off; landed files and rounds are never sent"
	}
	if !in.SendMetrics {
		return "export.otlp.metrics is off; the metrics derived from a session are never sent"
	}
	want := resolved(filepath.Join(r.Source, "plugins", "data"))
	if r.Changes == nil {
		return fmt.Sprintf("the changes adapter is off; the plugin output the build wrote under %s would never be landed", want)
	}
	if got := resolved(r.ChangesRoot); got != want {
		return fmt.Sprintf("the changes adapter reads %s, not %s; the plugin output the build wrote would never be landed", r.ChangesRoot, want)
	}
	// A source directory that is one of Claude Code's own, holds one, or
	// lies inside one would let a marker path reach a real transcript. Each
	// source is compared with every place Claude Code may keep its files,
	// not only the one this process's environment selects. The pipeline can
	// run with CLAUDE_CONFIG_DIR or XDG_CONFIG_HOME set differently from the
	// Claude Code a person runs on the same machine.
	for _, src := range []string{r.Source, r.ChangesRoot} {
		for _, own := range claudeCodeDirs() {
			if overlaps(src, own) {
				return fmt.Sprintf("%s is, holds, or lies inside %s, where Claude Code keeps its own files; asz never removes there", src, own)
			}
		}
	}
	return ""
}

// claudeCodeDirs lists every directory where Claude Code may keep its
// files. These are the three places claudecode.ResolveSourceRoot chooses
// between: what CLAUDE_CONFIG_DIR names, XDG_CONFIG_HOME/claude, and
// ~/.claude. Each is listed with its projects and plugins/data, because a
// link can put either of them somewhere else, and each is resolved on its
// own. The directory itself is listed too, so a source anywhere inside it is
// refused. With no home directory and neither variable set, the list is
// empty: the machine has no such directory to protect.
func claudeCodeDirs() []string {
	var bases []string
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		bases = append(bases, d)
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		bases = append(bases, filepath.Join(d, "claude"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		bases = append(bases, filepath.Join(home, ".claude"))
	}
	dirs := make([]string, 0, 3*len(bases))
	for _, b := range bases {
		dirs = append(dirs, b, filepath.Join(b, "projects"), filepath.Join(b, "plugins", "data"))
	}
	return dirs
}

// overlaps reports whether a and b are the same directory, or one lies
// inside the other. They are compared as written, made absolute, and again
// with symbolic links resolved, so a link on either side cannot hide it.
func overlaps(a, b string) bool {
	la, lb := absolute(a), absolute(b)
	ra, rb := resolved(a), resolved(b)
	return under(la, lb) || under(lb, la) || under(ra, rb) || under(rb, ra)
}

// pass is what one removal pass reads once and shares between sessions.
type pass struct {
	r       *Remover
	in      Input
	root    string
	now     time.Time
	sent    *otlp.Sent
	derived *metrics.Derived
	// spool lists the spool files by the session that owns them.
	spool map[string][]string

	discovered bool
	discovery  verdict
	local      map[string]claudecode.Session
	changes    map[string]claudecodechanges.Session
}

// session is one marked session while a pass works on it.
type session struct {
	id          string
	markerPath  string
	m           *scenario.Marker
	sessionLock *storage.SessionLock
	chainLock   *storage.SessionLock
	local       claudecode.Session
	hasLocal    bool
	changes     claudecodechanges.Session
	hasChanges  bool
}

func (s *session) unlock() {
	if s.chainLock != nil {
		_ = s.chainLock.Unlock()
		s.chainLock = nil
	}
	if s.sessionLock != nil {
		_ = s.sessionLock.Unlock()
		s.sessionLock = nil
	}
}

// decide checks the conditions of one session in order, and holds its locks
// when every one of them holds.
func (p *pass) decide(s *session) verdict {
	policy, _ := s.m.Removal()
	if !policy.Immediate() {
		if t, ok := p.r.last[s.id]; ok && p.now.Before(t.Add(policy.Retain)) {
			return waiting("kept until %s", t.Add(policy.Retain).UTC().Format(time.RFC3339))
		}
	}
	if p.in.Busy[s.id] {
		return waiting("busy")
	}
	if v := p.lock(s); v.reason != "" {
		return v
	}
	if v := p.whole(s); v.reason != "" {
		return v
	}
	if v := p.covered(s); v.reason != "" {
		return v
	}
	landed, v := p.landed(s)
	if v.reason != "" {
		return v
	}
	if v := p.sentAll(landed.files, "not sent yet"); v.reason != "" {
		return v
	}
	if v := p.chainSent(s, landed); v.reason != "" {
		return v
	}
	if v := p.metricsSent(s, landed); v.reason != "" {
		return v
	}
	if !policy.Immediate() {
		if !landed.timed {
			return blocked("no landed record carries a time, so the retention cannot be counted")
		}
		if until := landed.last.Add(policy.Retain); p.now.Before(until) {
			return waiting("kept until %s", until.UTC().Format(time.RFC3339))
		}
	}
	return verdict{}
}

// lock takes the session's lock and its chain's lock. Each is taken only when
// its directory exists, because taking a lock creates its directory.
func (p *pass) lock(s *session) verdict {
	take := func(dir string, lockFn func(string) (*storage.SessionLock, error), busy error) (*storage.SessionLock, verdict) {
		fi, err := os.Lstat(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil, verdict{}
		case err != nil:
			return nil, blocked("%v", err)
		case !fi.IsDir():
			return nil, blocked("%s is a symbolic link or not a directory", dir)
		}
		l, err := lockFn(dir)
		if errors.Is(err, busy) {
			return nil, waiting("busy")
		}
		if err != nil {
			return nil, blocked("%v", err)
		}
		return l, verdict{}
	}
	var v verdict
	if s.sessionLock, v = take(p.r.Zone.SessionDir(s.id), storage.LockSession, storage.ErrSessionBusy); v.reason != "" {
		return v
	}
	s.chainLock, v = take(filepath.Join(p.root, conversationsDir, s.id), storage.LockChain, storage.ErrChainBusy)
	return v
}

// discover runs both adapters' discovery once per pass, and only when a
// session gets this far. A pass where every session is waiting on its
// retention discovers nothing.
func (p *pass) discover() verdict {
	if p.discovered {
		return p.discovery
	}
	p.discovered = true
	all := func(format string, a ...any) verdict {
		v := blocked(format, a...)
		v.every = true
		return v
	}
	sessions, warnings, err := claudecode.DiscoverWithWarnings(p.r.Source)
	if err == nil && len(warnings) > 0 {
		err = warnings[0]
	}
	if err != nil {
		p.discovery = all("discovery could not read everything under %s (%v); files of a session may be hidden", p.r.Source, err)
		return p.discovery
	}
	p.local = map[string]claudecode.Session{}
	for _, s := range sessions {
		p.local[s.ID] = s
	}
	changed, warnings, err := claudecodechanges.DiscoverWithWarnings(p.r.ChangesRoot)
	if err == nil && len(warnings) > 0 {
		err = warnings[0]
	}
	if err != nil {
		p.discovery = all("discovery could not read everything under %s (%v); files of a session may be hidden", p.r.ChangesRoot, err)
		return p.discovery
	}
	p.changes = map[string]claudecodechanges.Session{}
	for _, s := range changed {
		p.changes[s.ID] = s
	}
	return p.discovery
}

// whole checks that the build finished the session and nothing changed it
// since: the adapters find exactly the files the marker lists, each with the
// bytes the build wrote, and their filters take the session.
func (p *pass) whole(s *session) verdict {
	if v := p.discover(); v.reason != "" {
		return v
	}
	want := map[string]bool{}
	for _, f := range s.m.Files {
		want[f.Path] = true
	}
	found := map[string]bool{}
	s.local, s.hasLocal = p.local[s.id]
	for _, src := range s.local.Sources {
		found[src.Rel] = true
	}
	s.changes, s.hasChanges = p.changes[s.id]
	for _, src := range s.changes.Sources {
		found[changesPrefix+src.Rel] = true
	}
	for _, rel := range sortedKeys(found) {
		if !want[rel] {
			return blocked("%s is not a file the build wrote", p.source(rel))
		}
	}
	for _, f := range s.m.Files {
		abs := p.source(f.Path)
		fi, err := os.Lstat(abs)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return blocked("%s, which the build wrote, is missing", abs)
		case err != nil:
			return blocked("%v", err)
		case !fi.Mode().IsRegular():
			return blocked("%s is a symbolic link or not a regular file", abs)
		case !found[f.Path]:
			return blocked("%s, which the build wrote, is not found by either adapter's discovery", abs)
		case fi.Size() != f.Size:
			return blocked("%s changed after the build wrote it", abs)
		}
		sum, err := storage.FileDigest(abs)
		if err != nil {
			return blocked("%v", err)
		}
		if sum != f.SHA256 {
			return blocked("%s changed after the build wrote it", abs)
		}
	}
	if s.hasLocal && p.r.LocalMatch != nil && !p.r.LocalMatch(s.local) {
		return blocked("the local adapter's include and exclude leave %s out", s.id)
	}
	if s.hasChanges && p.r.ChangesMatch != nil && !p.r.ChangesMatch(s.changes) {
		return blocked("the changes adapter's include and exclude leave %s out", s.id)
	}
	return verdict{}
}

// covered checks that each adapter would land nothing of the session now.
// A source deleted with a line not yet landed would lose that line.
func (p *pass) covered(s *session) verdict {
	if s.hasLocal {
		ok, reason, err := p.r.Local.Covered(s.local)
		if err != nil {
			return blocked("%v", err)
		}
		if !ok {
			return waiting("%s", reason)
		}
	}
	if s.hasChanges {
		ok, reason, err := p.r.Changes.Covered(s.changes)
		if err != nil {
			return blocked("%v", err)
		}
		if !ok {
			return waiting("%s", reason)
		}
	}
	return verdict{}
}

// sentFile is a file that must be recorded as sent: its path under the root
// with forward slashes, and its digest now.
type sentFile struct{ rel, digest string }

// landedSet is what a session's landed files hold.
type landedSet struct {
	files  []sentFile
	maxSeq uint64
	// last is the latest record time, when timed.
	last  time.Time
	timed bool
}

// landed reads every landed file of the session once: its header, its digest
// and its latest record time. Every one must have come from a file the build
// wrote. That keeps a real session whose id a scenario reused, and whose
// transcript was copied beside the scenario's, from ever being removed.
func (p *pass) landed(s *session) (*landedSet, verdict) {
	files, err := storage.LandedFiles(p.r.Zone, s.id)
	if err != nil {
		return nil, blocked("%v", err)
	}
	if len(files) == 0 {
		return nil, blocked("nothing of it is landed, so nothing of it can be sent")
	}
	want := map[string]bool{}
	for _, f := range s.m.Files {
		want[f.Path] = true
	}
	set := &landedSet{}
	for _, lf := range files {
		rel := p.rel(lf.Path)
		info, err := readLanded(lf.Path)
		if err != nil {
			return nil, blocked("landed file %s cannot be read: %v", rel, err)
		}
		// The header names the adapter and its version, as name/version.
		src := info.Src
		switch name, _, _ := strings.Cut(info.Adapter, "/"); name {
		case claudecode.Name:
		case claudecodechanges.Name:
			src = changesPrefix + src
		default:
			return nil, blocked("landed file %s came through the adapter %q, which reads no file a build writes", rel, info.Adapter)
		}
		if info.Session != s.id || !want[src] {
			return nil, blocked("landed file %s names a source the build did not write", rel)
		}
		set.files = append(set.files, sentFile{rel: rel, digest: info.digest})
		set.maxSeq = max(set.maxSeq, lf.Seq)
		if info.timed && (!set.timed || info.last.After(set.last)) {
			set.last, set.timed = info.last, true
		}
	}
	if set.timed {
		p.r.last[s.id] = set.last
	}
	return set, verdict{}
}

// sentAll checks that every file was sent to this pipeline's endpoint with
// the digest it has now, and that no receiver rejected records of it. A
// blocked reason wins over a waiting one, since it will not change on its
// own.
func (p *pass) sentAll(files []sentFile, notYet string) verdict {
	var wait verdict
	for _, f := range files {
		d, ok := p.sent.Digest(f.rel)
		switch {
		case !ok:
			if wait.reason == "" {
				wait = waiting("%s", notYet)
			}
		case p.sent.Rejected(f.rel):
			return blocked("the receiver rejected records of %s", f.rel)
		case d != f.digest:
			return blocked("%s changed after it was sent", f.rel)
		case !p.sent.SentTo(p.in.Endpoint, f.rel, f.digest):
			return blocked("%s is not recorded as sent to %s", f.rel, p.in.Endpoint)
		}
	}
	return wait
}

// roundName is the name a chain gives a round, as the pusher reads it.
var roundName = regexp.MustCompile(`^r(\d{6,})-([0-9a-f]{12})\.sf$`)

// chainSent checks that the chain reaches the last landed file, and that
// every round of it is sent. A round still being written is never pushed, so
// it never passes.
func (p *pass) chainSent(s *session, landed *landedSet) verdict {
	const notYet = "not parsed or not sent yet"
	dir := filepath.Join(p.root, conversationsDir, s.id, "rounds")
	items, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return waiting(notYet)
	}
	if err != nil {
		return blocked("%v", err)
	}
	var rounds []sentFile
	var head string
	var headN uint64
	for _, it := range items {
		if it.IsDir() || !strings.HasSuffix(it.Name(), ".sf") {
			continue
		}
		path := filepath.Join(dir, it.Name())
		m := roundName.FindStringSubmatch(it.Name())
		if m == nil {
			return blocked("%s has a name no chain writes", p.rel(path))
		}
		n, _ := strconv.ParseUint(m[1], 10, 64)
		if head == "" || n > headN {
			head, headN = path, n
		}
		digest, err := storage.FileDigest(path)
		if err != nil {
			return blocked("%v", err)
		}
		rounds = append(rounds, sentFile{rel: p.rel(path), digest: digest})
	}
	if head == "" {
		return waiting(notYet)
	}
	hdr, err := readRoundHeader(head)
	if err != nil {
		return blocked("the head round %s cannot be read: %v", p.rel(head), err)
	}
	if hdr.Conversation != s.id || hdr.Session != s.id {
		return blocked("the head round %s names conversation %q and session %q, not %s", p.rel(head), hdr.Conversation, hdr.Session, s.id)
	}
	switch {
	case hdr.ThroughSeq < landed.maxSeq:
		return waiting(notYet)
	case hdr.ThroughSeq > landed.maxSeq:
		return blocked("the head round %s reaches sequence %d, past the last landed file, %d", p.rel(head), hdr.ThroughSeq, landed.maxSeq)
	}
	return p.sentAll(rounds, notYet)
}

// metricsSent checks that metrics were derived from every landed file, when
// the pipeline derives them, and that every spool file of the session is
// sent. A spool file of an earlier configuration counts either way.
func (p *pass) metricsSent(s *session, landed *landedSet) verdict {
	if p.r.Derives {
		for _, f := range landed.files {
			if !p.derived.Has(f.rel, f.digest) {
				v := waiting("metrics not derived yet")
				v.derive = true
				return v
			}
		}
	}
	var spool []sentFile
	for _, name := range p.spool[s.id] {
		path := filepath.Join(p.root, storage.SpoolDir, name)
		digest, err := storage.FileDigest(path)
		if err != nil {
			return blocked("%v", err)
		}
		spool = append(spool, sentFile{rel: storage.SpoolDir + "/" + name, digest: digest})
	}
	return p.sentAll(spool, "metrics not sent yet")
}

// source is a marker path as a path on disk.
func (p *pass) source(rel string) string {
	return filepath.Join(p.r.Source, filepath.FromSlash(rel))
}

// rel is a path under the storage root as push.state and metrics.state
// write it.
func (p *pass) rel(path string) string {
	rel, err := filepath.Rel(p.root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

// run takes a session through steps 0 to 4. The order is what makes each
// step safe to stop at and to run again:
//
//   - The source goes before any cursor. The cursors live in <root>/S, and a
//     source that outlived them would be read again from byte 0, landed under
//     new names and sent again.
//   - The landed files go before the chain. With them gone, nothing parses S
//     again, and every round left is already sent. The other way round, a
//     restart would parse the whole landed range into a new round 1, whose
//     name carries a new digest, and send it again.
//   - Each directory goes in one rename. Deleting one in place takes a call
//     per file, and a crash in the middle left a session with part of its
//     landed files and no index. A parse then published a round of
//     tombstones over what was left.
func (r *Remover) run(s *session) error {
	if s.m.State != scenario.MarkerRemoving {
		m := *s.m
		m.State = scenario.MarkerRemoving
		if err := scenario.WriteMarker(s.markerPath, &m); err != nil {
			return err
		}
		s.m = &m
		if err := r.hook(Point{Step: 0, Where: WhereDone, Session: s.id}); err != nil {
			return err
		}
	}
	if err := r.removeSource(s); err != nil {
		return err
	}
	root := r.Zone.Root()
	if err := r.removeTree(s, 2, r.Zone.SessionDir(s.id), s.id, &s.sessionLock); err != nil {
		return err
	}
	// The name is outside _conversations. The pusher and the page list every
	// directory there, and a round under a new path would be sent again.
	if err := r.removeTree(s, 3, filepath.Join(root, conversationsDir, s.id), s.id+".chain", &s.chainLock); err != nil {
		return err
	}
	return r.removeSpool(s)
}

// removeSource is step 1: every file the marker lists, then the directories
// named for the session. A file already gone was deleted by a removal that
// stopped part way, so it counts as done.
func (r *Remover) removeSource(s *session) error {
	source := resolved(r.Source)
	for k, f := range s.m.Files {
		abs := filepath.Join(r.Source, filepath.FromSlash(f.Path))
		fi, err := os.Lstat(abs)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return err
		case !fi.Mode().IsRegular():
			return stop{abs, "is a symbolic link or not a regular file"}
		default:
			if !under(resolved(filepath.Dir(abs)), source) {
				return stop{abs, "is reached through a symbolic link that leads out of " + r.Source}
			}
			if fi.Size() != f.Size {
				return stop{abs, "changed after the build wrote it"}
			}
			sum, err := storage.FileDigest(abs)
			if err != nil {
				return err
			}
			if sum != f.SHA256 {
				return stop{abs, "changed after the build wrote it"}
			}
			if err := os.Remove(abs); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
		if err := r.hook(Point{Step: 1, Where: WhereFile, Session: s.id, N: k + 1}); err != nil {
			return err
		}
	}
	// Only directories named for the session, and only when empty. A project
	// directory, which every session of a feed shares, is never touched, and
	// neither is the plugin's directory nor the marker directory.
	for _, dir := range dirsNamedFor(s.id, s.m.Files) {
		abs := filepath.Join(r.Source, filepath.FromSlash(dir))
		fi, err := os.Lstat(abs)
		if err != nil || !fi.IsDir() {
			continue
		}
		if !under(resolved(abs), source) {
			return stop{abs, "is reached through a symbolic link that leads out of " + r.Source}
		}
		items, err := os.ReadDir(abs)
		if err != nil {
			return err
		}
		if len(items) > 0 {
			continue
		}
		if err := os.Remove(abs); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return r.hook(Point{Step: 1, Where: WhereDirs, Session: s.id})
}

// removeTree is steps 2 and 3: one directory, renamed away whole and then
// deleted. The lock inside it is released first, on every platform, since
// Windows cannot rename a directory that holds a file opened with no
// sharing, which is how a lock is held there.
func (r *Remover) removeTree(s *session, step int, dir, name string, held **storage.SessionLock) error {
	fi, err := os.Lstat(dir)
	switch {
	case err == nil && !fi.IsDir():
		return stop{dir, "is a symbolic link or not a directory"}
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if *held != nil {
		_ = (*held).Unlock()
		*held = nil
	}
	if err := r.hook(Point{Step: step, Where: WhereUnlocked, Session: s.id}); err != nil {
		return err
	}
	err = storage.RemoveDir(r.Zone.Root(), dir, name, func(where string) error {
		return r.hook(Point{Step: step, Where: where, Session: s.id})
	})
	if err != nil {
		var c crash
		if !errors.As(err, &c) && errors.Is(err, storage.ErrExists) {
			return stop{filepath.Join(r.Zone.Root(), storage.RemovedDir, name), "is left from an earlier removal and could not be deleted"}
		}
		return err
	}
	return r.hook(Point{Step: step, Where: WhereDone, Session: s.id})
}

// removeSpool is step 4: every spool file the session owns.
func (r *Remover) removeSpool(s *session) error {
	dir := filepath.Join(r.Zone.Root(), storage.SpoolDir)
	items, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	k := 0
	for _, it := range items {
		if it.IsDir() {
			continue
		}
		if owner, ok := metrics.SpoolOwner(it.Name()); !ok || owner != s.id {
			continue
		}
		// A spool file is read-only, as a landed file is. DeleteTree makes it
		// writable before it deletes it.
		if err := storage.DeleteTree(filepath.Join(dir, it.Name()), nil); err != nil {
			return err
		}
		k++
		if err := r.hook(Point{Step: 4, Where: WhereFile, Session: s.id, N: k}); err != nil {
			return err
		}
	}
	return r.hook(Point{Step: 4, Where: WhereDone, Session: s.id})
}

// dirsNamedFor lists, deepest first, the directories of the marker's files
// that sit at or below a path segment equal to the session id.
func dirsNamedFor(session string, files []scenario.MarkerFile) []string {
	seen := map[string]bool{}
	for _, f := range files {
		parts := strings.Split(f.Path, "/")
		for i := 0; i < len(parts)-1; i++ {
			if parts[i] != session {
				continue
			}
			for j := i + 1; j < len(parts); j++ {
				seen[strings.Join(parts[:j], "/")] = true
			}
			break
		}
	}
	out := sortedKeys(seen)
	sort.SliceStable(out, func(i, j int) bool {
		di, dj := strings.Count(out[i], "/"), strings.Count(out[j], "/")
		if di != dj {
			return di > dj
		}
		return out[i] > out[j]
	})
	return out
}

// ownedBy says which lines of push.state belong to the sessions given:
// their landed files, their rounds and the spool files they own.
func ownedBy(ids map[string]bool) func(rel string) bool {
	return func(rel string) bool {
		first, rest, ok := strings.Cut(rel, "/")
		if !ok || rest == "" {
			return false
		}
		switch first {
		case conversationsDir:
			conv, _, _ := strings.Cut(rest, "/")
			return ids[conv]
		case storage.SpoolDir:
			owner, ok := metrics.SpoolOwner(rest)
			return ok && !strings.Contains(rest, "/") && ids[owner]
		}
		return ids[first]
	}
}

// spoolOwners lists the spool files by the session that owns them. A
// request the receiver adapter put there names no session and is left out.
func spoolOwners(root string) (map[string][]string, error) {
	items, err := os.ReadDir(filepath.Join(root, storage.SpoolDir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string][]string{}, nil
		}
		return nil, err
	}
	out := map[string][]string{}
	for _, it := range items {
		if it.IsDir() {
			continue
		}
		if owner, ok := metrics.SpoolOwner(it.Name()); ok {
			out[owner] = append(out[owner], it.Name())
		}
	}
	return out, nil
}

// landedInfo is what a removal reads from a landed file.
type landedInfo struct {
	Adapter string `json:"adapter"`
	Src     string `json:"src"`
	Session string `json:"session"`
	digest  string
	last    time.Time
	timed   bool
}

// readLanded reads a landed file once: the header on its first line, the
// SHA-256 of all of it, and the latest record time, by the rule the pusher
// uses to stamp a file. Lines reach a megabyte and more, so a line is read
// to its newline rather than with a scanner.
func readLanded(path string) (landedInfo, error) {
	var info landedInfo
	f, err := os.Open(path)
	if err != nil {
		return info, err
	}
	defer f.Close()
	h := sha256.New()
	br := bufio.NewReaderSize(io.TeeReader(f, h), 1<<20)
	first := true
	var hi int64
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimRight(line, "\n")
			if first {
				if jerr := json.Unmarshal(line, &info); jerr != nil {
					return info, fmt.Errorf("the header: %w", jerr)
				}
				first = false
			} else if ns, ok := sessiondata.LineTime(line); ok && (!info.timed || ns > hi) {
				hi, info.timed = ns, true
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return info, err
		}
	}
	if first {
		return info, errors.New("it has no header")
	}
	info.digest = hex.EncodeToString(h.Sum(nil))
	if info.timed {
		info.last = time.Unix(0, hi).UTC()
	}
	return info, nil
}

// roundHeader is the part of a round's first line a removal reads. The field
// names are Session Flow's, written out because its package is server side.
type roundHeader struct {
	Conversation string `json:"conversation"`
	Session      string `json:"session"`
	ThroughSeq   uint64 `json:"through_seq"`
}

func readRoundHeader(path string) (roundHeader, error) {
	var hdr roundHeader
	f, err := os.Open(path)
	if err != nil {
		return hdr, err
	}
	defer f.Close()
	line, err := bufio.NewReader(f).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return hdr, err
	}
	return hdr, json.Unmarshal(bytes.TrimRight(line, "\n"), &hdr)
}

// absolute is a path made absolute and clean, with nothing resolved.
func absolute(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return filepath.Clean(p)
}

// resolved is a path made absolute and clean, with symbolic links resolved
// as far as the path exists.
func resolved(p string) string {
	abs := absolute(p)
	if target, err := filepath.EvalSymlinks(abs); err == nil {
		return target
	}
	parent := filepath.Dir(abs)
	if parent == abs {
		return abs
	}
	return filepath.Join(resolved(parent), filepath.Base(abs))
}

// under reports whether p is dir or inside it. Both are absolute and clean.
func under(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
