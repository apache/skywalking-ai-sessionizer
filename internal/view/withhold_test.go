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

package view_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario/run"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// A view withholds by the flags the adapter set: for every reader by its
// configuration, and for one reader by the hide parameter, which only adds.
// The scenario prompt-snapshot-withheld checks the document's rules in both
// formats. This is the HTTP layer over it: the parameter, what the record
// endpoint answers, and what the files endpoint serves.
func TestAViewWithholdsByFlag(t *testing.T) {
	out := t.TempDir()
	rep, err := run.Check(filepath.Join("..", "..", "tests", "scenarios", "prompt-snapshot-withheld.yaml"),
		run.Options{Out: out, Formats: []scenario.Format{scenario.FormatClaudeCode}})
	if err != nil || rep.Failed {
		t.Fatalf("building the scenario: %v %v", err, rep.Lines)
	}
	srv := view.New(storage.NewZone(filepath.Join(out, string(scenario.FormatClaudeCode))), nil)
	ids, err := srv.List()
	if err != nil || len(ids) != 1 {
		t.Fatalf("conversations: %v %v", ids, err)
	}
	api := "/api/c/" + ids[0]
	h := srv.Handler()
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	document := func(path string) *sessionview.Conversation {
		t.Helper()
		rec := get(path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
		var doc sessionview.Conversation
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		return &doc
	}
	// The step that carries the prompt, found by its flag and nothing else.
	var find func([]sessionview.Node, string) *sessionview.Node
	find = func(nodes []sessionview.Node, flag string) *sessionview.Node {
		for i := range nodes {
			for _, f := range nodes[i].Flags {
				if f == flag {
					return &nodes[i]
				}
			}
			if n := find(nodes[i].Children, flag); n != nil {
				return n
			}
		}
		return nil
	}

	// Whole: the flags are on the steps, the text with them, and the bodies
	// on the call.
	whole := document(api + "/view")
	if len(whole.Summary.Withheld) != 0 || whole.Summary.CapturedPrompts == 0 || whole.Summary.ProviderBodies == 0 {
		t.Fatalf("a whole document withholds, or holds no body to withhold: %v, captured %d, bodies %d",
			whole.Summary.Withheld, whole.Summary.CapturedPrompts, whole.Summary.ProviderBodies)
	}
	// How many steps carry each name, counted on the whole document, so the
	// counts below follow the scenario rather than restate it.
	prompts, schemas := flagged(whole.Talks, sessiondata.FlagSystemPrompt), flagged(whole.Talks, sessiondata.FlagToolSchemas)
	if prompts == 0 || schemas == 0 {
		t.Fatalf("the scenario names %d prompt and %d schema steps", prompts, schemas)
	}
	shown := find(whole.Talks, sessiondata.FlagSystemPrompt)
	if shown == nil || shown.Text == "" || shown.State != "available" {
		t.Fatalf("the snapshot step is not shown whole: %+v", shown)
	}

	// One reader withholds the prompt: the step stays, its text goes, and
	// the bodies go with it.
	part := document(api + "/view?hide=system_prompt")
	want := map[string]int{"system_prompt": prompts, "provider_bodies": whole.Summary.ProviderBodies}
	if fmt.Sprint(part.Summary.Withheld) != fmt.Sprint(want) || part.Summary.CapturedPrompts != 0 {
		t.Fatalf("withheld %v, captured %d, want %v and 0", part.Summary.Withheld, part.Summary.CapturedPrompts, want)
	}
	hidden := find(part.Talks, sessiondata.FlagSystemPrompt)
	if hidden == nil || hidden.ID != shown.ID || hidden.Text != "" || hidden.State != "omitted" || hidden.Bytes != shown.Bytes {
		t.Fatalf("the withheld step is not kept with its text gone: %+v", hidden)
	}
	// Every model call lists its bodies in the whole document, and none in
	// the withheld one.
	if calls := ofKind(whole.Talks, model.KindLLMCall); len(calls) == 0 || bodiesListed(calls) != len(calls) {
		t.Fatalf("%d model calls, %d listing a body, in the whole document", len(calls), bodiesListed(calls))
	}
	if calls := ofKind(part.Talks, model.KindLLMCall); len(calls) == 0 || bodiesListed(calls) != 0 {
		t.Fatalf("%d of %d model calls list a body while something is withheld", bodiesListed(calls), len(calls))
	}
	if other := find(part.Talks, "external_input"); other == nil || other.Text == "" {
		t.Fatalf("a step carrying no withheld flag lost its text: %+v", other)
	}

	// A withheld document is built from the same fold as the whole one, so
	// a field the encoding leaves out, such as an edge's relation id, is
	// there in both, and building it leaves the whole one as it was.
	c, err := srv.Load(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	base, _ := c.Build()
	withheldDoc, _ := c.BuildWithheld([]string{sessiondata.FlagSystemPrompt})
	if a, b := edgeIDs(base.Talks), edgeIDs(withheldDoc.Talks); a == 0 || a != b {
		t.Fatalf("edges with a relation id: %d in the whole document, %d in the withheld one", a, b)
	}
	if shown.Text == "" || find(base.Talks, sessiondata.FlagSystemPrompt).Text == "" {
		t.Fatal("withholding changed the whole document")
	}

	// The parameter given twice, or once as a list, names the same set.
	twice := document(api + "/view?hide=system_prompt&hide=tool_schemas")
	list := document(api + "/view?hide=tool_schemas,system_prompt")
	if fmt.Sprint(twice.Summary.Withheld) != fmt.Sprint(list.Summary.Withheld) || twice.Summary.Withheld["tool_schemas"] != schemas {
		t.Fatalf("two spellings of one set: %v and %v", twice.Summary.Withheld, list.Summary.Withheld)
	}

	// A name the page cannot withhold is refused, not ignored. So is the
	// parameter spelled another way, and a query that does not parse, since
	// either would otherwise be read as no hide at all.
	for _, query := range []string{"hide=finished", "Hide=system_prompt", "hide[]=system_prompt", "hide%5B0%5D=system_prompt", "hide.0=system_prompt", "hide=%zz", "seq=1;hide=system_prompt"} {
		if rec := get(api + "/view?" + query); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s answered %d", query, rec.Code)
		}
	}
	// A key that only holds the letters inside a word is some other key.
	if rec := get(api + "/view?pushIdentity=x"); rec.Code != http.StatusOK {
		t.Fatalf("pushIdentity answered %d", rec.Code)
	}

	// The record endpoint keeps the envelope and loses the content.
	address := fmt.Sprintf("%s/record/%d/%d", api, shown.Ref.Seq, shown.Ref.Row)
	var rec sessiondata.Record
	if r := get(address); r.Code != http.StatusOK || json.Unmarshal(r.Body.Bytes(), &rec) != nil || len(rec.Parts) != 1 || len(rec.Parts[0].Data) == 0 {
		t.Fatalf("the whole record: %d %s", r.Code, r.Body.String())
	}
	var withheld sessiondata.Record
	if r := get(address + "?hide=system_prompt"); r.Code != http.StatusOK || json.Unmarshal(r.Body.Bytes(), &withheld) != nil || len(withheld.Parts) != 1 {
		t.Fatalf("the withheld record: %d %s", r.Code, r.Body.String())
	}
	if p := withheld.Parts[0]; len(p.Data) != 0 || p.Text != "" || p.State != "omitted" || p.Bytes != rec.Parts[0].Bytes || len(withheld.Flags) == 0 {
		t.Fatalf("the withheld record kept content or lost its envelope: %+v flags %v", p, withheld.Flags)
	}

	// The files endpoint serves provider body files only, and none to a
	// reader that withholds anything.
	var body uint64
	for _, f := range whole.Files {
		if f.Kind == string(sessiondata.KindProviderBody) && f.Seq != nil {
			body = *f.Seq
		}
	}
	if body == 0 {
		t.Fatal("the scenario landed no provider body file")
	}
	if r := get(fmt.Sprintf("%s/files?seq=%d", api, shown.Ref.Seq)); r.Code != http.StatusBadRequest {
		t.Fatalf("a transcript was served whole: %d", r.Code)
	}
	if r := get(fmt.Sprintf("%s/files?seq=%d", api, body)); r.Code != http.StatusOK {
		t.Fatalf("a provider body file was not served: %d %s", r.Code, r.Body.String())
	}
	if r := get(fmt.Sprintf("%s/files?seq=%d&hide=system_prompt", api, body)); r.Code != http.StatusForbidden {
		t.Fatalf("a provider body file was served to a reader that withholds: %d", r.Code)
	}
	// Nor is a body served by its record: a request record holds pieces of
	// the system prompt and the tool schemas, and carries no flag of its own.
	if r := get(fmt.Sprintf("%s/record/%d/1", api, body)); r.Code != http.StatusOK {
		t.Fatalf("a provider body record was not served to a reader that withholds nothing: %d", r.Code)
	}
	if r := get(fmt.Sprintf("%s/record/%d/1?hide=tool_schemas", api, body)); r.Code != http.StatusForbidden {
		t.Fatalf("a provider body record was served to a reader that withholds: %d %.200s", r.Code, r.Body.String())
	}

	// The instance withholds for every reader, and a request adds to that
	// and never takes from it.
	if err := srv.SetHide([]string{sessiondata.FlagToolSchemas}); err != nil {
		t.Fatal(err)
	}
	if doc := document(api + "/view"); doc.Summary.Withheld["tool_schemas"] != schemas || doc.Summary.Withheld["system_prompt"] != 0 {
		t.Fatalf("the instance's own setting: %v", doc.Summary.Withheld)
	}
	if doc := document(api + "/view?hide=system_prompt"); doc.Summary.Withheld["tool_schemas"] != schemas || doc.Summary.Withheld["system_prompt"] != prompts {
		t.Fatalf("a request did not add to the instance's setting: %v", doc.Summary.Withheld)
	}
	if err := srv.SetHide([]string{"finished"}); err == nil {
		t.Fatal("the instance accepted a flag the page cannot withhold")
	}

	// A file whose header does not read is not known to be a provider body,
	// so it is refused rather than served whole. This damages the root, so
	// it comes last.
	var transcript string
	for _, f := range whole.Files {
		if f.Kind == string(sessiondata.KindTranscript) && f.Seq != nil && *f.Seq == shown.Ref.Seq {
			transcript = filepath.Join(out, string(scenario.FormatClaudeCode), filepath.FromSlash(f.File))
		}
	}
	if err := srv.SetHide(nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(transcript, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte("not a header\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := get(fmt.Sprintf("%s/files?seq=%d", api, shown.Ref.Seq)); r.Code != http.StatusBadRequest {
		t.Fatalf("a file whose header does not read answered %d: %.200s", r.Code, r.Body.String())
	}
}

// Readers of different names asking at once each get their own document, the
// same as one built alone.
func TestReadersOfDifferentNamesAreServedTogether(t *testing.T) {
	out := t.TempDir()
	rep, err := run.Check(filepath.Join("..", "..", "tests", "scenarios", "prompt-snapshot-withheld.yaml"),
		run.Options{Out: out, Formats: []scenario.Format{scenario.FormatClaudeCode}})
	if err != nil || rep.Failed {
		t.Fatalf("building the scenario: %v %v", err, rep.Lines)
	}
	zone := storage.NewZone(filepath.Join(out, string(scenario.FormatClaudeCode)))
	sets := [][]string{nil, {sessiondata.FlagSystemPrompt}, {sessiondata.FlagToolSchemas}, {sessiondata.FlagSystemPrompt, sessiondata.FlagToolSchemas}}
	encode := func(c *view.Conversation, names []string) string {
		v, err := c.BuildWithheld(names)
		if err != nil {
			t.Error(err)
			return ""
		}
		b, _ := json.Marshal(v)
		return string(b)
	}
	alone := view.New(zone, nil)
	ids, err := alone.List()
	if err != nil || len(ids) != 1 {
		t.Fatalf("conversations: %v %v", ids, err)
	}
	want := map[int]string{}
	for i, names := range sets {
		c, err := view.New(zone, nil).Load(ids[0])
		if err != nil {
			t.Fatal(err)
		}
		want[i] = encode(c, names)
	}
	c, err := view.New(zone, nil).Load(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 4 {
		for i, names := range sets {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if got := encode(c, names); got != want[i] {
					t.Errorf("hide %v built together differs from one built alone", names)
				}
			}()
		}
	}
	wg.Wait()
}

func edgeIDs(nodes []sessionview.Node) int {
	n := 0
	for _, node := range nodes {
		for _, e := range node.Edges {
			if e.ID != "" {
				n++
			}
		}
		n += edgeIDs(node.Children)
	}
	return n
}

// ofKind collects the steps of one kind.
func ofKind(nodes []sessionview.Node, kind string) []sessionview.Node {
	var out []sessionview.Node
	for _, node := range nodes {
		if node.Kind == kind {
			out = append(out, node)
		}
		out = append(out, ofKind(node.Children, kind)...)
	}
	return out
}

// bodiesListed counts the calls that list a provider body.
func bodiesListed(calls []sessionview.Node) int {
	n := 0
	for _, c := range calls {
		if len(c.ProviderBodies) > 0 {
			n++
		}
	}
	return n
}

// flagged counts the steps that carry a flag.
func flagged(nodes []sessionview.Node, flag string) int {
	n := 0
	for _, node := range nodes {
		for _, f := range node.Flags {
			if f == flag {
				n++
				break
			}
		}
		n += flagged(node.Children, flag)
	}
	return n
}
