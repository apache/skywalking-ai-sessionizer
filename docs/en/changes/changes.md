# Changes in 0.5.0

> In development, not yet released. `tools/release/release.sh prepare 0.5.0` removes this note.

## Install

- [Install](../setup/install.md#homebrew-on-macos-and-linux) tells a Homebrew user to run
  `brew trust https://github.com/apache/skywalking-ai-sessionizer` before tapping. Homebrew 7 loads
  a tap that is not its own only after `brew trust`, and it matches a `user/repository` entry only
  against a tap at its default remote, which this one is not: the repository is not named
  `homebrew-skywalking-ai-sessionizer`. Without the trust, `brew tap` stops with
  `Refusing to load formula ... from untrusted tap`.

## Release

- The downloads page lists the source package and the binary archives only. `publish` listed the
  Debian packages there too, beside the archives of the same binaries. They stay in the release
  directory with their signatures and checksums, and apt installs them from the repository on the
  website.
