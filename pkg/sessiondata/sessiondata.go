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

// Package sessiondata defines Session Data: what was actually in a
// conversation, in one shape regardless of which agent produced it.
//
// A landed file has the extension .sd. Its first line is a header carrying
// everything constant for the file; every line after it is one record, and a
// record's content is broken into PARTS - a message, a thought, a call, its
// result - using names no runtime owns.
//
// The conversion happens once, while the source is read, and never again. That
// is what keeps a runtime's vocabulary out of everything above: a reader is
// handed parts, so there is no runtime shape for it to reach into. A dialect
// that meets something it cannot describe keeps the bytes verbatim in an
// `unknown` part rather than guessing or dropping them.
package sessiondata

import (
	"encoding/json"
	"fmt"
)

// Schema is the format version, carried in every file's header.
const Schema = "sd/1"

// Ext is the file extension.
const Ext = ".sd"

// Kind identifies what a file was collected from.
type Kind string

const (
	KindTranscript       Kind = "transcript"
	KindAgentMeta        Kind = "agent_meta"
	KindJournal          Kind = "journal"
	KindWorkflowManifest Kind = "workflow_manifest"
	KindWorkflowScript   Kind = "workflow_script"
	KindOTLPLog          Kind = "otlp_log"  // reserved, a push transport
	KindOTLPSpan         Kind = "otlp_span" // reserved, a push transport
	KindProviderBody     Kind = "provider_body"
)

// PartKind names what a piece of content IS.
//
// The set is small because the thing being described is small: measured across
// a corpus of 3,032 files, six content shapes exist and four of them cover
// 99.99%. A message, a thought, a call, its result, and an attachment is the
// whole of what an agent does.
type PartKind string

const (
	// PartText is readable text.
	PartText PartKind = "text"
	// PartReasoning is the model's own reasoning.
	PartReasoning PartKind = "reasoning"
	// PartCall is a request to run something: a tool, a function, a skill.
	PartCall PartKind = "call"
	// PartResult is what a call returned.
	PartResult PartKind = "result"
	// PartMedia is an image or a document.
	PartMedia PartKind = "media"
	// PartData is structured content that is not prose: a record the runtime
	// keeps for its own purposes, an attachment whose shape is a set of fields
	// rather than a sentence, a manifest.
	//
	// It is distinct from PartUnknown because the difference matters to a
	// reader: this is content we recognised and could not render as a sentence,
	// not content we failed to recognise at all.
	PartData PartKind = "data"
	// PartUnknown is content the dialect could not describe.
	//
	// It keeps the bytes. A dialect that meets a shape it does not recognise
	// must not guess and must not drop, so the raw form travels and a later
	// version of that dialect can interpret it without re-collecting.
	PartUnknown PartKind = "unknown"
)

// From says who produced a record.
type From string

const (
	// FromAgent means the model produced it.
	FromAgent From = "agent"
	// FromExternal means it came from outside the agent: a person, a tool
	// returning, the runtime reporting that a child finished.
	FromExternal From = "external"
	// FromRuntime means the harness produced it: a reset boundary, an error, a
	// sidecar, a journal.
	FromRuntime From = "runtime"
)

// Part is one piece of a record's content.
type Part struct {
	Kind PartKind `json:"k"`

	// Text is the readable text, for a message, a thought, or a result.
	Text string `json:"text,omitempty"`
	// Data is structure a reader may want but cannot read as prose: a call's
	// input, a result's parsed form, the raw bytes of an unknown part.
	Data json.RawMessage `json:"data,omitempty"`

	// ID is a call's own identifier; Of is the call a result belongs to.
	ID string `json:"id,omitempty"`
	Of string `json:"of,omitempty"`
	// Name is what was called.
	Name string `json:"name,omitempty"`
	// Failed reports whether a call returned an error, when the runtime said so.
	//
	// It is a pointer because absence and false are different answers: most
	// results carry no error flag at all, and reading that as success would
	// report something the runtime never claimed.
	Failed *bool `json:"failed,omitempty"`
	// Media is the type of an image or document, e.g. "image/png".
	Media string `json:"media,omitempty"`

	// State says how much of the original is here: available, truncated,
	// redacted, omitted or unavailable. A reader is always told.
	State string `json:"state,omitempty"`
	// Bytes is the size of the original, even when this part holds less of it.
	Bytes int `json:"bytes,omitempty"`
}

// Header is the first line of a .sd file.
type Header struct {
	H      int    `json:"h"`
	Schema string `json:"schema"`

	Seq  uint64 `json:"seq"`
	At   string `json:"at"`   // when it was collected, RFC3339 with nanoseconds
	Kind Kind   `json:"kind"` // what it was collected from

	// Adapter says how the records were acquired; Dialect says whose schema
	// they were read as. A push receiver and a local reader for one runtime
	// share a dialect and nothing else.
	Adapter string `json:"adapter"`
	Dialect string `json:"dialect"`

	Src     string `json:"src"` // the source, relative to the adapter's root
	Session string `json:"session"`
	Stream  string `json:"stream,omitempty"`
	Batch   string `json:"batch,omitempty"`
}

