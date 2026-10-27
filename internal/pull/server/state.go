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

package server

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/boundedjson"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/format"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// stateSchema is every server state record's schema.
const stateSchema = "pull-state/1"

// timeLayout is how a record writes a time: UTC with nine fractional digits.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

// maxRecord bounds a small state record. A conversation's history and an
// archive's names are streamed instead: each grows with its list.
const maxRecord = 1 << 20

// errState is server state that does not read as it must: a storage-state
// problem, never taken for an empty default.
var errState = errors.New("server: the pull state is not as it must be")

func stateError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errState, fmt.Sprintf(format, args...))
}

// paths are where a production root keeps what it serves pulls with, all
// under _pull/.
type paths struct{ root string }

func (p paths) pull() string                { return filepath.Join(p.root, storage.PullDir) }
func (p paths) state() string               { return filepath.Join(p.pull(), "_state") }
func (p paths) freezes() string             { return filepath.Join(p.pull(), "_freezes") }
func (p paths) freeze(id string) string     { return filepath.Join(p.freezes(), id+".json") }
func (p paths) freezeLock(id string) string { return filepath.Join(p.freezes(), id+".lock") }
func (p paths) building() string            { return filepath.Join(p.pull(), "building") }
func (p paths) build(id string) string      { return filepath.Join(p.building(), id) }
func (p paths) conversations() string       { return filepath.Join(p.pull(), "conversations") }
func (p paths) conversation(c string) string {
	return filepath.Join(p.conversations(), c)
}
func (p paths) history(c string) string { return filepath.Join(p.conversation(c), "_history") }
func (p paths) convRoot(c string) string {
	return filepath.Join(p.conversation(c), storage.RootLockDir)
}
func (p paths) removing(c string) string {
	return filepath.Join(p.conversation(c), storage.RemovingDir)
}
func (p paths) snapshot(c, tag string) string {
	return filepath.Join(p.conversation(c), tag)
}

// snapshot files.
func snapshotRecordPath(s string) string { return filepath.Join(s, format.SnapshotRecord) }
func lastUsePath(s string) string        { return filepath.Join(s, format.LastUseRecord) }
func leasesDir(s string) string          { return filepath.Join(s, storage.LeasesDir) }
func leasePath(s, lease string) string   { return filepath.Join(leasesDir(s), lease+".json") }
func requestsDir(s string) string        { return filepath.Join(s, format.RequestsDir) }
func archivesDir(s string) string        { return filepath.Join(s, format.ArchivesDir) }
func archivePath(s, id, ext string) string {
	return filepath.Join(archivesDir(s), id+ext)
}

