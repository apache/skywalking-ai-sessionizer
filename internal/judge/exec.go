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

package judge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
)

// Task is one stage given to an evaluator.
type Task struct {
	Stage  string
	Prompt string
	Schema json.RawMessage
	// Dir is the stage's own directory, where the evaluator runs and
	// where what it was given and said is kept.
	Dir string
	// MCP starts the navigation server for an agent CLI: the program and
	// its arguments.
	MCP []string
	// Nav answers an API evaluator's tool calls in this process.
	Nav *Nav
	// Progress, when set, is told of each model request as it ends, so a
	// long stage can say what it has consumed so far.
	Progress func(Request)
}

// Request is one model request inside an evaluator call, as the provider
// counted it. Whole says the usage is of the whole call, for a CLI that
// reports only that, as codex does.
type Request struct {
	Model string            `json:"model,omitempty"`
	Usage sessiondata.Usage `json:"usage"`
	Whole bool              `json:"whole,omitempty"`
}

// Result is what an evaluator gave.
type Result struct {
	// Output is the answer, one JSON object.
	Output json.RawMessage
	// Usage is what the evaluator consumed, when it said. Nil is unknown.
	Usage   *sessiondata.Usage
	Credits *float64
	// Model is the model that did the work. A CLI can use a second, small
	// model for its own chores; ByModel then holds each model's usage, so
	// each is priced at its own rate.
	Model   string
	ByModel map[string]sessiondata.Usage
	// CostUSD is what the evaluator itself said the stage cost, in US
	// dollars, when it said.
	CostUSD *float64
	Version string
	// Requests are the model requests of the call, as they ended, and
	// ContextWindow the window of the model that did the work, when the
	// evaluator said.
	Requests      []Request
	ContextWindow int
	// UnknownRequests counts the model requests whose usage the provider
	// never said, as one whose answer was lost on the way: Usage holds the
	// others, and says nothing of these.
	UnknownRequests int
	// InProcess says the evaluator read through the run's own navigation
	// and delivered its receipts there, as an API evaluator does; an agent
	// CLI reads through a server of its own, whose log holds them.
	InProcess bool
}

// Executor runs a stage.
type Executor interface {
	Run(ctx context.Context, t Task) (Result, error)
	// Describe says who the evaluator is, for the stage file's header.
	Describe() sessionjudge.Evaluator
}

// NewExecutor makes the executor a configured evaluator names.
func NewExecutor(name string, e config.Evaluator) (Executor, error) {
	d := sessionjudge.Evaluator{Name: name, Kind: e.Kind, Program: e.Program, Model: e.Model}
	switch e.Kind {
	case sessionjudge.EvaluatorCLI:
		switch e.Program {
		case "claude", "codex", "kiro-cli":
			return &cliExecutor{desc: d, conf: e}, nil
		}
		return nil, fmt.Errorf("judge: evaluator %s: no CLI %q; claude, codex and kiro-cli are known", name, e.Program)
	case sessionjudge.EvaluatorAPI:
		switch e.Program {
		case "anthropic", "openai":
			if e.Model == "" || e.Endpoint == "" {
				return nil, fmt.Errorf("judge: evaluator %s: an API needs endpoint and model", name)
			}
			return &apiExecutor{desc: d, conf: e}, nil
		case "bedrock":
			if e.Model == "" {
				return nil, fmt.Errorf("judge: evaluator %s: Bedrock needs a model id", name)
			}
			return &apiExecutor{desc: d, conf: e}, nil
		}
		return nil, fmt.Errorf("judge: evaluator %s: no API %q; anthropic, openai and bedrock are known", name, e.Program)
	}
	return nil, fmt.Errorf("judge: evaluator %s: kind %q is neither cli nor api", name, e.Kind)
}

// cliExecutor runs an agent CLI with the navigation server as its only tools.
type cliExecutor struct {
	desc sessionjudge.Evaluator
	conf config.Evaluator
}

func (c *cliExecutor) Describe() sessionjudge.Evaluator { return c.desc }

// pipeWait is how long a CLI's pipes are drained once it exits, as Run
// says where it sets it; a variable so a test can make it short.
var pipeWait = 10 * time.Second

