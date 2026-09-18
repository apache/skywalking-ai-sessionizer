# Changes in 0.5.0

> In development, not yet released. `tools/release/release.sh prepare 0.5.0` removes this note.

## Release

- The downloads page lists the source package and the binary archives only. `publish` listed the
  Debian packages there too, beside the archives of the same binaries. They stay in the release
  directory with their signatures and checksums, and apt installs them from the repository on the
  website.
