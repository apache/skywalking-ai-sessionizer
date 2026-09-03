# SkyWalking AI Sessionizer

<img src="https://skywalking.apache.org/assets/logo.svg" alt="SkyWalking logo" height="90px" align="right"/>

[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](https://www.apache.org/licenses/LICENSE-2.0)

**Conversation-level observability, measurement, and export for long-lived AI agents.**

> **Status:** pre-alpha. The conversation model and the Claude Code data mapping are defined and
> evidence-backed. Collection and assembly are implemented. The local page that serves the
> assembled conversations is in progress; static export and OTLP push are not started.

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
| [Claude Code](docs/en/adapters/claude-code.md) | collection implemented | local files — no configuration required, and it works on history that already exists |
| Codex | planned | — |
| LangChain / LangGraph | planned | — |

## Quick start

Releases ship a binary package for macOS, Linux and Windows; see the
[quick start](docs/en/setup/quick-start.md). Or build it:

```sh
make build                 # builds ./bin/asz
./bin/asz sources          # list discovered sessions and their sources
./bin/asz collect -once    # land everything currently on disk
./bin/asz view             # serve the conversations at http://127.0.0.1:8787
```

Every command reads [`asz.yaml`](asz.yaml) from the working directory when no `-config` flag is
given. The file at the repository root is the default configuration with every value written out,
so it can be read and edited without reading Go.

`asz view` serves what has been assembled. With the local Claude Code adapter it also runs the
collector and the parser in the same process, once with `-once` or on the collector interval
otherwise, and the list page shows when the data was last refreshed and when it will be next. On a
storage root copied from another machine there is no local source, so it serves what is there.

## Documentation

Official documentation lives in [`docs/`](docs/) and is indexed by
[`docs/menu.yml`](docs/menu.yml): concepts and designs, setup, the data formats, adapters, guides
and changes. It is published at
[skywalking.apache.org/docs/skywalking-ai-sessionizer](https://skywalking.apache.org/docs/skywalking-ai-sessionizer/next/readme/).

[`design-notes/`](design-notes/) holds working engineering notes — measurements, corrections and open
questions produced while designing against real runtime data. They are deliberately unpolished and
are **not** part of the published documentation.

## Contributing

Early contributions should focus on schemas, privacy-safe fixtures, deterministic assembly,
qualification rules and golden tests. Please avoid adding inferred identities or causal edges that
cannot retain their source evidence and resolution state.

## License

[Apache License 2.0](LICENSE).

Apache SkyWalking, SkyWalking, and the Apache feather logo are trademarks of The Apache Software Foundation.

## Container image

CI publishes a Linux image for amd64 and arm64 to GHCR. It carries the `asz`
binary only. Docker Desktop on Windows runs it as a Linux container; the
Windows binary package is the path without Docker. Mount a storage root at
`/asz/data`; by default the container serves the page on port 8787.

```sh
docker run --rm -p 8787:8787 -v "$PWD/data:/asz/data" \
  ghcr.io/apache/skywalking-ai-sessionizer:latest
```

| Tag | Points at |
| --- | --- |
| `0.1.0` | that release, never moved |
| `0.1` | the newest patch of that line |
| `latest` | the newest release |
| `main` | the development head |
| `<commit id>` | one commit, by its complete id, never moved |

A `v*` tag is a release candidate until the Apache vote passes, so pushing one
publishes only its commit tag. The version tags are attached when the GitHub
release is published. Any `asz` command runs the same way: put it after the
image name. See [Container Image](docs/en/setup/container-image.md).
`make docker` builds the image locally.
