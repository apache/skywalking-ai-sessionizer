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

package client

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/pull/format"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// These tests answer the client with a transport of their own, for what a
// real server never sends: bodies that fail at their end, stall, or carry
// an archive whose member is not the manifest's.

type transport func(*http.Request) (*http.Response, error)

func (t transport) RoundTrip(r *http.Request) (*http.Response, error) { return t(r) }

var testTag = storage.Stamp(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))

// sourceOver is a source of one snapshot, its requests answered by rt.
func sourceOver(rt transport) *RemoteSource {
	base, _ := url.Parse("http://remote.test")
	r := &Remote{Name: "test", base: base, token: "t", http: &http.Client{Transport: rt}, slots: make(chan struct{}, perOrigin)}
	return &RemoteSource{r: r, conversation: "c", resp: format.Response{Snapshot: testTag, Lease: "l"}, tries: &Tries{}}
}

// failingReader gives its bytes, then err.
func failingReader(b []byte, err error) io.ReadCloser {
	return io.NopCloser(io.MultiReader(bytes.NewReader(b), &errReader{err}))
}

// lastWithError gives its bytes and err in the same read, as a reader may.
type lastWithError struct {
	b   []byte
	err error
}

func (l *lastWithError) Read(p []byte) (int, error) {
	n := copy(p, l.b)
	l.b = l.b[n:]
	if len(l.b) == 0 {
		return n, l.err
	}
	return n, nil
}

func (l *lastWithError) Close() error { return nil }

type errReader struct{ err error }

func (e *errReader) Read([]byte) (int, error) { return 0, e.err }

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ranged answers a range request for data with the headers a ready
// archive's range has, and body.
func ranged(r *http.Request, data []byte, body func(part []byte) io.ReadCloser) *http.Response {
	var start, end int
	if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
		return &http.Response{StatusCode: http.StatusBadRequest, Body: http.NoBody, Header: http.Header{}, Request: r}
	}
	h := http.Header{}
	h.Set("ETag", `"`+digestOf(data)+`"`)
	h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
	h.Set("Content-Length", strconv.Itoa(end-start+1))
	return &http.Response{StatusCode: http.StatusPartialContent, Header: h, ContentLength: int64(end - start + 1),
		Body: body(data[start : end+1]), Request: r}
}

// An error the transport reports at a range's end, after all its bytes, is
// that range's failure: a length that does not match the frames, or a
// stream reset after the last byte. Its bytes are dropped.
func TestAnErrorAtARangesEndFailsIt(t *testing.T) {
	data := []byte("the archive's bytes")
	s := sourceOver(func(r *http.Request) (*http.Response, error) {
		return ranged(r, data, func(part []byte) io.ReadCloser {
			return failingReader(part, errors.New("http2: response body closed with a length mismatch"))
		}), nil
	})
	f, err := os.Create(filepath.Join(t.TempDir(), archivePart))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st := format.ArchiveState{Bytes: uint64(len(data)), Digest: digestOf(data)}
	got, result, _, err := s.rangeOf(context.Background(), "http://remote.test/data", "a", 0, uint64(len(data)-1), st, f, newBudget())
	if err == nil || result != rangeFailed || got != 0 {
		t.Fatalf("kept %d, result %d: %v", got, result, err)
	}
	if info, err := f.Stat(); err != nil || info.Size() != 0 {
		t.Fatalf("the part holds %v bytes: %v", info.Size(), err)
	}
}

