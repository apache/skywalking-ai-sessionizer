---
name: binary-distribution
description: Put released versions of Apache SkyWalking AI Sessionizer into Homebrew and apt. Homebrew is Formula/ on main in apache/skywalking-ai-sessionizer, the tap; apt is static/apt in apache/skywalking-website, served at https://skywalking.apache.org/apt. Writes and checks the formulae and the signed apt index for each version, and opens the two pull requests. Use after a version is published, or to add older released versions.
user-invocable: true
---

# Binary distribution: Homebrew and apt

The user names one version or several. A version must be released: voted,
moved to the download site, and its GitHub release promoted. Do both parts
unless the user names one.

**Homebrew.** `Formula/` on main in this repository is the tap. It holds, for
every released version, `asz@VERSION.rb` and `asz-claude-code@VERSION.rb`, and
`asz.rb` and `asz-claude-code.rb` for the newest version. The formulae download
the packages from the GitHub release.

```sh
brew tap apache/skywalking-ai-sessionizer https://github.com/apache/skywalking-ai-sessionizer
brew install apache/skywalking-ai-sessionizer/asz
brew install apache/skywalking-ai-sessionizer/asz-claude-code@0.4.0
```

**apt.** `static/apt` in apache/skywalking-website is the repository at
`https://skywalking.apache.org/apt`. It holds the signed index of every released
version and a `.htaccess` that redirects apt to each `.deb`: the newest version
through the mirrors, every older one on archive.apache.org. It holds no package.
0.4.0 is the first version with Debian packages.

```sh
sudo apt install asz asz-claude-code
sudo apt install asz=0.4.0 asz-claude-code=0.4.0
```

A local proxy can break HTTPS to Apache hosts, so every command below that
reaches them goes around it.

## 1. Check the versions are released

Each version must answer 200 on one of the two Apache sites, and its GitHub
release must carry the packages:

```sh
for v in <versions>; do
  for u in "https://downloads.apache.org/skywalking/ai-sessionizer/$v/" "https://archive.apache.org/dist/skywalking/ai-sessionizer/$v/"; do
    curl --noproxy '*' -s -o /dev/null -w "$v $u %{http_code}\n" "$u"
  done
  gh release view "v$v" --repo apache/skywalking-ai-sessionizer --json isPrerelease,assets \
    --jq '"v'"$v"' prerelease=\(.isPrerelease) assets=\(.assets|length)"'
done
```

Stop and tell the user about any version that is not released, or whose release
is still a prerelease. A version before 0.4.0 goes to Homebrew only.

## 2. Branch

Work in worktrees, so another session's changes in the main clones are left
alone:

```sh
git fetch origin
git worktree add -b dist-<versions> <scratch dir>/asz origin/main
git -C ~/github/skywalking-website fetch origin
git -C ~/github/skywalking-website worktree add -b apt-<versions> <scratch dir>/website origin/master
cd <scratch dir>/asz
```

## 3. Homebrew: write and check the formulae, on macOS

```sh
env -u http_proxy -u https_proxy -u all_proxy tools/release/homebrew-formula.sh --check <versions>
```

For each version it downloads the six binary packages and their `.sha512` from
the Apache sites, holds each package to its `.sha512`, and runs the formulae
through `brew style`, `brew audit --strict`, `brew install` and `brew test`,
removing what it installed. Only then does it write `Formula/asz@VERSION.rb`
and `Formula/asz-claude-code@VERSION.rb`, and move `Formula/asz.rb` and
`Formula/asz-claude-code.rb` to the newest version. Adding an older version
never moves them back. With `--from DIR` it reads `DIR/VERSION/` instead, such
as the release manager's `dist/`.

If `asz` or `asz-claude-code` is already installed on this machine, the check
refuses to run rather than replace it. Say so and stop; do not uninstall the
user's own install.

## 4. apt: write, check and sign the index

The index is signed with the user's own key, which must be in
https://downloads.apache.org/skywalking/KEYS. Ask the user which key to use if
they have not said, and never sign with a key they did not name. gpg may ask for
the passphrase in a window of its own; tell the user to expect it.

```sh
env -u http_proxy -u https_proxy -u all_proxy GPG_USER=<key> \
  tools/release/apt-repository.sh --check <scratch dir>/website/static/apt <versions>
```

For each version it downloads the four `.deb` packages with their `.sha512` and
`.asc`, holds each to its `.sha512` and its signature to KEYS, and, with
`--check`, installs them with apt in Debian and Ubuntu in docker, through Apache
httpd serving the same kind of index and redirects. Only then does it add them to
the index, write the redirects, sign `dists/stable/Release` into `InRelease` and
`Release.gpg`, and check both signatures against KEYS as apt would read it.
Adding an older version keeps the newest version on the mirrors. A version
already in the index with the same bytes changes nothing, and one with other
bytes is refused.

`--check` needs docker. If docker is not running, say so and ask the user
whether to start it or to go on without `--check`. CI's `apt` job runs the same
check on every change.

## 5. Look at the changes

Read what both scripts printed. In the asz worktree, `git status --short` may
show changes under `Formula/` only. In the website worktree, only under
`static/apt/`: `.htaccess`, `dists/stable/Release`, `InRelease`, `Release.gpg`,
and `Packages` and `Packages.gz` for amd64 and arm64. Read `.htaccess`: the
newest version goes to `closer.lua`, and every other to archive.apache.org.

## 6. Commit and open the pull requests

```sh
git add Formula
git commit -m "Homebrew formulae for <versions>"
git push -u origin dist-<versions>
gh pr create --repo apache/skywalking-ai-sessionizer --base main --title "Homebrew formulae for <versions>"

cd <scratch dir>/website
git add static/apt
git commit -m "AI Sessionizer apt repository: add <versions>"
git push -u origin apt-<versions>
gh pr create --repo apache/skywalking-website --base master --title "AI Sessionizer apt repository: add <versions>"
```

The commit messages and the pull requests carry no AI attribution: no
Co-Authored-By line and no "Generated with" line. Say in each pull request which
versions were added and that the check passed. In the Homebrew one, say which
version `asz` and `asz-claude-code` now install. In the apt one, say which
version apt installs by default, and which key signed the index.

## 7. After the merges

Anyone who tapped runs `brew update`, then `brew upgrade asz asz-claude-code`.
apt users run `sudo apt update && sudo apt upgrade` once the website has been
built, which its CI does on every merge to master.

`tools/release/release.sh publish VERSION --remove-old` removes older versions from the
download site. It must wait for the apt pull request, because until it merges
apt downloads the previous version through the mirrors. Tell the user the pull
request links, and remove both worktrees once they are merged.
