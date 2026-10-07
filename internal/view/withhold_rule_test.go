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
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// The scenarios reach these rules only through records of one part, the shape
// a named Claude Code attachment lands in. A named LangChain record can have
// several, and the rules hold for any shape, so they are checked here on what
// the view builds.

// A record carrying a withheld name is withheld as it is read, before any
// field is taken from it. Every field a document reads from a record, a
// tool's result or a talk's label as much as a step's text, then follows the
// rule. A record carrying no withheld name is read whole, and so is one
// carrying a name this reader does not withhold.
func TestARecordIsWithheldAsItIsRead(t *testing.T) {
	const session = "0b6c8f8e-7d0e-4c55-9a43-2f0b1c9d3e21"
	z := storage.NewZone(t.TempDir())
	dir := filepath.Join(z.SessionDir(session), "streams", "main")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, "transcript-20260101T000000.000000000Z-000001.sd"))
	if err != nil {
		t.Fatal(err)
	}
	w, err := sessiondata.NewWriter(f, &sessiondata.Header{Seq: 1, At: "2026-01-01T00:00:00Z", Kind: sessiondata.KindTranscript,
		Adapter: "test/1", Dialect: "test", Src: "main.jsonl", Session: session, Stream: "main"})
	if err != nil {
		t.Fatal(err)
	}
	text := func(s string) sessiondata.Part {
		return sessiondata.Part{Kind: sessiondata.PartText, Text: s, State: "available", Bytes: len(s)}
	}
	for _, rec := range []*sessiondata.Record{
		{Ord: 1, Flags: []string{"injected", sessiondata.FlagSystemPrompt}, Parts: []sessiondata.Part{text("You are a helpful assistant."), text("Use the tools when they help.")}},
		{Ord: 2, Flags: []string{"external_input"}, Parts: []sessiondata.Part{text("summarise what changed in the docs")}},
		{Ord: 3, Flags: []string{"injected", sessiondata.FlagToolSchemas}, Parts: []sessiondata.Part{{Kind: sessiondata.PartData,
			Data: json.RawMessage(`{"tools":[{"name":"Read"}]}`), State: "available", Bytes: 27}}},
	} {
		if err := w.Write(rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	c := &Conversation{Session: session, zone: z}
	refs := []*sessionflow.Ref{{Seq: 1, Row: 1}, {Seq: 1, Row: 2}, {Seq: 1, Row: 3}}
	hide := []string{sessiondata.FlagSystemPrompt}

	recs := c.records(refs, hide)
	for _, p := range recs[[2]uint64{1, 1}].Parts {
		if p.Text != "" || p.State != "omitted" || p.Bytes == 0 {
			t.Fatalf("a part of the withheld record: %+v", p)
		}
	}
	if got := recs[[2]uint64{1, 2}].Parts[0]; got.Text != "summarise what changed in the docs" || got.State != "available" {
		t.Fatalf("a record carrying no withheld name: %+v", got)
	}
	if got := recs[[2]uint64{1, 3}].Parts[0]; len(got.Data) == 0 || got.State != "available" {
		t.Fatalf("a record carrying a name this reader does not withhold: %+v", got)
	}

	// The text a label is read from follows the same rule, and says which
	// records are what the runtime sent the model, for any reader.
	texts, sent := c.texts(refs, hide)
	if texts[[2]uint64{1, 1}] != "" || texts[[2]uint64{1, 2}] != "summarise what changed in the docs" {
		t.Fatalf("texts: %v", texts)
	}
	if !sent[[2]uint64{1, 1}] || sent[[2]uint64{1, 2}] || !sent[[2]uint64{1, 3}] {
		t.Fatalf("sent: %v", sent)
	}
	if whole, _ := c.texts(refs, nil); whole[[2]uint64{1, 1}] == "" {
		t.Fatal("a reader that withholds nothing was not shown the prompt")
	}

	// A step is filled from the record as it was read. One that names no
	// single part of a record with several takes the size of all of them.
	// A model call carries no content, but its own record is read for the
	// flags a reader may withhold, though no step under it stands there.
	injection := &sessionflow.Node{Entity: sessionflow.Entity{ID: "context/1"}, Kind: model.KindContextInjection, Ref: &sessionflow.Ref{Seq: 1, Row: 1}}
	call := &sessionflow.Node{Entity: sessionflow.Entity{ID: "call/1"}, Kind: model.KindLLMCall, Ref: &sessionflow.Ref{Seq: 1, Row: 1}}
	c.View = &sessionflow.View{Nodes: map[string]*sessionflow.Node{injection.ID: injection, call.ID: call},
		Relations: map[string]*sessionflow.Relation{}}
	size := len("You are a helpful assistant.") + len("Use the tools when they help.")
	if got := c.step(injection, 0, recs, hiddenSet(hide)); got.Text != "" || got.State != "omitted" || got.Bytes != size {
		t.Fatalf("a step on the withheld record: text %q, state %q, %d bytes, want %d", got.Text, got.State, got.Bytes, size)
	}
	got := c.step(call, 0, c.records(c.refsUnder(call), hide), hiddenSet(hide))
	if !slices.Equal(got.Flags, []string{sessiondata.FlagSystemPrompt}) || got.State != "omitted" {
		t.Fatalf("a model call on the withheld record: flags %v, state %q", got.Flags, got.State)
	}
}

// A label or a reply read from a record a reader withholds is withheld from
// that reader, and shown to one that withholds nothing: a person's input
// names its talk whatever it carries.
func TestALabelReadFromAWithheldRecordIsWithheld(t *testing.T) {
	const session = "6e5d4c3b-2a19-4f08-b7e6-d5c4b3a29180"
	z := storage.NewZone(t.TempDir())
	dir := filepath.Join(z.SessionDir(session), "streams", "main")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, "transcript-20260101T000000.000000000Z-000001.sd"))
	if err != nil {
		t.Fatal(err)
	}
	w, err := sessiondata.NewWriter(f, &sessiondata.Header{Seq: 1, At: "2026-01-01T00:00:00Z", Kind: sessiondata.KindTranscript,
		Adapter: "test/1", Dialect: "test", Src: "main.jsonl", Session: session, Stream: "main"})
	if err != nil {
		t.Fatal(err)
	}
	text := func(s string) []sessiondata.Part {
		return []sessiondata.Part{{Kind: sessiondata.PartText, Text: s, State: "available", Bytes: len(s)}}
	}
	for _, rec := range []*sessiondata.Record{
		{Ord: 1, Flags: []string{"external_input", sessiondata.FlagSystemPrompt}, Parts: text("You check links.")},
		{Ord: 2, Flags: []string{sessiondata.FlagSystemPrompt}, Parts: text("Report what you find.")},
	} {
		if err := w.Write(rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	nodes := map[string]*sessionflow.Node{
		"talk/1":    {Entity: sessionflow.Entity{ID: "talk/1"}, Kind: model.KindTalk, Stream: "main"},
		"message/1": {Entity: sessionflow.Entity{ID: "message/1"}, Kind: model.KindMessageExternal, Parent: "talk/1", Stream: "main", Ref: &sessionflow.Ref{Seq: 1, Row: 1}},
		"message/2": {Entity: sessionflow.Entity{ID: "message/2"}, Kind: model.KindMessageAssistant, Parent: "talk/1", Stream: "main", Ref: &sessionflow.Ref{Seq: 1, Row: 2}},
	}
	c := &Conversation{Session: session, zone: z, View: &sessionflow.View{Nodes: nodes, Relations: map[string]*sessionflow.Relation{}}}
	talk := func(hide []string) sessionview.Node {
		t.Helper()
		v, err := c.BuildWithheld(hide)
		if err != nil || len(v.Talks) != 1 {
			t.Fatalf("%v %d talks", err, len(v.Talks))
		}
		return v.Talks[0]
	}
	if got := talk(nil); got.Label != "You check links." || got.Reply != "Report what you find." {
		t.Fatalf("for a reader that withholds nothing, the talk is named %q and answered %q", got.Label, got.Reply)
	}
	if got := talk([]string{sessiondata.FlagSystemPrompt}); got.Label != "" || got.Reply != "" {
		t.Fatalf("for a reader that withholds the prompt, the talk is named %q and answered %q", got.Label, got.Reply)
	}
}

// A step read from a withheld record loses its text and says omitted. One
// that names one part keeps that part's size. One that names no single part
// of a record with several has no size of its own, and takes the size of all
// of them, so a withheld step never says it withheld nothing.
func TestAWithheldStepKeepsItsSize(t *testing.T) {
	hidden := hiddenSet([]string{sessiondata.FlagSystemPrompt})
	rec := &sessiondata.Record{Flags: []string{"injected", sessiondata.FlagSystemPrompt},
		Parts: []sessiondata.Part{{Kind: sessiondata.PartText, Bytes: 25}, {Kind: sessiondata.PartData, Bytes: 15}}}
	one := step{Text: "a prompt", Bytes: 8}
	markWithheld(&one, rec, hidden)
	several := step{Text: "a prompt and more"}
	markWithheld(&several, rec, hidden)
	if one.Bytes != 8 || several.Bytes != 40 || one.Text != "" || several.Text != "" || several.State != "omitted" {
		t.Fatalf("sizes after withholding: %+v %+v", one, several)
	}
	other := step{Text: "kept", Bytes: 4}
	markWithheld(&other, &sessiondata.Record{Flags: []string{"injected"}}, hidden)
	if other.Text != "kept" || other.State != "" {
		t.Fatalf("a step carrying no withheld flag changed: %+v", other)
	}
}

// A name counts what the load counted over the landed files. The scenario
// prompt-snapshot-withheld checks that a replayed record counts once, and
// the LangChain tests check records no step stands on, and a call and the
// error it became, which share one record. Here: a name nothing carries
// counts zero, a flag not asked for is not listed, and the provider bodies
// are counted beside the names.
func TestWithheldRecordsAreCounted(t *testing.T) {
	named := map[string]int{sessiondata.FlagSystemPrompt: 2, "injected": 5}
	got := withheldCounts(named, []string{sessiondata.FlagSystemPrompt, sessiondata.FlagToolSchemas})
	if got[sessiondata.FlagSystemPrompt] != 2 || got[sessiondata.FlagToolSchemas] != 0 || len(got) != 2 {
		t.Fatalf("withheld %v, want two records under system_prompt and a zero under tool_schemas", got)
	}
	v := &sessionview.Conversation{Summary: sessionview.Summary{ProviderBodies: 3, CapturedPrompts: 2},
		Talks: []sessionview.Node{{ID: "call", ProviderBodies: []sessionview.ProviderBody{{Role: "request"}}}}}
	withhold(v, got)
	if v.Summary.Withheld["provider_bodies"] != 3 || v.Summary.CapturedPrompts != 0 || v.Talks[0].ProviderBodies != nil {
		t.Fatalf("the summary after withholding: %+v, the call lists %v", v.Summary, v.Talks[0].ProviderBodies)
	}
}

// A withheld part keeps its kind and its size, and nothing that describes
// bytes it no longer has.
func TestAWithheldPartKeepsNoEncoding(t *testing.T) {
	rec := &sessiondata.Record{Flags: []string{sessiondata.FlagToolSchemas}}
	part := sessiondata.Part{Kind: sessiondata.PartUnknown, Text: "not described", State: "available", Bytes: 3}
	part.SetRaw([]byte{0xff, 0xfe, 0x00})
	if part.Encoding != sessiondata.EncodingBase64 {
		t.Fatalf("the part is not base64: %q", part.Encoding)
	}
	rec.Parts = []sessiondata.Part{part}
	withholdRecord(rec, hiddenSet([]string{sessiondata.FlagToolSchemas}))
	if p := rec.Parts[0]; p.Encoding != "" || len(p.Data) != 0 || p.Text != "" || p.State != "omitted" || p.Bytes != 3 || p.Kind != sessiondata.PartUnknown {
		t.Fatalf("the withheld part: %+v", p)
	}
}

// A run journal names a child stream from a result row it carries. A record
// that also carries what the runtime sent the model names no stream, since the
// names are made once for every reader.
func TestAJournalRecordThatIsWithheldNamesNoStream(t *testing.T) {
	row := sessiondata.Part{Kind: sessiondata.PartData, State: "available",
		Data: json.RawMessage(`{"type":"result","result":{"summary":"links checked"}}`)}
	plain := &sessiondata.Record{Child: "agent-1", Parts: []sessiondata.Part{row}}
	if got := journalName(plain); got != "links checked" {
		t.Fatalf("a journal record names its child %q", got)
	}
	named := &sessiondata.Record{Child: "agent-1", Flags: []string{sessiondata.FlagSystemPrompt}, Parts: []sessiondata.Part{row}}
	if got := journalName(named); got != "" {
		t.Fatalf("a record carrying system_prompt names its child %q", got)
	}
}
