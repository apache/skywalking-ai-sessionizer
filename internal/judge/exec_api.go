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
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionjudge"
)

// submitTool is how an API evaluator gives its answer: a tool whose input is
// the stage's answer schema, so every model answers in the same shape.
const submitTool = "submit"

// keptResultBytes bounds what tool results stay in an API evaluator's context.
// Older results past it are replaced by a line saying they were read, so a
// long investigation does not carry every page it ever read into every later
// call; the evaluator calls a tool again when it needs one back.
const keptResultBytes = 256 << 10

// defaultMaxTurns bounds an API evaluator's model calls in one stage.
const defaultMaxTurns = 40

// defaultMaxTokens bounds what one model call of an API evaluator may answer,
// within the 8K that DeepSeek R1 on Bedrock allows, the smallest of the
// models the docs name.
const defaultMaxTokens = 8000

func (a *apiExecutor) maxTokens() int {
	if a.conf.MaxTokens > 0 {
		return a.conf.MaxTokens
	}
	return defaultMaxTokens
}

// apiExecutor calls a provider's API itself, with the navigation tools
// answered in this process.
type apiExecutor struct {
	desc   sessionjudge.Evaluator
	conf   config.Evaluator
	client *http.Client
	once   sync.Once
}

func (a *apiExecutor) Describe() sessionjudge.Evaluator { return a.desc }

// turn is one exchange of the loop, in a form both APIs map onto.
type turn struct {
	role  string // "user" or "assistant"
	text  string
	calls []toolCall
	// results answer the previous assistant turn's calls, in order.
	results []toolResult
}

type toolCall struct {
	ID   string
	Name string
	Args json.RawMessage
}

type toolResult struct {
	ID     string
	Text   string
	Error  bool
	masked bool
	// given is the receipt of what the answer gave, and sent says a request
	// has carried the result, or never will: it counts once, when it first
	// reaches the model whole.
	given Receipt
	sent  bool
}

func (a *apiExecutor) Run(ctx context.Context, t Task) (Result, error) {
	// Calls of one stage run at once on one executor, so the client is made
	// once, by whichever comes first.
	a.once.Do(func() {
		if a.client == nil {
			a.client = &http.Client{Timeout: 10 * time.Minute}
		}
	})
	key := ""
	if a.conf.APIKeyEnv != "" {
		key = os.Getenv(a.conf.APIKeyEnv)
		if key == "" {
			return Result{}, fmt.Errorf("%s: %s is empty", a.desc.Name, a.conf.APIKeyEnv)
		}
	}
	if err := os.WriteFile(filepath.Join(t.Dir, "prompt.md"), []byte(t.Prompt), 0o600); err != nil {
		return Result{}, err
	}
	log, err := os.Create(filepath.Join(t.Dir, "calls.jsonl"))
	if err != nil {
		return Result{}, err
	}
	defer log.Close()
	tools := append([]Tool{}, Tools()...)
	tools = append(tools, Tool{Name: submitTool, Description: "Give your answer. Call this once, when you are done, with the whole answer.", InputSchema: t.Schema})

	maxTurns := a.conf.MaxTurns
	if maxTurns <= 0 {
		maxTurns = defaultMaxTurns
	}
	turns := []turn{{role: "user", text: t.Prompt + "\n\nWhen you are done, call the submit tool with your answer."}}
	// What the evaluator consumed is what the responses said, and a request
	// that got no answer, or one that did not say, is counted apart as
	// unknown: never estimated, and never hiding what the others said. A
	// request lost on the way may still have been billed. Measured on
	// DeepSeek through Bedrock: 4 of 19 calls of one stage lost a response,
	// which made the whole stage's usage unknown.
	var sum sessiondata.Usage
	var requests []Request
	unknown := 0
	result := func(output json.RawMessage) Result {
		u := sum
		return Result{Output: output, Usage: &u, Model: a.conf.Model, Requests: requests, UnknownRequests: unknown, InProcess: true}
	}
	for i := 0; i < maxTurns; i++ {
		maskOld(turns)
		deliverNew(t.Nav, turns)
		// Once the reading budget is spent, submitting is all that is left.
		if _, refused := t.Nav.Spent(); refused > 0 {
			tools = tools[len(tools)-1:]
		}
		reply, u, err := a.call(ctx, key, turns, tools)
		if err != nil {
			unknown++
			return result(nil), err
		}
		if u == nil {
			unknown++
		} else {
			sum.Input += u.Input
			sum.Output += u.Output
			sum.CacheRead += u.CacheRead
			sum.CacheWrite += u.CacheWrite
			r := Request{Model: a.conf.Model, Usage: *u}
			requests = append(requests, r)
			if t.Progress != nil {
				t.Progress(r)
			}
		}
		turns = append(turns, reply)
		// The model's own words and calls are kept, for a person reading how
		// the stage went. What a tool answered is not: the navigation is
		// pinned to the run's head, so the same call answers the same again.
		said := map[string]any{"turn": i + 1, "text": clip(reply.text, 2000)}
		var asked []map[string]any
		for _, c := range reply.calls {
			asked = append(asked, map[string]any{"tool": c.Name, "args": json.RawMessage(nonEmpty(c.Args))})
		}
		said["calls"] = asked
		if line, err := json.Marshal(said); err == nil {
			_, _ = log.Write(append(line, '\n'))
		}
		if len(reply.calls) == 0 {
			// A model that answers in text is asked once more to use the
			// tool, rather than taken at its word.
			turns = append(turns, turn{role: "user", text: "Call the submit tool with your answer."})
			continue
		}
		var results []toolResult
		for _, c := range reply.calls {
			if c.Name == submitTool {
				return result(c.Args), nil
			}
			out, given, err := t.Nav.Answer(c.Name, c.Args)
			r := toolResult{ID: c.ID, given: given}
			if err != nil {
				r.Text, r.Error = err.Error(), true
			} else {
				b, _ := json.Marshal(out)
				r.Text = string(b)
			}
			line, _ := json.Marshal(map[string]any{"tool": c.Name, "args": c.Args, "bytes": len(r.Text), "error": r.Error})
			_, _ = log.Write(append(line, '\n'))
			results = append(results, r)
		}
		turns = append(turns, turn{role: "user", results: results})
	}
	return result(nil), fmt.Errorf("%s: no answer after %d model calls", a.desc.Name, maxTurns)
}

