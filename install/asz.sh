#!/bin/sh
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

# Installs asz, the collector of Apache SkyWalking AI Sessionizer, from the
# binary package of a released version, on macOS or Linux. Run it again with
# another version to install that one over it. The Claude Code plugin is
# installed on its own, by install/claude-code-plugin.sh.
#
#   curl -fsSL "https://raw.githubusercontent.com/apache/skywalking-ai-sessionizer/v$VERSION/install/asz.sh" | sh -s -- "$VERSION"
#
# It downloads the package through the Apache mirrors, or from
# archive.apache.org for a version no longer on the download site, with its
# sha512 from the same Apache site, stops unless the two match, checks that
# asz starts, and moves it into ~/.local/bin. docs/en/setup/install.md says why.

set -eu

me=asz-install
say() { printf '%s: %s\n' "$me" "$*"; }
fail() { printf '%s: %s\n' "$me" "$*" >&2; exit 1; }

version=${1:-}
[ -n "$version" ] || fail "give the version to install, one https://skywalking.apache.org/downloads/ lists as released"
case "$version" in *[!0-9A-Za-z.-]*) fail "$version is not a version" ;; esac
[ "$#" -le 1 ] || fail "give the version alone"

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) fail "there is no package for $(uname -s). docs/en/setup/install.md lists the other ways to install" ;;
esac
case "$(uname -m)" in
  arm64 | aarch64) arch=arm64 ;;
  x86_64 | amd64) arch=amd64 ;;
  *) fail "there is no package for $(uname -m)" ;;
esac
# A shell under Rosetta reports x86_64 on Apple silicon. The Claude Code
# installer takes the arm64 build there, and so does this.
if [ "$os" = darwin ] && [ "$arch" = amd64 ] && [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || true)" = 1 ]; then
  arch=arm64
fi

pkg=apache-skywalking-ai-sessionizer-$version-bin-$os-$arch.tgz
bin=$HOME/.local/bin
mkdir -p "$bin"
# The download waits in a directory inside bin, so the last step is a
# rename, which replaces a running binary too.
tmp=$(mktemp -d "$bin/.asz-install.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
trap 'exit 1' HUP INT TERM

# The download site keeps only the newest release. A version there comes
# through the mirrors. Any other comes from the archive, which keeps every
# version but slows down heavy use, so it is not asked first.
site=https://downloads.apache.org/skywalking/ai-sessionizer/$version
archive=https://archive.apache.org/dist/skywalking/ai-sessionizer/$version
# Only a 404 sends it to the archive. A download site that cannot be reached
# says nothing about the version, and the reason is reported instead.
code=$(curl -sSL -o "$tmp/$pkg.sha512" -w '%{http_code}' "$site/$pkg.sha512") ||
  fail "cannot reach downloads.apache.org. The error is above."
case "$code" in
  200)
    from="https://www.apache.org/dyn/closer.lua?path=skywalking/ai-sessionizer/$version/$pkg&action=download"
    say "downloading $pkg through the Apache mirrors" ;;
  404)
    curl -fsSL -o "$tmp/$pkg.sha512" "$archive/$pkg.sha512" ||
      fail "neither downloads.apache.org nor archive.apache.org has $pkg. Is $version released, and is there a package for $os/$arch?"
    from=$archive/$pkg
    say "downloading $pkg from archive.apache.org, as the download site holds only the newest release" ;;
  *) fail "downloads.apache.org answered $code for $pkg.sha512" ;;
esac
curl -fsSL -o "$tmp/$pkg" "$from" || fail "cannot download $pkg"
want=$(cut -d ' ' -f 1 "$tmp/$pkg.sha512")
if command -v sha512sum >/dev/null 2>&1; then got=$(sha512sum "$tmp/$pkg"); else got=$(shasum -a 512 "$tmp/$pkg"); fi
[ "${#want}" -eq 128 ] && [ "${got%% *}" = "$want" ] || fail "the sha512 of $pkg does not match $pkg.sha512, so nothing was installed"
# From 0.5.0 the archive holds asz and asz-changes, the change recorder,
# and both are installed, as Homebrew and apt install both: a recorder of
# one version beside asz of another is what one package is meant to
# prevent. An older version's archive holds the recorder under its old
# name, and this installs asz alone from it.
tar -xzf "$tmp/$pkg" -C "$tmp" asz
"$tmp/asz" version
if tar -tzf "$tmp/$pkg" | grep -qx asz-changes; then
  tar -xzf "$tmp/$pkg" -C "$tmp" asz-changes
  "$tmp/asz-changes" version
  mv -f "$tmp/asz" "$bin/asz"
  mv -f "$tmp/asz-changes" "$bin/asz-changes"
  say "installed asz and asz-changes into $bin"
else
  mv -f "$tmp/asz" "$bin/asz"
  say "installed asz into $bin; $version has no asz-changes"
fi
if [ "$(command -v asz || true)" != "$bin/asz" ]; then
  say "Put $bin first on your PATH, in your shell profile, so asz there is the one that runs." >&2
fi
