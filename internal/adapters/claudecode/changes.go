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

package claudecode

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/pkg/changes"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// patchHunk is one hunk as the runtime writes it on an editing tool's
// result: a unified diff as JSON. It is the shape changes/1 uses too, so
// the copy below renames the keys and nothing else.
type patchHunk struct {
	OldStart int      `json:"oldStart"`
	OldLines int      `json:"oldLines"`
	NewStart int      `json:"newStart"`
	NewLines int      `json:"newLines"`
	Lines    []string `json:"lines"`
}

// nativeChanges derives a changes/1 record from what the runtime recorded
// about its own editing tool, when it recorded a patch. Measured on the
// main stream of 52 sessions, every successful Edit and Write result
// carries one; a subagent's never does, and those the plugin observes.
//
// The record travels as a data part beside the raw result, which stays as
// it was. Nothing is inferred: the paths, the patch and the original
// content are the runtime's own claims, copied into the model's shape and
// hashed, so a view joins them to the step the way it joins the plugin's
// records, by the tool-use id the result already carries.
func nativeChanges(d *indexRecord, tur *toolResult, src Source, rec *sessiondata.Record) (sessiondata.Part, bool) {
	// The tool-use id a result answers is on its result part, which is
	// where a tool step reads it from too.
	tool := ""
	for _, p := range rec.Parts {
		if p.Kind == sessiondata.PartResult && p.Of != "" {
			tool = p.Of
			break
		}
	}
	if tur == nil || tool == "" {
		return sessiondata.Part{}, false
	}
	filePath, original, content := tur.FilePath, tur.OriginalFile, tur.Content
	if filePath == "" && tur.NotebookPath != "" {
		filePath, original, content = tur.NotebookPath, tur.NotebookBefore, tur.NotebookAfter
	}
	if filePath == "" {
		return sessiondata.Part{}, false
	}
	var before, after []byte
	created := original == nil
	if !created {
		before = []byte(*original)
	}
	switch {
	case content != nil:
		after = []byte(*content)
	case tur.OldString != "" && !created:
		n := 1
		if tur.ReplaceAll {
			n = -1
		}
		after = []byte(strings.Replace(string(before), tur.OldString, tur.NewString, n))
	}
	if len(tur.StructuredPatch) == 0 && after == nil {
		// No patch and no way to know what the file became: the runtime
		// said nothing a record could carry.
		return sessiondata.Part{}, false
	}

	fc := changes.FileChange{
		Path:      filePath,
		Operation: changes.OpModify,
		Before:    changes.EndpointOf(before, !created),
		After:     changes.EndpointOf(after, true),
		Diff:      changes.DiffAvailable,
	}
	if created {
		fc.Operation = changes.OpCreate
	}
	root := d.Cwd
	if root != "" {
		if rel, err := filepath.Rel(root, filePath); err == nil && !strings.HasPrefix(rel, "..") {
			fc.Path = filepath.ToSlash(rel)
		}
	}
	if after == nil {
		// The patch says what changed; what the whole file became is
		// unknown, and its hash stays unknown rather than invented.
		fc.After = changes.Endpoint{Present: true, Bytes: nil}
	}
	add, del := 0, 0
	if len(tur.StructuredPatch) > 0 {
		for _, h := range tur.StructuredPatch {
			fc.Hunks = append(fc.Hunks, changes.Hunk{
				OldStart: h.OldStart, OldLines: h.OldLines, NewStart: h.NewStart, NewLines: h.NewLines, Lines: h.Lines,
			})
			for _, l := range h.Lines {
				switch {
				case strings.HasPrefix(l, "+"):
					add++
				case strings.HasPrefix(l, "-"):
					del++
				}
			}
		}
	} else if changes.IsText(before) && changes.IsText(after) {
		hunks, a, r, ok := changes.Diff(changes.SplitLines(before), changes.SplitLines(after))
		if !ok {
			fc.Diff = changes.DiffTooLarge
		}
		fc.Hunks, add, del = hunks, a, r
	} else {
		fc.Diff = changes.DiffBinary
	}
	if fc.Diff == changes.DiffAvailable {
		fc.Additions, fc.Deletions = changes.Int(add), changes.Int(del)
	}

	r := &changes.Record{
		Schema: changes.Schema, ID: tool, CapturedBy: changes.CapturedByClaudeCode, Session: src.Session, Stream: src.Stream,
		Tool: tool, Time: d.Timestamp, Basis: changes.BasisRuntimeReported,
		ChangedFiles: changes.Int(1), Changes: []changes.FileChange{fc},
	}
	if root != "" {
		r.Root = &changes.Root{Path: root}
	}
	if r.Validate() != nil {
		return sessiondata.Part{}, false
	}
	data, err := r.Marshal()
	if err != nil {
		return sessiondata.Part{}, false
	}
	return sessiondata.Part{Kind: sessiondata.PartData, Data: json.RawMessage(data),
		State: model.ContentAvailable, Bytes: len(data)}, true
}
