# Container Image

CI publishes a multi-platform image, `linux/amd64` and `linux/arm64`, to the GitHub container
registry:

```text
ghcr.io/apache/skywalking-ai-sessionizer
```

It carries the `asz` binary and nothing else. The base is distroless, there is no shell, and the
process runs as a non-root user.

The image is a Linux image. On Windows, Docker Desktop runs it as a Linux container, so the same
image and the same commands work there. No native Windows container image is built; on Windows
without Docker, use the binary package from the [quick start](quick-start.md). Its working directory is `/asz`, so the default storage root is
`/asz/data`, which is declared as a volume, and a configuration file placed at `/asz/asz.yaml` is
read the same way it is on a host.

## Tags

| Tag | Points at | Moves |
| --- | --- | --- |
| `0.1.0` | that release | never |
| `0.1` | the newest patch release of that line | on each patch |
| `latest` | the newest release | on each release |
| `main` | the development head | on each push to `main` |
| `<commit id>` | one commit, by its complete 40-character id | never |

A `v*` tag is a release candidate until the Apache vote has passed, and pushing one publishes
nothing. The GitHub release is created after the vote, and releasing it is what publishes the
image under its version. A draft or a prerelease on GitHub publishes nothing either. A pre-release
version such as `0.2.0-rc1`, if one is ever released, gets its own tag and moves nothing else.

Every image carries `org.opencontainers.image.version` and `org.opencontainers.image.revision`
labels, and `asz version` inside it prints the same version: the release version, or the complete
commit id for a build that is not a release.

## Serve a storage root

The default command serves the page on port 8787, listening on every interface because a
container's loopback is not reachable from outside.

```sh
docker run --rm -p 8787:8787 -v "$PWD/data:/asz/data" \
  ghcr.io/apache/skywalking-ai-sessionizer:latest
```

There is no Claude Code inside the container, so the page serves what the storage root holds and
shows no refresh. This is the way to read a storage root that was collected elsewhere.

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
and no third-party action. The version is passed as a build argument; `make docker` passes the
nearest git tag.