// Record is one source record, converted.
type Record struct {
	// Ord and Off locate the record in the SOURCE: its line number and its byte
	// offset. Sha is the digest of the source bytes.
	//
	// The bytes themselves are not kept - the parts below are what a reader
	// wants, and the rest of a raw record is envelope. The digest stays because
	// provenance is still provable without them: two collectors reading the same
	// source record produce the same digest, and a record that claims a source
	// it did not come from is detectable.
	Ord   uint64 `json:"ord"`
	Off   uint64 `json:"off"`
	Sha   string `json:"sha"`
	Bytes int    `json:"bytes"` // the size of the source record

	// Role-named identifiers. A runtime's own field names stop at its adapter;
	// these are what everything above sees. Empty means the runtime did not
	// supply one, which is common.
	ID        string `json:"id,omitempty"`
	Parent    string `json:"parent,omitempty"`
	Call      string `json:"call,omitempty"`
	Run       string `json:"run,omitempty"`
	Continues string `json:"continues,omitempty"`
	Tool      string `json:"tool,omitempty"`
	Child     string `json:"child,omitempty"`
	// Batch is the group of children this record names or belongs to. A launch
	// result carries it even though the file it sits in does not, which is what
	// connects a parent's call to the children it started.
	Batch string `json:"batch,omitempty"`
	// Label is a human-readable name the runtime gave something: a title for the
	// conversation, a description of what a child agent was asked to do, the
	// name of a workflow.
	//
	// It is the only naming evidence anywhere in the data. Everything else a
	// reader could use to tell one talk from another has to be derived.
	Label string `json:"label,omitempty"`
	// StartedBy is the stream that started the one this record belongs to.
	//
	// It matters only for a child of a child. Without it a nested child is
	// attached to the session's parent lineage instead of to the agent that
	// actually started it, which flattens the nesting silently.
	StartedBy string `json:"started_by,omitempty"`

	// From says who produced the record. A record's own type is not what a
	// record is - most records that look like a person are a tool answering -
	// so this states the producer rather than a label.
	From From `json:"from,omitempty"`

	Time    string   `json:"time,omitempty"`
	Trigger string   `json:"trigger,omitempty"`
	Flags   []string `json:"flags,omitempty"`

	// Usage is what the provider reported about a call, on the records that
	// carry it. Only meaningful where the call finished - an unfinished call
	// still carries a usage block, and its output count is a streaming stub of
	// a few tokens.
	Usage *Usage `json:"usage,omitempty"`

	Parts []Part `json:"parts,omitempty"`

	// Dropped names what the conversion deliberately discarded, and how many
	// bytes went with it.
	//
	// It exists so the loss is stated rather than silent. Everything the dialect
	// did not understand is kept as an unknown part; this is only for what it
	// understood and chose not to carry - a provider's verification signature,
	// for instance, which no reader can use.
	Dropped []Drop `json:"dropped,omitempty"`
}

// Usage is what a provider reported it spent.
type Usage struct {
	Input      int `json:"in,omitempty"`
	Output     int `json:"out,omitempty"`
	CacheRead  int `json:"cache_read,omitempty"`
	CacheWrite int `json:"cache_write,omitempty"`
}

// Drop is one thing the conversion left behind.
type Drop struct {
	What  string `json:"what"`
	Bytes int    `json:"bytes"`
	Why   string `json:"why,omitempty"`
}

// Validate reports a header that cannot be acted on.
func (h *Header) Validate() error {
	switch {
	case h.H != 1:
		return fmt.Errorf("sessiondata: unsupported envelope version %d", h.H)
	case h.Schema != Schema:
		return fmt.Errorf("sessiondata: unsupported schema %q, want %q", h.Schema, Schema)
	case h.Kind == "":
		return fmt.Errorf("sessiondata: header missing kind")
	case h.Session == "":
		return fmt.Errorf("sessiondata: header missing session")
	case h.Src == "":
		return fmt.Errorf("sessiondata: header missing src")
	case h.Dialect == "":
		// Without it, nothing can say whose vocabulary the parts were read as,
		// and a later reader cannot tell a shape it should understand from one
		// it should not.
		return fmt.Errorf("sessiondata: header missing dialect")
	}
	return nil
}

// Text returns a record's readable text, parts joined in order.
func (r *Record) Text() string {
	var out string
	for _, p := range r.Parts {
		if p.Kind == PartText && p.Text != "" {
			if out != "" {
				out += "\n"
			}
			out += p.Text
		}
	}
	return out
}
