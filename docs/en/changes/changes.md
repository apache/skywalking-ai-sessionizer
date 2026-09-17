# Changes in 0.4.0

> In development, not yet released. `tools/release/release.sh prepare 0.4.0` removes this note.

## Provider bodies

- A new adapter, `claude-code-provider`, lands the request and response bodies Claude Code writes
  for its model provider when `OTEL_LOG_RAW_API_BODIES=file:<dir>` is in its environment: the
  system prompt, the tool schemas and every message as it was sent, which no transcript holds. A
  request names its session; a response is claimed by a later request that names its request id,
  or by the one session whose transcript holds its message id, and waits otherwise. On by default,
  it does nothing until the variable is set. See
  [Claude Code Provider Bodies](../setup/claude-code-provider-bodies.md).
- The kind `provider_body`, reserved until now, lands under `<session>/provider_body/`, in files of
  at most `max_delta_bytes` each; only a body larger than that lands in a larger file of its own. A record keeps only what its session does not hold yet: each tool definition
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
- **`asz view` draws the prompt too.** The embedded renderer moves to Horizon `53c1f63b`. Horizon
  `0ce1f8d6` added the inspector's Prompt tab, and the page gains the files route that tab reads: `GET
  /api/c/{conversation}/files?session=&seq=` returns the landed files by sequence, at most 32 a
  request, as base64 in a JSON array. The renderer offers the tab only to a host that can read files,
  so without the route the page beside the storage root would have shown less than the SkyWalking UI.
- **The Prompt tab reads a message in either spelling.** Claude Code sends a message of one text
  block as a list of blocks while it is the newest, and as a plain string once it is not. The
  renderer before Horizon `53c1f63b` took the two for different messages, so it reported an
  ordinary growth step as a rewritten history: 60 of 62 steps on one real conversation of 94 calls.
  It now reads both as one message. When the history does change, it says the context was replaced
  by a summary, with how many messages became how many, or warns when the history changed without
  getting shorter. [Claude Code Provider Bodies](../setup/claude-code-provider-bodies.md) describes
  both spellings.
- **A scenario writes the prompt its agent sends.** `system_prompt` and `tools` say what a stream's
  requests carry, on the scenario for the main stream and on an `agent`, `skill` or workflow child
  for its own. Before this every scenario sent the same stand-in, which advertised `Read` and `Bash`
  whatever the agent did, so a calendar assistant was drawn as a coding agent. The stand-in stays for
  a scenario that writes neither, because it is sized to make a body worth cutting. A response now
  carries the call's own `usage`, the numbers its transcript already records.
- A scenario build records the signature it drops from every thinking block, as the adapter does.
  The `.sd` writer recorded the drop only for reasoning whose text was already gone, so a session
  whose reasoning kept its text folded to another conversation through the transcript writer.
  `tests/scenarios/reasoning-text.yaml` writes both shapes in one session.

## Collection

- The collector's default `interval` is 10 minutes, in `asz.yaml` and when no configuration is
  given; it was 5 seconds. Every pass that finds new records lands new files, so a period of
  seconds wrote many small files, and each one is a log record when sent. `asz collect` and
  `asz server` still make a pass when they start. A configuration that sets `interval` keeps its
  value. [Configuration](../setup/configuration.md#choosing-an-interval) explains the choice.
- A scenario build writes `interval: 5s` into the configuration it creates, so a collector beside a
  feed still picks up each session as it arrives. A directory an earlier build wrote without an
  interval is brought up to date instead of refused.

## Install

- **The Claude Code plugin installs from a marketplace.** 0.3.0 told users to run
  `claude --plugin-dir` on the packaged plugin, which Claude Code keeps for one session only, so the
  next plain `claude` recorded nothing and said nothing. The repository now carries
  `.claude-plugin/marketplace.json`, and `claude plugin marketplace add` at a version's tag, then
  `claude plugin install asz-changes@skywalking-ai-sessionizer`, keeps the plugin installed across
  sessions. The manifest and the hooks moved to `plugins/claude-code/plugin/`, which holds only what
  Claude Code reads and its own `LICENSE` and `NOTICE`, so no Go source reaches a user's plugin
  cache. The manifest names no version, so the version is the tag's commit and no release can leave
  a stale one behind. [Claude Code Plugin](../setup/claude-code-plugin.md#install) gives the commands, and
  how to upgrade without losing records asz has not collected yet: removing the marketplace deletes
  the plugin's data directory unless the plugin is first uninstalled with `--keep-data`.
- **The hooks run `asz-claude-plugin` by name from `PATH`**, as Anthropic's language server plugins
  run their servers. A binary inside the plugin would have to be committed for every platform, and
  a source release carries no compiled file. With no binary on `PATH`, Claude Code 2.1.274 reported
  `Executable not found in $PATH` for the hook, and the tool still ran.
- **Each binary package holds `asz` and `asz-claude-plugin` side by side**, with `LICENSE`, `NOTICE`
  and `licenses/`, and no `claude-code-plugin/` directory. `make build` writes both to `bin/`. The
  package smoke test finds the plugin's binary by name on `PATH`. The candidate check in
  `tools/release/release.sh`, and the Scoop and winget manifests follow the new layout, and put both
  binaries on the path. The Homebrew formula becomes two, one for each install: `asz` and
  `asz-claude-code`. The tap is this repository: after each release the formulae go to `Formula/`
  on main, which `.gitattributes` keeps out of the source package, and users run
  `brew tap apache/skywalking-ai-sessionizer https://github.com/apache/skywalking-ai-sessionizer`,
  then `brew install apache/skywalking-ai-sessionizer/asz` and `.../asz-claude-code`.
