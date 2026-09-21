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

// pluginDir is the directory a marketplace installs. It holds only what
// Claude Code reads and the legal files, so no Go source is copied into a
// user's plugin cache.
const pluginDir = "plugin"

// TestHooksRunTheBinaryWithoutAShell holds hooks/hooks.json to the exec
// form: the binary as the command and hook as its one argument. Without
// "args", Claude Code hands the command to a shell, and on Windows without
// Git Bash that shell is PowerShell. PowerShell cannot parse a quoted path
// followed by a word, so every hook failed before the plugin started. With
// "args", Claude Code starts the binary itself, on every platform.
//
// The command is the bare name, which Claude Code looks up on PATH, as
// Anthropic's own language server plugins do. The binary is installed with
// asz, not inside the plugin, so one package installs both and the plugin
// directory holds no binary.
func TestHooksRunTheBinaryWithoutAShell(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(pluginDir, "hooks", "hooks.json"))
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
				if h["type"] != "command" || h["command"] != "asz-changes" || !reflect.DeepEqual(h["args"], []any{"hook"}) {
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

// TestTheMarketplaceInstallsThePlugin reads .claude-plugin/marketplace.json
// at the repository root, which is what claude plugin marketplace add reads.
// A marketplace without owner.name, or an entry whose source is not the
// plugin directory, fails when a user adds it, and nothing here would run.
//
// Neither file sets a version. A version pins the plugin to that string, and
// Claude Code then offers no update until the string changes, so a version
// left behind by a release would hold every user on it. Without one, the
// version is the commit the marketplace was added at, which is the release
// tag the install instructions name.
func TestTheMarketplaceInstallsThePlugin(t *testing.T) {
	root := filepath.Join("..", "..")
	var market struct {
		Name  string `json:"name"`
		Owner struct {
			Name string `json:"name"`
		} `json:"owner"`
		Plugins []map[string]any `json:"plugins"`
	}
	readJSON(t, filepath.Join(root, ".claude-plugin", "marketplace.json"), &market)
	if market.Name != "skywalking-ai-sessionizer" || market.Owner.Name == "" {
		t.Errorf("marketplace %q, owner %q: the documented install names skywalking-ai-sessionizer, and Claude Code requires an owner", market.Name, market.Owner.Name)
	}
	if len(market.Plugins) != 1 {
		t.Fatalf("the marketplace lists %d plugins, want file-changes alone", len(market.Plugins))
	}
	entry := market.Plugins[0]
	if entry["name"] != "file-changes" || entry["source"] != "./plugins/claude-code/"+pluginDir {
		t.Errorf("entry %v is not file-changes from ./plugins/claude-code/%s", entry, pluginDir)
	}
	if _, ok := entry["version"]; ok {
		t.Errorf("the marketplace entry sets a version")
	}

	var manifest map[string]any
	readJSON(t, filepath.Join(pluginDir, ".claude-plugin", "plugin.json"), &manifest)
	if manifest["name"] != "file-changes" {
		t.Errorf("plugin.json names %v, and the marketplace entry names file-changes", manifest["name"])
	}
	if _, ok := manifest["version"]; ok {
		t.Errorf("plugin.json sets a version")
	}

	// A marketplace install copies this directory alone, so it carries its
	// own copy of the project's license and notice.
	for _, name := range []string{"LICENSE", "NOTICE"} {
		want, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(pluginDir, name))
		if err != nil || string(got) != string(want) {
			t.Errorf("%s/%s is not the repository's %s: %v", pluginDir, name, name, err)
		}
	}

	// Anything else in the directory would be copied into every user's
	// plugin cache.
	entries, err := os.ReadDir(pluginDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		switch e.Name() {
		case ".claude-plugin", "hooks", "LICENSE", "NOTICE":
		default:
			t.Errorf("%s holds %s, which a marketplace install would copy", pluginDir, e.Name())
		}
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("%s: %v", path, err)
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
