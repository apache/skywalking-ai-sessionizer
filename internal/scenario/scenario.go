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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	// Entrypoint is how the runtime was started, as it records it. Empty is
	// the interactive command line. "sdk-cli" is a headless run, claude -p or
	// an Agent SDK application: every input of the main stream is then the
	// caller's prompt, which carries promptSource "sdk" and no origin, and is
	// sent as a plain string.
	Entrypoint string `yaml:"entrypoint"`
	// Interval is the session's clock: the gap between consecutive steps in
	// every stream, unless a step says after. Zero means one second.
	Interval time.Duration `yaml:"interval"`
	// ProviderBodies writes the request and response body of every provider
	// call, as Claude Code does when OTEL_LOG_RAW_API_BODIES names a
	// directory. A claude-code build writes them as files; an sd build lands
	// them.
	ProviderBodies bool `yaml:"provider_bodies"`
	// SystemPrompt is the system prompt the main stream sends, written as the
	// runtime sends it. Empty leaves the stand-in, which is filler sized to
	// exercise cutting rather than something to read.
	SystemPrompt string `yaml:"system_prompt"`
	// Tools are the tools the main stream advertises, in the order it sends
	// them. Empty leaves the stand-in. See ToolDef for why they are written
	// out rather than derived from the calls.
	Tools []ToolDef `yaml:"tools"`
	Steps []Step    `yaml:"steps"`

	// omitBodies writes no provider bodies while keeping everything else a
	// scenario with provider bodies has, its ids included, so the two builds
	// can be compared.
	omitBodies bool
}

// ToolDef is one tool a request advertises: the name the model calls, what
// the tool is told to be, and the shape of its input.
//
// A scenario writes its own, and shares none with another scenario. A request
// body is read as a document, and a borrowed description describes the wrong
// agent: the same name means a different thing to a release manager and to an
// inbox assistant. Deriving one from the calls cannot work either, because a
// description is prose about what the tool is for, which no call carries.
type ToolDef struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description"`
	InputSchema map[string]any `yaml:"input_schema"`
}

// Step is one thing that happened. Exactly one of the kind fields is set.
type Step struct {
	// After is the delta since the previous step in the same stream. Zero
	// means the scenario's interval.
	After time.Duration `yaml:"after"`
	// Checkpoint names a point a build can stop at, so a test can land what
	// exists so far, parse it, and check the fold before the rest arrives.
	Checkpoint string `yaml:"checkpoint"`
	// Lost says the original file does not hold what this step wrote: a
	// person trimmed the transcript, or the write never reached the disk.
	// The step still happened, so the clock and the ids move as if the
	// records were there, and the record after them still names them as
	// its parent. A writer leaves them out, in either format, and the
	// collector lands what exists.
	Lost bool `yaml:"lost"`

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
	// Changes are the files the tool changed, each as its content before
	// and after. For Edit, Write and NotebookEdit they are written the way
	// the runtime records its own patch, on the result; for any other tool
	// they are written the way the plugin records what it observed, as a
	// change record beside the stream. Both land as changes/1 and both
	// join the step, so the same scenario checks the two paths.
	Changes []Change `yaml:"changes"`
}

// Change is one file a tool changed. A side that is absent, nil in YAML or
// left out, means the file did not exist on that side.
type Change struct {
	Path   string  `yaml:"path"`
	Before *string `yaml:"before"`
	After  *string `yaml:"after"`
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
	// Lost says the result never reached the file, while the call did.
	Lost bool `yaml:"lost"`
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
	After time.Duration `yaml:"after"`
	// SystemPrompt and Tools are what this child's own calls send. A child
	// is given a narrower set than its parent, which is the point of
	// starting one, so it writes its own rather than inheriting.
	SystemPrompt string    `yaml:"system_prompt"`
	Tools        []ToolDef `yaml:"tools"`
	Steps        []Step    `yaml:"steps"`
	Notify       bool      `yaml:"notify"`
	// Lost says the child's file never reached the collector, nor its meta
	// file. The parent's records about the child stay.
	Lost bool `yaml:"lost"`
}

