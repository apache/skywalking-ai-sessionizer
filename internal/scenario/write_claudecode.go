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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// writeClaudeCode writes a plan as the files Claude Code leaves on disk: a
// transcript per stream, a meta file per child, and a journal, a manifest
// and a script per workflow run, in the layout the adapter discovers. Every
// file is written whole, so a build through a later checkpoint into the
// same directory is an append to what the earlier build wrote, which is
// what the collector's cursor expects of a live session.
func writeClaudeCode(p *Plan, root string) ([]string, error) {
	proj := filepath.Join(root, p.Project)
	w := &ccWriter{p: p, files: map[string][]string{}}
	for i := range p.Events {
		if err := w.event(&p.Events[i]); err != nil {
			return nil, err
		}
	}
	var written []string
	putUnder := func(dir, rel string, lines []string) error {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		var buf []byte
		for _, l := range lines {
			buf = append(buf, l...)
			buf = append(buf, '\n')
		}
		if err := os.WriteFile(path, buf, 0o644); err != nil {
			return err
		}
		written = append(written, path)
		return nil
	}
	put := func(rel string, lines []string) error { return putUnder(proj, rel, lines) }
	if err := put(p.Session+".jsonl", w.files["main"]); err != nil {
		return nil, err
	}
	// Noise a real project directory holds: a file whose name is not a
	// session id, and a directory that is not a session. Discovery must pass
	// over both, and every scenario checks that it does.
	if err := put("not-a-uuid.jsonl", []string{`{"type":"summary","summary":"not a session"}`}); err != nil {
		return nil, err
	}
	if err := put("memory/notes.md", []string{"# notes", "", "not a session either"}); err != nil {
		return nil, err
	}
	for _, s := range p.Streams {
		lines := w.files[s.ID]
		if s.Batch != "" {
			if err := put(fmt.Sprintf("%s/subagents/workflows/%s/agent-%s.jsonl", p.Session, s.Batch, s.ID), lines); err != nil {
				return nil, err
			}
			continue
		}
		if err := put(fmt.Sprintf("%s/subagents/agent-%s.jsonl", p.Session, s.ID), lines); err != nil {
			return nil, err
		}
		meta, _ := json.Marshal(map[string]any{
			"agentType": "general-purpose", "description": s.Label, "toolUseId": s.Tool, "spawnDepth": 1,
		})
		if err := put(fmt.Sprintf("%s/subagents/agent-%s.meta.json", p.Session, s.ID), []string{string(meta)}); err != nil {
			return nil, err
		}
	}
	for _, r := range p.Runs {
		var journal []string
		for _, j := range r.Journal {
			m := map[string]any{"type": j.Type, "agentId": j.Child, "key": "v2:" + j.Child}
			if j.Type == "result" {
				m["result"] = map[string]any{"done": true}
			}
			journal = append(journal, w.rec(m, ""))
		}
		if err := put(fmt.Sprintf("%s/subagents/workflows/%s/journal.jsonl", p.Session, r.ID), journal); err != nil {
			return nil, err
		}
		manifest, _ := json.Marshal(map[string]any{
			"runId": r.ID, "taskId": "task-" + r.ID, "workflowName": r.Name, "status": "completed", "agentCount": len(r.Children),
		})
		if err := put(fmt.Sprintf("%s/workflows/%s.json", p.Session, r.ID), []string{string(manifest)}); err != nil {
			return nil, err
		}
		scriptDir := proj
		if r.ScriptProject != "" {
			scriptDir = filepath.Join(root, r.ScriptProject)
		}
		if err := putUnder(scriptDir, fmt.Sprintf("%s/workflows/scripts/%s-%s.js", p.Session, strings.ReplaceAll(r.Name, " ", "-"), r.ID), strings.Split(r.Script, "\n")); err != nil {
			return nil, err
		}
	}
	sort.Strings(written)
	return written, nil
}

type ccWriter struct {
	p     *Plan
	files map[string][]string
}

func ccTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// rec adds the envelope every Claude Code record carries.
func (w *ccWriter) rec(m map[string]any, stream string) string {
	for k, v := range map[string]any{
		"sessionId": w.p.Session, "version": "2.1.245", "userType": "external",
		"entrypoint": "cli", "cwd": "/Users/dev/" + strings.TrimPrefix(w.p.Project, "-Users-dev-"), "gitBranch": "main",
	} {
		if _, ok := m[k]; !ok {
			m[k] = v
		}
	}
	if _, ok := m["isSidechain"]; !ok {
		m["isSidechain"] = stream != "" && stream != "main"
	}
	if stream != "" && stream != "main" {
		if _, ok := m["agentId"]; !ok {
			m["agentId"] = stream
		}
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func ccUsage(u Usage) map[string]any {
	return map[string]any{
		"input_tokens": u.In, "cache_creation_input_tokens": u.CacheWrite,
		"cache_read_input_tokens": u.CacheRead, "output_tokens": u.Out,
		"service_tier": "standard", "speed": "standard",
		"iterations": []map[string]any{{"type": "message"}}, "server_tool_use": map[string]any{},
	}
}

func parentOf(id string) any {
	if id == "" {
		return nil
	}
	return id
}

func (w *ccWriter) add(stream, line string) { w.files[stream] = append(w.files[stream], line) }

func (w *ccWriter) event(e *Event) error {
	s := e.Stream
	switch e.Kind {
	case EvTitle:
		w.add(s, w.rec(map[string]any{"type": "ai-title", "aiTitle": e.Label}, s))
	case EvInput:
		m := map[string]any{
			"type": "user", "uuid": e.ID, "parentUuid": parentOf(e.Parent), "promptId": e.Run,
			"timestamp": ccTime(e.At),
		}
		if s == "main" {
			m["origin"] = map[string]any{"kind": "human"}
			m["permissionMode"] = "auto"
			m["message"] = map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": e.Text}}}
		} else {
			m["message"] = map[string]any{"role": "user", "content": e.Text}
		}
		w.add(s, w.rec(m, s))
	case EvQueued:
		w.add(s, w.rec(map[string]any{
			"type": "attachment", "uuid": e.ID, "parentUuid": parentOf(e.Parent), "timestamp": ccTime(e.At),
			"attachment": map[string]any{"type": "queued_command", "commandMode": e.Mode, "content": e.Text},
		}, s))
	case EvInject:
		w.add(s, w.rec(map[string]any{
			"type": "attachment", "uuid": e.ID, "parentUuid": parentOf(e.Parent), "timestamp": ccTime(e.At),
			"attachment": map[string]any{"type": e.Type, "content": e.Text},
		}, s))
	case EvFragment:
		var block map[string]any
		switch e.Frag {
		case FragThinking:
			block = map[string]any{"type": "thinking", "thinking": e.Text, "signature": "sig"}
		case FragText:
			block = map[string]any{"type": "text", "text": e.Text}
		case FragToolUse:
			input := e.Tool.Input
			if input == nil {
				input = map[string]any{}
			}
			block = map[string]any{"type": "tool_use", "id": e.Tool.ID, "name": e.Tool.Name, "input": input}
		}
		var stop any
		if e.Last {
			stop = e.Stop
		}
		w.add(s, w.rec(map[string]any{
			"type": "assistant", "uuid": e.ID, "parentUuid": parentOf(e.Parent), "requestId": e.Req, "timestamp": ccTime(e.At),
			"message": map[string]any{"id": e.Call, "type": "message", "role": "assistant", "model": "claude-opus-5",
				"stop_reason": stop, "content": []map[string]any{block}, "usage": ccUsage(e.Usage)},
		}, s))
	case EvResult:
		block := map[string]any{"tool_use_id": e.Of, "type": "tool_result", "content": e.Text}
		if e.Failed != nil && *e.Failed {
			block["is_error"] = true
		}
		m := map[string]any{
			"type": "user", "uuid": e.ID, "parentUuid": parentOf(e.Parent), "promptId": e.Run,
			"sourceToolAssistantUUID": e.Parent, "timestamp": ccTime(e.At),
			"message": map[string]any{"role": "user", "content": []map[string]any{block}},
		}
		if !e.Replayed {
			switch {
			case e.Ack != nil:
				m["toolUseResult"] = map[string]any{"agentId": e.Ack.Child, "isAsync": true, "status": "async_launched",
					"prompt": e.Ack.Prompt, "description": e.Ack.Label}
			case e.Fork != nil:
				m["toolUseResult"] = map[string]any{"agentId": e.Fork.Child, "status": "forked"}
			case e.Launch != nil:
				m["toolUseResult"] = map[string]any{"runId": e.Launch.Run, "status": "async_launched",
					"transcriptDir": "subagents/workflows/" + e.Launch.Run, "workflowName": e.Launch.Name}
			case e.StringEnrichment:
				m["toolUseResult"] = "a bare string, not an object"
			default:
				m["toolUseResult"] = map[string]any{"stdout": e.Text, "stderr": ""}
			}
		}
		w.add(s, w.rec(m, s))
	case EvNotice:
		w.add(s, w.rec(map[string]any{
			"type": "user", "uuid": e.ID, "parentUuid": parentOf(e.Parent), "promptId": e.Run,
			"origin": map[string]any{"kind": "task-notification"}, "timestamp": ccTime(e.At),
			"message": map[string]any{"role": "user", "content": e.Text},
		}, s))
	case EvSynthetic:
		w.add(s, w.rec(map[string]any{
			"type": "assistant", "uuid": e.ID, "parentUuid": parentOf(e.Parent), "requestId": e.Req, "timestamp": ccTime(e.At),
			"message": map[string]any{"id": e.Call, "type": "message", "role": "assistant", "model": "<synthetic>",
				"stop_reason": nil, "content": []map[string]any{{"type": "text", "text": e.Text}}, "usage": ccUsage(Usage{})},
		}, s))
	case EvBoundary:
		w.add(s, w.rec(map[string]any{
			"type": "system", "subtype": "compact_boundary", "uuid": e.ID, "parentUuid": nil,
			"logicalParentUuid": e.Continues, "timestamp": ccTime(e.At),
			"compactMetadata": map[string]any{"trigger": "auto", "preservedMessages": map[string]any{"allUuids": []any{e.Continues}}},
		}, s))
	case EvSummary:
		w.add(s, w.rec(map[string]any{
			"type": "user", "uuid": e.ID, "parentUuid": parentOf(e.Parent), "isCompactSummary": true, "promptId": e.Run,
			"timestamp": ccTime(e.At),
			"message":   map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": e.Text}}},
		}, s))
	case EvSystem:
		m := map[string]any{"type": "system", "subtype": e.Type, "uuid": e.ID, "parentUuid": parentOf(e.Parent), "timestamp": ccTime(e.At)}
		for k, v := range e.Fields {
			m[k] = v
		}
		w.add(s, w.rec(m, s))
	default:
		return fmt.Errorf("scenario: event kind %d cannot be written", e.Kind)
	}
	return nil
}
