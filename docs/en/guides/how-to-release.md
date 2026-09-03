# How to Release

This guide is for the release manager, and for anyone checking a release candidate before voting.
Apache SkyWalking AI Sessionizer follows the Apache release process: a source release is voted on
by the SkyWalking PMC, and only after the vote passes is anything published as a release.

## What counts as a release

A release is the vote, the signed packages in the release directory on dist.apache.org, and the
announcement on the website and the announce list. Nothing else is. A git tag is a candidate. A
GitHub release page, a container image tag and a binaries artifact are conveniences this
project's CI produces, and the GitHub release is only the trigger that publishes the image after
the vote. Until the website announces a version, it is not an official release, whatever GitHub
shows.

## Prerequisites

1. Close every issue in the milestone, or move what is unfinished to the next one.
2. Make sure the changelog is complete. The version has had its own page,
   `docs/en/changes/changes-$VERSION.md`, since its development started, and Current Version in
   the menu points at it.
3. `make check` passes on `main`.

## Prepare

```sh
git checkout main && git pull
tools/release.sh prepare
```

It asks for the version to release and the version development moves to, or takes them as
arguments. On a branch `release/$VERSION` cut from `main` it then, in this order:

1. refuses a dirty tree, an existing tag or branch, and a next version that does not advance;
2. checks the license headers and runs `make check` (`--skip-check` leaves that out);
3. removes the in-development note from the version's changelog page, lists the version under
   Changelog and in `CHANGES.md`, writes the release notes to
   `docs/en/changes/release-notes-$VERSION.md`, commits "Release $VERSION", and puts the annotated
   tag `v$VERSION` on that commit, so the tag carries the finished changelog and the notes;
4. opens the next version in a second commit: its changelog page with the in-development note,
   Current Version pointed at it, and its entry in `CHANGES.md` and on the welcome page;
5. pushes the branch and the tag, and opens the pull request against `main`.

`main` is never pushed to directly. `--dry-run` prints the plan and writes nothing; `--no-push`
stops after the commits and prints the push and pull request commands.

Review and merge the pull request.

## Complete

```sh
tools/release.sh complete
```

It asks for the version, or takes it as an argument, checks that the tag is on GitHub and carries
the release notes, and creates the GitHub release: tag `v$VERSION`, title `$VERSION`, the notes as
stored in the tag, not a draft, not a prerelease. That is all it does. CI publishes the container
image under `$VERSION`, its `major.minor` line, and `latest` when it is the newest release, when
the released event fires, and attaches the binary packages for every platform, with their
checksums, to the release, which is where people download them.

A release that predates this workflow, or a publish that failed, is published by running the CI
workflow by hand with the tag as its input.

## Apache release

Not in use yet. The project is not making Apache releases at this stage, and what the two commands
above produce is a GitHub release and an image, which is not one. When Apache releases start, these
are the steps, between Prepare and Complete, that make a version official.

### Build and sign the candidate

