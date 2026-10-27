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

// Package format is what a pull's two ends agree on: the manifest of one
// conversation's evidence, its canonical file list and digest, the checks
// both ends apply to it, and evidence acceptance, the one procedure that
// decides a tree holds exactly that evidence, whole. The server freezes with
// it, the client builds with it, and asz verify checks a snapshot with it.
//
// It imports none of the pull's source, server or client packages, which all
// import it.
package format

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/internal/boundedjson"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// Schema is the version of every answer and saved manifest.
const Schema = "pull/1"

// MaxFiles bounds a manifest's entries. A source over it is refused.
const MaxFiles = 1_000_000

// ErrManifest is a manifest that breaks a rule. Its text says which.
var ErrManifest = errors.New("pull: invalid manifest")

// The digests a pull compares are of different things, and each has its own
// type, so one cannot be given where another is meant without saying so.
type (
	// StoredDigest is the SHA-256 of a file's stored bytes.
	StoredDigest string
	// OriginalDigest is the SHA-256 of the landed file a landed file or a
	// skeleton stands for, which a round's input digest binds.
	OriginalDigest string
	// ContentDigest is the SHA-256 of a store entry's bytes once
	// decompressed.
	ContentDigest string
	// CommitDigest is a round's commit digest.
	CommitDigest string
	// ManifestDigest is the SHA-256 of a manifest's canonical file list.
	ManifestDigest string
)

// Kind is what a manifest entry is.
type Kind string

// The four kinds of evidence a manifest lists.
const (
	KindRound    Kind = "round"
	KindLanded   Kind = "landed"
	KindSkeleton Kind = "skeleton"
	KindStore    Kind = "store"
)

// Entry is one file of the evidence.
type Entry struct {
	// File is the path relative to the snapshot, with forward slashes.
	File string
	Kind Kind
	// Seq is a landed file's or a skeleton's sequence, Round a round's
	// number. Each is zero for the other kinds.
	Seq, Round uint64
	// Bytes and Digest are the stored file's size and SHA-256.
	Bytes  uint64
	Digest StoredDigest
	// LandedDigest is a skeleton's original landed-file digest.
	LandedDigest OriginalDigest
	// Content is a store entry's decompressed SHA-256.
	Content ContentDigest
}

// Original is the digest of the landed file an entry stands for: a landed
// file's stored digest, or the digest a skeleton keeps of the file it
// replaced. Empty for rounds and store entries.
func (e Entry) Original() OriginalDigest {
	switch e.Kind {
	case KindLanded:
		// A landed file is the file the round bound.
		return OriginalDigest(e.Digest)
	case KindSkeleton:
		return e.LandedDigest
	}
	return ""
}

// Head is the round a manifest was taken at.
type Head struct {
	Round      uint64
	Digest     CommitDigest
	ThroughSeq uint64
}

// Formats are the evidence formats' versions.
type Formats struct {
	SD, SF string
}

// Manifest is one conversation's evidence at its head round.
type Manifest struct {
	Conversation, Session string
	Head                  Head
	// Generation is the session's removal generation when it was scanned.
	Generation int64
	Formats    Formats
	// Files is sorted by the bytes of File.
	Files []Entry
}

// Response is what the manifest route adds to a manifest: the server's tag
// for the frozen snapshot, and the lease the client holds on it.
type Response struct {
	Snapshot, Lease string
}

// ReadManifest reads a saved manifest and applies every manifest check, for
// the conversation that was asked for. Unknown members are passed over, as
// the schema allows, without being kept.
func ReadManifest(r io.Reader, conversation string) (*Manifest, error) {
	m, _, err := readManifest(r, conversation, false)
	return m, err
}

// ReadManifestResponse reads the manifest route's answer: the manifest, its
// snapshot tag and its lease.
func ReadManifestResponse(r io.Reader, conversation string) (*Manifest, Response, error) {
	return readManifest(r, conversation, true)
}

func manifestError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrManifest, fmt.Sprintf(format, args...))
}

