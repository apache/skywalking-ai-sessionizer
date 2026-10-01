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
	"path/filepath"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario/run"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
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

	// Whole: the flags are on the steps, the text with them, the bodies on the call.
	whole := document(api + "/view")
	if len(whole.Summary.Withheld) != 0 || whole.Summary.CapturedPrompts != 1 {
		t.Fatalf("a whole document withholds: %v, captured %d", whole.Summary.Withheld, whole.Summary.CapturedPrompts)
	}
	shown := find(whole.Talks, sessiondata.FlagSystemPrompt)
	if shown == nil || shown.Text == "" || shown.State != "available" {
		t.Fatalf("the snapshot step is not shown whole: %+v", shown)
	}

	// One reader withholds the prompt: the step stays, its text goes, and
	// the bodies go with it.
	part := document(api + "/view?hide=system_prompt")
	want := map[string]int{"system_prompt": 2, "provider_bodies": 2}
	if fmt.Sprint(part.Summary.Withheld) != fmt.Sprint(want) || part.Summary.CapturedPrompts != 0 {
		t.Fatalf("withheld %v, captured %d; want %v and 0", part.Summary.Withheld, part.Summary.CapturedPrompts, want)
	}
	hidden := find(part.Talks, sessiondata.FlagSystemPrompt)
	if hidden == nil || hidden.ID != shown.ID || hidden.Text != "" || hidden.State != "omitted" || hidden.Bytes != shown.Bytes {
		t.Fatalf("the withheld step is not kept with its text gone: %+v", hidden)
	}
	if call := find(part.Talks, "finished"); call == nil || len(call.ProviderBodies) != 0 {
		t.Fatalf("a call still lists bodies while something is withheld: %+v", call)
	}
	if other := find(part.Talks, "external_input"); other == nil || other.Text == "" {
		t.Fatalf("a step carrying no withheld flag lost its text: %+v", other)
	}

	// The parameter given twice, or once as a list, names the same set.
	twice := document(api + "/view?hide=system_prompt&hide=tool_schemas")
	list := document(api + "/view?hide=tool_schemas,system_prompt")
	if fmt.Sprint(twice.Summary.Withheld) != fmt.Sprint(list.Summary.Withheld) || twice.Summary.Withheld["tool_schemas"] != 1 {
		t.Fatalf("two spellings of one set: %v and %v", twice.Summary.Withheld, list.Summary.Withheld)
	}

	// A name the page cannot withhold is refused, not ignored.
	if rec := get(api + "/view?hide=finished"); rec.Code != http.StatusBadRequest {
		t.Fatalf("hide=finished answered %d", rec.Code)
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

	// The instance withholds for every reader, and a request adds to that
	// and never takes from it.
	if err := srv.SetHide([]string{sessiondata.FlagToolSchemas}); err != nil {
		t.Fatal(err)
	}
	if doc := document(api + "/view"); doc.Summary.Withheld["tool_schemas"] != 1 || doc.Summary.Withheld["system_prompt"] != 0 {
		t.Fatalf("the instance's own setting: %v", doc.Summary.Withheld)
	}
	if doc := document(api + "/view?hide=system_prompt"); doc.Summary.Withheld["tool_schemas"] != 1 || doc.Summary.Withheld["system_prompt"] != 2 {
		t.Fatalf("a request did not add to the instance's setting: %v", doc.Summary.Withheld)
	}
	if err := srv.SetHide([]string{"finished"}); err == nil {
		t.Fatal("the instance accepted a flag the page cannot withhold")
	}
}
