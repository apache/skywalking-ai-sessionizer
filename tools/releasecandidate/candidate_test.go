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
	"strings"
	"testing"
)

type candidateFixture struct {
	dir, binaries, commands, archive string
	env                              []string
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
	for _, name := range []string{"svn", "gpgconf"} {
		write(filepath.Join(f.commands, name), "#!/bin/sh\nexit 0\n", 0o755)
	}
	write(filepath.Join(f.commands, "gh"), "#!/bin/sh\nexit 99\n", 0o755)
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
	for _, name := range []string{"asz", "claude-code-plugin/bin/asz-claude-plugin", "claude-code-plugin/.claude-plugin/plugin.json", "claude-code-plugin/hooks/hooks.json", "LICENSE", "NOTICE", "licenses/license.txt"} {
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
	f.env = append(os.Environ(),
		"PATH="+f.commands+string(os.PathListSeparator)+os.Getenv("PATH"),
		"RELEASE_TEST_GIT="+realGit, "RELEASE_TEST_COMMIT="+commit,
		"RELEASE_TEST_BINARIES="+f.binaries,
		"RELEASE_TEST_LOG="+filepath.Join(base, "gpg.log"),
		"RELEASE_TEST_SELECTION="+filepath.Join(base, "selection"),
		"GPG_USER=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	)
	return f
}

func (f *candidateFixture) run(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{"tools/release.sh", "candidate", "0.3.0", "--no-upload"}, args...)...)
	cmd.Dir, cmd.Env = f.dir, f.env
	return cmd.CombinedOutput()
}

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