// newID is 32 random bytes as 64 lower-case hexadecimal characters.
func newID() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func stamp(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseStamp(s string) (time.Time, error) {
	t, err := time.Parse(timeLayout, s)
	if err != nil || stamp(t) != s {
		return time.Time{}, fmt.Errorf("%q is not a UTC time with nine fractional digits", s)
	}
	return t, nil
}

// readRecord reads a state record strictly: the bounded scan refuses
// duplicate members, escaped twins included, and oversized values; every
// member is one the record has, the required ones are there, and none is
// null. A record that is missing is os.ErrNotExist; any other failure is a
// storage-state error.
func readRecord(path string, limit int64, v any, required ...string) error {
	// A control is read where it is, never through a link to a record
	// somewhere else.
	f, err := storage.OpenEvidence(path, nil)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	f.Close()
	if err != nil {
		return stateError("%s: %v", path, err)
	}
	if int64(len(data)) > limit {
		return stateError("%s is larger than a record may be", path)
	}
	if err := decodeRecord(data, v, required...); err != nil {
		return stateError("%s: %v", path, err)
	}
	return nil
}

func decodeRecord(data []byte, v any, required ...string) error {
	s := boundedjson.New(bytes.NewReader(data))
	if err := s.Skip(); err != nil {
		return err
	}
	if _, err := s.Next(); !errors.Is(err, io.EOF) {
		return errors.New("a value follows the record")
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		return err
	}
	for name, raw := range members {
		if string(bytes.TrimSpace(raw)) == "null" {
			return fmt.Errorf("%s is null", name)
		}
	}
	for _, name := range append([]string{"schema"}, required...) {
		if _, ok := members[name]; !ok {
			return fmt.Errorf("%s is missing", name)
		}
	}
	if raw, ok := members["problem"]; ok {
		if err := checkProblemMembers(raw); err != nil {
			return fmt.Errorf("problem: %w", err)
		}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	var schema struct {
		Schema string `json:"schema"`
	}
	_ = json.Unmarshal(data, &schema)
	if schema.Schema != stateSchema {
		return fmt.Errorf("schema %q is not %s", schema.Schema, stateSchema)
	}
	return nil
}

// scanRecord reads a state record member by member as it streams in, so a
// record that grows with a list is never held whole: each member is read
// by its reader, a member with none refuses the record, every one in
// required must be there, schema must be this one, and nothing may follow.
// The bounded scan refuses repeated members and oversized values. A record
// that is missing is os.ErrNotExist.
func scanRecord(path string, read map[string]func(*boundedjson.Scanner) error, required ...string) error {
	f, err := storage.OpenEvidence(path, nil)
	if err != nil {
		return err
	}
	defer f.Close()
	s := boundedjson.New(f)
	if err := scanMembers(s, read, required); err != nil {
		return stateError("%s: %v", path, err)
	}
	return nil
}

func scanMembers(s *boundedjson.Scanner, read map[string]func(*boundedjson.Scanner) error, required []string) error {
	t, err := s.Next()
	if err != nil {
		return err
	}
	if t.Kind != boundedjson.Object {
		return errors.New("not an object")
	}
	seen := map[string]bool{}
	for {
		t, err := s.Next()
		if err != nil {
			return err
		}
		if t.Kind == boundedjson.ObjectEnd {
			break
		}
		name := t.Text
		if name == "schema" {
			v, err := scanString(s)
			if err == nil && v != stateSchema {
				err = fmt.Errorf("%q is not %s", v, stateSchema)
			}
			if err != nil {
				return fmt.Errorf("schema: %w", err)
			}
			seen[name] = true
			continue
		}
		fn, ok := read[name]
		if !ok {
			return fmt.Errorf("%s is not a member of this record", name)
		}
		if err := fn(s); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		seen[name] = true
	}
	for _, name := range append([]string{"schema"}, required...) {
		if !seen[name] {
			return fmt.Errorf("%s is missing", name)
		}
	}
	if _, err := s.Next(); !errors.Is(err, io.EOF) {
		return errors.New("a value follows the record")
	}
	return nil
}

// scanString reads a string, never null.
func scanString(s *boundedjson.Scanner) (string, error) {
	t, err := s.Next()
	if err != nil {
		return "", err
	}
	if t.Kind != boundedjson.String {
		return "", errors.New("not a string")
	}
	return t.Text, nil
}

// scanStrings reads an array of strings, giving each to each in order. An
// array of more than most is refused before its next one is kept.
func scanStrings(s *boundedjson.Scanner, most int, each func(string) error) error {
	t, err := s.Next()
	if err != nil {
		return err
	}
	if t.Kind != boundedjson.Array {
		return errors.New("not an array")
	}
	for n := 0; ; n++ {
		t, err := s.Next()
		if err != nil {
			return err
		}
		if t.Kind == boundedjson.ArrayEnd {
			return nil
		}
		if t.Kind != boundedjson.String {
			return errors.New("an element is not a string")
		}
		if n == most {
			return fmt.Errorf("more than %d", most)
		}
		if err := each(t.Text); err != nil {
			return err
		}
	}
}

// checkProblemMembers checks a saved problem's members as an answer's are:
// type, title, detail and status are there, none is null, and a member the
// problem's type adds is there only with a value, never as an empty string
// or a zero that would read as its absence.
func checkProblemMembers(raw json.RawMessage) error {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return err
	}
	for name, value := range members {
		switch v := string(bytes.TrimSpace(value)); {
		case v == "null":
			return fmt.Errorf("%s is null", name)
		case (name == "conversation" || name == "freeze" || name == "snapshot" || name == "archive") && v == `""`,
			name == "attempt" && v == "0":
			return fmt.Errorf("%s is there with no value", name)
		}
	}
	for _, name := range []string{"type", "title", "detail", "status"} {
		if _, ok := members[name]; !ok {
			return fmt.Errorf("%s is missing", name)
		}
	}
	return nil
}

// replaceRecord writes a record whole through replace-record.
func replaceRecord(path string, v any, files storage.FileSystem) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return storage.ReplaceControlDurable(path, 0o644, func(w io.Writer) error {
		_, err := w.Write(append(data, '\n'))
		return err
	}, files)
}

