# Claude Code Plugin

The asz Claude Code plugin records which files each tool call changed. It runs inside Claude Code
as a set of hooks, needs nothing from asz, and leaves one JSON line per observed call in its own
data directory. asz collects those lines the way it collects Claude Code's own transcripts, and
the conversation page shows each change beside the step that made it.

The plugin is named `asz-changes`. Its code is under `plugins/claude-code/` in the repository, and
what Claude Code installs, the manifest and the hooks, is under `plugins/claude-code/plugin/`. Its
binary, `asz-claude-plugin`, ships beside `asz` in every binary package.

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

The plugin comes in two parts, and each has its own step. Both steps use the same version.

1. **The binary.** `asz-claude-plugin` is in every binary package beside `asz`, and
   [Quick install](install.md#quick-install) puts both on your `PATH`. The hooks run the binary by
   its name, so Claude Code must find it on the `PATH` it starts with.
2. **The plugin.** Claude Code installs the manifest and the hooks from the marketplace in the asz
   repository. Set the version first, the same one as the binary: `VERSION=<version>` in a shell,
   or `$Version = "<version>"` in PowerShell. Then, in a shell:

   ```sh
   claude plugin marketplace add "https://github.com/apache/skywalking-ai-sessionizer.git#v$VERSION" --sparse .claude-plugin plugins/claude-code/plugin &&
     claude plugin install asz-changes@skywalking-ai-sessionizer
   ```

   or in PowerShell:

   ```powershell
   claude plugin marketplace add "https://github.com/apache/skywalking-ai-sessionizer.git#v$Version" --sparse .claude-plugin plugins/claude-code/plugin
   if ($LASTEXITCODE -eq 0) { claude plugin install asz-changes@skywalking-ai-sessionizer }
   ```

The address ends with the version's tag, so the hooks Claude Code installs are the ones released
with that binary, not whatever the default branch holds later. It is an HTTPS address because
Claude Code clones a GitHub `owner/repo` shorthand over SSH, which fails on a machine with no SSH
key for GitHub. `--sparse` keeps most of the repository out of the clone, though git still checks
out the files beside each directory it names. What Claude Code copies into its plugin cache is the
plugin's own directory alone: the manifest, the hooks, `LICENSE` and `NOTICE`, and no binary. The manifest names no version, so Claude Code shows the tag's commit as
the plugin's version.

Claude Code sets `CLAUDE_PLUGIN_DATA` for the plugin's hooks and creates the directory it names,
`~/.claude/plugins/data/asz-changes-skywalking-ai-sessionizer/`, under `CLAUDE_CONFIG_DIR` when that
is set. The plugin keeps its state and its output there. Nothing else is configured, and asz need
not be installed for the plugin to record. asz collects the output when it runs.

To check the two parts:

```sh
asz-claude-plugin version
claude plugin list
```

A hook whose binary Claude Code cannot find never stops a tool, and records nothing. Claude Code
2.1.274 reported `Executable not found in $PATH: "asz-claude-plugin"` for the session's start and
end hooks, and the shell command in that session ran. When Claude Code reports that while
`asz-claude-plugin version` works in your terminal, Claude Code started with another `PATH`. Restart
it from that terminal. When no hook error is reported and a shell command still leaves no record,
look in the plugin's `log/plugin.log` in its data directory: a hook that fails inside the plugin
writes there and exits 0. An `Edit` or `Write` on the main stream leaves no plugin record by design,
because Claude Code records that change itself.

### Upgrade

Install the binary of the new version first, with [Quick install](install.md#quick-install) and the
new version set. Then move the plugin to the same version, in a shell:

```sh
(
  set -eu
  : "${VERSION:?set VERSION to the version to move to}"
  claude plugin uninstall asz-changes@skywalking-ai-sessionizer --keep-data
  claude plugin marketplace remove skywalking-ai-sessionizer
  claude plugin marketplace add "https://github.com/apache/skywalking-ai-sessionizer.git#v$VERSION" --sparse .claude-plugin plugins/claude-code/plugin
  claude plugin install asz-changes@skywalking-ai-sessionizer
)
```

or in PowerShell:

```powershell
& {
  if (-not $Version) { throw "set `$Version to the version to move to" }
  $Steps = @(
    @("plugin", "uninstall", "asz-changes@skywalking-ai-sessionizer", "--keep-data"),
    @("plugin", "marketplace", "remove", "skywalking-ai-sessionizer"),
    @("plugin", "marketplace", "add", "https://github.com/apache/skywalking-ai-sessionizer.git#v$Version", "--sparse", ".claude-plugin", "plugins/claude-code/plugin"),
    @("plugin", "install", "asz-changes@skywalking-ai-sessionizer"))
  foreach ($Step in $Steps) {
    & claude @Step
    if ($LASTEXITCODE -ne 0) { throw "claude $Step failed, so the steps after it did not run" }
  }
}
```

Each step is needed, in this order, and each block stops at the first step that fails. Claude Code
2.1.274 refused to add the marketplace again at another tag while it was declared at the first one.
Removing a marketplace uninstalls its plugins, and uninstalling a plugin deletes its data directory,
which holds the plugin's settings and every record asz has not collected yet. `--keep-data` keeps
that directory, and the new version goes on writing into it. If adding the marketplace fails, for
example with no network, the plugin stays uninstalled with its data kept. Run the last two steps
again.

The blocks move a plugin installed at the default scope, `user`, which is where [Install](#install)
puts it. Removing the marketplace removes it from every scope. If you also installed the plugin at
the `project` or `local` scope, uninstall it there with `--keep-data` and `--scope` first.

### Remove

```sh
claude plugin uninstall asz-changes@skywalking-ai-sessionizer
claude plugin marketplace remove skywalking-ai-sessionizer
```

The uninstall deletes the data directory. Let asz collect first, or pass `--keep-data`.

### From 0.3.0

0.3.0 packaged the whole plugin and ran it with `claude --plugin-dir`, which lasts for one session.
Its data directory is `asz-changes-inline`. asz still collects that directory, because it collects
every directory named `asz-changes` or `asz-changes-<marketplace>`. The settings do not move by
themselves. If you wrote a `settings.yaml` for 0.3.0, copy it before the first session with the
installed plugin:

```sh
(
  D=${CLAUDE_CONFIG_DIR:-$HOME/.claude}/plugins/data
  if [ -f "$D/asz-changes-inline/settings.yaml" ] && [ ! -e "$D/asz-changes-skywalking-ai-sessionizer/settings.yaml" ]; then
    mkdir -p "$D/asz-changes-skywalking-ai-sessionizer"
    cp "$D/asz-changes-inline/settings.yaml" "$D/asz-changes-skywalking-ai-sessionizer/"
  fi
)
```

### From a checkout

`make build` writes `bin/asz` and `bin/asz-claude-plugin`. To run the checkout's plugin, put `bin`
first on `PATH` and load the plugin's directory for one session:

```sh
PATH="$PWD/bin:$PATH" claude --plugin-dir plugins/claude-code/plugin
```

Its data directory is `asz-changes-inline`.

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
hook. Subagent edits must be inside one of these roots and pass the same exclusion and symbolic
link rules as a scan. Their record names the root containing the file and the policy in force.
`tools.scope` names the tools observed with a scan; the editing tools need no entry.

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
This applies to subagent edits too, including patches supplied by the runtime. The cap applies
to both the original and resulting content, and to the patch itself.

## The read-only skip

Before scanning around a shell command, the plugin classifies the command. The classifier is
small and fails toward scanning. It works on the whole text, never the first word, because most
commands are compound and many read first and then write; it honours quotes, so a `|` inside a
grep pattern is not a segment boundary, a `>` inside single quotes is text, and a backtick inside
double quotes still runs a command.

Any of these makes the command scan: an unquoted redirection to anything but `/dev/null` or
another descriptor, a heredoc, a command substitution, a process substitution. Otherwise the
command is split on unquoted `|`, `||`, `&&`, `;`, `&` and newlines, and every segment's first
word must be on this list, `readonly-v2`, with the restrictions below:

```text
cd grep cat ls head tail wc cut tr diff cmp echo printf pwd which type stat
du df date printenv jq basename dirname realpath readlink test [ true false sleep
ps uname hostname whoami id nl column comm od hexdump strings sha256sum shasum md5sum
seq expr wait read
env    with no arguments
sort   without -o, --output or --compress-program
tree   without -o or --output
rg     without --pre or --hostname-bin
sed    only -n followed by a print command, such as '10,20p' or '/start/,/end/p', then file names
find   without -delete, -exec, -execdir, -ok, -okdir, -fprint, -fprint0, -fprintf or -fls
awk    one inline program without system, redirection, pipes or extensions; no option arguments
git    status log diff show rev-parse describe blame ls-files ls-tree cat-file grep shortlog
       for-each-ref check-ignore version merge-base count-objects name-rev, and stash list,
       stash show, worktree list, worktree prune; branch, remote, tag, config, symbolic-ref
       and reflog only in recognized query forms
go     version, or env without -w or -u
```

An `env` invocation with arguments and every `xargs` invocation are scanned: they can run another
program or supply writing options. Git commands with explicit output files, external programs,
filters, text conversion or `-c` settings are scanned too. `git add`, `git fetch` and `git commit`
are scanned because filters or hooks can write outside `.git/`. Go package loading can update
module files, so `go list` and `go doc` are scanned. Unknown forms, including compound `for` and
`case` syntax, also scan.

A variable assignment at the start of a segment scans too, as in `GIT_EXTERNAL_DIFF=./tool git diff`,
because a variable such as `GIT_EXTERNAL_DIFF`, `GIT_CONFIG_*` or `LD_PRELOAD` can run another
program. A bare assignment scans as well: it changes a variable the shell may already export. Only
these variables leave a segment read-only, since they change how output is formatted: `LANG`,
`LANGUAGE`, `LC_ALL`, `LC_COLLATE`, `LC_CTYPE`, `LC_MESSAGES`, `LC_NUMERIC`, `LC_TIME`, `TZ`,
`TERM`, `COLUMNS`, `LINES`, `NO_COLOR`, `CLICOLOR`, `CLICOLOR_FORCE` and `FORCE_COLOR`.

The previous `readonly-v1` classified 69.3% of the corpus above as read-only. The skip rate of
`readonly-v2` has not been measured. The fixture
`plugins/claude-code/internal/readonly/testdata/commands.txt` holds the same 93 real commands,
with their expectations updated for this policy, and the classifier's test reads it. The new
policy has its own name so older records keep the meaning of the classifier they ran under.

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

`hooks/hooks.json` gives every hook the binary's name as its command and `hook` as its one argument:

```json
{"type": "command", "command": "asz-claude-plugin", "args": ["hook"], "timeout": 60}
```

The Claude Code [hooks reference](https://code.claude.com/docs/en/hooks#exec-form-and-shell-form)
says that when a hook has `args`, Claude Code resolves `command` as an executable on `PATH` and
starts it directly, with `args` as its arguments and no shell, and that no shell splits the command
into words on any platform. Claude Code 2.1.274 ran the hooks in this form on macOS. It has not run
them on Windows.

The command is a name and not a path inside the plugin, because the binary is installed with asz
and the plugin holds none. Anthropic's language server plugins for Claude Code name their servers
the same way, `gopls` for Go, and ask for the server to be installed on `PATH` first. Up to 0.3.0,
the package carried the binary inside the plugin, and the command was
`${CLAUDE_PLUGIN_ROOT}/bin/asz-claude-plugin`. A marketplace installs a plugin from the repository,
so a plugin that held the binary would need a built copy for every platform committed there, and
an Apache source release carries no compiled file.

Before that, the plugin gave one command line and no `args`:
`"${CLAUDE_PLUGIN_ROOT}/bin/asz-claude-plugin" hook`. The hooks reference says Claude Code runs
such a line in a shell, and that on Windows without Git Bash the shell is PowerShell. PowerShell's
own documentation, in `about_Operators`, says a quoted path is shown as a string, not run, unless
the call operator `&` comes first. The Claude Code 2.1.260 program, read on macOS, puts the line
between a short preamble and a suffix that sets the exit status, and adds no `&`. On macOS,
PowerShell 7.5.3 refused the line with `Unexpected token 'hook' in expression or statement`, both
alone and inside that preamble and suffix, and the binary did not start. With `&` in front, it
started. Neither Claude Code on Windows nor Windows PowerShell 5.1 was tried.

A Claude Code that does not know `args` may drop it and run the binary with no argument. The binary
would then print its usage text and exit 2, and exit status 2 from a `PreToolUse` hook blocks the
tool. So with no argument, the binary runs as `hook` when standard input is a pipe, a socket, a
file, or anything else that is not a terminal or another character device.
Claude Code 2.1.245, 2.1.259 and 2.1.260 know `args`. The first version that knows it is
unavailable.

## What was verified

Each of these was read from a run of Claude Code 2.1.260 with a logging plugin, not from
documentation:

- A `hooks/hooks.json` that names `${CLAUDE_PLUGIN_ROOT}` loads and fires.
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

On 2026-09-11, Claude Code 2.1.260 on macOS ran the hooks with `args`, in a session whose one
shell command wrote a file. The command ran, and the plugin's record named the file as created. The
session had its own configuration directory. Its model was a local program that answers as the
Messages API does, so no request left the machine. The same session with `args` removed from every
hook ran the binary with no argument. A binary without the no-argument rule above printed its usage
text and exited 2, and Claude Code blocked the shell command. The current binary ran as `hook` and
recorded the change. In both forms, Claude Code handed each hook its event on a socket.

On 2026-09-17, Claude Code 2.1.274 on macOS installed the plugin the way [Install](#install) says,
with a configuration directory of its own. The repository was served from the same machine over
HTTP with a tag on the change, because Claude Code refused a `file://` address as a marketplace
source. The marketplace was added at the tag with `--sparse`, and the plugin installed. The plugin
cache held `.claude-plugin/plugin.json`, `hooks/hooks.json`, `LICENSE` and `NOTICE`, under a
directory named for the tag's commit. A headless session, with a local program answering as the
Messages API does, ran one shell command that wrote a file:

- With `asz-claude-plugin` on `PATH`, the hooks ran it by name. Its record in
  `asz-changes-skywalking-ai-sessionizer/output/<session-id>/main.jsonl` named the file as created,
  and `asz collect` landed the record under the session's main stream.
- With no `asz-claude-plugin` on `PATH`, the session's start hook reported
  `Executable not found in $PATH: "asz-claude-plugin"`, and so did its end hook. The shell command
  ran, and no record was written.

For an upgrade, a second tag was made on a changed `hooks/hooks.json`. Adding the marketplace at the
second tag was refused while it was declared at the first. Removing the marketplace uninstalled the
plugin and deleted its data directory, with its `settings.yaml` and an output file in it.
Uninstalling with `--keep-data` first, then removing the marketplace, adding it at the second tag
and installing, kept both files, and the cache then held the changed hooks.

`tools/claudecodecheck` repeats all of this on the machine it runs on, the way a person follows the
install pages. It reads each block from the page and changes only the download addresses, to a
server on the same machine that holds the package, the marketplace repository at two tags, and a
stand-in for the model's API. It runs the Quick install block in every shell there is for it, the
plugin's install commands, a session the plugin must record and asz must collect, a session with no
`asz-claude-plugin` on `PATH`, and the Upgrade block, after which the data must still be there and
the next session must be recorded. CI's `claude-code` job runs it with Claude Code 2.1.274 on each
binary package's own platform: Linux, macOS and Windows, each on x86-64 and ARM 64. On 2026-09-17 it
passed on macOS on Apple silicon, and on Linux on ARM 64 in a Debian 13.6 container and in an Alpine
3.22.5 container, which runs Claude Code's build for musl.

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

Claude Code itself has not run the hooks on Windows. The hooks reference says a hook with `args`
uses no shell and ignores `shell`, so whether Git Bash is installed should not matter. That has
not been tried. It is also unknown whether the binary starts at all. `hooks/hooks.json` names
`asz-claude-plugin`, and the file on Windows is `asz-claude-plugin.exe`. The hooks reference says
that on Windows, `command` must resolve to a real executable, such as a `.exe`. It does not say
whether a name without `.exe` resolves to one. The package smoke test now looks the binary up by
that name, through Git Bash on Windows, which is not how Claude Code looks it up, and it has not
run on Windows yet.

To find out, on Windows x86-64 or ARM 64:

1. Install the binary and the plugin as [Install](#install) says, and restart Claude Code.
2. Ask for one shell command that writes a file.
3. Look in `%USERPROFILE%\.claude\plugins\data\asz-changes-skywalking-ai-sessionizer`. A record in
   `output\<session-id>\main.jsonl` there that names the file means the hooks work.
4. Without a record, start Claude Code with `claude --debug hooks` from a terminal where
   `Get-Command asz-claude-plugin` finds `asz-claude-plugin.exe`, and repeat step 2. If Claude Code
   still reports `Executable not found in $PATH` for `asz-claude-plugin`, the name without `.exe` did
   not resolve, and the plugin would need a hooks file for Windows that names `asz-claude-plugin.exe`.
   If it reports no hook error, read `log\plugin.log` in the data directory.
