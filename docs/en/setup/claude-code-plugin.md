# Claude Code Plugin

The asz Claude Code plugin records which files each tool call changed. It runs inside Claude Code
as a set of hooks, needs nothing from asz, and leaves one JSON line per observed call in its own
data directory. asz collects those lines the way it collects Claude Code's own transcripts, and
the conversation page shows each change beside the step that made it.

The plugin is named `asz-changes`. Its code is under `plugins/claude-code/` in the repository,
and its binary, `asz-claude-plugin`, ships beside `asz` in every binary package.

## What is recorded, and by whom

Claude Code records some changes itself. Every successful `Edit` and `Write` on the main stream
carries the runtime's own patch in the transcript, measured on 2,221 of 2,221 such results in a
52-session corpus. asz reads those without the plugin: the Claude Code adapter copies each patch
into a change record beside the raw result when it lands the transcript.

The plugin records what Claude Code does not:

| Tool | Main stream | Inside a subagent | How |
| --- | --- | --- | --- |
| `Bash`, `PowerShell`, `Monitor` | plugin | plugin | a scan of the workspace before and after the call |
| `Edit`, `Write`, `NotebookEdit` | Claude Code | plugin | the patch the hook's own response carries; a subagent's transcript holds none |
| `Read`, `Grep`, `Glob` and the other read-only tools | nobody | nobody | they change nothing |
| `Agent` | nobody | | the child's own hooks observe the child's calls |

A shell command the plugin classifies as read-only skips its scans and still leaves a record, so
the absence of changes reads as "not observed" and never as "nothing changed". A change made
between two observed calls by something no hook covered, a person, an editor, an unhooked tool,
is recorded as well, unattributed, when the next scan finds it. An edit the runtime recorded is
not one of those: the plugin's hook on the editing tools brings its manifest up to date with the
file the tool wrote, on both streams, so the next scan does not find that edit as nobody's, and a
window open at the same time sees it as shared with the edit and names it.

## Install

The binary package for your platform holds `claude-code-plugin/`: the plugin's manifest, its
hooks, and its binary under `bin/`. [Install](install.md) says where to get the package, and
where each package manager puts this directory. Point Claude Code at it:

```sh
claude --plugin-dir /path/to/claude-code-plugin
```

or add it to your Claude Code settings the way the Claude Code documentation describes for a
local plugin. Claude Code sets `CLAUDE_PLUGIN_DATA` for the plugin's hooks and creates the
directory it names, `~/.claude/plugins/data/asz-changes-<marketplace>/`, which is where the
plugin keeps its state and its output. Nothing else is configured, and asz need not be installed.

To build the plugin from a checkout, `make build` puts the binary at
`plugins/claude-code/bin/asz-claude-plugin`, where `hooks/hooks.json` expects it, so
`claude --plugin-dir plugins/claude-code` runs the checkout's plugin.

`asz-claude-plugin status`, with `CLAUDE_PLUGIN_DATA` set, prints the settings in force and the
exclusion rules they expand to.

## Output

One file per stream of a session, one line per record, appended and never rewritten:

```text
${CLAUDE_PLUGIN_DATA}/output/<session-id>/main.jsonl
${CLAUDE_PLUGIN_DATA}/output/<session-id>/<agent-id>.jsonl
```

