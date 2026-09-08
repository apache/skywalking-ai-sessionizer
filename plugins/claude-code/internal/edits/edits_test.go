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

package edits_test

import (
	"encoding/json"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/pkg/changes"
	"github.com/apache/skywalking-ai-sessionizer/plugins/claude-code/internal/edits"
)

// The responses below are the shapes a run of Claude Code 2.1.260 handed
// a logging hook, with the paths and the text replaced.

const editResponse = `{"filePath":"/work/project/poc.txt","oldString":"alpha","newString":"beta","originalFile":"line one alpha\nline two\n","structuredPatch":[{"oldStart":1,"oldLines":2,"newStart":1,"newLines":2,"lines":["-line one alpha","+line one beta"," line two"]}],"userModified":false,"replaceAll":false}`

const writeResponse = `{"type":"create","filePath":"/work/project/sub.txt","content":"from subagent","structuredPatch":[],"originalFile":null,"userModified":false}`

func TestEditResponseBecomesARecord(t *testing.T) {
	rec, ok := edits.Record(edits.Context{ID: "plugin/toolu_1", Session: "S", Stream: "a1", Tool: "toolu_1", ToolName: "Edit", Time: "2026-09-08T10:00:00Z", Root: "/work/project"}, json.RawMessage(editResponse))
	if !ok {
		t.Fatal("no record")
	}
	if err := rec.Validate(); err != nil {
		t.Fatal(err)
	}
	if rec.Basis != changes.BasisRuntimeReported || rec.Stream != "a1" || rec.Root.Path != "/work/project" || *rec.ChangedFiles != 1 {
		t.Fatalf("record: %+v", rec)
	}
	c := rec.Changes[0]
	if c.Path != "poc.txt" || c.Operation != changes.OpModify || len(c.Hunks) != 1 || c.Hunks[0].Lines[1] != "+line one beta" {
		t.Fatalf("change: %+v", c)
	}
	if *c.Additions != 1 || *c.Deletions != 1 || !c.Before.Present || *c.Before.Bytes != 24 || !c.After.Present || *c.After.Bytes != 23 {
		t.Fatalf("counts and endpoints: %+v", c)
	}
	if c.Before.SHA256 == c.After.SHA256 || c.After.SHA256 == "" {
		t.Fatal("the after content was not derived from the edit")
	}
}

func TestWriteOfANewFileIsACreate(t *testing.T) {
	rec, ok := edits.Record(edits.Context{ID: "plugin/toolu_2", Session: "S", Stream: "a1", Tool: "toolu_2", ToolName: "Write", Time: "t", Root: "/work/project"}, json.RawMessage(writeResponse))
	if !ok {
		t.Fatal("no record")
	}
	c := rec.Changes[0]
	if c.Path != "sub.txt" || c.Operation != changes.OpCreate || c.Before.Present || !c.After.Present || !c.After.NoNewlineAtEnd {
		t.Fatalf("change: %+v", c)
	}
	// The runtime wrote an empty patch for a new file; the hunks come from
	// the content.
	if len(c.Hunks) != 1 || c.Hunks[0].Lines[0] != "+from subagent" || *c.Additions != 1 {
		t.Fatalf("hunks: %+v", c.Hunks)
	}
}

// The runtime's patch for a Write over an existing file ends with the
// git marker for a missing final newline. It is the runtime's line and
// travels as written; the endpoint says the same in its own field.
const writeOverResponse = `{"type":"update","filePath":"/work/project/poc.txt","content":"replaced","structuredPatch":[{"oldStart":1,"oldLines":2,"newStart":1,"newLines":1,"lines":["-line one alpha","-line two","+replaced","\\ No newline at end of file"]}],"originalFile":"line one alpha\nline two\n","userModified":false}`

const notebookResponse = `{"new_source":"print(2)","old_source":"print(1)\n","cell_type":"code","language":"python","edit_mode":"replace","cell_id":"c1","error":"","notebook_path":"/work/project/nb.ipynb","original_file":"{\"cells\":[{\"source\":[\"print(1)\\n\"]}]}\n","updated_file":"{\n \"cells\": [\n  {\n   \"source\": \"print(2)\"\n  }\n ]\n}"}`

func TestWriteOverAnExistingFile(t *testing.T) {
	rec, ok := edits.Record(edits.Context{ID: "plugin/t", Session: "S", Stream: "a1", Tool: "t", ToolName: "Write", Time: "t", Root: "/work/project"}, json.RawMessage(writeOverResponse))
	if !ok {
		t.Fatal("no record")
	}
	c := rec.Changes[0]
	if c.Operation != changes.OpModify || !c.Before.Present || !c.After.Present || !c.After.NoNewlineAtEnd || c.After.NoNewlineAtEnd == c.Before.NoNewlineAtEnd {
		t.Fatalf("change: %+v", c)
	}
	if *c.Additions != 1 || *c.Deletions != 2 || c.Hunks[0].Lines[3] != "\\ No newline at end of file" {
		t.Fatalf("hunk: %+v add=%d del=%d", c.Hunks, *c.Additions, *c.Deletions)
	}
}

func TestNotebookEditHasNoPatchAndIsDiffed(t *testing.T) {
	rec, ok := edits.Record(edits.Context{ID: "plugin/t", Session: "S", Stream: "a1", Tool: "t", ToolName: "NotebookEdit", Time: "t", Root: "/work/project"}, json.RawMessage(notebookResponse))
	if !ok {
		t.Fatal("no record")
	}
	c := rec.Changes[0]
	if c.Path != "nb.ipynb" || c.Operation != changes.OpModify || c.Diff != changes.DiffAvailable || len(c.Hunks) == 0 {
		t.Fatalf("change: %+v", c)
	}
	if *c.Deletions == 0 || *c.Additions == 0 || !c.After.NoNewlineAtEnd {
		t.Fatalf("counts: %+v", c)
	}
}

func TestAResponseWithoutAFileIsNoRecord(t *testing.T) {
	if _, ok := edits.Record(edits.Context{}, json.RawMessage(`{"stdout":"","stderr":""}`)); ok {
		t.Fatal("a shell response made a record")
	}
	if _, ok := edits.Record(edits.Context{}, nil); ok {
		t.Fatal("nothing made a record")
	}
}
