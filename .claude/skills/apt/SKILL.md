---
name: apt
description: Add released versions of Apache SkyWalking AI Sessionizer to its apt repository, static/apt in apache/skywalking-website, which the website serves at https://skywalking.apache.org/apt. Verifies the voted .deb packages, adds them to the index with apt-ftparchive, signs the index with the release manager's key, writes the .htaccess redirects, installs through them with apt, and opens a pull request to apache/skywalking-website. Use after a version is published.
user-invocable: true
---

# apt repository

`static/apt` in apache/skywalking-website is served at `https://skywalking.apache.org/apt`. It
holds no package, only files that describe them:

- `dists/stable/main/binary-amd64/Packages` and `binary-arm64/Packages`, each with `Packages.gz`.
  They list every released version of each package. The recorder's package is `asz-claude-code` up
  to 0.4.0 and `asz-changes` from 0.5.0, so a repository that spans the rename lists both. Each
  entry's `Filename` is
  `pool/<package>_<version>_<arch>.deb`.
- `dists/stable/Release`, which names the hashes of the `Packages` files, and its signatures
  `InRelease` and `Release.gpg`, by a key in KEYS.
- `.htaccess`, one redirect for each `pool/` path, to the voted `.deb` on the Apache download sites.
  The newest version goes through the mirrors. Every older one goes to archive.apache.org, which
  keeps every version. apt follows the redirect and checks the file against `Packages`.

Users run:

```sh
sudo apt install asz asz-changes
sudo apt install asz=0.4.0 asz-claude-code=0.4.0   # the recorder's package name before 0.5.0
```

The user names one version or several. Each must be released and on the download site or the
archive. 0.4.0 is the first version with Debian packages. Do this at least one hour after the
version moved to the release directory, so the mirrors have it. It needs docker, gpg, curl and
shasum. It changes nothing but a pull request to apache/skywalking-website.

The index is signed with the user's own key, which must be in
https://downloads.apache.org/skywalking/KEYS. Ask which key to use if the user has not said, and
never sign with a key they did not name. gpg may ask for the passphrase in a window of its own.

A local proxy can break HTTPS to Apache hosts, so the commands below go around it.

## 1. Branch the website

```sh
git -C ~/github/skywalking-website fetch origin
git -C ~/github/skywalking-website worktree add -b apt-<versions> <scratch dir>/website origin/master
APT=<scratch dir>/website/static/apt
W=<scratch dir>/apt
mkdir -p "$W/pool"
```

A version already in the index is never added again, because a published package never changes.
If this prints anything, drop that version and tell the user:

```sh
for v in <versions>; do grep -h "^Filename: pool/.*_${v}_" "$APT"/dists/stable/main/binary-*/Packages 2>/dev/null; done
```

## 2. Download and verify the packages

Each `.deb` must match its voted `.sha512`, and its `.asc` must be a good signature by a key in
KEYS. apt reads KEYS the same way the install page has a person read it, with `gpg --dearmor`.

```sh
curl --noproxy '*' -fsSL -o "$W/KEYS" https://downloads.apache.org/skywalking/KEYS
gpg --dearmor < "$W/KEYS" > "$W/keys.gpg"
# Which packages a version published depends on when it was released.
# 0.5.0 renamed the recorder's package: asz-claude-code up to 0.4.x, and
# asz-changes from 0.5.0. No version publishes both, so asking for both
# fails the download on every version.
packages_of() {
  case "$1" in
    0.1.*|0.2.*|0.3.*|0.4.*) echo "asz asz-claude-code" ;;
    *) echo "asz asz-changes" ;;
  esac
}
for v in <versions>; do
  for p in $(packages_of "$v"); do
    for a in amd64 arm64; do
      f=apache-skywalking-ai-sessionizer-$v-bin-$p-$a.deb
      for g in "$f" "$f.sha512" "$f.asc"; do
        curl --noproxy '*' -fsSL -o "$W/$g" "https://downloads.apache.org/skywalking/ai-sessionizer/$v/$g" ||
          curl --noproxy '*' -fsSL -o "$W/$g" "https://archive.apache.org/dist/skywalking/ai-sessionizer/$v/$g"
      done
      (cd "$W" && shasum -a 512 -c "$f.sha512")
      gpgv --keyring "$W/keys.gpg" "$W/$f.asc" "$W/$f"
      cp "$W/$f" "$W/pool/${p}_${v}_${a}.deb"
    done
  done
done
```

