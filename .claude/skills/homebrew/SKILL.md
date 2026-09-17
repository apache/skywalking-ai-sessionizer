---
name: homebrew
description: Add released versions of Apache SkyWalking AI Sessionizer to its Homebrew tap, Formula/ on main in apache/skywalking-ai-sessionizer. Writes asz@VERSION and asz-claude-code@VERSION from the voted packages, moves asz and asz-claude-code to the newest version, checks every formula with brew, and opens a pull request to main in this repository. Use after a version is published, or to add older released versions.
user-invocable: true
---

# Homebrew tap

`Formula/` on main in this repository is the tap. It holds:

- `asz@VERSION.rb` and `asz-claude-code@VERSION.rb` for every released version, keg-only, so an
  exact version stays installable;
- `asz.rb` and `asz-claude-code.rb` for the newest version.

`asz.rb` and `asz-claude-code.rb` beside this file are the templates. A formula installs the voted
binary package for the machine, macOS or Linux on arm64 or amd64, from the version's GitHub release,
with archive.apache.org as its mirror. That URL keeps working after the version leaves the download
site. Users run:

```sh
brew tap apache/skywalking-ai-sessionizer https://github.com/apache/skywalking-ai-sessionizer
brew install apache/skywalking-ai-sessionizer/asz apache/skywalking-ai-sessionizer/asz-claude-code
brew install apache/skywalking-ai-sessionizer/asz@0.4.0
```

The user names one version or several. Each must be released: voted, on the download site or the
archive, and its GitHub release promoted. 0.4.0 is the first version these templates fit; before
it, the plugin's binary was not beside `asz` in the package. This runs on macOS with Homebrew. It
changes nothing but a pull request to main in this repository.

A local proxy can break HTTPS to Apache hosts, so the commands below go around it.

## 1. Check the versions are released

```sh
for v in <versions>; do
  for u in "https://downloads.apache.org/skywalking/ai-sessionizer/$v/" "https://archive.apache.org/dist/skywalking/ai-sessionizer/$v/"; do
    curl --noproxy '*' -s -o /dev/null -w "$v $u %{http_code}\n" "$u"
  done
  gh release view "v$v" --repo apache/skywalking-ai-sessionizer --json isPrerelease --jq '"v'"$v"' prerelease=\(.isPrerelease)"'
done
```

Each version must answer 200 on one of the two sites, and its GitHub release must not be a
prerelease. Otherwise stop and tell the user.

## 2. Branch from main

Work in a worktree, so changes in the main clone are left alone. `W` is a scratch directory.

```sh
git fetch origin
git worktree add -b homebrew-<versions> <scratch dir>/asz origin/main
cd <scratch dir>/asz
W=<scratch dir>/homebrew
```

## 3. Download and check the packages

A formula names the sha256 of each package, so each must be the voted file, and must hold what the
formula installs. A package that fails here would fail for everyone on its platform.

```sh
for v in <versions>; do
  mkdir -p "$W/$v"
  for p in darwin-arm64 darwin-amd64 linux-arm64 linux-amd64; do
    f=apache-skywalking-ai-sessionizer-$v-bin-$p.tgz
    for g in "$f" "$f.sha512"; do
      curl --noproxy '*' -fsSL -o "$W/$v/$g" "https://downloads.apache.org/skywalking/ai-sessionizer/$v/$g" ||
        curl --noproxy '*' -fsSL -o "$W/$v/$g" "https://archive.apache.org/dist/skywalking/ai-sessionizer/$v/$g"
    done
    (cd "$W/$v" && shasum -a 512 -c "$f.sha512")
    tar -tvzf "$W/$v/$f" | awk '($NF == "asz" || $NF == "asz-claude-plugin") && $1 ~ /^-rwx/ {n++}
      $NF == "LICENSE" || $NF == "NOTICE" {m++} $NF ~ /^licenses\/./ {l = 1}
      END {if (n != 2 || m != 2 || !l) {print "missing a file the formula installs"; exit 1}}'
  done
done
```

Stop at the first failure and tell the user.

## 4. Write the formulae

