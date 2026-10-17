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

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
)

func warningConfig(t *testing.T, retire string) *config.Config {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("USERPROFILE", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "user"))
	if err := os.MkdirAll(filepath.Join(root, "user", "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	was := claudeManagedDir
	claudeManagedDir = filepath.Join(root, "managed")
	t.Cleanup(func() { claudeManagedDir = was })
	cfg := config.Default()
	cfg.Storage.Root = filepath.Join(root, "landed")
	cfg.Storage.Retention = config.Retention{MetadataOnlyAfter: "1d", RetireAfter: retire}
	cfg.Adapters = []config.Adapter{{
		Name: config.AdapterClaudeCodeLocal, Enabled: true,
		Collector: config.Collector{Mode: config.ModeOnce, MaxDeltaBytes: 1 << 20},
	}}
	return cfg
}

func writeWarningSettings(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func warningText(t *testing.T, cfg *config.Config) string {
	t.Helper()
	_, retire, err := cfg.Storage.Retention.Durations()
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(retentionWarnings(cfg.Adapters, retire), "\n")
}

func checkWarningText(t *testing.T, text string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("warning does not contain %q:\n%s", want, text)
		}
	}
}

func TestRetentionWarningComparesInspectedDays(t *testing.T) {
	for _, retire := range []string{"7d", "30d", "60d", "720h"} {
		t.Run(retire, func(t *testing.T) {
			cfg := warningConfig(t, retire)
			path := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json")
			writeWarningSettings(t, path, `{"cleanupPeriodDays":30}`)
			text := warningText(t, cfg)
			checkWarningText(t, text, "The inspected user settings", "set cleanupPeriodDays to 30 days.",
				"Claude Code's effective retention was not determined.", "Project settings", "command-line settings", "device policy", "settings managed by a server",
				"version 2.1.248", "Claude Desktop and Cowork keep sessions at any age by default.",
				"desktopSessionCleanupPeriodDays or a managed cleanupPeriodDays can limit them.",
				"The organization's HIPAA configuration also ends this exemption when it applies.",
				"Settings that cannot be read or parsed pause Claude Code's cleanup unless managed settings provide cleanupPeriodDays.")
			if !strings.Contains(text, filepath.Base(path)) {
				t.Errorf("warning does not name the inspected file:\n%s", text)
			}
			if warns := strings.Contains(text, "A session resumed after it is retired is not collected."); warns != (retire == "7d") {
				t.Errorf("retire_after %s comparison:\n%s", retire, text)
			}
			if retire == "7d" {
				checkWarningText(t, text, "For the inspected value: Claude Code keeps a session for 30 days; storage.retention.retire_after is 7 days.")
			}
		})
	}
}

func TestRetentionWarningUsesDefaultOnlyWhenUnset(t *testing.T) {
	for _, c := range []struct{ name, body, want string }{
		{"unset", `{}`, "Claude Code's default is 30 days."},
		{"invalid", `{"cleanupPeriodDays":0}`, "No default was assumed."},
		{"cannot parse", `{`, "No default was assumed."},
		{"not an object", `[]`, "No default was assumed."},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := warningConfig(t, "7d")
			path := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json")
			writeWarningSettings(t, path, c.body)
			text := warningText(t, cfg)
			checkWarningText(t, text, c.want, "Claude Code's effective retention was not determined.")
			if c.name == "unset" {
				checkWarningText(t, text, "No inspected file", "For the default: Claude Code keeps a session for 30 days;", "is 7 days.")
			} else {
				checkWarningText(t, text, path, "No value was assumed for it.")
				if strings.Contains(text, "keeps a session for") || strings.Contains(text, "default is 30 days") {
					t.Errorf("a failed inspection assumed a duration:\n%s", text)
				}
			}
		})
	}
}

