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

package format

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/verify"
	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// ErrEvidence is evidence that is not exactly what its manifest lists, or
// not whole. Its text names the file and the rule.
var ErrEvidence = errors.New("pull: the evidence does not hold")

// ManifestFile is where a tree keeps its manifest.
const ManifestFile = "_manifest"

// Layout is the kind of tree acceptance checks, which decides the few files
// it holds besides the evidence.
type Layout int

const (
	// Building is a client's private build. Its _work/ directory is checked
	// but never entered, and the controls a published snapshot carries may
	// not exist yet.
	Building Layout = iota
	// Published is a published local snapshot: its _copy, _root/.lock and
	// _leases/ must be there, and judge runs may sit in its session's
	// judgments/.
	Published
	// ServerSnapshot is a snapshot a server published: its _snapshot and
	// _last_use records, and its _leases/, _requests/ and _archives/
	// directories, which are checked as what they are and never entered.
	ServerSnapshot
)

// The controls a server snapshot holds besides its manifest.
const (
	SnapshotRecord = "_snapshot"
	LastUseRecord  = "_last_use"
	RequestsDir    = "_requests"
	ArchivesDir    = "_archives"
)

// Want is what the manifest is bound to.
type Want struct {
	// Conversation is the one that was asked for.
	Conversation string
	// ManifestDigest, when set, is the file-list digest a marker saved.
	ManifestDigest ManifestDigest
	// Session and the head round and digest, when set, are what a marker
	// saved.
	Session string
	Round   uint64
	Digest  CommitDigest
	// Remote and Tag name a snapshot's own marker, which a published tree
	// must hold and a build may: it is read and bound to the manifest. A
	// build without them may hold no marker.
	Remote, Tag string
}

// Accepted is what acceptance checked, for the decisions that follow it:
// whether a source conflicts with a base, and which files can be reused.
type Accepted struct {
	Manifest *Manifest
	// Digest is the manifest's file-list digest.
	Digest ManifestDigest
	// Stored is each listed file's checked SHA-256, by name.
	Stored map[string]StoredDigest
	// Originals is the digest of the landed file each sequence stands for:
	// a landed file's own, or the one its skeleton keeps.
	Originals map[uint64]OriginalDigest
}

// Accept decides that tree holds exactly the evidence its manifest lists,
// whole: the files and nothing else but the layout's own controls, every
// size and SHA-256, every Session Data file read to its end, the rounds
// bounded and folded and verified as one chain over those files, every
// provider body rebuilt, and the inventory and the manifest unchanged at the
// end. The caller holds the lease or the lock that keeps the tree from being
// removed.
//
// Nothing here is a transition: acceptance reads only, and runs outside every
// transition lock.
func Accept(tree string, layout Layout, want Want) (*Accepted, error) {
	return accept(tree, layout, want, nil)
}

