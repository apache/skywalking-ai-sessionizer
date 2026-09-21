# Changes in 0.5.0

> In development, not yet released. `tools/release/release.sh prepare 0.5.0` removes this note.

## LangChain and LangGraph

- What each model call was sent, and what came back, lands beside the conversation as a provider
  body, cut against what the session already holds by `pkg/providerbody` — the same mechanism as
  Claude Code's bodies. It is what the continuity check between calls runs on, and it shows a
  nested agent its own prompt. Measured on the captured corpus, a twenty-turn conversation lands
  at 13% of what it was on the wire, bodies included, and a conversation of a few short calls at
  about a third. `provider_bodies: false` on the `langsmith-ingest` adapter turns it off. A
  request on this wire names its call, the run's own id, and the assembler now joins a request by
  the call it names, when exactly one names it; a request that names none, as Claude Code's do,
  joins as before. A nested stream's records name the tool they ran inside as their prompt, not
  the trace.

- asz assembles conversations from an agent built on LangChain or LangGraph without changing the
  application. The `langsmith-ingest` adapter is a receiver the tracing client is pointed at, and
  since that client is a dependency of `langchain-core`, every LangChain application already
  carries it: four environment variables are the whole integration. See
  [LangChain and LangGraph](../setup/langchain.md).

- What the receiver leaves out of its capability answer is what keeps it simple. The client
  compresses only when told the receiver can read zstd, so not advertising it keeps every body
  plain and keeps a compression library out of this project. A client that compresses anyway is
  refused with the flag named.

- A run arrives more than once: posted when it starts, patched when it ends. Each arrival is its
  own record sharing one call, and only the arrival carrying the run's end carries its content —
  otherwise one call would emit the assistant message twice, in a round that called itself
  verified. Work in progress lands as data, so a slow turn is visible before it finishes and a
  process that died mid-turn still leaves what it had reported.

- A graph's own runs land trimmed. They were 30 of 39 runs and 74% of the bytes in one capture,
  because each repeats the whole message list its model call already carries. The envelope and the
  graph's metadata land, the repeated content does not, and the record says how many bytes went
  with it.

- Ownership is a namespace, not a thread key: two applications can both use the project
  `production` and the thread `123`. A supplied key is never used as a path either, since
  `../outside`, `team/customer`, `_hidden` and `会话-1` all arrive as real thread keys.

- A demo application under `tests/apps/langchain`, and the fixtures captured from it. It is a real
  LangGraph application, one case per shape a conversation can take, driven by a stand-in model
  over HTTP so it needs no API key, no network and no provider account. `make langchain-capture`
  runs every case and writes `MEASUREMENTS.md` from what it captured, so the numbers there never
  drift from the fixtures. Fifteen cases cover a plain turn, three turns on one thread, a tool that
  fails, parallel tool calls, a graph that loops, two threads at once, a slow tool whose runs are
  posted before they finish, a process that dies mid-turn, large content, a sub-agent, a missing
  thread key, two applications sharing one, keys that are not safe as paths, and the tracing
  client's own decorator with no LangChain in it.

- A LangChain plugin, `apache-skywalking-asz-langchain`, records what each of an agent's tool calls
  changed on disk. It is thirty lines of glue: `asz-changes` does the scanning and diffing, and the
  shim says when a tool call begins and ends, which LangChain has no way to say on its own. The
  application imports nothing — a `.pth` runs a loader at interpreter start, and that loader reads
  one environment variable and imports nothing else, so the plugin costs 0.01 s of interpreter
  start when it is off. See [LangChain Plugin](../setup/langchain-plugin.md).

- `asz-changes` is no longer Claude Code's alone. A tool with no shell command is no longer
  classified read-only and skipped; `tools.scope` takes an exact name, a regular expression
  beginning `re:`, or `*` for every tool, with an `exclude` list in the same three forms; the data
  directory can be named with `ASZ_CHANGES_DATA`; and the events have neutral names — `tool.begin`,
  `tool.end`, `tool.failed` — meaning exactly what Claude Code's do.

- `asz glossary` takes a dialect, and `asz conversation -terms native` reads the one a session's
  own records were landed in. A root can hold conversations from more than one runtime, and the
  runtime's words have to be the ones belonging to what is being read.

## Renamed

The change recorder is not Claude Code's: it records what any runtime's tool call changed, and a
LangChain application now produces the same records. The names say so, and every one of them is
carried across rather than dropped.

These are breaking, and deliberately so: the project is before 1.0, and carrying two names for
every artifact would cost more than the upgrade does.

- The binary `asz-claude-plugin` is **`asz-changes`**. Reinstall the Claude Code plugin once so its
  hooks call the new name; `install/claude-code-plugin.sh` does it and carries the plugin's data
  across.

- The Debian package `asz-claude-code` is **`asz-changes`**. Run
  `sudo apt install asz-changes && sudo apt remove asz-claude-code`.

- **One Homebrew formula.** `asz` installs both binaries, where two formulae used to fetch the
  identical archive and the identical checksum and install one binary each — which only invited a
  machine to end up with `asz` of one version and the recorder of another. Run
  `brew uninstall asz-claude-code`.

- The Claude Code plugin `asz-changes` is **`file-changes`**, so the plugin and the program it runs
  no longer share a name. A plugin's data directory is named after the plugin, so the installer
  copies the old one across before the first session: the settings, and any change record asz had
  not collected yet. Nothing is lost by upgrading.

- The adapter `claude-code-changes` is **`changes`**. A configuration that still names it is read
  as the same adapter, and so is every header landed under it, which keeps its name forever.

- The recorder's data directory can be named with `ASZ_CHANGES_DATA`. `CLAUDE_PLUGIN_DATA` still
  works, because Claude Code sets it for every hook.

## Install

- [Install](../setup/install.md#homebrew-on-macos-and-linux) tells a Homebrew user to run
  `brew trust https://github.com/apache/skywalking-ai-sessionizer` before tapping. Homebrew 7 loads
  a tap that is not its own only after `brew trust`, and it matches a `user/repository` entry only
  against a tap at its default remote, which this one is not: the repository is not named
  `homebrew-skywalking-ai-sessionizer`. Without the trust, `brew tap` stops with
  `Refusing to load formula ... from untrusted tap`.

## Release

- The downloads page lists the source package and the binary archives only. `publish` listed the
  Debian packages there too, beside the archives of the same binaries. They stay in the release
  directory with their signatures and checksums, and apt installs them from the repository on the
  website.
