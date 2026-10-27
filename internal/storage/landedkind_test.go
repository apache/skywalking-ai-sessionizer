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

package storage_test

import (
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

func TestLandedKind(t *testing.T) {
	stamp := storage.Stamp(time.Date(2026, 10, 25, 10, 32, 0, 1, time.UTC))
	for prefix, want := range map[string]sessiondata.Kind{
		"transcript":    sessiondata.KindTranscript,
		"meta":          sessiondata.KindAgentMeta,
		"journal":       sessiondata.KindJournal,
		"manifest":      sessiondata.KindWorkflowManifest,
		"script":        sessiondata.KindWorkflowScript,
		"changes":       sessiondata.KindChanges,
		"execution":     sessiondata.KindExecution,
		"provider_body": sessiondata.KindProviderBody,
	} {
		name := storage.LandedName(prefix, stamp, 3)
		for _, n := range []string{name, storage.SkeletonPath(name)} {
			if got, ok := storage.LandedKind(n); !ok || got != want {
				t.Fatalf("%s: %q %v, want %q", n, got, ok, want)
			}
		}
	}
	for _, name := range []string{
		storage.LandedName("otlp_log", stamp, 1),
		storage.LandedName("otlp_span", stamp, 1),
		storage.LandedName("agent_meta", stamp, 1),
		storage.LandedName("workflow_manifest", stamp, 1),
		storage.LandedName("Transcript", stamp, 1),
		"transcript-" + stamp + "-00001.sd",
		"transcript-" + stamp + "-00000a.sd",
		"transcript-2026-000001.sd",
		"transcript-" + stamp + "-000001.json",
		"transcript.sd",
		"-" + stamp + "-000001.sd",
	} {
		if kind, ok := storage.LandedKind(name); ok {
			t.Fatalf("%s read as %q", name, kind)
		}
	}
}
