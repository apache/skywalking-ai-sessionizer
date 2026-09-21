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

package cibinaries_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	debfixture "github.com/apache/skywalking-ai-sessionizer/tools/test/deb-fixture"
)

const (
	version    = "0.3.0"
	commit     = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	platforms  = "darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64 windows/arm64"
	repository = "apache/skywalking-ai-sessionizer"
)

type member struct {
	name string
	data []byte
}

type fixture struct {
	t            *testing.T
	dir          string
	run          map[string]any
	workflow     map[string]any
	ref          map[string]any
	release      map[string]any
	finalRelease map[string]any
	members      []member
	// signed are files candidate attached beside CI's, uploaded by a person.
	signed         []member
	assetOverrides map[string]any
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("release download tests use a POSIX gh fixture")
	}
	for _, tool := range []string{"bash", "python3", "tar", "unzip"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("release download tests need %s", tool)
		}
	}
	f := &fixture{t: t, dir: t.TempDir()}
	repo := map[string]any{"full_name": repository, "id": 100}
	f.run = map[string]any{"id": 123, "repository": repo, "head_repository": repo,
		"head_branch": "v" + version, "head_sha": commit, "event": "push",
		"run_started_at": "2026-09-14T10:00:00Z", "updated_at": "2026-09-14T10:30:00Z",
		"status": "completed", "conclusion": "success", "workflow_id": 101, "run_attempt": 1,
		"path": ".github/workflows/ci.yaml@refs/tags/v" + version}
	f.workflow = map[string]any{"id": 101, "name": "CI", "path": ".github/workflows/ci.yaml"}
	f.ref = map[string]any{"object": map[string]any{"type": "commit", "sha": commit}}
	f.release = map[string]any{"id": 456, "tag_name": "v" + version, "name": version,
		"draft": false, "prerelease": true,
		"body": "Developer testing only. <!-- asz-ci-binaries run_id=123 run_attempt=1 commit=" + commit + " assets=ASSETS -->"}
	for _, platform := range strings.Fields(platforms) {
		ext := "tgz"
		if strings.HasPrefix(platform, "windows/") {
			ext = "zip"
		}
		name := "apache-skywalking-ai-sessionizer-" + version + "-bin-" + strings.ReplaceAll(platform, "/", "-") + "." + ext
		data := packageBytes(t, ext, "LICENSE")
		f.members = append(f.members, member{name: name, data: data}, member{name: name + ".sha512", data: []byte(fmt.Sprintf("%x  %s\n", sha512.Sum512(data), name))})
	}
	bin := filepath.Join(f.dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// The fixture refuses any non-GET operation or unexpected endpoint.
	// Tests never contact GitHub or invoke an installed gh executable.
	stub := `#!/usr/bin/env python3
import json, os, pathlib, sys
root = pathlib.Path(os.environ["CI_BINARY_FIXTURE"])
args = sys.argv[1:]
assert args[:3] == ["api", "--method", "GET"], args
endpoint = args[-1]
prefix = "repos/apache/skywalking-ai-sessionizer/"
assert endpoint.startswith(prefix), endpoint
endpoint = endpoint[len(prefix):]
routes = {
    "git/ref/tags/v0.3.0": "ref.json",
    "git/tags/" + "b" * 40: "tag.json",
    "actions/runs/123": "run.json",
    "actions/workflows/ci.yaml": "workflow.json",
    "releases/tags/v0.3.0": "release.json",
    "releases/456/assets?per_page=100": "assets.json",
}
if endpoint.startswith("releases/assets/"):
    assert "Accept: application/octet-stream" in args
    name = "asset-" + endpoint.rsplit("/", 1)[1]
else:
    name = routes[endpoint]
if (root / "downloaded").exists() and (root / ("final-" + name)).exists():
    name = "final-" + name
sys.stdout.buffer.write((root / name).read_bytes())
if name.startswith("asset-"):
    (root / "downloaded").write_text("yes")
`
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func packageBytes(t *testing.T, ext, name string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if ext == "zip" {
		w := zip.NewWriter(&buffer)
		entry, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte("fixture\n")); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		gz := gzip.NewWriter(&buffer)
		w := tar.NewWriter(gz)
		if err := w.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 8}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("fixture\n")); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return buffer.Bytes()
}

