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
	"cmp"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
)

// redacted stands where a secret was.
const redacted = "[redacted]"

// redactDepth is how many levels of JSON string inside JSON string a secret
// is looked for in: a writer that escaped what another writer escaped, up
// to four writers. A secret deeper than this is not found, and the docs say
// so.
const redactDepth = 4

// redactor takes the secrets of one fetch out of everything asz keeps of
// it, and counts how many it took.
type redactor struct {
	// secrets are each secret once. Where two overlap, the stretches found
	// are merged, so one that holds another is taken whole.
	secrets []string
	count   int
}

func newRedactor(secrets []string) *redactor {
	var out []string
	for _, s := range secrets {
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return &redactor{secrets: out}
}

// credentials are the secrets a header value holds: the value, and after a
// Basic or Bearer scheme word the credential; for Basic, also what it
// decodes to, user:token, and its user and its token apart.
func credentials(value string) []string {
	out := []string{value}
	scheme, cred, ok := strings.Cut(strings.TrimSpace(value), " ")
	cred = strings.TrimSpace(cred)
	if !ok || cred == "" {
		return out
	}
	switch {
	case strings.EqualFold(scheme, "Bearer"):
		out = append(out, cred)
	case strings.EqualFold(scheme, "Basic"):
		out = append(out, cred)
		if b, err := base64.StdEncoding.DecodeString(cred); err == nil {
			user, token, _ := strings.Cut(string(b), ":")
			out = append(out, string(b), user, token)
		}
	}
	return out
}

// proxySecrets are the user and password of a proxy URL, as written and as
// decoded, and the Basic credential Go sends a proxy for them.
func proxySecrets(proxy *url.URL) []string {
	if proxy == nil || proxy.User == nil {
		return nil
	}
	user := proxy.User.Username()
	pass, _ := proxy.User.Password()
	pair := user + ":" + pass
	basic := base64.StdEncoding.EncodeToString([]byte(pair))
	return []string{user, pass, pair, proxy.User.String(), basic, "Basic " + basic}
}

// text takes the secrets out of a string. A secret is looked for as
// written, and through every JSON spelling: in each view of the string with
// one more level of JSON escapes decoded wherever they stand, and in every
// JSON string literal of each view, decoded as a JSON parser decodes one,
// to redactDepth levels in all. A quote a backslash escapes is so tried as
// an opening quote in the view where it is a quote, though no literal holds
// it. Each stretch found is replaced in the string: a literal whole, its
// quotes kept as they were written, with "[redacted]" inside.
func (r *redactor) text(s string) string {
	if len(r.secrets) == 0 {
		return s
	}
	spans := r.found(s, redactDepth)
	if len(spans) == 0 {
		return s
	}
	slices.SortFunc(spans, func(a, b span) int { return cmp.Or(cmp.Compare(a.from, b.from), cmp.Compare(b.to, a.to)) })
	merged := []span{spans[0]}
	for _, sp := range spans[1:] {
		if last := &merged[len(merged)-1]; sp.from < last.to {
			last.to = max(last.to, sp.to)
			continue
		}
		merged = append(merged, sp)
	}
	var b strings.Builder
	at := 0
	for _, sp := range merged {
		b.WriteString(s[at:sp.from])
		b.WriteString(replacement(s[sp.from:sp.to]))
		at = sp.to
	}
	b.WriteString(s[at:])
	r.count += len(merged)
	return b.String()
}

// replacement is what stands where a stretch held a secret: a literal keeps
// its quotes as they were escaped, so the text around it reads the same.
func replacement(stretch string) string {
	q := strings.IndexByte(stretch, '"')
	if q >= 0 && strings.Trim(stretch[:q], `\`) == "" && len(stretch) >= 2*(q+1) && strings.HasSuffix(stretch, stretch[:q+1]) {
		return stretch[:q+1] + redacted + stretch[:q+1]
	}
	return redacted
}

// span is a stretch of a text, from inclusive to exclusive.
type span struct{ from, to int }

// found are the stretches of s that hold a secret within depth levels of
// decoding: as written in s and in each view of it, a view one level, and a
// literal of a view, which its decoding takes one level more, when it holds
// one.
func (r *redactor) found(s string, depth int) []span {
	var out []span
	v := viewOf(s)
	for level := 0; ; level++ {
		for _, sec := range r.secrets {
			for i := 0; ; {
				j := strings.Index(v.text[i:], sec)
				if j < 0 {
					break
				}
				out = append(out, span{v.from[i+j], v.to[i+j+len(sec)-1]})
				i += j + len(sec)
			}
		}
		if level == depth {
			return out
		}
		for _, l := range literals(v.text) {
			if r.holds(l.decoded, depth-level-1) {
				out = append(out, span{v.from[l.from], v.to[l.to]})
			}
		}
		next, decoded := v.unescaped()
		if !decoded {
			return out
		}
		v = next
	}
}

// holds says whether a string holds a secret within depth levels of
// decoding.
func (r *redactor) holds(s string, depth int) bool {
	return len(r.found(s, depth)) > 0
}

// textView is a text with some levels of JSON escapes decoded, and for each of
// its bytes the stretch of the original text it came from.
type textView struct {
	text     string
	from, to []int
}

func viewOf(s string) textView {
	v := textView{text: s, from: make([]int, len(s)), to: make([]int, len(s))}
	// Every byte, not every character: a match is found by bytes.
	for i := 0; i < len(s); i++ {
		v.from[i], v.to[i] = i, i+1
	}
	return v
}

// unescaped is the view with one more level of JSON escapes decoded,
// wherever they stand: in prose, a log line or escaped JSON with no quotes
// around it. A backslash before what no escape begins with stays as it is.
// decoded is false when there was no escape to decode.
func (v textView) unescaped() (out textView, decoded bool) {
	s := v.text
	// Every byte written from s[i:j] came from the original stretch those
	// bytes of the view came from.
	var b strings.Builder
	write := func(bs string, i, j int) {
		b.WriteString(bs)
		for range len(bs) {
			out.from = append(out.from, v.from[i])
			out.to = append(out.to, v.to[j-1])
		}
	}
	for i := 0; i < len(s); {
		if s[i] != '\\' || i+1 == len(s) {
			write(s[i:i+1], i, i+1)
			i++
			continue
		}
		var c string
		n := 2
		switch s[i+1] {
		case '"', '\\', '/':
			c = s[i+1 : i+2]
		case 'b':
			c = "\b"
		case 'f':
			c = "\f"
		case 'n':
			c = "\n"
		case 'r':
			c = "\r"
		case 't':
			c = "\t"
		case 'u':
			var r rune
			if r, n = escapedRune(s[i:]); n > 0 {
				c = string(r)
			}
		}
		if c == "" {
			write(s[i:i+1], i, i+1)
			i++
			continue
		}
		write(c, i, i+n)
		decoded = true
		i += n
	}
	out.text = b.String()
	return out, decoded
}

// escapedRune reads a \u escape at the start of s, with the low half of a
// surrogate pair after it when it is the high half, as a JSON parser does,
// a lone half read as U+FFFD; n is the bytes it took, 0 for no escape.
func escapedRune(s string) (r rune, n int) {
	hex := func(t string) (rune, bool) {
		if len(t) < 6 || t[0] != '\\' || t[1] != 'u' {
			return 0, false
		}
		v, err := strconv.ParseUint(t[2:6], 16, 16)
		return rune(v), err == nil
	}
	r, ok := hex(s)
	if !ok {
		return 0, 0
	}
	if utf16.IsSurrogate(r) {
		if low, ok := hex(s[6:]); ok && len(s) >= 12 {
			if pair := utf16.DecodeRune(r, low); pair != unicode.ReplacementChar {
				return pair, 12
			}
		}
		return unicode.ReplacementChar, 6
	}
	return r, 6
}

// value takes the secrets out of a decoded JSON value: each string and each
// key as text does, and a number written as a secret becomes the string
// "[redacted]", so the value encodes again as valid JSON.
func (r *redactor) value(v any) any {
	switch x := v.(type) {
	case string:
		return r.text(x)
	case json.Number:
		if slices.Contains(r.secrets, x.String()) {
			r.count++
			return redacted
		}
		return x
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = r.value(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[r.text(k)] = r.value(e)
		}
		return out
	}
	return v
}

// literal is one JSON string literal found in a text: where it stands, its
// quotes included, and what it decodes to.
type literal struct {
	from, to int
	decoded  string
}

// literals are the JSON string literals in a text, from every double quote
// not escaped by a backslash before it, each to the first quote after it
// that its escapes leave closing it, decoded as a parser that takes a raw
// control character decodes one. One that does not decode is no literal:
// the text is searched as written there.
func literals(s string) []literal {
	var out []literal
	for i := 0; i < len(s); i++ {
		if s[i] != '"' || escaped(s, i) {
			continue
		}
		end := -1
		for j := i + 1; j < len(s); j++ {
			if s[j] == '\\' {
				j++
				continue
			}
			if s[j] == '"' {
				end = j
				break
			}
		}
		if end < 0 {
			break
		}
		var d string
		if json.Unmarshal(controlsEscaped(s[i:end+1]), &d) == nil {
			out = append(out, literal{from: i, to: end, decoded: d})
		}
	}
	return out
}

// controlsEscaped writes each control character of a literal as its escape.
// A strict parser refuses one written raw, but text blocks joined by a blank
// line can put one inside a literal that holds a secret; no escape holds a
// raw control character, so the rest decodes as written.
func controlsEscaped(lit string) []byte {
	if !strings.ContainsFunc(lit, func(c rune) bool { return c < 0x20 }) {
		return []byte(lit)
	}
	var b strings.Builder
	for _, c := range lit {
		if c < 0x20 {
			fmt.Fprintf(&b, "\\u%04x", c)
			continue
		}
		b.WriteRune(c)
	}
	return []byte(b.String())
}

// escaped says whether the character at i follows an odd number of
// backslashes.
func escaped(s string, i int) bool {
	n := 0
	for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
		n++
	}
	return n%2 == 1
}
