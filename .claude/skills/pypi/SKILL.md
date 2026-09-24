---
name: pypi
description: Publish a released version of the Apache SkyWalking AI Sessionizer LangChain plugin, apache-skywalking-asz-langchain, to PyPI. Says what a release manager sets up before the first upload, downloads the voted source package, verifies it, builds the source distribution and the wheel from plugins/langchain inside it, checks and installs them, and uploads them with twine. Use after a version is published.
user-invocable: true
---

# PyPI

`apache-skywalking-asz-langchain` is the LangChain plugin under `plugins/langchain`. It is in the
voted source package, and PyPI is a convenience channel written after the vote, as the Homebrew tap
and the apt repository are. Nothing is uploaded for a candidate, and the version uploaded is the
released one, which `release.sh prepare` wrote into `plugins/langchain/pyproject.toml`.

Unlike the Homebrew and apt skills, this one opens no pull request. It uploads, and an upload
cannot be undone: PyPI never accepts a file name twice, even after the file is deleted.

Ask for the version when it is not given. Then:

## 0. Set up the upload

The project `apache-skywalking-asz-langchain` is on PyPI since 0.5.0, whose upload created it. Only
an owner or a maintainer of the project on PyPI can upload. A release manager who is neither asks
an owner to add them.

1. **A PyPI account with two-factor authentication.** PyPI refuses an upload from an account
   without it.
2. **An API token.** A token scoped to a project can only be made once the project exists. So the
   upload that creates a project needs a token scoped to the whole account, as 0.5.0 did. Every
   later upload uses a token scoped to `apache-skywalking-asz-langchain`. After the first upload,
   replace the account token with a project token, and delete the account token.
3. **The token in `SW_PYPI_TOKEN`,** exported from the shell profile, such as `~/.zprofile`,
   before Claude Code starts. When the 0.5.0 token was added to the profile during a session, the
   session did not see it until Claude Code was started again. Reading it out of the profile from
   inside the session was refused by the permission check of auto mode.

Check it without printing it. Stop and tell the user when it is not set:

```sh
[ -n "$SW_PYPI_TOKEN" ] && echo "SW_PYPI_TOKEN is set, ${#SW_PYPI_TOKEN} characters"
```

Never print the token, and never write it into a file.

## 1. Download the voted source package and verify it

The newest version is on downloads.apache.org minutes after the move. archive.apache.org holds every
version, but gets a new one later: for 0.5.0 it still answered 404 fifteen minutes after the move,
and 200 when checked again four hours later. So try the download site first.

```sh
v=VERSION
pkg=apache-skywalking-ai-sessionizer-$v-src.tgz
W=$(mktemp -d); cd "$W"
for f in "$pkg" "$pkg.sha512" "$pkg.asc"; do
  curl --noproxy '*' -fsSLO "https://downloads.apache.org/skywalking/ai-sessionizer/$v/$f" ||
    curl --noproxy '*' -fsSLO "https://archive.apache.org/dist/skywalking/ai-sessionizer/$v/$f"
done
curl --noproxy '*' -fsSL -o KEYS https://downloads.apache.org/skywalking/KEYS
gpg --dearmor < KEYS > keys.gpg
shasum -a 512 -c "$pkg.sha512" && gpgv --keyring "$W/keys.gpg" "$pkg.asc" "$pkg"
tar -xzf "$pkg"
```

`gpgv` reads KEYS from a file of its own, so the user's keyring is left alone. `gpg --import` into
a scratch directory under a long path hung on macOS, waiting to start `gpg-agent`. A local proxy can
break HTTPS to Apache hosts, which is why the commands go around it.

Stop if either check fails. Nothing is built from a package that did not verify.

## 2. Build from the package, not from a checkout

A checkout can hold what the vote did not.

```sh
python3 -m venv build-env && . build-env/bin/activate
pip install --quiet build twine
python -m build --outdir dist "apache-skywalking-ai-sessionizer-$v-src/plugins/langchain"
twine check dist/*
```

`dist/` holds `apache_skywalking_asz_langchain-$v.tar.gz` and
`apache_skywalking_asz_langchain-$v-py3-none-any.whl`. Check the version in the file names is
`$v`, and that `unzip -l dist/*.whl` lists `LICENSE` and `NOTICE` under the `.dist-info`
directory.

## 3. Install the wheel where nothing else is, and run it

`asz-langchain status` exits 1 while the plugin is not enabled and 0 while it is, so the checks
below say which exit code they expect. `enable` writes only into the environment's own site
directory.

```sh
python3 -m venv try-env && . try-env/bin/activate
pip install --quiet dist/*.whl
asz-langchain status && { echo "enabled before enable"; exit 1; }
asz-langchain enable && asz-langchain status || { echo "enable did not take"; exit 1; }
asz-langchain disable && ! asz-langchain status || { echo "disable did not take"; exit 1; }
deactivate
```

`status` must say "not enabled", then "enabled at ...", then "not enabled" again. Stop if a
line prints its message.

## 4. Upload

```sh
. build-env/bin/activate
TWINE_USERNAME=__token__ TWINE_PASSWORD="${SW_PYPI_TOKEN:?SW_PYPI_TOKEN is not set}" \
  twine upload --non-interactive dist/*
```

`--non-interactive` makes a missing or refused token fail, rather than wait for a password prompt
that nobody sees. To correct a version once it is uploaded, release the next version.

## 5. Check it landed

PyPI must hold the two files that were built, byte for byte, and they must install. Run this in
the directory of the upload: a build run again from the same source gives other bytes. Measured on
0.5.0, a rebuilt wheel held the same files with the same bytes as the uploaded one, and differed
only in the time recorded for the files the build writes.

```sh
curl -fsSL "https://pypi.org/pypi/apache-skywalking-asz-langchain/$v/json" |
  python3 -c 'import json, sys; [print(f["digests"]["sha256"] + "  " + f["filename"]) for f in json.load(sys.stdin)["urls"]]'
shasum -a 256 dist/*
python3 -m venv pypi-env && . pypi-env/bin/activate
pip install --quiet --no-cache-dir "apache-skywalking-asz-langchain==$v"
pip index versions apache-skywalking-asz-langchain
asz-langchain status; echo "exit $? (1: installed, not enabled)"
deactivate
```

Then say what was uploaded and where, and clean up `$W`. After an upload made with a token scoped to
the whole account, remind the user to replace it with a token scoped to the project.