func TestRetentionWarningUsesManagedValue(t *testing.T) {
	cfg := warningConfig(t, "7d")
	writeWarningSettings(t, filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"), `{"cleanupPeriodDays":30}`)
	path := filepath.Join(claudeManagedDir, "managed-settings.d", "policy.json")
	writeWarningSettings(t, path, `{"cleanupPeriodDays":2}`)
	text := warningText(t, cfg)
	checkWarningText(t, text, "The inspected managed settings", "policy.json", "set cleanupPeriodDays to 2 days.", "effective retention was not determined")
	if strings.Contains(text, "keeps a session for") || strings.Contains(text, "30 days") {
		t.Errorf("the comparison used the user value:\n%s", text)
	}
}

func TestRetentionWarningResolvesUserSettings(t *testing.T) {
	for _, source := range []string{"CLAUDE_CONFIG_DIR", "XDG_CONFIG_HOME", "home"} {
		t.Run(source, func(t *testing.T) {
			cfg := warningConfig(t, "7d")
			paths := []string{
				filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"),
				filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "claude", "settings.json"),
				filepath.Join(os.Getenv("HOME"), ".claude", "settings.json"),
			}
			for i, body := range []string{`{"cleanupPeriodDays":10}`, `{"cleanupPeriodDays":20}`, `{"cleanupPeriodDays":40}`} {
				writeWarningSettings(t, paths[i], body)
			}
			want := "10 days"
			if source != "CLAUDE_CONFIG_DIR" {
				t.Setenv("CLAUDE_CONFIG_DIR", "")
				want = "20 days"
			}
			if source == "home" {
				t.Setenv("XDG_CONFIG_HOME", "")
				want = "40 days"
			}
			checkWarningText(t, warningText(t, cfg), "set cleanupPeriodDays to "+want+".")
		})
	}
}

func TestRetentionWarningWithExplicitSourceDoesNotInspect(t *testing.T) {
	cfg := warningConfig(t, "7d")
	cfg.Adapters[0].SourceRoot = t.TempDir()
	writeWarningSettings(t, filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"), "invalid")
	writeWarningSettings(t, filepath.Join(claudeManagedDir, "managed-settings.json"), "invalid")
	text := warningText(t, cfg)
	checkWarningText(t, text, "No Claude Code settings were inspected.", "explicit source_root", "effective retention was not determined", "Claude Desktop and Cowork", "pause Claude Code's cleanup unless managed settings provide cleanupPeriodDays")
	if strings.Contains(text, "cannot parse") || strings.Contains(text, "keeps a session for") || strings.Contains(text, "default is") {
		t.Errorf("an explicit source_root inspected or assumed settings:\n%s", text)
	}
}

func TestRetentionWarningForKiro(t *testing.T) {
	for _, retire := range []string{"7d", "60d", "100000d"} {
		t.Run(retire, func(t *testing.T) {
			cfg := warningConfig(t, retire)
			cfg.Adapters = []config.Adapter{{Name: config.AdapterKiroLocal, Enabled: true}}
			checkWarningText(t, warningText(t, cfg), "Kiro keeps a session until it is deleted; storage.retention.retire_after is", "A session resumed after it is retired is not collected.")
		})
	}
}

func TestRetentionWarningIsSilentWhenNotNeeded(t *testing.T) {
	for _, c := range []struct {
		name, retire string
		ads          []config.Adapter
	}{
		{"off", "-1", []config.Adapter{{Name: config.AdapterClaudeCodeLocal, Enabled: true}, {Name: config.AdapterKiroLocal, Enabled: true}}},
		{"omitted", "", []config.Adapter{{Name: config.AdapterClaudeCodeLocal, Enabled: true}, {Name: config.AdapterKiroLocal, Enabled: true}}},
		{"disabled", "7d", []config.Adapter{{Name: config.AdapterClaudeCodeLocal}, {Name: config.AdapterKiroLocal}}},
		{"LangChain alone", "7d", []config.Adapter{{Name: config.AdapterLangSmithIngest, Enabled: true}, {Name: config.AdapterChanges, Enabled: true}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := warningConfig(t, c.retire)
			cfg.Adapters = c.ads
			if text := warningText(t, cfg); text != "" {
				t.Fatalf("unexpected warning:\n%s", text)
			}
		})
	}
}

func TestRetentionWarningAtStartupInStatusAndFirstPass(t *testing.T) {
	cfg := warningConfig(t, "7d")
	writeWarningSettings(t, filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"), `{"cleanupPeriodDays":30}`)
	ref := warningRefresher(t, cfg)
	text := warningText(t, cfg)
	startup := stdoutOf(t, func() { printPipeline(ref, cfg) })
	for _, line := range strings.Split(text, "\n") {
		if strings.Count(startup, "warning  : "+line+"\n") != 1 {
			t.Errorf("startup did not print the warning once:\n%s", startup)
		}
	}
	if strings.Index(startup, "warning  :") < strings.Index(startup, "retention:") {
		t.Errorf("startup printed the warning before retention:\n%s", startup)
	}
	for _, st := range []view.Status{ref.base, {Refreshing: true}, {LastError: "a pass failed"}, {}} {
		ref.setStatus(st)
		checkRetentionStatus(t, ref.srv, text)
	}
	for pass := range 2 {
		output := stderrOf(t, func() {
			stdoutOf(t, func() {
				if err := ref.pass(); err != nil {
					t.Fatal(err)
				}
			})
		})
		if pass == 0 {
			for _, line := range strings.Split(text, "\n") {
				if strings.Count(output, "  warning: "+line+"\n") != 1 {
					t.Errorf("first pass did not print the warning once on standard error:\n%s", output)
				}
			}
		} else if output != "" {
			t.Errorf("next pass repeated a warning:\n%s", output)
		}
		checkRetentionStatus(t, ref.srv, text)
	}
}

func TestRetentionWarningOnAWaitingFirstPass(t *testing.T) {
	cfg := warningConfig(t, "7d")
	ref := warningRefresher(t, cfg)
	lock, err := storage.LockRoot(ref.zone.Root())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Unlock() })
	for pass := range 2 {
		output := stderrOf(t, func() {
			stdoutOf(t, func() {
				if err := ref.pass(); err != nil {
					t.Fatal(err)
				}
			})
		})
		if warned := strings.Contains(output, "effective retention was not determined"); warned != (pass == 0) {
			t.Errorf("waiting pass %d warning:\n%s", pass, output)
		}
		checkRetentionStatus(t, ref.srv, warningText(t, cfg))
	}
}

