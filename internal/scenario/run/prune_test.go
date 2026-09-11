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

package run

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// A scenario cannot set a cursor's state. A conflict already recorded must
// survive pruning and restoration, including when a child's file stays
// discovered after the main transcript is removed.
func TestPrunedSourcesGoneKeepsExistingConflicts(t *testing.T) {
	for _, child := range []bool{false, true} {
		name := "main"
		if child {
			name = "child"
		}
		t.Run(name, func(t *testing.T) {
			sc, err := scenario.Load(filepath.Join("..", "..", "..", "tests", "scenarios", "delegation.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(t.TempDir(), "root")
			at := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
			built, err := scenario.Build(sc, scenario.FormatClaudeCode, out, scenario.Options{At: at})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := collect(scenario.FormatClaudeCode, out); err != nil {
				t.Fatal(err)
			}
			if _, err := parseAll(out, built.Session, 0); err != nil {
				t.Fatal(err)
			}
			states, err := cursorStates(storage.NewZone(out), built.Session)
			if err != nil {
				t.Fatal(err)
			}
			var cursorPath string
			for path := range states {
				if filepath.Base(path) == "transcript.cursor" && (filepath.Base(filepath.Dir(path)) != "main") == child {
					cursorPath = path
					break
				}
			}
			if cursorPath == "" {
				t.Fatal("the scenario wrote no matching transcript cursor")
			}
			cur, err := storage.LoadCursor(cursorPath, storage.CursorAppend, "")
			if err != nil {
				t.Fatal(err)
			}
			cur.State = storage.CursorConflict
			if err := cur.Save(cursorPath, at); err != nil {
				t.Fatal(err)
			}
			problems, err := prunedSourcesGone(out, built.Session, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(problems) != 0 {
				t.Fatalf("pruning or restoration changed an existing conflict: %v", problems)
			}
		})
	}
}
