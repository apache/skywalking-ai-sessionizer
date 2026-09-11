#!/usr/bin/env bash
#
# Licensed to the Apache Software Foundation (ASF) under one
# or more contributor license agreements.  See the NOTICE file
# distributed with this work for additional information
# regarding copyright ownership.  The ASF licenses this file
# to you under the Apache License, Version 2.0 (the
# "License"); you may not use this file except in compliance
# with the License.  You may obtain a copy of the License at
#
#   http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing,
# software distributed under the License is distributed on an
# "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
# KIND, either express or implied.  See the License for the
# specific language governing permissions and limitations
# under the License.
#

# Writes the manifests that let package managers install a released version.
#
#   tools/install-manifests.sh VERSION PKG_DIR OUT_DIR
#
# PKG_DIR is the directory tools/release.sh publish filled with the voted
# files from dist.apache.org. It must hold the binary packages for macOS and
# Linux on arm64 and amd64, and for Windows on amd64 and arm64, each with the
# .sha512 of the vote beside it. OUT_DIR must be new or empty. Nothing is
# downloaded: every hash comes from the packages in PKG_DIR, and each must
# match its .sha512.
# It writes into OUT_DIR:
#
#   homebrew/skywalking-ai-sessionizer.rb   a formula that installs the macOS and Linux packages
#   scoop/skywalking-ai-sessionizer.json    a Scoop manifest for the Windows packages
#   winget/manifests/a/Apache/SkyWalkingAISessionizer/VERSION/
#                                           the three winget manifests
#   README.md                               where each file goes, how to test and submit it
#
# The manifests are conveniences. The release is the PMC vote, the signed
# packages in the release directory on dist.apache.org, and the announcement.
# Submit a manifest only after its version is on the download site.

set -euo pipefail

me=install-manifests
fail() { printf '%s: %s\n' "$me" "$*" >&2; exit 1; }
usage() { sed -n '/^# Writes the manifests/,/^$/s/^# \{0,1\}//p' "$0"; }

case "${1:-}" in -h|--help) usage; exit 0 ;; esac
[ "$#" -eq 3 ] || { usage >&2; exit 2; }

version=$1
pkg_dir=$2
out_dir=$3

printf '%s' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$' ||
  fail "$version is not of the form MAJOR.MINOR.PATCH"
command -v shasum >/dev/null 2>&1 || fail "shasum is needed to compute the hashes. On Linux it comes with perl."
[ -d "$pkg_dir" ] || fail "$pkg_dir is not a directory"

# A file left from an earlier run, such as the winget directory of another
# version, would be submitted beside these by mistake. So start empty.
if [ -e "$out_dir" ]; then
  [ -d "$out_dir" ] || fail "$out_dir exists and is not a directory"
  [ -z "$(ls -A "$out_dir")" ] || fail "$out_dir is not empty. Give a new or empty directory, so no file from an earlier run is left beside these."
fi

base=apache-skywalking-ai-sessionizer-$version
mac_arm64=$base-bin-darwin-arm64.tgz
mac_amd64=$base-bin-darwin-amd64.tgz
linux_arm64=$base-bin-linux-arm64.tgz
linux_amd64=$base-bin-linux-amd64.tgz
win_x64=$base-bin-windows-amd64.zip
win_arm64=$base-bin-windows-arm64.zip
tgz="$mac_arm64 $mac_amd64 $linux_arm64 $linux_amd64"
packages="$tgz $win_x64 $win_arm64"

missing=""
for f in $packages; do
  [ -f "$pkg_dir/$f" ] || missing="$missing
  $f"
done
[ -z "$missing" ] || fail "$pkg_dir lacks packages the manifests are written from:$missing
Give the directory of the voted packages of $version."

sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
sha512() { shasum -a 512 "$1" | awk '{print $1}'; }
upper() { printf '%s' "$1" | tr '[:lower:]' '[:upper:]'; }

# The manifests must describe the bytes that were voted on. A package built
# again has other bytes, so each one is held to the vote's .sha512 beside
# it, and a package without one is refused. The hash is found wherever it
# sits in the line: shasum --tag writes "SHA512 (name) = hash".
for f in $packages; do
  [ -f "$pkg_dir/$f.sha512" ] || fail "$f has no $f.sha512 beside it. Give the directory tools/release.sh publish filled from dist.apache.org, which holds the voted checksums."
  want=$(grep -Eo '[0-9a-fA-F]{128}' "$pkg_dir/$f.sha512" | tr 'A-F' 'a-f' || true)
  want=${want%%[!0-9a-f]*}
  [ -n "$want" ] || fail "$f.sha512 holds no sha512 of 128 hexadecimal digits"
  [ "$want" = "$(sha512 "$pkg_dir/$f")" ] || fail "$f does not match $f.sha512 beside it. The manifests must describe the voted bytes."