// accept is Accept with between, which runs after every content check and
// before the final inventory: a test changes the tree there.
func accept(tree string, layout Layout, want Want, between func()) (*Accepted, error) {
	m, manifest, err := readSavedManifest(tree, want.Conversation)
	if err != nil {
		return nil, err
	}
	a := &Accepted{Manifest: m, Digest: m.Digest(), Stored: map[string]StoredDigest{}, Originals: map[uint64]OriginalDigest{}}
	switch {
	case want.ManifestDigest != "" && a.Digest != want.ManifestDigest:
		return nil, evidenceError("the manifest's file-list digest is %s, not the %s its marker saved", a.Digest, want.ManifestDigest)
	case want.Session != "" && m.Session != want.Session:
		return nil, evidenceError("the manifest is of session %s, not %s", m.Session, want.Session)
	case want.Round != 0 && (m.Head.Round != want.Round || m.Head.Digest != want.Digest):
		return nil, evidenceError("the manifest's head is round %d %s, not round %d %s", m.Head.Round, m.Head.Digest, want.Round, want.Digest)
	}
	first, err := inventory(tree, layout, m)
	if err != nil {
		return nil, err
	}
	if err := bindMarker(tree, layout, want, a); err != nil {
		return nil, err
	}
	// The files, in order of their sequence for landed files, so a provider
	// body is read after everything it may be built on.
	landed := slices.Clone(m.Files)
	slices.SortStableFunc(landed, func(x, y Entry) int { return cmpUint64(x.Seq, y.Seq) })
	for _, e := range landed {
		path := filepath.Join(tree, filepath.FromSlash(e.File))
		switch e.Kind {
		case KindLanded, KindSkeleton:
			var original OriginalDigest
			original, err = checkSessionData(path, first[e.File], e, m)
			if err == nil {
				a.Originals[e.Seq] = original
			}
		default:
			err = checkStored(path, first[e.File], e)
		}
		if err != nil {
			return nil, err
		}
		a.Stored[e.File] = e.Digest
	}
	zone := storage.NewZone(tree)
	if err := checkRounds(zone, m, want.Conversation); err != nil {
		return nil, err
	}
	originals := make(map[uint64]string, len(a.Originals))
	for seq, d := range a.Originals {
		originals[seq] = string(d)
	}
	report, err := verify.Chain(zone, want.Conversation, originals)
	if err != nil {
		return nil, evidenceError("the chain: %v", err)
	}
	if !report.OK() {
		return nil, evidenceError("the chain: %s", strings.Join(report.Details(), "; "))
	}
	if err := checkProvider(tree, zone, m, landed, first); err != nil {
		return nil, err
	}
	if between != nil {
		between()
	}
	last, err := inventory(tree, layout, m)
	if err != nil {
		return nil, err
	}
	for name, info := range first {
		if !os.SameFile(info, last[name]) || info.Size() != last[name].Size() {
			return nil, evidenceError("%s changed while it was checked", name)
		}
	}
	// The manifest governs every decision above, so it is still the same
	// file holding the same bytes.
	if err := manifest.recheck(tree); err != nil {
		return nil, err
	}
	return a, nil
}

// savedManifest is the manifest file as acceptance read it.
type savedManifest struct {
	info os.FileInfo
	sum  [sha256.Size]byte
}

func (s savedManifest) recheck(tree string) error {
	path := filepath.Join(tree, ManifestFile)
	f, err := storage.OpenEvidence(path, s.info)
	if err != nil {
		return evidenceError("%s changed while it was checked: %v", ManifestFile, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return evidenceError("%s: %v", ManifestFile, err)
	}
	if !bytes.Equal(h.Sum(nil), s.sum[:]) {
		return evidenceError("%s changed while it was checked", ManifestFile)
	}
	return nil
}

// bindMarker reads a snapshot's own marker and binds it to the manifest: a
// published tree must hold one, and a build holds one only when it is about
// to be published under the tag the caller names.
func bindMarker(tree string, layout Layout, want Want, a *Accepted) error {
	_, err := os.Lstat(filepath.Join(tree, storage.CopyMarker))
	present := err == nil
	switch {
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return evidenceError("%v", err)
	case !present && layout == Published:
		return evidenceError("a published snapshot needs its %s", storage.CopyMarker)
	case !present:
		return nil
	case want.Remote == "" || want.Tag == "":
		return evidenceError("%s is there, and nothing names the snapshot it should be the marker of", storage.CopyMarker)
	}
	marker, err := storage.ReadSnapshotCopy(tree, want.Remote, want.Conversation, want.Tag)
	if err != nil {
		return evidenceError("%v", err)
	}
	m := a.Manifest
	switch {
	case ManifestDigest(marker.ManifestDigest) != a.Digest:
		return evidenceError("its marker names manifest %s, and the manifest is %s", marker.ManifestDigest, a.Digest)
	case marker.Session != m.Session:
		return evidenceError("its marker names session %s, and the manifest %s", marker.Session, m.Session)
	case marker.Round != m.Head.Round || CommitDigest(marker.Digest) != m.Head.Digest:
		return evidenceError("its marker names head round %d %s, and the manifest round %d %s", marker.Round, marker.Digest, m.Head.Round, m.Head.Digest)
	}
	return nil
}

func evidenceError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrEvidence, fmt.Sprintf(format, args...))
}

