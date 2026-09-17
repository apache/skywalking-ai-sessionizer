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
- **The round carries that join.** An `llm.call` in a `.sf` round carries `provider_bodies` in its
  attributes, and the `session` node how many bodies the session holds. The join was made when a
  conversation was rendered, which meant every reader repeated it, and to repeat it a reader had to
  open every landed body — the largest files a session holds — to read one line of each. A server
  that mirrors the format paid that on every read of a conversation: measured on the end-to-end
  session, 27,884 bytes of provider bodies against 8,158 bytes of transcript, none of it in the
  document. It is now resolved once, when the round is parsed, from the index alone; the index keeps
  each body's role, its request id and the request before it, and each record's line in its stream,
  which is what tells a gapped stream from a whole one. The rules are unchanged, the document is
  unchanged, and a round reader refuses a body reference past the round's own range as it refuses
  any other. A conversation whose rounds were parsed before this carries no bodies on its calls;
  parsing its chain again brings them in. The index schema is bumped, so it rebuilds itself.
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
- **`asz view` draws the prompt too.** The embedded renderer moves to Horizon `0ce1f8d6`, which added
  the inspector's Prompt tab, and the page gains the files route that tab reads: `GET
  /api/c/{conversation}/files?session=&seq=` returns the landed files by sequence, at most 32 a
  request, as base64 in a JSON array. The renderer offers the tab only to a host that can read files,
  so without the route the page beside the storage root would have shown less than the SkyWalking UI.
- **A scenario writes the prompt its agent sends.** `system_prompt` and `tools` say what a stream's
  requests carry, on the scenario for the main stream and on an `agent`, `skill` or workflow child
  for its own. Before this every scenario sent the same stand-in, which advertised `Read` and `Bash`
  whatever the agent did, so a calendar assistant was drawn as a coding agent. The stand-in stays for
  a scenario that writes neither, because it is sized to make a body worth cutting. A response now
  carries the call's own `usage`, the numbers its transcript already records.

## Collection

- The collector's default `interval` is 10 minutes, in `asz.yaml` and when no configuration is
  given; it was 5 seconds. Every pass that finds new records lands new files, so a period of
  seconds wrote many small files, and each one is a log record when sent. `asz collect` and
  `asz server` still make a pass when they start. A configuration that sets `interval` keeps its
  value. [Configuration](../setup/configuration.md#choosing-an-interval) explains the choice.
- A scenario build writes `interval: 5s` into the configuration it creates, so a collector beside a
  feed still picks up each session as it arrives. A directory an earlier build wrote without an
  interval is brought up to date instead of refused.

## Release

- `publish` asks whether the version becomes the latest GitHub release before anything moves, and
  promotes with that answer. Before, promotion left the Latest label on the older release, because
  CI creates the prerelease with `--latest=false`. The answer offered is yes for a version newer
  than every full release, and no for a patch of an older line. `--latest` and `--not-latest` answer
  without asking. The `latest` image tag follows GitHub's latest release, so one decision moves
  both. Before, the image took the highest version on its own.
