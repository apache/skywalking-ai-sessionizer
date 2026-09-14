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

# The stages of a release, in the order they run. None of them pushes to main.
#
#   tools/release.sh prepare [VERSION] [NEXT] [--dry-run] [--skip-check] [--no-push]
#       On a branch release/VERSION cut from the current commit: check the
#       tree, the headers and the suite. docs/en/changes/changes.md is the
#       changelog of the version in development, and must name VERSION in
#       its heading and carry the in-development note. Remove the note,
#       commit, and tag vVERSION on that commit. The tag keeps the finished
#       changelog at changes.md, the page its menu and its welcome page
#       link, because the website publishes the docs of each version from
#       its tag. Then, in a second commit, move changes.md to
#       changes-VERSION.md, list VERSION under Changelog in docs/menu.yml,
#       and write a new changes.md for NEXT. Push the branch and the tag,
#       and raise the pull request against main. CI on the tag push builds
#       and tests every package, then creates the GitHub prerelease and
#       attaches the binary archives with their .sha512 files. Nothing here
#       waits for it. Both versions are asked for when not given, and the
#       heading of changes.md gives the offered VERSION. A dry run installs
#       no tool: a check whose tool is not in bin/ yet is listed, not run.
#
#   tools/release.sh candidate [VERSION] [--ci-run RUN_ID] [--dry-run] [--no-upload]
#       After CI has attached the binaries to the prerelease: download those
#       exact archives, archive the tagged source locally, sign and verify
#       every package in dist/VERSION, run the binary package for this
#       machine with the tag's tools/package-smoke.sh, upload the candidate
#       to the dev area of dist.apache.org, attach the source package, its
#       .sha512 and every .asc to the GitHub prerelease, and write the vote
#       mail to dist/VERSION/vote.txt. The signing key must be RSA of at
#       least 2048 bits, carry an apache.org user ID, and be in the
#       SkyWalking KEYS file. GPG_USER picks the key; empty means gpg's
#       default key. --ci-run checks the uploader run named by the
#       prerelease's readiness marker. Without it, use that marker's run.
#       Never rebuild binaries here. APACHE_ID, when set, is the name svn
#       logs in with. When the candidate is on dist.apache.org already and
#       this checkout's dist/VERSION holds the same files, candidate signs
#       and uploads nothing again: it attaches what the prerelease is
#       missing and writes the vote mail. --no-upload prepares, verifies
#       and runs the package, uploads nothing, and writes the mail to
#       dist/VERSION/vote-preview.txt. It refuses once a candidate of
#       VERSION is uploaded.
#
#   tools/release.sh publish [VERSION] [--dry-run] [--remove-old]
#       Run by a PMC member after the vote passed. Verify the candidate's
#       signatures against KEYS and move it from the dev area to the release
#       directory of dist.apache.org. The move publishes the voted packages.
#       With the vote and the announcement, it makes the release. Then check
#       that the GitHub prerelease holds exactly the voted files, attach any
#       that are missing, and promote it to a full release; CI publishes the
#       image when the released event fires. Then write the announcement,
#       the website entries and the install manifests into dist/VERSION,
#       and print what is left to do. A run after the move skips the move
#       and runs every later step again. --remove-old removes older versions
#       from the release directory, and archive.apache.org keeps them. It is
#       refused in the run that moves: run publish again with it once the
#       website links the older versions from the archive.
#
# When VERSION is not given, candidate and publish offer the newest version
# with a page docs/en/changes/changes-X.Y.Z.md. prepare gives a version that
# page in the commit after the tag, and main holds it once the prepare pull
# request has merged.
#
# --dry-run prints what the stage would do. It makes no commit and no push,
# changes nothing on dist.apache.org or GitHub, and writes no file in dist/.
# candidate and publish read the tag to make the plan, so, like a real run,
# they fetch vVERSION from origin into this repository when it does not have
# the tag. A candidate dry run also signs a scratch file and imports KEYS, in
# a temporary directory that it removes.

set -euo pipefail

# usage prints the comment above, down to the blank line after it.
usage() { sed -n '/^# The stages of a release/,/^$/p' "$0" | sed -E '/^$/d; s/^# ?//'; }

cmd="${1:-}"
case "$cmd" in
  prepare)     options="--dry-run --skip-check --no-push" ;;
  candidate)   options="--dry-run --no-upload --ci-run" ;;
  publish)     options="--dry-run --remove-old" ;;
  -h|--help|"") usage; exit 0 ;;
  *) echo "unknown command: $cmd" >&2; usage >&2; exit 2 ;;
esac
shift

version=""
next=""
dry_run=false
skip_check=false
no_push=false
no_upload=false
ci_run=auto
remove_old=false
while [ $# -gt 0 ]; do
  arg="$1"
  shift
  value=""
  given=false
  case "$arg" in
    -h|--help) usage; exit 0 ;;
    --*=*) value="${arg#*=}"; arg="${arg%%=*}"; given=true ;;
  esac
  case "$arg" in
    -*)
      # An option of another stage is refused, never ignored: publish
      # --no-upload must not publish while its user thinks it will not.
      case " $options " in
        *" $arg "*) ;;
        *) echo "$cmd does not take $arg" >&2; exit 2 ;;
      esac ;;
  esac
  case "$arg" in
    --ci-run)
      needs="the numeric ID of a successful CI run for the release tag"
      # The next argument is taken only when no = was given, and never when
      # it is an option: "--ci-run= --dry-run" must not read --dry-run as
      # the run.
      if [ "$given" = false ]; then
        case "${1:-}" in ""|-*) echo "$arg needs $needs" >&2; exit 2 ;; esac
        value="$1"
        shift
      fi
      [ -n "$value" ] || { echo "$arg needs $needs" >&2; exit 2; }
      [ "$ci_run" = auto ] || { echo "--ci-run is given twice" >&2; exit 2; }
      [[ "$value" =~ ^[1-9][0-9]*$ ]] || { echo "--ci-run needs a positive integer" >&2; exit 2; }
      ci_run="$value" ;;
    -*)
      [ "$given" = false ] || { echo "$arg takes no value" >&2; exit 2; }
      case "$arg" in
        --dry-run) dry_run=true ;;
        --skip-check) skip_check=true ;;
        --no-push) no_push=true ;;
        --no-upload) no_upload=true ;;
        --remove-old) remove_old=true ;;
      esac ;;
    *)
      if [ -z "$version" ]; then version="$arg"
      elif [ -z "$next" ] && [ "$cmd" = prepare ]; then next="$arg"
      else echo "too many arguments" >&2; exit 2; fi ;;
  esac
done

