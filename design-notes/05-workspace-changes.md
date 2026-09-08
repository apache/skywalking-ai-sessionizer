# Design Plan 05 — Workspace Changes

**Status:** implemented on 2026-09-08 as decided below: `pkg/changes`, the `changes` kind, the
`claude-code-changes` adapter, the adapter's native part, `workspace_changes` in `asz.view`, the scenario
`tests/scenarios/workspace-changes.yaml`, and the plugin under `plugins/claude-code/`. The
documentation is `docs/en/setup/claude-code-plugin.md`. This note replaces the
plan "Claude Code workspace capture: ASZ → OAP → Horizon" of 2026-09-07, which is kept outside the
repository and is superseded, not patched.
**Scope:** which files a tool call changed, and how that reaches the reader beside the step. A
Claude Code plugin captures what the runtime does not record; asz collects the plugin's output the
way it collects the runtime's own logs; the view joins both to the step.

Every number below was measured on 2026-09-08 on one machine's storage root: 52 Claude Code
sessions, every stream, 104,142 tool calls. Where a number is missing it says `unmeasured`.

---

## 0. The principle

The plugin runs standalone. It needs no asz server, no endpoint, no credential and no
acknowledgement. Its only output is local files. The asz server discovers and collects those files
read-only, as it collects Claude Code's transcripts, and keeps every cursor and every piece of
progress in its own storage root. Stopping either side never affects the other.

Session Data and Session Flow are not touched. No new header field, no relaxed field, no new node
family. The plugin's output lands as a new `kind` of ordinary `sd/1` file, which is the extension
point the format already has. Existing readers, chains and rounds do not change.

## 1. Decisions

1. **Hook-only plugin.** Two short-lived processes per captured tool call. State lives on disk under
   the plugin's data directory. There is no recorder daemon and no local protocol.
2. **Nothing about git.** The plugin neither runs nor reads git. `.git/` is only an excluded
   directory name. Change detection is a stat cache and hashing.
3. **Defaults follow Claude Code.** The root is the session's project directory as the hook reports
   it. Settings live in the plugin's data directory. The tool scope, the exclusions and the
   read-only list are the ones below unless the user changes them.
4. **The change record is a git-style change log.** For each changed file: path, operation, size
   and hash before and after, and hunks as a unified diff with line numbers. One line per capture,
   final when written. No revisions, no multi-part results.
5. **A second adapter entry, `claude-code-changes`,** enabled by default, with its root resolved
   beside the transcript root and overridable with `source_root`. One adapter has one root because
   a landed header's `src` and the cursor are relative to it.
6. **Code under `plugins/claude-code/`.** Binaries in the ASF release and on the GitHub release page.
7. **The first milestone is the view document.** The analysis code adds the field to `asz.view`;
   the OAP follows; Horizon adopts.
8. **Claude Code's own record is used where it exists.** The runtime records a structured patch for
   `Edit`, `Write` and `NotebookEdit` on the main stream. The plugin does not hook those tools there.
9. **Subagents are covered by the plugin.** The runtime records no patch inside a subagent
   transcript, so there the plugin hooks the three editing tools as well, on `PostToolUse` only:
   the hook's own response carries the patch and the original content, proven in section 11.
   On the main stream the same hook writes no record and only brings the plugin's manifest up
   to date with the file, so the runtime's own edit is never found as nobody's. Shell tools are
   hooked everywhere.
10. **Retention is two rules, neither waiting for a collector.** Snapshot state for a root is dropped
    after 30 minutes idle. Output files are dropped by a TTL, 30 days by default.

## 2. Shape

```text
Claude Code hooks ──► plugins/claude-code (Go, hook-only)
                        └─ ${CLAUDE_PLUGIN_DATA}/output/<session>/<stream>.jsonl   append-only
                                          │
asz claude-code-changes adapter tails it ─┘  ──► <root>/<session>/streams/<stream>/changes-<stamp>-<seq>.sd
asz claude-code-local adapter               ──► <root>/<session>/streams/<stream>/transcript-…sd
                                                 (Edit/Write/NotebookEdit results gain a changes/1 data part)
view joins changes records to steps by tool id ──► asz.view workspace_changes
```

**Plugin output.** One JSONL file per stream of a session, `main.jsonl` or `<agent id>.jsonl`,
one line per capture, each line written whole and synced, so a landed file has one stream as a
transcript's does. The plugin never rewrites a line. The file is a source in the sense of Plan 01: the adapter
tails it, stops at the last complete newline, and records `src`, `ord`, `off` and `sha` as for a
transcript.

