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

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/plugins/claude-code/internal/hook"
)

// jsonString is s as a JSON string. A Windows path holds backslashes,
// which JSON must escape, so a test never pastes a path into JSON as it is.
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestHooksRunTheBinaryWithoutAShell holds hooks/hooks.json to the exec
// form: the binary as the command and hook as its one argument. Without
// "args", Claude Code hands the command to a shell, and on Windows without
// Git Bash that shell is PowerShell. PowerShell cannot parse a quoted path
// followed by a word, so every hook failed before the plugin started. With
// "args", Claude Code starts the binary itself, on every platform.
func TestHooksRunTheBinaryWithoutAShell(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("hooks", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Hooks map[string][]struct {
			Hooks []map[string]any `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{string(hook.SessionStart), string(hook.SessionEnd), string(hook.PreToolUse), string(hook.PostToolUse), string(hook.PostToolUseFailure)} {
		if len(file.Hooks[event]) == 0 {
			t.Errorf("runHook handles %s, and no hook runs it", event)
		}
	}
	n := 0
	for event, groups := range file.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				n++
				if h["type"] != "command" || h["command"] != "${CLAUDE_PLUGIN_ROOT}/bin/asz-claude-plugin" || !reflect.DeepEqual(h["args"], []any{"hook"}) {
					t.Errorf("%s: %v is not the binary with the one argument hook", event, h)
				}
				// Claude Code ignores a shell when args is set, so naming one
				// would only mislead a reader.
				if _, ok := h["shell"]; ok {
					t.Errorf("%s: %v names a shell", event, h)
				}
			}
		}
	}
	if n == 0 {
		t.Fatal("hooks/hooks.json holds no hook")
	}
}

// TestNoArgumentWithAnEventIsAHook: a Claude Code that drops the hook's
// args runs the binary with no argument and the event on standard input.
// It must run as a hook, which exits 0, and not print the usage text,
// which exits 2 and so would block the tool. A character device, as a
// terminal is, still gets the usage text.
func TestNoArgumentWithAnEventIsAHook(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if got := subcommand(nil, r); got != "hook" {
		t.Fatalf("no argument and a pipe: %q, want hook", got)
	}
	if got := subcommand([]string{"status"}, r); got != "status" {
		t.Fatalf("the argument status: %q", got)
	}
	f, err := os.Create(filepath.Join(t.TempDir(), "event.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if got := subcommand(nil, f); got != "hook" {
		t.Fatalf("no argument and a file: %q, want hook", got)
	}
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if got := subcommand(nil, null); got != "" {
		t.Fatalf("no argument and the null device: %q, want the usage text", got)
	}
}