A record goes to the file of the stream its hook event came from. An event inside a subagent
carries `agent_id`, and the file takes that name. No event on the main stream carries `agent_id`,
so an event without one goes to `main.jsonl`. The same absence decides who records an edit: the
plugin records one only when the event carries `agent_id`. See
[What was verified](#what-was-verified).

Each line is a `changes/1` record: the tool-use id it belongs to as its id, `captured_by:
asz-plugin`, the session, the stream, the time, how it was observed, the root, the policy it ran
under, and for each changed file its path, the operation, size and hash before and after, and the
hunks as a unified diff with line numbers. `pkg/changes` in the repository defines the shape.
Hook input carries neither a message id nor a request id. So a record names its tool call and
never the provider call that made it, and the view joins it to its step by the tool-use id alone.
See [Workspace changes](../adapters/claude-code.md#workspace-changes).

Two hooks can run at the same time. So each record is written whole, in one write to a file
opened for appending, and synced to disk before the hook returns. asz lands a file only up to its
last complete newline, and a line still being written waits for the next pass. Appending with
`cat >>` to one shared file from concurrent subagents was seen to corrupt lines above about 64 KB.
The sample it was seen on is unavailable.

asz's `claude-code-changes` adapter, on by default, finds these files beside Claude Code's own,
tails them, and lands each line as a record of kind `changes` under the stream the tool ran in.
See [Configuration](configuration.md#the-changes-adapter).

Each file it lands names the dialect `asz-changes/1` in its
[header](../formats/session-data.md#header), not `claude-code/1`. The plugin writes its records in
the model's own words, not in Claude Code's shape. Records of a different shape get a different
dialect, even when the same runtime produced them, as the
[adapter contract](../concepts-and-designs/unified-conversation-model.md#adapter-contract) says.

## Settings

Optional, in `${CLAUDE_PLUGIN_DATA}/settings.yaml`. Every value has a default, and the plugin runs
with no file at all.

```yaml
roots: []                     # empty: the session's project directory
exclude:
  defaults: standard-v1
  add: []
  remove: []
read_only:
  enabled: true
tools:
  scope: [Bash, PowerShell, Monitor]
retention:
  idle: 30m
  ttl: 720h
scan_timeout: 30s
size_cap: 1048576
```

`roots` are the directories observed; empty means the project directory Claude Code hands every
hook. `tools.scope` names the tools observed with a scan; the editing tools need no entry.

## Exclusions

Exclusion means no observation: a change under an excluded directory is never seen. Every record
names the set it ran under and the rules it expanded to, so history explains itself after the
set changes. The set is frozen under its name; a changed set gets a new name.

Two kinds of rule, because the names build tools use are also names source uses. An
**unconditional** name is excluded wherever it appears; it is never source. A **conditional**
name is excluded only beside the ecosystem's own project file, which is how the build tools
themselves decide: `build/` beside `pom.xml` is Maven output, while `build/` in a repository with
no project file beside it is kept, because many repositories keep scripts there.

`standard-v1`:

| Ecosystem | Always excluded | Excluded only beside |
| --- | --- | --- |
| version control | `.git` `.hg` `.svn` | |
| JVM | `target` `.gradle` | `build/` beside `pom.xml`, `build.gradle` or `build.gradle.kts` |
| Go | | `bin/` `dist/` `vendor/` beside `go.mod` |
| Node | `node_modules` `.next` `.nuxt` `.turbo` `.parcel-cache` `.svelte-kit` | `dist/` `build/` beside `package.json` |
| Python | `__pycache__` `.venv` `venv` `.tox` `.nox` `.mypy_cache` `.pytest_cache` `.ruff_cache` `*.egg-info` `.eggs` | `build/` `dist/` beside `pyproject.toml` or `setup.py` |
| Rust | `target` | |
| C and C++ | `cmake-build-*` `CMakeFiles` | `build/` beside `CMakeLists.txt` |
| .NET | | `bin/` `obj/` beside a `*.csproj` or `*.sln` |
| Ruby | `.bundle` `vendor/bundle` | `tmp/` `log/` beside `Gemfile` |
| PHP | | `vendor/` beside `composer.json` |
| Swift and Xcode | `.build` `DerivedData` `Pods` `Carthage/Build` | |
| Scala | `.bloop` `.metals` | |
| Elixir | `_build` | `deps/` beside `mix.exs` |
| Haskell | `dist-newstyle` `.stack-work` | |
| Dart and Flutter | `.dart_tool` | `build/` beside `pubspec.yaml` |
| Zig | `zig-cache` `.zig-cache` `zig-out` | |
| Terraform | `.terraform` | |
| Hugo | | `public/` `resources/_gen/` beside `hugo.toml`, `hugo.yaml` or `config.toml` |
| Jekyll and MkDocs | | `_site/` beside `_config.yml`; `site/` beside `mkdocs.yml` |
| coverage | `coverage` `.nyc_output` `htmlcov` | |
| caches and IDE state | `.cache` `.idea` `.vs` | |

Deliberately not in the defaults: `packages/`, because monorepos keep source there; `.claude/`,
because skills and settings are content people edit; `data/`, because the name is generic. An
asz checkout adds its own storage root:

```yaml
exclude:
  defaults: standard-v1
  add: ["/data/"]
```

`effective = (defaults − remove) ∪ add`. A rule with a leading `/` is anchored to the root; any
other name matches at any depth. `remove` names a default to keep observing, for example
`**/vendor/`. `.gitignore` is never read. Symbolic links are neither followed nor recorded. A
file over `size_cap`, or one holding a NUL byte, lands as path and hash only, with the reason.

## The read-only skip

Before scanning around a shell command, the plugin classifies the command. The classifier is
small and fails toward scanning. It works on the whole text, never the first word, because most
commands are compound and many read first and then write; it honours quotes, so a `|` inside a
grep pattern is not a segment boundary, a `>` inside single quotes is text, and a backtick inside
double quotes still runs a command.

Any of these makes the command scan: an unquoted redirection to anything but `/dev/null` or
another descriptor, a heredoc, a command substitution, a process substitution. Otherwise the
command is split on unquoted `|`, `||`, `&&`, `;`, `&` and newlines, and every segment's first
word must be on this list, `readonly-v1`:

```text
cd grep rg cat ls head tail wc sort uniq cut tr diff cmp echo printf pwd which type stat file
du df date env printenv jq tree basename dirname realpath readlink test [ true false sleep
ps uname hostname whoami id nl column comm od xxd hexdump strings sha256sum shasum md5sum
seq expr wait read
sed    without -i or --in-place
find   without -delete, -exec, -execdir, -ok or -fprint
awk    without system(
xargs  when the program it runs is on this list
git    status log diff show branch remote rev-parse describe blame ls-files ls-tree cat-file
       tag fetch grep shortlog for-each-ref check-ignore reflog version merge-base config
       count-objects name-rev symbolic-ref add, and stash list, stash show, worktree list,
       worktree prune
go     version env list doc
```

`git add` is on the list because it changes only `.git/`, which is excluded scope. `git commit`
is not, because a pre-commit hook can rewrite files. A miss is not a loss: the next scan compares
against the last manifest, so a write that slipped through lands as an unattributed change. On
the corpus above, 69.3% of shell commands are read-only under this list, so most shell commands
wait for no scan. The fixture
`plugins/claude-code/internal/readonly/testdata/commands.txt` holds 93 real commands with the
outcome each must get, and the classifier's test reads it.

## Retention

Two rules, and neither waits for a collector:

- **Snapshot bytes.** A scan keeps the bytes of every file it saw, so a later diff has both
  sides. When a root has had no window open for `retention.idle`, those bytes and the chain of
  steps are dropped; the manifest of paths and hashes stays, so the next scan still knows what
  was there and starts fresh.
- **Output.** A file under `output/` not written to for `retention.ttl` is removed, 30 days by
  default, which matches Claude Code's own transcript cleanup. Whatever asz has landed is asz's,
  under its own retention.

The rules run at the session's start and end, and before each scan. `asz-claude-plugin prune`
runs them now.

## What a scan is

A scan walks the root under the exclusion rules and hashes what it finds. A file whose size,
modification time and inode are as the last scan saw them keeps its hash, unless its
modification time falls within the last scan's own second, when it is hashed again because the
clock cannot tell. Every scan appends a step to the root's chain: which paths changed and from
what hash to what. A window is the span of steps between its two scans.

Two tools open at once on one root, two subagents say, see each other. A file changed in a step
only one window spans is that window's alone; one changed in a step both span is `shared`, and
each record names the other under `overlaps`. Nothing claims who wrote a byte: a person or an
editor can write inside any window. The record says which tool windows could have.

Claude Code runs a hook synchronously, so the tool call waits until the hook returns. That is why
a scan has a cap. A scan stops at `scan_timeout` and the record says so, with `coverage: partial`.
The cap is for one scan, and a hook scans each root in turn. The default, 30 seconds, is half the
60 seconds `hooks/hooks.json` gives each tool event. A hook still running at its timeout is killed
and leaves no record for its call, so a `scan_timeout` near 60 seconds loses records instead of
marking them partial. The scans of one root take turns under a lock, and a hook waiting for that
lock is bounded only by its own timeout. The session's start and end run no scan, and get 30
seconds.

On 5 live headless sessions with a hook on every event, a hook invocation took about 5.6 ms.
Those hooks ran no scan. How long a scan takes has not been measured.

A hook that fails, for any reason, exits 0 and writes to `${CLAUDE_PLUGIN_DATA}/log/plugin.log`.
It never stops the tool.

## The hook command

`hooks/hooks.json` gives every hook the binary as its command and `hook` as its one argument:

```json
{"type": "command", "command": "${CLAUDE_PLUGIN_ROOT}/bin/asz-claude-plugin", "args": ["hook"], "timeout": 60}
```

When a hook has `args`, Claude Code starts the binary itself, with no shell, on every platform. It
puts the plugin's directory in place of `${CLAUDE_PLUGIN_ROOT}` as plain text, so a space, an
apostrophe or a `$` in that path does no harm. The Claude Code
[hooks reference](https://code.claude.com/docs/en/hooks#exec-form-and-shell-form) calls this the
exec form.

The plugin used to give one command line and no `args`:
`"${CLAUDE_PLUGIN_ROOT}/bin/asz-claude-plugin" hook`. Claude Code runs such a line in a shell. On
Windows without Git Bash, that shell is PowerShell. PowerShell does not run a quoted path followed
by a word unless the call operator `&` comes first, and Claude Code 2.1.260 does not add it.
PowerShell 7.5.3 refused the line with `Unexpected token 'hook' in expression or statement`, so no
hook started.

A Claude Code that does not know `args` may drop it and run the binary with no argument. The binary
would then print its usage text and exit 2, and exit status 2 from a `PreToolUse` hook blocks the
tool. So with no argument, the binary runs as `hook` when standard input is a pipe, a socket, a
file, or anything else that is not a terminal or another character device.
Claude Code 2.1.245, 2.1.259 and 2.1.260 know `args`. The first version that knows it is
unavailable.

## What was verified

Each of these was read from a run of Claude Code 2.1.260 with a logging plugin, not from
documentation:

- `hooks/hooks.json` with `${CLAUDE_PLUGIN_ROOT}` loads and fires.
- Every tool event carries `session_id`, `tool_use_id`, `tool_input` and `cwd`.
- Every event inside a subagent carries `agent_id`. It is the id in the subagent transcript's file
  name.
- No event on the main stream carries `agent_id`.
- A failed shell command fires `PostToolUseFailure`, with the exit code in `error`.
- The `Edit` response carries the patch and the original content.
- So does a `Write` over an existing file. Its patch ends with the git marker line for a missing
  final newline.
- The `NotebookEdit` response names its file as `notebook_path`. It carries the whole file before
  and after and no patch, so the plugin computes the hunks from the two.
- `CLAUDE_PLUGIN_DATA` exists before the first hook runs.
- A hook past its `timeout` is killed, and the tool proceeds.

A second sample, 5 live headless sessions with a hook on every event, agrees. `agent_id` is on
every event inside a subagent and on no event of the main stream. No event in that sample carries
a message id or a request id.

The plugin itself was run inside Claude Code on macOS with a shell command, an edit and a
subagent. asz collected and showed the result.

On 2026-09-11, Claude Code 2.1.260 on macOS ran the hooks in exec form, in a session whose one
shell command wrote a file. The command ran, and the plugin's record named the file as created. The
session had its own configuration directory. Its model was a local program that answers as the
Messages API does, so no request left the machine. The same session with `args` removed from every
hook ran the binary with no argument. A binary without the no-argument rule above printed its usage
text and exited 2, and Claude Code blocked the shell command. The current binary ran as `hook` and
recorded the change. In both forms, Claude Code handed each hook its event on a socket.

On Windows the plugin has run only outside Claude Code. CI's `packages` job runs
`tools/package-smoke.sh` on each binary package, on a runner of the package's own platform. On
2026-09-11, the CI of pull request #6 ran it on `windows-latest`, x86-64, and on `windows-11-arm`,
ARM 64. Both jobs passed, in 30 and 33 seconds. The script unpacks the zip with `Expand-Archive`.
It runs the packaged plugin with a `SessionStart`, a `PreToolUse`, a `PostToolUse` and a
`SessionEnd` event on standard input, and writes a file between the two tool events. Those runs
checked only that some file in the plugin's data directory named it. The plugin's scan writes its
own files, which name it too, before the plugin writes the record. So those runs do not show that
the plugin wrote its record on Windows. The script now requires the record in
`output/<session-id>/main.jsonl` to name the file as created, and CI's unit tests now run the
plugin's own tests on each system. Neither has run on Windows yet.

Claude Code itself has not run the hooks on Windows. The exec form involves no shell, so whether
Git Bash is installed no longer matters. One thing is still unknown. The Windows packages hold
`bin\asz-claude-plugin.exe`, and `hooks/hooks.json` names `bin/asz-claude-plugin`. The Claude Code
documentation says only that on Windows the command must resolve to a real executable, such as a
`.exe`. To find out, on Windows x86-64 or ARM 64:

1. Unpack the package for the machine, and start Claude Code with
   `claude --plugin-dir <package>\claude-code-plugin`.
2. Ask for one shell command that writes a file.
3. Look in the plugin's data directory. For a plugin loaded with `--plugin-dir`, Claude Code 2.1.260
   on macOS put it at `plugins/data/asz-changes-inline` under its configuration directory. A record
   in `output\<session-id>\main.jsonl` there that names the file means the hooks work. If
   `log\plugin.log` does not exist, the binary never started. The likely cause is that the name did
   not resolve, and then the Windows packages need a `hooks/hooks.json` of their own that names
   `asz-claude-plugin.exe`.
