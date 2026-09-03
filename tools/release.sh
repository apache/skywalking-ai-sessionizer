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

# The two bookends of a release, each one commit, so that the tag carries the
# finished changelog of its version and nothing about the next one.
#
#   tools/release.sh release VERSION [--tag] [--dry-run] [--skip-check]
#       Finalise VERSION: refuse a dirty tree and an existing tag, check the
#       license headers, run make check, remove the in-development note from
#       the version's changelog page, list the version under Changelog and in
#       CHANGES.md, and commit. With --tag, also create the annotated tag on
#       that commit. Pushing, building and voting are the next steps, printed
#       at the end and described in docs/en/guides/how-to-release.md.
#
#   tools/release.sh next VERSION [--pr] [--dry-run]
#       Open VERSION as the version in development, after the release: create
#       its changelog page, point Current Version at it, list it in CHANGES.md,
#       and commit on a branch named open-VERSION. With --pr, push the branch
#       and open the pull request.
#
# --dry-run prints what would change and writes nothing.

set -euo pipefail

usage() { sed -n '21,38p' "$0"; }

cmd="${1:-}"
case "$cmd" in
  release|next) shift ;;
  -h|--help|"") usage; exit "${1:+0}" ;;
  *) echo "unknown command: $cmd" >&2; usage >&2; exit 2 ;;
esac

version=""
dry_run=false
skip_check=false
do_tag=false
do_pr=false
for arg in "$@"; do
  case "$arg" in
    --dry-run) dry_run=true ;;
    --skip-check) skip_check=true ;;
    --tag) do_tag=true ;;
    --pr) do_pr=true ;;
    -*) echo "unknown option: $arg" >&2; exit 2 ;;
    *) [ -z "$version" ] || { echo "too many arguments" >&2; exit 2; }; version="$arg" ;;
  esac
done