say()  { printf '%s\n' "$*"; }
fail() { printf 'release: %s\n' "$*" >&2; exit 1; }
step() { printf '\n== %s\n' "$*"; }
is_version() { printf '%s' "$1" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$'; }
doit() { [ "$dry_run" = false ]; }

cd "$(git rev-parse --show-toplevel)"
changes_dir=docs/en/changes
# On main, the changelog of the version in development is always at this
# path, so Current Version in the menu and the welcome page link it once and
# never change at a release. The tag keeps the finished changelog of its
# version at this path too, because the website publishes the docs of each
# version from its tag, and there the same links reach it. prepare moves it
# to changes-VERSION.md in the commit after the tag.
dev_page=$changes_dir/changes.md
menu=docs/menu.yml
project="Apache SkyWalking AI Sessionizer"
github=https://github.com/apache/skywalking-ai-sessionizer
pkg=apache-skywalking-ai-sessionizer
dist_dev=https://dist.apache.org/repos/dist/dev/skywalking
dist_release=https://dist.apache.org/repos/dist/release/skywalking
keys_url="$dist_release/KEYS"
downloads=https://downloads.apache.org/skywalking/ai-sessionizer
# The ASF rules for download pages require closer.lua. closer.cgi only
# redirects to it now.
closer=https://www.apache.org/dyn/closer.lua/skywalking/ai-sessionizer
# The ASF does not allow a Category B work in a source release. Fonts are
# the Category B works this project carries: the renderer's two fonts are
# under the SIL Open Font License. A font in the source package is refused
# by its name, because file(1) often reports a web font only as data.
# The source archive is checked before it is signed or uploaded.
font_files='\.(woff2?|ttf|otf|eot)$'

# newest_page is the newest version prepare has finished, since only
# prepare gives a version its page changes-VERSION.md. It does so in the
# commit after the tag, which main holds once the prepare pull request has
# merged. On main, changes.md is the version in development, never one to
# build, publish or release on GitHub.
newest_page() { ls "$changes_dir" | sed -nE 's/^changes-([0-9]+\.[0-9]+\.[0-9]+.*)\.md$/\1/p' | sort -V | tail -1; }

# page_version prints the version a changelog page on standard input names
# in its heading, as in "# Changes in 0.4.0". Only the first top-level
# heading counts, because it is the title of the page. awk reads to the
# end rather than exit at the heading: a writer into a pipe that closed
# early fails, and pipefail reports that as a failure of the pipe.
page_version() {
  awk '!seen && /^# / {seen = 1; if (sub(/^# Changes in /, "")) {sub(/[ \t\r]+$/, ""); print}}'
}

# has_note says whether a changelog page on standard input carries the
# in-development note. It reads to the end too, for the same reason.
has_note() { awk '/^> In development/ {n = 1} END {exit !n}'; }

ask() { # ask VAR PROMPT DEFAULT
  # The prompt goes to stderr. Every caller captures stdout, and a prompt
  # there became part of the answer, which then never looked like a version.
  local val=""
  printf '%s [%s]: ' "$2" "$3" >&2
  read -r val || true
  printf '%s' "${val:-$3}"
}

# has_line LIST LINE says whether LIST holds LINE as a whole line. It is a
# pattern match, not grep -q in a pipe: grep -q stops reading at the first
# match, and pipefail then reports the writer's broken pipe as a failure.
has_line() {
  case "
$1
" in *"
$2
"*) return 0 ;; esac
  return 1
}

# has_prefix LIST PREFIX says whether a line of LIST starts with PREFIX
# and goes on after it.
has_prefix() {
  case "
$1" in *"
$2"?*) return 0 ;; esac
  return 1
}

need_tools() {
  local t missing=""
  for t in "$@"; do command -v "$t" >/dev/null 2>&1 || missing="$missing $t"; done
  [ -z "$missing" ] || fail "missing tools:$missing"
}

# keys_verify KEYRING FINGERPRINT ASC FILE verifies a signature the way a
# voter does, against KEYRING, and prints what is wrong with it. It prints
# nothing when gpg reports a good signature by the key whose primary key is
# FINGERPRINT, and does not report the key that signed, or the signature,
# as expired or revoked. The ASF counts a signature as valid only when gpg
# verifies it as a good signature and does not complain about expired or
# revoked keys. For a key that has expired or is revoked in KEYRING, gpg
# still writes VALIDSIG and exits 0. It writes EXPKEYSIG or REVKEYSIG in
# place of GOODSIG, and it writes EXPSIG for a signature that has expired.
# So the signature needs GOODSIG, and any of those three lines fails it, as
# BADSIG and ERRSIG do. KEYEXPIRED and KEYREVOKED are not about the
# signature: gpg writes them for any key in the key block that has expired
# or is revoked. With gpg 2.5.18, a KEYS entry that held an old signing
# subkey that had expired, and a newer subkey that signed, gave KEYEXPIRED
# beside GOODSIG, and a voter read only "Good signature". So those two lines
# fail nothing here. candidate refuses an expired or revoked primary key or
# signing subkey with key_state, before this runs.
keys_verify() {
  local st by bad
  st=$(gpg --batch --homedir "$1" --status-fd 1 --verify "$3" "$4" 2>/dev/null || true)
  by=$(printf '%s\n' "$st" | awk '$2 == "VALIDSIG" && !f {f = ($12 != "" ? $12 : $3)} END {print f}')
  bad=$(printf '%s\n' "$st" | awk '$2 ~ /^(BADSIG|ERRSIG|EXPSIG|EXPKEYSIG|REVKEYSIG)$/ && !seen[$2]++ {printf "%s%s", (n++ ? ", " : ""), $2}')
  if [ -n "$bad" ]; then printf 'gpg reports %s' "$bad"
  elif [ -z "$by" ]; then printf 'gpg reports no valid signature'
  elif [ "$by" != "$2" ]; then printf 'the signature is by %s, not by %s' "$by" "$2"
  elif ! printf '%s\n' "$st" | awk '$2 == "GOODSIG" {g = 1} END {exit !g}'; then printf 'gpg does not report a good signature'
  fi
  return 0
}

# asf_svn logs in as APACHE_ID when it is set, for a person whose local
# user name is not their Apache ID.
asf_svn() {
  if [ -n "${APACHE_ID:-}" ]; then svn --username "$APACHE_ID" "$@"; else svn "$@"; fi
}

# svn_root lists a directory that must exist, so a network or access
# failure stops the run instead of reading as an empty directory.
svn_root() {
  asf_svn ls "$1" 2>/dev/null | sed 's|/$||' || fail "cannot list $1; check the network and your svn access"
}

# svn_list prints the entries of an svn directory, one per line without
# the trailing slash, and nothing when the directory does not exist.
svn_list() {
  { asf_svn ls "$1" 2>/dev/null || true; } | sed 's|/$||'
}

# fetch_tag checks that vVERSION is on origin, and that a local tag of the
# same name, if there is one, is the same object. The vote names the tag on
# origin, so a local tag that differs would build or describe something else.
# When there is no local tag, it fetches that one, in a dry run too, because
# every stage that calls it reads the tag.
# The tag must hold the finished changelog of the version at changes.md: its
# heading names the version and the in-development note is gone. The docs
# the website publishes from the tag link that page, the vote mail and the
# announcement link it, and publish builds the text of the GitHub release
# from it. A tag prepare did not make fails here, before anything is built.
fetch_tag() {
  local refs remote_id local_id tag_page
  refs=$(git ls-remote --tags origin "refs/tags/$tag") || fail "cannot list the tags on origin"
  remote_id=$(printf '%s\n' "$refs" | awk -v r="refs/tags/$tag" '$2 == r {print $1}')
  [ -n "$remote_id" ] || fail "$tag is not on origin; run prepare and merge its pull request first"
  if local_id=$(git rev-parse -q --verify "refs/tags/$tag"); then
    [ "$local_id" = "$remote_id" ] || fail "the local tag $tag is $local_id but origin's is $remote_id. Remove the local one with 'git tag -d $tag' and run again"
  else
    # Only this tag. Without --no-tags, git also fetches every other tag on
    # origin that points into its history.
    git fetch -q --no-tags origin "refs/tags/$tag:refs/tags/$tag" || fail "cannot fetch $tag from origin"
    say "fetched $tag from origin into this repository, which did not have it"
  fi
  tag_page=$(git show "$tag:$dev_page" 2>/dev/null) || fail "$tag does not carry $dev_page, so prepare did not make it"
  [ "$(printf '%s\n' "$tag_page" | page_version)" = "$version" ] || fail "$dev_page in $tag does not name $version in its heading, so prepare did not make $tag. The heading must read '# Changes in $version'"
  if printf '%s\n' "$tag_page" | has_note; then fail "$dev_page in $tag still carries the in-development note, so prepare did not make $tag"; fi
  commit=$(git rev-parse "$tag^{commit}")
}

# The packages of a version are the ones the Makefile in its tag builds:
# the source package, and one binary package per entry in PLATFORMS. The
# tag is read rather than the working tree, so a platform added later is
# never demanded of an older version.
tag_platforms() {
  local list t
  list=$(git show "$tag:Makefile" | sed -nE 's/^PLATFORMS[[:space:]]*:?=[[:space:]]*//p')
  [ -n "$list" ] || fail "the Makefile in $tag has no PLATFORMS"
  for t in $list; do printf '%s\n' "$t"; done
}

binary_package() { # binary_package OS/ARCH
  local os=${1%/*} arch=${1#*/} ext=tgz
  if [ "$os" = windows ]; then ext=zip; fi
  printf '%s' "$pkg-$version-bin-$os-$arch.$ext"
}

# host_platform prints the PLATFORMS entry of the machine running this, such
# as darwin/arm64. It prints nothing for a system or a processor that no
# package is built for. Git Bash on Windows names the system MINGW64_NT-10.0
# or similar, and an MSYS2 shell names it MSYS_NT-10.0.
host_platform() {
  local os arch
  case "$(uname -s)" in
    Darwin) os=darwin ;;
    Linux) os=linux ;;
    MINGW*|MSYS*) os=windows ;;
    *) return 0 ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) return 0 ;;
  esac
  printf '%s/%s' "$os" "$arch"
}

expected_packages() {
  local t
  printf '%s\n' "$pkg-$version-src.tgz"
  for t in $platforms; do printf '%s\n' "$(binary_package "$t")"; done
}

pick_version() { # pick_version PROMPT
  [ -n "$version" ] || version=$(ask v "$1" "$(newest_page)")
  is_version "$version" || fail "'$version' is not of the form MAJOR.MINOR.PATCH"
  tag="v$version"
  out="dist/$version"
}

release_repo=apache/skywalking-ai-sessionizer

# ci_names prints the files CI attaches to the prerelease: each binary
# package and its .sha512. The release stages never upload these.
ci_names() {
  local t b
  for t in $platforms; do
    b=$(binary_package "$t")
    printf '%s\n%s\n' "$b" "$b.sha512"
  done
}

# release_names prints every file the GitHub release holds once candidate
# has attached its part: each package with its .asc and .sha512.
release_names() {
  local p
  for p in $packages; do printf '%s\n%s\n%s\n' "$p" "$p.asc" "$p.sha512"; done
}

# release_identity prints the GitHub release of the tag as "ID TAG DRAFT
# PRERELEASE TITLE", separated by tabs.
release_identity() {
  gh release view "$tag" --repo "$release_repo" --json databaseId,tagName,isDraft,isPrerelease,name \
    --jq '[.databaseId, .tagName, .isDraft, .isPrerelease, .name] | @tsv'
}