done

mac_arm64_sha256=$(sha256 "$pkg_dir/$mac_arm64")
mac_amd64_sha256=$(sha256 "$pkg_dir/$mac_amd64")
linux_arm64_sha256=$(sha256 "$pkg_dir/$linux_arm64")
linux_amd64_sha256=$(sha256 "$pkg_dir/$linux_amd64")
x64_sha256=$(sha256 "$pkg_dir/$win_x64")
x64_sha512=$(sha512 "$pkg_dir/$win_x64")
arm64_sha256=$(sha256 "$pkg_dir/$win_arm64")
arm64_sha512=$(sha512 "$pkg_dir/$win_arm64")

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# The formula installs these paths from each macOS and Linux package, and
# its test runs both binaries. A package without one of them, or with a
# binary that lost its executable bit, gives a formula that fails for every
# user of that platform. The name is the last field of a tar -tv line, in
# GNU tar and in bsdtar.
for f in $tgz; do
  tar -tvzf "$pkg_dir/$f" > "$tmp/tgz.list" 2>/dev/null || fail "$f is not a gzip tar archive"
  for p in asz claude-code-plugin/bin/asz-claude-plugin LICENSE NOTICE \
           claude-code-plugin/.claude-plugin/plugin.json claude-code-plugin/hooks/hooks.json; do
    awk -v p="$p" '$NF == p { found = 1 } END { exit !found }' "$tmp/tgz.list" ||
      fail "$f does not hold $p at its root, where the Homebrew formula expects it"
  done
  for p in asz claude-code-plugin/bin/asz-claude-plugin; do
    awk -v p="$p" '$NF == p && $1 ~ /^-rwx/ { found = 1 } END { exit !found }' "$tmp/tgz.list" ||
      fail "$f holds $p without its executable bit. The formula installs it as it is, so nobody could run it."
  done
  awk '$NF ~ /^licenses\/./ { found = 1 } END { exit !found }' "$tmp/tgz.list" ||
    fail "$f holds no file under licenses/, which the formula installs beside LICENSE"
done

# Scoop runs asz.exe from the root of the package, and winget names it by
# that relative path. The plugin notes point at claude-code-plugin beside it.
if command -v unzip >/dev/null 2>&1; then
  for f in "$win_x64" "$win_arm64"; do
    unzip -Z1 "$pkg_dir/$f" > "$tmp/zip.list" 2>/dev/null || fail "$f is not a zip archive"
    for p in asz.exe claude-code-plugin/bin/asz-claude-plugin.exe \
             claude-code-plugin/.claude-plugin/plugin.json claude-code-plugin/hooks/hooks.json; do
      grep -Fqx "$p" "$tmp/zip.list" || fail "$f does not hold $p at its root, where the Scoop and winget manifests expect it"
    done
  done
else
  printf '%s: unzip is not installed, so the check that each Windows package holds asz.exe at its root was skipped\n' "$me" >&2
fi

# The winget manifest schema. 1.12.0 is the version microsoft/winget-pkgs'
# own Tools/YamlCreate.ps1 writes, and what most new manifests there use:
# 34 of the 39 installer manifests in its 40 newest commits on 2026-09-11.
# Two used 1.28.0, which winget-cli also publishes, so 1.28.0 is accepted
# too. All three files here validate against 1.12.0.
winget_schema=1.12.0