- **Every release stays installable with Homebrew.** Each release adds `asz@VERSION` and
  `asz-claude-code@VERSION`, keg-only, beside the current formulae, and moves `asz` and
  `asz-claude-code` only forward.
- **The steps after the vote that write for a package manager are skills and documented steps, not
  scripts.** `tools/install-manifests.sh`, which `publish` ran, is gone, and `publish` now
  writes only the announcement and the website entries. The `homebrew` skill in `.claude/skills/`
  holds the formula templates, writes the formulae of a released version, runs each through
  `brew style`, `brew audit --strict`, `brew install` and `brew test`, and opens a pull request to
  main. The `apt` skill adds a released version to the apt repository and opens a pull request to
  apache/skywalking-website. The Scoop and winget manifests are in
  [How to Release](../guides/how-to-release.md#scoop), with the commands that fill and test them,
  and its [Release targets](../guides/how-to-release.md#release-targets) list where a released
  version goes.
- **Debian packages, and an apt repository on the website.** Each release ships
  `apache-skywalking-ai-sessionizer-VERSION-bin-asz-ARCH.deb` and `...-bin-asz-claude-code-ARCH.deb`
  for amd64 and arm64, voted and signed with the other packages. `tools/release/deb-package` writes them in
  `make binaries` from the same staged files, with the same time, owner and modes, so a rebuild
  gives the same bytes on any machine. `https://skywalking.apache.org/apt` serves an index of every
  released version, signed with a key in KEYS, and `.htaccess` redirects that send apt to the newest
  version on the mirrors and to every older one on archive.apache.org. The `apt` skill writes them.
  CI's `packages` job installs the Debian packages with apt on Linux, on amd64 and arm64.
  [Install](../setup/install.md) gives the commands. `candidate` checks each `.deb`'s contents and
  control file, and `make binaries` removes `.DS_Store`, `._` and `__MACOSX` files from what it
  packages.
- **Two install scripts, one for each install.** asz, the collector, and the Claude Code plugin are
  installed apart. `install/asz.sh` and `install/asz.ps1` install `asz`, and
  [Install](../setup/install.md#install-script) runs them from the version's tag in one command. The
  plugin's own scripts, `install/claude-code-plugin.sh` and `install/claude-code-plugin.ps1`,
  install `asz-claude-plugin` and add the plugin to Claude Code at the same tag, and
  [Claude Code Plugin](../setup/claude-code-plugin.md#install) runs them. The reader sets the
  version. Each script downloads the package and its `.sha512`: through the mirrors for the newest
  release, which is all downloads.apache.org holds, and from archive.apache.org for any older one.
  It stops unless they match, checks that its binary starts, and installs it
  where the Claude Code installer puts `claude`: `~/.local/bin`, or `%USERPROFILE%\.local\bin`.
  Run again with a newer version, the plugin's script uninstalls the plugin with `--keep-data`
  before it moves the marketplace, and copies a 0.3.0 `settings.yaml` over once.
- **CI runs the install pages with Claude Code.** A new `claude-code` job runs
  `tools/test/claude-code-check` with Claude Code 2.1.274 on each binary package's platform, Linux, macOS
  and Windows on x86-64 and ARM 64. It runs both install commands as the pages write them, the
  plugin's upgrade by hand and by its script, and its install by hand, with only the download
  addresses pointed at the runner. A headless session must be recorded by the plugin and collected
  by asz, a session without the binary must still run its tool, and each upgrade must keep the
  plugin's data. Before, no platform ran the plugin's hooks inside Claude Code in CI, and Windows
  never had. It passes on all six. On Windows the hooks' `asz-claude-plugin` starts
  `asz-claude-plugin.exe`, and both scripts run in Windows PowerShell and PowerShell 7.

## Documentation

- The setup pages say how to install, set up and use asz, and little else. Quick Start starts from an
  installed `asz` and says where it keeps its data and its configuration. How the plugin, the provider
  bodies, the export and the commands work inside moved to their own pages:
  [Claude Code Plugin Internals](../adapters/claude-code-plugin.md),
  [Claude Code Provider Bodies Internals](../adapters/claude-code-provider-bodies.md),
  [OTLP Records](../formats/otlp.md) and [Command Line Internals](../guides/command-line-internals.md).

## Release

- `publish` asks whether the version becomes the latest GitHub release before anything moves, and
  promotes with that answer. Before, promotion left the Latest label on the older release, because
  CI creates the prerelease with `--latest=false`. The answer offered is yes for a version newer
  than every full release, and no for a patch of an older line. `--latest` and `--not-latest` answer
  without asking. The `latest` image tag follows GitHub's latest release, so one decision moves
  both. Before, the image took the highest version on its own.
- `website.txt` is a release for `data/projects.yml`, the one file where the SkyWalking website now
  keeps its downloads and its documentation. Before, it held entries for `data/releases.yml` and
  `data/docs.yml`, which the website no longer has, so the 0.3.0 entries were rewritten by hand.
  The release is marked the latest, with the Latest documentation, only when GitHub's Latest label
  names it. A run after promotion reads the label, so its entries agree with the label too. The
  release guide describes the new shape, and the steps `publish` prints name the new file.
- The text of the GitHub release links the changelog's other pages at the tag. The changelog links
  them by a path relative to itself, which on the 0.3.0 release page resolved under `/releases/`
  and found nothing.
- `tools/` is in two folders. `tools/release/` holds what makes a release: `release.sh`,
  `ci-binaries.sh`, `ci-upload-binaries.sh`, `package-check.sh`, `dep-notices.sh` and
  `deb-package`. `tools/test/` holds the checks CI runs and the tests of the release scripts. Folder
  names separate their words with a hyphen. `tools/otlpdump` is gone: `TestCaptures` in
  `internal/metrics` writes a new capture with its identifying values replaced, and checks every
  committed capture on each run.
- `prepare` refuses to run on a detached HEAD, where it would push the tag and then have no branch
  to raise the pull request against.
