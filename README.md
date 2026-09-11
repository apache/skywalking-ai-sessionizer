# SkyWalking AI Sessionizer

<img src="https://skywalking.apache.org/assets/logo.svg" alt="SkyWalking logo" height="90px" align="right"/>

[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](https://www.apache.org/licenses/LICENSE-2.0)

**Conversation-level observability, measurement, and export for long-lived AI agents.**

SkyWalking AI Sessionizer assembles fragmented agent telemetry into one durable conversation structure. It
preserves sessions as source provenance, keeps parent and child-agent execution lineages separate,
measures model-message continuity, and projects the same committed snapshot into storage, export and
a local preview.

## Why

Agent runtimes record a single user-visible conversation as many unrelated artifacts — transcripts,
traces, logs, provider request bodies, tool events, subagent metadata. That conversation can outlive
one process, reactivate after a long idle period, and contain several concurrent agent lineages.

Trace-level inspection alone cannot answer: what did the whole conversation do; which input, model
response, tool result or child-agent result led to the next model call; did that call preserve the
previous message history or start a new context; which agents contributed; and what should be
measured, stored and exported as one unit.

## Model

```text
Conversation                          durable identity · ownership boundary
 └ Segment                            activity window · the COMMIT unit
    └ Session                         observed source provenance
       ├ ExecutionStream  main        ordered parent-agent lineage
       │  └ Context epoch × N         model-context lifetime
       │     └ Talk × N               one readable input → run → output
       │        └ Run → Step × N
       └ ExecutionStream  child × N   independent context per child agent
```

Two boundaries carry the design. **Conversation** is the durable aggregation and ownership boundary,
and its identity is supplied — never inferred from a person, an account, or timestamp proximity.
**ExecutionStream** is the ordered continuity boundary; model-message continuity is evaluated within
one stream and one context epoch, never across them.

See the [Unified Conversation Model](docs/en/concepts-and-designs/unified-conversation-model.md).

## Evidence discipline

Nothing is presented as observed unless it was observed.

Every claim carries a qualification (`observed_replayable`, `observed_report_only`, `proposed`,
`unavailable`) and every correlation carries a resolution state (`exact_unique`, `exact_ambiguous`,
`strong_inference`, `unresolved`, `conflict`). An exact identifier with several candidates stays
ambiguous — the assembler never silently chooses one. Where a runtime cannot supply something, the
adapter reports it as `unavailable` rather than approximating it.

## Adapters

| Runtime | Status | Collection |
| --- | --- | --- |
| [Claude Code](docs/en/adapters/claude-code.md) | collection implemented | local files — no configuration required, and it works on history that already exists. The [plugin](docs/en/setup/claude-code-plugin.md) adds which files each shell command changed. The [receiver](docs/en/setup/configuration.md#the-receiver-adapter), `claude-code-otlp`, is off by default. When Claude Code's own OpenTelemetry exporter is pointed at it, it lands the exporter's metrics requests. It drops the exporter's logs and traces. |
| Codex | planned | — |
| LangChain / LangGraph | planned | — |

## Quick start

From 0.3.0 on, each release ships a signed binary package for macOS, Linux and Windows, on ARM 64
and x86-64. [Install](docs/en/setup/install.md) says where to download them and how to verify
them. Or build from a checkout, as the [quick start](docs/en/setup/quick-start.md) does:

```sh
make build                 # builds ./bin/asz
./bin/asz sources          # list discovered sessions and their sources
./bin/asz collect -once    # land, parse and send everything currently on disk
./bin/asz server           # keep doing that, and serve at http://127.0.0.1:8787
```

Every command reads [`asz.yaml`](asz.yaml) from the working directory when no `-config` flag is
given. The file at the repository root is the default configuration with every value written out,
so it can be read and edited without reading Go.

`asz collect` is the pipeline: every period it lands what is new, parses what moved, and sends
what `export.otlp` asks for. `asz server` runs that pipeline and serves the page in one process,
and its list page shows when the data was last refreshed and when it will be next. `asz view`
serves an existing storage root and only reads, which is what a root copied from another machine
or filled by the receiver needs.

## Documentation

Official documentation lives in [`docs/`](docs/) and is indexed by
[`docs/menu.yml`](docs/menu.yml): concepts and designs, setup, the data formats, adapters, guides
and the changelog. It is published at
[skywalking.apache.org/docs/skywalking-ai-sessionizer](https://skywalking.apache.org/docs/skywalking-ai-sessionizer/next/readme/).

## Contributing

Early contributions should focus on schemas, privacy-safe fixtures, deterministic assembly,
qualification rules and golden tests. Please avoid adding inferred identities or causal edges that
cannot retain their source evidence and resolution state.

## License

[Apache License 2.0](LICENSE).

Apache SkyWalking, SkyWalking, and the Apache feather logo are trademarks of The Apache Software Foundation.

## Container image

CI publishes a Linux image for amd64 and arm64 to GHCR. It carries the `asz`
binary and its license files. Docker Desktop on Windows runs it as a Linux
container. Without Docker, use the Windows binary package from
[Install](docs/en/setup/install.md). Mount a storage root at `/asz/data`. The
default command is `server`, which collects and serves. With no source
mounted, ask for `view`, which only reads:

```sh
docker run --rm -p 8787:8787 -v "$PWD/data:/asz/data" \
  ghcr.io/apache/skywalking-ai-sessionizer:latest view 0.0.0.0:8787
```

| Tag | Points at |
| --- | --- |
| `<version>`, such as `0.2.0` | the image built from the git tag `v<version>`, moved only by a run started by hand for that tag |
| `latest` | the highest version tag, once its GitHub release is created or a run started by hand publishes it |
| `main` | the development head |
| `<commit id>` | one commit, by its complete id, moved only by a run started by hand for a tag on it |

A git tag `v*` names a release candidate, and pushing one publishes nothing.
The GitHub release of a version is created after the Apache vote passes, and
creating it publishes the image under that version. The image is a
convenience, not part of the Apache release. Any `asz` command runs the same
way: put it after the image name. See
[Container Image](docs/en/setup/container-image.md). `make docker` builds the
image locally.
