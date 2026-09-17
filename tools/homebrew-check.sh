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

# Runs the Homebrew formulae of a version through Homebrew before they are
# published. CI runs it on macOS on every change, and a release manager can
# run it on the voted packages.
#
#   tools/homebrew-check.sh VERSION PKG_DIR
#
# PKG_DIR holds the six binary packages of VERSION with their .sha512, as
# tools/install-manifests.sh needs them. The script writes the formulae with
# it into a local tap and runs brew style and brew audit --strict on them as
# written. A version that is not released has no GitHub release to download
# from, so brew install and brew test then run on a copy that downloads the
# package from PKG_DIR. The binaries must report VERSION, which the formulae
# test. Everything installed is uninstalled, and the tap is removed.

set -euo pipefail

me=homebrew-check
fail() { printf '%s: %s\n' "$me" "$*" >&2; exit 1; }
step() { printf '\n== %s\n' "$*"; }

[ "$#" -eq 2 ] || { echo "usage: tools/homebrew-check.sh VERSION PKG_DIR" >&2; exit 2; }
version=$1
[ -d "$2" ] || fail "$2 is not a directory"
pkg_dir=$(cd "$2" && pwd)
tree=$(cd "$(dirname "$0")/.." && pwd)
command -v brew >/dev/null 2>&1 || fail "brew is not on PATH"
command -v perl >/dev/null 2>&1 || fail "perl is not on PATH"

export HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_ANALYTICS=1 HOMEBREW_NO_INSTALL_CLEANUP=1 HOMEBREW_NO_ENV_HINTS=1
tap=aszcheck/formulae
formulae="asz asz-claude-code"
work=$(mktemp -d)
installed=""
cleanup() {
  for f in $installed; do brew uninstall --formula "$f" >/dev/null 2>&1 || true; done
  brew untap "$tap" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

if brew tap | grep -qx "$tap"; then fail "the tap $tap exists already. Remove it with brew untap $tap"; fi
for f in $formulae; do
  if brew list --formula "$f" >/dev/null 2>&1; then fail "$f is installed already, and this would replace it"; fi
done

step "Write the formulae"
bash "$tree/tools/install-manifests.sh" "$version" "$pkg_dir" "$work/manifests" >/dev/null
brew tap-new --no-git "$tap" >/dev/null
dir=$(brew --repository "$tap")/Formula
for f in $formulae; do
  cp "$work/manifests/homebrew/$f.rb" "$dir/"
done
echo "wrote $formulae into $tap"

step "brew style and brew audit, as written"
brew style "$tap"
for f in $formulae; do brew audit --strict --formula "$tap/$f"; done
echo "no offenses"

step "brew install and brew test, from $pkg_dir"
for f in $formulae; do
  # The GitHub release becomes the packages here, the mirror goes, and the
  # version is stated, because Homebrew read it from the release's path.
  URL_FROM="https://github.com/apache/skywalking-ai-sessionizer/releases/download/v$version/" \
  URL_TO="file://$pkg_dir/" VERSION="$version" \
    perl -0pi -e 's/\Q$ENV{URL_FROM}\E/$ENV{URL_TO}/g; s/^ *mirror .*\n//mg; s/^(  license "Apache-2\.0"\n)/  version "$ENV{VERSION}"\n$1/m' "$dir/$f.rb"
  grep -q "file://$pkg_dir/" "$dir/$f.rb" || fail "$f.rb no longer downloads from the GitHub release, which this check replaces"
  brew install --formula "$tap/$f"
  installed="$installed $f"
  brew test "$tap/$f"
done

prefix=$(brew --prefix)
"$prefix/bin/asz" version | grep -qF "$version" || fail "asz on Homebrew's PATH does not report $version"
"$prefix/bin/asz-claude-plugin" version | grep -qF "$version" || fail "asz-claude-plugin on Homebrew's PATH does not report $version"
brew info --formula "$tap/asz-claude-code" | grep -qF "claude plugin marketplace add" ||
  fail "the caveats of asz-claude-code do not give the command that adds the marketplace"
[ ! -e "$prefix/bin/asz-claude-plugin" ] || [ -e "$(brew --prefix asz-claude-code)/bin/asz-claude-plugin" ] ||
  fail "asz-claude-plugin does not come from asz-claude-code"
[ ! -e "$(brew --prefix asz)/bin/asz-claude-plugin" ] || fail "asz installs asz-claude-plugin too, which is the plugin's own formula"

step "Done"
echo "brew install asz and brew install asz-claude-code work for $version"
