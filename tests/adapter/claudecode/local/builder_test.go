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

package local_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// transcript builds a Claude Code session incrementally, so a test can append
// turns between collection rounds - which is the mode that actually runs.
//
// Ids are shaped the way discovery requires (session ids are UUID-shaped, agent
// ids are `a` plus 16 hex) but chosen to be recognisable. Everything
// unconstrained says what it is.
type transcript struct {
	t       *testing.T
	root    string // the fixture source root
	slug    string // project directory, a slugified working directory
	session string
	lines   []string // main-stream records, appended across turns
	turn    int
	agents  map[string][]string // child stream -> its records
	journal map[string][]string // batch -> its journal records
	// wfAgents keys are "<batch>/<agent>", because a workflow's children are
	// filed under the batch directory rather than beside the direct children.
	wfAgents map[string][]string
}

const growSession = "add1c7ed-0003-4000-8000-000000000003" // the growing session

func newTranscript(t *testing.T, root string) *transcript {
	return &transcript{
		t: t, root: root, slug: "-Users-dev-growing-work", session: growSession,
		agents: map[string][]string{}, journal: map[string][]string{},
		wfAgents: map[string][]string{},
	}
}

func (x *transcript) rec(m map[string]any) string {
	for k, v := range map[string]any{
		"sessionId": x.session, "version": "2.1.245", "userType": "external",
		"entrypoint": "cli", "cwd": "/Users/dev/growing-work", "gitBranch": "main",
	} {
		if _, ok := m[k]; !ok {
			m[k] = v
		}
	}
	if _, ok := m["isSidechain"]; !ok {
		m["isSidechain"] = false
	}
	b, err := json.Marshal(m)
	if err != nil {
		x.t.Fatal(err)
	}
	return string(b)
}

func usageFull(out int) map[string]any {
	return map[string]any{
		"input_tokens": 2, "cache_creation_input_tokens": 100,
		"cache_read_input_tokens": 900, "output_tokens": out,
		"service_tier": "standard", "speed": "standard",
		"iterations": []map[string]any{{"type": "message"}}, "server_tool_use": map[string]any{},
	}
}

