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
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/metrics"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
)

func TestDeferredRealSessionRetriesWhileAnotherSessionGrows(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	root := t.TempDir()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	load := func(name string) *scenario.Scenario {
		sc, err := scenario.Load(filepath.Join("..", "..", "tests", "scenarios", name+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		return sc
	}
	growth, ended := load("growth"), load("assembly")
	if _, err := scenario.Build(growth, scenario.FormatClaudeCode, root, scenario.Options{At: at, Through: "turn1"}); err != nil {
		t.Fatal(err)
	}
	b, err := scenario.Build(ended, scenario.FormatClaudeCode, root, scenario.Options{At: at})
	if err != nil {
		t.Fatal(err)
	}
	ref := pipelineOver(t, root, bothAdapters(root, ""))
	// A real-session pipeline has no scenario-removal derivation retries.
	ref.remover = nil
	now := time.Now()
	ref.deriver.Grace = metrics.DefaultGrace
	ref.deriver.Now = func() time.Time { return now }
	if err := ref.pass(); err != nil {
		t.Fatal(err)
	}
	done, err := metrics.DerivedAll(ref.zone, b.Session, nil)
	if err != nil || done {
		t.Fatalf("fixture must defer final file: done=%v err=%v", done, err)
	}
	now = now.Add(time.Hour)
	if _, err := scenario.Build(growth, scenario.FormatClaudeCode, root, scenario.Options{At: at, Through: "turn2"}); err != nil {
		t.Fatal(err)
	}
	if err := ref.pass(); err != nil {
		t.Fatal(err)
	}
	done, err = metrics.DerivedAll(ref.zone, b.Session, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Fatal("completed real session is still not derived one hour past the two-minute grace because another session changed")
	}
}
