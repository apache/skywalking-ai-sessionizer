# Configuration

One YAML file. Every command reads `asz.yaml` from the working directory when no `-config` flag is
given, and `-config FILE` names another one. The file at the repository root is the default
configuration with every value written out, and a test holds it to the compiled defaults, so
reading that file is reading the defaults.

```yaml
storage:
  root: ./data

adapters:
  - name: claude-code-local
    enabled: true
    source_root: ""
    include: []
    exclude:
      - /private/tmp/**
    collector:
      mode: watch
      interval: 5s
      max_delta_bytes: 2097152
```

## storage

| Key | Default | Meaning |
| --- | --- | --- |
| `root` | `./data` | The storage root: where collected data lands and where conversations are assembled. A relative path is resolved from the working directory. |

The default is ignored by git, so running the collector inside a checkout never stages private
transcripts.

## adapters

A list. Version 0.1.0 has one adapter, `claude-code-local`, which reads Claude Code's files from
this machine. Every command runs once per enabled adapter.

| Key | Default | Meaning |
| --- | --- | --- |
| `name` | | `claude-code-local` |
| `enabled` | `true` | A disabled adapter is skipped by every command. |
| `source_root` | empty | Where Claude Code keeps its files. Empty resolves it the way Claude Code does: `CLAUDE_CONFIG_DIR`, then `XDG_CONFIG_HOME/claude`, then `~/.claude`, each followed by `projects`. Set it only to collect from a copy or a mounted directory. |
| `include` | empty | Session filters, see below. Empty means every session is a candidate. |
| `exclude` | `/private/tmp/**` | Session filters, see below. |
| `metrics` | `false` | Derive the runtime's own metric family from the landed files, `claude_code.token.usage` in phase one, name for name with the runtime's exporter. See [Metrics](export-otlp.md#metrics). |
| `listen` | none | On `claude-code-otlp` only: the address the runtime's exporter is pointed at, such as `127.0.0.1:4317`, serving gRPC and HTTP with protobuf on the one port. |
| `metrics_lookback` | `24h` | How far back the first derivation over a root reaches. A duration such as `24h`, or a number of days such as `7d`; `0` or `none` derives everything. Later passes derive every new file whole. |

### Session filters

A session is judged by the working directory its main transcript was recorded under. An entry that
starts with `/` is a working directory, and `**` after it matches everything beneath. Anything else
is a glob matched against the source directory name as Claude Code wrote it, which is the working
directory with every separator replaced by `-`.

A session's child agents can run in other directories, so the filter looks at the main transcript
only. A session that merely used a scratch directory for a child agent is still collected. When the
main transcript has been pruned, the session is judged on all of its directories together and is
excluded only when every one of them matches, so an orphaned child stream from a real project is
still collected.

Claude Code runs its own helper agents in scratch directories under `/private/tmp`. Those sessions
are the tool's, not yours, which is why they are excluded by default.

## collector

| Key | Default | Meaning |
| --- | --- | --- |
| `mode` | `watch` | `watch` polls the source continuously. `once` makes a single pass and exits, which is the backfill path over history that already exists. `-once` on the command line overrides the file. |
| `interval` | `5s` | How long the collector sleeps between passes in watch mode. `asz view` refreshes on the same interval. |
| `max_delta_bytes` | `2097152` | The largest `.sd` file the collector writes, 2 MiB. A large catch-up is split into several files, and a single record larger than this is landed whole. A file travels whole as one log record, so this is also the largest record a receiver has to accept. A change applies to new files only; `asz repack` brings an existing root under a new budget. |

## parse

```yaml
parse:
  max_round_bytes: 2097152
```

| Key | Default | Meaning |
| --- | --- | --- |
| `max_round_bytes` | `2097152` | The largest `.sf` round file the parser writes, 2 MiB, the same budget as a landed file. A round travels whole as one log record. The parser narrows a round's input window until the round fits and leaves the rest of the evidence to the next round, so one parse pass may write several rounds. A round covering a single landed file is published whole even when larger. |

## export

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
    interval: 5s
```

| Key | Default | Meaning |
| --- | --- | --- |
| `protocol` | `grpc` | The transport: `grpc`, or `http` with a protobuf body. The SkyWalking OAP accepts both. |
| `endpoint` | empty | Where the receiver listens. For `grpc`, `host:port`: the OAP's gRPC port, `127.0.0.1:11800` by default. For `http`, the receiver's base URL, to which `/v1/logs` is appended: the OAP's REST port, `http://127.0.0.1:12800`. Empty means `asz push` refuses to run. |
| `tls` | `false` | For `grpc`, connect with TLS, verified against the system's roots. For `http`, the scheme of the endpoint decides. |
| `service_name` | empty | The service every record is attributed to. Empty means the runtime that produced each session, read off its landed header: `Claude Code` for `claude-code-local`, `Mock Agent` for `mock`. One service per kind of agent. |
| `instance_id` | empty | Sent as `service.instance.id`: who is pushing, in words the people reading the receiver recognise, for example a mailbox such as `wusheng@tetrate.io`, a name, or a machine. Empty means `user@host` of the machine running `asz push`, which is stable across restarts. |
| `layer` | `AI_AGENT` | Sent as `service.layer`, the layer the receiver places the service in. The OAP selects its rules by layer, and a layer name is upper case with underscores. |
| `headers` | none | Sent with every request, as gRPC metadata or as HTTP headers, for example `Authorization`. |
| `batch_bytes` | `8388608` | How many file bytes one request carries at most, 8 MiB, which keeps a request under the 10 MiB the OAP's HTTP server accepts and well under the 50 MB its gRPC server accepts. A file larger than this is sent alone, in a request of its own. |
| `max_bytes_per_minute` | `0` | At most this many bytes on the wire per minute: a pass waits before a request until a minute's budget covers it. Zero is no limit. See [Rate](export-otlp.md#rate). |
| `interval` | `5s` | How long `asz push` sleeps between passes in watch mode. |

See [Export over OpenTelemetry](export-otlp.md) for what is sent.

## The receiver adapter

```yaml
adapters:
  - name: claude-code-otlp
    enabled: true
    listen: 127.0.0.1:4317
    metrics: true
```

`claude-code-otlp` receives what Claude Code's own OpenTelemetry exporter sends, with the
runtime configured as its documentation says: `CLAUDE_CODE_ENABLE_TELEMETRY=1`,
`OTEL_METRICS_EXPORTER=otlp`, `OTEL_EXPORTER_OTLP_ENDPOINT` at `listen`, over gRPC or
`http/protobuf`. Phase one lands its metrics in the storage root's spool for `asz push`; logs and
traces are accepted and dropped. It runs while `asz collect` or `asz view` runs, beside the
local adapter, and not with `-once`. `metrics` may be on here or on `claude-code-local`, never
on both: the configuration refuses to load, since the two would count the same tokens twice.

## Precedence

1. `-config FILE` on the command line.
2. `./asz.yaml` in the working directory.
3. The compiled defaults, which are the values shown above.

A file may leave keys out. Anything unset takes its default, except that a file which lists
`adapters` replaces the whole list.
