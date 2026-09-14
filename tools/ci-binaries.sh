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

# Download the CI-built binary packages from the tag's GitHub prerelease.
# Usage: tools/ci-binaries.sh VERSION COMMIT RUN_ID OUT_DIR "OS/ARCH ..."
# OUT_DIR must be new or empty. PLATFORMS comes from the release tag's Makefile.
# An empty RUN_ID or auto uses the run recorded by CI after uploading its files.
# Only GitHub API reads are made. No package is installed before its run,
# release identity, asset digests, checksums and archive checks have passed.
set -euo pipefail
[ "$#" -eq 5 ] || { echo 'usage: tools/ci-binaries.sh VERSION COMMIT RUN_ID OUT_DIR "OS/ARCH ..."' >&2; exit 2; }
command -v python3 >/dev/null || { echo 'ci-binaries: python3 is required' >&2; exit 1; }
command -v gh >/dev/null || { echo 'ci-binaries: gh is required' >&2; exit 1; }
script_dir=$(cd "$(dirname "$0")" && pwd)

python3 - "$@" "$script_dir/package-check.sh" <<'PY'
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile


def fail(message):
    raise ValueError(message)


def require(condition, message):
    if not condition:
        fail(message)


def api(endpoint, *, pages=False, output=None):
    command = ["gh", "api", "--method", "GET"]
    if pages:
        command += ["--paginate", "--slurp"]
    if output is not None:
        command += ["-H", "Accept: application/octet-stream"]
    command.append(endpoint)
    result = subprocess.run(command, stdout=output or subprocess.PIPE, check=True)
    if output is None:
        return json.loads(result.stdout)


def digest(path, algorithm):
    result = hashlib.new(algorithm)
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            result.update(block)
    return result.hexdigest()


def tag_commit(base, tag):
    obj = api(base + "/git/ref/tags/" + tag)["object"]
    seen = set()
    while obj.get("type") == "tag":
        sha = obj.get("sha", "")
        require(re.fullmatch(r"[0-9a-f]{40}", sha) and sha not in seen and len(seen) < 8,
                "the release tag does not resolve to one commit")
        seen.add(sha)
        obj = api(base + "/git/tags/" + sha)["object"]
    require(obj.get("type") == "commit", "the release tag does not name a commit")
    return obj.get("sha")


def empty_destination(path):
    require(not path.is_symlink(), "OUT_DIR must not be a symbolic link")
    if path.exists():
        require(path.is_dir() and not any(path.iterdir()), "OUT_DIR must be new or empty")


def release_identity(release, tag, version, commit):
    require(type(release.get("id")) is int and release["id"] > 0 and
            release.get("tag_name") == tag and release.get("name") == version and
            release.get("draft") is False and release.get("prerelease") is True,
            "the GitHub release must be the public developer prerelease for " + tag)
    markers = re.findall(r"<!-- asz-ci-binaries run_id=([1-9][0-9]*) run_attempt=([1-9][0-9]*) commit=([0-9a-f]{40}) -->",
                         release.get("body") or "")
    require(len(markers) == 1 and markers[0][2] == commit,
            "the prerelease is not ready: wait for CI to upload and verify all binary assets")
    return release["id"], markers[0]


def release_assets(base, release_id, expected):
    pages = api(base + f"/releases/{release_id}/assets?per_page=100", pages=True)
    require(isinstance(pages, list) and all(isinstance(page, list) for page in pages),
            "invalid release asset page response")
    assets = [asset for page in pages for asset in page]
    require(len(assets) == len(expected) and {asset.get("name") for asset in assets} == expected,
            "the prerelease must contain exactly the expected packages and their .sha512 files; wait for CI or remove the rejected prerelease before another candidate")
    require(len({asset.get("id") for asset in assets}) == len(assets), "duplicate release asset IDs")
    for asset in assets:
        require(type(asset.get("id")) is int and asset["id"] > 0 and asset.get("state") == "uploaded" and
                type(asset.get("size")) is int and asset["size"] > 0,
                "the release asset is not fully uploaded: " + asset["name"])
        require(re.fullmatch(r"sha256:[0-9a-f]{64}", asset.get("digest") or ""),
                "the release asset has no SHA-256 digest from GitHub: " + asset["name"])
    return {asset["name"]: (asset["id"], asset["size"], asset["digest"]) for asset in assets}


