# How to Release

This guide is for the release manager, and for anyone checking a release candidate before voting.
Apache SkyWalking AI Sessionizer follows the Apache release process. The SkyWalking PMC votes on a
candidate, and only after the vote passes is anything published as a release.

`tools/release.sh` runs the stages, one command for each. None of them pushes to `main`. For every
stage this page shows the command, says what it does, and lists the svn commands it runs, so a
release manager can finish a stage by hand when the script stops.

## What counts as a release

A release is the PMC vote, the signed packages in the release directory on dist.apache.org, and
the announcement on the website and the announce list. Nothing else is. Until the announcement, a
version is not an official release, whatever GitHub shows.

- **The source package** is what the PMC votes on. It is the Apache release.
- **The binary packages** are the convenience binaries of the vote. They are built from the tagged
  source, and signed and checksummed like the source package. They sit beside it in the candidate
  directory, and voters check them too.
- **A git tag** is a candidate. `prepare` pushes it before the vote, so a tag alone says nothing
  about whether a version was released.
- **The GitHub release** is a convenience. From 0.3.0 on, it is created after the release, and it
  carries the voted packages.
- **The container image** is a convenience, and how it is published for a release is still
  pending. Today, creating the GitHub release starts CI's image job. Nothing in the release waits
  for it.
- **The Homebrew, Scoop and winget manifests** are conveniences, submitted after the release.

0.3.0 is the first version to go through this process. 0.1.0 and 0.2.0 were published on GitHub
before it, as full GitHub releases carrying binary packages that CI built. No vote was held for
them, their packages are not signed, and they are not Apache releases. Whether to mark those two
GitHub releases as pre-releases, or to remove their packages, is for the PMC to decide.

## The stages

