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

// Tools names the tools observed with a scan.
type Tools struct {
	Scope []string `yaml:"scope"`
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
	if len(loaded.Tools.Scope) > 0 {
		s.Tools.Scope = loaded.Tools.Scope
	}
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
	for _, t := range s.Tools.Scope {
		if t == name {
			return true
		}
	}
	return false
}
