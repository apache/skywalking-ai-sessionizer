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

# Writes the Homebrew formulae of released versions into Formula/, the tap
# this repository is.
#
#   tools/homebrew-formula.sh [--from DIR] [--check] VERSION...
#
# For each VERSION it takes the six binary packages and their .sha512 from
# DIR/VERSION, or else downloads them from downloads.apache.org, or from
# archive.apache.org for a version the download site no longer holds.
# tools/install-manifests.sh holds each package to its .sha512 and writes the
# formulae.
#
# Every VERSION gets Formula/asz@VERSION.rb and Formula/asz-claude-code@VERSION.rb.
# Formula/asz.rb and Formula/asz-claude-code.rb follow the newest version among
# those given and the one Formula/asz.rb names already, so adding an older
# version never moves them back. The script changes Formula/ only, and leaves
# the commit and the pull request to the person who runs it.
#
# With --check, each version's formulae go through tools/homebrew-check.sh,
# brew style, brew audit, brew install and brew test, before any of them is
# written into Formula/. It needs Homebrew, and runs on macOS.

set -euo pipefail

me=homebrew-formula
fail() { printf '%s: %s\n' "$me" "$*" >&2; exit 1; }
say() { printf '%s: %s\n' "$me" "$*"; }

from=""
check=false
while [ "$#" -gt 0 ]; do
  case "$1" in
    --from)
      [ -n "${2:-}" ] || fail "--from needs a directory"
      from=$(cd "$2" && pwd) || fail "$2 is not a directory"
      shift 2 ;;
    --check) check=true; shift ;;
    --*) fail "unknown option $1" ;;
    *) break ;;
  esac
done
[ "$#" -ge 1 ] || { echo "usage: tools/homebrew-formula.sh [--from DIR] [--check] VERSION..." >&2; exit 2; }
for v in "$@"; do
  # Homebrew takes asz@VERSION as a version of asz only when VERSION is
  # digits and dots, which every Apache release is.
  printf '%s' "$v" | grep -Eq '^[0-9]+(\.[0-9]+)*$' || fail "$v is not a released version: digits and dots"
done

tree=$(cd "$(dirname "$0")/.." && pwd)
formula=$tree/Formula
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# The version the current formulae install, read from the release path in
# their download URL.
current=""
if [ -f "$formula/asz.rb" ]; then
  current=$(grep -Eo '/releases/download/v[0-9.]+/' "$formula/asz.rb" | head -1 | sed -E 's#/releases/download/v([0-9.]+)/#\1#')
fi

packages() {
  local v=$1 dir=$2 base=apache-skywalking-ai-sessionizer-$1-bin
  mkdir -p "$dir"
  for p in darwin-arm64.tgz darwin-amd64.tgz linux-arm64.tgz linux-amd64.tgz windows-amd64.zip windows-arm64.zip; do
    for f in "$base-$p" "$base-$p.sha512"; do
      local site=https://downloads.apache.org/skywalking/ai-sessionizer/$v/$f
      local archive=https://archive.apache.org/dist/skywalking/ai-sessionizer/$v/$f
      local code
      code=$(curl -sSL -o "$dir/$f" -w '%{http_code}' "$site") || fail "cannot reach downloads.apache.org for $f"
      case "$code" in
        200) ;;
        404) curl -fsSL -o "$dir/$f" "$archive" || fail "neither downloads.apache.org nor archive.apache.org has $f. Is $v released?" ;;
        *) fail "downloads.apache.org answered $code for $f" ;;
      esac
    done
  done
}

newest=$current
for v in "$@"; do
  if [ -n "$from" ]; then
    dir=$from/$v
    [ -d "$dir" ] || fail "$dir does not exist"
  else
    say "downloading the packages of $v"
    dir=$work/packages-$v
    packages "$v" "$dir"
  fi
  bash "$tree/tools/install-manifests.sh" "$v" "$dir" "$work/manifests-$v" >/dev/null
  if [ "$check" = true ]; then
    say "checking the formulae of $v with Homebrew"
    bash "$tree/tools/homebrew-check.sh" "$v" "$dir" || fail "the formulae of $v did not pass tools/homebrew-check.sh, so none of $v was written"
  fi
  mkdir -p "$formula"
  cp "$work/manifests-$v/homebrew/asz@$v.rb" "$work/manifests-$v/homebrew/asz-claude-code@$v.rb" "$formula/"
  say "wrote Formula/asz@$v.rb and Formula/asz-claude-code@$v.rb"
  newest=$(printf '%s\n%s\n' "$newest" "$v" | sed '/^$/d' | sort -V | tail -1)
done

if [ "$newest" != "$current" ]; then
  cp "$work/manifests-$newest/homebrew/asz.rb" "$work/manifests-$newest/homebrew/asz-claude-code.rb" "$formula/"
  say "Formula/asz.rb and Formula/asz-claude-code.rb now install $newest${current:+, not $current}"
else
  say "Formula/asz.rb and Formula/asz-claude-code.rb stay at $current, the newest version"
fi
