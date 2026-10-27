// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements.  See the NOTICE file distributed with
// this work for additional information regarding copyright ownership.
// The ASF licenses this file to You under the Apache License, Version 2.0
// (the "License"); you may not use this file except in compliance with
// the License.  You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package judge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
)

// TestFetchCommandStandIn is a fetch command when ASZ_FETCH_STAND_IN is
// set: it writes the source it was given, or fails when the source says so.
// It is the test binary itself, so it runs wherever the tests do.
func TestFetchCommandStandIn(_ *testing.T) {
	if os.Getenv("ASZ_FETCH_STAND_IN") == "" {
		return
	}
	src := os.Args[len(os.Args)-1]
	if strings.HasSuffix(src, "/denied") {
		fmt.Fprint(os.Stderr, "401 Unauthorized")
		os.Exit(3)
	}
	fmt.Printf("# Ticket %s\nroot cause: max_attempts is never checked\n", src)
	os.Exit(0)
}

func fetched(t *testing.T, cfg config.References, sources ...string) []savedSource {
	t.Helper()
	at := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	return fetchSources(context.Background(), sources, cfg, func() time.Time { return at }, "")
}

// A source is a file, a page fetched over http, or what a configured
// command writes; each is saved with how it was fetched and its digest.
func TestASourceIsSavedWithItsManifest(t *testing.T) {
	dir := t.TempDir()
	notes := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(notes, []byte("the queue was full\nretries never stopped"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/page":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("ETag", `"v7"`)
			w.Header().Set("Last-Modified", "Wed, 21 Oct 2026 07:28:00 GMT")
			fmt.Fprint(w, "<html><head><style>p{}</style><script>x()</script></head><body><h1>Incident</h1><p>Retries &amp; limits</p><!-- note --><p>fixed</p></body></html>")
		case "/pdf":
			w.Header().Set("Content-Type", "application/pdf")
			w.Write([]byte{'%', 'P', 'D', 'F', 0, 1, 2})
		default:
			http.Error(w, "no", http.StatusForbidden)
		}
	}))
	defer srv.Close()
	t.Setenv("ASZ_FETCH_STAND_IN", "1")
	stand := []string{os.Args[0], "-test.run=TestFetchCommandStandIn", "--"}
	cfg := config.References{MaxBytes: 1 << 20, Timeout: "10s", Fetch: []config.Fetcher{{Prefix: "https://jira.example/", Command: stand, ContentType: "text/markdown"},
		{Prefix: "https://wiki.example/", Command: stand}}}
	got := fetched(t, cfg, notes, srv.URL+"/page", "https://jira.example/browse/OPS-1", srv.URL+"/missing", srv.URL+"/pdf",
		"https://jira.example/denied", "ftp://example/x", filepath.Join(dir, "absent.txt"), "https://wiki.example/page")
	if m := got[8].manifest; m.Method != sessionjudge.MethodCommand || m.ContentType != "text/plain" || m.Outcome != sessionjudge.SourceSaved {
		t.Fatalf("a command with no content type: %+v", m)
	}
	file, page, ticket := got[0], got[1], got[2]
	if m := file.manifest; m.ID != "R1" || m.Method != sessionjudge.MethodFile || m.Outcome != sessionjudge.SourceSaved || m.Lines != 2 || m.Bytes != 40 ||
		len(m.SHA256) != 64 || m.Captured != "2026-10-23T09:00:00Z" || m.Text != nil || file.text != "the queue was full\nretries never stopped" {
		t.Fatalf("file %+v %q", m, file.text)
	}
	if m := page.manifest; m.Method != sessionjudge.MethodHTTP || m.Revision != `"v7"` || m.Published != "Wed, 21 Oct 2026 07:28:00 GMT" || m.Text == nil ||
		m.Text.Bytes != len(page.text) || page.text != "Incident\nRetries & limits\nfixed\n" || m.Lines != 3 {
		t.Fatalf("page %+v %q", m, page.text)
	}
	if m := ticket.manifest; m.Method != sessionjudge.MethodCommand || m.ContentType != "text/markdown" || !strings.Contains(ticket.text, "max_attempts is never checked") ||
		!strings.Contains(ticket.text, "# Ticket https://jira.example/browse/OPS-1") {
		t.Fatalf("ticket %+v %q", m, ticket.text)
	}
	for i, want := range map[int]string{3: "403 Forbidden", 4: "holds no text asz can read", 5: "401 Unauthorized", 6: "no fetch command is configured", 7: "absent.txt"} {
		if m := got[i].manifest; m.Outcome != sessionjudge.SourceFailed || !strings.Contains(m.Error, want) || got[i].text != "" {
			t.Errorf("source %d: %+v, want an error with %q", i+1, m, want)
		}
	}
}