say()  { printf '%s\n' "$*"; }
fail() { printf 'release: %s\n' "$*" >&2; exit 1; }
step() { printf '\n== %s\n' "$*"; }
is_version() { printf '%s' "$1" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$'; }

cd "$(git rev-parse --show-toplevel)"
changes_dir=docs/en/changes
menu=docs/menu.yml
plan=()
want() { plan+=("$1"); }
doit() { [ "$dry_run" = false ]; }
# list_in_changes puts a line at the top of the version list in CHANGES.md,
# so versions read newest first. The list is the run of lines starting "- [".
list_in_changes() {
  awk -v line="$1" '/^- \[/ && !d {print line; d=1} {print} END {if (!d) {print ""; print line}}' CHANGES.md > CHANGES.md.tmp && mv CHANGES.md.tmp CHANGES.md
}

step "The tree"
[ -z "$(git status --porcelain)" ] || fail "the working tree has changes; start from a clean tree"
branch=$(git branch --show-current)
say "on branch $branch"

step "The version"
if [ -z "$version" ]; then
  current=$(ls "$changes_dir" | sed -nE 's/^changes-([0-9]+\.[0-9]+\.[0-9]+.*)\.md$/\1/p' | sort -V | tail -1)
  if [ "$cmd" = release ]; then suggested="$current"; else suggested=$(printf '%s' "$current" | awk -F. '{printf "%d.%d.0", $1, $2 + 1}'); fi
  printf 'Version to %s [%s]: ' "$cmd" "$suggested"
  read -r version
  version=${version:-$suggested}
fi
is_version "$version" || fail "$version is not of the form MAJOR.MINOR.PATCH"
page="$changes_dir/changes-$version.md"

if [ "$cmd" = release ]; then
  ! git rev-parse -q --verify "refs/tags/v$version" >/dev/null || fail "tag v$version already exists"
  [ -f "$page" ] || fail "$page does not exist; the version's changelog page is written during its development"
  grep -q "path: /en/changes/changes-$version\$" "$menu" || fail "$menu does not point Current Version at $version"
  say "releasing $version"

  step "License headers"
  make license-check
  if [ "$skip_check" = false ]; then
    step "The full check: vet, lint, licenses, tests"
    make check
  fi

  step "The changelog page"
  if grep -q '^> In development' "$page"; then
    want "remove the in-development note from $page"
    # The note is a blockquote followed by a blank line.
    doit && { awk 'BEGIN{skip=0} /^> In development/{skip=1} skip&&/^$/{skip=0; next} !skip{print}' "$page" > "$page.tmp" && mv "$page.tmp" "$page"; }
  else
    say "$page carries no in-development note"
  fi

  step "The menu"
  # Current Version keeps pointing at this version until the next one opens;
  # the version's own entry is what stays once it has moved on.
  if grep -q "^        - name: $version\$" "$menu"; then
    say "$menu already lists $version"
  else
    want "list $version under Changelog in $menu, right after Current Version"
    doit && { awk -v v="$version" '
      {print}
      /^        - name: Current Version$/ {cv=1; next_path=1; next}
      cv && next_path && /^          path:/ {
        print "        - name: " v
        print "          path: /en/changes/changes-" v
        cv=0; next_path=0
      }' "$menu" > "$menu.tmp" && mv "$menu.tmp" "$menu"; }
  fi

  step "Root CHANGES.md"
  if grep -q "^- \[$version\](.*) (in development)\$" CHANGES.md; then
    want "mark $version released in CHANGES.md"
    doit && { sed -i.bak "s|^- \[$version\](\(.*\)) (in development)\$|- [$version](\1)|" CHANGES.md && rm -f CHANGES.md.bak; }
  elif grep -q "^- \[$version\]" CHANGES.md; then
    say "CHANGES.md already lists $version as released"
  else
    want "link $version from CHANGES.md"
    doit && list_in_changes "- [$version](docs/en/changes/changes-$version.md)"
  fi

  step "Summary"
  if [ "${#plan[@]}" -eq 0 ]; then say "nothing to change: $version is already finalised"; else for p in "${plan[@]}"; do say "- $p"; done; fi
  if [ "$dry_run" = true ]; then say "dry run: nothing was written"; exit 0; fi
  if [ "${#plan[@]}" -gt 0 ]; then
    git add "$changes_dir" "$menu" CHANGES.md
    git commit -q -m "Release $version

The $version changelog page loses its in-development note and the version
is listed under Changelog. The tag goes on this commit."
    say "committed: $(git log --oneline -1)"
  fi
  if [ "$do_tag" = true ]; then
    git tag -a "v$version" -m "Release Apache SkyWalking AI Sessionizer v$version"
    say "tagged: v$version at $(git rev-parse --short HEAD)"
  fi
  cat <<NEXT

Next, from the release guide:
  git push origin $branch
$([ "$do_tag" = true ] || printf '  git tag -a v%s -m "Release Apache SkyWalking AI Sessionizer v%s"\n' "$version" "$version")  git push origin v$version
  make release VERSION=$version          # dist/: source and binary packages, checksums, signatures
  make release-notes VERSION=$version    # the text for the GitHub release page, after the vote
  tools/release.sh next <next version> --pr    # after the tag: open the next version
NEXT
  exit 0
fi

# next
[ ! -f "$page" ] || fail "$page already exists; $version is already open"
prev=$(ls "$changes_dir" | sed -nE 's/^changes-([0-9]+\.[0-9]+\.[0-9]+.*)\.md$/\1/p' | sort -V | tail -1)
if [ -n "$prev" ] && [ "$(printf '%s\n%s\n' "$prev" "$version" | sort -V | tail -1)" != "$version" ]; then
  fail "$version does not come after $prev"
fi
say "opening $version after $prev"

step "The changelog page"
want "create $page with the in-development note"
doit && cat > "$page" <<PAGE
# Changes in $version

> In development, not yet released. \`tools/release.sh release $version\` removes this note.

Nothing yet.
PAGE

step "The menu"
want "point Current Version at $version in $menu"
doit && { awk -v v="$version" '
  /^        - name: Current Version$/ {cv=1; print; next}
  cv && /^          path:/ {print "          path: /en/changes/changes-" v; cv=0; next}
  {print}' "$menu" > "$menu.tmp" && mv "$menu.tmp" "$menu"; }

step "Root CHANGES.md"
want "list $version as in development in CHANGES.md"
doit && list_in_changes "- [$version](docs/en/changes/changes-$version.md) (in development)"

step "The welcome page"
want "point the changelog link in docs/README.md at $version"
doit && { sed -i.bak "s|en/changes/changes-[0-9][0-9.A-Za-z-]*\.md|en/changes/changes-$version.md|" docs/README.md && rm -f docs/README.md.bak; }

step "Summary"
for p in "${plan[@]}"; do say "- $p"; done
if [ "$dry_run" = true ]; then say "dry run: nothing was written"; exit 0; fi
git checkout -q -b "open-$version"
git add "$changes_dir" "$menu" CHANGES.md docs/README.md
git commit -q -m "Open $version

$version has its changelog page and Current Version points at it. The
released versions keep their own pages and entries."
say "committed on branch open-$version: $(git log --oneline -1)"
if [ "$do_pr" = true ]; then
  git push -u origin "open-$version"
  gh pr create --title "Open $version" --body "Opens $version as the version in development: its changelog page, Current Version in the menu, and the root list." --base main
else
  cat <<NEXT

Next:
  git push -u origin open-$version
  gh pr create --title "Open $version" --base main
NEXT
fi