# Where each manifest downloads from. Package managers keep old versions in
# different ways, so each gets the site that fits it.
#
# The download site, downloads.apache.org, holds only the releases users
# should choose. A version leaves it when a newer one replaces it.
# dlcdn.apache.org is the content delivery network in front of it, and
# closer.lua sends people there. archive.apache.org keeps every version.
# It tells people to take current releases from the mirrors, and it slows
# down and then bans heavy use, with no exception for installers.
#
# Scoop: one url per package. A bucket holds only its newest manifest, and
# checkver with autoupdate moves it to each new version. So the url always
# names a version the download site still holds, and it uses the content
# delivery network rather than the archive, which slows down heavy use. The Apache packages
# in Scoop's main bucket do the same. The checksum is read from
# downloads.apache.org itself, where Apache download pages link it, never
# from a mirror.
#
# winget: one url per package. microsoft/winget-pkgs keeps the manifest of
# every version, and "winget install --version" still installs an old one.
# A url on the download site stops working when the next version replaces
# this one. The archive keeps every version, but winget runs in scripts and
# on shared CI runners, the heavy use the archive slows down and bans. So
# the url is the asset of the GitHub release, an ASF managed platform, whose
# url never moves. tools/release.sh complete attaches each file there only
# after checking it against downloads.apache.org, so it is the voted file,
# and the winget files wait for complete.
#
# Homebrew: one url per platform. The formula lives in a tap. A tap installs
# the version its formula names until someone replaces the formula, and it
# keeps its older formulae in git history. A url on the download site stops
# working when the version leaves it. So the url is the asset of the GitHub
# release, as for winget, and the formula waits for complete too. Its
# mirror is archive.apache.org, which also keeps every version. Homebrew
# tries a mirror only when the url fails, so everyday installs do not reach
# the archive.
#
# A closer.lua url was the other choice, and Homebrew's audit asks for one
# when a url is on apache.org. Two things speak against it. Homebrew asks
# closer.lua for its list of mirrors before it tries the url or any mirror,
# and it stops when www.apache.org does not answer with the list. This was
# read in Homebrew's download code on 2026-09-11. And for a version that has
# left the download site, the list names only dlcdn.apache.org and
# downloads.apache.org, which no longer hold it. closer.lua answered so for
# SkyWalking 8.0.0 on 2026-09-11.
#
# Homebrew reads the version from /releases/download/vVERSION/ in the url.
# So the formula states none, and Homebrew's audit flags a stated version
# that the url already gives. From an Apache url, Homebrew would read the
# version as 64, from the arm64 or amd64 at the end of the file name.

fill() {
  sed -e "s|@VERSION@|$version|g" \
      -e "s|@WINGET_SCHEMA@|$winget_schema|g" \
      -e "s|@DARWIN_ARM64_SHA256@|$mac_arm64_sha256|g" \
      -e "s|@DARWIN_AMD64_SHA256@|$mac_amd64_sha256|g" \
      -e "s|@LINUX_ARM64_SHA256@|$linux_arm64_sha256|g" \
      -e "s|@LINUX_AMD64_SHA256@|$linux_amd64_sha256|g" \
      -e "s|@X64_SHA256@|$(upper "$x64_sha256")|g" \
      -e "s|@ARM64_SHA256@|$(upper "$arm64_sha256")|g" \
      -e "s|@X64_SHA256_LOWER@|$x64_sha256|g" \
      -e "s|@ARM64_SHA256_LOWER@|$arm64_sha256|g" \
      -e "s|@X64_SHA512@|$x64_sha512|g" \
      -e "s|@ARM64_SHA512@|$arm64_sha512|g"
}

winget_dir=winget/manifests/a/Apache/SkyWalkingAISessionizer/$version
mkdir -p "$out_dir/homebrew" "$out_dir/scoop" "$out_dir/$winget_dir"