func readManifest(r io.Reader, conversation string, response bool) (*Manifest, Response, error) {
	var resp Response
	if err := storage.CheckNamePart(conversation); err != nil {
		return nil, resp, manifestError("conversation %q: %v", conversation, err)
	}
	s := boundedjson.New(r)
	if err := openObject(s); err != nil {
		return nil, resp, manifestError("%v", err)
	}
	m := &Manifest{}
	seen := map[string]bool{}
	for {
		token, err := s.Next()
		if err != nil {
			return nil, resp, manifestError("%v", err)
		}
		if token.Kind == boundedjson.ObjectEnd {
			break
		}
		name := token.Text
		known := true
		switch name {
		case "schema":
			var schema string
			if schema, err = stringValue(s); err == nil && schema != Schema {
				err = fmt.Errorf("%q is not %s", schema, Schema)
			}
		case "conversation":
			m.Conversation, err = stringValue(s)
		case "session":
			m.Session, err = stringValue(s)
		case "round":
			m.Head, err = readHead(s)
		case "generation":
			m.Generation, err = int64Value(s)
		case "formats":
			m.Formats, err = readFormats(s)
		case "files":
			m.Files, err = readFiles(s, conversation)
		case "snapshot":
			if !response {
				known = false
				err = s.Skip()
				break
			}
			if resp.Snapshot, err = stringValue(s); err == nil {
				_, err = storage.ParseTag(resp.Snapshot)
			}
		case "lease":
			if !response {
				known = false
				err = s.Skip()
				break
			}
			if resp.Lease, err = stringValue(s); err == nil && !storage.Digest64(resp.Lease) {
				err = errors.New("not 64 lower-case hexadecimal characters")
			}
		default:
			known = false
			err = s.Skip()
		}
		if err != nil {
			return nil, resp, manifestError("%s: %v", name, err)
		}
		if known {
			seen[name] = true
		}
	}
	if _, err := s.Next(); !errors.Is(err, io.EOF) {
		return nil, resp, manifestError("content follows the manifest: %v", err)
	}
	required := []string{"schema", "conversation", "session", "round", "generation", "formats", "files"}
	if response {
		required = append(required, "snapshot", "lease")
	}
	for _, name := range required {
		if !seen[name] {
			return nil, resp, manifestError("%s is missing", name)
		}
	}
	// Each entry and its order were checked as it was read.
	if err := m.checkWhole(conversation); err != nil {
		return nil, resp, err
	}
	return m, resp, nil
}

func openObject(s *boundedjson.Scanner) error {
	token, err := s.Next()
	if err != nil {
		return err
	}
	if token.Kind != boundedjson.Object {
		return errors.New("expected an object")
	}
	return nil
}

// stringValue reads a string. A null, as any other kind, is refused: a
// known member is never null.
func stringValue(s *boundedjson.Scanner) (string, error) {
	token, err := s.Next()
	if err != nil {
		return "", err
	}
	if token.Kind != boundedjson.String {
		return "", errors.New("expected a string")
	}
	return token.Text, nil
}

// digestValue reads a digest of one kind. Its spelling is checked where the
// member is.
func digestValue[D ~string](s *boundedjson.Scanner) (D, error) {
	text, err := stringValue(s)
	return D(text), err
}

func uint64Value(s *boundedjson.Scanner) (uint64, error) {
	token, err := s.Next()
	if err != nil {
		return 0, err
	}
	return token.Uint64()
}

func int64Value(s *boundedjson.Scanner) (int64, error) {
	token, err := s.Next()
	if err != nil {
		return 0, err
	}
	return token.Int64()
}

// members reads an object, giving each member's name to fn, which reads the
// value of a name it knows and reports whether it did. Unknown members are
// skipped. Every name in required must appear.
func members(s *boundedjson.Scanner, required []string, fn func(name string) (bool, error)) error {
	if err := openObject(s); err != nil {
		return err
	}
	return membersOpened(s, required, fn)
}

// membersOpened is members for an object whose opening brace was read.
func membersOpened(s *boundedjson.Scanner, required []string, fn func(name string) (bool, error)) error {
	seen := map[string]bool{}
	for {
		token, err := s.Next()
		if err != nil {
			return err
		}
		if token.Kind == boundedjson.ObjectEnd {
			break
		}
		known, err := fn(token.Text)
		if err != nil {
			return fmt.Errorf("%s: %w", token.Text, err)
		}
		if !known {
			if err := s.Skip(); err != nil {
				return err
			}
			continue
		}
		seen[token.Text] = true
	}
	for _, name := range required {
		if !seen[name] {
			return fmt.Errorf("%s is missing", name)
		}
	}
	return nil
}

