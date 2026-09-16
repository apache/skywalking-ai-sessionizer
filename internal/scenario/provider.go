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
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ProviderBodyDir is where, under the source root, a Claude Code build writes
// the provider bodies: the directory OTEL_LOG_RAW_API_BODIES would name.
const ProviderBodyDir = "provider-bodies"

// ProviderBody is one body a claude-code build writes as a file, and an sd
// build lands directly. Both formats take the same bytes in the same order.
type ProviderBody struct {
	Name  string
	At    time.Time
	Bytes []byte
}

// ProviderBodies renders the request and response of every provider call the
// plan holds, in the order Claude Code writes them: a request when the call
// is built, and its response when the last fragment has arrived.
//
// They follow what Claude Code 2.1.260 was measured to write. Every call
// sends its stream's whole message list again. The newest message carries
// the cache marker, and a one-block text message becomes a plain string once
// the marker has left it. The first system block is a billing header naming
// the prompt and the previous call's request. metadata.user_id is a JSON
// string holding the session. A compaction starts the list again with the
// summary. Thinking text is written as <REDACTED>, as Claude Code writes it.
// A response is named by the request id; a request by a UUID drawn from the
// call, so the same plan writes the same files.
func (p *Plan) ProviderBodies() []ProviderBody {
	if !p.bodies {
		return nil
	}
	lost := p.lostStreams()
	declared := map[string]Stream{}
	for _, s := range p.Streams {
		declared[s.ID] = s
	}
	chains := map[string]*bodyChain{}
	chainOf := func(stream string) *bodyChain {
		c := chains[stream]
		if c == nil {
			c = &bodyChain{main: stream == "main"}
			if c.main {
				c.system, c.tools = p.SystemPrompt, p.Tools
			} else if s, ok := declared[stream]; ok {
				c.system, c.tools = s.SystemPrompt, s.Tools
			}
			chains[stream] = c
		}
		return c
	}
	var out []ProviderBody
	done := map[string]bool{}
	for i := range p.Events {
		e := &p.Events[i]
		if e.Lost || lost[e.Stream] || e.Replayed {
			continue
		}
		c := chainOf(e.Stream)
		switch e.Kind {
		case EvInput, EvNotice:
			c.prompt = e.Run
			c.user(e.Text)
		case EvSummary:
			c.prompt = e.Run
			c.msgs = nil
			c.user(e.Text)
		case EvResult:
			c.push(map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": e.Of, "content": e.Text},
			}})
		case EvFragment:
			if done[e.Call] {
				continue
			}
			if c.call != e.Call {
				c.call, c.blocks = e.Call, nil
				out = append(out, ProviderBody{
					Name:  requestName(p.Session, e.Call),
					At:    e.At.Add(-time.Millisecond),
					Bytes: c.request(p.Session),
				})
			}
			c.blocks = append(c.blocks, fragmentBlock(e))
			if e.Last {
				done[e.Call] = true
				out = append(out, ProviderBody{
					Name: e.Req + ".response.json",
					At:   e.At.Add(time.Millisecond),
					// The same usage the transcript records for this call, so a
					// reader comparing the two sees one number, not two.
					Bytes: encodeBody(map[string]any{
						"id": e.Call, "type": "message", "role": "assistant", "model": ccModel,
						"content": c.blocks, "stop_reason": e.Stop, "usage": ccUsage(e.Usage),
					}),
				})
				c.push(map[string]any{"role": "assistant", "content": c.blocks})
				c.prev, c.call, c.blocks = e.Req, "", nil
			}
		}
	}
	// The order the adapter lands them in: by when the file was written,
	// then by name. An sd build lands them in the same order, so a body
	// holds the same parts in both formats.
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// WithoutProviderBodies is the scenario with no provider bodies written, and
// every id as it is with them.
func (sc *Scenario) WithoutProviderBodies() *Scenario {
	out := *sc
	out.omitBodies = true
	return &out
}

type bodyChain struct {
	main   bool
	msgs   []map[string]any
	prompt string
	prev   string
	call   string
	blocks []any
	// system and tools are what the scenario wrote for this stream. Empty
	// leaves the stand-in.
	system string
	tools  []ToolDef
}

// user adds a prompt. The first message of a chain carries the injected
// reminder beside it, as Claude Code's does.
func (c *bodyChain) user(text string) {
	if len(c.msgs) == 0 {
		reminder := "<system-reminder>\nAs you answer the user's questions, you can use the following context.\n</system-reminder>"
		if c.main {
			reminder = "<system-reminder>\nCodebase and user instructions are shown below.\n" + strings.Repeat("Follow the project's rules. ", 60) + "\n</system-reminder>"
		}
		c.push(map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": reminder},
			map[string]any{"type": "text", "text": text},
		}})
		return
	}
	c.push(map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}})
}

