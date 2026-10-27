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
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/boundedjson"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/format"
)

// streamed writes a manifest of n entries, one round and n-1 landed files,
// as it is read, so the test holds none of it. Past format.MaxFiles an entry
// is malformed inside: refusing it for its place in the list, not for what
// it holds, shows the list stopped before decoding it.
func streamed(n int) (*io.PipeReader, *int) {
	pr, pw := io.Pipe()
	written := 0
	go func() {
		bw := bufio.NewWriter(pw)
		fmt.Fprintf(bw, `{"schema":"pull/1","conversation":"c1","session":"s1","round":{"round":1,"digest":"%s","through_seq":%d},"generation":0,"formats":{"sd":"sd/1","sf":"sf/1"},"files":[`, digest, n-1)
		fmt.Fprintf(bw, `{"file":"_conversations/c1/rounds/r000001-%s.sf","kind":"round","bytes":1,"digest":"%s","round":1}`, digest[:12], other)
		written = 1
		for i := 1; i < n; i++ {
			if written == format.MaxFiles {
				fmt.Fprint(bw, `,{"file":7,"kind":null}`)
				written++
				continue
			}
			fmt.Fprintf(bw, `,{"file":"s1/streams/main/transcript-20261025T103200.000000000Z-%07d.sd","kind":"landed","bytes":1,"digest":"%s","seq":%d}`, i, other, i)
			written++
		}
		fmt.Fprint(bw, "]}")
		_ = bw.Flush()
		_ = pw.Close()
	}()
	return pr, &written
}

// A manifest lists at most a million files. One with a million reads; one
// more is refused at that entry.
func TestAManifestHoldsAtMostAMillionFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("reads a million entries")
	}
	r, _ := streamed(format.MaxFiles)
	m, err := format.ReadManifest(r, conv)
	if err != nil || len(m.Files) != format.MaxFiles {
		t.Fatalf("a million files: %v", err)
	}
	r, _ = streamed(format.MaxFiles + 1)
	_, err = format.ReadManifest(r, conv)
	// The reader stopped early; closing it ends the writer.
	_ = r.Close()
	if !errors.Is(err, format.ErrManifest) || !strings.Contains(err.Error(), "more than 1000000 entries") {
		t.Fatalf("a million and one: %v", err)
	}
}

// The scanner's limits hold inside a manifest, in members the schema does
// not know as in those it does: at each limit it reads, one past it is
// refused. A duplicate written with an escape is a duplicate, the last
// member of the object included.
func TestTheScannersLimitsHoldInAManifest(t *testing.T) {
	text := encoded(t, sample())
	unknown := func(value string) string {
		return strings.Replace(text, `{"schema"`, `{"later":`+value+`,"schema"`, 1)
	}
	nested := func(depth int) string { return strings.Repeat("[", depth) + strings.Repeat("]", depth) }
	members := func(n int) string {
		var b strings.Builder
		b.WriteString("{")
		for i := range n {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `"m%d":0`, i)
		}
		b.WriteString("}")
		return b.String()
	}
	long := func(n int) string { return `"` + strings.Repeat("a", n) + `"` }
	for name, tc := range map[string]struct {
		body string
		ok   bool
	}{
		// The manifest itself is one level, so its member may nest one less.
		"nesting at the limit":          {unknown(nested(boundedjson.MaxDepth - 1)), true},
		"nesting past the limit":        {unknown(nested(boundedjson.MaxDepth)), false},
		"members at the limit":          {unknown(members(boundedjson.MaxMembers)), true},
		"members past the limit":        {unknown(members(boundedjson.MaxMembers + 1)), false},
		"a string at the limit":         {unknown(long(boundedjson.MaxStringBytes)), true},
		"a string past the limit":       {unknown(long(boundedjson.MaxStringBytes + 1)), false},
		"a known string past the limit": {strings.Replace(text, `"session":"s1"`, `"session":`+long(boundedjson.MaxStringBytes+1), 1), false},
		"an escaped duplicate last":     {strings.TrimSuffix(strings.TrimSpace(text), "}") + `,"schem\u0061":"pull/1"}`, false},
	} {
		_, err := format.ReadManifest(strings.NewReader(tc.body), conv)
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v, want ok %v", name, err, tc.ok)
		}
	}
}

// The canonical file list is exactly these bytes for each kind, and its
// digest is their SHA-256.
func TestTheFileListBytesAreExact(t *testing.T) {
	want := strings.Join([]string{
		"_conversations/c1/rounds/r000001-fedcba987654.sf\tround\t-\t1\t10\t" + other + "\t-\t-",
		"_conversations/c1/rounds/r000002-0123456789ab.sf\tround\t-\t2\t10\t" + other + "\t-\t-",
		"s1/provider/pieces/" + other + ".gz\tstore\t-\t-\t7\t" + digest + "\t-\t" + other,
		"s1/provider/tips/1/" + digest + ".gz\tstore\t-\t-\t7\t" + digest + "\t-\t" + other,
		"s1/provider_body/provider_body-20261025T103200.000000000Z-000003.sd\tlanded\t3\t-\t9\t" + other + "\t-\t-",
		"s1/streams/main/transcript-20261025T103200.000000000Z-000001.sk\tskeleton\t1\t-\t9\t" + other + "\t" + digest + "\t-",
		"s1/streams/main/transcript-20261025T103201.000000000Z-000002.sd\tlanded\t2\t-\t9\t" + other + "\t-\t-",
	}, "\n") + "\n"
	var b strings.Builder
	if err := sample().WriteFileList(&b); err != nil {
		t.Fatal(err)
	}
	if b.String() != want {
		t.Fatalf("file list:\n%q\nwant:\n%q", b.String(), want)
	}
	sum := sha256.Sum256([]byte(want))
	if got := sample().Digest(); string(got) != hex.EncodeToString(sum[:]) {
		t.Fatalf("digest %s, want the SHA-256 of the list", got)
	}
	for name, edit := range map[string]func(*format.Manifest){
		"kind":  func(m *format.Manifest) { m.Files[4].Kind = format.KindSkeleton; m.Files[4].LandedDigest = other },
		"round": func(m *format.Manifest) { m.Files[0].Round = 3 },
	} {
		changed := sample()
		edit(changed)
		if changed.Digest() == sample().Digest() {
			t.Errorf("a changed %s did not change the digest", name)
		}
	}
}
