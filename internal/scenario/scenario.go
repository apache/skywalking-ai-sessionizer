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

// Package scenario turns a short YAML description of a conversation into the
// input a session leaves behind: a runtime's own files, for an adapter to
// collect, or Session Data written directly in the model's vocabulary. The
// rounds are never written here; the ordinary parser makes them.
//
// A scenario speaks the model's words, not a runtime's. Every step maps to one
// shape of record, and the same scenario built through every writer must fold
// to the same conversation, which is what makes it a conformance test for an
// adapter as well as a fixture generator.
package scenario

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Scenario is one session, described as the steps of its main stream.
type Scenario struct {
	// Session is the session id. Empty means one derived from the steps, so
	// the same scenario always names the same session.
	Session string `yaml:"session"`
	// Project is the working directory the session ran in, as a runtime
	// would slug it. It names the project directory of the runtime's layout.
	Project string `yaml:"project"`
	// Title is the name the runtime gave the session, written as the
	// runtime writes it. Empty means the session has no title.
	Title string `yaml:"title"`
	// Interval is the session's clock: the gap between consecutive steps in
	// every stream, unless a step says after. Zero means one second.
	Interval time.Duration `yaml:"interval"`
	Steps    []Step        `yaml:"steps"`
}

// Step is one thing that happened. Exactly one of the kind fields is set.
type Step struct {
	// After is the delta since the previous step in the same stream. Zero
	// means the scenario's interval.
	After time.Duration `yaml:"after"`
	// Checkpoint names a point a build can stop at, so a test can land what
	// exists so far, parse it, and check the fold before the rest arrives.
	Checkpoint string `yaml:"checkpoint"`

	// Input is a person's message: it opens a run and a talk.
	Input string `yaml:"input"`
	// Queued is input that exists only as a queued command attachment.
	Queued *Queued `yaml:"queued"`
	// Inject is material the harness put into model context.
	Inject *Inject `yaml:"inject"`
	// Call is one provider call, in fragments.
	Call *Call `yaml:"call"`
	// Result is a tool result arriving on its own, for a tool whose call
	// gave none: the late result a test watches resolve.
	Result *Result `yaml:"result"`
	// Error is an assistant-role message the client fabricated.
	Error string `yaml:"error"`
	// Reset is a context reset and the summary that replaced the context.
	Reset *Reset `yaml:"reset"`
	// Replay re-emits the last N records of the main stream, as the runtime
	// does before it resets context, with a rewritten run.
	Replay int `yaml:"replay"`
	// System is a system record of any subtype.
	System *System `yaml:"system"`
}

// Queued is input that exists only as an attachment. Mode is what the
// runtime says the queued command is: "prompt" is a person's input,
// "task-notification" is not.
type Queued struct {
	Text string `yaml:"text"`
	Mode string `yaml:"mode"`
}

// Inject is material the harness put into model context, of a named type.
type Inject struct {
	Type string `yaml:"type"`
	Text string `yaml:"text"`
}

// Call is one provider call. Its fragments are written in this order:
// thinking, text, then the tool, agent, skill or workflow request. The last
// fragment carries the stop reason and every fragment repeats the usage, as
// a main transcript does.
type Call struct {
	// Thinking is a reasoning part. "unavailable" writes one without text,
	// as a runtime that keeps only the signature does; any other value is
	// the text.
	Thinking string `yaml:"thinking"`
	Text     string `yaml:"text"`
	Tool     *Tool  `yaml:"tool"`
	Agent    *Agent `yaml:"agent"`
	Skill    *Skill `yaml:"skill"`
	// Workflow starts children as a batch, with a journal, a manifest and a
	// script filed with the run.
	Workflow *Workflow `yaml:"workflow"`
	Usage    *Usage    `yaml:"usage"`
}

// Usage is what the provider reported. Zero fields take the defaults a
// runtime reports on a small call.
type Usage struct {
	In         int `yaml:"in"`
	Out        int `yaml:"out"`
	CacheRead  int `yaml:"cache_read"`
	CacheWrite int `yaml:"cache_write"`
}

// Tool is a request to run something. A tool without a result is an
// unfinished tool; a later Result step may supply it.
type Tool struct {
	ID     string         `yaml:"id"`
	Name   string         `yaml:"name"`
	Input  map[string]any `yaml:"input"`
	Result *Result        `yaml:"result"`
}

// Result is what a tool returned. In a Tool it may be written as a plain
// string. As a step of its own it names the tool with Of.
type Result struct {
	Of    string        `yaml:"of"`
	Text  string        `yaml:"text"`
	After time.Duration `yaml:"after"`
	// Failed is what the runtime said, when it said anything.
	Failed *bool `yaml:"failed"`
	// String writes the runtime's enrichment as a bare string rather than
	// an object, a shape a real corpus has on a noticeable share of results.
	String bool `yaml:"string"`
}

// UnmarshalYAML lets a result be a plain string.
func (r *Result) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		r.Text = n.Value
		return nil
	}
	type plain Result
	var p plain
	if err := n.Decode(&p); err != nil {
		return err
	}
	*r = Result(p)
	return nil
}

