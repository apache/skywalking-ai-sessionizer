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

package hook_test

import (
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/plugins/claude-code/internal/hook"
)

// The inputs are what a run of Claude Code 2.1.260 wrote to a logging
// hook, with the identifiers and paths replaced.
const failure = `{"session_id":"48489556-217d-4c18-a0df-d37806fb0331","transcript_path":"/home/u/.claude/projects/-w/48489556.jsonl","cwd":"/w","permission_mode":"default","hook_event_name":"PostToolUseFailure","tool_name":"Bash","tool_input":{"command":"cat /nonexistent-file","description":"Read a file"},"tool_use_id":"toolu_014njBmrgxYHSRwjy5DoZR6n","error":"Exit code 1\ncat: /nonexistent-file: No such file or directory","is_interrupt":false,"duration_ms":40,"prompt_id":"p1","agent_id":"afb0b607bcfa2020a","agent_type":"general-purpose"}`

func TestReadTakesWhatTheRuntimeWrites(t *testing.T) {
	in, err := hook.Read(strings.NewReader(failure))
	if err != nil {
		t.Fatal(err)
	}
	if in.Event != hook.PostToolUseFailure || in.ToolName != "Bash" || in.ToolUseID != "toolu_014njBmrgxYHSRwjy5DoZR6n" || in.Cwd != "/w" || in.PromptID != "p1" {
		t.Fatalf("input: %+v", in)
	}
	if in.Stream() != "afb0b607bcfa2020a" || in.Command() != "cat /nonexistent-file" {
		t.Fatalf("stream %q command %q", in.Stream(), in.Command())
	}
	if code, ok := in.ExitCode(); !ok || code != 1 {
		t.Fatalf("exit code %d ok=%v", code, ok)
	}
	main, _ := hook.Read(strings.NewReader(`{"hook_event_name":"PreToolUse","session_id":"s","tool_name":"Edit","tool_input":{"file_path":"x"}}`))
	if main.Stream() != "main" || main.Command() != "" {
		t.Fatalf("main stream: %q %q", main.Stream(), main.Command())
	}
	if _, err := hook.Read(strings.NewReader(`{}`)); err == nil {
		t.Fatal("an input with no event was accepted")
	}
}
