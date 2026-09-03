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

package local_test

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// TestSharedCollectorIsSafe runs two passes concurrently on ONE Collector.
// Per-session state on the shared struct would race here; the -race detector
// is the assertion.
func TestSharedCollectorIsSafe(t *testing.T) {
	src := t.TempDir()
	x := newTranscript(t, src)
	x.AddTurn("do the thing", true)
	x.Flush()

	col := claudecode.New(src, storage.NewZone(t.TempDir()), 0)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = col.CollectAll(nil) }()
	}
	wg.Wait()
}

// hasFlag reports whether a converted record carries one of the flags the
// dialect set on it.
//
// The flags are what a runtime's shape becomes: "synthetic" for a record no
// model produced, "context_reset" for a boundary, "injected" for material the
// harness put into context. Tests assert on these rather than on the runtime's
// own fields, because the runtime's fields no longer survive conversion - which
// is the point.
func hasFlag(rec sessiondata.Record, name string) bool {
	for _, f := range rec.Flags {
		if f == name {
			return true
		}
	}
	return false
}

// content returns everything a converted record holds, as text.
//
// It exists for tests that ask "did this survive conversion?". The answer has
// to come from the record itself, because the source bytes are no longer kept -
// which is exactly the property worth testing.
func content(rec sessiondata.Record) string {
	b, err := json.Marshal(rec)
	if err != nil {
		return ""
	}
	return string(b)
}
