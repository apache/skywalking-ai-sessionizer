# How to Release

This guide is for the release manager, and for anyone checking a release candidate before voting.
Apache SkyWalking AI Sessionizer follows the Apache release process: a source release is voted on
by the SkyWalking PMC, and only after the vote passes is anything published as a release.

## Prerequisites

1. Close every issue in the milestone, or move what is unfinished to the next one.
2. Write the changes page, `docs/en/changes/changes-<version>.md`, and add it to `docs/menu.yml`.
3. Run `make check`. It includes the dependency license check.
4. Have a GPG key. Upload its public key to a key server, add its fingerprint at
   [id.apache.org](https://id.apache.org/), and add it to the
   [SkyWalking KEYS file](https://dist.apache.org/repos/dist/release/skywalking/KEYS). Only a PMC
   member can commit to that file.

## Tag and build the candidate

```sh
export VERSION=0.1.0
git clone git@github.com:apache/skywalking-ai-sessionizer.git && cd skywalking-ai-sessionizer
git tag -a "v$VERSION" -m "Release Apache SkyWalking AI Sessionizer v$VERSION"
git push origin "v$VERSION"
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

Pushing the tag builds the container image and publishes it under its `sha-` tag only, and builds
the same binary packages as a workflow artifact. The version tags and the release assets wait for
the vote.

## Upload the candidate

```sh
svn co https://dist.apache.org/repos/dist/dev/skywalking/ skywalking-dev
mkdir -p skywalking-dev/ai-sessionizer/$VERSION
cp dist/apache-skywalking-ai-sessionizer-$VERSION-*.tgz* dist/apache-skywalking-ai-sessionizer-$VERSION-*.zip* skywalking-dev/ai-sessionizer/$VERSION/
cd skywalking-dev/ai-sessionizer && svn add $VERSION && svn commit -m "Draft Apache SkyWalking AI Sessionizer release $VERSION"
```

## Call the vote

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

## Check the candidate

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

## Publish

1. Move the candidate to the release directory. Only a PMC member can do this.

   ```sh
   svn mv https://dist.apache.org/repos/dist/dev/skywalking/ai-sessionizer/$VERSION \
          https://dist.apache.org/repos/dist/release/skywalking/ai-sessionizer/$VERSION \
          -m "Release Apache SkyWalking AI Sessionizer $VERSION"
   ```

2. Publish the GitHub release for tag `v$VERSION`, with the changes page as its notes. Publishing
   the release is what attaches the version tags to the container image, `$VERSION`, its
   `major.minor` line, and `latest` when it is the newest release, and what attaches the binary
   packages and their checksums to the release as assets. A release that predates this workflow,
   or a publish that failed, is promoted by running the CI workflow by hand with the tag as its
   input.

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
