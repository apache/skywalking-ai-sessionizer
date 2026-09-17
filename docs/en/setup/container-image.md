# Container Image

```text
ghcr.io/apache/skywalking-ai-sessionizer
```

A Linux image for `amd64` and `arm64`, which Docker Desktop also runs on macOS and Windows. It has
no shell and runs as a non-root user. The storage root is `/asz/data`, and a configuration file at
`/asz/asz.yaml` is read.

## Tags

| Tag | Image |
| --- | --- |
| `latest` | the latest release |
| `<version>` | that release, from the [downloads page](https://skywalking.apache.org/downloads/) |
| `main` | the development head |

## Read a storage root

`view` serves the page and only reads:

```sh
docker run --rm -p 8787:8787 -v "$PWD/data:/asz/data" \
  ghcr.io/apache/skywalking-ai-sessionizer:latest view 0.0.0.0:8787
```

Open <http://127.0.0.1:8787>.

## Collect from the host

Mount Claude Code's directory read-only, and point the adapter at it with a configuration file:

```yaml
# asz.yaml
storage:
  root: /asz/data
adapters:
  - name: claude-code-local
    source_root: /claude/projects
```

```sh
docker run --rm -p 8787:8787 \
  -v "$HOME/.claude/projects:/claude/projects:ro" \
  -v "$PWD/asz.yaml:/asz/asz.yaml:ro" \
  -v "$PWD/data:/asz/data" \
  ghcr.io/apache/skywalking-ai-sessionizer:latest
```

The default command, `server`, collects every 10 minutes and serves the page. On Linux, add
`--user "$(id -u):$(id -g)"` so the container can write to `./data`.

## Run any command

Put the command after the image name:

```sh
docker run --rm -v "$PWD/data:/asz/data" ghcr.io/apache/skywalking-ai-sessionizer:latest verify
docker run --rm ghcr.io/apache/skywalking-ai-sessionizer:latest version
```
