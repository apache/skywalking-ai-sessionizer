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

package settings_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/plugins/claude-code/internal/settings"
)

// TestWhichToolsAreScanned holds the three forms a scope entry takes, and the
// exclusions that make the wildcard usable.
//
// The wildcard is the only workable default for a runtime whose tools the
// application names, and it is only safe with exclusions: the read-only
// classifier understands shell commands and nothing else, so for any other
// runtime the exclusions are the sole way to keep a scan off a tool that
// reads. An exclusion silently ignored would turn every such setting into
// "scan everything".
func TestWhichToolsAreScanned(t *testing.T) {
	for _, tc := range []struct {
		name    string
		file    string
		scanned map[string]bool
	}{{
		name: "the default names shells only",
		file: "",
		scanned: map[string]bool{
			"Bash": true, "PowerShell": true, "Monitor": true,
			"write_report": false, "run_command": false},
	}, {
		name: "exact names",
		file: "tools:\n  scope: [write_report, run_command]\n",
		scanned: map[string]bool{
			"write_report": true, "run_command": true, "Bash": false},
	}, {
		name: "a regular expression",
		file: "tools:\n  scope: [\"re:^(write|run)_\"]\n",
		scanned: map[string]bool{
			"write_report": true, "run_command": true, "read_status": false},
	}, {
		name: "every tool",
		file: "tools:\n  scope: [\"*\"]\n",
		scanned: map[string]bool{
			"write_report": true, "anything_at_all": true, "Bash": true},
	}, {
		name: "every tool, minus what reads",
		file: "tools:\n  scope: [\"*\"]\n  exclude: [\"re:^(read|get|list)_\", search_docs]\n",
		scanned: map[string]bool{
			"write_report": true, "read_status": false, "get_pods": false,
			"list_clusters": false, "search_docs": false, "Bash": true},
	}, {
		name:    "an exclusion beats an exact scope entry",
		file:    "tools:\n  scope: [write_report, read_status]\n  exclude: [read_status]\n",
		scanned: map[string]bool{"write_report": true, "read_status": false},
	}, {
		name:    "a regular expression that does not compile matches nothing",
		file:    "tools:\n  scope: [\"re:([\"]\n",
		scanned: map[string]bool{"write_report": false, "anything": false},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.file != "" {
				if err := os.WriteFile(filepath.Join(dir, settings.File),
					[]byte(tc.file), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			s, err := settings.Load(dir)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			for tool, want := range tc.scanned {
				if got := s.IsScopeTool(tool); got != want {
					t.Errorf("%s: scanned=%v, want %v", tool, got, want)
				}
			}
		})
	}
}
