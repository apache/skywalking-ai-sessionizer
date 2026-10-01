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

package view

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// Withholding is how a conversation is served to the people an agent served
// without the system prompt and the tool schemas the runtime sent them.
//
// asz knows nothing about who is reading. What it knows is what a record
// carries, by the flags its adapter set, and it withholds by those names,
// never by the text or the size of a part. An instance withholds what its
// configuration says for every reader; a request adds to that with its hide
// parameter and never takes from it, so a host that serves the API through
// its own route can withhold more for one reader. Two audiences are two
// instances over the same root, each behind the deployment's own
// authentication.
//
// A withheld step keeps its node, its flags and its size, loses its text,
// and says so with the state omitted. It is never deleted, so every count,
// the round chain and the integrity badge still hold. The provider bodies go
// with it: a request carries the system prompt and the tool schemas again,
// and the renderer checks a body's digest, so a body is served whole or not
// at all.

// SetHide says what this instance withholds from every reader. It is set
// before the handler serves and read by every request after.
func (s *Server) SetHide(names []string) error {
	for _, name := range names {
		if !sessiondata.IsWithholdable(name) {
			return fmt.Errorf("view: hide names %q; the page can withhold %s", name, strings.Join(sessiondata.Withholdable(), " and "))
		}
	}
	s.hide = withheldNames(names)
	return nil
}

// hideFor is what one request withholds: what the instance withholds, and
// what the request's hide parameter adds, given once or more, each a name or
// a comma-separated list. A name the page cannot withhold is refused rather
// than ignored, since a reader that asked for less than it got would not
// know.
func (s *Server) hideFor(q url.Values) ([]string, error) {
	names := append([]string(nil), s.hide...)
	for _, value := range q["hide"] {
		for _, name := range strings.Split(value, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if !sessiondata.IsWithholdable(name) {
				return nil, fmt.Errorf("view: hide names %q; the page can withhold %s", name, strings.Join(sessiondata.Withholdable(), " and "))
			}
			names = append(names, name)
		}
	}
	return withheldNames(names), nil
}

// withheldNames is a set of names in one order, so the same set keys the
// same document.
func withheldNames(names []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(names))
	for _, name := range names {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// withhold takes the withheld steps' content out of a document and says what
// was withheld. The document is this reader's own copy.
func withhold(v *sessionview.Conversation, names []string) {
	hidden := map[string]bool{}
	for _, name := range names {
		hidden[name] = true
	}
	counts := map[string]int{}
	var walk func([]sessionview.Node)
	walk = func(nodes []sessionview.Node) {
		for i := range nodes {
			n := &nodes[i]
			if carriesAny(n.Flags, hidden) {
				for _, name := range n.Flags {
					if hidden[name] {
						counts[name]++
					}
				}
				n.Text, n.State = "", model.ContentOmitted
			}
			// No call lists a body, and none is served, because a request
			// holds both of the things a reader may withhold.
			n.ProviderBodies = nil
			walk(n.Children)
		}
	}
	walk(v.Talks)
	walk(v.Loose)
	if v.Summary.ProviderBodies > 0 {
		counts["provider_bodies"] = v.Summary.ProviderBodies
	}
	v.Summary.CapturedPrompts = 0
	v.Summary.Withheld = counts
}

// withholdRecord takes the content out of a landed record that carries a
// withheld flag: every part keeps its kind and its size and says omitted, so
// a reader is told how much it is not shown.
func withholdRecord(rec *sessiondata.Record, names []string) {
	hidden := map[string]bool{}
	for _, name := range names {
		hidden[name] = true
	}
	if !carriesAny(rec.Flags, hidden) {
		return
	}
	for i := range rec.Parts {
		p := &rec.Parts[i]
		p.Text, p.Data, p.State = "", nil, model.ContentOmitted
	}
}

func carriesAny(flags []string, hidden map[string]bool) bool {
	for _, name := range flags {
		if hidden[name] {
			return true
		}
	}
	return false
}

// copyDocument gives a reader a document of its own to withhold from. The
// base document is shared by every reader that withholds nothing, and a
// round trip through its own encoding copies every node, child and list
// without a second walk that would have to know each field.
func copyDocument(v *sessionview.Conversation) (*sessionview.Conversation, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	var out sessionview.Conversation
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// headerKind reads the kind a landed file's header names.
func headerKind(data []byte) sessiondata.Kind {
	line, _, _ := bytes.Cut(data, []byte("\n"))
	var hdr sessiondata.Header
	_ = json.Unmarshal(line, &hdr)
	return hdr.Kind
}