// Agent is a request to start a child agent: the call, a launch
// acknowledgement, the child's own stream, and, when Notify is set, the
// runtime reporting the child finished, which continues the talk in a new
// run.
type Agent struct {
	Name   string `yaml:"name"`
	Prompt string `yaml:"prompt"`
	// After is the delta from the request to the child's first record.
	After  time.Duration `yaml:"after"`
	Steps  []Step        `yaml:"steps"`
	Notify bool          `yaml:"notify"`
}

// Skill is a fork: a child announced only by the parent's result block,
// with no launch acknowledgement and no notification.
type Skill struct {
	Name  string `yaml:"name"`
	Agent string `yaml:"agent"`
	Steps []Step `yaml:"steps"`
}

// Workflow starts several children as one batch.
type Workflow struct {
	Name     string  `yaml:"name"`
	Children []Child `yaml:"children"`
}

// Child is one workflow child.
type Child struct {
	Name   string `yaml:"name"`
	Prompt string `yaml:"prompt"`
	Steps  []Step `yaml:"steps"`
}

// Reset is a context reset and the summary that replaced the context.
type Reset struct {
	Summary string `yaml:"summary"`
}

// System is a system record: its subtype and any fields.
type System struct {
	Subtype string         `yaml:"subtype"`
	Fields  map[string]any `yaml:"fields"`
}

// Load reads and validates a scenario file.
func Load(path string) (*Scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// An unknown key is an error, not silence: a misplaced "after" would
	// otherwise change the clock without a word.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var sc Scenario
	if err := dec.Decode(&sc); err != nil {
		return nil, fmt.Errorf("scenario: %s: %w", path, err)
	}
	if err := sc.Validate(); err != nil {
		return nil, fmt.Errorf("scenario: %s: %w", path, err)
	}
	return &sc, nil
}

// Validate reports a scenario a writer cannot express.
func (sc *Scenario) Validate() error {
	if len(sc.Steps) == 0 {
		return errors.New("no steps")
	}
	if sc.Interval < 0 {
		return errors.New("interval must not be negative")
	}
	seen := map[string]bool{}
	return validateSteps(sc.Steps, "steps", seen, true)
}

func validateSteps(steps []Step, where string, seen map[string]bool, main bool) error {
	for i := range steps {
		s := &steps[i]
		at := fmt.Sprintf("%s[%d]", where, i)
		n := 0
		for _, set := range []bool{s.Input != "", s.Queued != nil, s.Inject != nil, s.Call != nil,
			s.Result != nil, s.Error != "", s.Reset != nil, s.Replay > 0, s.System != nil} {
			if set {
				n++
			}
		}
		if n != 1 {
			return fmt.Errorf("%s: a step is exactly one of input, queued, inject, call, result, error, reset, replay, system", at)
		}
		if s.After < 0 {
			return fmt.Errorf("%s: after must not be negative", at)
		}
		if s.Checkpoint != "" {
			if !main {
				return fmt.Errorf("%s: a checkpoint belongs to the main stream", at)
			}
			if seen[s.Checkpoint] {
				return fmt.Errorf("%s: checkpoint %q is named twice", at, s.Checkpoint)
			}
			seen[s.Checkpoint] = true
		}
		if s.Queued != nil && s.Queued.Mode != "prompt" && s.Queued.Mode != "task-notification" {
			return fmt.Errorf("%s: queued mode is prompt or task-notification", at)
		}
		if s.Result != nil && s.Result.Of == "" {
			return fmt.Errorf("%s: a result step names the tool it answers with of", at)
		}
		if s.Replay > 0 && !main {
			return fmt.Errorf("%s: replay belongs to the main stream", at)
		}
		if s.Reset != nil && !main {
			return fmt.Errorf("%s: a reset belongs to the main stream", at)
		}
		if c := s.Call; c != nil {
			kinds := 0
			for _, set := range []bool{c.Tool != nil, c.Agent != nil, c.Skill != nil, c.Workflow != nil} {
				if set {
					kinds++
				}
			}
			if kinds > 1 {
				return fmt.Errorf("%s: a call requests at most one of tool, agent, skill, workflow", at)
			}
			if c.Tool != nil && c.Tool.Name == "" {
				return fmt.Errorf("%s: a tool has a name", at)
			}
			if c.Agent != nil {
				if c.Agent.Name == "" {
					return fmt.Errorf("%s: an agent has a name", at)
				}
				if err := validateSteps(c.Agent.Steps, at+".agent.steps", seen, false); err != nil {
					return err
				}
			}
			if c.Skill != nil {
				if c.Skill.Name == "" || c.Skill.Agent == "" {
					return fmt.Errorf("%s: a skill has a name and an agent", at)
				}
				if err := validateSteps(c.Skill.Steps, at+".skill.steps", seen, false); err != nil {
					return err
				}
			}
			if c.Workflow != nil {
				if c.Workflow.Name == "" || len(c.Workflow.Children) == 0 {
					return fmt.Errorf("%s: a workflow has a name and children", at)
				}
				for j, ch := range c.Workflow.Children {
					if ch.Name == "" {
						return fmt.Errorf("%s: workflow child %d has a name", at, j)
					}
					if err := validateSteps(ch.Steps, fmt.Sprintf("%s.workflow.children[%d].steps", at, j), seen, false); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// Checkpoints lists the scenario's checkpoints in order.
func (sc *Scenario) Checkpoints() []string {
	var out []string
	for _, s := range sc.Steps {
		if s.Checkpoint != "" {
			out = append(out, s.Checkpoint)
		}
	}
	return out
}
