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

package releasecandidate_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha512"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

type candidateFixture struct {
	dir, binaries, commands, archive, state string
	env                                     []string
}

func candidateFixtureFor(t *testing.T) *candidateFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("release script fixtures use POSIX commands")
	}
	for _, tool := range []string{"bash", "git", "python3", "shasum", "tar", "unzip", "file"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("release candidate fixtures need %s", tool)
		}
	}
	base := t.TempDir()
	f := &candidateFixture{
		dir:      filepath.Join(base, "repo"),
		binaries: filepath.Join(base, "ci"),
		commands: filepath.Join(base, "commands"),
	}
	write := func(path, content string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"release.sh", "package-check.sh"} {
		body, err := os.ReadFile(filepath.Join("..", name))
		if err != nil {
			t.Fatal(err)
		}
		write(filepath.Join(f.dir, "tools", name), string(body), 0o755)
	}
	platform := "linux/amd64"
	if runtime.GOOS == "linux" {
		platform = "darwin/arm64"
	}
	f.archive = "apache-skywalking-ai-sessionizer-0.3.0-bin-" + strings.ReplaceAll(platform, "/", "-") + ".tgz"
	write(filepath.Join(f.dir, "Makefile"), "PLATFORMS := "+platform+"\nCOMPILED_TYPES := application/x-executable\nCOMPILED_FILES := \\.(exe|o)\n", 0o644)
	write(filepath.Join(f.dir, "LICENSE"), "Apache License fixture\n", 0o644)
	write(filepath.Join(f.dir, "NOTICE"), "ASF fixture\n", 0o644)
	write(filepath.Join(f.dir, "docs/en/changes/changes.md"), "# Changes in 0.3.0\n\nFixture release.\n", 0o644)
	write(filepath.Join(f.dir, ".gitattributes"), "._* export-ignore\n.DS_Store export-ignore\n__MACOSX/** export-ignore\n", 0o644)
	write(filepath.Join(f.dir, "._noise"), "metadata", 0o644)
	write(filepath.Join(f.dir, ".DS_Store"), "metadata", 0o644)

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(realGit, args...)
		cmd.Dir = f.dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("add", ".")
	git("-c", "user.name=Release Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture")
	git("tag", "v0.3.0")
	git("remote", "add", "origin", "https://example.invalid/release.git")
	commit := git("rev-parse", "HEAD")
	write(filepath.Join(f.dir, "untracked-local.txt"), "must not enter source archive", 0o644)

	write(filepath.Join(f.commands, "git"), `#!/bin/sh
if [ "$1" = ls-remote ]; then printf '%s\trefs/tags/v0.3.0\n' "$RELEASE_TEST_COMMIT"; exit 0; fi
exec "$RELEASE_TEST_GIT" "$@"
`, 0o755)
	write(filepath.Join(f.commands, "gpgconf"), "#!/bin/sh\nexit 0\n", 0o755)
	// svn and gh keep their state in files, so an upload and a later run of
	// candidate see what an earlier one left. No command reaches a network.
	f.state = filepath.Join(base, "state")
	for _, dir := range []string{"svn/dev/skywalking", "svn/release/skywalking", "gh"} {
		if err := os.MkdirAll(filepath.Join(f.state, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(f.commands, "svn"), fakeSVN, 0o755)
	write(filepath.Join(f.commands, "gh"), fakeGH, 0o755)
	write(filepath.Join(f.commands, "curl"), `#!/bin/sh
while [ "$#" -gt 0 ]; do
  if [ "$1" = -o ]; then printf 'fixture keys\n' > "$2"; exit 0; fi
  shift
done
exit 1
`, 0o755)
	write(filepath.Join(f.commands, "gpg"), `#!/bin/sh
printf 'gpg %s\n' "$*" >> "$RELEASE_TEST_LOG"
case "$*" in
  *--list-keys*)
    printf 'pub:-:4096:1::::::::\nfpr:::::::::AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA:\nuid:-::::::::Fixture <fixture@apache.org>:\n'
    exit 0 ;;
  *--verify*)
    printf '[GNUPG:] GOODSIG AAAAAAAA Fixture\n[GNUPG:] VALIDSIG AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n'
    exit 0 ;;
  *--import*) exit 0 ;;
  *--detach-sign*)
    dest=''
    while [ "$#" -gt 0 ]; do
      if [ "$1" = --output ]; then dest=$2; shift; fi
      last=$1
      shift
    done
    [ -n "$dest" ] || dest="$last.asc"
    printf 'fixture signature\n' > "$dest"
    exit 0 ;;
esac
exit 1
`, 0o755)
	write(filepath.Join(f.dir, "tools/ci-binaries.sh"), `#!/bin/sh
printf '%s\n' "$1|$2|$3" > "$RELEASE_TEST_SELECTION"
[ "${RELEASE_TEST_FAIL_CI:-}" != 1 ] || exit 1
mkdir -p "$4"
cp "$RELEASE_TEST_BINARIES/"* "$4/"
echo 'ci-binaries: verified packages from https://github.com/apache/skywalking-ai-sessionizer/actions/runs/123'
echo 'ci-binaries: artifact 456, sha256:fixture, commit fixture'
`, 0o755)

	var packed bytes.Buffer
	gz := gzip.NewWriter(&packed)
	tw := tar.NewWriter(gz)
	for _, name := range []string{"asz", "asz-claude-plugin", "LICENSE", "NOTICE", "licenses/license.txt"} {
		body := []byte("CI bytes for " + name + "\n")
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(f.binaries, f.archive), packed.String(), 0o644)
	write(filepath.Join(f.binaries, f.archive+".sha512"), fmt.Sprintf("%x  %s\n", sha512.Sum512(packed.Bytes()), f.archive), 0o644)
	// The prerelease holds what CI attached on the tag push.
	for _, name := range []string{f.archive, f.archive + ".sha512"} {
		body, err := os.ReadFile(filepath.Join(f.binaries, name))
		if err != nil {
			t.Fatal(err)
		}
		write(filepath.Join(f.state, "gh", name), string(body), 0o644)
	}
	f.env = append(os.Environ(),
		"PATH="+f.commands+string(os.PathListSeparator)+os.Getenv("PATH"),
		"RELEASE_TEST_GIT="+realGit, "RELEASE_TEST_COMMIT="+commit,
		"RELEASE_TEST_BINARIES="+f.binaries,
		"RELEASE_TEST_LOG="+filepath.Join(base, "gpg.log"),
		"RELEASE_TEST_SELECTION="+filepath.Join(base, "selection"),
		"RELEASE_TEST_STATE="+f.state,
		"GPG_USER=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	)
	return f
}

func (f *candidateFixture) run(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	return f.stage(t, append([]string{"--no-upload"}, args...)...)
}

func (f *candidateFixture) stage(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{"tools/release.sh", "candidate", "0.3.0"}, args...)...)
	cmd.Dir, cmd.Env = f.dir, f.env
	return cmd.CombinedOutput()
}

