# Container Image

CI publishes a multi-platform image, `linux/amd64` and `linux/arm64`, to the GitHub container
registry:

```text
ghcr.io/apache/skywalking-ai-sessionizer
```

It carries the `asz` binary, and under `/licenses` the license files a binary distribution must
carry. The base is distroless, there is no shell, and the process runs as a non-root user. Its
working directory is `/asz`, so the default storage root is `/asz/data`, which is declared as a
volume, and a configuration file placed at `/asz/asz.yaml` is read the same way it is on a host.

The image is a Linux image. On Windows, Docker Desktop runs it as a Linux container, so the same
image and the same commands work there. Without Docker, use the Windows binary package that
[Install](install.md#binary-package) describes.

The image is a convenience, not part of the Apache release, and how it is published for a released
version is not settled yet. Today the GitHub release of a version publishes it, as the tags below
say.

## Tags

| Tag | Points at | Moves |
| --- | --- | --- |
| `<version>`, such as `0.2.0` | the image built from the git tag `v<version>` | only when a run started by hand publishes that tag again |
| `latest` | the highest version tag | when the GitHub release of that tag is created, or a run started by hand publishes it |
| `main` | the development head | on each push to `main` |
| `<commit id>` | one commit, by its complete 40-character id | only when a run started by hand publishes a tag on that commit again |

The tags `0.1` and `0.2` remain from an earlier workflow, and no new `MAJOR.MINOR` tag is made,
because a reader who pulls one cannot tell which version answered.

A git tag `v*` names a release candidate, and pushing one publishes nothing. The GitHub release of
a version is created after the Apache vote has passed, and creating it publishes the image under
that version. A draft or a prerelease on GitHub publishes nothing. A version with a suffix, such as
`0.2.0-rc1`, is published under its own version tag and under its commit id, because CI tags every
build with its commit id. It moves no floating tag, so `latest` stays where it is.

The images `0.1.0` and `0.2.0` were built from the tags of versions released on GitHub before the
project's first Apache vote. Neither is the image of an Apache release.

Every image CI publishes carries `org.opencontainers.image.version` and
`org.opencontainers.image.revision` labels, and `asz version` inside it prints the same version.
The version is the one its git tag names when a GitHub release, or a run started by hand for that
tag, published the image. For a push to `main` it is the complete commit id. The revision label is
always the complete commit id.

## Serve a storage root

`view` serves the page on port 8787, listening on every interface because a container's loopback
is not reachable from outside. It only reads.

```sh
docker run --rm -p 8787:8787 -v "$PWD/data:/asz/data" \
  ghcr.io/apache/skywalking-ai-sessionizer:latest view 0.0.0.0:8787
```

This is the way to read a storage root that was collected elsewhere. The image's default command
is `server`, which serves the same page and also collects; use `view` when nothing is mounted for
it to collect from.

## Collect from the host

To collect inside the container, mount Claude Code's directory read-only and tell the adapter
where it is. Put a configuration file beside the storage root:

```yaml
# asz.yaml
storage:
  root: /asz/data
adapters:
  - name: claude-code-local
    enabled: true
    source_root: /claude/projects
    exclude:
      - /private/tmp/**
```

A file that lists `adapters` replaces the whole list, so the exclude has to be repeated or Claude
Code's own helper sessions under `/private/tmp` are collected too. Measured on one machine: 44
sessions with the exclude, 64 without.

```sh
docker run --rm -p 8787:8787 \
  -v "$HOME/.claude/projects:/claude/projects:ro" \
  -v "$PWD/asz.yaml:/asz/asz.yaml:ro" \
  -v "$PWD/data:/asz/data" \
  ghcr.io/apache/skywalking-ai-sessionizer:latest
```

The default command is `server`, so this lands, parses and serves on the collector's interval. To
collect without serving a page, put `collect` after the image name.

The container user is not the host user, so on Linux the storage root must be writable by it.
Running with `--user "$(id -u):$(id -g)"` is the simplest way. Docker Desktop on macOS maps
bind mounts for you.

## Run any command

Put the command after the image name. The entrypoint is the binary.

```sh
docker run --rm -v "$PWD/data:/asz/data" ghcr.io/apache/skywalking-ai-sessionizer:latest verify
docker run --rm ghcr.io/apache/skywalking-ai-sessionizer:latest glossary
docker run --rm ghcr.io/apache/skywalking-ai-sessionizer:latest version
```

## Build locally

```sh
make docker          # -> skywalking-ai-sessionizer:dev
```

The Dockerfile cross-compiles from the build host, so a multi-platform build needs no emulation
and no third-party action. The version is passed as a build argument. `make docker` passes the
output of `git describe --tags --always --dirty` without its leading `v`. On a tagged commit that
is the tag, such as `0.3.0`. On any other commit it is the nearest tag, the number of commits since
it and the short commit id, such as `0.3.0-4-g1a2b3c4`. A tree with changes adds `-dirty`.
