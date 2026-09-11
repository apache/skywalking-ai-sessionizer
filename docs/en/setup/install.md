# Install

asz is one binary, `asz`. This page lists the ways to get it, and says when each one is available.
Every way gives the same program. The binary packages also carry the
[Claude Code plugin](claude-code-plugin.md).

## Binary package

Each release ships one binary package for each platform. The release manager signs every package,
and the PMC votes on them together with the source package. Once a version is released, the
[SkyWalking downloads page](https://skywalking.apache.org/downloads/) links its packages under
SkyWalking AI Sessionizer.

| Platform | Package |
| --- | --- |
| macOS, Apple silicon | `apache-skywalking-ai-sessionizer-<version>-bin-darwin-arm64.tgz` |
| macOS, Intel | `apache-skywalking-ai-sessionizer-<version>-bin-darwin-amd64.tgz` |
| Linux, x86-64 | `apache-skywalking-ai-sessionizer-<version>-bin-linux-amd64.tgz` |
| Linux, ARM 64 | `apache-skywalking-ai-sessionizer-<version>-bin-linux-arm64.tgz` |
| Windows, x86-64 | `apache-skywalking-ai-sessionizer-<version>-bin-windows-amd64.zip` |
| Windows, ARM 64 | `apache-skywalking-ai-sessionizer-<version>-bin-windows-arm64.zip` |

Every package holds:

- `asz`, or `asz.exe` on Windows.
- `claude-code-plugin/`, the Claude Code plugin: its manifest, its hooks, and its binary under
  `bin/`. [Claude Code Plugin](claude-code-plugin.md) says how to point Claude Code at it.
- `LICENSE`, `NOTICE`, and `licenses/` with the license of every module built into the binaries.

The same files are in three places:

- `https://downloads.apache.org/skywalking/ai-sessionizer/<version>/` holds the versions users
  should choose, each package with its `.asc` and `.sha512`. The downloads page links the packages
  through the Apache mirror selector, and the `.asc` and `.sha512` files here.
- `https://archive.apache.org/dist/skywalking/ai-sessionizer/` keeps every version, also after a
  newer one replaces it on the download site. The mirror selector cannot find a version that has
  left the download site, so take such a version from the archive.
- From 0.3.0 on, the [GitHub release](https://github.com/apache/skywalking-ai-sessionizer/releases)
  of a version carries the same packages, `.asc` and `.sha512` files. They are uploaded only after
  each one is checked against the download site.

The GitHub releases of 0.1.0 and 0.2.0 were made before the project's first Apache vote. They are
not Apache releases, and their packages, which CI built, are not signed.

On macOS or Linux, download a package with its checksum and its signature:

```sh
VERSION=0.3.0
PKG=apache-skywalking-ai-sessionizer-$VERSION-bin-linux-amd64.tgz
curl -fL -o "$PKG" "https://www.apache.org/dyn/closer.lua?path=skywalking/ai-sessionizer/$VERSION/$PKG&action=download"
curl -fLO "https://downloads.apache.org/skywalking/ai-sessionizer/$VERSION/$PKG.sha512"
curl -fLO "https://downloads.apache.org/skywalking/ai-sessionizer/$VERSION/$PKG.asc"
```

[Verify it](#verify-a-package), then unpack it and check that it runs:

```sh
mkdir asz
tar -xzf "$PKG" -C asz
./asz/asz version
```

Put the directory on your `PATH`, or copy `asz` into a directory that is on it. If you use the
plugin, keep `claude-code-plugin/` whole, because its hooks run the binary under its own `bin/`.

The binaries are not notarized by Apple. A browser marks the files it downloads, and macOS may
refuse to start a marked binary that is not notarized. `curl` does not mark what it downloads. If
macOS refuses, remove the mark: `xattr -dr com.apple.quarantine asz`.

On Windows, in PowerShell:

```powershell
$Version = "0.3.0"
$Pkg = "apache-skywalking-ai-sessionizer-$Version-bin-windows-amd64.zip"
Invoke-WebRequest -OutFile $Pkg "https://www.apache.org/dyn/closer.lua?path=skywalking/ai-sessionizer/$Version/$Pkg&action=download"
Invoke-WebRequest -OutFile "$Pkg.sha512" "https://downloads.apache.org/skywalking/ai-sessionizer/$Version/$Pkg.sha512"
Invoke-WebRequest -OutFile "$Pkg.asc" "https://downloads.apache.org/skywalking/ai-sessionizer/$Version/$Pkg.asc"
```

[Verify it](#verify-a-package), then unpack it:

```powershell
Expand-Archive $Pkg -DestinationPath asz
.\asz\asz.exe version
```

Claude Code has not yet run the plugin's hooks on Windows, so the command line in `hooks/hooks.json`
has not run there. CI starts the packaged plugin on its Windows runners and gives it each hook
event on standard input, outside Claude Code.
[Claude Code Plugin](claude-code-plugin.md#what-was-verified) says what was verified.

## Verify a package

Verify every package before you use it, the source package too. A package may come from a mirror,
so take the `.sha512`, the `.asc` and the KEYS file from downloads.apache.org itself, never from a
mirror. KEYS holds the public keys of the SkyWalking release managers.

```sh
shasum -a 512 -c "$PKG.sha512"
curl -fLO https://downloads.apache.org/skywalking/KEYS
gpg --import KEYS
gpg --verify "$PKG.asc" "$PKG"
```

`shasum` must print the file name and `OK`. On Linux, `sha512sum -c "$PKG.sha512"` does the same.
`gpg` must print `Good signature`. It may also warn that the key is not certified with a trusted
signature. That warning only says your own keyring does not vouch for the key. It does not say
the signature is bad.

On Windows, in PowerShell, this must print `True`:

```powershell
(Get-FileHash -Algorithm SHA512 $Pkg).Hash -eq (Get-Content "$Pkg.sha512").Split(" ")[0]
```

For the signature, install [Gpg4win](https://www.gpg4win.org/), which provides `gpg`. Download
KEYS, then run the same two `gpg` commands.

## Homebrew, on macOS and Linux

**Available only once its formula is published after a release.** Until then, Homebrew does not
know the package.

Each release writes a formula from the voted binary packages, for macOS and Linux on ARM 64 and
x86-64. The release manager submits it to a Homebrew tap, and this section will name the tap once
the formula is published there. It does not go to homebrew-core, which takes only formulae that
build from source or install output that is the same on every platform. This formula installs a
binary built for each platform.

The formula downloads the binary package for your machine from the GitHub release of the version.
That release carries the voted packages, and its URL keeps working after a newer version comes
out. When GitHub fails, Homebrew takes the same package from archive.apache.org, which keeps every
version. Either way, Homebrew checks the download against the sha256 of the voted package. The
formula builds nothing, so it needs no Go. It installs `asz`, the Claude Code plugin under the
formula's `libexec/claude-code-plugin`, and `LICENSE`, `NOTICE` and `licenses/` at the root of the
formula's prefix. Because it installs the binary package, `asz view` draws the page with the
renderer's fonts.

Once the tap is named here, install by the formula's full name:

```sh
brew install <owner>/<tap>/skywalking-ai-sessionizer
```

Since Homebrew 6.0.0, a formula from a tap that is not Homebrew's own must be trusted before it is
loaded. Installing by the full name trusts that one formula only. Point Claude Code at the plugin
with:

```sh
claude --plugin-dir "$(brew --prefix skywalking-ai-sessionizer)/libexec/claude-code-plugin"
```

`brew --prefix skywalking-ai-sessionizer` prints the formula's `opt` directory, which stays the
same across upgrades. `brew info skywalking-ai-sessionizer` prints the same plugin path.

## Scoop, on Windows

**Available only once its manifest is published after a release.**

Each release writes a Scoop manifest for Windows on x86-64 and ARM 64. The release manager submits
it to a Scoop bucket, and this section will name the bucket once the manifest is published. The
manifest downloads the binary package from dlcdn.apache.org, the delivery network in front of the
download site, checks its sha512, and puts `asz` on the path. The plugin is the
`claude-code-plugin` folder in the app's directory, which `scoop prefix skywalking-ai-sessionizer`
prints.

## winget, on Windows

**Available only once its manifest is published after a release.**

Each release writes winget manifests for Windows on x86-64 and ARM 64. The release manager submits
them to [microsoft/winget-pkgs](https://github.com/microsoft/winget-pkgs). Once that repository
accepts them:

```powershell
winget install --id Apache.SkyWalkingAISessionizer
```

The manifest downloads the binary package from the GitHub release of the version, which carries
the voted packages and keeps its URL after a newer version comes out. It checks the package's
sha256, and adds `asz` as a command. The plugin is the `claude-code-plugin` folder beside
`asz.exe`, in the folder winget installs the package into.

## Build from the source package

The source package is the Apache release itself. It builds with Go 1.27 or later, the version
`go.mod` declares, and the build downloads the Go modules `go.mod` names. It needs no Node.js,
because the conversation renderer the page draws with is committed in the source, built from a
pinned Horizon commit. The source package does not carry the renderer's two fonts, because they
are under the SIL Open Font License, which the ASF keeps out of source releases. A binary built
from it draws the page with system fonts. The binary packages carry the fonts.

```sh
VERSION=0.3.0
SRC=apache-skywalking-ai-sessionizer-$VERSION-src.tgz
curl -fL -o "$SRC" "https://www.apache.org/dyn/closer.lua?path=skywalking/ai-sessionizer/$VERSION/$SRC&action=download"
curl -fLO "https://downloads.apache.org/skywalking/ai-sessionizer/$VERSION/$SRC.sha512"
curl -fLO "https://downloads.apache.org/skywalking/ai-sessionizer/$VERSION/$SRC.asc"
```

[Verify it](#verify-a-package), with `$SRC` in place of `$PKG`. Then:

```sh
tar -xzf "$SRC"
cd apache-skywalking-ai-sessionizer-$VERSION-src
make build VERSION=$VERSION
./bin/asz version
```

`make build` writes `bin/asz`. It writes the plugin's binary to
`plugins/claude-code/bin/asz-claude-plugin`, where the plugin's hooks expect it, so
`claude --plugin-dir plugins/claude-code` runs the plugin from the source. Pass `VERSION`. The
Makefile reads the version from git, and an unpacked source package has no git history, so without
it `asz version` prints an empty version.

Without make, on Windows for example, run the two commands `make build` runs:

```sh
go build -ldflags "-X main.version=0.3.0" -o bin/asz.exe ./cmd/asz
go build -ldflags "-X main.version=0.3.0" -o plugins/claude-code/bin/asz-claude-plugin.exe ./plugins/claude-code
```

Leave out `.exe` on macOS and Linux. `make binaries VERSION=$VERSION` cross-compiles every
platform into `dist/`, the way the binary packages are built.

A git checkout builds the same way, and there the Makefile takes the version from the nearest tag.
See [Quick Start](quick-start.md#build).

## go install

**This builds from the source repository, not from the Apache release.** The Apache release is
the signed source package above, and the binary packages built from it. Use `go install` only if
you accept a build of the tagged source that no vote checked.

For Go users, with Go 1.27 or later:

```sh
go install github.com/apache/skywalking-ai-sessionizer/cmd/asz@v0.3.0
```

It installs `asz` into Go's `bin` directory, `$(go env GOPATH)/bin` unless `GOBIN` is set. It
differs from a package in four ways:

- Go fetches the module from the tag on GitHub, through the Go module proxy. That is the tagged
  source, not the signed source package the vote approved.
- Name a version the downloads page lists as released. A tag is pushed before its vote, so
  `@latest` can name a candidate that was never released. The tags of 0.1.0 and 0.2.0 predate
  the project's first Apache vote.
- `asz version` prints `dev`. The Makefile sets the version at build time, and `go install` does
  not.
- The Claude Code plugin is not installed. Take it from a binary package, or build it from the
  source package.

Measured on 2026-09-11 on macOS: `go install github.com/apache/skywalking-ai-sessionizer/cmd/asz@main`
built commit `dd083cc`, through the Go module proxy and again from GitHub directly. The module Go
downloaded held the committed conversation renderer, and the binary printed
`asz dev (go1.27.1 darwin/arm64)`.

## Not offered

- **No deb or rpm package**, and no apt or yum repository. On Linux, use a binary package, the
  source package, or Homebrew once its formula is published.
- **The container image is pending.** [Container Image](container-image.md) describes the image
  CI builds, and how to build it yourself. It is a convenience, not part of the Apache release, and
  how it is published for a released version is not settled yet.
