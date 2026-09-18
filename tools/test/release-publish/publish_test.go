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

package releasepublish_test

import (
	"bytes"
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
)

const version = "0.3.0"

// releaseState is the GitHub release the gh fixture serves.
type releaseState struct {
	Exists     bool `json:"exists"`
	Prerelease bool `json:"prerelease"`
	// Interrupt stops the first upload after one file.
	Interrupt bool `json:"interrupt"`
	// Replace gives the release another ID once it has been downloaded.
	Replace bool `json:"replace"`
	// Released are the tags of the other full releases.
	Released []string `json:"released"`
	// Latest is the answer the promotion was given.
	Latest string `json:"latest"`
}

type event struct {
	Action string   `json:"action"`
	Files  []string `json:"files"`
}

type fixture struct {
	t      *testing.T
	dir    string
	script string
	// voted are the files of the vote: each package with its .asc and .sha512.
	voted []string
	// ci are the files CI attached to the prerelease.
	ci []string
}

const devDir = "svn/dev/skywalking/ai-sessionizer/" + version
const releaseDir = "svn/release/skywalking/ai-sessionizer/" + version

// newFixture is the state after candidate and a passed vote: the candidate
// in the dev area, and the prerelease holding CI's files and the candidate's.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("release publish tests use POSIX command fixtures")
	}
	for _, tool := range []string{"bash", "python3", "shasum", "cmp", "tar", "awk"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("release publish tests need %s", tool)
		}
	}
	script, err := filepath.Abs("../../release/release.sh")
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, dir: t.TempDir(), script: script}
	for _, dir := range []string{"bin", "tools", "voted", "gh", devDir, "svn/release/skywalking"} {
		if err := os.MkdirAll(filepath.Join(f.dir, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, kind := range []string{"src.tgz", "bin-linux-amd64.tgz", "bin-asz-amd64.deb"} {
		name := "apache-skywalking-ai-sessionizer-" + version + "-" + kind
		data := []byte("voted package: " + name + "\n")
		files := map[string][]byte{
			name:             data,
			name + ".asc":    []byte("voted signature: " + name + "\n"),
			name + ".sha512": []byte(fmt.Sprintf("%x  %s\n", sha512.Sum512(data), name)),
		}
		for file, content := range files {
			f.write("voted/"+file, content)
			f.write(devDir+"/"+file, content)
			f.write("gh/"+file, content)
			f.voted = append(f.voted, file)
			if strings.HasPrefix(kind, "bin-") && !strings.HasSuffix(file, ".asc") {
				f.ci = append(f.ci, file)
			}
		}
	}
	sort.Strings(f.voted)
	sort.Strings(f.ci)
	for _, tool := range []string{"git", "gh", "curl", "svn", "gpg", "gpgconf"} {
		if err := os.WriteFile(filepath.Join(f.dir, "bin", tool), []byte(fakeTools), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.setState(releaseState{Exists: true, Prerelease: true})
	return f
}

func (f *fixture) write(path string, data []byte) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(f.dir, path)), 0o755); err != nil {
		f.t.Fatal(err)
	}
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

func (f *fixture) names(path string) []string {
	f.t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.dir, path))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
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

func (f *fixture) events(action string) []event {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, "events.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		f.t.Fatal(err)
	}
	var out []event
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var e event
		if err := json.Unmarshal(line, &e); err != nil {
			f.t.Fatal(err)
		}
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

func (f *fixture) run(args ...string) (string, error) {
	f.t.Helper()
	return f.answer("", args...)
}

// answer runs publish with input as what the release manager types.
func (f *fixture) answer(input string, args ...string) (string, error) {
	f.t.Helper()
	cmd := exec.Command("bash", append([]string{f.script, "publish", version}, args...)...)
	cmd.Stdin = strings.NewReader(input)
	cmd.Dir = f.dir
	cmd.Env = append(os.Environ(), "PATH="+filepath.Join(f.dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"), "RELEASE_PUBLISH_FIXTURE="+f.dir)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

// requireFailure runs publish, expects want in its failure, and checks that
// the GitHub release was not promoted.
func (f *fixture) requireFailure(want string) string {
	f.t.Helper()
	output, err := f.run()
	if err == nil || !strings.Contains(output, want) {
		f.t.Fatalf("publish error = %v, want %q in output:\n%s", err, want, output)
	}
	if state := f.state(); (state.Exists && !state.Prerelease) || len(f.events("promote")) > 0 {
		f.t.Fatal("a failed publish promoted the GitHub release")
	}
	return output
}

func (f *fixture) requireNotMoved() {
	f.t.Helper()
	if len(f.events("svn-mv")) > 0 || len(f.names(releaseDir)) > 0 || len(f.names(devDir)) != len(f.voted) {
		f.t.Fatalf("the candidate was moved: release %v, dev %v", f.names(releaseDir), f.names(devDir))
	}
}

func (f *fixture) requirePublished() {
	f.t.Helper()
	if got := strings.Join(f.names(releaseDir), " "); got != strings.Join(f.voted, " ") {
		f.t.Fatalf("release directory = %s, want %v", got, f.voted)
	}
	if f.names(devDir) != nil {
		f.t.Fatalf("the dev candidate is still there: %v", f.names(devDir))
	}
	if got := strings.Join(f.names("gh"), " "); got != strings.Join(f.voted, " ") {
		f.t.Fatalf("GitHub release assets = %s, want %v", got, f.voted)
	}
	for _, name := range f.voted {
		if !bytes.Equal(f.read("gh/"+name), f.read("voted/"+name)) {
			f.t.Fatalf("the GitHub release's %s is not the voted file", name)
		}
	}
	if state := f.state(); state.Prerelease {
		f.t.Fatal("the GitHub release was not promoted")
	}
	for _, name := range []string{"announce.txt", "website.txt"} {
		if _, err := os.Stat(filepath.Join(f.dir, "dist", version, name)); err != nil {
			f.t.Fatalf("publish did not write %s: %v", name, err)
		}
	}
}

func TestPublishMovesTheCandidateAndPromotesThePrerelease(t *testing.T) {
	f := newFixture(t)
	if output, err := f.run(); err != nil {
		t.Fatalf("publish: %v\n%s", err, output)
	}
	f.requirePublished()
	if len(f.events("upload")) != 0 {
		t.Fatalf("publish uploaded files the prerelease already held: %+v", f.events("upload"))
	}
	if len(f.events("svn-mv")) != 1 || len(f.events("promote")) != 1 {
		t.Fatalf("moves/promotions = %d/%d, want 1/1", len(f.events("svn-mv")), len(f.events("promote")))
	}
}

// A prerelease holding only CI's files gets the voted source package and
// signatures before it is promoted. CI's own files are never uploaded.
func TestPublishAttachesWhatThePrereleaseIsMissing(t *testing.T) {
	f := newFixture(t)
	for _, name := range f.voted {
		if !contains(f.ci, name) {
			if err := os.Remove(filepath.Join(f.dir, "gh", name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if output, err := f.run(); err != nil {
		t.Fatalf("publish: %v\n%s", err, output)
	}
	f.requirePublished()
	uploads := f.events("upload")
	if len(uploads) != 1 || len(uploads[0].Files) != len(f.voted)-len(f.ci) {
		t.Fatalf("uploads = %+v, want the %d files CI does not attach", uploads, len(f.voted)-len(f.ci))
	}
	for _, name := range uploads[0].Files {
		if contains(f.ci, name) {
			t.Fatalf("publish uploaded CI's file %s", name)
		}
	}
}

func TestPublishRefusesBeforeMovingWhenTheReleaseIsNotReady(t *testing.T) {
	for _, tc := range []struct {
		name, diagnostic string
		change           func(*fixture)
	}{
		{"no-release", "cannot read the GitHub release", func(f *fixture) { f.setState(releaseState{}) }},
		{"missing-ci-archive", "has no apache-skywalking-ai-sessionizer-0.3.0-bin-linux-amd64.tgz.", func(f *fixture) {
			if err := os.Remove(filepath.Join(f.dir, "gh", "apache-skywalking-ai-sessionizer-0.3.0-bin-linux-amd64.tgz")); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"missing-ci-checksum", "has no apache-skywalking-ai-sessionizer-0.3.0-bin-linux-amd64.tgz.sha512", func(f *fixture) {
			if err := os.Remove(filepath.Join(f.dir, "gh", "apache-skywalking-ai-sessionizer-0.3.0-bin-linux-amd64.tgz.sha512")); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"unexpected-asset", "unexpected asset, unapproved.txt", func(f *fixture) { f.write("gh/unapproved.txt", []byte("x")) }},
		{"bad-signature", "has no valid signature", func(f *fixture) {
			f.write(devDir+"/apache-skywalking-ai-sessionizer-0.3.0-src.tgz.asc", []byte("forged\n"))
		}},
		{"stale-local-copy", "is not the voted one", func(f *fixture) {
			name := "apache-skywalking-ai-sessionizer-0.3.0-src.tgz"
			for _, file := range []string{name, name + ".asc"} {
				f.write("dist/"+version+"/"+file, f.read("voted/"+file))
			}
			f.write("dist/"+version+"/"+name+".sha512", []byte("stale checksum\n"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.change(f)
			f.requireFailure(tc.diagnostic)
			f.requireNotMoved()
		})
	}
}

// gpg exits 2 when one KEYS entry cannot be imported. Each package's
// signature is still verified against the keys that did import.
func TestPublishToleratesAnUnimportableKeysEntry(t *testing.T) {
	f := newFixture(t)
	t.Setenv("RELEASE_PUBLISH_IMPORT_STATUS", "2")
	if output, err := f.run(); err != nil {
		t.Fatalf("publish with a partial KEYS import: %v\n%s", err, output)
	}
	f.requirePublished()
}

// A promotion that stops after the move is finished by running publish
// again. The move is not repeated, and an attached file is never replaced.
func TestPublishResumesAfterTheMove(t *testing.T) {
	f := newFixture(t)
	for _, name := range f.voted {
		if !contains(f.ci, name) {
			if err := os.Remove(filepath.Join(f.dir, "gh", name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	f.setState(releaseState{Exists: true, Prerelease: true, Interrupt: true})
	f.requireFailure("The move is done. Run 'tools/release/release.sh publish 0.3.0' again")
	if len(f.names(releaseDir)) != len(f.voted) {
		t.Fatal("the interrupted run did not move the candidate")
	}
	output, err := f.run()
	if err != nil || !strings.Contains(output, "an earlier publish moved the candidate") {
		t.Fatalf("resumed publish: %v\n%s", err, output)
	}
	f.requirePublished()
	if len(f.events("svn-mv")) != 1 {
		t.Fatalf("the move ran %d times", len(f.events("svn-mv")))
	}
	uploads := f.events("upload")
	if len(uploads) != 2 || len(uploads[1].Files) != len(f.voted)-len(f.ci)-1 {
		t.Fatalf("uploads = %+v, want the rest after the one file the first run attached", uploads)
	}
}

func TestPublishRefusesConflictingAttachmentsWithoutReplacingThem(t *testing.T) {
	f := newFixture(t)
	name := "apache-skywalking-ai-sessionizer-0.3.0-src.tgz.asc"
	f.write("gh/"+name, []byte("another signature\n"))
	f.requireFailure("the GitHub release's " + name + " differs")
	if got := string(f.read("gh/" + name)); got != "another signature\n" {
		t.Fatalf("the conflicting asset was replaced: %q", got)
	}
	if len(f.events("upload")) != 0 {
		t.Fatal("publish uploaded beside a conflicting asset")
	}
}

// CI's files are never uploaded, so they are compared only in the final
// check of every byte. A binary replaced on GitHub under its own name must
// stop the promotion, and a later run must find it too.
func TestPublishRefusesAReplacedCIBinary(t *testing.T) {
	binary := "apache-skywalking-ai-sessionizer-0.3.0-bin-linux-amd64.tgz"
	t.Run("before-promotion", func(t *testing.T) {
		f := newFixture(t)
		f.write("gh/"+binary, []byte("rebuilt elsewhere\n"))
		f.requireFailure("the GitHub release's " + binary + " differs")
		if got := string(f.read("gh/" + binary)); got != "rebuilt elsewhere\n" {
			t.Fatalf("the replaced binary was overwritten: %q", got)
		}
	})
	t.Run("after-promotion", func(t *testing.T) {
		f := newFixture(t)
		if output, err := f.run(); err != nil {
			t.Fatalf("publish: %v\n%s", err, output)
		}
		f.write("gh/"+binary, []byte("rebuilt elsewhere\n"))
		output, err := f.run()
		if err == nil || !strings.Contains(output, "the GitHub release's "+binary+" differs") {
			t.Fatalf("a replaced binary on the promoted release passed: %v\n%s", err, output)
		}
	})
}

func TestPublishRefusesAReleaseReplacedDuringVerification(t *testing.T) {
	f := newFixture(t)
	f.setState(releaseState{Exists: true, Prerelease: true, Replace: true})
	f.requireFailure("changed while publish was checking it")
}

// A later run, such as the one with --remove-old, finds the release promoted
// already. It checks the files again and promotes nothing.
func TestPublishAfterPromotionOnlyChecks(t *testing.T) {
	f := newFixture(t)
	if output, err := f.run(); err != nil {
		t.Fatalf("publish: %v\n%s", err, output)
	}
	output, err := f.run()
	if err != nil || !strings.Contains(output, "is a full GitHub release already") {
		t.Fatalf("second publish: %v\n%s", err, output)
	}
	f.requirePublished()
	if len(f.events("promote")) != 1 || len(f.events("svn-mv")) != 1 || len(f.events("upload")) != 0 {
		t.Fatal("the second run moved, uploaded or promoted again")
	}
}

func TestPublishNeedsNoLocalCandidate(t *testing.T) {
	f := newFixture(t)
	if _, err := os.Stat(filepath.Join(f.dir, "dist")); !os.IsNotExist(err) {
		t.Fatal("the fixture must start without a local candidate")
	}
	if output, err := f.run(); err != nil {
		t.Fatalf("publish without a local candidate: %v\n%s", err, output)
	}
	f.requirePublished()
	for _, name := range f.voted {
		if !bytes.Equal(f.read("dist/"+version+"/"+name), f.read("voted/"+name)) {
			t.Fatalf("fetched %s is not the voted file", name)
		}
	}
}

// Promotion does not move GitHub's Latest label by itself, and the label
// decides the latest image tag, so publish asks. The answer offered is yes
// only for a version newer than every full release.
func TestPublishAsksWhetherTheVersionBecomesLatest(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		released    []string
		args        []string
		latest      string
	}{
		{name: "first-release", latest: "true"},
		{name: "newest", released: []string{"v0.2.0", "v0.1.0"}, latest: "true"},
		{name: "patch-of-an-older-line", released: []string{"v0.4.0", "v0.2.0"}, latest: "false"},
		{name: "answered-no", input: "no\n", released: []string{"v0.2.0"}, latest: "false"},
		{name: "answered-yes", input: "Y\n", released: []string{"v0.4.0"}, latest: "true"},
		{name: "not-latest-option", released: []string{"v0.2.0"}, args: []string{"--not-latest"}, latest: "false"},
		{name: "latest-option", released: []string{"v0.4.0"}, args: []string{"--latest"}, latest: "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.setState(releaseState{Exists: true, Prerelease: true, Released: tc.released})
			if output, err := f.answer(tc.input, tc.args...); err != nil {
				t.Fatalf("publish: %v\n%s", err, output)
			}
			f.requirePublished()
			if got := f.state().Latest; got != tc.latest {
				t.Fatalf("promoted with --latest=%s, want %s", got, tc.latest)
			}
			f.requireWebsiteLatest(tc.latest == "true")
		})
	}
}

// The website entries are a release of data/projects.yml in
// apache/skywalking-website. They mark the release as the latest only when
// GitHub's label does, and only the latest release carries the Latest
// documentation.
func (f *fixture) requireWebsiteLatest(latest bool) {
	f.t.Helper()
	text := string(f.read("dist/" + version + "/website.txt"))
	pkg := "apache-skywalking-ai-sessionizer-" + version
	for _, want := range []string{
		"data/projects.yml",
		"          - version: v" + version + "\n",
		"              link: /docs/skywalking-ai-sessionizer/v" + version + "/readme/\n",
		"              - name: Source archive\n                type: source\n" +
			"                link: https://www.apache.org/dyn/closer.lua/skywalking/ai-sessionizer/" + version + "/" + pkg + "-src.tgz\n" +
			"                asc: https://downloads.apache.org/skywalking/ai-sessionizer/" + version + "/" + pkg + "-src.tgz.asc\n" +
			"                sha512: https://downloads.apache.org/skywalking/ai-sessionizer/" + version + "/" + pkg + "-src.tgz.sha512\n",
		"              - name: Linux AMD64\n                type: binary\n" +
			"                link: https://www.apache.org/dyn/closer.lua/skywalking/ai-sessionizer/" + version + "/" + pkg + "-bin-linux-amd64.tgz\n",
		"            date: Sep. 15th, 2026\n",
	} {
		if !strings.Contains(text, want) {
			f.t.Fatalf("website.txt has no %q:\n%s", want, text)
		}
	}
	// The Debian packages are in the release directory, and apt installs
	// them. The downloads page lists the source and the binary archives.
	for _, stale := range []string{"releases.yml", "docs.yml", "downloadLink", ".deb"} {
		if strings.Contains(text, stale) {
			f.t.Fatalf("website.txt still has the retired shape %q:\n%s", stale, text)
		}
	}
	marked := strings.Contains(text, "            latest: true\n")
	if marked != latest || strings.Contains(text, "            latest: false\n") == latest {
		f.t.Fatalf("website.txt marks the release latest=%v, want %v:\n%s", marked, latest, text)
	}
	if strings.Contains(text, "latestLink: /docs/skywalking-ai-sessionizer/latest/readme/") != latest {
		f.t.Fatalf("website.txt gives the Latest documentation=%v, want %v:\n%s", !latest, latest, text)
	}
}

// A later run cannot be told whether the release is the latest, so it reads
// the label the promoting run set, and the website entries agree with it.
func TestPublishAfterPromotionReadsTheLatestLabel(t *testing.T) {
	for _, tc := range []struct {
		name     string
		released []string
		args     []string
		latest   bool
	}{
		{name: "latest", released: []string{"v0.2.0"}, latest: true},
		{name: "not-latest", released: []string{"v0.4.0"}, args: []string{"--not-latest"}, latest: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.setState(releaseState{Exists: true, Prerelease: true, Released: tc.released})
			if output, err := f.run(tc.args...); err != nil {
				t.Fatalf("publish: %v\n%s", err, output)
			}
			if err := os.Remove(filepath.Join(f.dir, "dist", version, "website.txt")); err != nil {
				t.Fatal(err)
			}
			if output, err := f.run(); err != nil {
				t.Fatalf("second publish: %v\n%s", err, output)
			}
			f.requirePublished()
			f.requireWebsiteLatest(tc.latest)
		})
	}
}

func TestPublishRefusesAnUnclearLatestAnswer(t *testing.T) {
	t.Run("not-yes-or-no", func(t *testing.T) {
		f := newFixture(t)
		output, err := f.answer("maybe\n")
		if err == nil || !strings.Contains(output, "answer yes or no") {
			t.Fatalf("an unclear answer was accepted: %v\n%s", err, output)
		}
		f.requireNotMoved()
	})
	t.Run("both-options", func(t *testing.T) {
		f := newFixture(t)
		output, err := f.run("--latest", "--not-latest")
		if err == nil || !strings.Contains(output, "only one of --latest and --not-latest") {
			t.Fatalf("both options were accepted: %v\n%s", err, output)
		}
		f.requireNotMoved()
	})
	// The run that promoted decided the label, so a later run refuses to
	// ignore the option and says how to change the label by hand.
	t.Run("already-promoted", func(t *testing.T) {
		f := newFixture(t)
		if output, err := f.run(); err != nil {
			t.Fatalf("publish: %v\n%s", err, output)
		}
		output, err := f.run("--not-latest")
		if err == nil || !strings.Contains(output, "--latest=<true or false>") {
			t.Fatalf("an option on a promoted release was accepted: %v\n%s", err, output)
		}
		if f.state().Latest != "true" || len(f.events("promote")) != 1 {
			t.Fatal("the promoted release was changed")
		}
	})
}

func contains(list []string, name string) bool {
	for _, item := range list {
		if item == name {
			return true
		}
	}
	return false
}

// These stand in for git, svn, gh, curl and gpg. dist.apache.org is the svn
// directory of the fixture, and the GitHub release's assets are its gh
// directory. No command reaches a network.
const fakeTools = `#!/usr/bin/env python3
import json, os, pathlib, shutil, sys

root = pathlib.Path(os.environ["RELEASE_PUBLISH_FIXTURE"])
args = sys.argv[1:]
tool = pathlib.Path(sys.argv[0]).name
tag = "v0.3.0"
sha = "a" * 40

def record(action, files=None):
    with (root / "events.jsonl").open("a") as stream:
        stream.write(json.dumps({"action": action, "files": files or []}) + "\n")

if tool == "git":
    if args == ["rev-parse", "--show-toplevel"]:
        print(root)
    elif args == ["ls-remote", "--tags", "origin", "refs/tags/" + tag]:
        print(sha + "\trefs/tags/" + tag)
    elif args in [["rev-parse", "-q", "--verify", "refs/tags/" + tag], ["rev-parse", tag + "^{commit}"]]:
        print(sha)
    elif args == ["show", tag + ":Makefile"]:
        print("PLATFORMS := linux/amd64\nDEB_PACKAGES := asz")
    elif args == ["show", tag + ":docs/en/changes/changes.md"]:
        print("# Changes in 0.3.0\n\nFixture release. See [Install](../setup/install.md#verify-a-package), [this](#fixes) and [KEYS](https://downloads.apache.org/skywalking/KEYS).")
    else:
        raise AssertionError("unexpected git command: " + repr(args))
    sys.exit(0)

if tool == "curl":
    assert args[:2] == ["-fsSL", "https://dist.apache.org/repos/dist/release/skywalking/KEYS"] and args[2] == "-o", args
    pathlib.Path(args[3]).write_text("fixture release key")
    sys.exit(0)

if tool == "gpgconf":
    sys.exit(0)

if tool == "gpg":
    if "--import" in args:
        assert pathlib.Path(args[args.index("--import") + 1]).read_text() == "fixture release key", args
        sys.exit(int(os.environ.get("RELEASE_PUBLISH_IMPORT_STATUS", "0")))
    assert "--verify" in args and args[args.index("--status-fd") + 1] == "1", args
    signature, package = map(pathlib.Path, args[-2:])
    if signature.read_text() == "voted signature: " + package.name + "\n":
        print("[GNUPG:] GOODSIG " + "B" * 16 + " Fixture Release <fixture@apache.org>")
        print("[GNUPG:] VALIDSIG " + "B" * 40 + " 2026-09-14 0 0 4 0 1 10 00 " + "B" * 40)
    sys.exit(0)

if tool == "svn":
    if args[:1] == ["--username"]:
        args = args[2:]
    prefix = "https://dist.apache.org/repos/dist/"
    def local(url):
        assert url.startswith(prefix), url
        return root / "svn" / url[len(prefix):]
    if args[0] == "ls":
        path = local(args[1])
        if not path.is_dir():
            sys.exit(1)
        for entry in sorted(path.iterdir()):
            print(entry.name + ("/" if entry.is_dir() else ""))
    elif args[0] == "cat":
        sys.stdout.buffer.write(local(args[1]).read_bytes())
    elif args[:3] == ["export", "-q", "--force"]:
        shutil.copyfile(local(args[3]), args[4])
    elif args[:2] == ["mkdir", "-m"]:
        local(args[3]).mkdir()
    elif args[:2] == ["mv", "-m"]:
        source, target = local(args[3]), local(args[4])
        assert source.is_dir() and not target.exists(), args
        target.parent.mkdir(parents=True, exist_ok=True)
        source.rename(target)
        record("svn-mv")
    elif args[:2] == ["rm", "-m"]:
        shutil.rmtree(local(args[3]))
    elif args[:3] == ["info", "--show-item", "last-changed-date"]:
        print("2026-09-15T01:02:03.000000Z")
    else:
        raise AssertionError("unexpected svn command: " + repr(args))
    sys.exit(0)

state_path = root / "state.json"
state = json.loads(state_path.read_text())
if args[:2] == ["release", "list"]:
    assert args[2:4] == ["--repo", "apache/skywalking-ai-sessionizer"] and "--json" in args, args
    tags = list(state.get("released") or [])
    if state["exists"] and not state["prerelease"]:
        tags.append(tag)
    if "isLatest" in args[args.index("--json") + 1]:
        # GitHub's Latest label: this release when its promotion said so,
        # and otherwise the newest of the others.
        if tag in tags and state.get("latest") == "true":
            tags = [tag]
        else:
            others = [t for t in tags if t != tag]
            tags = [max(others, key=lambda t: [int(n) for n in t[1:].split(".")])] if others else []
    for t in tags:
        print(t)
    sys.exit(0)
assert tool == "gh" and args[:1] == ["release"] and args[2:5] == [tag, "--repo", "apache/skywalking-ai-sessionizer"], args
action, rest = args[1], args[5:]
assets = root / "gh"
def save():
    state_path.write_text(json.dumps(state))
def option(name):
    return rest[rest.index(name) + 1]
if action == "view":
    if not state["exists"]:
        sys.exit(1)
    field = option("--json")
    if field == "databaseId,tagName,isDraft,isPrerelease,name":
        release_id = "456" if state.get("replaced") else "123"
        print("\t".join([release_id, tag, "false", str(state["prerelease"]).lower(), "0.3.0"]))
    elif field == "assets":
        for entry in sorted(assets.iterdir()):
            print(entry.name)
    else:
        raise AssertionError("unexpected view field: " + field)
elif action == "download":
    destination = pathlib.Path(option("--dir"))
    destination.mkdir(parents=True, exist_ok=True)
    if "--pattern" in rest:
        chosen = [option("--pattern")]
    else:
        chosen = [entry.name for entry in assets.iterdir()]
        if state["replace"]:
            state["replaced"] = True
            save()
    for name in chosen:
        shutil.copyfile(assets / name, destination / name)
elif action == "upload":
    assert all(not arg.startswith("-") for arg in rest), rest
    record("upload", [pathlib.Path(arg).name for arg in rest])
    for index, arg in enumerate(rest):
        path = pathlib.Path(arg)
        assert not (assets / path.name).exists(), "replaced an existing asset: " + path.name
        shutil.copyfile(path, assets / path.name)
        if state["interrupt"] and index == 0:
            state["interrupt"] = False
            save()
            sys.exit(1)
elif action == "edit":
    assert state["prerelease"], "promoted twice"
    assert "--draft=false" in rest and "--prerelease=false" in rest and option("--title") == "0.3.0", rest
    notes = pathlib.Path(option("--notes-file")).read_text()
    assert "Fixture release." in notes, notes
    # A relative link of the changelog would resolve under /releases/.
    assert "[Install](https://github.com/apache/skywalking-ai-sessionizer/blob/v0.3.0/docs/en/setup/install.md#verify-a-package)" in notes, notes
    assert "[this](https://github.com/apache/skywalking-ai-sessionizer/blob/v0.3.0/docs/en/changes/changes.md#fixes)" in notes, notes
    assert "[KEYS](https://downloads.apache.org/skywalking/KEYS)" in notes, notes
    latest = [arg for arg in rest if arg.startswith("--latest=")]
    assert len(latest) == 1 and latest[0] in ("--latest=true", "--latest=false"), rest
    record("promote")
    state["prerelease"] = False
    state["latest"] = latest[0].split("=", 1)[1]
    save()
else:
    raise AssertionError("unexpected gh command: " + repr(args))
`