// AddTurn appends one complete human turn: the prompt, a provider call split
// across fragments, a tool call and its result, and a final answer.
//
// withAgent additionally spawns a child agent and lands the async return chain -
// launch acknowledgement, child stream, completion notification, parent resume -
// which is a second prompt cycle inside the SAME Talk.
func (x *transcript) AddTurn(prompt string, withAgent bool) {
	x.turn++
	n := x.turn
	id := func(f string, a ...any) string { return fmt.Sprintf("t%d-%s", n, fmt.Sprintf(f, a...)) }
	ts := func(s int) string { return fmt.Sprintf("2026-02-%02dT10:%02d:%02d.000Z", n, s/60, s%60) }

	x.lines = append(x.lines,
		x.rec(map[string]any{
			"type": "user", "uuid": id("human-input"), "parentUuid": nil,
			"promptId": id("cycle-human"), "origin": map[string]any{"kind": "human"},
			"timestamp": ts(1), "permissionMode": "auto",
			"message": map[string]any{"role": "user",
				"content": []map[string]any{{"type": "text", "text": prompt}}},
		}),
		// one provider call, two fragments - usage repeated, as the main stream does
		x.rec(map[string]any{
			"type": "assistant", "uuid": id("call-frag1"), "parentUuid": id("human-input"),
			"requestId": id("req"), "timestamp": ts(2),
			"message": map[string]any{"id": id("call"), "type": "message", "role": "assistant",
				"model": "claude-opus-5", "stop_reason": nil,
				"content": []map[string]any{{"type": "text", "text": "Working on turn " + fmt.Sprint(n)}},
				"usage":   usageFull(40)},
		}),
		x.rec(map[string]any{
			"type": "assistant", "uuid": id("call-frag2"), "parentUuid": id("call-frag1"),
			"requestId": id("req"), "timestamp": ts(3),
			"message": map[string]any{"id": id("call"), "type": "message", "role": "assistant",
				"model": "claude-opus-5", "stop_reason": "tool_use",
				"content": []map[string]any{{"type": "tool_use", "id": id("tool"), "name": "Bash",
					"input": map[string]any{"command": fmt.Sprintf("echo turn-%d", n)}}},
				"usage": usageFull(40)},
		}),
		x.rec(map[string]any{
			"type": "user", "uuid": id("tool-result"), "parentUuid": id("call-frag2"),
			"promptId": id("cycle-human"), "sourceToolAssistantUUID": id("call-frag2"),
			"timestamp": ts(4),
			"message": map[string]any{"role": "user",
				"content": []map[string]any{{"tool_use_id": id("tool"), "type": "tool_result",
					"content": fmt.Sprintf("turn-%d", n)}}},
			"toolUseResult": map[string]any{"stdout": fmt.Sprintf("turn-%d", n), "stderr": ""},
		}),
	)

	last := id("tool-result")
	if withAgent {
		agent := fmt.Sprintf("a%016x", 0xadd1c7ed0000+n) // `a` + 16 hex, as required
		x.lines = append(x.lines,
			x.rec(map[string]any{
				"type": "assistant", "uuid": id("spawn-call"), "parentUuid": last,
				"requestId": id("req-spawn"), "timestamp": ts(5),
				"message": map[string]any{"id": id("call-spawn"), "type": "message", "role": "assistant",
					"model": "claude-opus-5", "stop_reason": "tool_use",
					"content": []map[string]any{{"type": "tool_use", "id": id("tool-spawn"), "name": "Agent",
						"input": map[string]any{"description": "helper", "prompt": "help with turn " + fmt.Sprint(n)}}},
					"usage": usageFull(15)},
			}),
			x.rec(map[string]any{
				"type": "user", "uuid": id("spawn-ack"), "parentUuid": id("spawn-call"),
				"promptId": id("cycle-human"), "sourceToolAssistantUUID": id("spawn-call"),
				"timestamp": ts(6),
				"message": map[string]any{"role": "user",
					"content": []map[string]any{{"tool_use_id": id("tool-spawn"), "type": "tool_result",
						"content": "launched"}}},
				"toolUseResult": map[string]any{"agentId": agent, "isAsync": true,
					"status": "async_launched", "prompt": "help with turn " + fmt.Sprint(n)},
			}),
			// a NEW prompt cycle, but the human said nothing - same Talk
			x.rec(map[string]any{
				"type": "user", "uuid": id("agent-done-notice"), "parentUuid": id("spawn-ack"),
				"promptId": id("cycle-notification"), "origin": map[string]any{"kind": "task-notification"},
				"timestamp": ts(30),
				"message": map[string]any{"role": "user",
					"content": "<task-notification>\n<task-id>" + agent + "</task-id>\n<tool-use-id>" +
						id("tool-spawn") + "</tool-use-id>\n<status>completed</status>\n</task-notification>"},
			}),
		)
		last = id("agent-done-notice")

		x.agents[agent] = []string{
			x.rec(map[string]any{
				"type": "user", "uuid": id("agent-prompt"), "parentUuid": nil,
				"promptId": id("agent-cycle"), "agentId": agent, "isSidechain": true,
				"timestamp": ts(7),
				"message":   map[string]any{"role": "user", "content": "help with turn " + fmt.Sprint(n)},
			}),
			x.rec(map[string]any{
				"type": "assistant", "uuid": id("agent-answer"), "parentUuid": id("agent-prompt"),
				"requestId": id("agent-req"), "agentId": agent, "isSidechain": true,
				"timestamp": ts(8),
				"message": map[string]any{"id": id("agent-call"), "type": "message", "role": "assistant",
					"model": "claude-opus-5", "stop_reason": "end_turn",
					"content": []map[string]any{{"type": "text",
						"text": fmt.Sprintf("helper finished turn %d", n)}},
					"usage": usageFull(20)},
			}),
		}
	}

	x.lines = append(x.lines, x.rec(map[string]any{
		"type": "assistant", "uuid": id("final"), "parentUuid": last,
		"requestId": id("req-final"), "timestamp": ts(40),
		"message": map[string]any{"id": id("call-final"), "type": "message", "role": "assistant",
			"model": "claude-opus-5", "stop_reason": "end_turn",
			"content": []map[string]any{{"type": "text",
				"text": fmt.Sprintf("Turn %d complete.", n)}},
			"usage": usageFull(25)},
	}))
}

