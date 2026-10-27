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

// Package source reads a live production root for pulls, and takes no lock
// its collector takes: the listing of its conversations, and the freeze of
// one conversation's head into a private tree a server publishes.
package source

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/internal/pull/format"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// The ways a freeze fails, each answered its own way.
var (
	// ErrNoChain is a conversation with no rounds: 404.
	ErrNoChain = errors.New("source: no such conversation")
	// ErrRetired is a conversation whose session was retired: 410.
	ErrRetired = errors.New("source: the session is retired")
	// ErrRemoving is a session whose files a removal is taking: 409.
	ErrRemoving = errors.New("source: a removal of the session's files is in progress")
	// ErrInvalid is evidence that is wrong while the source holds still:
	// 422, and the pull of that conversation stops.
	ErrInvalid = errors.New("source: the evidence is not valid")
	// ErrChanging is a source that changed while it was read: the freeze is
	// tried again.
	ErrChanging = errors.New("source: the evidence changed while it was read")
	// ErrRead is a read that failed with nothing invalid shown: 503.
	ErrRead = errors.New("source: the evidence could not be read")
)

func classified(kind error, format string, args ...any) error {
	return fmt.Errorf("%w: %s", kind, fmt.Sprintf(format, args...))
}

// state is what a freeze checks again at its end: the session's lifecycle
// and the chain's first and head rounds.
type state struct {
	session    string
	generation int
	rounds     []sessionflow.RoundFile
	first      string
	head       *sessionflow.Header
	headCommit string
}

func (a *state) same(b *state) bool {
	return a.session == b.session && a.generation == b.generation && len(a.rounds) == len(b.rounds) &&
		a.first == b.first && a.headCommit == b.headCommit
}

// readState reads the source's lifecycle and chain, without a lock. On
// Windows a round is read from its copy in copies.
func readState(root, conversation, copies string) (*state, error) {
	if err := storage.CheckNamePart(conversation); err != nil {
		return nil, classified(ErrInvalid, "conversation %q: %v", conversation, err)
	}
	chain := sessionflow.OpenChain(root, conversation)
	rounds, err := chain.ListAtMost(format.MaxFiles)
	if errors.Is(err, storage.ErrTooMany) {
		return nil, classified(ErrInvalid, "the conversation holds more than %d files", format.MaxFiles)
	}
	if err != nil {
		return nil, classified(ErrRead, "%v", err)
	}
	if len(rounds) == 0 {
		return nil, ErrNoChain
	}
	for i, rf := range rounds {
		if rf.Round != uint64(i+1) {
			return nil, classified(ErrInvalid, "the chain has no round %d", i+1)
		}
	}
	head, err := openRound(rounds[len(rounds)-1], copies)
	if err != nil {
		return nil, err
	}
	s := &state{session: head.Header.Session, rounds: rounds, head: &head.Header, headCommit: head.Commit.Digest}
	if head.Header.Conversation != conversation {
		return nil, classified(ErrInvalid, "the head round is of conversation %q", head.Header.Conversation)
	}
	if err := storage.CheckNamePart(s.session); err != nil || strings.HasPrefix(s.session, "_") {
		return nil, classified(ErrInvalid, "session %q is not a name a pull can hold", s.session)
	}
	zone := storage.NewZone(root)
	// Retirement is asked of the session the chain names, never of an
	// earlier snapshot's.
	retired, err := storage.Retired(zone, s.session)
	if err != nil {
		return nil, classified(ErrRead, "%v", err)
	}
	if retired {
		return nil, ErrRetired
	}
	generation, removing, err := storage.RemovalGeneration(zone, s.session)
	if err != nil {
		return nil, classified(ErrRead, "%v", err)
	}
	if removing {
		return nil, ErrRemoving
	}
	s.generation = generation
	if len(rounds) == 1 {
		s.first = s.headCommit
	} else {
		first, err := openRound(rounds[0], copies)
		if err != nil {
			return nil, err
		}
		s.first = first.Commit.Digest
	}
	return s, nil
}

// openRound reads one round by its commit digest. A round that does not
// read is invalid evidence; one that went away mid-read is a changing source.
func openRound(rf sessionflow.RoundFile, copies string) (*sessionflow.Round, error) {
	var r *sessionflow.Round
	var parsed error
	if err := readLive(rf.Path, copies, func(in io.Reader) error {
		r, parsed = sessionflow.Read(in)
		return nil
	}); err != nil {
		return nil, readError(rf.Path, err)
	}
	if parsed != nil {
		return nil, classified(ErrInvalid, "%s: %v", filepath.Base(rf.Path), parsed)
	}
	if r.Header.Round != rf.Round || !strings.HasPrefix(r.Commit.Digest, rf.Digest) {
		return nil, classified(ErrInvalid, "%s holds round %d %s", filepath.Base(rf.Path), r.Header.Round, r.Commit.Digest)
	}
	return r, nil
}

