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

package judge

import (
	"bytes"
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
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/mcpclient"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
)

// reference is a source as a person gave it, which a fetch by http or by a
// command uses, and as asz keeps it: a URL without its query and fragment.
type reference struct {
	given, kept  string
	queryRemoved bool
}

// referenceOf is a source with what asz keeps of it. A source that names a
// scheme is a URL, as fetchOne reads one.
func referenceOf(src string) reference {
	r := reference{given: src, kept: src}
	if i := strings.Index(src, "://"); i > 0 {
		if cut := strings.IndexAny(src[i+3:], "?#"); cut >= 0 {
			r.kept, r.queryRemoved = src[:i+3+cut], true
		}
	}
	return r
}

// CheckReferences refuses a URL that holds a user or a password, before a
// run writes anything. The error never names it: nothing of it is kept. A
// credential goes through judge.references.fetch.
func CheckReferences(sources []string) error {
	for i, src := range sources {
		if j := strings.Index(src, "://"); j > 0 {
			rest := src[j+3:]
			if end := strings.IndexAny(rest, "/?#"); end >= 0 {
				rest = rest[:end]
			}
			if strings.Contains(rest, "@") {
				return fmt.Errorf("judge: reference %d holds a user or a password in its URL, and is not used or kept: give a credential through judge.references.fetch", i+1)
			}
		}
	}
	return nil
}

// fetcherOf is the configured fetch whose prefix a source starts with, the
// first in order, or nil.
func fetcherOf(src string, cfg config.References) *config.Fetcher {
	for i := range cfg.Fetch {
		if strings.HasPrefix(src, cfg.Fetch[i].Prefix) {
			return &cfg.Fetch[i]
		}
	}
	return nil
}

// failed is a fetch's failure as asz keeps it: its class and the detail
// asz built, each URL the request used written as it is kept, and every
// secret redacted, before it is cut to 300 bytes. A URL can come back
// spelled another way, its slashes escaped in JSON, so each URL used and
// what follows its kept part are secrets too.
func failed(r *redactor, used map[string]string, class, detail string) error {
	secrets := slices.Clone(r.secrets)
	for full, kept := range used {
		if full == "" || full == kept {
			continue
		}
		detail = strings.ReplaceAll(detail, full, kept)
		secrets = append(secrets, full)
		if tail, ok := strings.CutPrefix(full, kept); ok && len(tail) > 1 {
			secrets = append(secrets, tail)
		}
	}
	return errors.New(clip(newRedactor(secrets).text(class+": "+detail), 300))
}

// mcpSetup is what an MCP fetch is made with: the server to call, as the
// client takes it, the secrets of the fetch, and how the server is kept.
type mcpSetup struct {
	server  mcpclient.Server
	secrets []string
	kept    string
	stderr  *boundedBuffer
}

// mcpServer resolves an entry's server: each header's value from its
// variable, which must be set, and the secrets the fetch holds: the
// headers' credentials, the variables a local server reads one from, and
// the user and password of the proxy a remote one is reached through.
func mcpServer(m *config.MCPFetch, version string) (*mcpSetup, error) {
	s := &mcpSetup{stderr: &boundedBuffer{max: 64 << 10}}
	s.server = mcpclient.Server{URL: m.URL, Command: m.Command, Version: version, Stderr: s.stderr}
	if m.URL != "" {
		u, _ := url.Parse(m.URL)
		s.kept = u.Scheme + "://" + u.Host
		s.server.Headers = map[string]string{}
		for name, env := range m.Headers {
			v := os.Getenv(env)
			if v == "" {
				return s, fmt.Errorf("the variable %s, which holds the header %s, is not set", env, name)
			}
			s.server.Headers[name] = v
			s.secrets = append(s.secrets, credentials(v)...)
		}
		req, _ := http.NewRequest(http.MethodPost, m.URL, nil)
		if proxy, err := proxyFor(req); err == nil {
			s.secrets = append(s.secrets, proxySecrets(proxy)...)
		}
	} else {
		s.kept = programName(m.Command[0])
	}
	for _, env := range m.SecretEnv {
		s.secrets = append(s.secrets, os.Getenv(env))
	}
	return s, nil
}