# ------------------------------------------------------------------ Homebrew
fill > "$out_dir/homebrew/skywalking-ai-sessionizer.rb" <<'EOF'
# Written by tools/install-manifests.sh in apache/skywalking-ai-sessionizer
# from the voted binary packages of @VERSION@.
class SkywalkingAiSessionizer < Formula
  desc "Conversation-level observability for long-lived AI agents"
  homepage "https://github.com/apache/skywalking-ai-sessionizer"
  license "Apache-2.0"

  # Each package comes from the GitHub release of v@VERSION@, which carries
  # the voted files. Its URL keeps working after a newer version replaces
  # this one on the download site. The mirror, archive.apache.org, keeps
  # every version too, and Homebrew tries it only when GitHub fails.
  # Homebrew reads the version from the release tag in each URL.
  on_macos do
    on_arm do
      url "https://github.com/apache/skywalking-ai-sessionizer/releases/download/v@VERSION@/apache-skywalking-ai-sessionizer-@VERSION@-bin-darwin-arm64.tgz"
      mirror "https://archive.apache.org/dist/skywalking/ai-sessionizer/@VERSION@/apache-skywalking-ai-sessionizer-@VERSION@-bin-darwin-arm64.tgz"
      sha256 "@DARWIN_ARM64_SHA256@"
    end
    on_intel do
      url "https://github.com/apache/skywalking-ai-sessionizer/releases/download/v@VERSION@/apache-skywalking-ai-sessionizer-@VERSION@-bin-darwin-amd64.tgz"
      mirror "https://archive.apache.org/dist/skywalking/ai-sessionizer/@VERSION@/apache-skywalking-ai-sessionizer-@VERSION@-bin-darwin-amd64.tgz"
      sha256 "@DARWIN_AMD64_SHA256@"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/apache/skywalking-ai-sessionizer/releases/download/v@VERSION@/apache-skywalking-ai-sessionizer-@VERSION@-bin-linux-arm64.tgz"
      mirror "https://archive.apache.org/dist/skywalking/ai-sessionizer/@VERSION@/apache-skywalking-ai-sessionizer-@VERSION@-bin-linux-arm64.tgz"
      sha256 "@LINUX_ARM64_SHA256@"
    end
    on_intel do
      url "https://github.com/apache/skywalking-ai-sessionizer/releases/download/v@VERSION@/apache-skywalking-ai-sessionizer-@VERSION@-bin-linux-amd64.tgz"
      mirror "https://archive.apache.org/dist/skywalking/ai-sessionizer/@VERSION@/apache-skywalking-ai-sessionizer-@VERSION@-bin-linux-amd64.tgz"
      sha256 "@LINUX_AMD64_SHA256@"
    end
  end

  def install
    bin.install "asz"
    # The plugin stays whole, because hooks/hooks.json runs the binary under
    # its own bin/.
    libexec.install "claude-code-plugin"
    # Homebrew keeps a formula's license files at the root of its prefix. It
    # moves LICENSE and NOTICE there by itself, but not a directory, so
    # licenses/ goes with them here. It holds the license of every module
    # and font built into the binaries.
    prefix.install "LICENSE", "NOTICE", "licenses"
  end

  def caveats
    # The opt path stays the same across upgrades, so the directory given to
    # Claude Code keeps working after brew upgrade.
    <<~EOS
      The Claude Code plugin, which records which files each tool call
      changed, is installed in:
        #{opt_libexec}/claude-code-plugin

      Point Claude Code at that directory:
        claude --plugin-dir #{opt_libexec}/claude-code-plugin

      or add it to your Claude Code settings the way the Claude Code
      documentation describes for a local plugin. See:
        https://github.com/apache/skywalking-ai-sessionizer/blob/v#{version}/docs/en/setup/claude-code-plugin.md
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/asz version")
    # glossary needs no configuration and no input, and its table names
    # agent.call, so the program does real work, not only print its version.
    assert_match "agent.call", shell_output("#{bin}/asz glossary")

    plugin = libexec/"claude-code-plugin"
    assert_path_exists plugin/".claude-plugin/plugin.json"
    assert_path_exists plugin/"hooks/hooks.json"
    assert_match version.to_s, shell_output("#{plugin}/bin/asz-claude-plugin version")
    # Claude Code names the plugin's data directory in CLAUDE_PLUGIN_DATA.
    # status reads the settings there and compiles the exclusion rules. With
    # no settings file it takes the defaults, the set named standard-v1.
    ENV["CLAUDE_PLUGIN_DATA"] = (testpath/"plugin-data").to_s
    assert_match "standard-v1", shell_output("#{plugin}/bin/asz-claude-plugin status")
  end
end
EOF

