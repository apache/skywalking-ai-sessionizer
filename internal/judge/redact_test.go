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
	"encoding/base64"
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// A secret is found however a JSON writer spelled it, to four levels of
// string inside string, and a literal that holds one is replaced whole; a
// secret is found as written everywhere else.
func TestASecretIsFoundHoweverItIsSpelled(t *testing.T) {
	nest := func(s string, levels int) string {
		for range levels {
			b, _ := json.Marshal(s)
			s = string(b)
		}
		return s
	}
	for _, c := range []struct {
		name, secret, in, out string
	}{
		{"as written", "abc123", "token abc123 here", "token [redacted] here"},
		{"a character of more than one byte", "café", "café", "[redacted]"},
		{"a character of more than one byte, after another", "café", "x café", "x [redacted]"},
		{"escaped as \\u", "abc123", `{"password":"ab\u0063123"}`, `{"password":"[redacted]"}`},
		{"an apostrophe as .NET writes it", "a'b", `{"password":"a\u0027b"}`, `{"password":"[redacted]"}`},
		{"a quote in loose prose before the JSON", "a'b", `He said "the payload is {"password":"a\u0027b"}"`,
			`He said "the payload is {"password":"[redacted]"}"`},
		{"a secret holding a quote", `a"b`, `{"password":"a\"b"}`, `{"password":"[redacted]"}`},
		{"a lone JSON string", `a"b`, `"a\"b"`, `"[redacted]"`},
		{"three levels down", "a'b", "copy: " + nest(`{"k":"a\u0027b"}`, 2), `copy: "[redacted]"`},
		{"Python, then a Go wrapper", "café", `wrapper: "{\"p\": \"caf\\u00e9\"}"`, `wrapper: "[redacted]"`},
		{"two blocks joined inside a literal", "abc", "{\"k\":\"x\n\nab\\u0063\"}", `{"k":"[redacted]"}`},
		{"a literal that does not decode", "abc123", `say "bad \x abc123" then`, `say "bad \x [redacted]" then`},
		// A quote a backslash escapes opens a literal too, and escaped JSON
		// with no quotes around it, as a log line prints it, is decoded.
		{"an escaped opening quote", "a'b", `payload: \"a\u0027b"`, "payload: [redacted]"},
		{"escaped JSON with no quotes around it", "a'b", `body={\"token\":\"a\\u0027b\"}`,
			`body={\"token\":\"[redacted]\"}`},
		{"an escaped quote in prose", `a"b`, `x a\"b y`, "x [redacted] y"},
		{"an escape in a log line", "tok-123", `token=tok\u002d123 sent`, "token=[redacted] sent"},
		{"five levels down, not found", "a'b", nest(`{"k":"a\u0027b"}`, 4), nest(`{"k":"a\u0027b"}`, 4)},
	} {
		r := newRedactor([]string{c.secret})
		if got := r.text(c.in); got != c.out {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.out)
		}
	}
}

// A decoded result keeps its shape: every string and key is redacted, and a
// number written as a secret becomes a string, so it encodes as valid JSON.
func TestARedactedValueStaysJSON(t *testing.T) {
	dec := json.NewDecoder(strings.NewReader(`{"n":12345,"m":1.50,"s":["the key 12345"],"12345":true}`))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	r := newRedactor([]string{"12345"})
	b, err := json.Marshal(r.value(v))
	if err != nil || string(b) != `{"[redacted]":true,"m":1.50,"n":"[redacted]","s":["the key [redacted]"]}` || r.count != 3 {
		t.Fatalf("%s %v, %d redactions", b, err, r.count)
	}
}

// A header's secrets are its value and its credential; a Basic credential
// also what it decodes to, and its user and token. A proxy's are its user
// and password, as written and decoded, and the Basic form Go sends it.
func TestTheSecretsOfAFetch(t *testing.T) {
	basic := base64.StdEncoding.EncodeToString([]byte("me@example.com:tok"))
	got := credentials("Basic " + basic)
	for _, want := range []string{"Basic " + basic, basic, "me@example.com:tok", "me@example.com", "tok"} {
		if !slices.Contains(got, want) {
			t.Errorf("basic: %q missing from %q", want, got)
		}
	}
	if got := credentials("Bearer k1"); !slices.Contains(got, "k1") {
		t.Errorf("bearer: %q", got)
	}
	proxy, _ := url.Parse("http://u:p%40ss@proxy.example:8080")
	got = proxySecrets(proxy)
	pair := base64.StdEncoding.EncodeToString([]byte("u:p@ss"))
	for _, want := range []string{"p@ss", "u:p%40ss", pair, "Basic " + pair} {
		if !slices.Contains(got, want) {
			t.Errorf("proxy: %q missing from %q", want, got)
		}
	}
}
