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

package output_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/pkg/changes"
	"github.com/apache/skywalking-ai-sessionizer/plugins/claude-code/internal/output"
)

func rec(id, stream string) *changes.Record {
	return &changes.Record{Schema: changes.Schema, ID: id, CapturedBy: changes.CapturedByASZPlugin, Session: "S", Stream: stream, Tool: "t", Time: "t", Basis: changes.BasisToolWindow}
}

func TestAppendWritesOneLinePerRecordPerStream(t *testing.T) {
	dir := t.TempDir()
	for _, r := range []*changes.Record{rec("a", "main"), rec("b", "main"), rec("c", "a1")} {
		if err := output.Append(dir, r); err != nil {
			t.Fatal(err)
		}
	}
	main, err := os.ReadFile(output.Path(dir, "S", "main"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(main), "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], `{"schema":"changes/1","id":"a"`) || !strings.HasSuffix(string(main), "\n") {
		t.Fatalf("main stream file:\n%s", main)
	}
	if a1, _ := os.ReadFile(output.Path(dir, "S", "a1")); strings.Count(string(a1), "\n") != 1 {
		t.Fatalf("agent stream file:\n%s", a1)
	}
	if err := output.Append(dir, &changes.Record{Schema: changes.Schema}); err == nil {
		t.Fatal("an invalid record was written")
	}
}

func TestPruneRemovesExpiredFiles(t *testing.T) {
	dir := t.TempDir()
	_ = output.Append(dir, rec("a", "main"))
	_ = output.Append(dir, rec("b", "a1"))
	old := time.Now().Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(output.Path(dir, "S", "a1"), old, old); err != nil {
		t.Fatal(err)
	}
	removed, err := output.Prune(dir, 30*24*time.Hour, time.Now())
	if err != nil || removed != 1 {
		t.Fatalf("removed %d err=%v", removed, err)
	}
	if _, err := os.Stat(output.Path(dir, "S", "a1")); !os.IsNotExist(err) {
		t.Fatal("the expired file is still there")
	}
	if _, err := os.Stat(output.Path(dir, "S", "main")); err != nil {
		t.Fatal("the fresh file is gone")
	}
}
