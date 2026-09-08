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

// Package edits records an editing tool's change from the hook's own
// response, for the calls the runtime writes no patch of its own for:
// those inside a subagent.
//
// The PostToolUse response for Edit and Write carries the file, its
// content before, and the patch as a unified diff in JSON, seen in a run
// of Claude Code 2.1.260. That is everything a record needs: no scan and
// no kept bytes, one file read to hash what the file became. The same
// fields are read by the asz adapter from the transcript of the main
// stream; the plugin reads them here because a subagent's transcript
// does not carry them.
package edits

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/pkg/changes"
)

// IsEditingTool names the tools whose response carries a patch.
func IsEditingTool(name string) bool {
	switch name {
	case "Edit", "Write", "NotebookEdit":
		return true
	}
	return false
}

// response is what the hook hands over after an editing tool ran. Edit
// and Write name the file as filePath with the patch and the content
// before; NotebookEdit names it as notebook_path with the whole file
// before and after and no patch. Both were read from a run of Claude Code
// 2.1.260.
type response struct {
	FilePath        string  `json:"filePath"`
	OriginalFile    *string `json:"originalFile"`
	Content         *string `json:"content"`
	OldString       string  `json:"oldString"`
	NewString       string  `json:"newString"`
	ReplaceAll      bool    `json:"replaceAll"`
	NotebookPath    string  `json:"notebook_path"`
	NotebookBefore  *string `json:"original_file"`
	NotebookAfter   *string `json:"updated_file"`
	StructuredPatch []struct {
		OldStart int      `json:"oldStart"`
		OldLines int      `json:"oldLines"`
		NewStart int      `json:"newStart"`
		NewLines int      `json:"newLines"`
		Lines    []string `json:"lines"`
	} `json:"structuredPatch"`
}

// Context is what the record says about where it came from.
type Context struct {
	ID       string
	Session  string
	Stream   string
	Tool     string
	ToolName string
	Time     string
	Root     string
}

// Record builds the change record from the response. ok is false when the
// response says nothing a record could carry, which is how a failed edit
// or an unknown tool response reads.
func Record(ctx Context, raw json.RawMessage) (*changes.Record, bool) {
	var resp response
	if len(raw) == 0 || json.Unmarshal(raw, &resp) != nil {
		return nil, false
	}
	if resp.NotebookPath != "" && resp.FilePath == "" {
		resp.FilePath, resp.OriginalFile, resp.Content = resp.NotebookPath, resp.NotebookBefore, resp.NotebookAfter
	}
	if resp.FilePath == "" {
		return nil, false
	}
	created := resp.OriginalFile == nil
	var before, after []byte
	if !created {
		before = []byte(*resp.OriginalFile)
	}
	switch {
	case resp.Content != nil:
		after = []byte(*resp.Content)
	case resp.OldString != "" && !created:
		n := 1
		if resp.ReplaceAll {
			n = -1
		}
		after = []byte(strings.Replace(string(before), resp.OldString, resp.NewString, n))
	default:
		// The patch alone: what the file became is read back from disk,
		// which is the file the tool just wrote.
		if data, err := os.ReadFile(resp.FilePath); err == nil {
			after = data
		}
	}
	if len(resp.StructuredPatch) == 0 && after == nil {
		return nil, false
	}
	fc := changes.FileChange{
		Path:      resp.FilePath,
		Operation: changes.OpModify,
		Before:    changes.EndpointOf(before, !created),
		After:     changes.EndpointOf(after, true),
		Diff:      changes.DiffAvailable,
	}
	if created {
		fc.Operation = changes.OpCreate
	}
	if ctx.Root != "" {
		if rel, err := filepath.Rel(ctx.Root, resp.FilePath); err == nil && !strings.HasPrefix(rel, "..") {
			fc.Path = filepath.ToSlash(rel)
		}
	}
	if after == nil {
		fc.After = changes.Endpoint{Present: true}
	}
	add, del := 0, 0
	if len(resp.StructuredPatch) > 0 {
		for _, h := range resp.StructuredPatch {
			fc.Hunks = append(fc.Hunks, changes.Hunk{OldStart: h.OldStart, OldLines: h.OldLines, NewStart: h.NewStart, NewLines: h.NewLines, Lines: h.Lines})
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
	rec := &changes.Record{
		Schema: changes.Schema, ID: ctx.ID, Session: ctx.Session, Stream: ctx.Stream,
		Tool: ctx.Tool, ToolName: ctx.ToolName, Time: ctx.Time, Basis: changes.BasisRuntimeReported,
		ChangedFiles: changes.Int(1), Changes: []changes.FileChange{fc},
	}
	if ctx.Root != "" {
		rec.Root = &changes.Root{Path: ctx.Root}
	}
	return rec, true
}
