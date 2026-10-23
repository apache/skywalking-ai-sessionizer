// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements.  See the NOTICE file distributed with
// this work for additional information regarding copyright ownership.
// The ASF licenses this file to You under the Apache License, Version 2.0
// (the "License"); you may not use this file except in compliance with
// the License.  You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/kiro"
	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/langsmith"
	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/mock"
	"github.com/apache/skywalking-ai-sessionizer/internal/config"
)

// Each dialect reads its own runtime's editing tools, and a dialect whose
// runtime has none reads nothing.
func TestEachDialectReadsItsOwnEditingTools(t *testing.T) {
	for _, tc := range []struct {
		dialect, tool, input, want string
	}{
		{claudecode.Dialect, "Write", `{"file_path":"/w/a.go"}`, "[{/w/a.go write}]"},
		{claudecode.Dialect, "fs_write", `{"path":"/w/a.go"}`, "[]"},
		{kiro.Dialect, "fs_write", `{"path":"/w/a.go"}`, "[{/w/a.go write}]"},
		{kiro.Dialect, "Write", `{"file_path":"/w/a.go"}`, "[]"},
	} {
		if got := fmt.Sprint(decoderOf(tc.dialect)(tc.tool, json.RawMessage(tc.input))); got != tc.want {
			t.Errorf("%s %s: %s, want %s", tc.dialect, tc.tool, got, tc.want)
		}
	}
	for _, d := range []string{langsmith.Dialect, mock.Dialect, ""} {
		if decoderOf(d) != nil {
			t.Errorf("%q has a decoder", d)
		}
	}
}

// Sources add the stages that read them to a plan that lists neither, given
// to -evaluator or to the plan's last stage's evaluator.
func TestSourcesAddTheStagesThatReadThem(t *testing.T) {
	plan := []config.JudgeStage{{Stage: "targets", Evaluator: "claude"}, {Stage: "achievement", Evaluator: "codex"}}
	if got := withReferences(plan, nil, ""); len(got) != 2 {
		t.Fatalf("no source: %v", got)
	}
	if got := fmt.Sprint(withReferences(plan, []string{"a.md"}, "")); got != "[{targets claude} {achievement codex} {reference codex} {divergence codex}]" {
		t.Fatalf("with a source: %s", got)
	}
	if got := fmt.Sprint(withReferences(plan, []string{"a.md"}, "kiro")); got != "[{targets claude} {achievement codex} {reference kiro} {divergence kiro}]" {
		t.Fatalf("with -evaluator: %s", got)
	}
	listed := append(plan, config.JudgeStage{Stage: "reference", Evaluator: "claude"})
	if got := withReferences(listed, []string{"a.md"}, ""); len(got) != 3 {
		t.Fatalf("a plan that lists one: %v", got)
	}
	if len(plan) != 2 {
		t.Fatal("the plan given was changed")
	}
	var r repeated
	_ = r.Set("a.md")
	_ = r.Set("https://x/y")
	if r.String() != "a.md,https://x/y" {
		t.Fatalf("repeated %v", r)
	}
}
