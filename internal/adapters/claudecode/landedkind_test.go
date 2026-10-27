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

package claudecode

import (
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// Every file this adapter lands has a name whose prefix the shared map reads
// back as the kind its header carries. Pull and provider verification choose
// files by that prefix.
func TestLandedNamesCarryTheirKinds(t *testing.T) {
	c := &Collector{Zone: storage.NewZone(t.TempDir())}
	stamp := storage.Stamp(time.Now())
	for _, kind := range []SourceKind{SrcMainTranscript, SrcAgentTranscript, SrcAgentMeta, SrcJournal, SrcWorkflowManifest, SrcWorkflowScript} {
		_, _, prefix := c.cursorPaths(Source{Kind: kind, Session: "s", Stream: "main", RunID: "r"})
		got, ok := storage.LandedKind(storage.LandedName(prefix, stamp, 1))
		if !ok || got != kind.RecordKind() {
			t.Fatalf("source kind %d lands as %q, read back as %q %v", kind, kind.RecordKind(), got, ok)
		}
	}
}