// makeKiroAgentDir makes the stage's .kiro/agents one level at a time, never
// with MkdirAll: the stage's directory is inside the session's, and a session
// retired meanwhile, its directory gone, must not be made again.
func makeKiroAgentDir(stage string) (string, error) {
	agentDir := filepath.Join(stage, ".kiro", "agents")
	for _, d := range []string{filepath.Dir(agentDir), agentDir} {
		if err := os.Mkdir(d, 0o700); err != nil && !os.IsExist(err) {
			return "", err
		}
	}
	return agentDir, nil
}

func (c *cliExecutor) Run(ctx context.Context, t Task) (Result, error) {
	prog := c.conf.Command
	if prog == "" {
		prog = c.conf.Program
	}
	if err := os.WriteFile(filepath.Join(t.Dir, "prompt.md"), []byte(t.Prompt), 0o600); err != nil {
		return Result{}, err
	}
	schemaPath := filepath.Join(t.Dir, "schema.json")
	if err := os.WriteFile(schemaPath, t.Schema, 0o600); err != nil {
		return Result{}, err
	}
	var args, last []string
	var stdin []byte
	answerPath := filepath.Join(t.Dir, "answer.json")
	switch c.conf.Program {
	case "claude":
		cfg, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{MCPServerName: map[string]any{
			"type": "stdio", "command": t.MCP[0], "args": t.MCP[1:]}}})
		mcpPath := filepath.Join(t.Dir, "mcp.json")
		if err := os.WriteFile(mcpPath, cfg, 0o600); err != nil {
			return Result{}, err
		}
		// No built-in tool, no other MCP server, no user or project
		// settings (and so no hooks or plugins), and no session written to
		// disk: the evaluator can read the conversation through asz and do
		// nothing else, and its own session is not collected as a
		// conversation of its own.
		// stream-json, with each request's start and end, counts every
		// request exactly as the provider did, while the call runs.
		args = []string{"-p", "--output-format", "stream-json", "--verbose", "--include-partial-messages", "--json-schema", string(compact(t.Schema)),
			"--mcp-config", mcpPath, "--strict-mcp-config", "--tools", "",
			"--allowedTools", "mcp__" + MCPServerName, "--permission-mode", "dontAsk",
			"--setting-sources", "", "--no-session-persistence"}
		if c.conf.MaxTurns > 0 {
			args = append(args, "--max-turns", fmt.Sprint(c.conf.MaxTurns))
		}
		if c.conf.Model != "" {
			args = append(args, "--model", c.conf.Model)
		}
		stdin = []byte(t.Prompt)
	case "codex":
		quote := func(s string) string { b, _ := json.Marshal(s); return string(b) }
		var mcpArgs []string
		for _, a := range t.MCP[1:] {
			mcpArgs = append(mcpArgs, quote(a))
		}
		// The shell and every other built-in tool off, the user's own
		// configuration and its MCP servers not loaded, nothing persisted;
		// the read-only sandbox stays as a second wall.
		args = []string{"exec", "--skip-git-repo-check", "--ephemeral", "--ignore-user-config",
			"-C", t.Dir, "-s", "read-only", "--disable", "shell_tool", "--disable", "apps",
			"--disable", "browser_use", "--disable", "computer_use", "--disable", "view_image",
			"--disable", "image_generation",
			"-c", `web_search="disabled"`, "-c", `approval_policy="never"`,
			"-c", "mcp_servers." + MCPServerName + ".command=" + quote(t.MCP[0]),
			"-c", "mcp_servers." + MCPServerName + ".args=[" + strings.Join(mcpArgs, ",") + "]",
			"--output-schema", schemaPath, "-o", answerPath, "--json", "--color", "never"}
		if c.conf.Model != "" {
			args = append(args, "-m", c.conf.Model)
		}
		last = []string{"-"}
		stdin = []byte(t.Prompt)
	case "kiro-cli":
		// Kiro finds an agent in the directory it runs in. This one has the
		// navigation server and nothing else, and it is written in the
		// stage's own directory, never among the person's agents. tools
		// limits what exists; allowedTools trusts it under Kiro's 2.x engine,
		// and a permissions block under V3, which skips a profile that has
		// allowedTools and no permissions.
		agent := map[string]any{
			"name": "asz-judge", "description": "Reads one conversation through asz and judges it.",
			"prompt": nil, "mcpServers": map[string]any{MCPServerName: map[string]any{"command": t.MCP[0], "args": t.MCP[1:]}},
			"tools": []string{"@" + MCPServerName}, "allowedTools": []string{"@" + MCPServerName}, "useLegacyMcpJson": false,
			"permissions": map[string]any{"rules": []any{
				map[string]any{"capability": "mcp", "match": []string{MCPServerName + "/*"}, "effect": "allow"},
			}},
		}
		b, _ := json.MarshalIndent(agent, "", "  ")
		agentDir, err := makeKiroAgentDir(t.Dir)
		if err != nil {
			return Result{}, err
		}
		if err := os.WriteFile(filepath.Join(agentDir, "asz-judge.json"), b, 0o600); err != nil {
			return Result{}, err
		}
		args = []string{"chat", "--no-interactive", "--agent", "asz-judge", "--trust-tools=@" + MCPServerName}
		if c.conf.Model != "" {
			args = append(args, "--model", c.conf.Model)
		}
		last = []string{t.Prompt + "\n\nThe answer schema:\n```json\n" + string(t.Schema) + "\n```\n"}
	}
	// The configured arguments go before the prompt argument: after it, a
	// CLI reads them as part of the prompt, or not at all.
	args = append(append(args, c.conf.Args...), last...)
	cmd := exec.CommandContext(ctx, prog, args...)
	ownGroup(cmd)
	// Once the CLI has exited, or been killed, its pipes are drained for at
	// most this long, so a child of it that is still alive cannot hold the
	// run.
	cmd.WaitDelay = pipeWait
	cmd.Dir = t.Dir
	cmd.Stdin = bytes.NewReader(stdin)
	stdout := &lineWriter{}
	var requests []Request
	started := 0
	note := func(r Request) {
		requests = append(requests, r)
		if t.Progress != nil {
			t.Progress(r)
		}
	}
	switch c.conf.Program {
	case "claude":
		stdout.line = claudeRequests(note, func() { started++ })
	case "codex":
		stdout.line = codexRequests(note)
	}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = stdout, &stderr
	err := cmd.Run()
	_ = os.WriteFile(filepath.Join(t.Dir, "stdout.txt"), stdout.Bytes(), 0o600)
	_ = os.WriteFile(filepath.Join(t.Dir, "stderr.txt"), stderr.Bytes(), 0o600)
	runErr := err
	var res Result
	switch c.conf.Program {
	case "claude":
		res, err = claudeResult(stdout.Bytes())
	case "codex":
		res, err = codexResult(stdout.Bytes(), answerPath)
	default:
		out, lerr := lastObject(string(stdout.Bytes()))
		res, err = Result{Output: out}, lerr
	}
	stdout.mu.Lock()
	res.Requests = requests
	// A claude call stopped before its result says nothing of its total,
	// but its stream said each request that ended: those are counted, each
	// by the model it named, and one that started and never ended is
	// counted apart. A request on its way that the stream had not shown yet
	// is possible too, so a stopped call with no request running counts one
	// apart all the same: never zero for what may have been billed. The
	// small model claude runs beside the one asked for is in its result
	// only, so an interrupted call's usage holds the requests its stream
	// showed.
	if c.conf.Program == "claude" && res.Usage == nil {
		var u sessiondata.Usage
		by := map[string]sessiondata.Usage{}
		for _, r := range requests {
			model := r.Model
			if model == "" {
				model = c.conf.Model
			}
			add(&u, r.Usage)
			m := by[model]
			add(&m, r.Usage)
			by[model] = m
			if res.Model == "" {
				res.Model = model
			}
		}
		res.Usage, res.UnknownRequests = &u, max(started-len(requests), 1)
		if len(requests) > 0 {
			res.ByModel = by
		}
	}
	// codex says what a call consumed only when the call ends, so one
	// stopped before then said nothing of it: at least one request is
	// unknown, and nothing is known of the rest.
	if c.conf.Program == "codex" && res.Usage == nil {
		res.Usage, res.UnknownRequests = &sessiondata.Usage{}, 1
	}
	stdout.mu.Unlock()
	// A CLI reports the model it ran only sometimes; the one it was told
	// to use is what its usage is priced by.
	if res.Model == "" {
		res.Model = c.conf.Model
	}
	// A call that failed keeps what was counted of it: its requests, and
	// its usage when the CLI got to say it. The answer is what fails.
	switch {
	case ctx.Err() != nil:
		return Result{Usage: res.Usage, Model: res.Model, ByModel: res.ByModel, CostUSD: res.CostUSD, Requests: res.Requests,
			UnknownRequests: res.UnknownRequests}, fmt.Errorf("%s: %w", c.conf.Program, ctx.Err())
	case runErr != nil:
		return Result{Usage: res.Usage, Model: res.Model, ByModel: res.ByModel, CostUSD: res.CostUSD, Requests: res.Requests,
			UnknownRequests: res.UnknownRequests}, fmt.Errorf("%s: %w: %s", c.conf.Program, runErr, lastLine(stderr.String()))
	}
	return res, err
}