// Flush writes the accumulated transcript, appending rather than rewriting -
// which is exactly what Claude Code does to a live session.
func (x *transcript) Flush() {
	x.t.Helper()
	write := func(path string, lines []string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			x.t.Fatal(err)
		}
		var buf []byte
		for _, l := range lines {
			buf = append(buf, l...)
			buf = append(buf, '\n')
		}
		if err := os.WriteFile(path, buf, 0o644); err != nil {
			x.t.Fatal(err)
		}
	}
	proj := filepath.Join(x.root, x.slug)
	write(filepath.Join(proj, x.session+".jsonl"), x.lines)
	for agent, lines := range x.agents {
		write(filepath.Join(proj, x.session, "subagents", "agent-"+agent+".jsonl"), lines)
	}
	for run, lines := range x.journal {
		write(filepath.Join(proj, x.session, "subagents", "workflows", run, "journal.jsonl"), lines)
	}
	for key, lines := range x.wfAgents {
		run, agent, _ := strings.Cut(key, "/")
		write(filepath.Join(proj, x.session, "subagents", "workflows", run, "agent-"+agent+".jsonl"), lines)
	}
}

// Turns reports how many human turns have been written.
func (x *transcript) Turns() int { return x.turn }

// AddInjection appends material the harness put into model context.
//
// It shapes a response as much as a human turn does, and in some cases it is the
// only record of an input, so it must survive assembly as its own step.
func (x *transcript) AddInjection(kind, text string) {
	x.turn++
	n := x.turn
	x.lines = append(x.lines, x.rec(map[string]any{
		"type": "attachment", "uuid": fmt.Sprintf("t%d-injection", n),
		"parentUuid": x.lastUUID(), "timestamp": fmt.Sprintf("2026-02-%02dT09:00:00.000Z", n),
		"attachment": map[string]any{"type": kind, "content": text},
	}))
}

// AddQueuedPrompt appends human input that exists ONLY as an attachment.
//
// A reader following message records alone loses it. The companion case is a
// queued attachment whose command mode says it is a completion notification -
// that one is NOT human input, and reading the type without the mode pulls it in
// as something the user typed.
func (x *transcript) AddQueuedPrompt(text string, mode string) {
	x.turn++
	n := x.turn
	x.lines = append(x.lines, x.rec(map[string]any{
		"type": "attachment", "uuid": fmt.Sprintf("t%d-queued-%s", n, mode),
		"parentUuid": x.lastUUID(), "timestamp": fmt.Sprintf("2026-02-%02dT09:30:00.000Z", n),
		"attachment": map[string]any{
			"type": "queued_command", "commandMode": mode, "content": text,
		},
	}))
}

// AddSynthetic appends an assistant-role record the CLIENT produced.
//
// No model made it. It carries a request id, so anything filtering on a missing
// request id would miss it, and anything counting it as output would attribute
// text to the agent that the agent never wrote.
func (x *transcript) AddSynthetic(text string) {
	x.turn++
	n := x.turn
	id := fmt.Sprintf("t%d-synthetic", n)
	x.lines = append(x.lines, x.rec(map[string]any{
		"type": "assistant", "uuid": id, "parentUuid": x.lastUUID(),
		"requestId": fmt.Sprintf("t%d-req-synth", n),
		"timestamp": fmt.Sprintf("2026-02-%02dT09:45:00.000Z", n),
		"message": map[string]any{
			"id": fmt.Sprintf("%08x-0000-4000-8000-000000000099", n), "type": "message",
			"role": "assistant", "model": "<synthetic>", "stop_reason": nil,
			"content": []map[string]any{{"type": "text", "text": text}},
			"usage":   usageFull(0),
		},
	}))
}

