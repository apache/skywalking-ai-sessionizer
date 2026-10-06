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
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
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
//
// Loading supplies the defaults, so a loaded configuration cannot say
// whether a key was written. The file is read as a document and its keys are
// held against the configuration's own, section by section. An adapter entry
// is held to the keys every entry carries, its name and its switch, since a
// receiver and a local adapter spell out different settings.
func TestRepoConfigSpellsOutEveryValue(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "asz.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		t.Fatal("asz.yaml is not one mapping")
	}
	var missing []string
	keysWritten(reflect.TypeOf(Config{}), doc.Content[0], "", &missing)
	if len(missing) > 0 {
		t.Fatalf("asz.yaml leaves these to the defaults, and a reader who edits the file would see nothing change: %v", missing)
	}
	got, err := Load(filepath.Join("..", "..", "asz.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Adapters) != 5 {
		t.Fatalf("adapters: got %d, want the local adapter, the two receivers, the changes adapter and the provider adapter", len(got.Adapters))
	}
	for _, a := range got.Adapters {
		if isReceiver(a.Name) {
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

// The receiver adapter needs an address, and it is a server with no
// collector.
func TestReceiverNeedsAnAddress(t *testing.T) {
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
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a receiver with an address must be accepted: %v", err)
	}
}

// Metrics are one section for the root, read as written, false included.
// A section left out derives every metric with a look-back of three days.
func TestMetricsAreOneSectionForTheRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "asz.yaml")
	load := func(body string) (*Config, error) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return Load(path)
	}
	cfg, err := load("parse:\n  max_round_bytes: 1048576\n")
	if err != nil {
		t.Fatal(err)
	}
	if d, err := cfg.Metrics.LookbackDuration(); !cfg.Metrics.On() || err != nil || d != 72*time.Hour {
		t.Fatalf("a file without a metrics section: on=%v look-back %s %v, want on and 72h", cfg.Metrics.On(), d, err)
	}
	// A section built in code, with nothing set, is the same.
	if d, err := (Metrics{}).LookbackDuration(); !(Metrics{}).On() || err != nil || d != 72*time.Hour {
		t.Fatalf("an empty metrics section: look-back %s %v, want on and 72h", d, err)
	}
	if cfg, err = load("metrics:\n  enabled: false\n"); err != nil || cfg.Metrics.On() {
		t.Fatalf("enabled: false was not read: %v", err)
	}
	for body, want := range map[string]time.Duration{
		"metrics:\n  lookback: 3d\n":   72 * time.Hour,
		"metrics:\n  lookback: 6h\n":   6 * time.Hour,
		"metrics:\n  lookback: none\n": 0,
	} {
		cfg, err := load(body)
		if err != nil {
			t.Fatal(err)
		}
		if d, err := cfg.Metrics.LookbackDuration(); err != nil || d != want {
			t.Fatalf("%q: look-back %s %v, want %s", body, d, err, want)
		}
	}
	if _, err := load("metrics:\n  lookback: soon\n"); err == nil {
		t.Fatal("a look-back that is not a duration was accepted")
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

// TestAnOldAdapterNameStillGetsItsDefaults.
//
// An adapter that was renamed answers to both names. The defaults were found
// by literal name, so a configuration written before the rename got none of
// them - including the exclusion that keeps the tool's own sessions out of a
// person's conversations. Nothing failed; the setting was simply gone.
func TestAnOldAdapterNameStillGetsItsDefaults(t *testing.T) {
	var current, old Adapter
	if err := yaml.Unmarshal([]byte("name: changes\n"), &current); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte("name: claude-code-changes\n"), &old); err != nil {
		t.Fatal(err)
	}
	if len(current.Exclude) == 0 {
		t.Fatal("the current name has no default exclusions, so this test proves nothing")
	}
	if len(old.Exclude) != len(current.Exclude) {
		t.Fatalf("the old name got %v, the current name %v", old.Exclude, current.Exclude)
	}
	if old.Name != "claude-code-changes" {
		t.Errorf("the name was rewritten to %q; a configuration says what it says", old.Name)
	}
}