func readHead(s *boundedjson.Scanner) (Head, error) {
	var h Head
	err := members(s, []string{"round", "digest", "through_seq"}, func(name string) (bool, error) {
		var err error
		switch name {
		case "round":
			h.Round, err = uint64Value(s)
		case "digest":
			h.Digest, err = digestValue[CommitDigest](s)
		case "through_seq":
			h.ThroughSeq, err = uint64Value(s)
		default:
			return false, nil
		}
		return true, err
	})
	return h, err
}

func readFormats(s *boundedjson.Scanner) (Formats, error) {
	var f Formats
	err := members(s, []string{"sd", "sf"}, func(name string) (bool, error) {
		var err error
		switch name {
		case "sd":
			f.SD, err = stringValue(s)
		case "sf":
			f.SF, err = stringValue(s)
		default:
			return false, nil
		}
		return true, err
	})
	return f, err
}

// readFiles streams the entries, checking each and its order before it is
// kept, and stops before entry MaxFiles+1.
func readFiles(s *boundedjson.Scanner, conversation string) ([]Entry, error) {
	token, err := s.Next()
	if err != nil {
		return nil, err
	}
	if token.Kind != boundedjson.Array {
		return nil, errors.New("expected an array")
	}
	var out []Entry
	for {
		token, err := s.Next()
		if err != nil {
			return nil, err
		}
		if token.Kind == boundedjson.ArrayEnd {
			return out, nil
		}
		if len(out) == MaxFiles {
			return nil, fmt.Errorf("more than %d entries", MaxFiles)
		}
		if token.Kind != boundedjson.Object {
			return nil, errors.New("an entry is not an object")
		}
		e, err := readEntry(s)
		if err != nil {
			return nil, fmt.Errorf("entry %d: %w", len(out)+1, err)
		}
		if err := e.check(conversation); err != nil {
			return nil, fmt.Errorf("entry %d: %w", len(out)+1, err)
		}
		if n := len(out); n > 0 && out[n-1].File >= e.File {
			return nil, fmt.Errorf("entry %d: %q does not sort after %q", n+1, e.File, out[n-1].File)
		}
		out = append(out, e)
	}
}

// readEntry reads the members of an entry whose opening brace was read.
func readEntry(s *boundedjson.Scanner) (Entry, error) {
	var e Entry
	seen := map[string]bool{}
	for {
		token, err := s.Next()
		if err != nil {
			return e, err
		}
		if token.Kind == boundedjson.ObjectEnd {
			break
		}
		name := token.Text
		switch name {
		case "file":
			e.File, err = stringValue(s)
		case "kind":
			var kind string
			kind, err = stringValue(s)
			e.Kind = Kind(kind)
		case "bytes":
			e.Bytes, err = uint64Value(s)
		case "digest":
			e.Digest, err = digestValue[StoredDigest](s)
		case "seq":
			e.Seq, err = uint64Value(s)
		case "round":
			e.Round, err = uint64Value(s)
		case "landed_digest":
			e.LandedDigest, err = digestValue[OriginalDigest](s)
		case "content":
			e.Content, err = digestValue[ContentDigest](s)
		default:
			if err := s.Skip(); err != nil {
				return e, err
			}
			continue
		}
		if err != nil {
			return e, fmt.Errorf("%s: %w", name, err)
		}
		seen[name] = true
	}
	for _, name := range []string{"file", "kind", "bytes", "digest"} {
		if !seen[name] {
			return e, fmt.Errorf("%s is missing", name)
		}
	}
	// Each kind has exactly its own members: one that belongs to another
	// kind is refused.
	own := map[Kind][]string{
		KindRound: {"round"}, KindLanded: {"seq"}, KindSkeleton: {"seq", "landed_digest"}, KindStore: {"content"},
	}
	want, ok := own[e.Kind]
	if !ok {
		return e, fmt.Errorf("kind %q is not round, landed, skeleton or store", e.Kind)
	}
	for _, name := range []string{"seq", "round", "landed_digest", "content"} {
		mine := false
		for _, w := range want {
			mine = mine || w == name
		}
		switch {
		case mine && !seen[name]:
			return e, fmt.Errorf("a %s entry needs %s", e.Kind, name)
		case !mine && seen[name]:
			return e, fmt.Errorf("a %s entry has no %s", e.Kind, name)
		}
	}
	return e, nil
}

