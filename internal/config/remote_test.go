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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

func loadText(t *testing.T, body string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "asz.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

const oneRemote = "remotes:\n  - name: prod\n    url: https://asz.example:8788\n    token_env: ASZ_PROD_TOKEN\n"

func TestThePullSettingsDefault(t *testing.T) {
	c, err := loadText(t, "storage:\n  root: ./data\n")
	if err != nil {
		t.Fatal(err)
	}
	local, err := c.RemoteSnapshotTTLDuration()
	if err != nil || local != 720*time.Hour {
		t.Fatalf("remote_snapshot_ttl: %v, %v", local, err)
	}
	server, err := c.RemoteServer.SnapshotTTLDuration()
	if err != nil || server != time.Hour {
		t.Fatalf("remote_server.snapshot_ttl: %v, %v", server, err)
	}
	if c.RemoteServer.MaxConcurrentPerToken != 2 || c.RemoteServer.Enabled() || c.RemoteServer.TLSEnabled() || len(c.Remotes) != 0 {
		t.Fatalf("remote_server defaults: %+v, remotes %v", c.RemoteServer, c.Remotes)
	}
}

func TestSnapshotTimesToLiveHaveALowerBound(t *testing.T) {
	for _, tc := range []struct {
		value string
		ok    bool
	}{
		{"2s", true}, {"2000ms", true}, {"720h", true},
		{"1999ms", false}, {"0", false}, {"0s", false}, {"-1h", false},
		{"10000000000h", false}, {"thirty days", false}, {"30d", false},
	} {
		for _, body := range []string{
			"remote_snapshot_ttl: " + tc.value + "\n",
			"remote_server:\n  snapshot_ttl: " + tc.value + "\n",
		} {
			_, err := loadText(t, body)
			if (err == nil) != tc.ok {
				t.Errorf("%q: error %v, want ok %v", body, err, tc.ok)
			}
		}
	}
}

func TestTheServerIntervalIsHalfTheTimeToLiveWithinBounds(t *testing.T) {
	for ttl, want := range map[string]time.Duration{
		"2s": time.Second, "3s": 1500 * time.Millisecond, "90s": 45 * time.Second,
		"2m": time.Minute, "1h": time.Minute,
	} {
		got, err := RemoteServer{SnapshotTTL: ttl}.SweepInterval()
		if err != nil || got != want {
			t.Errorf("%s: %v, %v; want %v", ttl, got, err, want)
		}
	}
}

func TestRequestsPerTokenAreOneToSixtyFour(t *testing.T) {
	// The decoder would make 64.9 into 64 and 1e1 into 10: only a whole
	// number is a count of requests.
	for value, ok := range map[string]bool{"1": true, "64": true, "0": false, "65": false, "-1": false,
		"64.9": false, "2.0": false, "1e1": false, `"2"`: false, "true": false, "[2]": false} {
		_, err := loadText(t, "remote_server:\n  max_concurrent_per_token: "+value+"\n")
		if (err == nil) != ok {
			t.Errorf("%s: error %v, want ok %v", value, err, ok)
		}
	}
}

func TestRemoteNamesArePortable(t *testing.T) {
	for _, name := range []string{"", ".", "..", "a/b", "a b", "prod:1", "CON", "nul.txt", ".tmp-x", "é", "end.", strings.Repeat("a", 256)} {
		_, err := loadText(t, "remotes:\n  - name: \""+name+"\"\n    url: http://127.0.0.1:8787\n    token_env: T\n")
		if err == nil {
			t.Errorf("remote name %q was accepted", name)
		}
	}
	for _, name := range []string{"prod", "Prod-2", "a_b.c", strings.Repeat("a", 255)} {
		if _, err := loadText(t, "remotes:\n  - name: "+name+"\n    url: http://127.0.0.1:8787\n    token_env: T\n"); err != nil {
			t.Errorf("remote name %q: %v", name, err)
		}
	}
}

func TestTwoRemotesCannotShareAName(t *testing.T) {
	for _, second := range []string{"prod", "PROD", "Prod"} {
		body := oneRemote + "  - name: " + second + "\n    url: http://127.0.0.1:8787\n    token_env: T\n"
		if _, err := loadText(t, body); err == nil {
			t.Errorf("prod and %s were both accepted", second)
		}
	}
}

func TestARemoteURLIsAnOrigin(t *testing.T) {
	for _, u := range []string{
		"", "ftp://host", "host:8787", "https://", "https://user:pw@host", "https://user@host",
		"https://host/?a=1", "https://host/?", "https://host/#x", "https://host#",
	} {
		body := "remotes:\n  - name: prod\n    url: \"" + u + "\"\n    token_env: T\n"
		if _, err := loadText(t, body); err == nil {
			t.Errorf("url %q was accepted", u)
		}
	}
	for u, origin := range map[string]string{
		"https://ASZ.Example/":            "https://asz.example:443",
		"http://asz.example":              "http://asz.example:80",
		"https://asz.example:8788/asz/":   "https://asz.example:8788",
		"http://[::1]:8787":               "http://[::1]:8787",
		"http://127.0.0.1:8787/prefix/v1": "http://127.0.0.1:8787",
	} {
		r := Remote{Name: "prod", URL: u, TokenEnv: "T"}
		got, err := r.Origin()
		if err != nil || got != origin {
			t.Errorf("%s: origin %q, %v; want %q", u, got, err, origin)
		}
	}
	base, err := Remote{Name: "prod", URL: "https://asz.example:8788/asz/"}.BaseURL()
	if err != nil || base.String() != "https://asz.example:8788/asz" {
		t.Fatalf("a path is kept as the routes' prefix: %v, %v", base, err)
	}
}

func TestARemoteTakesExactlyOneTokenSource(t *testing.T) {
	base := "remotes:\n  - name: prod\n    url: http://127.0.0.1:8787\n"
	for body, ok := range map[string]bool{
		base: false,
		base + "    token_file: t\n    token_env: T\n": false,
		base + "    token_file: ~/t\n":                 true,
		base + "    token_env: T\n":                    true,
	} {
		if _, err := loadText(t, body); (err == nil) != ok {
			t.Errorf("%q: error %v, want ok %v", body, err, ok)
		}
	}
}

func TestCertificatesComeInPairs(t *testing.T) {
	for body, ok := range map[string]bool{
		oneRemote + "    client_cert_file: c\n":                                               false,
		oneRemote + "    client_key_file: k\n":                                                false,
		oneRemote + "    client_cert_file: c\n    client_key_file: k\n":                       true,
		"remote_server:\n  tls:\n    cert_file: c\n":                                          false,
		"remote_server:\n  tls:\n    key_file: k\n":                                           false,
		"remote_server:\n  tls:\n    client_ca_file: ca\n":                                    false,
		"remote_server:\n  tls:\n    cert_file: c\n    key_file: k\n":                         true,
		"remote_server:\n  tls:\n    cert_file: c\n    key_file: k\n    client_ca_file: ca\n": true,
	} {
		if _, err := loadText(t, body); (err == nil) != ok {
			t.Errorf("%q: error %v, want ok %v", body, err, ok)
		}
	}
}

// Loading checks structure only. A credential is read when the operation
// that needs it opens it, so a missing token file, certificate or variable
// loads, and offline work never touches them.
func TestLoadingReadsNoCredential(t *testing.T) {
	body := "remotes:\n  - name: prod\n    url: https://asz.example\n    token_file: /nonexistent/token\n" +
		"    ca_file: /nonexistent/ca.pem\n    client_cert_file: /nonexistent/c.pem\n    client_key_file: /nonexistent/k.pem\n" +
		"  - name: other\n    url: https://asz.example\n    token_env: ASZ_NOT_SET_ANYWHERE\n" +
		"remote_server:\n  tokens_file: /nonexistent/tokens\n  tls:\n    cert_file: /nonexistent/c\n    key_file: /nonexistent/k\n"
	c, err := loadText(t, body)
	if err != nil {
		t.Fatal(err)
	}
	// On Windows a path from the root takes the working directory's drive.
	want, err := ResolvePath("/nonexistent/token")
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := c.Remote("prod"); !ok || r.TokenFile != want {
		t.Fatalf("remote prod: %+v %v", r, ok)
	}
	if _, ok := c.Remote("PROD"); ok {
		t.Fatal("a remote is found by its exact name only")
	}
}

func TestConfiguredPathsExpandHomeAndWorkingDirectory(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	got, err := ResolvePath("~/.asz/prod.token")
	if err != nil || got != filepath.Join(home, ".asz", "prod.token") {
		t.Fatalf("~/: %q, %v", got, err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	got, err = ResolvePath("certs/ca.pem")
	if err != nil || got != filepath.Join(wd, "certs", "ca.pem") {
		t.Fatalf("relative: %q, %v", got, err)
	}
	// Only a leading ~/ is the home directory.
	got, err = ResolvePath("~other/x")
	if err != nil || got != filepath.Join(wd, "~other", "x") {
		t.Fatalf("~other: %q, %v", got, err)
	}
}

// The configuration file is a supplied path: a file in a tree pull owns is
// refused before it is read, whether it is named there or reached through a
// link.
func TestAConfigurationInAProtectedTreeIsRefused(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"_remote/prod", "_pull", "x/_admit", "y/_leases"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		inside := filepath.Join(root, dir, "asz.yaml")
		if err := os.WriteFile(inside, []byte("storage:\n  root: ./data\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(inside); !errors.Is(err, storage.ErrProtected) {
			t.Errorf("%s: %v, want the protected-path refusal", inside, err)
		}
		link := filepath.Join(root, strings.ReplaceAll(dir, "/", "-")+".yaml")
		if err := os.Symlink(inside, link); err != nil {
			t.Skip(err)
		}
		if _, err := Load(link); !errors.Is(err, storage.ErrProtected) {
			t.Errorf("%s -> %s: %v, want the protected-path refusal", link, inside, err)
		}
	}
	lock := filepath.Join(root, ".lock")
	if err := os.WriteFile(lock, []byte("storage:\n  root: ./data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(lock); !errors.Is(err, storage.ErrProtected) {
		t.Errorf("%s: %v, want the protected-path refusal", lock, err)
	}
}

func TestASuppliedCredentialIsCheckedAndBounded(t *testing.T) {
	root := t.TempDir()
	token := filepath.Join(root, "prod.token")
	if err := os.WriteFile(token, []byte("asz_secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := ReadSupplied(token, 64)
	if err != nil || string(b) != "asz_secret\n" {
		t.Fatalf("read: %q, %v", b, err)
	}
	if _, err := ReadSupplied(token, 4); err == nil {
		t.Fatal("a file over its limit was read")
	}
	if err := os.MkdirAll(filepath.Join(root, "_pull"), 0o755); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(root, "_pull", "tokens")
	if err := os.WriteFile(inside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSupplied(inside, 64); !errors.Is(err, storage.ErrProtected) {
		t.Fatalf("%s: %v, want the protected-path refusal", inside, err)
	}
}

// A time to live written as an empty string is refused, as any other value
// that is not a duration is. One left out, or a key with no value at all,
// which YAML reads as null as it does for every other key, takes its default.
func TestAnEmptyTimeToLiveIsRefused(t *testing.T) {
	for _, body := range []string{"remote_snapshot_ttl: \"\"\n", "remote_server:\n  snapshot_ttl: \"\"\n"} {
		if _, err := loadText(t, body); err == nil {
			t.Errorf("%q was accepted", body)
		}
	}
}

// A path the URL carries is kept as written, escapes included, without its
// trailing slash.
func TestABaseURLKeepsAnEscapedPath(t *testing.T) {
	for raw, want := range map[string]string{
		"https://host/team%2Fprod/":    "https://host/team%2Fprod",
		"https://host/team%2Fprod%2F":  "https://host/team%2Fprod%2F",
		"https://host/team%2Fprod%2F/": "https://host/team%2Fprod%2F",
		"https://host/a%2F/b/":         "https://host/a%2F/b",
		"https://host/team/prod/":      "https://host/team/prod",
		"https://host/a%20b":           "https://host/a%20b",
		"https://host":                 "https://host",
	} {
		u, err := Remote{Name: "prod", URL: raw}.BaseURL()
		if err != nil || u.String() != want {
			t.Errorf("%s: %v, %v; want %s", raw, u, err, want)
		}
	}
}

// Credential paths are resolved when the configuration loads, from the
// working directory it was loaded in, and read later from there.
func TestCredentialPathsAreResolvedAtLoad(t *testing.T) {
	first := t.TempDir()
	t.Chdir(first)
	c, err := loadText(t, "remotes:\n  - name: prod\n    url: https://h\n    token_file: certs/token\n    ca_file: ~/ca.pem\n"+
		"remote_server:\n  tokens_file: tokens\n")
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	r, _ := c.Remote("prod")
	if r.TokenFile != filepath.Join(first, "certs", "token") || r.CAFile != filepath.Join(home, "ca.pem") ||
		c.RemoteServer.TokensFile != filepath.Join(first, "tokens") {
		t.Fatalf("paths %q %q %q", r.TokenFile, r.CAFile, c.RemoteServer.TokensFile)
	}
	t.Chdir(t.TempDir())
	if r.TokenFile != filepath.Join(first, "certs", "token") {
		t.Fatal("a resolved path moved with the working directory")
	}
}