// A source larger than a saved copy may be is saved cut, never inside a
// character.
func TestALargeSourceIsCut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.txt")
	if err := os.WriteFile(path, []byte("ab═cd"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := fetched(t, config.References{MaxBytes: 3}, path)[0]
	if !s.manifest.Cut || s.text != "ab" || s.manifest.Bytes != 2 || s.manifest.Outcome != sessionjudge.SourceSaved {
		t.Fatalf("cut %+v %q", s.manifest, s.text)
	}
	if s := fetched(t, config.References{MaxBytes: 100}, path)[0]; s.manifest.Cut || s.text != "ab═cd" {
		t.Fatalf("whole %+v %q", s.manifest, s.text)
	}
	// What a command writes is cut at the same bound.
	t.Setenv("ASZ_FETCH_STAND_IN", "1")
	stand := []string{os.Args[0], "-test.run=TestFetchCommandStandIn", "--"}
	cfg := config.References{MaxBytes: 8, Timeout: "10s", Fetch: []config.Fetcher{{Prefix: "https://jira.example/", Command: stand}}}
	if s := fetched(t, cfg, "https://jira.example/OPS-1")[0]; !s.manifest.Cut || s.text != "# Ticket" || s.manifest.Bytes != 8 {
		t.Fatalf("command %+v %q", s.manifest, s.text)
	}
	// A directory, like a named pipe or a device, is no file a source is
	// read from.
	if s := fetched(t, config.References{}, t.TempDir())[0]; s.manifest.Outcome != sessionjudge.SourceFailed || !strings.Contains(s.manifest.Error, "is not a regular file") {
		t.Fatalf("directory %+v", s.manifest)
	}
}

// What a command writes past the bound is dropped as it comes, so it never
// fills memory, and the command is never told to stop.
func TestABoundedBufferKeepsOnlyItsStart(t *testing.T) {
	b := &boundedBuffer{max: 4}
	for _, w := range []string{"ab", "cdef", "gh"} {
		if n, err := b.Write([]byte(w)); n != len(w) || err != nil {
			t.Fatalf("write %q: %d %v", w, n, err)
		}
	}
	if b.buf.String() != "abcd" || b.buf.Cap() > 64 {
		t.Fatalf("kept %q in %d bytes", b.buf.String(), b.buf.Cap())
	}
}

// Each source is kept in the run's work directory as it was fetched: its
// manifest, the bytes fetched and the text read; a failed one its manifest.
func TestASourceIsKeptAsItWasFetched(t *testing.T) {
	page := []byte("<p>Retries &amp; limits</p>")
	saved := []savedSource{{manifest: sessionjudge.Source{ID: "R1", Outcome: sessionjudge.SourceSaved}, data: page, text: "Retries & limits\n"},
		{manifest: sessionjudge.Source{ID: "R2", Outcome: sessionjudge.SourceFailed, Error: "the server answered 401"}}}
	files, err := sourceFiles(saved)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	var names []string
	for _, f := range files {
		got[f.Name] = string(f.Data)
		names = append(names, f.Name)
	}
	var m sessionjudge.Source
	if fmt.Sprint(names) != "[R1.json R1.fetched R1.txt R2.json]" || got["R1.fetched"] != string(page) || got["R1.txt"] != "Retries & limits\n" ||
		json.Unmarshal([]byte(got["R2.json"]), &m) != nil || m.Error != "the server answered 401" {
		t.Fatalf("files %v", got)
	}
}

// A saved text is written in runs of lines, each run saying the line it
// starts at, and a line too long for one frame in pieces; the frames in order
// are the text.
func TestASourceIsWrittenInRunsOfLines(t *testing.T) {
	line := strings.Repeat("x", 9000) + "\n"
	long := strings.Repeat("y", sourceTextBytes+10) + "\n"
	// A line of three-byte characters, cut where the bound falls inside one.
	wide := strings.Repeat("═", sourceTextBytes/3+10) + "\n"
	s := savedSource{manifest: sessionjudge.Source{ID: "R1"}, text: line + line + long + "end\n" + wide}
	frames := sourceFrames(s)
	var got []string
	for _, f := range frames[1:] {
		x := f.(*sessionjudge.SourceText)
		if len(x.Text) > sourceTextBytes || !utf8.ValidString(x.Text) {
			t.Fatalf("a frame of %d bytes, whole characters %v", len(x.Text), utf8.ValidString(x.Text))
		}
		got = append(got, fmt.Sprintf("%d:%d", x.From, len(x.Text)))
	}
	// The rest of the long line and the line after it share a frame, which
	// starts on the long line.
	if strings.Join(got, " ") != fmt.Sprintf("1:9001 2:9001 3:%d 3:15 5:%d 5:%d", sourceTextBytes, sourceTextBytes-1, len(wide)-(sourceTextBytes-1)) {
		t.Fatalf("frames %v", got)
	}
	var whole strings.Builder
	for _, f := range frames[1:] {
		whole.WriteString(f.(*sessionjudge.SourceText).Text)
	}
	if whole.String() != s.text {
		t.Fatal("the frames are not the text")
	}
	if frames[0].(*sessionjudge.Source).ID != "R1" || len(sourceFrames(savedSource{manifest: sessionjudge.Source{ID: "R2"}})) != 1 {
		t.Fatal("a failed source is its manifest alone")
	}
}
