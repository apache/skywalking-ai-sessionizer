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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/boundedjson"
)

// CopySchema names the version of both copy markers. A marker of any other
// version, or with a member this version does not list, is refused rather
// than read as far as it goes: a marker decides what a command may read.
const CopySchema = "pull-copy/1"

// ErrMarker says a copy marker is malformed, or names another remote,
// conversation or tag than its place. It is never filled in or repaired.
var ErrMarker = errors.New("storage: malformed copy marker")

// Publishing is a publication in progress. While it is recorded, its tag is
// reserved and blocked: nothing reads it, and recovery discards it whole.
type Publishing struct {
	// Build is the random name of the private build being published.
	Build string `json:"build"`
	// Tag equals the history's greatest tag.
	Tag string `json:"tag"`
	// ManifestDigest is the full digest of the build's accepted file list.
	ManifestDigest string `json:"manifest_digest"`
}

// OuterCopy is the marker of a conversation's directory under _remote: what
// it copies, and its publication history. The history outlives every
// snapshot, so a tag is never issued twice within it.
type OuterCopy struct {
	Remote       string `json:"remote"`
	Conversation string `json:"conversation"`
	// GreatestTag is the greatest tag ever reserved, empty before the first.
	GreatestTag string      `json:"greatest_tag"`
	Publishing  *Publishing `json:"publishing,omitempty"`
	// The marker also lists every tag whose tree was removed, or is being
	// removed, in increasing order. It is never pruned: a tree a power loss
	// brings back under a listed tag is refused and removed again. One tag is
	// added per pull and nothing else bounds the list, so it is streamed from
	// the file whenever it is checked or rewritten, never held: IsRemoved,
	// EachRemoved and WriteOuterCopy.
}

// SnapshotCopy is a published snapshot's marker. It is written once, before
// the snapshot gets its name, and never rewritten.
type SnapshotCopy struct {
	Remote       string `json:"remote"`
	Conversation string `json:"conversation"`
	// Session is the snapshot's session. The judge takes it from here.
	Session string `json:"session"`
	// Round and Digest are the head round and its full commit digest.
	Round  uint64 `json:"round"`
	Digest string `json:"digest"`
	// ManifestDigest is the full digest of the snapshot's file list.
	ManifestDigest string `json:"manifest_digest"`
	// PublishedAt is the publication time. Its Stamp is the snapshot's tag.
	// It is written in UTC with exactly nine fractional digits, as the tag is.
	PublishedAt time.Time `json:"-"`
}

// publishedLayout writes a publication time as RFC 3339 with every one of its
// nine fractional digits, so the text names the same instant as the tag and
// reads back byte for byte. time.Time's own JSON drops trailing zeros.
const publishedLayout = "2006-01-02T15:04:05.000000000Z"

// CheckCopyNames checks the two names that make a copy's directories,
// _remote/<remote>/<conversation>, before any path is made from them. Neither
// may take a name pull's own directories use.
func CheckCopyNames(remote, conversation string) error {
	if err := CheckRemoteName(remote); err != nil {
		return err
	}
	if err := CheckNamePart(conversation); err != nil {
		return err
	}
	for _, name := range []string{remote, conversation} {
		if protectedName(name) {
			return fmt.Errorf("storage: %q is a name pull's own directories use", name)
		}
	}
	return nil
}

// Digest64 reports whether s is a full digest as pull writes every one: 64
// lower-case hexadecimal characters.
func Digest64(s string) bool { return lowerHex64(s) }

func markerError(path, format string, args ...any) error {
	return fmt.Errorf("%w: %s: %s", ErrMarker, path, fmt.Sprintf(format, args...))
}

// Check validates the marker and binds it to the remote and conversation its
// directory names.
func (c OuterCopy) Check(remote, conversation string) error {
	if err := CheckCopyNames(c.Remote, c.Conversation); err != nil {
		return fmt.Errorf("%w: %w", ErrMarker, err)
	}
	if c.Remote != remote || c.Conversation != conversation {
		return fmt.Errorf("%w: it names %s/%s, not %s/%s", ErrMarker, c.Remote, c.Conversation, remote, conversation)
	}
	if c.GreatestTag != "" {
		if _, err := ParseTag(c.GreatestTag); err != nil {
			return fmt.Errorf("%w: greatest tag: %w", ErrMarker, err)
		}
	}
	if p := c.Publishing; p != nil {
		if !lowerHex64(p.Build) || !Digest64(p.ManifestDigest) || p.Tag == "" || p.Tag != c.GreatestTag {
			return fmt.Errorf("%w: malformed publication intent", ErrMarker)
		}
	}
	return nil
}