// proxyFor is the proxy a request to a server goes through, as the client
// finds it. Go reads the environment for it once, so a test sets this.
var proxyFor = http.ProxyFromEnvironment

// programName is a program's name without its directory.
func programName(path string) string {
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		return path[i+1:]
	}
	return path
}

// fetchMCP calls an entry's tool for one source and saves what it answered.
// The result is read as a tool result before anything is redacted: which
// blocks are text, whether the tool failed. Then every string of it is
// redacted, and the text the evaluators read is taken from the redacted
// values: the text blocks that are not empty, joined by a blank line, or
// with none, the structured content as JSON. A last pass redacts the text
// joined, before anything is digested or cut.
func fetchMCP(ctx context.Context, ref reference, f config.Fetcher, maxBytes int, m *sessionjudge.Source, version string) (savedSource, error) {
	mc := f.MCP
	m.Method, m.ContentType = sessionjudge.MethodMCP, "application/json"
	args, keptArgs := map[string]any{}, map[string]any{}
	for name, v := range mc.Arguments {
		args[name] = placed(v, ref.kept, strings.TrimPrefix(ref.kept, f.Prefix))
		keptArgs[name] = args[name]
	}
	for name, env := range mc.ArgumentsEnv {
		keptArgs[name] = "[env " + env + "]"
	}
	info := &sessionjudge.SourceMCP{Tool: mc.Tool, Arguments: keptArgs}
	m.MCP = info
	setup, err := mcpServer(mc, version)
	info.Server = setup.kept
	if err != nil {
		return savedSource{}, err
	}
	for name, env := range mc.ArgumentsEnv {
		v := os.Getenv(env)
		if v == "" {
			return savedSource{}, fmt.Errorf("the variable %s, which holds the argument %s, is not set", env, name)
		}
		args[name] = v
		setup.secrets = append(setup.secrets, v)
	}
	if mc.Tool == "" {
		return savedSource{}, fmt.Errorf("no tool is configured for %s: run asz judge -reference-tools %s to list the server's tools", f.Prefix, ref.kept)
	}
	called, err := mcpclient.Call(ctx, setup.server, mc.Tool, args)
	if err != nil {
		return savedSource{}, setup.failure(err)
	}
	info.Era, info.Version = called.Era, called.Version
	r := newRedactor(append(setup.secrets, called.Sessions...))
	return savedMCP(called.Result, r, setup.used(), maxBytes, *m)
}

// used are the URLs a fetch's requests used, with what is kept of each.
func (s *mcpSetup) used() map[string]string {
	return map[string]string{s.server.URL: s.kept}
}

// failure is a call's or a listing's failure as asz keeps it: its class, and
// what the client said with what a local server wrote on its standard error,
// each redacted whole before anything is cut, the session ids the server
// gave among the secrets.
func (s *mcpSetup) failure(err error) error {
	class, detail := mcpclient.ClassTransport, err.Error()
	var e *mcpclient.Error
	if errors.As(err, &e) {
		s.secrets = append(s.secrets, e.Sessions...)
		class, detail = e.Class, strings.TrimPrefix(detail, e.Class+": ")
	}
	r := newRedactor(s.secrets)
	if stderr := strings.TrimSpace(r.text(s.stderr.buf.String())); stderr != "" {
		detail += "; it wrote: " + clip(stderr, 300)
	}
	return failed(r, s.used(), class, detail)
}