// deliverNew counts what the results the next request carries for the first
// time gave the model. A result shortened before any request carried it
// whole never reached the model, and gave it nothing.
func deliverNew(nav *Nav, turns []turn) {
	for i := range turns {
		for j := range turns[i].results {
			r := &turns[i].results[j]
			if r.sent {
				continue
			}
			r.sent = true
			if !r.masked {
				nav.Deliver(r.given)
			}
		}
	}
}

// maskOld keeps the newest tool results within keptResultBytes and replaces
// the rest, so the context of the next call stays bounded.
func maskOld(turns []turn) {
	kept := 0
	for i := len(turns) - 1; i >= 0; i-- {
		for j := len(turns[i].results) - 1; j >= 0; j-- {
			r := &turns[i].results[j]
			if r.masked {
				continue
			}
			kept += len(r.Text)
			if kept > keptResultBytes {
				r.Text = "[read earlier and taken out of context to keep it small; call the tool again if you need it]"
				r.masked = true
			}
		}
	}
}

func (a *apiExecutor) call(ctx context.Context, key string, turns []turn, tools []Tool) (turn, *sessiondata.Usage, error) {
	switch a.conf.Program {
	case "anthropic":
		return a.callAnthropic(ctx, key, turns, tools)
	case "bedrock":
		return a.callBedrock(ctx, key, turns, tools)
	default:
		return a.callOpenAI(ctx, key, turns, tools)
	}
}

func (a *apiExecutor) post(ctx context.Context, url string, headers map[string]string, body any, into any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var secrets []string
		for _, k := range []string{"x-api-key", "Authorization"} {
			if v := headers[k]; v != "" {
				secrets = append(secrets, v, strings.TrimPrefix(v, "Bearer "))
			}
		}
		return fmt.Errorf("%s: %s: %s", a.desc.Name, resp.Status, clip(redact(string(data), secrets...), 300))
	}
	return json.Unmarshal(data, into)
}

// redact takes the secrets a request carried out of what the provider
// answered, before the answer reaches an error, a log or a stage note: a
// gateway can echo the key it refused, and a stage note is exported.
func redact(text string, secrets ...string) string {
	for _, s := range secrets {
		if s != "" {
			text = strings.ReplaceAll(text, s, "[redacted]")
		}
	}
	return text
}

