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

# Prepare a release: the checks, the changelog roll and the menu, in one
# commit. It does not tag, build or push; those are separate steps in the
# release guide, docs/en/guides/how-to-release.md, and each is easy to undo
# on its own.
#
#   tools/release.sh [CURRENT] [NEXT] [--dry-run] [--skip-check]
#
# CURRENT is the version being released, for example 0.1.0. NEXT is the
# version that development moves to, for example 0.2.0. Both are asked for
# when not given. --dry-run prints what would change and writes nothing.
# --skip-check leaves out make check, which takes about a minute.

set -euo pipefail

current=""
next=""
dry_run=false
skip_check=false
for arg in "$@"; do
  case "$arg" in
    --dry-run) dry_run=true ;;
    --skip-check) skip_check=true ;;
    -h|--help) sed -n '20,31p' "$0"; exit 0 ;;
    -*) echo "unknown option: $arg" >&2; exit 2 ;;
    *) if [ -z "$current" ]; then current="$arg"; elif [ -z "$next" ]; then next="$arg"; else echo "too many arguments" >&2; exit 2; fi ;;
  esac
done

say()  { printf '%s\n' "$*"; }
fail() { printf 'release: %s\n' "$*" >&2; exit 1; }
step() { printf '\n== %s\n' "$*"; }

# Everything is relative to the repository root.
cd "$(git rev-parse --show-toplevel)"
changes_dir=docs/en/changes
menu=docs/menu.yml

step "The tree"
if [ -n "$(git status --porcelain)" ]; then
  fail "the working tree has changes; a release is prepared from a clean tree"
fi
branch=$(git branch --show-current)
if [ "$branch" != "main" ]; then
  say "note: on branch $branch, not main"
fi

step "Versions"
is_version() { printf '%s' "$1" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$'; }
if [ -z "$current" ]; then
  suggested=$(ls "$changes_dir" | sed -nE 's/^changes-([0-9]+\.[0-9]+\.[0-9]+.*)\.md$/\1/p' | sort -V | tail -1)
  printf 'Version to release [%s]: ' "${suggested:-}"
  read -r current
  current=${current:-$suggested}
fi
is_version "$current" || fail "current version $current is not of the form MAJOR.MINOR.PATCH"
if [ -z "$next" ]; then
  suggested=$(printf '%s' "$current" | awk -F. '{printf "%d.%d.0", $1, $2 + 1}')
  printf 'Next version, where development continues [%s]: ' "$suggested"
  read -r next
  next=${next:-$suggested}
fi
is_version "$next" || fail "next version $next is not of the form MAJOR.MINOR.PATCH"
if [ "$(printf '%s\n%s\n' "$current" "$next" | sort -V | head -1)" != "$current" ] || [ "$current" = "$next" ]; then
  fail "next version $next must come after $current"
fi
if git rev-parse -q --verify "refs/tags/v$current" >/dev/null; then
  fail "tag v$current already exists; the version is already prepared or released"
fi
say "releasing $current, development continues on $next"

step "License headers"
make license-check

if [ "$skip_check" = false ]; then
  step "The full check: vet, lint, licenses, tests"
  make check
fi

step "The changelog"
page="$changes_dir/changes-$current.md"
rolling="$changes_dir/changes.md"
plan=()
# The version's page. It exists from the start of the version's development;
# a rolling page that still carries this version's title is renamed to it.
if [ ! -f "$page" ]; then
  if [ -f "$rolling" ] && head -1 "$rolling" | grep -q "^# Changes in $current\$"; then
    plan+=("rename $rolling to $page")
    if [ "$dry_run" = false ]; then git mv "$rolling" "$page"; fi
  else
    fail "$page does not exist and $rolling is not the $current page; write the changelog first"
  fi
fi
if grep -q '^> In development' "$page"; then
  plan+=("remove the in-development note from $page")
  if [ "$dry_run" = false ]; then
    # The note is a blockquote followed by a blank line.
    awk 'BEGIN{skip=0} /^> In development/{skip=1} skip&&/^$/{skip=0; next} !skip{print}' "$page" > "$page.tmp" && mv "$page.tmp" "$page"
  fi
fi
# The rolling page for the next version.
if [ -f "$rolling" ] && head -1 "$rolling" | grep -q "^# Changes in $next\$"; then
  say "$rolling already collects $next"
else
  plan+=("start $rolling for $next")
  if [ "$dry_run" = false ]; then
    cat > "$rolling" <<PAGE
# Changes in $next

> In development, not yet released. This page collects the changes for the next version, and
> \`tools/release.sh\` turns it into the version's own page at release time.

Nothing yet.
PAGE
  fi
fi

step "The menu"
if grep -q "path: /en/changes/changes-$current\$" "$menu"; then
  say "$menu already lists $current"
else
  plan+=("list $current under Changelog in $menu, newest first")
  if [ "$dry_run" = false ]; then
    # Insert right after the Current Version entry, so versions read newest first.
    awk -v v="$current" '
      {print}
      /path: \/en\/changes\/changes$/ && !done {
        print "        - name: " v
        print "          path: /en/changes/changes-" v
        done=1
      }' "$menu" > "$menu.tmp" && mv "$menu.tmp" "$menu"
  fi
fi

step "Root CHANGES.md"
if grep -q "changes-$current.md" CHANGES.md; then
  say "CHANGES.md already links $current"
else
  plan+=("link $current from CHANGES.md")
  if [ "$dry_run" = false ]; then
    awk -v v="$current" '
      {print}
      /^- \[Next version, in development\]/ && !done {
        print "- [" v "](docs/en/changes/changes-" v ".md)"
        done=1
      }' CHANGES.md > CHANGES.md.tmp && mv CHANGES.md.tmp CHANGES.md
  fi
fi

step "Summary"
if [ "${#plan[@]}" -eq 0 ]; then
  say "nothing to change: $current is already prepared"
else
  for p in "${plan[@]}"; do say "- $p"; done
fi
if [ "$dry_run" = true ]; then
  say "dry run: nothing was written"
  exit 0
fi
if [ "${#plan[@]}" -gt 0 ]; then
  git add "$changes_dir" "$menu" CHANGES.md
  git commit -q -m "Prepare the $current release and open $next

The $current changelog page loses its in-development note, $next starts
collecting on the rolling page, and $current is listed under Changelog."
  say "committed: $(git log --oneline -1)"
fi

cat <<NEXT

Next, from the release guide:
  git push origin $branch
  git tag -a v$current -m "Release Apache SkyWalking AI Sessionizer v$current"
  git push origin v$current
  make release VERSION=$current          # dist/: source and binary packages, checksums, signatures
  make release-notes VERSION=$current    # the text for the GitHub release page, after the vote
NEXT
