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

package claudecode

import "strings"

// mcpPrefix starts the name Claude Code gives a call to an MCP server.
const mcpPrefix = "mcp__"

// MCPName splits the name Claude Code gives a call to an MCP server,
// mcp__<server>__<tool>, into the server and the tool, and reports whether
// the split is exact.
//
// It is exact only when exactly one "__" follows the prefix. Claude Code
// builds the name by joining the two with "__", so a server or a tool whose
// own name holds "__" would make two splits possible, and then none is made
// and the name is left whole. Measured on the local corpus on 2026-09-25, no
// MCP tool name held a second "__". The server part is Claude Code's form of
// the configured name: "claude.ai Claude Docs" is called
// "claude_ai_Claude_Docs", so it is not the configured name itself.
func MCPName(name string) (server, tool string, ok bool) {
	rest, found := strings.CutPrefix(name, mcpPrefix)
	if !found {
		return "", "", false
	}
	server, tool, found = strings.Cut(rest, "__")
	if !found || server == "" || tool == "" || strings.Contains(tool, "__") {
		return "", "", false
	}
	return server, tool, true
}