// newRecord writes a record that must not exist yet, durably.
func newRecord(path string, v any, files storage.FileSystem) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return storage.WriteNewDurable(path, 0o444, func(w io.Writer) error {
		_, err := w.Write(append(data, '\n'))
		return err
	}, files)
}

// state is _pull/_state: the greatest tag the server reserved, across
// conversations, and the one publication in progress.
type state struct {
	Schema      string      `json:"schema"`
	GreatestTag string      `json:"greatest_tag"`
	Publishing  *publishing `json:"publishing,omitempty"`
}

type publishing struct {
	Conversation   string `json:"conversation"`
	Build          string `json:"build"`
	Tag            string `json:"tag"`
	ManifestDigest string `json:"manifest_digest"`
}

func (s *state) check() error {
	if s.GreatestTag != "" {
		if _, err := storage.ParseTag(s.GreatestTag); err != nil {
			return err
		}
	}
	if p := s.Publishing; p != nil {
		switch {
		case storage.CheckNamePart(p.Conversation) != nil, !storage.Digest64(p.Build), !storage.Digest64(p.ManifestDigest):
			return errors.New("publishing is not a publication")
		case p.Tag != s.GreatestTag:
			return errors.New("the publication's tag is not the greatest tag")
		}
	}
	return nil
}

func readState(p paths) (*state, error) {
	var s state
	if err := readRecord(p.state(), maxRecord, &s, "greatest_tag"); err != nil {
		return nil, err
	}
	if err := s.check(); err != nil {
		return nil, stateError("%s: %v", p.state(), err)
	}
	return &s, nil
}

// history is a conversation's _history: the greatest tag it reserved, and
// every tag removed, for good.
type history struct {
	Schema       string   `json:"schema"`
	Conversation string   `json:"conversation"`
	GreatestTag  string   `json:"greatest_tag"`
	Removed      []string `json:"removed"`
}

// readHistory reads a conversation's history as it streams in: its removed
// tags grow with its pulls, and are each a tag, in increasing order, none
// past its greatest tag.
func readHistory(p paths, conversation string) (*history, error) {
	h := history{Schema: stateSchema}
	path := p.history(conversation)
	err := scanRecord(path, map[string]func(*boundedjson.Scanner) error{
		"conversation": func(s *boundedjson.Scanner) (err error) { h.Conversation, err = scanString(s); return },
		"greatest_tag": func(s *boundedjson.Scanner) (err error) { h.GreatestTag, err = scanString(s); return },
		"removed": func(s *boundedjson.Scanner) error {
			h.Removed = []string{}
			return scanStrings(s, -1, func(tag string) error {
				if _, err := storage.ParseTag(tag); err != nil {
					return err
				}
				if n := len(h.Removed); n > 0 && tag <= h.Removed[n-1] {
					return fmt.Errorf("%s does not come after %s", tag, h.Removed[n-1])
				}
				h.Removed = append(h.Removed, tag)
				return nil
			})
		},
	}, "conversation", "greatest_tag", "removed")
	if err != nil {
		return nil, err
	}
	if h.Conversation != conversation {
		return nil, stateError("%s names conversation %q", path, h.Conversation)
	}
	if h.GreatestTag != "" {
		if _, err := storage.ParseTag(h.GreatestTag); err != nil {
			return nil, stateError("%s: %v", path, err)
		}
	}
	if n := len(h.Removed); n > 0 && h.Removed[n-1] > h.GreatestTag {
		return nil, stateError("%s removed %s, past its greatest tag", path, h.Removed[n-1])
	}
	return &h, nil
}