// lineWriter takes a CLI's output as it is written and hands each whole
// line to line, which says whether to keep it. A CLI that streams events can
// so be followed while it runs, and only the lines worth keeping are saved.
type lineWriter struct {
	mu   sync.Mutex
	part []byte
	kept bytes.Buffer
	line func([]byte) bool
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.part = append(w.part, p...)
	for {
		i := bytes.IndexByte(w.part, '\n')
		if i < 0 {
			break
		}
		if w.line == nil || w.line(w.part[:i]) {
			w.kept.Write(w.part[:i+1])
		}
		w.part = w.part[i+1:]
	}
	return len(p), nil
}

// Bytes is what was kept, with a last line that had no newline.
func (w *lineWriter) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := append([]byte{}, w.kept.Bytes()...)
	return append(out, w.part...)
}

// claudeRequests follows claude's stream-json output. A request's input
// side is known when it starts and its output when it ends, so each request
// is counted exactly as it ends. Measured: the output in claude's per-message
// events is the count so far, 3 where the request ended at 104, so only the
// final delta is read. The many partial text events are not kept.
func claudeRequests(note func(Request), started func()) func([]byte) bool {
	var cur *Request
	return func(line []byte) bool {
		var e struct {
			Type  string `json:"type"`
			Event struct {
				Type    string `json:"type"`
				Message struct {
					Model string      `json:"model"`
					Usage claudeUsage `json:"usage"`
				} `json:"message"`
				Usage claudeUsage `json:"usage"`
			} `json:"event"`
		}
		if json.Unmarshal(line, &e) != nil || e.Type != "stream_event" {
			return true
		}
		switch e.Event.Type {
		case "message_start":
			cur = &Request{Model: e.Event.Message.Model, Usage: e.Event.Message.Usage.usage()}
			started()
		case "message_delta":
			if cur != nil {
				final := e.Event.Usage.usage()
				cur.Usage.Output = final.Output
				note(*cur)
				cur = nil
			}
		}
		return false
	}
}

