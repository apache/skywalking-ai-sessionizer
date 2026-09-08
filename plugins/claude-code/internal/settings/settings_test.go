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

package settings_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/plugins/claude-code/internal/settings"
)

func TestNoFileMeansTheDefaults(t *testing.T) {
	s, err := settings.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if s.Exclude.Defaults != "standard-v1" || !s.ReadOnly.On() || !s.IsScopeTool("Bash") || s.IsScopeTool("Edit") ||
		s.Retention.Idle != 30*time.Minute || s.Retention.TTL != 30*24*time.Hour || s.ScanTimeout != 30*time.Second || s.SizeCap != 1<<20 {
		t.Fatalf("defaults: %+v", s)
	}
}

func TestAFileOverridesWhatItNamesAndKeepsTheRest(t *testing.T) {
	dir := t.TempDir()
	text := "roots: [/work/a]\nexclude:\n  add: [\"/data/\"]\n  remove: [\"**/vendor/\"]\nread_only:\n  enabled: false\ntools:\n  scope: [Bash]\nretention:\n  ttl: 48h\nsize_cap: 4096\n"
	if err := os.WriteFile(filepath.Join(dir, settings.File), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := settings.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Roots) != 1 || s.Roots[0] != "/work/a" || s.Exclude.Defaults != "standard-v1" || len(s.Exclude.Add) != 1 || len(s.Exclude.Remove) != 1 {
		t.Fatalf("roots and exclusions: %+v", s)
	}
	if s.ReadOnly.On() || !s.IsScopeTool("Bash") || s.IsScopeTool("Monitor") {
		t.Fatalf("read-only and tools: %+v", s)
	}
	if s.Retention.TTL != 48*time.Hour || s.Retention.Idle != 30*time.Minute || s.SizeCap != 4096 || s.ScanTimeout != 30*time.Second {
		t.Fatalf("retention and caps: %+v", s)
	}
	if err := os.WriteFile(filepath.Join(dir, settings.File), []byte("roots: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := settings.Load(dir); err == nil {
		t.Fatal("a file that does not parse was accepted")
	}
}
