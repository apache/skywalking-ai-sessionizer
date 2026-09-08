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
	"github.com/apache/skywalking-ai-sessionizer/pkg/changes"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// The result records below are the shapes a run of Claude Code 2.1.260
// wrote to its transcript, with the identifiers and paths replaced: a
// Write over an existing file, whose patch ends with the git marker for a
// missing final newline, and a NotebookEdit, which names its file and its
// content under other keys and carries no patch.

const writeOverRecord = `{"parentUuid":"p","isSidechain":false,"cwd":"/work/project","sessionId":"s","version":"2.1.260","type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_w","type":"tool_result","content":"The file has been updated."}]},"uuid":"u-write","timestamp":"2026-09-08T10:00:00.000Z","toolUseResult":{"type":"update","filePath":"/work/project/poc.txt","content":"replaced","structuredPatch":[{"oldStart":1,"oldLines":2,"newStart":1,"newLines":1,"lines":["-line one alpha","-line two","+replaced","\\ No newline at end of file"]}],"originalFile":"line one alpha\nline two\n","userModified":false}}`

const notebookRecord = `{"parentUuid":"p","isSidechain":false,"cwd":"/work/project","sessionId":"s","version":"2.1.260","type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_n","type":"tool_result","content":"Updated cell c1."}]},"uuid":"u-nb","timestamp":"2026-09-08T10:00:01.000Z","toolUseResult":{"new_source":"print(2)","old_source":"print(1)\n","cell_type":"code","language":"python","edit_mode":"replace","cell_id":"c1","error":"","notebook_path":"/work/project/nb.ipynb","original_file":"{\"cells\":[{\"source\":[\"print(1)\\n\"]}]}\n","updated_file":"{\n \"cells\": [\n  {\n   \"source\": \"print(2)\"\n  }\n ]\n}"}}`

func nativePart(t *testing.T, payload string) *changes.Record {
	t.Helper()
	src := claudecode.Source{Kind: claudecode.SrcMainTranscript, Rel: "p/s.jsonl", Session: "s", Stream: "main"}
	rec := claudecode.Convert(src, 1, 0, []byte(payload))
	var found *changes.Record
	for _, p := range rec.Parts {
		if p.Kind == sessiondata.PartData {
			if r, ok := changes.Decode(p.Data); ok {
				found = r
			}
		}
	}
	if found == nil {
		t.Fatalf("no change record among the parts: %+v", rec.Parts)
	}
	if rec.Parts[0].Kind != sessiondata.PartResult {
		t.Fatalf("the raw result did not stay first: %+v", rec.Parts)
	}
	return found
}

func TestWriteOverAnExistingFileKeepsTheRuntimesPatch(t *testing.T) {
	r := nativePart(t, writeOverRecord)
	if r.Basis != changes.BasisRuntimeReported || r.Tool != "toolu_w" || r.Root.Path != "/work/project" || r.ID != "native/u-write" {
		t.Fatalf("record: %+v", r)
	}
	c := r.Changes[0]
	if c.Path != "poc.txt" || c.Operation != changes.OpModify || !c.After.NoNewlineAtEnd || c.Before.NoNewlineAtEnd {
		t.Fatalf("change: %+v", c)
	}
	if *c.Additions != 1 || *c.Deletions != 2 || len(c.Hunks) != 1 || c.Hunks[0].Lines[3] != "\\ No newline at end of file" {
		t.Fatalf("hunk: %+v", c.Hunks)
	}
}

func TestNotebookEditIsDiffedFromItsTwoVersions(t *testing.T) {
	r := nativePart(t, notebookRecord)
	c := r.Changes[0]
	if c.Path != "nb.ipynb" || c.Operation != changes.OpModify || c.Diff != changes.DiffAvailable || len(c.Hunks) == 0 || *c.Additions == 0 || *c.Deletions == 0 {
		t.Fatalf("change: %+v", c)
	}
	if !c.Before.Present || !c.After.Present || c.Before.SHA256 == c.After.SHA256 {
		t.Fatalf("endpoints: %+v %+v", c.Before, c.After)
	}
}