Have a GPG key: upload its public key to a key server, add its fingerprint at
[id.apache.org](https://id.apache.org/), and add it to the
[SkyWalking KEYS file](https://dist.apache.org/repos/dist/release/skywalking/KEYS), which only a PMC
member can commit to. Then, with the tag checked out:

```sh
git checkout v$VERSION
make release VERSION=$VERSION
```

`make release` refuses to run unless the tag is checked out and the tree is clean, because the
binaries are built from the working tree. It writes into `dist/`:

- the source package, `apache-skywalking-ai-sessionizer-$VERSION-src.tgz`, which is `git archive`
  of the tag and so holds exactly what is committed;
- one binary package per platform, `apache-skywalking-ai-sessionizer-$VERSION-bin-<os>-<arch>`,
  as `.tgz` for macOS and Linux and `.zip` for Windows, each holding the binary, `LICENSE` and
  `NOTICE`;
- a `.sha512` checksum and an `.asc` signature beside every package.

The platforms are the `PLATFORMS` list in the Makefile: macOS on Apple silicon and Intel, Linux on
x86-64 and ARM 64, and Windows on x86-64. Every one is cross-compiled from the release manager's
machine, with the Go version the module declares.

### Upload the candidate

```sh
svn co https://dist.apache.org/repos/dist/dev/skywalking/ skywalking-dev
mkdir -p skywalking-dev/ai-sessionizer/$VERSION
cp dist/apache-skywalking-ai-sessionizer-$VERSION-*.tgz* dist/apache-skywalking-ai-sessionizer-$VERSION-*.zip* skywalking-dev/ai-sessionizer/$VERSION/
cd skywalking-dev/ai-sessionizer && svn add $VERSION && svn commit -m "Draft Apache SkyWalking AI Sessionizer release $VERSION"
```

### Call the vote

Send to `dev@skywalking.apache.org`. Check every link before sending.

```text
Subject: [VOTE] Release Apache SkyWalking AI Sessionizer version $VERSION

Hi the SkyWalking Community:
This is a call for vote to release Apache SkyWalking AI Sessionizer version $VERSION.

Release notes:
 * https://github.com/apache/skywalking-ai-sessionizer/blob/v$VERSION/docs/en/changes/changes-$VERSION.md

Release Candidate:
 * https://dist.apache.org/repos/dist/dev/skywalking/ai-sessionizer/$VERSION
 * sha512 checksums
   - <sha512> apache-skywalking-ai-sessionizer-$VERSION-src.tgz
   - <sha512> apache-skywalking-ai-sessionizer-$VERSION-bin-<os>-<arch>.tgz, one per platform

Release Tag:
 * (Git Tag) v$VERSION

Release Commit Hash:
 * https://github.com/apache/skywalking-ai-sessionizer/tree/<commit hash>

Keys to verify the Release Candidate:
 * https://dist.apache.org/repos/dist/release/skywalking/KEYS

Guide to build the release from source:
 * https://github.com/apache/skywalking-ai-sessionizer/blob/v$VERSION/docs/en/guides/how-to-release.md

Voting will start now and will remain open for at least 72 hours. All PMC members are requested to give their votes.

[ ] +1 Release this package.
[ ] +0 No opinion.
[ ] -1 Do not release this package because....

Thanks.
```

### Check the candidate

Everyone voting should check these before a +1:

1. The source package and every binary package are in the candidate directory, each with its
   `.asc` and `.sha512`.
2. `shasum -a 512 -c <package>.sha512` passes for each.
3. `gpg --verify <package>.asc` passes for each, against KEYS.
4. `LICENSE` and `NOTICE` are in every package, and every source file carries the license header.
5. The source package builds and tests: unpack it, then `make build && make test`.
6. A binary works on real data, whether built from source or unpacked from the package for your
   platform: `asz version`, `asz sources`, `asz collect -once`, `asz parse`, `asz verify`.

A PMC vote is binding. The vote passes after 72 hours with at least three binding +1 and more +1
than -1. Send the result to the same list, listing the voters:

```text
Subject: [RESULT][VOTE] Release Apache SkyWalking AI Sessionizer version $VERSION

72 hours passed, we have got (N) +1 bindings (and M +1 non-bindings):

+1 bindings:
<names>

+1 non-bindings:
<names>

Thank you for voting, I will continue the release process.
```

### Publish

1. Move the candidate to the release directory. Only a PMC member can do this.

   ```sh
   svn mv https://dist.apache.org/repos/dist/dev/skywalking/ai-sessionizer/$VERSION \
          https://dist.apache.org/repos/dist/release/skywalking/ai-sessionizer/$VERSION \
          -m "Release Apache SkyWalking AI Sessionizer $VERSION"
   ```

2. Run `tools/release.sh complete` for the GitHub release, the image and the attached packages,
   as above. The signed packages are also distributed through dist.apache.org and the downloads
   page, like every SkyWalking project.

3. Update the [website](https://github.com/apache/skywalking-website): add the version and its
   commit to the project's entry in `data/docs.yml` so the documentation is published for it, and
   add the download link.

4. Announce to `dev@skywalking.apache.org` and `announce@apache.org` from an Apache address.

   ```text
   Subject: [ANNOUNCEMENT] Apache SkyWalking AI Sessionizer $VERSION Released

   Hi the SkyWalking Community,

   On behalf of the SkyWalking Team, I am glad to announce that Apache SkyWalking AI Sessionizer $VERSION is now released.

   SkyWalking AI Sessionizer: conversation-level observability for long-lived AI agents. It assembles fragmented agent telemetry into one durable conversation structure.

   SkyWalking: APM (application performance monitor) tool for distributed systems, especially designed for microservices, cloud native and container-based architectures.

   Download Links: https://skywalking.apache.org/downloads/
   Release Notes: https://github.com/apache/skywalking-ai-sessionizer/blob/v$VERSION/docs/en/changes/changes-$VERSION.md
   Website: https://skywalking.apache.org/
   Documents: https://skywalking.apache.org/docs/skywalking-ai-sessionizer/v$VERSION/readme/

   Resources:
   - Issue: https://github.com/apache/skywalking/issues
   - Mailing list: dev@skywalking.apache.org

   The Apache SkyWalking Team
   ```

## Remove old releases

Keep only the releases users should choose in the release directory. Older ones stay available in
the Apache archive.