**Landed file.** Kind `changes`, dialect `asz-changes/1`, prefix `changes-`, under the stream the
hook reported: `main`, or the agent id inside a subagent. The kind is one lowercase word because the
OAP's filename parser accepts only `[a-z]+-` and its default mapping already places an unknown kind
under `streams/<stream>/`. Records carry a stable `id`, `tool` set to the tool-use id, `time` set to
the end of the after-scan, and one `data` part. They carry no `from` and no flags, so assembly emits
no node for them: steps come only from flags and assistant call blocks (`internal/assemble/emit.go`).
The chain still binds the file through the round's input digest, as it binds any landed file.

**Native changes.** The Claude adapter already decodes the runtime's structured tool result
(`internal/adapters/claudecode/convert.go`). For `Edit`, `Write` and `NotebookEdit` it adds a
`data` part in the `changes/1` shape beside the raw result, with `basis: runtime_reported`. The raw
result stays byte for byte. Files landed before this change stay as they are.

**View.** `asz.view` gains `workspace_changes[]`, one entry per change record, joined to its step
by `tool`. A step may have a native entry and a captured entry for the same tool id if a future
runtime starts recording patches for subagents; the view prefers the native one. Records with no
tool id are unattributed and are shown between the steps they fall between. A file whose change
several windows could have made appears on each of their steps, marked shared and linking to the
others, and is counted once in any total.

**Server code that changes.** One kind constant in `pkg/sessiondata`, one case in the index
classifier, `changes` added to the streams kinds in `internal/verify/session.go`, the adapter, the
refresher generalised to more than one local source, and the view join. Nothing in landed-file
enumeration, repack, push or the OAP's storage.

## 3. The change record, `changes/1`

One JSON object per line. Field names are the model's, never the runtime's.

```yaml
schema: changes/1
id: <producer>/<capture>            # stable; the landed record id
session: <session id>
stream: main | <agent id>
tool: <tool-use id>                 # absent on an unattributed record
tool_name: Bash
time: <RFC3339, end of the after-scan>
basis: tool_window | runtime_reported | unattributed | skipped_read_only
root: {path: /abs/project, id: <root id>}
policy: {exclusions: standard-v1, read_only: readonly-v1, expanded: [...]}   # frozen per record
window: {before: [from, to], after: [from, to]}
outcome: {state: returned | failed | not_executed | unfinished, exit_code: <int or null>}
coverage: complete | partial        # partial lists gaps
gaps: []
overlaps:                           # every other window open on this root during this one
  - {capture: <id>, session: <session id>, stream: main | <agent id>,
     tool: <tool-use id>, tool_name: Bash, state: closed | open}
changed_files: <int or null>
changes:
  - path: internal/x.go
    operation: create | modify | delete | type_change
    before: {present: true, bytes: 1234, sha256: ...}
    after:  {present: true, bytes: 1240, sha256: ..., no_newline_at_end: true}
    diff: available | binary | too_large | unavailable
    attribution: only_this_window | shared | outside_any_window
    windows: [<capture ids>]        # every window spanning a step in which this file changed
    additions: 1
    deletions: 1
    hunks:
      - {old_start: 12, old_lines: 1, new_start: 12, new_lines: 1,
         lines: ["-timeout := 10", "+timeout := 30"]}
```

The hunk shape is the one the runtime writes for its own patches: start and count on each side and
lines prefixed with `-`, `+` or a space. That is a unified diff as JSON, so the adapter copies a
native patch into the same shape almost unchanged. Line endings are preserved and a missing final
newline is stated. A file over the size cap, or one containing a NUL byte, lands as path and hash
only with `diff` saying why. The size cap is 1 MiB, picked, not measured: the plugin's own cost
is out of scope for now, decided 2026-09-08.

## 4. What is captured, by tool

| Tool | Main stream | Inside a subagent | Who records it |
| --- | --- | --- | --- |
| `Bash`, `PowerShell`, `Monitor` | scope scan | scope scan | plugin |
| `Edit`, `Write`, `NotebookEdit` | not hooked | `PostToolUse` only, from the hook's response | runtime on main, plugin in a subagent |
| `Read`, `Grep`, `Glob`, `LSP`, `WebFetch`, `WebSearch` | not hooked | not hooked | nobody; they do not write |
| `Agent` | not hooked | not hooked | the child's own hooks |
| MCP tools | opt-in matcher | opt-in matcher | plugin, when listed |

