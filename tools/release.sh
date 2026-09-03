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

# The two halves of a release. Neither touches main directly.
#
#   tools/release.sh prepare [VERSION] [NEXT] [--dry-run] [--skip-check] [--no-push]
#       On a branch release/VERSION cut from the current commit: check the
#       tree, the headers and the suite; finalise VERSION's changelog page;
#       list VERSION under Changelog and in CHANGES.md; write the release
#       notes into docs/en/changes/release-notes-VERSION.md; commit and tag
#       vVERSION on that commit, so the tag carries the notes and the
#       finished changelog. Then open NEXT as the version in development in
#       a second commit, push the branch and the tag, and raise the pull
#       request against main. Both versions are asked for when not given.
#
#   tools/release.sh complete [VERSION] [--dry-run]
#       Create the GitHub release for vVERSION from the notes stored in the
#       tag: not a draft, not a prerelease. CI publishes the image when the
#       released event fires; nothing here waits for it.
#
# --dry-run prints what would change and writes nothing.

set -euo pipefail

usage() { sed -n '21,37p' "$0"; }

cmd="${1:-}"
case "$cmd" in
  prepare|complete) shift ;;
  -h|--help|"") usage; exit 0 ;;
  *) echo "unknown command: $cmd" >&2; usage >&2; exit 2 ;;
esac

version=""
next=""
dry_run=false
skip_check=false
no_push=false
for arg in "$@"; do
  case "$arg" in
    --dry-run) dry_run=true ;;
    --skip-check) skip_check=true ;;
    --no-push) no_push=true ;;
    -*) echo "unknown option: $arg" >&2; exit 2 ;;
    *) if [ -z "$version" ]; then version="$arg"; elif [ -z "$next" ]; then next="$arg"; else echo "too many arguments" >&2; exit 2; fi ;;
  esac
done