// A range whose end reports an error is asked again whole, whether more
// ranges follow it or it is the last, and whether the error comes after its
// last bytes or with them, and the archive is still whole.
func TestARangeFailedAtItsEndIsAskedAgainWhole(t *testing.T) {
	failing := map[string]func(part []byte, err error) io.ReadCloser{
		"after":     failingReader,
		"with them": func(part []byte, err error) io.ReadCloser { return &lastWithError{b: part, err: err} },
	}
	for name, size := range map[string]int{"more follow": 9<<20 + 5, "the last": 100} {
		for how, fail := range failing {
			t.Run(name+", the error "+how, func(t *testing.T) {
				data := make([]byte, size)
				for i := range data {
					data[i] = byte(i * 11)
				}
				var mu sync.Mutex
				var asked []string
				s := sourceOver(func(r *http.Request) (*http.Response, error) {
					mu.Lock()
					asked = append(asked, r.Header.Get("Range"))
					first := len(asked) == 1
					mu.Unlock()
					return ranged(r, data, func(part []byte) io.ReadCloser {
						if first {
							return fail(part, errors.New("http2: response body closed with a length mismatch"))
						}
						return io.NopCloser(bytes.NewReader(part))
					}), nil
				})
				st := format.ArchiveState{Bytes: uint64(len(data)), Digest: digestOf(data)}
				if err := s.download(context.Background(), filepath.Join(t.TempDir(), archivePart), []string{"a"}, "a", st, newBudget()); err != nil {
					t.Fatal(err)
				}
				if len(asked) < 2 || asked[0] != asked[1] {
					t.Fatalf("asked %v", asked)
				}
			})
		}
	}
}

// A member that cannot be read whole, a tar cut inside it under a gzip
// member that is whole and the archive's digest right, stops the remote.
func TestAnArchiveMemberCutShortStopsTheRemote(t *testing.T) {
	name := "streams/main/a-000001.sd"
	body := []byte("hello")
	var whole bytes.Buffer
	tw := tar.NewWriter(&whole)
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Size: int64(len(body)), Mode: 0o444, Format: tar.FormatPAX}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	// The tar ends two bytes into its member.
	cut := whole.Bytes()[:bytes.Index(whole.Bytes(), body)+2]
	var archive bytes.Buffer
	zw := gzip.NewWriter(&archive)
	if _, err := zw.Write(cut); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	s := archiveSource(name, archive.Bytes())
	asked := map[string]format.Entry{name: {File: name, Bytes: uint64(len(body)), Digest: format.StoredDigest(digestOf(body))}}
	work := t.TempDir()
	sum := sha256.Sum256(body)
	err := s.acquire(context.Background(), filepath.Join(work, archivePart), []string{name}, asked, newBudget(),
		func(_ string, r io.Reader) error {
			return storage.WriteChecked(filepath.Join(work, "member"), r, int64(len(body)), sum, nil)
		})
	if !errors.Is(err, ErrRemote) || errors.Is(err, ErrConversation) {
		t.Fatalf("a member cut short: %v", err)
	}
}

// archiveSource serves one ready archive of name: its state, and its bytes
// in ranges.
func archiveSource(name string, archive []byte) *RemoteSource {
	id := format.ArchiveID([]string{name})
	digest := digestOf(archive)
	return sourceOver(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/data") {
			return ranged(r, archive, func(part []byte) io.ReadCloser { return io.NopCloser(bytes.NewReader(part)) }), nil
		}
		body, _ := json.Marshal(format.ArchiveState{Schema: format.Schema, Snapshot: testTag, Archive: id,
			State: format.StateReady, Bytes: uint64(len(archive)), Digest: digest})
		h := http.Header{}
		h.Set("Content-Type", "application/json")
		h.Set("ETag", `"`+digest+`"`)
		return &http.Response{StatusCode: http.StatusOK, Header: h, Body: io.NopCloser(bytes.NewReader(body)), Request: r}, nil
	})
}

// A write of this machine's that fails stops the conversation, never tried
// again over the network.
func TestALocalWriteFailureStopsTheConversation(t *testing.T) {
	data := []byte("the archive's bytes")
	s := sourceOver(func(r *http.Request) (*http.Response, error) {
		return ranged(r, data, func(part []byte) io.ReadCloser { return io.NopCloser(bytes.NewReader(part)) }), nil
	})
	path := filepath.Join(t.TempDir(), archivePart)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// A file open only for reading refuses every write.
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st := format.ArchiveState{Bytes: uint64(len(data)), Digest: digestOf(data)}
	_, _, _, err = s.rangeOf(context.Background(), "http://remote.test/data", "a", 0, uint64(len(data)-1), st, f, newBudget())
	if !errors.Is(err, ErrConversation) {
		t.Fatalf("a failed write: %v", err)
	}
}

