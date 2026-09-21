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

# Installs asz, the collector of Apache SkyWalking AI Sessionizer, from the
# binary package of a released version, on Windows. Run it again with another
# version to install that one over it. The Claude Code plugin is installed on
# its own, by install/claude-code-plugin.ps1.
#
#   & ([scriptblock]::Create((Invoke-RestMethod -UseBasicParsing "https://raw.githubusercontent.com/apache/skywalking-ai-sessionizer/v$Version/install/asz.ps1"))) $Version
#
# It downloads the package through the Apache mirrors, or from
# archive.apache.org for a version no longer on the download site, with its
# sha512 from the same Apache site, stops unless the two match, checks that
# asz.exe starts, and copies it into %USERPROFILE%\.local\bin, which it adds
# to the user's Path. docs/en/setup/install.md says why.

param([string]$Version)

$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"
$Me = "asz-install"
if (-not $Version) { throw "${Me}: give the version to install, one https://skywalking.apache.org/downloads/ lists as released" }
if ($Version -notmatch '^[0-9A-Za-z.-]+$') { throw "${Me}: $Version is not a version" }

$Arch = switch ((Get-CimInstance Win32_Processor | Select-Object -First 1).Architecture) {
  9 { "amd64" } 12 { "arm64" } default { throw "${Me}: there is no package for this processor" } }
$Pkg = "apache-skywalking-ai-sessionizer-$Version-bin-windows-$Arch.zip"
$Exe = "asz.exe"
$Exes = @("asz.exe", "asz-changes.exe")
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
  # From 0.5.0 the archive holds asz and asz-changes, the change recorder,
  # and both are installed, as Homebrew and apt install both. An older
  # version's archive holds the recorder under its old name, and this
  # installs asz alone from it.
  $Exes = @($Exes | Where-Object { Test-Path -LiteralPath (Join-Path $Unpacked $_) })
  foreach ($One in $Exes) {
    & (Join-Path $Unpacked $One) version
    if ($LASTEXITCODE -ne 0) { throw "${Me}: $One does not run" }
  }
  New-Item -ItemType Directory -Force -Path $Bin | Out-Null
  # Windows does not replace the file of a running program. Finding that
  # out before the copy leaves the installed ones whole.
  foreach ($One in $Exes) {
    $Old = Join-Path $Bin $One
    if (Test-Path -LiteralPath $Old) {
      try { [IO.File]::Open($Old, "Open", "ReadWrite", "None").Dispose() }
      catch { throw "${Me}: $Old is in use. Stop asz and Claude Code, then run this again." } }
  }
  foreach ($One in $Exes) {
    Copy-Item -LiteralPath (Join-Path $Unpacked $One) -Destination $Bin -Force
  }
} finally {
  Remove-Item -LiteralPath $Tmp -Recurse -Force -ErrorAction SilentlyContinue
}
Write-Host "${Me}: installed $($Exes -join ' and ') into $Bin"

$Sep = [IO.Path]::PathSeparator
$UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
if (-not (($UserPath -split $Sep) -contains $Bin)) {
  [Environment]::SetEnvironmentVariable("Path", $(if ($UserPath) { "$UserPath$Sep$Bin" } else { $Bin }), "User")
  Write-Host "${Me}: added $Bin to your user Path. Open a new terminal to use it." }
if (-not (($env:PATH -split $Sep) -contains $Bin)) { $env:PATH = "$env:PATH$Sep$Bin" }
$Found = (Get-Command $Exe -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1).Source
if ($Found -ne (Join-Path $Bin $Exe)) {
  Write-Warning "${Me}: $Exe on this Path is $(if ($Found) { $Found } else { 'not found' }), not the one in $Bin. Put $Bin before it in your Path." }