// AddCompaction appends a context reset and the summary it produced.
//
// Two measured properties are reproduced exactly, because both break naive
// implementations. The boundary's own parent is null, so the ONLY link back is
// the logical pointer. And the summary is timestamped EARLIER than the boundary
// that produced it, so anything ordering an epoch by timestamp gets it backwards.
func (x *transcript) AddCompaction() {
	x.turn++
	n := x.turn
	continuesFrom := x.lastUUID()
	boundary := fmt.Sprintf("t%d-compact-boundary", n)
	x.lines = append(x.lines,
		x.rec(map[string]any{
			"type": "system", "subtype": "compact_boundary", "uuid": boundary,
			"parentUuid": nil, "logicalParentUuid": continuesFrom,
			"timestamp": fmt.Sprintf("2026-02-%02dT11:00:00.500Z", n),
			"compactMetadata": map[string]any{
				"trigger":           "auto",
				"preservedMessages": map[string]any{"allUuids": []any{continuesFrom}},
			},
		}),
		x.rec(map[string]any{
			"type": "user", "uuid": fmt.Sprintf("t%d-compact-summary", n),
			"parentUuid": boundary, "isCompactSummary": true,
			"promptId":  fmt.Sprintf("t%d-cycle-compact", n),
			"timestamp": fmt.Sprintf("2026-02-%02dT11:00:00.100Z", n), // earlier, deliberately
			"message": map[string]any{"role": "user",
				"content": []map[string]any{{"type": "text", "text": "summary of the work so far"}}},
		}),
	)
}

// AddOrphanTool appends a tool request whose result never arrives.
//
// The session ended, or the run was interrupted. It must be kept with an
// unavailable result, not dropped and not given an empty one.
func (x *transcript) AddOrphanTool() {
	x.turn++
	n := x.turn
	x.lines = append(x.lines, x.rec(map[string]any{
		"type": "assistant", "uuid": fmt.Sprintf("t%d-orphan-call", n),
		"parentUuid": x.lastUUID(), "requestId": fmt.Sprintf("t%d-req-orphan", n),
		"timestamp": fmt.Sprintf("2026-02-%02dT12:00:00.000Z", n),
		"message": map[string]any{
			"id": fmt.Sprintf("t%d-call-orphan", n), "type": "message", "role": "assistant",
			"model": "claude-opus-5", "stop_reason": "tool_use",
			"content": []map[string]any{{"type": "tool_use",
				"id": fmt.Sprintf("t%d-tool-orphan", n), "name": "Bash",
				"input": map[string]any{"command": "sleep 1000"}}},
			"usage": usageFull(5),
		},
	}))
}

// AddSkillFork appends a fork: a child announced only by the parent's result.
//
// There is no separate pointer to the call, so the tool id has to come from the
// result's own content block. This is the mechanism with the least evidence.
func (x *transcript) AddSkillFork() string {
	x.turn++
	n := x.turn
	agent := fmt.Sprintf("a%016x", 0xf07ced0000+n)
	tool := fmt.Sprintf("t%d-tool-fork", n)
	x.lines = append(x.lines,
		x.rec(map[string]any{
			"type": "assistant", "uuid": fmt.Sprintf("t%d-fork-call", n),
			"parentUuid": x.lastUUID(), "requestId": fmt.Sprintf("t%d-req-fork", n),
			"timestamp": fmt.Sprintf("2026-02-%02dT13:00:00.000Z", n),
			"message": map[string]any{
				"id": fmt.Sprintf("t%d-call-fork", n), "type": "message", "role": "assistant",
				"model": "claude-opus-5", "stop_reason": "tool_use",
				"content": []map[string]any{{"type": "tool_use", "id": tool, "name": "Skill",
					"input": map[string]any{"skill": "collect"}}},
				"usage": usageFull(8),
			},
		}),
		x.rec(map[string]any{
			"type": "user", "uuid": fmt.Sprintf("t%d-fork-result", n),
			"parentUuid": fmt.Sprintf("t%d-fork-call", n),
			"promptId":   fmt.Sprintf("t%d-cycle-human", n),
			"timestamp":  fmt.Sprintf("2026-02-%02dT13:00:01.000Z", n),
			"message": map[string]any{"role": "user",
				"content": []map[string]any{{"tool_use_id": tool, "type": "tool_result",
					"content": "forked"}}},
			"toolUseResult": map[string]any{"agentId": agent, "status": "forked"},
		}),
	)
	x.agents[agent] = []string{
		x.rec(map[string]any{
			"type": "user", "uuid": fmt.Sprintf("t%d-fork-prompt", n), "parentUuid": nil,
			"promptId": fmt.Sprintf("t%d-fork-cycle", n), "agentId": agent, "isSidechain": true,
			"timestamp": fmt.Sprintf("2026-02-%02dT13:00:02.000Z", n),
			"message":   map[string]any{"role": "user", "content": "run the skill"},
		}),
		x.rec(map[string]any{
			"type": "assistant", "uuid": fmt.Sprintf("t%d-fork-answer", n),
			"parentUuid": fmt.Sprintf("t%d-fork-prompt", n), "agentId": agent, "isSidechain": true,
			"requestId": fmt.Sprintf("t%d-fork-req", n),
			"timestamp": fmt.Sprintf("2026-02-%02dT13:00:03.000Z", n),
			"message": map[string]any{
				"id": fmt.Sprintf("t%d-fork-call-id", n), "type": "message", "role": "assistant",
				"model": "claude-opus-5", "stop_reason": "end_turn",
				"content": []map[string]any{{"type": "text", "text": "skill finished"}},
				"usage":   usageFull(12),
			},
		}),
	}
	return agent
}

