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
# Usage: tools/release/ci-binaries.sh VERSION COMMIT RUN_ID OUT_DIR "OS/ARCH ..." ["DEB_PACKAGES"]
# OUT_DIR must be new or empty. PLATFORMS and DEB_PACKAGES come from the
# release tag's Makefile.
# An empty RUN_ID or auto uses the run recorded by CI after uploading its files.
# Only GitHub API reads are made. No package is installed before its run,
# release identity, asset digests, checksums and archive checks have passed.
set -euo pipefail
[ "$#" -eq 5 ] || [ "$#" -eq 6 ] || { echo 'usage: tools/release/ci-binaries.sh VERSION COMMIT RUN_ID OUT_DIR "OS/ARCH ..." ["DEB_PACKAGES"]' >&2; exit 2; }
[ "$#" -eq 6 ] || set -- "$@" ""
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
    markers = re.findall(r"<!-- asz-ci-binaries run_id=([1-9][0-9]*) run_attempt=([1-9][0-9]*) commit=([0-9a-f]{40}) assets=([0-9a-f]{64}) -->",
                         release.get("body") or "")
    require(len(markers) == 1 and markers[0][2] == commit,
            "the prerelease is not ready: wait for CI to upload and verify all binary assets")
    return release["id"], markers[0]


# ci-upload-binaries.sh writes the same fingerprint into the readiness marker
# after it has verified the uploaded bytes. A marker copied from another run,
# or files uploaded again by hand, then no longer match.
def asset_fingerprint(assets):
    lines = sorted(f"{name} {asset_id} {size} {checksum}" for name, (asset_id, size, checksum) in assets.items())
    return hashlib.sha256(("\n".join(lines) + "\n").encode("utf-8")).hexdigest()


def release_assets(base, release_id, expected, signed, run=None):
    pages = api(base + f"/releases/{release_id}/assets?per_page=100", pages=True)
    require(isinstance(pages, list) and all(isinstance(page, list) for page in pages),
            "invalid release asset page response")
    every = [asset for page in pages for asset in page]
    names = [asset.get("name") for asset in every]
    # candidate attaches the source package and the signatures beside CI's
    # files, so a later run of candidate finds them. Nothing else may be there.
    require(len(names) == len(set(names)) and expected <= set(names) and set(names) <= expected | signed,
            "the prerelease must contain exactly the expected packages and their .sha512 files, and at most the candidate's source package and signatures; wait for CI or remove the rejected prerelease before another candidate")
    assets = [asset for asset in every if asset.get("name") in expected]
    require(len({asset.get("id") for asset in every}) == len(every), "duplicate release asset IDs")
    for asset in assets:
        require(type(asset.get("id")) is int and asset["id"] > 0 and asset.get("state") == "uploaded" and
                type(asset.get("size")) is int and asset["size"] > 0,
                "the release asset is not fully uploaded: " + asset["name"])
        require(re.fullmatch(r"sha256:[0-9a-f]{64}", asset.get("digest") or ""),
                "the release asset has no SHA-256 digest from GitHub: " + asset["name"])
        # A person with write access uploads as themselves. The workflow token
        # uploads as this bot, and only while the run named by the marker ran.
        uploader = asset.get("uploader") or {}
        require(uploader.get("login") == "github-actions[bot]" and uploader.get("type") == "Bot",
                "the release asset was not uploaded by CI: " + asset["name"])
        if run is not None:
            created = asset.get("created_at") or ""
            require(re.fullmatch(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z", created) and
                    run["run_started_at"] <= created <= run["updated_at"],
                    "the release asset was not uploaded during the CI run: " + asset["name"])
    return {asset["name"]: (asset["id"], asset["size"], asset["digest"]) for asset in assets}


def main():
    version, commit, run_id, directory, platform_text, deb_text, package_check = sys.argv[1:]
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
    # DEB_PACKAGES in the tag's Makefile names the Debian packages, one for
    # each Linux platform. A tag from before them passes none.
    debs = deb_text.split()
    require(len(debs) == len(set(debs)) and set(debs) <= {"asz", "asz-claude-code"},
            "DEB_PACKAGES must name distinct packages among asz and asz-claude-code")
    for name in debs:
        for platform in platforms:
            os_name, arch = platform.split("/")
            if os_name == "linux":
                packages.append(f"apache-skywalking-ai-sessionizer-{version}-bin-{name}-{arch}.deb")
    expected = set(packages + [name + ".sha512" for name in packages])
    source = f"apache-skywalking-ai-sessionizer-{version}-src.tgz"
    signed = {source, source + ".sha512"} | {name + ".asc" for name in packages + [source]}
    output = Path(directory).absolute()
    empty_destination(output)
    repository = "apache/skywalking-ai-sessionizer"
    base = "repos/" + repository
    tag = "v" + version
    require(tag_commit(base, tag) == commit, f"{tag} does not point at COMMIT in {repository}")
    release_endpoint = base + "/releases/tags/" + tag
    identity = release_identity(api(release_endpoint), tag, version, commit)
    release_id, (build_run, build_attempt, _, build_assets) = identity
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
            "the CI run must run on the release tag at COMMIT: " + tag)
    # Only the push of the tag starts the job that creates the prerelease and
    # uploads. A manual run of the workflow never attaches files, so it
    # cannot vouch for them.
    require(run.get("event") == "push", "the CI run is not the build of the tag push")
    require(run.get("workflow_id") == workflow.get("id") and
            run.get("path", "").split("@", 1)[0] == workflow["path"],
            "the run is not the repository's CI workflow")
    time_format = r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z"
    require(re.fullmatch(time_format, run.get("run_started_at") or "") and
            re.fullmatch(time_format, run.get("updated_at") or ""),
            "the CI run has no start or update time")
    assets = release_assets(base, release_id, expected, signed, run)
    require(asset_fingerprint(assets) == build_assets,
            "the prerelease assets are not the files CI verified; remove the prerelease explicitly and recreate it for a new CI run")
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
                release_assets(base, release_id, expected, signed) == assets,
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