var (
	roundFileRe = regexp.MustCompile(`^r([0-9]{6,})-([0-9a-f]{12})\.sf$`)
	storeFileRe = regexp.MustCompile(`^[0-9a-f]{64}\.gz$`)
)

// reservedTop are the names a snapshot keeps for its own controls. No
// evidence path may start with one.
var reservedTop = []string{"_copy", "_manifest", "_root", "_leases", "_work"}

// check applies the rules one entry can be held to alone. The session is
// bound after the whole manifest is read, since its member may come later.
func (e Entry) check(conversation string) error {
	if err := storage.CheckRelativePath(e.File); err != nil {
		return err
	}
	// Each kind has its own members and no other's, in memory as in JSON.
	switch {
	case e.Kind != KindRound && e.Round != 0:
		return fmt.Errorf("%s: a %s entry has no round", e.File, e.Kind)
	case e.Kind != KindLanded && e.Kind != KindSkeleton && e.Seq != 0:
		return fmt.Errorf("%s: a %s entry has no seq", e.File, e.Kind)
	case e.Kind != KindSkeleton && e.LandedDigest != "":
		return fmt.Errorf("%s: a %s entry has no landed_digest", e.File, e.Kind)
	case e.Kind != KindStore && e.Content != "":
		return fmt.Errorf("%s: a %s entry has no content", e.File, e.Kind)
	}
	if e.Bytes == 0 {
		return fmt.Errorf("%s is empty, and empty evidence is invalid", e.File)
	}
	// A file's offsets are signed 64-bit numbers on every platform asz runs
	// on: a size past them is no file's.
	if e.Bytes > math.MaxInt64 {
		return fmt.Errorf("%s is %d bytes, more than a file can hold", e.File, e.Bytes)
	}
	if !storage.Digest64(string(e.Digest)) {
		return fmt.Errorf("%s: digest is not 64 lower-case hexadecimal characters", e.File)
	}
	parts := strings.Split(e.File, "/")
	for _, r := range reservedTop {
		if strings.EqualFold(parts[0], r) {
			return fmt.Errorf("%s overlaps the snapshot's own %s", e.File, r)
		}
	}
	switch e.Kind {
	case KindRound:
		if len(parts) != 4 || parts[0] != "_conversations" || parts[1] != conversation || parts[2] != "rounds" {
			return fmt.Errorf("%s is not a round of conversation %s", e.File, conversation)
		}
		n, _, err := roundFile(parts[3])
		if err != nil {
			return fmt.Errorf("%s: %w", e.File, err)
		}
		if e.Round == 0 || n != e.Round {
			return fmt.Errorf("%s is named for round %d and listed as round %d", e.File, n, e.Round)
		}
		return nil
	case KindLanded, KindSkeleton:
		if strings.HasPrefix(parts[0], "_") {
			return fmt.Errorf("%s: a session name cannot start with _", e.File)
		}
		owner := len(parts) == 4 && (parts[1] == "streams" || parts[1] == "runs")
		provider := len(parts) == 3 && parts[1] == "provider_body"
		if !owner && !provider {
			return fmt.Errorf("%s is not in a session's streams/<owner>/, runs/<owner>/ or provider_body/", e.File)
		}
		name := parts[len(parts)-1]
		seq, err := landedName(name, e.Kind)
		if err != nil {
			return fmt.Errorf("%s: %w", e.File, err)
		}
		if e.Seq == 0 || seq != e.Seq {
			return fmt.Errorf("%s is named for sequence %d and listed as %d", e.File, seq, e.Seq)
		}
		if e.Kind == KindSkeleton && !storage.Digest64(string(e.LandedDigest)) {
			return fmt.Errorf("%s: landed_digest is not 64 lower-case hexadecimal characters", e.File)
		}
		return nil
	case KindStore:
		if strings.HasPrefix(parts[0], "_") {
			return fmt.Errorf("%s: a session name cannot start with _", e.File)
		}
		if !storage.Digest64(string(e.Content)) {
			return fmt.Errorf("%s: content is not 64 lower-case hexadecimal characters", e.File)
		}
		tip := len(parts) == 5 && parts[1] == "provider" && parts[2] == "tips" && generationName(parts[3])
		piece := len(parts) == 4 && parts[1] == "provider" && parts[2] == "pieces"
		if (!tip && !piece) || !storeFileRe.MatchString(parts[len(parts)-1]) {
			return fmt.Errorf("%s is not a provider store tip or piece", e.File)
		}
		if piece && strings.TrimSuffix(parts[3], ".gz") != string(e.Content) {
			return fmt.Errorf("%s: a piece holds what its name says, not %s", e.File, e.Content)
		}
		return nil
	}
	return fmt.Errorf("kind %q is unknown", e.Kind)
}