// rereadError classifies a failed read of the source's state once the
// freeze holds its files: a retired, removing or empty conversation and a
// copy this machine could not make are what they are; anything else is a
// source that changed.
func rereadError(err error) error {
	var local *localCopyError
	switch {
	case errors.Is(err, ErrRetired), errors.Is(err, ErrRemoving), errors.Is(err, ErrNoChain):
		return err
	case errors.As(err, &local):
		return local
	}
	return classified(ErrChanging, "the source could not be read again: %v", err)
}

// readError classifies a failed read of live evidence: a file that went away
// is a changing source unless the caller shows otherwise, a refused
// permission is invalid, anything else is a read to try again.
func readError(path string, err error) error {
	var local *localCopyError
	switch {
	case errors.As(err, &local):
		// This machine's own storage failed: never a source to read again.
		return local
	case errors.Is(err, storage.ErrProtected):
		// A link, or a file that is not a regular one, where evidence must be.
		return classified(ErrInvalid, "%v", err)
	case errors.Is(err, fs.ErrNotExist):
		return classified(ErrChanging, "%s went away", path)
	case errors.Is(err, fs.ErrPermission):
		return classified(ErrInvalid, "%s cannot be read: %v", path, err)
	}
	return classified(ErrRead, "%v", err)
}

// storeName is a provider store entry's file name: a SHA-256 and .gz.
var storeName = regexp.MustCompile(`^[0-9a-f]{64}\.gz$`)

// generationName is a tip generation directory: a whole number from 1,
// written without a leading zero.
var generationName = regexp.MustCompile(`^[1-9][0-9]*$`)

// candidate is one live file a freeze lists.
type candidate struct {
	entry format.Entry
	live  string
}

// candidates lists the evidence of the head: rounds 1 to N, every landed
// file or skeleton through its through_seq, and every published provider
// store entry. Regular .tmp- files in the store's canonical directories are
// a writer's and are passed over unread; anything else there that is not an
// entry is invalid.
func candidates(root string, s *state) ([]candidate, error) {
	zone := storage.NewZone(root)
	var out []candidate
	add := func(path string, e format.Entry) error {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return classified(ErrInvalid, "%v", err)
		}
		// Evidence is a regular file at its own name: a link there, as to a
		// device that never ends, is refused before anything reads it.
		info, err := os.Lstat(path)
		if err != nil {
			return readError(path, err)
		}
		if !info.Mode().IsRegular() {
			return classified(ErrInvalid, "%s is not a regular file", filepath.ToSlash(rel))
		}
		e.File = filepath.ToSlash(rel)
		if len(out) >= format.MaxFiles {
			return classified(ErrInvalid, "the conversation holds more than %d files", format.MaxFiles)
		}
		out = append(out, candidate{entry: e, live: path})
		return nil
	}
	for _, rf := range s.rounds {
		if err := add(rf.Path, format.Entry{Kind: format.KindRound, Round: rf.Round}); err != nil {
			return nil, err
		}
	}
	landed, err := storage.LandedFilesAtMost(zone, s.session, format.MaxFiles-len(out))
	if errors.Is(err, storage.ErrTooMany) {
		return nil, classified(ErrInvalid, "the conversation holds more than %d files", format.MaxFiles)
	}
	if err != nil {
		return nil, classified(ErrRead, "%v", err)
	}
	for _, lf := range landed {
		if lf.Seq > s.head.ThroughSeq {
			continue
		}
		e := format.Entry{Kind: format.KindLanded, Seq: lf.Seq}
		if lf.Skeleton {
			e.Kind = format.KindSkeleton
		}
		if err := add(lf.Path, e); err != nil {
			return nil, err
		}
	}
	store := filepath.Join(zone.SessionDir(s.session), "provider")
	entries, err := storeEntries(store, format.MaxFiles-len(out))
	if err != nil {
		return nil, err
	}
	for _, path := range entries {
		if err := add(path, format.Entry{Kind: format.KindStore}); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(out, func(a, b candidate) int { return strings.Compare(a.entry.File, b.entry.File) })
	return out, nil
}