// callAnthropic speaks the Messages API. The prompt and the tools are marked
// for the provider's cache, so a stage's calls after the first read them from
// the cache rather than as new input.
func (a *apiExecutor) callAnthropic(ctx context.Context, key string, turns []turn, tools []Tool) (turn, *sessiondata.Usage, error) {
	type block map[string]any
	var msgs []map[string]any
	for i, t := range turns {
		var content []block
		if t.text != "" {
			b := block{"type": "text", "text": t.text}
			if i == 0 {
				b["cache_control"] = block{"type": "ephemeral"}
			}
			content = append(content, b)
		}
		for _, c := range t.calls {
			content = append(content, block{"type": "tool_use", "id": c.ID, "name": c.Name, "input": json.RawMessage(nonEmpty(c.Args))})
		}
		for _, r := range t.results {
			content = append(content, block{"type": "tool_result", "tool_use_id": r.ID, "content": r.Text, "is_error": r.Error})
		}
		msgs = append(msgs, map[string]any{"role": t.role, "content": content})
	}
	var defs []block
	for i, tool := range tools {
		d := block{"name": tool.Name, "description": tool.Description, "input_schema": tool.InputSchema}
		if i == len(tools)-1 {
			d["cache_control"] = block{"type": "ephemeral"}
		}
		defs = append(defs, d)
	}
	body := map[string]any{"model": a.conf.Model, "max_tokens": a.maxTokens(), "messages": msgs, "tools": defs}
	var resp struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		Usage *struct {
			Input      int `json:"input_tokens"`
			Output     int `json:"output_tokens"`
			CacheRead  int `json:"cache_read_input_tokens"`
			CacheWrite int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	headers := map[string]string{"anthropic-version": "2023-06-01"}
	if key != "" {
		headers["x-api-key"] = key
	}
	if err := a.post(ctx, strings.TrimRight(a.conf.Endpoint, "/")+"/v1/messages", headers, body, &resp); err != nil {
		return turn{}, nil, err
	}
	out := turn{role: "assistant"}
	for _, c := range resp.Content {
		switch c.Type {
		case "text":
			out.text += c.Text
		case "tool_use":
			out.calls = append(out.calls, toolCall{ID: c.ID, Name: c.Name, Args: c.Input})
		}
	}
	if resp.Usage == nil {
		return out, nil, nil
	}
	u := resp.Usage
	return out, &sessiondata.Usage{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite}, nil
}

// callOpenAI speaks the chat completions API, which many providers and
// gateways serve. OpenAI counts cached input inside its prompt tokens, so the
// cached part is taken out once.
func (a *apiExecutor) callOpenAI(ctx context.Context, key string, turns []turn, tools []Tool) (turn, *sessiondata.Usage, error) {
	var msgs []map[string]any
	for _, t := range turns {
		if t.role == "assistant" {
			m := map[string]any{"role": "assistant", "content": t.text}
			var calls []map[string]any
			for _, c := range t.calls {
				calls = append(calls, map[string]any{"id": c.ID, "type": "function",
					"function": map[string]any{"name": c.Name, "arguments": string(nonEmpty(c.Args))}})
			}
			if len(calls) > 0 {
				m["tool_calls"] = calls
			}
			msgs = append(msgs, m)
			continue
		}
		if t.text != "" {
			msgs = append(msgs, map[string]any{"role": "user", "content": t.text})
		}
		for _, r := range t.results {
			msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": r.ID, "content": r.Text})
		}
	}
	var defs []map[string]any
	for _, tool := range tools {
		defs = append(defs, map[string]any{"type": "function", "function": map[string]any{
			"name": tool.Name, "description": tool.Description, "parameters": tool.InputSchema}})
	}
	body := map[string]any{"model": a.conf.Model, "messages": msgs, "tools": defs}
	// Sent only when configured: OpenAI's reasoning models refuse
	// max_tokens, and other servers know no other name for it.
	if a.conf.MaxTokens > 0 {
		body["max_tokens"] = a.conf.MaxTokens
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage *struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
			Details    struct {
				Cached int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	headers := map[string]string{}
	if key != "" {
		headers["Authorization"] = "Bearer " + key
	}
	if err := a.post(ctx, strings.TrimRight(a.conf.Endpoint, "/")+"/chat/completions", headers, body, &resp); err != nil {
		return turn{}, nil, err
	}
	if len(resp.Choices) == 0 {
		return turn{}, nil, fmt.Errorf("%s: no choice in the answer", a.desc.Name)
	}
	msg := resp.Choices[0].Message
	out := turn{role: "assistant", text: msg.Content}
	for _, c := range msg.ToolCalls {
		out.calls = append(out.calls, toolCall{ID: c.ID, Name: c.Function.Name, Args: json.RawMessage(c.Function.Arguments)})
	}
	if resp.Usage == nil {
		return out, nil, nil
	}
	u := resp.Usage
	return out, &sessiondata.Usage{Input: u.Prompt - u.Details.Cached, Output: u.Completion, CacheRead: u.Details.Cached}, nil
}

func nonEmpty(b json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(b)) == 0 {
		return json.RawMessage("{}")
	}
	return b
}