func (h *history) removed(tag string) bool {
	_, found := slices.BinarySearch(h.Removed, tag)
	return found
}

// remove records tag removed, in its place in the increasing list.
func (h *history) remove(tag string) {
	if i, found := slices.BinarySearch(h.Removed, tag); !found {
		h.Removed = slices.Insert(h.Removed, i, tag)
	}
	if h.GreatestTag < tag {
		h.GreatestTag = tag
	}
}

// The states of a job.
const (
	jobBuilding = "building"
	jobReady    = "ready"
	jobFailed   = "failed"
)

// freezeJob is _pull/_freezes/<freeze>.json.
type freezeJob struct {
	Schema       string   `json:"schema"`
	Freeze       string   `json:"freeze"`
	Conversation string   `json:"conversation"`
	Attempt      uint64   `json:"attempt"`
	Started      bool     `json:"started"`
	State        string   `json:"state"`
	Snapshot     string   `json:"snapshot,omitempty"`
	Problem      *Problem `json:"problem,omitempty"`
	FinishedAt   string   `json:"finished_at,omitempty"`
}

func (j *freezeJob) check(id string) error {
	switch {
	case j.Freeze != id:
		return fmt.Errorf("it names freeze %q", j.Freeze)
	case storage.CheckNamePart(j.Conversation) != nil:
		return errors.New("its conversation is not a name")
	case j.Attempt == 0:
		return errors.New("its attempt is not positive")
	}
	switch j.State {
	case jobBuilding:
		if j.Snapshot != "" || j.Problem != nil || j.FinishedAt != "" {
			return errors.New("a building job holds a result")
		}
	case jobReady:
		if !j.Started || j.Problem != nil || j.FinishedAt == "" {
			return errors.New("a ready job is not as one must be")
		}
		if _, err := storage.ParseTag(j.Snapshot); err != nil {
			return err
		}
	case jobFailed:
		if j.Snapshot != "" || j.Problem == nil || j.FinishedAt == "" {
			return errors.New("a failed job is not as one must be")
		}
		if err := checkFreezeProblem(j.Problem, j, id); err != nil {
			return fmt.Errorf("its problem: %w", err)
		}
	default:
		return fmt.Errorf("state %q", j.State)
	}
	if j.FinishedAt != "" {
		if _, err := parseStamp(j.FinishedAt); err != nil {
			return err
		}
	}
	return nil
}

// checkProblem checks a saved problem is one an answer can carry: a type,
// a title and a status from 400 to 599.
func checkProblem(p *Problem) error {
	switch {
	case p.Type == "" || p.Title == "":
		return errors.New("it has no type or no title")
	case p.Status < 400 || p.Status > 599:
		return fmt.Errorf("status %d is not an error's", p.Status)
	}
	return nil
}

// freezeProblems are the statuses a classified source failure is saved
// with, by type; the recovery limit's is checked on its own.
var freezeProblems = map[string][]int{
	"about:blank":               {http.StatusNotFound, http.StatusConflict, http.StatusGone},
	format.ProblemSourceInvalid: {http.StatusUnprocessableEntity},
	format.ProblemSourceRead:    {http.StatusServiceUnavailable},
	format.ProblemSourceChange:  {http.StatusServiceUnavailable},
	format.ProblemStorageState:  {http.StatusInternalServerError},
}

// checkFreezeProblem checks a failed freeze's problem: a classified source
// failure, which names nothing, or the recovery limit, which names this
// conversation, this freeze and its attempt.
func checkFreezeProblem(p *Problem, j *freezeJob, id string) error {
	if err := checkProblem(p); err != nil {
		return err
	}
	if p.Type == format.ProblemFreezeFailed {
		if p.Status != http.StatusInternalServerError || p.Conversation != j.Conversation || p.Freeze != id ||
			p.Attempt != j.Attempt || p.Attempt < recoveryLimit || p.Snapshot != "" || p.Archive != "" {
			return errors.New("it is not this freeze's recovery limit")
		}
		return nil
	}
	if !slices.Contains(freezeProblems[p.Type], p.Status) {
		return fmt.Errorf("type %q with status %d is not a source failure", p.Type, p.Status)
	}
	if p.Conversation != "" || p.Freeze != "" || p.Snapshot != "" || p.Archive != "" || p.Attempt != 0 {
		return errors.New("a source failure names a job")
	}
	return nil
}

