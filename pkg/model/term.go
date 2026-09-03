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

package model

import "sort"

// A Term is what one runtime calls something the model has its own name for.
//
// The model is deliberately runtime-agnostic, and that is right for storing and
// reasoning about a conversation. It is not always right for showing one: a
// person debugging their own session recognises the words their agent product
// uses, and telling them a "run" when their transcript says `promptId` makes
// them translate in their head.
//
// So both are kept, side by side, and a reader chooses. Nothing is lost either
// way, and neither name is presented as the other.
type Term struct {
	// Unified is the model's name - a node kind, a relation type, a role, or a
	// qualification value.
	Unified string
	// Native is what the runtime calls it. EMPTY means the runtime has no name
	// for this, which is a real and common answer: a Talk, a Segment and a
	// correlation quality are things this project derived, and no agent product
	// records them.
	Native string
	// Where says where the native name appears in the runtime's own data, so a
	// reader can go and look. Empty when Native is empty.
	Where string
	// Note carries anything a reader needs in order not to misread the pairing -
	// most often that the two are close but not identical.
	Note string
}

// Derived reports whether this is something the model derived rather than
// something the runtime recorded.
func (t Term) Derived() bool { return t.Native == "" }

// Glossary is one runtime's whole vocabulary, keyed by the model's name.
type Glossary struct {
	// Dialect names the runtime and the version of this mapping, e.g.
	// "claude-code/1". A reader shows native terms only for data produced by a
	// dialect it has a glossary for.
	Dialect string
	terms   map[string]Term
}

// NewGlossary builds a glossary from a term list.
func NewGlossary(dialect string, terms ...Term) *Glossary {
	g := &Glossary{Dialect: dialect, terms: make(map[string]Term, len(terms))}
	for _, t := range terms {
		g.terms[t.Unified] = t
	}
	return g
}

// Lookup returns the term for a unified name.
func (g *Glossary) Lookup(unified string) (Term, bool) {
	if g == nil {
		return Term{}, false
	}
	t, ok := g.terms[unified]
	return t, ok
}

// Native returns what the runtime calls something.
//
// It falls back to the unified name rather than to nothing, so a caller that
// asks for native names still gets a usable string for the things the runtime
// has no word for. That is the honest default: showing "talk" where the runtime
// says nothing is better than showing a blank, and better than inventing a
// plausible-looking native term.
func (g *Glossary) Native(unified string) string {
	if t, ok := g.Lookup(unified); ok && t.Native != "" {
		return t.Native
	}
	return unified
}

// Terms returns the whole glossary, ordered by the model's name.
func (g *Glossary) Terms() []Term {
	if g == nil {
		return nil
	}
	out := make([]Term, 0, len(g.terms))
	for _, t := range g.terms {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Unified < out[j].Unified })
	return out
}

// Vocabulary is every name the model can put in its output.
//
// It exists so a glossary can be checked for completeness: anything here with
// no entry is a term a reader could meet and not be able to translate. A test
// enforces that, because the failure is silent - the reader simply sees the
// unified name and never learns there was a native one.
func Vocabulary() []string {
	return []string{
		// structure
		KindConversation, KindSegment, KindSession, KindStream, KindEpoch,
		KindTalk, KindRun,
		// steps
		KindMessageExternal, KindMessageAssistant, KindMessageSynthetic,
		KindContextInjection, KindLLMCall, KindThinking, KindTool,
		KindAgentCall, KindAgentLaunchAck, KindAgentOutput, KindRuntimeNotification,
		KindEpochBoundary, KindEpochSummary,
		KindErrorAPI, KindControlInterrupt, KindControlPermission, KindControlCommand,
		KindTurnDuration,
		// relations
		RelStarts, RelReports, RelEndsWith, RelResultOf, RelFollows, RelSummarizes,
		RelInSegment, RelRetries, RelCancels, RelInputOf,
		// the roles a record plays, which a reader meets in a reference
		RoleID, RoleParent, RoleCall, RoleRun, RoleBatch, RoleStream,
		RoleContinues, RoleTool, RoleChild, RoleTime, RoleTrigger,
		// qualification
		ExactUnique, ExactAmbiguous, StrongInference, WeakInference, Unresolved, Conflict,
		ContentAvailable, ContentRedacted, ContentOmitted, ContentHashOnly,
		ContentSizeOnly, ContentTruncated, ContentUnavailable,
		TriggerExternal, TriggerNotification, TriggerUnknown,
	}
}

// Roles a record plays. These are the names the index and the details format
// use for a record's identifiers, and a reader meets them whenever it is shown
// where something came from.
const (
	RoleID        = "id"
	RoleParent    = "parent"
	RoleCall      = "call"
	RoleRun       = "run"
	RoleBatch     = "batch"
	RoleStream    = "stream"
	RoleContinues = "continues"
	RoleTool      = "tool"
	RoleChild     = "child"
	RoleTime      = "time"
	RoleTrigger   = "trigger"
)