# sync_release_assets DIR HINT makes the GitHub release of the tag hold
# exactly release_names, each byte for byte the file of that name in DIR.
# CI's files must be there already. A signature or source file that is
# there must be identical, and one that is missing is uploaded. Nothing is
# ever replaced: a conflicting asset is reported for a person to remove.
# HINT tells how to resume after a failure. It uses $scratch.
sync_release_assets() {
  local dir=$1 hint=$2 names existing f dup missing=()
  names=$(release_names)
  existing=$(gh release view "$tag" --repo "$release_repo" --json assets --jq '.assets[].name') \
    || fail "cannot list the assets of the GitHub release $tag. $hint"
  dup=$(printf '%s\n' "$existing" | LC_ALL=C sort | uniq -d)
  [ -z "$dup" ] || fail "the GitHub release $tag holds more than one asset named $dup. Remove the extra one, then run again"
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    has_line "$names" "$f" || fail "the GitHub release $tag holds an unexpected asset, $f. Check why it is there, remove it with 'gh release delete-asset $tag $f --repo $release_repo', then run again"
  done <<< "$existing"
  for f in $(ci_names); do
    has_line "$existing" "$f" || fail "the GitHub release $tag has no $f. CI attaches each binary package and its .sha512, and the release stages never upload them"
  done
  rm -rf "$scratch/github"
  mkdir -p "$scratch/github"
  for f in $names; do
    if ! has_line "$existing" "$f"; then
      missing+=("$dir/$f")
    elif ! has_line "$(ci_names)" "$f"; then
      # A file attached by an earlier run must be the one in DIR. CI's own
      # files are compared with the rest below, after a single download.
      gh release download "$tag" --repo "$release_repo" --pattern "$f" --dir "$scratch/github/one" \
        || fail "cannot download $f from the GitHub release $tag. $hint"
      cmp -s "$dir/$f" "$scratch/github/one/$f" || fail "the GitHub release's $f differs from $dir/$f. Check why, remove it with 'gh release delete-asset $tag $f --repo $release_repo', then run again"
    fi
  done
  if [ "${#missing[@]}" -gt 0 ]; then
    gh release upload "$tag" --repo "$release_repo" "${missing[@]}" \
      || fail "the upload to the GitHub release $tag stopped. $hint"
    say "attached ${#missing[@]} files to the GitHub release $tag"
  fi
  existing=$(gh release view "$tag" --repo "$release_repo" --json assets --jq '.assets[].name') \
    || fail "cannot list the assets of the GitHub release $tag. $hint"
  [ "$(printf '%s\n' "$existing" | LC_ALL=C sort)" = "$(printf '%s\n' "$names" | LC_ALL=C sort)" ] \
    || fail "the GitHub release $tag does not hold exactly the voted files. $hint"
  gh release download "$tag" --repo "$release_repo" --dir "$scratch/github/all" \
    || fail "cannot download the GitHub release $tag to verify it. $hint"
  for f in $names; do
    cmp -s "$dir/$f" "$scratch/github/all/$f" || fail "the GitHub release's $f differs from $dir/$f. Remove it with 'gh release delete-asset $tag $f --repo $release_repo', then run again"
  done
  say "ok  the GitHub release $tag holds exactly the $(printf '%s\n' "$names" | wc -l | tr -d ' ') voted files"
}

# --------------------------------------------------------------- candidate
if [ "$cmd" = candidate ]; then
  step "The version"
  pick_version "Version to prepare the release candidate for"
  tools="git gpg shasum tar gzip unzip file curl gh python3 sh"
  if [ "$no_upload" = false ]; then tools="$tools svn"; fi
  # shellcheck disable=SC2086 # the list is split on purpose
  need_tools $tools
  ci_helper=tools/ci-binaries.sh
  package_check=tools/package-check.sh
  [ -f "$ci_helper" ] || fail "$ci_helper is missing"
  [ -f "$package_check" ] || fail "$package_check is missing"
  fetch_tag
  platforms=$(tag_platforms)
  packages=$(expected_packages)
  origin_url=$(git remote get-url origin)
  dev_dir="$dist_dev/ai-sessionizer/$version"
  compiled=$(git show "$tag:Makefile" | sed -nE 's/^COMPILED_TYPES[[:space:]]*:?=[[:space:]]*//p')
  [ -n "$compiled" ] || fail "the Makefile in $tag has no COMPILED_TYPES, the file types a source package must not carry"
  compiled_files=$(git show "$tag:Makefile" | sed -nE 's/^COMPILED_FILES[[:space:]]*:?=[[:space:]]*//p')
  [ -n "$compiled_files" ] || fail "the Makefile in $tag has no COMPILED_FILES, the names of compiled files a source package must not carry"
  # CI runs each package on its platform, and the release manager runs the
  # package for this machine again before upload. The script and the scenarios
  # come from the tag, not the working tree, because a scenario newer than
  # the tag may use what the tag's binary does not have.
  smoke=tools/package-smoke.sh
  host=$(host_platform)
  host_pkg=""
  if [ -n "$host" ] && has_line "$platforms" "$host"; then
    host_pkg=$(binary_package "$host")
    git cat-file -e "$tag:$smoke" 2>/dev/null || fail "$tag has no $smoke, so the package for this machine cannot be run before the upload"
  fi
  say "tag      : $tag, on origin, at commit $commit"
  say "origin   : $origin_url"
  say "packages : the source package and one binary package for each of $(printf '%s\n' "$platforms" | awk 'NR>1{printf " "} {printf "%s", $0}')"
  if [ -n "$host_pkg" ]; then say "machine  : $host, so $host_pkg is run before the upload"
  else say "machine  : $(uname -s) $(uname -m), which no package is built for, so none is run here"; fi

  # The source package is git archive of the tag, which leaves out what the
  # tag's .gitattributes marks export-ignore. So its listing is known now,
  # before a key is asked for or any package is created.
  fonts=$(git archive --format=tar "$tag" | tar -tf - | grep -E "$font_files" || true)
  [ -z "$fonts" ] || fail "the source package of $tag would hold font files:
