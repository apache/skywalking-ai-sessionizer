---
name: binary-distribution
description: Put released versions of Apache SkyWalking AI Sessionizer into Homebrew by updating Formula/ on main in apache/skywalking-ai-sessionizer, the tap. Writes asz@VERSION and asz-claude-code@VERSION for each version, moves asz and asz-claude-code to the newest, checks the formulae with Homebrew, and opens the pull request. Use after a version is published, or to add older released versions.
user-invocable: true
---

# Binary distribution: Homebrew

`Formula/` on main is the Homebrew tap. It holds, for every released version,
`asz@VERSION.rb` and `asz-claude-code@VERSION.rb`, and `asz.rb` and
`asz-claude-code.rb` for the newest version. Users install with:

```sh
brew tap apache/skywalking-ai-sessionizer https://github.com/apache/skywalking-ai-sessionizer
brew install apache/skywalking-ai-sessionizer/asz
brew install apache/skywalking-ai-sessionizer/asz-claude-code@0.4.0
```

The user names one version or several. A version must be released: voted,
moved to the download site, and its GitHub release promoted, because the
formulae download the packages from that GitHub release.

## 1. Check the versions are released

Each version must answer 200 on one of the two Apache sites, and its GitHub
release must carry the packages. A local proxy can break HTTPS to Apache hosts,
so these commands go around it:

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
is still a prerelease.

## 2. Branch from main

Work in a worktree so another session's changes in the main clone are left
alone:

```sh
git fetch origin
git worktree add -b brew-<versions> <scratch dir> origin/main
cd <scratch dir>
```

## 3. Write and check the formulae, on macOS

```sh
env -u http_proxy -u https_proxy -u all_proxy tools/homebrew-formula.sh --check <versions>
```

For each version it downloads the six packages and their `.sha512` from the
Apache sites, holds each package to its `.sha512`, and runs the formulae
through `brew style`, `brew audit --strict`, `brew install` and `brew test`,
removing what it installed. Only then does it write `Formula/asz@VERSION.rb`
and `Formula/asz-claude-code@VERSION.rb`, and move `Formula/asz.rb` and
`Formula/asz-claude-code.rb` to the newest version. Adding an older version
never moves them back. With `--from DIR` it reads `DIR/VERSION/` instead, such
as the release manager's `dist/`.

If `asz` or `asz-claude-code` is already installed on this machine, the check
refuses to run rather than replace it. Say so and stop; do not uninstall the
user's own install.

## 4. Look at the change

Read what it printed, and `git status --short`. Only files under `Formula/` may
change.

## 5. Commit and open the pull request

```sh
git add Formula
git commit -m "Homebrew formulae for <versions>"
git push -u origin brew-<versions>
gh pr create --repo apache/skywalking-ai-sessionizer --base main --title "Homebrew formulae for <versions>"
```

The commit message and the pull request carry no AI attribution: no
Co-Authored-By line and no "Generated with" line. Say in the pull request which
versions were added, which version `asz` and `asz-claude-code` now install, and
that the check passed.

## 6. After the merge

Anyone who tapped runs `brew update`, then `brew upgrade asz asz-claude-code`.
Tell the user the pull request link, and remove the worktree once it is merged.