// storeEntries lists a session's provider store: tips/<generation>/ and
// pieces/, each entry a regular file under a SHA-256 name. The directories
// are walked as they are read, and only store entries count: a store of
// more than most of them is refused before it is held, and a writer's
// temporaries, passed over unread, never count.
func storeEntries(store string, most int) ([]string, error) {
	var out []string
	list := func(dir string) error {
		err := storage.EachDirEntry(dir, func(it fs.DirEntry) error {
			name := it.Name()
			path := filepath.Join(dir, name)
			info, err := it.Info()
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return classified(ErrRead, "%v", err)
			}
			if strings.HasPrefix(name, ".tmp-") {
				// A writer's temporary: never read, hashed, copied or
				// deleted. Only a regular file earns that.
				if !info.Mode().IsRegular() {
					return classified(ErrInvalid, "%s is not a regular file", path)
				}
				return nil
			}
			if !info.Mode().IsRegular() || !storeName.MatchString(name) {
				return classified(ErrInvalid, "%s is not a store entry", path)
			}
			if len(out) == most {
				return classified(ErrInvalid, "the conversation holds more than %d files", format.MaxFiles)
			}
			out = append(out, path)
			return nil
		})
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil
		case errors.Is(err, ErrInvalid), errors.Is(err, ErrRead):
			return err
		case err != nil:
			return classified(ErrRead, "%v", err)
		}
		return nil
	}
	// Each generation is listed as it is met, so no list of them is held.
	tips := filepath.Join(store, "tips")
	err := storage.EachDirEntry(tips, func(g fs.DirEntry) error {
		if !g.IsDir() || !generationName.MatchString(g.Name()) {
			return classified(ErrInvalid, "%s is not a tip generation", filepath.Join(tips, g.Name()))
		}
		return list(filepath.Join(tips, g.Name()))
	})
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case errors.Is(err, ErrInvalid), errors.Is(err, ErrRead):
		return nil, err
	case err != nil:
		return nil, classified(ErrRead, "%v", err)
	}
	if err := list(filepath.Join(store, "pieces")); err != nil {
		return nil, err
	}
	return out, nil
}

// Freeze freezes a conversation's head on a live root into build/tree: it
// lists the evidence, checks the listing with the manifest's checks, saves
// the manifest there before any named evidence, links or copies each file
// in with its size and SHA-256 checked, and then reads the source's state
// and its store again. A source that changed on the way is ErrChanging, and
// the caller discards build and tries again.
//
// build is a private directory the caller owns and made. It holds tree/,
// files/ for temporaries and, on Windows, copies/: a live file there is held
// open only while it is copied, and every later read is of the copy.
func Freeze(ctx context.Context, root, conversation, build string, files storage.FileSystem) (*format.Manifest, error) {
	for _, dir := range []string{"tree", "files", "copies"} {
		if err := storage.MakeOwnedDir(filepath.Join(build, dir), files); err != nil {
			return nil, err
		}
	}
	copies := filepath.Join(build, "copies")
	before, err := readState(root, conversation, copies)
	if err != nil {
		return nil, err
	}
	list, err := candidates(root, before)
	if err != nil {
		return nil, err
	}
	held := make([]string, len(list))
	for i := range list {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if held[i], err = describe(root, conversation, before, &list[i], copies); err != nil {
			return nil, err
		}
	}
	m := &format.Manifest{
		Conversation: conversation, Session: before.session,
		Head:       format.Head{Round: before.head.Round, Digest: format.CommitDigest(before.headCommit), ThroughSeq: before.head.ThroughSeq},
		Generation: int64(before.generation),
		Formats:    format.Formats{SD: sessiondata.Schema, SF: sessionflow.Schema},
	}
	for _, c := range list {
		m.Files = append(m.Files, c.entry)
	}
	if err := m.Check(conversation); err != nil {
		return nil, classified(ErrInvalid, "%v", err)
	}
	tree := filepath.Join(build, "tree")
	if err := storage.WriteNewDurable(filepath.Join(tree, format.ManifestFile), 0o444, func(w io.Writer) error {
		return format.WriteManifest(w, m)
	}, files); err != nil {
		return nil, err
	}
	for i, c := range list {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := install(tree, filepath.Join(build, "files"), c, held[i], files); err != nil {
			return nil, err
		}
	}
	after, err := readState(root, conversation, copies)
	if err != nil {
		return nil, rereadError(err)
	}
	if !before.same(after) {
		return nil, classified(ErrChanging, "the source's state changed while it was frozen")
	}
	// A tip can be written before a removal begins, so an unchanged removal
	// state alone does not show the store held still.
	if err := sameStore(after, root, list, copies); err != nil {
		return nil, err
	}
	return m, nil
}