// AddStringToolResult appends a tool result whose runtime enrichment is a bare
// STRING rather than an object.
//
// This happens on a noticeable share of results. Decoding it into a struct fails
// and, because the failure is on the whole record, every identifier on that
// record is lost with it - the tool join, the cycle, the parent link.
func (x *transcript) AddStringToolResult() {
	x.turn++
	n := x.turn
	tool := fmt.Sprintf("t%d-tool-str", n)
	x.lines = append(x.lines,
		x.rec(map[string]any{
			"type": "assistant", "uuid": fmt.Sprintf("t%d-str-call", n),
			"parentUuid": x.lastUUID(), "requestId": fmt.Sprintf("t%d-req-str", n),
			"timestamp": fmt.Sprintf("2026-02-%02dT14:00:00.000Z", n),
			"message": map[string]any{
				"id": fmt.Sprintf("t%d-call-str", n), "type": "message", "role": "assistant",
				"model": "claude-opus-5", "stop_reason": "tool_use",
				"content": []map[string]any{{"type": "tool_use", "id": tool, "name": "Read",
					"input": map[string]any{"file_path": "/tmp/x"}}},
				"usage": usageFull(6),
			},
		}),
		x.rec(map[string]any{
			"type": "user", "uuid": fmt.Sprintf("t%d-str-result", n),
			"parentUuid": fmt.Sprintf("t%d-str-call", n),
			"promptId":   fmt.Sprintf("t%d-cycle-human", n),
			"timestamp":  fmt.Sprintf("2026-02-%02dT14:00:01.000Z", n),
			"message": map[string]any{"role": "user",
				"content": []map[string]any{{"tool_use_id": tool, "type": "tool_result",
					"content": "file contents"}}},
			"toolUseResult": "a bare string, not an object",
		}),
	)
}

// AddReplay appends a copy of an earlier run of records, as the runtime does
// just before it resets model context.
//
// The copies carry the same record ids and the same message content, but the
// runtime rewrites some of the envelope - including the prompt cycle - so the
// later copy is the WORSE one. Keeping the first is what preserves position and
// the captured tool output.
func (x *transcript) AddReplay(count int) {
	start := len(x.lines) - count
	if start < 0 {
		start = 0
	}
	replayed := make([]string, 0, count)
	for _, l := range x.lines[start:] {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			x.t.Fatal(err)
		}
		m["promptId"] = "replayed-cycle"
		delete(m, "toolUseResult")
		b, err := json.Marshal(m)
		if err != nil {
			x.t.Fatal(err)
		}
		replayed = append(replayed, string(b))
	}
	x.lines = append(x.lines, replayed...)
}

// lastUUID returns the id of the most recently appended record.
func (x *transcript) lastUUID() any {
	if len(x.lines) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(x.lines[len(x.lines)-1]), &m); err != nil {
		x.t.Fatal(err)
	}
	return m["uuid"]
}

// AddOrphanResult appends the result the previous unfinished tool was waiting
// for, so a test can watch a reference resolve across rounds.
func (x *transcript) AddOrphanResult() {
	n := x.turn
	tool := fmt.Sprintf("t%d-tool-orphan", n)
	x.lines = append(x.lines, x.rec(map[string]any{
		"type": "user", "uuid": fmt.Sprintf("t%d-orphan-result", n),
		"parentUuid": fmt.Sprintf("t%d-orphan-call", n),
		"promptId":   fmt.Sprintf("t%d-cycle-human", n),
		"timestamp":  fmt.Sprintf("2026-02-%02dT12:16:40.000Z", n),
		"message": map[string]any{"role": "user",
			"content": []map[string]any{{"tool_use_id": tool, "type": "tool_result",
				"content": "finally done"}}},
		"toolUseResult": map[string]any{"stdout": "finally done", "stderr": ""},
	}))
}

