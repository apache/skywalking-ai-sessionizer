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

# Installs the Debian packages of some versions with apt, from an apt
# repository served the way the SkyWalking website serves it. CI runs it on
# amd64 and on arm64, and tools/release/apt-repository.sh runs it on released
# packages before it changes the website.
#
#   tools/test/apt-check.sh DIR VERSION...
#
# DIR/VERSION holds the Debian packages of each VERSION, as make binaries
# writes them with DIST=DIR/VERSION. The versions are digits and dots.
#
# tools/release/apt-index writes the index one version at a time, oldest first, and a
# second run with every package must change nothing. The index is signed
# with a key made for the run. Apache httpd serves it with the .htaccess
# aptindex wrote, as the website does, and serves the packages as the
# Apache download sites do: the newest version behind a redirect, like the
# mirror selector, and every version in the archive. In Debian and in Ubuntu,
# apt then installs the newest version, each older one by its version, and
# the newest again, and each must report its version. The server's log must
# show the newest downloaded from the mirrors and every older one from the
# archive. Needs docker, go and gpg.

set -euo pipefail

me=apt-check
fail() { printf '%s: %s\n' "$me" "$*" >&2; exit 1; }
step() { printf '\n== %s\n' "$*"; }

[ "$#" -ge 2 ] || { echo "usage: tools/test/apt-check.sh DIR VERSION..." >&2; exit 2; }
[ -d "$1" ] || fail "$1 is not a directory"
dir=$(cd "$1" && pwd)
shift
for v in "$@"; do
  printf '%s' "$v" | grep -Eq '^[0-9]+(\.[0-9]+)*$' || fail "$v is not digits and dots, which a release version is"
  [ -d "$dir/$v" ] || fail "$dir/$v does not exist"
done
for t in docker go gpg; do command -v "$t" >/dev/null 2>&1 || fail "$t is not on PATH"; done
tree=$(cd "$(dirname "$0")/../.." && pwd)
versions=$(printf '%s\n' "$@" | sort -V | uniq)
newest=$(printf '%s\n' "$versions" | tail -1)
packages="asz asz-claude-code"
arches="amd64 arm64"
images="debian:stable ubuntu:24.04"

work=$(mktemp -d)
id=asz-apt-check-$$
cleanup() {
  docker rm -f "$id-site" >/dev/null 2>&1 || true
  docker network rm "$id" >/dev/null 2>&1 || true
  if command -v gpgconf >/dev/null 2>&1; then gpgconf --homedir "$work/gnupg" --kill all >/dev/null 2>&1 || true; fi
  rm -rf "$work"
}
trap cleanup EXIT

step "Write the repository, one version at a time"
(cd "$tree" && go build -o "$work/aptindex" ./tools/release/apt-index)
site=$work/site
mkdir -p "$site/apt" "$site/closer"
# The mirror selector answers with a redirect to a mirror, which has no
# query. So does this.
printf 'RewriteEngine On\nRewriteRule ^(.*)$ /downloads/$1 [R=302,L,QSD]\n' > "$site/closer/.htaccess"
all=()
for v in $versions; do
  debs=()
  for p in $packages; do
    for a in $arches; do
      f=$dir/$v/apache-skywalking-ai-sessionizer-$v-bin-$p-$a.deb
      [ -f "$f" ] || fail "$f is missing"
      debs+=("$f")
    done
  done
  mkdir -p "$site/archive/$v"
  cp "${debs[@]}" "$site/archive/$v/"
  if [ "$v" = "$newest" ]; then
    mkdir -p "$site/downloads/$v"
    cp "${debs[@]}" "$site/downloads/$v/"
  fi
  "$work/aptindex" -dir "$site/apt" \
    -current 'http://site/closer/{version}/{file}?action=download' \
    -archive 'http://site/archive/{version}/{file}' "${debs[@]}"
  all+=("${debs[@]}")
done
before=$(cd "$site/apt" && find . -type f -exec shasum -a 256 {} + | sort)
out=$("$work/aptindex" -dir "$site/apt" \
  -current 'http://site/closer/{version}/{file}?action=download' \
  -archive 'http://site/archive/{version}/{file}' "${all[@]}")
printf '%s\n' "$out"
[ "$(cd "$site/apt" && find . -type f -exec shasum -a 256 {} + | sort)" = "$before" ] ||
  fail "a second run with the same packages changed the repository"
printf '%s\n' "$out" | grep -q 'the index did not change' || fail "a second run with the same packages did not say the index is unchanged"
for v in $versions; do
  n=$(grep -c "^RewriteRule \^pool/[a-z-]*_${v//./\\\\.}_" "$site/apt/.htaccess" || true)
  want=4
  [ "$n" = "$want" ] || fail ".htaccess has $n rules for $v, not $want"
done
echo "every version is in the index, and a second run changed nothing"

step "Sign the index"
gnupg=$work/gnupg
mkdir -m 700 "$gnupg"
g() { gpg --homedir "$gnupg" --batch --pinentry-mode loopback --passphrase '' "$@"; }
# KEYS holds many keys. apt must find the one that signed among them.
g --quiet --quick-gen-key 'Another committer <another@example.invalid>' rsa2048 sign never
g --quiet --quick-gen-key 'Release manager <release@example.invalid>' rsa2048 sign never
{
  echo "This file contains the PGP keys of the developers."
  g --armor --export another@example.invalid
  echo
  g --armor --export release@example.invalid
} > "$work/KEYS"
g --yes --local-user release@example.invalid --clearsign -o "$site/apt/dists/stable/InRelease" "$site/apt/dists/stable/Release"
g --yes --local-user release@example.invalid --armor --detach-sign -o "$site/apt/dists/stable/Release.gpg" "$site/apt/dists/stable/Release"
mkdir "$work/keyring"
# What the install page has a person run on KEYS.
gpg --dearmor < "$work/KEYS" > "$work/keyring/apache-skywalking.gpg"
echo "signed with a key that is second in KEYS"