// claudeUsage is the usage block of claude's events and results.
type claudeUsage struct {
	Input      int `json:"input_tokens"`
	Output     int `json:"output_tokens"`
	CacheRead  int `json:"cache_read_input_tokens"`
	CacheWrite int `json:"cache_creation_input_tokens"`
}

func (u claudeUsage) usage() sessiondata.Usage {
	return sessiondata.Usage{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite}
}

// codexRequests follows codex's JSON events. codex says only what the whole
// call consumed, when it ends.
func codexRequests(note func(Request)) func([]byte) bool {
	return func(line []byte) bool {
		if u, ok := codexTurnUsage(line); ok {
			note(Request{Usage: u, Whole: true})
		}
		return true
	}
}

// claudeResult reads claude -p's JSON result: the structured answer, and
// the usage it reports.
func claudeResult(stdout []byte) (Result, error) {
	var r struct {
		IsError          bool            `json:"is_error"`
		Subtype          string          `json:"subtype"`
		Result           string          `json:"result"`
		StructuredOutput json.RawMessage `json:"structured_output"`
		Usage            *struct {
			Input      int `json:"input_tokens"`
			Output     int `json:"output_tokens"`
			CacheRead  int `json:"cache_read_input_tokens"`
			CacheWrite int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
		ModelUsage map[string]struct {
			Input         int     `json:"inputTokens"`
			Output        int     `json:"outputTokens"`
			CacheRead     int     `json:"cacheReadInputTokens"`
			CacheWrite    int     `json:"cacheCreationInputTokens"`
			CostUSD       float64 `json:"costUSD"`
			ContextWindow int     `json:"contextWindow"`
		} `json:"modelUsage"`
		TotalCostUSD *float64 `json:"total_cost_usd"`
	}
	if err := json.Unmarshal(lastJSONLine(stdout), &r); err != nil {
		return Result{}, fmt.Errorf("claude: the result does not read: %w", err)
	}
	out := Result{CostUSD: r.TotalCostUSD}
	if r.Usage != nil {
		out.Usage = &sessiondata.Usage{Input: r.Usage.Input, Output: r.Usage.Output, CacheRead: r.Usage.CacheRead, CacheWrite: r.Usage.CacheWrite}
	}
	// claude runs a small model for its own chores beside the one asked
	// for, and its usage field counts only the main one. Measured on
	// 772204d1: claude-haiku-5-5 read about 3,500 tokens per stage beside
	// claude-opus-5-5. Every model's usage is counted, and the model that
	// cost the most is the one that did the work.
	if len(r.ModelUsage) > 0 {
		var total sessiondata.Usage
		out.ByModel = map[string]sessiondata.Usage{}
		best := -1.0
		for m, u := range r.ModelUsage {
			mu := sessiondata.Usage{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite}
			out.ByModel[m] = mu
			add(&total, mu)
			weight := u.CostUSD
			if weight == 0 {
				weight = float64(u.Input+u.Output+u.CacheRead+u.CacheWrite) / 1e9
			}
			if weight > best || (weight == best && m < out.Model) {
				best, out.Model, out.ContextWindow = weight, m, u.ContextWindow
			}
		}
		out.Usage = &total
	}
	if r.IsError {
		return out, fmt.Errorf("claude: %s: %s", r.Subtype, lastLine(r.Result))
	}
	if len(r.StructuredOutput) > 0 && string(r.StructuredOutput) != "null" {
		out.Output = r.StructuredOutput
		return out, nil
	}
	obj, err := lastObject(r.Result)
	out.Output = obj
	return out, err
}

// codexResult reads the answer codex wrote with -o, and the usage its JSON
// events report. OpenAI counts cached input inside its input, so it is taken
// out once.
func codexResult(stdout []byte, answerPath string) (Result, error) {
	out := Result{}
	var usage sessiondata.Usage
	seen := false
	for _, line := range bytes.Split(stdout, []byte("\n")) {
		if u, ok := codexTurnUsage(line); ok {
			seen = true
			add(&usage, u)
		}
	}
	if seen {
		out.Usage = &usage
	}
	b, err := os.ReadFile(answerPath)
	if err != nil {
		return out, fmt.Errorf("codex: no answer: %w", err)
	}
	obj, err := lastObject(string(b))
	out.Output = obj
	return out, err
}

// lastObject finds the last JSON object in text, for an evaluator that
// answers in text with the object at the end.
func lastObject(text string) (json.RawMessage, error) {
	for end := strings.LastIndex(text, "}"); end >= 0; end = strings.LastIndex(text[:end], "}") {
		for start := strings.LastIndex(text[:end], "{"); start >= 0; start = strings.LastIndex(text[:start], "{") {
			candidate := text[start : end+1]
			var probe map[string]json.RawMessage
			if json.Unmarshal([]byte(candidate), &probe) == nil {
				return json.RawMessage(candidate), nil
			}
		}
	}
	return nil, errors.New("no JSON object in the answer")
}

func lastJSONLine(b []byte) []byte {
	lines := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		if json.Valid(lines[i]) {
			return lines[i]
		}
	}
	return bytes.TrimSpace(b)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return clip(lines[len(lines)-1], 300)
}

func compact(b []byte) []byte {
	var buf bytes.Buffer
	if json.Compact(&buf, b) != nil {
		return b
	}
	return buf.Bytes()
}

// codexTurnUsage reads the usage of one codex event that carries it, its
// cached input taken out of its input.
func codexTurnUsage(line []byte) (sessiondata.Usage, bool) {
	var ev struct {
		Usage *struct {
			Input      int `json:"input_tokens"`
			Cached     int `json:"cached_input_tokens"`
			CacheWrite int `json:"cache_write_input_tokens"`
			Output     int `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(line, &ev) != nil || ev.Usage == nil {
		return sessiondata.Usage{}, false
	}
	u := ev.Usage
	return sessiondata.Usage{Input: u.Input - u.Cached, CacheRead: u.Cached, CacheWrite: u.CacheWrite, Output: u.Output}, true
}