// AddWorkflow appends a workflow launch and the children it started.
//
// The launch result names the batch; the batch's journal names each child. The
// children's own transcripts say nothing about where they came from, which is
// why both halves have to survive.
func (x *transcript) AddWorkflow(children int) {
	x.turn++
	n := x.turn
	run := fmt.Sprintf("wf_%08x", 0xf10c0000+n)
	tool := fmt.Sprintf("t%d-tool-wf", n)
	x.lines = append(x.lines,
		x.rec(map[string]any{
			"type": "assistant", "uuid": fmt.Sprintf("t%d-wf-call", n),
			"parentUuid": x.lastUUID(), "requestId": fmt.Sprintf("t%d-req-wf", n),
			"timestamp": fmt.Sprintf("2026-02-%02dT15:00:00.000Z", n),
			"message": map[string]any{
				"id": fmt.Sprintf("t%d-call-wf", n), "type": "message", "role": "assistant",
				"model": "claude-opus-5", "stop_reason": "tool_use",
				"content": []map[string]any{{"type": "tool_use", "id": tool, "name": "Workflow",
					"input": map[string]any{"script": "export const meta = {}"}}},
				"usage": usageFull(9),
			},
		}),
		x.rec(map[string]any{
			"type": "user", "uuid": fmt.Sprintf("t%d-wf-result", n),
			"parentUuid": fmt.Sprintf("t%d-wf-call", n),
			"promptId":   fmt.Sprintf("t%d-cycle-human", n),
			"timestamp":  fmt.Sprintf("2026-02-%02dT15:00:01.000Z", n),
			"message": map[string]any{"role": "user",
				"content": []map[string]any{{"tool_use_id": tool, "type": "tool_result",
					"content": "workflow launched"}}},
			"toolUseResult": map[string]any{
				"runId": run, "status": "async_launched",
				"transcriptDir": "subagents/workflows/" + run, "workflowName": "check",
			},
		}),
	)
	for i := range children {
		agent := fmt.Sprintf("a%016x", 0xf10c00000000+n*100+i)
		x.journal[run] = append(x.journal[run],
			x.rec(map[string]any{"type": "started", "agentId": agent, "key": "v2:" + agent}))
		x.wfAgents[run+"/"+agent] = []string{
			x.rec(map[string]any{
				"type": "user", "uuid": fmt.Sprintf("t%d-wf%d-prompt", n, i), "parentUuid": nil,
				"promptId": fmt.Sprintf("t%d-wf%d-cycle", n, i), "agentId": agent, "isSidechain": true,
				"timestamp": fmt.Sprintf("2026-02-%02dT15:00:0%dZ", n, i+2),
				"message":   map[string]any{"role": "user", "content": "do part " + fmt.Sprint(i)},
			}),
			x.rec(map[string]any{
				"type": "assistant", "uuid": fmt.Sprintf("t%d-wf%d-answer", n, i),
				"parentUuid": fmt.Sprintf("t%d-wf%d-prompt", n, i), "agentId": agent, "isSidechain": true,
				"requestId": fmt.Sprintf("t%d-wf%d-req", n, i),
				"timestamp": fmt.Sprintf("2026-02-%02dT15:00:1%dZ", n, i),
				"message": map[string]any{
					"id": fmt.Sprintf("t%d-wf%d-call", n, i), "type": "message", "role": "assistant",
					"model": "claude-opus-5", "stop_reason": "end_turn",
					"content": []map[string]any{{"type": "text", "text": "part " + fmt.Sprint(i) + " done"}},
					"usage":   usageFull(7),
				},
			}),
		}
	}
}

// AddSystem appends a system record of any subtype.
func (x *transcript) AddSystem(subtype string, fields map[string]any) {
	x.turn++
	n := x.turn
	rec := map[string]any{
		"type": "system", "subtype": subtype,
		"uuid": fmt.Sprintf("t%d-sys-%s", n, subtype), "parentUuid": x.lastUUID(),
		"timestamp": fmt.Sprintf("2026-02-%02dT16:00:00.000Z", n),
	}
	for k, v := range fields {
		rec[k] = v
	}
	x.lines = append(x.lines, x.rec(rec))
}