// savedMCP saves a tool's result. Every decision is made from the result as
// it came, before anything is redacted, so a secret that matches a word of
// the protocol, such as a user named text, changes no decision.
func savedMCP(raw json.RawMessage, r *redactor, used map[string]string, maxBytes int, m sessionjudge.Source) (savedSource, error) {
	info := m.MCP
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var result map[string]any
	if dec.Decode(&result) != nil {
		return savedSource{manifest: m}, failed(r, used, mcpclient.ClassProtocol, "a tool result that is not an object")
	}
	var texts []string
	content, _ := result["content"].([]any)
	for _, b := range content {
		block, _ := b.(map[string]any)
		if block["type"] != "text" {
			info.BlocksOmitted++
			continue
		}
		if s, _ := block["text"].(string); s != "" {
			texts = append(texts, s)
		}
	}
	structured := result["structuredContent"]
	failedTool, _ := result["isError"].(bool)

	// The pieces are redacted apart from the result, so the count is the
	// result's: a scratch redactor takes them.
	scratch := newRedactor(r.secrets)
	if failedTool {
		said := scratch.text(strings.Join(texts, "\n\n"))
		if said == "" {
			said = "the tool reported a failure and said nothing"
		}
		return savedSource{manifest: m}, failed(r, used, mcpclient.ClassToolError, said)
	}
	kept := r.value(result)
	var text string
	switch {
	case len(texts) > 0:
		for i := range texts {
			texts[i] = scratch.text(texts[i])
		}
		text = strings.Join(texts, "\n\n")
		info.StructuredOmitted = structured != nil
	case structured != nil:
		text = string(encodeJSON(scratch.value(structured)))
	default:
		return savedSource{manifest: m}, failed(r, used, mcpclient.ClassProtocol, "the tool answered no text and no structured content")
	}
	// What only shows once the blocks are joined.
	text = r.text(text)
	info.Redactions = r.count

	fetched := encodeJSON(kept)
	sum := sha256.Sum256(fetched)
	m.Bytes, m.SHA256 = len(fetched), hex.EncodeToString(sum[:])
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	if len(text) > maxBytes {
		text, m.Cut = text[:maxBytes], true
		for len(text) > 0 && !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
	}
	ts := sha256.Sum256([]byte(text))
	m.Text = &sessionjudge.Extracted{Bytes: len(text), SHA256: hex.EncodeToString(ts[:])}
	m.Lines = strings.Count(text, "\n")
	if text != "" && !strings.HasSuffix(text, "\n") {
		m.Lines++
	}
	return savedSource{manifest: m, data: fetched, text: text}, nil
}

// encodeJSON writes a decoded value as JSON, its numbers as they were
// written and nothing escaped for HTML.
func encodeJSON(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

// placed copies a configured argument with {source} and {rest} in each
// string, put in once: a value that holds either is not read again.
func placed(v any, source, rest string) any {
	switch x := v.(type) {
	case string:
		return strings.NewReplacer("{source}", source, "{rest}", rest).Replace(x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = placed(e, source, rest)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = placed(e, source, rest)
		}
		return out
	}
	return v
}

// ReferenceTools prints the tools of the MCP server the fetch entry for src
// names, for setting up its tool: each tool's name, description and input
// schema, with the server's settings alone. A tool whose header annotations
// break the rules is counted, not printed, and a list longer than twenty
// pages says it is incomplete.
func ReferenceTools(ctx context.Context, cfg config.References, src, version string, w io.Writer) error {
	if err := CheckReferences([]string{src}); err != nil {
		return err
	}
	ref := referenceOf(src)
	f := fetcherOf(src, cfg)
	if f == nil || f.MCP == nil {
		return fmt.Errorf("judge: no judge.references.fetch entry with an MCP server takes %s", ref.kept)
	}
	setup, err := mcpServer(f.MCP, version)
	if err != nil {
		return fmt.Errorf("judge: %w", err)
	}
	timeout := time.Minute
	if d, err := time.ParseDuration(cfg.Timeout); err == nil && d > 0 {
		timeout = d
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	l, err := mcpclient.List(ctx, setup.server, 20)
	if err != nil {
		return fmt.Errorf("judge: %s: %w", setup.kept, setup.failure(err))
	}
	r := newRedactor(append(setup.secrets, l.Sessions...))
	fmt.Fprintf(w, "%s speaks %s (%s); %d tools\n", setup.kept, l.Version, l.Era, len(l.Tools))
	for _, t := range l.Tools {
		fmt.Fprintf(w, "\n%s\n", r.text(t.Name))
		if t.Description != "" {
			fmt.Fprintf(w, "  %s\n", r.text(strings.Join(strings.Fields(t.Description), " ")))
		}
		fmt.Fprintf(w, "  input: %s\n", r.text(string(t.InputSchema)))
	}
	if l.Invalid > 0 {
		fmt.Fprintf(w, "\n%d tools left out: their x-mcp-header annotations break the rules, and asz cannot call them\n", l.Invalid)
	}
	if !l.Complete {
		fmt.Fprintln(w, "\nthe list is incomplete: the server has more than 20 pages of tools, and a tool not shown may still be there")
	}
	return nil
}