// names lists a directory, or nothing when it does not exist.
func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func (f *candidateFixture) voted() []string {
	var out []string
	for _, p := range []string{"apache-skywalking-ai-sessionizer-0.3.0-src.tgz", f.archive} {
		out = append(out, p, p+".asc", p+".sha512")
	}
	sort.Strings(out)
	return out
}

func (f *candidateFixture) packageSignatures(t *testing.T) int {
	t.Helper()
	log, err := os.ReadFile(filepath.Join(filepath.Dir(f.dir), "gpg.log"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(log), "\n") {
		if strings.Contains(line, "--detach-sign") && strings.Contains(line, "dist/0.3.0/") {
			n++
		}
	}
	return n
}

// The candidate goes to the dev area of dist.apache.org and the same files
// to the GitHub prerelease, beside CI's own, before the vote mail is written.
func TestCandidateUploadsToSVNAndAttachesToThePrerelease(t *testing.T) {
	f := candidateFixtureFor(t)
	output, err := f.stage(t)
	if err != nil {
		t.Fatalf("candidate: %s\n%v", output, err)
	}
	dev := filepath.Join(f.state, "svn/dev/skywalking/ai-sessionizer/0.3.0")
	if got := names(t, dev); strings.Join(got, " ") != strings.Join(f.voted(), " ") {
		t.Fatalf("svn candidate = %v, want %v", got, f.voted())
	}
	if got := names(t, filepath.Join(f.state, "gh")); strings.Join(got, " ") != strings.Join(f.voted(), " ") {
		t.Fatalf("prerelease assets = %v, want %v", got, f.voted())
	}
	for _, name := range f.voted() {
		local, err := os.ReadFile(filepath.Join(f.dir, "dist/0.3.0", name))
		if err != nil {
			t.Fatal(err)
		}
		for _, remote := range []string{filepath.Join(dev, name), filepath.Join(f.state, "gh", name)} {
			if got, err := os.ReadFile(remote); err != nil || !bytes.Equal(got, local) {
				t.Fatalf("%s differs from the signed candidate: %v", remote, err)
			}
		}
	}
	vote, err := os.ReadFile(filepath.Join(f.dir, "dist/0.3.0/vote.txt"))
	if err != nil || !strings.Contains(string(vote), "releases/tag/v0.3.0") {
		t.Fatalf("vote mail does not link the prerelease: %v\n%s", err, vote)
	}
}

// The prerelease must hold the very binaries candidate signed. One replaced
// on GitHub after the download is found by the check of every byte, and no
// vote mail is written.
func TestCandidateRefusesAPrereleaseWhoseBinaryChanged(t *testing.T) {
	f := candidateFixtureFor(t)
	if err := os.WriteFile(filepath.Join(f.state, "gh", f.archive), []byte("rebuilt elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output, err := f.stage(t)
	if err == nil || !strings.Contains(string(output), "the GitHub release's "+f.archive+" differs") {
		t.Fatalf("a changed prerelease binary passed: %v\n%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "dist/0.3.0/vote.txt")); !os.IsNotExist(err) {
		t.Fatal("a vote mail was written for a prerelease that differs from the candidate")
	}
}

// An attachment that stops after the upload is finished by running candidate
// again. The uploaded signatures are the ones the vote is about, so nothing
// is signed or uploaded to svn a second time.
func TestCandidateResumesAnInterruptedAttachmentWithoutSigningAgain(t *testing.T) {
	f := candidateFixtureFor(t)
	env := f.env
	f.env = append(env, "RELEASE_TEST_GH_UPLOAD_FAIL=1")
	output, err := f.stage(t)
	if err == nil || !strings.Contains(string(output), "it signs and uploads nothing again") {
		t.Fatalf("interrupted attachment: %v\n%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "dist/0.3.0/vote.txt")); !os.IsNotExist(err) {
		t.Fatal("a vote mail was written before the prerelease held the candidate")
	}
	signed := f.packageSignatures(t)
	f.env = env
	output, err = f.stage(t)
	if err != nil || !strings.Contains(string(output), "signed and uploaded already") {
		t.Fatalf("resumed candidate: %v\n%s", err, output)
	}
	if n := f.packageSignatures(t); n != signed {
		t.Fatalf("resumed candidate signed %d packages again", n-signed)
	}
	commits, err := os.ReadFile(filepath.Join(f.state, "svn-commits"))
	if err != nil || strings.Count(string(commits), "\n") != 1 {
		t.Fatalf("svn commits = %q, want one: %v", commits, err)
	}
	if got := names(t, filepath.Join(f.state, "gh")); strings.Join(got, " ") != strings.Join(f.voted(), " ") {
		t.Fatalf("prerelease assets = %v, want %v", got, f.voted())
	}
	if _, err := os.Stat(filepath.Join(f.dir, "dist/0.3.0/vote.txt")); err != nil {
		t.Fatalf("resumed candidate wrote no vote mail: %v", err)
	}
}

// A candidate on dist.apache.org that this checkout did not sign is never
// attached or described in a mail: it is removed before a new one.
func TestCandidateRefusesAnUploadedCandidateItDidNotSign(t *testing.T) {
	f := candidateFixtureFor(t)
	if output, err := f.stage(t); err != nil {
		t.Fatalf("candidate: %s\n%v", output, err)
	}
	for _, name := range names(t, filepath.Join(f.state, "gh")) {
		if strings.HasSuffix(name, ".asc") || strings.Contains(name, "-src.tgz") {
			if err := os.Remove(filepath.Join(f.state, "gh", name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.RemoveAll(filepath.Join(f.dir, "dist")); err != nil {
		t.Fatal(err)
	}
	output, err := f.stage(t)
	if err == nil || !strings.Contains(string(output), "does not hold the same files") {
		t.Fatalf("foreign candidate accepted: %v\n%s", err, output)
	}
	if got := names(t, filepath.Join(f.state, "gh")); len(got) != 2 {
		t.Fatalf("prerelease changed to %v", got)
	}
}

// These stand in for svn and gh. dist.apache.org is a directory under
// RELEASE_TEST_STATE/svn, and the prerelease's assets are RELEASE_TEST_STATE/gh.
const fakeSVN = `#!/usr/bin/env python3
import os, pathlib, shutil, sys
state = pathlib.Path(os.environ["RELEASE_TEST_STATE"])
args = sys.argv[1:]
if args[:1] == ["--username"]:
    args = args[2:]
prefix = "https://dist.apache.org/repos/dist/"
def local(url):
    assert url.startswith(prefix), url
    return state / "svn" / url[len(prefix):]
if args[0] == "ls":
    path = local(args[1])
    if not path.is_dir():
        sys.exit(1)
    for entry in sorted(path.iterdir()):
        print(entry.name + ("/" if entry.is_dir() else ""))
elif args[0] == "cat":
    path = local(args[1])
    if not path.is_file():
        sys.exit(1)
    sys.stdout.buffer.write(path.read_bytes())
elif args[:4] == ["checkout", "-q", "--depth", "empty"]:
    pathlib.Path(args[5]).mkdir(parents=True)
    (pathlib.Path(args[5]) / ".url").write_text(args[4])
elif args[:4] == ["update", "-q", "--set-depth", "immediates"]:
    pathlib.Path(args[4]).mkdir(parents=True, exist_ok=True)
elif args[:2] == ["add", "-q"]:
    pass
elif args[:2] == ["commit", "-m"]:
    wc = pathlib.Path.cwd()
    target = local((wc / ".url").read_text())
    for entry in wc.iterdir():
        if entry.name != ".url":
            shutil.copytree(entry, target / entry.name, dirs_exist_ok=True)
    with (state / "svn-commits").open("a") as log:
        log.write(args[2] + "\n")
else:
    raise AssertionError("unexpected svn command: " + repr(args))
`

const fakeGH = `#!/usr/bin/env python3
import os, pathlib, shutil, sys
state = pathlib.Path(os.environ["RELEASE_TEST_STATE"])
assets = state / "gh"
args = sys.argv[1:]
assert args[:1] == ["release"] and args[2:5] == ["v0.3.0", "--repo", "apache/skywalking-ai-sessionizer"], args
action, rest = args[1], args[5:]
if action == "view":
    assert rest == ["--json", "assets", "--jq", ".assets[].name"], rest
    for entry in sorted(assets.iterdir()):
        print(entry.name)
elif action == "download":
    destination = pathlib.Path(rest[rest.index("--dir") + 1])
    destination.mkdir(parents=True, exist_ok=True)
    chosen = [rest[rest.index("--pattern") + 1]] if "--pattern" in rest else [e.name for e in assets.iterdir()]
    for name in chosen:
        shutil.copyfile(assets / name, destination / name)
elif action == "upload":
    for index, name in enumerate(rest):
        path = pathlib.Path(name)
        assert not (assets / path.name).exists(), "replaced an existing asset: " + path.name
        shutil.copyfile(path, assets / path.name)
        if os.environ.get("RELEASE_TEST_GH_UPLOAD_FAIL") and index == 0:
            sys.exit(1)
else:
    raise AssertionError("unexpected gh command: " + repr(args))
`

func TestCandidateSignsUnchangedCIBinariesAndArchivesOnlyTaggedSource(t *testing.T) {
	for _, args := range [][]string{nil, {"--ci-run", "123"}} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			f := candidateFixtureFor(t)
			output, err := f.run(t, args...)
			if err != nil {
				t.Fatalf("candidate: %s\n%v", output, err)
			}
			dir := filepath.Join(f.dir, "dist/0.3.0")
			for _, suffix := range []string{"", ".sha512"} {
				want, err := os.ReadFile(filepath.Join(f.binaries, f.archive+suffix))
				if err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(filepath.Join(dir, f.archive+suffix))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("CI %s was changed", suffix)
				}
			}
			for _, name := range []string{f.archive + ".asc", "apache-skywalking-ai-sessionizer-0.3.0-src.tgz.asc", "ci-provenance.txt", "vote-preview.txt"} {
				if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "vote.txt")); !os.IsNotExist(err) {
				t.Fatal("no-upload wrote a vote")
			}
			source, err := os.Open(filepath.Join(dir, "apache-skywalking-ai-sessionizer-0.3.0-src.tgz"))
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			gz, err := gzip.NewReader(source)
			if err != nil {
				t.Fatal(err)
			}
			defer gz.Close()
			tr := tar.NewReader(gz)
			for {
				header, err := tr.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(header.Name, "._noise") || strings.Contains(header.Name, ".DS_Store") || strings.Contains(header.Name, "untracked-local") {
					t.Fatalf("local noise in source archive: %s", header.Name)
				}
			}
			selection, err := os.ReadFile(filepath.Join(filepath.Dir(f.dir), "selection"))
			if err != nil {
				t.Fatal(err)
			}
			wantRun := "|auto\n"
			if len(args) > 0 {
				wantRun = "|123\n"
			}
			if !strings.HasSuffix(string(selection), wantRun) {
				t.Fatalf("CI selection: %q", selection)
			}
		})
	}
}

func TestCandidateWithNoVerifiedCIStopsBeforeSigning(t *testing.T) {
	f := candidateFixtureFor(t)
	f.env = append(f.env, "RELEASE_TEST_FAIL_CI=1")
	output, err := f.run(t)
	if err == nil || !strings.Contains(string(output), "no verified CI binary packages") {
		t.Fatalf("candidate: %s\n%v", output, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(f.dir), "gpg.log")); !os.IsNotExist(err) {
		t.Fatal("candidate asked for signing before CI was ready")
	}
	if _, err := os.Stat(filepath.Join(f.dir, "dist/0.3.0")); !os.IsNotExist(err) {
		t.Fatal("failed CI modified the candidate directory")
	}
}