func (f *fixture) json(name string, value any) {
	f.t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, name), data, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) write() {
	f.t.Helper()
	assets := []map[string]any{}
	var lines []string
	for i, m := range f.members {
		id := 1000 + i
		if err := os.WriteFile(filepath.Join(f.dir, fmt.Sprintf("asset-%d", id)), m.data, 0o600); err != nil {
			f.t.Fatal(err)
		}
		asset := map[string]any{"id": id, "name": m.name, "state": "uploaded", "size": len(m.data),
			"digest": fmt.Sprintf("sha256:%x", sha256.Sum256(m.data)), "created_at": "2026-09-14T10:20:00Z",
			"uploader": map[string]any{"login": "github-actions[bot]", "type": "Bot"}}
		if i == 0 {
			for k, v := range f.assetOverrides {
				asset[k] = v
			}
		}
		assets = append(assets, asset)
		lines = append(lines, fmt.Sprintf("%s %d %d %s", m.name, id, len(m.data), asset["digest"]))
	}
	for i, m := range f.signed {
		assets = append(assets, map[string]any{"id": 2000 + i, "name": m.name, "state": "uploaded", "size": len(m.data),
			"digest": fmt.Sprintf("sha256:%x", sha256.Sum256(m.data)), "created_at": "2026-09-15T08:00:00Z",
			"uploader": map[string]any{"login": "release-manager", "type": "User"}})
	}
	// The marker names the asset set the way ci-upload-binaries.sh records it.
	sort.Strings(lines)
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(lines, "\n")+"\n")))
	withAssets := func(source map[string]any) map[string]any {
		out := make(map[string]any)
		for k, v := range source {
			out[k] = v
		}
		if body, ok := out["body"].(string); ok {
			out["body"] = strings.ReplaceAll(body, "ASSETS", fingerprint)
		}
		return out
	}
	if f.finalRelease != nil {
		f.json("final-release.json", withAssets(f.finalRelease))
	}
	f.json("run.json", f.run)
	f.json("workflow.json", f.workflow)
	f.json("ref.json", f.ref)
	f.json("release.json", withAssets(f.release))
	f.json("assets.json", []any{assets})
}

func (f *fixture) execute(selected ...string) (string, error) {
	f.t.Helper()
	return f.executeWith("", selected...)
}

// addDebs adds the Debian packages CI builds for DEB_PACKAGES to the release.
func (f *fixture) addDebs(debs string) {
	f.t.Helper()
	for _, p := range strings.Fields(debs) {
		for _, arch := range []string{"amd64", "arm64"} {
			name := "apache-skywalking-ai-sessionizer-" + version + "-bin-" + p + "-" + arch + ".deb"
			data, err := debfixture.Bytes("Package: "+p+"\n", map[string]string{"usr/bin/" + p: "fixture\n"})
			if err != nil {
				f.t.Fatal(err)
			}
			f.members = append(f.members, member{name: name, data: data}, member{name: name + ".sha512", data: []byte(fmt.Sprintf("%x  %s\n", sha512.Sum512(data), name))})
		}
	}
}