Stop at the first failure and tell the user.

## 3. Add them to the index

`apt-ftparchive`, from Debian's `apt-utils`, writes the entries of the new packages, which are
added to each `Packages` file, and then writes `Release` from all of them. The old signatures no
longer match, so they go.

```sh
mkdir -p "$APT/dists/stable/main/binary-amd64" "$APT/dists/stable/main/binary-arm64"
rm -f "$APT/dists/stable/Release" "$APT/dists/stable/InRelease" "$APT/dists/stable/Release.gpg"
docker run --rm -v "$W:/work" -v "$APT:/apt" debian:stable bash -euc '
  apt-get update -qq && apt-get install -y -qq apt-utils > /dev/null
  cd /work
  for a in amd64 arm64; do
    apt-ftparchive --arch "$a" packages pool >> "/apt/dists/stable/main/binary-$a/Packages"
    gzip -9nc "/apt/dists/stable/main/binary-$a/Packages" > "/apt/dists/stable/main/binary-$a/Packages.gz"
  done
  cd /apt/dists/stable
  apt-ftparchive \
    -o APT::FTPArchive::Release::Origin="Apache SkyWalking" \
    -o APT::FTPArchive::Release::Label="Apache SkyWalking AI Sessionizer" \
    -o APT::FTPArchive::Release::Suite=stable \
    -o APT::FTPArchive::Release::Codename=stable \
    -o APT::FTPArchive::Release::Architectures="amd64 arm64" \
    -o APT::FTPArchive::Release::Components=main \
    release . > /work/Release
  cp /work/Release Release'
grep -c '^Package:' "$APT"/dists/stable/main/binary-*/Packages
```

Each `Packages` file must have two more entries for each version added.

## 4. Write the redirects

`.htaccess` is written again from the `Packages` files, so every entry has exactly one rule, and
the newest version is the one on the mirrors:

```sh
newest=$(sed -n 's/^Version: //p' "$APT"/dists/stable/main/binary-*/Packages | sort -uV | tail -1)
{
  echo "# The apt repository of Apache SkyWalking AI Sessionizer. The apt skill in"
  echo "# apache/skywalking-ai-sessionizer writes this file from the Packages files."
  echo "# apt asks for each package under pool/, and each rule sends it to the voted"
  echo "# file: the newest version through the mirrors, every older one to the archive."
  echo "RewriteEngine On"
  sed -n 's/^Filename: pool\///p' "$APT"/dists/stable/main/binary-*/Packages | sort | while IFS=_ read -r p v a; do
    a=${a%.deb}
    f=apache-skywalking-ai-sessionizer-$v-bin-$p-$a.deb
    if [ "$v" = "$newest" ]; then
      url="https://www.apache.org/dyn/closer.lua/skywalking/ai-sessionizer/$v/$f?action=download"
    else
      url="https://archive.apache.org/dist/skywalking/ai-sessionizer/$v/$f"
    fi
    printf 'RewriteRule ^pool/%s_%s_%s\\.deb$ %s [R=302,L]\n' "$p" "$(printf '%s' "$v" | sed 's/\./\\./g')" "$a" "$url"
  done
} > "$APT/.htaccess"
cat "$APT/.htaccess"
```

## 5. Sign the index

