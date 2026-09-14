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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// The command must follow its index back to the current landed format,
// for both a tool call and its result.
func TestShowReadsCollectedToolPayloads(t *testing.T) {
	root := t.TempDir()
	sc, err := scenario.Load(filepath.Join("..", "..", "tests", "scenarios", "tool-both-sides.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := scenario.Build(sc, scenario.FormatClaudeCode, root, scenario.Options{At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	c := claudecode.New(filepath.Join(root, "_source"), storage.NewZone(root), 0)
	if _, err := c.CollectAll(nil); err != nil {
		t.Fatal(err)
	}
	saved := positional
	positional = []string{b.Session, "t1"}
	t.Cleanup(func() { positional = saved })
	cfg := config.Default()
	cfg.Storage.Root = root
	output := stdoutOf(t, func() {
		if err := cmdShow(cfg, config.Adapter{}, false); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"resolves to 2 block(s)", "tool_use", "tool_result", ".sd", "source:"} {
		if !strings.Contains(output, want) {
			t.Errorf("show output lacks %q: %s", want, output)
		}
	}
}
