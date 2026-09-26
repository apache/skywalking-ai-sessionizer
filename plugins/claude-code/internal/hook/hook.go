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

// Package hook reads what Claude Code hands a hook on its standard input.
//
// The fields below are the ones a run of Claude Code 2.1.260 was seen to
// write, read from a logging plugin's output rather than from
// documentation: the event, the session, the agent inside a subagent, the
// tool and its call id, the tool's input and, after the call, its response
// or its error. Anything else is left where it is.
package hook

import (
	"encoding/json"
	"fmt"
	"io"
)

// Events this plugin acts on, in its own vocabulary.
//
// The names are Claude Code's, because that is the runtime this was written
// for and changing them would break every installed manifest for nothing. What
// matters is that they are no longer the ONLY names: see neutral below.
const (
	PreToolUse         = "PreToolUse"
	PostToolUse        = "PostToolUse"
	PostToolUseFailure = "PostToolUseFailure"
	SessionStart       = "SessionStart"
	SessionEnd         = "SessionEnd"
)

// neutral maps a runtime-neutral event name onto the one this acts on.
//
// This program records what a tool call changed, which is not a Claude Code
// idea. Another runtime's integration should not have to send "PreToolUse" to
// say a tool began, so it can say so plainly instead. The two vocabularies
// mean the same thing and take the same path.
var neutral = map[string]string{
	"tool.begin":    PreToolUse,
	"tool.end":      PostToolUse,
	"tool.failed":   PostToolUseFailure,
	"session.begin": SessionStart,
	"session.end":   SessionEnd,
}

// Neutral returns the neutral name for an event, for a caller writing one.
func Neutral(event string) string {
	for name, mapped := range neutral {
		if mapped == event {
			return name
		}
	}
	return event
}

// Input is one hook invocation.
type Input struct {
	Event     string `json:"hook_event_name"`
	SessionID string `json:"session_id"`
	// AgentID is set on every event inside a subagent, and is the id in
	// the subagent transcript's file name. Empty on the main stream.
	AgentID   string `json:"agent_id"`
	AgentType string `json:"agent_type"`
	Cwd       string `json:"cwd"`
	PromptID  string `json:"prompt_id"`

	ToolName  string          `json:"tool_name"`
	ToolUseID string          `json:"tool_use_id"`
	ToolInput json.RawMessage `json:"tool_input"`
	// ToolResponse is on PostToolUse; Error on PostToolUseFailure, holding
	// the exit code and the standard error text for a shell command.
	ToolResponse json.RawMessage `json:"tool_response"`
	Error        string          `json:"error"`

	// MCPServer is set on every event of a call to an MCP server: the
	// server's name and where its configuration came from. DurationMS is on
	// the events after a call, the runtime's own measure around it, and
	// IsInterrupt on a failure a person caused. All three measured on Claude
	// Code 2.1.282.
	MCPServer   *MCPServer `json:"mcp_server"`
	DurationMS  *int64     `json:"duration_ms"`
	IsInterrupt *bool      `json:"is_interrupt"`
}

// MCPServer is the MCP server a call went to, as the runtime names it.
type MCPServer struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

// Read decodes one invocation.
func Read(r io.Reader) (*Input, error) {
	var in Input
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		return nil, fmt.Errorf("hook: read input: %w", err)
	}
	if in.Event == "" {
		return nil, fmt.Errorf("hook: input names no event")
	}
	if mapped, ok := neutral[in.Event]; ok {
		in.Event = mapped
	}
	return &in, nil
}

// Stream is the stream the call ran in: main, or the subagent's id.
func (in *Input) Stream() string {
	if in.AgentID != "" {
		return in.AgentID
	}
	return "main"
}

// Command is the command text of a shell tool's input, or empty.
func (in *Input) Command() string {
	var ti struct {
		Command string `json:"command"`
	}
	if len(in.ToolInput) == 0 || json.Unmarshal(in.ToolInput, &ti) != nil {
		return ""
	}
	return ti.Command
}

// ExitCode reads the exit code a failure reports, when it reports one.
// The runtime writes "Exit code N" ahead of the standard error text.
func (in *Input) ExitCode() (int, bool) {
	var n int
	if _, err := fmt.Sscanf(in.Error, "Exit code %d", &n); err != nil {
		return 0, false
	}
	return n, true
}