say()  { printf '%s\n' "$*"; }
fail() { printf 'release: %s\n' "$*" >&2; exit 1; }
step() { printf '\n== %s\n' "$*"; }
is_version() { printf '%s' "$1" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$'; }
doit() { [ "$dry_run" = false ]; }

cd "$(git rev-parse --show-toplevel)"
changes_dir=docs/en/changes
menu=docs/menu.yml

# list_in_changes puts a line at the top of the version list in CHANGES.md,
# so versions read newest first. The list is the run of lines starting "- [".
list_in_changes() {
  awk -v line="$1" '/^- \[/ && !d {print line; d=1} {print} END {if (!d) {print ""; print line}}' CHANGES.md > CHANGES.md.tmp && mv CHANGES.md.tmp CHANGES.md
}

newest_page() { ls "$changes_dir" | sed -nE 's/^changes-([0-9]+\.[0-9]+\.[0-9]+.*)\.md$/\1/p' | sort -V | tail -1; }

ask() { # ask VAR PROMPT DEFAULT
  local val
  printf '%s [%s]: ' "$2" "$3"
  read -r val
  printf '%s' "${val:-$3}"
}

# ---------------------------------------------------------------- complete
if [ "$cmd" = complete ]; then
  step "The version"
  [ -n "$version" ] || version=$(ask v "Version to release on GitHub" "$(newest_page)")
  is_version "$version" || fail "$version is not of the form MAJOR.MINOR.PATCH"
  tag="v$version"
  notes="$changes_dir/release-notes-$version.md"
  git ls-remote --exit-code --tags origin "refs/tags/$tag" >/dev/null 2>&1 || fail "$tag is not on origin; run prepare and push first"
  git fetch -q origin "refs/tags/$tag:refs/tags/$tag" 2>/dev/null || true
  git cat-file -e "$tag:$notes" 2>/dev/null || fail "$tag does not carry $notes; it was not made by prepare"
  if gh release view "$tag" >/dev/null 2>&1; then fail "the GitHub release $tag already exists"; fi

  step "The release"
  say "tag      : $tag"
  say "title    : $version"
  say "notes    : $notes, as stored in the tag"
  say "----"
  git show "$tag:$notes" | head -12
  say "..."
  if [ "$dry_run" = true ]; then say "dry run: no release created"; exit 0; fi
  tmp=$(mktemp)
  git show "$tag:$notes" > "$tmp"
  gh release create "$tag" --verify-tag --title "$version" --notes-file "$tmp"
  rm -f "$tmp"
  say "created. CI publishes the image when the released event fires; nothing to wait for here."
  exit 0
fi

# ----------------------------------------------------------------- prepare
step "The tree"
[ -z "$(git status --porcelain)" ] || fail "the working tree has changes; start from a clean tree"
from=$(git branch --show-current)
say "cutting the release branch from $from at $(git rev-parse --short HEAD)"

step "The versions"
current=$(newest_page)
[ -n "$version" ] || version=$(ask v "Version to release" "$current")
is_version "$version" || fail "$version is not of the form MAJOR.MINOR.PATCH"
[ -n "$next" ] || next=$(ask v "Next version, where development continues" "$(printf '%s' "$version" | awk -F. '{printf "%d.%d.0", $1, $2 + 1}')")
is_version "$next" || fail "$next is not of the form MAJOR.MINOR.PATCH"
[ "$(printf '%s\n%s\n' "$version" "$next" | sort -V | tail -1)" = "$next" ] && [ "$version" != "$next" ] || fail "next version $next must come after $version"
tag="v$version"
branch="release/$version"
page="$changes_dir/changes-$version.md"
notes="$changes_dir/release-notes-$version.md"
next_page="$changes_dir/changes-$next.md"
! git rev-parse -q --verify "refs/tags/$tag" >/dev/null || fail "tag $tag already exists"
! git rev-parse -q --verify "refs/heads/$branch" >/dev/null || fail "branch $branch already exists"
[ -f "$page" ] || fail "$page does not exist; the version's changelog page is written during its development"
grep -q "path: /en/changes/changes-$version\$" "$menu" || fail "$menu does not point Current Version at $version"
[ ! -f "$next_page" ] || fail "$next_page already exists; $next is already open"
say "releasing $version on $branch, then opening $next"

step "License headers"
make license-check
if [ "$skip_check" = false ]; then
  step "The full check: vet, lint, licenses, tests"
  make check
fi

step "Release $version"
plan=()
if grep -q '^> In development' "$page"; then plan+=("remove the in-development note from $page"); fi
grep -q "^        - name: $version\$" "$menu" || plan+=("list $version under Changelog in $menu, right after Current Version")
if grep -q "^- \[$version\](.*) (in development)\$" CHANGES.md; then plan+=("mark $version released in CHANGES.md")
elif ! grep -q "^- \[$version\]" CHANGES.md; then plan+=("link $version from CHANGES.md"); fi
plan+=("write $notes from the changelog page")
plan+=("commit \"Release $version\" on $branch and tag $tag on it")
for p in "${plan[@]}"; do say "- $p"; done

if doit; then
  git checkout -q -b "$branch"
  if grep -q '^> In development' "$page"; then
    awk 'BEGIN{skip=0} /^> In development/{skip=1} skip&&/^$/{skip=0; next} !skip{print}' "$page" > "$page.tmp" && mv "$page.tmp" "$page"
  fi
  if ! grep -q "^        - name: $version\$" "$menu"; then
    awk -v v="$version" '
      {print}
      /^        - name: Current Version$/ {cv=1; next}
      cv && /^          path:/ {print "        - name: " v; print "          path: /en/changes/changes-" v; cv=0}' "$menu" > "$menu.tmp" && mv "$menu.tmp" "$menu"
  fi
  if grep -q "^- \[$version\](.*) (in development)\$" CHANGES.md; then
    sed -i.bak "s|^- \[$version\](\(.*\)) (in development)\$|- [$version](\1)|" CHANGES.md && rm -f CHANGES.md.bak
  elif ! grep -q "^- \[$version\]" CHANGES.md; then
    list_in_changes "- [$version](docs/en/changes/changes-$version.md)"
  fi
  make -s release-notes VERSION="$version" > "$notes"
  git add "$changes_dir" "$menu" CHANGES.md
  git commit -q -m "Release $version

The $version changelog page loses its in-development note, the version
is listed under Changelog, and its release notes are stored beside it.
The tag goes on this commit."
  git tag -a "$tag" -m "Release Apache SkyWalking AI Sessionizer $tag"
  say "committed $(git rev-parse --short HEAD) and tagged $tag"
fi

step "Open $next"
say "- create $next_page with the in-development note"
say "- point Current Version at $next in $menu"
say "- list $next as in development in CHANGES.md and on the welcome page"
say "- commit \"Open $next\" on $branch"
if doit; then
  cat > "$next_page" <<PAGE
# Changes in $next

> In development, not yet released. \`tools/release.sh prepare $next\` removes this note.

Nothing yet.
PAGE
  awk -v v="$next" '
    /^        - name: Current Version$/ {cv=1; print; next}
    cv && /^          path:/ {print "          path: /en/changes/changes-" v; cv=0; next}
    {print}' "$menu" > "$menu.tmp" && mv "$menu.tmp" "$menu"
  list_in_changes "- [$next](docs/en/changes/changes-$next.md) (in development)"
  sed -i.bak "s|en/changes/changes-[0-9][0-9.A-Za-z-]*\.md|en/changes/changes-$next.md|" docs/README.md && rm -f docs/README.md.bak
  git add "$changes_dir" "$menu" CHANGES.md docs/README.md
  git commit -q -m "Open $next

$next has its changelog page and Current Version points at it. $version
keeps its own page, its entry and its release notes."
  say "committed $(git rev-parse --short HEAD)"
fi

step "Push and pull request"
if [ "$dry_run" = true ]; then say "dry run: nothing was written, pushed or opened"; exit 0; fi
if [ "$no_push" = true ]; then
  cat <<NEXT
not pushed. When ready:
  git push -u origin $branch
  git push origin $tag
  gh pr create --base $from --head $branch --title "Release $version and open $next"
then, once the pull request is merged:
  tools/release.sh complete $version
NEXT
  exit 0
fi
git push -u origin "$branch"
git push origin "$tag"
gh pr create --base "$from" --head "$branch" --title "Release $version and open $next" \
  --body "Finalises the $version changelog, stores its release notes, and opens $next. Tag $tag is on the release commit. After merging, run \`tools/release.sh complete $version\` to create the GitHub release; CI publishes the image on the released event."
say "pushed $branch and $tag; pull request opened. After it merges: tools/release.sh complete $version"
