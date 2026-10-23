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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
)

// sourceTextBytes bounds one source_text frame, so a large source is many
// small frames, each read and sent whole.
const sourceTextBytes = 16 << 10

// savedSource is a source as asz saved it: its manifest, the bytes fetched,
// bounded, and the text an evaluator reads, both empty when it could not be
// fetched.
type savedSource struct {
	manifest sessionjudge.Source
	data     []byte
	text     string
}

// fetchSources copies or fetches each source, before any evaluator runs. A
// source that cannot be fetched is kept as failed, with why. A file is
// read, a source a configured command's prefix names goes through that
// command, which never sees the conversation, and a plain http or https URL
// is fetched; anything else is no source asz can read.
func fetchSources(ctx context.Context, sources []string, cfg config.References, now func() time.Time) []savedSource {
	timeout := time.Minute
	if d, err := time.ParseDuration(cfg.Timeout); err == nil && d > 0 {
		timeout = d
	}
	var out []savedSource
	for i, src := range sources {
		m := sessionjudge.Source{ID: fmt.Sprintf("R%d", i+1), Source: src, Captured: now().UTC().Format(time.RFC3339), Outcome: sessionjudge.SourceSaved}
		fctx, cancel := context.WithTimeout(ctx, timeout)
		data, err := fetchOne(fctx, src, cfg, &m)
		cancel()
		s := savedSource{manifest: m}
		if err == nil {
			s, err = saved(m, data, cfg.MaxBytes)
		}
		if err != nil {
			s = savedSource{manifest: m}
			s.manifest.Outcome, s.manifest.Error = sessionjudge.SourceFailed, err.Error()
		}
		out = append(out, s)
	}
	return out
}

// fetchOne reads a source's bytes, setting how it was fetched and what the
// source says of itself.
func fetchOne(ctx context.Context, src string, cfg config.References, m *sessionjudge.Source) ([]byte, error) {
	// One byte past the bound is read, so saved can tell a source was cut.
	limit := cfg.MaxBytes
	if limit <= 0 {
		limit = 1 << 20
	}
	for _, f := range cfg.Fetch {
		if strings.HasPrefix(src, f.Prefix) {
			m.Method, m.ContentType = sessionjudge.MethodCommand, f.ContentType
			if m.ContentType == "" {
				m.ContentType = "text/plain"
			}
			cmd := exec.CommandContext(ctx, f.Command[0], append(f.Command[1:], src)...)
			stdout, stderr := &boundedBuffer{max: limit + 1}, &boundedBuffer{max: 4 << 10}
			cmd.Stdout, cmd.Stderr = stdout, stderr
			if err := cmd.Run(); err != nil {
				return nil, fmt.Errorf("%s: %w: %s", f.Command[0], err, strings.TrimSpace(clip(stderr.buf.String(), 300)))
			}
			return stdout.buf.Bytes(), nil
		}
	}
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		m.Method = sessionjudge.MethodHTTP
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "asz-judge")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return nil, fmt.Errorf("the server answered %s", resp.Status)
		}
		m.ContentType = resp.Header.Get("Content-Type")
		m.Revision = resp.Header.Get("ETag")
		m.Published = resp.Header.Get("Last-Modified")
		return io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	}
	if strings.Contains(src, "://") {
		return nil, fmt.Errorf("no fetch command is configured for %s, and asz fetches only http and https itself", src)
	}
	m.Method = sessionjudge.MethodFile
	// Opening a named pipe waits for a writer, and a device never ends, so
	// only a regular file is read.
	info, err := os.Stat(src)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", src)
	}
	f, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	m.ContentType = mime.TypeByExtension(strings.ToLower(extOf(src)))
	if m.ContentType == "" {
		m.ContentType = "text/plain"
	}
	return data, nil
}

