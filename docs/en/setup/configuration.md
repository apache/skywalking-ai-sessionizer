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

## adapters

A list of the sources asz collects from.

| Adapter | On by default | Collects |
| --- | --- | --- |
| `claude-code-local` | yes | Claude Code's transcripts on this machine |
| `changes` | yes | what a tool call changed on disk, from the [Claude Code](claude-code-plugin.md) or [LangChain](langchain-plugin.md) plugin; see [below](#the-changes-adapter) |
| `claude-code-provider` | yes | the request and response bodies Claude Code writes when asked; see [below](#the-provider-adapter) |
| `claude-code-otlp` | no | what Claude Code's own OpenTelemetry exporter sends; see [below](#the-receiver-adapter) |
| `langsmith-ingest` | no | what the LangSmith tracing client sends, from a LangChain or LangGraph application; see [LangChain and LangGraph](langchain.md) |

`changes` was called `claude-code-changes` until 0.5.0, because only Claude Code wrote those
records. A configuration that still names it is read as the same adapter.

| Key | Default | Meaning |
| --- | --- | --- |
| `name` | | One of the adapters above. |
| `enabled` | see above | A disabled adapter is skipped. |
| `source_root` | empty | Where to read from. Empty means where Claude Code keeps its files: `CLAUDE_CONFIG_DIR`, else `XDG_CONFIG_HOME/claude`, else `~/.claude`. Set it to collect from a copy. |
| `include` | empty | [Session filters](#session-filters) a session must match. Empty means every session. |
| `exclude` | `/private/tmp/**` | [Session filters](#session-filters) that leave a session out. `exclude: []` clears the default. |
| `metrics` | `false` | Derive Claude Code's token metrics from the collected data. See [Metrics](export-otlp.md#metrics). |
| `metrics_lookback` | `24h` | How far back the first derivation reaches, such as `24h` or `7d`. `0` or `none` derives everything. |
| `listen` | none | `claude-code-otlp` only: the address to receive on, such as `127.0.0.1:4317`, gRPC and HTTP on one port. |

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
| `max_delta_bytes` | `2097152` | The largest file asz writes from one source, 2 MiB. A single longer record gets a file of its own. |

When several adapters are enabled, asz collects from all of them together, at the shortest
`interval` among them, and watches if any of them has `mode: watch`.

### Choosing an interval

Use minutes. Every collection that finds something new writes new files, so a short interval
writes many small files, and sends many small records to a receiver. A longer interval shows new
data later.

## parse

| Key | Default | Meaning |
| --- | --- | --- |
| `max_round_bytes` | `2097152` | The largest round file the parser writes, 2 MiB. |

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

## The changes adapter

`changes` collects the records of the [Claude Code plugin](claude-code-plugin.md). It
finds them under `plugins/data` in Claude Code's directory. It does nothing when the plugin is not
installed.

## The provider adapter

`claude-code-provider` collects the request and response bodies Claude Code sends to its model
provider. Claude Code writes them only when you turn it on, as
[Claude Code Provider Bodies](claude-code-provider-bodies.md) shows.

## The receiver adapter

```yaml
adapters:
  - name: claude-code-otlp
    enabled: true
    listen: 127.0.0.1:4317
    metrics: true
```

`claude-code-otlp` receives Claude Code's own metrics. Start Claude Code with:

```sh
CLAUDE_CODE_ENABLE_TELEMETRY=1 OTEL_METRICS_EXPORTER=otlp OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4317 claude
```

It runs while `asz collect` or `asz server` runs, but not with `-once`. It keeps the metrics for
[export](#export), and drops logs and traces. Turn `metrics` on here or on `claude-code-local`, not
on both, since both would count the same tokens.
