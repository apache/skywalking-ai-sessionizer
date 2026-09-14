#!/usr/bin/env bash
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

# CI calls this on a release tag push, only after the entire build and all
# package tests pass. It creates the GitHub prerelease of the tag, or reuses
# an empty one an earlier attempt of the run created, then attaches the
# packages. Files already attached to a candidate are never replaced. A
# failed or rejected candidate must be removed explicitly before its
# replacement is prepared.
set -euo pipefail
[ "$#" -eq 6 ] || { echo 'usage: ci-upload-binaries.sh VERSION COMMIT RUN_ID RUN_ATTEMPT PKG_DIR "OS/ARCH ..."' >&2; exit 2; }
script_dir=$(cd "$(dirname "$0")" && pwd)
python3 - "$@" "$script_dir/package-check.sh" <<'PY'
import hashlib
import json
from pathlib import Path
import re
import subprocess
import sys
import tempfile

def require(condition, message):
    if not condition:
        raise ValueError(message)

def api(endpoint):
    result = subprocess.run(["gh", "api", "--method", "GET", endpoint], check=True, stdout=subprocess.PIPE)
    return json.loads(result.stdout)

def release_by_tag(endpoint):
    # Only a missing release reads as none. Any other failure stops the run.
    result = subprocess.run(["gh", "api", "--method", "GET", endpoint], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if result.returncode != 0:
        if b"HTTP 404" in result.stderr:
            return None
        sys.stderr.buffer.write(result.stderr)
        raise subprocess.CalledProcessError(result.returncode, result.args)
    return json.loads(result.stdout)

def digest(path, algorithm="sha512"):
    result = hashlib.new(algorithm)
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            result.update(block)
    return result.hexdigest()

def main():
    version, commit, run_id, attempt, directory, platforms, checker = sys.argv[1:]
    require(re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?", version), "invalid version")
    require(re.fullmatch(r"[0-9a-f]{40}", commit), "invalid commit")
    for identifier in (run_id, attempt):
        require(re.fullmatch(r"[1-9][0-9]*", identifier), "invalid run or attempt ID")
    repository = "apache/skywalking-ai-sessionizer"
    base = "repos/" + repository
    tag = "v" + version
    package_dir = Path(directory).resolve()
    pairs = platforms.split()
    allowed = {os_name + "/" + arch for os_name in ("darwin", "linux", "windows") for arch in ("amd64", "arm64")}
    require(pairs and len(set(pairs)) == len(pairs) and set(pairs) <= allowed, "invalid platforms")
    packages = []
    for platform in pairs:
        os_name, arch = platform.split("/")
        extension = "zip" if os_name == "windows" else "tgz"
        packages.append(f"apache-skywalking-ai-sessionizer-{version}-bin-{os_name}-{arch}.{extension}")
    names = sorted(packages + [name + ".sha512" for name in packages])
    require({entry.name for entry in package_dir.iterdir()} == set(names), "the CI package directory has missing or extra files")
    for name in names:
        require((package_dir / name).is_file() and not (package_dir / name).is_symlink(), "not a regular package file: " + name)
    for name in packages:
        text = (package_dir / (name + ".sha512")).read_text(encoding="ascii")
        match = re.fullmatch(r"([0-9a-fA-F]{128}) [ *]" + re.escape(name) + r"\n?", text)
        require(match and digest(package_dir / name) == match[1].lower(), "invalid checksum for " + name)
    subprocess.run(["sh", checker] + [str(package_dir / name) for name in packages], check=True)

    def prerelease_identity(release):
        return (type(release.get("id")) is int and release["id"] > 0 and release.get("tag_name") == tag and
                release.get("name") == version and release.get("prerelease") is True and
                release.get("draft") is False)

    release_id = None

    def current_release():
        release = api(base + "/releases/tags/" + tag)
        require(release.get("id") == release_id and prerelease_identity(release),
                "the prerelease was removed, replaced or changed")
        return release

    def check_tag():
        obj = api(base + "/git/ref/tags/" + tag)["object"]
        seen = set()
        while obj.get("type") == "tag":
            sha = obj.get("sha", "")
            require(re.fullmatch(r"[0-9a-f]{40}", sha) and sha not in seen and len(seen) < 8, "invalid annotated tag")
            seen.add(sha)
            obj = api(base + "/git/tags/" + sha)["object"]
        require(obj.get("type") == "commit" and obj.get("sha") == commit, "the release tag moved away from this CI commit")

    check_tag()
    release = release_by_tag(base + "/releases/tags/" + tag)
    if release is None:
        with tempfile.TemporaryDirectory(prefix="asz-ci-create-") as scratch:
            notes = Path(scratch) / "notes.md"
            notes.write_text(
                "Development candidate for review by the SkyWalking community. This is not an official Apache release.\n\n"
                "CI attached these unsigned binary archives and SHA-512 checksums after all checks passed on the tag push. "
                "The release manager downloads those exact archives, signs them, creates the source archive locally, "
                "stages the candidate on dist.apache.org for the PMC vote, and attaches the source archive and every signature here.\n\n"
                "If this candidate is rejected, remove this prerelease and its tag before preparing its replacement. "
                "Do not replace its files in place.\n", encoding="utf-8")
            subprocess.run(["gh", "release", "create", tag, "--repo", repository, "--verify-tag", "--prerelease",
                            "--latest=false", "--title", version, "--notes-file", str(notes)], check=True)
        release = release_by_tag(base + "/releases/tags/" + tag)
        require(release is not None, "the prerelease was not created")
    # An earlier attempt of this run may have created the prerelease and
    # stopped before attaching anything. Such an empty prerelease is reused.
    require(prerelease_identity(release), "the GitHub release of " + tag + " is not a public prerelease titled " + version +
            "; remove it explicitly before preparing another candidate")
    require(not release.get("assets") and "<!-- asz-ci-binaries" not in (release.get("body") or ""),
            "the prerelease already has assets or a readiness marker; remove the rejected prerelease explicitly before preparing another candidate")
    release_id = release["id"]
    subprocess.run(["gh", "release", "upload", tag, "--repo", repository] +
                   [str(package_dir / name) for name in names], check=True)
    release = current_release()
    assets = release.get("assets", [])
    require(len(assets) == len(names) and {asset.get("name") for asset in assets} == set(names),
            "the uploaded prerelease does not hold exactly the expected files")
    def asset_identity(items):
        return sorted((asset["name"], asset.get("id"), asset.get("digest"), asset.get("size")) for asset in items)

    identities = asset_identity(assets)
    require(len({asset.get("id") for asset in assets}) == len(names), "the asset IDs are not unique")
    for asset in assets:
        path = package_dir / asset["name"]
        require(type(asset.get("id")) is int and asset["id"] > 0 and
                asset.get("digest") == "sha256:" + digest(path, "sha256") and
                asset.get("size") == path.stat().st_size,
                "the uploaded asset identity, digest or size differs: " + asset["name"])
    with tempfile.TemporaryDirectory(prefix="asz-ci-upload-") as scratch:
        scratch = Path(scratch)
        downloads = scratch / "downloads"
        subprocess.run(["gh", "release", "download", tag, "--repo", repository, "--dir", str(downloads)], check=True)
        require({entry.name for entry in downloads.iterdir()} == set(names), "the downloaded asset set differs")
        for name in names:
            require(digest(downloads / name) == digest(package_dir / name), "uploaded bytes differ for " + name)
        release = current_release()
        require("<!-- asz-ci-binaries" not in (release.get("body") or ""), "a readiness marker appeared during upload")
        require(len(release.get("assets", [])) == len(names) and
                {asset.get("name") for asset in release["assets"]} == set(names), "the prerelease asset set changed")
        require(asset_identity(release["assets"]) == identities, "an uploaded asset was replaced during verification")
        check_tag()
        # ci-binaries.sh recomputes this from the release and refuses a
        # mismatch, so the marker names exactly the files verified here.
        lines = sorted(f"{name} {asset_id} {size} {checksum}" for name, asset_id, checksum, size in identities)
        fingerprint = hashlib.sha256(("\n".join(lines) + "\n").encode("utf-8")).hexdigest()
        marker = f"<!-- asz-ci-binaries run_id={run_id} run_attempt={attempt} commit={commit} assets={fingerprint} -->"
        notes = scratch / "notes.md"
        notes.write_text((release.get("body") or "").rstrip() + "\n\n" + marker + "\n", encoding="utf-8")
        subprocess.run(["gh", "release", "edit", tag, "--repo", repository, "--notes-file", str(notes)], check=True)
    print(f"ci-upload-binaries: attached and verified {len(packages)} binary packages for {tag}")

try:
    main()
except (ValueError, KeyError, TypeError, OSError, subprocess.CalledProcessError) as error:
    print("ci-upload-binaries: " + str(error), file=sys.stderr)
    print("Remove an incomplete or rejected GitHub prerelease explicitly before preparing its replacement.", file=sys.stderr)
    sys.exit(1)
PY