func cmpUint64(x, y uint64) int {
	switch {
	case x < y:
		return -1
	case x > y:
		return 1
	}
	return 0
}

// readSavedManifest reads a tree's manifest as a regular file, never through
// a link, and keeps what it read so the end of acceptance can see it is
// unchanged.
func readSavedManifest(tree, conversation string) (*Manifest, savedManifest, error) {
	var saved savedManifest
	path := filepath.Join(tree, ManifestFile)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, saved, evidenceError("%s: %v", ManifestFile, err)
	}
	f, err := storage.OpenEvidence(path, info)
	if err != nil {
		return nil, saved, evidenceError("%s: %v", ManifestFile, err)
	}
	defer f.Close()
	h := sha256.New()
	m, err := ReadManifest(io.TeeReader(f, h), conversation)
	if err != nil {
		return nil, saved, err
	}
	// The parser stopped at the end of its value; the hash covers the rest.
	if _, err := io.Copy(h, f); err != nil {
		return nil, saved, evidenceError("%s: %v", ManifestFile, err)
	}
	saved.info = info
	copy(saved.sum[:], h.Sum(nil))
	return m, saved, nil
}

var (
	stageFileRe = regexp.MustCompile(`^s[0-9]-[a-z]+-[0-9a-f]{12}\.sj$`)
	regroupRe   = regexp.MustCompile(`^g[0-9]+-regroup-[0-9a-f]{12}\.sj$`)
	stageTempRe = regexp.MustCompile(`^\.tmp-(0|[1-9][0-9]{0,9})$`)
)

// inventory walks tree without following a link and returns every listed
// file's identity. Beside the evidence it allows only the layout's own
// controls, and refuses anything else: a file the manifest does not list, a
// directory no listed name passes through, a link anywhere it looks.
func inventory(tree string, layout Layout, m *Manifest) (map[string]os.FileInfo, error) {
	info, err := os.Lstat(tree)
	if err != nil {
		return nil, evidenceError("%v", err)
	}
	if info.Mode().Type() != fs.ModeDir {
		return nil, evidenceError("%s is not a real directory", tree)
	}
	listed := make(map[string]Entry, len(m.Files))
	dirs := map[string]bool{}
	for _, e := range m.Files {
		listed[e.File] = e
		for dir := pathDir(e.File); dir != ""; dir = pathDir(dir) {
			dirs[dir] = true
		}
	}
	found := make(map[string]os.FileInfo, len(m.Files))
	controls := map[string]bool{}
	var walk func(rel string) error
	walk = func(rel string) error {
		entries, err := os.ReadDir(filepath.Join(tree, filepath.FromSlash(rel)))
		if err != nil {
			return evidenceError("%v", err)
		}
		for _, d := range entries {
			name := d.Name()
			path := name
			if rel != "" {
				path = rel + "/" + name
			}
			info, err := d.Info()
			if err != nil {
				return evidenceError("%v", err)
			}
			kind := info.Mode().Type()
			if rel == "" {
				if ok, err := topControl(tree, layout, name, info); err != nil {
					return err
				} else if ok {
					controls[name] = true
					continue
				}
			}
			if rel == m.Session && name == storage.JudgmentsDir && layout == Published {
				if kind != fs.ModeDir {
					return evidenceError("%s is not a real directory", path)
				}
				if err := judgments(filepath.Join(tree, filepath.FromSlash(path))); err != nil {
					return err
				}
				continue
			}
			switch {
			case kind == fs.ModeDir && dirs[path]:
				if err := walk(path); err != nil {
					return err
				}
			case kind.IsRegular() && listed[path].File == path:
				if uint64(info.Size()) != listed[path].Bytes {
					return evidenceError("%s holds %d bytes, and the manifest lists %d", path, info.Size(), listed[path].Bytes)
				}
				found[path] = info
			case kind&fs.ModeSymlink != 0:
				return evidenceError("%s is a link", path)
			case kind == fs.ModeDir:
				return evidenceError("%s is a directory no listed file is in", path)
			default:
				return evidenceError("%s is not listed in the manifest", path)
			}
		}
		return nil
	}
	if err := walk(""); err != nil {
		return nil, err
	}
	if layout == Published {
		for _, name := range []string{storage.CopyMarker, storage.RootLockDir, storage.LeasesDir} {
			if !controls[name] {
				return nil, evidenceError("a published snapshot needs its %s", name)
			}
		}
	}
	for _, e := range m.Files {
		if found[e.File] == nil {
			return nil, evidenceError("%s is missing", e.File)
		}
	}
	return found, nil
}

