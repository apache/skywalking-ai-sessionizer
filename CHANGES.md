# Changes by Version

The changelog lives in the documentation, one page per version, and is published with the docs on
the SkyWalking website.

- [Next version, in development](docs/en/changes/changes.md)
- [0.1.0](docs/en/changes/changes-0.1.0.md)

The version being prepared has its own page from the start; the next one collects on
`changes.md`. `tools/release.sh` rolls them at release time and lists the version under Changelog
in [`docs/menu.yml`](docs/menu.yml). The text for a GitHub release page is produced from the
version's page with `make release-notes VERSION=<version>`.