func (f *fixture) executeWith(debs string, selected ...string) (string, error) {
	f.t.Helper()
	f.write()
	script, err := filepath.Abs(filepath.Join("..", "..", "release", "ci-binaries.sh"))
	if err != nil {
		f.t.Fatal(err)
	}
	runID := "123"
	if len(selected) > 0 {
		runID = selected[0]
	}
	args := []string{script, version, commit, runID, filepath.Join(f.dir, "output"), platforms}
	if debs != "" {
		args = append(args, debs)
	}
	command := exec.Command("bash", args...)
	command.Env = append(os.Environ(), "PATH="+filepath.Join(f.dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"), "CI_BINARY_FIXTURE="+f.dir)
	output, err := command.CombinedOutput()
	return string(output), err
}

func TestCIBinariesUsePrereleaseBuildRun(t *testing.T) {
	for _, selection := range []string{"", "auto", "123"} {
		t.Run(selection, func(t *testing.T) {
			f := newFixture(t)
			output, err := f.execute(selection)
			if err != nil || !strings.Contains(output, "verified 6 packages") {
				t.Fatalf("prerelease download failed: %v\n%s", err, output)
			}
		})
	}
	f := newFixture(t)
	output, err := f.execute("999")
	if err == nil || !strings.Contains(output, "must name the CI run that uploaded") {
		t.Fatalf("unrelated CI run accepted: %v\n%s", err, output)
	}
}

// A tag whose Makefile names Debian packages has one of each for every Linux
// platform, and the prerelease must hold them too.
func TestCIBinariesExpectTheDebianPackages(t *testing.T) {
	f := newFixture(t)
	f.addDebs("asz asz-changes")
	output, err := f.executeWith("asz asz-changes")
	if err != nil || !strings.Contains(output, "verified 10 packages") {
		t.Fatalf("prerelease with Debian packages rejected: %v\n%s", err, output)
	}
	f = newFixture(t)
	output, err = f.executeWith("asz asz-changes")
	if err == nil || !strings.Contains(output, "exactly the expected") {
		t.Fatalf("prerelease without the Debian packages accepted: %v\n%s", err, output)
	}
	f = newFixture(t)
	f.addDebs("asz")
	output, err = f.executeWith("asz")
	if err != nil || !strings.Contains(output, "verified 8 packages") {
		t.Fatalf("prerelease with the asz Debian packages rejected: %v\n%s", err, output)
	}
}

// A candidate that stopped after attaching its signatures is run again. The
// prerelease then holds the source package and the signatures too, uploaded
// by the release manager, and only CI's files are downloaded and checked.
func TestCIBinariesAcceptTheCandidatesOwnAttachments(t *testing.T) {
	f := newFixture(t)
	source := "apache-skywalking-ai-sessionizer-" + version + "-src.tgz"
	f.signed = []member{{name: source, data: []byte("source")}, {name: source + ".sha512", data: []byte("sum")}, {name: source + ".asc", data: []byte("sig")}}
	for _, m := range f.members {
		if !strings.HasSuffix(m.name, ".sha512") {
			f.signed = append(f.signed, member{name: m.name + ".asc", data: []byte("sig")})
		}
	}
	output, err := f.execute()
	if err != nil || !strings.Contains(output, "verified 6 packages") {
		t.Fatalf("prerelease with the candidate's attachments rejected: %v\n%s", err, output)
	}
	entries, err := os.ReadDir(filepath.Join(f.dir, "output"))
	if err != nil || len(entries) != len(f.members) {
		t.Fatalf("installed %d files, want only CI's %d: %v", len(entries), len(f.members), err)
	}
}

func TestVerifiedCIBinariesAreInstalledUnchanged(t *testing.T) {
	for _, annotated := range []bool{false, true} {
		t.Run(fmt.Sprintf("annotated=%v", annotated), func(t *testing.T) {
			f := newFixture(t)
			if annotated {
				f.ref = map[string]any{"object": map[string]any{"type": "tag", "sha": strings.Repeat("b", 40)}}
				f.json("tag.json", map[string]any{"object": map[string]any{"type": "commit", "sha": commit}})
				if err := os.Mkdir(filepath.Join(f.dir, "output"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			output, err := f.execute()
			if err != nil || !strings.Contains(output, "verified 6 packages") {
				t.Fatalf("valid prerelease rejected: %v\n%s", err, output)
			}
			for _, m := range f.members {
				data, err := os.ReadFile(filepath.Join(f.dir, "output", m.name))
				if err != nil || !bytes.Equal(data, m.data) {
					t.Fatalf("verified file changed: %s: %v", m.name, err)
				}
			}
		})
	}
}

func TestUnverifiedCIBinariesLeaveNoPackages(t *testing.T) {
	cases := []struct {
		name, message string
		change        func(*fixture)
	}{
		{"wrong-tag", "does not point", func(f *fixture) { f.ref["object"].(map[string]any)["sha"] = strings.Repeat("c", 40) }},
		{"failed-run", "completed and successful", func(f *fixture) { f.run["conclusion"] = "failure" }},
		{"wrong-commit", "release tag at COMMIT", func(f *fixture) { f.run["head_sha"] = strings.Repeat("c", 40) }},
		{"main-run", "release tag at COMMIT", func(f *fixture) { f.run["head_branch"] = "main" }},
		{"pull-request", "not the build of the tag push", func(f *fixture) { f.run["event"] = "pull_request" }},
		{"manual-run", "not the build of the tag push", func(f *fixture) { f.run["event"] = "workflow_dispatch" }},
		{"release-event", "not the build of the tag push", func(f *fixture) { f.run["event"] = "release" }},
		{"run-without-times", "no start or update time", func(f *fixture) { delete(f.run, "run_started_at") }},
		{"asset-by-person", "not uploaded by CI", func(f *fixture) {
			f.assetOverrides = map[string]any{"uploader": map[string]any{"login": "someone", "type": "User"}}
		}},
		{"asset-before-run", "during the CI run", func(f *fixture) { f.assetOverrides = map[string]any{"created_at": "2026-09-14T09:59:59Z"} }},
		{"asset-after-run", "during the CI run", func(f *fixture) { f.assetOverrides = map[string]any{"created_at": "2026-09-14T10:30:01Z"} }},
		{"marker-other-assets", "not the files CI verified", func(f *fixture) {
			f.release["body"] = strings.ReplaceAll(f.release["body"].(string), "ASSETS", strings.Repeat("0", 64))
		}},
		{"asset-uploaded-again", "not the files CI verified", func(f *fixture) {
			f.release["body"] = strings.ReplaceAll(f.release["body"].(string), "ASSETS", fmt.Sprintf("%x", sha256.Sum256([]byte("an earlier upload\n"))))
		}},
		{"fork", "not a fork", func(f *fixture) { f.run["head_repository"] = map[string]any{"full_name": "person/fork", "id": 999} }},
		{"wrong-workflow", "not the repository's CI", func(f *fixture) { f.run["workflow_id"] = 999 }},
		{"release-missing-marker", "not ready", func(f *fixture) { f.release["body"] = "CI is still building" }},
		{"release-wrong-commit", "not ready", func(f *fixture) {
			f.release["body"] = strings.ReplaceAll(f.release["body"].(string), commit, strings.Repeat("c", 40))
		}},
		{"release-duplicate-marker", "not ready", func(f *fixture) { f.release["body"] = strings.Repeat(f.release["body"].(string), 2) }},
		{"release-draft", "developer prerelease", func(f *fixture) { f.release["draft"] = true }},
		{"release-official", "developer prerelease", func(f *fixture) { f.release["prerelease"] = false }},
		{"release-wrong-title", "developer prerelease", func(f *fixture) { f.release["name"] = "other" }},
		{"run-other-attempt", "run attempt differs", func(f *fixture) { f.run["run_attempt"] = 2 }},
		{"asset-incomplete", "not fully uploaded", func(f *fixture) { f.assetOverrides = map[string]any{"state": "starter"} }},
		{"digest-missing", "no SHA-256 digest", func(f *fixture) { f.assetOverrides = map[string]any{"digest": ""} }},
		{"digest-wrong", "GitHub's SHA-256", func(f *fixture) { f.assetOverrides = map[string]any{"digest": "sha256:" + strings.Repeat("0", 64)} }},
		{"missing-member", "exactly the expected", func(f *fixture) { f.members = f.members[1:] }},
		{"duplicate-member", "exactly the expected", func(f *fixture) { f.members = append(f.members, f.members[0]) }},
		{"traversal", "exactly the expected", func(f *fixture) { f.members[0].name = "../escaped" }},
		{"nul-suffix", "exactly the expected", func(f *fixture) { f.members[0].name += "\x00../escaped" }},
		{"extra-member", "exactly the expected", func(f *fixture) { f.members = append(f.members, member{name: "unexpected", data: []byte("x")}) }},
		{"extra-signed-name", "exactly the expected", func(f *fixture) { f.signed = []member{{name: "unexpected.asc", data: []byte("x")}} }},
		{"package-checksum", "SHA-512 mismatch", func(f *fixture) { f.members[0].data = []byte("changed package") }},
		{"checksum-name", "invalid checksum document", func(f *fixture) { f.members[1].data = []byte(strings.Repeat("0", 128) + "  ../other\n") }},
		{"package-metadata", "contains macOS metadata", func(f *fixture) {
			f.members[0].data = packageBytes(f.t, "tgz", "._asz")
			f.members[1].data = []byte(fmt.Sprintf("%x  %s\n", sha512.Sum512(f.members[0].data), f.members[0].name))
		}},
		{"tag-moved", "tag moved", func(f *fixture) {
			f.json("final-ref.json", map[string]any{"object": map[string]any{"type": "commit", "sha": strings.Repeat("c", 40)}})
		}},
		{"run-restarted", "run changed", func(f *fixture) { f.json("final-run.json", map[string]any{"status": "in_progress"}) }},
		{"release-recreated", "prerelease changed", func(f *fixture) {
			other := make(map[string]any)
			for k, v := range f.release {
				other[k] = v
			}
			other["id"] = 789
			f.finalRelease = other
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.change(f)
			output, err := f.execute()
			if err == nil || !strings.Contains(output, tc.message) {
				t.Fatalf("invalid prerelease not refused correctly: %v\n%s", err, output)
			}
			if _, err := os.Stat(filepath.Join(f.dir, "output")); !os.IsNotExist(err) {
				t.Fatalf("failed validation installed output: %v", err)
			}
			if _, err := os.Stat(filepath.Join(f.dir, "escaped")); !os.IsNotExist(err) {
				t.Fatal("an unsafe archive member was extracted")
			}
		})
	}
}

func TestCIBinariesRefuseOccupiedOutput(t *testing.T) {
	f := newFixture(t)
	directory := filepath.Join(f.dir, "output")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(directory, "keep")
	if err := os.WriteFile(sentinel, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := f.execute()
	if err == nil || !strings.Contains(output, "new or empty") {
		t.Fatalf("occupied output accepted: %v\n%s", err, output)
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "original" {
		t.Fatal("occupied output changed")
	}
}
