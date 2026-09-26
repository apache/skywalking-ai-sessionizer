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
	"time"

	"github.com/apache/skywalking-ai-sessionizer/pkg/execution"
)

// Execution is what the plugin's hook saw of one call to an MCP server.
type Execution struct {
	Server string `yaml:"server"`
	Source string `yaml:"source"`
	// Outcome is returned, failed or interrupted. Empty follows the result:
	// failed when the result failed, returned otherwise.
	Outcome  string        `yaml:"outcome"`
	Duration time.Duration `yaml:"duration"`
	// Arguments are what the server was sent, when a hook before the call
	// changed them. Empty means the tool's own input.
	Arguments map[string]any `yaml:"arguments"`
	// Twice writes the record a second time, as the plugin does when the
	// runtime hands its hook the same event again. It is one call.
	Twice bool `yaml:"twice"`
}

// executionRecord is the record the plugin writes for a call to an MCP
// server, from the hook after it.
func (p *Plan) executionRecord(e *Event) *execution.Record {
	x := e.Execution
	outcome := x.Outcome
	if outcome == "" {
		outcome = execution.OutcomeReturned
		if e.Failed != nil && *e.Failed {
			outcome = execution.OutcomeFailed
		}
	}
	args := x.Arguments
	if args == nil {
		args = e.ToolInput
	}
	if args == nil {
		args = map[string]any{}
	}
	r := &execution.Record{
		Schema: execution.Schema, ID: e.Of + "/" + execution.BoundaryClientHook,
		ObservedBy: execution.ObservedByASZPlugin, Boundary: execution.BoundaryClientHook,
		Session: p.Session, Stream: e.Stream, Tool: e.Of, ToolName: e.ToolName, Cwd: p.Cwd(),
		Protocol: execution.ProtocolMCP, Server: &execution.Server{Name: x.Server, Source: x.Source},
		Time: ccTime(e.At), Outcome: outcome,
		Arguments: execution.Measure(jsLine(args)),
	}
	if x.Duration > 0 {
		ms := x.Duration.Milliseconds()
		r.DurationMS = &ms
	}
	if outcome == execution.OutcomeReturned {
		// The runtime hands the hook the MCP answer's content blocks.
		answer, _ := json.Marshal([]map[string]any{{"type": "text", "text": e.Text}})
		r.Result = execution.Measure(answer)
	}
	return r
}

// HasExecutions reports whether any tool in the steps has an execution.
func HasExecutions(steps []Step) bool {
	return anyTool(steps, func(t *Tool) bool { return t.Execution != nil })
}

// WithoutExecutions is the scenario with every execution removed, so it can
// be built again and the two folds compared.
func (sc *Scenario) WithoutExecutions() *Scenario {
	out := *sc
	out.Steps = mapTools(sc.Steps, func(t *Tool) { t.Execution = nil })
	return &out
}