// Reserve takes the next tag for a publication and records the intent with
// it. The caller writes the record durably before anything is published under
// the tag. One publication is in progress at a time.
func (c *OuterCopy) Reserve(now time.Time, build, manifestDigest string) (string, error) {
	if c.Publishing != nil {
		return "", fmt.Errorf("storage: the publication of %s has not finished", c.Publishing.Tag)
	}
	if !lowerHex64(build) || !Digest64(manifestDigest) {
		return "", fmt.Errorf("storage: malformed build %q or manifest digest %q", build, manifestDigest)
	}
	tag, err := NextTag(now, c.GreatestTag)
	if err != nil {
		return "", err
	}
	c.GreatestTag = tag
	c.Publishing = &Publishing{Build: build, Tag: tag, ManifestDigest: manifestDigest}
	return tag, nil
}

// Check validates the marker and binds it to its directory's remote,
// conversation and tag.
func (c SnapshotCopy) Check(remote, conversation, tag string) error {
	if err := CheckCopyNames(c.Remote, c.Conversation); err != nil {
		return fmt.Errorf("%w: %w", ErrMarker, err)
	}
	if c.Remote != remote || c.Conversation != conversation {
		return fmt.Errorf("%w: it names %s/%s, not %s/%s", ErrMarker, c.Remote, c.Conversation, remote, conversation)
	}
	if err := CheckNamePart(c.Session); err != nil || c.Session[0] == '_' {
		return fmt.Errorf("%w: session %q", ErrMarker, c.Session)
	}
	if c.Round < 1 || !Digest64(c.Digest) || !Digest64(c.ManifestDigest) {
		return fmt.Errorf("%w: malformed head or manifest digest", ErrMarker)
	}
	if Stamp(c.PublishedAt) != tag {
		return fmt.Errorf("%w: published at %s, which is not tag %s", ErrMarker, c.PublishedAt.Format(time.RFC3339Nano), tag)
	}
	return nil
}

// ReadOuterCopy reads and checks the marker of the conversation directory
// dir, which _remote/<remote>/<conversation> names, the removal list included.
func ReadOuterCopy(dir, remote, conversation string) (OuterCopy, error) {
	return scanOuterCopy(dir, remote, conversation, nil)
}

// IsRemoved reads the marker of the conversation directory dir and reports
// whether tag is in its removal list. Every caller reads it from the file:
// none relies on a list it read before.
func IsRemoved(dir, remote, conversation, tag string) (bool, error) {
	found := false
	_, err := scanOuterCopy(dir, remote, conversation, func(removed string) error {
		found = found || removed == tag
		return nil
	})
	return found, err
}

// EachRemoved calls fn with every tag in the removal list, in increasing order,
// streamed from the marker of the conversation directory dir. A sweep uses it
// to delete any tree a power loss brought back under a removed tag.
func EachRemoved(dir, remote, conversation string, fn func(tag string) error) error {
	_, err := scanOuterCopy(dir, remote, conversation, fn)
	return err
}

