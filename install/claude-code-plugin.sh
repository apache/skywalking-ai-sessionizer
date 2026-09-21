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
# 1. Downloads the binary package of VERSION through the Apache mirrors, or
#    from archive.apache.org for a version no longer on the download site,
#    with its sha512 from the same Apache site, and stops unless they match.
# 2. Checks that asz-changes starts, and moves it into ~/.local/bin,
#    where the Claude Code installer puts claude. The plugin's hooks run it by
#    name from PATH.
# 3. Installs the file-changes plugin into Claude Code from the marketplace at
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
tar -xzf "$tmp/$pkg" -C "$tmp" asz-changes
"$tmp/asz-changes" version
mv -f "$tmp/asz-changes" "$bin/asz-changes"
say "installed asz-changes into $bin"
if [ "$(command -v asz-changes || true)" != "$bin/asz-changes" ]; then
  say "Put $bin first on your PATH, in your shell profile, then restart Claude Code. The plugin's hooks look for asz-changes there." >&2
fi

market=skywalking-ai-sessionizer
id=file-changes@$market
tag=v$version

# Both lists are JSON, one key per line. A value is read from the line of
# its key within the object that names this plugin or this marketplace.
plugins=$(claude plugin list --json) || fail "claude plugin list failed"
markets=$(claude plugin marketplace list --json) || fail "claude plugin marketplace list failed"
scopes_of() {
  printf '%s\n' "$plugins" | awk -v id="$1" '
    /"id":/ { this = index($0, "\"" id "\"") > 0 }
    this && /"scope":/ { sub(/.*"scope": *"/, ""); sub(/".*/, ""); print }'
}
scopes=$(scopes_of "$id")
# The plugin was named asz-changes until 0.5.0. One installed under that
# name is moved the same way: its data carried across, then uninstalled
# with the data kept, before its marketplace is removed.
was_id=asz-changes@$market
was_scopes=$(scopes_of "$was_id")
declared=$(printf '%s\n' "$markets" | awk -v name="$market" '
  /"name":/ { this = index($0, "\"" name "\"") > 0; if (this) print "yes" }')
ref=$(printf '%s\n' "$markets" | awk -v name="$market" '
  /"name":/ { this = index($0, "\"" name "\"") > 0 }
  this && /"ref":/ { sub(/.*"ref": *"/, ""); sub(/".*/, ""); print }')

for scope in $scopes $was_scopes; do
  [ "$scope" = user ] || fail "the plugin is installed at the $scope scope. Uninstall it there with --keep-data first. Moving the marketplace removes it from every scope, and deletes its data."
done

# A plugin's data directory is named after the plugin, so a rename would
# otherwise start it empty: the settings gone, and any change record asz had
# not collected yet left where nothing will look. Both earlier names are
# carried across once, before the first session, so a rename costs nothing.
#
#   asz-changes-inline   0.3.0, loaded with --plugin-dir
#   asz-changes-$market  0.4.0, before this plugin was named file-changes
#
# The directory is found the way asz finds it: CLAUDE_CONFIG_DIR, else
# XDG_CONFIG_HOME/claude, else ~/.claude. The copy goes to a directory
# beside the final one and is renamed into place, so a copy that stops
# half way leaves nothing that looks finished, and running this again
# copies again rather than moving on to remove the marketplace, which
# deletes the old data.
config=${CLAUDE_CONFIG_DIR:-${XDG_CONFIG_HOME:+$XDG_CONFIG_HOME/claude}}
data=${config:-$HOME/.claude}/plugins/data
here="$data/file-changes-$market"
if [ ! -e "$here" ]; then
  for was in "$data/asz-changes-$market" "$data/asz-changes-inline"; do
    [ -d "$was" ] || continue
    rm -rf "$here.partial"
    mkdir -p "$here.partial"
    # cp, not a tar pipe: a pipe's status is the reader's, so a member the
    # writer could not read left a copy that looked complete.
    cp -Rp "$was/." "$here.partial/" ||
      fail "could not copy $was; nothing was changed. Run this again."
    mv "$here.partial" "$here" || fail "could not move the copy into place; nothing was changed. Run this again."
    say "carried the plugin data over from $(basename "$was")"
    break
  done
fi

if [ "$declared" = yes ] && [ "$ref" = "$tag" ] && [ -n "$scopes" ]; then
  say "the Claude Code plugin is installed at $tag already"
  exit 0
fi
if [ "$declared" = yes ] && [ "$ref" != "$tag" ]; then
  if [ -n "$scopes" ]; then
    claude plugin uninstall "$id" --keep-data || fail "claude plugin uninstall failed, so the marketplace was left as it is"
  fi
  if [ -n "$was_scopes" ]; then
    claude plugin uninstall "$was_id" --keep-data || fail "claude plugin uninstall of $was_id failed, so the marketplace was left as it is"
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