# --------------------------------------------------------------------- Scoop
fill > "$out_dir/scoop/skywalking-ai-sessionizer.json" <<'EOF'
{
    "##": [
        "Written by tools/install-manifests.sh in apache/skywalking-ai-sessionizer from the voted packages of @VERSION@.",
        "The packages come from dlcdn.apache.org, the content delivery network in front of the download site. A bucket holds only the newest version, and the download site holds it too. The archive keeps every version, but it slows down and then bans heavy use.",
        "The checksums come from downloads.apache.org itself, never from a mirror."
    ],
    "version": "@VERSION@",
    "description": "Conversation-level observability for long-lived AI agents",
    "homepage": "https://github.com/apache/skywalking-ai-sessionizer",
    "license": "Apache-2.0",
    "architecture": {
        "64bit": {
            "url": "https://dlcdn.apache.org/skywalking/ai-sessionizer/@VERSION@/apache-skywalking-ai-sessionizer-@VERSION@-bin-windows-amd64.zip",
            "hash": "sha512:@X64_SHA512@"
        },
        "arm64": {
            "url": "https://dlcdn.apache.org/skywalking/ai-sessionizer/@VERSION@/apache-skywalking-ai-sessionizer-@VERSION@-bin-windows-arm64.zip",
            "hash": "sha512:@ARM64_SHA512@"
        }
    },
    "bin": "asz.exe",
    "notes": [
        "The Claude Code plugin is in $dir\\claude-code-plugin.",
        "Point Claude Code at it with: claude --plugin-dir \"$dir\\claude-code-plugin\"",
        "Claude Code has not yet run the plugin's hooks on Windows, so the command line in hooks/hooks.json has not run there. CI unpacks each Windows package on a Windows runner of its processor, outside Claude Code. It runs the packaged plugin with a SessionStart, a PreToolUse, a PostToolUse and a SessionEnd event on standard input. See https://github.com/apache/skywalking-ai-sessionizer/blob/main/docs/en/setup/claude-code-plugin.md"
    ],
    "checkver": {
        "url": "https://downloads.apache.org/skywalking/ai-sessionizer/?C=N;O=D;V=1",
        "regex": "href=\"(\\d+\\.\\d+\\.\\d+)/\""
    },
    "autoupdate": {
        "architecture": {
            "64bit": {
                "url": "https://dlcdn.apache.org/skywalking/ai-sessionizer/$version/apache-skywalking-ai-sessionizer-$version-bin-windows-amd64.zip"
            },
            "arm64": {
                "url": "https://dlcdn.apache.org/skywalking/ai-sessionizer/$version/apache-skywalking-ai-sessionizer-$version-bin-windows-arm64.zip"
            }
        },
        "hash": {
            "url": "https://downloads.apache.org/skywalking/ai-sessionizer/$version/$basename.sha512"
        }
    }
}
EOF

# -------------------------------------------------------------------- winget
fill > "$out_dir/$winget_dir/Apache.SkyWalkingAISessionizer.yaml" <<'EOF'
# Written by tools/install-manifests.sh in apache/skywalking-ai-sessionizer from the voted packages of @VERSION@.
# yaml-language-server: $schema=https://aka.ms/winget-manifest.version.@WINGET_SCHEMA@.schema.json

PackageIdentifier: Apache.SkyWalkingAISessionizer
PackageVersion: @VERSION@
DefaultLocale: en-US
ManifestType: version
ManifestVersion: @WINGET_SCHEMA@
EOF

fill > "$out_dir/$winget_dir/Apache.SkyWalkingAISessionizer.installer.yaml" <<'EOF'
# Written by tools/install-manifests.sh in apache/skywalking-ai-sessionizer from the voted packages of @VERSION@.
# The packages come from the GitHub release of v@VERSION@, which carries the voted files. Its URL
# keeps working after a newer version replaces this one on the download site.
# yaml-language-server: $schema=https://aka.ms/winget-manifest.installer.@WINGET_SCHEMA@.schema.json

PackageIdentifier: Apache.SkyWalkingAISessionizer
PackageVersion: @VERSION@
InstallerType: zip
NestedInstallerType: portable
NestedInstallerFiles:
- RelativeFilePath: asz.exe
  PortableCommandAlias: asz
Commands:
- asz
Installers:
- Architecture: x64
  InstallerUrl: https://github.com/apache/skywalking-ai-sessionizer/releases/download/v@VERSION@/apache-skywalking-ai-sessionizer-@VERSION@-bin-windows-amd64.zip
  InstallerSha256: @X64_SHA256@
- Architecture: arm64
  InstallerUrl: https://github.com/apache/skywalking-ai-sessionizer/releases/download/v@VERSION@/apache-skywalking-ai-sessionizer-@VERSION@-bin-windows-arm64.zip
  InstallerSha256: @ARM64_SHA256@
ManifestType: installer
ManifestVersion: @WINGET_SCHEMA@
EOF

fill > "$out_dir/$winget_dir/Apache.SkyWalkingAISessionizer.locale.en-US.yaml" <<'EOF'
# Written by tools/install-manifests.sh in apache/skywalking-ai-sessionizer from the voted packages of @VERSION@.
# yaml-language-server: $schema=https://aka.ms/winget-manifest.defaultLocale.@WINGET_SCHEMA@.schema.json

