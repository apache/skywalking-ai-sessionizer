# Configuration

One YAML file. asz reads the file given with `-config`, else `asz.yaml` in the working directory,
else the built-in defaults. A file may leave keys out; anything unset takes its default. A file
that lists `adapters` replaces the whole default list.

[`asz.yaml`](https://github.com/apache/skywalking-ai-sessionizer/blob/main/asz.yaml) in the
repository holds every default, written out. Download the one of your version to start from it:

```sh
VERSION=$(asz version | cut -d ' ' -f 2)
curl -fsSL -o asz.yaml "https://raw.githubusercontent.com/apache/skywalking-ai-sessionizer/v$VERSION/asz.yaml"
```

## storage

| Key | Default | Meaning |
| --- | --- | --- |
| `root` | `./data` | Where collected data lands and conversations are assembled. A relative path is resolved from the working directory. |
| `retention.metadata_only_after` | `-1` | How long a conversation may be idle, with nothing new landed for it, before the root keeps only its metadata, to save disk: what was said and run is removed, and who did what, when, in what order and at what cost stays. New activity continues the same conversation. The removed content cannot be restored from the root; only a receiver that accepted it still has it. A duration such as `72h`, or days such as `30d`. It applies whether the conversation was sent or not, and whether an endpoint is set or not. `-1` is off. See [Retention](../formats/storage-root.md#retention). |
| `retention.retire_after` | `-1` | How long a conversation may be idle before everything of it is removed, its metadata too, and it is retired. A retired conversation is never collected again, even when its runtime resumes it. Set this longer than the runtime keeps its own history. It uses the same clock as `metadata_only_after` and must be at least as long. Equal values retire a conversation with no metadata-only step first. Setting it with `metadata_only_after` off or shorter than that value is a configuration error. When this is set, asz reports what it could inspect in Claude Code's settings. It says that effective retention was not determined. It warns when this duration is shorter than the inspected `cleanupPeriodDays` or the reported default of 30 days. Kiro always warns when this is set. `-1` is off. Only both off keeps everything. See [Retirement](../formats/storage-root.md#retirement). |

## adapters

A list of the sources asz collects from.

| Adapter | On by default | Collects |
| --- | --- | --- |
| `claude-code-local` | yes | Claude Code's transcripts on this machine |
| `changes` | yes | what a tool call changed on disk, from the [Claude Code](claude-code-plugin.md) or [LangChain](langchain-plugin.md) plugin, and each call to an MCP server the Claude Code plugin saw; see [below](#the-changes-adapter) |
| `claude-code-provider` | yes | the request and response bodies Claude Code writes when asked; see [below](#the-provider-adapter) |
| `kiro-local` | yes | the sessions Kiro's V3 agent engine writes on this machine, for kiro-cli and Kiro Crew; see [below](#the-kiro-adapter) and [Kiro](kiro.md) |
| `langsmith-ingest` | no | what the LangSmith tracing client sends, from a LangChain or LangGraph application; see [LangChain and LangGraph](langchain.md) |

### More than one agent

- **Several Claude Code instances of one user, on one machine.** Supported with nothing to
  configure. They all write under one directory, `~/.claude`, and one `claude-code-local` entry
  discovers every session there; sessions are told apart by their own ids, and each is collected
  and parsed behind a lock of its own.
- **Several LangChain or LangGraph applications, or instances of one.** Supported by one
  `langsmith-ingest` receiver. A conversation is owned by its project and its thread, so two
  deployments never share one unless they share both.
- **Several machines.** One asz on each, with its own storage root, exporting to one SkyWalking
  OAP. `instance_id` says who is pushing, `user@host` by default, so instances stay apart there.
- **kiro-cli and Kiro Crew on one machine.** Supported by one `kiro-local` entry. Both write
  under `~/.kiro`, and each session is collected on its own.
- **Several recorder directories on one machine.** One `changes` entry per directory, as
  [below](#the-changes-adapter).
- **Several Claude Code directories on one machine** - two users, or instances started with
  different `CLAUDE_CONFIG_DIR` values - need one asz process per directory today, each with its
  own `source_root` and storage root. One process reads one Claude Code directory.

| Key | Default | Meaning |
| --- | --- | --- |
| `name` | | One of the adapters above. |
| `enabled` | see above | A disabled adapter is skipped. |
| `source_root` | empty | The directory Claude Code keeps its projects in, one directory per project. Empty means `projects` under `CLAUDE_CONFIG_DIR`, else under `XDG_CONFIG_HOME/claude`, else under `~/.claude`. To collect from a copy, name the copy's `projects` directory, not the directory above it. |
| `include` | empty | [Session filters](#session-filters) a session must match. Empty means every session. |
| `exclude` | `/private/tmp/**` | [Session filters](#session-filters) that leave a session out. `exclude: []` clears the default. |
| `listen` | `127.0.0.1:1985` | `langsmith-ingest` only: the address to receive on; see [LangChain and LangGraph](langchain.md). |

### Session filters

A session is matched by the directory Claude Code ran in. A filter that starts with `/` is a
directory, and `**` after it matches everything beneath it, such as `/Users/me/scratch/**`. Any
other filter is a glob matched against Claude Code's name for the directory: the path with every
separator replaced by `-`.

The default excludes `/private/tmp/**`, where Claude Code runs its own helper agents.

## collector

Set on each adapter.

| Key | Default | Meaning |
| --- | --- | --- |
| `mode` | `watch` | `watch` collects again every `interval`. `once` collects once; `asz collect` then exits. `-once` on the command line does the same. |
| `interval` | `10m` | The time between collections. `asz collect` and `asz server` also collect when they start. |
| `max_delta_bytes` | `2097152` | The largest file asz writes from one source, 2 MiB. A single longer record gets a file of its own, and its largest payloads are left out until it fits. A record still longer with every payload left out, such as one with a very long label, lands as it is, over this size: see [Too large to land](../formats/session-data.md#too-large-to-land). |

When several adapters are enabled, asz collects from all of them together, at the shortest
`interval` among them, and watches if any of them has `mode: watch`.

### Choosing an interval

Use minutes. Every collection that finds something new writes new files, so a short interval
writes many small files, and sends many small records to a receiver. A longer interval shows new
data later.

## parse

| Key | Default | Meaning |
| --- | --- | --- |
| `max_round_bytes` | `2097152` | The largest round file the parser writes, 2 MiB. A round over it covers fewer landed files, and revisions that need no new evidence are cut by their frames. A round covering one landed file may exceed it. |

## metrics

The metrics asz derives from the collected data, whichever adapter collected it. See
[Metrics](export-otlp.md#metrics).

| Key | Default | Meaning |
| --- | --- | --- |
| `enabled` | `true` | Derive the metrics. They are derived only when they will be sent: `export.otlp.endpoint` is set and `export.otlp.metrics` is on. |
| `lookback` | `72h` | How far back the first derivation reaches, such as `72h` or `3d`. `0` or `none` derives everything. Later ones derive everything new. |

## export

Where to send the collected data. See [Export over OpenTelemetry](export-otlp.md).

```yaml
export:
  otlp:
    protocol: grpc
    endpoint: ""
    tls: false
    service_name: ""
    instance_id: ""
    layer: AI_AGENT
    batch_bytes: 8388608
    max_bytes_per_minute: 0
    logs: true
    metrics: true
```

| Key | Default | Meaning |
| --- | --- | --- |
| `protocol` | `grpc` | `grpc`, or `http` with a protobuf body. |
| `endpoint` | empty | For `grpc`, `host:port`, such as the SkyWalking OAP's `127.0.0.1:11800`. For `http`, a base URL, such as `http://127.0.0.1:12800`. Empty sends nothing. |
| `tls` | `false` | For `grpc`, connect with TLS. For `http`, the URL's scheme decides. |
| `service_name` | empty | The service every record belongs to. Empty means the agent that produced the session, such as `Claude Code`. |
| `instance_id` | empty | Who is sending, such as a name or an email address. Empty means `user@host`. |
| `layer` | `AI_AGENT` | The layer the receiver puts the service in. |
| `headers` | none | Sent with every request, such as `Authorization`. |
| `batch_bytes` | `8388608` | The most file bytes in one request, 8 MiB. A larger file goes alone. |
| `max_bytes_per_minute` | `0` | A limit on bytes sent per minute. `0` is no limit. |
| `logs` | `true` | Send the collected files and rounds. |
| `metrics` | `true` | Send the metrics. `logs` or `metrics` must be on. |

## view

What `asz view` and `asz server` serve. asz knows nothing about who is reading, so these apply to
every reader of the instance. Two audiences are two instances over the same root, each behind the
deployment's own authentication. Or one instance with `hide_override` on serves the API through a
host's own route, and the host sets the `hide` parameter on every request for each reader. See
[Withholding](../formats/asz-view.md#withholding).

A key this section does not have is refused, and the refusal names the key and its line. Every
other section is read loosely, as it always was. This one says what a reader is kept from, so a
misspelled key here would otherwise show everything to everyone.

`hide` is read only inside `view`, written there or reached by an alias or a merge key. Written
anywhere else, such as at the top level or under a misspelled `view`, it is one more key the
configuration does not have: ignored, and withholding nothing.

Only the first YAML document of a file is read, so a file with a later document that holds anything
is refused. `view` is read from 0.6.0. An older asz ignores it, and withholds nothing.

When `hide` is set, or `hide_override` is on, `asz view` and `asz server` say so as they start. A record is named for what it
carries when it lands, so a root collected before asz named these records carries no names, and
withholds nothing. Collect it again into a new root to withhold there.

| Key | Default | Meaning |
| --- | --- | --- |
| `hide` | `[]` | The flags whose steps the page withholds from every reader: `system_prompt`, `tool_schemas`. A withheld step keeps its place, its flags and its size, loses its text, and has state `omitted`. The provider bodies go with it, because a request carries both again. With `hide_override` on, this applies to a request that sets no `hide` parameter. |
| `hide_override` | `false` | Whether a request's `hide` parameter replaces `hide` for that request. On, a host that serves the API through its own route decides what each reader is kept from, and `hide=` with no name withholds nothing. Off, `hide` is the whole answer, and a request that carries the parameter is refused with status 400. |

## judge

What [`asz judge`](judge.md) and `asz measure` use. A run happens only when a person runs `asz
judge`.

| Key | Default | Meaning |
| --- | --- | --- |
| `pricing` | `""` | The price file. Empty prices nothing, and the tokens still count. |
| `hide` | `[]` | The flags whose records the evaluators are never shown: `system_prompt`, `tool_schemas`. |
| `timeout` | `30m` | How long one evaluator call may take. A stage cut into calls runs several, and a call that fails, a timeout included, is tried once more. |
| `evaluators` | `claude`, `codex` and `kiro` | The evaluators a stage can be given to, by name. Each has `kind` (`cli` or `api`) and `program` (`claude`, `codex` or `kiro-cli`; `anthropic`, `openai` or `bedrock`), and as it needs `command`, `args`, `model`, `endpoint`, `api_key_env`, `region`, `profile`, `max_turns` and `max_tokens`. See [Evaluators](judge.md#evaluators). |
| `read_bytes` | `131072` | What one evaluator call may read through the navigation, in bytes of answers. An answer that would read past it is refused whole, the evaluator is told to answer with what it read, and the stage records it. `0` is no bound |
| `mode` | `auto` | How a stage is given to its evaluator: `single`, one call; `stream`, cut into calls; `auto`, one call when the rows it is given fit `single_bytes`, calls of `chunk_bytes` beyond |
| `single_bytes` | `32768` | The rows one call of a stage may be given in `auto` mode |
| `chunk_bytes` | `24576` | The rows one call of a stage is given when it is cut |
| `state_bytes` | `16384` | What one call of a targets scan is given of the targets found so far. Past it they are shortened, and the `targets` tool gives the rest whole |
| `progress` | `30s` | How often a running stage prints what it has consumed so far. `0` prints it only when the stage ends |
| `concurrency` | `4` | How many calls of one stage run at once. A targets scan always runs one at a time |
| `plan` | `targets`, `achievement`, `subtasks` and `efficiency`, each by `claude` | The stages `asz judge` runs when it is not told a plan, in order, each with its `stage` and `evaluator`. The first is `targets`. A run given `-reference` adds `reference` and `divergence` when the plan lists neither. |
| `references.max_bytes` | `1048576` | The bytes a saved copy of a source may take. A larger source is saved cut, and says so |
| `references.timeout` | `1m` | How long fetching one source may take |
| `references.fetch` | `[]` | Commands that fetch the sources a login guards, such as tickets: each a `prefix` the source starts with, a `command` given the source as its last argument, which writes the content on its standard output and never sees the conversation, and the `content_type` it writes, `text/plain` when empty |

## The changes adapter

`changes` collects the records `asz-changes` writes: what each tool call changed on disk, for
the [Claude Code plugin](claude-code-plugin.md) or the [LangChain plugin](langchain-plugin.md),
and each call to an MCP server the Claude Code plugin saw.
With no `source_root` it reads Claude Code's plugin directory, `plugins/data` under
`CLAUDE_CONFIG_DIR`, else `XDG_CONFIG_HOME/claude`, else `~/.claude`, and does nothing when the
plugin is not installed. A LangChain recorder writes where `ASZ_CHANGES_DATA` points, so that is
its `source_root`.

One entry reads one directory. A machine that runs both names the adapter twice:

```yaml
adapters:
  - name: changes
    enabled: true
  - name: changes
    enabled: true
    source_root: /var/lib/asz/changes
```

Two entries that read one directory are refused.

## The provider adapter

`claude-code-provider` collects the request and response bodies Claude Code sends to its model
provider. Claude Code writes them only when you turn it on, as
[Claude Code Provider Bodies](claude-code-provider-bodies.md) shows.

## The Kiro adapter

`kiro-local` collects the sessions Kiro's V3 agent engine writes, as [Kiro](kiro.md) shows how to
turn on. With no `source_root` it reads `~/.kiro`, the directory that holds Kiro's `sessions`
directory, and does nothing where Kiro is not installed. Kiro does not read `KIRO_HOME` for these
sessions, so neither does asz.

Its session filters match the workspace paths in each session's `session.json`, not a directory
name. A filter ending in `/**` matches that path and everything beneath it; any other filter is a
glob over the whole path. A session is left out only when every one of its paths is. Kiro records
the real path, so on macOS a session started in `/tmp` is under `/private/tmp`.
