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

# Checks one binary package the way a person uses it: its checksum, what it
# unpacks to, and then asz and the Claude Code plugin run from the unpacked
# files. CI runs it on each platform's own runner, because a cross-compiled
# binary that was never started proves nothing about the platform. A release
# manager or a voter can run it on a candidate package for their platform.
#
#   tools/package-smoke.sh PACKAGE [VERSION]
#
# PACKAGE is a .tgz or a .zip with its .sha512 beside it. VERSION, when given,
# must appear in what both binaries report. The scenarios come from
# tests/scenarios in the tree this script sits in.

set -euo pipefail

pkg=${1:?usage: tools/package-smoke.sh PACKAGE [VERSION]}
want=${2:-}
tree=$(cd "$(dirname "$0")/.." && pwd)
# Checked before the path is made absolute. For a package whose directory
# is missing, that turns into /NAME, and the error then names a wrong path.
[ -f "$pkg" ] || { printf 'package-smoke: %s does not exist\n' "$pkg" >&2; exit 1; }
pkg=$(cd "$(dirname "$pkg")" && pwd)/$(basename "$pkg")
work=$(mktemp -d)
view_pid=
# Under set -e, a command that fails in the trap ends the shell before rm,
# so kill and wait must not fail. kill fails once asz view has ended, and
# wait returns the status of the server it stopped. wait reaps the server,
# so the shell does not report its end below the reason the script stopped.
cleanup() {
  if [ -n "$view_pid" ]; then
    kill "$view_pid" 2>/dev/null || true
    wait "$view_pid" 2>/dev/null || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT

step() { printf '\n== %s\n' "$*"; }
fail() { printf 'package-smoke: %s\n' "$*" >&2; exit 1; }

# A native Windows binary reads Windows paths, and Git Bash hands out POSIX
# ones, so every path given to a binary goes through cygpath there.
native() { if command -v cygpath >/dev/null 2>&1; then cygpath -m "$1"; else printf '%s' "$1"; fi; }

exe=
case "$pkg" in
  *.zip) exe=.exe ;;
  *.tgz) ;;
  *) fail "$pkg is neither a .tgz nor a .zip" ;;
esac

step "Checksum"
sum="$pkg.sha512"
[ -f "$sum" ] || fail "$sum is missing"
# The script compares the digests itself. sha512sum -c and shasum -c check
# whichever file the sidecar names, so a sidecar that named another file
# passed without the package being read. The BSD sha512sum on macOS also
# passed an empty sidecar, and one with no well-formed line. So the sidecar
# must hold one line, a sha512 and this package's name, as make checksums
# writes it.
lines=$(tr -d '\r' <"$sum" | grep -v '^[[:space:]]*$' || true)
n=$(printf '%s\n' "$lines" | grep -c . || true)
[ "$n" = 1 ] || fail "$sum must hold one line, a sha512 and a name. It holds $n."
read -r listed name extra <<EOF
$lines
EOF
# sha512sum -b and shasum -b put a star before the name.
name=${name#\*}
listed=$(printf '%s' "$listed" | tr 'A-F' 'a-f')
case "$listed" in *[!0-9a-f]*) listed= ;; esac
if [ "${#listed}" != 128 ] || [ -n "$extra" ]; then
  fail "$sum is not one sha512 followed by one name: $lines"
fi
[ "$name" = "$(basename "$pkg")" ] || fail "$sum is for ${name:-no file}, not for $(basename "$pkg")"
if command -v sha512sum >/dev/null 2>&1; then actual=$(sha512sum <"$pkg")
else actual=$(shasum -a 512 <"$pkg"); fi
actual=${actual%%[[:space:]]*}
[ "$actual" = "$listed" ] || fail "the sha512 of $(basename "$pkg") is $actual, and $sum says $listed"
echo "$(basename "$pkg"): its sha512 matches $(basename "$sum")"

