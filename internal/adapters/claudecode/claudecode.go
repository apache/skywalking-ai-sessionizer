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

// Package claudecode implements the claude-code-local adapter: a pull
// collector that reads Claude Code's own on-disk files.
//
// It requires no configuration of Claude Code, no environment variables and no
// cooperation from the running agent. Because it reads what is already on disk,
// it also works on conversation history that predates its installation.
package claudecode

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Name is the adapter identifier used in config and in landed headers.
const Name = "claude-code-local"

// Version is the adapter's contract version, recorded in landed headers so a
// bundle can be interpreted against the adapter that produced it.
const Version = "0.1.0"

// sessionIDRe matches a Claude Code session identifier.
//
// Case is significant: real corpora contain uppercase session ids, so every
// comparison must be byte-exact and never case-folded.
var sessionIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// agentIDRe matches a Claude Code agent identifier.
var agentIDRe = regexp.MustCompile(`^a[0-9a-f]{16}$`)

// IsSessionID reports whether name is a session identifier.
func IsSessionID(name string) bool { return sessionIDRe.MatchString(name) }

// IsAgentID reports whether name is an agent identifier.
func IsAgentID(name string) bool { return agentIDRe.MatchString(name) }

// runIDRe validates the run id extracted from a script filename.
var runIDRe = regexp.MustCompile(`^wf_[A-Za-z0-9][A-Za-z0-9_-]*$`)

// ScriptRunID returns the workflow run id embedded in a script filename.
//
// Claude Code writes workflow scripts as "<label>-wf_<run-id>.js". A run id
// itself contains hyphens (wf_afa31e47-f6c), so a greedy pattern would start at
// the FIRST "-wf_" and swallow any later one - taking the label as part of the
// id whenever the label happens to contain that sequence. Splitting on the LAST
// occurrence is what makes the boundary unambiguous.
func ScriptRunID(name string) (string, bool) {
	base, ok := strings.CutSuffix(name, ".js")
	if !ok {
		return "", false
	}
	i := strings.LastIndex(base, "-wf_")
	if i < 0 {
		return "", false
	}
	id := base[i+1:]
	if !runIDRe.MatchString(id) {
		return "", false
	}
	return id, true
}

// Slugify converts a working directory to the source directory name Claude
// Code files it under.
//
// The transformation collapses '/', '.' and ' ' to '-'. It is therefore lossy
// and NOT reversible: "-a-b" could have come from "/a/b", "/a.b" or "/a b".
// Forward slugification is deterministic, so configuration slugifies the user's
// path to match; nothing ever attempts the inverse. The real working directory
// is recoverable only from record content.
func Slugify(dir string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '/', '.', ' ':
			return '-'
		}
		return r
	}, dir)
}

// ResolveSourceRoot determines where Claude Code keeps its project data.
//
// Resolution order: an explicit override, then CLAUDE_CONFIG_DIR, then
// XDG_CONFIG_HOME, then the default. Claude Code honours both environment
// variables, so the location must never be hardcoded.
func ResolveSourceRoot(override string) (string, error) {
	if override != "" {
		return filepath.Abs(override)
	}
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "projects"), nil
	}
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "claude", "projects"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects"), nil
}
