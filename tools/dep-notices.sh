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
# the NOTICE of every module built into the binary that ships one, as the
# Apache License asks of a distribution that bundles such a work. The
# modules come from the build itself, not from go.mod, so a module that is
# only needed by the tests does not appear. Used by make dep-licenses and
# make dep-licenses-check.
set -eu
cd "$(dirname "$0")/.."
out="${1:-dist-material/NOTICE}"
{
  cat NOTICE
  go list -deps -f '{{if and (not .Standard) .Module}}{{if not .Module.Main}}{{.Module.Path}}@{{.Module.Version}}{{end}}{{end}}' ./cmd/asz \
    | sort -u | while read -r mod; do
    [ -n "$mod" ] || continue
    dir=$(go list -m -f '{{.Dir}}' "$mod")
    for n in "$dir/NOTICE" "$dir/NOTICE.txt" "$dir/NOTICE.md"; do
      [ -f "$n" ] || continue
      printf '\n========================================================================\n\n%s %s NOTICE\n\n========================================================================\n' "${mod%@*}" "${mod#*@}"
      cat "$n"
    done
  done
} > "$out"
