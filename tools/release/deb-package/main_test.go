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
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func staged(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"asz": "binary", "asz-changes": "recorder", "LICENSE": "license", "NOTICE": "notice", "licenses/license-a.txt": "a"} {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A rebuild is held against the voted package, so the same files, version
// and time must give the same bytes.
func TestTheSameFilesGiveTheSameBytes(t *testing.T) {
	from := staged(t)
	out := t.TempDir()
	var packages [][]byte
	for _, name := range []string{"one.deb", "two.deb"} {
		if err := run("asz-changes", "0.5.0", "arm64", from, 1757000000, filepath.Join(out, name)); err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		packages = append(packages, body)
	}
	if !bytes.Equal(packages[0], packages[1]) {
		t.Fatal("two builds of the same files differ")
	}
	if !bytes.HasPrefix(packages[0], []byte("!<arch>\ndebian-binary   ")) {
		t.Fatalf("not an ar archive that starts with debian-binary: %q", packages[0][:40])
	}
}

// The checks read a package with -list and -control, so what they print must
// be what was written.
func TestAPackageReadsBack(t *testing.T) {
	deb := filepath.Join(t.TempDir(), "asz.deb")
	if err := run("asz", "0.4.0", "amd64", staged(t), 1757000000, deb); err != nil {
		t.Fatal(err)
	}
	listing := capture(t, func() error { return printMember(deb, "data") })
	for _, want := range []string{"usr/\n", "usr/bin/asz\n", "usr/share/doc/asz/LICENSE\n", "usr/share/doc/asz/NOTICE\n", "usr/share/doc/asz/licenses/license-a.txt\n"} {
		if !strings.Contains(listing, want) {
			t.Fatalf("-list has no %q:\n%s", want, listing)
		}
	}
	control := capture(t, func() error { return printMember(deb, "control") })
	for _, want := range []string{"Package: asz\n", "Version: 0.4.0\n", "Architecture: amd64\n"} {
		if !strings.Contains(control, want) {
			t.Fatalf("-control has no %q:\n%s", want, control)
		}
	}
	if err := printMember(filepath.Join(staged(t), "LICENSE"), "data"); err == nil {
		t.Fatal("a file that is not a Debian package was read as one")
	}
}

// capture returns what fn prints to standard output.
func capture(t *testing.T, fn func() error) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	done := make(chan []byte)
	go func() {
		body, _ := io.ReadAll(r)
		done <- body
	}()
	runErr := fn()
	os.Stdout = stdout
	_ = w.Close()
	body := <-done
	if runErr != nil {
		t.Fatal(runErr)
	}
	return string(body)
}

// make binaries removes macOS metadata before it packages. One that is
// still in the staged files stops the build rather than being packaged.
func TestMacOSMetadataIsRefused(t *testing.T) {
	for _, name := range []string{".DS_Store", "._license-a.txt", "__MACOSX"} {
		t.Run(name, func(t *testing.T) {
			from := staged(t)
			if err := os.WriteFile(filepath.Join(from, "licenses", name), []byte("metadata"), 0o644); err != nil {
				t.Fatal(err)
			}
			err := run("asz", "0.4.0", "amd64", from, 1757000000, filepath.Join(t.TempDir(), "asz.deb"))
			if err == nil || !strings.Contains(err.Error(), "macOS metadata") {
				t.Fatalf("metadata was not refused: %v", err)
			}
		})
	}
}

func TestDebianVersion(t *testing.T) {
	for version, want := range map[string]string{
		"0.4.0":                  "0.4.0",
		"0.3.0-12-gabcdef-dirty": "0.3.0-12-gabcdef-dirty",
		"3321751abc":             "3321751abc",
		"e55c406abc":             "0~e55c406abc",
		"dev":                    "0~dev",
	} {
		if got := DebianVersion(version); got != want {
			t.Errorf("DebianVersion(%q) = %q, want %q", version, got, want)
		}
	}
}
