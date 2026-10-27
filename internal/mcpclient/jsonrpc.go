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

package mcpclient

import (
	"bytes"
	"encoding/json"
	"io"
	"slices"
)

// The JSON-RPC error codes the modern revision defines. Any of them in an
// answer shows a modern server.
const (
	codeHeaderMismatch     = -32020
	codeMissingCapability  = -32021
	codeUnsupportedVersion = -32022
	codeMethodNotFound     = -32601
)

// message is any JSON-RPC 2.0 message: a request, a notification or a
// response.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// readMessage reads one JSON-RPC 2.0 message and checks its shape: a request
// has an id and a method, a notification a method and no id, a response an
// id, or none for an error that could not name one, and exactly one of a
// result and an error.
func readMessage(b []byte) (*message, error) {
	var m message
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, failure(ClassProtocol, "a message that is not JSON")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, failure(ClassProtocol, "a message with more after it")
	}
	if m.JSONRPC != "2.0" {
		return nil, failure(ClassProtocol, "a message that is not JSON-RPC 2.0")
	}
	hasID := len(m.ID) > 0 && string(m.ID) != "null"
	switch {
	case m.Method != "":
		if m.Result != nil || m.Error != nil {
			return nil, failure(ClassProtocol, "a request that carries a result")
		}
	case (m.Result == nil) == (m.Error == nil):
		return nil, failure(ClassProtocol, "a response with neither or both of a result and an error")
	case !hasID && m.Error == nil:
		return nil, failure(ClassProtocol, "a result with no id")
	}
	if !hasID {
		m.ID = nil
	}
	return &m, nil
}

// isRequest says whether a message is a request from the server: a method
// and an id.
func (m *message) isRequest() bool { return m.Method != "" && m.ID != nil }

// isNotification says whether a message is a notification.
func (m *message) isNotification() bool { return m.Method != "" && m.ID == nil }

// answers says whether a response answers the request with id.
func (m *message) answers(id int) bool {
	if m.Method != "" {
		return false
	}
	if m.ID == nil {
		// An error the server could not tie to a request answers the one in
		// flight: an unsupported version can come so.
		return m.Error != nil
	}
	// A string is another id than the number asz sent, though it holds
	// the same digits.
	var n json.Number
	if m.ID[0] == '"' || json.Unmarshal(m.ID, &n) != nil {
		return false
	}
	got, err := n.Int64()
	return err == nil && got == int64(id)
}

// modernError says whether an error is one only a modern server sends.
func modernError(e *rpcError) bool {
	return e != nil && (e.Code == codeHeaderMismatch || e.Code == codeMissingCapability || e.Code == codeUnsupportedVersion)
}

// supportsModern says whether an unsupported-version error lists the modern
// revision asz speaks among those the server supports.
func supportsModern(e *rpcError) bool {
	var d struct {
		Supported []string `json:"supported"`
	}
	_ = json.Unmarshal(e.Data, &d)
	return slices.Contains(d.Supported, Modern)
}

// request makes a request. A modern one carries in params._meta the
// protocol version, who asks, and its capabilities: none.
func (s Server) request(id int, method string, params map[string]any, modern bool) []byte {
	if params == nil {
		params = map[string]any{}
	}
	if modern {
		params["_meta"] = map[string]any{
			"io.modelcontextprotocol/protocolVersion":    Modern,
			"io.modelcontextprotocol/clientInfo":         s.clientInfo(),
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		}
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	return b
}

// notification makes a notification.
func notification(method string, params map[string]any) []byte {
	m := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		m["params"] = params
	}
	b, _ := json.Marshal(m)
	return b
}

// reply makes a response to a server's request.
func reply(id json.RawMessage, result any, e *rpcError) []byte {
	m := map[string]any{"jsonrpc": "2.0", "id": id}
	if e != nil {
		m["error"] = e
	} else {
		m["result"] = result
	}
	b, _ := json.Marshal(m)
	return b
}

func (s Server) clientInfo() map[string]any {
	v := s.Version
	if v == "" {
		v = "unknown"
	}
	return map[string]any{"name": "asz", "version": v}
}

// initializeParams are a legacy handshake's: the newest legacy version, and
// no capability.
func (s Server) initializeParams() map[string]any {
	return map[string]any{"protocolVersion": Legacy[0], "capabilities": map[string]any{}, "clientInfo": s.clientInfo()}
}

// negotiated is the version a legacy server answered its handshake with,
// when asz speaks it.
func negotiated(result json.RawMessage) (string, error) {
	var r struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(result, &r) != nil || !slices.Contains(Legacy, r.ProtocolVersion) {
		return "", failure(ClassProtocol, "the server answered the handshake with version %q, which asz does not speak", r.ProtocolVersion)
	}
	return r.ProtocolVersion, nil
}

// complete checks a call's result: one with no resultType is complete, as
// every legacy result is and as the modern revision reads one; one that asks
// for input, or of a type asz does not know, is a failure, since asz gives
// no input.
func complete(result json.RawMessage) error {
	var r struct {
		ResultType *string `json:"resultType"`
	}
	if json.Unmarshal(result, &r) != nil {
		return failure(ClassProtocol, "a result that is not an object")
	}
	switch {
	case r.ResultType == nil, *r.ResultType == "complete":
		return nil
	case *r.ResultType == "input_required":
		return failure(ClassProtocol, "the server asked for input, which asz does not give")
	}
	return failure(ClassProtocol, "a result of a type asz does not know: %q", *r.ResultType)
}

// serverError is a JSON-RPC error a server answered.
func serverError(e *rpcError) *Error {
	return &Error{Class: ClassServerError, Code: e.Code, Message: e.Message}
}

// listPage reads one page of a tool list: its tools and the cursor of the
// next page, kept as given, an empty one included.
func listPage(result json.RawMessage) ([]json.RawMessage, *string, error) {
	var r struct {
		Tools      []json.RawMessage `json:"tools"`
		NextCursor *string           `json:"nextCursor"`
	}
	if err := complete(result); err != nil {
		return nil, nil, err
	}
	if json.Unmarshal(result, &r) != nil {
		return nil, nil, failure(ClassProtocol, "a tool list that does not read")
	}
	return r.Tools, r.NextCursor, nil
}

// pageParams asks for the page after cursor.
func pageParams(cursor *string) map[string]any {
	if cursor == nil {
		return nil
	}
	return map[string]any{"cursor": *cursor}
}