// push adds a message, moving the cache marker to it.
func (c *bodyChain) push(m map[string]any) {
	for _, old := range c.msgs {
		blocks, ok := old["content"].([]any)
		if !ok {
			continue
		}
		for _, b := range blocks {
			if block, ok := b.(map[string]any); ok {
				delete(block, "cache_control")
			}
		}
		if len(blocks) == 1 {
			if block, ok := blocks[0].(map[string]any); ok && block["type"] == "text" {
				old["content"] = block["text"]
			}
		}
	}
	if blocks, ok := m["content"].([]any); ok && len(blocks) > 0 {
		last := map[string]any{}
		for k, v := range blocks[len(blocks)-1].(map[string]any) {
			last[k] = v
		}
		last["cache_control"] = map[string]any{"type": "ephemeral"}
		copied := append([]any(nil), blocks...)
		copied[len(copied)-1] = last
		m = map[string]any{"role": m["role"], "content": copied}
	}
	c.msgs = append(c.msgs, m)
}

// request renders the body of the call about to be made.
func (c *bodyChain) request(session string) []byte {
	header := "x-anthropic-billing-header: cc_version=2.1.260; cc_entrypoint=cli; cch=00000;"
	if c.prev != "" {
		header += " cc_prev_req=" + c.prev + ";"
	}
	if c.prompt != "" {
		header += " cc_prompt_id=" + c.prompt + ";"
	}
	// The stand-in, for a scenario that writes none. It is filler sized to
	// make a body worth cutting, not something to read: a scenario meant to
	// be read writes its own system prompt and tools.
	system := "You are Claude Code, working in a scenario. " + strings.Repeat("Use the tools to answer. ", 80)
	tools := []any{
		map[string]any{"name": "Read", "description": strings.Repeat("Reads a file from the local filesystem. ", 40),
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}}}},
		map[string]any{"name": "Bash", "description": strings.Repeat("Executes a shell command and returns its output. ", 40),
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}}},
	}
	if !c.main {
		system = "You are an agent that searches a repository. " + strings.Repeat("Report what you find. ", 60)
		tools = tools[:1]
	}
	if c.system != "" {
		system = c.system
	}
	// Written and empty is not the same as unwritten: a scenario may say its
	// agent advertises no tools at all, which is a request a runtime sends.
	if c.tools != nil {
		// Empty, never nil: a stream that advertises nothing sends an empty
		// list, and a nil slice would be written as null, which is not a
		// request any runtime sends.
		tools = []any{}
		for _, t := range c.tools {
			schema := t.InputSchema
			if schema == nil {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, map[string]any{
				"name": t.Name, "description": t.Description, "input_schema": schema,
			})
		}
	}
	user, _ := json.Marshal(map[string]string{"device_id": "scenario", "account_uuid": "scenario", "session_id": session})
	type body struct {
		Model     string           `json:"model"`
		Messages  []map[string]any `json:"messages"`
		System    []any            `json:"system"`
		Tools     []any            `json:"tools"`
		Metadata  map[string]any   `json:"metadata"`
		MaxTokens int              `json:"max_tokens"`
	}
	return encodeBody(body{
		Model: ccModel, Messages: c.msgs,
		System: []any{
			map[string]any{"type": "text", "text": header},
			map[string]any{"type": "text", "text": system, "cache_control": map[string]any{"type": "ephemeral"}},
		},
		Tools:     tools,
		Metadata:  map[string]any{"user_id": string(user)},
		MaxTokens: 64000,
	})
}

func fragmentBlock(e *Event) any {
	switch e.Frag {
	case FragThinking:
		return map[string]any{"type": "thinking", "thinking": "<REDACTED>", "signature": "sig"}
	case FragToolUse:
		input := e.Tool.Input
		if input == nil {
			input = map[string]any{}
		}
		return map[string]any{"type": "tool_use", "id": e.Tool.ID, "name": e.Tool.Name, "input": input}
	default:
		return map[string]any{"type": "text", "text": e.Text}
	}
}

// encodeBody writes compact JSON with no HTML escapes, the way JSON.stringify
// does.
func encodeBody(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'})
}

// requestName is the file name of a call's request: a UUID drawn from the
// session and the call, in the form Claude Code's random ones take.
func requestName(session, call string) string {
	sum := sha256.Sum256([]byte("request:" + session + ":" + call))
	h := hex.EncodeToString(sum[:16])
	return fmt.Sprintf("%s-%s-4%s-8%s-%s.request.json", h[0:8], h[8:12], h[13:16], h[17:20], h[20:32])
}
