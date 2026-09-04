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

package tests_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/scenario/run"
)

// TestScenarios runs every scenario under scenarios/ in every format, at
// every checkpoint, against its expectation file, and compares the formats
// with each other. A scenario is a property of assembly written as the
// evidence that shows it; the expectation file says what the fold must hold.
func TestScenarios(t *testing.T) {
	files, err := filepath.Glob("scenarios/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var scenarios []string
	for _, f := range files {
		if !strings.HasSuffix(f, ".expect.yaml") {
			scenarios = append(scenarios, f)
		}
	}
	if len(scenarios) == 0 {
		t.Fatal("no scenarios under scenarios/")
	}
	for _, path := range scenarios {
		name := strings.TrimSuffix(filepath.Base(path), ".yaml")
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rep, err := run.Check(path, run.Options{Out: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			for _, l := range rep.Lines {
				t.Log(l)
			}
			if rep.Failed {
				t.Fail()
			}
		})
	}
}