// TestProviderBodiesIsASettingOfTheReceiverAlone.
//
// The langsmith-ingest receiver lands what each call was sent unless told
// not to, and no other adapter takes the setting: Claude Code's bodies come
// through their own adapter. A collector test that sets the flag on the
// collector directly cannot show that the setting reaches it, so this holds
// the configuration side.
func TestProviderBodiesIsASettingOfTheReceiverAlone(t *testing.T) {
	for _, a := range Default().Adapters {
		if a.Name == AdapterLangSmithIngest && !a.ProviderBodies {
			t.Error("the receiver does not land bodies by default")
		}
		if a.Name != AdapterLangSmithIngest && a.ProviderBodies {
			t.Errorf("%s takes provider_bodies by default", a.Name)
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "asz.yaml")
	if err := os.WriteFile(path, []byte("storage:\n  root: "+dir+"\nadapters:\n  - name: langsmith-ingest\n    enabled: true\n    listen: 127.0.0.1:0\n    provider_bodies: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range cfg.Adapters {
		if a.Name == AdapterLangSmithIngest && a.ProviderBodies {
			t.Error("provider_bodies: false was read as true")
		}
	}
	if err := os.WriteFile(path, []byte("storage:\n  root: "+dir+"\nadapters:\n  - name: langsmith-ingest\n    enabled: true\n    listen: 127.0.0.1:0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range cfg.Adapters {
		if a.Name == AdapterLangSmithIngest && !a.ProviderBodies {
			t.Error("a receiver entry that says nothing about provider_bodies lost the default")
		}
	}
	if err := os.WriteFile(path, []byte("storage:\n  root: "+dir+"\nadapters:\n  - name: claude-code-local\n    provider_bodies: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("another adapter was allowed provider_bodies")
	}
}

// keysWritten walks a configuration type's yaml keys against a mapping node
// and collects every key the node does not carry. A nested section is
// walked; a list of adapters is held to name and enabled per entry.
func keysWritten(typ reflect.Type, node *yaml.Node, prefix string, missing *[]string) {
	keys := map[string]*yaml.Node{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		keys[node.Content[i].Value] = node.Content[i+1]
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if tag == "" || tag == "-" {
			continue
		}
		child, ok := keys[tag]
		if !ok {
			*missing = append(*missing, prefix+tag)
			continue
		}
		switch {
		case f.Type.Kind() == reflect.Struct && child.Kind == yaml.MappingNode:
			keysWritten(f.Type, child, prefix+tag+".", missing)
		case f.Type.Kind() == reflect.Slice && f.Type.Elem().Kind() == reflect.Struct && child.Kind == yaml.SequenceNode:
			for j, el := range child.Content {
				for _, must := range []string{"name", "enabled"} {
					found := false
					for k := 0; k+1 < len(el.Content); k += 2 {
						found = found || el.Content[k].Value == must
					}
					if !found {
						*missing = append(*missing, fmt.Sprintf("%s%s[%d].%s", prefix, tag, j, must))
					}
				}
			}
		}
	}
}

// TestSeveralChangeRecordersAreOneAdapterEach.
//
// A machine can run Claude Code's plugin and a LangChain shim at once, each
// writing its own directory, so the changes adapter may be named once per
// directory. Two entries reading one directory would land it twice, and the
// adapter's old name is the same adapter, so both are refused - before, the
// second silently replaced the first. Every other adapter is one entry.
func TestSeveralChangeRecordersAreOneAdapterEach(t *testing.T) {
	once := Collector{Mode: ModeOnce, Interval: time.Minute, MaxDeltaBytes: 1 << 20}
	cfg := Default()
	cfg.Adapters = append(cfg.Adapters, Adapter{Name: AdapterChanges, Enabled: true, SourceRoot: "/var/lib/asz/changes", Collector: once})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a second changes entry with a directory of its own was refused: %v", err)
	}
	cfg.Adapters = append(cfg.Adapters, Adapter{Name: AdapterClaudeCodeChanges, Enabled: true, SourceRoot: "/var/lib/asz/changes", Collector: once})
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "both read") {
		t.Fatalf("two entries reading one directory were accepted: %v", err)
	}
	cfg = Default()
	cfg.Adapters = append(cfg.Adapters, Adapter{Name: AdapterClaudeCodeChanges, Enabled: true, Collector: once})
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "both read") {
		t.Fatalf("the old name beside the default entry, both reading Claude Code's directory, was accepted: %v", err)
	}
	cfg = Default()
	cfg.Adapters = append(cfg.Adapters, Adapter{Name: AdapterClaudeCodeLocal, Enabled: true, Collector: once})
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("a second local adapter was accepted: %v", err)
	}
}

