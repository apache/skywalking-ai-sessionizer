# Licensed to the Apache Software Foundation (ASF) under one
# or more contributor license agreements.  See the NOTICE file
# distributed with this work for additional information
# regarding copyright ownership.  The ASF licenses this file
# to you under the Apache License, Version 2.0 (the
# "License"); you may not use this file except in compliance
# with the License.  You may obtain a copy of the License at
#
#   http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing,
# software distributed under the License is distributed on an
# "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
# KIND, either express or implied.  See the License for the
# specific language governing permissions and limitations
# under the License.

# Installs the asz Claude Code plugin of a released version on Windows, and
# moves an installed one to that version. asz itself is installed on its own,
# by install/asz.ps1.
#
#   & ([scriptblock]::Create((Invoke-RestMethod -UseBasicParsing "https://raw.githubusercontent.com/apache/skywalking-ai-sessionizer/v$Version/install/claude-code-plugin.ps1"))) $Version
#
# 1. Downloads the binary package of the version through the Apache mirrors,
#    or from archive.apache.org for a version no longer on the download site,
#    with its sha512 from the same Apache site, and stops unless they match.
# 2. Checks that asz-claude-plugin.exe starts, and copies it into
#    %USERPROFILE%\.local\bin, where the Claude Code installer puts
#    claude.exe, adding that directory to the user's Path. The plugin's hooks
#    run it by name from the Path.
# 3. Installs the asz-changes plugin into Claude Code from the marketplace at
#    the version's tag. A plugin at another tag is uninstalled with its data
#    kept first, because removing the marketplace would delete the records
#    asz has not collected yet.
#
# docs/en/setup/claude-code-plugin.md says why.

param([string]$Version)

$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"
$Me = "asz-claude-code-plugin-install"
if (-not $Version) { throw "${Me}: give the version to install, one https://skywalking.apache.org/downloads/ lists as released" }
if ($Version -notmatch '^[0-9A-Za-z.-]+$') { throw "${Me}: $Version is not a version" }
if (-not (Get-Command claude -ErrorAction SilentlyContinue)) { throw "${Me}: claude is not on the Path. Install Claude Code first, then run this again." }

$Arch = switch ((Get-CimInstance Win32_Processor | Select-Object -First 1).Architecture) {
  9 { "amd64" } 12 { "arm64" } default { throw "${Me}: there is no package for this processor" } }
