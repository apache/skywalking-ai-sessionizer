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

package claudecode_test

import (
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
)

// Every name the model can put in its output must be in the glossary, even if
// only to say the runtime has no word for it.
//
// The failure this prevents is silent: a reader asks for native terms, meets a
// name with no entry, and is shown the unified one with nothing saying a native
// name was missing rather than absent. Adding a node kind without a term is the
// easy way to cause it, so the check is here rather than in a review.
func TestGlossaryCoversTheWholeVocabulary(t *testing.T) {
	g := claudecode.Glossary()
	var missing []string
	for _, name := range model.Vocabulary() {
		if _, ok := g.Lookup(name); !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d name(s) have no term:\n  %v", len(missing), missing)
	}
}

// A term that names a runtime field must say where to find it, or a reader is
// told a word and given nowhere to look.
func TestNativeTermsSayWhereToLook(t *testing.T) {
	for _, term := range claudecode.Glossary().Terms() {
		if term.Native != "" && term.Where == "" {
			t.Errorf("%q maps to %q but does not say where it appears", term.Unified, term.Native)
		}
		if term.Native == "" && term.Where != "" {
			t.Errorf("%q has no native name but claims a location", term.Unified)
		}
	}
}

// Roughly half of what the model reports is its own work, and a reader should
// be able to see which half. If everything had a native name the model would be
// a rename of one runtime rather than a model of any.
func TestDerivedTermsAreVisiblyDerived(t *testing.T) {
	var derived, native int
	for _, term := range claudecode.Glossary().Terms() {
		if term.Derived() {
			derived++
			if term.Note == "" {
				t.Errorf("%q is derived but says nothing about what it is", term.Unified)
			}
		} else {
			native++
		}
	}
	if derived == 0 || native == 0 {
		t.Fatalf("derived=%d native=%d; expected both", derived, native)
	}
	t.Logf("%d terms: %d named by the runtime, %d derived here", derived+native, native, derived)
}

// Asking for a native name must never return nothing.
func TestNativeFallsBackToTheModelName(t *testing.T) {
	g := claudecode.Glossary()
	if got := g.Native(model.KindTalk); got != model.KindTalk {
		t.Errorf("a derived kind rendered as %q, want the model's own name", got)
	}
	if got := g.Native(model.RoleRun); got != "promptId" {
		t.Errorf("run rendered as %q, want promptId", got)
	}
	if got := g.Native("something-that-does-not-exist"); got != "something-that-does-not-exist" {
		t.Errorf("an unknown name rendered as %q", got)
	}
	var nilG *model.Glossary
	if got := nilG.Native(model.KindTalk); got != model.KindTalk {
		t.Errorf("a nil glossary rendered %q; a reader with no glossary must still get a name", got)
	}
}