| Stage | Run by | What it leaves |
| --- | --- | --- |
| [1. Prepare](#1-prepare) | the release manager | the tag `v$VERSION` on GitHub, and a pull request |
| [2. Candidate](#2-candidate) | the release manager | the signed candidate in the dev area of dist.apache.org, and the vote mail |
| [3. The vote](#3-the-vote) | the PMC | the votes on the dev list, over at least 72 hours |
| [4. Vote result](#4-vote-result) | the release manager | the result mail |
| [5. Publish](#5-publish) | a PMC member | the voted packages in the release directory, and the files the later stages use |
| [6. Complete](#6-complete) | the release manager | the GitHub release, carrying the voted packages |
| [7. The website](#7-the-website) | the release manager | the downloads entry and the documentation of the version |
| [8. The announcement](#8-the-announcement) | the release manager | the mail to the dev and announce lists |
| [9. Install manifests](#9-install-manifests) | the release manager, once the PMC agrees to each channel | the Homebrew, Scoop and winget manifests, each submitted |
| [Later: remove old versions](#later-remove-old-versions) | a PMC member | the release directory without the versions the new one replaces |

Stages 1, 2, 4, 5 and 6 are commands of `tools/release.sh`. What they share:

- Each takes the version as its first argument, or asks for it. `prepare` offers the version the
  heading of `docs/en/changes/changes.md` names. `candidate`, `vote-result`, `publish` and
  `complete` offer the newest version `prepare` has finished, read from the newest
  `docs/en/changes/changes-X.Y.Z.md` in the checkout. `prepare` makes that page in the commit
  after the tag, and `main` holds it once the prepare pull request has merged.
- `candidate`, `vote-result` and `publish` write their files under `dist/$VERSION/` in the
  checkout, and `complete` reads the voted packages from there. `prepare` writes nothing there.
  Git ignores `dist/`.
- `--dry-run` prints what the stage would do. It makes no commit and no push, changes nothing on
  dist.apache.org or GitHub, and writes no file in `dist/`. `candidate`, `publish` and `complete`
  read the tag to make the plan. So, like a real run, they fetch `v$VERSION` from origin into the
  local repository when it does not have the tag. They fetch that one tag and no other.
- An option that belongs to another stage is refused, never ignored.
- When `APACHE_ID` is set, every svn command runs as `svn --username "$APACHE_ID"`. Set it when
  your local user name is not your Apache ID.
- `tools/release.sh --help` prints every stage and its options.

## Before the first release

1. **A signing key in KEYS.** Create an RSA key of 4096 bits with your apache.org address as its
   user ID, as the [ASF release signing guide](https://infra.apache.org/release-signing.html)
   describes. The ASF requires a release signing key to be RSA of at least 2048 bits, and asks a
   new key to be 4096 bits. Recent gpg versions offer an elliptic curve key by default, so run
   `gpg --full-generate-key`, choose "RSA and RSA", and give 4096 as the size. Publish the public
   key to a key server, and add its fingerprint to your account at
   [id.apache.org](https://id.apache.org/). Then add the key to the SkyWalking KEYS file. Only a
   PMC member can commit to that file:

   ```sh
   svn checkout --depth files https://dist.apache.org/repos/dist/release/skywalking skywalking-release
   cd skywalking-release
   (gpg --list-sigs <key id> && gpg --armor --export <key id>) >> KEYS
   svn commit -m "Add the key of <your name>" KEYS
   ```

   `candidate` refuses to build with a key that is not in KEYS, because every voter checks the
   signatures against that file. It also refuses a key that has expired or is revoked in KEYS, a
   key that is not RSA of at least 2048 bits, and a key that has no user ID with an apache.org
   address. gpg reads the expiry of a key from KEYS, not from your machine. So when you extend
   your key, have a PMC member commit the renewed public key to KEYS before the next candidate.
2. **The tools.** git, Go 1.27 or later, make, gpg, shasum, tar, zip, unzip, file, curl, svn, and
   gh logged in to an account that can write to the repository. `prepare`, `candidate`, `publish`
   and `complete` check for the tools they use before they change anything, and name any that is
   missing. `vote-result` checks for none, because it needs only standard tools such as sed and
   sort. `prepare` needs gh only when it pushes.
3. **svn access.** Uploading a candidate needs write access to the dev area,
   `https://dist.apache.org/repos/dist/dev/skywalking`. Moving it to the release directory needs a
   PMC member.
4. **The directories.** Neither `dist/dev/skywalking/ai-sessionizer` nor
   `dist/release/skywalking/ai-sessionizer` exists before the first release. The upload of the
   first candidate creates the first one. The first `publish` creates the second one with
   `svn mkdir`.

## The changelog

The changelog is part of the documentation. The SkyWalking website publishes the docs of each
version from the commit of its tag, the changelog with them. It has the layout Apache SkyWalking
and SkyWalking SWCK use: one page for the version in development, and one page for each finished
version. Their tags show the order. Apache SkyWalking `v10.3.0` and SkyWalking SWCK `v0.11.0` each
hold `docs/en/changes/changes.md` headed with their own version, and the page moves to its own
name in a later commit.

- On `main`, `docs/en/changes/changes.md` is the changelog of the version in development. Its
  heading names that version, as in `# Changes in 0.4.0`, and a note under the heading starts
  `> In development`. Current Version in `docs/menu.yml` points at `/en/changes/changes`, and the
  welcome page links `en/changes/changes.md`, so neither has to change at a release.
- In the commit it tags, `prepare` only removes the note. So the tag and the source package hold
  the finished changelog at `changes.md`, where the menu and the welcome page of the tag link it.
  The vote mail, the announcement and the GitHub release link that page at the tag, and
  `complete` builds the text of the GitHub release from it.
- In the next commit, `prepare` moves the page to `docs/en/changes/changes-$VERSION.md`, the
  changelog of that one version. It lists the version under Changelog in `docs/menu.yml`, right
  after Current Version, so the versions read newest first. It writes a new `changes.md` for the
  next version, with its heading and the note. `prepare` moves the page with `git mv` in this one
  commit, so `git log --follow` traces the page back through the move.

## Before each release

1. Close every issue in the milestone, or move what is unfinished to the next one.
2. Make sure the changelog is complete. It is `docs/en/changes/changes.md`, and its heading names
   `$VERSION`.
3. Make sure this page is right. The vote mail links this page as it is in the tag, so a change
   made after `prepare` does not reach the voters.
4. `make check` passes on `main`.

## 1. Prepare

```sh
git checkout main && git pull
tools/release.sh prepare
```

It asks for the version to release and the version development moves to, or takes them as
arguments. It offers the version the heading of `docs/en/changes/changes.md` names. On a branch
`release/$VERSION` cut from `main`, it then does these steps in order:

1. Checks for git, make, Go, awk, sed, grep and sort, and for gh unless `--no-push` is given.
   Refuses a dirty tree, an existing tag or branch, and a next version that does not come after
   this one. Refuses too when `changes.md` does not name `$VERSION` in its heading or has no
   in-development note, when `changes-$VERSION.md` or `changes-$NEXT.md` exists, and when Current
   Version in `docs/menu.yml` does not point at `/en/changes/changes`.
2. Checks the license headers and runs `make check`. `--skip-check` leaves out `make check`.
3. Removes the in-development note from `docs/en/changes/changes.md`, and leaves the page at its
   path and the menu and the welcome page as they are. It commits "Prepare the $VERSION candidate"
   and puts the annotated tag `v$VERSION` on that commit. So the tag holds the finished changelog
   at `changes.md`, where the menu and the welcome page of the tag link it. The tag's message is
   `Apache SkyWalking AI Sessionizer $VERSION`. Neither calls the tag a release, because the tag
   is the candidate.
4. In a second commit, "Open $NEXT", moves `docs/en/changes/changes.md` to
   `docs/en/changes/changes-$VERSION.md` with `git mv`, and lists the version under Changelog in
   `docs/menu.yml`, right after Current Version. It writes a new `docs/en/changes/changes.md` for
   `$NEXT`, with the heading `# Changes in $NEXT` and the in-development note. Current Version and
   the welcome page link `changes.md` already, so they stay as they are.
5. Pushes the branch and the tag, and opens the pull request "Prepare the $VERSION candidate and
   open $NEXT" against `main`. The pull request names `candidate` as the next step.

By hand, the two commits are:

```sh
git checkout -b release/$VERSION
# Remove the in-development note from docs/en/changes/changes.md.
git commit -am "Prepare the $VERSION candidate"
git tag -a v$VERSION -m "Apache SkyWalking AI Sessionizer $VERSION"
git mv docs/en/changes/changes.md docs/en/changes/changes-$VERSION.md
# List $VERSION in docs/menu.yml. Write docs/en/changes/changes.md for $NEXT, with its heading
# and the in-development note.
git add docs/menu.yml docs/en/changes/changes.md
git commit -m "Open $NEXT"
```

The second commit lists the version right after Current Version, so the Changelog of
`docs/menu.yml` starts:

```yaml
    - name: Changelog
      catalog:
        - name: Current Version
          path: /en/changes/changes
        - name: $VERSION
          path: /en/changes/changes-$VERSION
```

`--dry-run` prints the plan and writes nothing. It installs nothing either. It runs
`make license-check` only when `bin/license-eye` is there, and `make check` only when
`bin/license-eye` and `bin/golangci-lint` are there. Otherwise it lists the check it left out.
`--no-push` stops after the commits and prints the push and pull request commands. `prepare` runs
no svn command.

Review and merge the pull request. From here the tag is on GitHub, as a candidate.

## 2. Candidate

After the prepare pull request has merged:

```sh
GPG_USER=<key id, fingerprint or email> tools/release.sh candidate $VERSION
```

`GPG_USER` picks the signing key. When it is empty, gpg's default key signs. If gpg fails without
asking for the passphrase, run `export GPG_TTY=$(tty)` first.

It does these steps in order, and stops at the first one that fails.

1. **Check the tools and the tag.** `v$VERSION` must be on origin. Its
   `docs/en/changes/changes.md` must name `$VERSION` in its heading and carry no in-development
   note, as `prepare` leaves it in the commit it tags. The vote mail links that page. A local tag
   of the same name must be the same object as the one on origin. When there is no local tag, it
   fetches that one tag from origin, in a dry run too, and says so.
2. **Name the packages.** They are the ones the Makefile in the tag builds: the source package,
   and one binary package for each entry in its `PLATFORMS`. The tag is read, never the working
   tree, so a platform added later is never demanded of an older version. It also finds the
   package for this machine, which step 8 runs. `uname` gives the system and the processor:
   macOS, Linux, or Windows under Git Bash or MSYS2, on x86-64 or ARM 64. When `PLATFORMS` has
   that platform, the tag must carry `tools/package-smoke.sh`. The `machine` line of the output
   names the package, or says that no package is built for this machine.
3. **Look for fonts in the source package.** It lists what `git archive` of the tag holds, which
   is what the source package will hold, and refuses a font file: `.woff`, `.woff2`, `.ttf`,
   `.otf` or `.eot`. Fonts come under licenses such as the SIL Open Font License, which the ASF
   puts in [Category B](https://www.apache.org/legal/resolved.html), and a Category B work must
   not be in a source release. The two fonts of the conversation renderer are marked
   `export-ignore` in `.gitattributes`, so `git archive` leaves them out, and they are in the
   binary packages only. `--dry-run` runs this step too.
4. **Check the candidate directory.** With `--no-upload`, see the paragraph after these steps.

   ```sh
   svn ls https://dist.apache.org/repos/dist/dev/skywalking
   svn ls https://dist.apache.org/repos/dist/release/skywalking
   svn ls https://dist.apache.org/repos/dist/release/skywalking/ai-sessionizer   # when it exists
   svn ls https://dist.apache.org/repos/dist/dev/skywalking/ai-sessionizer       # when it exists
   ```

   It refuses when `$VERSION` is in the release directory already. It also refuses when a
   candidate of `$VERSION` was uploaded before, and prints the command that removes that
   candidate:

   ```sh
   svn rm -m "Remove the Apache SkyWalking AI Sessionizer $VERSION candidate for a new one" \
     https://dist.apache.org/repos/dist/dev/skywalking/ai-sessionizer/$VERSION
   ```

5. **Check the signing key.** It signs a scratch file with the key, and reads from gpg's status
   output the key that signed and its primary key. It downloads KEYS from
   `https://dist.apache.org/repos/dist/release/skywalking/KEYS`, imports it into an empty scratch
   keyring, and refuses when:
   - the primary key is not there. A voter checks every signature against KEYS, so a key missing
     there is found before the build, not during the vote.
   - gpg marks the primary key, or the subkey that signs, as expired or revoked in that keyring.
     Every voter's gpg reads the same KEYS, and would warn about it. The
     [ASF release signing guide](https://infra.apache.org/release-signing.html) counts a signature
     as valid only when gpg verifies it as a good signature and does not complain about expired
     or revoked keys. The usual case is a key extended on your machine, while KEYS still holds the
     old copy.
   - gpg, reading that keyring, does not report the signature of the scratch file as good, or
     reports the key that signed, or the signature, as expired or revoked. Those are the
     `EXPKEYSIG`, `REVKEYSIG` and `EXPSIG` lines of its status output. A KEYS entry that also
     holds an old subkey that has expired, beside a newer subkey that signs, is not refused. gpg
     prints a good signature by the newer subkey and nothing about the old one. Its status output
     has a `KEYEXPIRED` line for the old subkey. That line is not about the signature, so
     `candidate` does not refuse it.
   - the key that signed is not RSA, or has fewer than 2048 bits. The ASF requires both. A key of
     fewer than 4096 bits gets a warning, because the ASF asks a new key to be 4096 bits.
   - no user ID of the key, as KEYS holds it, has an apache.org address. A revoked or expired user
     ID does not count.
6. **Build from a fresh clone of the tag.** It never builds from the working tree, so the packages
   hold exactly what the tag holds. By hand:

   ```sh
   git init dist/$VERSION/build
   git -C dist/$VERSION/build remote add origin <the origin URL of this checkout>
   git -C dist/$VERSION/build fetch --depth 1 origin refs/tags/v$VERSION:refs/tags/v$VERSION
   git -C dist/$VERSION/build checkout v$VERSION
   (cd dist/$VERSION/build && make release VERSION=$VERSION GPG_USER=<key>)
   mv dist/$VERSION/build/dist/*.tgz* dist/$VERSION/build/dist/*.zip* dist/$VERSION/
   ```

   `make release` refuses to run unless the tag is checked out, and the tree has no change and no
   file that git does not track, ignored files included. Only its build output may be there, in
   `dist/`, `bin/` and `plugins/claude-code/bin/`. An untracked Go file would be compiled into the
   binaries. `asz` embeds every file in `internal/view/conversation-view/` whose name does not
   start with `.` or `_`. The binary packages take the plugin's `.claude-plugin/` and `hooks/` and
   `dist-material/licenses/` whole, so an ignored `.DS_Store` there would be packaged. The source
   package, made from the tag, holds none of them. The binaries are built with `GOWORK=off` and
   `GOFLAGS=-mod=readonly`, so a `go.work` in the tree or in a directory above it does not change
   what they are built from.
   `make release` also refuses when git tracks a file of a type `COMPILED_TYPES` in the Makefile
   names, or with a name `COMPILED_FILES` names, because an Apache source release must not carry
   compiled code. `COMPILED_FILES` is for the compiled files `file` cannot tell by type: the
   `file` 5.41 that ships with macOS reports a WebAssembly module and a Python `.pyc` file as
   `application/octet-stream`. It removes packages left in `dist/` by an earlier build, so none is
   signed with these.
   Then it builds the packages [described below](#what-make-release-builds). Right after
   `git archive`, it lists the source package and stops when a font file is in it. It looks for
   the file types step 3 looks for, which `FONT_FILES` in the Makefile names. It removes that
   source package before any checksum or signature is made, so it cannot be uploaded by hand.
7. **Verify the candidate** in `dist/$VERSION/`.
   - Every package is there with its `.asc` and `.sha512`, and no other package is.
   - `shasum -a 512 -c` passes for each package.
   - gpg, reading the KEYS keyring, reports a good signature by the checked key for each package.
     It does not report the key that signed, or the signature, as expired or revoked: no
     `EXPKEYSIG`, `REVKEYSIG` or `EXPSIG` line. gpg exits 0 for a key that has expired in KEYS
     too, so its exit status is not enough.
   - The source package holds `LICENSE` and `NOTICE` at its top level, and nothing outside its top
     directory. It holds no font file. `file` finds no file of a type `COMPILED_TYPES` in the
     tag's Makefile names, and no file has a name `COMPILED_FILES` there names.
   - Each binary package holds `asz`, `claude-code-plugin/bin/asz-claude-plugin`,
     `claude-code-plugin/.claude-plugin/`, `claude-code-plugin/hooks/`, `LICENSE`, `NOTICE` and
     `licenses/`. On Windows the two binaries end in `.exe`.
8. **Run the package for this machine** with `tools/package-smoke.sh`, the check a voter runs in
   item 9 of the [check list for voters](#check-list-for-voters). `make release` cross-compiles
   every platform on this machine, and a binary that was never started proves nothing about its
   platform. The script and its scenarios come from the clone of the tag, never from the working
   tree, because a scenario newer than the tag may need what the tag's binary does not have. By
   hand:

   ```sh
   bash dist/$VERSION/build/tools/package-smoke.sh dist/$VERSION/<package for this machine> $VERSION
   ```

   When the check fails, `candidate` stops, so nothing is uploaded and no vote mail is written.
   When no package is built for this machine, it says so and goes on.
9. **Upload** from a scratch working copy. `--no-upload` skips this step. By hand:

   ```sh
   svn checkout --depth empty https://dist.apache.org/repos/dist/dev/skywalking dev
   cd dev
   svn update --set-depth immediates ai-sessionizer   # leave out on the first release
   mkdir -p ai-sessionizer/$VERSION
   cp <checkout>/dist/$VERSION/apache-skywalking-ai-sessionizer-$VERSION-* ai-sessionizer/$VERSION/
   svn add ai-sessionizer/$VERSION                    # svn add ai-sessionizer on the first release
   svn commit -m "Add the Apache SkyWalking AI Sessionizer $VERSION release candidate"
   ```

10. **Write the vote mail** to `dist/$VERSION/vote.txt`, with every link and checksum filled in,
    the fingerprint of the signing key, and the notes for voters, and print it.

`--no-upload` builds, verifies and runs the package for this machine, and uploads nothing. It
writes the mail to `dist/$VERSION/vote-preview.txt`, never to `vote.txt`, because its checksums
are those of a build that is not a candidate. It refuses when `dist/$VERSION/vote.txt` is there,
because a candidate was uploaded from this checkout. When svn is installed, it also lists the dev
area and the release directory, and refuses when a candidate of `$VERSION` is there or `$VERSION`
is released already. A new build would replace the uploaded or voted files in `dist/$VERSION/`,
and its signatures at least would differ from them.

`--dry-run` builds, runs and uploads nothing, and writes nothing into `dist/$VERSION/`. It still
fetches a missing tag, as step 1 says. It still checks the signing key as step 5 does: it signs a
scratch file, so gpg may ask for the passphrase, and it reads svn and KEYS. The scratch file, its
signature and the KEYS keyring are in a temporary directory that it removes. Its plan names the
package it would run on this machine.

Running `candidate` again rebuilds everything in `dist/$VERSION/` from the tag. After an upload it
refuses, as step 4 says.

### What make release builds

`make release VERSION=$VERSION`, with the tag checked out, writes into `dist/`:

- `apache-skywalking-ai-sessionizer-$VERSION-src.tgz`, the source package. It is `git archive` of
  the tag, so it holds what is committed, under one directory,
  `apache-skywalking-ai-sessionizer-$VERSION-src/`, less what `.gitattributes` marks
  `export-ignore`. That leaves out the two fonts of the conversation renderer, which are under the
  SIL Open Font License, a Category B license. A build from the source package draws the page with
  system fonts.
- `apache-skywalking-ai-sessionizer-$VERSION-bin-<os>-<arch>.tgz`, or `.zip` for Windows, one
  binary package for each platform. Each holds `asz` (`asz.exe` on Windows), `claude-code-plugin/`
  with the Claude Code plugin's `.claude-plugin/`, `hooks/` and `bin/asz-claude-plugin`, and the
  `LICENSE`, `NOTICE` and `licenses/` of a binary distribution, generated into `dist-material/`.
  They name the modules built into the two binaries on the six platforms, and no module that
  only tests need. The binaries embed the two fonts, which `dist-material/LICENSE` names.
- A `.sha512` checksum and an `.asc` signature beside every package. `GPG_USER` picks the key.
  The first package that cannot be checksummed or signed stops the release.

The platforms are the `PLATFORMS` list in the Makefile: macOS on Apple silicon and Intel, Linux on
x86-64 and ARM 64, and Windows on x86-64 and ARM 64. That is seven packages with the source
package. Every platform is cross-compiled from the release manager's machine without cgo, with the
Go on the release manager's `PATH`. It must be at least the version `go.mod` declares. With an
older Go, the go command stops, or downloads that version and builds with it, as `GOTOOLCHAIN`
decides. `asz version` and the plugin's `version` print the Go version that built them, so a voter
can see which one the release manager used.

Two builds of the tag give the same bytes when they use the same Go, the same tar and the same
gzip. Go records the tag in each binary as the module's version, so a build of the same commit made
before it was tagged differs. Every file in a binary package gets the time of the tag's
commit and the same modes, the entries are stored in one sorted order with owner and group 0, and
neither gzip nor zip stores a time or a local user of its own. So no package records who built it
or when. GNU tar and bsdtar write their headers differently, and GNU gzip and Apple's gzip compress
differently. So a package CI builds, with GNU tar and GNU gzip, differs from one built on macOS,
with bsdtar and Apple's gzip. The signatures still differ from one signing to the next.

## 3. The vote

Check every link in `dist/$VERSION/vote.txt`, then send it to `dev@skywalking.apache.org`. By hand,
the mail is:

```text
Subject: [VOTE] Release Apache SkyWalking AI Sessionizer version $VERSION

Hi the SkyWalking Community:
This is a call for vote to release Apache SkyWalking AI Sessionizer version $VERSION.

Release notes:
 * https://github.com/apache/skywalking-ai-sessionizer/blob/v$VERSION/docs/en/changes/changes.md

Release Candidate:
 * https://dist.apache.org/repos/dist/dev/skywalking/ai-sessionizer/$VERSION
 * sha512 checksums
   - <sha512>  apache-skywalking-ai-sessionizer-$VERSION-src.tgz
   - <sha512>  apache-skywalking-ai-sessionizer-$VERSION-bin-darwin-arm64.tgz
   - <sha512>  apache-skywalking-ai-sessionizer-$VERSION-bin-darwin-amd64.tgz
   - <sha512>  apache-skywalking-ai-sessionizer-$VERSION-bin-linux-amd64.tgz
   - <sha512>  apache-skywalking-ai-sessionizer-$VERSION-bin-linux-arm64.tgz
   - <sha512>  apache-skywalking-ai-sessionizer-$VERSION-bin-windows-amd64.zip
   - <sha512>  apache-skywalking-ai-sessionizer-$VERSION-bin-windows-arm64.zip

Release Tag:
 * (Git Tag) v$VERSION

Release Commit Hash:
 * https://github.com/apache/skywalking-ai-sessionizer/tree/<commit hash>

Keys to verify the Release Candidate:
 * https://dist.apache.org/repos/dist/release/skywalking/KEYS
 * Signed with key <fingerprint of the primary key>, which is in KEYS.

Guide to build the release from source:
 * https://github.com/apache/skywalking-ai-sessionizer/blob/v$VERSION/docs/en/guides/how-to-release.md

Notes for voters:
 * internal/view/conversation-view/ in the source package is the build output of Horizon's conversation renderer, from apache/skywalking-horizon-ui at the commit its HORIZON_COMMIT file names. It is Apache-2.0 code of the ASF with no third-party code in it. The source package builds and runs with it as it is. `make conversation-view-check`, which needs Node.js 24 and pnpm, rebuilds it from that commit and compares. In the unpacked source package it compares every file except the two fonts, which the source package does not carry, and it names the two it left out.
 * The two fonts the page draws with are under the SIL Open Font License, a Category B license, so they are in the binary packages only. A build from the source package draws the page with system fonts.

Voting will start now and will remain open for at least 72 hours. All PMC members are requested to give their votes.

[ ] +1 Release this package.
[ ] +0 No opinion.
[ ] -1 Do not release this package because....

Thanks.
```

Each checksum line is the content of the package's `.sha512` file. The fingerprint is the one
`candidate` checked in KEYS.

The vote stays open for at least 72 hours. Only the votes of PMC members are binding. Anyone else
is welcome to vote, and those votes count as non-binding. The vote passes with at least three
binding +1 votes, and more binding +1 votes than binding -1 votes.

### Check list for voters

Everyone voting should check these before a +1. Fetch the candidate and the keys first:

```sh
VERSION=<the version under vote>
svn export https://dist.apache.org/repos/dist/dev/skywalking/ai-sessionizer/$VERSION candidate
cd candidate
curl -fsSLO https://dist.apache.org/repos/dist/release/skywalking/KEYS
gpg --import KEYS
for f in *.tgz *.zip; do shasum -a 512 -c "$f.sha512" && gpg --verify "$f.asc" "$f"; done
```

1. The source package and one binary package for each platform are there, each with its `.asc`
   and `.sha512`. The platforms are the `PLATFORMS` list in the Makefile of the tag.
2. `shasum -a 512 -c` passes for each package.
3. `gpg --verify` passes for each package, with a key from KEYS. gpg prints `Good signature`,
   and nothing about an expired or revoked key: no `[expired]` after the name, no
   `Note: This key has expired!`, and no warning that the key or a subkey
   `has been revoked by its owner`. gpg 2.5.18 prints `Good signature` and exits 0 for an expired
   or revoked key too. The
   [ASF release signing guide](https://infra.apache.org/release-signing.html) counts a signature
   as valid only when gpg verifies it as a good signature and does not complain about expired or
   revoked keys. A warning that the key is not certified with a trusted signature only says your
   own keyring does not vouch for that key.
4. The source package is the tag. It unpacks into one directory, and holds exactly what
   `git archive` of the tag holds. `git archive` leaves out what the tag's `.gitattributes` marks
   `export-ignore`, such as the two fonts, so a plain clone of the tag has more files:

   ```sh
   tar -xzf apache-skywalking-ai-sessionizer-$VERSION-src.tgz
   git -c advice.detachedHead=false clone --quiet --depth 1 --branch v$VERSION https://github.com/apache/skywalking-ai-sessionizer.git tag
   mkdir from-tag && git -C tag archive HEAD | tar -xf - -C from-tag
   diff -r from-tag apache-skywalking-ai-sessionizer-$VERSION-src && echo "same as the tag"
   ```

   The clone warns that `refs/tags/v$VERSION` is not a commit, which is expected for an annotated
   tag.

   The tag is on the commit that finished the changelog. So the source package holds
   `docs/en/changes/changes.md`, the page the vote mail links, with `$VERSION` in its heading and
   without the in-development note:

   ```sh
   p=apache-skywalking-ai-sessionizer-$VERSION-src/docs/en/changes/changes.md
   [ "$(head -1 $p)" = "# Changes in $VERSION" ] && ! grep -q '^> In development' $p && echo "the changelog is final"
   ```

5. The source package carries no compiled file and no font file. All three commands print
   nothing:

   ```sh
   find apache-skywalking-ai-sessionizer-$VERSION-src -type f -exec file {} + | grep -E 'ELF|Mach-O|PE32|current ar archive|Java archive|compiled Java|WebAssembly|byte-compiled'
   find apache-skywalking-ai-sessionizer-$VERSION-src -type f | grep -E '\.(a|o|so|dylib|dll|exe|lib|obj|class|jar|war|pyc|pyo|wasm)$'
   find apache-skywalking-ai-sessionizer-$VERSION-src -type f | grep -E '\.(woff2?|ttf|otf|eot)$'
   ```

   The first finds compiled files by what `file` says they are: executables, libraries, object
   files, Java classes and archives, WebAssembly modules and Python byte code. The second finds
   them by name, because `file` cannot tell some of them. The `file` 5.41 that ships with macOS
   calls a Python `.pyc` file only `data`. These are the files `make release` refuses too.

   Fonts come under licenses such as the SIL Open Font License, which the ASF puts in Category B.
   A Category B work may be in a convenience binary, never in a source release.
6. `internal/view/conversation-view/` is not written by hand. It is the build output of Horizon's
   conversation renderer, from apache/skywalking-horizon-ui at the commit its `HORIZON_COMMIT` file
   names. It is Apache-2.0 code of the ASF. Horizon's package has development dependencies only,
   and none of them is bundled into it, so no third-party code is in it. Its JavaScript file
   carries no license header, and its CSS files carry Horizon's. With Node.js 24 and pnpm,
   `make conversation-view-check` rebuilds it from that commit and fails when it differs. In the
   unpacked source package it leaves the two fonts, `inter-latin-wght-normal.woff2` and
   `jetbrains-mono-latin-wght-normal.woff2`, out of the comparison, because the source package does
   not carry them, and it prints their names. It compares every other file. In a clone of the tag
   it compares the fonts too.
7. Every package carries `LICENSE` and `NOTICE`. A binary package also carries `licenses/`, and
   its `NOTICE` carries the notices of the bundled modules. In the unpacked source,
   `make license-check` checks the license header of every source file, and
   `make dep-licenses-check` says whether `dist-material/` is what the modules built into the two
   binaries resolve to. A module that `go.mod` requires only for the tests of a dependency, such as
   `github.com/kr/text`, is not in the binaries, so `dist-material/` does not list it.
8. The source package builds and passes its tests:

   ```sh
   cd apache-skywalking-ai-sessionizer-$VERSION-src
   make build VERSION=$VERSION
   make test
   ./bin/asz version
   ```

   Pass `VERSION`. The Makefile reads the version from git, and an unpacked source package has no
   git history, so without it `asz version` prints an empty version. The build downloads the Go
   modules `go.mod` names. It needs no Node.js, because the conversation renderer the page draws
   with is committed, built from a pinned Horizon commit. The page draws with system fonts, because
   the source package does not carry the renderer's fonts.
9. The binary package for your platform works on your machine. Run `tools/package-smoke.sh` from
   the source package on it, in the candidate directory where the packages are, not in the source
   directory item 8 moved into. Here the package is the one for Linux on x86-64. On Windows, run
   it in Git Bash and name the `.zip`:

   ```sh
   bash apache-skywalking-ai-sessionizer-$VERSION-src/tools/package-smoke.sh \
     apache-skywalking-ai-sessionizer-$VERSION-bin-linux-amd64.tgz $VERSION
   ```

   It checks the package's sha512, unpacks it the way a person would, and checks that every file
   is there. On macOS and Linux, `asz` and the plugin's binary must keep their executable bits.
   Then it runs both binaries from the unpacked files, and both must report `$VERSION`. Four
   scenarios from `tests/scenarios` in the source package pass through `asz scenario check`.
   `asz view` serves the list page and one conversation page. A shell command sent through the
   plugin's hooks is recorded as a change. The last line is `<package> works on this machine`. It
   took about ten seconds on macOS on Apple silicon.

   It needs bash and curl, and port 18787 on 127.0.0.1 must be free. When another program holds
   the port, the script stops and says so. Its requests to that port skip any proxy you have set.

   CI's `packages` job runs the same script on all six packages, each on a GitHub runner of its
   own platform, on every CI run. If you use Claude Code, the unpacked `asz` can also be tried on
   your own conversations: `asz sources`, `asz collect -once`, `asz parse`, `asz verify`.

### When the vote fails

The script has no stage for a failed vote. Reply on the vote thread to say what was found. What
comes next depends on where the problem is.

- **In the packages, not in the tagged source.** For example, a signature by the wrong key, or a
  file missing from the upload. Remove the candidate with the `svn rm` command from step 4 of
  [Candidate](#2-candidate), remove `dist/$VERSION/`, run `candidate` again, and call a new vote.
  It builds from the same tag.
- **In the source.** The tag has to change, and the script does not change a tag. `prepare`
  refuses a version whose tag exists. Fix the problem on `main`, and agree on the dev list how to
  go on: a new tag for the same version, or the next version. Either way, remove the old candidate
  with `svn rm`.

## 4. Vote result

After the vote has been open for at least 72 hours, and has passed:

```sh
tools/release.sh vote-result $VERSION \
  --binding "Name, Name, Name" \
  --non-binding "Name, Name" \
  --against "Name" \
  --non-binding-against "Name" \
  --abstain "Name" \
  --thread "https://lists.apache.org/thread/<id>"
```

`--binding` names the PMC members who voted +1, and `--non-binding` everyone else who voted +1.
`--against` names the PMC members who voted -1, and `--non-binding-against` everyone else who voted
-1. `--abstain` names everyone who voted +0. Names are separated by commas. Each of these options
can also be written as `--binding=...`, and can be given more than once. `--thread` is the link of
the vote thread on lists.apache.org, and the mail carries it. Without it the mail has no link, and
the stage says so.

It refuses when:

- a name is listed twice, in one list or across them. Names are compared without case, and a run
  of spaces inside a name counts as one space.
- an option has no value, such as an empty `--binding=`, or `--binding` followed by another
  option.
- there are fewer than three binding +1 votes.
- the binding +1 votes are not more than the binding -1 votes.

Every vote is listed in the mail, so a non-binding -1 that reports a real problem stays on record.
Only binding votes decide the result. It trusts that the vote was open for 72 hours. It writes the
result mail to `dist/$VERSION/result.txt` and prints it. `--dry-run` prints the mail and writes
nothing. It runs no svn command.

Send the mail to `dev@skywalking.apache.org`. By hand, it is:

```text
Subject: [RESULT][VOTE] Release Apache SkyWalking AI Sessionizer version $VERSION

72 hours passed, we have got 4 +1 bindings, 2 +1 non-bindings, 1 -1 binding, 1 -1 non-binding and 1 +0:

The vote thread: https://lists.apache.org/thread/<id>

+1 bindings:
<name>
<name>
<name>
<name>

+1 non-bindings:
<name>
<name>

-1 bindings:
<name>

-1 non-bindings:
<name>

+0:
<name>

The binding votes, those of PMC members, decide the result: 4 +1 and 1 -1, so the vote passed.

Thank you for voting, I will continue the release process.
```

A list with no names is left out, and so is its count in the first line. The thread line is left
out without `--thread`.

## 5. Publish

A PMC member runs this stage, because only PMC members can write to the release directory. It
need not be the release manager. On a machine without the candidate, `publish` fetches the voted
files from dist.apache.org.

```sh
tools/release.sh publish $VERSION
```

It does these steps in order.

1. **Check** for git, svn, shasum, tar and awk, and that `tools/install-manifests.sh` exists,
   before anything moves. The script needs tar and awk, and it runs after the move. Then check the
   tag, as `candidate` does.
2. **Find the candidate:**

   ```sh
   svn ls https://dist.apache.org/repos/dist/dev/skywalking
   svn ls https://dist.apache.org/repos/dist/release/skywalking
   svn ls https://dist.apache.org/repos/dist/dev/skywalking/ai-sessionizer/$VERSION
   svn ls https://dist.apache.org/repos/dist/release/skywalking/ai-sessionizer
   ```

   It refuses when the candidate is not in the dev area, and when `$VERSION` is in both places.
   When `$VERSION` is only in the release directory, an earlier run moved it. This run skips the
   move and does the steps after it again. Either way, every package with its `.asc` and `.sha512`
   must be there. `--remove-old` is refused unless the move is done already, as
   [Later: remove old versions](#later-remove-old-versions) says.
3. **Hold the local files to the voted ones.** The GitHub release and the install manifests are
   made from `dist/$VERSION/`, so each file there must be the one voted on. A package built again
   has other bytes, and a package signed again has another signature. For each package already in
   `dist/$VERSION/` with its `.asc` and `.sha512`, it reads the voted checksum and signature:

   ```sh
   svn cat <candidate directory>/<package>.sha512
   svn cat <candidate directory>/<package>.asc
   ```

   It refuses when the voted `.sha512` differs from the local `.sha512` file or from the package's
   own sha512, and when the voted `.asc` differs from the local `.asc` file. Remove
   `dist/$VERSION` and run again to fetch the voted files. A package missing locally is fetched
   before the move, with its `.asc` and `.sha512`, and checked with `shasum -a 512 -c`:

   ```sh
   svn export --force <candidate directory>/<file> dist/$VERSION/<file>
   ```

4. **Move the candidate to the release directory.** The move publishes the voted packages. With
   the vote and the announcement, it makes the release:

   ```sh
   # only on the first release, while the directory does not exist
   svn mkdir -m "Create the Apache SkyWalking AI Sessionizer release directory" \
     https://dist.apache.org/repos/dist/release/skywalking/ai-sessionizer
   svn mv -m "Release Apache SkyWalking AI Sessionizer $VERSION" \
     https://dist.apache.org/repos/dist/dev/skywalking/ai-sessionizer/$VERSION \
     https://dist.apache.org/repos/dist/release/skywalking/ai-sessionizer/$VERSION
   ```

   Then it lists the release directory, and checks that every file arrived. Only in a later run
   with `--remove-old`, it removes the older versions next, as
   [Later: remove old versions](#later-remove-old-versions) describes. Otherwise it names the
   older versions it kept.
5. **Write the files for the later stages** into `dist/$VERSION/`:
   - `announce.txt`, the announcement mail.
   - `website.txt`, the entries for the website. They carry the day of the move, as svn records
     it in UTC, so a later run of `publish` writes the same date.
   - `install/`, the install manifests. It removes the old `install/` first, then runs
     `tools/install-manifests.sh $VERSION dist/$VERSION dist/$VERSION/install`. If that script
     fails, the move is still done. Fix the script and run `publish` again: it skips the move and
     writes `install/` from scratch. To run the script by hand instead, remove
     `dist/$VERSION/install` first, because the script refuses a directory that is not empty.
6. **Print the next steps**, which are the stages below, in order. They wait at least one hour
   after the move before the website change and the announcement, as the
   [ASF release policy](https://www.apache.org/legal/release-policy.html) asks. The step for the
   install manifests says that the Homebrew formula and the winget files wait for `complete`,
   because both download from the GitHub release. When older versions are in the release
   directory, the last step is the later run with `--remove-old`.

`--dry-run` reads svn and prints the plan. It fetches no package, and moves, removes and writes
nothing. Like a real run, it fetches a missing tag, as `candidate` does.

## 6. Complete

```sh
tools/release.sh complete $VERSION
```

downloads.apache.org serves the release directory a short while after the move. How long that
takes has not been measured. Until then, `complete` refuses, so run it again later.

1. It checks the tag, and refuses when the GitHub release exists already.
2. For every package, `dist/$VERSION/` must hold it with its `.asc` and `.sha512`. Each must match
   what `https://downloads.apache.org/skywalking/ai-sessionizer/$VERSION/` serves: the `.sha512`
   file, the package's own sha512, and the `.asc`. Every file is checked before anything is
   created, because creating the GitHub release starts CI's image job.
3. It builds the text of the GitHub release and prints it. The text is
   `docs/en/changes/changes.md` as the tag holds it, without its heading, because the release has
   its own title. The section "Where to get it", shown below, follows it. The text is
   built from the tag each time and is not stored in the repository, so a change made on `main`
   after `prepare` does not reach it. `--dry-run` stops after printing it.
4. It creates the GitHub release with that text: tag `v$VERSION`, title `$VERSION`, not a draft,
   not a prerelease. By hand:

   ```sh
   git show v$VERSION:docs/en/changes/changes.md | tail -n +2 > notes.md
   # Add the section "Where to get it" below to the end of notes.md.
   gh release create v$VERSION --verify-tag --title $VERSION --notes-file notes.md
   ```

5. It uploads the voted packages, each with its `.asc` and `.sha512`. That is 21 files for six
   platforms. It never replaces a file already on the release:

   ```sh
   gh release upload v$VERSION dist/$VERSION/apache-skywalking-ai-sessionizer-$VERSION-*
   ```

   When the upload stops part way, run that command again with `--clobber`.
6. It says that the Homebrew formula and the winget manifests can be submitted now, once the PMC
   has agreed to each channel, because both download from this GitHub release.

The section "Where to get it" sends a reader to the Apache release and to the signatures, as the
[ASF release policy](https://www.apache.org/legal/release-policy.html) asks. It also links the
documentation and the changelog of the tag:

```markdown
#### Where to get it

- The Apache release of $VERSION is the source package. The binary packages for macOS, Linux and Windows are conveniences built from it. The [SkyWalking downloads page](https://skywalking.apache.org/downloads/) links each package with its signature and checksum.
- The files attached to this GitHub release are the same signed packages, each with its `.asc` signature and `.sha512` checksum. Verify them against https://downloads.apache.org/skywalking/KEYS, as [Install](https://github.com/apache/skywalking-ai-sessionizer/blob/v$VERSION/docs/en/setup/install.md#verify-a-package) describes.
- To build from the source package, see [Install](https://github.com/apache/skywalking-ai-sessionizer/blob/v$VERSION/docs/en/setup/install.md#build-from-the-source-package).
- Documentation: https://github.com/apache/skywalking-ai-sessionizer/blob/v$VERSION/docs/README.md
- Full changelog: https://github.com/apache/skywalking-ai-sessionizer/blob/v$VERSION/docs/en/changes/changes.md
```

The GitHub release is a convenience, a page on GitHub carrying the same bytes as the release
directory. CI never attaches the packages it builds, because they are not the voted, signed files.
Its `binaries` job still builds every platform on every run, so a broken cross-compile shows at
once, and keeps the packages only as a workflow artifact. Its `packages` job then runs each of
those packages with `tools/package-smoke.sh`, on a runner of the package's own platform.

The released event starts CI's `docker` job. Today that job publishes the container image to the
GitHub container registry under `$VERSION`, and under `latest` when it is the highest version tag.
The image is a convenience, and how it is published for a release is still pending. Nothing in the
release waits for it. If the job fails, start the CI workflow by hand with the tag as its input.

## 7. The website

Wait at least one hour after the move, as the
[ASF release policy](https://www.apache.org/legal/release-policy.html) asks before a download page
changes. Then open a pull request on
[apache/skywalking-website](https://github.com/apache/skywalking-website) with the entries in
`dist/$VERSION/website.txt`:

- **`data/releases.yml`**, the downloads page. The first release adds the whole entry to the list
  under `- type: Foundations`, after Grafana Plugins, which is where `data/docs.yml` lists AI
  Sessionizer. A later release puts its own items first under `source` and under `distribution`.
  It also points the links of versions no longer in the release directory at
  `https://archive.apache.org/dist/skywalking/ai-sessionizer/`.
- **`data/docs.yml`**, the documentation. The AI Sessionizer entry there lists Next only. Put the
  `Latest` and `v$VERSION` items right after Next. A later release sets the `commitId` of Latest
  to its own commit, and puts its own item right after Latest.

By hand, the entry in `data/releases.yml` is:

```yaml
    - name: SkyWalking AI Sessionizer
      icon: skywalking
      description: Conversation-level observability, measurement and export for long-lived AI agents.
      source:
        - version: v$VERSION
          date: Sep. 11th, 2026
          downloadLink:
            - name: src
              link: https://www.apache.org/dyn/closer.lua/skywalking/ai-sessionizer/$VERSION/apache-skywalking-ai-sessionizer-$VERSION-src.tgz
            - name: asc
              link: https://downloads.apache.org/skywalking/ai-sessionizer/$VERSION/apache-skywalking-ai-sessionizer-$VERSION-src.tgz.asc
            - name: sha512
              link: https://downloads.apache.org/skywalking/ai-sessionizer/$VERSION/apache-skywalking-ai-sessionizer-$VERSION-src.tgz.sha512
      distribution:
        - version: v$VERSION
          date: Sep. 11th, 2026
          downloadLink:
            - name: MacOS ARM64
              link: https://www.apache.org/dyn/closer.lua/skywalking/ai-sessionizer/$VERSION/apache-skywalking-ai-sessionizer-$VERSION-bin-darwin-arm64.tgz
            - name: asc
              link: https://downloads.apache.org/skywalking/ai-sessionizer/$VERSION/apache-skywalking-ai-sessionizer-$VERSION-bin-darwin-arm64.tgz.asc
            - name: sha512
              link: https://downloads.apache.org/skywalking/ai-sessionizer/$VERSION/apache-skywalking-ai-sessionizer-$VERSION-bin-darwin-arm64.tgz.sha512
            - name: "|"
            # The same three items for each other platform, in the order of PLATFORMS, with an
            # item named "|" between two platforms: MacOS AMD64, Linux AMD64, Linux ARM64,
            # Windows AMD64 and Windows ARM64.
```

The items in `data/docs.yml` are:

```yaml
        - version: Latest
          link: /docs/skywalking-ai-sessionizer/latest/readme/
          commitId: <commit hash of v$VERSION>
        - version: v$VERSION
          link: /docs/skywalking-ai-sessionizer/v$VERSION/readme/
          commitId: <commit hash of v$VERSION>
```

The date is the day of the move, as svn records it in UTC, in the form the website uses. A later
run of `publish` writes the same date. The packages are linked through closer.lua, which the
[ASF rules for download pages](https://infra.apache.org/release-download-pages.html) require.

The website also posts news of each SkyWalking release under `content/events/`, and the script
does not write that post. `content/events/release-apache-skywalking-mcp-0-2-0/index.md` in the
website repository shows the shape: a title, a date, a link to the downloads page, what changed,
and the release packages. It can go in the same pull request.

Wait until the website is deployed and its downloads page lists the version before the
announcement. The mail links the downloads page and the documentation.

## 8. The announcement

At least one hour after the move, as the
[ASF release policy](https://www.apache.org/legal/release-policy.html) asks, and once the
downloads page lists the version, send `dist/$VERSION/announce.txt` to `dev@skywalking.apache.org`
and `announce@apache.org`, as plain text, from your apache.org address. The policy recommends an
OpenPGP signature on the announcement, so sign it with your release key if your mail program can.
By hand, the mail is:

```text
Subject: [ANNOUNCE] Apache SkyWalking AI Sessionizer $VERSION released

Hi the SkyWalking Community,

On behalf of the SkyWalking Team, I am glad to announce that Apache SkyWalking AI Sessionizer $VERSION is now released.

SkyWalking AI Sessionizer: conversation-level observability for long-lived AI agents. It assembles fragmented agent telemetry into one durable conversation structure.

SkyWalking: APM (application performance monitor) tool for distributed systems, especially designed for microservices, cloud native and container-based architectures.

Download Links: https://skywalking.apache.org/downloads/
Release Notes: https://github.com/apache/skywalking-ai-sessionizer/blob/v$VERSION/docs/en/changes/changes.md
Website: https://skywalking.apache.org/
Documents: https://skywalking.apache.org/docs/skywalking-ai-sessionizer/v$VERSION/readme/

Resources:
- Issue: https://github.com/apache/skywalking/issues
- Mailing list: dev@skywalking.apache.org

The Apache SkyWalking Team
```

## 9. Install manifests

**Before the first submission to each package manager, the PMC agrees to it on
dev@skywalking.apache.org.** A manifest distributes the project under the ASF's name in a new
place: the winget manifests name The Apache Software Foundation as their publisher. The ASF allows
other distribution platforms only for binaries that follow its release, licensing, branding and
trademark policies. A Homebrew tap or a Scoop bucket under github.com/apache is a new repository,
which the PMC asks INFRA to create.

`dist/$VERSION/install/` holds the manifests that let package managers install the version. They
are conveniences.

| File | Where it goes | What it downloads |
| --- | --- | --- |
| `homebrew/skywalking-ai-sessionizer.rb` | a Homebrew tap | the macOS or Linux binary package, from the GitHub release, with archive.apache.org as its mirror |
| `scoop/skywalking-ai-sessionizer.json` | a Scoop bucket | the Windows packages, from dlcdn.apache.org |
| `winget/manifests/a/Apache/SkyWalkingAISessionizer/$VERSION/` | microsoft/winget-pkgs | the Windows packages, from the GitHub release |

The Homebrew formula installs the voted binary package for the machine: macOS or Linux, on ARM 64
or x86-64. It names a URL, a mirror and a sha256 for each of the four packages. It builds nothing,
so it needs no Go. It installs `asz`, the Claude Code plugin under `libexec/claude-code-plugin`,
and `LICENSE`, `NOTICE` and `licenses/`, and its test runs both binaries. It goes to a tap only.
Homebrew/homebrew-core takes a formula only when it builds from source or installs output that is
the same on every platform, and this formula installs a binary built for each platform.

Submit each file only after the version is on the download site. The Homebrew formula and the
winget files also wait for [Complete](#6-complete), because they download from the GitHub release.
That URL keeps working after a newer version replaces this one on the download site.
archive.apache.org keeps every version too, but it slows down and then bans heavy use, and winget
runs in scripts and on shared CI machines. The Homebrew formula names the archive as its mirror,
which Homebrew tries only when the GitHub URL fails. The archive can show a version later than the
download site does. A manifest that names a file that is not there yet fails its review.
`dist/$VERSION/install/README.md` checks every URL against its expected hash, and says how to test
and submit each file.

A Scoop bucket holds only its newest manifest, and that manifest names the version on the download
site. Move the bucket to a new version before the previous version leaves the release directory.
The winget manifests of an old version keep working, because its GitHub release stays. So does the
Homebrew formula of an old version, which a tap keeps in its git history.

To write the manifests again, give a new or empty directory:

```sh
tools/install-manifests.sh $VERSION dist/$VERSION <new or empty directory>
```

It reads the six voted binary packages in `dist/$VERSION/` and downloads nothing. It does not read
the source package. It refuses a package that has no `.sha512` beside it, or that does not match
that `.sha512`, so the manifests describe the voted bytes. It also refuses a macOS or Linux package
that lacks a file the formula installs, or that holds `asz` or the plugin's binary without its
executable bit, because such a formula would fail for everyone on that platform.

[Install](../setup/install.md) marks each package manager as not published yet. When a manifest is
accepted for the first time, update that page with the command that installs from it.

## Later: remove old versions

The release directory should hold only the versions users should choose, and archive.apache.org
keeps every version removed from it. A PMC member removes the older versions in a later run of
`publish`:

```sh
tools/release.sh publish $VERSION --remove-old
```

`publish` refuses `--remove-old` in the run that moves the candidate. Run it only after both of
these:

1. The website pull request that points the links of the older versions at
   `https://archive.apache.org/dist/skywalking/ai-sessionizer/` has merged. Until then, the
   downloads page links the files that would be removed.
2. The Scoop bucket names the new version. Its manifest downloads the version it names from the
   download site, and that URL stops working when the version is removed.

The move is not repeated, and the steps after it run again. For each version older than
`$VERSION`, it runs:

```sh
svn rm -m "Remove Apache SkyWalking AI Sessionizer $OLD, superseded by $VERSION" \
  https://dist.apache.org/repos/dist/release/skywalking/ai-sessionizer/$OLD
```

A newer version stays. The first release has nothing older to remove.
