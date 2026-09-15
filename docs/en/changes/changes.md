# Changes in 0.4.0

> In development, not yet released. `tools/release.sh prepare 0.4.0` removes this note.

## Provider bodies

- A new adapter, `claude-code-provider`, lands the request and response bodies Claude Code writes
  for its model provider when `OTEL_LOG_RAW_API_BODIES=file:<dir>` is in its environment: the
  system prompt, the tool schemas and every message as it was sent, which no transcript holds. A
  request names its session; a response is claimed by a later request that names its request id,
  or by the one session whose transcript holds its message id, and waits otherwise. On by default,
  it does nothing until the variable is set. See
  [Claude Code Provider Bodies](../setup/claude-code-provider-bodies.md).
- The kind `provider_body`, reserved until now, lands under `<session>/provider_body/`, one file per
  session per pass. A record keeps only what its session does not hold yet: each tool definition
  and every string of 1 KiB or more is a piece named by its SHA-256, and the front a body shares
  with the previous body of its chain is one copy. Its last part, a `provider_body/1` manifest,
  rebuilds the body byte for byte, which the adapter checks through the writer and reader before a
  record lands. On two captures of Claude Code 2.1.260 the landed files were 25.5% and 21.6% of the
  bytes written, and all 42 bodies rebuilt exactly. `pkg/providerbody` cuts and rebuilds bodies for
  any reader.
- Assembly leaves provider bodies out of the fold, and the index keeps them out of every lookup, so
  a session folds to the same nodes with and without them. In the `asz.view` document, an `llm.call`
  lists its request and response under `provider_bodies`, each by role and the landed record it
  rebuilds from, joined by the bodies' own ids; a reader loads the bodies when it wants them.
  `summary.provider_bodies` counts the bodies and `summary.captured_prompts` the calls whose request is
  captured. The version stays `1.0`.
- `asz verify` rebuilds every landed provider body and compares its digest. `storage.LandedFiles`
  lists the session's `provider_body/` directory, so parse, push, repack and the view see the files.
  The root keeps a derived table of the body files, `_provider/seen.state`.
- A scenario with `provider_bodies: true` writes every call's bodies in both formats. The new
  property `provider_bodies_rebuild` reads each body a call lists from only the provider files up to
  its own, as an on-demand reader does, on the root and on its repacked copy.
  `provider_bodies_leave_the_fold` compares the fold without them, and `parts_keep_source_bytes`
  rebuilds every body and compares it with its file. An expectation's `collect.max_delta_bytes` is
  written into the build's configuration, and the check collects by it. A scenario removal waits until
  every body the build wrote is landed, deletes the body files with the session's other files, and
  drops the session's lines from `_provider/seen.state`. `tests/scenarios/provider-bodies.yaml` is the
  example, with bodies cut into five files.

## Collection

- The collector's default `interval` is 10 minutes, in `asz.yaml` and when no configuration is
  given; it was 5 seconds. Every pass that finds new records lands new files, so a period of
  seconds wrote many small files, and each one is a log record when sent. `asz collect` and
  `asz server` still make a pass when they start. A configuration that sets `interval` keeps its
  value. [Configuration](../setup/configuration.md#choosing-an-interval) explains the choice.
- A scenario build writes `interval: 5s` into the configuration it creates, so a collector beside a
  feed still picks up each session as it arrives. A directory an earlier build wrote without an
  interval is brought up to date instead of refused.
