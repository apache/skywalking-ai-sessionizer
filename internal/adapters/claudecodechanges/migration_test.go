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

package claudecodechanges

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// TestBothPluginDirectoriesAreCollected.
//
// Renaming the plugin leaves two data directories: the old one with records
// nothing has collected yet, and the new one filling up. Both hold the same
// session under the same stream, because a session id does not change.
//
// They are different files. One cursor for both stops collection for good:
// once the newer file passes the older one's length, the older is read at a
// position past its end, which is a truncation conflict, and after that
// neither is collected again.
func TestBothPluginDirectoriesAreCollected(t *testing.T) {
	data := t.TempDir()
	const session = "ls-demo-0123456789ab"
	write := func(plugin string, records int) {
		t.Helper()
		dir := filepath.Join(data, plugin+"-skywalking-ai-sessionizer", "output", session)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		var body []byte
		for i := 0; i < records; i++ {
			line, _ := json.Marshal(map[string]any{
				"schema": "changes/1", "id": fmt.Sprintf("%s-call-%d", plugin, i),
				"captured_by": "test", "cwd": data,
				"at":   time.Unix(int64(1700000000+i), 0).UTC().Format(time.RFC3339Nano),
				"tool": "write_report", "changes": []any{},
			})
			body = append(body, line...)
			body = append(body, '\n')
		}
		if err := os.WriteFile(filepath.Join(dir, "main.jsonl"), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The new copy is longer, which is what made the old one look truncated.
	write("asz-changes", 2)
	write("file-changes", 6)

	zone := storage.NewZone(t.TempDir())
	if _, err := New(data, zone, 2<<20).CollectAll(nil); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	// A second pass is where the shared cursor used to fail: the first pass
	// left one position behind, and the other file was read against it.
	if _, err := New(data, zone, 2<<20).CollectAll(nil); err != nil {
		t.Fatalf("second pass: %v", err)
	}

	files, err := storage.LandedFiles(zone, session)
	if err != nil {
		t.Fatal(err)
	}
	var landed []byte
	for _, f := range files {
		data, err := os.ReadFile(f.Path)
		if err != nil {
			t.Fatal(err)
		}
		landed = append(landed, data...)
	}
	for _, want := range []string{"asz-changes-call-1", "file-changes-call-5"} {
		if !bytes.Contains(landed, []byte(want)) {
			t.Errorf("%s never landed; one of the two directories was lost", want)
		}
	}
}
