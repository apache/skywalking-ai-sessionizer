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
trap '[ -n "$view_pid" ] && kill "$view_pid" 2>/dev/null; rm -rf "$work"' EXIT

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
[ -f "$pkg.sha512" ] || fail "$pkg.sha512 is missing"
(
  cd "$(dirname "$pkg")"
  if command -v sha512sum >/dev/null 2>&1; then sha512sum -c "$(basename "$pkg").sha512"
  else shasum -a 512 -c "$(basename "$pkg").sha512"; fi
)

step "Unpack"
dir="$work/package"
mkdir -p "$dir"
if [ -n "$exe" ]; then
  # Expand-Archive is what a Windows user's own tools do with a zip.
  if command -v pwsh >/dev/null 2>&1; then
    pwsh -NoProfile -Command "Expand-Archive -LiteralPath '$(native "$pkg")' -DestinationPath '$(native "$dir")'"
  elif command -v powershell >/dev/null 2>&1; then
    powershell -NoProfile -Command "Expand-Archive -LiteralPath '$(native "$pkg")' -DestinationPath '$(native "$dir")'"
  else
    unzip -q "$pkg" -d "$dir"
  fi
else
  tar -xzf "$pkg" -C "$dir"
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
get() { curl --noproxy '*' -fsS "http://127.0.0.1:$port$1" >/dev/null 2>&1; }
up=false
for _ in $(seq 1 100); do
  check_view
  # asz view prints its address only after it holds the port. Before that
  # line, an answer may come from another program that holds it.
  if grep -q "http://127.0.0.1:$port" "$work/view.log" && get /; then up=true; break; fi
  sleep 0.2
done
check_view
[ "$up" = true ] || { cat "$work/view.log"; fail "the list page never answered"; }
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

# The hooks run the packaged plugin exactly as Claude Code does: the event on
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
grep -rq 'smoke.txt' "$work/plugin-data" || fail "the plugin recorded no change for smoke.txt"
echo "the plugin recorded the change"

step "Done"
echo "$(basename "$pkg") works on this machine"