func pathDir(p string) string {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return ""
	}
	return p[:i]
}

// topControl says whether name, at the top of the tree, is one of the
// layout's controls, and checks it is the right kind of file.
func topControl(tree string, layout Layout, name string, info os.FileInfo) (bool, error) {
	kind := info.Mode().Type()
	regular := func() (bool, error) {
		if !kind.IsRegular() {
			return false, evidenceError("%s is not a regular file", name)
		}
		return true, nil
	}
	if layout == ServerSnapshot {
		switch name {
		case SnapshotRecord, LastUseRecord:
			return regular()
		case storage.LeasesDir, RequestsDir, ArchivesDir:
			// Each holds only the regular files of its kind, by name:
			// checked without opening any, so no link and no directory
			// hides inside.
			return true, onlyRegular(filepath.Join(tree, name), serverControls[name])
		case storage.CopyMarker, storage.RootLockDir:
			return false, evidenceError("a server snapshot holds no %s", name)
		}
	}
	switch {
	case name == ManifestFile:
		return regular()
	case name == storage.CopyMarker:
		return regular()
	case strings.HasPrefix(name, ".tmp-"):
		// A control replacement's leftover: inspected, never opened.
		return regular()
	case name == storage.RootLockDir:
		dir := filepath.Join(tree, name)
		if err := onlyEmpty(dir, func(n string) bool { return n == ".lock" }); err != nil {
			return false, err
		}
		// A published snapshot's judge locks it, without making anything.
		if _, err := os.Lstat(filepath.Join(dir, ".lock")); err != nil && layout == Published {
			return false, evidenceError("a published snapshot needs its %s/.lock", name)
		}
		return true, nil
	case name == storage.LeasesDir:
		return true, onlyEmpty(filepath.Join(tree, name), func(n string) bool { _, _, ok := storage.ParseLeaseName(n); return ok })
	case name == "_work" && layout == Building:
		// Checked, never entered: what a build keeps there is its own.
		if kind != fs.ModeDir {
			return false, evidenceError("%s is not a real directory", name)
		}
		return true, nil
	}
	return false, nil
}

// serverControls are the names a server snapshot's control directories may
// hold: leases, request locks, and an archive's names, job, lock, bytes,
// retained failures and temporary output. A control write's .tmp- leftover
// may be in any of them.
var serverControls = map[string]*regexp.Regexp{
	storage.LeasesDir: regexp.MustCompile(`^(?:[0-9a-f]{64}\.json|\.tmp-.*)$`),
	RequestsDir:       regexp.MustCompile(`^(?:[0-9a-f]{64}\.lock|\.tmp-.*)$`),
	ArchivesDir:       regexp.MustCompile(`^(?:[0-9a-f]{64}\.(?:names|json|lock|tar\.gz|failed-[1-9][0-9]*\.json)|\.tmp-.*)$`),
}

