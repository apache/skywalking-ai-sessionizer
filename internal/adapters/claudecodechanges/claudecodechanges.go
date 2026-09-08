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

// Package claudecodechanges implements the claude-code-changes adapter: a
// pull collector for the workspace change records the asz Claude Code
// plugin writes.
//
// The plugin runs inside Claude Code's hooks and writes one JSON line per
// observed tool call into its own data directory, which Claude Code keeps
// beside the transcripts. It needs nothing from asz. This adapter finds
// those files the way the local adapter finds transcripts, tails them, and
// lands each line as a record of kind changes under the stream the tool
// ran in, with the same cursor, lock and sequence the transcript adapter
// uses for the session.
package claudecodechanges

import (
	"os"
	"path/filepath"

	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
)

// Name is the adapter identifier used in configuration and in landed headers.
const Name = "claude-code-changes"

// Version is the adapter's contract version, recorded in landed headers.
const Version = "0.1.0"

// Dialect names the vocabulary the records are read as. The plugin writes
// changes/1 records in the model's own words, so there is no runtime shape
// between the source and the landed record.
const Dialect = "asz-changes/1"

// PluginName is the plugin's name in its manifest. Claude Code names the
// plugin's data directory after it, with the marketplace it was installed
// from as a suffix, so discovery matches the prefix.
const PluginName = "asz-changes"

// RuntimeName is the service the records are attributed to when pushed:
// they describe Claude Code sessions, so they belong to the same service
// as the transcripts.
const RuntimeName = "Claude Code"

// ResolveSourceRoot determines where the plugin keeps its output: the
// plugins/data directory beside projects, under the same directory Claude
// Code itself resolves. An explicit override wins.
func ResolveSourceRoot(override string) (string, error) {
	if override != "" {
		return filepath.Abs(override)
	}
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "plugins", "data"), nil
	}
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "claude", "plugins", "data"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "plugins", "data"), nil
}

// Glossary says what the plugin's records call each name the model uses.
// A change record carries a session, a stream, a tool-use id and a time in
// the model's own words; everything else the model names is structure the
// plugin does not record.
func Glossary() *model.Glossary {
	native := map[string]string{
		model.KindSession: "session", model.KindStream: "stream",
		model.RoleID: "id", model.RoleTool: "tool", model.RoleTime: "time",
	}
	var terms []model.Term
	for _, name := range model.Vocabulary() {
		t := model.Term{Unified: name}
		if n, ok := native[name]; ok {
			t.Native, t.Where = n, "a change record"
		} else {
			t.Note = "not recorded; a change record is evidence beside a step, not a step"
		}
		terms = append(terms, t)
	}
	return model.NewGlossary(Dialect, terms...)
}