```sh
cd "$APT/dists/stable"
gpg --local-user <key> --clearsign -o InRelease Release
gpg --local-user <key> --armor --detach-sign -o Release.gpg Release
gpgv --keyring "$W/keys.gpg" InRelease
gpgv --keyring "$W/keys.gpg" Release.gpg Release
cd -
```

Both `gpgv` commands must report a good signature. If not, the key is not in KEYS, and apt would
refuse the whole repository: stop and tell the user.

## 6. Install through the redirects

Apache httpd serves the directory with its `.htaccess`, as the website does. apt in Debian and in
Ubuntu then installs the newest version and every older version in the index, following the
redirects to the real Apache sites. Not only the versions added: adding a newer version moves the
one that was newest from the mirrors to the archive, so its redirect changed too.

```sh
docker run --rm httpd:2.4 cat /usr/local/apache2/conf/httpd.conf > "$W/httpd.conf"
perl -0pi -e 's/^#(LoadModule rewrite_module )/$1/m; s/(<Directory "\/usr\/local\/apache2\/htdocs">.*?)AllowOverride None/$1AllowOverride All/s' "$W/httpd.conf"
older=$(sed -n 's/^Version: //p' "$APT"/dists/stable/main/binary-*/Packages | sort -uV | grep -vxF "$newest" | tr '\n' ' ')
docker network create asz-apt
docker run -d --name asz-apt-site --network asz-apt -v "$APT:/usr/local/apache2/htdocs/apt:ro" \
  -v "$W/httpd.conf:/usr/local/apache2/conf/httpd.conf:ro" httpd:2.4
for image in debian:stable ubuntu:24.04; do
  docker run --rm --network asz-apt -v "$W/keys.gpg:/keys.gpg:ro" -e NEWEST="$newest" -e OLDER="$older" "$image" bash -euc '
    # The redirects lead to https, and these images carry no certificates.
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq && apt-get install -y -qq ca-certificates > /dev/null
    install -m 644 /keys.gpg /usr/share/keyrings/apache-skywalking.gpg
    echo "deb [signed-by=/usr/share/keyrings/apache-skywalking.gpg] http://asz-apt-site/apt stable main" > /etc/apt/sources.list.d/apache-skywalking.list
    apt-get update
    apt-get install -y asz asz-changes
    asz version | grep -F "$NEWEST"
    asz-changes version | grep -F "$NEWEST"
    for v in $OLDER; do
      recorder=asz-changes
      case "$v" in 0.1.*|0.2.*|0.3.*|0.4.*) recorder=asz-claude-code ;; esac
      apt-get install -y --allow-downgrades "asz=$v" "$recorder=$v"
      asz version | grep -F "$v"
    done'
done
docker logs asz-apt-site 2>&1 | grep 'GET /apt/pool/'
docker rm -f asz-apt-site
docker network rm asz-apt
```

Each image must install every version, and the log must show a `302` for each package. A download
that fails for the newest version means the mirrors do not have it yet: wait and run this step
again. A certificate error means `ca-certificates` was not installed in the image first.

## 7. Open the pull request to the website

```sh
cd <scratch dir>/website
git status --short
git add static/apt
git commit -m "AI Sessionizer apt repository: add <versions>"
git push -u origin apt-<versions>
gh pr create --repo apache/skywalking-website --base master --title "AI Sessionizer apt repository: add <versions>"
```

Only files under `static/apt/` may change. The commit message and the pull request carry no AI
attribution: no Co-Authored-By line and no "Generated with" line. Say in the pull request which
versions were added, which version apt installs by default, which key signed the index, and that
apt installed every version through the redirects.

## 8. After the merge

The website's CI builds and publishes it on every merge to master. Then users run
`sudo apt update && sudo apt upgrade`. `tools/release/release.sh publish VERSION --remove-old` waits
for this merge, because until then apt fetches the previous version through the mirrors. Tell the
user the pull request link, and remove the worktree and `W` once it is merged.
