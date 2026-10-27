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
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"time"
)

// ErrArchive is an archive that does not hold exactly the files asked for,
// as the manifest says they are.
var ErrArchive = errors.New("pull: invalid archive")

func archiveError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrArchive, fmt.Sprintf(format, args...))
}

// The bytes an archive allows beyond its files: each file's header, long
// name and padding, and the end of the tar.
const (
	perFileAllowance = 4096
	endAllowance     = 1024
)

// ArchiveID identifies a set of names within one snapshot: the SHA-256 of
// each name followed by a newline, in ascending byte order, the last
// included. A name holds no newline. The archive's bytes have a digest of
// their own.
func ArchiveID(names []string) string {
	sorted := slices.Clone(names)
	slices.Sort(sorted)
	h := sha256.New()
	for _, n := range sorted {
		_, _ = io.WriteString(h, n)
		_, _ = h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ArchiveFile is one file to put in an archive: its manifest entry and how
// to open its frozen bytes.
type ArchiveFile struct {
	Entry Entry
	Open  func() (io.ReadCloser, error)
}

// WriteArchive writes the files as one POSIX tar inside one level-6 gzip
// member, in ascending name order, and returns the archive's size and
// SHA-256. Each file is read in bounded pieces and checked against its
// entry's size and digest as it is written: a frozen file that changed fails
// the archive. Headers carry the name and size alone, read-only, owned by no
// one, dated the Unix epoch, so nothing of the live files leaks into them.
func WriteArchive(w io.Writer, files []ArchiveFile) (size int64, digest string, err error) {
	sorted := slices.Clone(files)
	slices.SortFunc(sorted, func(a, b ArchiveFile) int { return strings.Compare(a.Entry.File, b.Entry.File) })
	for i := 1; i < len(sorted); i++ {
		if sorted[i].Entry.File == sorted[i-1].Entry.File {
			return 0, "", archiveError("%s is asked for twice", sorted[i].Entry.File)
		}
	}
	h := sha256.New()
	counted := &countingWriter{w: io.MultiWriter(w, h)}
	zw, err := gzip.NewWriterLevel(counted, 6)
	if err != nil {
		return 0, "", err
	}
	tw := tar.NewWriter(zw)
	for _, f := range sorted {
		if err := writeEntry(tw, f); err != nil {
			return 0, "", err
		}
	}
	// The tar's end and the gzip trailer are part of the bytes and so of
	// their digest; either close can fail.
	if err := tw.Close(); err != nil {
		return 0, "", err
	}
	if err := zw.Close(); err != nil {
		return 0, "", err
	}
	return counted.n, hex.EncodeToString(h.Sum(nil)), nil
}

func writeEntry(tw *tar.Writer, f ArchiveFile) error {
	e := f.Entry
	if e.Bytes > math.MaxInt64 {
		return archiveError("%s is too large for an archive", e.File)
	}
	hdr := &tar.Header{Typeflag: tar.TypeReg, Name: e.File, Size: int64(e.Bytes), Mode: 0o444,
		ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	r, err := f.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	h := sha256.New()
	// Exactly the entry's size goes into the tar; a file shorter or longer
	// than that, or of other bytes, is no longer the file listed.
	_, err = io.CopyN(io.MultiWriter(tw, h), r, int64(e.Bytes))
	if errors.Is(err, io.EOF) {
		return archiveError("%s is shorter than the manifest lists", e.File)
	}
	if err != nil {
		return err
	}
	var one [1]byte
	if n, _ := io.ReadFull(r, one[:]); n > 0 {
		return archiveError("%s is longer than the manifest lists", e.File)
	}
	if hex.EncodeToString(h.Sum(nil)) != string(e.Digest) {
		return archiveError("%s is no longer the file the manifest lists", e.File)
	}
	return nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// errBound is a decompressed stream that went past what its files allow.
var errBound = errors.New("the archive decompresses to more than its files allow")

// boundedReader stops a stream at a bound with an error, never with a made
// up end: a stream past it is refused, not cut.
type boundedReader struct {
	r    io.Reader
	left uint64
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if b.left == 0 {
		var one [1]byte
		n, err := b.r.Read(one[:])
		if n > 0 {
			return 0, errBound
		}
		return 0, err
	}
	if uint64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.r.Read(p)
	b.left -= uint64(n)
	return n, err
}

// archiveBound is what an archive of these entries may decompress to, with
// checked arithmetic.
func archiveBound(want map[string]Entry) (uint64, error) {
	bound := uint64(endAllowance)
	for _, e := range want {
		add := e.Bytes
		if add > math.MaxUint64-perFileAllowance {
			return 0, archiveError("the files are too large to bound")
		}
		add += perFileAllowance
		if bound > math.MaxUint64-add {
			return 0, archiveError("the files are too large to bound")
		}
		bound += add
	}
	return bound, nil
}

// Extract checks an archive against the entries it was asked for and gives
// each file's bytes to put once. The archive must be one gzip member, read
// to its checksum and trailer with nothing after it, decompressing within
// what its files allow; it must hold every entry asked for exactly once, as
// a regular file of the entry's size and SHA-256, and nothing else. Its
// metadata names no destination: put is told the entry, never the header.
//
// put may stop reading early; the rest of the file is still read and
// checked. Nothing put received counts until Extract returns nil.
func Extract(r io.Reader, want map[string]Entry, put func(e Entry, r io.Reader) error) error {
	bound, err := archiveBound(want)
	if err != nil {
		return err
	}
	br := bufio.NewReader(r)
	zr, err := gzip.NewReader(br)
	if err != nil {
		return archiveError("gzip: %v", err)
	}
	zr.Multistream(false)
	limited := &boundedReader{r: zr, left: bound}
	tr := tar.NewReader(limited)
	seen := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return archiveError("tar: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			return archiveError("%q is not a regular file", hdr.Name)
		}
		e, ok := want[hdr.Name]
		if !ok {
			return archiveError("%q was not asked for", hdr.Name)
		}
		if seen[hdr.Name] {
			return archiveError("%q comes twice", hdr.Name)
		}
		seen[hdr.Name] = true
		if hdr.Size < 0 || uint64(hdr.Size) != e.Bytes {
			return archiveError("%q holds %d bytes, and the manifest says %d", hdr.Name, hdr.Size, e.Bytes)
		}
		h := sha256.New()
		body := io.TeeReader(io.LimitReader(tr, hdr.Size), h)
		if err := put(e, body); err != nil {
			return err
		}
		// What put left of the file is read into the hash too.
		if _, err := io.Copy(io.Discard, body); err != nil {
			return archiveError("%q: %v", hdr.Name, err)
		}
		if hex.EncodeToString(h.Sum(nil)) != string(e.Digest) {
			return archiveError("%q is not the file the manifest lists", hdr.Name)
		}
	}
	for name := range want {
		if !seen[name] {
			return archiveError("%q is missing", name)
		}
	}
	// What follows the tar's end is read through the same bound, never
	// extracted, and the gzip member is read to its checked end.
	if _, err := io.Copy(io.Discard, limited); err != nil {
		return archiveError("after the tar: %v", err)
	}
	if _, err := br.ReadByte(); !errors.Is(err, io.EOF) {
		return archiveError("bytes follow the gzip member")
	}
	return nil
}
