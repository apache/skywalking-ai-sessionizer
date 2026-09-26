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

// Package settings holds the plugin's settings: what to watch, what to
// leave out, and how long to keep things. They live in one file in the
// plugin's data directory, and every value has a default, so the plugin
// runs with no file at all.
package settings

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// File is the settings file's name under the data directory.
const File = "settings.yaml"

// Settings is the whole file.
type Settings struct {
	// Roots are the directories to observe. Empty means the session's
	// project directory, which is what the hook reports as the working
	// directory, and is the default.
	Roots []string `yaml:"roots"`
	// Exclude names the directories never observed: a default set by
	// name, project additions, and defaults to keep after all.
	Exclude Exclude `yaml:"exclude"`
	// ReadOnly says whether a shell command the classifier marks read-only
	// skips its scans. On by default.
	ReadOnly ReadOnly `yaml:"read_only"`
	// MCP is the recording of calls to MCP servers.
	MCP MCP `yaml:"mcp"`
	// Tools names the tools observed with a scan: the shells. Editing
	// tools are observed from their own response and need no entry.
	Tools Tools `yaml:"tools"`
	// Retention is how long snapshot state and output are kept.
	Retention Retention `yaml:"retention"`
	// ScanTimeout caps one scan. Past it the observation records a gap.
	ScanTimeout time.Duration `yaml:"scan_timeout"`
	// SizeCap is the largest file whose bytes are kept for a diff; a
	// larger one lands as path and hash only.
	SizeCap int64 `yaml:"size_cap"`
}

// Exclude is the exclusion setting.
type Exclude struct {
	Defaults string   `yaml:"defaults"`
	Add      []string `yaml:"add"`
	Remove   []string `yaml:"remove"`
}

// ReadOnly is the read-only skip setting.
type ReadOnly struct {
	Enabled *bool `yaml:"enabled"`
}

// On reports whether the skip is enabled.
func (r ReadOnly) On() bool { return r.Enabled == nil || *r.Enabled }

// MCP is the recording of calls to MCP servers: which server, how the call
// ended, how long it took, and the size of what went each way. It keeps no
// parameter or answer text, so it is on unless turned off.
type MCP struct {
	Enabled *bool `yaml:"enabled"`
}

// On reports whether calls to MCP servers are recorded.
func (m MCP) On() bool { return m.Enabled == nil || *m.Enabled }

// Tools names the tools observed with a scan.
//
// A runtime with a fixed tool set can be listed by name, which is what Claude
// Code's entry does. A runtime whose tools the application defines cannot be:
// nobody knows their names in advance. So an entry takes three forms, and the
// exclusions take the same three.
type Tools struct {
	// Scope names the tools observed. An entry is an exact name, a regular
	// expression when it begins "re:", or "*" alone for every tool.
	Scope []string `yaml:"scope"`
	// Exclude names tools never observed, in the same three forms, applied
	// after Scope. It is what makes "*" usable: the read-only classifier
	// only understands shell commands, so for any other runtime this is the
	// only way to keep a scan off a tool that reads.
	Exclude []string `yaml:"exclude"`
}

// Retention is how long things are kept.
type Retention struct {
	// Idle is how long a root's snapshot bytes are kept after its last
	// window closed with nothing pending.
	Idle time.Duration `yaml:"idle"`
	// TTL is how long an output file is kept after its last write.
	TTL time.Duration `yaml:"ttl"`
}

// Default returns the settings used when no file is present. The numbers
// are picked, not measured: the plugin's own cost is out of scope for now.
func Default() *Settings {
	return &Settings{
		Exclude:     Exclude{Defaults: "standard-v1"},
		Tools:       Tools{Scope: []string{"Bash", "PowerShell", "Monitor"}},
		Retention:   Retention{Idle: 30 * time.Minute, TTL: 30 * 24 * time.Hour},
		ScanTimeout: 30 * time.Second,
		SizeCap:     1 << 20,
	}
}

// Load reads the settings file under dataDir, or the defaults when there
// is none. A value the file leaves out keeps its default.
func Load(dataDir string) (*Settings, error) {
	s := Default()
	data, err := os.ReadFile(filepath.Join(dataDir, File))
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("settings: %w", err)
	}
	var loaded Settings
	if err := yaml.Unmarshal(data, &loaded); err != nil {
		return nil, fmt.Errorf("settings: %s: %w", File, err)
	}
	if len(loaded.Roots) > 0 {
		s.Roots = loaded.Roots
	}
	if loaded.Exclude.Defaults != "" {
		s.Exclude.Defaults = loaded.Exclude.Defaults
	}
	s.Exclude.Add, s.Exclude.Remove = loaded.Exclude.Add, loaded.Exclude.Remove
	if loaded.ReadOnly.Enabled != nil {
		s.ReadOnly = loaded.ReadOnly
	}
	if loaded.MCP.Enabled != nil {
		s.MCP = loaded.MCP
	}
	if len(loaded.Tools.Scope) > 0 {
		s.Tools.Scope = loaded.Tools.Scope
	}
	// Exclusions are taken whether or not any were given, because emptying
	// the list is a real setting: it says observe everything the scope names.
	// Reading them only when non-empty would make removing the last one do
	// nothing.
	s.Tools.Exclude = loaded.Tools.Exclude
	if loaded.Retention.Idle > 0 {
		s.Retention.Idle = loaded.Retention.Idle
	}
	if loaded.Retention.TTL > 0 {
		s.Retention.TTL = loaded.Retention.TTL
	}
	if loaded.ScanTimeout > 0 {
		s.ScanTimeout = loaded.ScanTimeout
	}
	if loaded.SizeCap > 0 {
		s.SizeCap = loaded.SizeCap
	}
	return s, nil
}

// IsScopeTool reports whether a tool is observed with a scan.
func (s *Settings) IsScopeTool(name string) bool {
	if !matchesAny(s.Tools.Scope, name) {
		return false
	}
	return !matchesAny(s.Tools.Exclude, name)
}

// matchesAny reads the three entry forms.
//
// A regular expression that does not compile matches nothing rather than
// everything. A setting someone got wrong should observe too little and be
// noticed, not observe everything and be expensive.
func matchesAny(entries []string, name string) bool {
	for _, entry := range entries {
		switch {
		case entry == "*":
			return true
		case strings.HasPrefix(entry, "re:"):
			re, err := regexp.Compile(entry[len("re:"):])
			if err == nil && re.MatchString(name) {
				return true
			}
		case entry == name:
			return true
		}
	}
	return false
}
