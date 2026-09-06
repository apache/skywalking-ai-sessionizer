#!/bin/sh
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

# The conversation renderer asz view embeds is Horizon's
# @skywalking-horizon-ui/conversation-view, built from the Horizon commit
# named in internal/view/conversation-view/HORIZON_COMMIT and committed here,
# so the two hosts draw a conversation identically and asz builds without
# a JavaScript toolchain.
#
#   tools/conversation-view.sh update [COMMIT]   build at COMMIT (default: the pin) and copy the output in
#   tools/conversation-view.sh check             build at the pin into a temporary directory and compare
#
# HORIZON_DIR names a local clone to build in; otherwise the repository is
# cloned into a temporary directory. Needs node 24 and pnpm.
set -eu
cd "$(dirname "$0")/.."
here=$(pwd)
dest=internal/view/conversation-view
pin_file=$dest/HORIZON_COMMIT
repo=https://github.com/apache/skywalking-horizon-ui.git
pkg=packages/conversation-view

cmd="${1:-}"
case "$cmd" in
  update|check) ;;
  *) sed -n '18,29p' "$0"; exit 2 ;;
esac
commit="${2:-}"
if [ -z "$commit" ]; then
  [ -f "$pin_file" ] || { echo "no pin in $pin_file and no commit given" >&2; exit 2; }
  commit=$(sed -n 's/^commit //p' "$pin_file")
fi
[ -n "$commit" ] || { echo "no commit" >&2; exit 2; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# The source: a local clone at the commit, or a fresh clone of just that commit.
if [ -n "${HORIZON_DIR:-}" ]; then
  src=$HORIZON_DIR
  git -C "$src" cat-file -e "$commit^{commit}" 2>/dev/null || git -C "$src" fetch -q origin "$commit"
  [ "$(git -C "$src" rev-parse HEAD)" = "$(git -C "$src" rev-parse "$commit^{commit}")" ] \
    || { echo "$src is not at $commit; check it out first" >&2; exit 1; }
else
  src=$work/horizon
  git init -q "$src"
  git -C "$src" remote add origin "$repo"
  git -C "$src" fetch -q --depth 1 origin "$commit"
  git -C "$src" checkout -q FETCH_HEAD
fi
full=$(git -C "$src" rev-parse HEAD)

# Only the package and what it depends on inside the workspace.
(cd "$src" && pnpm install --frozen-lockfile --filter "@skywalking-horizon-ui/conversation-view..." >/dev/null)
(cd "$src" && pnpm --filter @skywalking-horizon-ui/conversation-view build >/dev/null)

# What asz embeds: the module, its stylesheet, the host shell, and the
# licenses of the two fonts, which the binary distribution must carry.
out=$work/out
mkdir -p "$out/host-shell/fonts"
cp "$src/$pkg/dist/conversation-view.js" "$src/$pkg/dist/conversation-view.css" "$out/"
cp "$src/$pkg/dist/host-shell/horizon-theme.css" "$src/$pkg/dist/host-shell/themes.json" "$out/host-shell/"
cp "$src/$pkg/dist/host-shell/fonts/"*.woff2 "$out/host-shell/fonts/"
cp "$src/$pkg/node_modules/@fontsource-variable/inter/LICENSE" "$out/host-shell/fonts/LICENSE-inter.txt"
cp "$src/$pkg/node_modules/@fontsource-variable/jetbrains-mono/LICENSE" "$out/host-shell/fonts/LICENSE-jetbrains-mono.txt"
version=$(sed -n 's/^  "version": "\(.*\)",$/\1/p' "$src/$pkg/package.json")
printf 'commit %s\npackage @skywalking-horizon-ui/conversation-view %s\nsource %s\n' "$full" "$version" "$repo" > "$out/HORIZON_COMMIT"

case "$cmd" in
  update)
    rm -rf "$here/$dest"
    mkdir -p "$here/$dest"
    cp -R "$out/." "$here/$dest/"
    echo "conversation-view: built from Horizon $full into $dest"
    ;;
  check)
    if diff -r "$out" "$here/$dest"; then
      echo "conversation-view matches Horizon $full"
    else
      echo "conversation-view differs from a build of Horizon $full: run tools/conversation-view.sh update and commit the result" >&2
      exit 1
    fi
    ;;
esac