// The view section says what a reader is kept from, so a key it does not have
// is refused rather than ignored, and so is a hide written anywhere else that
// names a flag view does not withhold.
func TestAMistakenViewKeyIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		ok         bool
	}{
		{"hide in the view section", "view:\n  hide: [system_prompt]\n", true},
		{"an empty view section", "view:\n", true},
		{"no view section", "storage:\n  root: ./data\n", true},
		{"a misspelled key", "view:\n  hidden: [system_prompt]\n", false},
		{"hide at the top level", "hide: [system_prompt]\n", false},
		{"a misspelled section", "views:\n  hide: [system_prompt]\n", false},
		{"a section spelled in capitals", "View:\n  hide: [system_prompt]\n", false},
		{"a name the view cannot withhold", "view:\n  hide: [finished]\n", false},
		{"view from an anchor kept at the top level", "x: &v {hide: [system_prompt]}\nview: *v\n", true},
		{"a list kept at the top level for view", "hide: &h [system_prompt]\nview: {hide: *h}\n", true},
		{"a misspelled key through an alias", "x: &v {hidden: [system_prompt]}\nview: *v\n", false},
		{"hide merged in at the top level", "<<: {hide: [system_prompt]}\n", false},
		{"hide merged in from an anchor", "x: &h {hide: [system_prompt]}\n<<: *h\n", false},
		{"a misspelled section holding an anchor nothing refers to", "views: &v {hide: [system_prompt]}\n", false},
		{"a misspelled section holding hide, its anchor merged elsewhere", "veiw: &v {hide: [system_prompt]}\nstorage: {<<: *v, root: ./data}\n", false},
		{"hide under a section", "storage:\n  root: ./data\n  hide: [system_prompt]\n", false},
		{"hide under an adapter", "adapters:\n  - name: claude-code-local\n    hide: [system_prompt]\n", false},
		{"hide merged into an adapter", "adapters:\n  - name: claude-code-local\n    <<: {hide: [system_prompt]}\n", false},
		{"hide outside view naming more than view", "x: &h {hide: [system_prompt, tool_schemas]}\nview: {hide: [system_prompt]}\n", false},
		{"hide merged into view", "x: &h {hide: [system_prompt]}\nview: {<<: *h}\n", true},
		{"hide merged into view through two anchors", "a: &a {hide: [system_prompt]}\nb: &b {<<: *a}\nview: {<<: *b}\n", true},
		{"view merged in at the top level", "<<: {view: {hide: [system_prompt]}}\n", true},
		{"view merged in from an anchor", "base: &b\n  view:\n    hide: [system_prompt]\n<<: *b\n", true},
		{"view indented under another section", "export:\n  otlp:\n    endpoint: x:1\n  view:\n    hide: [system_prompt]\n", false},
		{"view indented under an adapter", "adapters:\n  - name: claude-code-local\n    view:\n      hide: [system_prompt]\n", false},
		{"a misspelled section holding hide twice", "veiw:\n  hide: [system_prompt]\n  hide: [tool_schemas]\n", false},
		{"a misspelled section merging a number", "veiw:\n  <<: 1\n  hide: [system_prompt]\n", false},
		{"a misspelled section merging itself", "veiw: &v\n  hide: [system_prompt]\n  <<: *v\n", false},
		{"a hide in view that its own key overrides", "view:\n  <<: {hide: [tool_schemas]}\n  hide: [system_prompt]\n", false},
		{"view and hide written as one top-level key", "view.hide: [system_prompt]\n", false},
		{"a misspelled section and hide written as one top-level key", "views.hide: [system_prompt]\n", false},
		{"view and hide written as one word", "viewHide: [system_prompt]\nVIEWHIDE: [tool_schemas]\n", false},
		{"one name under view and hide written as one key", "view.hide: system_prompt\n", false},
		{"hide spelled in capitals", "Hide: [system_prompt]\n", false},
		{"hide spelled in capitals under a misspelled section", "veiw: {Hide: [system_prompt]}\n", false},
		{"one name under a misspelled section", "veiw:\n  hide: system_prompt\n", false},
		{"names written as one value under a misspelled section", "views:\n  hide: system_prompt,tool_schemas\n", false},
		{"one name under view indented into another section", "export:\n  view:\n    hide: system_prompt\n", false},
		{"a list that also holds a mapping", "veiw:\n  hide:\n    - system_prompt\n    - {}\n", false},
		{"view under a key with no name", "\"\": {view: {hide: [system_prompt]}}\n", false},
		{"a list of lists at the top level", "hide: [[tool_schemas]]\n", false},
		{"a list of lists under a misspelled section", "veiw:\n  hide: [[system_prompt]]\n", false},
		{"a list under a section that holds itself", "storage: {hide: &a [*a, system_prompt]}\n", false},
		{"a list of lists merged in at the top level", "<<: {hide: [[system_prompt]]}\n", false},
		{"a list of lists in a list of merges", "<<: [{storage: {root: ./data}}, {hide: [[system_prompt]]}]\n", false},
		{"a second document", "storage:\n  root: ./data\n---\nview:\n  hide: [system_prompt]\n", false},
		{"a first document left empty", "---\n---\nview:\n  hide: [system_prompt]\n", false},
		{"a document marker at the end", "view:\n  hide: [system_prompt]\n---\n", true},
	} {
		path := filepath.Join(t.TempDir(), "asz.yaml")
		if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if (err == nil) != tc.ok {
			t.Errorf("%s: error %v, want ok %v", tc.name, err, tc.ok)
			continue
		}
		// Every file that loads with a hide withholds what it says.
		if err == nil && strings.Contains(tc.body, "system_prompt") && !slices.Contains(cfg.View.Hide, "system_prompt") {
			t.Errorf("%s: loaded with hide %v, want system_prompt withheld", tc.name, cfg.View.Hide)
		}
	}
}