func readFreeze(p paths, id string) (*freezeJob, error) {
	var j freezeJob
	if err := readRecord(p.freeze(id), maxRecord, &j, "freeze", "conversation", "attempt", "started", "state"); err != nil {
		return nil, err
	}
	if err := j.check(id); err != nil {
		return nil, stateError("%s: %v", p.freeze(id), err)
	}
	return &j, nil
}

// snapshotRecord is S/_snapshot: what the snapshot is, fixed once
// published, but for the supersession recorded when a later one is.
type snapshotRecord struct {
	Schema         string `json:"schema"`
	Conversation   string `json:"conversation"`
	Session        string `json:"session"`
	Tag            string `json:"tag"`
	ManifestDigest string `json:"manifest_digest"`
	SupersededBy   string `json:"superseded_by,omitempty"`
}

func readSnapshot(dir, conversation, tag string) (*snapshotRecord, error) {
	var r snapshotRecord
	if err := readRecord(snapshotRecordPath(dir), maxRecord, &r, "conversation", "session", "tag", "manifest_digest"); err != nil {
		return nil, err
	}
	switch {
	case r.Conversation != conversation || r.Tag != tag:
		return nil, stateError("%s names %s at %s", snapshotRecordPath(dir), r.Conversation, r.Tag)
	case storage.CheckNamePart(r.Session) != nil || !storage.Digest64(r.ManifestDigest):
		return nil, stateError("%s is not a snapshot record", snapshotRecordPath(dir))
	case r.SupersededBy != "" && r.SupersededBy <= tag:
		return nil, stateError("%s is superseded by an earlier tag", snapshotRecordPath(dir))
	}
	return &r, nil
}

// The states of a snapshot's last use.
const (
	useActive  = "active"
	useExpired = "expired"
)

// lastUse is S/_last_use, the one clock a snapshot expires by.
type lastUse struct {
	Schema   string `json:"schema"`
	Snapshot string `json:"snapshot"`
	LastUse  string `json:"last_use"`
	State    string `json:"state"`
}

func readLastUse(dir, tag string) (*lastUse, time.Time, error) {
	var u lastUse
	if err := readRecord(lastUsePath(dir), maxRecord, &u, "snapshot", "last_use", "state"); err != nil {
		return nil, time.Time{}, err
	}
	at, err := parseStamp(u.LastUse)
	if err != nil || u.Snapshot != tag || (u.State != useActive && u.State != useExpired) {
		return nil, time.Time{}, stateError("%s is not this snapshot's last use", lastUsePath(dir))
	}
	return &u, at, nil
}

// The states of a lease.
const (
	leaseOpen     = "open"
	leaseReleased = "released"
)

// leaseRecord is S/_leases/<lease>.json, bound to its snapshot and to the
// hash of the token that was granted it.
type leaseRecord struct {
	Schema    string `json:"schema"`
	Lease     string `json:"lease"`
	Snapshot  string `json:"snapshot"`
	TokenHash string `json:"token_hash"`
	State     string `json:"state"`
}

func readLease(dir, tag, lease string) (*leaseRecord, error) {
	var l leaseRecord
	if err := readRecord(leasePath(dir, lease), maxRecord, &l, "lease", "snapshot", "token_hash", "state"); err != nil {
		return nil, err
	}
	if l.Lease != lease || l.Snapshot != tag || !storage.Digest64(l.TokenHash) || (l.State != leaseOpen && l.State != leaseReleased) {
		return nil, stateError("%s is not this lease", leasePath(dir, lease))
	}
	return &l, nil
}

