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
#       and raise the pull request against main. Both versions are asked
#       for when not given, and the heading of changes.md gives the offered
#       VERSION. A dry run installs no tool: a check whose tool is not in
#       bin/ yet is listed, not run.
#
#   tools/release.sh candidate [VERSION] [--dry-run] [--no-upload]
#       After the prepare pull request has merged: build the release
#       candidate from a fresh clone of origin at vVERSION, sign it and
#       verify it in dist/VERSION, run the binary package for this machine
#       with the tag's tools/package-smoke.sh, upload the candidate to the
#       dev area of dist.apache.org for the vote, and write the vote mail to
#       dist/VERSION/vote.txt. The signing key must be RSA of at least 2048
#       bits, carry an apache.org user ID, and be in the SkyWalking KEYS
#       file. GPG_USER picks the key; empty means gpg's default key.
#       APACHE_ID, when set, is the name svn logs in with. --no-upload
#       builds, verifies and runs the package, uploads nothing, and writes
#       the mail to dist/VERSION/vote-preview.txt. It refuses once a
#       candidate of VERSION is uploaded.
#
#   tools/release.sh vote-result [VERSION] --binding "Name, Name, Name"
#           [--non-binding "Name, ..."] [--against "Name, ..."]
#           [--non-binding-against "Name, ..."] [--abstain "Name, ..."]
#           [--thread URL] [--dry-run]
#       After the vote has been open for 72 hours. --binding names the PMC
#       members who voted +1, --non-binding the other +1 voters, --against
#       the PMC members who voted -1, --non-binding-against the other -1
#       voters, and --abstain everyone who voted +0. --thread is the link
#       of the vote thread on lists.apache.org. Every vote is listed in the
#       mail; only binding votes decide. Refuse unless the vote passed: at
#       least three binding +1 votes, and more binding +1 than binding -1
#       votes. Write the result mail to dist/VERSION/result.txt.
#
#   tools/release.sh publish [VERSION] [--dry-run] [--remove-old]
#       Run by a PMC member after the vote passed. Move the candidate from
#       the dev area to the release directory of dist.apache.org. The move
#       publishes the voted packages. With the vote and the announcement,
#       it makes the release. Then write the announcement, the website
#       entries and the install manifests into dist/VERSION, and print what
#       is left to do. --remove-old removes older versions from the release
#       directory, and archive.apache.org keeps them. It is refused in the
#       run that moves: run publish again with it once the website links
#       the older versions from the archive.
#
#   tools/release.sh complete [VERSION] [--dry-run]
#       Create the GitHub release for vVERSION, not a draft and not a
#       prerelease. Its text is built at this point from the tag's
#       docs/en/changes/changes.md, followed by where to get the version,
#       and printed. Attach the voted packages from dist/VERSION once each
#       matches the file downloads.apache.org serves. The GitHub release is
#       a convenience. CI publishes the image when the released event
#       fires, and nothing here waits for it.
#
# When VERSION is not given, candidate, vote-result, publish and complete
# offer the newest version with a page docs/en/changes/changes-X.Y.Z.md.
# prepare gives a version that page in the commit after the tag, and main
# holds it once the prepare pull request has merged.
#
# --dry-run prints what would change and writes nothing.

set -euo pipefail

usage() { sed -n '/^# The stages of a release/,/^# --dry-run/p' "$0" | sed -E 's/^# ?//'; }

