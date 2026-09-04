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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Format names a writer: what a build leaves in the output directory.
type Format string

// The two writers. A runtime format writes the runtime's own files for its
// adapter to collect; sd lands Session Data directly.
const (
	FormatClaudeCode Format = "claude-code"
	FormatSD         Format = "sd"
)

// Formats lists the writers, in the order a check runs them.
var Formats = []Format{FormatClaudeCode, FormatSD}

// Built reports what one build wrote.
type Built struct {
	Plan    *Plan
	Session string
	Format  Format
	Out     string
	Config  string
	Files   []string
	Events  int
}

// Build plans a scenario and writes it into out with the named writer, then
// writes out/asz.yaml so the ordinary commands finish the job: for a runtime
// format, asz collect reads out/_source and lands into out; asz parse then
// writes the rounds. The build itself never collects or parses. The source
// directory starts with an underscore so the storage root's session listing
// passes over it, as it passes over _conversations.
func Build(sc *Scenario, format Format, out string, opts Options) (*Built, error) {
	p, err := sc.Plan(opts)
	if err != nil {
		return nil, err
	}
	out, err = filepath.Abs(out)
	if err != nil {
		return nil, err
	}
	source := filepath.Join(out, "_source")
	if err := os.MkdirAll(source, 0o755); err != nil {
		return nil, err
	}
	res := &Built{Plan: p, Session: p.Session, Format: format, Out: out, Events: len(p.Events)}
	switch format {
	case FormatClaudeCode:
		res.Files, err = writeClaudeCode(p, source)
	case FormatSD:
		now := opts.At
		if now.IsZero() {
			now = time.Now()
		}
		res.Files, err = writeSD(p, out, now.UTC())
	default:
		return nil, fmt.Errorf("scenario: unknown format %q; use %s or %s", format, FormatClaudeCode, FormatSD)
	}
	if err != nil {
		return nil, err
	}
	res.Config, err = writeConfig(out, source)
	return res, err
}

// writeConfig writes the configuration the ordinary commands read: the
// storage root is the output directory, and the adapter's source is the
// source directory beside it, which an sd build leaves empty. The export
// block is left commented, with the one key a push needs.
func writeConfig(out, source string) (string, error) {
	path := filepath.Join(out, "asz.yaml")
	text := fmt.Sprintf(`# Written by asz scenario build. The storage root is this directory; the
# adapter's source is the source directory beside it, which an sd build
# leaves empty.
storage:
  root: %s
adapters:
  - name: claude-code-local
    enabled: true
    source_root: %s
    collector:
      mode: once
# To export the session with asz push, name the receiver:
# export:
#   otlp:
#     endpoint: http://127.0.0.1:12800
`, out, source)
	// A file that starts with what the build writes is the build's, with
	// whatever a person appended, such as the export block for a push.
	if old, err := os.ReadFile(path); err == nil && strings.HasPrefix(string(old), text) {
		return path, nil
	} else if err == nil {
		return "", errors.New("scenario: " + path + " exists and is not this build's; use another --out")
	}
	return path, os.WriteFile(path, []byte(text), 0o644)
}

func mkdirAll(dir string) error { return os.MkdirAll(dir, 0o755) }
