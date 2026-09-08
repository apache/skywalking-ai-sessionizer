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

package scenario

import (
	"fmt"
	"strings"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/pkg/changes"
)

// Cwd is the working directory every record of the plan names, which is
// the root a change record's paths are relative to.
func (p *Plan) Cwd() string {
	return "/Users/dev/" + strings.TrimPrefix(p.Project, "-Users-dev-")
}

// isEditingTool says whether the runtime records the tool's own patch. For
// these a scenario writes the change the way the runtime does, on the
// result; for every other tool it writes what the plugin would observe.
func isEditingTool(name string) bool {
	switch name {
	case "Edit", "Write", "NotebookEdit":
		return true
	}
	return false
}

// checkChanges rejects what neither producer could have written.
func checkChanges(t *Tool, id string) error {
	for i, ch := range t.Changes {
		if ch.Path == "" || strings.HasPrefix(ch.Path, "/") {
			return fmt.Errorf("scenario: step %s: change %d needs a path relative to the workspace", id, i+1)
		}
		if ch.Before == nil && ch.After == nil {
			return fmt.Errorf("scenario: step %s: change %s has neither a before nor an after", id, ch.Path)
		}
	}
	if isEditingTool(t.Name) {
		if len(t.Changes) > 1 {
			return fmt.Errorf("scenario: step %s: %s changes one file; the runtime records one patch per result", id, t.Name)
		}
		if len(t.Changes) == 1 && t.Name == "Edit" && (t.Changes[0].Before == nil || t.Changes[0].After == nil) {
			return fmt.Errorf("scenario: step %s: Edit changes an existing file; use Write to create one", id)
		}
	}
	return nil
}

// fileChange builds one file's entry from its content before and after,
// with the hunks the diff gives, as every producer computes them.
func fileChange(ch Change) changes.FileChange {
	var before, after []byte
	if ch.Before != nil {
		before = []byte(*ch.Before)
	}
	if ch.After != nil {
		after = []byte(*ch.After)
	}
	fc := changes.FileChange{
		Path:   ch.Path,
		Before: changes.EndpointOf(before, ch.Before != nil),
		After:  changes.EndpointOf(after, ch.After != nil),
		Diff:   changes.DiffAvailable,
	}
	switch {
	case ch.Before == nil:
		fc.Operation = changes.OpCreate
	case ch.After == nil:
		fc.Operation = changes.OpDelete
	default:
		fc.Operation = changes.OpModify
	}
	if !changes.IsText(before) || !changes.IsText(after) {
		fc.Diff = changes.DiffBinary
		return fc
	}
	hunks, add, del, ok := changes.Diff(changes.SplitLines(before), changes.SplitLines(after))
	if !ok {
		fc.Diff = changes.DiffTooLarge
		return fc
	}
	fc.Hunks, fc.Additions, fc.Deletions = hunks, changes.Int(add), changes.Int(del)
	return fc
}

// nativeRecord is the record the adapter derives from the runtime's own
// patch on an editing tool's result, built here the same way so the two
// formats land the same bytes.
func (p *Plan) nativeRecord(e *Event) *changes.Record {
	fc := fileChange(e.Changes[0])
	return &changes.Record{
		Schema: changes.Schema, ID: e.Of, CapturedBy: changes.CapturedByClaudeCode, Session: p.Session, Stream: e.Stream,
		Tool: e.Of, Time: ccTime(e.At), Basis: changes.BasisRuntimeReported,
		Root: &changes.Root{Path: p.Cwd()}, ChangedFiles: changes.Int(1),
		Changes: []changes.FileChange{fc},
	}
}

