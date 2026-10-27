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

package format_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/pull/format"
)

func entryOf(name string, data []byte) format.Entry {
	sum := sha256.Sum256(data)
	return format.Entry{File: name, Kind: format.KindStore, Bytes: uint64(len(data)), Digest: format.StoredDigest(hex.EncodeToString(sum[:]))}
}

// files are three files, one with a name past tar's old 100-byte limit and
// one already gzip, as store entries are.
func files(t *testing.T) (map[string][]byte, map[string]format.Entry) {
	t.Helper()
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte("a body"))
	_ = zw.Close()
	data := map[string][]byte{
		"s1/streams/main/transcript-20261025T103200.000000000Z-000001.sd": []byte("first"),
		"s1/provider/pieces/" + strings.Repeat("a", 64) + ".gz":           gz.Bytes(),
		"s1/streams/" + strings.Repeat("o", 150) + "/meta-x-000002.sd":    bytes.Repeat([]byte("x"), 70000),
	}
	want := map[string]format.Entry{}
	for name, b := range data {
		want[name] = entryOf(name, b)
	}
	return data, want
}

func archiveOf(t *testing.T, data map[string][]byte, want map[string]format.Entry) ([]byte, string) {
	t.Helper()
	var list []format.ArchiveFile
	for name, b := range data {
		b := b
		list = append(list, format.ArchiveFile{Entry: want[name], Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }})
	}
	var out bytes.Buffer
	size, digest, err := format.WriteArchive(&out, list)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(out.Bytes())
	if size != int64(out.Len()) || digest != hex.EncodeToString(sum[:]) {
		t.Fatalf("size %d digest %s for %d bytes", size, digest, out.Len())
	}
	return out.Bytes(), digest
}

// An archive's identifier is the SHA-256 of its sorted names, each with a
// newline after it, whatever order they were asked in.
func TestAnArchiveIsNamedByItsSortedNames(t *testing.T) {
	sum := sha256.Sum256([]byte("a\nb/c\n"))
	if got := format.ArchiveID([]string{"b/c", "a"}); got != hex.EncodeToString(sum[:]) || got != format.ArchiveID([]string{"a", "b/c"}) {
		t.Fatalf("%s", got)
	}
}

