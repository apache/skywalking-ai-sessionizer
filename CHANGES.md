# Changes by Version

The changelog lives in the documentation, one page per version, and is published with the docs on
the SkyWalking website. The version in development has its own page from the start.

- [0.1.0](docs/en/changes/changes-0.1.0.md) (in development)

`tools/release.sh release <version>` finalises a version's page and lists it under Changelog in
[`docs/menu.yml`](docs/menu.yml); `tools/release.sh next <version>` opens the next one. The text
for a GitHub release page is produced from the version's page with
`make release-notes VERSION=<version>`.
