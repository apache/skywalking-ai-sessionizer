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

package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestNamedAdaptersKeepDefaultsAndExplicitOverrides(t *testing.T) {
	for _, def := range Default().Adapters {
		t.Run(def.Name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "asz.yaml")
			write := func(body string) *Config {
				t.Helper()
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
				c, err := Load(path)
				if err != nil {
					t.Fatal(err)
				}
				return c
			}
			c := write("adapters:\n  - name: " + def.Name + "\n")
			if len(c.Adapters) != 1 || !reflect.DeepEqual(c.Adapters[0], def) {
				t.Fatalf("named defaults: got %+v, want %+v", c.Adapters, def)
			}
			override := "adapters:\n  - name: " + def.Name + "\n    enabled: false\n    exclude: []\n    metrics: false\n"
			c = write(override)
			a := c.Adapters[0]
			if a.Enabled || a.Metrics || len(a.Exclude) != 0 {
				t.Fatalf("explicit false/empty values were lost: %+v", a)
			}
		})
	}
}

func TestCollectorFieldsMergeWithNamedDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "asz.yaml")
	if err := os.WriteFile(path, []byte("adapters:\n  - name: claude-code-local\n    source_root: /capture\n    collector:\n      mode: once\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	a := c.Adapters[0]
	if !a.Enabled || a.SourceRoot != "/capture" || a.Collector.Mode != ModeOnce || a.Collector.Interval != Default().Adapters[0].Collector.Interval || len(a.Exclude) != 1 {
		t.Fatalf("partial adapter lost defaults: %+v", a)
	}
}

// TestTheOldAdapterNameStillLoads holds the promise a rename has to keep.
//
// An adapter's name lives in two places that outlast a release: a
// configuration someone wrote, and the header of every file landed under it.
// If either stopped being read, an upgrade would skip the adapter as unknown
// and collect nothing, saying so only in a line on standard error.
func TestTheOldAdapterNameStillLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "asz.yaml")
	if err := os.WriteFile(path, []byte(`
storage:
  root: ./data
adapters:
  - name: claude-code-changes
    enabled: true
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("a configuration written before the rename no longer loads: %v", err)
	}
	var found bool
	for _, a := range cfg.Adapters {
		if a.Name == AdapterClaudeCodeChanges {
			found = true
			if !a.Enabled {
				t.Error("the adapter loaded but is not enabled")
			}
			if a.Collector.Mode == "" {
				t.Error("the adapter loaded without its collector defaults")
			}
		}
	}
	if !found {
		t.Fatal("the old name did not survive loading")
	}
}