// Skill is a fork: a child announced only by the parent's result block,
// with no launch acknowledgement and no notification.
type Skill struct {
	Name  string `yaml:"name"`
	Agent string `yaml:"agent"`
	// SystemPrompt and Tools are what this fork's own calls send.
	SystemPrompt string    `yaml:"system_prompt"`
	Tools        []ToolDef `yaml:"tools"`
	Steps        []Step    `yaml:"steps"`
	// Lost says the child's file never reached the collector.
	Lost bool `yaml:"lost"`
}

// Workflow starts several children as one batch. ScriptProject files the
// script under another project directory, as a real corpus does, so
// discovery must group by session across directories.
type Workflow struct {
	Name          string  `yaml:"name"`
	Children      []Child `yaml:"children"`
	ScriptProject string  `yaml:"script_project"`
}

// Child is one workflow child.
type Child struct {
	Name   string `yaml:"name"`
	Prompt string `yaml:"prompt"`
	// SystemPrompt and Tools are what this child's own calls send.
	SystemPrompt string    `yaml:"system_prompt"`
	Tools        []ToolDef `yaml:"tools"`
	Steps        []Step    `yaml:"steps"`
	// Lost says the child's file never reached the collector. The run's
	// journal still names the child.
	Lost bool `yaml:"lost"`
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
	if err := validateTools(sc.Tools, "tools"); err != nil {
		return err
	}
	if err := bothOrNeither(sc.SystemPrompt, sc.Tools, "the scenario"); err != nil {
		return err
	}
	seen := map[string]bool{}
	return validateSteps(sc.Steps, "steps", seen, true, advertised(sc.Tools))
}

// validateTools checks one advertised tool list. A name is required, and no
// name is advertised twice: a provider is sent one definition per name, and a
// repeat would quietly shadow the other.
//
// A description is not required. A runtime may declare a tool without one, and
// a scenario has to be able to write what a runtime sends.
func validateTools(tools []ToolDef, where string) error {
	seen := map[string]bool{}
	for i, t := range tools {
		at := fmt.Sprintf("%s[%d]", where, i)
		if t.Name == "" {
			return fmt.Errorf("%s: a tool needs a name", at)
		}
		if seen[t.Name] {
			return fmt.Errorf("%s: %s is advertised twice", at, t.Name)
		}
		seen[t.Name] = true
		if err := writableAsJSON(t.InputSchema); err != nil {
			return fmt.Errorf("%s: %s: input_schema %w", at, t.Name, err)
		}
	}
	return nil
}

// bothOrNeither refuses a stream that writes one of its system prompt and its
// tools without the other.
//
// Each falls back on its own, so half a declaration leaves the other half as
// the stand-in: an authored calendar prompt beside the stand-in's Read and
// Bash, which is the very thing writing them is meant to stop. Writing neither
// is fine and keeps the stand-in whole.
func bothOrNeither(system string, tools []ToolDef, who string) error {
	if system != "" && tools == nil {
		return fmt.Errorf("%s writes a system_prompt but no tools; write both or neither", who)
	}
	if system == "" && tools != nil {
		return fmt.Errorf("%s writes tools but no system_prompt; write both or neither", who)
	}
	return nil
}

// writableAsJSON reports whether a value a scenario supplied can be written
// into a body.
//
// A body is written with encoding/json, which refuses an infinity and a NaN --
// both of which YAML writes as plain scalars, `.inf` and `.nan`. The writer
// cannot report that failure, so such a value would leave an empty body and
// surface much later as a digest that does not match. Refusing it here names
// the tool and the file instead.
func writableAsJSON(v any) error {
	if v == nil {
		return nil
	}
	if _, err := json.Marshal(v); err != nil {
		return fmt.Errorf("cannot be written as JSON: %w", err)
	}
	return nil
}

// advertised is the set of tool names a stream sends, or nil when it writes
// none. A stream that writes none is not checked against its calls.
func advertised(tools []ToolDef) map[string]bool {
	if len(tools) == 0 {
		return nil
	}
	out := map[string]bool{}
	for _, t := range tools {
		out[t.Name] = true
	}
	return out
}