// onlyRegular checks dir is a real directory holding only regular files
// whose names allowed matches, opening none of them.
func onlyRegular(dir string, allowed *regexp.Regexp) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return evidenceError("%v", err)
	}
	if info.Mode().Type() != fs.ModeDir {
		return evidenceError("%s is not a real directory", filepath.Base(dir))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return evidenceError("%v", err)
	}
	for _, d := range entries {
		info, err := d.Info()
		if err != nil {
			return evidenceError("%v", err)
		}
		if !allowed.MatchString(d.Name()) || !info.Mode().IsRegular() {
			return evidenceError("%s/%s is not one of its control files", filepath.Base(dir), d.Name())
		}
	}
	return nil
}

// onlyEmpty checks dir is a real directory holding only empty regular files
// whose names allow says are its own.
func onlyEmpty(dir string, allow func(string) bool) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return evidenceError("%v", err)
	}
	if info.Mode().Type() != fs.ModeDir {
		return evidenceError("%s is not a real directory", filepath.Base(dir))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return evidenceError("%v", err)
	}
	for _, d := range entries {
		info, err := d.Info()
		if err != nil {
			return evidenceError("%v", err)
		}
		if !allow(d.Name()) || !info.Mode().IsRegular() || info.Size() != 0 {
			return evidenceError("%s/%s is not one of its empty lock files", filepath.Base(dir), d.Name())
		}
	}
	return nil
}

// judgments checks a session's judge runs as far as acceptance looks: each
// run directory, its stage and regroup files and their write temporaries,
// and its work directory, which is checked and never entered. Nothing in
// them is read: they are output, not evidence.
func judgments(dir string) error {
	runs, err := os.ReadDir(dir)
	if err != nil {
		return evidenceError("%v", err)
	}
	for _, r := range runs {
		info, err := r.Info()
		if err != nil {
			return evidenceError("%v", err)
		}
		if !storage.ValidRun(r.Name()) || info.Mode().Type() != fs.ModeDir {
			return evidenceError("judgments/%s is not a judge run", r.Name())
		}
		files, err := os.ReadDir(filepath.Join(dir, r.Name()))
		if err != nil {
			return evidenceError("%v", err)
		}
		for _, f := range files {
			info, err := f.Info()
			if err != nil {
				return evidenceError("%v", err)
			}
			name, kind := f.Name(), info.Mode().Type()
			switch {
			case name == storage.WorkDir && kind == fs.ModeDir:
			case kind.IsRegular() && (stageFileRe.MatchString(name) || regroupRe.MatchString(name) || stageTemp(name)):
			default:
				return evidenceError("judgments/%s/%s is not part of a judge run", r.Name(), name)
			}
		}
	}
	return nil
}

// stageTemp is a judge's stage-write temporary, .tmp- and the decimal
// os.CreateTemp gives it: 0 to 4294967295, with no leading zero but in 0
// (src/os/tempfile.go).
func stageTemp(name string) bool {
	m := stageTempRe.FindStringSubmatch(name)
	if m == nil {
		return false
	}
	n, err := strconv.ParseUint(m[1], 10, 32)
	return err == nil && n <= math.MaxUint32
}

// hashingFile reads a listed file once, hashing every byte it holds.
type hashingFile struct {
	f    *os.File
	h    hash.Hash
	read int64
}

func openListed(path string, info os.FileInfo) (*hashingFile, error) {
	f, err := storage.OpenEvidence(path, info)
	if err != nil {
		return nil, evidenceError("%v", err)
	}
	return &hashingFile{f: f, h: sha256.New()}, nil
}

func (h *hashingFile) Read(p []byte) (int, error) {
	n, err := h.f.Read(p)
	h.h.Write(p[:n])
	h.read += int64(n)
	return n, err
}