step "Unpack"
dir="$work/package"
if [ -n "$exe" ]; then
  # Expand-Archive is what a Windows user's own tools do with a zip. The
  # two paths reach PowerShell in the environment, never in the command
  # text. In the text they would sit inside quotes, and a path such as
  # C:/Users/O'Brien ends a quoted string at its apostrophe. Out-Null
  # drops a table that PowerShell 7.5.3 printed above the error for a bad zip.
  # Expand-Archive makes the destination itself, so it is not made first.
  # When it was, and its path held [ or ], PowerShell 7.5.3 stopped with
  # "An item with the specified name ... already exists".
  unpack='$ErrorActionPreference = "Stop"; Expand-Archive -LiteralPath $env:SMOKE_ZIP -DestinationPath $env:SMOKE_DEST | Out-Null'
  ps=
  if command -v pwsh >/dev/null 2>&1; then ps=pwsh
  elif command -v powershell >/dev/null 2>&1; then ps=powershell; fi
  if [ -n "$ps" ]; then
    SMOKE_ZIP=$(native "$pkg") SMOKE_DEST=$(native "$dir") "$ps" -NoProfile -NonInteractive -Command "$unpack" ||
      fail "$ps could not unpack $pkg. Its error is above."
  else
    mkdir -p "$dir"
    unzip -q "$pkg" -d "$dir" || fail "unzip could not unpack $pkg"
  fi
else
  mkdir -p "$dir"
  tar -xzf "$pkg" -C "$dir" || fail "tar could not unpack $pkg"
fi

step "Contents"
for f in "asz$exe" LICENSE NOTICE \
  claude-code-plugin/.claude-plugin/plugin.json claude-code-plugin/hooks/hooks.json \
  "claude-code-plugin/bin/asz-claude-plugin$exe"; do
  [ -f "$dir/$f" ] || fail "the package lacks $f"
done
[ -n "$(ls -A "$dir/licenses" 2>/dev/null)" ] || fail "the package's licenses directory is empty"
if [ -z "$exe" ]; then
  # A tar that drops the mode leaves a binary nobody can start.
  [ -x "$dir/asz" ] || fail "asz lost its executable bit in the archive"
  [ -x "$dir/claude-code-plugin/bin/asz-claude-plugin" ] || fail "the plugin lost its executable bit in the archive"
fi
asz="$dir/asz$exe"
plugin="$dir/claude-code-plugin/bin/asz-claude-plugin$exe"
echo "every file is there"

cd "$work"

step "Version"
got=$("$asz" version)
pgot=$("$plugin" version)
echo "asz    : $got"
echo "plugin : $pgot"
if [ -n "$want" ]; then
  case "$got" in *"$want"*) ;; *) fail "asz reports $got, not $want" ;; esac
  case "$pgot" in *"$want"*) ;; *) fail "the plugin reports $pgot, not $want" ;; esac
fi
"$asz" glossary >/dev/null

# scenario check builds the sources, lands them, parses them and checks every
# expectation, so it is collection and assembly run by the packaged binary.
step "Scenarios"
for s in fixture delegation workflow workspace-changes; do
  printf -- '-- %s\n' "$s"
  "$asz" scenario check "$(native "$tree/tests/scenarios/$s.yaml")"
done

step "The page"
"$asz" scenario build "$(native "$tree/tests/scenarios/fixture.yaml")" --format sd \
  --out "$(native "$work/root")" --at 2026-01-01T00:00:00Z >/dev/null