// Keys outside view are read as they always were: a key the configuration
// does not have is ignored, a mapping that is free to hold any key may hold
// one named hide, and a key that is no merge is never followed as one. What
// they set is checked, not only that the file loads.
func TestKeysOutsideViewAreReadAsBefore(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		check      func(*Config) bool
	}{
		{"a key the configuration does not have", "version: 1\nstorage: {root: /a}\n", func(c *Config) bool { return c.Storage.Root == "/a" }},
		{"an extension key", "x-defaults: 2\n", nil},
		{"a section spelled in capitals", "Storage:\n  root: /elsewhere\n", func(c *Config) bool { return c.Storage.Root == Default().Storage.Root }},
		{"a key holding an alias", "storage: {root: &r /a}\nkeep: *r\n", func(c *Config) bool { return c.Storage.Root == "/a" }},
		{"a section merged from an anchor", "base: &b\n  root: /b\nstorage:\n  <<: *b\n", func(c *Config) bool { return c.Storage.Root == "/b" }},
		{"a merge key at the top level", "<<: {storage: {root: /c}}\n", func(c *Config) bool { return c.Storage.Root == "/c" }},
		{"a list of merges, each a section", "<<: [{storage: {root: /d}}, {parse: {max_round_bytes: 1048576}}]\n",
			func(c *Config) bool { return c.Storage.Root == "/d" && c.Parse.MaxRoundBytes == 1048576 }},
		{"headers named for hide", "export:\n  otlp:\n    headers:\n      hide: \"yes\"\n      x-hide-token: abc\n",
			func(c *Config) bool {
				return c.Export.OTLP.Headers["hide"] == "yes" && c.Export.OTLP.Headers["x-hide-token"] == "abc"
			}},
		{"a quoted merge key holding itself", "\"<<\": &a [*a]\n", nil},
		{"a quoted merge key holding a merge of itself", "\"<<\": &a {\"<<\": *a}\n", nil},
		{"a merge key tagged as a string", "!!str <<: &a [*a]\n", nil},
		{"an anchor holding itself", "junk: &j [*j]\n", nil},
		{"a key that holds the letters of hide inside a word", "pushIdentity: ci-runner-7\nx-hide-these: &skip [a]\nhideBanner: true\n", nil},
		{"a hide at the top level with no value", "hide:\nstorage: {root: /e}\n", func(c *Config) bool { return c.Storage.Root == "/e" && len(c.View.Hide) == 0 }},
	} {
		path := filepath.Join(t.TempDir(), "asz.yaml")
		if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if tc.check != nil && !tc.check(cfg) {
			t.Errorf("%s: loaded, but not as written: %+v", tc.name, cfg)
		}
	}
}

// A refusal gives the line of the key, the key as spelled, and says how hide
// is written.
func TestAMisplacedHideSaysWhere(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"under an adapter", "adapters:\n  - name: claude-code-local\n    hide: [system_prompt]\n", "line 3: hide names"},
		{"a key the view section does not have", "view:\n  hidden: [system_prompt]\n", "line 2: hidden is not a key of the view section"},
		// The name view cannot withhold is the mistake, not the other hide
		// that names what view meant to.
		{"beside a view that names something it cannot withhold", "view: {hide: [\"system_prompt,tool_schemas\"]}\ndefaults: {hide: [system_prompt]}\n",
			`view.hide: "system_prompt,tool_schemas" is not a flag`},
	} {
		path := filepath.Join(t.TempDir(), "asz.yaml")
		if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(path)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: the refusal is %v, want it to say %q", tc.name, err, tc.want)
		}
		if err != nil && strings.Contains(tc.want, "names") && !strings.Contains(err.Error(), "written as view: and hide: under it") {
			t.Errorf("%s: the refusal %v does not say how hide is written", tc.name, err)
		}
	}
}