// roundFile reads a round's file name: its number, with a checked
// conversion, and the first twelve characters of its commit digest.
func roundFile(name string) (uint64, string, error) {
	m := roundFileRe.FindStringSubmatch(name)
	if m == nil {
		return 0, "", errors.New("a round is named r<number of six or more digits>-<twelve hexadecimal characters>.sf")
	}
	n, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("round number %s: %w", m[1], err)
	}
	if n == 0 {
		return 0, "", errors.New("rounds start at 1")
	}
	return n, m[2], nil
}

// generationName is a tip generation directory: a decimal whole number from
// 1, without a leading zero. The store reads 01 as 1, so its tips could not
// be found there.
func generationName(s string) bool {
	if s == "" || s[0] == '0' || strings.Trim(s, "0123456789") != "" {
		return false
	}
	n, err := strconv.ParseUint(s, 10, 64)
	return err == nil && n <= math.MaxInt64
}

// landedName checks a landed file's or skeleton's name is one storage reads
// for its kind, and returns its sequence as storage reads it. Two spellings of
// one sequence are two entries for it, which the sequence check refuses.
func landedName(name string, kind Kind) (uint64, error) {
	ext := sessiondata.Ext
	if kind == KindSkeleton {
		ext = sessiondata.SkeletonExt
	}
	if !strings.HasSuffix(name, ext) {
		return 0, fmt.Errorf("a %s file ends in %s", kind, ext)
	}
	if _, ok := storage.LandedKind(name); !ok {
		return 0, errors.New("not a landed file name a producer writes")
	}
	seq, ok := storage.LandedFileSeq(name)
	if !ok {
		return 0, errors.New("no sequence in the name")
	}
	return seq, nil
}

// Check applies the manifest checks for the conversation that was asked
// for, to a manifest built in memory: each entry and its order, as reading
// checks them, then checkWhole.
func (m *Manifest) Check(conversation string) error {
	for i, e := range m.Files {
		if i > 0 && m.Files[i-1].File >= e.File {
			return manifestError("%q does not sort after %q", e.File, m.Files[i-1].File)
		}
		if err := e.check(conversation); err != nil {
			return manifestError("%v", err)
		}
	}
	return m.checkWhole(conversation)
}

