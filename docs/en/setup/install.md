# Install

asz is one binary, `asz`. This page lists the ways to get it, and says when each one is available.
Every way gives the same program. The binary packages also carry `asz-claude-plugin`, the binary
of the [Claude Code plugin](claude-code-plugin.md).

## Quick install

Each block below installs the binary package of one released version, 0.4.0 or later, for the
machine it runs on. It downloads the package through the Apache mirror selector and its `.sha512`
from downloads.apache.org itself, and stops unless the two match. It checks that both binaries
start, and only then puts `asz` and `asz-claude-plugin` in the directory where the Claude Code
installer puts `claude`. It checks the checksum and not the signature.
[Verify a package](#verify-a-package) says how to check both by hand.

First set the version to install, one the [downloads page](https://skywalking.apache.org/downloads/)
lists as released: `VERSION=<version>` in a shell, or `$Version = "<version>"` in PowerShell. A block
with no version set stops and says so. Run the block again with another version to install that
one over it.

On macOS or Linux, paste it into zsh, bash or any other POSIX shell. It runs in a subshell, so a
failure stops the block and leaves your terminal open:

```sh
(
  set -eu
  : "${VERSION:?set VERSION to a released version first}"
  case "$(uname -s)" in Darwin) OS=darwin ;; Linux) OS=linux ;; *) echo "no package for $(uname -s)" >&2; exit 1 ;; esac
  case "$(uname -m)" in arm64|aarch64) ARCH=arm64 ;; x86_64|amd64) ARCH=amd64 ;; *) echo "no package for $(uname -m)" >&2; exit 1 ;; esac
  PKG=apache-skywalking-ai-sessionizer-$VERSION-bin-$OS-$ARCH.tgz
  BIN=$HOME/.local/bin
  mkdir -p "$BIN"
  TMP=$(mktemp -d "$BIN/.asz-install.XXXXXX")
  trap 'rm -rf "$TMP"' EXIT
  cd "$TMP"
  curl -fsSL -o "$PKG" "https://www.apache.org/dyn/closer.lua?path=skywalking/ai-sessionizer/$VERSION/$PKG&action=download"
  curl -fsSL -o "$PKG.sha512" "https://downloads.apache.org/skywalking/ai-sessionizer/$VERSION/$PKG.sha512"
  WANT=$(cut -d ' ' -f 1 "$PKG.sha512")
  if command -v sha512sum >/dev/null 2>&1; then GOT=$(sha512sum "$PKG"); else GOT=$(shasum -a 512 "$PKG"); fi
  [ "${#WANT}" -eq 128 ] && [ "${GOT%% *}" = "$WANT" ] || { echo "the sha512 of $PKG does not match $PKG.sha512" >&2; exit 1; }
  tar -xzf "$PKG" asz asz-claude-plugin
  ./asz version
  ./asz-claude-plugin version
  mv -f asz asz-claude-plugin "$BIN/"
  echo "installed asz and asz-claude-plugin into $BIN"
  [ "$(command -v asz-claude-plugin || true)" = "$BIN/asz-claude-plugin" ] ||
    echo "Put $BIN first on your PATH, in your shell profile, then restart Claude Code. The plugin's hooks look for asz-claude-plugin there." >&2
)
```

It installs into `~/.local/bin`. When the `asz-claude-plugin` your shell finds is not the one it
installed, because `~/.local/bin` is not on `PATH` or another copy comes first, the block says so.
Then put `~/.local/bin` first on `PATH` in your shell profile, as the Claude Code installer asks
for `claude`, and restart Claude Code. Claude Code runs the plugin's hooks with its own `PATH`.

On Windows, in PowerShell:

```powershell
& {
  $ErrorActionPreference = "Stop"
  $ProgressPreference = "SilentlyContinue"
  if (-not $Version) { throw "set `$Version to a released version first" }
  $Arch = switch ((Get-CimInstance Win32_Processor | Select-Object -First 1).Architecture) {
    9 { "amd64" } 12 { "arm64" } default { throw "no package for this processor" } }
  $Pkg = "apache-skywalking-ai-sessionizer-$Version-bin-windows-$Arch.zip"
  $Bin = Join-Path $env:USERPROFILE ".local\bin"
  $Tmp = Join-Path ([IO.Path]::GetTempPath()) ("asz-install-" + [guid]::NewGuid())
  New-Item -ItemType Directory -Path $Tmp | Out-Null
  try {
    $Zip = Join-Path $Tmp $Pkg
    Invoke-WebRequest -UseBasicParsing -OutFile $Zip "https://www.apache.org/dyn/closer.lua?path=skywalking/ai-sessionizer/$Version/$Pkg&action=download"
    Invoke-WebRequest -UseBasicParsing -OutFile "$Zip.sha512" "https://downloads.apache.org/skywalking/ai-sessionizer/$Version/$Pkg.sha512"
    $Want = (Get-Content -LiteralPath "$Zip.sha512" -Raw).Trim().Split(" ")[0]
    if ($Want.Length -ne 128 -or (Get-FileHash -LiteralPath $Zip -Algorithm SHA512).Hash -ne $Want) {
      throw "the sha512 of $Pkg does not match $Pkg.sha512" }
    Expand-Archive -LiteralPath $Zip -DestinationPath (Join-Path $Tmp "pkg")
    foreach ($Exe in "asz.exe", "asz-claude-plugin.exe") {
      & (Join-Path (Join-Path $Tmp "pkg") $Exe) version
      if ($LASTEXITCODE -ne 0) { throw "$Exe does not run" } }
    New-Item -ItemType Directory -Force -Path $Bin | Out-Null
    foreach ($Exe in "asz.exe", "asz-claude-plugin.exe") {
      $Old = Join-Path $Bin $Exe
      if (Test-Path -LiteralPath $Old) {
        try { [IO.File]::Open($Old, "Open", "ReadWrite", "None").Dispose() }
        catch { throw "$Old is in use. Stop asz and Claude Code, then run this again." } } }
    foreach ($Exe in "asz.exe", "asz-claude-plugin.exe") {
      Copy-Item -LiteralPath (Join-Path (Join-Path $Tmp "pkg") $Exe) -Destination $Bin -Force }
  } finally {
    Remove-Item -LiteralPath $Tmp -Recurse -Force -ErrorAction SilentlyContinue
  }
  Write-Host "installed asz and asz-claude-plugin into $Bin"
  $Sep = [IO.Path]::PathSeparator
  $UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
  if (-not (($UserPath -split $Sep) -contains $Bin)) {
    [Environment]::SetEnvironmentVariable("Path", $(if ($UserPath) { "$UserPath$Sep$Bin" } else { $Bin }), "User")
    Write-Host "added $Bin to your user Path" }
  if (-not (($env:PATH -split $Sep) -contains $Bin)) { $env:PATH = "$env:PATH$Sep$Bin" }
  $Found = (Get-Command asz-claude-plugin.exe -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1).Source
  if ($Found -ne (Join-Path $Bin "asz-claude-plugin.exe")) {
    Write-Warning "asz-claude-plugin.exe on this Path is $(if ($Found) { $Found } else { 'not found' }), not the one in $Bin. Put $Bin before it in your Path." }
  Write-Host "Restart Claude Code from a new terminal, so its hooks find asz-claude-plugin."
}
```

It installs into `%USERPROFILE%\.local\bin`, where the Claude Code installer puts `claude.exe`. It
adds that directory to your user `Path` when it is not there, and to the `Path` of the terminal it
runs in. When the `asz-claude-plugin.exe` that terminal finds is not the one it installed, it warns.
Restart Claude Code from a new terminal afterwards, so its hooks see the new `Path`. Stop `asz` and
Claude Code before you install over them. Windows does not replace the file of a running program,
so the block checks both files first, and stops before it copies either when one is in use.

On 2026-09-17 both blocks ran against packages built from the development tree and served from
the same machine, with only the two download addresses changed:

- The macOS and Linux block ran in zsh, bash and sh on macOS on Apple silicon. There, a wrong
  checksum, a version that is not on the site and no version at all each stopped the block before
  anything was installed, left no temporary directory, and left the shell that ran it running. A
  second run installed over the first. On Linux on ARM 64 the block installed both binaries in a
  Debian container, with GNU tar and `sha512sum`, and in an Alpine container, with BusyBox.
- The Windows block ran in PowerShell 7.4.7 on Linux. The package there held Linux binaries named
  `.exe`, and a stub answered the processor query. It installed both binaries, added the directory
  to the terminal's path, and installed again over them. With another `asz-claude-plugin.exe`
  earlier on the path, it warned. With the installed `asz-claude-plugin.exe` held open, a wrong
  checksum, or no version, it stopped with nothing copied and no temporary directory left. The
  user `Path`, which Linux does not keep, and a running program's file, which Linux does not lock,
  were not tested. It has not run on Windows or in Windows PowerShell 5.1.

The plugin itself is installed into Claude Code with two more commands, which
[Claude Code Plugin](claude-code-plugin.md#install) gives.

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
- `asz-claude-plugin`, or `asz-claude-plugin.exe`, the binary the Claude Code plugin's hooks run.
  The plugin's manifest and hooks are not in the package. Claude Code installs them from the
  marketplace at the version's tag, as [Claude Code Plugin](claude-code-plugin.md#install) says.
  Up to 0.3.0, the package held the whole plugin under `claude-code-plugin/`.
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

The commands below use the version you set, as in [Quick install](#quick-install).

On macOS or Linux, download a package with its checksum and its signature:

```sh
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

Put the directory on your `PATH`, or copy `asz` and `asz-claude-plugin` into a directory that is on
it. The plugin's hooks run `asz-claude-plugin` by name, so Claude Code must find it on its `PATH`.

The binaries are not notarized by Apple. A browser marks the files it downloads, and macOS may
refuse to start a marked binary that is not notarized. `curl` does not mark what it downloads. If
macOS refuses, remove the mark: `xattr -dr com.apple.quarantine asz`.

On Windows, in PowerShell:

```powershell
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

Claude Code has not yet run the plugin's hooks on Windows. CI unpacks each Windows package on a
Windows runner of its processor, outside Claude Code. It finds `asz-claude-plugin` by name on the
path, and runs it with a `SessionStart`, a `PreToolUse`, a `PostToolUse` and a `SessionEnd` event on
standard input. [Claude Code Plugin](claude-code-plugin.md#what-was-verified) says what was verified.

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
`gpg` must print `Good signature`, and nothing about an expired or revoked key: no `[expired]`
after the name, no `Note: This key has expired!`, and no warning that the key or a subkey
`has been revoked by its owner`. gpg 2.5.18 prints `Good signature` and exits 0 for an expired or
revoked key too, and the
[ASF release signing guide](https://infra.apache.org/release-signing.html) counts a signature as
valid only when gpg verifies it as good and does not complain about an expired or revoked key. If
gpg does complain, the package is not verified: do not use it, and ask on
`dev@skywalking.apache.org`. gpg may also warn that the key is not certified with a trusted
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
formula builds nothing, so it needs no Go. It installs `asz` and `asz-claude-plugin` on your
`PATH`, and `LICENSE`, `NOTICE` and `licenses/` at the root of the formula's prefix. Because it installs the binary package, `asz view` draws the page with the
renderer's fonts.

Once the tap is named here, install by the formula's full name:

```sh
brew install <owner>/<tap>/skywalking-ai-sessionizer
```

Since Homebrew 6.0.0, a formula from a tap that is not Homebrew's own must be trusted before it is
loaded. Installing by the full name trusts that one formula only. The formula's caveats, which
`brew info skywalking-ai-sessionizer` prints again, give the two commands that install the plugin
into Claude Code at the formula's version. [Claude Code Plugin](claude-code-plugin.md#install)
gives the same commands.

## Scoop, on Windows

**Available only once its manifest is published after a release.**

Each release writes a Scoop manifest for Windows on x86-64 and ARM 64. The release manager submits
it to a Scoop bucket, and this section will name the bucket once the manifest is published. The
manifest downloads the binary package from dlcdn.apache.org, the delivery network in front of the
download site, checks its sha512, and puts `asz` and `asz-claude-plugin` on the path. The plugin
itself is installed into Claude Code as [Claude Code Plugin](claude-code-plugin.md#install) says.

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
sha256, and adds `asz` and `asz-claude-plugin` as commands. The plugin itself is installed into
Claude Code as [Claude Code Plugin](claude-code-plugin.md#install) says.

## Build from the source package

The source package is the Apache release itself. It builds with Go 1.27 or later, the version
`go.mod` declares, and the build downloads the Go modules `go.mod` names. It needs no Node.js,
because the conversation renderer the page draws with is committed in the source, built from a
pinned Horizon commit. The source package does not carry the renderer's two fonts, because they
are under the SIL Open Font License, which the ASF keeps out of source releases. A binary built
from it draws the page with system fonts. The binary packages carry the fonts.

With `VERSION` set to a released version:

```sh
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

`make build` writes `bin/asz` and `bin/asz-claude-plugin`. Pass `VERSION`. The Makefile reads the
version from git, and an unpacked source package has no git history, so without it `asz version`
prints an empty version. Up to 0.3.0, `make build` wrote the plugin's binary to
`plugins/claude-code/bin/` instead.

To run the plugin from the source of 0.4.0 or later, put `bin` first on `PATH` and load the plugin's
directory:

```sh
PATH="$PWD/bin:$PATH" claude --plugin-dir plugins/claude-code/plugin
```

`--plugin-dir` lasts for that one session. Its data directory is `asz-changes-inline`, not the
`asz-changes-skywalking-ai-sessionizer` of an installed plugin.

Without make, on Windows for example, run the two commands `make build` runs:

```sh
go build -ldflags "-X main.version=$VERSION" -o bin/asz.exe ./cmd/asz
go build -ldflags "-X main.version=$VERSION" -o bin/asz-claude-plugin.exe ./plugins/claude-code
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
go install "github.com/apache/skywalking-ai-sessionizer/cmd/asz@v$VERSION"
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
- The Claude Code plugin's binary is not installed. Take it from a binary package, or build it from
  the source package. `go install` of `./plugins/claude-code` would name the binary `claude-code`,
  which the plugin's hooks do not run.

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