// calls reports the tool a step invokes, as the writers name it. Starting a
// child is a tool call like any other: the response carries a tool_use block
// named Agent, Skill or Workflow.
func calls(c *Call) string {
	switch {
	case c.Tool != nil:
		return c.Tool.Name
	case c.Agent != nil:
		return "Agent"
	case c.Skill != nil:
		return "Skill"
	case c.Workflow != nil:
		return "Workflow"
	}
	return ""
}

func validateSteps(steps []Step, where string, seen map[string]bool, main bool, sent map[string]bool) error {
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
		if s.Replay > 0 && s.Lost {
			return fmt.Errorf("%s: a replay copies what the runtime holds in memory; mark the original step lost instead", at)
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
			// A model cannot call a tool the request did not offer it, so a
			// stream that says what it advertises must advertise what it calls.
			if name := calls(c); sent != nil && name != "" && !sent[name] {
				return fmt.Errorf("%s: %s is called but not advertised; add it to this stream's tools", at, name)
			}
			if c.Tool != nil {
				if c.Tool.Name == "" {
					return fmt.Errorf("%s: a tool has a name", at)
				}
				// A tool's input is written into a body by the same encoder.
				if err := writableAsJSON(c.Tool.Input); err != nil {
					return fmt.Errorf("%s: %s: input %w", at, c.Tool.Name, err)
				}
			}
			if c.Agent != nil {
				if c.Agent.Name == "" {
					return fmt.Errorf("%s: an agent has a name", at)
				}
				if err := validateTools(c.Agent.Tools, at+".agent.tools"); err != nil {
					return err
				}
				if err := bothOrNeither(c.Agent.SystemPrompt, c.Agent.Tools, at+".agent"); err != nil {
					return err
				}
				if err := validateSteps(c.Agent.Steps, at+".agent.steps", seen, false, advertised(c.Agent.Tools)); err != nil {
					return err
				}
			}
			if c.Skill != nil {
				if c.Skill.Name == "" || c.Skill.Agent == "" {
					return fmt.Errorf("%s: a skill has a name and an agent", at)
				}
				if err := validateTools(c.Skill.Tools, at+".skill.tools"); err != nil {
					return err
				}
				if err := bothOrNeither(c.Skill.SystemPrompt, c.Skill.Tools, at+".skill"); err != nil {
					return err
				}
				if err := validateSteps(c.Skill.Steps, at+".skill.steps", seen, false, advertised(c.Skill.Tools)); err != nil {
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
					if err := validateTools(ch.Tools, fmt.Sprintf("%s.workflow.children[%d].tools", at, j)); err != nil {
						return err
					}
					if err := bothOrNeither(ch.SystemPrompt, ch.Tools, fmt.Sprintf("%s.workflow.children[%d]", at, j)); err != nil {
						return err
					}
					if err := validateSteps(ch.Steps, fmt.Sprintf("%s.workflow.children[%d].steps", at, j), seen, false, advertised(ch.Tools)); err != nil {
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

// Loaded is one scenario and the file it came from, so a feed can name the
// scenario it is emitting.
type Loaded struct {
	Path     string
	Scenario *Scenario
}

// LoadSet loads every scenario named by paths. A path that names a
// directory contributes every ".yaml" file directly inside it, except the
// expectation files, which are not scenarios. The result keeps the order
// the paths were given, and directory entries are sorted, so a fixed list
// is emitted in a fixed order.
func LoadSet(paths []string) ([]Loaded, error) {
	var out []Loaded
	seen := map[string]bool{}
	for _, p := range paths {
		files, err := scenarioFiles(p)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			abs, err := filepath.Abs(f)
			if err != nil {
				return nil, err
			}
			if seen[abs] {
				continue
			}
			seen[abs] = true
			sc, err := Load(f)
			if err != nil {
				return nil, err
			}
			out = append(out, Loaded{Path: f, Scenario: sc})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("scenario: no scenario files given")
	}
	return out, nil
}

// scenarioFiles expands one path into the scenario files it names.
func scenarioFiles(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{path}, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".expect.yaml") {
			continue
		}
		files = append(files, filepath.Join(path, name))
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, fmt.Errorf("scenario: %s holds no scenario files", path)
	}
	return files, nil
}
