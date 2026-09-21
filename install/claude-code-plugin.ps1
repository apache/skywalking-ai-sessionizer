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
# 2. Checks that asz-changes.exe starts, and copies it into
#    %USERPROFILE%\.local\bin, where the Claude Code installer puts
#    claude.exe, adding that directory to the user's Path. The plugin's hooks
#    run it by name from the Path.
# 3. Installs the file-changes plugin into Claude Code from the marketplace at
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
$Exe = "asz-changes.exe"
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
    # Only a 404 sends it to the archive. A download site that cannot be
    # reached says nothing about the version, and the reason is reported.
    $Status = if ($_.Exception.Response) { [int]$_.Exception.Response.StatusCode } else { 0 }
    if ($Status -ne 404) { throw "${Me}: cannot download $Pkg.sha512 from downloads.apache.org: $($_.Exception.Message)" }
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
Write-Host "${Me}: installed asz-changes into $Bin"

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
$Id = "file-changes@$Market"
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
$Plugins = @(Read-Claude plugin list --json | ForEach-Object { $_ })
$Installed = @($Plugins | Where-Object { $_.id -eq $Id })
# The plugin was named asz-changes until 0.5.0. One installed under that
# name is moved the same way: its data carried across, then uninstalled
# with the data kept, before its marketplace is removed.
$WasId = "asz-changes@$Market"
$WasInstalled = @($Plugins | Where-Object { $_.id -eq $WasId })
$Declared = @(Read-Claude plugin marketplace list --json | ForEach-Object { $_ } | Where-Object { $_.name -eq $Market })
foreach ($Entry in ($Installed + $WasInstalled)) {
  if ($Entry.scope -ne "user") {
    throw "${Me}: the plugin is installed at the $($Entry.scope) scope. Uninstall it there with --keep-data first. Moving the marketplace removes it from every scope, and deletes its data." }
}

# 0.3.0 ran the plugin with --plugin-dir, whose data directory is
# A plugin's data directory is named after the plugin, so a rename would
# otherwise start it empty: the settings gone, and any change record asz had
# not collected yet left where nothing will look. Both earlier names are
# carried across once, before the first session, so a rename costs nothing.
#
#   asz-changes-inline   0.3.0, loaded with -PluginDir
#   asz-changes-$Market  0.4.0, before this plugin was named file-changes
#
# The directory is found the way asz finds it: CLAUDE_CONFIG_DIR, else
# XDG_CONFIG_HOME\claude, else the profile's .claude. The copy goes to a
# directory beside the final one and is renamed into place, so a copy that
# stops half way leaves nothing that looks finished, and running this
# again copies again rather than moving on to remove the marketplace,
# which deletes the old data.
$Config = if ($env:CLAUDE_CONFIG_DIR) { $env:CLAUDE_CONFIG_DIR }
  elseif ($env:XDG_CONFIG_HOME) { Join-Path $env:XDG_CONFIG_HOME "claude" }
  else { Join-Path $env:USERPROFILE ".claude" }
$Data = Join-Path $Config "plugins\data\file-changes-$Market"
if (-not (Test-Path -LiteralPath $Data)) {
  foreach ($Was in @((Join-Path $Config "plugins\data\asz-changes-$Market"),
                     (Join-Path $Config "plugins\data\asz-changes-inline"))) {
    if (Test-Path -LiteralPath $Was) {
      $Partial = "$Data.partial"
      if (Test-Path -LiteralPath $Partial) { Remove-Item -LiteralPath $Partial -Recurse -Force }
      Copy-Item -LiteralPath $Was -Destination $Partial -Recurse
      Move-Item -LiteralPath $Partial -Destination $Data
      Write-Host "${Me}: carried the plugin data over from $(Split-Path -Leaf $Was)"
      break
    }
  }
}

$Ref = if ($Declared.Count -gt 0) { $Declared[0].ref } else { $null }
if ($Declared.Count -gt 0 -and $Ref -eq $Tag -and $Installed.Count -gt 0) {
  Write-Host "${Me}: the Claude Code plugin is installed at $Tag already"
  return
}
if ($Declared.Count -gt 0 -and $Ref -ne $Tag) {
  if ($Installed.Count -gt 0) { Invoke-Claude plugin uninstall $Id --keep-data }
  if ($WasInstalled.Count -gt 0) { Invoke-Claude plugin uninstall $WasId --keep-data }
  Invoke-Claude plugin marketplace remove $Market
  $Declared = @()
}
if ($Declared.Count -eq 0) {
  Invoke-Claude plugin marketplace add "https://github.com/apache/skywalking-ai-sessionizer.git#$Tag" --sparse .claude-plugin plugins/claude-code/plugin
}
Invoke-Claude plugin install $Id
Write-Host "${Me}: installed the Claude Code plugin at $Tag. Restart Claude Code to load it."
