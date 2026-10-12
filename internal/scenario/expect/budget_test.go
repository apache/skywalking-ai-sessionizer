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

package expect

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// landOne writes one landed file of one record, unfitted, as a file a
// collector could not make smaller would be.
func landOne(t *testing.T, root string, kind sessiondata.Kind, part sessiondata.Part) {
	t.Helper()
	hdr := &sessiondata.Header{Seq: 1, At: "2026-01-01T00:00:00Z", Kind: kind, Adapter: "a/1", Dialect: "d", Src: "s.jsonl", Session: "s1", Stream: "main"}
	var buf bytes.Buffer
	w, err := sessiondata.NewWriter(&buf, hdr)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(&sessiondata.Record{Ord: 1, Sha: "x", Bytes: 1, Parts: []sessiondata.Part{part}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	dir := storage.NewZone(root).StreamDir("s1", "main")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, storage.LandedName(string(kind), "20260101T000000.000000000Z", 1)), buf.Bytes(), 0o444); err != nil {
		t.Fatal(err)
	}
}

// A file over the budget is allowed only when it holds one record Fit could
// make no smaller, by Fit's own rule: an execution record is never taken
// out, so it may stand over a tiny budget, and a record that still holds
// text may not.
func TestFilesFitTheBudgetFollowsFitsRule(t *testing.T) {
	exec := json.RawMessage(`{"schema":"execution/1","id":"e1","observed_by":"asz-plugin","boundary":"mcp","session":"s1","stream":"main","tool":"t1","protocol":"mcp","time":"2026-01-01T00:00:00Z","outcome":"returned"}`)
	root := t.TempDir()
	landOne(t, root, sessiondata.KindExecution, sessiondata.Part{Kind: sessiondata.PartData, Data: exec, State: "available"})
	if out, err := FilesFitTheBudget(root, "s1", 50); err != nil || len(out) != 0 {
		t.Fatalf("an execution record over a 50-byte budget: %v, %v", out, err)
	}
	root = t.TempDir()
	landOne(t, root, sessiondata.KindTranscript, sessiondata.Part{Kind: sessiondata.PartText, Text: strings.Repeat("a", 500), State: "available"})
	if out, err := FilesFitTheBudget(root, "s1", 200); err != nil || len(out) != 1 {
		t.Fatalf("a record that still holds text over the budget: %v, %v", out, err)
	}
}

// Only a provider body's own manifest is excused, not a transcript record
// whose data happens to read like one.
func TestOnlyAProviderBodysManifestIsExcused(t *testing.T) {
	root := t.TempDir()
	lookalike := json.RawMessage(`{"schema":"provider_body/1","payload":"` + strings.Repeat("p", 500) + `"}`)
	landOne(t, root, sessiondata.KindTranscript, sessiondata.Part{Kind: sessiondata.PartData, Data: lookalike, State: "available"})
	if out, err := FilesFitTheBudget(root, "s1", 200); err != nil || len(out) != 1 {
		t.Fatalf("a transcript record that reads like a manifest, over the budget: %v, %v", out, err)
	}
}