// scanOuterCopy reads the outer marker. With a visitor, it reads the marker
// twice through one handle: first to check all of it, then to call visit with
// each removed tag. A visitor deletes trees, so it never sees a tag from a
// marker that turns out malformed, incomplete or another conversation's.
func scanOuterCopy(dir, remote, conversation string, visit func(tag string) error) (_ OuterCopy, err error) {
	file, err := openMarker(dir)
	if err != nil {
		return OuterCopy{}, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	path := filepath.Join(dir, CopyMarker)
	c, err := parseOuterCopy(file, path, remote, conversation, nil)
	if err != nil || visit == nil {
		return c, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return OuterCopy{}, err
	}
	return parseOuterCopy(file, path, remote, conversation, visit)
}

// parseOuterCopy checks an outer marker as it streams past, calling visit,
// when it is not nil, with each removed tag.
func parseOuterCopy(r io.Reader, path, remote, conversation string, visit func(tag string) error) (OuterCopy, error) {
	var c OuterCopy
	var last string
	err := parseMarker(r, path, func(s *boundedjson.Scanner, name string) error {
		var err error
		switch name {
		case "remote":
			c.Remote, err = markerString(s)
		case "conversation":
			c.Conversation, err = markerString(s)
		case "greatest_tag":
			c.GreatestTag, err = markerString(s)
		case "removed":
			err = markerTags(s, func(tag string) error {
				// Canonical tags order by their bytes.
				if last != "" && tag <= last {
					return markerError(path, "removed tags are not in increasing order")
				}
				last = tag
				if visit != nil {
					return visit(tag)
				}
				return nil
			})
		case "publishing":
			c.Publishing, err = markerPublishing(s)
		default:
			return markerError(path, "unknown member %q", name)
		}
		return err
	}, []string{"remote", "conversation", "greatest_tag", "removed"})
	if err != nil {
		return OuterCopy{}, err
	}
	if err := c.Check(remote, conversation); err != nil {
		return OuterCopy{}, fmt.Errorf("%s: %w", path, err)
	}
	// The members may come in any order, so the greatest removed tag is
	// checked against the history once both are known.
	if last > c.GreatestTag {
		return OuterCopy{}, markerError(path, "removed tag %s was never reserved", last)
	}
	return c, nil
}

// ReadSnapshotCopy reads and checks the marker of the snapshot directory dir,
// which _remote/<remote>/<conversation>/<tag> names.
func ReadSnapshotCopy(dir, remote, conversation, tag string) (SnapshotCopy, error) {
	var c SnapshotCopy
	path := filepath.Join(dir, CopyMarker)
	err := readMarker(dir, func(s *boundedjson.Scanner, name string) error {
		var err error
		switch name {
		case "remote":
			c.Remote, err = markerString(s)
		case "conversation":
			c.Conversation, err = markerString(s)
		case "session":
			c.Session, err = markerString(s)
		case "round":
			c.Round, err = markerUint64(s)
		case "digest":
			c.Digest, err = markerString(s)
		case "manifest_digest":
			c.ManifestDigest, err = markerString(s)
		case "published_at":
			var text string
			if text, err = markerString(s); err == nil {
				c.PublishedAt, err = time.Parse(publishedLayout, text)
				if err == nil && c.PublishedAt.Format(publishedLayout) != text {
					err = markerError(path, "published_at %q is not a canonical UTC time", text)
				}
			}
		default:
			return markerError(path, "unknown member %q", name)
		}
		return err
	}, []string{"remote", "conversation", "session", "round", "digest", "manifest_digest", "published_at"})
	if err != nil {
		return SnapshotCopy{}, err
	}
	if err := c.Check(remote, conversation, tag); err != nil {
		return SnapshotCopy{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// readMarker parses the marker in dir. member reads the value of each member
// it knows; schema is checked here. Every required member must appear. The
// marker is opened through dir's checked handle and never through a link: a
// marker decides ownership, and a link to a matching marker elsewhere would
// lend a tree an owner it does not have.
func readMarker(dir string, member func(*boundedjson.Scanner, string) error, required []string) (err error) {
	file, err := openMarker(dir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	return parseMarker(file, filepath.Join(dir, CopyMarker), member, required)
}

// openMarker opens the marker in dir through the directory's bound handle.
func openMarker(dir string) (*os.File, error) {
	checked, err := OpenDir(dir)
	if err != nil {
		return nil, err
	}
	file, err := checked.open(CopyMarker, readShared, 0)
	return file, errors.Join(err, checked.Close())
}

// parseMarker reads one marker object from r. member reads the value of each
// member it knows; schema is checked here. Every required member must appear.
func parseMarker(r io.Reader, path string, member func(*boundedjson.Scanner, string) error, required []string) error {
	s := boundedjson.New(r)
	if token, err := s.Next(); err != nil {
		return markerError(path, "%v", err)
	} else if token.Kind != boundedjson.Object {
		return markerError(path, "not an object")
	}
	seen := map[string]bool{}
	for {
		token, err := s.Next()
		if err != nil {
			return markerError(path, "%v", err)
		}
		if token.Kind == boundedjson.ObjectEnd {
			break
		}
		seen[token.Text] = true
		if token.Text == "schema" {
			schema, err := markerString(s)
			if err != nil {
				return markerError(path, "%v", err)
			}
			if schema != CopySchema {
				return markerError(path, "schema %q is not %s", schema, CopySchema)
			}
			continue
		}
		if err := member(s, token.Text); err != nil {
			if errors.Is(err, ErrMarker) {
				return err
			}
			return markerError(path, "%s: %v", token.Text, err)
		}
	}
	if _, err := s.Next(); !errors.Is(err, io.EOF) {
		return markerError(path, "content follows the marker: %v", err)
	}
	for _, name := range append([]string{"schema"}, required...) {
		if !seen[name] {
			return markerError(path, "%s is missing", name)
		}
	}
	return nil
}

func markerString(s *boundedjson.Scanner) (string, error) {
	token, err := s.Next()
	if err != nil {
		return "", err
	}
	if token.Kind != boundedjson.String {
		return "", errors.New("expected a string")
	}
	return token.Text, nil
}

func markerUint64(s *boundedjson.Scanner) (uint64, error) {
	token, err := s.Next()
	if err != nil {
		return 0, err
	}
	return token.Uint64()
}

// markerTags streams an array of canonical tags to fn, holding none of them.
func markerTags(s *boundedjson.Scanner, fn func(tag string) error) error {
	token, err := s.Next()
	if err != nil {
		return err
	}
	if token.Kind != boundedjson.Array {
		return errors.New("expected an array")
	}
	for {
		token, err := s.Next()
		if err != nil {
			return err
		}
		if token.Kind == boundedjson.ArrayEnd {
			return nil
		}
		if token.Kind != boundedjson.String {
			return errors.New("expected tags")
		}
		if _, err := ParseTag(token.Text); err != nil {
			return err
		}
		if err := fn(token.Text); err != nil {
			return err
		}
	}
}

func markerPublishing(s *boundedjson.Scanner) (*Publishing, error) {
	token, err := s.Next()
	if err != nil {
		return nil, err
	}
	if token.Kind != boundedjson.Object {
		return nil, errors.New("expected an object")
	}
	var p Publishing
	seen := map[string]bool{}
	for {
		token, err := s.Next()
		if err != nil {
			return nil, err
		}
		if token.Kind == boundedjson.ObjectEnd {
			break
		}
		var value string
		if value, err = markerString(s); err != nil {
			return nil, err
		}
		switch token.Text {
		case "build":
			p.Build = value
		case "tag":
			p.Tag = value
		case "manifest_digest":
			p.ManifestDigest = value
		default:
			return nil, fmt.Errorf("unknown member %q", token.Text)
		}
		seen[token.Text] = true
	}
	if !seen["build"] || !seen["tag"] || !seen["manifest_digest"] {
		return nil, errors.New("publishing lacks a member")
	}
	return &p, nil
}

// WriteOuterCopy checks c and replaces the conversation's marker with a fresh,
// durable write, with remove, when it is not empty, added to the removal list
// in its place. The list is streamed from the marker being replaced into the
// new one, never held. Every write of the history is fresh, so a write that
// succeeds is what a later step may depend on, whatever an earlier failed sync
// left; a removal writes its tag this way before it renames or deletes
// anything of the tree, even when an earlier pass recorded it already.
func WriteOuterCopy(dir string, c OuterCopy, remove string, files FileSystem) error {
	remote, conversation := filepath.Base(filepath.Dir(dir)), filepath.Base(dir)
	if err := c.Check(remote, conversation); err != nil {
		return err
	}
	if remove != "" {
		if _, err := ParseTag(remove); err != nil {
			return err
		}
		if remove > c.GreatestTag {
			return fmt.Errorf("storage: tag %s was never reserved here", remove)
		}
	}
	path := filepath.Join(dir, CopyMarker)
	_, err := os.Lstat(path)
	existing := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return ReplaceControlDurable(path, PermState, func(w io.Writer) error {
		out := &markerWriter{w: w}
		out.raw(`{"schema":`)
		out.value(CopySchema)
		out.raw(`,"remote":`)
		out.value(c.Remote)
		out.raw(`,"conversation":`)
		out.value(c.Conversation)
		out.raw(`,"greatest_tag":`)
		out.value(c.GreatestTag)
		out.raw(`,"removed":[`)
		first, inserted := true, remove == ""
		tag := func(t string) {
			if !first {
				out.raw(",")
			}
			first = false
			out.value(t)
		}
		if existing {
			err := EachRemoved(dir, remote, conversation, func(old string) error {
				if old > c.GreatestTag {
					return fmt.Errorf("storage: removed tag %s is past the history being written", old)
				}
				if !inserted && remove <= old {
					if remove < old {
						tag(remove)
					}
					inserted = true
				}
				tag(old)
				return nil
			})
			if err != nil {
				return err
			}
		}
		if !inserted {
			tag(remove)
		}
		out.raw("]")
		if c.Publishing != nil {
			out.raw(`,"publishing":`)
			out.value(c.Publishing)
		}
		out.raw("}\n")
		return out.err
	}, files)
}

// markerWriter writes a marker piece by piece and keeps the first error.
type markerWriter struct {
	w   io.Writer
	err error
}

func (m *markerWriter) raw(text string) {
	if m.err == nil {
		_, m.err = io.WriteString(m.w, text)
	}
}

func (m *markerWriter) value(v any) {
	if m.err != nil {
		return
	}
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if m.err = encoder.Encode(v); m.err == nil {
		_, m.err = m.w.Write(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
	}
}

// WriteSnapshotCopy checks c and writes the marker of the snapshot that will
// be published as dir's tag. dir is the private build being published.
func WriteSnapshotCopy(dir, tag string, c SnapshotCopy, files FileSystem) error {
	if err := c.Check(c.Remote, c.Conversation, tag); err != nil {
		return err
	}
	return writeMarker(filepath.Join(dir, CopyMarker), struct {
		Schema string `json:"schema"`
		SnapshotCopy
		PublishedAt string `json:"published_at"`
	}{CopySchema, c, c.PublishedAt.UTC().Format(publishedLayout)}, files)
}

func writeMarker(path string, value any, files FileSystem) error {
	return ReplaceControlDurable(path, PermState, func(w io.Writer) error {
		encoder := json.NewEncoder(w)
		encoder.SetEscapeHTML(false)
		return encoder.Encode(value)
	}, files)
}
