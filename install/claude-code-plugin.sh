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

# Installs the asz Claude Code plugin of a released version on macOS or Linux,
# and moves an installed one to that version. asz itself is installed on its
# own, by install/asz.sh.
#
#   curl -fsSL "https://raw.githubusercontent.com/apache/skywalking-ai-sessionizer/v$VERSION/install/claude-code-plugin.sh" | sh -s -- "$VERSION"
#
# 1. Downloads the binary package of VERSION through the Apache mirror
#    selector, and its sha512 from downloads.apache.org itself, and stops
#    unless the two match.
# 2. Checks that asz-claude-plugin starts, and moves it into ~/.local/bin,
#    where the Claude Code installer puts claude. The plugin's hooks run it by
#    name from PATH.
# 3. Installs the asz-changes plugin into Claude Code from the marketplace at
#    the tag of VERSION. A plugin at another tag is uninstalled with its data
#    kept first, because removing the marketplace would delete the records
#    asz has not collected yet.
#
# docs/en/setup/claude-code-plugin.md says why.

set -eu

me=asz-claude-code-plugin-install
say() { printf '%s: %s\n' "$me" "$*"; }
fail() { printf '%s: %s\n' "$me" "$*" >&2; exit 1; }

version=${1:-}
[ -n "$version" ] || fail "give the version to install, one https://skywalking.apache.org/downloads/ lists as released"
case "$version" in *[!0-9A-Za-z.-]*) fail "$version is not a version" ;; esac
[ "$#" -le 1 ] || fail "give the version alone"
command -v claude >/dev/null 2>&1 || fail "claude is not on PATH. Install Claude Code first, then run this again."

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

say "downloading $pkg"
curl -fsSL -o "$tmp/$pkg" "https://www.apache.org/dyn/closer.lua?path=skywalking/ai-sessionizer/$version/$pkg&action=download" ||
  fail "cannot download $pkg. Is $version released, and is there a package for $os/$arch?"
curl -fsSL -o "$tmp/$pkg.sha512" "https://downloads.apache.org/skywalking/ai-sessionizer/$version/$pkg.sha512" ||
  fail "cannot download $pkg.sha512 from downloads.apache.org"
want=$(cut -d ' ' -f 1 "$tmp/$pkg.sha512")
if command -v sha512sum >/dev/null 2>&1; then got=$(sha512sum "$tmp/$pkg"); else got=$(shasum -a 512 "$tmp/$pkg"); fi
[ "${#want}" -eq 128 ] && [ "${got%% *}" = "$want" ] || fail "the sha512 of $pkg does not match $pkg.sha512, so nothing was installed"
tar -xzf "$tmp/$pkg" -C "$tmp" asz-claude-plugin
"$tmp/asz-claude-plugin" version
mv -f "$tmp/asz-claude-plugin" "$bin/asz-claude-plugin"
say "installed asz-claude-plugin into $bin"
if [ "$(command -v asz-claude-plugin || true)" != "$bin/asz-claude-plugin" ]; then
  say "Put $bin first on your PATH, in your shell profile, then restart Claude Code. The plugin's hooks look for asz-claude-plugin there." >&2
fi

market=skywalking-ai-sessionizer
id=asz-changes@$market
tag=v$version

# Both lists are JSON, one key per line. A value is read from the line of
# its key within the object that names this plugin or this marketplace.
plugins=$(claude plugin list --json) || fail "claude plugin list failed"
markets=$(claude plugin marketplace list --json) || fail "claude plugin marketplace list failed"
scopes=$(printf '%s\n' "$plugins" | awk -v id="$id" '
  /"id":/ { this = index($0, "\"" id "\"") > 0 }
  this && /"scope":/ { sub(/.*"scope": *"/, ""); sub(/".*/, ""); print }')
declared=$(printf '%s\n' "$markets" | awk -v name="$market" '
  /"name":/ { this = index($0, "\"" name "\"") > 0; if (this) print "yes" }')
ref=$(printf '%s\n' "$markets" | awk -v name="$market" '
  /"name":/ { this = index($0, "\"" name "\"") > 0 }
  this && /"ref":/ { sub(/.*"ref": *"/, ""); sub(/".*/, ""); print }')

for scope in $scopes; do
  [ "$scope" = user ] || fail "asz-changes is installed at the $scope scope. Uninstall it there with --keep-data first. Moving the marketplace removes it from every scope, and deletes its data."
done

# 0.3.0 ran the plugin with --plugin-dir, whose data directory is
# asz-changes-inline. Its settings move to the installed plugin's directory
# once, before the first session.
data=${CLAUDE_CONFIG_DIR:-$HOME/.claude}/plugins/data
if [ -f "$data/asz-changes-inline/settings.yaml" ] && [ ! -e "$data/asz-changes-$market/settings.yaml" ]; then
  mkdir -p "$data/asz-changes-$market"
  cp "$data/asz-changes-inline/settings.yaml" "$data/asz-changes-$market/"
  say "copied the settings of the plugin loaded with --plugin-dir"
fi

if [ "$declared" = yes ] && [ "$ref" = "$tag" ] && [ -n "$scopes" ]; then
  say "the Claude Code plugin is installed at $tag already"
  exit 0
fi
if [ "$declared" = yes ] && [ "$ref" != "$tag" ]; then
  if [ -n "$scopes" ]; then
    claude plugin uninstall "$id" --keep-data || fail "claude plugin uninstall failed, so the marketplace was left as it is"
  fi
  claude plugin marketplace remove "$market" || fail "claude plugin marketplace remove failed. The plugin's data is kept. Run this again."
  declared=
fi
if [ "$declared" != yes ]; then
  claude plugin marketplace add "https://github.com/apache/skywalking-ai-sessionizer.git#$tag" --sparse .claude-plugin plugins/claude-code/plugin ||
    fail "claude plugin marketplace add failed. The plugin's data is kept. Run this again."
fi
claude plugin install "$id" || fail "claude plugin install failed. The plugin's data is kept. Run this again."
say "installed the Claude Code plugin at $tag. Restart Claude Code to load it."
