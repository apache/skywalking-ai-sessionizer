# Install

asz runs on macOS, Linux and Windows, on x86-64 and ARM 64. To record which files each tool call
changed, also install the [Claude Code plugin](claude-code-plugin.md).

## Install script

Use a version from the [downloads page](https://skywalking.apache.org/downloads/).

On macOS or Linux:

```sh
VERSION=<version>
curl -fsSL "https://raw.githubusercontent.com/apache/skywalking-ai-sessionizer/v$VERSION/install/asz.sh" | sh -s -- "$VERSION"
```

On Windows, in PowerShell:

```powershell
$Version = "<version>"
& ([scriptblock]::Create((Invoke-RestMethod -UseBasicParsing "https://raw.githubusercontent.com/apache/skywalking-ai-sessionizer/v$Version/install/asz.ps1"))) $Version
```

The script checks the package's sha512 and puts `asz` in `~/.local/bin`, or
`%USERPROFILE%\.local\bin` on Windows, which it adds to your `Path`. Run it again with another
version to upgrade. On Windows, stop `asz` first.

## Homebrew, on macOS and Linux

```sh
brew tap apache/skywalking-ai-sessionizer https://github.com/apache/skywalking-ai-sessionizer
brew install apache/skywalking-ai-sessionizer/asz
brew install apache/skywalking-ai-sessionizer/asz-claude-code
```

`asz-claude-code` installs `asz-claude-plugin`, the Claude Code plugin's binary. To install the
plugin into Claude Code, run the two commands that `brew info asz-claude-code` prints. Upgrade with
`brew upgrade asz asz-claude-code`.

## Binary package

The [downloads page](https://skywalking.apache.org/downloads/) links one package per platform:

```text
apache-skywalking-ai-sessionizer-<version>-bin-<os>-<arch>.tgz    os: darwin, linux; arch: amd64, arm64
apache-skywalking-ai-sessionizer-<version>-bin-windows-<arch>.zip  arch: amd64, arm64
```

Each holds `asz`, `asz-claude-plugin` (the Claude Code plugin's binary), `LICENSE`, `NOTICE` and
`licenses/`.

On macOS or Linux:

```sh
PKG=apache-skywalking-ai-sessionizer-$VERSION-bin-linux-amd64.tgz
curl -fL -o "$PKG" "https://www.apache.org/dyn/closer.lua?path=skywalking/ai-sessionizer/$VERSION/$PKG&action=download"
curl -fLO "https://downloads.apache.org/skywalking/ai-sessionizer/$VERSION/$PKG.sha512"
curl -fLO "https://downloads.apache.org/skywalking/ai-sessionizer/$VERSION/$PKG.asc"
```

[Verify it](#verify-a-package), then unpack it and put `asz` on your `PATH`:

```sh
mkdir asz && tar -xzf "$PKG" -C asz
./asz/asz version
```

If macOS refuses to start a binary a browser downloaded, run `xattr -dr com.apple.quarantine asz`.

On Windows, in PowerShell:

```powershell
$Pkg = "apache-skywalking-ai-sessionizer-$Version-bin-windows-amd64.zip"
Invoke-WebRequest -OutFile $Pkg "https://www.apache.org/dyn/closer.lua?path=skywalking/ai-sessionizer/$Version/$Pkg&action=download"
Invoke-WebRequest -OutFile "$Pkg.sha512" "https://downloads.apache.org/skywalking/ai-sessionizer/$Version/$Pkg.sha512"
Invoke-WebRequest -OutFile "$Pkg.asc" "https://downloads.apache.org/skywalking/ai-sessionizer/$Version/$Pkg.asc"
Expand-Archive $Pkg -DestinationPath asz
.\asz\asz.exe version
```

A version that is no longer on downloads.apache.org is under
`https://archive.apache.org/dist/skywalking/ai-sessionizer/`. Each GitHub release carries the same
files.

## Verify a package

Take the `.sha512`, the `.asc` and KEYS from downloads.apache.org, not from a mirror.

```sh
shasum -a 512 -c "$PKG.sha512"
curl -fLO https://downloads.apache.org/skywalking/KEYS
gpg --import KEYS
gpg --verify "$PKG.asc" "$PKG"
```

`shasum` must print `OK`. `gpg` must print `Good signature`, with no word that the key has expired
or has been revoked. If it has, do not use the package, and ask on `dev@skywalking.apache.org`. A
warning that the key is not certified with a trusted signature is expected.

On Windows, in PowerShell, this must print `True`. For the signature, install
[Gpg4win](https://www.gpg4win.org/) and run the same `gpg` commands.

```powershell
(Get-FileHash -Algorithm SHA512 $Pkg).Hash -eq (Get-Content "$Pkg.sha512").Split(" ")[0]
```

## Build from the source package

With Go 1.27 or later:

```sh
SRC=apache-skywalking-ai-sessionizer-$VERSION-src.tgz
curl -fL -o "$SRC" "https://www.apache.org/dyn/closer.lua?path=skywalking/ai-sessionizer/$VERSION/$SRC&action=download"
curl -fLO "https://downloads.apache.org/skywalking/ai-sessionizer/$VERSION/$SRC.sha512"
curl -fLO "https://downloads.apache.org/skywalking/ai-sessionizer/$VERSION/$SRC.asc"
```

[Verify it](#verify-a-package) with `$SRC` in place of `$PKG`, then:

```sh
tar -xzf "$SRC"
cd apache-skywalking-ai-sessionizer-$VERSION-src
make build VERSION=$VERSION
./bin/asz version
```

`make build` writes `bin/asz` and `bin/asz-claude-plugin`. Without make, on Windows for example:

```sh
go build -ldflags "-X main.version=$VERSION" -o bin/asz.exe ./cmd/asz
go build -ldflags "-X main.version=$VERSION" -o bin/asz-claude-plugin.exe ./plugins/claude-code
```

A build from source draws the page with system fonts. The binary packages carry the page's fonts.

## go install

This builds the tagged source, not the voted release:

```sh
go install "github.com/apache/skywalking-ai-sessionizer/cmd/asz@v$VERSION"
```

`asz version` then prints `dev`, and the Claude Code plugin's binary is not installed.
