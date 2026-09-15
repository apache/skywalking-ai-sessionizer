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

// Package claudecodeprovider implements the claude-code-provider adapter: a
// pull collector for the request and response bodies Claude Code exchanges
// with its model provider.
//
// Claude Code writes them itself when OTEL_LOG_RAW_API_BODIES is set to
// file:<dir>, one file per body, every session into the same directory. A
// request is named by a random UUID and carries its session in
// metadata.user_id; a response is named by the provider's request id and
// carries no session. This adapter lists that directory, finds each body's
// session from what the bodies and the landed transcripts say, and lands the
// bodies of a session as provider_body records under the session's
// provider directory, with what the session already holds taken out.
package claudecodeprovider

import (
	"os"
	"path/filepath"

	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
)

// Name is the adapter identifier used in configuration and in landed headers.
const Name = "claude-code-provider"

// Version is the adapter's contract version, recorded in landed headers.
const Version = "0.1.0"

// Dialect names the vocabulary the records are read as: the bodies as Claude
// Code wrote them, cut by pkg/providerbody.
const Dialect = "claude-code-provider/1"

// RuntimeName is the service the records are attributed to when pushed.
const RuntimeName = "Claude Code"

// ResolveSourceRoot determines where Claude Code is told to write the bodies:
// asz/provider-bodies under the directory Claude Code itself resolves. An
// explicit override wins. Claude Code resolves a relative path against each
// session's own working directory, so the variable must name this directory
// by its absolute path.
func ResolveSourceRoot(override string) (string, error) {
	if override != "" {
		return filepath.Abs(override)
	}
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "asz", "provider-bodies"), nil
	}
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "claude", "asz", "provider-bodies"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "asz", "provider-bodies"), nil
}

// Glossary says what the bodies call each name the model uses. A body is
// evidence beside a call, not a step, so it names a session and, for a
// response, the call; everything else the model names is structure a body
// does not record.
func Glossary() *model.Glossary {
	native := map[string][2]string{
		model.KindSession: {"metadata.user_id → session_id", "a request body"},
		model.RoleRun:     {"cc_prompt_id", "the billing header, the first system block of a request body"},
		model.RoleCall:    {"id", "a response body"},
		model.RoleID:      {"the file name without .json", "the body's file"},
		model.RoleModel:   {"model", "a request or response body"},
	}
	var terms []model.Term
	for _, name := range model.Vocabulary() {
		t := model.Term{Unified: name}
		if n, ok := native[name]; ok {
			t.Native, t.Where = n[0], n[1]
		} else {
			t.Note = "not recorded; a provider body is evidence beside a call, not a step"
		}
		terms = append(terms, t)
	}
	return model.NewGlossary(Dialect, terms...)
}