// pluginRecord is the record the plugin writes for a tool it observed: a
// window around the call, the policy it ran under, and every changed file
// attributed to this window alone.
func (p *Plan) pluginRecord(e *Event) *changes.Record {
	id := e.Of
	r := &changes.Record{
		Schema: changes.Schema, ID: id, CapturedBy: changes.CapturedByASZPlugin, Session: p.Session, Stream: e.Stream,
		Tool: e.Of, ToolName: e.ToolName, Time: ccTime(e.At), Basis: changes.BasisToolWindow,
		Root:   &changes.Root{Path: p.Cwd()},
		Policy: &changes.Policy{Exclusions: "standard-v1", ReadOnly: "readonly-v1"},
		Window: &changes.Window{
			Before: changes.Interval{From: ccTime(e.At.Add(-time.Second)), To: ccTime(e.At.Add(-time.Second))},
			After:  changes.Interval{From: ccTime(e.At), To: ccTime(e.At)},
		},
		Outcome:      &changes.Outcome{State: changes.OutcomeReturned, ExitCode: changes.Int(0)},
		Coverage:     changes.CoverageComplete,
		ChangedFiles: changes.Int(len(e.Changes)),
	}
	if e.Failed != nil && *e.Failed {
		r.Outcome = &changes.Outcome{State: changes.OutcomeFailed, ExitCode: changes.Int(1)}
	}
	for _, ch := range e.Changes {
		fc := fileChange(ch)
		fc.Attribution, fc.Windows = changes.AttributionOnlyThisWindow, []string{id}
		r.Changes = append(r.Changes, fc)
	}
	return r
}

// runtimePatch is the runtime's own shape of a patch, as it writes it on an
// Edit or Write result: the same hunks under its own key names.
func runtimePatch(fc changes.FileChange) []map[string]any {
	out := make([]map[string]any, 0, len(fc.Hunks))
	for _, h := range fc.Hunks {
		out = append(out, map[string]any{
			"oldStart": h.OldStart, "oldLines": h.OldLines, "newStart": h.NewStart, "newLines": h.NewLines, "lines": h.Lines,
		})
	}
	return out
}

// runtimeResult is what the runtime records on an editing tool's result
// for a change: the file, its content before, and its patch. An Edit names
// the text it replaced; a Write names the content it wrote.
func (p *Plan) runtimeResult(e *Event) map[string]any {
	ch := e.Changes[0]
	fc := fileChange(ch)
	m := map[string]any{
		"filePath": p.Cwd() + "/" + ch.Path, "structuredPatch": runtimePatch(fc), "userModified": false,
	}
	if ch.Before != nil {
		m["originalFile"] = *ch.Before
	} else {
		m["originalFile"] = nil
	}
	switch e.ToolName {
	case "Edit":
		m["oldString"], m["newString"], m["replaceAll"] = *ch.Before, *ch.After, false
	default:
		m["type"] = "update"
		if ch.Before == nil {
			m["type"] = "create"
		}
		if ch.After != nil {
			m["content"] = *ch.After
		} else {
			m["content"] = ""
		}
	}
	return m
}

// stripChanges returns the steps with every change removed, sharing
// nothing with the original, so a scenario can be built again without its
// changes and the two folds compared.
func stripChanges(steps []Step) []Step {
	out := make([]Step, len(steps))
	for i, s := range steps {
		if s.Call != nil {
			c := *s.Call
			if c.Tool != nil && len(c.Tool.Changes) > 0 {
				t := *c.Tool
				t.Changes = nil
				c.Tool = &t
			}
			if c.Agent != nil {
				a := *c.Agent
				a.Steps = stripChanges(a.Steps)
				c.Agent = &a
			}
			if c.Skill != nil {
				k := *c.Skill
				k.Steps = stripChanges(k.Steps)
				c.Skill = &k
			}
			if c.Workflow != nil {
				w := *c.Workflow
				w.Children = make([]Child, len(w.Children))
				for j, child := range c.Workflow.Children {
					child.Steps = stripChanges(child.Steps)
					w.Children[j] = child
				}
				c.Workflow = &w
			}
			s.Call = &c
		}
		out[i] = s
	}
	return out
}

// HasChanges reports whether any tool in the steps changes a file.
func HasChanges(steps []Step) bool {
	for _, s := range steps {
		if s.Call == nil {
			continue
		}
		c := s.Call
		if c.Tool != nil && len(c.Tool.Changes) > 0 {
			return true
		}
		if c.Agent != nil && HasChanges(c.Agent.Steps) {
			return true
		}
		if c.Skill != nil && HasChanges(c.Skill.Steps) {
			return true
		}
		if c.Workflow != nil {
			for _, child := range c.Workflow.Children {
				if HasChanges(child.Steps) {
					return true
				}
			}
		}
	}
	return false
}

// WithoutChanges is the scenario with every change removed.
func (sc *Scenario) WithoutChanges() *Scenario {
	out := *sc
	out.Steps = stripChanges(sc.Steps)
	return &out
}