// finish reads what is left, which must be nothing when rest is false, and
// checks size and SHA-256 against the entry.
func (h *hashingFile) finish(e Entry, rest bool) error {
	defer h.f.Close()
	left, err := io.Copy(io.Discard, h)
	if err != nil {
		return evidenceError("%s: %v", e.File, err)
	}
	if left != 0 && !rest {
		return evidenceError("%s holds %d bytes after its end", e.File, left)
	}
	if uint64(h.read) != e.Bytes || StoredDigest(hex.EncodeToString(h.h.Sum(nil))) != e.Digest {
		return evidenceError("%s is not the %d bytes with SHA-256 %s the manifest lists", e.File, e.Bytes, e.Digest)
	}
	return nil
}

// checkStored checks a round or a store entry: its stored bytes, and for a
// store entry, what it holds once decompressed.
func checkStored(path string, info os.FileInfo, e Entry) error {
	h, err := openListed(path, info)
	if err != nil {
		return err
	}
	if e.Kind != KindStore {
		return h.finish(e, true)
	}
	content, err := gunzipDigest(h)
	if err != nil {
		_ = h.f.Close()
		return evidenceError("%s: %v", e.File, err)
	}
	if err := h.finish(e, false); err != nil {
		return err
	}
	if ContentDigest(content) != e.Content {
		return evidenceError("%s holds content %s, and the manifest lists %s", e.File, content, e.Content)
	}
	return nil
}

// singleMember reads r as exactly one gzip member, checksum and trailer
// included. Bytes after it, a second member among them, are refused by the
// caller's end check.
func singleMember(r *bufio.Reader) (*gzip.Reader, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	zr.Multistream(false)
	return zr, nil
}

// atEnd requires that nothing follows the gzip member r was read to the end of.
// The buffered reader holds what the member did not use, so it is asked,
// never the file.
func atEnd(r *bufio.Reader) error {
	_, err := r.ReadByte()
	switch {
	case err == nil:
		return errors.New("bytes follow its gzip member")
	case errors.Is(err, io.EOF):
		return nil
	}
	return fmt.Errorf("after its gzip member: %w", err)
}

func gunzipDigest(r io.Reader) (string, error) {
	br := bufio.NewReader(r)
	zr, err := singleMember(br)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, zr); err != nil {
		return "", err
	}
	if err := atEnd(br); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// checkSessionData reads a landed file or a skeleton completely and binds its
// header to the manifest and its name. It returns the digest of the landed
// file it stands for.
func checkSessionData(path string, info os.FileInfo, e Entry, m *Manifest) (OriginalDigest, error) {
	h, err := openListed(path, info)
	if err != nil {
		return "", err
	}
	original, err := readSessionData(h, e, m)
	if err != nil {
		_ = h.f.Close()
		return "", evidenceError("%s: %v", e.File, err)
	}
	if err := h.finish(e, false); err != nil {
		return "", err
	}
	return original, nil
}

func readSessionData(h *hashingFile, e Entry, m *Manifest) (OriginalDigest, error) {
	br := bufio.NewReader(h)
	var source io.Reader = br
	if e.Kind == KindSkeleton {
		zr, err := singleMember(br)
		if err != nil {
			return "", err
		}
		source = zr
	}
	rd, err := sessiondata.NewReader(source)
	if err != nil {
		return "", err
	}
	if err := bindHeader(rd.Header(), e, m); err != nil {
		return "", err
	}
	for {
		if _, err := rd.Next(); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return "", err
		}
	}
	if err := rd.Complete(); err != nil {
		return "", err
	}
	if e.Kind == KindSkeleton {
		if err := atEnd(br); err != nil {
			return "", err
		}
		return e.LandedDigest, nil
	}
	return e.Original(), nil
}

