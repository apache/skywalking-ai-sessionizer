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

package sessiondata_test

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// readToEnd reads every record, as Next allows, then calls Complete.
func readToEnd(t *testing.T, r io.Reader) (records int, nextErr, completeErr error) {
	t.Helper()
	rd, err := sessiondata.NewReader(r)
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return records, err, nil
		}
		records++
	}
	return records, nil, rd.Complete()
}

func twoRecords(t *testing.T) []byte {
	t.Helper()
	return writeRecords(t,
		&sessiondata.Record{Ord: 1, Parts: []sessiondata.Part{{Kind: sessiondata.PartText, Text: "one"}}},
		&sessiondata.Record{Ord: 2, Parts: []sessiondata.Part{{Kind: sessiondata.PartText, Text: "two"}}})
}

func TestCompleteAcceptsAFileThatEndsAtItsClosingLine(t *testing.T) {
	n, nextErr, err := readToEnd(t, bytes.NewReader(twoRecords(t)))
	if n != 2 || nextErr != nil || err != nil {
		t.Fatalf("records %d, next %v, complete %v", n, nextErr, err)
	}
}

// A record after the closing line is held in the reader's buffer, where
// Next never looks. Draining the file after Next would find nothing: the
// bytes were already read into the buffer.
func TestCompleteFindsARecordBufferedAfterTheClosingLine(t *testing.T) {
	file := twoRecords(t)
	lines := bytes.SplitAfter(file, []byte{'\n'})
	trailing := append(append([]byte{}, file...), lines[1]...)
	f := bytes.NewReader(trailing)
	n, nextErr, err := readToEnd(t, f)
	if n != 2 || nextErr != nil {
		t.Fatalf("Next: records %d, %v", n, nextErr)
	}
	if err == nil {
		t.Fatal("a record after the closing line was accepted")
	}
	if rest, _ := io.ReadAll(f); len(rest) != 0 {
		t.Fatalf("the fixture must hold the record in the reader's buffer, not the file: %d bytes left", len(rest))
	}
	for _, extra := range []string{"\n", " ", "x"} {
		_, _, err := readToEnd(t, bytes.NewReader(append(append([]byte{}, file...), extra...)))
		if err == nil {
			t.Errorf("%q after the closing line was accepted", extra)
		}
	}
}

// failWithLast returns its last bytes together with an error, in one read,
// as a reader that fails at the end of what it could read does.
type failWithLast struct {
	data []byte
	err  error
}

func (f *failWithLast) Read(p []byte) (int, error) {
	n := copy(p, f.data)
	f.data = f.data[n:]
	if len(f.data) == 0 {
		return n, f.err
	}
	return n, nil
}

// An error the underlying reader returns with the bytes that end at the
// closing line is saved in the reader's buffer behind them, and only
// Complete reports it.
func TestCompleteReportsAnErrorSavedBehindTheClosingLine(t *testing.T) {
	broken := errors.New("disk read failed")
	n, nextErr, err := readToEnd(t, &failWithLast{data: twoRecords(t), err: broken})
	if n != 2 || nextErr != nil {
		t.Fatalf("Next: records %d, %v", n, nextErr)
	}
	if !errors.Is(err, broken) {
		t.Fatalf("Complete: %v, want the saved read error", err)
	}
}

func gzipped(t *testing.T, b []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func openSkeleton(t *testing.T, b []byte) io.ReadCloser {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript-x-000001"+sessiondata.SkeletonExt)
	if err := os.WriteFile(path, b, 0o444); err != nil {
		t.Fatal(err)
	}
	f, err := sessiondata.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// A skeleton's compression trailer is checked only when the gzip stream is
// read to its end, which Next never does.
func TestCompleteChecksASkeletonsCompressionTrailer(t *testing.T) {
	good := gzipped(t, twoRecords(t))
	if _, nextErr, err := readToEnd(t, openSkeleton(t, good)); nextErr != nil || err != nil {
		t.Fatalf("valid skeleton: next %v, complete %v", nextErr, err)
	}
	for name, damage := range map[string]func([]byte) []byte{
		"checksum": func(b []byte) []byte { b[len(b)-8] ^= 0xff; return b },
		"length":   func(b []byte) []byte { b[len(b)-1] ^= 0xff; return b },
		"cut":      func(b []byte) []byte { return b[:len(b)-3] },
	} {
		bad := damage(append([]byte{}, good...))
		n, nextErr, err := readToEnd(t, openSkeleton(t, bad))
		if nextErr != nil {
			continue
		}
		if n != 2 || err == nil {
			t.Errorf("%s: records %d and Complete %v; the damaged trailer was accepted", name, n, err)
		}
	}
	// A second gzip member that holds a byte is more of the file. An empty
	// one decompresses to nothing, so the strict opener in the pull format
	// refuses it, reading a single member.
	joined := append(append([]byte{}, good...), gzipped(t, []byte("x\n"))...)
	if _, nextErr, err := readToEnd(t, openSkeleton(t, joined)); nextErr == nil && err == nil {
		t.Error("a second gzip member was accepted")
	}
}

func TestCompleteNeedsTheClosingLineFirst(t *testing.T) {
	rd, err := sessiondata.NewReader(bytes.NewReader(twoRecords(t)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rd.Next(); err != nil {
		t.Fatal(err)
	}
	if err := rd.Complete(); err == nil {
		t.Fatal("Complete accepted a file whose closing line was never read")
	}
	// A file cut after its header holds nothing more to read, and still has
	// no closing line.
	header := bytes.SplitAfter(twoRecords(t), []byte{'\n'})[0]
	cut, err := sessiondata.NewReader(bytes.NewReader(header))
	if err != nil {
		t.Fatal(err)
	}
	if err := cut.Complete(); err == nil {
		t.Fatal("Complete accepted a file with nothing after its header")
	}
}

// A closing line that fails its checks is read, and the file still did not
// end at a valid closing line.
func TestCompleteNeedsAClosingLineThatPassed(t *testing.T) {
	file := twoRecords(t)
	lines := bytes.SplitAfter(file, []byte{'\n'})
	for name, last := range map[string][]byte{
		"count":  bytes.Replace(lines[len(lines)-2], []byte(`"records":2`), []byte(`"records":3`), 1),
		"digest": bytes.Replace(lines[len(lines)-2], []byte(`"digest":"`), []byte(`"digest":"0`), 1),
	} {
		if bytes.Equal(last, lines[len(lines)-2]) {
			t.Fatalf("%s: the fixture did not change the closing line %q", name, lines[len(lines)-2])
		}
		damaged := append(bytes.Join(lines[:len(lines)-2], nil), last...)
		rd, err := sessiondata.NewReader(bytes.NewReader(damaged))
		if err != nil {
			t.Fatal(err)
		}
		for {
			_, err := rd.Next()
			if err != nil {
				break
			}
		}
		if err := rd.Complete(); err == nil {
			t.Errorf("%s: Complete accepted a closing line that failed its checks", name)
		}
	}
}
