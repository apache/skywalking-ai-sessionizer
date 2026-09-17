# How to Release

This guide is for the release manager, and for anyone checking a release candidate before voting.
Apache SkyWalking AI Sessionizer follows the Apache release process. The SkyWalking PMC votes on a
candidate, and only after the vote passes is anything published as an official release.

A release takes three commands of `tools/release.sh`: `prepare`, `candidate` and `publish`. The
vote comes between the last two. None of them pushes to `main`. For every command this page shows
what it does, and lists the svn commands it runs, so a release manager can finish a step by hand
when the script stops.

## What counts as a release

An official Apache release has the PMC's approval. Its signed packages are published through
Apache's distribution channel. The website and announcement then direct users to that release.
A GitHub release alone cannot provide that approval. See the
[ASF release policy](https://www.apache.org/legal/release-policy.html).

- **The source package** is what the PMC votes on. It is the Apache release.
- **The binary packages** are the convenience binaries of the vote. They are built from the tagged
  source, and signed and checksummed like the source package. They sit beside it in the candidate
  directory, and voters check them too.
- **A git tag** is a candidate. `prepare` pushes it before the vote, so a tag alone says nothing
  about whether a version was released.
- **The GitHub prerelease** is created by CI on the tag push, with the binaries CI built and
  tested. It is marked as a development candidate, not an official Apache release. `candidate`
  attaches the source package and every signature to it, so it holds the same files as the
  candidate directory. After the vote and the move to the release directory, `publish` promotes it
  to a full GitHub release.
- **The container image** is a convenience. The promotion of the GitHub release starts CI's image
  job. A tag push and a default manual CI run publish no image.
- **The Homebrew, Scoop and winget manifests** are conveniences, submitted after the release.

0.3.0 is the first version to go through this process. 0.1.0 and 0.2.0 were published on GitHub
before it, as full GitHub releases carrying binary packages that CI built. No vote was held for
them, their packages are not signed, and they are not Apache releases. Whether to mark those two
GitHub releases as pre-releases, or to remove their packages, is for the PMC to decide.

## The steps

| Step | Run by | What it leaves |
| --- | --- | --- |
| [1. Prepare](#1-prepare) | the release manager | the tag `v$VERSION` and a pull request. CI on the tag push then creates the GitHub prerelease with the binaries |
| [2. Candidate](#2-candidate) | the release manager | the signed candidate in the dev area of dist.apache.org, its source package and signatures on the GitHub prerelease, and the vote mail |
| [The vote](#the-vote) | the PMC, then the release manager | the votes on the dev list, over at least 72 hours, and the result mail |
| [3. Publish](#3-publish) | a PMC member | the voted packages in the release directory, the promoted GitHub release, and the files the later steps use |
| [The website](#the-website) | the release manager | the downloads entry and the documentation of the version |
| [The announcement](#the-announcement) | the release manager | the mail to the dev and announce lists |
| [Install manifests](#install-manifests) | the release manager, once the PMC agrees to each channel | the Homebrew, Scoop and winget manifests, each submitted |
| [Later: remove old versions](#later-remove-old-versions) | a PMC member | the release directory without the versions the new one replaces |

Between step 1 and step 2, the release manager waits for CI by hand. Nothing in the script waits.
What the three commands share:

- Each takes the version as its first argument, or asks for it. `prepare` offers the version the
  heading of `docs/en/changes/changes.md` names. `candidate` and `publish` offer the newest version
  `prepare` has finished, read from the newest `docs/en/changes/changes-X.Y.Z.md` in the checkout.
  `prepare` makes that page in the commit after the tag, and `main` holds it once the prepare pull
  request has merged.
- `candidate` and `publish` write their files under `dist/$VERSION/` in the checkout. `publish`
  fetches the voted files from dist.apache.org when they are not there. `prepare` writes nothing
  there. Git ignores `dist/`.
- `--dry-run` prints what the command would do. It makes no commit and no push, changes nothing on
  dist.apache.org or GitHub, and writes no file in `dist/`. `candidate` and `publish` read the tag
  to make the plan. So, like a real run, they fetch `v$VERSION` from origin into the local
  repository when it does not have the tag. They fetch that one tag and no other.
- An option that belongs to another command is refused, never ignored.
- When `APACHE_ID` is set, every svn command runs as `svn --username "$APACHE_ID"`. Set it when
  your local user name is not your Apache ID.
- `tools/release.sh --help` prints every command and its options.

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

   `candidate` refuses to sign with a key that is not in KEYS, because every voter checks the
   signatures against that file. It also refuses a key that has expired or is revoked in KEYS, a
   key that is not RSA of at least 2048 bits, and a key that has no user ID with an apache.org
   address. gpg reads the expiry of a key from KEYS, not from your machine. So when you extend
   your key, have a PMC member commit the renewed public key to KEYS before the next candidate.
2. **The tools.** git, Go 1.27 or later, make, Python 3, gpg, shasum, tar, gzip, zip, unzip, file, curl, svn, and
   gh logged in to an account that can write to the repository. `prepare`, `candidate` and
   `publish` check for the tools they use before they change anything, and name any that is
   missing. `prepare` needs gh only when it pushes.
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
  `publish` builds the text of the GitHub release from it.
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
   Refuses a dirty tree, an existing tag, branch or GitHub release, and a next version that does not come after
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
   open $NEXT" against `main`. It creates no GitHub release. A GitHub release of `v$VERSION` that
   exists already belongs to an earlier candidate, and `prepare` refuses to go on, as it does when
   gh cannot check.

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

The push of the tag starts CI. It builds and tests everything: the suite on three systems, the
source package, all six binary packages, and each package on a runner of its own platform. Only
when every job has passed does its `Prerelease binaries` job create the GitHub prerelease titled
`$VERSION`, not marked latest, and attach the six archives and their six `.sha512` files. The
prerelease is created with the workflow token, so it starts no other CI run, and no image is
published. The job downloads what it uploaded, compares every byte, and adds a readiness marker to
the prerelease notes. The marker names the run, its attempt, the commit, and a SHA-256 fingerprint
of the verified files: each file's name, asset ID, size and GitHub digest.

Wait for that run by hand, and review and merge the pull request. Both are needed before step 2.
If a job of the run fails for a reason outside the source, such as a runner problem, rerun the
failed jobs. The upload job reuses an empty prerelease an earlier attempt created, and refuses one
that holds any file. The scripts never delete a prerelease or replace its files. Remove a
prerelease that is incomplete or rejected explicitly before preparing its replacement, as
[When the vote fails](#when-the-vote-fails) describes.

## 2. Candidate

Once CI has created the prerelease with the binaries, and the prepare pull request has merged:

```sh
GPG_USER=<key id, fingerprint or email> tools/release.sh candidate $VERSION
```

`make release VERSION=$VERSION` runs the same command with `--no-upload`, writing the signed
packages under `dist/$VERSION/`. `CI_RUN=<id>` selects a run for that make target.

The binary archives uploaded to SVN always come from the GitHub prerelease. `candidate` reads its
readiness marker and verifies the successful uploader run for the exact release tag and commit.
The run must be the push of the tag. Every binary archive and `.sha512` file must be uploaded by
`github-actions[bot]` while that run ran, and they must match the marker's fingerprint. Files a
person uploads, or uploads again, are refused. A manual run of the workflow never attaches files,
so it cannot make a prerelease ready. To require a specific uploader run:

```sh
tools/release.sh candidate $VERSION --ci-run <run id>
```

`GPG_USER` picks the signing key. When it is empty, gpg's default key signs. If gpg fails without
asking for the passphrase, run `export GPG_TTY=$(tty)` first.

The command takes these steps and stops at the first failure:

1. **Check the tools and tag.** It needs git, gh, Python 3, gpg, shasum, tar, gzip, unzip, file,
   curl and sh, plus svn when uploading. The tag must exist on origin, agree with any local tag,
   and hold the finished changelog at `docs/en/changes/changes.md`. A missing local tag is
   fetched. The packages are the source package and the platforms in the tag's Makefile.
   `git archive` must contain no fonts; `.gitattributes` excludes the two OFL fonts and local
   metadata files such as `._*`, `.DS_Store` and `__MACOSX`.
2. **Check the candidate directory.** It lists the dev and release roots, then the project's
   version directories when they exist:

   ```sh
   svn ls https://dist.apache.org/repos/dist/dev/skywalking
   svn ls https://dist.apache.org/repos/dist/release/skywalking
   svn ls https://dist.apache.org/repos/dist/dev/skywalking/ai-sessionizer
   svn ls https://dist.apache.org/repos/dist/release/skywalking/ai-sessionizer
   ```

   It refuses an already released version. A candidate of `$VERSION` in the dev area is refused
   too, unless `dist/$VERSION/` in this checkout holds the same files: every package with the same
   `.asc` and `.sha512`, and a package that matches its `.sha512`. That is a run that uploaded and
   then stopped before the GitHub prerelease held the candidate. Signing the same archive again
   gives another signature, and the vote is about the uploaded one, so such a run signs and
   uploads nothing again. It checks the uploaded signatures against KEYS, checks that the uploaded
   binaries are the ones CI attached, attaches what the prerelease is missing, and writes the vote
   mail. To replace a withdrawn candidate, first remove it, then call a new vote on the
   replacement:

   ```sh
   svn rm -m "Remove the Apache SkyWalking AI Sessionizer $VERSION candidate for a new one" \
     https://dist.apache.org/repos/dist/dev/skywalking/ai-sessionizer/$VERSION
   ```

3. **Download and verify prerelease binaries.** `tools/ci-binaries.sh` requires the expected
   public prerelease and its readiness marker. The marker must identify a completed, successful
   run and attempt of this repository's CI workflow, from the Apache repository, for the push of
   `v$VERSION` at the release commit. A fork, a branch run, a manual run, a failed run or a wrong
   commit is refused. The prerelease must hold every expected archive and its `.sha512` file, and
   nothing else except the source package and signatures an earlier run of `candidate` attached.
   It downloads each of CI's files by its asset ID, checks GitHub's SHA-256 digest and verifies
   every package's SHA-512 checksum.
   `tools/package-check.sh` also rejects AppleDouble `._*` files, `.DS_Store` and `__MACOSX`
   entries inside each package. The helper rechecks the tag, release and CI run after downloading.
   It does not depend on Actions artifact retention. There is no local binary build fallback.
   If the prerelease is not ready, the command stops before signing.
4. **Check the signing key.** It signs a scratch file with the key, and reads from gpg's status
   output the key that signed and its primary key. It downloads KEYS from
   `https://dist.apache.org/repos/dist/release/skywalking/KEYS`, imports it into an empty scratch
   keyring, and refuses when:
   - the primary key is not there. A voter checks every signature against KEYS, so a key missing
     there is found before the packages are signed, not during the vote.
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
5. **Create the source package and copy the CI packages.** Only the source archive is made
   locally, directly from the immutable release commit:

   ```sh
   git archive --format=tar --prefix=apache-skywalking-ai-sessionizer-$VERSION-src/ <release commit> \
     | gzip -n > dist/$VERSION/apache-skywalking-ai-sessionizer-$VERSION-src.tgz
   ```

   This archives committed files, subject to the tag's `.gitattributes`, without filesystem
   metadata from the release manager's checkout. The verified CI binary archives and checksums
   are copied unchanged into `dist/$VERSION/`. They are never unpacked and repacked for release.
   `ci-provenance.txt` records the prerelease, CI run, asset digests and commit for the vote mail. It stays
   local; only the release packages, their signatures and checksums go to SVN.
6. **Verify before signing.** All expected archives must exist and pass the metadata checker and
   their SHA-512 checksums. The source archive must have one top directory with `LICENSE` and
   `NOTICE`, no font file and no compiled file named by the tag's Makefile. Each binary package
   must contain both binaries, `LICENSE`, `NOTICE` and `licenses/`. The command then signs every verified archive and verifies each signature
   against the SkyWalking KEYS keyring. The CI binary checksums remain unchanged.
7. **Run the package for this machine again.** The command unpacks the source archive in a
   temporary directory and runs its `tools/package-smoke.sh` on the signed CI package for the
   release manager's platform. The script and scenarios therefore come from the candidate
   source. A failed check stops the upload. A host outside the release platforms is reported and
   skips this additional local check; CI has already run all six packages.
8. **Upload the exact signed files.** `--no-upload` skips this step. By hand:

   ```sh
   svn checkout --depth empty https://dist.apache.org/repos/dist/dev/skywalking dev
   cd dev
   svn update --set-depth immediates ai-sessionizer   # leave out on the first release
   mkdir -p ai-sessionizer/$VERSION
   cp <checkout>/dist/$VERSION/apache-skywalking-ai-sessionizer-$VERSION-* ai-sessionizer/$VERSION/
   svn add ai-sessionizer/$VERSION                    # svn add ai-sessionizer on the first release
   svn commit -m "Add the Apache SkyWalking AI Sessionizer $VERSION release candidate"
   ```

9. **Attach to the GitHub prerelease.** `--no-upload` skips this step. It attaches the source
   package, its `.sha512` and every `.asc`, beside CI's files, which it never uploads. A file that
   is there already must have the same bytes, and a conflicting one is reported for a person to
   remove; nothing is replaced. It then checks that the prerelease holds exactly the voted files,
   downloads them all and compares every byte. If the upload stops, the candidate is on SVN
   already. Run `candidate` again from the same checkout, as step 2 describes. By hand:

   ```sh
   cd <checkout>/dist/$VERSION
   gh release upload v$VERSION --repo apache/skywalking-ai-sessionizer \
     apache-skywalking-ai-sessionizer-$VERSION-src.tgz apache-skywalking-ai-sessionizer-$VERSION-src.tgz.sha512 *.asc
   ```

10. **Write the vote mail** to `dist/$VERSION/vote.txt`, including the checksums, signing key,
    CI run and asset provenance. The vote approves these exact bytes. `publish` moves them within
    SVN, and checks the GitHub release against them before promoting it.

`--no-upload` downloads and verifies the CI binaries, creates the source archive, signs and tests
the packages, and writes `vote-preview.txt`. It uploads nothing. It refuses when `vote.txt`
shows that a candidate was uploaded from this checkout, or when SVN shows the version in the
dev or release directory. A repeated run must never silently replace the files being voted on.

`--dry-run` creates no package, downloads no prerelease asset, runs no package and uploads nothing.
It fetches a missing tag, checks SVN and the signing key, and prints the CI selection it would
make. The signing check uses a scratch file and keyring, so gpg may ask for the passphrase. It
writes nothing into `dist/$VERSION/`. The plan does not claim that prerelease assets were verified.

### What the packages contain

- The source package is `apache-skywalking-ai-sessionizer-$VERSION-src.tgz`. It contains the
  tagged source under `apache-skywalking-ai-sessionizer-$VERSION-src/`, with the fonts and local
  metadata excluded by `.gitattributes`. A build from it uses system fonts.
- Each binary package is `apache-skywalking-ai-sessionizer-$VERSION-bin-<os>-<arch>.tgz`, or
  `.zip` for Windows. It contains `asz`, `asz-claude-plugin`, which is the binary of the Claude
  Code plugin, and the binary distribution's `LICENSE`, `NOTICE` and `licenses/`. Both binaries
  end in `.exe` on Windows. These packages include the renderer's two OFL fonts and their license
  texts.
- Every archive has a `.sha512` checksum and an ASCII-armored detached `.asc` signature.

No package carries the Claude Code plugin's manifest and hooks. Users install them from the
marketplace at the release tag: `.claude-plugin/marketplace.json` and `plugins/claude-code/plugin/`
in the tagged source, which is what the source package holds. The install instructions name the
tag, so the plugin a user installs is the one the vote approved, once the version is released.

The tag's `PLATFORMS` names macOS, Linux and Windows, each on x86-64 and ARM 64. CI cross-compiles
without cgo on Linux with its configured Go toolchain and packages with GNU tar, gzip and zip.
It uses `GOWORK=off` and `GOFLAGS=-mod=readonly`. Both binaries report `$VERSION` and the Go
version used to build them. The run must pass the whole CI workflow, including package smoke
tests on all six platforms, and on the same six the Claude Code check, which installs each package
and the plugin as the install pages say and runs a session the plugin must record. Only then are
its archives eligible for the candidate.

`make binaries` remains useful for local build checks. Those archives are not the release
candidate. GNU and macOS archive tools can produce different bytes even from the same files, so
`candidate` always uses the unchanged CI archives. Neither signing nor promotion repacks them.

## The vote

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
 * The same files, on the GitHub prerelease: https://github.com/apache/skywalking-ai-sessionizer/releases/tag/v$VERSION
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
 * The binary archives are the unchanged packages CI attached to the GitHub prerelease after all its checks passed on the tag push, verified below. Only the source archive was created locally. The release manager signed every archive after checking its contents and checksums, and attached the source archive and every signature to the same prerelease.
 * CI run: https://github.com/apache/skywalking-ai-sessionizer/actions/runs/<run id>
 * Prerelease: https://github.com/apache/skywalking-ai-sessionizer/releases/tag/v$VERSION, commit <release commit>
 * Asset: <asset id>, sha256:<asset digest>, <package name>
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
binding +1 votes, and more binding +1 votes than binding -1 votes. No command counts the votes.
[The result](#the-result) shows the mail that closes the vote.

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

### The result

After the vote has been open for at least 72 hours, count it by hand. Each voter counts once. List
every vote in the mail, so a non-binding -1 that reports a real problem stays on record. Only
binding votes decide the result. Send the result to `dev@skywalking.apache.org`:

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

Leave out a list with no names, and its count in the first line.

### When the vote fails

The script has no step for a failed vote. Reply on the vote thread to say what was found. What
comes next depends on where the problem is.

- **In the packages, not in the tagged source.** For example, a signature by the wrong key, or a
  file missing from the upload. Remove the candidate with the `svn rm` command from step 2 of
  [Candidate](#2-candidate), remove the source package and signatures it attached to the GitHub
  prerelease, remove `dist/$VERSION/`, run `candidate` again, and call a new vote. It takes the same
  verified CI binaries and recreates the source archive from the same tag:

  ```sh
  for f in apache-skywalking-ai-sessionizer-$VERSION-src.tgz apache-skywalking-ai-sessionizer-$VERSION-src.tgz.sha512 \
      $(gh release view v$VERSION --repo apache/skywalking-ai-sessionizer --json assets --jq '.assets[].name | select(endswith(".asc"))'); do
    gh release delete-asset v$VERSION "$f" --repo apache/skywalking-ai-sessionizer --yes
  done
  ```

- **In the source.** The tag has to change, and the script does not change a tag. `prepare`
  refuses a version whose tag or GitHub release exists. Fix the problem on `main`, and agree on the
  dev list how to go on: a new tag for the same version, or the next version. Either way, remove
  the old candidate with `svn rm`. For a new tag of the same version, remove the GitHub prerelease
  and the tag explicitly, on GitHub and in your checkout, before `prepare`. Its push starts a new
  CI run, which creates a new prerelease with a new readiness marker:

  ```sh
  gh release delete v$VERSION --repo apache/skywalking-ai-sessionizer --cleanup-tag --yes
  git tag -d v$VERSION
  ```

## 3. Publish

A PMC member runs this step, because only PMC members can write to the release directory. It
need not be the release manager. On a machine without the candidate, `publish` fetches the voted
files from dist.apache.org.

```sh
tools/release.sh publish $VERSION
```

It does these steps in order.

1. **Check** for git, svn, shasum, tar, awk, gh, gpg, curl and cmp, and that
   `tools/install-manifests.sh` exists, before anything moves. The script needs tar and awk, and it
   runs after the move. Then check the tag, as `candidate` does.
2. **Check the GitHub release.** It must be the release of `v$VERSION`, titled `$VERSION`, and not
   a draft. It must hold every binary archive and `.sha512` file CI attached, and no file that is
   not one of the voted files. A missing prerelease, a missing CI file or a stray file stops the
   command before anything moves. A full release is accepted too: an earlier `publish` promoted
   it, and its files are checked again.

   For a prerelease, it then asks whether `$VERSION` becomes the latest GitHub release. The label
   also decides the `latest` image tag. The answer offered is yes when `$VERSION` is newer than
   every full release, and no otherwise, such as for a patch of an older line or a version with a
   suffix. `--latest` or `--not-latest` gives the answer without asking. Promotion does not move the
   label by itself, because CI creates the prerelease with `--latest=false`. After promotion, the
   label is changed only by hand, and `publish` refuses both options on a promoted release:

   ```sh
   gh release edit v$VERSION --repo apache/skywalking-ai-sessionizer --latest=true
   ```

   On a promoted release, it reads which release the label names, because the website entries
   must say whether `$VERSION` is the latest. A label changed by hand is followed too.
3. **Find the candidate:**

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
4. **Hold the local files to the voted ones.** The GitHub release and the install manifests are
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

5. **Verify the signatures.** It imports KEYS into an empty scratch keyring. Each package must have
   a good signature by a key in KEYS, which gpg does not report as expired or revoked. Otherwise
   nothing moves.
6. **Move the candidate to the release directory.** The move publishes the packages approved by
   the vote:

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
7. **Promote the GitHub release.** It attaches any voted source package, `.sha512` or `.asc` file
   the release is missing. It never uploads CI's files, and never replaces a file: one that differs
   from the voted file is reported for a person to remove. It checks that the release holds exactly
   the voted files, downloads them all and compares every byte with the files from the release
   directory. It reads the release again, and stops if it changed. Then it promotes the prerelease:

   ```sh
   gh release edit v$VERSION --repo apache/skywalking-ai-sessionizer --draft=false --prerelease=false \
     --latest=<the answer> --title $VERSION --notes-file <text>
   ```

   The text is the tag's `docs/en/changes/changes.md` without its heading, followed by "Where to
   get it" below. If this step stops, the move is still done. Run `publish` again: it skips the move
   and resumes here. A release promoted already is not promoted again.
8. **Write the files for the later steps** into `dist/$VERSION/`:
   - `announce.txt`, the announcement mail.
   - `website.txt`, the release for `data/projects.yml` on the website. It is marked the latest
     release only when GitHub's label names `v$VERSION`. It carries the day of the move, as svn
     records it in UTC, so a later run of `publish` writes the same date.
   - `install/`, the install manifests. It removes the old `install/` first, then runs
     `tools/install-manifests.sh $VERSION dist/$VERSION dist/$VERSION/install`. If that script
     fails, the move and the promotion are still done. Fix the script and run `publish` again: it
     skips the move and writes `install/` from scratch. To run the script by hand instead, remove
     `dist/$VERSION/install` first, because the script refuses a directory that is not empty.
9. **Print the next steps**, which are the sections below, in order. They wait at least one hour
   after the move before the website change and the announcement, as the
   [ASF release policy](https://www.apache.org/legal/release-policy.html) asks. When older versions
   are in the release directory, the last step is the later run with `--remove-old`.

The promotion checks the files against the release directory on dist.apache.org, which is the
authority after the vote, not against downloads.apache.org. downloads.apache.org serves the
release directory a short while after the move, and that check would stop almost every first run.

`--dry-run` reads svn and GitHub and prints the plan. It fetches no package, and moves, removes,
promotes and writes nothing. Like a real run, it fetches a missing tag, as `candidate` does.

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
directory. CI creates it on the tag push with unsigned binaries and checksums. The release manager
downloads those bytes, signs them locally, stages them with the source archive for the SVN vote,
and attaches the source archive and signatures to the same page. `publish` checks every file
against the release directory, then promotes that prerelease. CI also retains diagnostic workflow
artifacts, but no step depends on them. The `binaries` job builds every platform on every run but
the promotion, and `packages` runs each with `tools/package-smoke.sh` on a runner of its own
platform.

The released event starts CI's `docker` job. That job publishes the container image to the
GitHub container registry under `$VERSION`, and under `latest` when GitHub names `v$VERSION` its
latest release, as the answer in `publish` decided. Candidate tags and prereleases do not affect
`latest`.
The image is a convenience built from the released source. The source and binary archive
release does not wait for the image job. If the image job fails, retry it on the release tag with `publish_image=true`:

```sh
gh workflow run ci.yaml --repo apache/skywalking-ai-sessionizer --ref v$VERSION -f tag=v$VERSION -f publish_image=true
```

## The website

Wait at least one hour after the move, as the
[ASF release policy](https://www.apache.org/legal/release-policy.html) asks before a download page
changes. Then open a pull request on
[apache/skywalking-website](https://github.com/apache/skywalking-website) with the release in
`dist/$VERSION/website.txt`.

The website keeps every project in one file, `data/projects.yml`. A release there gives both the
downloads page and the documentation of its version. Find the project whose `repo` is
`skywalking-ai-sessionizer`, and add the release to its list under `releases`. The project must
have exactly one release with `latest: true`. `website.txt` follows GitHub's Latest label, which
[Publish](#3-publish) set, and says which of these two cases the version is:

- **The latest release.** Put it first, with `latest: true`. Its `docs` carry `latestLink` and
  `latestCommitId`, and they make the Latest documentation. On the release that was the latest
  before it, set `latest` to `false`, remove `docs.latestLink` and `docs.latestCommitId`, and point
  its `link`, `asc` and `sha512` at `https://archive.apache.org/dist/skywalking/ai-sessionizer/`.
  Change nothing else in an older release.
- **Not the latest release**, such as a patch of an older line. Put it after every newer release,
  with `latest: false` and no Latest documentation. Change no other release.

By hand, the release when it is the latest is:

```yaml
          - version: v$VERSION
            latest: true
            docs:
              link: /docs/skywalking-ai-sessionizer/v$VERSION/readme/
              commitId: <commit hash of v$VERSION>
              latestLink: /docs/skywalking-ai-sessionizer/latest/readme/
              latestCommitId: <commit hash of v$VERSION>
            downloads:
              - name: Source archive
                type: source
                link: https://www.apache.org/dyn/closer.lua/skywalking/ai-sessionizer/$VERSION/apache-skywalking-ai-sessionizer-$VERSION-src.tgz
                asc: https://downloads.apache.org/skywalking/ai-sessionizer/$VERSION/apache-skywalking-ai-sessionizer-$VERSION-src.tgz.asc
                sha512: https://downloads.apache.org/skywalking/ai-sessionizer/$VERSION/apache-skywalking-ai-sessionizer-$VERSION-src.tgz.sha512
              - name: MacOS ARM64
                type: binary
                link: https://www.apache.org/dyn/closer.lua/skywalking/ai-sessionizer/$VERSION/apache-skywalking-ai-sessionizer-$VERSION-bin-darwin-arm64.tgz
                asc: https://downloads.apache.org/skywalking/ai-sessionizer/$VERSION/apache-skywalking-ai-sessionizer-$VERSION-bin-darwin-arm64.tgz.asc
                sha512: https://downloads.apache.org/skywalking/ai-sessionizer/$VERSION/apache-skywalking-ai-sessionizer-$VERSION-bin-darwin-arm64.tgz.sha512
              # The same four lines for each other platform, in the order of PLATFORMS:
              # MacOS AMD64, Linux AMD64, Linux ARM64, Windows AMD64 and Windows ARM64.
            date: Sep. 17th, 2026
```

The date is the day of the move, as svn records it in UTC, in the form the website uses. A later
run of `publish` writes the same date. The packages are linked through closer.lua, which the
[ASF rules for download pages](https://infra.apache.org/release-download-pages.html) require. The
commit of the documentation is the commit the tag names, and the website builds the documentation
of `v$VERSION` and of Latest from it.

The website also posts news of each SkyWalking release under `content/events/`, and the script
does not write that post. `content/events/release-apache-skywalking-ai-sessionizer-0-3-0/index.md`
in the website repository shows the shape: a title, a date, a link to the downloads page, and what
changed, with a link to the milestone at the end. It can go in the same pull request.

Wait until the website is deployed and its downloads page lists the version before the
announcement. The mail links the downloads page and the documentation.

## The announcement

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

## Install manifests

**Before the first submission to each package manager, the PMC agrees to it on
dev@skywalking.apache.org.** A manifest distributes the project under the ASF's name in a new
place: the winget manifests name The Apache Software Foundation as their publisher. The ASF allows
other distribution platforms only for binaries that follow its release, licensing, branding and
trademark policies. The Homebrew tap is this repository, so it needs no new repository. A Scoop
bucket under github.com/apache would be one, which the PMC asks INFRA to create.

`dist/$VERSION/install/` holds the manifests that let package managers install the version. They
are conveniences.

| File | Where it goes | What it downloads |
| --- | --- | --- |
| `homebrew/asz.rb`, `homebrew/asz-claude-code.rb` | `Formula/` on main in this repository | the macOS or Linux binary package, from the GitHub release, with archive.apache.org as its mirror |
| `scoop/skywalking-ai-sessionizer.json` | a Scoop bucket | the Windows packages, from dlcdn.apache.org |
| `winget/manifests/a/Apache/SkyWalkingAISessionizer/$VERSION/` | microsoft/winget-pkgs | the Windows packages, from the GitHub release |

The two Homebrew formulae install from the voted binary package for the machine: macOS or Linux, on
ARM 64 or x86-64. Each names a URL, a mirror and a sha256 for each of the four packages. They build
nothing, so they need no Go. `asz` installs `asz`, and `asz-claude-code` installs
`asz-claude-plugin`, whose caveats give the two commands that install the plugin into Claude Code.
Each installs `LICENSE`, `NOTICE` and `licenses/`, and its test runs its binary.
Homebrew/homebrew-core takes a formula only when it builds from source, and these install a binary
built for each platform.

The tap is this repository. Homebrew looks for a tap's formulae in `Formula/` first, so after
Publish, open a pull request to main that replaces `Formula/asz.rb` and `Formula/asz-claude-code.rb`
with the two files from `dist/$VERSION/install/homebrew/`. The formulae name the sha256 of the voted
packages, so they cannot be committed before the vote, and the tag of a version never holds its own
formulae. `.gitattributes` keeps `Formula/` out of the source package, which would otherwise carry
the formulae of the version before it. Users add the tap and install by the full name, as
[Install](../setup/install.md#homebrew-on-macos-and-linux) shows.

Before the pull request, run the formulae through Homebrew on the voted packages. It writes them into
a local tap, runs `brew style` and `brew audit --strict`, installs and tests both, and removes
everything it installed:

```sh
tools/homebrew-check.sh $VERSION dist/$VERSION
```

CI's `homebrew` job runs the same script on every change, on packages it builds with a version of
its own, so a change that breaks the formulae fails before a release.

Submit each file only after the version is on the download site. The Homebrew formulae and the
winget files download from the GitHub release, which [Publish](#3-publish) promotes.
That URL keeps working after a newer version replaces this one on the download site.
archive.apache.org keeps every version too, but it slows down and then bans heavy use, and winget
runs in scripts and on shared CI machines. The Homebrew formulae name the archive as their mirror,
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