step "Serve it as the website and the download sites do"
docker network create "$id" >/dev/null
docker run --rm httpd:2.4 cat /usr/local/apache2/conf/httpd.conf > "$work/httpd.conf"
# The website's server reads .htaccess files, with mod_rewrite.
perl -0pi -e 's/^#(LoadModule rewrite_module )/$1/m; s/(<Directory "\/usr\/local\/apache2\/htdocs">.*?)AllowOverride None/$1AllowOverride All/s' "$work/httpd.conf"
grep -q '^LoadModule rewrite_module' "$work/httpd.conf" || fail "cannot turn on mod_rewrite in httpd.conf"
chmod -R a+rX "$site"
docker run -d --name "$id-site" --network "$id" --network-alias site \
  -v "$site:/usr/local/apache2/htdocs:ro" -v "$work/httpd.conf:/usr/local/apache2/conf/httpd.conf:ro" httpd:2.4 >/dev/null
echo "httpd serves the repository at http://site/apt"

cat > "$work/keyring/install.sh" <<'SH'
set -euo pipefail
for i in $(seq 100); do
  if (exec 3<>/dev/tcp/site/80) 2>/dev/null; then break; fi
  sleep 0.1
done
# Only this repository, so apt reaches nothing else.
rm -f /etc/apt/sources.list /etc/apt/sources.list.d/*
install -m 644 /check/apache-skywalking.gpg /usr/share/keyrings/apache-skywalking.gpg
echo "deb [signed-by=/usr/share/keyrings/apache-skywalking.gpg] http://site/apt stable main" > /etc/apt/sources.list.d/apache-skywalking.list
export DEBIAN_FRONTEND=noninteractive
apt-get update
listed=$(apt list -a asz 2>/dev/null)
printf '%s\n' "$listed"
for v in $VERSIONS; do
  printf '%s\n' "$listed" | grep -q " $v " || { echo "apt list does not show asz $v"; exit 1; }
done
expect() { # expect VERSION
  asz version | grep -qF "$1" || { echo "asz reports $(asz version), not $1"; exit 1; }
  asz-claude-plugin version | grep -qF "$1" || { echo "asz-claude-plugin reports $(asz-claude-plugin version), not $1"; exit 1; }
  [ "$(command -v asz)" = /usr/bin/asz ] || { echo "asz is at $(command -v asz), not /usr/bin/asz"; exit 1; }
  # A minimal image leaves out /usr/share/doc, and dpkg --verify reports
  # those files as missing. Anything else it reports is a failure.
  differ=$(dpkg --verify asz asz-claude-code 2>&1 | grep -v "^missing  *$DOCS_LEFT_OUT" || true)
  [ -z "$differ" ] || { printf '%s\n' "$differ"; echo "dpkg --verify found installed files that differ from the packages"; exit 1; }
  echo "ok  asz and asz-claude-plugin $1"
}
DOCS_LEFT_OUT='/usr/share/doc/'
if ! grep -rqs 'path-exclude.*/usr/share/doc' /etc/dpkg/dpkg.cfg.d/; then DOCS_LEFT_OUT='^$'; fi
apt-get install -y asz asz-claude-code
expect "$NEWEST"
if [ "$DOCS_LEFT_OUT" = '^$' ]; then
  for p in asz asz-claude-code; do
    for f in LICENSE NOTICE; do [ -f "/usr/share/doc/$p/$f" ] || { echo "/usr/share/doc/$p/$f is missing"; exit 1; }; done
    [ -n "$(ls -A "/usr/share/doc/$p/licenses")" ] || { echo "/usr/share/doc/$p/licenses is empty"; exit 1; }
  done
  echo "ok  LICENSE, NOTICE and licenses/ under /usr/share/doc"
fi
for v in $VERSIONS; do
  [ "$v" != "$NEWEST" ] || continue
  apt-get install -y --allow-downgrades "asz=$v" "asz-claude-code=$v"
  expect "$v"
done
apt-get install -y asz asz-claude-code
expect "$NEWEST"
apt-get remove -y asz asz-claude-code
[ ! -e /usr/bin/asz ] && [ ! -e /usr/bin/asz-claude-plugin ] || { echo "apt-get remove left a binary"; exit 1; }
echo "ok  removed"
SH

for image in $images; do
  step "apt in $image"
  docker run --rm --network "$id" -v "$work/keyring:/check:ro" \
    -e VERSIONS="$versions" -e NEWEST="$newest" "$image" bash /check/install.sh ||
    fail "apt in $image did not install the packages as it should. Its output is above"
done

step "Where apt downloaded from"
log=$(docker logs "$id-site" 2>&1)
arch=$(docker run --rm debian:stable dpkg --print-architecture)
for v in $versions; do
  for p in $packages; do
    f=apache-skywalking-ai-sessionizer-$v-bin-$p-$arch.deb
    if [ "$v" = "$newest" ]; then where=downloads; else where=archive; fi
    printf '%s\n' "$log" | grep -q "\"GET /$where/$v/$f HTTP/1.1\" 200 " ||
      { printf '%s\n' "$log" | grep 'GET /' >&2; fail "apt did not download $f from /$where/"; }
  done
done
if printf '%s\n' "$log" | grep -E '" (4|5)[0-9][0-9] ' >&2; then fail "the server answered a request with an error, above"; fi
echo "the newest version came from the mirrors, and every older one from the archive"

step "Done"
echo "apt installs $(printf '%s\n' "$versions" | paste -sd ' ' -) in $images"