Hooks fire inside a subagent and the input carries `agent_id`, so a child's captures land under
the child's stream and join the child's own steps. Hooking `Agent` on the parent would only produce
a window that contains the children's work twice.

## 5. Capture strategy for a scope scan

- **Stat cache.** A manifest per root records path, size, modification time, inode and hash. A scan
  hashes only entries whose stat changed and re-hashes entries whose modification time falls within
  the scan's own clock granularity. Before-bytes of files whose hash changed since the last manifest
  are preserved in a content store under the plugin's data directory.
- **Two hook processes coordinate through a lock file per root.** Scans on one root serialise. A
  capture references two manifest ids, its before and its after, and compares those.
- **Overlapping windows on one root are stated in every record they touch.** Every scan appends a
  manifest to the root's chain, so the chain is a sequence of steps, each between two consecutive
  scans, and a window is the span of steps between its own before and after scans. With two tools
  running at once the chain reads `B1 B2 A1 A2`: step `B1→B2` lies only in window 1, `B2→A1` in
  both, `A1→A2` only in window 2. A file's hash changes in one or more steps, and the set of windows
  spanning those steps is what the record carries. Each record lists every other window that was
  open on the root during its own, with that window's session, stream and tool id, and each changed
  file says whether the change fell in steps this window alone spans, in steps shared with the
  listed windows, or in no window at all. Both records name each other, across streams and across
  sessions on the same root, because the root's chain is shared by every session in the plugin's
  data directory. Nothing here claims who wrote a byte: a person or the IDE can write inside any
  window. It says which tool windows could have. The view shows a shared change on each step it
  belongs to and counts it once.
- **The gap between two captures is recorded, unattributed.** When a before-scan finds changes
  since the previous after-manifest, that difference lands as a record with no tool id. It costs
  nothing extra and it is how an unhooked tool, the person, the IDE or a still-running background
  command surface.
- **A scan has a time cap.** Past it the capture records a gap and the hook returns; the tool runs.
  The cap is 30 seconds, picked, not measured; the plugin's own cost is out of scope for now.
- **Failure and cancellation.** `PostToolUseFailure` closes a window as a success does, because a
  failed command may have written. A denied or cancelled call gets no post hook, and nothing says
  whether its command still runs, so its window stays open until the idle time has passed, when
  it is closed as unfinished; until then a change in its span counts as shared with it rather
  than as nobody's. The editing tools have no BEFORE hook at all: their record comes from the
  AFTER hook's response.

## 6. Exclusions, `standard-v1`

Exclusion means no observation. A record names the set it used and the expanded rules, so history
explains itself, and an empty result means "no changes within the captured scope", never "no
changes". The set is frozen under its name; a changed set gets a new name.

Two kinds of rule, because the names build tools use are also names source uses:

- An **unconditional** name is excluded wherever it appears. It is never source.
- A **conditional** name is excluded only beside the ecosystem's own project file, which is how the
  build tools themselves decide. `build/` beside `pom.xml` is Maven output; `build/` in a repository
  with no project file beside it is kept, because many repositories keep scripts there.

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

Deliberately not in the defaults: `packages/`, because monorepos such as Horizon keep source there;
`.claude/`, because skills and settings are content people edit; `data/`, because the name is
generic. The plugin's own data directory is excluded automatically whenever a root contains it, so
it never captures its own writes.

Configuration:

```yaml
exclude:
  defaults: standard-v1
  add: ["/data/"]        # this checkout's asz storage root, a project addition
  remove: []             # for example "**/vendor/" to watch vendored code
```

`effective = (defaults − remove) ∪ add`. Rules are normalised and deduplicated before validation.
An unknown set name, a removal not in the set, and a rule in both lists are errors. A rule with a
leading `/` is anchored to the root; `**/` matches any depth; a trailing `/` selects a directory and
everything under it. Matching is case-sensitive. `.gitignore` is never read.

## 7. Read-only skip, `readonly-v1`

The hook writes its input to the plugin's stdin as JSON. For a shell tool it includes the command
text, verified on every one of the 92,181 shell calls in the corpus:

```json
{"hook_event_name": "PreToolUse", "session_id": "…", "agent_id": "…",
 "cwd": "…", "tool_name": "Bash", "tool_use_id": "toolu_…",
 "tool_input": {"command": "grep -rn foo internal/ | head", "description": "…"}}
```