func warningRefresher(t *testing.T, cfg *config.Config) *refresher {
	t.Helper()
	zone := storage.NewZone(cfg.Storage.Root)
	ref, err := newRefresher(view.New(zone, nil), zone, cfg.Adapters, noMetrics, 2<<20, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ref.close)
	metadataOnly, retire, err := cfg.Storage.Retention.Durations()
	if err != nil {
		t.Fatal(err)
	}
	ref.retain(metadataOnly, retire)
	return ref
}

func checkRetentionStatus(t *testing.T, srv *view.Server, want string) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status response: %d", rec.Code)
	}
	var status struct {
		RetentionWarning string `json:"retention_warning"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.RetentionWarning != want {
		t.Fatalf("status warning = %q, want %q", status.RetentionWarning, want)
	}
}

func TestRetentionWarningInSources(t *testing.T) {
	for _, name := range []string{config.AdapterClaudeCodeLocal, config.AdapterKiroLocal} {
		t.Run(name, func(t *testing.T) {
			cfg := warningConfig(t, "7d")
			writeWarningSettings(t, filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"), `{"cleanupPeriodDays":30}`)
			claude := cfg.Adapters[0]
			kiro := config.Adapter{Name: config.AdapterKiroLocal, Enabled: true, SourceRoot: t.TempDir()}
			cfg.Adapters = []config.Adapter{claude, kiro}
			ad := claude
			if name == config.AdapterKiroLocal {
				ad = kiro
			}
			output := stdoutOf(t, func() {
				if err := cmdSources(cfg, ad, false); err != nil {
					t.Fatal(err)
				}
			})
			for _, line := range retentionWarnings([]config.Adapter{ad}, 7*24*time.Hour) {
				if strings.Count(output, "warning  : "+line+"\n") != 1 {
					t.Errorf("sources did not print the warning once:\n%s", output)
				}
			}
			if name == config.AdapterClaudeCodeLocal && strings.Contains(output, "Kiro keeps") || name == config.AdapterKiroLocal && strings.Contains(output, "Claude Code") {
				t.Errorf("sources printed another runtime's warning:\n%s", output)
			}
		})
	}
}