// archiveJob is S/_archives/<id>.json, and a retained failure's
// <id>.failed-<attempt>.json.
type archiveJob struct {
	Schema   string   `json:"schema"`
	Snapshot string   `json:"snapshot"`
	Archive  string   `json:"archive"`
	Attempt  uint64   `json:"attempt"`
	Started  bool     `json:"started"`
	State    string   `json:"state"`
	Bytes    uint64   `json:"bytes,omitempty"`
	Digest   string   `json:"digest,omitempty"`
	Problem  *Problem `json:"problem,omitempty"`
}

func (j *archiveJob) check(tag, id string) error {
	switch {
	case j.Snapshot != tag || j.Archive != id:
		return errors.New("it names another archive")
	case j.Attempt == 0:
		return errors.New("its attempt is not positive")
	}
	switch j.State {
	case jobBuilding:
		if j.Bytes != 0 || j.Digest != "" || j.Problem != nil {
			return errors.New("a building archive holds a result")
		}
	case jobReady:
		if !j.Started || j.Bytes == 0 || !storage.Digest64(j.Digest) || j.Problem != nil {
			return errors.New("a ready archive is not as one must be")
		}
	case jobFailed:
		if j.Bytes != 0 || j.Digest != "" || j.Problem == nil {
			return errors.New("a failed archive is not as one must be")
		}
		p := j.Problem
		if err := checkProblem(p); err != nil {
			return fmt.Errorf("its problem: %w", err)
		}
		if p.Type != format.ProblemArchiveFailed || p.Status != http.StatusInternalServerError || p.Snapshot != tag ||
			p.Archive != id || p.Attempt != j.Attempt || p.Conversation != "" || p.Freeze != "" {
			return errors.New("its problem is not this archive's failure")
		}
	default:
		return fmt.Errorf("state %q", j.State)
	}
	return nil
}

func readArchive(path, tag, id string) (*archiveJob, error) {
	var j archiveJob
	if err := readRecord(path, maxRecord, &j, "snapshot", "archive", "attempt", "started", "state"); err != nil {
		return nil, err
	}
	if err := j.check(tag, id); err != nil {
		return nil, stateError("%s: %v", path, err)
	}
	return &j, nil
}

// archiveNames is S/_archives/<id>.names: the names an archive was asked
// for, fixed when its job was accepted.
type archiveNames struct {
	Schema   string   `json:"schema"`
	Snapshot string   `json:"snapshot"`
	Archive  string   `json:"archive"`
	Files    []string `json:"files"`
}

// readNames reads an archive's names as they stream in: at most count of
// them, the next refused before it is kept, each after the one before, and
// naming the archive they hash to.
func readNames(path, tag, id string, count int) (*archiveNames, error) {
	n := archiveNames{Schema: stateSchema}
	err := scanRecord(path, map[string]func(*boundedjson.Scanner) error{
		"snapshot": func(s *boundedjson.Scanner) (err error) { n.Snapshot, err = scanString(s); return },
		"archive":  func(s *boundedjson.Scanner) (err error) { n.Archive, err = scanString(s); return },
		"files": func(s *boundedjson.Scanner) error {
			return scanStrings(s, count, func(name string) error {
				// A name is checked before it is kept, so a damaged list
				// never holds what no manifest could name.
				if err := storage.CheckRelativePath(name); err != nil {
					return err
				}
				if k := len(n.Files); k > 0 && name <= n.Files[k-1] {
					return fmt.Errorf("%q does not come after %q", name, n.Files[k-1])
				}
				n.Files = append(n.Files, name)
				return nil
			})
		},
	}, "snapshot", "archive", "files")
	if err != nil {
		return nil, err
	}
	switch {
	case n.Snapshot != tag || n.Archive != id || len(n.Files) == 0:
		return nil, stateError("%s is not this archive's names", path)
	case format.ArchiveID(n.Files) != id:
		return nil, stateError("%s does not hash to its archive", path)
	}
	return &n, nil
}

// exists says whether a path is there, never taking a failure to look for
// absence.
func exists(path string) (bool, error) {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	}
	return false, err
}