// An archive is one gzip member holding a sorted PAX tar of regular files,
// read-only, owned by no one, dated the epoch, and extraction gives each
// file once with its bytes checked.
func TestAnArchiveRoundTrips(t *testing.T) {
	data, want := files(t)
	b, _ := archiveOf(t, data, want)
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	var names []string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag != tar.TypeReg || hdr.Mode != 0o444 || hdr.Uid != 0 || hdr.Uname != "" || !hdr.ModTime.Equal(time.Unix(0, 0)) {
			t.Fatalf("a header: %+v", hdr)
		}
		names = append(names, hdr.Name)
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Fatalf("not sorted: %v", names)
		}
	}
	got := map[string][]byte{}
	err = format.Extract(bytes.NewReader(b), want, func(e format.Entry, r io.Reader) error {
		got[e.File], err = io.ReadAll(r)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range data {
		if !bytes.Equal(got[name], b) {
			t.Fatalf("%s came out as %d bytes", name, len(got[name]))
		}
	}
	// A put that stops early still has the rest read and checked.
	if err := format.Extract(bytes.NewReader(b), want, func(format.Entry, io.Reader) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

// A frozen file that no longer matches its entry fails the archive.
func TestAnArchiveRefusesAFileThatChanged(t *testing.T) {
	data, want := files(t)
	for name := range data {
		data[name] = []byte("changed")
		break
	}
	var list []format.ArchiveFile
	for name, b := range data {
		b := b
		list = append(list, format.ArchiveFile{Entry: want[name], Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }})
	}
	if _, _, err := format.WriteArchive(io.Discard, list); !errors.Is(err, format.ErrArchive) {
		t.Fatal(err)
	}
}

// tarOf writes entries as the test says, without WriteArchive's care.
func tarOf(t *testing.T, write func(tw *tar.Writer)) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	write(tw)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func put(tw *tar.Writer, hdr *tar.Header, data []byte) {
	if hdr.Typeflag == 0 {
		hdr.Typeflag = tar.TypeReg
	}
	if hdr.Size == 0 && hdr.Typeflag == tar.TypeReg {
		hdr.Size = int64(len(data))
	}
	_ = tw.WriteHeader(hdr)
	_, _ = tw.Write(data)
}

// Extraction takes what an archive's metadata says only for the bytes it
// names: owners, times and modes may be anything, but a name not asked
// for, a name twice, a missing name, another size, other bytes, a link, a
// directory, a second gzip member and bytes after the member all fail.
func TestExtractionRefusesAnyOtherArchive(t *testing.T) {
	a, b := []byte("alpha"), []byte("beta")
	want := map[string]format.Entry{"x/a": entryOf("x/a", a), "x/b": entryOf("x/b", b)}
	harmless := tarOf(t, func(tw *tar.Writer) {
		put(tw, &tar.Header{Name: "x/b", Mode: 0o755, Uid: 1000, Uname: "someone", ModTime: time.Now()}, b)
		put(tw, &tar.Header{Name: "x/a", Mode: 0o600, Gid: 7, Gname: "staff"}, a)
	})
	if err := format.Extract(bytes.NewReader(harmless), want, func(format.Entry, io.Reader) error { return nil }); err != nil {
		t.Fatalf("harmless metadata: %v", err)
	}
	good, _ := archiveOf(t, map[string][]byte{"x/a": a, "x/b": b}, want)
	var twoMembers bytes.Buffer
	twoMembers.Write(good)
	empty := gzip.NewWriter(&twoMembers)
	_ = empty.Close()
	cases := map[string][]byte{
		"a name not asked for": tarOf(t, func(tw *tar.Writer) { put(tw, &tar.Header{Name: "x/a"}, a); put(tw, &tar.Header{Name: "x/c"}, b) }),
		"a name twice": tarOf(t, func(tw *tar.Writer) {
			put(tw, &tar.Header{Name: "x/a"}, a)
			put(tw, &tar.Header{Name: "x/a"}, a)
			put(tw, &tar.Header{Name: "x/b"}, b)
		}),
		"a missing name": tarOf(t, func(tw *tar.Writer) { put(tw, &tar.Header{Name: "x/a"}, a) }),
		"another size":   tarOf(t, func(tw *tar.Writer) { put(tw, &tar.Header{Name: "x/a"}, a[:3]); put(tw, &tar.Header{Name: "x/b"}, b) }),
		"other bytes": tarOf(t, func(tw *tar.Writer) {
			put(tw, &tar.Header{Name: "x/a"}, []byte("ALPHA"))
			put(tw, &tar.Header{Name: "x/b"}, b)
		}),
		"a link": tarOf(t, func(tw *tar.Writer) {
			put(tw, &tar.Header{Name: "x/a", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}, nil)
			put(tw, &tar.Header{Name: "x/b"}, b)
		}),
		"a directory": tarOf(t, func(tw *tar.Writer) {
			put(tw, &tar.Header{Name: "x/", Typeflag: tar.TypeDir}, nil)
			put(tw, &tar.Header{Name: "x/a"}, a)
			put(tw, &tar.Header{Name: "x/b"}, b)
		}),
		"a second gzip member":   twoMembers.Bytes(),
		"bytes after the member": append(append([]byte{}, good...), 0),
		"no gzip":                []byte("plain"),
		"a cut member":           good[:len(good)-4],
	}
	for name, archive := range cases {
		if err := format.Extract(bytes.NewReader(archive), want, func(format.Entry, io.Reader) error { return nil }); !errors.Is(err, format.ErrArchive) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// What an archive decompresses to is bounded by its files, and going past
// the bound is an error, never an end.
func TestExtractionIsBounded(t *testing.T) {
	a := []byte("alpha")
	want := map[string]format.Entry{"x/a": entryOf("x/a", a)}
	// Zeros after the tar's end, far past what one small file allows.
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	put(tw, &tar.Header{Name: "x/a"}, a)
	_ = tw.Close()
	_, _ = zw.Write(make([]byte, 1<<20))
	_ = zw.Close()
	if err := format.Extract(bytes.NewReader(b.Bytes()), want, func(format.Entry, io.Reader) error { return nil }); !errors.Is(err, format.ErrArchive) ||
		!strings.Contains(err.Error(), "more than its files allow") {
		t.Fatalf("an archive past its bound: %v", err)
	}
}