// bindHeader ties a Session Data header to where the file sits: the schema
// the manifest names, the manifest's session, the entry's sequence, the kind
// its name says, the time its name was stamped with, and its owner.
func bindHeader(hdr sessiondata.Header, e Entry, m *Manifest) error {
	parts := strings.Split(e.File, "/")
	name := parts[len(parts)-1]
	kind, ok := storage.LandedKind(name)
	switch {
	case hdr.Schema != m.Formats.SD:
		return fmt.Errorf("schema %s, and the manifest says %s", hdr.Schema, m.Formats.SD)
	case hdr.Session != m.Session:
		return fmt.Errorf("session %s, and the manifest's is %s", hdr.Session, m.Session)
	case hdr.Seq != e.Seq:
		return fmt.Errorf("sequence %d, and the manifest lists %d", hdr.Seq, e.Seq)
	case !ok || hdr.Kind != kind:
		return fmt.Errorf("kind %s, and its name says %s", hdr.Kind, kind)
	case (parts[1] == "provider_body") != (kind == sessiondata.KindProviderBody):
		return fmt.Errorf("a %s file in %s/", kind, parts[1])
	case parts[1] == "streams" && hdr.Stream != parts[2]:
		return fmt.Errorf("stream %q in streams/%s/", hdr.Stream, parts[2])
	case parts[1] == "runs" && hdr.Batch != parts[2]:
		return fmt.Errorf("batch %q in runs/%s/", hdr.Batch, parts[2])
	case e.Kind == KindSkeleton && (hdr.Removed == nil || OriginalDigest(hdr.Removed.Digest) != e.LandedDigest):
		return errors.New("a skeleton must keep the landed digest the manifest lists")
	case e.Kind == KindLanded && hdr.Removed != nil:
		return errors.New("a landed file says it was removed")
	}
	at, err := time.Parse(time.RFC3339Nano, hdr.At)
	if err != nil {
		return fmt.Errorf("at %q: %w", hdr.At, err)
	}
	// The manifest checks already held the name to <prefix>-<stamp>-<seq>.
	fields := strings.Split(strings.TrimSuffix(strings.TrimSuffix(name, sessiondata.Ext), sessiondata.SkeletonExt), "-")
	if stamp := fields[len(fields)-2]; storage.Stamp(at) != stamp {
		return fmt.Errorf("its name is stamped %s, and its header's time is %s", stamp, storage.Stamp(at))
	}
	return nil
}

// checkRounds reads every round. The chain must discover exactly the listed
// rounds, each range must be bounded before anything walks it, and the
// rounds must fold as one interpretation of the requested conversation and
// the manifest's session.
func checkRounds(zone *storage.Zone, m *Manifest, conversation string) error {
	chain := sessionflow.OpenChain(zone.Root(), conversation)
	files, err := chain.List()
	if err != nil {
		return evidenceError("rounds: %v", err)
	}
	var rounds []Entry
	for _, e := range m.Files {
		if e.Kind == KindRound {
			rounds = append(rounds, e)
		}
	}
	slices.SortFunc(rounds, func(x, y Entry) int { return cmpUint64(x.Round, y.Round) })
	if len(files) != len(rounds) {
		return evidenceError("the chain holds %d rounds, and the manifest lists %d", len(files), len(rounds))
	}
	view := sessionflow.NewView(conversation)
	var through uint64
	for i, rf := range files {
		e := rounds[i]
		if rf.Round != e.Round || filepath.Base(rf.Path) != e.File[strings.LastIndexByte(e.File, '/')+1:] {
			return evidenceError("the chain finds %s where the manifest lists %s", filepath.Base(rf.Path), e.File)
		}
		r, err := chain.Open(rf.Path)
		if err != nil {
			return evidenceError("%s: %v", e.File, err)
		}
		hdr := r.Header
		_, prefix, _ := roundFile(filepath.Base(rf.Path))
		switch {
		case hdr.Round != e.Round:
			return evidenceError("%s says it is round %d", e.File, hdr.Round)
		case !strings.HasPrefix(r.Commit.Digest, prefix):
			return evidenceError("%s is named for commit %s, and its commit is %s", e.File, prefix, r.Commit.Digest)
		}
		if err := boundRange(hdr.FromSeq, hdr.ThroughSeq, through, m.Head.ThroughSeq); err != nil {
			return evidenceError("%s: %v", e.File, err)
		}
		through = hdr.ThroughSeq
		if err := view.Apply(r); err != nil {
			return evidenceError("%s: %v", e.File, err)
		}
	}
	switch {
	case view.Session != m.Session:
		return evidenceError("the chain is of session %s, and the manifest's is %s", view.Session, m.Session)
	case CommitDigest(view.Digest) != m.Head.Digest:
		return evidenceError("the head round's commit is %s, and the manifest says %s", view.Digest, m.Head.Digest)
	case through != m.Head.ThroughSeq:
		return evidenceError("the chain ends at sequence %d, and the manifest's head at %d", through, m.Head.ThroughSeq)
	}
	return nil
}