PackageIdentifier: Apache.SkyWalkingAISessionizer
PackageVersion: @VERSION@
PackageLocale: en-US
Publisher: The Apache Software Foundation
PublisherUrl: https://www.apache.org/
PublisherSupportUrl: https://github.com/apache/skywalking/issues
Author: Apache SkyWalking
PackageName: Apache SkyWalking AI Sessionizer
PackageUrl: https://github.com/apache/skywalking-ai-sessionizer
License: Apache-2.0
LicenseUrl: https://www.apache.org/licenses/LICENSE-2.0
ShortDescription: Conversation-level observability for long-lived AI agents.
Description: Apache SkyWalking AI Sessionizer, asz, assembles fragmented agent telemetry, such as transcripts, subagent streams and workflow journals, into one durable conversation structure.
Moniker: asz
Tags:
- ai-agents
- claude-code
- observability
- skywalking
ReleaseNotesUrl: https://github.com/apache/skywalking-ai-sessionizer/blob/v@VERSION@/docs/en/changes/changes.md
InstallationNotes: The Claude Code plugin is the claude-code-plugin folder beside asz.exe in the package's install folder. Claude Code has not yet run the plugin's hooks on Windows, so the command line in hooks/hooks.json has not run there. CI unpacks each Windows package on a Windows runner of its processor, outside Claude Code. It runs the packaged plugin with a SessionStart, a PreToolUse, a PostToolUse and a SessionEnd event on standard input. See https://github.com/apache/skywalking-ai-sessionizer/blob/v@VERSION@/docs/en/setup/claude-code-plugin.md
Documentations:
- DocumentLabel: Documentation
  DocumentUrl: https://github.com/apache/skywalking-ai-sessionizer/blob/v@VERSION@/docs/README.md
ManifestType: defaultLocale
ManifestVersion: @WINGET_SCHEMA@
EOF

# -------------------------------------------------------------------- README
fill > "$out_dir/README.md" <<'EOF'
# Package manager manifests for Apache SkyWalking AI Sessionizer @VERSION@

`tools/install-manifests.sh` wrote these files from the voted packages of @VERSION@. They let
Homebrew, Scoop and winget install that version. They are conveniences. The release itself is
the PMC vote, the signed packages in the release directory on dist.apache.org, and the
announcement.

**Before the first submission to each package manager, the PMC agrees to it on
dev@skywalking.apache.org.** A manifest distributes the project under the ASF's name in a new
place. A tap or a bucket under github.com/apache is a new repository, which the PMC asks INFRA to
create.

**Submit a file only after @VERSION@ is on the download site,**
<https://downloads.apache.org/skywalking/ai-sessionizer/@VERSION@/>. The Homebrew formula and the
winget files also wait for the GitHub release of v@VERSION@, which `tools/release.sh complete`
creates and fills: <https://github.com/apache/skywalking-ai-sessionizer/releases/tag/v@VERSION@>.
The Homebrew formula names the archive,
<https://archive.apache.org/dist/skywalking/ai-sessionizer/@VERSION@/>, as its mirror, and the
archive can show a new version later than the download site does. A manifest that names a file
that is not there yet fails its review, and it fails for everyone who installs it.

| File | Where it goes | What it downloads |
| --- | --- | --- |
| `homebrew/skywalking-ai-sessionizer.rb` | `Formula/` in a tap | the macOS or Linux package, from the GitHub release, or else from archive.apache.org |
| `scoop/skywalking-ai-sessionizer.json` | `bucket/` in a Scoop bucket | the Windows package, from dlcdn.apache.org |
| `winget/manifests/a/Apache/SkyWalkingAISessionizer/@VERSION@/` | the same path in microsoft/winget-pkgs | the Windows package, from the GitHub release |

## Why the download URLs differ

The download site keeps only the releases users should choose, and a version leaves it when a
newer one replaces it. dlcdn.apache.org is the content delivery network in front of it. The
archive keeps every version. It tells people to take current releases from the mirrors, and it
slows down and then bans heavy use.

- A tap installs the version its formula names until someone replaces the formula, and it keeps
  its older formulae in git history. So the formula downloads from the GitHub release, whose URL
  never moves, and names the archive as its mirror. Homebrew tries the mirror only when GitHub
  fails, so everyday installs do not reach the archive.