// describe completes a candidate's entry: its size and SHA-256, a
// skeleton's original digest, a store entry's content digest. On Windows the
// live file is copied first and described from the copy, whose path it
// returns; elsewhere it is read in place and closed at once.
func describe(root, conversation string, s *state, c *candidate, copies string) (string, error) {
	path := c.live
	if runtime.GOOS == "windows" {
		copied, err := copyOut(c.live, copies)
		if err != nil {
			return "", vanished(root, conversation, s, copies, err)
		}
		path = copied
	}
	size, sum, err := hashFile(path)
	if err != nil {
		return "", vanished(root, conversation, s, copies, err)
	}
	if size == 0 {
		return "", classified(ErrInvalid, "%s is empty", c.entry.File)
	}
	c.entry.Bytes, c.entry.Digest = uint64(size), format.StoredDigest(sum)
	switch c.entry.Kind {
	case format.KindSkeleton:
		original, err := storage.LandedDigest(storage.LandedFile{Path: path, Seq: c.entry.Seq, Skeleton: true})
		if err != nil {
			return "", vanished(root, conversation, s, copies, err)
		}
		c.entry.LandedDigest = format.OriginalDigest(original)
	case format.KindStore:
		content, err := contentDigest(path)
		if err != nil {
			return "", vanished(root, conversation, s, copies, err)
		}
		c.entry.Content = format.ContentDigest(content)
	}
	if runtime.GOOS == "windows" {
		return path, nil
	}
	return "", nil
}

// vanished classifies a file that failed to read during a freeze: when the
// source's state changed meanwhile it is a changing source, and when it held
// still a missing file is invalid evidence.
func vanished(root, conversation string, s *state, copies string, err error) error {
	switch {
	case errors.Is(err, ErrInvalid), errors.Is(err, ErrRead), errors.Is(err, ErrChanging):
		// Already classified where it was read.
	default:
		err = readError("a listed file", err)
	}
	if !errors.Is(err, ErrChanging) {
		return err
	}
	now, serr := readState(root, conversation, copies)
	if serr != nil || !s.same(now) {
		return classified(ErrChanging, "a file went away as the source changed")
	}
	return classified(ErrInvalid, "a file the chain needs is missing: %v", err)
}

// install gives a candidate its name in tree: a hard link to the live file
// where one can be made, or a checked copy; on Windows, the copy made when
// it was described.
func install(tree, temps string, c candidate, held string, files storage.FileSystem) error {
	dest := filepath.Join(tree, filepath.FromSlash(c.entry.File))
	if err := makeParents(tree, c.entry.File, files); err != nil {
		return err
	}
	sum, err := hex.DecodeString(string(c.entry.Digest))
	if err != nil {
		return err
	}
	var want [sha256.Size]byte
	copy(want[:], sum)
	temp, err := tempName(temps)
	if err != nil {
		return err
	}
	src := c.live
	if held != "" {
		src = held
	}
	if _, err := storage.ShareFile(src, dest, temp, int64(c.entry.Bytes), want, files); err != nil {
		// Only the source changing is a reason to read it again. A link,
		// a write or a sync of the build that failed is this server's
		// storage failing, and stops the freeze.
		switch {
		case errors.Is(err, storage.ErrChecked):
			return classified(ErrChanging, "%s changed before it was frozen", c.entry.File)
		case errors.Is(err, fs.ErrNotExist):
			if _, lerr := os.Lstat(src); errors.Is(lerr, fs.ErrNotExist) {
				return classified(ErrChanging, "%s went away before it was frozen", c.entry.File)
			}
		}
		return fmt.Errorf("source: freezing %s: %w", c.entry.File, err)
	}
	return nil
}

// makeParents makes the directories of a manifest name inside tree, each a
// real directory made here, never through a link. The name is split on its
// own slashes, never through filepath.Dir: on Windows that returns
// backslashes, and the split would find one part whose parent was never made.
func makeParents(tree, name string, files storage.FileSystem) error {
	parts := strings.Split(name, "/")
	dir := tree
	for _, part := range parts[:len(parts)-1] {
		dir = filepath.Join(dir, part)
		info, err := os.Lstat(dir)
		switch {
		case err == nil && info.IsDir():
			continue
		case err == nil:
			return fmt.Errorf("source: %s is not a directory", dir)
		case !errors.Is(err, fs.ErrNotExist):
			return err
		}
		if err := storage.MakeOwnedDir(dir, files); err != nil {
			return err
		}
	}
	return nil
}

