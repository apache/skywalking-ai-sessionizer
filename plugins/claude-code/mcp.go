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

package main

import (
	"time"

	"github.com/apache/skywalking-ai-sessionizer/pkg/execution"
	"github.com/apache/skywalking-ai-sessionizer/plugins/claude-code/internal/hook"
	"github.com/apache/skywalking-ai-sessionizer/plugins/claude-code/internal/output"
)

// execution records one call to an MCP server, from the hook that runs after
// it.
//
// The hook sees what the transcript does not: which server ran the call and
// where its configuration came from, how long the runtime waited for it, and
// what was sent once every hook before the call had run. Measured on Claude
// Code 2.1.282, the input this hook sees is what the server received, which a
// hook before the call can change while the transcript keeps the model's own.
// Only the size and a digest of the input and the answer are kept: the text
// is in the transcript, or, when rewritten, not the plugin's to keep.
//
// The record's id is the call's id and where it was observed, so the same
// event handed over twice is one record, and another observer of the same
// call is another.
func (p *plugin) execution() error {
	in := p.in
	r := &execution.Record{
		Schema:     execution.Schema,
		ID:         in.ToolUseID + "/" + execution.BoundaryClientHook,
		ObservedBy: execution.ObservedByASZPlugin,
		Boundary:   execution.BoundaryClientHook,
		Session:    in.SessionID,
		Stream:     in.Stream(),
		Tool:       in.ToolUseID,
		ToolName:   in.ToolName,
		Cwd:        in.Cwd,
		Protocol:   execution.ProtocolMCP,
		Server:     &execution.Server{Name: in.MCPServer.Name, Source: in.MCPServer.Source},
		Time:       p.now.UTC().Format(time.RFC3339Nano),
		DurationMS: in.DurationMS,
		Outcome:    outcomeOf(in),
	}
	if len(in.ToolInput) > 0 {
		r.Arguments = execution.Measure(in.ToolInput)
	}
	if in.Event == hook.PostToolUse && len(in.ToolResponse) > 0 {
		r.Result = execution.Measure(in.ToolResponse)
	}
	return output.AppendExecution(p.dataDir, r)
}

// outcomeOf reads how the call ended. The runtime runs the failure hook for
// an error the server returned, a lost connection and a timeout alike, all
// measured on Claude Code 2.1.282, and says when a person stopped the call.
func outcomeOf(in *hook.Input) string {
	switch {
	case in.Event == hook.PostToolUse:
		return execution.OutcomeReturned
	case in.IsInterrupt != nil && *in.IsInterrupt:
		return execution.OutcomeInterrupted
	}
	return execution.OutcomeFailed
}