$Pkg = "apache-skywalking-ai-sessionizer-$Version-bin-windows-$Arch.zip"
$Exe = "asz-claude-plugin.exe"
$Bin = Join-Path $env:USERPROFILE ".local\bin"
$Tmp = Join-Path ([IO.Path]::GetTempPath()) ("asz-install-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path $Tmp | Out-Null
try {
  $Zip = Join-Path $Tmp $Pkg
  # The download site keeps only the newest release. A version there comes
  # through the mirrors. Any other comes from the archive, which keeps every
  # version but slows down heavy use, so it is not asked first.
  $Site = "https://downloads.apache.org/skywalking/ai-sessionizer/$Version"
  $Archive = "https://archive.apache.org/dist/skywalking/ai-sessionizer/$Version"
  try {
    Invoke-WebRequest -UseBasicParsing -OutFile "$Zip.sha512" "$Site/$Pkg.sha512"
    $From = "https://www.apache.org/dyn/closer.lua?path=skywalking/ai-sessionizer/$Version/$Pkg&action=download"
    Write-Host "${Me}: downloading $Pkg through the Apache mirrors"
  } catch {
    try { Invoke-WebRequest -UseBasicParsing -OutFile "$Zip.sha512" "$Archive/$Pkg.sha512" }
    catch { throw "${Me}: neither downloads.apache.org nor archive.apache.org has $Pkg. Is $Version released?" }
    $From = "$Archive/$Pkg"
    Write-Host "${Me}: downloading $Pkg from archive.apache.org, as the download site holds only the newest release"
  }
  Invoke-WebRequest -UseBasicParsing -OutFile $Zip $From
  $Want = (Get-Content -LiteralPath "$Zip.sha512" -Raw).Trim().Split(" ")[0]
  if ($Want.Length -ne 128 -or (Get-FileHash -LiteralPath $Zip -Algorithm SHA512).Hash -ne $Want) {
    throw "${Me}: the sha512 of $Pkg does not match $Pkg.sha512, so nothing was installed" }
  $Unpacked = Join-Path $Tmp "pkg"
  Expand-Archive -LiteralPath $Zip -DestinationPath $Unpacked
  & (Join-Path $Unpacked $Exe) version
  if ($LASTEXITCODE -ne 0) { throw "${Me}: $Exe does not run" }
  New-Item -ItemType Directory -Force -Path $Bin | Out-Null
  # Windows does not replace the file of a running program. Finding that
  # out before the copy leaves the installed one whole.
  $Old = Join-Path $Bin $Exe
  if (Test-Path -LiteralPath $Old) {
    try { [IO.File]::Open($Old, "Open", "ReadWrite", "None").Dispose() }
    catch { throw "${Me}: $Old is in use. Stop Claude Code, then run this again." } }
  Copy-Item -LiteralPath (Join-Path $Unpacked $Exe) -Destination $Bin -Force
} finally {
  Remove-Item -LiteralPath $Tmp -Recurse -Force -ErrorAction SilentlyContinue
}
Write-Host "${Me}: installed asz-claude-plugin into $Bin"

$Sep = [IO.Path]::PathSeparator
$UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
if (-not (($UserPath -split $Sep) -contains $Bin)) {
  [Environment]::SetEnvironmentVariable("Path", $(if ($UserPath) { "$UserPath$Sep$Bin" } else { $Bin }), "User")
  Write-Host "${Me}: added $Bin to your user Path" }
if (-not (($env:PATH -split $Sep) -contains $Bin)) { $env:PATH = "$env:PATH$Sep$Bin" }
$Found = (Get-Command $Exe -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1).Source
if ($Found -ne (Join-Path $Bin $Exe)) {
  Write-Warning "${Me}: $Exe on this Path is $(if ($Found) { $Found } else { 'not found' }), not the one in $Bin. Put $Bin before it in your Path." }

$Market = "skywalking-ai-sessionizer"
$Id = "asz-changes@$Market"
$Tag = "v$Version"
# Windows PowerShell 5.1 can turn what a program writes to standard error
# into an error, which "Stop" would make fatal. The exit code decides here.
function Invoke-Claude {
  $ErrorActionPreference = "Continue"
  & claude @args
  if ($LASTEXITCODE -ne 0) { throw "${Me}: claude $args failed. The plugin's data is kept. Run this again." }
}
function Read-Claude {
  $ErrorActionPreference = "Continue"
  $Text = (& claude @args) -join "`n"
  if ($LASTEXITCODE -ne 0) { throw "${Me}: claude $args failed" }
  if ($Text.Trim()) { return @($Text | ConvertFrom-Json) } else { return @() }
}
$Installed = @(Read-Claude plugin list --json | ForEach-Object { $_ } | Where-Object { $_.id -eq $Id })
$Declared = @(Read-Claude plugin marketplace list --json | ForEach-Object { $_ } | Where-Object { $_.name -eq $Market })
foreach ($Entry in $Installed) {
  if ($Entry.scope -ne "user") {
    throw "${Me}: asz-changes is installed at the $($Entry.scope) scope. Uninstall it there with --keep-data first. Moving the marketplace removes it from every scope, and deletes its data." }
}

# 0.3.0 ran the plugin with --plugin-dir, whose data directory is
# asz-changes-inline. Its settings move to the installed plugin's directory
# once, before the first session.
$Config = if ($env:CLAUDE_CONFIG_DIR) { $env:CLAUDE_CONFIG_DIR } else { Join-Path $env:USERPROFILE ".claude" }
$Inline = Join-Path $Config "plugins\data\asz-changes-inline\settings.yaml"
$Data = Join-Path $Config "plugins\data\asz-changes-$Market"
if ((Test-Path -LiteralPath $Inline) -and -not (Test-Path -LiteralPath (Join-Path $Data "settings.yaml"))) {
  New-Item -ItemType Directory -Force -Path $Data | Out-Null
  Copy-Item -LiteralPath $Inline -Destination $Data
  Write-Host "${Me}: copied the settings of the plugin loaded with --plugin-dir"
}

$Ref = if ($Declared.Count -gt 0) { $Declared[0].ref } else { $null }
if ($Declared.Count -gt 0 -and $Ref -eq $Tag -and $Installed.Count -gt 0) {
  Write-Host "${Me}: the Claude Code plugin is installed at $Tag already"
  return
}
if ($Declared.Count -gt 0 -and $Ref -ne $Tag) {
  if ($Installed.Count -gt 0) { Invoke-Claude plugin uninstall $Id --keep-data }
  Invoke-Claude plugin marketplace remove $Market
  $Declared = @()
}
if ($Declared.Count -eq 0) {
  Invoke-Claude plugin marketplace add "https://github.com/apache/skywalking-ai-sessionizer.git#$Tag" --sparse .claude-plugin plugins/claude-code/plugin
}
Invoke-Claude plugin install $Id
Write-Host "${Me}: installed the Claude Code plugin at $Tag. Restart Claude Code to load it."