// A range cut short is asked again from its next byte to the end it had,
// not to a new end eight mebibytes on: an archive of more than one full
// range, cut inside the first.
func TestACutRangeKeepsItsEnd(t *testing.T) {
	data := make([]byte, 12<<20+5)
	for i := range data {
		data[i] = byte(i * 7)
	}
	var mu sync.Mutex
	var asked []string
	s := sourceOver(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		asked = append(asked, r.Header.Get("Range"))
		first := len(asked) == 1
		mu.Unlock()
		return ranged(r, data, func(part []byte) io.ReadCloser {
			if first {
				return failingReader(part[:4<<20], io.ErrUnexpectedEOF)
			}
			return io.NopCloser(bytes.NewReader(part))
		}), nil
	})
	st := format.ArchiveState{Bytes: uint64(len(data)), Digest: digestOf(data)}
	part := filepath.Join(t.TempDir(), archivePart)
	if err := s.download(context.Background(), part, []string{"a"}, "a", st, newBudget()); err != nil {
		t.Fatal(err)
	}
	want := []string{"bytes=0-8388607", "bytes=4194304-8388607", fmt.Sprintf("bytes=8388608-%d", len(data)-1)}
	if strings.Join(asked, " ") != strings.Join(want, " ") {
		t.Fatalf("asked %v, want %v", asked, want)
	}
}

// A member of an archive whose digest is right, but whose bytes are not
// the ones the manifest lists, is the remote's fault: it stops the remote,
// never only this conversation.
func TestAnArchiveMemberThatIsNotItsEntryStopsTheRemote(t *testing.T) {
	sent, listed := []byte("hello"), []byte("world")
	name := "streams/main/a-000001.sd"
	var archive bytes.Buffer
	_, digest, err := format.WriteArchive(&archive, []format.ArchiveFile{{
		Entry: format.Entry{File: name, Bytes: uint64(len(sent)), Digest: format.StoredDigest(digestOf(sent))},
		Open:  func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(sent)), nil },
	}})
	if err != nil {
		t.Fatal(err)
	}
	if digest != digestOf(archive.Bytes()) {
		t.Fatal("the archive's digest")
	}
	s := archiveSource(name, archive.Bytes())
	asked := map[string]format.Entry{name: {File: name, Bytes: uint64(len(listed)), Digest: format.StoredDigest(digestOf(listed))}}
	work := t.TempDir()
	sum := sha256.Sum256(listed)
	err = s.acquire(context.Background(), filepath.Join(work, archivePart), []string{name}, asked, newBudget(),
		func(_ string, r io.Reader) error {
			return storage.WriteChecked(filepath.Join(work, "member"), r, int64(len(listed)), sum, nil)
		})
	if !errors.Is(err, ErrRemote) {
		t.Fatalf("a member that is not its entry: %v", err)
	}
}

// A refusal of a range whose body stalls is read within the metadata time,
// as any answer that is not the range's bytes is.
func TestAStalledRefusalOfARangeEnds(t *testing.T) {
	s := sourceOver(func(r *http.Request) (*http.Response, error) {
		h := http.Header{}
		h.Set("Content-Type", "application/problem+json")
		return &http.Response{StatusCode: http.StatusForbidden, Header: h, Request: r,
			Body: io.NopCloser(&stalled{ctx: r.Context(), b: []byte(`{"type":"about:blank",`)})}, nil
	})
	f, err := os.Create(filepath.Join(t.TempDir(), archivePart))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ended := make(chan error, 1)
	start := time.Now()
	go func() {
		_, _, _, err := s.rangeOf(context.Background(), "http://remote.test/data", "a", 0, 9,
			format.ArchiveState{Bytes: 10, Digest: strings.Repeat("0", 64)}, f, newBudget())
		ended <- err
	}()
	select {
	case err := <-ended:
		if err == nil || time.Since(start) < metadataWait {
			t.Fatalf("after %s: %v", time.Since(start), err)
		}
	case <-time.After(metadataWait + 30*time.Second):
		t.Fatal("a stalled refusal was waited for past the metadata time")
	}
}

// stalled gives its bytes, then nothing until its request ends.
type stalled struct {
	ctx context.Context
	b   []byte
}

func (s *stalled) Read(p []byte) (int, error) {
	if len(s.b) > 0 {
		n := copy(p, s.b)
		s.b = s.b[n:]
		return n, nil
	}
	<-s.ctx.Done()
	return 0, s.ctx.Err()
}