def main():
    version, commit, run_id, directory, platform_text, package_check = sys.argv[1:]
    require(re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?", version), "invalid release version")
    require(re.fullmatch(r"[0-9a-f]{40}", commit), "COMMIT must be the complete commit SHA")
    automatic = run_id in ("", "auto")
    require(automatic or re.fullmatch(r"[1-9][0-9]*", run_id), "RUN_ID must be auto or a positive integer")
    platforms = platform_text.split()
    allowed = {os_name + "/" + arch for os_name in ("darwin", "linux", "windows") for arch in ("amd64", "arm64")}
    require(platforms and len(platforms) == len(set(platforms)) and set(platforms) <= allowed,
            "PLATFORMS must contain distinct supported OS/ARCH pairs from the release tag")
    packages = []
    for platform in platforms:
        os_name, arch = platform.split("/")
        extension = "zip" if os_name == "windows" else "tgz"
        packages.append(f"apache-skywalking-ai-sessionizer-{version}-bin-{os_name}-{arch}.{extension}")
    expected = set(packages + [name + ".sha512" for name in packages])
    output = Path(directory).absolute()
    empty_destination(output)
    repository = "apache/skywalking-ai-sessionizer"
    base = "repos/" + repository
    tag = "v" + version
    require(tag_commit(base, tag) == commit, f"{tag} does not point at COMMIT in {repository}")
    release_endpoint = base + "/releases/tags/" + tag
    identity = release_identity(api(release_endpoint), tag, version, commit)
    release_id, (build_run, build_attempt, _) = identity
    require(automatic or run_id == build_run,
            "--ci-run must name the CI run that uploaded this prerelease: " + build_run)
    run_id = build_run
    workflow = api(base + "/actions/workflows/ci.yaml")
    require(workflow.get("name") == "CI" and workflow.get("path") == ".github/workflows/ci.yaml" and
            type(workflow.get("id")) is int and workflow["id"] > 0,
            "the repository's CI workflow could not be identified")
    run = api(base + "/actions/runs/" + run_id)
    require(run.get("id") == int(run_id), "the API returned a different CI run")
    require(run.get("repository", {}).get("full_name") == repository and
            run.get("head_repository", {}).get("full_name") == repository,
            "the CI run must come from the Apache repository, not a fork")
    require(type(run["repository"].get("id")) is int and run["repository"]["id"] > 0 and
            run["head_repository"].get("id") == run["repository"]["id"] and
            type(run.get("run_attempt")) is int and run["run_attempt"] > 0,
            "the CI run is missing its repository or attempt identity")
    require(run.get("status") == "completed" and run.get("conclusion") == "success",
            "the CI run must be completed and successful")
    require(run["run_attempt"] == int(build_attempt), "the CI run attempt differs from the prerelease's build")
    require(run.get("head_sha") == commit and run.get("head_branch") == tag,
            "the CI run must run on the release tag at COMMIT; dispatch it with --ref " + tag)
    require(run.get("event") in ("workflow_dispatch", "release"), "the CI run is not a prerelease build")
    require(run.get("workflow_id") == workflow.get("id") and
            run.get("path", "").split("@", 1)[0] == workflow["path"],
            "the run is not the repository's CI workflow")
    assets = release_assets(base, release_id, expected)
    output.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=".ci-binaries-", dir=output.parent) as scratch:
        scratch = Path(scratch)
        staging = scratch / "verified"
        staging.mkdir()
        # Names are checked against exact basenames before creating files.
        # Asset IDs bind downloads to this release, even if it is replaced.
        for name, (asset_id, size, checksum) in assets.items():
            path = staging / name
            with path.open("xb") as destination:
                api(base + "/releases/assets/" + str(asset_id), output=destination)
            require(path.stat().st_size == size and digest(path, "sha256") == checksum[7:],
                    "the downloaded asset does not match GitHub's SHA-256 digest and size: " + name)
        for name in packages:
            text = (staging / (name + ".sha512")).read_text(encoding="ascii")
            match = re.fullmatch(r"([0-9a-fA-F]{128}) [ *]" + re.escape(name) + r"\n?", text)
            require(match is not None, "invalid checksum document for " + name)
            require(digest(staging / name, "sha512") == match[1].lower(), "SHA-512 mismatch for " + name)
        subprocess.run(["sh", package_check] + [str(staging / name) for name in packages], check=True)
        require(tag_commit(base, tag) == commit, "the release tag moved while downloading the assets")
        require(release_identity(api(release_endpoint), tag, version, commit) == identity and
                release_assets(base, release_id, expected) == assets,
                "the GitHub prerelease changed while downloading the assets")
        latest = api(base + "/actions/runs/" + run_id)
        require(latest.get("status") == "completed" and latest.get("conclusion") == "success" and
                latest.get("head_sha") == commit and latest.get("run_attempt") == run.get("run_attempt"),
                "the CI run changed while downloading the assets")
        empty_destination(output)
        if output.exists():
            output.rmdir()
        os.rename(staging, output)
    print(f"ci-binaries: verified {len(packages)} packages from https://github.com/{repository}/actions/runs/{run_id}")
    print(f"ci-binaries: https://github.com/{repository}/releases/tag/{tag}, release {release_id}, attempt {build_attempt}, commit {commit}")
    for name, (asset_id, _, checksum) in sorted(assets.items()):
        print(f"ci-binaries: asset {asset_id}, {checksum}, {name}")


try:
    main()
except (ValueError, KeyError, TypeError, OSError, subprocess.CalledProcessError) as error:
    print("ci-binaries: " + str(error), file=sys.stderr)
    sys.exit(1)
PY