$fonts
Fonts are under licenses such as the SIL Open Font License, which the ASF puts in Category B, and the ASF does not allow a Category B work in a source release. Mark them export-ignore in .gitattributes, or have the PMC settle it with legal@apache.org first"
  say "fonts    : none in the source package"

  if [ "$no_upload" = true ]; then
    # --no-upload writes into dist/$version too. After an upload it could
    # replace files that are being voted on; even signing the same archive
    # again produces a different signature.
    [ ! -f "$out/vote.txt" ] || fail "$out/vote.txt is there, so a candidate of $version was uploaded from this checkout. --no-upload would replace the uploaded files. Use another checkout for a preview, or remove $out after withdrawing the uploaded candidate"
    step "The candidate directory"
    if ! command -v svn >/dev/null 2>&1; then
      say "svn is not installed, so whether a candidate of $version is uploaded was not checked"
    elif dev_root=$(asf_svn ls "$dist_dev" 2>/dev/null); then
      if has_line "$(printf '%s\n' "$dev_root" | sed 's|/$||')" ai-sessionizer && has_line "$(svn_list "$dist_dev/ai-sessionizer")" "$version"; then
        fail "a candidate of $version is uploaded already, in $dev_dir. --no-upload would replace files that are already being voted on"
      fi
      # publish on another machine fills dist/$version with the voted files
      # and no vote.txt, so a released version is refused here too.
      if has_line "$(svn_list "$dist_release/ai-sessionizer")" "$version"; then
        fail "$version is released already, in $dist_release/ai-sessionizer/$version. --no-upload would replace the voted files in $out with newly signed packages"
      fi
      say "no candidate of $version is uploaded"
    else
      say "cannot list $dist_dev, so whether a candidate of $version is uploaded was not checked"
    fi
  fi

  dev_has_dir=false
  # resume is a candidate this checkout signed and uploaded, whose run
  # stopped before the GitHub prerelease held it. Signing the same archive
  # again gives another signature, and the vote is about the uploaded one,
  # so such a run attaches and writes the mail without signing again.
  resume=false
  if [ "$no_upload" = false ]; then
    step "The candidate directory"
    dev_root=$(svn_root "$dist_dev")
    release_root=$(svn_root "$dist_release")
    if has_line "$release_root" ai-sessionizer && has_line "$(svn_list "$dist_release/ai-sessionizer")" "$version"; then
      fail "$version is released already: $dist_release/ai-sessionizer/$version exists"
    fi
    if has_line "$dev_root" ai-sessionizer; then
      dev_has_dir=true
      if has_line "$(svn_list "$dist_dev/ai-sessionizer")" "$version"; then
        uploaded=$(svn_list "$dev_dir")
        same=true
        for p in $packages; do
          for f in "$p" "$p.asc" "$p.sha512"; do
            if ! has_line "$uploaded" "$f" || [ ! -f "$out/$f" ]; then same=false; fi
          done
        done
        if [ "$same" = true ]; then
          for p in $packages; do
            [ "$(asf_svn cat "$dev_dir/$p.sha512")" = "$(cat "$out/$p.sha512")" ] || same=false
            [ "$(asf_svn cat "$dev_dir/$p.asc")" = "$(cat "$out/$p.asc")" ] || same=false
            (cd "$out" && shasum -a 512 --status -c "$p.sha512") || same=false
          done
        fi
        [ "$same" = true ] || fail "$dev_dir exists, and $out does not hold the same files, so a candidate of $version was uploaded before, from another checkout or with other files. To replace it with a new candidate, remove it first:
  svn rm -m \"Remove the Apache SkyWalking AI Sessionizer $version candidate for a new one\" $dev_dir"
        resume=true
        say "$dev_dir holds the candidate that $out holds: it is signed and uploaded already"
      else
        say "$dist_dev/ai-sessionizer has no $version yet"
      fi
    else
      say "$dist_dev/ai-sessionizer does not exist; the upload creates it, since this is the first candidate"
    fi
  fi

  work=$(mktemp -d)
  scratch="$work"
  keyring="$work/keyring"
  cleanup() {
    # gpg can start helpers for the scratch keyring. Stop them with it.
    if command -v gpgconf >/dev/null 2>&1; then gpgconf --homedir "$keyring" --kill all >/dev/null 2>&1 || true; fi
    rm -rf "$work"
  }
  trap cleanup EXIT
  mkdir -m 700 "$keyring"
  step "The CI binary packages"
  if [ "$dry_run" = true ]; then
    say "- $ci_helper $version $commit $ci_run <temporary directory> <platforms from $tag>"
    say "- read the prerelease's readiness marker, require its successful uploader run for $tag at $commit, and verify asset digests, package checksums and archive contents"
    say "dry run: no GitHub prerelease asset was downloaded or verified"
  else
    bash "$ci_helper" "$version" "$commit" "$ci_run" "$work/ci" "$platforms" | tee "$work/ci-check.txt" \
      || fail "no verified CI binary packages are available. Wait for the CI run of the $tag push to create the prerelease and attach the binaries. If a job failed, rerun it; if the prerelease is incomplete or was rejected, remove it explicitly first, as the release guide describes. --ci-run must match its successful uploader run"
    if [ "$resume" = true ]; then
      for t in $platforms; do
        p=$(binary_package "$t")
        for f in "$p" "$p.sha512"; do
          cmp -s "$work/ci/$f" "$out/$f" || fail "$out/$f is not the file CI attached to the prerelease, so the uploaded candidate is not the one CI built. Remove $dev_dir and run candidate again"
        done
      done
      say "ok  the uploaded binary packages are the ones CI attached"
    fi
  fi
  step "The signing key"
  if [ -z "${GPG_TTY:-}" ] && [ -t 0 ]; then GPG_TTY=$(tty); export GPG_TTY; fi
  # Sign a scratch file the way candidate signs a package, and read the
  # signer's fingerprint from gpg's status output. A voter checks every
  # signature against KEYS, so a key missing there fails the vote however
  # good the packages are. That is found out here, before the packages are signed.
  printf 'signing check for %s %s\n' "$project" "$version" > "$work/check"
  if [ -n "${GPG_USER:-}" ]; then
    gpg --yes --armor --detach-sign --local-user "$GPG_USER" --output "$work/check.asc" "$work/check" || fail "gpg cannot sign with $GPG_USER. If it asked for no passphrase, try: export GPG_TTY=\$(tty)"
  else
    gpg --yes --armor --detach-sign --output "$work/check.asc" "$work/check" || fail "gpg cannot sign with its default key; set GPG_USER to the key to use"
  fi
  status=$(gpg --batch --status-fd 1 --verify "$work/check.asc" "$work/check" 2>/dev/null) || fail "gpg cannot verify its own signature"
  # VALIDSIG names the key that signed in field 3, a subkey when the
  # primary key does not sign, and the primary key in field 12. KEYS lists
  # a person's key by its primary key.
  signed_with=$(printf '%s\n' "$status" | awk '$2 == "VALIDSIG" && !f {f = $3} END {print f}')
  fpr=$(printf '%s\n' "$status" | awk '$2 == "VALIDSIG" && !f {f = ($12 != "" ? $12 : $3)} END {print f}')
  [ -n "$fpr" ] || fail "gpg did not say which key signed"
  curl -fsSL "$keys_url" -o "$work/KEYS" || fail "cannot download $keys_url"
  # KEYS is read the way a voter reads it: imported into an empty keyring.
  # Some entries carry no fingerprint in text, so a text search is not enough.
  gpg --batch --quiet --homedir "$keyring" --import "$work/KEYS" >/dev/null 2>&1 || true
  keys=$(gpg --batch --homedir "$keyring" --with-colons --list-keys 2>/dev/null || true)
  known=$(printf '%s\n' "$keys" | awk -F: '$1 == "fpr" {print $10}')
  has_line "$known" "$fpr" || fail "the signing key $fpr is not in $keys_url. Add it there first; only a PMC member can commit to that file"
  # In gpg's key listing a pub or sub record carries the key's validity in
  # field 2, its length in field 3 and its algorithm in field 4. The fpr
  # record after it names that key. awk reads to the end rather than exit
  # at the key, as page_version does. It once exited there, and with more
  # than a pipe buffer of listing after the key, printf wrote into a closed
  # pipe and pipefail stopped candidate.
  key_record() { # key_record FINGERPRINT prints "VALIDITY ALGORITHM LENGTH"
    printf '%s\n' "$keys" | awk -F: -v f="$1" '
      $1 == "pub" || $1 == "sub" {valid = $2; len = $3; algo = $4; next}
      $1 == "fpr" && $10 == f && !done {print valid, algo, len; done = 1}'
  }
  key=$(key_record "$signed_with")
  [ -n "$key" ] || fail "the key that signed, $signed_with, is not in the KEYS keyring"
  # The ASF counts a signature as valid only when gpg reports it good and
  # does not complain about an expired or revoked key. gpg marks a key as
  # expired with e and as revoked with r. This keyring holds KEYS alone, so
  # what gpg marks here is what every voter's gpg reports, however valid the
  # key is on this machine. The usual case is a key extended here while
  # KEYS holds the old copy. The primary key and the subkey that signs both
  # count. A revoked primary key was once refused only because its user IDs
  # were revoked too, with a message about a missing apache.org address.
  key_state() { # key_state VALIDITY NAME
    case "$1" in
      e) fail "$2 has expired in $keys_url. Every voter's gpg would warn that the key has expired, and the ASF does not count such a signature as valid. If you extended the key, have a PMC member commit the renewed public key to KEYS first" ;;
      r) fail "$2 is revoked in $keys_url. Every voter's gpg would warn that it is revoked, and the ASF does not count such a signature as valid. Sign with a key that is not revoked, and add that key to KEYS" ;;
    esac
  }
  primary=$(key_record "$fpr")
  key_state "${primary%% *}" "the signing key $fpr"
  if [ "$signed_with" != "$fpr" ]; then key_state "${key%% *}" "the subkey $signed_with, which signs for $fpr,"; fi
  # Then the scratch signature is read as a voter reads a package's, so
  # anything else gpg would complain about is found before the packages are signed too.
  why=$(keys_verify "$keyring" "$fpr" "$work/check.asc" "$work/check")
  [ -z "$why" ] || fail "a signature by $signed_with does not verify cleanly against $keys_url: $why. Every voter would see the same"
  # The ASF requires a release signing key to be RSA of at least 2048 bits,
  # and asks for 4096 bits in a new key. Recent gpg versions offer an
  # elliptic curve key by default, which passes every other check here. In
  # the algorithm field, 1 and 3 are RSA.
  rest=${key#* }
  algo=${rest%% *}
  bits=${rest##* }
  case "$algo" in
    1|3) ;;
    *) fail "the key that signed, $signed_with, is not RSA: gpg names its algorithm $algo. The ASF requires RSA keys of at least 2048 bits to sign releases. Create an RSA key of 4096 bits, add it to KEYS, and set GPG_USER to it" ;;
  esac
  [ "$bits" -ge 2048 ] || fail "the key that signed, $signed_with, is RSA of $bits bits. The ASF requires at least 2048 bits, and asks for 4096 in a new key"
  if [ "$bits" -lt 4096 ]; then say "warning  : the key is RSA of $bits bits. The ASF asks a new key to be 4096 bits"; fi
  # SkyWalking asks the signer to be named by an apache.org address, so a
  # voter can tell whose key it is. The user IDs are those of the primary
  # key, as KEYS holds them. A revoked or expired one does not count.
  apache_uid=$(printf '%s\n' "$keys" | awk -F: -v f="$fpr" '
    $1 == "pub" {mine = 0; first = 1; next}
    first && $1 == "fpr" {mine = ($10 == f); first = 0; next}
    mine && $1 == "uid" && $2 != "r" && $2 != "e" {print $10}' | grep -Ei '@apache\.org>$' || true)
  [ -n "$apache_uid" ] || fail "the signing key $fpr has no user ID with an apache.org address in $keys_url. Add your apache.org address to the key as a user ID, and update KEYS with it"
  signer="${GPG_USER:-$fpr}"
  say "signer   : $fpr, RSA of $bits bits, in $keys_url as $(printf '%s\n' "$apache_uid" | sed -n 1p)"

  step "Package, verify, upload"
  if [ "$resume" = true ]; then
    say "- sign and upload nothing: $dev_dir holds the candidate in $out"
    say "- verify each signature in $out against KEYS as made by $fpr"
    say "- attach to the GitHub prerelease $tag what it is missing of: the source package, its .sha512, and every .asc"
    say "- write the vote mail to $out/vote.txt and print it"
    if [ "$dry_run" = true ]; then say "dry run: nothing was attached or written. The scratch signature and the KEYS keyring were in a temporary directory, which is removed"; exit 0; fi
  else
    say "- copy the verified CI binary archives and their checksums unchanged into $out"
    say "- git archive the source at $commit with its release prefix, compress with gzip -n, and reject archive metadata"
    say "- sign each archive with $signer and preserve the CI checksums in $out:"
    for p in $packages; do say "    $p"; done
    say "- verify every package: its .asc and .sha512 are there, shasum -a 512 -c passes, and gpg, reading KEYS, reports a good signature and does not report the key that signed, or the signature, as expired or revoked"
    say "- verify the source package holds LICENSE and NOTICE at its top level, no compiled file and no font file"
    if [ -n "$host_pkg" ]; then
      say "- run $host_pkg with $smoke from the source package, and stop if it fails"
    else
      say "- run no package: none is built for this machine"
    fi
    if [ "$no_upload" = true ]; then
      say "- upload nothing (--no-upload)"
      say "- write the vote mail to $out/vote-preview.txt and print it"
    else
      say "- svn checkout --depth empty $dist_dev"
      if [ "$dev_has_dir" = false ]; then say "- create ai-sessionizer in it"; fi
      say "- svn add ai-sessionizer/$version with every package, .asc and .sha512"
      say "- svn commit -m \"Add the $project $version release candidate\""
      say "- attach the source package, its .sha512 and every .asc to the GitHub prerelease $tag, beside CI's files"
      say "- write the vote mail to $out/vote.txt and print it"
    fi
    if [ "$dry_run" = true ]; then say "dry run: nothing was built or uploaded, and nothing was written into $out. The scratch signature and the KEYS keyring were in a temporary directory, which is removed"; exit 0; fi
  fi

  if [ "$resume" = true ]; then
    grep '^ci-binaries:' "$work/ci-check.txt" > "$out/ci-provenance.txt"
    step "The uploaded signatures"
    for p in $packages; do
      why=$(keys_verify "$keyring" "$fpr" "$out/$p.asc" "$out/$p")
      [ -z "$why" ] || fail "the signature of $p in $out does not verify cleanly against $keys_url as made by $fpr: $why. Sign with the key that signed the uploaded candidate, or remove $dev_dir and run candidate again"
      say "ok  $p: a good signature by $fpr"
    done
  else
    step "Assemble the candidate from CI and the tagged source"
    # CI's archives are copied without unpacking or repacking. Local platform
    # metadata and tool versions must never change the bytes the vote approves.
    rm -f "$out/vote.txt" "$out/vote-preview.txt"
    for p in $packages; do rm -f "$out/$p" "$out/$p.asc" "$out/$p.sha512"; done
    mkdir -p "$out"
    for t in $platforms; do
      p=$(binary_package "$t")
      cp "$work/ci/$p" "$work/ci/$p.sha512" "$out/"
    done
    grep '^ci-binaries:' "$work/ci-check.txt" > "$out/ci-provenance.txt"
    src="$pkg-$version-src.tgz"
    top="$pkg-$version-src"
    git archive --format=tar --prefix="$top/" "$commit" | gzip -n > "$out/$src"
    (cd "$out" && shasum -a 512 "$src" > "$src.sha512")

    step "Verify the candidate in $out"
    for p in $packages; do
      for f in "$p" "$p.sha512"; do [ -f "$out/$f" ] || fail "$out/$f is missing"; done
      sh "$package_check" "$out/$p" || fail "$p has forbidden archive metadata"
    done
    extra=""
    for f in "$out"/*.tgz "$out"/*.zip; do
      [ -f "$f" ] || continue
      has_line "$packages" "${f##*/}" || extra="$extra ${f##*/}"
    done
    [ -z "$extra" ] || fail "$out holds packages the Makefile in $tag does not name:$extra"
    for p in $packages; do
      (cd "$out" && shasum -a 512 --status -c "$p.sha512") || fail "the sha512 of $p does not match $p.sha512"
      say "ok  $p: sha512"
    done
    # Refuse compiled files before signing the source archive. file(1) is
    # asked for every file type, using the list in the tag's Makefile.
    listing=$(tar -tzf "$out/$src") || fail "cannot list $src"
    for f in LICENSE NOTICE; do has_line "$listing" "$top/$f" || fail "$src has no $f at its top level"; done
    stray=$(printf '%s\n' "$listing" | grep -v -e "^$top/" -e '^pax_global_header$' || true)
    [ -z "$stray" ] || fail "$src has entries outside $top/: $stray"
    fonts=$(printf '%s\n' "$listing" | grep -E "$font_files" || true)
    [ -z "$fonts" ] || fail "$src holds font files, which are Category B works the ASF does not allow in a source release:
$fonts"
    mkdir "$work/src"
    tar -xzf "$out/$src" -C "$work/src"
    found=$(cd "$work/src" && find . -type f -print0 | xargs -0 file -N --mime-type | grep -E ": *$compiled\$" || true)
    [ -z "$found" ] || fail "$src carries compiled files:
$found"
    # COMPILED_FILES names the compiled files file(1) cannot tell by type.
    found=$(cd "$work/src" && find . -type f | grep -E "$compiled_files\$" || true)
    [ -z "$found" ] || fail "$src carries files named as compiled files, which file(1) may not tell by type:
$found"
    say "ok  $src: LICENSE and NOTICE at the top, no compiled file, no font file"
    # The same list a voter checks in every binary package.
    for t in $platforms; do
      b=$(binary_package "$t")
      exe=""
      if [ "${t%/*}" = windows ]; then exe=.exe; fi
      case "$b" in
        *.zip) listing=$(unzip -Z1 "$out/$b") || fail "cannot list $b" ;;
        *) listing=$(tar -tzf "$out/$b") || fail "cannot list $b" ;;
      esac
      for f in "asz$exe" "claude-code-plugin/bin/asz-claude-plugin$exe" LICENSE NOTICE; do
        has_line "$listing" "$f" || fail "$b has no $f"
      done
      for d in licenses/ claude-code-plugin/.claude-plugin/ claude-code-plugin/hooks/; do
        has_prefix "$listing" "$d" || fail "$b has no $d"
      done
      say "ok  $b: asz$exe, the Claude Code plugin, LICENSE, NOTICE and licenses/"
    done

    step "Sign the verified archives"
    for p in $packages; do
      gpg --armor --detach-sign --yes --local-user "$signer" "$out/$p" || fail "gpg could not sign $p"
      why=$(keys_verify "$keyring" "$fpr" "$out/$p.asc" "$out/$p")
      [ -z "$why" ] || fail "the signature of $p does not verify cleanly against $keys_url as made by $fpr: $why"
      say "ok  $p: a good signature by $fpr"
    done

    step "Run the package for this machine"
    if [ -n "$host_pkg" ]; then
      # bash runs the script whatever mode the source archive gave it.
      bash "$work/src/$top/$smoke" "$out/$host_pkg" "$version" \
        || fail "$host_pkg did not pass $smoke, run from the $tag source package. Its output is above. Nothing was uploaded, and no vote mail was written"
      say "ok  $host_pkg runs on this machine"
    else
      say "no package of $version is built for this machine, $(uname -s) $(uname -m), so none was run here"
    fi

    if [ "$no_upload" = false ]; then
      step "Upload to $dev_dir"
      wc="$work/dev"
      asf_svn checkout -q --depth empty "$dist_dev" "$wc"
      if [ "$dev_has_dir" = true ]; then
        asf_svn update -q --set-depth immediates "$wc/ai-sessionizer"
        [ ! -e "$wc/ai-sessionizer/$version" ] || fail "$dev_dir appeared while this ran; see svn ls $dist_dev/ai-sessionizer"
        add="ai-sessionizer/$version"
      else
        add="ai-sessionizer"
      fi
      mkdir -p "$wc/ai-sessionizer/$version"
      for p in $packages; do cp "$out/$p" "$out/$p.asc" "$out/$p.sha512" "$wc/ai-sessionizer/$version/"; done
      (cd "$wc" && asf_svn add -q "$add" && asf_svn commit -m "Add the $project $version release candidate")
      say "uploaded $dev_dir"
    fi
  fi

  if [ "$no_upload" = false ]; then
    step "Attach to the GitHub prerelease $tag"
    # The prerelease then holds every file of the vote, so a voter can take
    # the signatures from either place. dist.apache.org stays the one the
    # vote is about.
    sync_release_assets "$out" "The candidate is on $dev_dir. Run 'tools/release.sh candidate $version' again from this checkout: it signs and uploads nothing again, and attaches what is missing."
  fi

  step "The vote mail"
  vote_mail() {
    local p
    cat <<MAIL
Subject: [VOTE] Release $project version $version

Hi the SkyWalking Community:
This is a call for vote to release $project version $version.

Release notes:
 * $github/blob/$tag/docs/en/changes/changes.md

Release Candidate:
 * $dev_dir
 * The same files, on the GitHub prerelease: $github/releases/tag/$tag
 * sha512 checksums
$(for p in $packages; do printf '   - %s\n' "$(cat "$out/$p.sha512")"; done)

Release Tag:
 * (Git Tag) $tag

Release Commit Hash:
 * $github/tree/$commit

Keys to verify the Release Candidate:
 * $keys_url
 * Signed with key $fpr, which is in KEYS.

Guide to build the release from source:
 * $github/blob/$tag/docs/en/guides/how-to-release.md

Notes for voters:
 * The binary archives are the unchanged packages CI attached to the GitHub prerelease after all its checks passed on the tag push, verified below. Only the source archive was created locally. The release manager signed every archive after checking its contents and checksums, and attached the source archive and every signature to the same prerelease.
$(sed 's/^ci-binaries:/ */' "$out/ci-provenance.txt")
 * internal/view/conversation-view/ in the source package is the build output of Horizon's conversation renderer, from apache/skywalking-horizon-ui at the commit its HORIZON_COMMIT file names. It is Apache-2.0 code of the ASF with no third-party code in it. The source package builds and runs with it as it is. \`make conversation-view-check\`, which needs Node.js 24 and pnpm, rebuilds it from that commit and compares. In the unpacked source package it compares every file except the two fonts, which the source package does not carry, and it names the two it left out.
 * The two fonts the page draws with are under the SIL Open Font License, a Category B license, so they are in the binary packages only. A build from the source package draws the page with system fonts.

Voting will start now and will remain open for at least 72 hours. All PMC members are requested to give their votes.

[ ] +1 Release this package.
[ ] +0 No opinion.
[ ] -1 Do not release this package because....

Thanks.
MAIL
  }
  # vote.txt is written only for an uploaded candidate, so the mail in it
  # always names files that are in the candidate directory.
  mail="$out/vote.txt"
  if [ "$no_upload" = true ]; then mail="$out/vote-preview.txt"; fi
  vote_mail > "$mail"
  cat "$mail"
  say "----"
  if [ "$no_upload" = true ]; then
    say "not uploaded. $mail shows the mail, with the checksums of these packages, which were not uploaded."
    say "Run candidate again without --no-upload to prepare, sign and upload the candidate the vote is about."
  else
    say "Next:"
    say "  1. Check every link, then send $out/vote.txt to dev@skywalking.apache.org."
    say "  2. After at least 72 hours, count the votes. The vote passes with at least three"
    say "     binding +1 votes and more binding +1 than binding -1 votes. Send the result to"
    say "     dev@skywalking.apache.org, with the subject [RESULT][VOTE] and every vote listed."
    say "  3. If it passed, a PMC member runs: tools/release.sh publish $version"
  fi
  exit 0
