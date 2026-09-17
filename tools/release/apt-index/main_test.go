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
	"os"
	"path/filepath"
	"strings"
	"testing"

	debfixture "github.com/apache/skywalking-ai-sessionizer/tools/test/deb-fixture"
)

const (
	current = "https://mirrors/{version}/{file}"
	archive = "https://archive/{version}/{file}"
)

func deb(t *testing.T, dir, pkg, version, arch, body string) string {
	t.Helper()
	control := "Package: " + pkg + "\nVersion: " + version + "\nArchitecture: " + arch + "\nDescription: fixture\n more text\n"
	data, err := debfixture.Bytes(control, map[string]string{"usr/bin/" + pkg: body})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, distName(pkg, version, arch))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func read(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// Each version is added in its own run. The index keeps every version, the
// newest goes to the mirrors and every older one to the archive, and a run
// that adds nothing new leaves Release and its signatures alone.
func TestVersionsAreAddedOneRunAtATime(t *testing.T) {
	apt, debs := t.TempDir(), t.TempDir()
	older := deb(t, debs, "asz", "0.4.0", "amd64", "old")
	if err := run(apt, current, archive, "2026-09-17T00:00:00Z", []string{older}); err != nil {
		t.Fatal(err)
	}
	newer := deb(t, debs, "asz", "0.10.0", "amd64", "new")
	if err := run(apt, current, archive, "2026-09-18T00:00:00Z", []string{newer}); err != nil {
		t.Fatal(err)
	}
	packages := read(t, filepath.Join(apt, "dists/stable/main/binary-amd64/Packages"))
	for _, want := range []string{"Filename: pool/asz_0.4.0_amd64.deb", "Filename: pool/asz_0.10.0_amd64.deb", "Description: fixture\n more text\n"} {
		if !strings.Contains(packages, want) {
			t.Fatalf("Packages has no %q:\n%s", want, packages)
		}
	}
	redirects := read(t, filepath.Join(apt, ".htaccess"))
	for _, want := range []string{
		"RewriteRule ^pool/asz_0\\.10\\.0_amd64\\.deb$ https://mirrors/0.10.0/apache-skywalking-ai-sessionizer-0.10.0-bin-asz-amd64.deb [R=302,L]\n",
		"RewriteRule ^pool/asz_0\\.4\\.0_amd64\\.deb$ https://archive/0.4.0/apache-skywalking-ai-sessionizer-0.4.0-bin-asz-amd64.deb [R=302,L]\n",
	} {
		if !strings.Contains(redirects, want) {
			t.Fatalf(".htaccess has no %q:\n%s", want, redirects)
		}
	}
	release := filepath.Join(apt, "dists/stable/Release")
	if !strings.Contains(read(t, release), "Date: Fri, 18 Sep 2026 00:00:00 UTC\n") {
		t.Fatalf("Release does not carry the date of the run that changed the index:\n%s", read(t, release))
	}
	for _, signature := range []string{"InRelease", "Release.gpg"} {
		if err := os.WriteFile(filepath.Join(apt, "dists/stable", signature), []byte("signed"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	before := read(t, release)
	if err := run(apt, current, archive, "2026-09-19T00:00:00Z", []string{older, newer}); err != nil {
		t.Fatal(err)
	}
	if read(t, release) != before {
		t.Fatal("a run that added nothing wrote Release again")
	}
	if _, err := os.Stat(filepath.Join(apt, "dists/stable/InRelease")); err != nil {
		t.Fatal("a run that added nothing removed the signature")
	}
	third := deb(t, debs, "asz-claude-code", "0.10.0", "arm64", "plugin")
	if err := run(apt, current, archive, "2026-09-19T00:00:00Z", []string{third}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(apt, "dists/stable/InRelease")); !os.IsNotExist(err) {
		t.Fatal("a changed index kept the signature of the old Release")
	}
}

// A published package never changes. apt would refuse the file the
// redirect sends it to, so the index refuses other bytes under the same name.
func TestOtherBytesUnderAPublishedVersionAreRefused(t *testing.T) {
	apt := t.TempDir()
	if err := run(apt, current, archive, "", []string{deb(t, t.TempDir(), "asz", "0.4.0", "amd64", "voted")}); err != nil {
		t.Fatal(err)
	}
	err := run(apt, current, archive, "", []string{deb(t, t.TempDir(), "asz", "0.4.0", "amd64", "rebuilt")})
	if err == nil || !strings.Contains(err.Error(), "never changes") {
		t.Fatalf("other bytes were accepted: %v", err)
	}
}

func TestAPackageMustBeAReleasedOneByItsName(t *testing.T) {
	debs := t.TempDir()
	renamed := filepath.Join(debs, "asz_0.4.0_amd64.deb")
	if err := os.Rename(deb(t, debs, "asz", "0.4.0", "amd64", "x"), renamed); err != nil {
		t.Fatal(err)
	}
	if err := run(t.TempDir(), current, archive, "", []string{renamed}); err == nil || !strings.Contains(err.Error(), "the release names this package") {
		t.Fatalf("a package under another name was accepted: %v", err)
	}
	build := deb(t, debs, "asz", "0~e55c406", "amd64", "x")
	if err := run(t.TempDir(), current, archive, "", []string{build}); err == nil || !strings.Contains(err.Error(), "not a release version") {
		t.Fatalf("a build of a commit was accepted: %v", err)
	}
}