cfg=$(native "$work/root/asz.yaml")
"$asz" parse -config "$cfg" >/dev/null
"$asz" verify -config "$cfg"
port=18787
"$asz" view -config "$cfg" "127.0.0.1:$port" >"$work/view.log" 2>&1 &
view_pid=$!
# When another program holds the port, asz view cannot listen and ends, and
# curl may still get an answer from that program. So the check fails once
# asz view has ended, whatever curl got.
check_view() {
  kill -0 "$view_pid" 2>/dev/null && return 0
  cat "$work/view.log"
  fail "asz view has ended. Its output is above. Is port $port on 127.0.0.1 in use by another program?"
}
# curl sends even a request for 127.0.0.1 through http_proxy when that is
# set, as curl 8.19.0 did on macOS, and a proxy cannot reach this machine.
# curl also sets no time limit of its own. A program that accepts the
# connection and never answers would hold the script with no end, and only
# CI stops a job after a while. Each page answered in under 2 ms on macOS
# on Apple silicon, so the limits leave a slow runner plenty of time.
get() { curl --noproxy '*' --connect-timeout 5 --max-time 10 -fsS "http://127.0.0.1:$port$1" >/dev/null 2>&1; }
up=false
# The wait is counted in seconds, not in attempts, because an attempt that
# runs to its time limit is much longer than the pause between attempts.
deadline=$((SECONDS + 20))
while [ "$SECONDS" -lt "$deadline" ]; do
  check_view
  # asz view prints its address only after it holds the port. Before that
  # line, an answer may come from another program that holds it.
  if grep -q "http://127.0.0.1:$port" "$work/view.log" && get /; then up=true; break; fi
  sleep 0.2
done
check_view
[ "$up" = true ] || { cat "$work/view.log"; fail "the list page did not answer within 20 seconds"; }
if ! get /c/00000001-0000-4000-8000-000000000001; then
  check_view
  cat "$work/view.log"
  fail "the conversation page did not answer"
fi
check_view
# wait reaps the stopped server, so the shell does not report its end.
kill "$view_pid" 2>/dev/null || true
wait "$view_pid" 2>/dev/null || true
view_pid=
echo "the list page and the conversation page answered"

# The hooks run the packaged plugin the way hooks/hooks.json has Claude Code
# run it: the binary with the one argument hook and no shell, the event on
# standard input, the plugin's own directory and data directory in the
# environment. A shell command that writes a file must come out as a change.
step "The Claude Code plugin"
CLAUDE_PLUGIN_ROOT=$(native "$dir/claude-code-plugin")
CLAUDE_PLUGIN_DATA=$(native "$work/plugin-data")
export CLAUDE_PLUGIN_ROOT CLAUDE_PLUGIN_DATA
"$plugin" status >/dev/null
ws="$work/workspace"
mkdir -p "$ws"
wsn=$(native "$ws")
sid=00000000-0000-4000-8000-0000000000aa
event() { printf '{"hook_event_name":"%s","session_id":"%s","cwd":"%s"%s}' "$1" "$sid" "$wsn" "${2:-}"; }
bash_call=',"tool_name":"Bash","tool_use_id":"toolu_package_smoke","tool_input":{"command":"echo hello > smoke.txt"}'
event SessionStart | "$plugin" hook
event PreToolUse "$bash_call" | "$plugin" hook
echo hello >"$ws/smoke.txt"
event PostToolUse "$bash_call"',"tool_response":{"stdout":"","stderr":"","interrupted":false}' | "$plugin" hook
event SessionEnd | "$plugin" hook
# Every hook exits 0 whatever happened, so only the record can tell. The
# check reads the record, which is what asz collects. The scan's own files
# under roots/ name smoke.txt too, and the plugin writes them before the
# record, so a search of the whole data directory passed with no record.
rec="$work/plugin-data/output/$sid/main.jsonl"
plugin_fail() {
  if [ -s "$work/plugin-data/log/plugin.log" ]; then
    echo "-- the plugin's log"
    cat "$work/plugin-data/log/plugin.log"
  fi
  fail "$*"
}
[ -f "$rec" ] || plugin_fail "the plugin wrote no record. $rec does not exist."
line=$(grep -F '"id":"toolu_package_smoke"' "$rec" || true)
# A file change starts with its path and then its operation, so this
# pattern matches a change to smoke.txt and nothing else in a record.
case "$line" in
  *'{"path":"smoke.txt","operation":"create",'*) ;;
  '') echo "-- $rec"; cat "$rec"; plugin_fail "the plugin wrote no record for the shell command." ;;
  *) echo "-- the record"; printf '%s\n' "$line"; plugin_fail "the plugin's record does not name smoke.txt as a created file." ;;
esac
echo "the plugin recorded the change"

step "Done"
echo "$(basename "$pkg") works on this machine"