fi

# ----------------------------------------------------------------- publish
if [ "$cmd" = publish ]; then
  step "The version"
  pick_version "Version the vote passed for"
  # tar and awk are for tools/install-manifests.sh, which runs after the
  # move. Checked here, a missing one stops the run before anything moves.
  need_tools git svn shasum tar awk gh gpg curl cmp
  manifests=tools/install-manifests.sh
  # Checked before anything moves. The manifests are written after the
  # move, and a missing script there would stop the run half done.
  [ -f "$manifests" ] || fail "$manifests is missing. publish runs it after the move, to write the install manifests from the voted packages. Add it and run publish again; nothing has changed."
  fetch_tag
  platforms=$(tag_platforms)
  packages=$(expected_packages)
  dev_dir="$dist_dev/ai-sessionizer/$version"
  rel_parent="$dist_release/ai-sessionizer"
  rel_dir="$rel_parent/$version"
  scratch=$(mktemp -d)
  keyring="$scratch/keyring"
  cleanup_publish() {
    if command -v gpgconf >/dev/null 2>&1; then gpgconf --homedir "$keyring" --kill all >/dev/null 2>&1 || true; fi
    rm -rf "$scratch"
  }
  trap cleanup_publish EXIT

  step "The GitHub release"
  # Read before anything moves: the release is promoted after the move, and
  # a prerelease that is gone would stop publish half done.
  release_info=$(release_identity) \
    || fail "cannot read the GitHub release $tag. CI creates it as a prerelease on the tag push, and candidate attaches its signatures; publish promotes it"
  IFS=$'\t' read -r release_id existing_tag is_draft is_prerelease existing_title <<< "$release_info"
  [ -n "$release_id" ] && [ "$existing_tag" = "$tag" ] && [ "$existing_title" = "$version" ] && [ "$is_draft" = false ] \
    || fail "the GitHub release $tag is not the prerelease CI created: its tag, its title or its draft state differs"
  promoted=false
  if [ "$is_prerelease" = false ]; then
    promoted=true
    say "$tag is a full GitHub release already: an earlier publish promoted it. Its files are checked again"
  else
    say "$tag is a GitHub prerelease, promoted after the move"
  fi
  # Only names are checked here. The bytes are compared after the voted
  # files are fetched, but a missing CI file or a stray asset is found now.
  release_assets=$(gh release view "$tag" --repo "$release_repo" --json assets --jq '.assets[].name') \
    || fail "cannot list the assets of the GitHub release $tag"
  for f in $(ci_names); do
    has_line "$release_assets" "$f" || fail "the GitHub release $tag has no $f. CI attaches each binary package and its .sha512 on the tag push, and the release stages never upload them. Nothing was moved"
  done
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    has_line "$(release_names)" "$f" || fail "the GitHub release $tag holds an unexpected asset, $f. Check why it is there and remove it with 'gh release delete-asset $tag $f --repo $release_repo'. Nothing was moved"
  done <<< "$release_assets"
  say "ok  it holds CI's binary packages and nothing but voted files"

  step "The candidate"
  svn_root "$dist_dev" >/dev/null
  release_root=$(svn_root "$dist_release")
  dev_files=$(svn_list "$dev_dir")
  rel_versions=$(svn_list "$rel_parent")
  moved=false
  if has_line "$rel_versions" "$version"; then
    [ -z "$dev_files" ] || fail "$version is in both $dev_dir and $rel_dir. Find out why before going on"
    # An earlier run moved the candidate and stopped after, or this is the
    # later run that removes older versions. The move is not repeated; what
    # follows it runs again.
    moved=true
    from="$rel_dir"
    files=$(svn_list "$rel_dir")
    say "$rel_dir exists and $dev_dir does not: an earlier publish moved the candidate."
    say "The move is done. The steps after it run again."
  else
    [ -n "$dev_files" ] || fail "$dev_dir does not exist; run candidate and hold the vote first"
    from="$dev_dir"
    files="$dev_files"
  fi
  # The downloads page links the older versions in the release directory
  # until the website pull request points them at the archive, and the
  # Scoop bucket names the previous version until it moves on. Removing them
  # in the run that moves would break both, so removal is a later run.
  if [ "$remove_old" = true ] && [ "$moved" = false ]; then
    fail "--remove-old runs only after the move, in a later run. Run publish without it now. Once the website pull request that points the older versions at archive.apache.org has merged, and the Scoop bucket names $version, run: tools/release.sh publish $version --remove-old"
  fi
  for p in $packages; do
    for f in "$p" "$p.asc" "$p.sha512"; do has_line "$files" "$f" || fail "$from has no $f"; done
  done
  say "ok  $from holds the source package and $(printf '%s\n' "$platforms" | wc -l | tr -d ' ') binary packages, each with its .asc and .sha512"

  step "The voted packages in $out"
  # The install manifests and the GitHub release are made from these files,
  # so each must be the one voted on. A package built again has other bytes,
  # and a package signed again has another signature.
  fetch=""
  for p in $packages; do
    if [ ! -f "$out/$p" ] || [ ! -f "$out/$p.asc" ] || [ ! -f "$out/$p.sha512" ]; then
      fetch="$fetch $p"
      continue
    fi
    voted=$(asf_svn cat "$from/$p.sha512") || fail "cannot read $from/$p.sha512"
    [ "$voted" = "$(cat "$out/$p.sha512")" ] || fail "$out/$p.sha512 is not the voted one. Remove $out and run publish again to fetch the voted files"
    sum=$(shasum -a 512 "$out/$p")
    [ "${sum%%[[:space:]]*}" = "${voted%%[[:space:]]*}" ] || fail "$out/$p is not the voted package: its sha512 differs. Remove $out and run publish again to fetch the voted files"
    voted=$(asf_svn cat "$from/$p.asc") || fail "cannot read $from/$p.asc"
    [ "$voted" = "$(cat "$out/$p.asc")" ] || fail "$out/$p.asc is not the voted signature. Remove $out and run publish again to fetch the voted files"
    say "ok  $p is the voted package, with the voted .asc and .sha512"
  done

  step "Publish $version"
  old=""
  newer=""
  for v in $rel_versions; do
    if [ "$v" = "$version" ] || ! is_version "$v"; then continue; fi
    if [ "$(printf '%s\n%s\n' "$v" "$version" | sort -V | head -1)" = "$v" ]; then old="$old $v"; else newer="$newer $v"; fi
  done
  if [ -n "$fetch" ]; then say "- fetch from $from, into $out:$fetch"; fi
  if [ "$moved" = false ]; then
    if ! has_line "$release_root" ai-sessionizer; then say "- svn mkdir $rel_parent, since this is the first release"; fi
    say "- svn mv $dev_dir $rel_dir"
  fi
  if [ "$remove_old" = true ]; then
    for v in $old; do say "- svn rm $rel_parent/$v, superseded by $version; archive.apache.org keeps it"; done
    if [ -z "$old" ]; then say "- nothing older to remove from $rel_parent"; fi
    if [ -n "$newer" ]; then say "- keep$newer, newer than $version"; fi
  elif [ -n "$old" ]; then
    say "- keep$old in $rel_parent. A later run with --remove-old removes them, once the website links them from the archive"
  fi
  say "- verify each package's signature against KEYS, before any move"
  say "- check that the GitHub release $tag holds CI's binary packages and .sha512 files as voted, attach the voted source package, .sha512 and .asc files it is missing, and check that it holds exactly the voted files"
  if [ "$promoted" = false ]; then
    say "- promote $tag to a full GitHub release, with the text of $dev_page in $tag; CI publishes the image on the released event"
  fi
  say "- write the announcement to $out/announce.txt"
  say "- write the website entries to $out/website.txt"
  say "- $manifests $version $out $out/install"
  if [ "$dry_run" = true ]; then say "dry run: no package was fetched, and nothing was moved, removed, promoted or written"; exit 0; fi

  mkdir -p "$out"
  for p in $fetch; do
    for f in "$p" "$p.asc" "$p.sha512"; do asf_svn export -q --force "$from/$f" "$out/$f"; done
    (cd "$out" && shasum -a 512 --status -c "$p.sha512") || fail "$p fetched from $from does not match its sha512"
    say "fetched $p"
  done

  step "The signatures"
  # A voter checked each signature against KEYS. It is checked once more
  # before the move, since after it the files are the release.
  mkdir -m 700 "$keyring"
  curl -fsSL "$keys_url" -o "$scratch/KEYS" || fail "cannot download $keys_url"
  # gpg exits 2 when any one entry of KEYS cannot be imported, even when the
  # key that signed did import. Each package still needs a good signature.
  gpg --batch --quiet --homedir "$keyring" --import "$scratch/KEYS" >/dev/null 2>&1 || true
  for p in $packages; do
    status=$(gpg --batch --homedir "$keyring" --status-fd 1 --verify "$out/$p.asc" "$out/$p" 2>/dev/null || true)
    by=$(printf '%s\n' "$status" | awk '$2 == "VALIDSIG" && !f {f = ($12 != "" ? $12 : $3)} END {print f}')
    [ -n "$by" ] || fail "$p has no valid signature by a key in $keys_url. Nothing was moved"
    why=$(keys_verify "$keyring" "$by" "$out/$p.asc" "$out/$p")
    [ -z "$why" ] || fail "the signature of $p does not verify cleanly against $keys_url: $why. Nothing was moved"
    say "ok  $p: a good signature by $by"
  done
  if [ "$moved" = false ]; then
    if ! has_line "$release_root" ai-sessionizer; then
      asf_svn mkdir -m "Create the $project release directory" "$rel_parent"
    fi
    asf_svn mv -m "Release $project $version" "$dev_dir" "$rel_dir"
  fi
  released=$(svn_list "$rel_dir")
  for p in $packages; do
    for f in "$p" "$p.asc" "$p.sha512"; do has_line "$released" "$f" || fail "$rel_dir has no $f after the move"; done
  done
  say "published: $rel_dir"
  if [ "$remove_old" = true ]; then
    for v in $old; do
      asf_svn rm -m "Remove $project $v, superseded by $version" "$rel_parent/$v"
      say "removed $rel_parent/$v; it stays on https://archive.apache.org/dist/skywalking/ai-sessionizer/$v/"
    done
  fi

  step "Promote the GitHub release $tag"
  # release_text prints the text of the GitHub release: the tag's
  # changes.md without its heading, because the release has its own title,
  # then where to get the version. It is built from the tag each time and
  # stored nowhere, so a change made on main after prepare cannot reach it.
  # It sends a reader to the Apache release and to the signatures, never to
  # a git checkout, as the ASF release policy asks.
  release_text() {
    git show "$tag:$dev_page" | tail -n +2 | sed '1{/^$/d;}'
    cat <<TEXT

#### Where to get it

- The Apache release of $version is the source package. The binary packages for macOS, Linux and Windows are conveniences built from it. The [SkyWalking downloads page](https://skywalking.apache.org/downloads/) links each package with its signature and checksum.
- The files attached to this GitHub release are the same signed packages, each with its \`.asc\` signature and \`.sha512\` checksum. Verify them against https://downloads.apache.org/skywalking/KEYS, as [Install]($github/blob/$tag/docs/en/setup/install.md#verify-a-package) describes.
- To build from the source package, see [Install]($github/blob/$tag/docs/en/setup/install.md#build-from-the-source-package).
- Documentation: $github/blob/$tag/docs/README.md
- Full changelog: $github/blob/$tag/docs/en/changes/changes.md
TEXT
  }
  resume_hint="The move is done. Run 'tools/release.sh publish $version' again: it skips the move and resumes here."
  sync_release_assets "$out" "$resume_hint"
  if [ "$promoted" = false ]; then
    release_text > "$scratch/notes.md"
    current_info=$(release_identity) || fail "cannot read the GitHub release $tag again before promoting it. $resume_hint"
    [ "$current_info" = "$release_info" ] || fail "the GitHub release $tag changed while publish was checking it, so it was not promoted. Find out why, then run publish again"
    gh release edit "$tag" --repo "$release_repo" --draft=false --prerelease=false --title "$version" --notes-file "$scratch/notes.md" \
      || fail "cannot promote the GitHub release $tag. $resume_hint"
    say "promoted $tag to a full GitHub release. CI publishes the image on the released event"
  fi

  announce_mail() {
    cat <<MAIL
Subject: [ANNOUNCE] $project $version released

Hi the SkyWalking Community,

On behalf of the SkyWalking Team, I am glad to announce that $project $version is now released.

SkyWalking AI Sessionizer: conversation-level observability for long-lived AI agents. It assembles fragmented agent telemetry into one durable conversation structure.

SkyWalking: APM (application performance monitor) tool for distributed systems, especially designed for microservices, cloud native and container-based architectures.

Download Links: https://skywalking.apache.org/downloads/
Release Notes: $github/blob/$tag/docs/en/changes/changes.md
Website: https://skywalking.apache.org/
Documents: https://skywalking.apache.org/docs/skywalking-ai-sessionizer/$tag/readme/

Resources:
- Issue: https://github.com/apache/skywalking/issues
- Mailing list: dev@skywalking.apache.org

The Apache SkyWalking Team
MAIL
  }

  # The website dates a version by the day it reached the release
  # directory. svn keeps that day, so a later run of publish, such as the
  # one with --remove-old, writes the same date and not its own. The day is
  # in UTC, as svn records it.
  published_on=$(asf_svn info --show-item last-changed-date "$rel_dir" 2>/dev/null || true)
  published_on=${published_on%%T*}
  case "$published_on" in
    [0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]) ;;
    *)
      published_on=$(date -u +%Y-%m-%d)
      say "svn did not say when $rel_dir changed, so $out/website.txt carries today's date, $published_on. Correct it if the move was on another day" ;;
  esac

  # The website writes dates as "Sep. 11th, 2026".
  site_date() { # site_date 2026-09-11
    local y=${1%%-*} m d s
    m=${1#*-}; m=${m%-*}; m=${m#0}
    d=${1##*-}; d=${d#0}
    case "$d" in 1|21|31) s=st ;; 2|22) s=nd ;; 3|23) s=rd ;; *) s=th ;; esac
    set -- Jan Feb Mar Apr May Jun Jul Aug Sep Oct Nov Dec
    shift $(( m - 1 ))
    printf '%s. %s%s, %s' "$1" "$d" "$s" "$y"
  }

  platform_name() { # platform_name linux/amd64 prints Linux AMD64
    local os=${1%/*} arch
    arch=$(printf '%s' "${1#*/}" | tr '[:lower:]' '[:upper:]')
    case "$os" in darwin) os=MacOS ;; linux) os=Linux ;; windows) os=Windows ;; esac
    printf '%s %s' "$os" "$arch"
  }

  # The shapes are those of data/releases.yml and data/docs.yml in
  # apache/skywalking-website: a package is linked through the mirror
  # selector, and its signature and checksum through downloads.apache.org.
  website_entries() {
    local d t b first=true
    d=$(site_date "$published_on")
    cat <<YAML
# data/releases.yml in apache/skywalking-website, for the downloads page.
# The first release adds this entry to the list of "- type: Foundations",
# after Grafana Plugins, the place data/docs.yml gives AI Sessionizer.
# A later release puts its own items first under source and under
# distribution, and points the links of versions no longer in the release
# directory at https://archive.apache.org/dist/skywalking/ai-sessionizer/.

    - name: SkyWalking AI Sessionizer
      icon: skywalking
      description: Conversation-level observability, measurement and export for long-lived AI agents.
      source:
        - version: $tag
          date: $d
          downloadLink:
            - name: src
              link: $closer/$version/$pkg-$version-src.tgz
            - name: asc
              link: $downloads/$version/$pkg-$version-src.tgz.asc
            - name: sha512
              link: $downloads/$version/$pkg-$version-src.tgz.sha512
      distribution:
        - version: $tag
          date: $d
          downloadLink:
YAML
    for t in $platforms; do
      b=$(binary_package "$t")
      if [ "$first" = false ]; then printf '            - name: "|"\n'; fi
      first=false
      printf '            - name: %s\n              link: %s\n' "$(platform_name "$t")" "$closer/$version/$b"
      printf '            - name: asc\n              link: %s\n' "$downloads/$version/$b.asc"
      printf '            - name: sha512\n              link: %s\n' "$downloads/$version/$b.sha512"
    done
    cat <<YAML

# data/docs.yml in apache/skywalking-website, for the documentation. The
# AI Sessionizer entry is there already, with Next only. Put these items
# right after Next. A later release sets the commitId of Latest to its own
# commit and puts its own item right after Latest.

        - version: Latest
          link: /docs/skywalking-ai-sessionizer/latest/readme/
          commitId: $commit
        - version: $tag
          link: /docs/skywalking-ai-sessionizer/$tag/readme/
          commitId: $commit
YAML
  }

  announce_mail > "$out/announce.txt"
  say "wrote $out/announce.txt"
  website_entries > "$out/website.txt"
  say "wrote $out/website.txt"

  step "The install manifests"
  # The script refuses a directory that is not empty, so no file from an
  # earlier run is submitted beside the new ones. publish owns this
  # directory and writes it again on every run.
  rm -rf "$out/install"
  if [ -x "$manifests" ]; then run=("$manifests"); else run=(bash "$manifests"); fi
  # The script refuses a directory that is not empty, so the command given
  # on failure empties it first, or lets publish do it.
  "${run[@]}" "$version" "$out" "$out/install" || fail "$manifests failed. The move is done and is not repeated. Fix the script, then run publish again, which skips the move and writes $out/install from scratch. Or run the script by hand: rm -rf $out/install && $manifests $version $out $out/install"
  say "wrote $out/install"

  say "----"
  say "Next, in this order:"
  say "  1. At least one hour after the move, as the ASF release policy asks, open a pull"
  say "     request on apache/skywalking-website with $out/website.txt: the downloads entry"
  say "     in data/releases.yml and the $tag documentation in data/docs.yml."
  say "  2. Once the website lists $version, and at least one hour after the move, send"
  say "     $out/announce.txt to dev@skywalking.apache.org and announce@apache.org, as plain"
  say "     text from your apache.org address. Sign it with your release key if your mail"
  say "     program can; the ASF recommends it."
  say "  3. Submit the install manifests in $out/install, once the PMC has agreed on"
  say "     dev@skywalking.apache.org to each channel. $out/install/README.md says where each"
  say "     one goes. The Homebrew formula and the winget files download from the GitHub"
  say "     release, which is promoted now."
  if [ -n "$old" ] && [ "$remove_old" = false ]; then
    say "  4. Later, once the website pull request that points$old at archive.apache.org"
    say "     has merged, and the Scoop bucket names $version:"
    say "     tools/release.sh publish $version --remove-old"
  fi
  exit 0
fi

# ----------------------------------------------------------------- prepare
step "The tree"
# gh is checked now, not when the pull request is raised: by then the
# branch and the tag are pushed already.
tools="git make go awk sed grep sort"
if [ "$no_push" = false ]; then tools="$tools gh"; fi
# shellcheck disable=SC2086 # the list is split on purpose
need_tools $tools
[ -z "$(git status --porcelain)" ] || fail "the working tree has changes; start from a clean tree"
from=$(git branch --show-current)
say "cutting the release branch from $from at $(git rev-parse --short HEAD)"

step "The versions"
# The heading of changes.md names the version in development, and that is
# the version offered. It is checked before the question, so a heading
# such as "# Changes in the next version" stops the run instead of being
# offered as the version.
[ -f "$dev_page" ] || fail "$dev_page is missing. It is the changelog of the version in development, which prepare moves to the version's own page after the tag"
current=$(page_version < "$dev_page")
is_version "$current" || fail "the first heading of $dev_page does not name a version of the form MAJOR.MINOR.PATCH. It must read '# Changes in VERSION', where VERSION is the version in development"
[ -n "$version" ] || version=$(ask v "Version to release" "$current")
is_version "$version" || fail "$version is not of the form MAJOR.MINOR.PATCH"
[ "$version" = "$current" ] || fail "$dev_page is the changelog of $current, as its heading says, not of $version. Release $current, or correct the heading first"
[ -n "$next" ] || next=$(ask v "Next version, where development continues" "$(printf '%s' "$version" | awk -F. '{printf "%d.%d.0", $1, $2 + 1}')")
is_version "$next" || fail "$next is not of the form MAJOR.MINOR.PATCH"
[ "$(printf '%s\n%s\n' "$version" "$next" | sort -V | tail -1)" = "$next" ] && [ "$version" != "$next" ] || fail "next version $next must come after $version"
tag="v$version"
branch="release/$version"
page="$changes_dir/changes-$version.md"
if [ "$no_push" = false ]; then
  # CI creates the prerelease when the tag is pushed. One that exists now
  # belongs to an earlier candidate, which must be removed explicitly.
  if previous=$(gh release view "$tag" --repo "$release_repo" --json isPrerelease --jq '.isPrerelease' 2>&1); then
    if [ "$previous" = true ]; then
      fail "a GitHub prerelease for $tag already exists. If its candidate was rejected, remove it and its tag explicitly before preparing another candidate: gh release delete $tag --repo $release_repo --cleanup-tag. The script never deletes a prerelease or moves a tag"
    fi
    fail "the GitHub release $tag already exists and is not a prerelease"
  fi
  # Only a missing release lets prepare go on. Any other failure, such as gh
  # not logged in or no network, would otherwise show up only after the
  # branch and the tag are pushed.
  case "$previous" in
    *"release not found"*) ;;
    *) fail "cannot check whether the GitHub release $tag exists: $previous" ;;
  esac
fi
! git rev-parse -q --verify "refs/tags/$tag" >/dev/null || fail "tag $tag already exists"
! git rev-parse -q --verify "refs/heads/$branch" >/dev/null || fail "branch $branch already exists"
has_note < "$dev_page" || fail "$dev_page has no in-development note, a line starting '> In development' under its heading. The page of the version in development always carries it, and prepare removes it in the commit it tags"
[ ! -e "$page" ] || fail "$page exists already, so $version was prepared before"
[ ! -e "$changes_dir/changes-$next.md" ] || fail "$changes_dir/changes-$next.md exists already, so $next was prepared before"
# Current Version stays on changes.md, so the menu and the welcome page
# link the version in development after every release without an edit.
current_path=$(awk '/^        - name: Current Version$/ {cv=1; next} cv && /^          path:/ {sub(/^ *path: */, ""); print; exit}' "$menu")
[ "$current_path" = /en/changes/changes ] || fail "Current Version in $menu points at '$current_path'. It must point at /en/changes/changes, the page of the version in development"
say "releasing $version on $branch, then opening $next"

step "License headers"
# make installs a missing tool into bin/ before it checks, and a dry run
# installs nothing. So in a dry run a check whose tool is missing is
# listed, not run.
if doit || [ -x bin/license-eye ]; then make license-check
else say "- make license-check, left out: it would install license-eye into bin/ first"; fi
if [ "$skip_check" = false ]; then
  step "The full check: vet, lint, licenses, tests"
  if doit || { [ -x bin/license-eye ] && [ -x bin/golangci-lint ]; }; then make check
  else say "- make check, left out: it would install license-eye and golangci-lint into bin/ first"; fi
fi

step "Prepare the $version candidate"
say "- remove the in-development note from $dev_page, which keeps its path"
say "- commit \"Prepare the $version candidate\" on $branch and tag $tag on it"

if doit; then
  git checkout -q -b "$branch"
  # Only the note goes, and the page keeps its path. The website publishes
  # the docs of each version from its tag, and the menu and the welcome
  # page of the tag link changes.md, so the finished changelog must be
  # there. That is also the order Apache SkyWalking and SkyWalking SWCK
  # follow on their tags.
  # Remove only the note's own lines and one blank line after them. A
  # heading written straight under the note must stay in the changelog.
  awk '/^> In development/{skip=1; next} skip && /^>/{next} skip && /^$/{skip=0; next} {skip=0; print}' "$dev_page" > "$dev_page.tmp" && mv "$dev_page.tmp" "$dev_page"
  git add "$dev_page"
  git commit -q -m "Prepare the $version candidate

The changelog of $version loses its in-development note and keeps its
path, docs/en/changes/changes.md, which the menu and the welcome page
link. The tag goes on this commit, as the candidate for the vote."
  # The tag is the candidate the vote is about, not the release, so its
  # message does not call it one, and neither does the commit.
  git tag -a "$tag" -m "$project $version"
  say "committed $(git rev-parse --short HEAD) and tagged $tag"
fi

step "Open $next"
say "- git mv $dev_page to $page"
if grep -Fxq "        - name: $version" "$menu"; then say "- leave $menu as it is: it lists $version under Changelog already"
else say "- list $version under Changelog in $menu, right after Current Version"; fi
say "- write a new $dev_page for $next, with the in-development note"
say "- commit \"Open $next\" on $branch"
if doit; then
  # The move and the new page at the old path go in one commit. git records
  # no rename, but git log --follow finds the moved page by its content, so
  # it traces changes-$version.md back through changes.md.
  git mv "$dev_page" "$page"
  if ! grep -Fxq "        - name: $version" "$menu"; then
    awk -v v="$version" '
      {print}
      /^        - name: Current Version$/ {cv=1; next}
      cv && /^          path:/ {print "        - name: " v; print "          path: /en/changes/changes-" v; cv=0}' "$menu" > "$menu.tmp" && mv "$menu.tmp" "$menu"
  fi
  # The heading and the note are the ones the next prepare reads back.
  cat > "$dev_page" <<PAGE
# Changes in $next

> In development, not yet released. \`tools/release.sh prepare $next\` removes this note.
PAGE
  git add "$page" "$menu" "$dev_page"
  git commit -q -m "Open $next

The changelog of $version moves from changes.md to its own page,
changes-$version.md, and the version is listed under Changelog, right
after Current Version. changes.md is the changelog of $next from here.
Current Version and the welcome page still link changes.md."
  say "committed $(git rev-parse --short HEAD)"
fi

step "Push and pull request"
say "- git push the branch $branch and the tag $tag"
say "- open the pull request against $from"
say "- CI on the $tag push then builds and tests every package, creates the GitHub prerelease $tag, and attaches the binary archives with their .sha512 files. Nothing here waits for it"
if [ "$dry_run" = true ]; then say "dry run: nothing was written, pushed or opened"; exit 0; fi
if [ "$no_push" = true ]; then
  cat <<NEXT
not pushed. When ready:
  git push -u origin $branch
  git push origin $tag
  gh pr create --base $from --head $branch --title "Prepare the $version candidate and open $next"
The CI run of the tag push creates the GitHub prerelease $tag and attaches the binaries.
Once it has, and the pull request has merged, sign and upload the candidate:
  tools/release.sh candidate $version
NEXT
  exit 0
fi
git push -u origin "$branch"
git push origin "$tag"
gh pr create --base "$from" --head "$branch" --title "Prepare the $version candidate and open $next" \
  --body "The first commit removes the in-development note from the $version changelog, docs/en/changes/changes.md. Tag $tag is on it, and is the candidate for the vote. The second commit moves the changelog to changes-$version.md, lists $version under Changelog, and opens $next in a new changes.md. The CI run of the $tag push creates the GitHub prerelease and attaches the binaries. After that and this merge, run \`tools/release.sh candidate $version\` to sign the candidate, upload it for the vote and attach its signatures to the prerelease."
say "pushed $branch and $tag, and opened the pull request."
say "Next: wait for the CI run of the $tag push to create the GitHub prerelease $tag with the binaries,"
say "and for the pull request to merge. Then: tools/release.sh candidate $version"