// boundedBuffer keeps the first max bytes written to it and drops the rest,
// so a command that writes more than a source may hold cannot fill memory.
type boundedBuffer struct {
	max int
	buf bytes.Buffer
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func extOf(path string) string {
	if i := strings.LastIndexByte(path, '.'); i > strings.LastIndexByte(path, '/') {
		return path[i:]
	}
	return ""
}

// saved bounds a source's bytes, digests them, and takes the text an
// evaluator reads: HTML as its text, other text as it is. Bytes that hold
// no text, such as a PDF or an image, leave the source failed.
func saved(m sessionjudge.Source, data []byte, maxBytes int) (savedSource, error) {
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	// A cut inside a character takes back the part of it that was kept.
	if len(data) > maxBytes {
		data, m.Cut = data[:maxBytes], true
		for i := 0; i < utf8.UTFMax-1 && len(data) > 0 && !utf8.Valid(data); i++ {
			data = data[:len(data)-1]
		}
	}
	sum := sha256.Sum256(data)
	m.Bytes, m.SHA256 = len(data), hex.EncodeToString(sum[:])
	media, _, _ := mime.ParseMediaType(m.ContentType)
	var text string
	switch {
	case media == "text/html" || media == "application/xhtml+xml":
		text = htmlText(string(data))
		ts := sha256.Sum256([]byte(text))
		m.Text = &sessionjudge.Extracted{Bytes: len(text), SHA256: hex.EncodeToString(ts[:])}
	case utf8.Valid(data) && !bytes.ContainsRune(data, 0):
		text = string(data)
	default:
		return savedSource{manifest: m}, fmt.Errorf("its content, %s, holds no text asz can read", m.ContentType)
	}
	m.Lines = strings.Count(text, "\n")
	if text != "" && !strings.HasSuffix(text, "\n") {
		m.Lines++
	}
	return savedSource{manifest: m, data: data, text: text}, nil
}

// sourceFiles are the files that keep each source in the run's work
// directory, written before any evaluator runs: its manifest as <id>.json
// and, when it was saved, the bytes fetched as <id>.fetched and the text an
// evaluator reads as <id>.txt. A run whose reference stage never runs still
// holds what it was given, and a page's digest can be checked against its
// bytes after the page changed.
func sourceFiles(saved []savedSource) ([]storage.WorkFile, error) {
	var out []storage.WorkFile
	for _, s := range saved {
		id := s.manifest.ID
		m, err := json.Marshal(s.manifest)
		if err != nil {
			return nil, err
		}
		out = append(out, storage.WorkFile{Name: id + ".json", Data: append(m, '\n')})
		if s.manifest.Outcome == sessionjudge.SourceSaved {
			out = append(out, storage.WorkFile{Name: id + ".fetched", Data: s.data}, storage.WorkFile{Name: id + ".txt", Data: []byte(s.text)})
		}
	}
	return out, nil
}

var (
	htmlDropped = regexp.MustCompile(`(?is)<(script|style|noscript)\b.*?</(script|style|noscript)\s*>|<!--.*?-->`)
	htmlBreaks  = regexp.MustCompile(`(?i)<(br|/p|/div|/li|/tr|/h[1-6]|/pre|/blockquote|/table|hr)\b[^>]*>`)
	htmlTags    = regexp.MustCompile(`(?s)<[^>]*>`)
	blankRuns   = regexp.MustCompile(`\n[ \t]*(\n[ \t]*)+`)
)

// htmlText is a page's text: its scripts, styles and comments dropped, a
// line break where a block ends, every other tag taken out, and character
// references read.
func htmlText(page string) string {
	s := htmlDropped.ReplaceAllString(page, "")
	s = htmlBreaks.ReplaceAllString(s, "\n")
	s = htmlTags.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		lines = append(lines, strings.Join(strings.Fields(l), " "))
	}
	return strings.TrimSpace(blankRuns.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")) + "\n"
}

// sourceFrames are a saved source as the reference stage writes it: its
// manifest, then its text in runs.
func sourceFrames(s savedSource) []any {
	m := s.manifest
	out := []any{&m}
	for _, r := range textRuns(s.text) {
		out = append(out, &sessionjudge.SourceText{Source: m.ID, From: r.from, Text: r.text})
	}
	return out
}

// textRun is a run of a text and the line it starts in, counting from 1.
type textRun struct {
	from int
	text string
}

// textRuns cuts a text into runs of lines, each within sourceTextBytes, so a
// frame of a run is read and sent whole. A line longer than that is cut
// between characters across runs that each name its line, and its last
// piece can share a run with the lines after it.
func textRuns(text string) []textRun {
	var out []textRun
	var b strings.Builder
	from, line := 1, 1
	flush := func(next int) {
		if b.Len() > 0 {
			out = append(out, textRun{from: from, text: b.String()})
			b.Reset()
		}
		from = next
	}
	for _, l := range strings.SplitAfter(text, "\n") {
		if l == "" {
			continue
		}
		if b.Len()+len(l) > sourceTextBytes {
			flush(line)
		}
		for len(l) > sourceTextBytes {
			cut := sourceTextBytes
			for cut > 0 && !utf8.RuneStart(l[cut]) {
				cut--
			}
			b.WriteString(l[:cut])
			l = l[cut:]
			flush(line)
		}
		b.WriteString(l)
		line++
	}
	flush(line)
	return out
}
