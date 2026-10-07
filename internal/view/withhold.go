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
	"fmt"
	"net/url"
	"slices"
	"strings"
	"unicode"

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
// configuration says for every reader. A request adds to that with its hide
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
	if err := sessiondata.CheckWithholdable(names); err != nil {
		return fmt.Errorf("view: hide: %w", err)
	}
	s.hide = withheldNames(names)
	return nil
}

// hideFor is what one request withholds: what the instance withholds, and
// what the request's hide parameter adds, given once or more, each a name or
// a comma-separated list. A name that is not a flag a reader may withhold is
// refused rather than ignored, since withholding by it would withhold nothing
// and the reader would not be told. So is the parameter spelled another way,
// such as Hide, hide[] or hide[0], the forms query libraries write a list
// in, which would otherwise be read as no parameter at all.
func (s *Server) hideFor(q url.Values) ([]string, error) {
	for key := range q {
		if key != "hide" && spelledAsHide(key) {
			return nil, fmt.Errorf("view: %q is not a parameter. The parameter is hide", key)
		}
	}
	names := append([]string(nil), s.hide...)
	for _, value := range q["hide"] {
		for _, name := range strings.Split(value, ",") {
			if name = strings.TrimSpace(name); name != "" {
				names = append(names, name)
			}
		}
	}
	if err := sessiondata.CheckWithholdable(names); err != nil {
		return nil, fmt.Errorf("view: hide: %w", err)
	}
	return withheldNames(names), nil
}

// spelledAsHide reports whether a key is hide however it was spelled: in any
// case or with marks around it. Only its letters are compared, so a key that
// holds the four letters inside a word, such as pushIdentity, is not one.
// Both pages pass such a key on by the same rule, every letter compared.
func spelledAsHide(key string) bool {
	letters := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, key)
	return letters == "hide"
}

// withheldNames is a set of names in one order, so the same set keys the
// same document.
func withheldNames(names []string) []string {
	out := slices.Clone(names)
	slices.Sort(out)
	return slices.Compact(out)
}

// withheldCounts is what a document withholds by name: the session's records
// carrying the name, as the load counted them, whether or not a step is drawn
// for one. Counting only the records a build read missed a record no step
// stands on, which the record endpoint withholds all the same: in a LangChain
// trace whose prompt a decorated function builds, the document said 0 where 2
// records were withheld.
//
// Every withheld name is counted, a zero included. A root landed before the
// adapter set these flags carries none, so withholding there finds nothing. A
// zero says the filter ran and matched nothing, where an empty count would
// read as a document nobody filtered.
func withheldCounts(named map[string]int, names []string) map[string]int {
	counts := make(map[string]int, len(names))
	for _, name := range names {
		counts[name] = named[name]
	}
	return counts
}

// withhold says what a withheld document withheld. No call lists a body,
// since a request carries both things a reader may withhold, and none is
// served. The provider bodies are counted beside the names.
func withhold(v *sessionview.Conversation, counts map[string]int) {
	var walk func([]sessionview.Node)
	walk = func(nodes []sessionview.Node) {
		for i := range nodes {
			nodes[i].ProviderBodies = nil
			walk(nodes[i].Children)
		}
	}
	walk(v.Talks)
	walk(v.Loose)
	counts[withheldBodies] = v.Summary.ProviderBodies
	v.Summary.CapturedPrompts = 0
	v.Summary.Withheld = counts
}

// markWithheld applies the rule to one step read from a withheld record: no
// text, the state omitted, and a size. A step that names one part already has
// that part's size. One that names no single part of a record with several
// has none of its own, and takes the size of all the parts, so a withheld
// step never says it withheld nothing.
func markWithheld(s *step, rec *sessiondata.Record, hidden map[string]bool) {
	if !carriesAny(rec.Flags, hidden) {
		return
	}
	s.Text, s.State = "", model.ContentOmitted
	if s.Bytes == 0 {
		for _, p := range rec.Parts {
			s.Bytes += p.Bytes
		}
	}
}

// withheldBodies is the name summary.withheld counts the provider bodies
// under: every landed body of the session, since none is served to a reader
// that withholds anything.
const withheldBodies = "provider_bodies"

// withholdRecord takes the content out of a landed record that carries a
// withheld flag: every part keeps its kind and its size and says omitted, so
// a reader is told how much it is not shown.
func withholdRecord(rec *sessiondata.Record, hidden map[string]bool) {
	if len(hidden) == 0 || !carriesAny(rec.Flags, hidden) {
		return
	}
	for i := range rec.Parts {
		p := &rec.Parts[i]
		// Encoding says how data holds bytes, and there is no data left.
		p.Text, p.Data, p.Encoding, p.State = "", nil, "", model.ContentOmitted
	}
}

// hiddenSet is a set of withheld names, for asking whether a flag is one.
// It is nil when nothing is withheld, so a read that withholds nothing
// allocates nothing.
func hiddenSet(names []string) map[string]bool {
	if len(names) == 0 {
		return nil
	}
	hidden := make(map[string]bool, len(names))
	for _, name := range names {
		hidden[name] = true
	}
	return hidden
}

func carriesAny(flags []string, hidden map[string]bool) bool {
	return slices.ContainsFunc(flags, func(name string) bool { return hidden[name] })
}

// errBodiesWithheld refuses a provider body to a reader that withholds
// anything. A request carries the system prompt and the tool schemas again,
// with no flag of its own, and the renderer checks a body's digest, so a body
// is served whole or not at all, by file or by record.
func errBodiesWithheld(hide []string) error {
	return fmt.Errorf("view: the provider bodies are withheld with %s: a request carries what is withheld, and a body is served whole or not at all", strings.Join(hide, " and "))
}

// withholdable reports whether a record carries a flag a reader may withhold.
func withholdable(flags []string) bool { return slices.ContainsFunc(flags, sessiondata.IsWithholdable) }