cmd="${1:-}"
case "$cmd" in
  prepare)     options="--dry-run --skip-check --no-push" ;;
  candidate)   options="--dry-run --no-upload" ;;
  vote-result) options="--dry-run --binding --non-binding --against --non-binding-against --abstain --thread" ;;
  publish)     options="--dry-run --remove-old" ;;
  complete)    options="--dry-run" ;;
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
remove_old=false
binding=""
non_binding=""
against=""
non_binding_against=""
abstain=""
thread=""
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
    --binding|--non-binding|--against|--non-binding-against|--abstain|--thread)
      needs="the names of the voters, separated by commas"
      if [ "$arg" = --thread ]; then needs="the link of the vote thread"; fi
      # The next argument is taken only when no = was given, and never when
      # it is an option: "--non-binding= --dry-run" once made --dry-run a
      # voter's name and wrote the mail.
      if [ "$given" = false ]; then
        case "${1:-}" in ""|-*) echo "$arg needs $needs" >&2; exit 2 ;; esac
        value="$1"
        shift
      fi
      [ -n "$value" ] || { echo "$arg needs $needs" >&2; exit 2; }
      case "$arg" in
        --binding) binding="${binding:+$binding, }$value" ;;
        --non-binding) non_binding="${non_binding:+$non_binding, }$value" ;;
        --against) against="${against:+$against, }$value" ;;
        --non-binding-against) non_binding_against="${non_binding_against:+$non_binding_against, }$value" ;;
        --abstain) abstain="${abstain:+$abstain, }$value" ;;
        --thread)
          [ -z "$thread" ] || { echo "--thread is given twice" >&2; exit 2; }
          thread="$value" ;;
      esac ;;
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
# make release refuses the same list, FONT_FILES in the Makefile, so change
# both together.
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
# The tag must hold the finished changelog of the version at changes.md: its
# heading names the version and the in-development note is gone. The docs
# the website publishes from the tag link that page, the vote mail and the
# announcement link it, and complete builds the text of the GitHub release
# from it. A tag prepare did not make fails here, before anything is built.
fetch_tag() {
  local refs remote_id local_id tag_page
  refs=$(git ls-remote --tags origin "refs/tags/$tag") || fail "cannot list the tags on origin"
  remote_id=$(printf '%s\n' "$refs" | awk -v r="refs/tags/$tag" '$2 == r {print $1}')
  [ -n "$remote_id" ] || fail "$tag is not on origin; run prepare and merge its pull request first"
  if local_id=$(git rev-parse -q --verify "refs/tags/$tag"); then
    [ "$local_id" = "$remote_id" ] || fail "the local tag $tag is $local_id but origin's is $remote_id. Remove the local one with 'git tag -d $tag' and run again"
  else
    git fetch -q origin "refs/tags/$tag:refs/tags/$tag" || fail "cannot fetch $tag from origin"
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

# --------------------------------------------------------------- candidate
if [ "$cmd" = candidate ]; then
  step "The version"
  pick_version "Version to build the release candidate for"
  tools="git go make gpg shasum tar zip unzip file curl"
  if [ "$no_upload" = false ]; then tools="$tools svn"; fi
  # shellcheck disable=SC2086 # the list is split on purpose
  need_tools $tools
  fetch_tag
  platforms=$(tag_platforms)
  packages=$(expected_packages)
  origin_url=$(git remote get-url origin)
  dev_dir="$dist_dev/ai-sessionizer/$version"
  compiled=$(git show "$tag:Makefile" | sed -nE 's/^COMPILED_TYPES[[:space:]]*:?=[[:space:]]*//p')
  [ -n "$compiled" ] || fail "the Makefile in $tag has no COMPILED_TYPES, the file types a source package must not carry"
  # make release cross-compiles every platform on this machine, and a binary
  # that was never started proves nothing about its platform. So the package
  # for this machine is run before the upload. The script and the scenarios
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
  # before a key is asked for and the build runs.
  fonts=$(git archive --format=tar "$tag" | tar -tf - | grep -E "$font_files" || true)
  [ -z "$fonts" ] || fail "the source package of $tag would hold font files:
$fonts
Fonts are under licenses such as the SIL Open Font License, which the ASF puts in Category B, and the ASF does not allow a Category B work in a source release. Mark them export-ignore in .gitattributes, or have the PMC settle it with legal@apache.org first"
  say "fonts    : none in the source package"

  if [ "$no_upload" = true ]; then
    # --no-upload builds into dist/$version too. After an upload that would
    # replace the uploaded files there with a new build, whose signatures at
    # least are new, and its mail would describe bytes the vote is not about.
    [ ! -f "$out/vote.txt" ] || fail "$out/vote.txt is there, so a candidate of $version was uploaded from this checkout. --no-upload would rebuild $out and replace the uploaded files. Build in another clone to look at a new build, or remove $out first once the uploaded candidate is removed"
    step "The candidate directory"
    if ! command -v svn >/dev/null 2>&1; then
      say "svn is not installed, so whether a candidate of $version is uploaded was not checked"
    elif dev_root=$(asf_svn ls "$dist_dev" 2>/dev/null); then
      if has_line "$(printf '%s\n' "$dev_root" | sed 's|/$||')" ai-sessionizer && has_line "$(svn_list "$dist_dev/ai-sessionizer")" "$version"; then
        fail "a candidate of $version is uploaded already, in $dev_dir. --no-upload would build other bytes than the ones the vote is about"
      fi
      # publish on another machine fills dist/$version with the voted files
      # and no vote.txt, so a released version is refused here too.
      if has_line "$(svn_list "$dist_release/ai-sessionizer")" "$version"; then
        fail "$version is released already, in $dist_release/ai-sessionizer/$version. --no-upload would replace the voted files in $out with a new build"
      fi
      say "no candidate of $version is uploaded"
    else
      say "cannot list $dist_dev, so whether a candidate of $version is uploaded was not checked"
    fi
  fi

  dev_has_dir=false
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
        fail "$dev_dir exists, so a candidate of $version was uploaded before. To replace it with a new candidate, remove it first:
  svn rm -m \"Remove the Apache SkyWalking AI Sessionizer $version candidate for a new one\" $dev_dir"
      fi
      say "$dist_dev/ai-sessionizer has no $version yet"
    else
      say "$dist_dev/ai-sessionizer does not exist; the upload creates it, since this is the first candidate"
    fi
  fi

  step "The signing key"
  work=$(mktemp -d)
  keyring="$work/keyring"
  cleanup() {
    # gpg can start helpers for the scratch keyring. Stop them with it.
    if command -v gpgconf >/dev/null 2>&1; then gpgconf --homedir "$keyring" --kill all >/dev/null 2>&1 || true; fi
    rm -rf "$work"
  }
  trap cleanup EXIT
  mkdir -m 700 "$keyring"
  if [ -z "${GPG_TTY:-}" ] && [ -t 0 ]; then GPG_TTY=$(tty); export GPG_TTY; fi
  # Sign a scratch file the way make release signs a package, and read the
  # signer's fingerprint from gpg's status output. A voter checks every
  # signature against KEYS, so a key missing there fails the vote however
  # good the packages are. That is found out here, before the build.
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
  # The ASF requires a release signing key to be RSA of at least 2048 bits,
  # and asks for 4096 bits in a new key. Recent gpg versions offer an
  # elliptic curve key by default, which passes every other check here. In
  # gpg's key listing a pub or sub record carries the length in field 3 and
  # the algorithm in field 4, where 1 and 3 are RSA. The fpr record after it
  # names that key.
  key=$(printf '%s\n' "$keys" | awk -F: -v f="$signed_with" '
    $1 == "pub" || $1 == "sub" {len = $3; algo = $4; next}
    $1 == "fpr" && $10 == f {print algo, len; exit}')
  algo=${key%% *}
  bits=${key##* }
  [ -n "$key" ] || fail "the key that signed, $signed_with, is not in the KEYS keyring"
  case "$algo" in
    1|3) ;;
    *) fail "the key that signed, $signed_with, is not RSA: gpg names its algorithm $algo. The ASF requires RSA keys of at least 2048 bits to sign releases. Create an RSA key of 4096 bits, add it to KEYS, and set GPG_USER to it" ;;
  esac
  [ "$bits" -ge 2048 ] || fail "the key that signed, $signed_with, is RSA of $bits bits. The ASF requires at least 2048 bits, and asks for 4096 in a new key"
  if [ "$bits" -lt 4096 ]; then say "warning  : the key is RSA of $bits bits. The ASF asks a new key to be 4096 bits"; fi
  # SkyWalking asks the signer to be named by an apache.org address, so a
  # voter can tell whose key it is. The user IDs are those of the primary
  # key, as KEYS holds them; a revoked one does not count.
  apache_uid=$(printf '%s\n' "$keys" | awk -F: -v f="$fpr" '
    $1 == "pub" {mine = 0; first = 1; next}
    first && $1 == "fpr" {mine = ($10 == f); first = 0; next}
    mine && $1 == "uid" && $2 != "r" {print $10}' | grep -Ei '@apache\.org>$' || true)
  [ -n "$apache_uid" ] || fail "the signing key $fpr has no user ID with an apache.org address in $keys_url. Add your apache.org address to the key as a user ID, and update KEYS with it"
  signer="${GPG_USER:-$fpr}"
  say "signer   : $fpr, RSA of $bits bits, in $keys_url as $(printf '%s\n' "$apache_uid" | sed -n 1p)"

  step "Build, verify, upload"
  say "- clone $origin_url at $tag into $out/build"
  say "- make release VERSION=$version GPG_USER=$signer, in the clone"
  say "- move the packages, each with its .asc and .sha512, into $out:"
  for p in $packages; do say "    $p"; done
  say "- verify every package: its .asc and .sha512 are there, shasum -a 512 -c passes, and gpg --verify passes against KEYS"
  say "- verify the source package holds LICENSE and NOTICE at its top level, no compiled file and no font file"
  if [ -n "$host_pkg" ]; then
    say "- run $host_pkg with $smoke from the clone, and stop if it fails"
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
    say "- write the vote mail to $out/vote.txt and print it"
  fi
  if [ "$dry_run" = true ]; then say "dry run: nothing was built, uploaded or written"; exit 0; fi

  step "Build $tag from a fresh clone of origin"
  # Never the working tree: the packages must be exactly what the tag holds,
  # whatever this checkout has on top of it.
  build="$out/build"
  rm -rf "$build"
  rm -f "$out/vote.txt" "$out/vote-preview.txt"
  for p in $packages; do rm -f "$out/$p" "$out/$p.asc" "$out/$p.sha512"; done
  mkdir -p "$build"
  git init -q "$build"
  git -C "$build" remote add origin "$origin_url"
  git -C "$build" fetch -q --depth 1 origin "refs/tags/$tag:refs/tags/$tag"
  git -C "$build" -c advice.detachedHead=false checkout -q "$tag"
  [ "$(git -C "$build" rev-parse HEAD)" = "$commit" ] || fail "the clone of $tag is not at $commit"
  (cd "$build" && make release VERSION="$version" GPG_USER="$signer")
  for f in "$build"/dist/*.tgz "$build"/dist/*.zip; do
    [ -f "$f" ] || continue
    for g in "$f" "$f.asc" "$f.sha512"; do
      [ -f "$g" ] || fail "make release did not write $g"
      mv "$g" "$out/"
    done
  done

  step "Verify the candidate in $out"
  for p in $packages; do
    for f in "$p" "$p.asc" "$p.sha512"; do [ -f "$out/$f" ] || fail "$out/$f is missing"; done
  done
  extra=""
  for f in "$out"/*.tgz "$out"/*.zip; do
    [ -f "$f" ] || continue
    has_line "$packages" "${f##*/}" || extra="$extra ${f##*/}"
  done
  [ -z "$extra" ] || fail "$out holds packages the Makefile in $tag does not name:$extra"
  for p in $packages; do
    (cd "$out" && shasum -a 512 --status -c "$p.sha512") || fail "the sha512 of $p does not match $p.sha512"
    signed_by=$(gpg --batch --homedir "$keyring" --status-fd 1 --verify "$out/$p.asc" "$out/$p" 2>/dev/null \
      | awk '$2 == "VALIDSIG" && !f {f = ($12 != "" ? $12 : $3)} END {print f}' || true)
    [ "$signed_by" = "$fpr" ] || fail "the signature of $p does not verify against $keys_url as made by $fpr"
    say "ok  $p: sha512, and signed by $fpr"
  done
  # The ASF does not allow compiled code in a source release. file(1) is
  # asked for the type of every file, with the types the tag's Makefile
  # names, as make release asks of the tree.
  src="$pkg-$version-src.tgz"
  top="$pkg-$version-src"
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

  step "Run the package for this machine"
  if [ -n "$host_pkg" ]; then
    # bash runs the script whatever mode the clone gave it.
    bash "$build/$smoke" "$out/$host_pkg" "$version" \
      || fail "$host_pkg did not pass $smoke, run from the $tag clone. Its output is above. Nothing was uploaded, and no vote mail was written"
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
 * internal/view/conversation-view/ in the source package is the build output of Horizon's conversation renderer, from apache/skywalking-horizon-ui at the commit its HORIZON_COMMIT file names. It is Apache-2.0 code of the ASF with no third-party code in it. The source package builds and runs with it as it is. \`make conversation-view-check\`, which needs Node.js 24 and pnpm, rebuilds it from that commit and compares.
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
    say "not uploaded. $mail shows the mail, with the checksums of this build, which is not a candidate."
    say "Run candidate again without --no-upload to build, sign and upload the candidate the vote is about."
  else
    say "Next:"
    say "  1. Check every link, then send $out/vote.txt to dev@skywalking.apache.org."
    say "  2. After 72 hours, count the votes:"
    say "     tools/release.sh vote-result $version --binding \"Name, Name, Name\" [--non-binding \"...\"] [--against \"...\"]"
  fi
  exit 0
fi

# ------------------------------------------------------------- vote-result
if [ "$cmd" = vote-result ]; then
  step "The version"
  pick_version "Version the vote was for"
  [ -n "$binding" ] || fail "--binding is required: the PMC members who voted +1, separated by commas"
  if [ -n "$thread" ]; then
    case "$thread" in https://*) ;; *) fail "--thread takes the https link of the vote thread, such as its page on lists.apache.org" ;; esac
  fi
  # Runs of spaces inside a name count as one, so "Ann  Lee" and "Ann Lee"
  # are the same voter.
  split_names() { printf '%s\n' "$1" | tr ',' '\n' | sed -E 's/[[:space:]]+/ /g; s/^ //; s/ $//' | grep -v '^$' || true; }
  count_lines() { if [ -z "$1" ]; then echo 0; else printf '%s\n' "$1" | wc -l | tr -d ' '; fi; }
  join_names() { printf '%s\n' "$1" | awk 'NR > 1 {printf ", "} {printf "%s", $0} END {print ""}'; }
  plural() { if [ "$1" -eq 1 ]; then printf '%s %s' "$1" "$2"; else printf '%s %ss' "$1" "$2"; fi; }
  yes_binding=$(split_names "$binding")
  yes_other=$(split_names "$non_binding")
  no_binding=$(split_names "$against")
  no_other=$(split_names "$non_binding_against")
  zero=$(split_names "$abstain")
  # Names are compared without case. "Alice" as binding and "alice" as
  # non-binding were once counted as two voters.
  twice=$(printf '%s\n' "$yes_binding" "$yes_other" "$no_binding" "$no_other" "$zero" | grep -v '^$' | tr '[:upper:]' '[:lower:]' | sort | uniq -d || true)
  [ -z "$twice" ] || fail "each voter counts once, and these are listed more than once, compared without case: $(join_names "$twice")"
  n_yes=$(count_lines "$yes_binding")
  n_other=$(count_lines "$yes_other")
  n_no=$(count_lines "$no_binding")
  n_no_other=$(count_lines "$no_other")
  n_zero=$(count_lines "$zero")

  step "The count"
  say "binding +1     : $n_yes"
  say "non-binding +1 : $n_other"
  say "binding -1     : $n_no"
  say "non-binding -1 : $n_no_other"
  say "+0             : $n_zero"
  # The Apache rule for a release: at least three binding +1 votes, and
  # more binding +1 than binding -1 votes. Only PMC votes are binding. The
  # other votes are listed in the mail, so a -1 that reports a real problem
  # stays on record, but they do not change the result.
  [ "$n_yes" -ge 3 ] || fail "the vote has not passed: it needs at least three binding +1 votes and has $n_yes"
  [ "$n_yes" -gt "$n_no" ] || fail "the vote has not passed: it needs more binding +1 than binding -1 votes, and has $n_yes against $n_no"
  say "the vote passed, provided it was open for at least 72 hours"
  if [ -z "$thread" ]; then say "no --thread: the mail does not link the vote thread. Give its link on lists.apache.org with --thread to add it"; fi

  step "The result mail"
  result_mail() {
    local parts=() counts="" i last
    parts+=("$(plural "$n_yes" "+1 binding")")
    if [ "$n_other" -gt 0 ]; then parts+=("$(plural "$n_other" "+1 non-binding")"); fi
    if [ "$n_no" -gt 0 ]; then parts+=("$(plural "$n_no" "-1 binding")"); fi
    if [ "$n_no_other" -gt 0 ]; then parts+=("$(plural "$n_no_other" "-1 non-binding")"); fi
    if [ "$n_zero" -gt 0 ]; then parts+=("$n_zero +0"); fi
    last=$(( ${#parts[@]} - 1 ))
    for i in "${!parts[@]}"; do
      if [ "$i" -eq 0 ]; then counts="${parts[0]}"
      elif [ "$i" -eq "$last" ]; then counts="$counts and ${parts[$i]}"
      else counts="$counts, ${parts[$i]}"; fi
    done
    printf 'Subject: [RESULT][VOTE] Release %s version %s\n\n' "$project" "$version"
    printf '72 hours passed, we have got %s:\n\n' "$counts"
    if [ -n "$thread" ]; then printf 'The vote thread: %s\n\n' "$thread"; fi
    printf '+1 bindings:\n%s\n' "$yes_binding"
    if [ -n "$yes_other" ]; then printf '\n+1 non-bindings:\n%s\n' "$yes_other"; fi
    if [ -n "$no_binding" ]; then printf '\n-1 bindings:\n%s\n' "$no_binding"; fi
    if [ -n "$no_other" ]; then printf '\n-1 non-bindings:\n%s\n' "$no_other"; fi
    if [ -n "$zero" ]; then printf '\n+0:\n%s\n' "$zero"; fi
    printf '\nThe binding votes, those of PMC members, decide the result: %s +1 and %s -1, so the vote passed.\n' "$n_yes" "$n_no"
    printf '\nThank you for voting, I will continue the release process.\n'
  }
  if [ "$dry_run" = true ]; then
    result_mail
    say "----"
    say "dry run: $out/result.txt was not written"
    exit 0
  fi
  mkdir -p "$out"
  result_mail > "$out/result.txt"
  cat "$out/result.txt"
  say "----"
  say "Next:"
  say "  1. Send $out/result.txt to dev@skywalking.apache.org."
  say "  2. A PMC member runs: tools/release.sh publish $version"
  exit 0
fi

# ----------------------------------------------------------------- publish
if [ "$cmd" = publish ]; then
  step "The version"
  pick_version "Version the vote passed for"
  # tar and awk are for tools/install-manifests.sh, which runs after the
  # move. Checked here, a missing one stops the run before anything moves.
  need_tools git svn shasum tar awk
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
  say "- write the announcement to $out/announce.txt"
  say "- write the website entries to $out/website.txt"
  say "- $manifests $version $out $out/install"
  if [ "$dry_run" = true ]; then say "dry run: nothing was moved, removed, fetched or written"; exit 0; fi

  mkdir -p "$out"
  for p in $fetch; do
    for f in "$p" "$p.asc" "$p.sha512"; do asf_svn export -q --force "$from/$f" "$out/$f"; done
    (cd "$out" && shasum -a 512 --status -c "$p.sha512") || fail "$p fetched from $from does not match its sha512"
    say "fetched $p"
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
  say "  1. tools/release.sh complete $version"
  say "     It creates the GitHub release and attaches the voted packages, once"
  say "     downloads.apache.org serves them, a short while after the move."
  say "  2. At least one hour after the move, as the ASF release policy asks, open a pull"
  say "     request on apache/skywalking-website with $out/website.txt: the downloads entry"
  say "     in data/releases.yml and the $tag documentation in data/docs.yml."
  say "  3. Once the website lists $version, and at least one hour after the move, send"
  say "     $out/announce.txt to dev@skywalking.apache.org and announce@apache.org, as plain"
  say "     text from your apache.org address. Sign it with your release key if your mail"
  say "     program can; the ASF recommends it."
  say "  4. Submit the install manifests in $out/install, once the PMC has agreed on"
  say "     dev@skywalking.apache.org to each channel. $out/install/README.md says where each"
  say "     one goes. The Homebrew formula and the winget files download from the GitHub"
  say "     release, so both wait for step 1."
  if [ -n "$old" ] && [ "$remove_old" = false ]; then
    say "  5. Later, once the website pull request that points$old at archive.apache.org"
    say "     has merged, and the Scoop bucket names $version:"
    say "     tools/release.sh publish $version --remove-old"
  fi
  exit 0
fi

# ---------------------------------------------------------------- complete
if [ "$cmd" = complete ]; then
  step "The version"
  pick_version "Version to release on GitHub"
  need_tools git gh curl shasum
  fetch_tag
  if gh release view "$tag" >/dev/null 2>&1; then fail "the GitHub release $tag already exists"; fi
  platforms=$(tag_platforms)
  packages=$(expected_packages)

  step "The voted packages"
  # The GitHub release carries the files the vote approved and nothing else.
  # Each local file is held against the one downloads.apache.org serves from
  # the release directory, and all of them are checked before anything is
  # created, since the release triggers the image.
  assets=()
  for p in $packages; do
    for f in "$p" "$p.asc" "$p.sha512"; do
      [ -f "$out/$f" ] || fail "$out/$f is missing; publish fetches the voted files into $out"
    done
    served=$(curl -fsSL "$downloads/$version/$p.sha512") || fail "$downloads/$version/$p.sha512 is not served yet. downloads.apache.org serves the release directory a short while after publish; run complete again then"
    [ "$served" = "$(cat "$out/$p.sha512")" ] || fail "$out/$p.sha512 is not the released checksum file"
    sum=$(shasum -a 512 "$out/$p")
    [ "${sum%%[[:space:]]*}" = "${served%%[[:space:]]*}" ] || fail "$out/$p is not the released package: its sha512 differs"
    served=$(curl -fsSL "$downloads/$version/$p.asc") || fail "cannot read $downloads/$version/$p.asc"
    [ "$served" = "$(cat "$out/$p.asc")" ] || fail "$out/$p.asc is not the released signature"
    say "ok  $p, with its .asc and .sha512"
    assets+=("$out/$p" "$out/$p.asc" "$out/$p.sha512")
  done

  step "The release"
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
  text=$(release_text)
  say "tag      : $tag"
  say "title    : $version"
  say "text     : $dev_page as $tag holds it, then where to get $version"
  say "assets   : ${#assets[@]} files, the voted packages with their .asc and .sha512"
  say "---- the text of the release"
  printf '%s\n' "$text"
  say "----"
  if [ "$dry_run" = true ]; then say "dry run: no release created and nothing uploaded"; exit 0; fi
  tmp=$(mktemp)
  trap 'rm -f "$tmp"' EXIT
  printf '%s\n' "$text" > "$tmp"
  gh release create "$tag" --verify-tag --title "$version" --notes-file "$tmp"
  say "created. CI publishes the image when the released event fires; nothing to wait for here."
  gh release upload "$tag" "${assets[@]}" || fail "the upload stopped part way. Finish it with: gh release upload $tag $out/$pkg-$version-* --clobber"
  say "attached ${#assets[@]} files to the GitHub release $tag: the voted packages, their signatures and their checksums"
  say "The Homebrew formula and the winget manifests in $out/install download from this GitHub release. Both can be submitted now, once the PMC has agreed to each channel."
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
if [ "$dry_run" = true ]; then say "dry run: nothing was written, pushed or opened"; exit 0; fi
if [ "$no_push" = true ]; then
  cat <<NEXT
not pushed. When ready:
  git push -u origin $branch
  git push origin $tag
  gh pr create --base $from --head $branch --title "Prepare the $version candidate and open $next"
then, once the pull request is merged, build and upload the candidate for the vote:
  tools/release.sh candidate $version
NEXT
  exit 0
fi
git push -u origin "$branch"
git push origin "$tag"
gh pr create --base "$from" --head "$branch" --title "Prepare the $version candidate and open $next" \
  --body "The first commit removes the in-development note from the $version changelog, docs/en/changes/changes.md. Tag $tag is on it, and is the candidate for the vote. The second commit moves the changelog to changes-$version.md, lists $version under Changelog, and opens $next in a new changes.md. After merging, run \`tools/release.sh candidate $version\` to build the candidate and upload it for the vote."
say "pushed $branch and $tag; pull request opened. After it merges: tools/release.sh candidate $version"
