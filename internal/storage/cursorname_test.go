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
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// A source that is not the first at its path lands under the path and a mark
// of its first line. The path is read back from the mark, and a path that
// only looks like one, a # not followed by 12 hexadecimal characters, is a
// path whole. Repack reads a cursor's path this way to name the cursor again
// for a new root.
func TestASourceMarkIsReadBack(t *testing.T) {
	const path = "file-changes-x/output/s/main.jsonl"
	head := strings.Repeat("0123456789abcdef", 4)
	marked := storage.MarkedSource(path, head)
	if marked != path+"#0123456789ab" {
		t.Fatalf("marked %q", marked)
	}
	for src, want := range map[string]string{
		marked:                 path,
		path:                   path,
		path + "#notahex00000": path + "#notahex00000",
		path + "#0123456789a":  path + "#0123456789a",
		"a#b/" + path:          "a#b/" + path,
	} {
		if got := storage.SourcePath(src); got != want {
			t.Errorf("SourcePath(%q) = %q, want %q", src, got, want)
		}
	}
	// Each name differs by what it is for.
	names := map[string]bool{
		storage.SourceCursorName("changes", "_recorder", path):              true,
		storage.SourceCursorName("changes", "/other", path):                 true,
		storage.LaterCursorName("changes", "_recorder", path, head):         true,
		storage.LaterCursorName("changes", "_recorder", path, head[1:]+"0"): true,
		storage.LaterCursorName("execution", "_recorder", path, head):       true,
	}
	if len(names) != 5 {
		t.Fatalf("two cursors share a name: %v", names)
	}
	for name := range names {
		if filepath.Ext(name) != ".cursor" || !strings.Contains(name, "-") {
			t.Fatalf("name %q", name)
		}
	}
}
