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

package releasecomplete_test

import (
	"bytes"
	"crypto/sha512"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
)

const version = "0.3.0"

type releaseState struct {
	Exists     bool   `json:"exists"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Interrupt  bool   `json:"interrupt"`
	Corrupt    string `json:"corrupt"`
	Omit       string `json:"omit"`
	Replace    bool   `json:"replace"`
}

type event struct {
	Action     string   `json:"action"`
	Draft      bool     `json:"draft"`
	Prerelease bool     `json:"prerelease"`
	Assets     []string `json:"assets"`
	Files      []string `json:"files"`
}

type fixture struct {
	t      *testing.T
	dir    string
	script string
	names  []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("release completion tests use POSIX command fixtures")
	}
	for _, tool := range []string{"bash", "python3", "shasum", "cmp"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("release completion tests need %s", tool)
		}
	}
	script, err := filepath.Abs("../release.sh")
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, dir: t.TempDir(), script: script}
	for _, dir := range []string{"bin", "remote", "served", "voted", "dist/" + version} {
		if err := os.MkdirAll(filepath.Join(f.dir, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, kind := range []string{"src.tgz", "bin-linux-amd64.tgz"} {
		name := "apache-skywalking-ai-sessionizer-" + version + "-" + kind
		data := []byte("voted package: " + name + "\n")
		files := map[string][]byte{
			name:             data,
			name + ".asc":    []byte("voted signature: " + name + "\n"),
			name + ".sha512": []byte(fmt.Sprintf("%x  %s\n", sha512.Sum512(data), name)),
		}
		for file, content := range files {
			f.write("dist/"+version+"/"+file, content)
			f.write("voted/"+file, content)
			f.write("served/"+file, content)
			if strings.HasPrefix(kind, "bin-") && !strings.HasSuffix(file, ".asc") {
				f.write("remote/"+file, content)
			}
			f.names = append(f.names, file)
		}
	}
	sort.Strings(f.names)
	for _, tool := range []string{"git", "gh", "curl", "svn", "gpg", "gpgconf"} {
		path := filepath.Join(f.dir, "bin", tool)
		if err := os.WriteFile(path, []byte(fakeTools), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.setState(releaseState{Exists: true, Prerelease: true})
	return f
}

func (f *fixture) write(path string, data []byte) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, path), data, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) read(path string) []byte {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, path))
	if err != nil {
		f.t.Fatal(err)
	}
	return data
}

func (f *fixture) setState(state releaseState) {
	f.t.Helper()
	data, err := json.Marshal(state)
	if err != nil {
		f.t.Fatal(err)
	}
	f.write("state.json", data)
}

func (f *fixture) state() releaseState {
	f.t.Helper()
	var state releaseState
	if err := json.Unmarshal(f.read("state.json"), &state); err != nil {
		f.t.Fatal(err)
	}
	return state
}

func (f *fixture) events() []event {
	f.t.Helper()
	var events []event
	for _, line := range bytes.Split(bytes.TrimSpace(f.read("events.jsonl")), []byte("\n")) {
		var e event
		if err := json.Unmarshal(line, &e); err != nil {
			f.t.Fatal(err)
		}
		events = append(events, e)
	}
	return events
}

func (f *fixture) run() (string, error) {
	f.t.Helper()
	cmd := exec.Command("bash", f.script, "complete", version)
	cmd.Dir = f.dir
	cmd.Env = append(os.Environ(), "PATH="+filepath.Join(f.dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"), "RELEASE_COMPLETE_FIXTURE="+f.dir)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func (f *fixture) requireFailure(want string) {
	f.t.Helper()
	output, err := f.run()
	if err == nil || !strings.Contains(output, want) {
		f.t.Fatalf("complete error = %v, want %q in output:\n%s", err, want, output)
	}
	if state := f.state(); !state.Draft && !state.Prerelease {
		f.t.Fatal("failed completion made the release official")
	}
	for _, e := range f.events() {
		if e.Action == "publish" {
			f.t.Fatal("failed completion triggered publication")
		}
	}
}

func (f *fixture) requirePublished() {
	f.t.Helper()
	if state := f.state(); !state.Exists || state.Draft || state.Prerelease {
		f.t.Fatalf("release state = %+v, want an existing official release", state)
	}
	events := f.events()
	publications, lastFullDownload := 0, -1
	for i, e := range events {
		if e.Action == "download_all" {
			lastFullDownload = i
		}
		if e.Action == "publish" {
			publications++
			if (!e.Draft && !e.Prerelease) || !reflect.DeepEqual(e.Assets, f.names) || lastFullDownload < 0 || i != len(events)-1 {
				f.t.Fatalf("publication happened before the complete asset set was downloaded for verification: %+v", events)
			}
		}
	}
	if publications != 1 {
		f.t.Fatalf("publication count = %d, want one", publications)
	}
	for _, name := range f.names {
		if !bytes.Equal(f.read("remote/"+name), f.read("voted/"+name)) {
			f.t.Fatalf("public asset %s differs from the voted file", name)
		}
	}
}

func TestCompletePromotesOnlyAfterUploadingAndVerifyingEveryAsset(t *testing.T) {
	f := newFixture(t)
	if output, err := f.run(); err != nil {
		t.Fatalf("complete: %v\n%s", err, output)
	}
	f.requirePublished()
	for _, e := range f.events() {
		if e.Action == "create" {
			t.Fatal("completion created a release instead of promoting the existing prerelease")
		}
		if e.Action == "upload" {
			if len(e.Files) != 4 {
				t.Fatalf("upload = %v, want source with sidecars and binary signature", e.Files)
			}
			for _, file := range e.Files {
				if strings.Contains(file, "-bin-") && !strings.HasSuffix(file, ".asc") {
					t.Fatalf("completion tried to replace CI's existing asset %s", file)
				}
			}
		}
	}
}

func TestCompleteCanPromoteMatchingRecoveryDraft(t *testing.T) {
	f := newFixture(t)
	f.setState(releaseState{Exists: true, Draft: true})
	if output, err := f.run(); err != nil {
		t.Fatalf("complete recovery draft: %v\n%s", err, output)
	}
	f.requirePublished()
}

func TestCompletePromotesAlreadySignedPrereleaseWithoutUploading(t *testing.T) {
	f := newFixture(t)
	for _, name := range f.names {
		f.write("remote/"+name, f.read("voted/"+name))
	}
	if output, err := f.run(); err != nil {
		t.Fatalf("complete signed prerelease: %v\n%s", err, output)
	}
	f.requirePublished()
	for _, e := range f.events() {
		if e.Action == "upload" {
			t.Fatalf("complete uploaded already signed files: %+v", e)
		}
	}
}

func TestCompleteRefusesMissingCIBinaryAssets(t *testing.T) {
	for _, sidecar := range []string{"", ".sha512"} {
		t.Run("archive"+sidecar, func(t *testing.T) {
			f := newFixture(t)
			name := "apache-skywalking-ai-sessionizer-" + version + "-bin-linux-amd64.tgz" + sidecar
			if err := os.Remove(filepath.Join(f.dir, "remote", name)); err != nil {
				t.Fatal(err)
			}
			f.requireFailure("missing CI's binary asset " + name)
			for _, e := range f.events() {
				if e.Action == "upload" {
					t.Fatalf("complete uploaded after finding a missing CI binary asset: %+v", e)
				}
			}
		})
	}
}

func TestCompleteRefusesReleaseReplacedDuringVerification(t *testing.T) {
	f := newFixture(t)
	f.setState(releaseState{Exists: true, Prerelease: true, Replace: true})
	f.requireFailure("the GitHub release changed while complete was verifying it")
}

func TestCompleteRefusesConflictingLocalCopyWithoutReplacingIt(t *testing.T) {
	f := newFixture(t)
	name := "dist/" + version + "/" + f.names[0]
	f.write(name, []byte("stale local file"))
	f.requireFailure("differs from the Apache release; move the conflicting local file aside")
	if got := string(f.read(name)); got != "stale local file" {
		t.Fatalf("local file was replaced: %q", got)
	}
}

func TestCompleteUsesVotedFilesWithoutLocalCandidateOrCI(t *testing.T) {
	f := newFixture(t)
	if err := os.RemoveAll(filepath.Join(f.dir, "dist")); err != nil {
		t.Fatal(err)
	}
	// The command fixtures reject every CI API call. Artifacts may have
	// expired by this stage; only the published voted files remain required.
	if output, err := f.run(); err != nil {
		t.Fatalf("complete with absent local candidate and unavailable CI: %v\n%s", err, output)
	}
	f.requirePublished()
	if _, err := os.Stat(filepath.Join(f.dir, "dist")); !os.IsNotExist(err) {
		t.Fatalf("completion should leave local dist absent; stat error = %v", err)
	}
}

func TestCompleteResumesInterruptedPrereleaseWithoutReplacingExistingAssets(t *testing.T) {
	f := newFixture(t)
	f.setState(releaseState{Exists: true, Prerelease: true, Interrupt: true})
	f.requireFailure("the upload stopped; the release was not promoted")
	entries, err := os.ReadDir(filepath.Join(f.dir, "remote"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("interrupted upload left %d assets, want four", len(entries))
	}
	if output, err := f.run(); err != nil {
		t.Fatalf("resume complete: %v\n%s", err, output)
	}
	f.requirePublished()
	created, uploads := 0, 0
	verifiedExisting := make(map[string]bool)
	for _, e := range f.events() {
		switch e.Action {
		case "create":
			created++
		case "download_one":
			verifiedExisting[e.Files[0]] = true
		case "upload":
			uploads++
			if uploads == 2 {
				if len(e.Files) != len(f.names)-4 {
					t.Fatalf("resumed upload = %v, want only two missing assets", e.Files)
				}
				for _, entry := range entries {
					if !verifiedExisting[entry.Name()] {
						t.Fatalf("existing asset %s was not verified before uploading", entry.Name())
					}
					for _, file := range e.Files {
						if file == entry.Name() {
							t.Fatalf("resumed upload tried to replace existing asset %s", file)
						}
					}
				}
			}
		}
	}
	if created != 0 || uploads != 2 {
		t.Fatalf("create/upload calls = %d/%d, want 0/2", created, uploads)
	}
}

func TestCompleteRejectsConflictingDraftAssets(t *testing.T) {
	for _, tc := range []struct {
		name, diagnostic string
		extra            bool
	}{
		{name: "different bytes", diagnostic: "differs from the voted file"},
		{name: "unexpected name", diagnostic: "unexpected asset: unapproved.txt", extra: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.setState(releaseState{Exists: true, Draft: true})
			name := f.names[0]
			if tc.extra {
				name = "unapproved.txt"
			}
			f.write("remote/"+name, []byte("unapproved bytes"))
			f.requireFailure(tc.diagnostic)
			for _, e := range f.events() {
				if e.Action == "upload" || e.Action == "create" {
					t.Fatalf("conflicting draft was changed: %+v", e)
				}
			}
			if got := string(f.read("remote/" + name)); got != "unapproved bytes" {
				t.Fatalf("conflicting asset was replaced: %q", got)
			}
		})
	}
}

func TestCompleteRejectsIncompleteOrCorruptedUpload(t *testing.T) {
	for _, tc := range []struct {
		name, diagnostic string
		corrupt          bool
	}{
		{name: "missing asset", diagnostic: "does not hold exactly the voted asset set"},
		{name: "corrupted bytes", diagnostic: "differs from the voted file", corrupt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			state := releaseState{Exists: true, Prerelease: true, Omit: f.names[1]}
			if tc.corrupt {
				state = releaseState{Exists: true, Prerelease: true, Corrupt: f.names[1]}
			}
			f.setState(state)
			f.requireFailure(tc.diagnostic)
		})
	}
}

// These fixtures accept only the release stage's expected commands. No command
// delegates to a real git remote, GitHub CLI, or HTTP client.
const fakeTools = `#!/usr/bin/env python3
import json
import os
from pathlib import Path
import shutil
import sys

root = Path(os.environ["RELEASE_COMPLETE_FIXTURE"])
args = sys.argv[1:]
tool = Path(sys.argv[0]).name
sha = "a" * 40
tag = "v0.3.0"
if tool == "git":
    if args == ["rev-parse", "--show-toplevel"]:
        print(root)
    elif args == ["ls-remote", "--tags", "origin", "refs/tags/" + tag]:
        print(sha + "\trefs/tags/" + tag)
    elif args in [["rev-parse", "-q", "--verify", "refs/tags/" + tag], ["rev-parse", tag + "^{commit}"]]:
        print(sha)
    elif args == ["show", tag + ":Makefile"]:
        print("PLATFORMS := linux/amd64")
    elif args == ["show", tag + ":docs/en/changes/changes.md"]:
        print("# Changes in 0.3.0\n\nFixture release.")
    else:
        raise AssertionError("unexpected git command: " + repr(args))
    sys.exit(0)
if tool == "curl":
    if args == ["-fsSL", "https://dist.apache.org/repos/dist/release/skywalking/KEYS", "-o", args[-1]] and len(args) == 4:
        Path(args[-1]).write_text("fixture release key")
        sys.exit(0)
    assert len(args) == 2 and args[0] == "-fsSL", args
    prefix = "https://downloads.apache.org/skywalking/ai-sessionizer/0.3.0/"
    assert args[1].startswith(prefix), args
    name = args[1][len(prefix):]
    assert "/" not in name and name.endswith((".asc", ".sha512")), name
    sys.stdout.buffer.write((root / "served" / name).read_bytes())
    sys.exit(0)
if tool == "svn":
    if args[:1] == ["--username"]:
        args = args[2:]
    prefix = "https://dist.apache.org/repos/dist/release/skywalking/ai-sessionizer/0.3.0/"
    if args[:1] == ["cat"]:
        assert len(args) == 2 and args[1].startswith(prefix), args
        name = args[1][len(prefix):]
        assert "/" not in name and name.endswith((".asc", ".sha512")), name
        sys.stdout.buffer.write((root / "voted" / name).read_bytes())
    elif args[:2] == ["export", "-q"]:
        assert len(args) == 4 and args[2].startswith(prefix), args
        name = args[2][len(prefix):]
        assert "/" not in name and name not in [".", ".."], name
        shutil.copyfile(root / "voted" / name, args[3])
    else:
        raise AssertionError("unexpected svn command: " + repr(args))
    sys.exit(0)
if tool == "gpgconf":
    assert len(args) == 4 and args[0] == "--homedir" and args[2:] == ["--kill", "all"], args
    sys.exit(0)
if tool == "gpg":
    assert "--batch" in args and "--homedir" in args, args
    if "--import" in args:
        assert Path(args[args.index("--import") + 1]).read_text() == "fixture release key", args
    elif "--verify" in args:
        assert "--status-fd" in args and args[args.index("--status-fd") + 1] == "1", args
        signature, package = map(Path, args[-2:])
        assert signature.read_bytes() == (root / "voted" / signature.name).read_bytes()
        assert package.read_bytes() == (root / "voted" / package.name).read_bytes()
        print("[GNUPG:] GOODSIG " + "B" * 16 + " Fixture Release <fixture@apache.org>")
        print("[GNUPG:] VALIDSIG " + "B" * 40 + " 2026-09-14 0 0 4 0 1 10 00 " + "B" * 40)
    else:
        raise AssertionError("unexpected gpg command: " + repr(args))
    sys.exit(0)
assert tool == "gh" and args[:1] == ["release"] and args[2:5] == [tag, "--repo", "apache/skywalking-ai-sessionizer"], args
action = args[1]
rest = args[5:]
state_path = root / "state.json"
state = json.loads(state_path.read_text())
remote = root / "remote"
def save():
    state_path.write_text(json.dumps(state))
def record(action, files=None):
    with (root / "events.jsonl").open("a") as stream:
        stream.write(json.dumps({"action": action, "draft": state["draft"], "prerelease": state["prerelease"], "assets": sorted(p.name for p in remote.iterdir()), "files": files or []}) + "\n")
def option(name):
    return rest[rest.index(name) + 1]
if action == "view":
    assert rest[:1] == ["--json"] and rest[2:3] == ["--jq"] and len(rest) == 4, rest
    if not state["exists"]:
        record("absent")
        sys.exit(1)
    field = option("--json")
    if field == "databaseId,tagName,isDraft,isPrerelease,name":
        record("view_release")
        release_id = "456" if state.get("replaced") else "123"
        print(release_id + "\t" + tag + "\t" + str(state["draft"]).lower() + "\t" + str(state["prerelease"]).lower() + "\t0.3.0")
    elif field == "assets":
        record("view_assets")
        for file in sorted(remote.iterdir()):
            print(file.name)
    elif field == "isDraft":
        record("view_draft")
        print(str(state["draft"]).lower())
    else:
        raise AssertionError("unexpected view field: " + field)
elif action == "upload":
    assert state["exists"] and (state["draft"] or state["prerelease"]), state
    assert rest and all(not arg.startswith("-") for arg in rest), rest
    names = [Path(arg).name for arg in rest]
    record("upload", names)
    for index, arg in enumerate(rest):
        path = Path(arg)
        assert not (remote / path.name).exists(), "attempted to replace an existing asset"
        if path.name != state["omit"]:
            shutil.copyfile(path, remote / path.name)
        if path.name == state["corrupt"]:
            (remote / path.name).write_bytes(b"corrupted upload")
        if state["interrupt"] and index == 1:
            state["interrupt"] = False
            save()
            sys.exit(1)
elif action == "download":
    assert state["exists"], state
    destination = Path(option("--dir"))
    destination.mkdir(parents=True, exist_ok=True)
    if "--pattern" in rest:
        names = [option("--pattern")]
        record("download_one", names)
    else:
        names = sorted(path.name for path in remote.iterdir())
        record("download_all", names)
        if state["replace"]:
            state["replaced"] = True
            save()
    for name in names:
        assert "/" not in name and name not in [".", ".."], name
        shutil.copyfile(remote / name, destination / name)
elif action == "edit":
    assert state["exists"] and (state["draft"] or state["prerelease"]), state
    assert "--draft=false" in rest and "--prerelease=false" in rest and option("--title") == "0.3.0", rest
    assert "Fixture release." in Path(option("--notes-file")).read_text()
    record("publish")
    state["draft"] = False
    state["prerelease"] = False
    save()
else:
    raise AssertionError("unexpected gh command: " + repr(args))
`