- A Scoop bucket holds only its newest manifest. Its URL names a version the download site still
  holds, as long as the bucket moves on before the old version is removed.
- microsoft/winget-pkgs keeps the manifest of every version, and `winget install --version` can
  still ask for an old one. winget also runs in scripts and on shared CI machines, the heavy use
  the archive stops. The GitHub release keeps its URL, and `tools/release.sh complete` uploads
  each file there only after checking it against the download site.

## Check the download sites first

Each command must succeed. A command that prints a hash must print the one in the comment above it.

Check downloads.apache.org first, because the other sites copy it. Fetch a dlcdn.apache.org URL
only once downloads.apache.org serves the file. If dlcdn.apache.org answers 404 while
downloads.apache.org has the file, dlcdn.apache.org is serving a 404 it cached from an earlier
request. Wait and try again, and do not change the manifest.

```sh
# The download site, where Scoop reads its checksums.
# x64: @X64_SHA512@
curl -fsSL https://downloads.apache.org/skywalking/ai-sessionizer/@VERSION@/apache-skywalking-ai-sessionizer-@VERSION@-bin-windows-amd64.zip.sha512
# arm64: @ARM64_SHA512@
curl -fsSL https://downloads.apache.org/skywalking/ai-sessionizer/@VERSION@/apache-skywalking-ai-sessionizer-@VERSION@-bin-windows-arm64.zip.sha512

# Scoop, x64: @X64_SHA512@
curl -fsSL https://dlcdn.apache.org/skywalking/ai-sessionizer/@VERSION@/apache-skywalking-ai-sessionizer-@VERSION@-bin-windows-amd64.zip | shasum -a 512

# Scoop, arm64: @ARM64_SHA512@
curl -fsSL https://dlcdn.apache.org/skywalking/ai-sessionizer/@VERSION@/apache-skywalking-ai-sessionizer-@VERSION@-bin-windows-arm64.zip | shasum -a 512

# Homebrew, from the GitHub release once it carries the packages, and from the
# mirror. Both commands under a comment print its hash.
# macOS arm64: @DARWIN_ARM64_SHA256@
curl -fsSL https://github.com/apache/skywalking-ai-sessionizer/releases/download/v@VERSION@/apache-skywalking-ai-sessionizer-@VERSION@-bin-darwin-arm64.tgz | shasum -a 256
curl -fsSL https://archive.apache.org/dist/skywalking/ai-sessionizer/@VERSION@/apache-skywalking-ai-sessionizer-@VERSION@-bin-darwin-arm64.tgz | shasum -a 256

# macOS amd64: @DARWIN_AMD64_SHA256@
curl -fsSL https://github.com/apache/skywalking-ai-sessionizer/releases/download/v@VERSION@/apache-skywalking-ai-sessionizer-@VERSION@-bin-darwin-amd64.tgz | shasum -a 256
curl -fsSL https://archive.apache.org/dist/skywalking/ai-sessionizer/@VERSION@/apache-skywalking-ai-sessionizer-@VERSION@-bin-darwin-amd64.tgz | shasum -a 256

# Linux arm64: @LINUX_ARM64_SHA256@
curl -fsSL https://github.com/apache/skywalking-ai-sessionizer/releases/download/v@VERSION@/apache-skywalking-ai-sessionizer-@VERSION@-bin-linux-arm64.tgz | shasum -a 256
curl -fsSL https://archive.apache.org/dist/skywalking/ai-sessionizer/@VERSION@/apache-skywalking-ai-sessionizer-@VERSION@-bin-linux-arm64.tgz | shasum -a 256

# Linux amd64: @LINUX_AMD64_SHA256@
curl -fsSL https://github.com/apache/skywalking-ai-sessionizer/releases/download/v@VERSION@/apache-skywalking-ai-sessionizer-@VERSION@-bin-linux-amd64.tgz | shasum -a 256
curl -fsSL https://archive.apache.org/dist/skywalking/ai-sessionizer/@VERSION@/apache-skywalking-ai-sessionizer-@VERSION@-bin-linux-amd64.tgz | shasum -a 256

# winget, x64, once the GitHub release carries the packages: @X64_SHA256_LOWER@
curl -fsSL https://github.com/apache/skywalking-ai-sessionizer/releases/download/v@VERSION@/apache-skywalking-ai-sessionizer-@VERSION@-bin-windows-amd64.zip | shasum -a 256

# winget, arm64: @ARM64_SHA256_LOWER@
curl -fsSL https://github.com/apache/skywalking-ai-sessionizer/releases/download/v@VERSION@/apache-skywalking-ai-sessionizer-@VERSION@-bin-windows-arm64.zip | shasum -a 256
```

