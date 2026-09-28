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

import "testing"

// TestMCPNameSplitsOnlyWhenOneWayIsPossible. Claude Code joins the server and
// the tool with "__", so a name splits only when exactly one "__" follows the
// prefix, counting the ones that overlap. Underscores that run together, as in
// "foo___bar", can be read as foo/_bar or foo_/bar, and leave the name whole.
func TestMCPNameSplitsOnlyWhenOneWayIsPossible(t *testing.T) {
	for _, c := range []struct {
		name, server, tool string
		ok                 bool
	}{
		{"mcp__status__lookup", "status", "lookup", true},
		{"mcp__claude_ai_Claude_Docs__read", "claude_ai_Claude_Docs", "read", true},
		{"mcp___x__y", "_x", "y", true},
		{"mcp__odd__name__twice", "", "", false},
		{"mcp__foo___bar", "", "", false},
		{"mcp__foo____bar", "", "", false},
		{"mcp____bar", "", "", false},
		{"mcp__foo__", "", "", false},
		{"mcp__foo", "", "", false},
		{"Bash", "", "", false},
	} {
		server, tool, ok := MCPName(c.name)
		if server != c.server || tool != c.tool || ok != c.ok {
			t.Errorf("MCPName(%q) = %q, %q, %v; want %q, %q, %v", c.name, server, tool, ok, c.server, c.tool, c.ok)
		}
	}
}
