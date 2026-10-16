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
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/langsmith"
	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
)

// TestAPassSaysHowManyRunsItDidNotCollect. An update whose run's start never
// came waits, and is dropped once its request has waited as long as it may.
// That pass lands nothing and parses nothing, and still prints its line, with
// the count. The quiet pass after it prints nothing.
func TestAPassSaysHowManyRunsItDidNotCollect(t *testing.T) {
	root := t.TempDir()
	zone := storage.NewZone(root)
	ads := []config.Adapter{{Name: config.AdapterLangSmithIngest, Enabled: true, Listen: "127.0.0.1:0"}}
	ref, err := newRefresher(view.New(zone, nil), zone, ads, noMetrics, 2<<20, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ref.close)
	const run = "16161616-1616-7616-8616-161616161611"
	update := `{"patch":[{"id":"` + run + `","trace_id":"` + run + `","dotted_order":"20260920T100000000000Z` + run + `",` +
		`"end_time":"2026-09-20T10:00:03Z","outputs":{"answer":"no start came"}}]}`
	if _, err := langsmith.NewInbox(zone).Put("application/json", "POST", "/runs/batch", []byte(update), time.Now()); err != nil {
		t.Fatal(err)
	}
	ref.pass() // The first pass always says what it did.
	for i := 0; i < 2; i++ {
		if out := stdoutOf(t, func() { ref.pass() }); out != "" {
			t.Fatalf("a pass that only held the request printed %q", out)
		}
	}
	out := stdoutOf(t, func() { ref.pass() })
	if !strings.Contains(out, " runs_not_collected=1") {
		t.Errorf("the pass that dropped the update printed %q, want runs_not_collected=1", out)
	}
	if out := stdoutOf(t, func() { ref.pass() }); out != "" {
		t.Errorf("the quiet pass after it printed %q", out)
	}
}
