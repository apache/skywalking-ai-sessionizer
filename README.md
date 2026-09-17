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

Install asz, on macOS or Linux, with a version from the
[downloads page](https://skywalking.apache.org/downloads/):

```sh
VERSION=<version>
curl -fsSL "https://raw.githubusercontent.com/apache/skywalking-ai-sessionizer/v$VERSION/install/asz.sh" | sh -s -- "$VERSION"
```

Then run it in the directory where it should keep its data, and open <http://127.0.0.1:8787>:

```sh
mkdir -p ~/asz && cd ~/asz
asz server
```

[Install](docs/en/setup/install.md) has Windows, Homebrew and the binary packages, and
[Quick Start](docs/en/setup/quick-start.md) the next steps.

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

```sh
docker run --rm -p 8787:8787 -v "$PWD/data:/asz/data" \
  ghcr.io/apache/skywalking-ai-sessionizer:latest view 0.0.0.0:8787
```

See [Container Image](docs/en/setup/container-image.md).
