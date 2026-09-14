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

# Reject macOS metadata in the archive itself. bsdtar can synthesize ._
# members from extended attributes even when no such file exists on disk.
set -eu
[ "$#" -gt 0 ] || { echo "usage: tools/package-check.sh PACKAGE..." >&2; exit 2; }
checked=0
for package in "$@"; do
  case "$package" in
    *.asc|*.sha512) continue ;;
  esac
  [ -f "$package" ] || { echo "package-check: $package does not exist" >&2; exit 1; }
  case "$package" in
    *.tgz|*.tar.gz)
      # On macOS the reader hides AppleDouble members by default, even
      # with COPYFILE_DISABLE set. Disable that reader behavior too.
      if tar --version 2>/dev/null | grep -q bsdtar; then
        listing=$(tar --options '!mac-ext' -tzf "$package")
      else
        listing=$(tar -tzf "$package")
      fi ;;
    *.zip) listing=$(unzip -Z1 "$package") ;;
    *) echo "package-check: unsupported archive $package" >&2; exit 2 ;;
  esac
  bad=$(printf '%s\n' "$listing" | grep -E '(^|/)(\._[^/]*|\.DS_Store|__MACOSX)(/|$)' || true)
  if [ -n "$bad" ]; then
    printf 'package-check: %s contains macOS metadata:\n%s\n' "$package" "$bad" >&2
    exit 1
  fi
  checked=$((checked + 1))
  printf 'package-check: %s has no macOS metadata\n' "$package"
done
[ "$checked" -gt 0 ] || { echo "package-check: no packages checked" >&2; exit 1; }