// boundRange checks one round's range against the round before and the
// manifest's sequences, before anything walks it. A round starts just after
// the one before ended, and ends no earlier than one before its start, which
// is the one empty range, and no later than the last landed sequence.
func boundRange(from, through, previous, last uint64) error {
	if previous == ^uint64(0) || from != previous+1 {
		return fmt.Errorf("it starts at sequence %d, and the round before ended at %d", from, previous)
	}
	if through < previous || through > last {
		return fmt.Errorf("it ends at sequence %d, outside %d to %d", through, previous, last)
	}
	return nil
}

// checkProvider feeds every provider body record, from skeletons and landed
// files in landing order, to one session with this tree's store, so each
// occurrence is rebuilt from its own representation. A tip in the store
// must hold the body of the record it is named for, when that record is
// there.
func checkProvider(tree string, zone *storage.Zone, m *Manifest, landed []Entry, infos map[string]os.FileInfo) error {
	s := providerbody.NewSession()
	s.Use(zone.BodyStore(m.Session))
	bodies := map[string]string{}
	for _, e := range landed {
		if (e.Kind != KindLanded && e.Kind != KindSkeleton) || !strings.HasPrefix(e.File, m.Session+"/provider_body/") {
			continue
		}
		path := filepath.Join(tree, filepath.FromSlash(e.File))
		f, err := storage.OpenEvidence(path, infos[e.File])
		if err != nil {
			return evidenceError("%s: %v", e.File, err)
		}
		err = providerRecords(f, e, s, bodies)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			// The cause stays testable: a body built on one retention removed
			// is providerbody.ErrRemoved.
			return fmt.Errorf("%w: %s: %w", ErrEvidence, e.File, err)
		}
	}
	for _, e := range m.Files {
		parts := strings.Split(e.File, "/")
		if e.Kind != KindStore || parts[2] != "tips" {
			continue
		}
		if want, ok := bodies[strings.TrimSuffix(parts[4], ".gz")]; ok && ContentDigest(want) != e.Content {
			return evidenceError("%s holds %s, and the body of its record is %s", e.File, e.Content, want)
		}
	}
	return nil
}

func providerRecords(f *os.File, e Entry, s *providerbody.Session, bodies map[string]string) error {
	var source io.Reader = f
	if e.Kind == KindSkeleton {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer zr.Close()
		source = zr
	}
	rd, err := sessiondata.NewReader(source)
	if err != nil {
		return err
	}
	for row := 1; ; row++ {
		rec, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, _, err := s.AddChecked(rec, e.Kind == KindSkeleton); err != nil {
			return fmt.Errorf("row %d: %w", row, err)
		}
		if sum, ok := s.Holds(rec.ID); ok {
			id := sha256.Sum256([]byte(rec.ID))
			bodies[hex.EncodeToString(id[:])] = sum
		}
	}
}