```sh
T=.claude/skills/homebrew
for v in <versions>; do
  sum() { shasum -a 256 "$W/$v/apache-skywalking-ai-sessionizer-$v-bin-$1.tgz" | cut -d ' ' -f 1; }
  mkdir -p "$W/formula-$v"
  for f in asz asz-claude-code; do
    sed -e "s/@VERSION@/$v/g" \
      -e "s/@DARWIN_ARM64_SHA256@/$(sum darwin-arm64)/" -e "s/@DARWIN_AMD64_SHA256@/$(sum darwin-amd64)/" \
      -e "s/@LINUX_ARM64_SHA256@/$(sum linux-arm64)/" -e "s/@LINUX_AMD64_SHA256@/$(sum linux-amd64)/" \
      "$T/$f.rb" > "$W/formula-$v/$f.rb"
  done
  # Homebrew names a versioned formula's class after its file: asz@0.4.0 is
  # AszAT040. keg_only keeps it from clashing with the current formula.
  for c in Asz:asz AszClaudeCode:asz-claude-code; do
    C=${c%%:*} V=${c%%:*}AT$(printf '%s' "$v" | tr -d .) perl -pe \
      's/^class \Q$ENV{C}\E < Formula$/class $ENV{V} < Formula/; s/^(  license "Apache-2\.0"\n)/$1\n  keg_only :versioned_formula\n/' \
      "$W/formula-$v/${c#*:}.rb" > "$W/formula-$v/${c#*:}@$v.rb"
  done
  grep -n '@[A-Z0-9_]*@' "$W/formula-$v"/*.rb && echo "a placeholder is left"
done
current=""
if [ -f Formula/asz.rb ]; then current=$(sed -nE 's#.*/releases/download/v([0-9.]+)/.*#\1#p' Formula/asz.rb | head -1); fi
newest=$(printf '%s\n' $current <versions> | sort -V | tail -1)
echo "current: ${current:-none}, newest: $newest"
```

`asz.rb` and `asz-claude-code.rb` move only forward: to `$newest`, when it is one of the given
versions. Adding an older version never moves them back.

## 5. Check them with Homebrew

The check installs every new formula from its real GitHub release URL, and removes it afterwards.
It must not touch the user's own install. If any of these is installed, stop, tell the user, and do
not uninstall it:

```sh
for f in asz asz-claude-code $(for v in <versions>; do printf 'asz@%s asz-claude-code@%s ' "$v" "$v"; done); do
  brew list --formula "$f" >/dev/null 2>&1 && echo "$f is installed"
done
```

Then, in a local tap:

```sh
export HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_INSTALL_CLEANUP=1 HOMEBREW_NO_ENV_HINTS=1
tap=aszcheck/formulae
brew tap-new --no-git "$tap"
dir=$(brew --repository "$tap")/Formula
for v in <versions>; do cp "$W/formula-$v/asz@$v.rb" "$W/formula-$v/asz-claude-code@$v.rb" "$dir/"; done
if [ "$newest" != "$current" ]; then cp "$W/formula-$newest/asz.rb" "$W/formula-$newest/asz-claude-code.rb" "$dir/"; fi
env -u http_proxy -u https_proxy -u all_proxy brew style "$tap"
for f in "$dir"/*.rb; do
  n=$tap/$(basename "$f" .rb)
  env -u http_proxy -u https_proxy -u all_proxy brew audit --strict --formula "$n"
  env -u http_proxy -u https_proxy -u all_proxy brew install --formula "$n"
  brew test "$n"
done
for v in <versions>; do
  "$(brew --prefix "asz@$v")/bin/asz" version
  "$(brew --prefix "asz-claude-code@$v")/bin/asz-claude-plugin" version
done
brew info --formula "$tap/asz-claude-code@$newest" | grep 'claude plugin marketplace add'
```

Every step must pass, and each binary must print its version. When `asz.rb` moved, the `asz` and
`asz-claude-plugin` on `$(brew --prefix)/bin` must print `$newest`, and link into the `asz` and
`asz-claude-code` kegs, not a versioned one. Then remove everything the check installed, whether it
passed or not:

```sh
for f in "$dir"/*.rb; do brew uninstall --formula "$tap/$(basename "$f" .rb)"; done
brew untap "$tap"
```

## 6. Put them into Formula/

```sh
mkdir -p Formula
for v in <versions>; do cp "$W/formula-$v/asz@$v.rb" "$W/formula-$v/asz-claude-code@$v.rb" Formula/; done
if [ "$newest" != "$current" ]; then cp "$W/formula-$newest/asz.rb" "$W/formula-$newest/asz-claude-code.rb" Formula/; fi
git status --short
```

Only files under `Formula/` may change.

## 7. Open the pull request to main

```sh
git add Formula
git commit -m "Homebrew formulae for <versions>"
git push -u origin homebrew-<versions>
gh pr create --repo apache/skywalking-ai-sessionizer --base main --title "Homebrew formulae for <versions>"
```

The commit message and the pull request carry no AI attribution: no Co-Authored-By line and no
"Generated with" line. Say in the pull request which versions were added, which version `asz` and
`asz-claude-code` install now, and that the check passed.

## 8. After the merge

Anyone who tapped runs `brew update`, then `brew upgrade asz asz-claude-code`. Tell the user the
pull request link, and remove the worktree and `W` once it is merged.