The plugin classifies the command before deciding whether to scan. The classifier is small and
fails toward scanning. It works on the whole text, never the first word, because 88% of shell
commands in the corpus are compound and 17% start with a read-only segment and then write. It
honours quotes: a `|` inside a grep pattern is not a segment boundary, a `>` inside single quotes
is text, and a backtick inside double quotes still runs a command.

**Whole-command rules.** Any of these makes the command scan: an unquoted redirection, `>`, `>>`,
`&>` or a heredoc `<<`, from any descriptor, to anything but `/dev/null` or another descriptor; a
command substitution, `$(` or a backtick, outside single quotes; a process substitution, `<(` or
`>(`.

**Segment rule.** Split the rest on unquoted `|`, `||`, `&&`, `;`, `&` and newlines. Drop a
leading `if`, `while`, `until`, `then`, `else`, `elif`, `do`, `time`, `!` and variable
assignments; a segment that is only `for … in …`, `case … in`, `done`, `fi` or `esac` holds no
command. Every remaining segment's first word must be on this list:

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
       worktree prune; with or without -C and --no-pager
go     version env list doc
```

`git add` is on the list because it changes only `.git/`, which is excluded scope. `git commit` is
not, because a pre-commit hook can rewrite files. `sudo`, `eval`, `exec`, `source`, `xargs`
handing to anything else, every interpreter and every build tool are simply absent, so they scan.

**Fixture.** `plugins/claude-code/internal/readonly/testdata/commands.txt` holds 93 commands
picked from the corpus on 2026-09-08, shortened and stripped of private names, each with the
outcome it must get. The Go test reads that file. A reference implementation checked every label
and measured the corpus: 69.3% of Bash calls are read-only under these rules, against 42.5% for a
first draft that split on `|` inside quotes and treated the `&` in `2>&1` as a separator, which is
why the fixture carries those shapes.

A read-only call still produces a record, `basis: skipped_read_only`, with no changes, so the view
never shows a silent blank. A misclassification cannot lose a change: the next scan compares
against the last manifest, so a write that slipped through lands as an unattributed change between
two steps. The list is versioned and named in every record, and the feature has an off switch.
It is on by default, decided 2026-09-08: it removes most scans and a miss is not a loss.

## 8. Retention

- **Snapshot state per root.** When a capture's record is written and nothing is pending on that
  root, a 30-minute idle clock starts. When it expires, the manifests' preserved bytes are deleted
  and the next BEFORE takes a fresh base. The last manifest, which is only paths, hashes and stat
  data, is kept so the next base can still report the idle-time gap as path and hash. The clock is
  per root, not per session, because two sessions in one checkout share it. With no daemon, the
  clock is checked at the next hook of any session and on `SessionStart` and `SessionEnd`.
- **Output files.** A TTL on the plugin's output, 30 days by default, measured from a session file's
  last write, deleting the whole file. This matches Claude Code's own transcript cleanup. Whatever
  the server has landed stays under the server's own retention; the adapter's cursor already
  handles a source that has gone away.

## 9. Measurements, 2026-09-08

One machine's storage root, 52 sessions, every stream. The corpus is skewed toward Bash by this
user's auto-mode instruction to edit through the shell; a typical user's native share is higher.

| Tool calls | Count | Share |
| --- | --- | --- |
| all | 104,142 | |
| `Bash` | 92,181 | 88.5% |
| of which inside a subagent | 65,335 | 70.9% of Bash |
| `Edit` and `Write` | 2,697 | 2.6% |
| read-only tools | 9,264 | 8.9% |

| `Edit` and `Write` results | With the runtime's patch | Without |
| --- | --- | --- |
| main stream, succeeded | 2,221 | 0 |
| subagent stream, succeeded | 0 | 453 |
| failed, any stream | 0 | 26 |

| Shell command text | Count | Share |
| --- | --- | --- |
| carries `command` | 92,181 | 100% |
| compound, more than one segment | 81,086 | 88.0% |
| five or more segments | 50,104 | 54.4% |
| first segment read-only, whole command writes or unknown | 15,645 | 17.0% |
| read-only under `readonly-v1` | 63,889 | 69.3% |
| must scan | 28,292 | 30.7% |

Per session: median 22 native edits, 581 shell calls, 160 shell calls classified as writing. The
largest session ran 15,913 shell calls.

The plan of 2026-09-07 measured a full read and hash of the SkyWalking checkout at 3.1 to 3.2 s per
warm pass over 49,749 files and 0.58 s over 11,284 files with exclusions, with a Python prototype.
A stat-cache pass is `unmeasured`.

## 10. Open items

- Everything about Claude Code's behaviour is proven by a run, never taken from documentation.
  Section 11 holds what has been proven. Windows is a build target whose hook command line has
  not been exercised.
- Windows is a target; its stat identity and lock live in platform files as elsewhere in the tree.
- Scenarios: a tool step in a scenario gains `changes`, the mock writer lands a `changes` file
  under the stream, and the first expectation is that the chain folds to the same nodes with and
  without it.

## 11. Proof of concept, 2026-09-08

Claude Code 2.1.260, headless with `-p`, loading a throwaway plugin through `--plugin-dir` whose
hooks append their stdin and environment to a log. The session ran `echo hello`, an `Edit`, a
failing `cat`, and one `general-purpose` subagent that ran `ls`, a `Write` and a failing `cat`.
Fourteen hook events were logged. Each fact below was read from the log or from the files on
disk, none from documentation.

- `hooks/hooks.json` with a top-level `hooks` object and `${CLAUDE_PLUGIN_ROOT}` in its commands
  loads and fires in headless mode.
- Every `PreToolUse`, `PostToolUse` and `PostToolUseFailure` input carries `session_id`,
  `tool_use_id`, `tool_name`, `tool_input`, `cwd`, `transcript_path` and `prompt_id`. For a shell
  tool `tool_input.command` is the command text.
- Inside the subagent every tool event carries `agent_id` and `agent_type`. The value
  `afb0b607bcfa2020a` is the file name `subagents/agent-afb0b607bcfa2020a.jsonl`. The assumption
  is proven.
- `transcript_path` inside a subagent still names the main transcript; `agent_transcript_path`
  appears on `SubagentStop` only. The plugin reads neither.
- A failed shell command fires `PostToolUseFailure`, with `error` holding the exit code and the
  standard error text, and `is_interrupt`. `PostToolUse` does not fire for it.
- `PostToolUse` for `Edit` carries `tool_response.structuredPatch`, hunks shaped
  `{oldStart, oldLines, newStart, newLines, lines[]}` with `-`, `+` and space prefixes, plus
  `originalFile`, the complete content before the edit, and `userModified`. For `Write` of a new
  file it carries `content`, `originalFile: null` and an empty patch. For `Write` over an existing
  file it carries `type: update`, `content`, `originalFile` and a patch whose last line can be the
  git marker `\ No newline at end of file`. `NotebookEdit` carries `notebook_path`,
  `original_file`, `updated_file`, `old_source`, `new_source`, `cell_id`, `edit_mode` and no
  patch. The transcript's `toolUseResult` carries the same keys. All measured on 2026-09-08.
- The subagent's own transcript holds no `structuredPatch` for its `Write`; the main transcript
  holds one for its `Edit`. This matches the corpus.
- `CLAUDE_PLUGIN_DATA` is `~/.claude/plugins/data/asz-poc-inline` and the directory exists before
  the first hook runs. A plugin loaded with `--plugin-dir` gets the suffix `-inline`; an installed
  one gets its marketplace name. `CLAUDE_PLUGIN_ROOT` is the plugin directory; `CLAUDE_PROJECT_DIR`
  and `cwd` are the workspace.
- A `Write` hook with `timeout: 2` running a 5-second sleep was killed: it never wrote its final
  line, and the file was written anyway. A timed-out hook does not block the tool.
- The subagent was started in the background by default, and its hooks fired all the same.

Consequence: for `Edit`, `Write` and `NotebookEdit` inside a subagent the plugin needs
`PostToolUse` only. The response hands it the patch and the original content; it hashes the file
after and writes the record. No before-scan and no preserved bytes.

A second run, of the built plugin itself inside Claude Code with a shell command, an edit and a
subagent, showed one gap: an edit the runtime records is not hooked by the plugin, so the plugin's
next scan found it as a change nobody made. Fixed the same day: the plugin's `PostToolUse` hook on
the editing tools brings the root's manifest up to date with the file the tool wrote, on both
streams, as a closed window of its own, so the next scan finds nothing and a window open at the
same time sees the edit as shared and names it.

The plugin, its log and the session it produced live in the session scratchpad, not in the
repository.
