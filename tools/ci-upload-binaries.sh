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

# CI calls this only after the entire prerelease build and all package tests pass.
# Files already attached to a candidate are never replaced. A failed or rejected
# candidate must be removed explicitly before its replacement is prepared.
set -euo pipefail
[ "$#" -eq 7 ] || { echo 'usage: ci-upload-binaries.sh VERSION COMMIT RUN_ID RUN_ATTEMPT RELEASE_ID PKG_DIR "OS/ARCH ..."' >&2; exit 2; }
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

def digest(path, algorithm="sha512"):
    result = hashlib.new(algorithm)
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            result.update(block)
    return result.hexdigest()

def main():
    version, commit, run_id, attempt, release_id, directory, platforms, checker = sys.argv[1:]
    require(re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?", version), "invalid version")
    require(re.fullmatch(r"[0-9a-f]{40}", commit), "invalid commit")
    for identifier in (run_id, attempt, release_id):
        require(re.fullmatch(r"[1-9][0-9]*", identifier), "invalid run, attempt or release ID")
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

    def current_release():
        release = api(base + "/releases/tags/" + tag)
        require(release.get("id") == int(release_id) and release.get("tag_name") == tag and
                release.get("name") == version and release.get("prerelease") is True and
                release.get("draft") is False, "the prerelease was removed, replaced or changed")
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

    release = current_release()
    require(not release.get("assets") and "<!-- asz-ci-binaries" not in (release.get("body") or ""),
            "the prerelease already has assets or a readiness marker; remove the rejected prerelease explicitly before preparing another candidate")
    check_tag()
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
