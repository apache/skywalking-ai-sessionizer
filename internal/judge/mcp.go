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
	"bufio"
	"encoding/json"
	"io"
	"sync"
)

// MCPServerName is the name the navigation server gives itself, and the name
// an agent CLI is told to know it by.
const MCPServerName = "asz"

// mcpVersions are the protocol versions this server speaks, newest first.
var mcpVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// ServeMCP answers the Model Context Protocol on one connection: JSON-RPC
// messages, one per line, the way an agent CLI talks to a server it starts.
// It offers the navigation tools and nothing else, so an evaluator given only
// this server can read the conversation and cannot change anything.
//
// Every call is logged through onCall, when it is set, so a run can say what
// its evaluator read.
// Once the call's reading budget is spent, every tool says so; after
// budgetRefusals such answers the server ends, so an evaluator that goes on
// asking has nothing left to ask and answers with what it read.
func ServeMCP(r io.Reader, w io.Writer, nav *Nav, onCall func(tool string, args json.RawMessage, bytes int, err error)) error {
	var mu sync.Mutex
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	send := func(v any) error {
		mu.Lock()
		defer mu.Unlock()
		return enc.Encode(v)
	}
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if serr := answer(line, nav, send, onCall); serr != nil {
				return serr
			}
			if _, refused := nav.Spent(); refused >= budgetRefusals {
				return nil
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// budgetRefusals is how many answers saying the budget is spent the server
// gives before it ends.
const budgetRefusals = 3

func answer(line []byte, nav *Nav, send func(any) error, onCall func(string, json.RawMessage, int, error)) error {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return send(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: "parse error"}})
	}
	// A notification has no id and gets no answer.
	if len(req.ID) == 0 {
		return nil
	}
	reply := func(result any) error { return send(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}) }
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := mcpVersions[0]
		for _, v := range mcpVersions {
			if v == p.ProtocolVersion {
				version = v
			}
		}
		return reply(map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": MCPServerName, "version": "1"},
			"instructions":    "Read-only navigation of one conversation at one head. Start with card, then inputs or talks, then talk, step, record.",
		})
	case "ping":
		return reply(map[string]any{})
	case "tools/list":
		var tools []map[string]any
		for _, t := range Tools() {
			tools = append(tools, map[string]any{"name": t.Name, "description": t.Description,
				"inputSchema": t.InputSchema, "annotations": readOnly})
		}
		return reply(map[string]any{"tools": tools})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return send(rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32602, Message: "invalid params"}})
		}
		out, err := nav.Call(p.Name, p.Arguments)
		if err != nil {
			if onCall != nil {
				onCall(p.Name, p.Arguments, 0, err)
			}
			// A tool's own failure is an answer the model reads, not a
			// protocol error, so it can correct the call.
			return reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": err.Error()}}, "isError": true})
		}
		b, _ := json.Marshal(out)
		if onCall != nil {
			onCall(p.Name, p.Arguments, len(b), nil)
		}
		return reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": string(b)}}})
	}
	return send(rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32601, Message: "method not found: " + req.Method}})
}
