---
name: pypi
description: Publish a released version of the Apache SkyWalking AI Sessionizer LangChain plugin, apache-skywalking-asz-langchain, to PyPI. Downloads the voted source package, verifies it, builds the sdist and the wheel from plugins/langchain inside it, checks and installs them, and uploads them with twine. Use after a version is published.
user-invocable: true
---

# PyPI

`apache-skywalking-asz-langchain` is the LangChain plugin under `plugins/langchain`. It is in the
voted source package, and PyPI is a convenience channel written after the vote, as the Homebrew tap
and the apt repository are. Nothing is uploaded for a candidate, and the version uploaded is the
released one, which `release.sh prepare` wrote into `plugins/langchain/pyproject.toml`.

Ask for the version when it is not given. Then:

1. **Download the voted source package and verify it.** Every released version is on
   archive.apache.org; the newest is on downloads.apache.org too.

   ```sh
   v=VERSION
   pkg=apache-skywalking-ai-sessionizer-$v-src.tgz
   base=https://archive.apache.org/dist/skywalking/ai-sessionizer/$v
   W=$(mktemp -d); cd "$W"
   curl -fsSLO "$base/$pkg" && curl -fsSLO "$base/$pkg.sha512" && curl -fsSLO "$base/$pkg.asc"
   curl -fsSL https://downloads.apache.org/skywalking/KEYS | gpg --import
   shasum -a 512 -c "$pkg.sha512" && gpg --verify "$pkg.asc" "$pkg"
   tar -xzf "$pkg"
   ```

   Stop if either check fails. Nothing is built from a package that did not verify.

2. **Build from the package, not from a checkout.** A checkout can hold what the vote did not.

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

3. **Install the wheel where nothing else is, and run it.** `asz-langchain status` exits 1 while
   the plugin is not enabled and 0 while it is, so the checks below say which exit code they
   expect.

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

4. **Upload.** The project's PyPI account owns the name; the release manager has a token for it.

   ```sh
   . build-env/bin/activate
   TWINE_USERNAME=__token__ TWINE_PASSWORD="$PYPI_TOKEN" twine upload dist/*
   ```

   PyPI refuses a file name it already holds, so a version is uploaded once; to correct one,
   release the next version.

5. **Check it landed.** `pip index versions apache-skywalking-asz-langchain` lists `$v`, and
   `pip install apache-skywalking-asz-langchain==$v` in a fresh environment installs it.

Then say what was uploaded and where, and clean up `$W`.
