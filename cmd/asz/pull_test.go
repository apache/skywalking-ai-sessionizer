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
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A pull's arguments are checked before anything is asked or written: a
// usage error exits 2, and the storage root is never made. No remote is
// reached either: the one configured listens nowhere.
func TestAPullsArgumentsAreCheckedFirst(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "data")
	cfg := filepath.Join(dir, "asz.yaml")
	if err := os.WriteFile(cfg, []byte("storage:\n  root: "+root+"\nremotes:\n  - name: prod\n    url: http://127.0.0.1:1\n    token_env: ASZ_TEST_NO_TOKEN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"nothing":                   {},
		"-list with a pair":         {"-list", "prod/c1"},
		"a listing flag on a pull":  {"-limit", "5", "prod/c1"},
		"an empty listing flag":     {"-match", "", "prod/c1"},
		"no separator":              {"prod"},
		"a tag":                     {"prod/c1:20261001T000000.000000000Z"},
		"latest":                    {"prod/c1:latest"},
		"an unconfigured remote":    {"other/c1"},
		"an unsafe conversation":    {"prod/.."},
		"-after without one remote": {"-list", "-after", "x"},
		"an unconfigured -remote":   {"-list", "-remote", "other"},
	} {
		if code := cmdPull(append([]string{"-config", cfg}, args...)); code != 2 {
			t.Errorf("%s: exit %d", name, code)
		}
		if _, err := os.Lstat(root); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s: the root was made: %v", name, err)
		}
	}
}

// Equal pairs are one pull.
func TestEqualPairsAreOnePull(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "asz.yaml")
	if err := os.WriteFile(path, []byte("storage:\n  root: "+filepath.Join(dir, "data")+"\nremotes:\n  - name: prod\n    url: http://127.0.0.1:1\n    token_env: X\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	targets, err := pullTargets(cfg, []string{"prod/c1", "prod/c2", "prod/c1"})
	if err != nil || len(targets) != 2 {
		t.Fatalf("%v %v", targets, err)
	}
}

// An https:// proxy is refused before anything is dialed: Go would use the
// remote's TLS settings on both hops. The pull runs in a child process,
// since Go reads the proxy settings once for a process.
func TestAnHTTPSProxyIsRefused(t *testing.T) {
	if os.Getenv("ASZ_PULL_CHILD") == "1" {
		os.Exit(cmdPull(strings.Fields(os.Getenv("ASZ_PULL_ARGS"))))
	}
	dir := t.TempDir()
	cfg := filepath.Join(dir, "asz.yaml")
	if err := os.WriteFile(cfg, []byte("storage:\n  root: "+filepath.Join(dir, "data")+
		"\nremotes:\n  - name: prod\n    url: https://asz.invalid:8788\n    token_env: ASZ_PULL_TOKEN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestAnHTTPSProxyIsRefused$")
	for _, kv := range os.Environ() {
		if !strings.HasSuffix(strings.ToLower(strings.SplitN(kv, "=", 2)[0]), "_proxy") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "ASZ_PULL_CHILD=1", "ASZ_PULL_ARGS=-config "+cfg+" prod/c1", "ASZ_PULL_TOKEN=x",
		"HTTPS_PROXY=https://proxy.invalid:3128")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(out), "use an http:// proxy or none") {
		t.Fatalf("%v: %s", err, out)
	}
}