// checkWhole applies the checks that need the whole manifest: the
// identities, the head, the formats, each entry's session, and over the
// whole list the letter-case aliases, the rounds 1 to the head and the
// sequences 1 to its through_seq, each exactly once.
func (m *Manifest) checkWhole(conversation string) error {
	switch {
	case m.Conversation != conversation:
		return manifestError("it is of conversation %q, not %q", m.Conversation, conversation)
	case storage.CheckNamePart(m.Session) != nil:
		return manifestError("session %q is not a name any machine can hold", m.Session)
	case strings.HasPrefix(m.Session, "_"):
		return manifestError("session %q starts with _, which storage keeps for its own directories", m.Session)
	case m.Head.Round == 0:
		return manifestError("the head round is 0")
	case !storage.Digest64(string(m.Head.Digest)):
		return manifestError("the head digest is not 64 lower-case hexadecimal characters")
	case m.Generation < 0:
		return manifestError("generation %d is negative", m.Generation)
	case m.Formats.SD != sessiondata.Schema || m.Formats.SF != sessionflow.Schema:
		return manifestError("formats %s and %s; this asz reads %s and %s", m.Formats.SD, m.Formats.SF, sessiondata.Schema, sessionflow.Schema)
	case len(m.Files) == 0:
		return manifestError("it lists no file")
	case len(m.Files) > MaxFiles:
		return manifestError("it lists more than %d files", MaxFiles)
	}
	names := make([]string, 0, len(m.Files))
	var rounds, seqs []uint64
	for _, e := range m.Files {
		if e.Kind != KindRound && !strings.HasPrefix(e.File, m.Session+"/") {
			return manifestError("%s is outside session %s", e.File, m.Session)
		}
		switch e.Kind {
		case KindRound:
			rounds = append(rounds, e.Round)
			if e.Round == m.Head.Round {
				if _, prefix, _ := roundFile(e.File[strings.LastIndexByte(e.File, '/')+1:]); prefix != string(m.Head.Digest[:12]) {
					return manifestError("the head round %s is not named for the head digest", e.File)
				}
			}
		case KindLanded, KindSkeleton:
			seqs = append(seqs, e.Seq)
		}
		names = append(names, e.File)
	}
	if err := storage.CheckCaseFolding(names); err != nil {
		return manifestError("%v", err)
	}
	if err := oneToN(rounds, m.Head.Round, "round"); err != nil {
		return err
	}
	return oneToN(seqs, m.Head.ThroughSeq, "sequence")
}

// oneToN checks that values hold 1 to n, each once. It sorts the values it
// was given and walks them, never counting to an unchecked n.
func oneToN(values []uint64, n uint64, what string) error {
	slices.Sort(values)
	for i, v := range values {
		if v != uint64(i)+1 {
			if i > 0 && v == values[i-1] {
				return manifestError("%s %d is listed twice", what, v)
			}
			return manifestError("%s %d is missing", what, uint64(i)+1)
		}
	}
	if uint64(len(values)) != n {
		return manifestError("it lists %s 1 to %d, and the head says %d", what, len(values), n)
	}
	return nil
}

// WriteManifest writes m as the saved manifest: one object, its files in
// their sorted order, without a server tag or lease. Each entry is written
// as it is reached, so a long list is never held twice.
func WriteManifest(w io.Writer, m *Manifest) error {
	return writeManifest(w, m, nil)
}

// WriteManifestResponse writes the manifest route's answer: the saved
// manifest's members, then the snapshot's tag and the lease granted.
func WriteManifestResponse(w io.Writer, m *Manifest, r Response) error {
	return writeManifest(w, m, &r)
}

func writeManifest(w io.Writer, m *Manifest, r *Response) error {
	bw := bufio.NewWriter(w)
	str := func(s string) string {
		b, _ := json.Marshal(s)
		return string(b)
	}
	fmt.Fprintf(bw, `{"schema":%s,"conversation":%s,"session":%s,"round":{"round":%d,"digest":%s,"through_seq":%d},"generation":%d,"formats":{"sd":%s,"sf":%s},"files":[`,
		str(Schema), str(m.Conversation), str(m.Session), m.Head.Round, str(string(m.Head.Digest)), m.Head.ThroughSeq,
		m.Generation, str(m.Formats.SD), str(m.Formats.SF))
	for i, e := range m.Files {
		if i > 0 {
			_ = bw.WriteByte(',')
		}
		fmt.Fprintf(bw, `{"file":%s,"kind":%s,"bytes":%d,"digest":%s`, str(e.File), str(string(e.Kind)), e.Bytes, str(string(e.Digest)))
		switch e.Kind {
		case KindRound:
			fmt.Fprintf(bw, `,"round":%d`, e.Round)
		case KindLanded:
			fmt.Fprintf(bw, `,"seq":%d`, e.Seq)
		case KindSkeleton:
			fmt.Fprintf(bw, `,"seq":%d,"landed_digest":%s`, e.Seq, str(string(e.LandedDigest)))
		case KindStore:
			fmt.Fprintf(bw, `,"content":%s`, str(string(e.Content)))
		}
		_ = bw.WriteByte('}')
	}
	_, _ = bw.WriteString("]")
	if r != nil {
		fmt.Fprintf(bw, `,"snapshot":%s,"lease":%s`, str(r.Snapshot), str(r.Lease))
	}
	_, _ = bw.WriteString("}\n")
	return bw.Flush()
}