// sameStore lists the store again and requires every entry the freeze
// listed, of the same size and digest, and no other.
func sameStore(s *state, root string, list []candidate, copies string) error {
	zone := storage.NewZone(root)
	listed := map[string]format.Entry{}
	for _, c := range list {
		if c.entry.Kind == format.KindStore {
			listed[c.live] = c.entry
		}
	}
	now, err := storeEntries(filepath.Join(zone.SessionDir(s.session), "provider"), format.MaxFiles)
	if err != nil {
		return classified(ErrChanging, "the store could not be listed again: %v", err)
	}
	if len(now) != len(listed) {
		return classified(ErrChanging, "the store changed while it was frozen")
	}
	for _, path := range now {
		e, ok := listed[path]
		if !ok {
			return classified(ErrChanging, "the store changed while it was frozen")
		}
		size, sum, err := hashLive(path, copies)
		// A copy this machine failed to write is its storage failing, never
		// a store that changed: it stops the freeze.
		var local *localCopyError
		if errors.As(err, &local) {
			return local.err
		}
		if err != nil || uint64(size) != e.Bytes || format.StoredDigest(sum) != e.Digest {
			return classified(ErrChanging, "%s changed while it was frozen", e.File)
		}
	}
	return nil
}

// readLive gives read the bytes of a live evidence file. On Windows the
// file is copied first, into copies or the system's temporary directory
// when copies is empty, and held open only while it is copied: read gets
// the copy, which is deleted after. Elsewhere it is read in place and
// closed at once. Either way it is never read through a link, and must be
// a regular file.
func readLive(path, copies string, read func(io.Reader) error) error {
	if runtime.GOOS == "windows" {
		if copies == "" {
			copies = os.TempDir()
		}
		copied, err := copyOut(path, copies)
		if err != nil {
			return err
		}
		defer os.Remove(copied)
		path = copied
	}
	f, err := storage.OpenEvidence(path, nil)
	if err != nil {
		return err
	}
	defer f.Close()
	return read(f)
}

// hashLive is hashFile of a live file, through readLive.
func hashLive(path, copies string) (int64, string, error) {
	var n int64
	var sum string
	err := readLive(path, copies, func(in io.Reader) error {
		h := sha256.New()
		var err error
		if n, err = io.Copy(h, in); err != nil {
			return err
		}
		sum = hex.EncodeToString(h.Sum(nil))
		return nil
	})
	return n, sum, err
}

// hashFile reads a file, never through a link, and closes it at once.
func hashFile(path string) (int64, string, error) {
	f, err := storage.OpenEvidence(path, nil)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// contentDigest is the SHA-256 of what a gzip store entry holds.
func contentDigest(path string) (string, error) {
	f, err := storage.OpenEvidence(path, nil)
	if err != nil {
		return "", err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return "", classified(ErrInvalid, "%s: %v", path, err)
	}
	h := sha256.New()
	if _, err := io.Copy(h, zr); err != nil {
		return "", classified(ErrInvalid, "%s: %v", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyOut copies a live file into copies under a new random name, holding
// the live file open only while it copies. The copy keeps the live file's
// extension: a skeleton is told by its name, and read through its
// compression only when the name says so.
func copyOut(live, copies string) (string, error) {
	in, err := storage.OpenEvidence(live, nil)
	if err != nil {
		return "", err
	}
	defer in.Close()
	name, err := tempName(copies)
	if err != nil {
		return "", &localCopyError{err}
	}
	name += filepath.Ext(live)
	out, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", &localCopyError{err}
	}
	written := &copyWriter{w: out}
	if _, err := io.Copy(written, in); err != nil {
		if written.err != nil {
			err = &localCopyError{err}
		}
		return "", errors.Join(err, out.Close())
	}
	if err := out.Close(); err != nil {
		return "", &localCopyError{err}
	}
	return name, nil
}

// localCopyError is a copy this machine failed to make or write: a failure
// of its own storage, which stops a freeze, never a source that changed.
type localCopyError struct{ err error }

func (e *localCopyError) Error() string {
	return "source: a copy here could not be written: " + e.err.Error()
}

func (e *localCopyError) Unwrap() error { return e.err }

// copyWriter keeps a write that failed, so a failed copy is told from a
// live file that could not be read.
type copyWriter struct {
	w   io.Writer
	err error
}

func (c *copyWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	if err != nil {
		c.err = err
	}
	return n, err
}

// tempName is a new random name in dir.
func tempName(dir string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return filepath.Join(dir, hex.EncodeToString(b[:])), nil
}
