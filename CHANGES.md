# Changes by Version

The changelog lives in the documentation, one page per version, and is published with the docs on
the SkyWalking website. The version in development has its own page from the start.

- [0.2.0](docs/en/changes/changes-0.2.0.md) (in development)
- [0.1.0](docs/en/changes/changes-0.1.0.md)

`tools/release.sh prepare` finalises a version's page, lists it under Changelog in
[`docs/menu.yml`](docs/menu.yml), stores its release notes beside it as
`release-notes-<version>.md`, tags, and opens the next version, all on a release branch.
`tools/release.sh complete` creates the GitHub release from the notes stored in the tag.