## Homebrew

Test the formula in a local tap. It downloads the package for your machine from the GitHub
release, so this also checks that URL and its hash. The commands above check all four. `brew test`
runs `asz version`, `asz glossary`, and the plugin's `version` and `status`.

```sh
brew tap-new --no-git "$USER/asz-test"
cp homebrew/skywalking-ai-sessionizer.rb "$(brew --repository "$USER/asz-test")/Formula/"
brew install "$USER/asz-test/skywalking-ai-sessionizer"
brew test "$USER/asz-test/skywalking-ai-sessionizer"
brew audit --strict --online --formula "$USER/asz-test/skywalking-ai-sessionizer"
brew uninstall skywalking-ai-sessionizer
brew untap "$USER/asz-test"
```

Submit it by committing the file under `Formula/` in a tap, a GitHub repository named
`homebrew-<name>`. People then run `brew install <owner>/<name>/skywalking-ai-sessionizer`.
Homebrew asks people to trust a formula from a tap that is not its own, and installing by that
full name trusts this one formula only.

Homebrew/homebrew-core does not take this formula. A formula there must build from source, or
install output that runs on every platform, and this one installs a binary built for each
platform. For a later version, run the script again on that version's voted packages and replace
the file.

## Scoop

Test the manifest on Windows, in PowerShell:

```powershell
scoop install .\scoop\skywalking-ai-sessionizer.json
asz version
scoop uninstall skywalking-ai-sessionizer
```

Submit it by committing the file under `bucket/` in a bucket repository: one the project keeps,
or ScoopInstaller/Main by pull request, which has its own rules for what it takes. From the
bucket repository, Scoop's own scripts check the version lookup and the hashes:

```powershell
& "$(scoop prefix scoop)\bin\checkver.ps1" -App skywalking-ai-sessionizer -Dir .\bucket
& "$(scoop prefix scoop)\bin\checkhashes.ps1" -App skywalking-ai-sessionizer -Dir .\bucket
```

For a later version, `checkver.ps1` with `-Update` rewrites the manifest from the download site.
**Move the bucket to the new version before the previous version is removed from the release
directory.** The manifest names the previous version on dlcdn.apache.org, and that URL stops
working when the version is removed.

## winget

Copy the `winget/manifests` directory over the `manifests` directory of a fork of
microsoft/winget-pkgs, so the files land in
`manifests/a/Apache/SkyWalkingAISessionizer/@VERSION@/`. Validate and test them on Windows:

```powershell
$dir = ".\manifests\a\Apache\SkyWalkingAISessionizer\@VERSION@"
winget validate --manifest $dir
winget settings --enable LocalManifestFiles   # once, as administrator
winget install --manifest $dir
asz version
winget uninstall --id Apache.SkyWalkingAISessionizer
```

`.\Tools\SandboxTest.ps1 $dir`, run from the winget-pkgs checkout, runs the same install in
Windows Sandbox, so nothing stays on your machine.

Submit a pull request to microsoft/winget-pkgs that adds only this directory, once the GitHub
release of v@VERSION@ carries both Windows packages. Its pipeline validates the manifests and
downloads both packages. For a later version, run the script again and submit the new directory.
The directory for @VERSION@ stays, and keeps working, because it downloads from the GitHub
release.
EOF

# Every placeholder must have been filled. One left over would be submitted
# as it is.
if grep -rn '@[A-Z0-9_]*@' "$out_dir" >&2; then fail "a placeholder above was not filled"; fi

cat <<DONE
wrote the manifests for $version into $out_dir:
  homebrew/skywalking-ai-sessionizer.rb   darwin and linux, arm64 and amd64, sha256
  scoop/skywalking-ai-sessionizer.json    windows-amd64 and windows-arm64, sha512
  $winget_dir/   schema $winget_schema, sha256
  README.md
Submit each only after $version is on the download site. The Homebrew formula and
the winget files also wait for the GitHub release. $out_dir/README.md says how.
DONE
