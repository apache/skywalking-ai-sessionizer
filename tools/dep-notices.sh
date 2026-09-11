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

# Writes the NOTICE a binary distribution carries: the project's own, then
# the NOTICE of every module built into the binaries that ships one, as the
# Apache License asks of a distribution that bundles such a work. The
# modules come from the build itself, not from go.mod: the packages of asz
# and of the Claude Code plugin, on every platform in the Makefile's
# PLATFORMS. So a module that is only needed by the tests does not appear.
#
#   tools/dep-notices.sh [NOTICE] [CONFIGURATION]
#
# Given CONFIGURATION, it also writes the configuration license-eye
# resolves the binary LICENSE and licenses/ with, so that they follow the
# same rule. It is .licenserc.yaml, with every module that go.mod requires
# and the build does not use excluded. license-eye lists the modules of
# go.mod, and without the exclusions LICENSE named github.com/kr/text and
# github.com/rogpeppe/go-internal, which only the tests of gopkg.in/yaml.v3
# reach. The ASF asks the LICENSE of a package to account for exactly what
# the package holds.
#
# Used by make dep-licenses and make dep-licenses-check.
set -eu
cd "$(dirname "$0")/.."
out="${1:-dist-material/NOTICE}"
config="${2:-}"
platforms=$(sed -nE 's/^PLATFORMS[[:space:]]*:?=[[:space:]]*//p' Makefile)
[ -n "$platforms" ] || { echo "dep-notices: the Makefile has no PLATFORMS" >&2; exit 1; }
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# Each go command writes to a file, not into a pipe, so that its failure
# stops the script. A go.work is ignored, as make binaries ignores it,
# because a workspace changes the modules the build uses.
: > "$work/deps"
for t in $platforms; do
  GOWORK=off CGO_ENABLED=0 GOOS=${t%/*} GOARCH=${t#*/} go list -deps \
    -f '{{with .Module}}{{if not .Main}}{{.Path}}@{{.Version}}{{end}}{{end}}' \
    ./cmd/asz ./plugins/claude-code >> "$work/deps"
done
sed '/^$/d' "$work/deps" | sort -u > "$work/modules"

{
  cat NOTICE
  while read -r mod; do
    dir=$(GOWORK=off go list -m -f '{{.Dir}}' "$mod")
    for n in "$dir/NOTICE" "$dir/NOTICE.txt" "$dir/NOTICE.md"; do
      [ -f "$n" ] || continue
      printf '\n========================================================================\n\n%s %s NOTICE\n\n========================================================================\n' "${mod%@*}" "${mod#*@}"
      cat "$n"
    done
  done < "$work/modules"
} > "$out"

[ -n "$config" ] || exit 0

# Every module in the graph of go.mod, less the ones the build uses.
# Excluding a module license-eye does not list changes nothing.
GOWORK=off go list -m -f '{{if not .Main}}{{.Path}}{{end}}' all > "$work/graph"
sed '/^$/d' "$work/graph" | sort -u > "$work/all"
sed 's/@.*//' "$work/modules" | sort -u > "$work/used"
comm -23 "$work/all" "$work/used" > "$work/unused"

# license-eye reads a relative path under dependency.files relative to the
# configuration file. This one is written elsewhere, so such a path is made
# absolute. The exclusions go first under dependency, and a second
# excludes key there would make license-eye stop, not ignore either list.
root=$(pwd -P)
awk -v root="$root" -v unused="$work/unused" '
  /^[^ #]/ {top = $1; key = ""}
  top == "dependency:" && /^  [^ #-]/ {key = $1}
  top == "dependency:" && key == "files:" && /^    - / && $2 !~ /^\// {print "    - " root "/" $2; next}
  {print}
  /^dependency:/ {print "  excludes:"; while ((getline m < unused) > 0) print "    - name: " m}
' .licenserc.yaml > "$config"
grep -qF "    - $root/go.mod" "$config" || { echo "dep-notices: .licenserc.yaml does not name go.mod under dependency.files" >&2; exit 1; }
