# Changes in 0.3.0

> In development, not yet released. `tools/release.sh prepare 0.3.0` removes this note.

## Collection

- A landed record keeps the provider model a call ran on, as `model`, the name the runtime wrote
  on the record. It is what a token count is reported under, so a metric per model can be
  produced from the landed data alone. Records landed before this carry none, and the glossary
  says where the runtime writes it.

## Read
- The conversation page is drawn by Horizon's conversation renderer,
  `@skywalking-horizon-ui/conversation-view`, embedded from a pinned Horizon commit with Horizon's
  themes and fonts, so `asz view` and the SkyWalking UI draw a conversation identically and the
  page needs nothing from the network. The hand-written viewer is gone with the API routes only it
  read; the page reads the `asz.view` document, the glossary, and the landed record behind a step.
  `tools/conversation-view.sh` rebuilds the copy from the pin, and CI fails when it differs.

## Release

- A release publishes the container image under the version the tag names, and under `latest`
  when it is the newest release, and nothing looser. The `major.minor` line tag, `0.2` beside
  `0.2.0`, is no longer published: a reader who pulled it could not tell which release answered.
  The `0.2` tag that 0.2.0 published stays on the registry and points at the same image as `0.2.0`.

## Metrics

- `claude-code-local` with `metrics: true` derives the runtime's own metric family from the
  landed files, `claude_code.token.usage` in phase one, name for name and attribute for attribute
  with Claude Code's OpenTelemetry exporter: one count per call, never per fragment, per minute,
  by type, model, query source and session. The points wait in the storage root's `_metrics/`
  spool and `asz push` sends them over the metrics service, under asz's identity, with the same
  budget and once-only rule as the files. `metrics_lookback`, 24 hours unless set, bounds the
  first derivation over a root with history. Every scenario checks the points on the wire against
  its plan, and the Collector job verifies the tokens that arrive.

- `claude-code-otlp` is a second adapter for the runtime: an OpenTelemetry receiver its own
  exporter is pointed at, gRPC and HTTP with protobuf on one `listen` port. Phase one lands the
  metrics requests it receives in the same spool, bytes as received, for `asz push`; logs and
  traces are accepted and dropped, counted in the status. It runs beside the local adapter under
  `asz collect` and `asz view`. `metrics` may be on for one adapter, never both, and the
  configuration refuses to load otherwise, so the same tokens are never counted twice.
