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

package packagecheck_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/tools/debfixture"
)

func TestSourceArchiveOmitsMetadata(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("source archive checks need git")
	}
	dir := t.TempDir()
	attributes, err := os.ReadFile(filepath.Join("..", "..", ".gitattributes"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), attributes, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"keep.txt", "._keep.txt", ".DS_Store", "sub/._keep.txt", "sub/.DS_Store", "__MACOSX/keep.txt", "sub/__MACOSX/keep.txt"} {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) []byte {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		output, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return output
	}
	run("init", "-q")
	run("add", "-f", ".")
	tree := strings.TrimSpace(string(run("write-tree")))
	reader := tar.NewReader(bytes.NewReader(run("archive", "--format=tar", tree)))
	kept := false
	for {
		entry, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if entry.Name == "keep.txt" {
			kept = true
		}
		for _, part := range strings.Split(entry.Name, "/") {
			if strings.HasPrefix(part, "._") || part == ".DS_Store" || part == "__MACOSX" {
				t.Errorf("source archive contains %s", entry.Name)
			}
		}
	}
	if !kept {
		t.Fatal("source archive lost the project file")
	}
}

func TestArchiveMetadataIsRejected(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("archive checks need the release shell")
	}
	for _, ext := range []string{"tgz", "zip", "deb"} {
		for _, name := range []string{"LICENSE", "._asz", "licenses/._LICENSE", ".DS_Store", "__MACOSX/asz"} {
			t.Run(ext+"/"+name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "package."+ext)
				f, err := os.Create(path)
				if err != nil {
					t.Fatal(err)
				}
				switch ext {
				case "deb":
					body, err := debfixture.Bytes("Package: asz\n", map[string]string{"usr/share/doc/asz/" + name: "x"})
					if err != nil {
						t.Fatal(err)
					}
					if _, err := f.Write(body); err != nil {
						t.Fatal(err)
					}
				case "zip":
					w := zip.NewWriter(f)
					entry, err := w.Create(name)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := entry.Write([]byte("x")); err != nil {
						t.Fatal(err)
					}
					if err := w.Close(); err != nil {
						t.Fatal(err)
					}
				default:
					gz := gzip.NewWriter(f)
					w := tar.NewWriter(gz)
					if err := w.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 1}); err != nil {
						t.Fatal(err)
					}
					if _, err := w.Write([]byte("x")); err != nil {
						t.Fatal(err)
					}
					if err := w.Close(); err != nil {
						t.Fatal(err)
					}
					if err := gz.Close(); err != nil {
						t.Fatal(err)
					}
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(shell, filepath.Join("..", "package-check.sh"), filepath.ToSlash(path))
				output, err := cmd.CombinedOutput()
				if name == "LICENSE" {
					if err != nil {
						t.Fatalf("clean archive rejected: %v, %s", err, output)
					}
				} else if err == nil || !strings.Contains(string(output), "contains macOS metadata") {
					t.Fatalf("metadata archive not rejected correctly: %v, %s", err, output)
				}
			})
		}
	}
}
