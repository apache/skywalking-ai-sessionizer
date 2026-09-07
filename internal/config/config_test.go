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

// The asz.yaml at the repository root claims to be the built-in defaults
// written out. This holds it to that claim: a default changed in one place
// and not the other fails here rather than surprising someone who edited
// the file and saw nothing change.
func TestRepoConfigMatchesDefaults(t *testing.T) {
	got, err := Load(filepath.Join("..", "..", "asz.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	want := Default()
	// A list written as [] in YAML decodes as an empty slice while the
	// compiled default leaves it nil. Both mean "no entries".
	for _, c := range []*Config{got, want} {
		for i := range c.Adapters {
			if len(c.Adapters[i].Include) == 0 {
				c.Adapters[i].Include = nil
			}
			if len(c.Adapters[i].Exclude) == 0 {
				c.Adapters[i].Exclude = nil
			}
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("asz.yaml differs from Default()\n got: %+v\nwant: %+v", got, want)
	}
}

// Nothing in the file may be left to the merge: every value has to be
// present, or the file stops being documentation of the defaults.
func TestRepoConfigSpellsOutEveryValue(t *testing.T) {
	got, err := Load(filepath.Join("..", "..", "asz.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Adapters) != 2 {
		t.Fatalf("adapters: got %d, want the local adapter and the receiver", len(got.Adapters))
	}
	for _, a := range got.Adapters {
		if a.Name == AdapterClaudeCodeOTLP {
			// A receiver is a server and has no collector to spell out.
			if a.Collector != (Collector{}) {
				t.Fatalf("%s: a receiver carries collector settings: %+v", a.Name, a.Collector)
			}
			continue
		}
		if a.Collector.Mode == "" || a.Collector.Interval == 0 || a.Collector.MaxDeltaBytes == 0 {
			t.Fatalf("%s: collector values not spelled out: %+v", a.Name, a.Collector)
		}
	}
}

// The receiver adapter needs an address, and the same tokens are never
// counted twice: local derivation and the runtime's exporter cannot both
// have metrics on.
func TestReceiverNeedsAnAddressAndMetricsComeFromOneSource(t *testing.T) {
	cfg := Default()
	if cfg.Adapters[1].Name != AdapterClaudeCodeOTLP {
		t.Fatalf("the defaults list %s second, want the receiver", cfg.Adapters[1].Name)
	}
	cfg.Adapters[1].Enabled = true
	cfg.Adapters[1].Listen = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("a receiver without listen was accepted")
	}
	cfg.Adapters[1].Listen = "127.0.0.1:4317"
	cfg.Adapters[1].Collector.Mode = ModeWatch
	if err := cfg.Validate(); err == nil {
		t.Fatal("a receiver with collector settings was accepted; it is a server")
	}
	cfg.Adapters[1].Collector = Collector{}
	cfg.Adapters[1].Listen = "127.0.0.1:4317"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a receiver with metrics beside a local adapter without metrics must be accepted: %v", err)
	}
	cfg.Adapters[0].Metrics = true
	if err := cfg.Validate(); err == nil {
		t.Fatal("metrics on both adapters was accepted")
	}
	cfg.Adapters[1].Metrics = false
	if err := cfg.Validate(); err != nil {
		t.Fatalf("local metrics beside a receiver without metrics must be accepted: %v", err)
	}
	cfg.Adapters[1].Enabled = false
	cfg.Adapters[1].Metrics = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a disabled receiver must not count: %v", err)
	}
}

// The two switches of a push are read as written, false included, and a
// push with both off is refused.
func TestPushSwitchesAreReadAsWritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "asz.yaml")
	if err := os.WriteFile(path, []byte("export:\n  otlp:\n    logs: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Export.OTLP.SendLogs() || !cfg.Export.OTLP.SendMetrics() {
		t.Fatalf("logs: false was not read: logs=%v metrics=%v", cfg.Export.OTLP.SendLogs(), cfg.Export.OTLP.SendMetrics())
	}
	if err := os.WriteFile(path, []byte("export:\n  otlp:\n    logs: false\n    metrics: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("both switches off was accepted")
	}
}
