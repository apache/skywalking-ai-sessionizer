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

# Adds released versions to the apt repository the SkyWalking website serves
# at https://skywalking.apache.org/apt, from static/apt in a checkout of
# apache/skywalking-website.
#
#   tools/apt-repository.sh [--from DIR] [--check] APT_DIR VERSION...
#
# For each VERSION it takes the four Debian packages with their .sha512 and
# .asc from DIR/VERSION, or else downloads them from downloads.apache.org, or
# from archive.apache.org for a version the download site no longer holds.
# Each package must match its .sha512, and its .asc must be a good signature
# by a key in KEYS. With --check, tools/apt-check.sh installs them with apt
# before anything is written, which needs docker.
#
# tools/aptindex then adds them to the index in APT_DIR, and writes the
# redirects: the newest version to the download mirrors, every older one to
# archive.apache.org. When the index changed, Release is signed into InRelease
# and Release.gpg with the key GPG_USER names, or gpg's default key. apt checks
# them against the keys a person took from KEYS, so both signatures must verify
# against KEYS read the same way. The script changes APT_DIR only, and leaves
# the commit and the pull request to the person who runs it.

set -euo pipefail

me=apt-repository
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
[ "$#" -ge 2 ] || { echo "usage: tools/apt-repository.sh [--from DIR] [--check] APT_DIR VERSION..." >&2; exit 2; }
apt_parent=$(dirname "$1")
[ -d "$apt_parent" ] || fail "$apt_parent does not exist. APT_DIR is static/apt in a checkout of apache/skywalking-website"
apt=$(cd "$apt_parent" && pwd)/$(basename "$1")
shift
for v in "$@"; do
  printf '%s' "$v" | grep -Eq '^[0-9]+(\.[0-9]+)*$' || fail "$v is not a released version: digits and dots"
done
for t in go gpg gpgv curl shasum; do command -v "$t" >/dev/null 2>&1 || fail "$t is not on PATH"; done

tree=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
packages="asz asz-claude-code"
arches="amd64 arm64"

# fetch FILE VERSION DEST downloads one file of a released version.
fetch() {
  local site=https://downloads.apache.org/skywalking/ai-sessionizer/$2/$1
  local archive=https://archive.apache.org/dist/skywalking/ai-sessionizer/$2/$1
  local code
  code=$(curl -sSL -o "$3/$1" -w '%{http_code}' "$site") || fail "cannot reach downloads.apache.org for $1"
  case "$code" in
    200) ;;
    404) curl -fsSL -o "$3/$1" "$archive" || fail "neither downloads.apache.org nor archive.apache.org has $1. Is $2 released, with Debian packages? 0.4.0 is the first version that has them" ;;
    *) fail "downloads.apache.org answered $code for $1" ;;
  esac
}

say "reading KEYS the way the install page has a person read it"
# A test gives a KEYS file of its own keys in APT_REPOSITORY_KEYS.
if [ -n "${APT_REPOSITORY_KEYS:-}" ]; then
  cp "$APT_REPOSITORY_KEYS" "$work/KEYS"
else
  curl -fsSL -o "$work/KEYS" https://downloads.apache.org/skywalking/KEYS || fail "cannot download https://downloads.apache.org/skywalking/KEYS"
fi
gpg --dearmor < "$work/KEYS" > "$work/keys.gpg" || fail "gpg cannot read KEYS"

debs=()
for v in "$@"; do
  dir=$work/packages/$v
  mkdir -p "$dir"
  for p in $packages; do
    for a in $arches; do
      f=apache-skywalking-ai-sessionizer-$v-bin-$p-$a.deb
      for g in "$f" "$f.sha512" "$f.asc"; do
        if [ -n "$from" ]; then
          [ -f "$from/$v/$g" ] || fail "$from/$v/$g does not exist"
          cp "$from/$v/$g" "$dir/"
        else
          fetch "$g" "$v" "$dir"
        fi
      done
      (cd "$dir" && shasum -a 512 --status -c "$f.sha512") || fail "$f does not match its .sha512"
      gpgv --keyring "$work/keys.gpg" "$dir/$f.asc" "$dir/$f" 2>"$work/gpgv.txt" ||
        { cat "$work/gpgv.txt" >&2; fail "$f.asc is not a good signature by a key in KEYS"; }
      debs+=("$dir/$f")
    done
  done
  say "$v: four Debian packages, each matching its .sha512 and signed by a key in KEYS"
done

if [ "$check" = true ]; then
  say "installing them with apt in Debian and Ubuntu"
  bash "$tree/tools/apt-check.sh" "$work/packages" "$@" || fail "tools/apt-check.sh failed, so $apt was not changed"
fi

(cd "$tree" && go build -o "$work/aptindex" ./tools/aptindex)
"$work/aptindex" -dir "$apt" "${debs[@]}"

release=$apt/dists/stable
if [ ! -f "$release/InRelease" ] || [ ! -f "$release/Release.gpg" ]; then
  signer=()
  if [ -n "${GPG_USER:-}" ]; then signer=(--local-user "$GPG_USER"); fi
  if [ -z "${GPG_TTY:-}" ] && [ -t 0 ]; then GPG_TTY=$(tty); export GPG_TTY; fi
  gpg --yes ${signer[@]+"${signer[@]}"} --clearsign -o "$release/InRelease" "$release/Release" ||
    fail "gpg could not sign Release. The index in $apt is written and not signed: run this again once gpg can sign"
  gpg --yes ${signer[@]+"${signer[@]}"} --armor --detach-sign -o "$release/Release.gpg" "$release/Release" ||
    fail "gpg could not sign Release. The index in $apt is written and not signed: run this again once gpg can sign"
  say "signed dists/stable/Release into InRelease and Release.gpg"
fi
gpgv --keyring "$work/keys.gpg" "$release/InRelease" 2>"$work/gpgv.txt" ||
  { cat "$work/gpgv.txt" >&2; fail "InRelease is not a good signature by a key in KEYS, so apt would refuse the repository. Remove $release/InRelease and $release/Release.gpg, and run this again with GPG_USER set to your key in KEYS"; }
gpgv --keyring "$work/keys.gpg" "$release/Release.gpg" "$release/Release" 2>"$work/gpgv.txt" ||
  { cat "$work/gpgv.txt" >&2; fail "Release.gpg is not a good signature by a key in KEYS. Remove $release/InRelease and $release/Release.gpg, and run this again with GPG_USER set to your key in KEYS"; }
listed=$(cat "$release"/main/binary-*/Packages | sed -n 's/^Version: //p' | sort -uV | paste -sd ' ' -)
say "the index is signed by a key in KEYS, and lists $listed"
